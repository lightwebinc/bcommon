package cbor

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// vectorNode is one item of the value in testdata/vectors/cbor-v1.json,
// described by its major type so that this test rebuilds it in this
// package's own types (see tools/vectors).
type vectorNode struct {
	Type  string       `json:"type"`
	Value string       `json:"value"`
	Items []vectorNode `json:"items"`
	Pairs []struct {
		Key   vectorNode `json:"key"`
		Value vectorNode `json:"value"`
	} `json:"pairs"`
}

func (n vectorNode) build(t *testing.T) Value {
	t.Helper()
	switch n.Type {
	case "uint":
		v, err := strconv.ParseUint(n.Value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	case "neg":
		v, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil || v >= 0 {
			t.Fatalf("neg %q: %v", n.Value, err)
		}
		return v
	case "bytes":
		b, err := hex.DecodeString(n.Value)
		if err != nil {
			t.Fatal(err)
		}
		return b
	case "text":
		return n.Value
	case "array":
		out := make([]Value, 0, len(n.Items))
		for _, it := range n.Items {
			out = append(out, it.build(t))
		}
		return out
	case "map":
		out := make(Map, 0, len(n.Pairs))
		for _, p := range n.Pairs {
			out = append(out, Pair{Key: p.Key.build(t), Val: p.Value.build(t)})
		}
		return out
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	t.Fatalf("unknown node type %q", n.Type)
	return nil
}

// The generic vector is a nested value an independent encoder
// (fxamacker/cbor under its core deterministic options) wrote: maps with
// integer and text keys listed out of canonical order, integers at every
// head boundary in both signs, byte and text strings at every head width up
// to two bytes, the simple values and empty containers. Encoding the value
// here must give its bytes exactly, and decoding its bytes must give back
// something that encodes to them again.
func TestIndependentVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "cbor-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Value       vectorNode `json:"value"`
		EncodingHex string     `json:"encodingHex"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(v.EncodingHex)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encode(v.Value.build(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoding differs from the independent encoder's:\n got %x\nwant %x", got, want)
	}
	back, err := DecodeValue(want)
	if err != nil {
		t.Fatalf("decoding the independent encoder's bytes: %v", err)
	}
	again, err := Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, want) {
		t.Fatalf("decode then encode does not return the vector:\n got %x\nwant %x", again, want)
	}
}
