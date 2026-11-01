package funding_test

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/funding"
)

// A record made from a node's proof may flag the sibling leaf as a
// transaction id too, which the hosts refuse: a transaction rebuilt from
// its record carries the minimal path whatever the record holds.
func TestRebuildKeepsTheMinimalPath(t *testing.T) {
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{0x11}, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	id := tx.TxID()
	sibling := chainhash.DoubleHashH([]byte("the other transaction of the block"))
	flagged := true
	mp := transaction.NewMerklePath(5, [][]*transaction.PathElement{{
		{Offset: 0, Hash: id, Txid: &flagged},
		{Offset: 1, Hash: &sibling, Txid: &flagged},
	}})
	if chaintoken.MinimalPath(mp, *id) {
		t.Fatal("the fixture is minimal already")
	}
	rebuilt, err := funding.Rebuild(tx.Hex(), mp.Hex(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.MerklePath == nil || !chaintoken.MinimalPath(rebuilt.MerklePath, *id) {
		t.Fatalf("rebuilt with %+v, want the minimal path", rebuilt.MerklePath)
	}
	want, err := mp.ComputeRoot(id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rebuilt.MerklePath.ComputeRoot(id)
	if err != nil || *got != *want {
		t.Fatalf("the minimal path proves %v, want %v (%v)", got, want, err)
	}
}
