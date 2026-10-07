package commit

import (
	"crypto/sha256"
	"errors"
	"sync"
)

// ErrBlockMissing reports SegmentRoots.Root with a block below the highest
// one set that was never set.
var ErrBlockMissing = errors.New("commit: a block's subtree root was never set")

// SegmentRoots composes SegmentRoot over content cut into blocks of the same
// power-of-two number of segments, the last block possibly shorter, from
// each block's own SegmentRoot set in any order and from any goroutine: the
// byte-leaf root of an object whose blocks are hashed on several cores, as
// a CHIRP blob's segments are by a parallel chunker. It is RootOfSubtrees
// over the blocks in order, under the same condition, and for no blocks it
// is SHA-256 of the empty string, as SegmentRoot of no bytes is. It holds 32
// bytes a block. The zero value is ready to use.
type SegmentRoots struct {
	mu    sync.Mutex
	roots [][32]byte
	set   []bool
}

// Set records block i's root, SegmentRoot of the block's bytes. Setting a
// block twice keeps the later root.
func (s *SegmentRoots) Set(i int, root [32]byte) {
	if i < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.roots) <= i {
		s.roots = append(s.roots, [32]byte{})
		s.set = append(s.set, false)
	}
	s.roots[i], s.set[i] = root, true
}

// Blocks is one more than the highest block index set.
func (s *SegmentRoots) Blocks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.roots)
}

// Root is the byte-leaf root over blocks 0 to Blocks()-1, or
// ErrBlockMissing when one of them was not set.
func (s *SegmentRoots) Root() ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.roots) == 0 {
		return sha256.Sum256(nil), nil
	}
	for _, ok := range s.set {
		if !ok {
			return [32]byte{}, ErrBlockMissing
		}
	}
	return nodes(s.roots), nil
}
