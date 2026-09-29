package pushdrop_test

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// A triple and tag that belong to no application, so what these tests
// establish is the mechanism's and not any one application's.
var (
	sample = pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "sample"},
		KeyID:    "entry",
	}
	tag     = []byte{'z', 'z', 0x01}
	payload = []byte("payload")
)

const originator = "sample.example"

func newWallet(t *testing.T) *wallet.CompletedProtoWallet {
	t.Helper()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// The measured fact the package rests on, measured again on the neutral
// triple: of every counterparty and forSelf a producer could lock with, only
// Anyone with forSelf=true gives a key the reader recomputes from the
// identity key alone AND a signature that verifies under it. Anyone with
// forSelf=false is the control: the counterparty alone is not enough.
func TestDerivationMatrix(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	want, err := sample.ExpectedLockingKey(goldentest.FixedKey().PubKey())
	if err != nil {
		t.Fatal(err)
	}
	self := wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}
	var anyoneSelf *script.Script
	for _, row := range []struct {
		name                string
		cp                  wallet.Counterparty
		forSelf             bool
		derivable, verifies bool
	}{
		{"zero counterparty, forSelf=true", wallet.Counterparty{}, true, false, false},
		{"zero counterparty, forSelf=false", wallet.Counterparty{}, false, false, false},
		{"Anyone, forSelf=true", pushdrop.Anyone(), true, true, true},
		{"Anyone, forSelf=false", pushdrop.Anyone(), false, false, false},
		{"Self, forSelf=true", self, true, false, true},
		{"Self, forSelf=false", self, false, false, true},
	} {
		pd := &sdkpushdrop.PushDrop{Wallet: w, Originator: originator}
		s, err := pd.Lock(ctx, [][]byte{tag, payload}, sample.Protocol, sample.KeyID, row.cp, row.forSelf, true, sdkpushdrop.LockBefore)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		d, err := pushdrop.DecodeTagged(s, tag, 2)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		derivable, verifies := want.IsEqual(d.LockingKey), d.VerifySignature()
		if derivable != row.derivable || verifies != row.verifies {
			t.Errorf("%s: reader-derivable %v, signature verifies %v; measured %v, %v",
				row.name, derivable, verifies, row.derivable, row.verifies)
		}
		if row.cp.Type == wallet.CounterpartyTypeAnyone && row.forSelf {
			anyoneSelf = s
		}
	}

	// Lock is that one row, byte for byte.
	s, err := sample.Lock(ctx, w, originator, [][]byte{tag, payload}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Equals(anyoneSelf) {
		t.Fatalf("Lock wrote %x, not the Anyone/forSelf=true script %x", *s, *anyoneSelf)
	}
}

// The SDK appends the signature into the capacity of the slice it is
// handed, so Lock hands it a copy: a caller's spare capacity stays untouched
// and a reused buffer never carries one output's signature into the next.
func TestLockCopiesFields(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	fields := make([][]byte, 2, 3)
	fields[0], fields[1] = tag, []byte("first")
	first, err := sample.Lock(ctx, w, originator, fields, true)
	if err != nil {
		t.Fatal(err)
	}
	if spare := fields[:3][2]; spare != nil {
		t.Fatalf("Lock wrote %x into the caller's spare capacity", spare)
	}
	before := append(script.Script(nil), *first...)
	fields[1] = []byte("second")
	second, err := sample.Lock(ctx, w, originator, fields, true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equals(&before) {
		t.Fatal("the second Lock changed the first script")
	}
	d, err := pushdrop.DecodeTagged(second, tag, 2)
	if err != nil || string(d.Fields[1]) != "second" || !d.VerifySignature() {
		t.Fatalf("second output: %v", err)
	}
}

// Without the signature the output carries the fields alone, under the same
// key, and so is not the signed shape DecodeTagged reads.
func TestLockUnsigned(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	s, err := sample.Lock(ctx, w, originator, [][]byte{tag}, false)
	if err != nil {
		t.Fatal(err)
	}
	d := sdkpushdrop.Decode(s)
	if d == nil || len(d.Fields) != 1 || string(d.Fields[0]) != string(tag) {
		t.Fatalf("unsigned output decodes as %+v", d)
	}
	want, _ := sample.ExpectedLockingKey(goldentest.FixedKey().PubKey())
	if !want.IsEqual(d.LockingPublicKey) {
		t.Fatal("unsigned output is not locked under the derivation")
	}
	if _, err := pushdrop.DecodeTagged(s, tag, 1); err == nil {
		t.Fatal("an unsigned output decoded as signed")
	}
}

func TestNilInputs(t *testing.T) {
	if _, err := sample.ExpectedLockingKey(nil); err == nil || err.Error() != "pushdrop: nil identity key" {
		t.Errorf("nil identity: %v", err)
	}
	if _, err := sample.Lock(context.Background(), nil, originator, [][]byte{tag}, true); err == nil || err.Error() != "pushdrop: nil wallet" {
		t.Errorf("nil wallet: %v", err)
	}
}
