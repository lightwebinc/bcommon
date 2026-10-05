package chirp

import (
	"crypto/sha256"
	"fmt"
	"hash"
)

// Fetch returns the bytes of the object named by hash, or an error when it
// has none; Verify checks the bytes against the hash itself. A blob may be
// asked for more than once when the closure repeats it.
type Fetch func(hash [32]byte) ([]byte, error)

// Limits bounds what Verify follows. A zero field takes its default.
type Limits struct {
	// MaxReferences bounds the child references followed, repeats counted;
	// DefaultMaxReferences when zero.
	MaxReferences int
	// MaxLength bounds the content's length, checked against the root before
	// anything else is fetched; no bound beyond MaxReferences when zero.
	MaxLength uint64
}

// Closure is a verified closure.
type Closure struct {
	Root     Root
	RootHash [32]byte
	// Blobs are the blob references in content order, repeats included.
	Blobs []Child
	// Objects are the closure's distinct objects, root first, then in the
	// order a depth-first walk in child order first meets them.
	Objects [][32]byte
	// Canonical is true when the root declares profile 1 and the closure is
	// profile 1's canonical construction. A closure of another profile is
	// verified for everything but its construction, and Canonical is false.
	Canonical bool
}

// Verify fetches and checks the closure of the root named rootHash, holding
// at most one blob at a time:
//
//  1. the root: missing, hash, its node checks (DecodeRoot), then too-large
//     against Limits.MaxLength;
//  2. a depth-first walk in child order, for each reference: references
//     (over the limit), depth (an object past MaxDepth), cycle (a branch
//     that is its own ancestor), missing; a branch's hash, its node checks,
//     and length (its length against the reference's); a blob's length (its
//     byte count against the reference's), then hash;
//  3. content-hash: the blobs in order against the root's content hash;
//  4. for profile 1, canonical: BuildTree over the blob references, with
//     the root's content hash and extensions, gives this root.
//
// Every node is hashed before any of its fields is read, as BRC-167 has it:
// bytes that are not the object asked for are the host's fault, while a
// node that hashes right and still fails its checks is a fault of the
// closure itself, which no other host can mend.
func Verify(rootHash [32]byte, fetch Fetch, lim Limits) (*Closure, error) {
	if lim.MaxReferences <= 0 {
		lim.MaxReferences = DefaultMaxReferences
	}
	rb, err := get(fetch, rootHash)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(rb) != rootHash {
		return nil, fmt.Errorf("%w: the root", ErrHash)
	}
	root, err := DecodeRoot(rb)
	if err != nil {
		return nil, err
	}
	if lim.MaxLength > 0 && root.Length > lim.MaxLength {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, root.Length)
	}
	w := walker{
		fetch:    fetch,
		lim:      lim,
		seen:     map[[32]byte]bool{rootHash: true},
		branches: map[[32]byte]Branch{},
		ancestry: map[[32]byte]bool{rootHash: true},
		content:  sha256.New(),
	}
	out := &Closure{Root: root, RootHash: rootHash, Objects: [][32]byte{rootHash}}
	w.out = out
	if err := w.children(root.Children, 1); err != nil {
		return nil, err
	}
	if [32]byte(w.content.Sum(nil)) != root.ContentHash {
		return nil, ErrContentHash
	}
	if root.Profile == Profile1 {
		built, err := BuildTree(out.Blobs, root.ContentHash, root.Extensions)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCanonical, err)
		}
		if built.RootHash != rootHash {
			return nil, fmt.Errorf("%w: another grouping of the same blobs", ErrCanonical)
		}
		out.Canonical = true
	}
	return out, nil
}

func get(fetch Fetch, h [32]byte) ([]byte, error) {
	b, err := fetch(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %x: %v", ErrMissing, h, err)
	}
	return b, nil
}

type walker struct {
	fetch    Fetch
	lim      Limits
	refs     int
	seen     map[[32]byte]bool
	branches map[[32]byte]Branch
	ancestry map[[32]byte]bool
	content  hash.Hash
	out      *Closure
}

// children walks one node's child list; depth is the number of nodes above
// the children, the root counted.
func (w *walker) children(list []Child, depth int) error {
	for _, c := range list {
		w.refs++
		if w.refs > w.lim.MaxReferences {
			return ErrReferences
		}
		if depth+1 > MaxDepth {
			return ErrDepth
		}
		if c.Kind == ChildBranch {
			if err := w.branch(c, depth); err != nil {
				return err
			}
			continue
		}
		if err := w.blob(c); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) branch(c Child, depth int) error {
	if w.ancestry[c.Hash] {
		return ErrCycle
	}
	br, ok := w.branches[c.Hash]
	if !ok {
		b, err := get(w.fetch, c.Hash)
		if err != nil {
			return err
		}
		if sha256.Sum256(b) != c.Hash {
			return fmt.Errorf("%w: branch %x", ErrHash, c.Hash)
		}
		if br, err = DecodeBranch(b); err != nil {
			return err
		}
		w.branches[c.Hash] = br
	}
	if br.Length != c.Length {
		return fmt.Errorf("%w: branch %x covers %d bytes, its reference %d", ErrLength, c.Hash, br.Length, c.Length)
	}
	w.note(c.Hash)
	w.ancestry[c.Hash] = true
	err := w.children(br.Children, depth+1)
	delete(w.ancestry, c.Hash)
	return err
}

func (w *walker) blob(c Child) error {
	b, err := get(w.fetch, c.Hash)
	if err != nil {
		return err
	}
	if uint64(len(b)) != c.Length {
		return fmt.Errorf("%w: blob %x is %d bytes, its reference %d", ErrLength, c.Hash, len(b), c.Length)
	}
	if sha256.Sum256(b) != c.Hash {
		return fmt.Errorf("%w: blob %x", ErrHash, c.Hash)
	}
	w.content.Write(b)
	w.out.Blobs = append(w.out.Blobs, c)
	w.note(c.Hash)
	return nil
}

func (w *walker) note(h [32]byte) {
	if !w.seen[h] {
		w.seen[h] = true
		w.out.Objects = append(w.out.Objects, h)
	}
}
