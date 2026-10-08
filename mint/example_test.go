package mint_test

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/verify"
)

// testCoin is a stand-in mined coin on a test chain that exists only in this
// process: one P2PKH output of 50,000 satoshis to the fixed test key, placed
// by a stand-in proof at offset 1 of a two-transaction block at height 90.
// The tracker knows that block's merkle root and nothing else, so SPV runs
// against it exactly as it would against a reader's own header source.
func testCoin() (*transaction.Transaction, *goldentest.Tracker, error) {
	addr, err := script.NewAddressFromPublicKey(goldentest.FixedKey().PubKey(), false)
	if err != nil {
		return nil, nil, err
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, nil, err
	}
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(goldentest.Fill(0x11))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: 50000, LockingScript: lock})
	sibling := chainhash.Hash(goldentest.Fill(0x33))
	isTxid := true
	coin.MerklePath = transaction.NewMerklePath(90, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: coin.TxID(), Txid: &isTxid},
	}})
	root, err := coin.MerklePath.ComputeRoot(coin.TxID())
	if err != nil {
		return nil, nil, err
	}
	return coin, &goldentest.Tracker{Roots: map[uint32]string{90: root.String()}, Tip: 100}, nil
}

// A funding tree on the test chain: four funding outputs of 1,000 satoshis
// under the application's funding lock, paid for by the coin, with the fee
// settled by the fee loop and the change sent back to the payer. The tree
// is not mined, so it verifies through its proven parent, and the state an
// application keeps about it carries the BEEF that proves it.
func ExampleFundingTree() {
	ctx := context.Background()
	coin, tracker, err := testCoin()
	if err != nil {
		fmt.Println(err)
		return
	}
	payer, err := p2pkh.Unlock(goldentest.FixedKey(), nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		fmt.Println(err)
		return
	}
	// The application's carrier parameters: its derivation and funding tag.
	// These are the test vectors' own, reserved for tests.
	params := carrier.Params{
		Derivation: pushdrop.Derivation{
			Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
			KeyID:    "object",
		},
		FundingTag: []byte{'v', 'x', 0x02},
	}
	lock, err := carrier.FundingLock(ctx, w, "example.com", params)
	if err != nil {
		fmt.Println(err)
		return
	}

	fee := mint.Input{Tx: coin, Vout: 0, Unlocker: payer}
	change := coin.Outputs[0].LockingScript
	tree, err := mint.FundingTree(lock, 4, 1000, fee, change, mint.DefaultFees)
	if err != nil {
		fmt.Println(err)
		return
	}
	for i, out := range tree.Outputs {
		_, isFunding := carrier.DecodeFunding(out.LockingScript, params.FundingTag)
		fmt.Printf("output %d: %d sats, funding=%t\n", i, out.Satoshis, isFunding)
	}
	fmt.Println("fee:", 50000-tree.TotalOutputSatoshis(), "sats for", tree.Size(), "bytes")

	verdict, err := verify.Check(ctx, tree, tracker)
	fmt.Println("proves through its parent:", verdict == verify.Passed, err)

	beef, err := funding.KeepBEEF(tree, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	state := funding.Tree{Txid: tree.TxID().String(), RawHex: tree.Hex(), BeefHex: beef, Sats: 1000, Count: 4}
	fmt.Println("carriers it can fund:", state.Remaining())
	// Output:
	// output 0: 1000 sats, funding=true
	// output 1: 1000 sats, funding=true
	// output 2: 1000 sats, funding=true
	// output 3: 1000 sats, funding=true
	// output 4: 45900 sats, funding=false
	// fee: 100 sats for 388 bytes
	// proves through its parent: true <nil>
	// carriers it can fund: 4
}
