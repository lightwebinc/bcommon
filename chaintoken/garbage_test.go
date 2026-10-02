package chaintoken_test

import (
	"errors"
	"testing"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/goldentest"
)

// shapes runs everything a host runs on a BEEF, and fails on anything but
// an answer or an ErrBEEF.
func shapes(t testing.TB, b []byte) {
	t.Helper()
	w, err := chaintoken.ReadWire(b, 0)
	if err != nil {
		if !errors.Is(err, chaintoken.ErrBEEF) {
			t.Fatalf("%x: refused with %v, want ErrBEEF", b, err)
		}
		return
	}
	tx := w.SubjectTx()
	if tx == nil {
		t.Fatalf("%x: read with no subject", b)
	}
	for _, err := range []error{
		func() error { _, err := w.Token(tx); return err }(),
		func() error { _, err := w.Carrier(tx); return err }(),
		w.Alone(tx),
	} {
		if err != nil && !errors.Is(err, chaintoken.ErrBEEF) {
			t.Fatalf("%x: a shape refused with %v, want ErrBEEF", b, err)
		}
	}
	if p, err := w.Token(tx); err == nil {
		for _, s := range chaintoken.Spends(tx, p) {
			chaintoken.ReadOutput(s.Output, s.Vout, 2)
		}
	}
	for i, o := range tx.Outputs {
		chaintoken.ReadOutput(o, uint32(i), 2)
	}
}

// Every truncation of every BEEF in the vector that reads is refused as a
// BEEF, and every single-byte change of every case is read or refused as
// one, and never panics.
func TestSurvivesEveryCutAndFlip(t *testing.T) {
	v, _ := loadVector(t)
	for _, c := range v.Shapes {
		b := goldentest.Hex(t, c.BeefHex)
		for n := 0; c.Reads && n < len(b); n++ {
			if _, err := chaintoken.ReadWire(b[:n], 0); !errors.Is(err, chaintoken.ErrBEEF) {
				t.Fatalf("%s cut at %d: %v", c.Name, n, err)
			}
		}
		for i := range b {
			m := append([]byte(nil), b...)
			m[i] ^= 0xff
			shapes(t, m)
			m[i] = b[i] ^ 0x01
			shapes(t, m)
		}
	}
}

func FuzzReadWire(f *testing.F) {
	v, _ := loadVector(f)
	for _, c := range v.Shapes {
		f.Add(goldentest.Hex(f, c.BeefHex))
	}
	f.Fuzz(func(t *testing.T, b []byte) { shapes(t, b) })
}
