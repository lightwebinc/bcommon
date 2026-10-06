package guard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// testBound stands in for a caller's answer bound. The guard has no bound of
// its own; this one is simply far above any proof built here.
const testBound = 1 << 20

// goodBump is a one-level BUMP placing a fixed txid at offset 0 beside one
// sibling.
func goodBump(t *testing.T) []byte {
	t.Helper()
	txid, err := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := chainhash.NewHashFromHex(strings.Repeat("5a", 32))
	if err != nil {
		t.Fatal(err)
	}
	isTxid := true
	return (&transaction.MerklePath{
		BlockHeight: 0x1234,
		Path: [][]*transaction.PathElement{{
			{Offset: 0, Hash: txid, Txid: &isTxid},
			{Offset: 1, Hash: sibling},
		}},
	}).Bytes()
}

func TestGuardAcceptsAWellFormedBump(t *testing.T) {
	if err := guardBUMP(goodBump(t), testBound, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}

func TestGuardRejectsWhatTheBytesCannotEncode(t *testing.T) {
	good := goodBump(t)
	cases := map[string]struct {
		body []byte
		want string
	}{
		// Level 0 declares 4,294,967,295 leaves in seven bytes: the exact
		// shape that asks a naive reader for a fatal allocation.
		"absurd leaf count": {[]byte{0x65, 0x01, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF}, "declares"},
		"64-bit leaf count": {[]byte{0x65, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F}, "declares"},
		"tree height 65":    {[]byte{0x65, 65, 0x01}, "tree height"},
		"truncated":         {good[:len(good)-5], "mid-structure"},
		"empty":             {nil, "mid-structure"},
		"trailing":          {append(append([]byte{}, good...), 0x00), "trailing"},
	}
	for name, c := range cases {
		err := guardBUMP(c.body, testBound, DefaultLimits())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}

// The bound is the caller's and is applied before anything is walked: a
// proof of exactly the bound passes, a bound one byte under the proof refuses
// with both sizes named, and a bound of zero admits nothing.
func TestParseBUMPTakesTheCallersBound(t *testing.T) {
	good := goodBump(t)
	n := len(good)
	if _, err := ParseBUMP(good, n); err != nil {
		t.Fatalf("a proof of exactly the bound was refused: %v", err)
	}
	for _, bound := range []int{n - 1, 0, -1} {
		_, err := ParseBUMP(good, bound)
		want := fmt.Sprintf("bump is %d bytes, max %d", n, bound)
		if err == nil || err.Error() != want {
			t.Errorf("bound %d: err %v, want %q", bound, err, want)
		}
	}
	if _, err := ParseBUMP(nil, 0); err == nil {
		t.Error("an empty body under a zero bound was admitted")
	}
	// Over the bound and malformed at once: the size is the refusal, so an
	// over-bound body is never walked. A walk first would name the leaf
	// count instead.
	absurd := []byte{0x65, 0x01, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ParseBUMP(absurd, 3); err == nil || err.Error() != "bump is 7 bytes, max 3" {
		t.Errorf("over the bound and malformed: err %v, want exactly %q", err, "bump is 7 bytes, max 3")
	}
}

// ParseBUMP never panics: every mutation of a valid proof is an error or a
// proof, and the process is still here to say which.
func TestParseBUMPNeverPanics(t *testing.T) {
	good := goodBump(t)
	if _, err := ParseBUMP(good, testBound); err != nil {
		t.Fatal(err)
	}
	for i := range good {
		for _, b := range []byte{0x00, 0x01, 0x7F, 0xFD, 0xFE, 0xFF} {
			m := append([]byte{}, good...)
			m[i] = b
			_, _ = ParseBUMP(m, testBound)
		}
		_, _ = ParseBUMP(good[:i], testBound)
	}
}
