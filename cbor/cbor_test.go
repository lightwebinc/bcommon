package cbor

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func mustEncode(t *testing.T, v Value) []byte {
	t.Helper()
	b, err := Encode(v)
	if err != nil {
		t.Fatalf("encode %v: %v", v, err)
	}
	return b
}

func TestIntegersShortestForm(t *testing.T) {
	cases := []struct {
		v    Value
		want []byte
	}{
		{uint64(0), []byte{0x00}},
		{uint64(23), []byte{0x17}},
		{uint64(24), []byte{0x18, 0x18}},
		{uint64(255), []byte{0x18, 0xff}},
		{uint64(256), []byte{0x19, 0x01, 0x00}},
		{uint64(65535), []byte{0x19, 0xff, 0xff}},
		{uint64(65536), []byte{0x1a, 0x00, 0x01, 0x00, 0x00}},
		{uint64(4294967295), []byte{0x1a, 0xff, 0xff, 0xff, 0xff}},
		{uint64(4294967296), []byte{0x1b, 0, 0, 0, 1, 0, 0, 0, 0}},
		{int64(-1), []byte{0x20}},
		{int64(-25), []byte{0x38, 0x18}},
		{int(7), []byte{0x07}},
	}
	for _, c := range cases {
		got := mustEncode(t, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("%v: got %x want %x", c.v, got, c.want)
		}
		back, err := DecodeValue(got)
		if err != nil {
			t.Errorf("%v: decode: %v", c.v, err)
		}
		// int encodes as uint64 and comes back as uint64.
		if i, ok := c.v.(int); ok {
			if back != uint64(i) {
				t.Errorf("%v: round trip %v", c.v, back)
			}
		} else if back != c.v {
			t.Errorf("%v: round trip %v", c.v, back)
		}
	}
}

func TestNonShortestIsRefused(t *testing.T) {
	for _, b := range [][]byte{
		{0x18, 0x05},                   // 5 in two bytes
		{0x19, 0x00, 0xff},             // 255 in three
		{0x1a, 0x00, 0x00, 0xff, 0xff}, // 65535 in five
		{0x1b, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
		{0x58, 0x01, 0xaa}, // a 1-byte byte string whose length takes two bytes
	} {
		if _, err := DecodeValue(b); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%x: got %v, want ErrNotCanonical", b, err)
		}
	}
}

func TestStringsAndBytes(t *testing.T) {
	if got := mustEncode(t, "a"); !bytes.Equal(got, []byte{0x61, 'a'}) {
		t.Errorf("text: %x", got)
	}
	if got := mustEncode(t, []byte{1, 2}); !bytes.Equal(got, []byte{0x42, 1, 2}) {
		t.Errorf("bytes: %x", got)
	}
	if _, err := Encode(string([]byte{0xff, 0xfe})); !errors.Is(err, ErrUTF8) {
		t.Errorf("encode bad utf8: %v", err)
	}
	if _, err := DecodeValue([]byte{0x62, 0xff, 0xfe}); !errors.Is(err, ErrUTF8) {
		t.Errorf("decode bad utf8: %v", err)
	}
	v, err := DecodeValue([]byte{0x42, 1, 2})
	if err != nil || !bytes.Equal(v.([]byte), []byte{1, 2}) {
		t.Errorf("bytes round trip: %v %v", v, err)
	}
}

func TestMapsAreSortedAndUnique(t *testing.T) {
	got := mustEncode(t, Map{{"b", uint64(1)}, {"a", uint64(2)}, {uint64(3), true}})
	// integer key 3 (0x03) sorts before text keys (0x61..); "a" before "b".
	want := []byte{0xa3, 0x03, 0xf5, 0x61, 'a', 0x02, 0x61, 'b', 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("sorted map: got %x want %x", got, want)
	}
	if _, err := Encode(Map{{"a", 1}, {"a", 2}}); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("encode duplicate: %v", err)
	}
	if _, err := DecodeValue([]byte{0xa2, 0x61, 'b', 0x01, 0x61, 'a', 0x02}); !errors.Is(err, ErrKeyOrder) {
		t.Errorf("decode unsorted: %v", err)
	}
	if _, err := DecodeValue([]byte{0xa2, 0x61, 'a', 0x01, 0x61, 'a', 0x02}); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("decode duplicate: %v", err)
	}
	m, err := DecodeValue(want)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := m.(Map).Get("a"); !ok || v != uint64(2) {
		t.Errorf("Get a: %v %v", v, ok)
	}
}

// Every map entry is a key and a value of at least one byte each, so a count
// past half the bytes left is refused before anything is allocated for it.
// The rows sit either side of that bound. The refused rows of two and three
// entries would, under a bound of all the bytes left, decode far enough to
// meet a different refusal (key order, a duplicate key), so their error pins
// the bound itself and not merely the fact that the bytes run out.
func TestMapCountBound(t *testing.T) {
	zero, one, two := uint64(0), uint64(1), uint64(2)
	for _, c := range []struct {
		in   []byte
		want Map    // when it decodes
		err  string // when it is refused
	}{
		// One entry needs two bytes.
		{[]byte{0xa1, 0x00}, nil, "cbor: truncated"},
		{[]byte{0xa1, 0x00, 0x00}, Map{{zero, zero}}, ""},
		// Two entries in three bytes; looser, the second key is out of order.
		{[]byte{0xa2, 0x01, 0x01, 0x00}, nil, "cbor: truncated"},
		// Two entries in three bytes; looser, the second key is a duplicate.
		{[]byte{0xa2, 0x00, 0x00, 0x00}, nil, "cbor: truncated"},
		{[]byte{0xa2, 0x00, 0x01, 0x01, 0x02}, Map{{zero, one}, {one, two}}, ""},
		// Three entries in five bytes; looser, the third key is a duplicate.
		{[]byte{0xa3, 0x00, 0x00, 0x01, 0x01, 0x01}, nil, "cbor: truncated"},
		{[]byte{0xa3, 0x00, 0x00, 0x01, 0x01, 0x02, 0x02}, Map{{zero, zero}, {one, one}, {two, two}}, ""},
	} {
		v, err := DecodeValue(c.in)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%x: got (%v, %v), want %q", c.in, v, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(v, c.want) {
			t.Errorf("%x: got (%#v, %v), want %#v", c.in, v, err, c.want)
		}
	}
}

func TestRefusedItems(t *testing.T) {
	cases := map[string]struct {
		b   []byte
		err error
	}{
		"indefinite array":  {[]byte{0x9f, 0xff}, ErrUnsupported},
		"indefinite bstr":   {[]byte{0x5f, 0xff}, ErrUnsupported},
		"tag":               {[]byte{0xc0, 0x00}, ErrUnsupported},
		"float16":           {[]byte{0xf9, 0x3c, 0x00}, ErrUnsupported},
		"float64":           {[]byte{0xfb, 0, 0, 0, 0, 0, 0, 0, 0}, ErrUnsupported},
		"undefined":         {[]byte{0xf7}, ErrUnsupported},
		"simple 32":         {[]byte{0xf8, 0x20}, ErrUnsupported},
		"reserved ai 28":    {[]byte{0x1c}, ErrUnsupported},
		"trailing":          {[]byte{0x01, 0x02}, ErrTrailing},
		"truncated bstr":    {[]byte{0x45, 1, 2}, ErrTruncated},
		"truncated array":   {[]byte{0x83, 0x01}, ErrTruncated},
		"huge array count":  {[]byte{0x1b | 0x80, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ErrTruncated},
		"huge map count":    {[]byte{0x1a | 0xa0, 0x00, 0x01, 0x00, 0x00, 0x00}, ErrTruncated},
		"empty":             {[]byte{}, ErrTruncated},
		"negative overflow": {[]byte{0x3b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ErrUnsupported},
	}
	for name, c := range cases {
		if _, err := DecodeValue(c.b); !errors.Is(err, c.err) {
			t.Errorf("%s: got %v want %v", name, err, c.err)
		}
	}
	deep := []byte{}
	for i := 0; i <= MaxDepth; i++ {
		deep = append(deep, 0x81)
	}
	deep = append(deep, 0x00)
	if _, err := DecodeValue(deep); !errors.Is(err, ErrDepth) {
		t.Errorf("depth: %v", err)
	}
	var v Value = uint64(0)
	for i := 0; i <= MaxDepth; i++ {
		v = []Value{v}
	}
	if _, err := Encode(v); !errors.Is(err, ErrDepth) {
		t.Errorf("encode depth: %v", err)
	}
	if _, err := Encode(3.5); !errors.Is(err, ErrUnsupported) {
		t.Errorf("encode float: %v", err)
	}
}

func TestSimpleValues(t *testing.T) {
	for _, c := range []struct {
		v Value
		b []byte
	}{{false, []byte{0xf4}}, {true, []byte{0xf5}}, {nil, []byte{0xf6}}} {
		if got := mustEncode(t, c.v); !bytes.Equal(got, c.b) {
			t.Errorf("%v: %x", c.v, got)
		}
		if back, err := DecodeValue(c.b); err != nil || back != c.v {
			t.Errorf("%x: %v %v", c.b, back, err)
		}
	}
}

func TestGenericRoundTrip(t *testing.T) {
	v := Map{
		{uint64(1), []byte{9, 9, 9}},
		{"nested", Map{{"list", []Value{uint64(1), int64(-2), "three", nil, true}}}},
	}
	b := mustEncode(t, v)
	back, err := DecodeValue(b)
	if err != nil {
		t.Fatal(err)
	}
	again := mustEncode(t, back)
	if !bytes.Equal(b, again) {
		t.Fatalf("not stable: %x vs %x", b, again)
	}
}
