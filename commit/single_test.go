package commit_test

import (
	"testing"

	"github.com/lightwebinc/bcommon/commit"
)

// A one-member store's root IS the leaf hash of its only member, so a reader
// holding the root and that member's commitment proves membership with no
// inclusion path at all. That is what makes a one-member store cheap to
// verify.
func TestSingleMemberRootIsTheLeafHash(t *testing.T) {
	var txid [32]byte
	for i := range txid {
		txid[i] = byte(i)
	}
	if commit.Root([][32]byte{txid}) != commit.LeafHash(txid) {
		t.Fatal("a one-member root is not its leaf hash")
	}
	// A different member gives a different root, so the root identifies the
	// member rather than merely counting it.
	var other [32]byte
	other[0] = 0xff
	if commit.Root([][32]byte{other}) == commit.LeafHash(txid) {
		t.Fatal("two different members share a root")
	}
	// Two members need a real path, and it verifies.
	leaves := [][32]byte{txid, other}
	root := commit.Root(leaves)
	p, err := commit.Prove(leaves, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Verify takes the raw leaf and hashes it itself, so a caller that
	// pre-hashes gets a silent false rather than a type error.
	if len(p) != 1 || !commit.Verify(txid, p, root) {
		t.Fatalf("path of %d step(s) did not verify", len(p))
	}
	if commit.Verify(commit.LeafHash(txid), p, root) {
		t.Fatal("a pre-hashed leaf verified; Verify must hash the leaf itself")
	}
}
