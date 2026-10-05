package commit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type byteTreeVector struct {
	Leaves []struct {
		Hex string `json:"hex"`
	} `json:"leaves"`
	Trees []struct {
		Size    int    `json:"size"`
		RootHex string `json:"rootHex"`
		Paths   []struct {
			Index int `json:"index"`
			Steps []struct {
				HashHex string `json:"hashHex"`
				Left    bool   `json:"left"`
			} `json:"steps"`
		} `json:"paths"`
	} `json:"trees"`
	SegmentSize int `json:"segmentSize"`
	Objects     []struct {
		Size     int    `json:"size"`
		Segments int    `json:"segments"`
		RootHex  string `json:"rootHex"`
		Samples  []struct {
			Index       int      `json:"index"`
			PathLen     int      `json:"pathLen"`
			SiblingsHex []string `json:"siblingsHex"`
		} `json:"samples"`
	} `json:"objects"`
}

func h32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("hash %q: %v", s, err)
	}
	return [32]byte(b)
}

// stream is the vectors' content rule: SHA-256("bcommon vector content" ||
// uint32be(j)) for j = 0, 1, ... concatenated, cut to n bytes.
func stream(n int) []byte {
	out := make([]byte, 0, n+32)
	for j := uint32(0); len(out) < n; j++ {
		h := sha256.Sum256(binary.BigEndian.AppendUint32([]byte("bcommon vector content"), j))
		out = append(out, h[:]...)
	}
	return out[:n]
}

func TestByteTreeVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "bytetree-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v byteTreeVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var leaves [][]byte
	for _, l := range v.Leaves {
		b, err := hex.DecodeString(l.Hex)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, b)
	}
	if len(v.Trees) != len(leaves)+1 {
		t.Fatalf("%d trees over %d leaves", len(v.Trees), len(leaves))
	}
	for _, tr := range v.Trees {
		d := leaves[:tr.Size]
		root := h32(t, tr.RootHex)
		if got := RootOfBytes(d); got != root {
			t.Errorf("n=%d: root %x", tr.Size, got)
		}
		var b Builder
		for _, l := range d {
			b.AddBytes(l)
		}
		if got := b.Root(); got != root {
			t.Errorf("n=%d: builder root %x", tr.Size, got)
		}
		for _, want := range tr.Paths {
			got, err := ProveBytes(d, want.Index)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want.Steps) {
				t.Fatalf("n=%d i=%d: %d steps, want %d", tr.Size, want.Index, len(got), len(want.Steps))
			}
			for k, s := range want.Steps {
				if got[k].Hash != h32(t, s.HashHex) || got[k].Left != s.Left {
					t.Errorf("n=%d i=%d step %d differs", tr.Size, want.Index, k)
				}
			}
			if !VerifyBytes(d[want.Index], got, root) {
				t.Errorf("n=%d i=%d: VerifyBytes fails", tr.Size, want.Index)
			}
			if !VerifyAt(HashLeaf(d[want.Index]), uint64(want.Index), uint64(tr.Size), got.Siblings(), root) {
				t.Errorf("n=%d i=%d: VerifyAt fails", tr.Size, want.Index)
			}
		}
	}
	for _, o := range v.Objects {
		c := stream(o.Size)
		root := h32(t, o.RootHex)
		got, err := SegmentRoot(c, v.SegmentSize)
		if err != nil || got != root {
			t.Errorf("size %d: segment root %x, %v", o.Size, got, err)
		}
		w, _ := NewSegmentWriter(v.SegmentSize)
		for off := 0; off < len(c); off += 1000 {
			w.Write(c[off:min(off+1000, len(c))])
		}
		if w.Root() != root || w.Segments() != uint64(o.Segments) {
			t.Errorf("size %d: writer root %x over %d segments", o.Size, w.Root(), w.Segments())
		}
		for _, s := range o.Samples {
			seg := c[s.Index*v.SegmentSize : min((s.Index+1)*v.SegmentSize, len(c))]
			var sib [][32]byte
			for _, x := range s.SiblingsHex {
				sib = append(sib, h32(t, x))
			}
			n, err := PathLen(uint64(s.Index), uint64(o.Segments))
			if err != nil || n != s.PathLen || n != len(sib) {
				t.Errorf("size %d i=%d: PathLen %d, %v", o.Size, s.Index, n, err)
			}
			if !VerifyAt(HashLeaf(seg), uint64(s.Index), uint64(o.Segments), sib, root) {
				t.Errorf("size %d i=%d: VerifyAt fails", o.Size, s.Index)
			}
			if len(sib) > 0 {
				bad := append([][32]byte(nil), sib...)
				bad[0][0] ^= 1
				if VerifyAt(HashLeaf(seg), uint64(s.Index), uint64(o.Segments), bad, root) {
					t.Errorf("size %d i=%d: a changed sibling verifies", o.Size, s.Index)
				}
				if VerifyAt(HashLeaf(seg), uint64(s.Index), uint64(o.Segments), sib[:len(sib)-1], root) {
					t.Errorf("size %d i=%d: a short path verifies", o.Size, s.Index)
				}
			}
			if VerifyAt(HashLeaf(seg), uint64(s.Index), uint64(o.Segments), append(append([][32]byte(nil), sib...), root), root) {
				t.Errorf("size %d i=%d: a long path verifies", o.Size, s.Index)
			}
		}
	}
}

// The compact check agrees with the sided one, and PathLen with the path,
// for every leaf of every tree up to 70 leaves; any other index or size
// fails.
func TestVerifyAtAgrees(t *testing.T) {
	var leaves [][]byte
	for i := range 70 {
		leaves = append(leaves, []byte{byte(i), byte(i >> 8)})
	}
	for n := 1; n <= len(leaves); n++ {
		d := leaves[:n]
		root := RootOfBytes(d)
		for i := range n {
			p, _ := ProveBytes(d, i)
			if l, err := PathLen(uint64(i), uint64(n)); err != nil || l != len(p) {
				t.Fatalf("n=%d i=%d: PathLen %d want %d", n, i, l, len(p))
			}
			lh := HashLeaf(d[i])
			if !VerifyAt(lh, uint64(i), uint64(n), p.Siblings(), root) {
				t.Fatalf("n=%d i=%d: fails", n, i)
			}
			for _, j := range []int{i - 1, i + 1} {
				if j >= 0 && j < n && VerifyAt(lh, uint64(j), uint64(n), p.Siblings(), root) {
					t.Fatalf("n=%d i=%d: verifies at %d", n, i, j)
				}
			}
		}
	}
	if _, err := PathLen(3, 3); err != ErrIndex {
		t.Fatal("index at size accepted")
	}
	if VerifyAt([32]byte{}, 0, 0, nil, [32]byte{}) {
		t.Fatal("empty tree verified")
	}
	// A tree past 2^32 leaves is described on every platform.
	if l, err := PathLen(1<<33, 1<<33+1); err != nil || l != 1 {
		t.Fatalf("PathLen at 2^33: %d, %v", l, err)
	}
}

// Per-block subtree roots compose into the root and the path of the whole
// tree when every block but the last holds the same power of two of leaves.
func TestSubtrees(t *testing.T) {
	const block = 8
	for n := 1; n <= 41; n++ {
		var hs [][32]byte
		for i := range n {
			hs = append(hs, HashLeaf([]byte{byte(i)}))
		}
		var roots [][32]byte
		for o := 0; o < n; o += block {
			roots = append(roots, RootOfLeafHashes(hs[o:min(o+block, n)]))
		}
		root, err := RootOfSubtrees(roots)
		if err != nil || root != RootOfLeafHashes(hs) {
			t.Fatalf("n=%d: subtree root differs", n)
		}
		for i := range n {
			b := i / block
			in, _ := ProveLeafHashes(hs[b*block:min(b*block+block, n)], i%block)
			up, _ := ProveSubtrees(roots, b)
			whole, _ := ProveLeafHashes(hs, i)
			got := append(in, up...)
			if len(got) != len(whole) {
				t.Fatalf("n=%d i=%d: %d steps, want %d", n, i, len(got), len(whole))
			}
			for k := range got {
				if got[k] != whole[k] {
					t.Fatalf("n=%d i=%d: step %d differs", n, i, k)
				}
			}
		}
	}
	if _, err := RootOfSubtrees(nil); err != ErrNoSubtrees {
		t.Fatal("no subtrees accepted")
	}
	if _, err := ProveSubtrees([][32]byte{{}}, 1); err != ErrIndex {
		t.Fatal("index past the subtrees accepted")
	}
}

// A 32-byte leaf hashes and roots the same through either set of functions.
func TestBytesMatchFixed(t *testing.T) {
	var fixed [][32]byte
	var bs [][]byte
	for i := range 9 {
		l := sha256.Sum256([]byte{byte(i)})
		fixed = append(fixed, l)
		bs = append(bs, l[:])
		if HashLeaf(l[:]) != LeafHash(l) || RootOfBytes(bs) != Root(fixed) {
			t.Fatalf("n=%d differs", i+1)
		}
	}
	if _, err := NewSegmentWriter(0); err != ErrSegmentSize {
		t.Fatal("segment size 0 accepted")
	}
}
