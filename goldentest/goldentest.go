// Package goldentest holds the helpers a package's tests share when they
// check themselves against a committed test vector: a fixed test key, hex and
// transaction parsing that fail the test rather than return an error, and a
// chain tracker that knows only the roots the test hands it.
//
// It carries no vector of its own. Each application keeps its vector, and
// the code that finds and reads it, in its own tree: a path resolved from
// this package's source file would point into the module cache once the
// package is imported from there.
//
// It is a test helper that happens to live outside a _test file so that
// several packages can share it; no binary should import it.
package goldentest

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
)

// Fill is a 32-byte array of one repeated byte.
func Fill(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}

// FixedKey is the TEST key vectors are generated from, fixed so that
// regenerating one reproduces it byte for byte. It must never be used for
// anything real: it is 32 bytes of 0x42.
func FixedKey() *ec.PrivateKey {
	seed := Fill(0x42)
	k, _ := ec.PrivateKeyFromBytes(seed[:])
	return k
}

// Hex decodes or fails the test.
func Hex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Tx parses a transaction from hex or fails the test.
func Tx(t testing.TB, s string) *transaction.Transaction {
	t.Helper()
	tx, err := transaction.NewTransactionFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// Tracker is a chain tracker that knows exactly the roots it was given, so a
// test proves a BUMP against a root it chose rather than against anything
// live. Any other height, and a nil root, answers (false, nil), as a header
// client answers a height it holds no header for: a test of an unproven
// parent then meets "not proven", not an error that would take a different
// refusal path.
type Tracker struct {
	Roots map[uint32]string
	Tip   uint32
}

var _ chaintracker.ChainTracker = (*Tracker)(nil)

func (s *Tracker) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	want, ok := s.Roots[height]
	if !ok || root == nil {
		return false, nil
	}
	return want == root.String(), nil
}

func (s *Tracker) CurrentHeight(context.Context) (uint32, error) { return s.Tip, nil }
