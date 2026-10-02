package testchain_test

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/testchain"
)

// A chain in the process: coinbase mined to the fixed test key, a spend of
// the first one sent and mined in a block of its own, and its proof
// verified against the chain as a tracker. Served over HTTP (it is an
// http.Handler), the same chain is a node, a broadcaster and a header
// source for the clients.
func ExampleChain() {
	ctx := context.Background()
	key := goldentest.FixedKey()
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		fmt.Println(err)
		return
	}
	c := testchain.New(700)
	if _, err := c.Generate(testchain.Maturity+1, addr.AddressString); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("tip:", c.Height())

	// The oldest coinbase is mature; the newest is not.
	var oldest, newest *transaction.Transaction
	for _, id := range c.Txids() {
		tx := c.Tx(id)
		_, h, _ := c.Proof(id)
		if h == 701 {
			oldest = tx
		}
		if h == c.Height() {
			newest = tx
		}
	}
	spend := func(coin *transaction.Transaction, fee uint64) *transaction.Transaction {
		unlock, _ := p2pkh.Unlock(key, nil)
		tx := transaction.NewTransaction()
		tx.AddInputFromTx(coin, 0, unlock)
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: coin.Outputs[0].Satoshis - fee, LockingScript: coin.Outputs[0].LockingScript})
		if err := tx.Sign(); err != nil {
			panic(err)
		}
		return tx
	}
	fmt.Println("young coinbase:", c.Send(spend(newest, 500)))

	tx := spend(oldest, 500)
	fmt.Println("mature coinbase:", c.Send(tx))
	mp, height, mined := c.Proof(tx.TxID().String())
	ok, err := mp.Verify(ctx, tx.TxID(), c)
	fmt.Println("mined:", mined, "at", height, "proof verifies:", ok, err)
	fmt.Println("the same output spent by another transaction:", c.Send(spend(oldest, 600)) != nil)
	// Output:
	// tip: 801
	// young coinbase: input 0 spends immature coinbase
	// mature coinbase: <nil>
	// mined: true at 802 proof verifies: true <nil>
	// the same output spent by another transaction: true
}
