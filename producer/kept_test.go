package producer_test

import (
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/producer"
)

// Every use of a kept transaction gets the same object, rebuilt once; a
// proof collected during the run reaches that object; a failed load is not
// remembered.
func TestKeptHandsOutOneCopy(t *testing.T) {
	parent := coinFor(&script.Script{script.OpTRUE}, 10, 0x71)
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(parent, 0, nil)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 9, LockingScript: &script.Script{script.OpTRUE}})
	beef, err := funding.KeepBEEF(tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := tx.TxID().String()
	loads, fail := 0, true
	k := &producer.Kept{Load: func(txid string) (*transaction.Transaction, error) {
		loads++
		if fail {
			return nil, errors.New("state unreadable")
		}
		if txid != id {
			return nil, errors.New(txid + " is not kept")
		}
		return funding.Rebuild(tx.Hex(), "", beef)
	}}

	if _, err := k.Tx(id); err == nil || err.Error() != "state unreadable" {
		t.Fatalf("a failed load: %v", err)
	}
	fail = false
	a, err := k.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Tx(id)
	if err != nil || a != b || loads != 2 {
		t.Fatalf("two copies, or a load per use: same %v, loads %d, %v", a == b, loads, err)
	}
	if _, err := k.Tx("other"); err == nil || err.Error() != "other is not kept" {
		t.Fatalf("the loader's own refusal: %v", err)
	}

	mp := proofAt(tx.TxID(), 95)
	k.Prove(id, mp)
	if a.MerklePath != mp {
		t.Fatal("the proof did not reach the copy handed out")
	}
	k.Prove("never loaded", mp) // nothing to reach, and no load
	if loads != 3 {
		t.Fatalf("Prove loaded something: %d loads", loads)
	}

	var none *producer.Kept
	none.Prove(id, mp)
	if _, err := none.Tx(id); err == nil {
		t.Fatal("a nil Kept answered")
	}
	if _, err := (&producer.Kept{}).Tx(id); err == nil {
		t.Fatal("a Kept with no loader answered")
	}
	nilTx := &producer.Kept{Load: func(string) (*transaction.Transaction, error) { return nil, nil }}
	if _, err := nilTx.Tx(id); err == nil {
		t.Fatal("a loader answering nothing was taken for a transaction")
	}
}
