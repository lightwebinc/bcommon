package chirp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"sync"
)

// DefaultBuffer is the bytes an Incremental keeps of pieces that arrived
// ahead of its walk when NewIncremental is given 0: sixteen blobs.
const DefaultBuffer = 16 * ChunkSize

// Incremental is Verify run as the closure's pieces arrive instead of at
// the end. Each piece handed to Add is hashed there (AddHashed takes the
// caller's hash instead), on the caller's
// goroutine, so pieces arriving on several goroutines are hashed on several
// cores; the walk then moves as far as the pieces on hand allow, and the
// content hash advances over the blobs in order as they come. When the last
// piece lands, what is left of the check is the canonical construction over
// the blob references, which hashes only nodes.
//
// The walk is Verify's: the same checks of the root, the same depth-first
// walk in child order with the same checks of each reference, the same
// content hash and canonical checks, and the same Closure. Where Verify
// would report missing, the walk waits instead: a refusal is reported when
// the walk reaches the object it concerns, which is after every object
// before it in Verify's order has arrived and passed. A piece that arrives
// early is never judged early, so the first refusal reported is always the
// one Verify reports over the same objects, and the result once complete is
// Verify's, byte for byte, with the same error sentinel. Check reports
// missing as Verify does, for the object the walk waits for.
//
// Memory is bounded as Verify's is (the walk's state, under
// Limits.MaxReferences), plus the copies of pieces that arrived ahead of the
// walk, at most the buffer given to NewIncremental (DefaultBuffer when 0)
// besides the one piece the walk waits for. A piece not kept, or one the
// closure repeats, is fetched when the walk reaches it and checked as Verify
// checks a fetched object, so fetch must serve every piece already handed
// to Add, as a store the pieces are written to before Add does. An
// Incremental made after a restart, with nothing handed to Add yet, walks
// what fetch serves on its first Advance, Add or Check and continues from
// there as pieces arrive: only the pieces held before the restart are read
// back, and each is checked once.
//
// An Incremental is safe for concurrent use. Its methods never return
// before the walk has moved as far as the pieces given allow, except Add
// and Advance while another goroutine is walking: that goroutine then
// carries the walk on to the new piece before it stops.
type Incremental struct {
	rootHash [32]byte
	fetch    Fetch
	lim      Limits

	// mu guards the kept pieces, what the walk waits for and the outcome.
	mu       sync.Mutex
	kept     map[[32]byte]keptPiece
	keptLen  int
	maxKept  int
	want     [32]byte
	wanting  bool
	kick     uint64
	result   *Closure
	refusal  error
	finished bool

	// walking is held by the one goroutine moving the walk; the fields
	// below it are the walk's state.
	walking  sync.Mutex
	started  bool
	root     Root
	stack    []frame
	ready    bool // the reference at the top of the stack passed its first checks
	refs     int
	seen     map[[32]byte]bool
	branches map[[32]byte]Branch
	ancestry map[[32]byte]bool
	content  hash.Hash
	out      *Closure
}

// keptPiece is a piece kept ahead of the walk; vouched when its hash was
// handed to AddHashed rather than computed by Add.
type keptPiece struct {
	b       []byte
	vouched bool
}

// frame is one node's child list being walked; depth is the number of
// nodes above the children, the root counted.
type frame struct {
	list   []Child
	next   int
	depth  int
	branch [32]byte
	root   bool
}

// NewIncremental returns an Incremental for the closure of the root named
// rootHash, with Verify's limits and a fetch for the pieces it does not keep
// (see Incremental). buffer bounds the bytes of pieces kept ahead of the
// walk; DefaultBuffer when 0 or less.
func NewIncremental(rootHash [32]byte, fetch Fetch, lim Limits, buffer int) *Incremental {
	if lim.MaxReferences <= 0 {
		lim.MaxReferences = DefaultMaxReferences
	}
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Incremental{rootHash: rootHash, fetch: fetch, lim: lim, kept: map[[32]byte]keptPiece{}, maxKept: buffer}
}

// Add hands over a piece of the closure, or any object: it is hashed here,
// a copy kept when the walk waits for it or the buffer has room, and the
// walk moved on. It returns the piece's hash, the object it is. A piece
// that is no object of the closure is kept until the buffer is needed and
// otherwise ignored.
func (v *Incremental) Add(piece []byte) [32]byte {
	h := sha256.Sum256(piece)
	v.add(h, piece, false)
	return h
}

// AddHashed is Add for a caller that has already hashed the piece: hash
// must be sha256 of piece, and the caller vouches for it. It saves the one
// hash of the piece that Add makes, which is most of Add's cost for a blob.
//
// The trust is narrow. A root or branch handed here is hashed again when
// the walk takes it (nodes are small), so the closure's structure, every
// reference's hash and length and the content hash it commits to, is
// always checked as Verify checks it. Only a blob's own hash is taken on
// the caller's word; its length is still checked against its reference,
// and its bytes still go into the content hash, which must equal the
// root's. So with a correct hash the walk and its verdict are Add's, the
// same as Verify's, byte for byte. With a wrong one, no bytes but the
// content the root commits to can ever pass: a blob whose bytes differ
// from the content in its place fails the content hash. What a wrong hash
// can do is place a piece under a reference that its bytes do not hash to
// and, where those bytes are the committed content anyway, pass a closure
// whose blob references Verify would refuse with ErrHash; and a piece kept
// under a hash that no reference names is ignored, as any stray piece is.
// A caller that cannot vouch for the hash, or must refuse such malformed
// closures, calls Add.
func (v *Incremental) AddHashed(hash [32]byte, piece []byte) {
	v.add(hash, piece, true)
}

func (v *Incremental) add(h [32]byte, piece []byte, vouched bool) {
	v.mu.Lock()
	if _, have := v.kept[h]; !v.finished && !have && ((v.wanting && v.want == h) || v.keptLen+len(piece) <= v.maxKept) {
		v.kept[h] = keptPiece{b: append(make([]byte, 0, len(piece)), piece...), vouched: vouched}
		v.keptLen += len(piece)
	}
	v.kick++
	v.mu.Unlock()
	v.pump()
}

// Advance moves the walk as far as the pieces kept and fetch allow: after a
// restart, or after a piece Add did not keep has been stored.
func (v *Incremental) Advance() {
	v.mu.Lock()
	v.kick++
	v.mu.Unlock()
	v.pump()
}

// pump moves the walk unless another goroutine is moving it, in which case
// that goroutine sees the kick and walks once more before it stops.
func (v *Incremental) pump() {
	for {
		if !v.walking.TryLock() {
			return
		}
		v.mu.Lock()
		seen := v.kick
		v.mu.Unlock()
		v.walk(false)
		v.walking.Unlock()
		v.mu.Lock()
		again := v.kick != seen && !v.finished
		v.mu.Unlock()
		if !again {
			return
		}
	}
}

// Result is the outcome so far: the verified closure, or the refusal, or
// neither while the walk waits for a piece.
func (v *Incremental) Result() (*Closure, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.result, v.refusal
}

// Waiting returns the object the walk waits for, and false when it waits
// for nothing: finished, or not started.
func (v *Incremental) Waiting() ([32]byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.want, v.wanting && !v.finished
}

// Check is Verify's answer over the pieces on hand: it moves the walk as
// far as it goes and returns the verified closure or the refusal, missing
// (ErrMissing) when the walk waits for an object that fetch does not serve.
// Missing is not final: the walk resumes when the piece arrives.
func (v *Incremental) Check() (*Closure, error) {
	var err error
	for {
		v.mu.Lock()
		seen := v.kick
		v.mu.Unlock()
		v.walking.Lock()
		err = v.walk(true)
		v.walking.Unlock()
		// A piece added while this walk held the walk, which its Add could
		// not carry on to, is walked to before Check answers.
		v.mu.Lock()
		again := v.kick != seen && !v.finished
		v.mu.Unlock()
		if !again {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return v.Result()
}

// obtain returns an object's bytes and how far they are known to hash to
// h: hashedHere (a kept piece hashed by Add), vouched (a kept piece whose
// hash came with it to AddHashed), or neither (fetched); or false when
// there are none yet. In final mode the absence is ErrMissing, as Verify's
// get.
func (v *Incremental) obtain(h [32]byte, final bool) (b []byte, hashedHere, vouched, got bool, err error) {
	v.mu.Lock()
	v.want, v.wanting = h, true
	if k, ok := v.kept[h]; ok {
		delete(v.kept, h)
		v.keptLen -= len(k.b)
		v.wanting = false
		v.mu.Unlock()
		return k.b, !k.vouched, k.vouched, true, nil
	}
	v.mu.Unlock()
	b, err = v.fetch(h)
	if err != nil {
		if final {
			return nil, false, false, false, fmt.Errorf("%w: %x: %v", ErrMissing, h, err)
		}
		return nil, false, false, false, nil
	}
	// A copy kept while fetch was asked is not needed any more.
	v.mu.Lock()
	if k, ok := v.kept[h]; ok {
		delete(v.kept, h)
		v.keptLen -= len(k.b)
	}
	v.wanting = false
	v.mu.Unlock()
	return b, false, false, true, nil
}

func (v *Incremental) settle(c *Closure, err error) {
	v.mu.Lock()
	v.result, v.refusal, v.finished = c, err, true
	v.wanting = false
	v.kept, v.keptLen = nil, 0
	v.mu.Unlock()
}

// walk moves the walk until it waits or ends; it returns ErrMissing in
// final mode when it waits, and nil otherwise (the outcome is in Result).
// The caller holds walking.
func (v *Incremental) walk(final bool) error {
	v.mu.Lock()
	done := v.finished
	v.mu.Unlock()
	if done {
		return nil
	}
	if !v.started {
		ok, err := v.startRoot(final)
		if err != nil || !ok {
			return err
		}
	}
	for len(v.stack) > 0 {
		f := &v.stack[len(v.stack)-1]
		if f.next == len(f.list) {
			if !f.root {
				delete(v.ancestry, f.branch)
			}
			v.stack = v.stack[:len(v.stack)-1]
			continue
		}
		c := f.list[f.next]
		if !v.ready {
			v.refs++
			if v.refs > v.lim.MaxReferences {
				v.settle(nil, ErrReferences)
				return nil
			}
			if f.depth+1 > MaxDepth {
				v.settle(nil, ErrDepth)
				return nil
			}
			v.ready = true
		}
		var (
			ok  bool
			err error
		)
		if c.Kind == ChildBranch {
			ok, err = v.branch(c, f.depth, final)
		} else {
			ok, err = v.blob(c, final)
		}
		if err != nil {
			if final && isMissing(err) {
				return err
			}
			v.settle(nil, err)
			return nil
		}
		if !ok {
			return nil
		}
	}
	v.finish()
	return nil
}

func isMissing(err error) bool { return errors.Is(err, ErrMissing) }

func (v *Incremental) startRoot(final bool) (bool, error) {
	rb, pre, _, ok, err := v.obtain(v.rootHash, final)
	if err != nil || !ok {
		return false, err
	}
	if !pre && sha256.Sum256(rb) != v.rootHash {
		v.settle(nil, fmt.Errorf("%w: the root", ErrHash))
		return false, nil
	}
	root, err := DecodeRoot(rb)
	if err != nil {
		v.settle(nil, err)
		return false, nil
	}
	if v.lim.MaxLength > 0 && root.Length > v.lim.MaxLength {
		v.settle(nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, root.Length))
		return false, nil
	}
	v.started = true
	v.root = root
	v.seen = map[[32]byte]bool{v.rootHash: true}
	v.branches = map[[32]byte]Branch{}
	v.ancestry = map[[32]byte]bool{v.rootHash: true}
	v.content = sha256.New()
	v.out = &Closure{Root: root, RootHash: v.rootHash, Objects: [][32]byte{v.rootHash}}
	v.stack = []frame{{list: root.Children, depth: 1, root: true}}
	return true, nil
}

// branch takes one branch reference whose first checks passed; it returns
// false while the branch has not arrived.
func (v *Incremental) branch(c Child, depth int, final bool) (bool, error) {
	if v.ancestry[c.Hash] {
		return false, ErrCycle
	}
	br, ok := v.branches[c.Hash]
	if !ok {
		b, pre, _, got, err := v.obtain(c.Hash, final)
		if err != nil || !got {
			return false, err
		}
		if !pre && sha256.Sum256(b) != c.Hash {
			return false, fmt.Errorf("%w: branch %x", ErrHash, c.Hash)
		}
		if br, err = DecodeBranch(b); err != nil {
			return false, err
		}
		v.branches[c.Hash] = br
	}
	if br.Length != c.Length {
		return false, fmt.Errorf("%w: branch %x covers %d bytes, its reference %d", ErrLength, c.Hash, br.Length, c.Length)
	}
	v.note(c.Hash)
	v.ancestry[c.Hash] = true
	v.stack[len(v.stack)-1].next++
	v.ready = false
	v.stack = append(v.stack, frame{list: br.Children, depth: depth + 1, branch: c.Hash})
	return true, nil
}

func (v *Incremental) blob(c Child, final bool) (bool, error) {
	b, pre, vouched, got, err := v.obtain(c.Hash, final)
	if err != nil || !got {
		return false, err
	}
	if uint64(len(b)) != c.Length {
		return false, fmt.Errorf("%w: blob %x is %d bytes, its reference %d", ErrLength, c.Hash, len(b), c.Length)
	}
	// A vouched blob's hash is the caller's (see AddHashed); its bytes are
	// still bound by its length here and by the content hash at the end.
	if !pre && !vouched && sha256.Sum256(b) != c.Hash {
		return false, fmt.Errorf("%w: blob %x", ErrHash, c.Hash)
	}
	v.content.Write(b)
	v.out.Blobs = append(v.out.Blobs, c)
	v.note(c.Hash)
	v.stack[len(v.stack)-1].next++
	v.ready = false
	return true, nil
}

func (v *Incremental) note(h [32]byte) {
	if !v.seen[h] {
		v.seen[h] = true
		v.out.Objects = append(v.out.Objects, h)
	}
}

// finish runs Verify's last two checks once every blob is in.
func (v *Incremental) finish() {
	if [32]byte(v.content.Sum(nil)) != v.root.ContentHash {
		v.settle(nil, ErrContentHash)
		return
	}
	if v.root.Profile == Profile1 {
		built, err := BuildTree(v.out.Blobs, v.root.ContentHash, v.root.Extensions)
		if err != nil {
			v.settle(nil, fmt.Errorf("%w: %v", ErrCanonical, err))
			return
		}
		if built.RootHash != v.rootHash {
			v.settle(nil, fmt.Errorf("%w: another grouping of the same blobs", ErrCanonical))
			return
		}
		v.out.Canonical = true
	}
	v.settle(v.out, nil)
}
