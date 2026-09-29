// Package pushdrop is the key derivation an application locks its PushDrop
// outputs under, and the tagged PushDrop those outputs carry: a leading tag
// field, the application's own fields, and the wallet's signature over them.
//
// Every derivation here is counterparty Anyone with forSelf=true. Measured at
// go-sdk v1.5.2, that is the only setting under which a reader holding just
// the identity key can recompute the locking key AND the embedded signature
// verifies under it, so a reader checks an output without asking its
// producer anything.
//
// A Derivation is frozen at an application's first mint: the protocol and key
// id are hashed into every derived key (BRC-43), so changing either orphans
// every output ever locked under it. The application owns its Derivation and
// its tags; nothing here names one.
package pushdrop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// Derivation is one key under an application's BRC-43 protocol: the protocol
// (security level and name) and the key id within it, which together give
// the invoice number "<level>-<name>-<key id>".
type Derivation struct {
	Protocol wallet.Protocol
	KeyID    string
}

// ErrDerivation is what Validate refuses a derivation with.
var ErrDerivation = errors.New("pushdrop: invalid derivation")

// The SDK's bounds, in bytes as it measures them.
const (
	maxKeyID = 800
	minName  = 5
	maxName  = 400
	// A linkage revelation names the protocol it reveals after this prefix,
	// so the SDK lets such a name run longer.
	linkagePrefix  = "specific linkage revelation "
	maxLinkageName = 430
)

// Validate applies the BRC-43 rules go-sdk v1.5.2's key deriver applies to a
// protocol and key id, in the SDK's order, so an application can refuse a
// derivation that will never produce a key when it builds one rather than at
// its first mint. The SDK stays the authority: no method here calls
// Validate, and each derivation still meets the SDK's own check.
//
// The name is measured as the SDK measures it, trimmed and lower-cased, so
// " Sample " passes here because the SDK derives it as "sample".
func (d Derivation) Validate() error {
	if lvl := d.Protocol.SecurityLevel; lvl < 0 || lvl > 2 {
		return fmt.Errorf("%w: security level %d is not 0, 1 or 2", ErrDerivation, lvl)
	}
	if len(d.KeyID) > maxKeyID {
		return fmt.Errorf("%w: key id is %d bytes, over %d", ErrDerivation, len(d.KeyID), maxKeyID)
	}
	if len(d.KeyID) < 1 {
		return fmt.Errorf("%w: empty key id", ErrDerivation)
	}
	name := strings.ToLower(strings.TrimSpace(d.Protocol.Protocol))
	if len(name) > maxName {
		if !strings.HasPrefix(name, linkagePrefix) {
			return fmt.Errorf("%w: protocol name is %d bytes, over %d", ErrDerivation, len(name), maxName)
		}
		if len(name) > maxLinkageName {
			return fmt.Errorf("%w: linkage revelation protocol name is %d bytes, over %d", ErrDerivation, len(name), maxLinkageName)
		}
	}
	if len(name) < minName {
		return fmt.Errorf("%w: protocol name %q is under %d characters", ErrDerivation, name, minName)
	}
	if strings.Contains(name, "  ") {
		return fmt.Errorf("%w: protocol name %q has consecutive spaces", ErrDerivation, name)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != ' ' {
			return fmt.Errorf("%w: protocol name %q has %q; only letters, digits and spaces", ErrDerivation, name, r)
		}
	}
	if strings.HasSuffix(name, " protocol") {
		return fmt.Errorf("%w: protocol name %q ends in \" protocol\"", ErrDerivation, name)
	}
	return nil
}

// Anyone is the counterparty every derivation here uses. A zero-value
// Counterparty is not the same thing (the SDK's GetPublicKey and
// CreateSignature default it differently) and is never passed.
func Anyone() wallet.Counterparty {
	return wallet.Counterparty{Type: wallet.CounterpartyTypeAnyone}
}

// ExpectedLockingKey is the reader's side of the derivation: from the anyone
// root, identity's key under d. It equals what identity's own wallet produces
// with counterparty Anyone and forSelf=true, which is the key Lock locks to.
func (d Derivation) ExpectedLockingKey(identity *ec.PublicKey) (*ec.PublicKey, error) {
	if identity == nil {
		return nil, errors.New("pushdrop: nil identity key")
	}
	return wallet.NewKeyDeriver(nil).DerivePublicKey(d.Protocol, d.KeyID,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: identity}, false)
}

// Lock builds a lock-before PushDrop of fields through the wallet, locked to
// the wallet's key under d with counterparty Anyone and forSelf=true. With
// sign, the wallet's signature over the fields concatenated follows them as
// one more field; without it the output carries the fields alone.
//
// The fields slice is freshly allocated on every call because the SDK
// appends the signature into whatever capacity it is handed, and a reused
// buffer would carry one output's signature into the next. The field bytes
// are copied too, so the SDK is never handed a buffer the caller still
// holds.
func (d Derivation) Lock(ctx context.Context, w wallet.Interface, originator string, fields [][]byte, sign bool) (*script.Script, error) {
	if w == nil {
		return nil, errors.New("pushdrop: nil wallet")
	}
	fresh := make([][]byte, len(fields))
	for i, f := range fields {
		fresh[i] = append([]byte(nil), f...)
	}
	pd := &sdkpushdrop.PushDrop{Wallet: w, Originator: originator}
	return pd.Lock(ctx, fresh, d.Protocol, d.KeyID, Anyone(), true, sign, sdkpushdrop.LockBefore)
}

// Unlocker returns the template that spends an output Lock locked under d,
// through the wallet, signing all outputs. The SDK's pushdrop.Unlocker does
// not satisfy transaction.UnlockingScriptTemplate on its own (its Sign takes
// an int and its EstimateLength takes nothing), so Template adapts it.
func (d Derivation) Unlocker(ctx context.Context, w wallet.Interface, originator string) Template {
	pd := &sdkpushdrop.PushDrop{Wallet: w, Originator: originator}
	return Template{U: pd.Unlock(ctx, d.Protocol, d.KeyID, Anyone(), wallet.SignOutputsAll, false)}
}
