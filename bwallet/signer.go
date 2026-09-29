package bwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// Signer is an identity that signs through a BRC-100 wallet: the embedded
// wallet here, or one reached over the wallet wire. Every derivation hangs
// off the identity key through GetPublicKey and every signature goes through
// CreateSignature, so nothing here depends on which program holds the root
// key.
type Signer struct {
	wallet.Interface
	// Identity is the root public key: the subject of every record.
	Identity *ec.PublicKey
	// Originator is the BRC-100 originator presented on every call.
	Originator string
	// Mainnet selects the address prefix where one is rendered.
	Mainnet bool
	// Profile names the funding key. It has no default: the fund methods
	// refuse a Signer whose Profile is incomplete rather than derive some
	// other key, while the calls that never touch the fund key work
	// without one.
	Profile Profile
}

// Signer views the embedded wallet as a Signer, under the profile it was
// opened with.
func (e *Embedded) Signer() *Signer {
	return &Signer{Interface: e, Identity: e.identity, Originator: e.Originator, Mainnet: e.Mainnet, Profile: e.profile}
}

// IdentityKey is the root public key.
func (s *Signer) IdentityKey() *ec.PublicKey { return s.Identity }

// IdentityHex is the identity key as compressed hex, the form state files
// and the domain's resolve document carry.
func (s *Signer) IdentityHex() string { return hex.EncodeToString(s.Identity.Compressed()) }

// FundKey is the public half of the funding key, derived through the wallet
// under the Signer's Profile.
func (s *Signer) FundKey() (*ec.PublicKey, error) {
	if err := s.Profile.Validate(); err != nil {
		return nil, err
	}
	forSelf := true
	res, err := s.GetPublicKey(context.Background(), wallet.GetPublicKeyArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   s.Profile.FundProtocol,
			KeyID:        s.Profile.FundKeyID,
			Counterparty: fundCounterparty,
		},
		ForSelf: &forSelf,
	}, s.Originator)
	if err != nil {
		return nil, fmt.Errorf("bwallet: derive fund key: %w", err)
	}
	return res.PublicKey, nil
}

// FundScript is the P2PKH locking script of the funding key: what coinbase
// pays to and what the pool holds.
func (s *Signer) FundScript() (*script.Script, error) {
	pub, err := s.FundKey()
	if err != nil {
		return nil, err
	}
	// The address is only a carrier for the hash160; the network flag does
	// not change the script, so mainnet=false is not a network decision.
	addr, err := script.NewAddressFromPublicKey(pub, false)
	if err != nil {
		return nil, err
	}
	return p2pkh.Lock(addr)
}

// FundAddress renders the funding key as a base58 address for
// generatetoaddress.
func (s *Signer) FundAddress(mainnet bool) (string, error) {
	pub, err := s.FundKey()
	if err != nil {
		return "", err
	}
	addr, err := script.NewAddressFromPublicKey(pub, mainnet)
	if err != nil {
		return "", err
	}
	return addr.AddressString, nil
}

// FundUnlocker returns a template that signs a pool output through the
// wallet. It satisfies transaction.UnlockingScriptTemplate, so it can be
// handed to AddInputFrom and signed by tx.Sign.
func (s *Signer) FundUnlocker() transaction.UnlockingScriptTemplate {
	return &fundUnlocker{w: s}
}

// PaymentDestination is the sender's side of BRC-29: the P2PKH script for
// recipient under the sender's root, derived through the wallet.
func (s *Signer) PaymentDestination(ctx context.Context, recipientHex, prefix, suffix string) (*script.Script, error) {
	cp, err := Counterparty(recipientHex)
	if err != nil {
		return nil, err
	}
	forSelf := false
	res, err := s.GetPublicKey(ctx, wallet.GetPublicKeyArgs{
		EncryptionArgs: wallet.EncryptionArgs{ProtocolID: PaymentProtocol, KeyID: PaymentKeyID(prefix, suffix), Counterparty: cp},
		ForSelf:        &forSelf,
	}, s.Originator)
	if err != nil {
		return nil, err
	}
	addr, err := script.NewAddressFromPublicKey(res.PublicKey, s.Mainnet)
	if err != nil {
		return nil, err
	}
	return p2pkh.Lock(addr)
}

// DerivedKey is the recipient's side: our public key under d, which must
// equal what the sender derived for us. It is how a payment is proven to be
// ours before it enters the pool.
func (s *Signer) DerivedKey(ctx context.Context, d *Derivation) (*ec.PublicKey, error) {
	if d.OwnerHex != s.IdentityHex() {
		return nil, fmt.Errorf("bwallet: derivation belongs to %.12q, not this wallet", d.OwnerHex)
	}
	args, err := d.args()
	if err != nil {
		return nil, err
	}
	forSelf := true
	res, err := s.GetPublicKey(ctx, wallet.GetPublicKeyArgs{EncryptionArgs: args, ForSelf: &forSelf}, s.Originator)
	if err != nil {
		return nil, err
	}
	return res.PublicKey, nil
}

// DerivedScript is the P2PKH script for DerivedKey.
func (s *Signer) DerivedScript(ctx context.Context, d *Derivation) (*script.Script, error) {
	pub, err := s.DerivedKey(ctx, d)
	if err != nil {
		return nil, err
	}
	addr, err := script.NewAddressFromPublicKey(pub, s.Mainnet)
	if err != nil {
		return nil, err
	}
	return p2pkh.Lock(addr)
}

// DerivedUnlocker spends a P2PKH output locked to the key d derives, the
// same way the fund unlocker spends the fund key: the sighash goes to
// CreateSignature under the derivation and the key never leaves the
// deriver.
func (s *Signer) DerivedUnlocker(d *Derivation) transaction.UnlockingScriptTemplate {
	return &derivedUnlocker{w: s, d: d}
}

// OpenPool opens the spendable outputs under dir without an identity file,
// creating the directory and an empty file when there is none. It is what a
// directory backed by a wire wallet uses: the key lives in that wallet, the
// coin lives here.
// The profile names the legacy file it adopts.
func OpenPool(dir string, prof Profile) (*Pool, error) {
	if err := prof.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path, err := walletPath(dir, prof.LegacyPoolFile)
	if err != nil {
		return nil, err
	}
	p, err := LoadPool(path)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if err := p.Save(); err != nil {
			return nil, err
		}
	}
	return p, nil
}
