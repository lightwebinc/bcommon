package producer

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
)

// A node's proof that flags the sibling leaf as a transaction id is cut to
// the minimal path before it is attached; a minimal one is left alone.
func TestMinimalProofCutsASiblingFlaggedAsTxid(t *testing.T) {
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1})
	id := tx.TxID()
	sib := chainhash.DoubleHashH([]byte("sibling"))
	yes := true
	mp := &transaction.MerklePath{BlockHeight: 7642, Path: [][]*transaction.PathElement{{
		{Offset: 0, Hash: id, Txid: &yes},
		{Offset: 1, Hash: &sib, Txid: &yes},
	}}}
	if chaintoken.MinimalPath(mp, *id) {
		t.Fatal("the fixture is already minimal")
	}
	got := minimalProof(mp, tx)
	if !chaintoken.MinimalPath(got, *id) {
		t.Fatalf("attached proof is not minimal: %+v", got.Path[0])
	}
	r1, err1 := got.ComputeRoot(id)
	r2, err2 := mp.ComputeRoot(id)
	if err1 != nil || err2 != nil || r1.String() != r2.String() {
		t.Fatalf("the cut path proves another root: %v %v", err1, err2)
	}
	if minimalProof(got, tx) != got {
		t.Fatal("a minimal path is not returned as it came")
	}
	if minimalProof(nil, tx) != nil || minimalProof(mp, nil) != mp {
		t.Fatal("nil handling")
	}
}
