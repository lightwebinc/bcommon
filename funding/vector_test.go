package funding_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
)

// The funding tree of testdata/vectors/transactions-v1.json, with what
// tools/vectors kept of it: the Atomic BEEF it had while unmined (the tree
// and its proven parent, the coin) and the proof it has once mined. These
// are the three states an application writes down, so each must go through
// KeepBEEF, Rebuild and BumpHex unchanged.
func TestIndependentVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "transactions-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Coin struct {
			TxHex   string `json:"txHex"`
			BumpHex string `json:"bumpHex"`
		} `json:"coin"`
		FundingTree struct {
			Txid        string `json:"txid"`
			TxHex       string `json:"txHex"`
			KeptBeefHex string `json:"keptBeefHex"`
			BumpHex     string `json:"bumpHex"`
			RootHex     string `json:"rootHex"`
		} `json:"fundingTree"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	coin, err := transaction.NewTransactionFromHex(v.Coin.TxHex)
	if err != nil {
		t.Fatal(err)
	}
	if coin.MerklePath, err = transaction.NewMerklePathFromHex(v.Coin.BumpHex); err != nil {
		t.Fatal(err)
	}
	tree, err := transaction.NewTransactionFromHex(v.FundingTree.TxHex)
	if err != nil {
		t.Fatal(err)
	}
	tree.Inputs[0].SourceTransaction = coin

	kept, err := funding.KeepBEEF(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kept != v.FundingTree.KeptBeefHex {
		t.Errorf("kept BEEF differs from the independent vector:\n got %s\nwant %s", kept, v.FundingTree.KeptBeefHex)
	}
	unmined, err := funding.Rebuild("", "", v.FundingTree.KeptBeefHex)
	if err != nil {
		t.Fatal(err)
	}
	if unmined.Hex() != v.FundingTree.TxHex || unmined.MerklePath != nil {
		t.Errorf("the tree rebuilt from its kept BEEF is %s (proof %v)", unmined.Hex(), unmined.MerklePath != nil)
	}
	if p := unmined.Inputs[0].SourceTransaction; p == nil || p.Hex() != v.Coin.TxHex || p.MerklePath == nil {
		t.Error("the tree rebuilt from its kept BEEF lost its proven parent")
	}

	mined, err := funding.Rebuild(v.FundingTree.TxHex, v.FundingTree.BumpHex, v.FundingTree.KeptBeefHex)
	if err != nil {
		t.Fatal(err)
	}
	if mined.Hex() != v.FundingTree.TxHex || mined.MerklePath == nil {
		t.Fatalf("the tree rebuilt with its proof is %s (proof %v)", mined.Hex(), mined.MerklePath != nil)
	}
	if got := funding.BumpHex(mined.MerklePath); got != v.FundingTree.BumpHex {
		t.Errorf("BumpHex:\n got %s\nwant %s", got, v.FundingTree.BumpHex)
	}
	root, err := mined.MerklePath.ComputeRoot(mined.TxID())
	if err != nil {
		t.Fatal(err)
	}
	if mined.TxID().String() != v.FundingTree.Txid || root.String() != v.FundingTree.RootHex {
		t.Errorf("the rebuilt proof gives txid %s root %s, want %s and %s", mined.TxID(), root, v.FundingTree.Txid, v.FundingTree.RootHex)
	}
	if kept, err := funding.KeepBEEF(mined, mined.MerklePath); err != nil || kept != "" {
		t.Errorf("KeepBEEF kept %q (%v) for a proven tree", kept, err)
	}
}
