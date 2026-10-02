package pushdrop_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// A script is canonical exactly when it is the one Script rebuilds from
// what Fields reads of it, under its own key: the rebuild decides, for
// every case of the shared vector, what CheckCanonical decides.
func TestRebuildDecidesWhatCheckCanonicalDecides(t *testing.T) {
	v := readPushdropVector(t)
	for _, c := range v.Cases {
		s := goldentest.Hex(t, c.LockHex)
		rebuilt := false
		if fields, ok := pushdrop.Fields(s); ok {
			// Fields held the first push to 33 bytes; where they sit
			// depends on the push, which only the rebuild judges.
			if key, err := guard.ParsePubKey(keyOf(s)); err == nil {
				rebuilt = bytes.Equal(s, pushdrop.Script(key, fields, nil))
			}
		}
		if rebuilt != c.Accept {
			t.Errorf("%s/%s: the rebuild matches %v, the vector accepts %v", c.Kind, c.Name, rebuilt, c.Accept)
		}
	}
}

// keyOf is the 33 bytes of a script's first push, whichever push holds
// them.
func keyOf(s []byte) []byte {
	switch {
	case len(s) >= 34 && s[0] == 33:
		return s[1:34]
	case len(s) >= 35 && s[0] == script.OpPUSHDATA1 && s[1] == 33:
		return s[2:35]
	}
	return nil
}

func TestFirstPush(t *testing.T) {
	head := append(append([]byte{33}, bytes.Repeat([]byte{2}, 33)...), script.OpCHECKSIG)
	with := func(tail ...byte) []byte { return append(append([]byte{}, head...), tail...) }
	big := bytes.Repeat([]byte{9}, 300)
	le16 := binary.LittleEndian.AppendUint16(nil, 300)
	le32 := binary.LittleEndian.AppendUint32(nil, 300)
	for _, c := range []struct {
		name string
		s    []byte
		want []byte
	}{
		{"a direct push", with(3, 'a', 'b', 'c'), []byte("abc")},
		{"a direct push with more after it", with(1, 'a', script.OpDROP), []byte("a")},
		{"OP_PUSHDATA1", with(script.OpPUSHDATA1, 2, 'a', 'b'), []byte("ab")},
		{"OP_PUSHDATA1 of nothing", with(script.OpPUSHDATA1, 0), []byte{}},
		{"OP_PUSHDATA2", with(append(append([]byte{script.OpPUSHDATA2}, le16...), big...)...), big},
		{"OP_PUSHDATA4", with(append(append([]byte{script.OpPUSHDATA4}, le32...), big...)...), big},
	} {
		got, ok := pushdrop.FirstPush(c.s)
		if !ok || !bytes.Equal(got, c.want) {
			t.Errorf("%s: %x, %v", c.name, got, ok)
		}
	}
	for name, s := range map[string][]byte{
		"nothing":                             nil,
		"the key and OP_CHECKSIG alone":       head,
		"OP_0 after OP_CHECKSIG":              with(script.Op0),
		"OP_5 after OP_CHECKSIG":              with(script.Op1 + 4),
		"OP_DROP after OP_CHECKSIG":           with(script.OpDROP),
		"a direct push one byte short":        with(3, 'a', 'b'),
		"OP_PUSHDATA1 with no length":         with(script.OpPUSHDATA1),
		"OP_PUSHDATA1 one byte short":         with(script.OpPUSHDATA1, 2, 'a'),
		"OP_PUSHDATA2 with half a length":     with(script.OpPUSHDATA2, 1),
		"OP_PUSHDATA2 short":                  with(script.OpPUSHDATA2, 5, 0, 'a'),
		"OP_PUSHDATA4 declaring 2^32-1 bytes": with(script.OpPUSHDATA4, 0xff, 0xff, 0xff, 0xff, 'a'),
		"OP_PUSHDATA4 short":                  with(script.OpPUSHDATA4, 5, 0, 0, 0, 'a'),
		"a 32-byte key":                       append(append([]byte{32}, bytes.Repeat([]byte{2}, 32)...), script.OpCHECKSIG, 1, 'a', 0),
		"no OP_CHECKSIG after the key":        append(append([]byte{33}, bytes.Repeat([]byte{2}, 33)...), script.OpDROP, 1, 'a'),
		"the key pushed with OP_PUSHDATA1":    append(append([]byte{script.OpPUSHDATA1, 33}, bytes.Repeat([]byte{2}, 33)...), script.OpCHECKSIG, 1, 'a'),
	} {
		if got, ok := pushdrop.FirstPush(s); ok {
			t.Errorf("%s: read %x", name, got)
		}
	}
}

// Fields stops at the first opcode that is not a data push, refuses a
// script whose first push is not 33 bytes or whose second opcode is not
// OP_CHECKSIG, and refuses the whole script when any push runs past its
// end, even one after the fields.
func TestFields(t *testing.T) {
	head := append(append([]byte{33}, bytes.Repeat([]byte{2}, 33)...), script.OpCHECKSIG)
	with := func(tail ...byte) []byte { return append(append([]byte{}, head...), tail...) }
	got, ok := pushdrop.Fields(with(1, 'a', script.Op0, script.OpPUSHDATA1, 1, 'b', script.Op2DROP, script.OpDROP, 1, 'z'))
	if !ok || len(got) != 3 || string(got[0]) != "a" || len(got[1]) != 0 || string(got[2]) != "b" {
		t.Fatalf("fields %q, %v", got, ok)
	}
	for name, s := range map[string][]byte{
		"nothing":                         nil,
		"the key and OP_CHECKSIG alone":   head,
		"an opcode where the first field": with(script.OpDROP, 1, 'a'),
		"a push past the end, later on":   with(1, 'a', script.OpDROP, 5, 'x'),
		"OP_PUSHDATA4 past the end":       with(script.OpPUSHDATA4, 9, 0, 0, 0, 'x'),
		"a 32-byte first push":            append(append([]byte{32}, bytes.Repeat([]byte{2}, 32)...), script.OpCHECKSIG, 1, 'a'),
		"no OP_CHECKSIG":                  append(append([]byte{33}, bytes.Repeat([]byte{2}, 33)...), script.OpDROP, 1, 'a'),
	} {
		if f, ok := pushdrop.Fields(s); ok {
			t.Errorf("%s: read %q", name, f)
		}
	}
	// OP_1 to OP_16 are not data pushes to this reader: a field written
	// with one ends the fields, and the rebuild then differs.
	if f, _ := pushdrop.Fields(with(1, 'a', script.Op1+4, 1, 'b')); len(f) != 1 {
		t.Errorf("fields after OP_5: %q", f)
	}
}
