package chirp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
)

// store is a map of objects keyed by the hash they are asked for, which a
// test may serve as other bytes, and the subset written so far.
type store struct {
	mu      sync.Mutex
	objects map[[32]byte][]byte
	held    map[[32]byte]bool
}

func newStore(objects map[[32]byte][]byte) *store {
	return &store{objects: objects, held: map[[32]byte]bool{}}
}

func (s *store) all(h [32]byte) ([]byte, error) {
	if o, ok := s.objects[h]; ok {
		return o, nil
	}
	return nil, errors.New("none")
}

func (s *store) fetch(h [32]byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held[h] {
		return nil, errors.New("not held")
	}
	return s.all(h)
}

func (s *store) hold(h [32]byte) {
	s.mu.Lock()
	s.held[h] = true
	s.mu.Unlock()
}

func (s *store) keys(rng *rand.Rand) [][32]byte {
	var out [][32]byte
	for h := range s.objects {
		out = append(out, h)
	}
	// Map order is random but not seeded; sort by a seeded shuffle.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if string(out[j][:]) < string(out[i][:]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// arrive hands every object of s to an Incremental in a random order from
// goroutines goroutines, by Add or AddHashed, each stored before it is added, checking after
// every piece that nothing but the final verdict is ever reported. It
// returns the outcome after the last piece.
func arrive(t *testing.T, name string, root [32]byte, s *store, lim Limits, buffer, goroutines int, rng *rand.Rand, want *Closure, wantErr error) {
	t.Helper()
	inc := NewIncremental(root, s.fetch, lim, buffer)
	// Half the pieces may be held already, as after a restart.
	keys := s.keys(rng)
	restart := 0
	if rng.IntN(3) == 0 {
		restart = rng.IntN(len(keys) + 1)
		for _, h := range keys[:restart] {
			s.hold(h)
		}
		inc.Advance()
	}
	final := func(where string) {
		c, err := inc.Result()
		if c == nil && err == nil {
			return
		}
		if errText(err) != errText(wantErr) || !reflect.DeepEqual(c, want) {
			t.Fatalf("%s: %s: reported %v, Verify says %v", name, where, err, wantErr)
		}
	}
	next := make(chan [32]byte)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for h := range next {
				s.hold(h)
				// Half the pieces come with the hash their holder computed,
				// an honest one: the verdict must not change.
				if p := s.objects[h]; h[1]&1 == 0 {
					inc.AddHashed(sha256.Sum256(p), p)
				} else {
					inc.Add(p)
				}
				final("during arrival")
			}
		})
	}
	for _, h := range keys[restart:] {
		next <- h
	}
	close(next)
	wg.Wait()
	c, err := inc.Result()
	if errors.Is(wantErr, ErrMissing) {
		if c != nil || err != nil {
			t.Fatalf("%s: result %v with an object missing", name, err)
		}
		if _, err := inc.Check(); !errors.Is(err, ErrMissing) {
			t.Fatalf("%s: check %v, want missing", name, err)
		}
		if _, ok := inc.Waiting(); !ok {
			t.Fatalf("%s: not waiting", name)
		}
		return
	}
	if errText(err) != errText(wantErr) || !reflect.DeepEqual(c, want) {
		t.Fatalf("%s: incremental %v, Verify %v", name, err, wantErr)
	}
	if c2, err2 := inc.Check(); errText(err2) != errText(wantErr) || c2 != c {
		t.Fatalf("%s: check after the result differs", name)
	}
}

// Over the vectors' closures, Check with nothing added and the walk over
// pieces arriving in any order give Verify's verdict, code and text.
func TestIncrementalVectors(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for _, c := range load(t).Closures {
		objects := map[[32]byte][]byte{}
		for _, o := range c.Objects {
			objects[h32(t, o.HashHex)] = unhex(t, o.Hex)
		}
		root := h32(t, c.RootHashHex)
		lim := Limits{MaxReferences: c.MaxReferences, MaxLength: c.MaxLength}
		s := newStore(objects)
		want, wantErr := Verify(root, s.all, lim)
		if reason(wantErr) != c.Reason {
			t.Fatalf("%s: Verify %v", c.Name, wantErr)
		}
		got, err := NewIncremental(root, s.all, lim, 0).Check()
		if errText(err) != errText(wantErr) || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: check %v, want %v", c.Name, err, wantErr)
		}
		for k := range 20 {
			// Verify again over the store as the incremental sees it once
			// everything is held, which is the same map.
			arrive(t, fmt.Sprintf("%s/%d", c.Name, k), root, newStore(objects), lim, []int{1, 64, 0}[k%3], 1+k%4, rng, want, wantErr)
		}
	}
}

// genTree makes a random closure: a root over branches and blobs, nested
// up to past MaxDepth, with repeated objects, and with faults: a lying
// reference length, a content hash that is wrong, an object served as
// other bytes or not at all, a root that is not profile 1's construction.
type genTree struct {
	rng     *rand.Rand
	objects map[[32]byte][]byte
	content []byte
	made    []Child
	deep    int // nest a chain of branches to this depth
}

func (g *genTree) child(depth int) Child {
	r := g.rng
	if len(g.made) > 0 && r.IntN(8) == 0 {
		// A repeat: an object already made, its content again.
		c := g.made[r.IntN(len(g.made))]
		if c.Kind == ChildBlob {
			if b, ok := g.objects[c.Hash]; ok && uint64(len(b)) == c.Length {
				g.content = append(g.content, b...)
				return c
			}
		}
	}
	chain := depth < g.deep
	if chain || (depth < 18 && r.IntN(3) == 0) {
		n := 1 + r.IntN(4)
		if chain {
			n = 1
		}
		var kids []Child
		var sum uint64
		for range n {
			k := g.child(depth + 1)
			kids = append(kids, k)
			sum += k.Length
		}
		b, err := EncodeBranch(Branch{Length: sum, Children: kids})
		if err != nil {
			panic(err)
		}
		h := sha256.Sum256(b)
		g.objects[h] = b
		c := Child{Kind: ChildBranch, Length: sum, Hash: h}
		if r.IntN(40) == 0 {
			c.Length++
		}
		g.made = append(g.made, c)
		return c
	}
	b := make([]byte, r.IntN(200))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	h := sha256.Sum256(b)
	g.objects[h] = b
	g.content = append(g.content, b...)
	c := Child{Kind: ChildBlob, Length: uint64(len(b)), Hash: h}
	if r.IntN(60) == 0 {
		c.Length++
	}
	g.made = append(g.made, c)
	return c
}

func (g *genTree) root() [32]byte {
	r := g.rng
	if r.IntN(10) == 0 {
		g.deep = 14 + r.IntN(4)
	}
	n := r.IntN(6)
	var kids []Child
	var sum uint64
	for range n {
		k := g.child(2)
		kids = append(kids, k)
		sum += k.Length
	}
	profile := uint16(2)
	if r.IntN(4) == 0 {
		profile = Profile1
	}
	ch := sha256.Sum256(g.content)
	if r.IntN(20) == 0 {
		ch[0] ^= 1
	}
	if sum == 0 {
		kids, ch = nil, sha256.Sum256(nil)
	}
	b, err := EncodeRoot(Root{Profile: profile, Length: sum, ContentHash: ch, Children: kids})
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	g.objects[h] = b
	// Faults of the store: an object served as other bytes, or missing.
	for k := range g.objects {
		switch r.IntN(80) {
		case 0:
			o := append([]byte(nil), g.objects[k]...)
			if len(o) > 0 {
				o[r.IntN(len(o))] ^= 0x80
			} else {
				o = []byte{1}
			}
			g.objects[k] = o
		case 1:
			delete(g.objects, k)
		}
	}
	return h
}

// Random closures, faults included, verified by Verify and by the
// incremental walk over random arrival orders, buffers and goroutines:
// the same closure or the same refusal, word for word, and never another
// refusal reported first.
func TestIncrementalDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	n := 600
	if testing.Short() {
		n = 150
	}
	counts := map[string]int{}
	for k := range n {
		g := &genTree{rng: rng, objects: map[[32]byte][]byte{}}
		root := g.root()
		lim := Limits{}
		switch rng.IntN(10) {
		case 0:
			lim.MaxReferences = 1 + rng.IntN(20)
		case 1:
			lim.MaxLength = uint64(rng.IntN(2000))
		}
		s := newStore(g.objects)
		want, wantErr := Verify(root, s.all, lim)
		counts[reason(wantErr)]++
		got, err := NewIncremental(root, s.all, lim, 0).Check()
		if errText(err) != errText(wantErr) || !reflect.DeepEqual(got, want) {
			t.Fatalf("tree %d: check %v, Verify %v", k, err, wantErr)
		}
		if len(g.objects) == 0 {
			continue
		}
		arrive(t, fmt.Sprintf("tree %d", k), root, newStore(g.objects), lim, []int{1, 300, 0}[k%3], 1+k%5, rng, want, wantErr)
	}
	for _, r := range []string{"", "missing", "hash", "length", "depth", "references", "too-large", "content-hash", "canonical"} {
		if counts[r] == 0 {
			t.Errorf("no tree refused %q: %v", r, counts)
		}
	}
}

// A canonical closure built from content, profile 1, at the blob
// boundaries, arriving in any order.
func TestIncrementalContent(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 77} {
		content := stream(n)
		b, err := Build(content, nil)
		if err != nil {
			t.Fatal(err)
		}
		objects := map[[32]byte][]byte{b.RootHash: b.Root}
		for _, bl := range b.Blobs {
			objects[sha256.Sum256(bl)] = bl
		}
		want, err := Verify(b.RootHash, newStore(objects).all, Limits{})
		if err != nil || !want.Canonical {
			t.Fatal(err)
		}
		for k := range 4 {
			arrive(t, fmt.Sprintf("size %d/%d", n, k), b.RootHash, newStore(objects), Limits{}, []int{1, ChunkSize, 0, 2 * ChunkSize}[k], 1+k, rng, want, nil)
		}
	}
}

// A piece that is no object of the closure is ignored, and the buffer is
// never exceeded but by the piece the walk waits for.
func TestIncrementalBuffer(t *testing.T) {
	content := stream(3*ChunkSize + 5)
	b, _ := Build(content, nil)
	s := newStore(map[[32]byte][]byte{b.RootHash: b.Root})
	for _, bl := range b.Blobs {
		s.objects[sha256.Sum256(bl)] = bl
	}
	inc := NewIncremental(b.RootHash, s.fetch, Limits{}, 100)
	inc.Add([]byte("not an object of this closure, kept: it fits"))
	inc.Add(make([]byte, 1000))
	for i := len(b.Blobs) - 1; i >= 0; i-- {
		inc.Add(b.Blobs[i])
		inc.mu.Lock()
		if inc.keptLen > 100 {
			t.Fatalf("kept %d bytes", inc.keptLen)
		}
		inc.mu.Unlock()
		if w, ok := inc.Waiting(); !ok || w != b.RootHash {
			t.Fatalf("waits for %x, %v", w, ok)
		}
	}
	s.hold(sha256.Sum256(b.Blobs[0]))
	inc.Add(b.Root)
	if w, ok := inc.Waiting(); !ok || w != sha256.Sum256(b.Blobs[1]) {
		t.Fatalf("waits for %x", w)
	}
	for _, bl := range b.Blobs {
		s.hold(sha256.Sum256(bl))
	}
	inc.Advance()
	if c, err := inc.Result(); err != nil || c == nil || !c.Canonical {
		t.Fatalf("result %v", err)
	}
	if _, ok := inc.Waiting(); ok {
		t.Fatal("waiting once verified")
	}
}

// A wrong hash handed to AddHashed never passes bytes other than the
// content the root commits to: a node is hashed again, and a blob's bytes
// meet its length and the content hash.
func TestIncrementalAddHashedWrong(t *testing.T) {
	content := stream(3*ChunkSize + 77)
	b, err := Build(content, nil)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[[32]byte][]byte{b.RootHash: b.Root}
	for _, br := range b.Branches {
		objects[sha256.Sum256(br)] = br
	}
	for _, bl := range b.Blobs {
		objects[sha256.Sum256(bl)] = bl
	}
	none := func([32]byte) ([]byte, error) { return nil, errors.New("none") }
	feed := func(sub map[[32]byte][]byte) (*Closure, error) {
		inc := NewIncremental(b.RootHash, none, Limits{}, 0)
		for h, o := range objects {
			if p, ok := sub[h]; ok {
				inc.AddHashed(h, p)
			} else {
				inc.AddHashed(h, o)
			}
		}
		return inc.Check()
	}
	if c, err := feed(nil); err != nil || !c.Canonical {
		t.Fatalf("honest: %v", err)
	}
	// Another blob's bytes, or flipped bytes, under a blob's hash.
	h1 := sha256.Sum256(b.Blobs[1])
	flip := append([]byte(nil), b.Blobs[1]...)
	flip[0] ^= 1
	for name, p := range map[string][]byte{"other blob": b.Blobs[0], "flipped": flip, "short": b.Blobs[1][1:]} {
		if _, err := feed(map[[32]byte][]byte{h1: p}); !errors.Is(err, ErrContentHash) && !errors.Is(err, ErrLength) {
			t.Fatalf("%s: %v, want a content hash or length refusal", name, err)
		}
	}
	// Forged nodes under the root's and a branch's hash.
	forged := append([]byte(nil), b.Root...)
	forged[len(forged)-1] ^= 1
	if _, err := feed(map[[32]byte][]byte{b.RootHash: forged}); !errors.Is(err, ErrHash) {
		t.Fatalf("forged root: %v", err)
	}
	if len(b.Branches) > 0 {
		hb := sha256.Sum256(b.Branches[0])
		if _, err := feed(map[[32]byte][]byte{hb: b.Root}); !errors.Is(err, ErrHash) {
			t.Fatalf("forged branch: %v", err)
		}
	}
	// A right piece under a wrong hash is a stray: the walk waits for it.
	inc := NewIncremental(b.RootHash, none, Limits{}, 0)
	for h, o := range objects {
		if h == h1 {
			h[0] ^= 1
		}
		inc.AddHashed(h, o)
	}
	if _, err := inc.Check(); !errors.Is(err, ErrMissing) {
		t.Fatalf("misfiled: %v", err)
	}
}
