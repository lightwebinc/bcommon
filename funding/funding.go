// Package funding keeps a producer's funding tree between runs, and the
// transactions it published before they mined.
//
// A carrier is never mined, but it still spends a real output: one output of
// a funding tree, a mined transaction of equal-valued outputs set aside for
// carriers. Tree is what an application writes down about that tree so the
// next carrier knows which output is still unspent and can rebuild the tree
// to spend it.
//
// A transaction spent before it mines has to carry its ancestry, or no one
// can verify its spender. KeepBEEF, Rebuild and BumpHex are the three pure
// steps of that bookkeeping: what to keep while a transaction is unproven,
// how to rebuild it from what was kept, and the proof's form once it mines.
package funding

import (
	"encoding/hex"
	"errors"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// Tree is a funding tree and how much of it is unspent. Its JSON form is
// part of an application's state file, so a tag or the field order changing
// changes every state written after it.
type Tree struct {
	// IdentityKeyHex is the identity whose derived key locks the outputs. A
	// successor cannot spend a predecessor's tree, so a rotation implies a
	// new tree.
	IdentityKeyHex string `json:"identityKey,omitempty"`
	Txid           string `json:"txid"`
	RawHex         string `json:"rawHex"`
	// BumpHex is the tree's proof; empty until it mined.
	BumpHex string `json:"bumpHex,omitempty"`
	// BeefHex is the tree's full BEEF while it is unmined, so a carrier
	// spending it can carry the tree's ancestry. Cleared once BumpHex is set,
	// because a proven tree needs nothing behind it.
	BeefHex string `json:"beefHex,omitempty"`
	Height  uint32 `json:"height,omitempty"`
	// Sats is the value of each funding output.
	Sats uint64 `json:"sats"`
	// Count is the number of funding outputs (indices 0..Count-1).
	Count uint32 `json:"count"`
	// Next is the first unspent funding output index.
	Next uint32 `json:"next"`
	// Funder is how this tree was paid for, in the application's own words.
	// Nothing here reads it. An application that can fund a tree either from
	// a wallet or from coin of its own needs it to know whether a wallet
	// holds the outputs, so it is kept with the tree it describes.
	Funder string `json:"funder,omitempty"`
}

// Remaining reports how many carriers this tree can still fund.
func (f *Tree) Remaining() uint32 {
	if f == nil || f.Next >= f.Count {
		return 0
	}
	return f.Count - f.Next
}

// BumpHex is a proof's hex, or empty for no proof.
func BumpHex(mp *transaction.MerklePath) string {
	if mp == nil {
		return ""
	}
	return mp.Hex()
}

// Rebuild rebuilds a transaction an application published: from its raw
// bytes and proof when it has mined, or from the BEEF kept while it had not,
// which carries its unproven ancestry. A proof wins over a kept BEEF, which
// is then not read at all: a proven transaction needs no ancestry. Each is
// walked by package guard before the SDK parses it.
func Rebuild(rawHex, bumpHex, beefHex string) (*transaction.Transaction, error) {
	if bumpHex == "" && beefHex != "" {
		b, err := hex.DecodeString(beefHex)
		if err != nil {
			return nil, err
		}
		_, tx, _, err := guard.ParseBEEF(b, guard.DefaultBound)
		if err != nil {
			return nil, err
		}
		if tx == nil {
			return nil, errors.New("kept BEEF names no subject transaction")
		}
		return tx, nil
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if bumpHex != "" {
		bump, err := hex.DecodeString(bumpHex)
		if err != nil {
			return nil, err
		}
		if tx.MerklePath, err = guard.ParseBUMP(bump, guard.DefaultBound); err != nil {
			return nil, err
		}
	}
	return tx, nil
}

// KeepBEEF is the BEEF to keep for a transaction published before it mined,
// or empty once it has a proof.
func KeepBEEF(tx *transaction.Transaction, mp *transaction.MerklePath) (string, error) {
	if mp != nil {
		return "", nil
	}
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
