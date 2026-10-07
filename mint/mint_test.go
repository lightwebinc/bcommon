package mint_test

import (
	"context"
	"errors"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
)

// party is a P2PKH key: the lock script and the template that spends it.
// P2PKH belongs to no application, so what these tests establish is the
// builders' and the fee loop's, not any one application's lock.
type party struct {
	lock   *script.Script
	unlock transaction.UnlockingScriptTemplate
}

func newParty(t *testing.T, seed byte) party {
	t.Helper()
	b := goldentest.Fill(seed)
	k, _ := ec.PrivateKeyFromBytes(b[:])
	addr, err := script.NewAddressFromPublicKey(k.PubKey(), true)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := p2pkh.Unlock(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	return party{lock: lock, unlock: unlock}
}

// The holder of the state token, and the payer whose coin funds every fee
// and takes every change output.
func parties(t *testing.T) (holder, payer party) {
	t.Helper()
	return newParty(t, 0x07), newParty(t, 0x42)
}

// coin is a parent with one output of sats locked to p. It has no inputs, so
// script verification stops at it; each value gives a distinct txid.
func (p party) coin(sats uint64) mint.Input {
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: p.lock})
	return mint.Input{Tx: tx, Vout: 0, Unlocker: p.unlock}
}

// paid is what tx leaves to the fee: its inputs' values minus its outputs'.
func paid(tx *transaction.Transaction) uint64 {
	var in uint64
	for _, i := range tx.Inputs {
		in += i.SourceTxOutput().Satoshis
	}
	return in - tx.TotalOutputSatoshis()
}

// verifies runs every input's script through the SDK's interpreter, down the
// chain of parents the builders linked.
func verifies(t *testing.T, tx *transaction.Transaction) {
	t.Helper()
	if ok, err := spv.VerifyScripts(context.Background(), tx); err != nil || !ok {
		t.Fatalf("scripts do not verify: ok=%v err=%v", ok, err)
	}
}

func TestTransition(t *testing.T) {
	holder, payer := parties(t)
	fee := payer.coin(5000)
	created, err := mint.Transition(holder.lock, 1, nil, fee, payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	// A create spends only the fee input; the token is output 0.
	if len(created.Inputs) != 1 || !created.Inputs[0].SourceTXID.Equal(*fee.Tx.TxID()) {
		t.Fatalf("create: %d inputs, want the fee input alone", len(created.Inputs))
	}
	if len(created.Outputs) != 2 || created.Outputs[0].Satoshis != 1 || !created.Outputs[0].LockingScript.Equals(holder.lock) ||
		!created.Outputs[1].LockingScript.Equals(payer.lock) {
		t.Fatal("create: want the token at output 0 and change at output 1")
	}
	verifies(t, created)

	// An update spends its predecessor first, then the fee input.
	prev := &mint.Input{Tx: created, Vout: 0, Unlocker: holder.unlock}
	fee2 := payer.coin(5001)
	updated, err := mint.Transition(holder.lock, 1, prev, fee2, payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Inputs) != 2 ||
		!updated.Inputs[0].SourceTXID.Equal(*created.TxID()) || updated.Inputs[0].SourceTxOutIndex != 0 ||
		!updated.Inputs[1].SourceTXID.Equal(*fee2.Tx.TxID()) {
		t.Fatal("update: want the previous token then the fee input")
	}
	if !updated.Outputs[0].LockingScript.Equals(holder.lock) || updated.Outputs[0].Satoshis != 1 {
		t.Fatal("update: token is not output 0")
	}
	verifies(t, updated)
}

func TestFundingTree(t *testing.T) {
	holder, payer := parties(t)
	tree, err := mint.FundingTree(holder.lock, 8, 3, payer.coin(5000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Inputs) != 1 || len(tree.Outputs) != 9 {
		t.Fatalf("%d in %d out, want 1 in and 8 + change out", len(tree.Inputs), len(tree.Outputs))
	}
	for i := 0; i < 8; i++ {
		if tree.Outputs[i].Satoshis != 3 || !tree.Outputs[i].LockingScript.Equals(holder.lock) {
			t.Fatalf("output %d is not 3 satoshis under the lock", i)
		}
	}
	if !tree.Outputs[8].LockingScript.Equals(payer.lock) {
		t.Fatal("change is not the last output")
	}
	verifies(t, tree)
}

func TestPayment(t *testing.T) {
	holder, payer := parties(t)
	tx, err := mint.Payment(context.Background(), holder.lock, 1000, payer.coin(5000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Inputs) != 1 || len(tx.Outputs) != 2 ||
		tx.Outputs[0].Satoshis != 1000 || !tx.Outputs[0].LockingScript.Equals(holder.lock) ||
		tx.Outputs[1].Satoshis != 5000-1000-250 || !tx.Outputs[1].LockingScript.Equals(payer.lock) {
		t.Fatal("want 1000 to the destination, then 3750 of change")
	}
	verifies(t, tx)
}

// Under the floor the fee is the floor exactly: the first pass pays it and
// is accepted because the size asks for less. Above it the rate governs, and
// the change-drop rule compares change with the floor, inclusively.
func TestFeeFloor(t *testing.T) {
	ctx := context.Background()
	holder, payer := parties(t)
	for _, floor := range []uint64{250, 1000} {
		fees := mint.Fees{SatPerByte: 1, Floor: floor}
		tx, err := mint.Transition(holder.lock, 1, nil, payer.coin(5000), payer.lock, fees)
		if err != nil {
			t.Fatal(err)
		}
		if uint64(tx.Size()) >= floor {
			t.Fatalf("size %d is not under the floor %d, so this case no longer tests it", tx.Size(), floor)
		}
		if got := paid(tx); got != floor {
			t.Fatalf("floor %d: paid %d for %d bytes, want the floor", floor, got, tx.Size())
		}
	}

	// Ten satoshis a byte on eight outputs puts the fee far above the floor.
	// The first pass pays the floor and measures; the second pays the rate
	// on that size plus two bytes per input, and covers what it measures.
	// Built by hand, the first pass gives the exact fee, so a change to the
	// margin moves it.
	fees := mint.Fees{SatPerByte: 10, Floor: 250}
	fee := payer.coin(20000)
	tree, err := mint.FundingTree(holder.lock, 8, 1, fee, payer.lock, fees)
	if err != nil {
		t.Fatal(err)
	}
	first := transaction.NewTransaction()
	first.AddInputFromTx(fee.Tx, 0, payer.unlock)
	for i := 0; i < 8; i++ {
		first.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: holder.lock})
	}
	first.AddOutput(&transaction.TransactionOutput{Satoshis: 20000 - 8 - 250, LockingScript: payer.lock})
	if err := first.Sign(); err != nil {
		t.Fatal(err)
	}
	want := 10 * uint64(first.Size()+2*len(first.Inputs))
	if got := paid(tree); got != want || got < 10*uint64(tree.Size()) {
		t.Fatalf("paid %d for %d bytes at 10 sat/byte, want %d", got, tree.Size(), want)
	}

	// 5000 in, 4500 out and the 250 floor leave change of exactly the floor,
	// which is kept; one satoshi more to the destination leaves 249, which
	// is dropped and paid to the fee.
	at, err := mint.Payment(ctx, holder.lock, 4500, payer.coin(5000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(at.Outputs) != 2 || at.Outputs[1].Satoshis != 250 || paid(at) != 250 {
		t.Fatalf("change at the floor: %d outputs, paid %d, want it kept", len(at.Outputs), paid(at))
	}
	under, err := mint.Payment(ctx, holder.lock, 4501, payer.coin(5000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(under.Outputs) != 1 || paid(under) != 499 {
		t.Fatalf("change under the floor: %d outputs, paid %d, want it dropped", len(under.Outputs), paid(under))
	}
	verifies(t, at)
	verifies(t, under)
}

// Signature lengths vary by a byte or two between passes, and a transition
// with two inputs re-signs both each pass. Many transitions with different
// fee amounts exercise that variance; every one must converge, pay at least
// the policy, and verify.
func TestFeeConverges(t *testing.T) {
	holder, payer := parties(t)
	created, err := mint.Transition(holder.lock, 1, nil, payer.coin(5000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	prev := &mint.Input{Tx: created, Vout: 0, Unlocker: holder.unlock}
	for i := 0; i < 40; i++ {
		tx, err := mint.Transition(holder.lock, 1, prev, payer.coin(3000+uint64(i)*137), payer.lock, mint.LegacyFees)
		if err != nil {
			t.Fatalf("amount %d: %v", i, err)
		}
		if got := paid(tx); got < mint.LegacyFees.Floor || got < uint64(tx.Size()) {
			t.Fatalf("amount %d paid %d for %d bytes", i, got, tx.Size())
		}
		verifies(t, tx)
	}
}

// A fee input that cannot cover the outputs and the fee is refused, never
// under-paid. Exactly the outputs plus the fee is enough.
func TestInsufficient(t *testing.T) {
	ctx := context.Background()
	holder, payer := parties(t)

	exact, err := mint.Payment(ctx, holder.lock, 1000, payer.coin(1250), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatalf("outputs plus fee exactly: %v", err)
	}
	if len(exact.Outputs) != 1 || paid(exact) != 250 {
		t.Fatalf("%d outputs, paid %d, want no change and the floor", len(exact.Outputs), paid(exact))
	}
	_, err = mint.Payment(ctx, holder.lock, 1001, payer.coin(1250), payer.lock, mint.LegacyFees)
	if !errors.Is(err, mint.ErrInsufficient) {
		t.Fatalf("one satoshi short: %v", err)
	}
	if want := "mint: fee input cannot cover the outputs and the fee: inputs 1250, outputs 1001, fee 250"; err.Error() != want {
		t.Fatalf("text %q, want %q", err, want)
	}

	if _, err := mint.FundingTree(holder.lock, 8, 1, payer.coin(1), payer.lock, mint.LegacyFees); !errors.Is(err, mint.ErrInsufficient) {
		t.Fatalf("one satoshi funded a tree: %v", err)
	}

	// The previous token's value counts toward the inputs: the same fee
	// input that cannot pay for a create of 1000 pays for an update that
	// spends 1000.
	_, err = mint.Transition(holder.lock, 1000, nil, payer.coin(1000), payer.lock, mint.LegacyFees)
	if !errors.Is(err, mint.ErrInsufficient) {
		t.Fatalf("create: %v", err)
	}
	prev := holder.coin(1000)
	if _, err := mint.Transition(holder.lock, 1000, &prev, payer.coin(1000), payer.lock, mint.LegacyFees); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// Each refusal with its text, and which one wins when two apply.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	holder, payer := parties(t)
	fee := payer.coin(5000)
	noTx := mint.Input{Vout: 0, Unlocker: holder.unlock}
	past := holder.coin(1)
	past.Vout = 1
	pastUnsigned := past
	pastUnsigned.Unlocker = nil
	unsigned := holder.coin(1)
	unsigned.Unlocker = nil
	unsignedFee := payer.coin(5000)
	unsignedFee.Unlocker = nil
	// 1<<31 is negative as a 32-bit int, so a check that converted the index
	// to int would pass it there and the index would panic.
	wrapPrev := holder.coin(1)
	wrapPrev.Vout = 1 << 31
	wrapFee := payer.coin(5000)
	wrapFee.Vout = 1 << 31

	const (
		noLock   = "mint: nil lock script"
		noChange = "mint: nil change script"
		prevOut  = "mint: previous token output out of range"
		feeOut   = "mint: fee input out of range or unsigned"
		badTree  = "mint: a funding tree needs at least one output of at least one satoshi"
		badPay   = "mint: a payment needs a destination and an amount"
	)
	for _, row := range []struct {
		name string
		call func() (*transaction.Transaction, error)
		want string
	}{
		{"transition nil lock", func() (*transaction.Transaction, error) {
			return mint.Transition(nil, 1, nil, fee, payer.lock, mint.LegacyFees)
		}, noLock},
		{"transition nil lock before nil change", func() (*transaction.Transaction, error) {
			return mint.Transition(nil, 1, nil, fee, nil, mint.LegacyFees)
		}, noLock},
		{"transition nil change before a bad input", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &past, unsignedFee, nil, mint.LegacyFees)
		}, noChange},
		{"transition previous without a transaction", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &noTx, fee, payer.lock, mint.LegacyFees)
		}, prevOut},
		{"transition previous past the outputs", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &past, fee, payer.lock, mint.LegacyFees)
		}, prevOut},
		{"transition previous without an unlocker", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &unsigned, fee, payer.lock, mint.LegacyFees)
		}, "mint: previous token output unsigned"},
		{"transition previous out of range before unsigned", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &pastUnsigned, fee, payer.lock, mint.LegacyFees)
		}, prevOut},
		{"transition previous before the fee input", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &past, unsignedFee, payer.lock, mint.LegacyFees)
		}, prevOut},
		{"transition unsigned fee input", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, nil, unsignedFee, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"transition previous at 1<<31", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, &wrapPrev, fee, payer.lock, mint.LegacyFees)
		}, prevOut},
		{"transition fee at 1<<31", func() (*transaction.Transaction, error) {
			return mint.Transition(holder.lock, 1, nil, wrapFee, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"tree no outputs", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 0, 1, fee, payer.lock, mint.LegacyFees)
		}, badTree},
		{"tree no value", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 1, 0, fee, payer.lock, mint.LegacyFees)
		}, badTree},
		{"tree count before nil lock", func() (*transaction.Transaction, error) {
			return mint.FundingTree(nil, 0, 1, fee, payer.lock, mint.LegacyFees)
		}, badTree},
		{"tree nil lock", func() (*transaction.Transaction, error) {
			return mint.FundingTree(nil, 1, 1, fee, payer.lock, mint.LegacyFees)
		}, noLock},
		{"tree nil change", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 1, 1, fee, nil, mint.LegacyFees)
		}, noChange},
		{"tree fee without a transaction", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 1, 1, noTx, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"tree unsigned fee input", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 1, 1, unsignedFee, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"tree fee at 1<<31", func() (*transaction.Transaction, error) {
			return mint.FundingTree(holder.lock, 1, 1, wrapFee, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"payment nil destination", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, nil, 1, fee, payer.lock, mint.LegacyFees)
		}, badPay},
		{"payment no amount", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, holder.lock, 0, fee, payer.lock, mint.LegacyFees)
		}, badPay},
		{"payment nil change", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, holder.lock, 1, fee, nil, mint.LegacyFees)
		}, noChange},
		{"payment fee past the outputs", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, holder.lock, 1, mint.Input{Tx: fee.Tx, Vout: 1, Unlocker: payer.unlock}, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"payment unsigned fee input", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, holder.lock, 1, unsignedFee, payer.lock, mint.LegacyFees)
		}, feeOut},
		{"payment fee at 1<<31", func() (*transaction.Transaction, error) {
			return mint.Payment(ctx, holder.lock, 1, wrapFee, payer.lock, mint.LegacyFees)
		}, feeOut},
	} {
		tx, err := row.call()
		if tx != nil || err == nil || err.Error() != row.want {
			t.Errorf("%s: tx=%v err=%v, want %q", row.name, tx != nil, err, row.want)
		}
		if row.want == noChange && !errors.Is(err, mint.ErrNoChange) {
			t.Errorf("%s: %v is not ErrNoChange", row.name, err)
		}
	}
}

// grower signs with a script a thousand bytes longer each time, so the size
// always outruns the fee the last pass chose.
type grower struct{ n int }

func (g *grower) Sign(*transaction.Transaction, uint32) (*script.Script, error) {
	g.n += 1000
	s := script.Script(make([]byte, g.n))
	return &s, nil
}

func (g *grower) EstimateLength(*transaction.Transaction, uint32) uint32 { return 0 }

// failing refuses to sign.
type failing struct{}

var errSign = errors.New("sample: signer refused")

func (failing) Sign(*transaction.Transaction, uint32) (*script.Script, error) { return nil, errSign }

func (failing) EstimateLength(*transaction.Transaction, uint32) uint32 { return 0 }

// The loop is bounded, so a signer whose size never settles ends in an error
// after six passes rather than spinning; a signer's own error is returned as
// it is.
func TestSignerFailures(t *testing.T) {
	ctx := context.Background()
	holder, payer := parties(t)
	g := &grower{}
	coin := payer.coin(1_000_000)
	_, err := mint.Payment(ctx, holder.lock, 1, mint.Input{Tx: coin.Tx, Vout: 0, Unlocker: g}, payer.lock, mint.LegacyFees)
	if err == nil || err.Error() != "mint: fee did not converge" {
		t.Fatalf("growing signer: %v", err)
	}
	if g.n != 6000 {
		t.Fatalf("signed %d times, want 6 passes", g.n/1000)
	}
	_, err = mint.Payment(ctx, holder.lock, 1, mint.Input{Tx: coin.Tx, Vout: 0, Unlocker: failing{}}, payer.lock, mint.LegacyFees)
	if !errors.Is(err, errSign) {
		t.Fatalf("failing signer: %v", err)
	}
}

// A tree holds at most MaxFundingOutputs funding outputs and its change: the
// largest is built, and one more is refused before anything is signed.
func TestFundingTreeCap(t *testing.T) {
	holder, payer := parties(t)
	tree, err := mint.FundingTree(holder.lock, mint.MaxFundingOutputs, 1, payer.coin(100000), payer.lock, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Outputs) != mint.MaxFundingOutputs+1 {
		t.Fatalf("%d outputs, want %d and change", len(tree.Outputs), mint.MaxFundingOutputs)
	}
	_, err = mint.FundingTree(holder.lock, mint.MaxFundingOutputs+1, 1, payer.coin(100000), payer.lock, mint.LegacyFees)
	if !errors.Is(err, mint.ErrTreeTooLarge) {
		t.Fatalf("one over the cap: %v", err)
	}
}
