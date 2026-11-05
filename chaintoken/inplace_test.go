package chaintoken_test

import (
	"bytes"
	"errors"
	"testing"
	"unsafe"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/goldentest"
)

// sameWire fails unless ReadWireInPlace reads b exactly as ReadWire does:
// both refuse it as a BEEF, or both read the same subject, the same entries
// with the same txids, BUMP indexes and transaction bytes.
func sameWire(t testing.TB, b []byte) {
	t.Helper()
	want, werr := chaintoken.ReadWire(b, 0)
	got, gerr := chaintoken.ReadWireInPlace(bytes.Clone(b), 0)
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("%x: ReadWire %v, in place %v", b, werr, gerr)
	}
	if werr != nil {
		if !errors.Is(gerr, chaintoken.ErrBEEF) {
			t.Fatalf("%x: in place refused with %v, want ErrBEEF", b, gerr)
		}
		return
	}
	if got.Subject != want.Subject || got.Atomic != want.Atomic || len(got.Entries) != len(want.Entries) || len(got.Bumps) != len(want.Bumps) {
		t.Fatalf("%x: the wire differs", b)
	}
	for i := range want.Entries {
		w, g := want.Entries[i], got.Entries[i]
		if w.Txid != g.Txid || w.Bump != g.Bump || w.TxidOnly != g.TxidOnly || (w.Tx == nil) != (g.Tx == nil) {
			t.Fatalf("%x: entry %d differs", b, i)
		}
		if w.Tx != nil && !bytes.Equal(w.Tx.Bytes(), g.Tx.Bytes()) {
			t.Fatalf("%x: entry %d serialises differently", b, i)
		}
		if w.Tx != nil && w.Tx.TxID().String() != g.Tx.TxID().String() {
			t.Fatalf("%x: entry %d's txid differs", b, i)
		}
	}
}

// The in-place read is ReadWire's on every vector case, every truncation of
// one and every single-byte change of one.
func TestInPlaceReadsAsReadWire(t *testing.T) {
	v, _ := loadVector(t)
	for _, c := range v.Shapes {
		b := goldentest.Hex(t, c.BeefHex)
		sameWire(t, b)
		for n := 0; n < len(b); n++ {
			sameWire(t, b[:n])
		}
		for i := range b {
			m := bytes.Clone(b)
			m[i] ^= 0xff
			sameWire(t, m)
			m[i] = b[i] ^ 0x01
			sameWire(t, m)
		}
	}
}

// Scripts read in place are slices of the bytes, capped at their own end:
// an append reallocates and leaves the bytes after the script alone.
func TestInPlaceScriptsAliasAndAreCapped(t *testing.T) {
	v, _ := loadVector(t)
	for _, c := range v.Shapes {
		if !c.Reads {
			continue
		}
		b := goldentest.Hex(t, c.BeefHex)
		orig := bytes.Clone(b)
		w, err := chaintoken.ReadWireInPlace(b, 0)
		if err != nil {
			t.Fatal(err)
		}
		lo, hi := uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&b[0]))+uintptr(len(b))
		for _, e := range w.Entries {
			if e.Tx == nil {
				continue
			}
			for _, o := range e.Tx.Outputs {
				s := *o.LockingScript
				if len(s) == 0 {
					continue
				}
				if p := uintptr(unsafe.Pointer(&s[0])); p < lo || p >= hi {
					t.Fatalf("%s: an output script is a copy", c.Name)
				}
				if cap(s) != len(s) {
					t.Fatalf("%s: an output script's capacity runs past it", c.Name)
				}
				_ = append(s, 0xAA)
			}
		}
		if !bytes.Equal(b, orig) {
			t.Fatalf("%s: an append wrote into the bytes", c.Name)
		}
	}
}

func FuzzReadWireInPlace(f *testing.F) {
	v, _ := loadVector(f)
	for _, c := range v.Shapes {
		f.Add(goldentest.Hex(f, c.BeefHex))
	}
	f.Fuzz(func(t *testing.T, b []byte) { sameWire(t, b) })
}
