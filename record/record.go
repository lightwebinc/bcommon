// Package record is the bounded, ordered reading of one application record:
// a canonical CBOR map with unsigned-integer keys, whose key 0 is the
// record's magic and whose keys above the last one a version defines are
// preserved and ignored.
//
// A record is refused for the first rule it breaks, in one order, so that
// two implementations refuse the same bytes for the same reason:
//
//  1. ErrTooLarge: the bytes exceed the record's bound, checked before
//     anything is decoded;
//  2. ErrCBOR: not one canonical CBOR item (package cbor), or not a map;
//  3. ErrTooLarge: more than MaxKeys entries;
//  4. ErrKeyType: a key that is not an unsigned integer;
//  5. ErrMagic: key 0 is not the byte string the caller names;
//  6. then each defined key as the caller reads it, in the caller's order:
//     ErrMissing, ErrType, and ErrRange or ErrList for the key's own rule.
//
// The application supplies the bound, the last defined key and the magic,
// and reads its own fields. Nothing here names a record.
package record

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// MaxKeys bounds the entries of a record map, unknown keys included.
const MaxKeys = 64

// The refusals of the record steps. An application counts them by Reason,
// and wraps nothing around them: errors.Is finds each through the detail a
// step adds.
var (
	ErrTooLarge = errors.New("record: record exceeds its bound")
	ErrCBOR     = errors.New("record: not a canonical CBOR map")
	ErrKeyType  = errors.New("record: record key is not an unsigned integer")
	ErrMagic    = errors.New("record: wrong magic")
	ErrMissing  = errors.New("record: required key missing")
	ErrType     = errors.New("record: field has the wrong type")
	ErrRange    = errors.New("record: field out of range")
	ErrList     = errors.New("record: list is over its bound, or not distinct and ascending")
)

// Reason is the fixed label of a record refusal, and false for any other
// error.
func Reason(err error) (string, bool) {
	for _, e := range []struct {
		err error
		s   string
	}{
		{ErrTooLarge, "too-large"}, {ErrCBOR, "cbor"}, {ErrKeyType, "key-type"},
		{ErrMagic, "magic"}, {ErrMissing, "missing"}, {ErrType, "type"},
		{ErrRange, "range"}, {ErrList, "list"},
	} {
		if errors.Is(err, e.err) {
			return e.s, true
		}
	}
	return "", false
}

// Fields is a decoded record: its known integer keys, and the unknown ones
// above the last defined key, which are preserved.
type Fields struct {
	known map[uint64]cbor.Value
	// Extra holds the pairs whose keys are above the last defined key, in
	// the record's order, exactly as decoded.
	Extra cbor.Map
}

// DecodeMap is steps 1 to 3: the bound, one canonical CBOR item that is a
// map, and at most MaxKeys entries. The bound is checked before the decoder
// runs, and the decoder bounds every declared length against the bytes
// present before it allocates.
func DecodeMap(b []byte, max int) (cbor.Map, error) {
	if len(b) > max {
		return nil, ErrTooLarge
	}
	v, err := cbor.DecodeValue(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, ErrCBOR
	}
	if len(m) > MaxKeys {
		return nil, fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
	}
	return m, nil
}

// Split is step 4: it splits a record map into its known integer keys and
// the unknown ones above last, which are preserved. Any key that is not an
// unsigned integer refuses the record.
func Split(m cbor.Map, last uint64) (*Fields, error) {
	f := &Fields{known: make(map[uint64]cbor.Value, len(m))}
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			return nil, ErrKeyType
		}
		if k > last {
			f.Extra = append(f.Extra, p)
			continue
		}
		f.known[k] = p.Val
	}
	return f, nil
}

// CheckMagic is step 5: key 0 present, a byte string, and equal to magic.
func (f *Fields) CheckMagic(magic []byte) error {
	if b, ok := f.known[0].([]byte); !ok || !bytes.Equal(b, magic) {
		return ErrMagic
	}
	return nil
}

// Decode is steps 1 to 5 in order: DecodeMap, Split and CheckMagic.
func Decode(b []byte, max int, last uint64, magic []byte) (*Fields, error) {
	m, err := DecodeMap(b, max)
	if err != nil {
		return nil, err
	}
	f, err := Split(m, last)
	if err != nil {
		return nil, err
	}
	if err := f.CheckMagic(magic); err != nil {
		return nil, err
	}
	return f, nil
}

// Has reports whether the record carries defined key k.
func (f *Fields) Has(k uint64) bool {
	_, ok := f.known[k]
	return ok
}

// Get returns key k's value and whether the record carries it.
func (f *Fields) Get(k uint64) (cbor.Value, bool) {
	v, ok := f.known[k]
	return v, ok
}

// Need returns key k's value, or ErrMissing.
func (f *Fields) Need(k uint64) (cbor.Value, error) {
	v, ok := f.known[k]
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrMissing, k)
	}
	return v, nil
}

// Bytes returns key k as a byte string of any length.
func (f *Fields) Bytes(k uint64) ([]byte, error) {
	v, err := f.Need(k)
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrType, k)
	}
	return b, nil
}

// BytesN returns key k as a byte string of exactly n bytes; a negative n
// takes any length.
func (f *Fields) BytesN(k uint64, n int) ([]byte, error) {
	b, err := f.Bytes(k)
	if err != nil {
		return nil, err
	}
	if n >= 0 && len(b) != n {
		return nil, fmt.Errorf("%w: key %d", ErrRange, k)
	}
	return b, nil
}

// BytesRange returns key k as a byte string of lo to hi bytes.
func (f *Fields) BytesRange(k uint64, lo, hi int) ([]byte, error) {
	b, err := f.Bytes(k)
	if err != nil {
		return nil, err
	}
	if len(b) < lo || len(b) > hi {
		return nil, fmt.Errorf("%w: key %d", ErrRange, k)
	}
	return b, nil
}

// Text returns key k as a text string. The decoder has already held it to
// valid UTF-8.
func (f *Fields) Text(k uint64) (string, error) {
	v, err := f.Need(k)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: key %d", ErrType, k)
	}
	return s, nil
}

// Uint returns key k as an unsigned integer within lo to hi.
func (f *Fields) Uint(k uint64, lo, hi uint64) (uint64, error) {
	v, err := f.Need(k)
	if err != nil {
		return 0, err
	}
	n, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("%w: key %d", ErrType, k)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%w: key %d", ErrRange, k)
	}
	return n, nil
}

// Bool returns key k as a boolean.
func (f *Fields) Bool(k uint64) (bool, error) {
	v, err := f.Need(k)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%w: key %d", ErrType, k)
	}
	return b, nil
}

// Array returns key k as an array of at most max elements. The length is
// checked before the caller allocates anything for the elements.
func (f *Fields) Array(k uint64, max int) ([]cbor.Value, error) {
	v, err := f.Need(k)
	if err != nil {
		return nil, err
	}
	a, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrType, k)
	}
	if len(a) > max {
		return nil, fmt.Errorf("%w: key %d", ErrList, k)
	}
	return a, nil
}

// List32 returns key k as an array of at most max 32-byte strings in
// strictly ascending byte order, so one set has one encoding.
func (f *Fields) List32(k uint64, max int) ([][32]byte, error) {
	a, err := f.Array(k, max)
	if err != nil {
		return nil, err
	}
	out := make([][32]byte, len(a))
	for i, e := range a {
		b, ok := e.([]byte)
		if !ok || len(b) != 32 {
			return nil, fmt.Errorf("%w: key %d", ErrList, k)
		}
		copy(out[i][:], b)
	}
	if err := Ascending32(out); err != nil {
		return nil, fmt.Errorf("%w: key %d", err, k)
	}
	return out, nil
}

// Ascending32 holds a list of 32-byte values to strictly ascending byte
// order, or ErrList.
func Ascending32(a [][32]byte) error {
	for i := 1; i < len(a); i++ {
		if bytes.Compare(a[i-1][:], a[i][:]) >= 0 {
			return ErrList
		}
	}
	return nil
}

// Values32 is a list of 32-byte values as the CBOR array Encode writes, each
// element a copy.
func Values32(a [][32]byte) []cbor.Value {
	out := make([]cbor.Value, len(a))
	for i := range a {
		out[i] = append([]byte(nil), a[i][:]...)
	}
	return out
}

// InRange holds n to lo through hi, or ErrRange naming what.
func InRange(n, lo, hi uint64, what string) error {
	if n < lo || n > hi {
		return fmt.Errorf("%w: %s", ErrRange, what)
	}
	return nil
}

// CheckExtraKeys holds preserved pairs to the rule that their keys are
// unsigned integers above last, so re-encoding cannot shadow a defined
// field.
func CheckExtraKeys(extra cbor.Map, last uint64) error {
	for _, p := range extra {
		k, ok := p.Key.(uint64)
		if !ok || k <= last {
			return ErrKeyType
		}
	}
	return nil
}

// CheckExtra is CheckExtraKeys and the bound on the whole map: the
// preserved pairs and the defined keys the record writes are at most
// MaxKeys together.
func CheckExtra(extra cbor.Map, last uint64, defined int) error {
	if err := CheckExtraKeys(extra, last); err != nil {
		return err
	}
	if len(extra)+defined > MaxKeys {
		return fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
	}
	return nil
}

// Encode writes the defined pairs then the preserved ones as one canonical
// CBOR map, and holds the result to max. The encoder sorts the keys, so the
// order of m does not matter.
func Encode(m cbor.Map, extra cbor.Map, max int) ([]byte, error) {
	all := make(cbor.Map, 0, len(m)+len(extra))
	all = append(append(all, m...), extra...)
	out, err := cbor.Encode(all)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	if len(out) > max {
		return nil, ErrTooLarge
	}
	return out, nil
}

// Claims reports whether payload claims to be a record under magic, reading
// only its head: a definite-length map head (0xa0 to 0xbb), key 0, then a
// byte-string head of four bytes and magic itself. Key 0 always sorts first
// in a canonical map with unsigned-integer keys. It allocates nothing and
// decides nothing else: a payload that claims a record is then held to
// Decode.
func Claims(payload, magic []byte) bool {
	if len(payload) < 1 || payload[0]>>5 != 5 {
		return false
	}
	var i int
	switch ai := payload[0] & 0x1f; {
	case ai < 24:
		i = 1
	case ai <= 27:
		i = 1 + 1<<(ai-24)
	default:
		return false
	}
	if len(payload) < i+2+len(magic) || payload[i] != 0x00 || payload[i+1] != 0x44 {
		return false
	}
	return bytes.Equal(payload[i+2:i+2+len(magic)], magic)
}
