package pushdrop

import (
	"bytes"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/goldentest"
)

// Script writes the bytes the canonical check rebuilds, field for field,
// at every push width and for the values with opcodes of their own: one
// script is canonical under both, so a host that rebuilds with Script and
// a reader that checks with CheckCanonical agree.
func TestScriptIsTheCanonicalLock(t *testing.T) {
	key := goldentest.FixedKey().PubKey()
	sizes := []int{0, 1, 2, 16, 33, 74, 75, 76, 77, 254, 255, 256, 257, 65535, 65536, 65537}
	var fields [][]byte
	for _, n := range sizes {
		fields = append(fields, bytes.Repeat([]byte{0x5a}, n))
	}
	for _, b := range []byte{0x00, 0x01, 0x05, 0x10, 0x11, 0x4f, 0x80, 0x81, 0xff} {
		fields = append(fields, []byte{b})
	}
	for i, f := range fields {
		for _, set := range [][][]byte{{f}, {[]byte("tag"), f}, {f, []byte("tag"), f}} {
			got := Script(key, set, nil)
			if want := canonicalLock(key.Compressed(), set); !bytes.Equal(got, want) {
				t.Fatalf("field %d (%d bytes), %d fields: Script writes %x..., the canonical lock is %x...", i, len(f), len(set), got[:40], want[:40])
			}
			signed := Script(key, set, []byte("sig"))
			if want := canonicalLock(key.Compressed(), append(append([][]byte{}, set...), []byte("sig"))); !bytes.Equal(signed, want) {
				t.Fatalf("field %d, signed: differs from the canonical lock", i)
			}
			// A one-byte field of 1 to 16 or 0x81 is written as an
			// opcode of its own, which Fields does not read as a push.
			own := func(f []byte) bool { return len(f) == 1 && ((f[0] >= 1 && f[0] <= 16) || f[0] == 0x81) }
			read, ok := Fields(got)
			if own(set[0]) {
				if ok {
					t.Fatalf("field %d: Fields reads a first field written as an opcode of its own", i)
				}
				continue
			}
			wantRead := len(set)
			if len(set) > 1 && own(set[1]) {
				wantRead = 1
			}
			if !ok || len(read) != wantRead {
				t.Fatalf("field %d: Fields reads %d of %d", i, len(read), wantRead)
			}
			if first, ok := FirstPush(got); len(set[0]) > 1 && (!ok || !bytes.Equal(first, read[0])) {
				t.Fatalf("field %d: FirstPush %x, Fields %x", i, first, read[0])
			}
		}
	}
	// An empty signature is still a signature: it is pushed.
	if a, b := Script(key, [][]byte{{1, 2}}, []byte{}), Script(key, [][]byte{{1, 2}}, nil); bytes.Equal(a, b) {
		t.Error("an empty signature is written as none")
	}
	// The caller's fields are not written into.
	set := make([][]byte, 1, 4)
	set[0] = []byte("tag")
	_ = Script(key, set, []byte("sig"))
	if len(set[:2]) != 2 || set[:2][1] != nil {
		t.Error("Script wrote into the caller's fields")
	}
}

// A push whose declared length runs past the end refuses the whole script.
func TestParseScriptRefusesTruncation(t *testing.T) {
	for _, s := range [][]byte{
		{0x05, 1, 2},
		{script.OpPUSHDATA1},
		{script.OpPUSHDATA1, 3, 1},
		{script.OpPUSHDATA2, 0xff},
		{script.OpPUSHDATA2, 0x02, 0x00, 1},
		{script.OpPUSHDATA4, 0xff, 0xff, 0xff},
		{script.OpPUSHDATA4, 0xff, 0xff, 0xff, 0xff, 0},
	} {
		if _, ok := parseScript(s); ok {
			t.Errorf("%x parsed", s)
		}
	}
	ops, ok := parseScript([]byte{script.Op0, 2, 'a', 'b', script.OpDROP, script.OpPUSHDATA1, 1, 'c'})
	if !ok || len(ops) != 4 || !ops[0].push || len(ops[0].data) != 0 || string(ops[1].data) != "ab" || ops[2].push || string(ops[3].data) != "c" {
		t.Errorf("a well-formed script parsed as %+v, %v", ops, ok)
	}
}
