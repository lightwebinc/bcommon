package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

// independent reads one of the vectors tools/vectors generates, from an
// independent encoder (fxamacker/cbor, core deterministic) and its own
// RFC 6962 implementation.
func independent(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unhex32(t *testing.T, s string) [32]byte {
	t.Helper()
	b := unhex(t, s)
	if len(b) != 32 {
		t.Fatalf("%q is %d bytes, want 32", s, len(b))
	}
	return [32]byte(b)
}

// The manifest vector's members take every shape the layout allows: an
// ordinary part, a member with an empty name and type and a zero size, a
// name at the 64-byte bound with a size past 32 bits, and a name outside
// ASCII. Body must write the independent encoder's bytes, ParseManifest must
// read them back to the same members, and Root over the members'
// commitments must be the independent RFC 6962 root.
func TestManifestIndependentVector(t *testing.T) {
	var v struct {
		Members []struct {
			CHex string `json:"cHex"`
			Name string `json:"name"`
			Size uint64 `json:"size"`
			Type string `json:"type"`
		} `json:"members"`
		BodyHex string `json:"bodyHex"`
		RootHex string `json:"rootHex"`
	}
	independent(t, "manifest-v1.json", &v)
	m := &Manifest{}
	for _, mem := range v.Members {
		m.Members = append(m.Members, Member{C: unhex32(t, mem.CHex), Name: mem.Name, Size: mem.Size, Type: mem.Type})
	}
	want := unhex(t, v.BodyHex)
	body, err := m.Body(math.MaxInt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cbor.Encode(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body differs from the independent encoder's:\n got %x\nwant %x", got, want)
	}

	dv, err := cbor.DecodeValue(want)
	if err != nil {
		t.Fatal(err)
	}
	dm, ok := dv.(cbor.Map)
	if !ok {
		t.Fatalf("body decodes to %T", dv)
	}
	parsed, err := ParseManifest(dm)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Members) != len(m.Members) {
		t.Fatalf("parsed %d members, want %d", len(parsed.Members), len(m.Members))
	}
	for i := range m.Members {
		if parsed.Members[i] != m.Members[i] {
			t.Errorf("member %d: parsed %+v, want %+v", i, parsed.Members[i], m.Members[i])
		}
	}

	head := unhex32(t, v.Members[0].CHex)
	ref := Ref{Name: "sample", Count: uint64(len(m.Members)), Head: &head}
	if got, want := Root(ref, m.Leaves()), unhex32(t, v.RootHex); got != want {
		t.Errorf("store root %x, want the independent root %x", got, want)
	}
}

// The refs vector is three entries: a store committed to without a head, a
// one-member store whose root is the leaf hash of its head, and an entry
// with two members this version does not define, one whose key sorts before
// every defined key and one that sorts among them (after root, before
// count). EncodeRefs must write the independent encoder's bytes, with the
// unknown members in their canonical place, and DecodeRefs must read back
// every entry with its unknown members kept.
func TestRefsIndependentVector(t *testing.T) {
	var v struct {
		Entries []struct {
			Name    string `json:"name"`
			RootHex string `json:"rootHex"`
			Count   uint64 `json:"count"`
			HeadHex string `json:"headHex"`
			Unknown []struct {
				Key      string `json:"key"`
				ValueHex string `json:"valueHex"`
			} `json:"unknown"`
		} `json:"entries"`
		EncodingHex string `json:"encodingHex"`
	}
	independent(t, "refs-v1.json", &v)
	var refs []Ref
	unknown := map[string]map[string]string{}
	for _, e := range v.Entries {
		ref := Ref{Name: e.Name, Root: unhex32(t, e.RootHex), Count: e.Count}
		if e.HeadHex != "" {
			h := unhex32(t, e.HeadHex)
			ref.Head = &h
		}
		unknown[e.Name] = map[string]string{}
		for _, u := range e.Unknown {
			val, err := cbor.DecodeValue(unhex(t, u.ValueHex))
			if err != nil {
				t.Fatal(err)
			}
			ref.Unknown = append(ref.Unknown, cbor.Pair{Key: u.Key, Val: val})
			unknown[e.Name][u.Key] = u.ValueHex
		}
		refs = append(refs, ref)
	}
	want := unhex(t, v.EncodingHex)
	if got := encodeRefs(t, refs); !bytes.Equal(got, want) {
		t.Fatalf("refs differ from the independent encoder's:\n got %x\nwant %x", got, want)
	}

	dec, err := decodeRefs(t, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != len(refs) {
		t.Fatalf("decoded %d entries, want %d", len(dec), len(refs))
	}
	var sawHead, sawUnknown bool
	for i, got := range dec {
		w := refs[i]
		if got.Name != w.Name || got.Root != w.Root || got.Count != w.Count ||
			(got.Head == nil) != (w.Head == nil) || (got.Head != nil && *got.Head != *w.Head) {
			t.Errorf("entry %d: decoded %+v, want %+v", i, got, w)
		}
		if len(got.Unknown) != len(unknown[w.Name]) {
			t.Errorf("entry %q: %d unknown members kept, want %d", w.Name, len(got.Unknown), len(unknown[w.Name]))
		}
		for _, p := range got.Unknown {
			k, _ := p.Key.(string)
			enc, err := cbor.Encode(p.Val)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(enc) != unknown[w.Name][k] {
				t.Errorf("entry %q: unknown member %q is %x, want %s", w.Name, k, enc, unknown[w.Name][k])
			}
		}
		// A one-member store's root is the leaf hash of its head, so the
		// independent root checks Root's one-member rule.
		if got.Count == 1 && got.Head != nil {
			sawHead = true
			if r := Root(got, nil); r != got.Root {
				t.Errorf("entry %q: Root %x, want the independent leaf hash %x", got.Name, r, got.Root)
			}
		}
		if got.Extended() {
			sawUnknown = true
			if _, err := Head(got); !errors.Is(err, ErrUnsupported) {
				t.Errorf("entry %q with unknown members: Head gave %v, want ErrUnsupported", got.Name, err)
			}
		}
	}
	if !sawHead || !sawUnknown {
		t.Errorf("the vector lacks a one-member entry with a head (%v) or an entry with unknown members (%v)", sawHead, sawUnknown)
	}
}
