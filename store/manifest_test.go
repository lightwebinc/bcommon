package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

// bound is a body bound large enough that only the member count limits a
// manifest, so these tests do not borrow any application's number.
const bound = 1 << 20

// The manifest vector's nested member maps are checked against a second
// encoder; nothing else in a store puts a map inside an array.
func TestManifestVector(t *testing.T) {
	want := vector(t, "manifest-body-v1")
	v, err := cbor.DecodeValue(want)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := v.(cbor.Map)
	if !ok {
		t.Fatalf("the vector is a %T, not a map", v)
	}
	m, err := ParseManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 2 ||
		m.Members[0] != (Member{C: fill(0xa1), Name: "part-1", Size: 1200, Type: "text/plain"}) ||
		m.Members[1] != (Member{C: fill(0xb2), Name: "", Size: 64, Type: ""}) {
		t.Fatalf("members: %+v", m.Members)
	}
	if l := m.Leaves(); len(l) != 2 || l[0] != fill(0xa1) || l[1] != fill(0xb2) {
		t.Fatalf("leaves: %x", l)
	}
	built, err := m.Body(len(want))
	if err != nil {
		t.Fatalf("a body at exactly its own size was refused: %v", err)
	}
	got, err := cbor.Encode(built)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Body differs from the independent encoder:\n got %x\nwant %x", got, want)
	}
}

// The bound is the caller's and is applied to the encoded body, so one byte
// under the vector's size refuses it, with the size and bound in the reason.
func TestBodyTakesTheBound(t *testing.T) {
	m := &Manifest{Members: []Member{
		{C: fill(0xa1), Name: "part-1", Size: 1200, Type: "text/plain"},
		{C: fill(0xb2), Size: 64},
	}}
	n := len(vector(t, "manifest-body-v1"))
	_, err := m.Body(n - 1)
	if !errors.Is(err, ErrMemberCount) {
		t.Fatalf("a body over its bound: %v", err)
	}
	if want := "store: manifest member count out of range: 2 members encode to 139 bytes, over the 138 bound"; err.Error() != want {
		t.Errorf("reason %q, want %q", err, want)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	a, b := fill(0xa1), fill(0xb2)
	man := &Manifest{Members: []Member{
		{C: a, Name: "part-1", Size: 1200, Type: "text/plain"},
		{C: b, Name: "", Size: 64, Type: ""},
	}}
	body, err := man.Body(bound)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := cbor.Encode(body)
	if err != nil {
		t.Fatal(err)
	}
	v, err := cbor.DecodeValue(enc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(v.(cbor.Map))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 || got.Members[0] != man.Members[0] || got.Members[1] != man.Members[1] {
		t.Fatalf("round trip: %+v", got.Members)
	}

	if _, err := (&Manifest{}).Body(bound); !errors.Is(err, ErrMemberCount) {
		t.Errorf("empty manifest: %v", err)
	}
	if _, err := (&Manifest{Members: make([]Member, MaxMembers+1)}).Body(bound); !errors.Is(err, ErrMemberCount) {
		t.Errorf("oversize manifest: %v", err)
	}
	if _, err := (&Manifest{Members: make([]Member, MaxMembers)}).Body(bound); err != nil {
		t.Errorf("a manifest of MaxMembers: %v", err)
	}
	if _, err := (&Manifest{Members: []Member{{Name: strings.Repeat("n", MaxRefName+1)}}}).Body(bound); !errors.Is(err, ErrField) {
		t.Errorf("a long member name: %v", err)
	}
	if _, err := (&Manifest{Members: []Member{{Type: strings.Repeat("t", MaxRefName+1)}}}).Body(bound); !errors.Is(err, ErrField) {
		t.Errorf("a long member type: %v", err)
	}

	member := func(c []byte) cbor.Map {
		return cbor.Map{{Key: "c", Val: c}, {Key: "name", Val: ""}, {Key: "size", Val: uint64(1)}, {Key: "type", Val: ""}}
	}
	for _, c := range []struct {
		name string
		body cbor.Map
		want error
	}{
		{"no members", cbor.Map{{Key: "members", Val: []cbor.Value{}}}, ErrMemberCount},
		{"too many members", cbor.Map{{Key: "members", Val: make([]cbor.Value, MaxMembers+1)}}, ErrMemberCount},
		{"a content body", cbor.Map{{Key: "text", Val: "hello"}}, ErrNotManifest},
		{"members not an array", cbor.Map{{Key: "members", Val: "x"}}, ErrNotManifest},
		// A manifest body carries the member list and nothing else: a second
		// field would be content smuggled into a structural record.
		{"a second field", cbor.Map{{Key: "members", Val: []cbor.Value{member(a[:])}}, {Key: "x", Val: "y"}}, ErrNotManifest},
		{"a three-key member", cbor.Map{{Key: "members", Val: []cbor.Value{member(a[:])[:3]}}}, ErrField},
		{"a short commitment", cbor.Map{{Key: "members", Val: []cbor.Value{member(a[:31])}}}, ErrField},
		{"a member not a map", cbor.Map{{Key: "members", Val: []cbor.Value{"x"}}}, ErrField},
	} {
		if _, err := ParseManifest(c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// A member's fixed cost is measured, and it is frozen with the format: a map
// of four (1), "c" (2) and 32 bytes (34), "name" and "" (6), "size" and a
// small integer (6), "type" and "" (6).
func TestMemberOverhead(t *testing.T) {
	if MemberOverhead != 55 {
		t.Fatalf("MemberOverhead %d, frozen as 55", MemberOverhead)
	}
	// And it is what a member adds, while the array's length fits its head.
	for n := 1; n < 24; n++ {
		b, err := (&Manifest{Members: make([]Member, n)}).Body(bound)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := cbor.Encode(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(enc) != 10+n*MemberOverhead {
			t.Fatalf("%d members encode to %d bytes, want %d", n, len(enc), 10+n*MemberOverhead)
		}
	}
}

func TestManifestRefusalTexts(t *testing.T) {
	body := func(m *Manifest) error { _, err := m.Body(bound); return err }
	parse := func(b cbor.Map) error { _, err := ParseManifest(b); return err }
	a := fill(0xa1)
	member := func(pairs ...cbor.Pair) cbor.Map {
		m := cbor.Map{{Key: "c", Val: a[:]}, {Key: "name", Val: ""}, {Key: "size", Val: uint64(1)}, {Key: "type", Val: ""}}
		for _, p := range pairs {
			for i := range m {
				if m[i].Key == p.Key {
					m[i].Val = p.Val
				}
			}
		}
		return m
	}
	list := func(ms ...cbor.Value) cbor.Map { return cbor.Map{{Key: "members", Val: ms}} }
	for _, c := range []struct {
		name string
		err  error
		is   error
		want string
	}{
		{"body empty", body(&Manifest{}), ErrMemberCount, "store: manifest member count out of range: 0"},
		{"body too many", body(&Manifest{Members: make([]Member, MaxMembers+1)}), ErrMemberCount, "store: manifest member count out of range: 1025"},
		{"body long name", body(&Manifest{Members: []Member{{}, {Name: strings.Repeat("n", 65)}}}), ErrField, "store: field has the wrong shape: member 1 name"},
		{"body long type", body(&Manifest{Members: []Member{{Type: strings.Repeat("t", 65)}}}), ErrField, "store: field has the wrong shape: member 0 type"},
		{"parse two fields", parse(cbor.Map{{Key: "members", Val: []cbor.Value{}}, {Key: "x", Val: "y"}}), ErrNotManifest, "store: not a manifest body: body holds 2 field(s)"},
		{"parse no members", parse(cbor.Map{{Key: "text", Val: "x"}}), ErrNotManifest, `store: not a manifest body: no "members"`},
		{"parse not an array", parse(cbor.Map{{Key: "members", Val: "x"}}), ErrNotManifest, `store: not a manifest body: "members" is not an array`},
		{"parse empty", parse(list()), ErrMemberCount, "store: manifest member count out of range: 0"},
		{"parse member shape", parse(list(member(), "x")), ErrField, "store: field has the wrong shape: member 1"},
		{"parse short c", parse(list(member(cbor.Pair{Key: "c", Val: a[:31]}))), ErrField, "store: field has the wrong shape: member c wants 32 bytes"},
		{"parse name", parse(list(member(cbor.Pair{Key: "name", Val: uint64(1)}))), ErrField, "store: field has the wrong shape: member 0 name"},
		{"parse size", parse(list(member(cbor.Pair{Key: "size", Val: "1"}))), ErrField, "store: field has the wrong shape: member size wants an unsigned integer"},
		{"parse type", parse(list(member(), member(), member(cbor.Pair{Key: "type", Val: strings.Repeat("t", 65)}))), ErrField, "store: field has the wrong shape: member 2 type"},
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
