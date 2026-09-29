package commit_test

import (
	"crypto/sha256"
	"fmt"

	"github.com/lightwebinc/bcommon/commit"
)

// leaves returns n 32-byte leaves, SHA-256 of the single byte i for leaf i:
// the first n leaves of testdata/vectors/rfc6962-v1.json.
func leaves(n int) [][32]byte {
	out := make([][32]byte, n)
	for i := range out {
		out[i] = sha256.Sum256([]byte{byte(i)})
	}
	return out
}

// Root is the RFC 6962 Merkle Tree Hash over the leaves in order. The root
// printed here is the independent generator's root for the same five leaves.
func ExampleRoot() {
	fmt.Printf("%x\n", commit.Root(leaves(5)))
	// The empty tree's root is SHA-256 of the empty string.
	fmt.Printf("%x\n", commit.Root(nil))
	// Output:
	// 6b313b611b40676b9e1dfd70c4503f2379f88f0f1c2740fb7e1cacc32c113465
	// e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
}

// Prove gives the inclusion path of one leaf, and Verify recomputes the root
// from the leaf and the path. A path verifies only its own leaf.
func ExampleProve() {
	ls := leaves(5)
	root := commit.Root(ls)

	path, err := commit.Prove(ls, 2)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, s := range path {
		fmt.Printf("sibling %x... left=%t\n", s.Hash[:4], s.Left)
	}
	fmt.Println("leaf 2 verifies:", commit.Verify(ls[2], path, root))
	fmt.Println("leaf 3 on leaf 2's path verifies:", commit.Verify(ls[3], path, root))

	_, err = commit.Prove(ls, 5)
	fmt.Println(err)
	// Output:
	// sibling 36e4970e... left=false
	// sibling 604d540f... left=true
	// sibling 12d24297... left=false
	// leaf 2 verifies: true
	// leaf 3 on leaf 2's path verifies: false
	// commit: index out of range
}
