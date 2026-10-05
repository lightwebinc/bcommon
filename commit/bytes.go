package commit

import (
	"crypto/sha256"
	"errors"
)

// RFC 6962 defines the Merkle Tree Hash over leaves that are byte strings of
// any length. Root, Prove and Verify take 32-byte leaves, the commitments a
// store holds; the functions below take the general case: leaves of any
// length (an application's key-map entry, a segment of content), leaves
// already hashed, and subtrees already reduced to their roots. The prefixes
// are the same 0x00 and 0x01, so a root over 32-byte leaves is the same
// whichever set of functions computes it.

// HashLeaf is SHA-256(0x00 || leaf) for a leaf of any length. For a 32-byte
// leaf it equals LeafHash.
func HashLeaf(leaf []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(leaf)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// RootOfBytes is the RFC 6962 root over leaves of any length, in order. The
// empty tree's root is SHA-256 of the empty string.
func RootOfBytes(leaves [][]byte) [32]byte {
	return RootOfLeafHashes(hashAll(leaves))
}

// RootOfLeafHashes is the RFC 6962 root over leaves already hashed with
// HashLeaf (or LeafHash), in order: what a holder that kept only the leaf
// hashes recomputes. The empty tree's root is SHA-256 of the empty string.
func RootOfLeafHashes(hashes [][32]byte) [32]byte {
	if len(hashes) == 0 {
		return sha256.Sum256(nil)
	}
	return nodes(hashes)
}

// RootOfSubtrees is the RFC 6962 split applied to subtree roots, each taken
// as a node and never hashed again as a leaf. It equals the root over the
// subtrees' leaves only when every subtree but the last covers the same
// power-of-two number of leaves and the last covers at most that many: an
// object cut into fixed blocks of 2^k leaves, one root per block. Then
// RFC 6962's split at the largest power of two below the leaf count always
// falls on a block boundary, and the two descriptions are one tree. No
// subtrees is ErrNoSubtrees, since the empty tree has no subtree roots.
func RootOfSubtrees(roots [][32]byte) ([32]byte, error) {
	if len(roots) == 0 {
		return [32]byte{}, ErrNoSubtrees
	}
	return nodes(roots), nil
}

// ErrNoSubtrees reports RootOfSubtrees over no subtrees.
var ErrNoSubtrees = errors.New("commit: no subtree roots")

func hashAll(leaves [][]byte) [][32]byte {
	out := make([][32]byte, len(leaves))
	for i, l := range leaves {
		out[i] = HashLeaf(l)
	}
	return out
}

// nodes is the RFC 6962 recursion over values that are already nodes.
func nodes(h [][32]byte) [32]byte {
	if len(h) == 1 {
		return h[0]
	}
	k := largestPowerOfTwoBelow(len(h))
	return NodeHash(nodes(h[:k]), nodes(h[k:]))
}

func proveNodes(h [][32]byte, index int) Path {
	if len(h) == 1 {
		return Path{}
	}
	k := largestPowerOfTwoBelow(len(h))
	if index < k {
		return append(proveNodes(h[:k], index), Step{Hash: nodes(h[k:]), Left: false})
	}
	return append(proveNodes(h[k:], index-k), Step{Hash: nodes(h[:k]), Left: true})
}

// ProveBytes returns the RFC 6962 audit path of leaves[index], leaf to root.
func ProveBytes(leaves [][]byte, index int) (Path, error) {
	return ProveLeafHashes(hashAll(leaves), index)
}

// ProveLeafHashes returns the audit path of the leaf at index from the leaf
// hashes alone.
func ProveLeafHashes(hashes [][32]byte, index int) (Path, error) {
	if index < 0 || index >= len(hashes) {
		return nil, ErrIndex
	}
	return proveNodes(hashes, index), nil
}

// ProveSubtrees returns the path of subtree root roots[index] up to
// RootOfSubtrees(roots), under the same condition on the subtrees. A leaf's
// whole audit path is its path within its subtree followed by this one.
func ProveSubtrees(roots [][32]byte, index int) (Path, error) {
	if index < 0 || index >= len(roots) {
		return nil, ErrIndex
	}
	return proveNodes(roots, index), nil
}

// VerifyBytes recomputes the root from a leaf of any length and its path and
// compares it with root.
func VerifyBytes(leaf []byte, path Path, root [32]byte) bool {
	cur := HashLeaf(leaf)
	for _, s := range path {
		if s.Left {
			cur = NodeHash(s.Hash, cur)
		} else {
			cur = NodeHash(cur, s.Hash)
		}
	}
	return cur == root
}

// Siblings returns a path's hashes without their sides: the compact form a
// proof sends when the verifier knows the index and the leaf count, from
// which every side follows (VerifyAt).
func (p Path) Siblings() [][32]byte {
	out := make([][32]byte, len(p))
	for i, s := range p {
		out[i] = s.Hash
	}
	return out
}

// PathLen is the length of the RFC 6962 audit path of leaf index in a tree
// of size leaves: what a verifier checks a compact path's length against
// before it hashes anything. Index and size are 64-bit so that a tree of
// more than 2^31 leaves is described on every platform.
func PathLen(index, size uint64) (int, error) {
	if index >= size {
		return 0, ErrIndex
	}
	n := 0
	fn, sn := index, size-1
	for sn != 0 {
		if fn&1 == 1 || fn == sn {
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		}
		fn >>= 1
		sn >>= 1
		n++
	}
	return n, nil
}

// VerifyAt checks a compact audit path: leafHash (HashLeaf of the leaf) at
// index in a tree of size leaves, with siblings leaf to root and no sides,
// against root. Every side follows from index and size, by the inclusion
// check of RFC 9162 section 2.1.3.2. A path longer or shorter than PathLen
// fails.
func VerifyAt(leafHash [32]byte, index, size uint64, siblings [][32]byte, root [32]byte) bool {
	if index >= size {
		return false
	}
	fn, sn := index, size-1
	r := leafHash
	for _, p := range siblings {
		if sn == 0 {
			return false
		}
		if fn&1 == 1 || fn == sn {
			r = NodeHash(p, r)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			r = NodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && r == root
}

// Builder computes an RFC 6962 root over leaves added one at a time, holding
// one hash per set bit of the count: a root over more leaves than fit in
// memory, such as the segments of a large object read as a stream.
type Builder struct {
	// stack holds the roots of the complete subtrees so far, largest first;
	// sizes[i] is the leaf count under stack[i], a power of two.
	stack [][32]byte
	sizes []uint64
	n     uint64
}

// Add appends a leaf already hashed with HashLeaf.
func (b *Builder) Add(leafHash [32]byte) {
	b.stack = append(b.stack, leafHash)
	b.sizes = append(b.sizes, 1)
	b.n++
	for len(b.stack) >= 2 && b.sizes[len(b.sizes)-1] == b.sizes[len(b.sizes)-2] {
		i := len(b.stack) - 2
		b.stack[i] = NodeHash(b.stack[i], b.stack[i+1])
		b.sizes[i] *= 2
		b.stack = b.stack[:i+1]
		b.sizes = b.sizes[:i+1]
	}
}

// AddBytes appends a leaf of any length.
func (b *Builder) AddBytes(leaf []byte) { b.Add(HashLeaf(leaf)) }

// Size is the number of leaves added.
func (b *Builder) Size() uint64 { return b.n }

// Root is the RFC 6962 root over the leaves added so far: the complete
// subtrees folded right to left, which is the RFC's split at the largest
// power of two at every level. With no leaves it is SHA-256 of the empty
// string. Adding more leaves afterwards is allowed.
func (b *Builder) Root() [32]byte {
	if len(b.stack) == 0 {
		return sha256.Sum256(nil)
	}
	r := b.stack[len(b.stack)-1]
	for i := len(b.stack) - 2; i >= 0; i-- {
		r = NodeHash(b.stack[i], r)
	}
	return r
}

// ErrSegmentSize reports a segment size that is not positive.
var ErrSegmentSize = errors.New("commit: segment size must be positive")

// SegmentRoot is the RFC 6962 root over content cut into segments of size
// bytes from offset 0, the last holding the remainder unpadded. Content of
// no bytes has no segments and its root is SHA-256 of the empty string.
func SegmentRoot(content []byte, size int) ([32]byte, error) {
	w, err := NewSegmentWriter(size)
	if err != nil {
		return [32]byte{}, err
	}
	w.Write(content)
	return w.Root(), nil
}

// SegmentWriter is SegmentRoot over content written in pieces of any size.
// It holds at most one segment.
type SegmentWriter struct {
	size int
	buf  []byte
	b    Builder
}

// NewSegmentWriter returns a writer that cuts what it is given into
// segments of size bytes.
func NewSegmentWriter(size int) (*SegmentWriter, error) {
	if size <= 0 {
		return nil, ErrSegmentSize
	}
	return &SegmentWriter{size: size, buf: make([]byte, 0, size)}, nil
}

// Write takes content in order. It never fails.
func (w *SegmentWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		k := min(w.size-len(w.buf), len(p))
		w.buf = append(w.buf, p[:k]...)
		p = p[k:]
		if len(w.buf) == w.size {
			w.b.AddBytes(w.buf)
			w.buf = w.buf[:0]
		}
	}
	return n, nil
}

// Segments is the number of segments the content written so far makes,
// counting a partial last one.
func (w *SegmentWriter) Segments() uint64 {
	if len(w.buf) > 0 {
		return w.b.Size() + 1
	}
	return w.b.Size()
}

// Root is the root over the content written so far, a partial last segment
// included as it is. Writing more afterwards is allowed.
func (w *SegmentWriter) Root() [32]byte {
	if len(w.buf) == 0 {
		return w.b.Root()
	}
	b := Builder{stack: append([][32]byte(nil), w.b.stack...), sizes: append([]uint64(nil), w.b.sizes...), n: w.b.n}
	b.AddBytes(w.buf)
	return b.Root()
}
