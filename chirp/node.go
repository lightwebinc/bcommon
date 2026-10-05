package chirp

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/bits"
)

// emptyHash is SHA-256 of no bytes, the content hash of empty content.
var emptyHash = sha256.Sum256(nil)

// EncodeRoot writes a root node. It refuses what DecodeRoot refuses.
func EncodeRoot(r Root) ([]byte, error) {
	if r.Profile == 0 {
		return nil, ErrProfile
	}
	if err := checkChildren(r.Children, r.Length, false); err != nil {
		return nil, err
	}
	if err := checkEmpty(r); err != nil {
		return nil, err
	}
	if err := checkExtensions(r.Extensions, KindRoot); err != nil {
		return nil, err
	}
	b := header(KindRoot)
	b = binary.BigEndian.AppendUint16(b, r.Profile)
	b = binary.BigEndian.AppendUint64(b, r.Length)
	b = append(b, r.ContentHash[:]...)
	b = appendTail(b, r.Children, r.Extensions)
	if len(b) > MaxNode {
		return nil, ErrNodeSize
	}
	return b, nil
}

// EncodeBranch writes a branch node. It refuses what DecodeBranch refuses.
func EncodeBranch(br Branch) ([]byte, error) {
	if err := checkChildren(br.Children, br.Length, true); err != nil {
		return nil, err
	}
	if err := checkExtensions(br.Extensions, KindBranch); err != nil {
		return nil, err
	}
	b := header(KindBranch)
	b = binary.BigEndian.AppendUint64(b, br.Length)
	b = appendTail(b, br.Children, br.Extensions)
	if len(b) > MaxNode {
		return nil, ErrNodeSize
	}
	return b, nil
}

func header(kind byte) []byte {
	b := make([]byte, 0, 64)
	b = append(b, Magic[:]...)
	return append(b, 1, 0, kind)
}

func appendTail(b []byte, children []Child, ext []Extension) []byte {
	b = appendCompact(b, uint64(len(children)))
	for _, c := range children {
		b = append(b, c.Kind)
		b = binary.BigEndian.AppendUint64(b, c.Length)
		b = append(b, c.Hash[:]...)
	}
	b = appendCompact(b, uint64(len(ext)))
	for _, e := range ext {
		b = appendCompact(b, e.Type)
		b = appendCompact(b, uint64(len(e.Value)))
		b = append(b, e.Value...)
	}
	return b
}

func appendCompact(b []byte, v uint64) []byte {
	switch {
	case v < 0xfd:
		return append(b, byte(v))
	case v <= 0xffff:
		return binary.LittleEndian.AppendUint16(append(b, 0xfd), uint16(v))
	case v <= 0xffffffff:
		return binary.LittleEndian.AppendUint32(append(b, 0xfe), uint32(v))
	default:
		return binary.LittleEndian.AppendUint64(append(b, 0xff), v)
	}
}

// checkChildren holds a child list to the version 1 rules: at most Fanout
// children (and at least one in a branch), known kinds, and lengths that sum
// to the node's length without overflow.
func checkChildren(children []Child, length uint64, branch bool) error {
	if len(children) > Fanout || (branch && len(children) == 0) {
		return fmt.Errorf("%w: %d children", ErrChildCount, len(children))
	}
	var sum uint64
	for i, c := range children {
		if c.Kind != ChildBlob && c.Kind != ChildBranch {
			return fmt.Errorf("%w: child %d is kind %d", ErrChildKind, i, c.Kind)
		}
		var carry uint64
		sum, carry = bits.Add64(sum, c.Length, 0)
		if carry != 0 {
			return fmt.Errorf("%w: child lengths overflow", ErrLength)
		}
	}
	if sum != length {
		return fmt.Errorf("%w: children sum to %d, node says %d", ErrLength, sum, length)
	}
	return nil
}

func checkEmpty(r Root) error {
	if r.Length == 0 && (len(r.Children) != 0 || r.ContentHash != emptyHash) {
		return ErrEmpty
	}
	return nil
}

// checkExtensions holds an extension vector to the version 1 rules: types
// strictly ascending and never 0, values within MaxExtensionBytes together,
// no critical (even) type, since version 1 assigns none, and mediaType only
// on a root and only as a media-type essence.
func checkExtensions(ext []Extension, kind byte) error {
	var prev uint64
	total := 0
	for i, e := range ext {
		if e.Type == 0 || (i > 0 && e.Type <= prev) {
			return fmt.Errorf("%w: type %d out of order or reserved", ErrExtension, e.Type)
		}
		prev = e.Type
		total += len(e.Value)
		if total > MaxExtensionBytes {
			return fmt.Errorf("%w: more than %d value bytes", ErrExtension, MaxExtensionBytes)
		}
		if e.Type%2 == 0 {
			return fmt.Errorf("%w: unknown critical type %d", ErrExtension, e.Type)
		}
		if e.Type == MediaType {
			if kind != KindRoot {
				return fmt.Errorf("%w: mediaType on a branch", ErrExtension)
			}
			if !ValidMediaType(string(e.Value)) {
				return fmt.Errorf("%w: mediaType %q", ErrExtension, e.Value)
			}
		}
	}
	return nil
}

// ValidMediaType reports whether s is a mediaType value BRC-167 admits: a
// lower-case media-type essence of 3 to 127 bytes, type and subtype each an
// RFC 6838 restricted name, joined by one "/", with no parameters and no
// whitespace.
func ValidMediaType(s string) bool {
	if len(s) < 3 || len(s) > 127 {
		return false
	}
	slash := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/':
			if slash >= 0 {
				return false
			}
			slash = i
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '&' || c == '-' || c == '^' || c == '_' || c == '.' || c == '+':
			// Not the first character of a name.
			if i == 0 || i == slash+1 {
				return false
			}
		default:
			return false
		}
	}
	return slash > 0 && slash < len(s)-1
}

// reader walks a node's bytes, refusing a read past the end.
type reader struct {
	b   []byte
	off int
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || len(r.b)-r.off < n {
		return nil, ErrTruncated
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out, nil
}

func (r *reader) u8() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (r *reader) compact() (uint64, error) {
	p, err := r.u8()
	if err != nil {
		return 0, err
	}
	var v, floor uint64
	switch p {
	case 0xfd:
		b, err := r.take(2)
		if err != nil {
			return 0, err
		}
		v, floor = uint64(binary.LittleEndian.Uint16(b)), 0xfd
	case 0xfe:
		b, err := r.take(4)
		if err != nil {
			return 0, err
		}
		v, floor = uint64(binary.LittleEndian.Uint32(b)), 0x10000
	case 0xff:
		b, err := r.take(8)
		if err != nil {
			return 0, err
		}
		v, floor = binary.LittleEndian.Uint64(b), 0x100000000
	default:
		return uint64(p), nil
	}
	if v < floor {
		return 0, ErrCompactSize
	}
	return v, nil
}

// header reads the common prefix and returns the node kind.
func (r *reader) header() (byte, error) {
	if len(r.b) > MaxNode {
		return 0, ErrNodeSize
	}
	m, err := r.take(5)
	if err != nil {
		return 0, err
	}
	if [5]byte(m) != Magic {
		return 0, ErrMagic
	}
	major, err := r.u8()
	if err != nil {
		return 0, err
	}
	minor, err := r.u8()
	if err != nil {
		return 0, err
	}
	// Minor versions above 0 may add profiles and extensions; this codec
	// knows 1.0 alone and refuses a later minor rather than read it as 1.0.
	if major != 1 || minor != 0 {
		return 0, fmt.Errorf("%w: %d.%d", ErrVersion, major, minor)
	}
	kind, err := r.u8()
	if err != nil {
		return 0, err
	}
	if kind != KindRoot && kind != KindBranch {
		return 0, fmt.Errorf("%w: %d", ErrNodeKind, kind)
	}
	return kind, nil
}

// tail reads the child list and the extension vector, and refuses trailing
// bytes.
func (r *reader) tail() ([]Child, []Extension, error) {
	n, err := r.compact()
	if err != nil {
		return nil, nil, err
	}
	if n > Fanout {
		return nil, nil, fmt.Errorf("%w: %d children", ErrChildCount, n)
	}
	children := make([]Child, n)
	for i := range children {
		k, err := r.u8()
		if err != nil {
			return nil, nil, err
		}
		if k != ChildBlob && k != ChildBranch {
			return nil, nil, fmt.Errorf("%w: child %d is kind %d", ErrChildKind, i, k)
		}
		l, err := r.u64()
		if err != nil {
			return nil, nil, err
		}
		h, err := r.take(32)
		if err != nil {
			return nil, nil, err
		}
		children[i] = Child{Kind: k, Length: l, Hash: [32]byte(h)}
	}
	ne, err := r.compact()
	if err != nil {
		return nil, nil, err
	}
	// Each entry takes at least two bytes, so a count past what remains is
	// a truncation, found before anything is allocated for it.
	if ne > uint64(len(r.b)-r.off)/2 {
		return nil, nil, ErrTruncated
	}
	var ext []Extension
	for range ne {
		t, err := r.compact()
		if err != nil {
			return nil, nil, err
		}
		l, err := r.compact()
		if err != nil {
			return nil, nil, err
		}
		if l > uint64(len(r.b)-r.off) {
			return nil, nil, ErrTruncated
		}
		v, err := r.take(int(l))
		if err != nil {
			return nil, nil, err
		}
		ext = append(ext, Extension{Type: t, Value: append([]byte(nil), v...)})
	}
	if r.off != len(r.b) {
		return nil, nil, ErrTrailing
	}
	return children, ext, nil
}

// Kind returns the kind of a node from its prefix alone, after the size,
// magic and version checks.
func Kind(b []byte) (byte, error) {
	r := reader{b: b}
	return r.header()
}

// DecodeRoot reads a root node. The checks run in order: size, magic,
// version, kind, profile, then the layout (truncated, compact-size,
// child-count, child-kind, trailing), then the child lengths against the
// root's (length), the empty-content rule (empty), and the extensions
// (extension).
func DecodeRoot(b []byte) (Root, error) {
	r := reader{b: b}
	kind, err := r.header()
	if err != nil {
		return Root{}, err
	}
	if kind != KindRoot {
		return Root{}, fmt.Errorf("%w: a branch where a root is wanted", ErrNodeKind)
	}
	pb, err := r.take(2)
	if err != nil {
		return Root{}, err
	}
	var out Root
	out.Profile = binary.BigEndian.Uint16(pb)
	if out.Profile == 0 {
		return Root{}, ErrProfile
	}
	if out.Length, err = r.u64(); err != nil {
		return Root{}, err
	}
	ch, err := r.take(32)
	if err != nil {
		return Root{}, err
	}
	out.ContentHash = [32]byte(ch)
	if out.Children, out.Extensions, err = r.tail(); err != nil {
		return Root{}, err
	}
	if err := checkChildren(out.Children, out.Length, false); err != nil {
		return Root{}, err
	}
	if err := checkEmpty(out); err != nil {
		return Root{}, err
	}
	if err := checkExtensions(out.Extensions, KindRoot); err != nil {
		return Root{}, err
	}
	return out, nil
}

// DecodeBranch reads a branch node, with DecodeRoot's checks in the same
// order, at least one child required.
func DecodeBranch(b []byte) (Branch, error) {
	r := reader{b: b}
	kind, err := r.header()
	if err != nil {
		return Branch{}, err
	}
	if kind != KindBranch {
		return Branch{}, fmt.Errorf("%w: a root where a branch is wanted", ErrNodeKind)
	}
	var out Branch
	if out.Length, err = r.u64(); err != nil {
		return Branch{}, err
	}
	if out.Children, out.Extensions, err = r.tail(); err != nil {
		return Branch{}, err
	}
	if err := checkChildren(out.Children, out.Length, true); err != nil {
		return Branch{}, err
	}
	if err := checkExtensions(out.Extensions, KindBranch); err != nil {
		return Branch{}, err
	}
	return out, nil
}
