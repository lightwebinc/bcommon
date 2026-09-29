package goldentest_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"runtime"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/bcommon/goldentest"
)

// Every vector generated from FixedKey carries keys derived from it, so a
// change to the key silently re-keys them all. The literal is the key's own
// public half, pinned here so the library notices without any vector.
func TestFixedKeyIsFrozen(t *testing.T) {
	k := goldentest.FixedKey()
	if got := hex.EncodeToString(k.Serialize()); got != "4242424242424242424242424242424242424242424242424242424242424242" {
		t.Fatalf("private key %s", got)
	}
	const pub = "0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c"
	if got := hex.EncodeToString(k.PubKey().Compressed()); got != pub {
		t.Fatalf("public key %s, want %s", got, pub)
	}
}

func TestHexAndTx(t *testing.T) {
	if got := goldentest.Hex(t, "00ff42"); !bytes.Equal(got, []byte{0x00, 0xff, 0x42}) {
		t.Fatalf("Hex = %x", got)
	}
	// No inputs and one empty output is enough: Tx only has to hand back
	// what the SDK parsed.
	const raw = "01000000000100000000000000000000000000"
	if got := goldentest.Tx(t, raw).Hex(); got != raw {
		t.Fatalf("Tx round trip %s", got)
	}
}

// fatalTB stands in for a test that Hex or Tx fails. It records the Fatal
// and ends the goroutine the way testing.T does, so the helper's failure
// path runs without failing the test around it. Anything else a helper
// reaches for is the nil embedded TB, and panics.
type fatalTB struct {
	testing.TB
	fatal string
}

func (f *fatalTB) Helper()           {}
func (f *fatalTB) Fatal(args ...any) { f.fatal = fmt.Sprint(args...); runtime.Goexit() }

// fatalOf runs fn against a fatalTB on a goroutine of its own, since Fatal
// ends the goroutine it is called on, and reports what Fatal was given and
// whether fn went on to return.
func fatalOf(fn func(testing.TB)) (msg string, returned bool) {
	f := &fatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(f)
		returned = true
	}()
	<-done
	return f.fatal, returned
}

// A vector that does not parse must stop the test at the helper, with the
// parser's reason, and never hand the caller a value to carry on with.
func TestHexAndTxFail(t *testing.T) {
	msg, returned := fatalOf(func(tb testing.TB) { goldentest.Hex(tb, "0g") })
	if returned || msg != "encoding/hex: invalid byte: U+0067 'g'" {
		t.Errorf("Hex: returned %v, Fatal %q", returned, msg)
	}
	msg, returned = fatalOf(func(tb testing.TB) { goldentest.Tx(tb, "01") })
	if returned || msg == "" {
		t.Errorf("Tx: returned %v, Fatal %q", returned, msg)
	}
}

// The tracker must answer an unknown height or a nil root as a header client
// does, (false, nil), so a test of an unproven parent sees "not proven"
// rather than an error that would take a different refusal path.
func TestTracker(t *testing.T) {
	ctx := context.Background()
	var h chainhash.Hash
	copy(h[:], bytes.Repeat([]byte{0xab}, 32))
	other := h
	other[0] ^= 1
	tr := &goldentest.Tracker{Roots: map[uint32]string{100: h.String()}, Tip: 110}

	for _, tc := range []struct {
		name   string
		root   *chainhash.Hash
		height uint32
		want   bool
	}{
		{"known root", &h, 100, true},
		{"other root", &other, 100, false},
		{"unknown height", &h, 101, false},
		{"nil root", nil, 100, false},
	} {
		ok, err := tr.IsValidRootForHeight(ctx, tc.root, tc.height)
		if err != nil || ok != tc.want {
			t.Errorf("%s: (%v, %v), want (%v, nil)", tc.name, ok, err, tc.want)
		}
	}
	if tip, err := tr.CurrentHeight(ctx); err != nil || tip != 110 {
		t.Fatalf("CurrentHeight = (%d, %v)", tip, err)
	}
}
