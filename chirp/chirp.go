// Package chirp is a codec for CHIRP (BRC-167) major version 1 and its
// chunking profile 1: the root and branch node encodings, the canonical
// construction of a closure from content, the verification of a closure
// fetched object by object, and the UHRP object identifier and CHIRP URL of
// a hash.
//
// A CHIRP object is a byte string named by its SHA-256 hash: a blob of
// content, a branch node or the root node. The root commits to the content's
// length, its SHA-256 and its children; profile 1 cuts the content into
// blobs of exactly ChunkSize bytes (the last holds the rest) and groups
// their references 256 at a time until at most 256 remain.
//
// The decoder refuses everything BRC-167 says a consumer must reject, and
// the encoder refuses to write what the decoder would refuse, so a node this
// package writes is one it reads. Each refusal is one of the errors below;
// Reason names it with the short code the TypeScript twin uses.
package chirp

import (
	"errors"
)

const (
	// ChunkSize is profile 1's blob size: every blob but the last is exactly
	// this long, and the last is 1 to ChunkSize bytes.
	ChunkSize = 4194304
	// Fanout is the most children a node holds, and profile 1's grouping.
	Fanout = 256
	// MaxNode is the largest node, root or branch, in bytes.
	MaxNode = 65536
	// MaxExtensionBytes bounds the extension values of one node together.
	MaxExtensionBytes = 16384
	// MaxDepth bounds the nodes on any path from the root to an object, the
	// root and the object included.
	MaxDepth = 16
	// Profile1 is the chunking profile this package constructs and checks.
	Profile1 = 1
	// MediaType is the one extension type version 1 assigns: advisory, on
	// the root only.
	MediaType = 1
	// DefaultMaxReferences bounds the child references Verify follows when
	// Limits leaves it zero: every blob and branch reference, repeats
	// counted, about 16 TiB of content in profile 1.
	DefaultMaxReferences = 1 << 22
)

// Node kinds and child kinds.
const (
	KindRoot   = 0
	KindBranch = 1

	ChildBlob   = 0
	ChildBranch = 1
)

// Magic is the five bytes every node starts with, ASCII "CHIRP".
var Magic = [5]byte{'C', 'H', 'I', 'R', 'P'}

// The refusals, in the order a node is read.
var (
	ErrNodeSize    = errors.New("chirp: node larger than MaxNode")
	ErrTruncated   = errors.New("chirp: node ends early")
	ErrMagic       = errors.New("chirp: not a CHIRP node")
	ErrVersion     = errors.New("chirp: version not 1.0")
	ErrNodeKind    = errors.New("chirp: unknown node kind")
	ErrProfile     = errors.New("chirp: chunking profile 0 is reserved")
	ErrCompactSize = errors.New("chirp: CompactSize not minimal")
	ErrChildCount  = errors.New("chirp: child count out of range")
	ErrChildKind   = errors.New("chirp: unknown child kind")
	ErrLength      = errors.New("chirp: lengths disagree")
	ErrExtension   = errors.New("chirp: extension refused")
	ErrTrailing    = errors.New("chirp: bytes after the node")
	ErrEmpty       = errors.New("chirp: empty content with children or another content hash")

	// Refusals of a closure.
	ErrMissing     = errors.New("chirp: object not available")
	ErrHash        = errors.New("chirp: object does not hash to its reference")
	ErrDepth       = errors.New("chirp: closure deeper than MaxDepth")
	ErrCycle       = errors.New("chirp: closure refers to an ancestor")
	ErrReferences  = errors.New("chirp: closure has more references than the limit")
	ErrTooLarge    = errors.New("chirp: content longer than the limit")
	ErrContentHash = errors.New("chirp: content does not hash to the root's content hash")
	ErrCanonical   = errors.New("chirp: not profile 1's canonical construction")

	// ErrIdentifier is a string that is not a UHRP object identifier or a
	// CHIRP URL.
	ErrIdentifier = errors.New("chirp: not an object identifier")
)

var reasons = []struct {
	err  error
	code string
}{
	{ErrNodeSize, "node-size"}, {ErrTruncated, "truncated"}, {ErrMagic, "magic"},
	{ErrVersion, "version"}, {ErrNodeKind, "node-kind"}, {ErrProfile, "profile"},
	{ErrCompactSize, "compact-size"}, {ErrChildCount, "child-count"},
	{ErrChildKind, "child-kind"}, {ErrLength, "length"}, {ErrExtension, "extension"},
	{ErrTrailing, "trailing"}, {ErrEmpty, "empty"}, {ErrMissing, "missing"},
	{ErrHash, "hash"}, {ErrDepth, "depth"}, {ErrCycle, "cycle"},
	{ErrReferences, "references"}, {ErrTooLarge, "too-large"},
	{ErrContentHash, "content-hash"}, {ErrCanonical, "canonical"},
	{ErrIdentifier, "identifier"},
}

// Reason returns the short code of a refusal of this package, the code the
// TypeScript twin's ChirpError carries, and false for any other error.
func Reason(err error) (string, bool) {
	for _, r := range reasons {
		if errors.Is(err, r.err) {
			return r.code, true
		}
	}
	return "", false
}

// Child is one child reference: 41 bytes on the wire.
type Child struct {
	Kind   uint8 // ChildBlob or ChildBranch
	Length uint64
	Hash   [32]byte
}

// Extension is one entry of a node's extension vector.
type Extension struct {
	Type  uint64
	Value []byte
}

// Root is a decoded root node.
type Root struct {
	Profile     uint16
	Length      uint64
	ContentHash [32]byte
	Children    []Child
	Extensions  []Extension
}

// Branch is a decoded branch node.
type Branch struct {
	Length     uint64
	Children   []Child
	Extensions []Extension
}
