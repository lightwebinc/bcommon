package record

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

var testMagic = []byte("vxr\x01")

func enc(t *testing.T, m cbor.Map) []byte {
	t.Helper()
	b, err := cbor.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every sentinel has its own label, a wrapped sentinel keeps it, and an
// error that is none of them has none.
func TestReason(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range []error{ErrTooLarge, ErrCBOR, ErrKeyType, ErrMagic, ErrMissing, ErrType, ErrRange, ErrList} {
		r, ok := Reason(e)
		if !ok || r == "" || seen[r] {
			t.Errorf("%v: label %q, known %v", e, r, ok)
		}
		seen[r] = true
		if w, ok := Reason(errors.Join(errors.New("context"), e)); !ok || w != r {
			t.Errorf("%v wrapped: label %q", e, w)
		}
	}
	if r, ok := Reason(errors.New("another")); ok || r != "" {
		t.Errorf("an unknown error has the label %q", r)
	}
	if _, ok := Reason(nil); ok {
		t.Error("nil has a label")
	}
}

// The bound is checked before the decoder runs: bytes over it are refused
// as too large whatever they are.
func TestBoundBeforeDecode(t *testing.T) {
	junk := bytes.Repeat([]byte{0xff}, 100)
	if _, err := Decode(junk, 99, 1, testMagic); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the bound: %v", err)
	}
	if _, err := Decode(junk, 100, 1, testMagic); !errors.Is(err, ErrCBOR) {
		t.Errorf("at the bound: %v", err)
	}
}

// A key above the last defined one is preserved in the record's order and
// never read as a defined key; a defined key is never preserved.
func TestExtraIsAboveLast(t *testing.T) {
	b := enc(t, cbor.Map{{Key: uint64(0), Val: testMagic}, {Key: uint64(1), Val: "a"}, {Key: uint64(2), Val: "b"}, {Key: uint64(40), Val: "c"}})
	f, err := Decode(b, 1024, 1, testMagic)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Has(1) || f.Has(2) || f.Has(40) || len(f.Extra) != 2 {
		t.Fatalf("defined %v %v %v, preserved %d", f.Has(1), f.Has(2), f.Has(40), len(f.Extra))
	}
	if f.Extra[0].Key != uint64(2) || f.Extra[1].Key != uint64(40) {
		t.Errorf("preserved keys %v, %v", f.Extra[0].Key, f.Extra[1].Key)
	}
	if _, err := f.Text(2); !errors.Is(err, ErrMissing) {
		t.Errorf("a preserved key is read as a defined one: %v", err)
	}
}

// Preserved pairs are held on the way out to keys above the last defined
// one, so that an encoder cannot be handed a pair that shadows a field, and
// CheckExtra also bounds the whole map.
func TestCheckExtra(t *testing.T) {
	ok := cbor.Map{{Key: uint64(8), Val: uint64(1)}}
	if err := CheckExtraKeys(ok, 7); err != nil {
		t.Error(err)
	}
	for name, extra := range map[string]cbor.Map{
		"a defined key":   {{Key: uint64(7), Val: uint64(1)}},
		"key 0":           {{Key: uint64(0), Val: testMagic}},
		"a text key":      {{Key: "a", Val: uint64(1)}},
		"a negative key":  {{Key: int64(-1), Val: uint64(1)}},
		"a byte key":      {{Key: []byte{1}, Val: uint64(1)}},
		"one bad of many": {{Key: uint64(9), Val: uint64(1)}, {Key: uint64(3), Val: uint64(1)}},
	} {
		if err := CheckExtraKeys(extra, 7); !errors.Is(err, ErrKeyType) {
			t.Errorf("%s: %v", name, err)
		}
		if err := CheckExtra(extra, 7, 8); !errors.Is(err, ErrKeyType) {
			t.Errorf("%s through CheckExtra: %v", name, err)
		}
	}
	var many cbor.Map
	for k := uint64(8); len(many) < MaxKeys-8; k++ {
		many = append(many, cbor.Pair{Key: k, Val: k})
	}
	if err := CheckExtra(many, 7, 8); err != nil {
		t.Errorf("exactly MaxKeys pairs: %v", err)
	}
	if err := CheckExtra(many, 7, 9); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one over MaxKeys: %v", err)
	}
	if err := CheckExtraKeys(append(many, cbor.Pair{Key: uint64(1000), Val: uint64(0)}), 7); err != nil {
		t.Errorf("CheckExtraKeys counts nothing: %v", err)
	}
}

// Encode holds its output to the bound, and never changes the slices it is
// handed: a caller's map with spare capacity is not written into.
func TestEncode(t *testing.T) {
	m := make(cbor.Map, 0, 8)
	m = append(m, cbor.Pair{Key: uint64(1), Val: "one"}, cbor.Pair{Key: uint64(0), Val: testMagic})
	extra := cbor.Map{{Key: uint64(9), Val: uint64(9)}}
	out, err := Encode(m, extra, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || len(m[:3]) != 3 || m[:3][2].Key != nil {
		t.Error("Encode wrote into the caller's map")
	}
	f, err := Decode(out, 64, 1, testMagic)
	if err != nil || len(f.Extra) != 1 {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := Encode(m, extra, len(out)-1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte under the output: %v", err)
	}
	if _, err := Encode(m, extra, len(out)); err != nil {
		t.Errorf("exactly the output: %v", err)
	}
	if _, err := Encode(cbor.Map{{Key: uint64(0), Val: struct{}{}}}, nil, 64); !errors.Is(err, ErrCBOR) {
		t.Errorf("a value the codec does not write: %v", err)
	}
	if _, err := Encode(cbor.Map{{Key: uint64(0), Val: testMagic}, {Key: uint64(0), Val: testMagic}}, nil, 64); !errors.Is(err, ErrCBOR) {
		t.Errorf("a key twice: %v", err)
	}
}

func TestAscending32(t *testing.T) {
	a, b := [32]byte{1}, [32]byte{2}
	for name, c := range map[string]struct {
		l  [][32]byte
		ok bool
	}{
		"empty":     {nil, true},
		"one":       {[][32]byte{a}, true},
		"ascending": {[][32]byte{a, b}, true},
		"equal":     {[][32]byte{a, a}, false},
		"falling":   {[][32]byte{b, a}, false},
	} {
		if err := Ascending32(c.l); (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrList)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	vals := Values32([][32]byte{a, b})
	vals[0].([]byte)[0] = 9
	if a[0] != 1 {
		t.Error("Values32 aliases its input")
	}
}

func TestInRange(t *testing.T) {
	if err := InRange(5, 5, 5, "x"); err != nil {
		t.Error(err)
	}
	for _, n := range []uint64{4, 6} {
		if err := InRange(n, 5, 5, "x"); !errors.Is(err, ErrRange) {
			t.Errorf("%d: %v", n, err)
		}
	}
}

// Decode survives arbitrary bytes: no panic, and every accepted input
// re-encodes to itself through the pairs it returned.
func FuzzDecode(f *testing.F) {
	seed, err := cbor.Encode(cbor.Map{{Key: uint64(0), Val: testMagic}, {Key: uint64(3), Val: []cbor.Value{uint64(1)}}, {Key: uint64(70), Val: "x"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{0xa1, 0x00, 0x44, 'v', 'x', 'r', 1})
	f.Add([]byte{0xbf, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		fs, err := Decode(b, 4096, 5, testMagic)
		if err != nil {
			if _, ok := Reason(err); !ok {
				t.Fatalf("a refusal with no label: %v", err)
			}
			return
		}
		var m cbor.Map
		for k := uint64(0); k <= 5; k++ {
			if fs.Has(k) {
				v, _ := fs.Need(k)
				m = append(m, cbor.Pair{Key: k, Val: v})
			}
		}
		out, err := Encode(m, fs.Extra, 4096)
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("accepted %x, re-encoded %x (%v)", b, out, err)
		}
	})
}
