package guard

import (
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// wide is a transaction of n inputs and m outputs, each output's script
// scriptLen bytes, its inputs' parents unknown.
func wide(t testing.TB, n, m, scriptLen int) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	for i := range n {
		h := chainhash.Hash{byte(i), byte(i >> 8), 0x77}
		tx.AddInput(&transaction.TransactionInput{SourceTXID: &h, SourceTxOutIndex: uint32(i), UnlockingScript: &script.Script{}, SequenceNumber: 0xffffffff})
	}
	for range m {
		s := script.Script(make([]byte, scriptLen))
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &s})
	}
	return tx
}

// v1BEEF is a BEEF V1 of bumps and then txs, the last naming BUMP 0 when
// there is one. The guard reads structure, so no parent need be present.
func v1BEEF(bumps [][]byte, txs ...*transaction.Transaction) []byte {
	b := []byte{0x01, 0x00, 0xbe, 0xef, byte(len(bumps))}
	for _, p := range bumps {
		b = append(b, p...)
	}
	b = append(b, varIntOf(len(txs))...)
	for i, tx := range txs {
		b = append(b, tx.Bytes()...)
		if i == len(txs)-1 && len(bumps) > 0 {
			b = append(b, 1, 0)
		} else {
			b = append(b, 0)
		}
	}
	return b
}

// Each limit refuses the first count past it and admits the count at it,
// in a BEEF and in a raw transaction, as ErrBEEF and ErrTransaction, and a
// zero limit is no limit.
func TestLimitsRefuseOnlyPastTheLimit(t *testing.T) {
	tx := wide(t, 3, 4, 5)
	raw := tx.Bytes()
	beef := v1BEEF(nil, tx)
	cases := []struct {
		name string
		at   Limits
		past Limits
		word string
	}{
		{"inputs", Limits{Inputs: 3}, Limits{Inputs: 2}, "3 inputs, the limit is 2"},
		{"outputs", Limits{Outputs: 4}, Limits{Outputs: 3}, "4 outputs, the limit is 3"},
		{"script", Limits{Script: 5}, Limits{Script: 4}, "declares 5 bytes, the limit is 4"},
	}
	for _, c := range cases {
		if _, err := ParseTransactionWithin(raw, DefaultBound, c.at); err != nil {
			t.Errorf("%s at the limit: %v", c.name, err)
		}
		if _, err := ParseTransactionWithin(raw, DefaultBound, c.past); !errors.Is(err, ErrTransaction) || !strings.Contains(err.Error(), c.word) {
			t.Errorf("%s past the limit: %v, want %q", c.name, err, c.word)
		}
		if err := CheckBEEFWithin(beef, DefaultBound, c.at); err != nil {
			t.Errorf("%s at the limit in a BEEF: %v", c.name, err)
		}
		if _, _, _, err := ParseBEEFWithin(beef, DefaultBound, c.past); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), c.word) {
			t.Errorf("%s past the limit in a BEEF: %v, want %q", c.name, err, c.word)
		}
	}
	if _, err := ParseTransactionWithin(raw, DefaultBound, Limits{}); err != nil {
		t.Errorf("no limits: %v", err)
	}

	five := v1BEEF(nil, wide(t, 1, 1, 1), wide(t, 1, 1, 1), wide(t, 1, 1, 1), wide(t, 1, 1, 1), wide(t, 1, 1, 1))
	if err := CheckBEEFWithin(five, DefaultBound, Limits{Transactions: 5}); err != nil {
		t.Errorf("transactions at the limit: %v", err)
	}
	if err := CheckBEEFWithin(five, DefaultBound, Limits{Transactions: 4}); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "5 transactions, the limit is 4") {
		t.Errorf("transactions past the limit: %v", err)
	}
}

// A BUMP is held to BUMPBytes alone and inside a BEEF, and the BUMP count
// to BUMPs.
func TestLimitsOnProofs(t *testing.T) {
	bump := goodBump(t)
	n := len(bump)
	if _, err := ParseBUMPWithin(bump, testBound, Limits{BUMPBytes: n}); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if _, err := ParseBUMPWithin(bump, testBound, Limits{BUMPBytes: n - 1}); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Errorf("past the limit: %v", err)
	}
	mined := v1BEEF([][]byte{bump, bump}, wide(t, 1, 1, 1))
	if err := CheckBEEF(mined, DefaultBound); err != nil {
		t.Fatal(err)
	}
	if err := CheckBEEFWithin(mined, DefaultBound, Limits{BUMPs: 2, BUMPBytes: n}); err != nil {
		t.Errorf("BUMPs at the limit: %v", err)
	}
	if err := CheckBEEFWithin(mined, DefaultBound, Limits{BUMPs: 1}); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "2 BUMPs, the limit is 1") {
		t.Errorf("BUMPs past the limit: %v", err)
	}
	if err := CheckBEEFWithin(mined, DefaultBound, Limits{BUMPBytes: 1}); !errors.Is(err, ErrBEEF) || !strings.Contains(err.Error(), "the limit is 1") {
		t.Errorf("BUMP bytes past the limit: %v", err)
	}
}

// The functions without Limits apply DefaultLimits: a transaction one input
// past the default is refused by ParseTransaction, RawTransaction and
// CheckBEEF alike, and one at it is not.
func TestDefaultLimitsApplyEverywhere(t *testing.T) {
	if testing.Short() {
		t.Skip("builds transactions of 100,001 inputs")
	}
	d := DefaultLimits()
	for _, c := range []struct {
		n    int
		pass bool
	}{{d.Inputs, true}, {d.Inputs + 1, false}} {
		tx := wide(t, c.n, 1, 1)
		raw := tx.Bytes()
		_, err := ParseTransaction(raw, DefaultBound)
		if (err == nil) != c.pass {
			t.Errorf("ParseTransaction, %d inputs: %v", c.n, err)
		}
		_, err = RawTransaction(raw, DefaultBound)
		if (err == nil) != c.pass {
			t.Errorf("RawTransaction, %d inputs: %v", c.n, err)
		}
		if err := CheckBEEF(v1BEEF(nil, tx), DefaultBound); (err == nil) != c.pass {
			t.Errorf("CheckBEEF, %d inputs: %v", c.n, err)
		}
	}
}

// FuzzCheckBEEFWithin: under small limits the walk never panics, and what
// it admits the SDK parses to a BEEF within every limit.
func FuzzCheckBEEFWithin(f *testing.F) {
	for _, c := range readBEEFVector(f).Cases {
		f.Add(unhex(f, c.BeefHex), uint8(2), uint8(3), uint8(40))
	}
	for _, b := range hostile {
		f.Add(b, uint8(1), uint8(1), uint8(1))
	}
	f.Fuzz(func(t *testing.T, b []byte, count, io, scriptLen uint8) {
		l := Limits{Transactions: int(count), BUMPs: int(count), Inputs: int(io), Outputs: int(io), Script: int(scriptLen), BUMPBytes: 64 * int(scriptLen)}
		beef, _, _, err := ParseBEEFWithin(b, DefaultBound, l)
		if err != nil || beef == nil {
			return
		}
		if over(uint64(len(beef.BUMPs)), l.BUMPs) || over(uint64(len(beef.Transactions)), l.Transactions) {
			t.Fatalf("admitted %d BUMPs and %d transactions under %+v", len(beef.BUMPs), len(beef.Transactions), l)
		}
		for _, bt := range beef.Transactions {
			tx := bt.Transaction
			if tx == nil {
				continue
			}
			if over(uint64(len(tx.Inputs)), l.Inputs) || over(uint64(len(tx.Outputs)), l.Outputs) {
				t.Fatalf("admitted %d inputs and %d outputs under %+v", len(tx.Inputs), len(tx.Outputs), l)
			}
			for _, o := range tx.Outputs {
				if over(uint64(len(*o.LockingScript)), l.Script) {
					t.Fatalf("admitted a %d-byte script under %+v", len(*o.LockingScript), l)
				}
			}
		}
	})
}

// FuzzParseTransaction: the walk never panics, a refusal is ErrTransaction
// or the SDK's own, and whatever it admits the SDK parses within a bounded
// allocation and writes back byte for byte.
func FuzzParseTransaction(f *testing.F) {
	raws, _, _ := efFixture(f)
	for _, r := range raws {
		f.Add(r)
	}
	f.Add(wide(f, 3, 4, 5).Bytes())
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Fuzz(func(t *testing.T, b []byte) {
		tx, err := ParseTransaction(b, DefaultBound)
		if err != nil {
			return
		}
		// Measured again only once admitted: a refusal allocates nothing.
		got := allocated(func() { _, _ = ParseTransaction(b, DefaultBound) })
		if limit := uint64(1<<20 + 512*len(b)); got > limit {
			t.Fatalf("allocated %d bytes for an admitted %d-byte transaction", got, len(b))
		}
		if string(tx.Bytes()) != string(b) {
			t.Fatal("admitted, then written back differently")
		}
	})
}

// FuzzParseBUMP: the walk never panics, and whatever it admits the SDK
// parses within a bounded allocation.
func FuzzParseBUMP(f *testing.F) {
	f.Add(goodBumpBytes(f))
	f.Add([]byte{0xfd, 0x34, 0x12, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Add([]byte{0x00, 0x40, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := ParseBUMP(b, testBound); err != nil {
			return
		}
		got := allocated(func() { _, _ = ParseBUMP(b, testBound) })
		if limit := uint64(1<<20 + 512*len(b)); got > limit {
			t.Fatalf("allocated %d bytes for an admitted %d-byte BUMP", got, len(b))
		}
	})
}

// goodBumpBytes is goodBump for any testing.TB.
func goodBumpBytes(tb testing.TB) []byte {
	tb.Helper()
	txid := chainhash.Hash{0x11}
	sibling := chainhash.Hash{0x5a}
	isTxid := true
	return (&transaction.MerklePath{
		BlockHeight: 0x1234,
		Path: [][]*transaction.PathElement{{
			{Offset: 0, Hash: &txid, Txid: &isTxid},
			{Offset: 1, Hash: &sibling},
		}},
	}).Bytes()
}

// varIntOf is n as a Bitcoin VarInt.
func varIntOf(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	default:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	}
}
