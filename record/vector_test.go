package record_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// recordVector is testdata/vectors/record-v1.json: one sample record read
// under a fixed plan, and the ways a record is refused. tools/vectors wrote
// it with an independent encoder.
type recordVector struct {
	MagicHex string `json:"magicHex"`
	Last     uint64 `json:"last"`
	Max      int    `json:"max"`
	MaxKeys  int    `json:"maxKeys"`
	Plan     []struct {
		Key      uint64 `json:"key"`
		Kind     string `json:"kind"`
		Lo       uint64 `json:"lo"`
		Hi       uint64 `json:"hi"`
		N        int    `json:"n"`
		Max      int    `json:"max"`
		Optional bool   `json:"optional"`
	} `json:"plan"`
	EncodedHex string `json:"encodedHex"`
	Cases      []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
		Extra  int    `json:"extra"`
		Hex    string `json:"hex"`
	} `json:"cases"`
	Claims []struct {
		Name   string `json:"name"`
		Claims bool   `json:"claims"`
		Hex    string `json:"hex"`
	} `json:"claims"`
}

func loadVector(t *testing.T) *recordVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "record-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v recordVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// read follows the vector's plan over b, as an application's decoder reads
// its own keys, and returns the pairs it read and the preserved ones.
func read(v *recordVector, b, magic []byte) (cbor.Map, cbor.Map, error) {
	f, err := record.Decode(b, v.Max, v.Last, magic)
	if err != nil {
		return nil, nil, err
	}
	m := cbor.Map{{Key: uint64(0), Val: magic}}
	for _, p := range v.Plan {
		if p.Optional && !f.Has(p.Key) {
			continue
		}
		var val cbor.Value
		switch p.Kind {
		case "text":
			val, err = f.Text(p.Key)
		case "uint":
			val, err = f.Uint(p.Key, p.Lo, p.Hi)
		case "bytes":
			val, err = f.Bytes(p.Key)
		case "bytesN":
			val, err = f.BytesN(p.Key, p.N)
		case "bytesRange":
			val, err = f.BytesRange(p.Key, int(p.Lo), int(p.Hi))
		case "bool":
			val, err = f.Bool(p.Key)
		case "list32":
			var l [][32]byte
			if l, err = f.List32(p.Key, p.Max); err == nil {
				val = record.Values32(l)
			}
		}
		if err != nil {
			return nil, nil, err
		}
		m = append(m, cbor.Pair{Key: p.Key, Val: val})
	}
	return m, f.Extra, nil
}

// Every case is accepted or refused for the reason the vector names, and an
// accepted record re-encodes to the bytes it was read from, preserved keys
// included.
func TestVectorCases(t *testing.T) {
	v := loadVector(t)
	magic := unhex(t, v.MagicHex)
	if v.MaxKeys != record.MaxKeys {
		t.Fatalf("the vector's bound on entries is %d, the package's %d", v.MaxKeys, record.MaxKeys)
	}
	if len(v.Cases) < 40 {
		t.Fatalf("%d cases; the vector is not the one this test was written for", len(v.Cases))
	}
	accepted := 0
	for _, c := range v.Cases {
		b := unhex(t, c.Hex)
		m, extra, err := read(v, b, magic)
		reason, known := record.Reason(err)
		if c.Reason == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.Name, err)
				continue
			}
			accepted++
			if len(extra) != c.Extra {
				t.Errorf("%s: %d preserved pairs, want %d", c.Name, len(extra), c.Extra)
			}
			if err := record.CheckExtra(extra, v.Last, len(m)); err != nil {
				t.Errorf("%s: the preserved pairs are refused on the way out: %v", c.Name, err)
			}
			out, err := record.Encode(m, extra, v.Max)
			if err != nil || !bytes.Equal(out, b) {
				t.Errorf("%s: re-encodes to %x (%v), want the bytes read", c.Name, out, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted, want %s", c.Name, c.Reason)
			continue
		}
		if !known || reason != c.Reason {
			t.Errorf("%s: refused as %q (%v), want %s", c.Name, reason, err, c.Reason)
		}
	}
	if accepted < 8 {
		t.Errorf("%d accepted cases, want at least 8", accepted)
	}
	if got := unhex(t, v.EncodedHex); !bytes.Equal(got, unhex(t, v.Cases[0].Hex)) {
		t.Error("the first case is not the sample record")
	}
}

// The split steps decide what Decode decides, case for case: an application
// that calls them one at a time refuses the same bytes for the same reason.
func TestVectorSplitStepsAgree(t *testing.T) {
	v := loadVector(t)
	magic := unhex(t, v.MagicHex)
	for _, c := range v.Cases {
		b := unhex(t, c.Hex)
		_, whole := record.Decode(b, v.Max, v.Last, magic)
		var split error
		m, err := record.DecodeMap(b, v.Max)
		if err != nil {
			split = err
		} else if f, err := record.Split(m, v.Last); err != nil {
			split = err
		} else {
			split = f.CheckMagic(magic)
		}
		a, _ := record.Reason(whole)
		bb, _ := record.Reason(split)
		if (whole == nil) != (split == nil) || a != bb {
			t.Errorf("%s: Decode says %v, the steps say %v", c.Name, whole, split)
		}
	}
}

func TestVectorClaims(t *testing.T) {
	v := loadVector(t)
	magic := unhex(t, v.MagicHex)
	if len(v.Claims) < 12 {
		t.Fatalf("%d claim cases", len(v.Claims))
	}
	for _, c := range v.Claims {
		if got := record.Claims(unhex(t, c.Hex), magic); got != c.Claims {
			t.Errorf("%s: claims %v, want %v", c.Name, got, c.Claims)
		}
	}
}
