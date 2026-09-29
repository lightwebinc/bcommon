package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// node is one CBOR item as a vector file describes it. The major type is
// spelled out, so a reader rebuilds the value in its own types without
// guessing what a JSON number or an empty string was meant to be. Integers
// are decimal strings because a JSON number loses precision above 2^53 in
// some readers.
type node struct {
	// Type is uint, neg, bytes, text, array, map, true, false or null.
	Type string `json:"type"`
	// Value is the decimal integer, the hex of a byte string, or the text.
	Value string  `json:"value,omitempty"`
	Items *[]node `json:"items,omitempty"`
	Pairs *[]pair `json:"pairs,omitempty"`
}

// pair is one map entry. A map's pairs are listed in the order they were
// written here, which is deliberately not the canonical order, so that a
// reader's encoder is checked for sorting as well as for its heads.
type pair struct {
	Key   node `json:"key"`
	Value node `json:"value"`
}

func uintN(v uint64) node  { return node{Type: "uint", Value: strconv.FormatUint(v, 10)} }
func negN(v int64) node    { return node{Type: "neg", Value: strconv.FormatInt(v, 10)} }
func bytesN(b []byte) node { return node{Type: "bytes", Value: hex.EncodeToString(b)} }
func textN(s string) node  { return node{Type: "text", Value: s} }

func arrayN(items ...node) node {
	if items == nil {
		items = []node{}
	}
	return node{Type: "array", Items: &items}
}

func mapN(pairs ...pair) node {
	if pairs == nil {
		pairs = []pair{}
	}
	return node{Type: "map", Pairs: &pairs}
}

func kv(k, v node) pair { return pair{Key: k, Value: v} }

// native is the value handed to the independent encoder. A map becomes a Go
// map, whose iteration order is random, so the encoder's own sorting is
// what puts the keys in order.
func (n node) native() (any, error) {
	switch n.Type {
	case "uint":
		return strconv.ParseUint(n.Value, 10, 64)
	case "neg":
		v, err := strconv.ParseInt(n.Value, 10, 64)
		if err == nil && v >= 0 {
			err = fmt.Errorf("neg %d is not negative", v)
		}
		return v, err
	case "bytes":
		return hex.DecodeString(n.Value)
	case "text":
		return n.Value, nil
	case "array":
		out := make([]any, 0, len(*n.Items))
		for _, it := range *n.Items {
			v, err := it.native()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case "map":
		out := make(map[any]any, len(*n.Pairs))
		for _, p := range *n.Pairs {
			k, err := p.Key.native()
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case uint64, int64, string:
			default:
				return nil, fmt.Errorf("map key of type %s cannot be a Go map key", p.Key.Type)
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("duplicate map key %v", k)
			}
			v, err := p.Value.native()
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown node type %q", n.Type)
}

// encoder is fxamacker/cbor under its core deterministic options (RFC 8949
// §4.2.1): shortest heads, definite lengths, and map keys sorted by the
// bytewise order of their encodings.
func encoder() (cbor.EncMode, error) {
	return cbor.CoreDetEncOptions().EncMode()
}

// encode is n's canonical encoding by the independent encoder.
func encode(n node) ([]byte, error) {
	em, err := encoder()
	if err != nil {
		return nil, err
	}
	v, err := n.native()
	if err != nil {
		return nil, err
	}
	return em.Marshal(v)
}

type cborVector struct {
	Value       node   `json:"value"`
	EncodingHex string `json:"encodingHex"`
}

// fill is n bytes counting up from start, wrapping, so a long byte string
// is not one repeated byte that would hide an offset error.
func fill(start byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

// nested is the generic value: maps with integer and text keys listed out of
// canonical order, arrays, byte and text strings whose lengths take every
// head width up to two bytes, integers at every head boundary in both signs
// out to the widest a Go uint64 and int64 hold, the three simple values,
// empty containers, and containers nested eight deep.
func nested() node {
	ints := arrayN(
		uintN(0), uintN(23), uintN(24), uintN(255), uintN(256),
		uintN(65535), uintN(65536), uintN(math.MaxUint32), uintN(math.MaxUint32+1),
		uintN(math.MaxUint64),
		negN(-1), negN(-24), negN(-25), negN(-256), negN(-257),
		negN(-65536), negN(-65537), negN(-(math.MaxUint32 + 1)), negN(-(math.MaxUint32 + 2)),
		negN(math.MinInt64),
	)
	byteStrings := arrayN(
		bytesN(nil), bytesN([]byte{0x00}), bytesN(fill(0x01, 23)), bytesN(fill(0x20, 24)),
		bytesN(fill(0x40, 255)), bytesN(fill(0x80, 256)), bytesN(fill(0xc0, 300)),
	)
	texts := arrayN(
		textN(""), textN("a"), textN(strings.Repeat("t", 23)), textN(strings.Repeat("u", 24)),
		textN("café naïve über 日本"), textN(strings.Repeat("ab", 128)),
	)
	deep := mapN(
		kv(textN("level"), uintN(1)),
		kv(textN("next"), arrayN(
			mapN(
				kv(uintN(3), arrayN(
					mapN(kv(negN(-4), arrayN(arrayN(), mapN(), bytesN(nil), textN("")))),
				)),
			),
		)),
	)
	return mapN(
		kv(textN("text"), texts),
		kv(uintN(1000), textN("a three-byte key")),
		kv(textN("bytes"), byteStrings),
		kv(negN(-1), bytesN([]byte{0xde, 0xad, 0xbe, 0xef})),
		kv(textN("ints"), ints),
		kv(uintN(0), textN("the smallest key")),
		kv(textN("aa"), uintN(2)),
		kv(uintN(24), textN("a two-byte key")),
		kv(textN("b"), uintN(1)),
		kv(negN(-25), textN("a two-byte negative key")),
		kv(textN("flags"), arrayN(node{Type: "true"}, node{Type: "false"}, node{Type: "null"})),
		kv(uintN(23), textN("the largest one-byte key")),
		kv(textN("deep"), deep),
		kv(uintN(math.MaxUint64), textN("the largest key")),
		kv(textN(""), textN("an empty key")),
	)
}

func cborFamilies() ([]family, error) {
	v := nested()
	enc, err := encode(v)
	if err != nil {
		return nil, fmt.Errorf("cbor: %w", err)
	}
	return []family{{"cbor-v1.json", cborVector{Value: v, EncodingHex: hex.EncodeToString(enc)}}}, nil
}
