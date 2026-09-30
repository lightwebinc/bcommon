package guard

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

type beefVector struct {
	Cases []struct {
		Name      string `json:"name"`
		Accept    bool   `json:"accept"`
		SDKParses bool   `json:"sdkParses"`
		BeefHex   string `json:"beefHex"`
	} `json:"cases"`
}

func readBEEFVector(t testing.TB) beefVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "beef-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v beefVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	return v
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every case of the shared BEEF vector: the forms a reader admits pass the
// guard and parse, and every other is refused by the guard itself, as
// ErrBEEF, whatever go-sdk would have made of it.
func TestBEEFIndependentVector(t *testing.T) {
	var accepted, stricter int
	for _, c := range readBEEFVector(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			b := unhex(t, c.BeefHex)
			err := CheckBEEF(b, DefaultBound)
			if c.Accept != (err == nil) || (err != nil && !errors.Is(err, ErrBEEF)) {
				t.Fatalf("CheckBEEF: %v, accept %v", err, c.Accept)
			}
			beef, tx, txid, err := ParseBEEF(b, DefaultBound)
			if c.Accept {
				if err != nil || beef == nil || tx == nil || txid == nil || !tx.TxID().IsEqual(txid) {
					t.Fatalf("ParseBEEF: %v", err)
				}
				accepted++
				return
			}
			if !errors.Is(err, ErrBEEF) {
				t.Fatalf("ParseBEEF: %v, want the guard's refusal", err)
			}
			if c.SDKParses {
				stricter++
			}
		})
	}
	if accepted == 0 || stricter == 0 {
		t.Fatalf("accepted %d, refused where go-sdk parses %d: the vector no longer covers both", accepted, stricter)
	}
}

// hostile are the inputs that have ended a process: 13 bytes declaring 2^63
// proofs, and a BEEF around the seven-byte BUMP that declares four billion
// leaves.
var hostile = map[string][]byte{
	"2^63 BUMPs":           {0x01, 0x00, 0xbe, 0xef, 0xff, 0, 0, 0, 0, 0, 0, 0, 0x80},
	"2^32-1 leaves":        {0x01, 0x00, 0xbe, 0xef, 0x01, 0x5a, 0x01, 0xfe, 0xff, 0xff, 0xff, 0xff},
	"2^32-1 transactions":  {0x02, 0x00, 0xbe, 0xef, 0x00, 0xfe, 0xff, 0xff, 0xff, 0xff},
	"atomic, 2^63 BUMPs":   append(append([]byte{0x01, 0x01, 0x01, 0x01}, make([]byte, 32)...), 0x01, 0x00, 0xbe, 0xef, 0xff, 0, 0, 0, 0, 0, 0, 0, 0x80),
	"2^64-1 input scripts": append([]byte{0x02, 0x00, 0xbe, 0xef, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}, append(make([]byte, 36), append([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 16)...)...)...),
}

// The guard refuses each hostile input itself, so the SDK never sees it, and
// the whole refusal allocates next to nothing. Neither depends on which
// go-sdk is linked: an SDK that sized a slice from these counts is never
// asked to.
func TestHostileBEEFIsRefusedBeforeTheSDK(t *testing.T) {
	for name, b := range hostile {
		if len(b) > 80 {
			t.Fatalf("%s: %d bytes is not a small hostile input", name, len(b))
		}
		_, _, _, err := ParseBEEF(b, DefaultBound)
		if !errors.Is(err, ErrBEEF) {
			t.Fatalf("%s: %v, want the guard's refusal", name, err)
		}
		const runs = 100
		got := allocated(func() {
			for range runs {
				_, _, _, _ = ParseBEEF(b, DefaultBound)
			}
		}) / runs
		if got > 1024 {
			t.Fatalf("%s: refusing it allocated %d bytes", name, got)
		}
	}
	if len(hostile["2^63 BUMPs"]) != 13 {
		t.Fatal("the 2^63 case is the 13-byte one")
	}
}

// The walk allocates nothing on any input, admitted or refused but for the
// refusal's own error value, so the guard's cost is bounded by the input's
// length alone.
func TestCheckBEEFAllocatesNothing(t *testing.T) {
	for _, c := range readBEEFVector(t).Cases {
		b := unhex(t, c.BeefHex)
		if !c.Accept {
			continue
		}
		if n := testing.AllocsPerRun(20, func() { _ = CheckBEEF(b, DefaultBound) }); n != 0 {
			t.Fatalf("%s: %v allocations admitting it", c.Name, n)
		}
	}
}

// allocated is the bytes f allocates, measured over the heap as a whole.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The bound is the caller's, applied before anything is walked.
func TestCheckBEEFTakesTheCallersBound(t *testing.T) {
	var good []byte
	for _, c := range readBEEFVector(t).Cases {
		if c.Accept {
			good = unhex(t, c.BeefHex)
			break
		}
	}
	if err := CheckBEEF(good, len(good)); err != nil {
		t.Fatalf("a BEEF of exactly the bound: %v", err)
	}
	for _, bound := range []int{len(good) - 1, 0, -1} {
		if err := CheckBEEF(good, bound); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "max") {
			t.Fatalf("bound %d: %v", bound, err)
		}
	}
	if _, err := ParseTransaction(good, 10); !errors.Is(err, ErrTransaction) {
		t.Fatalf("a transaction over the bound: %v", err)
	}
}

// ParseTransaction walks one raw transaction the way a BEEF's are walked.
func TestParseTransaction(t *testing.T) {
	v := readBEEFVector(t)
	beef, tx, _, err := ParseBEEF(unhex(t, v.Cases[0].BeefHex), DefaultBound)
	if err != nil || beef == nil {
		t.Fatal(err)
	}
	raw := tx.Bytes()
	got, err := ParseTransaction(raw, DefaultBound)
	if err != nil || !got.TxID().IsEqual(tx.TxID()) {
		t.Fatalf("%v", err)
	}
	for name, b := range map[string][]byte{
		"empty":             nil,
		"truncated":         raw[:len(raw)-1],
		"trailing":          append(append([]byte{}, raw...), 0),
		"2^32-1 inputs":     append([]byte{1, 0, 0, 0, 0xfe, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)...),
		"no inputs":         append([]byte{1, 0, 0, 0, 0, 1}, make([]byte, 60)...),
		"extended format":   append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0xef}, make([]byte, 60)...),
		"a long out script": append(append([]byte{1, 0, 0, 0, 1}, make([]byte, 36)...), append([]byte{0, 0, 0, 0, 0, 1}, append(make([]byte, 8), 0xfe, 0xff, 0xff, 0xff, 0x7f)...)...),
	} {
		if _, err := ParseTransaction(b, DefaultBound); !errors.Is(err, ErrTransaction) {
			t.Errorf("%s: %v, want the guard's refusal", name, err)
		}
	}
}

// ParseBEEF never panics: every mutation of an admitted BEEF is an error or
// a BEEF.
func TestParseBEEFNeverPanics(t *testing.T) {
	for _, c := range readBEEFVector(t).Cases {
		if !c.Accept {
			continue
		}
		good := unhex(t, c.BeefHex)
		for i := range good {
			for _, b := range []byte{0x00, 0x01, 0x02, 0x7f, 0xfd, 0xfe, 0xff} {
				m := append([]byte{}, good...)
				m[i] = b
				_, _, _, _ = ParseBEEF(m, DefaultBound)
			}
			_, _, _, _ = ParseBEEF(good[:i], DefaultBound)
		}
	}
}

// FuzzCheckBEEF: the guard never panics, and whatever it admits the SDK
// parses without a panic (none is recovered here) and within a bounded
// allocation. The bound is loose, a multiple of the input, because what is
// asserted is that no count reaches the SDK unchecked: an unchecked one
// asks for gigabytes.
func FuzzCheckBEEF(f *testing.F) {
	for _, c := range readBEEFVector(f).Cases {
		f.Add(unhex(f, c.BeefHex))
	}
	for _, b := range hostile {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if CheckBEEF(b, DefaultBound) != nil {
			return
		}
		got := allocated(func() { _, _, _, _ = transaction.ParseBeef(b) })
		if limit := uint64(1<<20 + 512*len(b)); got > limit {
			t.Fatalf("the SDK allocated %d bytes for an admitted %d-byte BEEF", got, len(b))
		}
	})
}

// A transaction of no inputs is refused wherever it sits in a BEEF, in
// every form: the shape of a stand-in parent (a coin's output under the
// coin's txid) written out as if it were a transaction, and of a hostile
// subject. No producer of this module writes one (funding.BEEF refuses to),
// so none reaches a reader from a well-behaved peer.
func TestInputlessTransactionIsRefusedInEveryBEEF(t *testing.T) {
	// A pushed 100-byte script makes each transaction long enough that the
	// count floors pass and the walk reaches the inputs.
	pad := &script.Script{}
	if err := pad.AppendPushData(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	coinTxid := chainhash.Hash{0x0c}
	standIn := transaction.NewTransaction()
	standIn.AddOutput(&transaction.TransactionOutput{Satoshis: 700, LockingScript: pad})
	standIn.SetTxHash(&coinTxid)
	spender := transaction.NewTransaction()
	spender.AddInputFromTx(standIn, 0, nil)
	spender.Inputs[0].UnlockingScript = &script.Script{}
	spender.AddOutput(&transaction.TransactionOutput{Satoshis: 600, LockingScript: pad})

	lone := transaction.NewTransaction()
	lone.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: pad})

	forms := map[string]func() ([]byte, error){
		"stand-in parent, atomic": func() ([]byte, error) { return spender.AtomicBEEF(false) },
		"stand-in parent, V1":     spender.BEEF,
		"stand-in parent, V2": func() ([]byte, error) {
			b, err := transaction.NewBeefFromTransaction(spender)
			if err != nil {
				return nil, err
			}
			return b.Bytes()
		},
		"input-less subject, atomic": func() ([]byte, error) { return lone.AtomicBEEF(false) },
		"input-less subject, V1":     lone.BEEF,
	}
	for name, build := range forms {
		b, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := CheckBEEF(b, DefaultBound); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "no inputs") {
			t.Errorf("%s: CheckBEEF %v, want the no-inputs refusal", name, err)
		}
		if _, tx, _, err := ParseBEEF(b, DefaultBound); !errors.Is(err, ErrBEEF) || tx != nil {
			t.Errorf("%s: ParseBEEF %v, want the no-inputs refusal", name, err)
		}
	}
}
