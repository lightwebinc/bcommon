package commit

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// segmentRootsOf cuts c into blocks of block bytes, sets each block's
// SegmentRoot from its own goroutine in a random order, and composes them.
func segmentRootsOf(t *testing.T, c []byte, block, segment int, rng *rand.Rand) [32]byte {
	t.Helper()
	var s SegmentRoots
	var idx []int
	for i := 0; i*block < len(c); i++ {
		idx = append(idx, i)
	}
	rng.Shuffle(len(idx), func(a, b int) { idx[a], idx[b] = idx[b], idx[a] })
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Go(func() {
			r, err := SegmentRoot(c[i*block:min((i+1)*block, len(c))], segment)
			if err != nil {
				t.Error(err)
			}
			s.Set(i, r)
		})
	}
	wg.Wait()
	if s.Blocks() != len(idx) {
		t.Fatalf("%d blocks, want %d", s.Blocks(), len(idx))
	}
	r, err := s.Root()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Per-block roots composed in any order give the vector's segment roots,
// for blocks of any power of two of segments.
func TestSegmentRootsVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "bytetree-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v byteTreeVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, o := range v.Objects {
		c := stream(o.Size)
		for k := 0; k <= 6; k++ {
			if got := segmentRootsOf(t, c, v.SegmentSize<<k, v.SegmentSize, rng); got != h32(t, o.RootHex) {
				t.Errorf("size %d, blocks of %d segments: %x", o.Size, 1<<k, got)
			}
		}
	}
}

func TestSegmentRootsDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 200 {
		seg := 1 + rng.IntN(64)
		block := seg << rng.IntN(5)
		c := make([]byte, rng.IntN(40*block+2))
		rand.NewChaCha8([32]byte{byte(len(c))}).Read(c)
		want, _ := SegmentRoot(c, seg)
		if got := segmentRootsOf(t, c, block, seg, rng); got != want {
			t.Fatalf("size %d seg %d block %d: differs", len(c), seg, block)
		}
	}
}

func TestSegmentRootsMissing(t *testing.T) {
	var s SegmentRoots
	if r, err := s.Root(); err != nil || r != sha256.Sum256(nil) {
		t.Fatalf("empty: %x %v", r, err)
	}
	s.Set(-1, [32]byte{1})
	s.Set(2, [32]byte{1})
	s.Set(0, [32]byte{2})
	if _, err := s.Root(); !errors.Is(err, ErrBlockMissing) {
		t.Fatalf("gap: %v", err)
	}
	s.Set(1, [32]byte{3})
	if r, err := s.Root(); err != nil || r != NodeHash(NodeHash([32]byte{2}, [32]byte{3}), [32]byte{1}) {
		t.Fatalf("root %x %v", r, err)
	}
}
