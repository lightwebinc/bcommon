package chaintoken_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// mineAt gives tx a stand-in proof at offset 1 of a two-transaction block
// at height, and tells the tracker that block's root.
func mineAt(tx *transaction.Transaction, height uint32, tracker *goldentest.Tracker) error {
	sibling := chainhash.Hash(goldentest.Fill(byte(height)))
	isTxid := true
	tx.MerklePath = transaction.NewMerklePath(height, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: tx.TxID(), Txid: &isTxid},
	}})
	root, err := tx.MerklePath.ComputeRoot(tx.TxID())
	if err != nil {
		return err
	}
	tracker.Roots[height] = root.String()
	return nil
}

// A chain of two tokens on a test chain, and what a host does with the
// second: it reads the BEEF as declared, holds it to the token's shape,
// finds the parent in it, and reads the predecessor from the parent, never
// from what it holds. The tag, the record and the derivation are the
// application's; here they are the ones reserved for tests.
func ExampleWire_Token() {
	ctx := context.Background()
	key := goldentest.FixedKey()
	w, err := wallet.NewCompletedProtoWallet(key)
	if err != nil {
		fmt.Println(err)
		return
	}
	state := pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
		KeyID:    "state",
	}
	tag := []byte{'v', 'x', 0x01}
	tracker := &goldentest.Tracker{Roots: map[uint32]string{}, Tip: 200}

	// A mined coin to pay the fees from.
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		fmt.Println(err)
		return
	}
	pay, err := p2pkh.Lock(addr)
	if err != nil {
		fmt.Println(err)
		return
	}
	payer, err := p2pkh.Unlock(key, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(goldentest.Fill(0x11))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: 50000, LockingScript: pay})
	if err := mineAt(coin, 90, tracker); err != nil {
		fmt.Println(err)
		return
	}

	// The first token, then the second, which spends it and pays its fee
	// from the first one's change. Each is mined in a block of its own.
	lock := func(record string) *script.Script {
		s, err := state.Lock(ctx, w, "example.com", [][]byte{tag, []byte(record)}, true)
		if err != nil {
			panic(err)
		}
		return s
	}
	first, err := mint.Transition(lock("state 0"), 1, nil, mint.Input{Tx: coin, Vout: 0, Unlocker: payer}, pay, mint.DefaultFees)
	if err != nil {
		fmt.Println(err)
		return
	}
	if err := mineAt(first, 110, tracker); err != nil {
		fmt.Println(err)
		return
	}
	second, err := mint.Transition(lock("state 1"), 1,
		&mint.Input{Tx: first, Vout: 0, Unlocker: state.Unlocker(ctx, w, "example.com")},
		mint.Input{Tx: first, Vout: 1, Unlocker: payer}, pay, mint.DefaultFees)
	if err != nil {
		fmt.Println(err)
		return
	}
	if err := mineAt(second, 120, tracker); err != nil {
		fmt.Println(err)
		return
	}

	// The publisher, or a replayer holding both as a host stores them,
	// assembles the one BEEF a host admits the second token in.
	beef, err := chaintoken.TokenBEEF(second, first)
	if err != nil {
		fmt.Println(err)
		return
	}

	// The host.
	wire, err := chaintoken.ReadWire(beef, chaintoken.DefaultMaxBEEF)
	if err != nil {
		fmt.Println(err)
		return
	}
	tx := wire.SubjectTx()
	parent, err := wire.Token(tx)
	fmt.Println("a token and its parent:", err == nil, "parent is the first token:", parent.Txid == *first.TxID())
	fmt.Println("both mined:", chaintoken.Mined(ctx, tx, tracker) && chaintoken.Mined(ctx, parent.Tx, tracker))

	lockingKey, err := state.ExpectedLockingKey(key.PubKey())
	if err != nil {
		fmt.Println(err)
		return
	}
	out, ok := chaintoken.ReadOutput(tx.Outputs[0], 0, 2)
	fmt.Println("two fields and a signature of 1 satoshi:", ok)
	fmt.Printf("record %q, canonical lock %t, signed %t\n", out.Fields[1],
		out.LockedTo(*tx.Outputs[0].LockingScript, lockingKey), out.SignedBy(lockingKey))

	// What it spends of the parent: the predecessor, and the change that
	// pays its fee. Only an output that reads as a token is one.
	for _, s := range chaintoken.Spends(tx, parent) {
		pred, isToken := chaintoken.ReadOutput(s.Output, s.Vout, 2)
		if isToken {
			fmt.Printf("input %d spends output %d: the predecessor, record %q\n", s.Input, s.Vout, pred.Fields[1])
		} else {
			fmt.Printf("input %d spends output %d: not a token\n", s.Input, s.Vout)
		}
	}

	// The second token alone, as a host stores it, is not admissible: the
	// rule needs the parent in the same BEEF. A reader reads it with
	// Stored.
	alone, err := second.AtomicBEEF(false)
	if err != nil {
		fmt.Println(err)
		return
	}
	wire, err = chaintoken.ReadWire(alone, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	_, err = wire.Token(wire.SubjectTx())
	fmt.Println("alone:", errors.Is(err, chaintoken.ErrBEEF))
	stored, err := chaintoken.Stored(alone, 0)
	fmt.Println("stored:", stored.TxID().String() == second.TxID().String(), err)
	// Output:
	// a token and its parent: true parent is the first token: true
	// both mined: true
	// two fields and a signature of 1 satoshi: true
	// record "state 1", canonical lock true, signed true
	// input 0 spends output 0: the predecessor, record "state 0"
	// input 1 spends output 1: not a token
	// alone: true
	// stored: true <nil>
}
