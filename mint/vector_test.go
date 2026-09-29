package mint_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// txVector is the part of testdata/vectors/transactions-v1.json this
// package builds: the funding tree, the state token's create and update
// transitions, and a payment, every fee paid from one output of the coin
// and every change sent to a literal P2PKH script. tools/vectors built them
// with go-sdk's primitives and templates called directly, under the fee rule
// the builders document.
type txVector struct {
	PrivateKeyHex   string `json:"privateKeyHex"`
	Originator      string `json:"originator"`
	SecurityLevel   int    `json:"securityLevel"`
	Protocol        string `json:"protocol"`
	StateKeyID      string `json:"stateKeyId"`
	SatPerByte      uint64 `json:"satPerByte"`
	Floor           uint64 `json:"floor"`
	PayScriptHex    string `json:"payScriptHex"`
	ChangeScriptHex string `json:"changeScriptHex"`
	Coin            struct {
		TxHex   string `json:"txHex"`
		BumpHex string `json:"bumpHex"`
	} `json:"coin"`
	FundingLockHex string `json:"fundingLockHex"`
	FundingTree    struct {
		FeeVout uint32 `json:"feeVout"`
		Count   int    `json:"count"`
		Sats    uint64 `json:"sats"`
		FeeSats uint64 `json:"feeSats"`
		TxHex   string `json:"txHex"`
	} `json:"fundingTree"`
	Create  vectorTransition `json:"create"`
	Update  vectorTransition `json:"update"`
	Payment struct {
		FeeVout       uint32 `json:"feeVout"`
		Sats          uint64 `json:"sats"`
		DestScriptHex string `json:"destScriptHex"`
		FeeSats       uint64 `json:"feeSats"`
		TxHex         string `json:"txHex"`
	} `json:"payment"`
}

type vectorTransition struct {
	PrevVout *uint32 `json:"prevVout"`
	FeeVout  uint32  `json:"feeVout"`
	Sats     uint64  `json:"sats"`
	LockHex  string  `json:"lockHex"`
	FeeSats  uint64  `json:"feeSats"`
	TxHex    string  `json:"txHex"`
}

func lockOf(t *testing.T, s string) *script.Script {
	t.Helper()
	out, err := script.NewFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every builder here must produce the independent vector's transaction byte
// for byte from the vector's inputs: the funding tree, whose size puts its
// fee above the floor; the create transition; the update, which spends the
// created token through the state derivation's unlocker; and the payment,
// whose fee is the floor.
func TestIndependentVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "transactions-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v txVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	seed := goldentest.Fill(0x42)
	if v.PrivateKeyHex != hex.EncodeToString(seed[:]) {
		t.Fatal("the vector was not built with goldentest.FixedKey")
	}
	ctx := context.Background()
	payer, err := p2pkh.Unlock(goldentest.FixedKey(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The coin is mined, so script verification stops at it rather than
	// asking for the parent of its stand-in input.
	coin := goldentest.Tx(t, v.Coin.TxHex)
	if coin.MerklePath, err = transaction.NewMerklePathFromHex(v.Coin.BumpHex); err != nil {
		t.Fatal(err)
	}
	for i, out := range coin.Outputs {
		if out.LockingScript.String() != v.PayScriptHex {
			t.Fatalf("coin output %d is not the fixed key's P2PKH", i)
		}
	}
	fee := func(vout uint32) mint.Input { return mint.Input{Tx: coin, Vout: vout, Unlocker: payer} }
	change := lockOf(t, v.ChangeScriptHex)
	fees := mint.Fees{SatPerByte: v.SatPerByte, Floor: v.Floor}
	check := func(name string, tx *transaction.Transaction, err error, wantHex string, wantFee uint64) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tx.Hex() != wantHex {
			t.Errorf("%s differs from the independent vector:\n got %s\nwant %s", name, tx.Hex(), wantHex)
		}
		if got := paid(tx); got != wantFee {
			t.Errorf("%s pays fee %d, want %d", name, got, wantFee)
		}
		verifies(t, tx)
	}

	ft := v.FundingTree
	tree, err := mint.FundingTree(lockOf(t, v.FundingLockHex), ft.Count, ft.Sats, fee(ft.FeeVout), change, fees)
	check("funding tree", tree, err, ft.TxHex, ft.FeeSats)

	if v.Create.PrevVout != nil || v.Update.PrevVout == nil {
		t.Fatal("the vector's create must spend no token and its update must spend one")
	}
	create, err := mint.Transition(lockOf(t, v.Create.LockHex), v.Create.Sats, nil, fee(v.Create.FeeVout), change, fees)
	check("create", create, err, v.Create.TxHex, v.Create.FeeSats)

	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	state := pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevel(v.SecurityLevel), Protocol: v.Protocol},
		KeyID:    v.StateKeyID,
	}
	// The update spends the vector's create, not the one built above, so a
	// difference in one case is reported once and not again in the next.
	created := goldentest.Tx(t, v.Create.TxHex)
	created.Inputs[0].SourceTransaction = coin
	prev := &mint.Input{Tx: created, Vout: *v.Update.PrevVout, Unlocker: state.Unlocker(ctx, w, v.Originator)}
	update, err := mint.Transition(lockOf(t, v.Update.LockHex), v.Update.Sats, prev, fee(v.Update.FeeVout), change, fees)
	check("update", update, err, v.Update.TxHex, v.Update.FeeSats)

	pay, err := mint.Payment(ctx, lockOf(t, v.Payment.DestScriptHex), v.Payment.Sats, fee(v.Payment.FeeVout), change, fees)
	check("payment", pay, err, v.Payment.TxHex, v.Payment.FeeSats)
}
