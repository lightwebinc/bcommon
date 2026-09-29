// Package commit computes RFC 6962 Merkle roots and inclusion paths: the
// root a store reference commits to, one per named store, over the
// commitments (carrier txids) of that store's members.
//
// Domain separation is the point of RFC 6962's prefixes: a leaf hash and an
// interior node hash can never collide, so a member cannot be passed off as
// a subtree or the other way round. The prefixes are frozen at an
// application's first mint along with everything else a reader recomputes.
package commit

import (
	"crypto/sha256"
	"errors"
)

const (
	leafPrefix = 0x00
	nodePrefix = 0x01
)

// LeafHash is SHA-256(0x00 || leaf).
func LeafHash(leaf [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(leaf[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// NodeHash is SHA-256(0x01 || left || right).
func NodeHash(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Root is the RFC 6962 root over leaves in order. The empty tree's root is
// SHA-256 of the empty string, as the RFC defines it, so an empty store still
// has one well-defined commitment.
func Root(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return sha256.Sum256(nil)
	}
	return subtree(leaves)
}

// largestPowerOfTwoBelow returns the largest power of two strictly less than
// n, for n >= 2. RFC 6962 splits at this point so trees are reproducible for
// every length.
func largestPowerOfTwoBelow(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

func subtree(leaves [][32]byte) [32]byte {
	if len(leaves) == 1 {
		return LeafHash(leaves[0])
	}
	k := largestPowerOfTwoBelow(len(leaves))
	return NodeHash(subtree(leaves[:k]), subtree(leaves[k:]))
}

// Step is one sibling on an inclusion path.
type Step struct {
	Hash [32]byte
	// Left is true when the sibling sits to the LEFT of the node being
	// proven, so the verifier hashes NodeHash(sibling, current).
	Left bool
}

// Path is an inclusion path from a leaf to the root.
type Path []Step

// ErrIndex reports an index outside the leaf set.
var ErrIndex = errors.New("commit: index out of range")

// Prove returns the inclusion path for leaves[index].
func Prove(leaves [][32]byte, index int) (Path, error) {
	if index < 0 || index >= len(leaves) {
		return nil, ErrIndex
	}
	return prove(leaves, index), nil
}

func prove(leaves [][32]byte, index int) Path {
	if len(leaves) == 1 {
		return Path{}
	}
	k := largestPowerOfTwoBelow(len(leaves))
	if index < k {
		p := prove(leaves[:k], index)
		return append(p, Step{Hash: subtree(leaves[k:]), Left: false})
	}
	p := prove(leaves[k:], index-k)
	return append(p, Step{Hash: subtree(leaves[:k]), Left: true})
}

// Verify recomputes the root from leaf and path and compares it with root.
func Verify(leaf [32]byte, path Path, root [32]byte) bool {
	cur := LeafHash(leaf)
	for _, s := range path {
		if s.Left {
			cur = NodeHash(s.Hash, cur)
		} else {
			cur = NodeHash(cur, s.Hash)
		}
	}
	return cur == root
}
