package publish_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/publish"
)

// The object leg carries a BEEF and nothing else. Facade.Submit checks the
// marker and the topic before it builds a request, so the refusals below
// never reach the network (overlay.example.com is never contacted).
func ExampleFacade_Submit() {
	tx := transaction.NewTransaction()
	nowhere := chainhash.Hash{}
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	isTxid := true
	tx.MerklePath = transaction.NewMerklePath(1, [][]*transaction.PathElement{{
		{Offset: 0, Hash: tx.TxID(), Txid: &isTxid},
		{Offset: 1, Duplicate: &isTxid},
	}})
	beef, err := tx.BEEF()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("BEEF:", publish.IsBEEF(beef), "raw transaction:", publish.IsBEEF(tx.Bytes()))

	f := &publish.Facade{Base: "https://overlay.example.com"}
	ctx := context.Background()
	_, err = f.Submit(ctx, "tm_example", tx.Bytes())
	fmt.Println(errors.Is(err, publish.ErrNotBEEF))
	_, err = f.Submit(ctx, "tm_example,tm_other", beef)
	fmt.Println(err)
	// Output:
	// BEEF: true raw transaction: false
	// true
	// publish: topic "tm_example,tm_other" must be one name with no separators
}
