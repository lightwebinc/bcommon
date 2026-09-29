package bwallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// NewIdentityFile writes a fresh root key to path at 0600 and returns its
// public key. It refuses an existing file. It is how a rotation's successor
// comes to exist: a second identity file beside the primary, promoted to
// identity.json once the successor has published under it.
func NewIdentityFile(path string) (*ec.PublicKey, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrIdentityExists, path)
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
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		return nil, err
	}
	return key.PubKey(), nil
}

// OpenIdentity opens the identity file at path as a wallet that shares pool
// with the primary. Two keys share one pool across a rotation: the
// predecessor still spends the fee outputs locked to its fund key and
// unlocks the last token it locked, while the successor signs everything
// new. Which wallet may spend a given pool output is decided by its locking
// script, never by which file was opened first. The profile names the
// opened wallet's fund derivation; the primary's is the one to pass, so that
// both sides of a rotation derive their fund keys the same way.
func OpenIdentity(path string, pool *Pool, p Profile) (*Embedded, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bwallet: open identity: %w", err)
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("bwallet: %s: %w", path, err)
	}
	key, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		return nil, fmt.Errorf("bwallet: %s wif: %w", path, err)
	}
	pw, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: key})
	if err != nil {
		return nil, err
	}
	return &Embedded{ProtoWallet: pw, Pool: pool, identity: key.PubKey(), profile: p}, nil
}
