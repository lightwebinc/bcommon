package pushdrop_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

func TestDecodeTaggedRoundTrip(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	a, b := []byte("first field"), bytes.Repeat([]byte{0x5c}, 80)
	s, err := sample.Lock(ctx, w, originator, [][]byte{tag, a, b}, true)
	if err != nil {
		t.Fatal(err)
	}
	d, err := pushdrop.DecodeTagged(s, tag, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fields) != 3 || !bytes.Equal(d.Fields[0], tag) || !bytes.Equal(d.Fields[1], a) || !bytes.Equal(d.Fields[2], b) {
		t.Fatalf("fields %x", d.Fields)
	}
	if want := append(append(append([]byte(nil), tag...), a...), b...); !bytes.Equal(d.Signed(), want) {
		t.Fatalf("signed bytes %x, want the fields concatenated", d.Signed())
	}
	if !d.VerifySignature() {
		t.Fatal("the signature Lock embedded does not verify")
	}

	// The signature covers every field, and a mangled signature is false
	// rather than an error.
	moved := &pushdrop.Tagged{Fields: [][]byte{tag, b, a}, LockingKey: d.LockingKey, Signature: d.Signature}
	if moved.VerifySignature() {
		t.Fatal("the signature verified over reordered fields")
	}
	bad := &pushdrop.Tagged{Fields: d.Fields, LockingKey: d.LockingKey, Signature: append([]byte{0x31}, d.Signature[1:]...)}
	if bad.VerifySignature() {
		t.Fatal("a mangled signature verified")
	}
}

// Each refusal, by text: an application that keeps its own sentinel puts it
// in front of the detail, so the detail is what its readers see. The rows
// that break two rules pin which one is reported, and so the check order: a
// P2PK script decodes as a PushDrop with no fields, and a tag check ahead of
// the count would index a field that is not there.
func TestDecodeTaggedRefusals(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	pd := &sdkpushdrop.PushDrop{Wallet: w, Originator: originator}
	lock := func(pos sdkpushdrop.LockPosition, sign bool, fields ...[]byte) *script.Script {
		t.Helper()
		s, err := pd.Lock(ctx, fields, sample.Protocol, sample.KeyID, pushdrop.Anyone(), true, sign, pos)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	p2pkh, err := script.NewFromHex("76a914" + strings.Repeat("22", 20) + "88ac")
	if err != nil {
		t.Fatal(err)
	}
	p2pk, err := script.NewFromHex("21" + hex.EncodeToString(goldentest.FixedKey().PubKey().Compressed()) + "ac")
	if err != nil {
		t.Fatal(err)
	}
	otherTag := []byte{'z', 'z', 0x02}
	for _, tc := range []struct {
		name    string
		s       *script.Script
		nfields int
		want    string
	}{
		{"nil script", nil, 2, "pushdrop: not a tagged PushDrop: nil script"},
		{"not a PushDrop", p2pkh, 2, "pushdrop: not a tagged PushDrop: not a lock-before PushDrop"},
		{"lock after", lock(sdkpushdrop.LockAfter, true, tag, payload), 2, "pushdrop: not a tagged PushDrop: not a lock-before PushDrop"},
		{"unsigned", lock(sdkpushdrop.LockBefore, false, tag, payload), 2, "pushdrop: not a tagged PushDrop: 2 fields, want 3"},
		{"one field short", lock(sdkpushdrop.LockBefore, true, tag), 2, "pushdrop: not a tagged PushDrop: 2 fields, want 3"},
		{"one field over", lock(sdkpushdrop.LockBefore, true, tag, payload, payload), 2, "pushdrop: not a tagged PushDrop: 4 fields, want 3"},
		{"other tag", lock(sdkpushdrop.LockBefore, true, otherTag, payload), 2, "pushdrop: not a tagged PushDrop: tag 7a7a02"},
		{"count and tag both wrong", lock(sdkpushdrop.LockBefore, true, otherTag), 2, "pushdrop: not a tagged PushDrop: 2 fields, want 3"},
		{"P2PK: no fields, so no tag", p2pk, 2, "pushdrop: not a tagged PushDrop: 0 fields, want 3"},
	} {
		_, err := pushdrop.DecodeTagged(tc.s, tag, tc.nfields)
		if err == nil {
			t.Errorf("%s: decoded", tc.name)
			continue
		}
		if !errors.Is(err, pushdrop.ErrNotTagged) {
			t.Errorf("%s: %v is not ErrNotTagged", tc.name, err)
		}
		if err.Error() != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, err, tc.want)
		}
	}

	// Asking for no fields is the caller's mistake, not a script's.
	good := lock(sdkpushdrop.LockBefore, true, tag)
	for _, n := range []int{0, -1} {
		if _, err := pushdrop.DecodeTagged(good, tag, n); err == nil || errors.Is(err, pushdrop.ErrNotTagged) {
			t.Errorf("nfields %d: %v", n, err)
		}
	}
}

// Unlocker spends what Lock locked, as the interpreter judges it, and only
// under the same derivation.
func TestUnlockerSpendsLock(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t)
	lock, err := sample.Lock(ctx, w, originator, [][]byte{tag, payload}, true)
	if err != nil {
		t.Fatal(err)
	}
	other := sample
	other.KeyID = "other entry"
	for _, tc := range []struct {
		name string
		d    pushdrop.Derivation
		ok   bool
	}{
		{"same derivation", sample, true},
		{"another key id", other, false},
	} {
		parent := transaction.NewTransaction()
		parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
		tx := transaction.NewTransaction()
		u := tc.d.Unlocker(ctx, w, originator)
		if err := tx.AddInputFrom(parent.TxID().String(), 0, hex.EncodeToString(*lock), 1000, u); err != nil {
			t.Fatal(err)
		}
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: lock})
		if err := tx.Sign(); err != nil {
			t.Fatal(err)
		}
		err := interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, 0, parent.Outputs[0]),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
			interpreter.WithAfterChronicle(),
		)
		if (err == nil) != tc.ok {
			t.Errorf("%s: interpreter says %v", tc.name, err)
		}
		if est := u.EstimateLength(tx, 0); est != 73 {
			t.Errorf("%s: EstimateLength %d, want the SDK's 73", tc.name, est)
		}
	}
}
