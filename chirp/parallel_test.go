package chirp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
)

// chunkParallel runs content through a ParallelChunker in pieces of random
// sizes and returns the closure and the blob hashes emit saw, by index.
func chunkParallel(t *testing.T, content []byte, ext []Extension, workers int, rng *rand.Rand) (Built, [][32]byte) {
	t.Helper()
	var mu sync.Mutex
	emitted := map[int][32]byte{}
	c := NewParallelChunker(workers, func(i int, ref Child, blob []byte) error {
		if sha256.Sum256(blob) != ref.Hash || uint64(len(blob)) != ref.Length || ref.Kind != ChildBlob {
			t.Errorf("blob %d: reference does not describe the blob", i)
		}
		mu.Lock()
		defer mu.Unlock()
		if _, dup := emitted[i]; dup {
			t.Errorf("blob %d emitted twice", i)
		}
		emitted[i] = ref.Hash
		return nil
	})
	for off := 0; off < len(content); {
		n := min(1+rng.IntN(3*ChunkSize/2), len(content)-off)
		if k, err := c.Write(content[off : off+n]); err != nil || k != n {
			t.Fatalf("write: %d, %v", k, err)
		}
		off += n
	}
	b, err := c.Finish(ext)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][32]byte, len(emitted))
	for i, h := range emitted {
		if i >= len(out) {
			t.Fatalf("blob index %d of %d", i, len(emitted))
		}
		out[i] = h
	}
	return b, out
}

func sameBuilt(t *testing.T, name string, got, want Built) {
	t.Helper()
	if !bytes.Equal(got.Root, want.Root) || got.RootHash != want.RootHash || len(got.Branches) != len(want.Branches) {
		t.Fatalf("%s: root differs", name)
	}
	for i := range want.Branches {
		if !bytes.Equal(got.Branches[i], want.Branches[i]) {
			t.Fatalf("%s: branch %d differs", name, i)
		}
	}
}

// The parallel builders produce the vectors' closures byte for byte.
func TestParallelVectors(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, c := range load(t).Contents {
		var content []byte
		for _, p := range c.Parts {
			if p.Hex != "" {
				content = append(content, unhex(t, p.Hex)...)
			} else {
				content = append(content, stream(p.Stream)...)
			}
		}
		for _, workers := range []int{0, 1, 3, 8} {
			b, err := BuildParallel(content, nil, workers)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			if hex.EncodeToString(b.Root) != c.RootHex || b.RootHash != h32(t, c.RootHashHex) || len(b.Blobs) != len(c.BlobHashesHex) {
				t.Errorf("%s: BuildParallel root differs", c.Name)
			}
			for i, bl := range b.Blobs {
				if sha256.Sum256(bl) != h32(t, c.BlobHashesHex[i]) {
					t.Errorf("%s: blob %d differs", c.Name, i)
				}
			}
			pb, emitted := chunkParallel(t, content, nil, workers, rng)
			if hex.EncodeToString(pb.Root) != c.RootHex || pb.Blobs != nil || len(emitted) != len(c.BlobHashesHex) {
				t.Errorf("%s: ParallelChunker root differs", c.Name)
			}
			for i, h := range emitted {
				if h != h32(t, c.BlobHashesHex[i]) {
					t.Errorf("%s: emitted blob %d differs", c.Name, i)
				}
			}
		}
	}
	for _, g := range load(t).Golden {
		var ext []Extension
		for _, e := range g.Extensions {
			ext = append(ext, Extension{Type: e.Type, Value: unhex(t, e.ValueHex)})
		}
		b, err := BuildParallel(unhex(t, g.ContentHex), ext, 2)
		if err != nil || hex.EncodeToString(b.Root) != g.RootHex {
			t.Errorf("%s: golden root differs: %v", g.Name, err)
		}
		pb, _ := chunkParallel(t, unhex(t, g.ContentHex), ext, 2, rng)
		if hex.EncodeToString(pb.Root) != g.RootHex {
			t.Errorf("%s: golden chunker root differs", g.Name)
		}
	}
}

// Build, Chunker, BuildParallel and ParallelChunker agree at every size
// around the blob boundaries and at random ones.
func TestParallelDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	sizes := []int{0, 1, 2, 31, ChunkSize - 1, ChunkSize, ChunkSize + 1, 2*ChunkSize - 1, 2 * ChunkSize, 2*ChunkSize + 1, 7*ChunkSize + 12345}
	for range 6 {
		sizes = append(sizes, rng.IntN(9*ChunkSize))
	}
	if testing.Short() {
		sizes = sizes[:8]
	}
	ext := []Extension{{Type: MediaType, Value: []byte("application/octet-stream")}}
	for _, n := range sizes {
		content := make([]byte, n)
		rand.NewChaCha8([32]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}).Read(content)
		want, err := Build(content, ext)
		if err != nil {
			t.Fatal(err)
		}
		ch := NewChunker(func([]byte) error { return nil })
		ch.Write(content)
		seq, err := ch.Finish(ext)
		if err != nil {
			t.Fatal(err)
		}
		sameBuilt(t, "chunker", seq, want)
		workers := 1 + rng.IntN(9)
		got, err := BuildParallel(content, ext, workers)
		if err != nil {
			t.Fatal(err)
		}
		sameBuilt(t, "BuildParallel", got, want)
		if !reflect.DeepEqual(got.Blobs, want.Blobs) {
			t.Fatalf("size %d: blobs differ", n)
		}
		pc, emitted := chunkParallel(t, content, ext, workers, rng)
		sameBuilt(t, "ParallelChunker", pc, want)
		for i, h := range emitted {
			if h != sha256.Sum256(want.Blobs[i]) {
				t.Fatalf("size %d: emitted blob %d differs", n, i)
			}
		}
	}
}

func TestParallelEmitError(t *testing.T) {
	boom := errors.New("boom")
	var mu sync.Mutex
	calls := 0
	c := NewParallelChunker(2, func(i int, _ Child, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if i == 1 {
			return boom
		}
		return nil
	})
	content := make([]byte, 20*ChunkSize)
	var err error
	for off := 0; off < len(content) && err == nil; off += ChunkSize / 2 {
		_, err = c.Write(content[off : off+ChunkSize/2])
	}
	if _, ferr := c.Finish(nil); !errors.Is(ferr, boom) {
		t.Fatalf("finish: %v", ferr)
	}
	if _, werr := c.Write([]byte{1}); !errors.Is(werr, ErrFinished) {
		t.Fatalf("write after finish: %v", werr)
	}
	if _, ferr := c.Finish(nil); !errors.Is(ferr, ErrFinished) {
		t.Fatalf("finish twice: %v", ferr)
	}
	c.Abort()
}

func TestParallelAbort(t *testing.T) {
	c := NewParallelChunker(3, func(int, Child, []byte) error { return nil })
	c.Write(make([]byte, 3*ChunkSize+5))
	c.Abort()
	c.Abort()
	if _, err := c.Finish(nil); !errors.Is(err, ErrFinished) {
		t.Fatalf("finish after abort: %v", err)
	}
}

var genBase = stream(ChunkSize)

// genBlob is blob i of the large test content: cheap to make, distinct per
// index.
func genBlob(buf []byte, i uint64) []byte {
	copy(buf, genBase)
	binary.LittleEndian.PutUint64(buf, i)
	return buf
}

// Over 1 GiB, where profile 1 groups blobs into branches: the parallel
// chunker against the sequential one, then Verify and Incremental over the
// closure with arrival in a random order.
func TestParallelOverGiB(t *testing.T) {
	if testing.Short() {
		t.Skip("hashes 2 GiB")
	}
	const blobs = 300
	last := 12345
	size := (blobs-1)*ChunkSize + last
	piece := func(i int) []byte {
		n := ChunkSize
		if i == blobs-1 {
			n = last
		}
		return genBlob(make([]byte, n), uint64(i))
	}
	seq := NewChunker(func([]byte) error { return nil })
	byHash := map[[32]byte]int{}
	par := NewParallelChunker(4, func(i int, ref Child, _ []byte) error { return nil })
	for i := range blobs {
		b := piece(i)
		seq.Write(b)
		par.Write(b)
		byHash[sha256.Sum256(b)] = i
	}
	want, err := seq.Finish(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := par.Finish(nil)
	if err != nil {
		t.Fatal(err)
	}
	sameBuilt(t, "over 1 GiB", got, want)
	if len(want.Branches) != 2 {
		t.Fatalf("%d branches", len(want.Branches))
	}
	nodes := map[[32]byte][]byte{want.RootHash: want.Root}
	for _, br := range want.Branches {
		nodes[sha256.Sum256(br)] = br
	}
	var mu sync.Mutex
	stored := map[[32]byte]bool{}
	fetch := func(h [32]byte) ([]byte, error) {
		mu.Lock()
		ok := stored[h]
		mu.Unlock()
		if !ok {
			return nil, errors.New("not held")
		}
		if n, ok := nodes[h]; ok {
			return n, nil
		}
		return piece(byHash[h]), nil
	}
	all := func(h [32]byte) ([]byte, error) {
		if n, ok := nodes[h]; ok {
			return n, nil
		}
		if i, ok := byHash[h]; ok {
			return piece(i), nil
		}
		return nil, errors.New("none")
	}
	vc, err := Verify(want.RootHash, all, Limits{})
	if err != nil || !vc.Canonical || len(vc.Blobs) != blobs {
		t.Fatalf("verify: %v", err)
	}
	if size != int(vc.Root.Length) {
		t.Fatalf("length %d", vc.Root.Length)
	}
	// Pieces arrive nearly in order on 4 goroutines, nodes first or last.
	var hashes [][32]byte
	for h := range nodes {
		hashes = append(hashes, h)
	}
	order := make([][32]byte, 0, blobs+3)
	for i := range blobs {
		order = append(order, sha256.Sum256(piece(i)))
	}
	order = append(order, hashes...)
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range order {
		j := min(len(order)-1, i+rng.IntN(6))
		order[i], order[j] = order[j], order[i]
	}
	inc := NewIncremental(want.RootHash, fetch, Limits{}, 0)
	next := make(chan [32]byte)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for h := range next {
				b, _ := all(h)
				mu.Lock()
				stored[h] = true
				mu.Unlock()
				inc.Add(b)
			}
		})
	}
	for _, h := range order {
		next <- h
	}
	close(next)
	wg.Wait()
	ic, ierr := inc.Result()
	if ierr != nil || !reflect.DeepEqual(ic, vc) {
		t.Fatalf("incremental: %v", ierr)
	}
}
