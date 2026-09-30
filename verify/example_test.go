package verify_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
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

// The example application's payload is a four-byte prefix, the producer's
// 33-byte compressed identity key, and then the content. The format, like
// the derivation and tags below (which are the test vectors' own and
// reserved for tests), belongs to no application.
var examplePrefix = []byte{'v', 'x', 'r', 0x01}

func exampleSpec() verify.CarrierSpec {
	return verify.CarrierSpec{
		Params: carrier.Params{
			Derivation: pushdrop.Derivation{
				Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
				KeyID:    "object",
			},
			FundingTag: []byte{'v', 'x', 0x02},
			ValidatePayload: func(p []byte) error {
				if !bytes.HasPrefix(p, examplePrefix) || len(p) < len(examplePrefix)+33 {
					return errors.New("example: not a version 1 payload")
				}
				return nil
			},
		},
		Classify: func(p []byte) (bool, error) { return bytes.HasPrefix(p, examplePrefix[:3]), nil },
		IdentityOf: func(p []byte) ([]byte, error) {
			if len(p) < len(examplePrefix)+33 {
				return nil, errors.New("example: payload names no identity")
			}
			return p[len(examplePrefix) : len(examplePrefix)+33], nil
		},
		// The reader's own expectations of the payload: none here.
		Expect: func([]byte) (verify.Code, string) { return "", "" },
	}
}

// testChainCarrier mints a carrier on a test chain that exists only in this
// process: a stand-in mined coin at height 90, whose block root the returned
// tracker holds, an unmined funding tree spending it, and a carrier spending
// the tree's first output.
func testChainCarrier(ctx context.Context, content string) (*transaction.Transaction, *goldentest.Tracker, error) {
	key := goldentest.FixedKey()
	w, err := wallet.NewCompletedProtoWallet(key)
	if err != nil {
		return nil, nil, err
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
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

	p := exampleSpec().Params
	payer, err := p2pkh.Unlock(key, nil)
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
	payload := append(append(append([]byte{}, examplePrefix...), key.PubKey().Compressed()...), content...)
	tx, err := carrier.Mint(ctx, w, "example.com", p, payload, tree, 0)
	if err != nil {
		return nil, nil, err
	}
	return tx, &goldentest.Tracker{Roots: map[uint32]string{90: root.String()}, Tip: 100}, nil
}

// A reader asked a host for the carrier a token commits to, and the host
// answered one output as a BEEF. VerifyCarrier parses the BEEF, checks the
// commitment first, validates the carrier, and proves its funding parent in
// the reader's own header source. Every refusal is a code a caller can
// branch on; ERROR is kept apart from them, so an outage is never read as a
// forgery.
func ExampleVerifyCarrier() {
	ctx := context.Background()
	tx, tracker, err := testChainCarrier(ctx, "an object")
	if err != nil {
		fmt.Println(err)
		return
	}
	beef, err := tx.BEEF()
	if err != nil {
		fmt.Println(err)
		return
	}
	want := carrier.Commitment(tx)
	answer := []verify.Item{{Beef: beef, OutputIndex: 0}}
	spec := exampleSpec()

	c, code, _, _ := verify.VerifyCarrier(ctx, answer, want, "record", spec, tracker)
	fmt.Printf("%s %q\n", code, c.Payload[len(examplePrefix)+33:])

	// A host that answers a different carrier than the one asked for.
	other := want
	other[0] ^= 0xff
	_, code, _, _ = verify.VerifyCarrier(ctx, answer, other, "record", spec, tracker)
	fmt.Println(code)

	// A BEEF cut short in transit is refused as undecodable, and so is a
	// hostile one: 13 bytes that declare 2^63 proofs. The guard walks every
	// BEEF before the SDK parses it and bounds every count it declares by
	// the bytes present, so neither asks for an allocation it cannot back.
	_, code, _, _ = verify.VerifyCarrier(ctx, []verify.Item{{Beef: beef[:len(beef)/2]}}, want, "record", spec, tracker)
	fmt.Println(code)
	hostile := []byte{0x01, 0x00, 0xbe, 0xef, 0xff, 0, 0, 0, 0, 0, 0, 0, 0x80}
	_, code, reason, _ := verify.VerifyCarrier(ctx, []verify.Item{{Beef: hostile}}, want, "record", spec, tracker)
	fmt.Println(code, reason)

	// A header source that does not hold the block the funding parent
	// proves against.
	_, code, _, _ = verify.VerifyCarrier(ctx, answer, want, "record", spec, &goldentest.Tracker{})
	fmt.Println(code)

	// No answer, and two answers where one is allowed.
	_, code, _, _ = verify.VerifyCarrier(ctx, nil, want, "record", spec, tracker)
	fmt.Println(code)
	_, code, _, _ = verify.VerifyCarrier(ctx, append(answer, answer[0]), want, "record", spec, tracker)
	fmt.Println(code)
	// Output:
	// VERIFIED "an object"
	// REFUSED-COMMIT
	// REFUSED-DECODE
	// REFUSED-DECODE record: BEEF does not parse: guard: BEEF refused: declares 9223372036854775808 BUMPs, 0 bytes remain
	// REFUSED-BUMP
	// NO-TOKEN
	// REFUSED-FORK
}

// Check runs SPV against the tracker it is handed and never against a
// default.
func ExampleCheck() {
	ctx := context.Background()
	tx, tracker, err := testChainCarrier(ctx, "an object")
	if err != nil {
		fmt.Println(err)
		return
	}
	verdict, err := verify.Check(ctx, tx, tracker)
	fmt.Println(verdict == verify.Passed, err)

	_, err = verify.Check(ctx, tx, nil)
	fmt.Println(err)
	// Output:
	// true <nil>
	// verify: no chain tracker; refusing to verify against a default
}
