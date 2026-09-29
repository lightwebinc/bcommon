package store

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

// The vectors are two CBOR values an independent encoder (fxamacker/cbor,
// core deterministic options) wrote: a refs array and a manifest body. They
// check this package against a second encoder.
func vector(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fill(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}

// encodeRefs is EncodeRefs through to bytes, the form a record carries.
func encodeRefs(t *testing.T, refs []Ref) []byte {
	t.Helper()
	v, err := EncodeRefs(refs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cbor.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeRefs is DecodeRefs from bytes.
func decodeRefs(t *testing.T, b []byte) ([]Ref, error) {
	t.Helper()
	v, err := cbor.DecodeValue(b)
	if err != nil {
		t.Fatal(err)
	}
	return DecodeRefs(v)
}

func TestRefsVector(t *testing.T) {
	want := vector(t, "refs-v1")
	refs, err := decodeRefs(t, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "links" || refs[0].Root != fill(0x44) || refs[0].Count != 3 ||
		refs[0].Head != nil || refs[0].Extended() {
		t.Fatalf("decoded: %+v", refs)
	}
	if got := encodeRefs(t, refs); !bytes.Equal(got, want) {
		t.Fatalf("encode differs from the independent encoder:\n got %x\nwant %x", got, want)
	}
}

// No refs is an empty array, not an absent one: a record always carries the
// key, so its bytes do not depend on whether a caller left the slice nil.
func TestNoRefsIsAnEmptyArray(t *testing.T) {
	if got := encodeRefs(t, nil); !bytes.Equal(got, []byte{0x80}) {
		t.Fatalf("nil refs encode to %x", got)
	}
	refs, err := decodeRefs(t, []byte{0x80})
	if err != nil || refs == nil || len(refs) != 0 {
		t.Fatalf("empty array decodes to %#v, %v", refs, err)
	}
}

// A store name must identify one root. The CBOR rules refuse a duplicate map
// KEY, but refs is an array, so nothing upstream catches a second entry with
// the same name.
func TestDuplicateRefNameIsRefused(t *testing.T) {
	a, b := fill(0xaa), fill(0xbb)
	if _, err := EncodeRefs([]Ref{{Name: "media", Root: a, Count: 1}, {Name: "media", Root: b, Count: 2}}); !errors.Is(err, ErrDupRef) {
		t.Fatalf("EncodeRefs accepted a duplicate ref name: %v", err)
	}
	// And on the way in, so refs written by something else are refused too.
	entry := func(root [32]byte) cbor.Map {
		return cbor.Map{{Key: "count", Val: uint64(1)}, {Key: "name", Val: "media"}, {Key: "root", Val: root[:]}}
	}
	if _, err := DecodeRefs([]cbor.Value{entry(a), entry(b)}); !errors.Is(err, ErrDupRef) {
		t.Fatalf("DecodeRefs accepted a duplicate ref name: %v", err)
	}
	if _, err := EncodeRefs([]Ref{{Name: "links", Root: a, Count: 1}, {Name: "media", Root: b, Count: 2}}); err != nil {
		t.Fatalf("distinct names must still encode: %v", err)
	}
}

// head is the one optional member of an entry and it round-trips. A member
// this version does not define is kept, not refused, and re-encoded
// faithfully; the entry's width is bounded.
func TestRefHeadRoundTripsAndEntryWidthIsBounded(t *testing.T) {
	root, head := fill(0xaa), fill(0xcc)
	enc := encodeRefs(t, []Ref{{Name: "notes", Root: root, Count: 1, Head: &head}, {Name: "links", Root: root, Count: 3}})
	dec, err := decodeRefs(t, enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != 2 || dec[0].Head == nil || *dec[0].Head != head || dec[1].Head != nil {
		t.Fatalf("head did not round-trip: %+v", dec)
	}
	if again := encodeRefs(t, dec); !bytes.Equal(again, enc) {
		t.Error("re-encode differs")
	}

	base := cbor.Map{{Key: "name", Val: "notes"}, {Key: "root", Val: root[:]}, {Key: "count", Val: uint64(1)}}
	with := func(extra ...cbor.Pair) []cbor.Value {
		return []cbor.Value{append(append(cbor.Map{}, base...), extra...)}
	}
	future := with(cbor.Pair{Key: "salt", Val: head[:]})
	got, err := DecodeRefs(future)
	if err != nil {
		t.Fatalf("an entry with a member from a later version was refused: %v", err)
	}
	if len(got) != 1 || !got[0].Extended() {
		t.Fatalf("the unknown member was not preserved: %+v", got)
	}
	if v, ok := got[0].Unknown.Get("salt"); !ok || !bytes.Equal(v.([]byte), head[:]) {
		t.Errorf("unknown member value: %v", v)
	}
	want, err := cbor.Encode(future)
	if err != nil {
		t.Fatal(err)
	}
	if again := encodeRefs(t, got); !bytes.Equal(again, want) {
		t.Errorf("re-encode of an extended entry differs:\n got %x\nwant %x", again, want)
	}

	for name, extra := range map[string][]cbor.Pair{
		"short head":  {{Key: "head", Val: head[:31]}},
		"wide entry":  {{Key: "a", Val: "1"}, {Key: "b", Val: "2"}, {Key: "c", Val: "3"}, {Key: "d", Val: "4"}, {Key: "e", Val: "5"}, {Key: "f", Val: "6"}},
		"numeric key": {{Key: uint64(9), Val: "x"}},
	} {
		if _, err := DecodeRefs(with(extra...)); !errors.Is(err, ErrField) {
			t.Errorf("%s: %v", name, err)
		}
	}
	atBound := with(cbor.Pair{Key: "head", Val: head[:]}, cbor.Pair{Key: "p", Val: "1"}, cbor.Pair{Key: "q", Val: "2"}, cbor.Pair{Key: "r", Val: "3"}, cbor.Pair{Key: "s", Val: "4"})
	if _, err := DecodeRefs(atBound); err != nil {
		t.Errorf("an entry at MaxRefMembers was refused: %v", err)
	}
	if _, err := EncodeRefs([]Ref{{Name: "notes", Unknown: cbor.Map{{Key: "head", Val: "x"}}}}); !errors.Is(err, ErrField) {
		t.Errorf("an unknown member colliding with a defined one encoded: %v", err)
	}
}

func TestRefCountIsBounded(t *testing.T) {
	refs := make([]Ref, MaxRefs+1)
	arr := make([]cbor.Value, MaxRefs+1)
	for i := range refs {
		refs[i] = Ref{Name: fmt.Sprintf("s%d", i), Count: 1}
		arr[i] = cbor.Map{{Key: "name", Val: refs[i].Name}, {Key: "root", Val: make([]byte, 32)}, {Key: "count", Val: uint64(1)}}
	}
	if _, err := EncodeRefs(refs[:MaxRefs]); err != nil {
		t.Fatalf("MaxRefs entries refused: %v", err)
	}
	if _, err := DecodeRefs(arr[:MaxRefs]); err != nil {
		t.Fatalf("MaxRefs entries refused on the way in: %v", err)
	}
	if _, err := EncodeRefs(refs); !errors.Is(err, ErrField) {
		t.Errorf("MaxRefs+1 entries encoded: %v", err)
	}
	if _, err := DecodeRefs(arr); !errors.Is(err, ErrField) {
		t.Errorf("MaxRefs+1 entries decoded: %v", err)
	}
}

// An application rewords these refusals by swapping the sentinel's text at
// the front for its own, so every text is pinned whole, by literal, and each
// must start with its sentinel and wrap exactly it.
func TestRefusalTexts(t *testing.T) {
	for err, want := range map[error]string{
		ErrField:       "store: field has the wrong shape",
		ErrDupRef:      "store: two refs entries name the same store",
		ErrNotManifest: "store: not a manifest body",
		ErrMemberCount: "store: manifest member count out of range",
		ErrUnsupported: "unsupported store",
		ErrNoHead:      "no head",
	} {
		if err.Error() != want {
			t.Errorf("sentinel %q, frozen as %q", err.Error(), want)
		}
	}

	root := fill(0x44)
	entry := func(pairs ...cbor.Pair) []cbor.Value { return []cbor.Value{cbor.Map(pairs)} }
	name := cbor.Pair{Key: "name", Val: "notes"}
	rootP := cbor.Pair{Key: "root", Val: root[:]}
	count := cbor.Pair{Key: "count", Val: uint64(1)}
	decode := func(v cbor.Value) error { _, err := DecodeRefs(v); return err }
	encode := func(refs ...Ref) error { _, err := EncodeRefs(refs); return err }
	many := make([]Ref, MaxRefs+1)
	for i := range many {
		many[i] = Ref{Name: fmt.Sprintf("s%d", i)}
	}
	wide := cbor.Map{{Key: "a", Val: "1"}, {Key: "b", Val: "1"}, {Key: "c", Val: "1"}, {Key: "d", Val: "1"}, {Key: "e", Val: "1"}, {Key: "f", Val: "1"}}
	for _, c := range []struct {
		name string
		err  error
		is   error
		want string
	}{
		{"decode not an array", decode("x"), ErrField, "store: field has the wrong shape: refs"},
		{"decode too many", decode(make([]cbor.Value, MaxRefs+1)), ErrField, "store: field has the wrong shape: 65 refs"},
		{"decode entry not a map", decode([]cbor.Value{"x"}), ErrField, "store: field has the wrong shape: ref"},
		{"decode narrow entry", decode(entry(name, rootP)), ErrField, "store: field has the wrong shape: ref"},
		{"decode short head", decode(entry(name, rootP, count, cbor.Pair{Key: "head", Val: root[:31]})), ErrField, "store: field has the wrong shape: ref head wants 32 bytes"},
		{"decode map key", decode(entry(name, rootP, count, cbor.Pair{Key: cbor.Map{}, Val: "x"})), ErrField, "store: field has the wrong shape: ref member key cbor.Map"},
		{"decode array key", decode(entry(name, rootP, count, cbor.Pair{Key: []cbor.Value{}, Val: "x"})), ErrField, "store: field has the wrong shape: ref member key []cbor.Value"},
		{"decode integer key", decode(entry(name, rootP, count, cbor.Pair{Key: uint64(9), Val: "x"})), ErrField, "store: field has the wrong shape: ref member key uint64"},
		{"decode empty name", decode(entry(cbor.Pair{Key: "name", Val: ""}, rootP, count)), ErrField, "store: field has the wrong shape: ref name"},
		{"decode short root", decode(entry(name, cbor.Pair{Key: "root", Val: root[:5]}, count)), ErrField, "store: field has the wrong shape: ref root wants 32 bytes"},
		{"decode text count", decode(entry(name, rootP, cbor.Pair{Key: "count", Val: "1"})), ErrField, "store: field has the wrong shape: ref count wants an unsigned integer"},
		{"decode duplicate", decode(append(entry(name, rootP, count), entry(name, rootP, count)...)), ErrDupRef, `store: two refs entries name the same store: "notes"`},
		// Two faults in one entry: the check that comes first in DecodeRefs
		// wins, and each adjacent pair is pinned so a reorder shows as a
		// changed reason.
		{"decode short head and integer key", decode(entry(name, rootP, count, cbor.Pair{Key: "head", Val: root[:31]}, cbor.Pair{Key: uint64(9), Val: "x"})), ErrField,
			"store: field has the wrong shape: ref head wants 32 bytes"},
		{"decode integer key and empty name", decode(entry(cbor.Pair{Key: "name", Val: ""}, rootP, count, cbor.Pair{Key: uint64(9), Val: "x"})), ErrField,
			"store: field has the wrong shape: ref member key uint64"},
		{"decode empty name and short root", decode(entry(cbor.Pair{Key: "name", Val: ""}, cbor.Pair{Key: "root", Val: root[:5]}, count)), ErrField,
			"store: field has the wrong shape: ref name"},
		{"decode short root and text count", decode(entry(name, cbor.Pair{Key: "root", Val: root[:5]}, cbor.Pair{Key: "count", Val: "1"})), ErrField,
			"store: field has the wrong shape: ref root wants 32 bytes"},
		{"decode text count and duplicate", decode(append(entry(name, rootP, count), entry(name, rootP, cbor.Pair{Key: "count", Val: "1"})...)), ErrField,
			"store: field has the wrong shape: ref count wants an unsigned integer"},
		{"encode too many", encode(many...), ErrField, "store: field has the wrong shape: 65 refs"},
		{"encode empty name", encode(Ref{}), ErrField, "store: field has the wrong shape: ref name"},
		{"encode long name", encode(Ref{Name: strings.Repeat("n", MaxRefName+1)}), ErrField, "store: field has the wrong shape: ref name"},
		{"encode duplicate", encode(Ref{Name: "notes"}, Ref{Name: "notes"}), ErrDupRef, `store: two refs entries name the same store: "notes"`},
		{"encode map key", encode(Ref{Name: "notes", Unknown: cbor.Map{{Key: cbor.Map{}, Val: "x"}}}), ErrField, "store: field has the wrong shape: ref member key cbor.Map"},
		{"encode defined member", encode(Ref{Name: "notes", Unknown: cbor.Map{{Key: "root", Val: "x"}}}), ErrField, `store: field has the wrong shape: "root" is a defined ref member`},
		{"encode wide", encode(Ref{Name: "notes", Unknown: wide}), ErrField, `store: field has the wrong shape: ref "notes" has 9 members`},
	} {
		if c.err == nil || c.err.Error() != c.want {
			t.Errorf("%s: got %v, want %q", c.name, c.err, c.want)
			continue
		}
		if errors.Unwrap(c.err) != c.is {
			t.Errorf("%s: wraps %v, want %v", c.name, errors.Unwrap(c.err), c.is)
		}
	}
}
