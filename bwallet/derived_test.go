package bwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// BRC-29 end to end between two wallets: the sender derives the recipient's
// destination from the recipient's identity key; the recipient re-derives
// the same key from the sender's, proves the output is theirs, and spends
// it with the derived unlocker under the SDK's script interpreter.
func TestPaymentRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sender, err := Create(filepath.Join(dir, "a"), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := Create(filepath.Join(dir, "b"), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	senderHex := hex.EncodeToString(sender.IdentityKey().Compressed())
	recipientHex := hex.EncodeToString(recipient.IdentityKey().Compressed())

	dest, err := sender.PaymentDestination(ctx, recipientHex, "prefix1", "suffix1")
	if err != nil {
		t.Fatal(err)
	}
	d := &Derivation{SecurityLevel: 2, Protocol: PaymentProtocol.Protocol, KeyID: PaymentKeyID("prefix1", "suffix1"),
		CounterpartyHex: senderHex, OwnerHex: recipientHex}
	mine, err := recipient.DerivedScript(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.Equals(mine) {
		t.Fatal("sender and recipient derived different destinations")
	}
	// A wrong suffix or the wrong owner must not match.
	other, _ := recipient.DerivedScript(ctx, &Derivation{SecurityLevel: 2, Protocol: PaymentProtocol.Protocol,
		KeyID: PaymentKeyID("prefix1", "suffix2"), CounterpartyHex: senderHex, OwnerHex: recipientHex})
	if dest.Equals(other) {
		t.Fatal("a different suffix derived the same key")
	}
	if _, err := sender.DerivedScript(ctx, d); err == nil {
		t.Fatal("the sender derived the recipient's private side")
	}

	// The payment, then a spend of it by the recipient.
	pay := transaction.NewTransaction()
	pay.AddOutput(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: dest})
	spend := transaction.NewTransaction()
	spend.AddInputFromTx(pay, 0, recipient.DerivedUnlocker(d))
	back, _ := recipient.FundScript()
	spend.AddOutput(&transaction.TransactionOutput{Satoshis: 4000, LockingScript: back})
	if err := spend.Sign(); err != nil {
		t.Fatal(err)
	}
	if ok, err := spv.VerifyScripts(ctx, spend); err != nil || !ok {
		t.Fatalf("derived spend does not verify: ok=%v err=%v", ok, err)
	}
}

// The derived unlocker refuses an input index past the transaction's inputs,
// including 1<<31, which is negative as a 32-bit int.
func TestDerivedUnlockerRefusesAnInputOutOfRange(t *testing.T) {
	e := newWallet(t)
	d := &Derivation{SecurityLevel: 2, Protocol: PaymentProtocol.Protocol, KeyID: PaymentKeyID("prefix1", "suffix1"),
		CounterpartyHex: hex.EncodeToString(e.IdentityKey().Compressed()), OwnerHex: hex.EncodeToString(e.IdentityKey().Compressed())}
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{})
	for _, i := range []uint32{1, 1 << 31} {
		want := fmt.Sprintf("bwallet: input %d out of range", i)
		if _, err := e.DerivedUnlocker(d).Sign(tx, i); err == nil || err.Error() != want {
			t.Errorf("input %d: %v, want %q", i, err, want)
		}
	}
}
