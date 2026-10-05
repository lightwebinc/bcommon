package chirp

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"math/bits"
)

// Built is a closure as profile 1 constructs it.
type Built struct {
	// Root is the root node's bytes and RootHash their SHA-256, the object
	// the closure is named by.
	Root     []byte
	RootHash [32]byte
	// Branches are the branch nodes' bytes, lowest level first and left to
	// right within a level. Two equal branches appear twice.
	Branches [][]byte
	// Blobs are the content's blobs in order when the closure was built from
	// content; nil when it was built from references.
	Blobs [][]byte
}

// BuildTree is profile 1's canonical tree over blob references in content
// order: at most Fanout references are the root's children; more are
// grouped Fanout at a time, left to right, into branches, repeated until at
// most Fanout remain. Every reference must be a blob of exactly ChunkSize
// bytes but the last, which holds 1 to ChunkSize; anything else is
// ErrCanonical. contentHash is the SHA-256 of the content, which BuildTree
// cannot compute from references; ext are the root's extensions.
func BuildTree(blobs []Child, contentHash [32]byte, ext []Extension) (Built, error) {
	var length uint64
	for i, c := range blobs {
		if c.Kind != ChildBlob {
			return Built{}, fmt.Errorf("%w: reference %d is not a blob", ErrCanonical, i)
		}
		last := i == len(blobs)-1
		if (!last && c.Length != ChunkSize) || (last && (c.Length == 0 || c.Length > ChunkSize)) {
			return Built{}, fmt.Errorf("%w: blob %d is %d bytes", ErrCanonical, i, c.Length)
		}
		var carry uint64
		length, carry = bits.Add64(length, c.Length, 0)
		if carry != 0 {
			return Built{}, fmt.Errorf("%w: content longer than 2^64 bytes", ErrLength)
		}
	}
	var out Built
	level := blobs
	for len(level) > Fanout {
		next := make([]Child, 0, (len(level)+Fanout-1)/Fanout)
		for i := 0; i < len(level); i += Fanout {
			group := level[i:min(i+Fanout, len(level))]
			var sum uint64
			for _, c := range group {
				sum += c.Length
			}
			b, err := EncodeBranch(Branch{Length: sum, Children: group})
			if err != nil {
				return Built{}, err
			}
			out.Branches = append(out.Branches, b)
			next = append(next, Child{Kind: ChildBranch, Length: sum, Hash: sha256.Sum256(b)})
		}
		level = next
	}
	root, err := EncodeRoot(Root{Profile: Profile1, Length: length, ContentHash: contentHash, Children: level, Extensions: ext})
	if err != nil {
		return Built{}, err
	}
	out.Root = root
	out.RootHash = sha256.Sum256(root)
	return out, nil
}

// Build is profile 1's closure of content held in memory: its blobs, the
// branches over them and the root, with ext the root's extensions.
func Build(content []byte, ext []Extension) (Built, error) {
	var refs []Child
	var blobs [][]byte
	for off := 0; off < len(content); off += ChunkSize {
		b := content[off:min(off+ChunkSize, len(content))]
		blobs = append(blobs, b)
		refs = append(refs, Child{Kind: ChildBlob, Length: uint64(len(b)), Hash: sha256.Sum256(b)})
	}
	out, err := BuildTree(refs, sha256.Sum256(content), ext)
	if err != nil {
		return Built{}, err
	}
	out.Blobs = blobs
	return out, nil
}

// Chunker builds a closure from content written in pieces, holding at most
// one blob: each blob is handed to emit as soon as it is complete, so that
// it can be published before the content ends. Finish hands over the last.
type Chunker struct {
	emit    func(blob []byte) error
	buf     []byte
	refs    []Child
	content hash.Hash
	err     error
}

// NewChunker returns a Chunker that hands each blob to emit. The slice emit
// receives is reused after emit returns.
func NewChunker(emit func(blob []byte) error) *Chunker {
	return &Chunker{emit: emit, buf: make([]byte, 0, ChunkSize), content: sha256.New()}
}

// Write takes content in order. An error from emit is returned, and every
// later call returns it too.
func (c *Chunker) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n := len(p)
	c.content.Write(p)
	for len(p) > 0 {
		k := min(ChunkSize-len(c.buf), len(p))
		c.buf = append(c.buf, p[:k]...)
		p = p[k:]
		if len(c.buf) == ChunkSize {
			if err := c.flush(); err != nil {
				return n - len(p), err
			}
		}
	}
	return n, nil
}

func (c *Chunker) flush() error {
	c.refs = append(c.refs, Child{Kind: ChildBlob, Length: uint64(len(c.buf)), Hash: sha256.Sum256(c.buf)})
	if err := c.emit(c.buf); err != nil {
		c.err = err
		return err
	}
	c.buf = c.buf[:0]
	return nil
}

// Finish hands over the last blob and returns the closure's root and
// branches; Built.Blobs is nil, since every blob went to emit.
func (c *Chunker) Finish(ext []Extension) (Built, error) {
	if c.err != nil {
		return Built{}, c.err
	}
	if len(c.buf) > 0 {
		if err := c.flush(); err != nil {
			return Built{}, err
		}
	}
	return BuildTree(c.refs, [32]byte(c.content.Sum(nil)), ext)
}
