package funding_test

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
)

// A proven parent and an unproven child spending it: the shape of a
// transaction published before it mined.
func unprovenChild(t *testing.T) (parent, child *transaction.Transaction) {
	t.Helper()
	parent = transaction.NewTransaction()
	// The parent spends nothing real, but it has an input: the guard refuses
	// a transaction of none.
	parent.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{0x11}, UnlockingScript: &script.Script{}})
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: &script.Script{script.OpTRUE}})
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(parent.TxID(), 500)
	if err != nil {
		t.Fatal(err)
	}
	parent.MerklePath = mp
	child = transaction.NewTransaction()
	child.AddInputFromTx(parent, 0, nil)
	child.Inputs[0].UnlockingScript = &script.Script{}
	child.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: &script.Script{script.OpTRUE}})
	return parent, child
}

// The next transaction spends this one. While this one is unmined the state
// keeps its BEEF, and the rebuilt transaction must carry its parent, or the
// spender's BEEF could not be verified by anyone.
func TestRebuildRebuildsAnUnminedTransactionWithItsAncestry(t *testing.T) {
	parent, child := unprovenChild(t)
	kept, err := funding.KeepBEEF(child, nil)
	if err != nil || kept == "" {
		t.Fatalf("an unproven transaction kept no BEEF: %q %v", kept, err)
	}
	// What is kept is written into the state file as it is, so its form is
	// part of that file: an Atomic BEEF naming the transaction. Rebuild reads
	// a plain BEEF as well, so only this catches the form changing.
	atomic, err := child.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(atomic); kept != want || !strings.HasPrefix(kept, "01010101") {
		t.Fatalf("kept\n %s\nwant the Atomic BEEF\n %s", kept, want)
	}
	got, err := funding.Rebuild(child.Hex(), "", kept)
	if err != nil {
		t.Fatal(err)
	}
	if got.TxID().String() != child.TxID().String() {
		t.Fatal("rebuilt a different transaction")
	}
	// The kept BEEF is the whole answer; the raw bytes beside it are not read.
	if alone, err := funding.Rebuild("", "", kept); err != nil || alone.TxID().String() != child.TxID().String() {
		t.Fatalf("the kept BEEF alone did not rebuild the transaction: %v", err)
	}
	src := got.Inputs[0].SourceTransaction
	if src == nil || src.TxID().String() != parent.TxID().String() || src.MerklePath == nil {
		t.Fatal("the rebuilt transaction lost its proven parent")
	}
	// And a transaction built on it has a complete BEEF.
	grand := transaction.NewTransaction()
	grand.AddInputFromTx(got, 0, nil)
	grand.Inputs[0].UnlockingScript = &script.Script{}
	grand.AddOutput(&transaction.TransactionOutput{Satoshis: 800, LockingScript: &script.Script{script.OpTRUE}})
	if _, err := grand.AtomicBEEF(false); err != nil {
		t.Fatalf("a spender of the rebuilt transaction has no complete BEEF: %v", err)
	}
}

// Once proven, nothing is kept but the proof: a proven transaction's BEEF
// needs no ancestry.
func TestRebuildPrefersTheProofOnceThereIsOne(t *testing.T) {
	_, child := unprovenChild(t)
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(child.TxID(), 501)
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := funding.KeepBEEF(child, mp); err != nil || kept != "" {
		t.Fatalf("a proven transaction kept a BEEF: %q %v", kept, err)
	}
	got, err := funding.Rebuild(child.Hex(), mp.Hex(), "stale beef that must not be read")
	if err != nil {
		t.Fatal(err)
	}
	if got.MerklePath == nil || got.MerklePath.BlockHeight != 501 {
		t.Fatal("the proof was not attached")
	}
}

// Keeping a BEEF with a gap in it would be worse than keeping none: nothing
// spending the transaction could ever be verified, and nothing would say so
// until a reader refused it. An input whose parent is not attached is
// refused here instead.
func TestKeepBEEFRefusesPartialAncestry(t *testing.T) {
	parent, _ := unprovenChild(t)
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       parent.TxID(),
		SourceTxOutIndex: 0,
		UnlockingScript:  &script.Script{},
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: &script.Script{script.OpTRUE}})
	if kept, err := funding.KeepBEEF(tx, nil); err == nil {
		t.Fatalf("kept a BEEF missing the parent: %q", kept)
	}
}

// With neither a proof nor a BEEF, the raw bytes are the whole answer: a
// transaction with no proof attached, and not an error.
func TestRebuildFromRawAlone(t *testing.T) {
	_, child := unprovenChild(t)
	got, err := funding.Rebuild(child.Hex(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.TxID().String() != child.TxID().String() || got.MerklePath != nil {
		t.Fatalf("rebuilt %s with proof %v", got.TxID(), got.MerklePath)
	}
}

// Every way what was kept can be unusable is an error, never a transaction
// missing the part that failed. Every text is pinned, and so is which check
// speaks first when two would fail: callers put their own prefix on these
// errors and print them, so one added here would reach the user as a second.
func TestRebuildRefusesWhatItCannotRead(t *testing.T) {
	_, child := unprovenChild(t)
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(child.TxID(), 501)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := transaction.NewBeefV2().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	// A BEEF whose one entry is a bare txid passes the guard and parses,
	// but holds no transaction to rebuild.
	txidOnly := transaction.NewBeefV2()
	txidOnly.MergeTxidOnly(child.TxID())
	bare, err := txidOnly.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	// With the raw bytes and the proof both bad, the raw bytes' error is the
	// one reported: the check order reaches the user as text. The two texts
	// must differ, or that row would pin nothing.
	_, rawErr := hex.DecodeString("zz")
	_, bumpErr := guard.ParseBUMP([]byte{0}, guard.DefaultBound)
	if rawErr == nil || bumpErr == nil || rawErr.Error() == bumpErr.Error() {
		t.Fatalf("the order row cannot tell the checks apart: %v, %v", rawErr, bumpErr)
	}
	// The other texts are the underlying call's own, passed through
	// unwrapped: the hex decoder's, or the guard's, which walks every BEEF,
	// transaction and proof before the SDK parses it.
	_, hexErr := hex.DecodeString("zz")
	_, _, _, beefErr := guard.ParseBEEF([]byte{0}, guard.DefaultBound)
	_, _, _, emptyErr := guard.ParseBEEF(empty, guard.DefaultBound)
	_, shortErr := guard.ParseTransaction([]byte{0}, guard.DefaultBound)
	for _, err := range []error{hexErr, beefErr, emptyErr, shortErr} {
		if err == nil {
			t.Fatal("an input meant to be refused was accepted by the call beneath Rebuild")
		}
	}
	cases := []struct {
		name, raw, bump, beef, want string
	}{
		{"beef not hex", child.Hex(), "", "zz", hexErr.Error()},
		{"beef not a BEEF", child.Hex(), "", "00", beefErr.Error()},
		{"beef with no subject", child.Hex(), "", hex.EncodeToString(empty), emptyErr.Error()},
		{"beef of a bare txid", child.Hex(), "", hex.EncodeToString(bare), "kept BEEF names no subject transaction"},
		{"raw not a transaction", "00", "", "", shortErr.Error()},
		{"raw not hex", "zz", mp.Hex(), "", rawErr.Error()},
		{"proof not a proof", child.Hex(), "00", "", bumpErr.Error()},
		{"raw and proof both bad", "zz", "00", "", rawErr.Error()},
	}
	for _, c := range cases {
		tx, err := funding.Rebuild(c.raw, c.bump, c.beef)
		if err == nil || tx != nil {
			t.Errorf("%s: %v %v, want an error and no transaction", c.name, tx, err)
			continue
		}
		if err.Error() != c.want {
			t.Errorf("%s: %q, want %q", c.name, err, c.want)
		}
	}
}

func TestBumpHex(t *testing.T) {
	if got := funding.BumpHex(nil); got != "" {
		t.Fatalf("no proof: %q", got)
	}
	_, child := unprovenChild(t)
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(child.TxID(), 501)
	if err != nil {
		t.Fatal(err)
	}
	if got := funding.BumpHex(mp); got != mp.Hex() || got == "" {
		t.Fatalf("proof: %q, want %q", got, mp.Hex())
	}
}

func TestRemaining(t *testing.T) {
	cases := []struct {
		tree *funding.Tree
		want uint32
	}{
		{nil, 0},
		{&funding.Tree{}, 0},
		{&funding.Tree{Count: 4, Next: 1}, 3},
		{&funding.Tree{Count: 4, Next: 4}, 0},
		// An index past the end is a state that went wrong somewhere; it
		// funds nothing rather than wrapping to a huge count.
		{&funding.Tree{Count: 4, Next: 5}, 0},
	}
	for _, c := range cases {
		if got := c.tree.Remaining(); got != c.want {
			t.Errorf("%+v: %d, want %d", c.tree, got, c.want)
		}
	}
}

// A Tree is written into an application's state file, so its tags, their
// order and which of them are omitted when empty are a file format. The
// literals here are that format.
func TestTreeJSONIsFrozen(t *testing.T) {
	full := funding.Tree{
		IdentityKeyHex: "ik", Txid: "id", RawHex: "raw", BumpHex: "bump", BeefHex: "beef",
		Height: 1, Sats: 2, Count: 3, Next: 4, Funder: "who",
	}
	cases := []struct {
		name string
		tree funding.Tree
		want string
	}{
		{"full", full, `{"identityKey":"ik","txid":"id","rawHex":"raw","bumpHex":"bump","beefHex":"beef","height":1,"sats":2,"count":3,"next":4,"funder":"who"}`},
		{"empty", funding.Tree{}, `{"txid":"","rawHex":"","sats":0,"count":0,"next":0}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.tree)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
		var back funding.Tree
		if err := json.Unmarshal([]byte(c.want), &back); err != nil || back != c.tree {
			t.Errorf("%s: read back %+v %v", c.name, back, err)
		}
	}
}
