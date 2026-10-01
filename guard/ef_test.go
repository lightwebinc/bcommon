package guard

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// efFixture is a coinbase and a two-input spend of it, each with its
// previous outputs set, as go-sdk writes them in raw and Extended Format.
func efFixture(t testing.TB) (raws, efs [][]byte, txids []*chainhash.Hash) {
	t.Helper()
	lock := &script.Script{script.OpDUP, script.OpHASH160, script.OpDATA1, 0x2a, script.OpEQUALVERIFY, script.OpCHECKSIG}
	cb := transaction.NewTransaction()
	cb.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{}, SourceTxOutIndex: 0xffffffff,
		UnlockingScript: &script.Script{0x03, 0x01, 0x02, 0x03}, SequenceNumber: transaction.MaxTxInSequenceNum})
	// A coinbase spends nothing: a node writes a zero previous output.
	cb.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{LockingScript: &script.Script{}})
	cb.AddOutput(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: lock})
	cb.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: &script.Script{script.OpFALSE, script.OpRETURN}})

	spend := transaction.NewTransaction()
	for i := range 2 {
		in := &transaction.TransactionInput{SourceTXID: cb.TxID(), SourceTxOutIndex: uint32(i),
			UnlockingScript: &script.Script{script.OpDATA2, byte(i), 0x51}, SequenceNumber: transaction.MaxTxInSequenceNum}
		in.SetSourceTxOutput(cb.Outputs[i])
		spend.AddInput(in)
	}
	spend.AddOutput(&transaction.TransactionOutput{Satoshis: 4900, LockingScript: lock})
	spend.LockTime = 7

	for _, tx := range []*transaction.Transaction{cb, spend} {
		ef, err := tx.EF()
		if err != nil {
			t.Fatal(err)
		}
		raws, efs, txids = append(raws, tx.Bytes()), append(efs, ef), append(txids, tx.TxID())
	}
	return raws, efs, txids
}

func TestRawTransactionReadsExtendedFormat(t *testing.T) {
	raws, efs, txids := efFixture(t)
	for i, ef := range efs {
		if !IsEF(ef) || IsEF(raws[i]) {
			t.Fatalf("%d: IsEF", i)
		}
		// The guard refuses the EF bytes as a raw transaction...
		if _, err := ParseTransaction(ef, DefaultBound); !errors.Is(err, ErrTransaction) {
			t.Fatalf("%d: ParseTransaction read Extended Format: %v", i, err)
		}
		// ...and RawTransaction turns them into the raw form go-sdk writes.
		raw, err := RawTransaction(ef, DefaultBound)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if !bytes.Equal(raw, raws[i]) || len(raw) > len(ef) {
			t.Fatalf("%d: raw form %x, want %x", i, raw, raws[i])
		}
		tx, err := ParseTransaction(raw, DefaultBound)
		if err != nil || !tx.TxID().IsEqual(txids[i]) {
			t.Fatalf("%d: %v", i, err)
		}
		for _, in := range tx.Inputs {
			if in.SourceTxOutput() != nil {
				t.Fatalf("%d: a previous output survived", i)
			}
		}
		if n := testing.AllocsPerRun(20, func() { _, _ = RawTransaction(ef, DefaultBound) }); n > 2 {
			t.Fatalf("%d: %v allocations converting it", i, n)
		}
	}
}

// Raw bytes are walked as ParseTransaction walks them and returned as they
// came, allocating nothing.
func TestRawTransactionPassesRawThrough(t *testing.T) {
	raws, _, _ := efFixture(t)
	for i, raw := range raws {
		got, err := RawTransaction(raw, DefaultBound)
		if err != nil || len(got) != len(raw) || &got[0] != &raw[0] {
			t.Fatalf("%d: %v", i, err)
		}
		if n := testing.AllocsPerRun(20, func() { _, _ = RawTransaction(raw, DefaultBound) }); n != 0 {
			t.Fatalf("%d: %v allocations", i, n)
		}
		for _, b := range [][]byte{raw[:len(raw)-1], append(append([]byte{}, raw...), 0)} {
			if _, err := RawTransaction(b, DefaultBound); !errors.Is(err, ErrTransaction) {
				t.Fatalf("%d: a damaged raw transaction: %v", i, err)
			}
		}
	}
}

// efHead is a version and the Extended Format marker.
var efHead = []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0xef}

func efOf(parts ...[]byte) []byte {
	b := append([]byte{}, efHead...)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// One input with an empty unlocking script and sequence, before its previous
// output.
var efInput = append(make([]byte, 36), 0, 0xff, 0xff, 0xff, 0xff)

// hostileEF is Extended Format whose declared counts and lengths would ask
// an unchecked reader for far more than the bytes hold.
var hostileEF = map[string][]byte{
	"2^32-1 inputs":                 efOf([]byte{0xfe, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)),
	"2^64-1 inputs":                 efOf([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)),
	"no inputs":                     efOf([]byte{0}, make([]byte, 64)),
	"a long unlocking script":       efOf([]byte{1}, make([]byte, 36), []byte{0xfe, 0xff, 0xff, 0xff, 0x7f}, make([]byte, 30)),
	"a long previous locking":       efOf([]byte{1}, efInput, make([]byte, 8), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 20)),
	"2^63 outputs":                  efOf([]byte{1}, efInput, make([]byte, 8), []byte{0, 0xff, 0, 0, 0, 0, 0, 0, 0, 0x80}, make([]byte, 20)),
	"a long output script":          efOf([]byte{1}, efInput, make([]byte, 8), []byte{0, 1}, make([]byte, 8), []byte{0xfe, 0xff, 0xff, 0xff, 0xff}, make([]byte, 4)),
	"trailing bytes":                efOf([]byte{1}, efInput, make([]byte, 8), []byte{0, 0}, make([]byte, 4), []byte{0}),
	"no lock time":                  efOf([]byte{1}, efInput, make([]byte, 8), []byte{0, 0}, make([]byte, 3)),
	"marker alone":                  efHead,
	"marker and a short input list": efOf([]byte{1}, make([]byte, 20)),
}

func TestHostileExtendedFormatIsRefusedBoundedly(t *testing.T) {
	// The well-formed shape the cases damage parses.
	if _, err := RawTransaction(efOf([]byte{1}, efInput, make([]byte, 8), []byte{0, 0}, make([]byte, 4)), DefaultBound); err != nil {
		t.Fatalf("the undamaged shape: %v", err)
	}
	for name, b := range hostileEF {
		if !IsEF(b) {
			t.Fatalf("%s: not marked", name)
		}
		if _, err := RawTransaction(b, DefaultBound); !errors.Is(err, ErrTransaction) {
			t.Fatalf("%s: %v, want the guard's refusal", name, err)
		}
		const runs = 100
		got := allocated(func() {
			for range runs {
				_, _ = RawTransaction(b, DefaultBound)
			}
		}) / runs
		if got > 1024 {
			t.Fatalf("%s: refusing it allocated %d bytes", name, got)
		}
	}
	_, efs, _ := efFixture(t)
	for _, ef := range efs {
		for n := range len(ef) {
			if _, err := RawTransaction(ef[:n], DefaultBound); !errors.Is(err, ErrTransaction) {
				t.Fatalf("a truncation at %d: %v", n, err)
			}
		}
		if _, err := RawTransaction(ef, len(ef)-1); !errors.Is(err, ErrTransaction) {
			t.Fatalf("over the bound: %v", err)
		}
	}
}

// FuzzRawTransaction: the walk never panics, whatever it admits the guard
// parses as a raw transaction, and the raw form is never longer than what
// was given.
func FuzzRawTransaction(f *testing.F) {
	raws, efs, _ := efFixture(f)
	for i := range raws {
		f.Add(raws[i])
		f.Add(efs[i])
	}
	for _, b := range hostileEF {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		raw, err := RawTransaction(b, DefaultBound)
		if err != nil {
			return
		}
		if len(raw) > len(b) {
			t.Fatalf("raw form of %d bytes from %d", len(raw), len(b))
		}
		if _, err := ParseTransaction(raw, DefaultBound); err != nil {
			t.Fatalf("admitted, then refused as raw: %v", err)
		}
	})
}
