// Package bwallet is an embedded BRC-100 wallet backend for an application
// that publishes on chain, and the Signer through which such an application
// signs with it or with any other wallet.Interface.
//
// It exists because the SDK's CompletedProtoWallet is a trap: it satisfies
// wallet.Interface and answers nil, nil to CreateAction, SignAction,
// ListOutputs and the rest, so a caller nil-derefs where it should have seen an
// error. Every method here that this backend does not implement returns
// ErrNotSupported with the method's name in it, and a test walks the interface
// by reflection to hold that line.
//
// Two files under one directory (mode 0700):
//
//	identity.json  {"wif": "..."}   the root key, mode 0600
//	wallet.json    the spendable outputs, mode 0600
//
// Both are written atomically (a temp file in the same directory, then rename)
// so a crash mid-write leaves the previous file rather than half of a new one.
//
// Create, Open, OpenIdentity and OpenPool take the application's Profile,
// which has no default. The funding key is derived INSIDE the wallet under
// the profile's fund protocol and key id with counterparty self, so only the
// holder of the root key can derive it and no *ec.PrivateKey ever leaves the
// wallet: spends are signed through CreateSignature with HashToDirectlySign,
// the same path go-sdk's own pushdrop.Unlocker uses. That is the whole point
// of the abstraction; a backend that hands its private key to a caller is
// not one.
//
// The coin this wallet holds is its own. Nothing else spends from it, because
// two tools drawing on one set of outputs double-spend each other.
package bwallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

const (
	identityFile = "identity.json"
	walletFile   = "wallet.json"
)

// fundCounterparty is self, stated rather than defaulted. The SDK defaults an
// uninitialised counterparty to self in GetPublicKey and to ANYONE in
// CreateSignature, so leaving it zero derives one key for the script and
// another for the signature.
var fundCounterparty = wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}

var (
	// ErrNotSupported is returned, wrapped with the method name, by every
	// wallet.Interface method this backend does not implement.
	ErrNotSupported = errors.New("bwallet: not supported by the embedded backend")
	// ErrNoChain is returned by GetHeight and GetHeaderForHeight when no
	// Chain is configured. The height source is a security choice, never a
	// default, so there is no fallback.
	ErrNoChain = errors.New("bwallet: no chain source configured")
	// ErrIdentityExists is returned by Create when the directory already
	// holds an identity. Overwriting one silently would orphan every token
	// and output it ever signed.
	ErrIdentityExists = errors.New("bwallet: identity already exists")
)

// Chain answers the tip height. The headers package's Client satisfies it.
type Chain interface {
	CurrentHeight(ctx context.Context) (uint32, error)
}

// HeaderSource is optionally implemented by a Chain that can also serve the
// 80 raw header bytes at a height, which is what GetHeaderForHeight returns.
type HeaderSource interface {
	HeaderForHeight(ctx context.Context, height uint32) ([]byte, error)
}

// Embedded is the wallet. The seven crypto methods are the SDK's ProtoWallet;
// everything else is here.
type Embedded struct {
	*wallet.ProtoWallet

	// Pool is the funding pool.
	Pool *Pool
	// Chain is optional; without it GetHeight answers ErrNoChain.
	Chain Chain
	// Mainnet selects the address prefix and the GetNetwork answer.
	Mainnet bool
	// Originator is the BRC-100 originator presented on every key call this
	// wallet makes for itself. Against a remote wallet an empty originator
	// defeats per-application permissioning for exactly the calls that spend.
	Originator string

	dir      string
	identity *ec.PublicKey
	profile  Profile
}

var _ wallet.Interface = (*Embedded)(nil)

type identityJSON struct {
	WIF string `json:"wif"`
}

// Create makes a new identity under dir and returns the opened wallet. It
// refuses to touch a directory that already holds one, and refuses an
// incomplete profile before touching the disk at all.
func Create(dir string, p Profile) (*Embedded, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	idPath := filepath.Join(dir, identityFile)
	if _, err := os.Stat(idPath); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrIdentityExists, idPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(identityJSON{WIF: key.Wif()})
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(idPath, raw, 0o600); err != nil {
		return nil, err
	}
	// The private key is not kept on the returned value: Open rebuilds the
	// wallet from the file so there is exactly one code path that holds it,
	// and that path hands it straight to the SDK's key deriver.
	e, err := Open(dir, p)
	if err != nil {
		return nil, err
	}
	// An empty pool file, so the directory is complete from the first
	// moment and its mode is set before anything is in it.
	if err := e.Pool.Save(); err != nil {
		return nil, err
	}
	return e, nil
}

// Open reads an existing identity and its pool from dir. A missing pool is an
// empty pool, not an error: an identity that has never been funded is a valid
// state. A missing identity is an error, never a silent Create.
func Open(dir string, p Profile) (*Embedded, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, fmt.Errorf("bwallet: open identity: %w", err)
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("bwallet: identity.json: %w", err)
	}
	key, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		return nil, fmt.Errorf("bwallet: identity.json wif: %w", err)
	}
	pw, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: key})
	if err != nil {
		return nil, err
	}
	path, err := walletPath(dir, p.LegacyPoolFile)
	if err != nil {
		return nil, err
	}
	pool, err := LoadPool(path)
	if err != nil {
		return nil, err
	}
	return &Embedded{
		ProtoWallet: pw,
		Pool:        pool,
		dir:         dir,
		identity:    key.PubKey(),
		profile:     p,
	}, nil
}

// walletPath answers the wallet file in dir, renaming a file left under the
// legacy name when there is one, because a wallet whose file is not found
// reads as an empty one, and an empty wallet is indistinguishable from spent
// coin. The rename is atomic and keeps no second copy, so the outputs cannot
// diverge between two files. A rename that fails is not fatal: the old file
// is read where it lies and the next save writes the new name.
func walletPath(dir, legacyWalletFile string) (string, error) {
	current := filepath.Join(dir, walletFile)
	if _, err := os.Stat(current); err == nil {
		return current, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	legacy := filepath.Join(dir, legacyWalletFile)
	if _, err := os.Stat(legacy); errors.Is(err, os.ErrNotExist) {
		return current, nil
	} else if err != nil {
		return "", err
	}
	if err := os.Rename(legacy, current); err != nil {
		return legacy, nil
	}
	return current, nil
}

// Dir is the directory the wallet was opened from.
func (e *Embedded) Dir() string { return e.dir }

// IdentityKey is the root public key: the subject of every record.
func (e *Embedded) IdentityKey() *ec.PublicKey { return e.identity }

// FundKey is the public half of the funding key. See Signer.
func (e *Embedded) FundKey() (*ec.PublicKey, error) { return e.Signer().FundKey() }

// FundScript is the P2PKH script for FundKey. See Signer.
func (e *Embedded) FundScript() (*script.Script, error) { return e.Signer().FundScript() }

// FundAddress is FundKey as an address. See Signer.
func (e *Embedded) FundAddress(mainnet bool) (string, error) { return e.Signer().FundAddress(mainnet) }

// FundUnlocker spends an output locked to FundScript. See Signer.
func (e *Embedded) FundUnlocker() transaction.UnlockingScriptTemplate {
	return e.Signer().FundUnlocker()
}
