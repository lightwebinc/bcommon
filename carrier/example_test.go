package carrier_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/verify"
)

// examplePrefix starts every payload of the example application: three
// bytes and a version byte of 1. A real application defines its own payload
// format; this one belongs to no application.
var examplePrefix = []byte{'v', 'x', 'r', 0x01}

// exampleParams are the example application's carrier parameters: the test
// vectors' derivation and funding tag, which are reserved for tests, and the
// payload's own rules.
func exampleParams() carrier.Params {
	return carrier.Params{
		Derivation: pushdrop.Derivation{
			Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
			KeyID:    "object",
		},
		FundingTag: []byte{'v', 'x', 0x02},
		ValidatePayload: func(p []byte) error {
			if !bytes.HasPrefix(p, examplePrefix) {
				return errors.New("example: not a version 1 payload")
			}
			return nil
		},
	}
}

// exampleClassify takes the outputs whose payload starts with the example
// prefix's first three bytes.
func exampleClassify(p []byte) (bool, error) {
	return bytes.HasPrefix(p, examplePrefix[:3]), nil
}

// testChainTree builds a test chain that exists only in this process: a
// stand-in mined coin at height 90, which the returned tracker holds the root
// of, and an unmined funding tree of four 1,000-satoshi outputs spending it.
func testChainTree(ctx context.Context, w wallet.Interface, p carrier.Params) (*transaction.Transaction, *goldentest.Tracker, error) {
	addr, err := script.NewAddressFromPublicKey(goldentest.FixedKey().PubKey(), false)
	if err != nil {
		return nil, nil, err
	}
	payTo, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, nil, err
	}
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(goldentest.Fill(0x11))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: 50000, LockingScript: payTo})
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
	tracker := &goldentest.Tracker{Roots: map[uint32]string{90: root.String()}, Tip: 100}

	payer, err := p2pkh.Unlock(goldentest.FixedKey(), nil)
	if err != nil {
		return nil, nil, err
	}
	lock, err := carrier.FundingLock(ctx, w, "example.com", p)
	if err != nil {
		return nil, nil, err
	}
	tree, err := mint.FundingTree(lock, 4, 1000, mint.Input{Tx: coin, Vout: 0, Unlocker: payer}, payTo, mint.DefaultFees)
	if err != nil {
		return nil, nil, err
	}
	return tree, tracker, nil
}

// A producer mints a carrier for a payload from one output of its funding
// tree; the carrier decodes, validates against the producer's identity key,
// and proves through the tree to the test chain's header, all offline.
func ExampleMint() {
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		fmt.Println(err)
		return
	}
	p := exampleParams()
	tree, tracker, err := testChainTree(ctx, w, p)
	if err != nil {
		fmt.Println(err)
		return
	}

	payload := append(append([]byte{}, examplePrefix...), "an object"...)
	tx, err := carrier.Mint(ctx, w, "example.com", p, payload, tree, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("locktime:", tx.LockTime, "sequence:", tx.Inputs[0].SequenceNumber)
	fmt.Println("output 0:", tx.Outputs[0].Satoshis, "sats, fee:", tree.Outputs[0].Satoshis-tx.TotalOutputSatoshis())

	c, err := carrier.Decode(tx, exampleClassify)
	if err != nil {
		fmt.Println(err)
		return
	}
	identity := goldentest.FixedKey().PubKey().Compressed()
	fmt.Printf("payload %q validates: %v\n", c.Payload, c.Validate(p, identity))

	// The commitment is the txid in hash byte order, which is the reverse
	// of the display hex.
	commitment := carrier.Commitment(tx)
	fmt.Println("commitment is the txid:", chainhash.Hash(commitment).String() == tx.TxID().String())

	verdict, err := verify.Check(ctx, tx, tracker)
	fmt.Println("proves through its funding tree:", verdict == verify.Passed, err)

	// The same carrier checked against somebody else's identity key.
	_, other := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x07}, 32))
	err = c.Validate(p, other.Compressed())
	fmt.Println("another identity:", errors.Is(err, carrier.ErrLock))
	// Output:
	// locktime: 4102444800 sequence: 0
	// output 0: 1000 sats, fee: 0
	// payload "vxr\x01an object" validates: <nil>
	// commitment is the txid: true
	// proves through its funding tree: true <nil>
	// another identity: true
}
