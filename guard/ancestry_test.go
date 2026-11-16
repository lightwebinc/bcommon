package guard

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/util"
)

// chain is a proven transaction and n more, each spending the one before:
// with fan inputs each, from that many of its outputs.
func chain(n, fan int) []*transaction.Transaction {
	lock := script.Script{script.OpTRUE}
	root := transaction.NewTransaction()
	root.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{1}, UnlockingScript: &script.Script{}})
	for range fan {
		root.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
	}
	isTxid := true
	root.MerklePath = transaction.NewMerklePath(800, [][]*transaction.PathElement{{{Offset: 0, Hash: root.TxID(), Txid: &isTxid}}})
	txs := []*transaction.Transaction{root}
	for range n {
		prev := txs[len(txs)-1]
		tx := transaction.NewTransaction()
		for v := range fan {
			tx.AddInput(&transaction.TransactionInput{SourceTXID: prev.TxID(), SourceTxOutIndex: uint32(v), UnlockingScript: &script.Script{}}) //nolint:gosec // small
		}
		for range fan {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
		}
		txs = append(txs, tx)
	}
	return txs
}

// encode writes txs as a BEEF by hand: the SDK's own writer walks the
// ancestry, which is the walk under test. txs[0] carries the one proof.
func encode(version uint32, txs []*transaction.Transaction, order func(i int) int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, version)
	b = append(b, 1)
	b = append(b, txs[0].MerklePath.Bytes()...)
	b = append(b, util.VarInt(uint64(len(txs))).Bytes()...)
	for i := range txs {
		j := order(i)
		raw := txs[j].Bytes()
		switch {
		case version == beefV1 && j == 0:
			b = append(append(b, raw...), 1, 0)
		case version == beefV1:
			b = append(append(b, raw...), 0)
		case j == 0:
			b = append(append(b, formatRawTxBump, 0), raw...)
		default:
			b = append(append(b, formatRawTx), raw...)
		}
	}
	return b
}

func atomic(subject *transaction.Transaction, beef []byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, beefAtomic)
	b = append(b, subject.TxID()[:]...)
	return append(b, beef...)
}

func inOrder(i int) int { return i }

// A few kilobytes of transactions, each spending two outputs of the one
// before, ask whoever walks their ancestry for 2^n steps. The count of
// transactions does not bound that; Limits.Ancestry does, in every BEEF
// form and whatever order the transactions are written in.
func TestAncestryIsBounded(t *testing.T) {
	txs := chain(64, 2)
	last := len(txs) - 1
	reversed := func(i int) int {
		if i == 0 {
			return 0
		}
		return len(txs) - i
	}
	for name, b := range map[string][]byte{
		"V1":             encode(beefV1, txs, inOrder),
		"V2":             encode(beefV2, txs, inOrder),
		"V2 reversed":    encode(beefV2, txs, reversed),
		"Atomic over V1": atomic(txs[last], encode(beefV1, txs, inOrder)),
		"Atomic over V2": atomic(txs[last], encode(beefV2, txs, inOrder)),
	} {
		if len(b) > 16<<10 {
			t.Fatalf("%s: %d bytes is not a small input", name, len(b))
		}
		start := time.Now()
		_, _, _, err := ParseBEEF(b, DefaultBound)
		if !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "ancestry") {
			t.Fatalf("%s: %v, want the ancestry refusal", name, err)
		}
		if el := time.Since(start); el > time.Second {
			t.Fatalf("%s: refused after %s", name, el)
		}
		// With no limit the walk admits it, as before.
		if err := CheckBEEFWithin(b, DefaultBound, Limits{}); err != nil {
			t.Fatalf("%s without a limit: %v", name, err)
		}
	}
}

// What stays inside the limit is admitted and parses: a long line of
// single-input transactions, which is linear however long, and a short run
// of branching ones.
func TestAncestryAdmitsOrdinaryShapes(t *testing.T) {
	for name, txs := range map[string][]*transaction.Transaction{
		"a line of 2000":    chain(2000, 1),
		"ten that branch":   chain(10, 2),
		"three, three wide": chain(3, 3),
	} {
		for _, v := range []uint32{beefV1, beefV2} {
			b := encode(v, txs, inOrder)
			if err := CheckBEEF(b, DefaultBound); err != nil {
				t.Fatalf("%s, version %#x: %v", name, v, err)
			}
		}
		if _, tx, _, err := ParseBEEF(encode(beefV2, txs, inOrder), DefaultBound); err != nil || tx == nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The count is exact at the boundary: n branching transactions over a
	// proven one take 2^(n+1)-1 steps.
	txs := chain(9, 2)
	b := encode(beefV2, txs, inOrder)
	if err := CheckBEEFWithin(b, DefaultBound, Limits{Ancestry: 1023}); err != nil {
		t.Fatalf("1023 steps under a limit of 1023: %v", err)
	}
	if err := CheckBEEFWithin(b, DefaultBound, Limits{Ancestry: 1022}); !errors.Is(err, ErrBEEF) {
		t.Fatalf("1023 steps under a limit of 1022: %v", err)
	}
}

// A walk that cannot exceed the limit, every unproven transaction having
// one input, allocates nothing for the count.
func TestAncestryCountAllocatesOnlyWhenItCanMatter(t *testing.T) {
	b := encode(beefV2, chain(50, 1), inOrder)
	if n := testing.AllocsPerRun(20, func() { _ = CheckBEEF(b, DefaultBound) }); n != 0 {
		t.Fatalf("%v allocations for a line of transactions", n)
	}
}

// A caller's small limit holds for a line of transactions too, which has no
// branch to count.
func TestAncestrySmallLimitOnALine(t *testing.T) {
	b := encode(beefV2, chain(2, 1), inOrder) // three steps to walk the tip
	if err := CheckBEEFWithin(b, DefaultBound, Limits{Ancestry: 3}); err != nil {
		t.Fatalf("three steps under a limit of three: %v", err)
	}
	if err := CheckBEEFWithin(b, DefaultBound, Limits{Ancestry: 2}); !errors.Is(err, ErrBEEF) {
		t.Fatalf("three steps under a limit of two: %v", err)
	}
}

// wideCount rewrites a transaction's input count, one byte at offset 4, in the
// three-byte form. The SDK reads it and names the transaction by its
// canonical bytes, so the bytes here hash to another id.
func wideCount(raw []byte) []byte {
	out := append([]byte{}, raw[:4]...)
	out = append(out, 0xFD, raw[4], 0x00)
	return append(out, raw[5:]...)
}

// A count written wider than its value needs gives a transaction two ids,
// and a count made over one of them misses the other. It is refused, in a
// BEEF and alone.
func TestWideCountsAreRefused(t *testing.T) {
	txs := chain(64, 2)
	b := binary.LittleEndian.AppendUint32(nil, beefV1)
	b = append(b, 1)
	b = append(b, txs[0].MerklePath.Bytes()...)
	b = append(b, util.VarInt(uint64(len(txs))).Bytes()...)
	b = append(append(b, txs[0].Bytes()...), 1, 0)
	for _, tx := range txs[1:] {
		b = append(append(b, wideCount(tx.Bytes())...), 0)
	}
	_, _, _, err := ParseBEEF(b, DefaultBound)
	if !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "shortest form") {
		t.Fatalf("a BEEF of wide counts: %v", err)
	}
	if _, err := ParseTransaction(wideCount(txs[1].Bytes()), DefaultBound); !errors.Is(err, ErrTransaction) {
		t.Fatalf("a transaction with a wide count: %v", err)
	}
	if _, err := ParseTransaction(txs[1].Bytes(), DefaultBound); err != nil {
		t.Fatalf("the same transaction written canonically: %v", err)
	}
}

// A transaction written twice could be counted as one copy and walked as
// the other: the branching chain, then its transactions again marked as
// proven, then a subject spending the tip.
func TestRepeatedTransactionIsRefused(t *testing.T) {
	txs := chain(64, 2)
	tip := txs[len(txs)-1]
	subject := transaction.NewTransaction()
	subject.AddInput(&transaction.TransactionInput{SourceTXID: tip.TxID(), UnlockingScript: &script.Script{}})
	subject.AddOutput(tip.Outputs[0])

	b := binary.LittleEndian.AppendUint32(nil, beefV1)
	b = append(b, 1)
	b = append(b, txs[0].MerklePath.Bytes()...)
	b = append(b, util.VarInt(uint64(2*len(txs))).Bytes()...)
	b = append(append(b, txs[0].Bytes()...), 1, 0)
	for _, tx := range txs[1:] {
		b = append(append(b, tx.Bytes()...), 0)
	}
	for _, tx := range txs[1:] {
		b = append(append(b, tx.Bytes()...), 1, 0)
	}
	b = append(append(b, subject.Bytes()...), 0)
	start := time.Now()
	_, _, _, err := ParseBEEF(b, DefaultBound)
	if !errors.Is(err, ErrBEEF) {
		t.Fatalf("%v, want the guard's refusal", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("refused after %s", el)
	}
}
