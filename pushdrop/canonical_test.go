package pushdrop_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

type pushdropVector struct {
	FundingTagHex string `json:"fundingTagHex"`
	StateTagHex   string `json:"stateTagHex"`
	Cases         []struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Accept     bool   `json:"accept"`
		SDKDecodes bool   `json:"sdkDecodes"`
		LockHex    string `json:"lockHex"`
	} `json:"cases"`
}

func readPushdropVector(t *testing.T) pushdropVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "pushdrop-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v pushdropVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	return v
}

// Every case of the shared PushDrop vector: CheckCanonical takes exactly the
// canonical layouts, and DecodeTagged refuses every other state token as
// ErrNonCanonical, although go-sdk's decoder reads most of them.
func TestCanonicalIndependentVector(t *testing.T) {
	v := readPushdropVector(t)
	stateTag := goldentest.Hex(t, v.StateTagHex)
	var malleable int
	for _, c := range v.Cases {
		t.Run(c.Kind+"/"+c.Name, func(t *testing.T) {
			s := script.Script(goldentest.Hex(t, c.LockHex))
			err := pushdrop.CheckCanonical(&s)
			if c.Accept != (err == nil) || (err != nil && !errors.Is(err, pushdrop.ErrNonCanonical)) {
				t.Fatalf("CheckCanonical: %v, accept %v", err, c.Accept)
			}
			if !c.Accept && c.SDKDecodes {
				malleable++
			}
			if c.Kind != "state" {
				return
			}
			_, err = pushdrop.DecodeTagged(&s, stateTag, 2)
			if c.Accept != (err == nil) || (err != nil && !errors.Is(err, pushdrop.ErrNonCanonical)) {
				t.Fatalf("DecodeTagged: %v, accept %v", err, c.Accept)
			}
		})
	}
	if malleable == 0 {
		t.Fatal("no case go-sdk reads and the rule refuses: the vector no longer shows the malleation")
	}
}

// canonicalOf is a lock-before PushDrop of fields to the x = 1 key, written
// out by hand.
func canonicalOf(fields ...[]byte) []byte {
	key := append([]byte{0x02}, make([]byte, 31)...)
	key = append(key, 1)
	out := append([]byte{33}, key...)
	out = append(out, script.OpCHECKSIG)
	for _, f := range fields {
		out = append(out, f...)
	}
	n := len(fields)
	for ; n > 1; n -= 2 {
		out = append(out, script.Op2DROP)
	}
	if n == 1 {
		out = append(out, script.OpDROP)
	}
	return out
}

// Each push is held to its one minimal form, including the ones the vector
// has no field for: the small integers, 0x81, an empty field and the
// widths either side of the direct and OP_PUSHDATA1 ranges.
func TestCheckCanonicalMinimalPushes(t *testing.T) {
	b75, b76, b255, b256 := bytes.Repeat([]byte{7}, 75), bytes.Repeat([]byte{7}, 76), bytes.Repeat([]byte{7}, 255), bytes.Repeat([]byte{7}, 256)
	pd1 := func(d []byte) []byte { return append([]byte{script.OpPUSHDATA1, byte(len(d))}, d...) }
	pd2 := func(d []byte) []byte {
		return append([]byte{script.OpPUSHDATA2, byte(len(d)), byte(len(d) >> 8)}, d...)
	}
	direct := func(d []byte) []byte { return append([]byte{byte(len(d))}, d...) }
	for _, c := range []struct {
		name   string
		accept bool
		field  []byte
	}{
		{"OP_0", true, []byte{script.Op0}},
		{"OP_5", true, []byte{script.Op1 + 4}},
		{"OP_16", true, []byte{script.Op16}},
		{"OP_1NEGATE", true, []byte{script.Op1NEGATE}},
		{"0x11 as a direct push, above OP_16", true, direct([]byte{0x11})},
		{"75 bytes direct", true, direct(b75)},
		{"76 bytes with OP_PUSHDATA1", true, pd1(b76)},
		{"255 bytes with OP_PUSHDATA1", true, pd1(b255)},
		{"256 bytes with OP_PUSHDATA2", true, pd2(b256)},
		{"0x05 as a direct push rather than OP_5", false, direct([]byte{0x05})},
		{"0x00 as a direct push rather than OP_0", false, direct([]byte{0x00})},
		{"0x81 as a direct push rather than OP_1NEGATE", false, direct([]byte{0x81})},
		{"an empty OP_PUSHDATA1 rather than OP_0", false, pd1(nil)},
		{"75 bytes with OP_PUSHDATA1", false, pd1(b75)},
		{"76 bytes with OP_PUSHDATA2", false, pd2(b76)},
		{"OP_RESERVED", false, []byte{script.Op1 - 1}},
		{"OP_NOP", false, []byte{script.OpNOP}},
	} {
		s := script.Script(canonicalOf([]byte{script.Op1}, c.field))
		if err := pushdrop.CheckCanonical(&s); c.accept != (err == nil) {
			t.Errorf("%s: %v, accept %v", c.name, err, c.accept)
		}
	}
	if err := pushdrop.CheckCanonical(nil); !errors.Is(err, pushdrop.ErrNonCanonical) {
		t.Errorf("nil: %v", err)
	}
	p2pkh := script.Script(goldentest.Hex(t, "76a914"+"00000000000000000000000000000000000000ff"+"88ac"))
	if err := pushdrop.CheckCanonical(&p2pkh); !errors.Is(err, pushdrop.ErrNonCanonical) {
		t.Errorf("a P2PKH: %v", err)
	}
}
