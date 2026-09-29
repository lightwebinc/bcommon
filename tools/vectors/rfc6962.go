package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/bits"
)

// The Merkle Tree Hash and audit path of RFC 6962 §2.1, written from the
// RFC's definitions. The inputs d(i) are byte strings of any length; the
// library's leaves are 32-byte commitments, which is one case of that.
//
//	MTH({})     = SHA-256()
//	MTH({d(0)}) = SHA-256(0x00 || d(0))
//	MTH(D[n])   = SHA-256(0x01 || MTH(D[0:k]) || MTH(D[k:n])), k the largest power of two < n

func leafHash(d []byte) [32]byte {
	return sha256.Sum256(append([]byte{0x00}, d...))
}

func interior(left, right [32]byte) [32]byte {
	b := make([]byte, 0, 65)
	b = append(b, 0x01)
	b = append(b, left[:]...)
	b = append(b, right[:]...)
	return sha256.Sum256(b)
}

// split is k for n > 1: the largest power of two strictly less than n, which
// is one bit below the highest bit of n-1.
func split(n int) int {
	return 1 << (bits.Len(uint(n-1)) - 1)
}

func mth(d [][]byte) [32]byte {
	switch n := len(d); n {
	case 0:
		return sha256.Sum256(nil)
	case 1:
		return leafHash(d[0])
	default:
		k := split(n)
		return interior(mth(d[:k]), mth(d[k:]))
	}
}

// step is one sibling on an audit path. Left is true when the sibling is the
// left input of the interior hash, so a verifier computes
// SHA-256(0x01 || sibling || current).
type step struct {
	HashHex string `json:"hashHex"`
	Left    bool   `json:"left"`
}

type pathStep struct {
	hash [32]byte
	left bool
}

// auditPath is PATH(m, D[n]) of RFC 6962 §2.1.1, leaf first:
//
//	PATH(0, {d(0)}) = {}
//	PATH(m, D[n])   = PATH(m, D[0:k]) : MTH(D[k:n])     for m < k
//	PATH(m, D[n])   = PATH(m - k, D[k:n]) : MTH(D[0:k]) for m >= k
//
// A sibling taken from D[k:n] is to the right of the node being proven, and
// one from D[0:k] to the left.
func auditPath(m int, d [][]byte) []pathStep {
	if len(d) == 1 {
		return nil
	}
	k := split(len(d))
	if m < k {
		return append(auditPath(m, d[:k]), pathStep{hash: mth(d[k:]), left: false})
	}
	return append(auditPath(m-k, d[k:]), pathStep{hash: mth(d[:k]), left: true})
}

// verifyPath is the inclusion check of RFC 9162 §2.1.3.2, a bit-by-bit
// algorithm that shares nothing with the recursion above. Every generated
// path must pass it, and every step's side must be the side it implies,
// before a vector is written.
func verifyPath(index, size int, leaf []byte, path []pathStep, root [32]byte) error {
	if index >= size {
		return fmt.Errorf("index %d outside a tree of %d", index, size)
	}
	fn, sn := index, size-1
	r := leafHash(leaf)
	for i, p := range path {
		if sn == 0 {
			return fmt.Errorf("step %d: path longer than the tree", i)
		}
		if fn&1 == 1 || fn == sn {
			if !p.left {
				return fmt.Errorf("step %d: sibling should be on the left", i)
			}
			r = interior(p.hash, r)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			if p.left {
				return fmt.Errorf("step %d: sibling should be on the right", i)
			}
			r = interior(r, p.hash)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return fmt.Errorf("path shorter than the tree")
	}
	if r != root {
		return fmt.Errorf("path does not reach the root")
	}
	return nil
}

type inclusion struct {
	Index int    `json:"index"`
	Steps []step `json:"steps"`
}

type tree struct {
	Size    int         `json:"size"`
	RootHex string      `json:"rootHex"`
	Paths   []inclusion `json:"paths"`
}

// rfc6962Vector holds the roots of the first n fixed leaves for n = 1..17,
// which runs one leaf past the full tree of sixteen, and the audit path of
// every leaf in each tree.
type rfc6962Vector struct {
	LeavesHex []string `json:"leavesHex"`
	Trees     []tree   `json:"trees"`
}

func rfc6962Families() ([]family, error) {
	const max = 17
	// Leaf i is SHA-256 of the one byte i: fixed, 32 bytes like a
	// commitment, and no two alike.
	leaves := make([][]byte, max)
	var v rfc6962Vector
	for i := range leaves {
		h := sha256.Sum256([]byte{byte(i)})
		leaves[i] = h[:]
		v.LeavesHex = append(v.LeavesHex, hex.EncodeToString(leaves[i]))
	}
	for n := 1; n <= max; n++ {
		d := leaves[:n]
		root := mth(d)
		t := tree{Size: n, RootHex: hex.EncodeToString(root[:])}
		for m := 0; m < n; m++ {
			p := auditPath(m, d)
			if err := verifyPath(m, n, d[m], p, root); err != nil {
				return nil, fmt.Errorf("rfc6962: n=%d m=%d: %w", n, m, err)
			}
			in := inclusion{Index: m, Steps: []step{}}
			for _, s := range p {
				in.Steps = append(in.Steps, step{HashHex: hex.EncodeToString(s.hash[:]), Left: s.left})
			}
			t.Paths = append(t.Paths, in)
		}
		v.Trees = append(v.Trees, t)
	}
	return []family{{"rfc6962-v1.json", v}}, nil
}
