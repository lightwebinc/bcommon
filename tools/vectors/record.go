package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// The record the family is built on. Its magic is the test prefix, a letter
// and a version byte; it belongs to no application.
var recordMagic = []byte("vxr\x01")

const (
	// recordLast is the last key the sample record defines.
	recordLast = 7
	// recordMax is the sample record's bound, in bytes.
	recordMax = 4096
	// recordMaxKeys is the bound on a record map's entries.
	recordMaxKeys = 64
	// recordListMax bounds the sample record's list.
	recordListMax = 4
)

// recordField is one step of the plan a reader follows after the magic:
// the key, how it is read, and the bounds of that reading.
type recordField struct {
	Key uint64 `json:"key"`
	// Kind is text, uint, bytes (any length), bytesN (exactly n bytes),
	// bytesRange (lo to hi bytes), bool or list32 (at most max 32-byte
	// strings, strictly ascending).
	Kind string `json:"kind"`
	Lo   uint64 `json:"lo,omitempty"`
	Hi   uint64 `json:"hi,omitempty"`
	N    int    `json:"n,omitempty"`
	Max  int    `json:"max,omitempty"`
	// Optional marks a key read only when the record carries it.
	Optional bool `json:"optional,omitempty"`
}

type recordCase struct {
	// Name says what the case is, or what it breaks.
	Name string `json:"name"`
	// Reason is the refusal's label, empty when the record is accepted.
	Reason string `json:"reason"`
	// Extra is the number of preserved pairs of an accepted record.
	Extra int    `json:"extra"`
	Hex   string `json:"hex"`
}

type claimCase struct {
	Name   string `json:"name"`
	Claims bool   `json:"claims"`
	Hex    string `json:"hex"`
}

// recordVector is one sample record read under a fixed plan, and the ways a
// record is refused, each for the first rule it breaks.
type recordVector struct {
	MagicHex string `json:"magicHex"`
	Last     uint64 `json:"last"`
	Max      int    `json:"max"`
	MaxKeys  int    `json:"maxKeys"`
	// Plan is how a reader reads the defined keys, in order, after the
	// magic.
	Plan []recordField `json:"plan"`
	// Record is the sample record, item by item.
	Record node `json:"record"`
	// EncodedHex is its canonical encoding by the independent encoder.
	EncodedHex string       `json:"encodedHex"`
	Cases      []recordCase `json:"cases"`
	Claims     []claimCase  `json:"claims"`
}

// samplePairs is the sample record's defined pairs, with replace applied
// to the pair of each key it names (a nil node drops the pair).
func samplePairs(replace map[uint64]*node) []pair {
	base := []pair{
		kv(uintN(0), bytesN(recordMagic)),
		kv(uintN(1), textN("sample record")),
		kv(uintN(2), uintN(1700000000)),
		kv(uintN(3), bytesN(fill(0xa0, 32))),
		kv(uintN(4), node{Type: "true"}),
		kv(uintN(5), arrayN(bytesN(fill(0x10, 32)), bytesN(fill(0x40, 32)), bytesN(fill(0x80, 32)))),
		kv(uintN(6), bytesN([]byte("any length"))),
	}
	var out []pair
	for i, p := range base {
		if n, ok := replace[uint64(i)]; ok {
			if n != nil {
				out = append(out, kv(uintN(uint64(i)), *n))
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

func recordFamilies() ([]family, error) {
	v := recordVector{
		MagicHex: hex.EncodeToString(recordMagic), Last: recordLast, Max: recordMax, MaxKeys: recordMaxKeys,
		Plan: []recordField{
			{Key: 1, Kind: "text"},
			{Key: 2, Kind: "uint", Lo: 1, Hi: 253402300799},
			{Key: 3, Kind: "bytesN", N: 32},
			{Key: 4, Kind: "bool"},
			{Key: 5, Kind: "list32", Max: recordListMax},
			{Key: 6, Kind: "bytesRange", Lo: 1, Hi: 16},
			{Key: 7, Kind: "bytes", Optional: true},
		},
		Record: mapN(samplePairs(nil)...),
	}
	canonical, err := encode(v.Record)
	if err != nil {
		return nil, err
	}
	v.EncodedHex = hex.EncodeToString(canonical)

	add := func(name, reason string, extra int, b []byte) {
		v.Cases = append(v.Cases, recordCase{Name: name, Reason: reason, Extra: extra, Hex: hex.EncodeToString(b)})
	}
	enc := func(name, reason string, extra int, pairs ...pair) error {
		b, err := encode(mapN(pairs...))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		add(name, reason, extra, b)
		return nil
	}
	with := func(k uint64, n node) []pair { return samplePairs(map[uint64]*node{k: &n}) }
	without := func(k uint64) []pair { return samplePairs(map[uint64]*node{k: nil}) }
	p := func(n node) *node { return &n }

	list := func(items ...node) node { return arrayN(items...) }
	a, b, c := bytesN(fill(0x10, 32)), bytesN(fill(0x40, 32)), bytesN(fill(0x80, 32))

	// The keys of a record with n entries: the sample's seven and enough
	// preserved keys above the last defined one.
	padded := func(n int) []pair {
		out := samplePairs(nil)
		for k := uint64(recordLast + 1); len(out) < n; k++ {
			out = append(out, kv(uintN(k), uintN(k)))
		}
		return out
	}

	steps := []struct {
		name, reason string
		extra        int
		pairs        []pair
	}{
		{"the sample record", "", 0, samplePairs(nil)},
		{"the optional key present", "", 0, append(samplePairs(nil), kv(uintN(7), bytesN([]byte{1, 2, 3})))},
		{"two keys above the last defined one, preserved", "", 2, append(samplePairs(nil), kv(uintN(9), textN("kept")), kv(uintN(300), arrayN(uintN(1), uintN(2))))},
		{"an empty list", "", 0, with(5, list())},
		{"a list at its bound", "", 0, with(5, list(a, b, c, bytesN(fill(0xc0, 32))))},
		{"the integer at its lower bound", "", 0, with(2, uintN(1))},
		{"the integer at its upper bound", "", 0, with(2, uintN(253402300799))},
		{"exactly the most entries a record map holds", "", recordMaxKeys - 7, padded(recordMaxKeys)},
		{"one entry more than a record map holds", "too-large", 0, padded(recordMaxKeys + 1)},
		{"a text key", "key-type", 0, append(samplePairs(nil), kv(textN("name"), uintN(1)))},
		{"a negative key", "key-type", 0, append(samplePairs(nil), kv(negN(-1), uintN(1)))},
		{"a text key and the wrong magic: the key is refused first", "key-type", 0, append(with(0, bytesN([]byte("vxq\x01"))), kv(textN("name"), uintN(1)))},
		{"another magic", "magic", 0, with(0, bytesN([]byte("vxq\x01")))},
		{"another version of the magic", "magic", 0, with(0, bytesN([]byte("vxr\x02")))},
		{"the magic as text", "magic", 0, with(0, textN("vxr\x01"))},
		{"no magic", "magic", 0, without(0)},
		{"no magic and a missing key: the magic is refused first", "magic", 0, samplePairs(map[uint64]*node{0: nil, 1: nil})},
		{"key 1 missing", "missing", 0, without(1)},
		{"key 5 missing", "missing", 0, without(5)},
		{"key 1 missing and key 2 out of range: the first key read is refused", "missing", 0, samplePairs(map[uint64]*node{1: nil, 2: p(uintN(0))})},
		{"key 1 a byte string", "type", 0, with(1, bytesN([]byte("sample record")))},
		{"key 2 text", "type", 0, with(2, textN("1700000000"))},
		{"key 2 negative", "type", 0, with(2, negN(-5))},
		{"key 2 below its range", "range", 0, with(2, uintN(0))},
		{"key 2 above its range", "range", 0, with(2, uintN(253402300800))},
		{"key 3 text", "type", 0, with(3, textN("x"))},
		{"key 3 one byte short", "range", 0, with(3, bytesN(fill(0xa0, 31)))},
		{"key 3 one byte long", "range", 0, with(3, bytesN(fill(0xa0, 33)))},
		{"key 4 an integer", "type", 0, with(4, uintN(1))},
		{"key 5 a byte string", "type", 0, with(5, bytesN(fill(0x10, 32)))},
		{"key 5 over its bound", "list", 0, with(5, list(a, b, c, bytesN(fill(0xc0, 32)), bytesN(fill(0xe0, 32))))},
		{"key 5 out of order", "list", 0, with(5, list(b, a, c))},
		{"key 5 with one value twice", "list", 0, with(5, list(a, b, b))},
		{"key 5 with an element of 31 bytes", "list", 0, with(5, list(a, bytesN(fill(0x40, 31)), c))},
		{"key 5 with a text element", "list", 0, with(5, list(a, textN("x"), c))},
		{"key 6 empty", "range", 0, with(6, bytesN(nil))},
		{"key 6 over its range", "range", 0, with(6, bytesN(fill(1, 17)))},
		{"key 7 present as text", "type", 0, append(samplePairs(nil), kv(uintN(7), textN("x")))},
	}
	for _, s := range steps {
		if err := enc(s.name, s.reason, s.extra, s.pairs...); err != nil {
			return nil, err
		}
	}

	// Written by hand: bytes the independent encoder never writes.
	add("over the bound, and otherwise the sample record", "too-large", 0, func() []byte {
		b, _ := encode(mapN(append(samplePairs(nil), kv(uintN(9), bytesN(fill(0, recordMax))))...))
		return b
	}())
	add("over the bound and not CBOR: the bound is refused first", "too-large", 0, bytes.Repeat([]byte{0xff}, recordMax+1))
	add("nothing", "cbor", 0, nil)
	add("a trailing byte", "cbor", 0, append(append([]byte{}, canonical...), 0x00))
	add("one byte short", "cbor", 0, canonical[:len(canonical)-1])
	arr, err := encode(arrayN(bytesN(recordMagic), textN("sample record")))
	if err != nil {
		return nil, err
	}
	add("an array", "cbor", 0, arr)
	add("a byte string", "cbor", 0, append([]byte{0x44}, recordMagic...))
	// The two-pair map {0: magic, 1: 1} with the pairs swapped: keys out
	// of canonical order.
	add("map keys out of order", "cbor", 0, cat([]byte{0xa2, 0x01, 0x01, 0x00, 0x44}, recordMagic))
	add("a key twice", "cbor", 0, cat([]byte{0xa2, 0x00, 0x44}, recordMagic, []byte{0x00, 0x44}, recordMagic))
	add("an indefinite-length map", "cbor", 0, cat([]byte{0xbf, 0x00, 0x44}, recordMagic, []byte{0xff}))
	add("a map head in a longer form than its count needs", "cbor", 0, cat([]byte{0xb8, 0x01, 0x00, 0x44}, recordMagic))
	add("a key in a longer form than it needs", "cbor", 0, cat([]byte{0xa1, 0x18, 0x00, 0x44}, recordMagic))

	// What claims a record: only the head is read.
	claim := func(name string, claims bool, b []byte) {
		v.Claims = append(v.Claims, claimCase{Name: name, Claims: claims, Hex: hex.EncodeToString(b)})
	}
	big, err := encode(mapN(padded(24)...))
	if err != nil {
		return nil, err
	}
	if big[0] != 0xb8 {
		return nil, fmt.Errorf("a 24-entry map starts %02x, want b8", big[0])
	}
	other, err := encode(mapN(with(0, bytesN([]byte("vxq\x01")))...))
	if err != nil {
		return nil, err
	}
	claim("the sample record", true, canonical)
	claim("a map of 24 entries, whose head takes two bytes", true, big)
	claim("only the head: a map head, key 0 and the magic", true, cat([]byte{0xa1, 0x00, 0x44}, recordMagic))
	claim("the head and nothing the map declares after it", true, cat([]byte{0xb7, 0x00, 0x44}, recordMagic))
	claim("a map head with a two-byte count", true, cat([]byte{0xb9, 0x00, 0x01, 0x00, 0x44}, recordMagic))
	claim("another magic", false, other)
	claim("the magic one byte short", false, cat([]byte{0xa1, 0x00, 0x44}, recordMagic[:3]))
	claim("the magic as a three-byte string", false, cat([]byte{0xa1, 0x00, 0x43}, recordMagic[:3]))
	claim("the magic as text", false, cat([]byte{0xa1, 0x00, 0x64}, recordMagic))
	claim("key 1 first", false, cat([]byte{0xa1, 0x01, 0x44}, recordMagic))
	claim("an array", false, arr)
	claim("an indefinite-length map", false, cat([]byte{0xbf, 0x00, 0x44}, recordMagic, []byte{0xff}))
	claim("the magic alone", false, recordMagic)
	claim("nothing", false, nil)
	return []family{{"record-v1.json", v}}, nil
}
