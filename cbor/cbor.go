// Package cbor is the deterministic CBOR that committed records are written
// in.
//
// The codec is the subset of RFC 8949 a record needs, with the core
// deterministic encoding rules of §4.2.1 enforced on BOTH sides. The encoder
// only ever writes canonical bytes; the decoder refuses anything that is not
// canonical or is outside the subset, so a record that decodes is byte for
// byte the record its encoder produced, and a hostile publisher cannot hand a
// host two different byte strings for one value.
//
// Supported: unsigned integers (major type 0), negative integers (1), byte
// strings (2), text strings (3, valid UTF-8), arrays (4), maps (5, keys in
// bytewise lexicographic order of their encodings, no duplicates), and the
// simple values false, true and null (7). Refused: indefinite lengths, tags,
// floats, undefined, every other simple value, and nesting deeper than
// MaxDepth. Nothing here allocates from a declared length before the bytes
// are known to be present.
package cbor

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

// MaxDepth bounds nesting. A committed record is two levels deep (a map whose
// body is a map); sixteen leaves room for a body that nests without letting
// a hostile item recurse without limit.
const MaxDepth = 16

var (
	ErrNotCanonical = errors.New("cbor: not canonical")
	ErrUnsupported  = errors.New("cbor: unsupported item")
	ErrTruncated    = errors.New("cbor: truncated")
	ErrTrailing     = errors.New("cbor: trailing bytes")
	ErrDepth        = errors.New("cbor: nesting too deep")
	ErrDuplicateKey = errors.New("cbor: duplicate map key")
	ErrKeyOrder     = errors.New("cbor: map keys out of order")
	ErrUTF8         = errors.New("cbor: invalid UTF-8")
)

// Value is one decoded item: uint64, int64 (negative values only; a decoder
// never produces a non-negative int64), []byte, string, []Value, Map, bool,
// or nil. Encoders additionally accept int, uint and uint8 for convenience.
type Value any

// Pair is one map entry. Map keeps entries in the order given; Encode sorts
// them, Decode returns them in the (already canonical) wire order.
type Pair struct {
	Key Value
	Val Value
}

// Map is a CBOR map. It is a slice, not a Go map, so keys of any supported
// type can be carried and unknown keys keep their wire position.
type Map []Pair

// Get returns the value for key, matching by encoded key bytes.
func (m Map) Get(key Value) (Value, bool) {
	kb, err := Encode(key)
	if err != nil {
		return nil, false
	}
	for _, p := range m {
		pb, err := Encode(p.Key)
		if err == nil && bytes.Equal(pb, kb) {
			return p.Val, true
		}
	}
	return nil, false
}

// Encode writes v in core deterministic encoding.
func Encode(v Value) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, v, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v Value, depth int) error {
	if depth > MaxDepth {
		return ErrDepth
	}
	switch x := v.(type) {
	case nil:
		b.WriteByte(0xf6)
	case bool:
		if x {
			b.WriteByte(0xf5)
		} else {
			b.WriteByte(0xf4)
		}
	case uint64:
		head(b, 0, x)
	case uint:
		head(b, 0, uint64(x))
	case uint8:
		head(b, 0, uint64(x))
	case int:
		encodeSigned(b, int64(x))
	case int64:
		encodeSigned(b, x)
	case []byte:
		head(b, 2, uint64(len(x)))
		b.Write(x)
	case string:
		if !utf8.ValidString(x) {
			return ErrUTF8
		}
		head(b, 3, uint64(len(x)))
		b.WriteString(x)
	case []Value:
		head(b, 4, uint64(len(x)))
		for _, e := range x {
			if err := encode(b, e, depth+1); err != nil {
				return err
			}
		}
	case Map:
		type entry struct{ k, v []byte }
		entries := make([]entry, len(x))
		for i, p := range x {
			var kb, vb bytes.Buffer
			if err := encode(&kb, p.Key, depth+1); err != nil {
				return err
			}
			if err := encode(&vb, p.Val, depth+1); err != nil {
				return err
			}
			entries[i] = entry{kb.Bytes(), vb.Bytes()}
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return bytes.Compare(entries[i].k, entries[j].k) < 0
		})
		for i := 1; i < len(entries); i++ {
			if bytes.Equal(entries[i-1].k, entries[i].k) {
				return ErrDuplicateKey
			}
		}
		head(b, 5, uint64(len(entries)))
		for _, e := range entries {
			b.Write(e.k)
			b.Write(e.v)
		}
	default:
		return fmt.Errorf("%w: %T", ErrUnsupported, v)
	}
	return nil
}

func encodeSigned(b *bytes.Buffer, x int64) {
	if x < 0 {
		head(b, 1, uint64(-1-x))
		return
	}
	head(b, 0, uint64(x))
}

// head writes the initial byte and the shortest argument encoding.
func head(b *bytes.Buffer, mt byte, n uint64) {
	mt <<= 5
	switch {
	case n < 24:
		b.WriteByte(mt | byte(n))
	case n <= math.MaxUint8:
		b.WriteByte(mt | 24)
		b.WriteByte(byte(n))
	case n <= math.MaxUint16:
		b.WriteByte(mt | 25)
		b.Write([]byte{byte(n >> 8), byte(n)})
	case n <= math.MaxUint32:
		b.WriteByte(mt | 26)
		b.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	default:
		b.WriteByte(mt | 27)
		b.Write([]byte{byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	}
}

// DecodeValue parses exactly one canonical item and refuses trailing bytes.
func DecodeValue(b []byte) (Value, error) {
	d := decoder{b: b}
	v, err := d.item(0)
	if err != nil {
		return nil, err
	}
	if d.i != len(d.b) {
		return nil, ErrTrailing
	}
	return v, nil
}

type decoder struct {
	b []byte
	i int
}

// arg reads the argument for an initial byte whose additional information is
// ai, enforcing the shortest encoding.
func (d *decoder) arg(ai byte) (uint64, error) {
	switch {
	case ai < 24:
		return uint64(ai), nil
	case ai == 24:
		if d.i+1 > len(d.b) {
			return 0, ErrTruncated
		}
		n := uint64(d.b[d.i])
		d.i++
		if n < 24 {
			return 0, ErrNotCanonical
		}
		return n, nil
	case ai == 25:
		if d.i+2 > len(d.b) {
			return 0, ErrTruncated
		}
		n := uint64(d.b[d.i])<<8 | uint64(d.b[d.i+1])
		d.i += 2
		if n <= math.MaxUint8 {
			return 0, ErrNotCanonical
		}
		return n, nil
	case ai == 26:
		if d.i+4 > len(d.b) {
			return 0, ErrTruncated
		}
		n := uint64(d.b[d.i])<<24 | uint64(d.b[d.i+1])<<16 | uint64(d.b[d.i+2])<<8 | uint64(d.b[d.i+3])
		d.i += 4
		if n <= math.MaxUint16 {
			return 0, ErrNotCanonical
		}
		return n, nil
	case ai == 27:
		if d.i+8 > len(d.b) {
			return 0, ErrTruncated
		}
		var n uint64
		for k := 0; k < 8; k++ {
			n = n<<8 | uint64(d.b[d.i+k])
		}
		d.i += 8
		if n <= math.MaxUint32 {
			return 0, ErrNotCanonical
		}
		return n, nil
	default:
		// 28..30 are reserved, 31 is an indefinite length; neither is canonical.
		return 0, ErrUnsupported
	}
}

func (d *decoder) item(depth int) (Value, error) {
	if depth > MaxDepth {
		return nil, ErrDepth
	}
	if d.i >= len(d.b) {
		return nil, ErrTruncated
	}
	ib := d.b[d.i]
	d.i++
	mt, ai := ib>>5, ib&0x1f

	if mt == 7 {
		// Simple values carry their meaning in the additional information;
		// floats and the one-byte simple-value form are not in the subset.
		switch ai {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22:
			return nil, nil
		default:
			return nil, ErrUnsupported
		}
	}

	n, err := d.arg(ai)
	if err != nil {
		return nil, err
	}
	remaining := uint64(len(d.b) - d.i)

	switch mt {
	case 0:
		return n, nil
	case 1:
		if n > math.MaxInt64 {
			return nil, ErrUnsupported
		}
		return int64(-1) - int64(n), nil
	case 2:
		if n > remaining {
			return nil, ErrTruncated
		}
		out := make([]byte, n)
		copy(out, d.b[d.i:d.i+int(n)])
		d.i += int(n)
		return out, nil
	case 3:
		if n > remaining {
			return nil, ErrTruncated
		}
		s := d.b[d.i : d.i+int(n)]
		if !utf8.Valid(s) {
			return nil, ErrUTF8
		}
		d.i += int(n)
		return string(s), nil
	case 4:
		// Every element is at least one byte, so a count beyond the bytes
		// left is refused before anything is allocated for it.
		if n > remaining {
			return nil, ErrTruncated
		}
		out := make([]Value, 0, n)
		for k := uint64(0); k < n; k++ {
			e, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case 5:
		if n > remaining/2 {
			return nil, ErrTruncated
		}
		out := make(Map, 0, n)
		var prev []byte
		for k := uint64(0); k < n; k++ {
			start := d.i
			key, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			kb := d.b[start:d.i]
			if prev != nil {
				switch bytes.Compare(prev, kb) {
				case 0:
					return nil, ErrDuplicateKey
				case 1:
					return nil, ErrKeyOrder
				}
			}
			prev = kb
			val, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, Pair{key, val})
		}
		return out, nil
	default: // 6, tags
		return nil, ErrUnsupported
	}
}
