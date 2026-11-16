package verify_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/verify"
)

// A derivation, tag and payload format that belong to no application. A
// sample payload is "smp", a version byte of 1, the 33-byte identity key it
// names, a kind byte, then anything.
var (
	sample = pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "sample"},
		KeyID:    "object",
	}
	fundingTag   = []byte{'z', 'z', 0x02}
	errShort     = errors.New("sample: too short")
	errBadSample = errors.New("sample: version is not 1")
	errNoID      = errors.New("sample: names no identity")
)

const originator = "sample.example"

func validateSample(p []byte) error {
	if len(p) < 4 || !bytes.HasPrefix(p, []byte("smp")) {
		return errShort
	}
	if p[3] != 1 {
		return errBadSample
	}
	return nil
}

func params() carrier.Params {
	return carrier.Params{Derivation: sample, FundingTag: fundingTag, ValidatePayload: validateSample}
}

func classify(p []byte) (bool, error) {
	if !bytes.HasPrefix(p, []byte("smp")) {
		return false, nil
	}
	if len(p) < 4 {
		return true, errShort
	}
	return true, nil
}

func identityOf(p []byte) ([]byte, error) {
	if len(p) < 37 {
		return nil, errNoID
	}
	return p[4:37], nil
}

// spec expects a sample of the given kind naming id, and says so without
// the label, so a reason that carries one proves the library added nothing.
func spec(kind byte, id []byte) verify.CarrierSpec {
	return verify.CarrierSpec{
		Params:     params(),
		Classify:   classify,
		IdentityOf: identityOf,
		Expect: func(p []byte) (verify.Code, string) {
			if len(p) < 38 || p[37] != kind {
				return verify.RefusedDecode, "not the kind asked for"
			}
			if !bytes.Equal(p[4:37], id) {
				return verify.RefusedKey, "names another identity"
			}
			return "", ""
		},
	}
}

func payload(id []byte, kind byte) []byte {
	p := append([]byte("smp\x01"), id...)
	return append(p, kind, 'o', 'b', 'j')
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	w1, w2   *wallet.CompletedProtoWallet
	id1, id2 []byte
	fund1    *transaction.Transaction
	fund2    *transaction.Transaction
	tracker  *goldentest.Tracker
}

// newFixture builds two identities, each with a fake mined funding tree
// under the sample derivation (four outputs of 1000 satoshis, proven at
// heights 100 and 101), and a tracker that knows both roots.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	k2seed := goldentest.Fill(0x43)
	k2, _ := ec.PrivateKeyFromBytes(k2seed[:])
	f := &fixture{t: t, ctx: context.Background(),
		tracker: &goldentest.Tracker{Roots: map[uint32]string{}, Tip: 110}}
	var err error
	if f.w1, err = wallet.NewCompletedProtoWallet(goldentest.FixedKey()); err != nil {
		t.Fatal(err)
	}
	if f.w2, err = wallet.NewCompletedProtoWallet(k2); err != nil {
		t.Fatal(err)
	}
	f.id1, f.id2 = goldentest.FixedKey().PubKey().Compressed(), k2.PubKey().Compressed()
	f.fund1, f.fund2 = f.tree(f.w1, 0x11, 100), f.tree(f.w2, 0x12, 101)
	return f
}

func (f *fixture) tree(w wallet.Interface, fake byte, height uint32) *transaction.Transaction {
	f.t.Helper()
	lock, err := carrier.FundingLock(f.ctx, w, originator, params())
	if err != nil {
		f.t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	src := chainhash.Hash(goldentest.Fill(fake))
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &src, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	for range 4 {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
	}
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(tx.TxID(), height)
	if err != nil {
		f.t.Fatal(err)
	}
	tx.MerklePath = mp
	root, err := mp.ComputeRoot(tx.TxID())
	if err != nil {
		f.t.Fatal(err)
	}
	f.tracker.Roots[height] = root.String()
	return tx
}

func (f *fixture) fundOf(w wallet.Interface) *transaction.Transaction {
	if w == f.w2 {
		return f.fund2
	}
	return f.fund1
}

// mint is a valid carrier of p under w, spending w's funding output vout.
func (f *fixture) mint(w wallet.Interface, p []byte, vout uint32) *transaction.Transaction {
	f.t.Helper()
	tx, err := carrier.Mint(f.ctx, w, originator, params(), p, f.fundOf(w), vout)
	if err != nil {
		f.t.Fatal(err)
	}
	return tx
}

// lock is the record output Mint writes for p under w.
func (f *fixture) lock(w wallet.Interface, p []byte) *script.Script {
	f.t.Helper()
	s, err := sample.Lock(f.ctx, w, originator, [][]byte{p}, true)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// hand assembles a carrier the way Mint does, with the nLockTime and input
// sequence as parameters, so a mineable one can be served.
func (f *fixture) hand(w wallet.Interface, lock *script.Script, vout, lockTime, seq uint32) *transaction.Transaction {
	f.t.Helper()
	fund := f.fundOf(w)
	tx := transaction.NewTransaction()
	tx.LockTime = lockTime
	tx.AddInputFromTx(fund, vout, sample.Unlocker(f.ctx, w, originator))
	tx.Inputs[0].SequenceNumber = seq
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: fund.Outputs[vout].Satoshis, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		f.t.Fatal(err)
	}
	return tx
}

func (f *fixture) item(tx *transaction.Transaction, index uint32) verify.Item {
	f.t.Helper()
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		f.t.Fatal(err)
	}
	return verify.Item{Beef: b, OutputIndex: index}
}

// bare answers tx in a BEEF that carries it alone, without the parent it
// spends.
func (f *fixture) bare(tx *transaction.Transaction) verify.Item {
	f.t.Helper()
	alone, err := transaction.NewTransactionFromBytes(tx.Bytes())
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := alone.AtomicBEEF(true)
	if err != nil {
		f.t.Fatal(err)
	}
	return verify.Item{Beef: b}
}

// downTracker never answers and counts how often it was asked.
type downTracker struct{ calls int }

var _ chaintracker.ChainTracker = (*downTracker)(nil)

func (d *downTracker) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	d.calls++
	return false, errors.New("header source unavailable")
}

func (d *downTracker) CurrentHeight(context.Context) (uint32, error) {
	d.calls++
	return 0, errors.New("header source unavailable")
}

// display writes a commitment as a txid is shown, by reversing it here
// rather than through the SDK helper the code under test uses.
func display(c [32]byte) string {
	r := make([]byte, 32)
	for i := range c {
		r[i] = c[31-i]
	}
	return hex.EncodeToString(r)
}

// The vocabulary is what readers print and scripts match, so it is pinned
// by literal.
func TestCodes(t *testing.T) {
	codes := map[verify.Code]string{
		verify.Verified: "VERIFIED", verify.VerifiedUnmined: "VERIFIED-UNMINED", verify.RecordPending: "RECORD-PENDING",
		verify.Unsupported: "UNSUPPORTED", verify.NoToken: "NO-TOKEN", verify.RefusedDecode: "REFUSED-DECODE",
		verify.RefusedKeyDerive: "REFUSED-KEY-DERIVE", verify.RefusedSig: "REFUSED-SIG", verify.RefusedKey: "REFUSED-KEY",
		verify.RefusedSeq: "REFUSED-SEQ", verify.RefusedFork: "REFUSED-FORK", verify.RefusedExpired: "REFUSED-EXPIRED",
		verify.RefusedBump: "REFUSED-BUMP", verify.RefusedCommit: "REFUSED-COMMIT", verify.RefusedWitness: "REFUSED-WITNESS",
		verify.RefusedMineable: "REFUSED-MINEABLE", verify.RefusedUnlocking: "REFUSED-UNLOCKING", verify.RefusedRetired: "REFUSED-RETIRED",
		verify.Error: "ERROR",
	}
	if len(codes) != 19 {
		t.Fatalf("%d distinct codes, want 19", len(codes))
	}
	for c, want := range codes {
		if string(c) != want {
			t.Errorf("code %q, frozen as %q", c, want)
		}
		if ok := c == verify.Verified || c == verify.VerifiedUnmined; c.OK() != ok {
			t.Errorf("%s.OK() = %v", c, c.OK())
		}
	}
	if verify.ErrNoTracker.Error() != "verify: no chain tracker; refusing to verify against a default" {
		t.Errorf("ErrNoTracker reads %q", verify.ErrNoTracker)
	}
}

// Each verdict Check gives, from the SDK outcome it stands for. A valid
// carrier proven through its funding parent is the positive control.
func TestCheck(t *testing.T) {
	f := newFixture(t)
	good := f.mint(f.w1, payload(f.id1, 5), 0)
	// A w2 carrier whose input spends w1's tree: w2's signature does not
	// unlock w1's funding output.
	stolen := transaction.NewTransaction()
	stolen.LockTime = carrier.LockTime
	stolen.AddInputFromTx(f.fund1, 1, sample.Unlocker(f.ctx, f.w2, originator))
	stolen.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: f.lock(f.w2, payload(f.id2, 5))})
	if err := stolen.Sign(); err != nil {
		t.Fatal(err)
	}
	parse := func(it verify.Item) *transaction.Transaction {
		_, tx, _, err := transaction.ParseBeef(it.Beef)
		if err != nil || tx == nil {
			t.Fatalf("parse: %v", err)
		}
		return tx
	}
	// The same carrier, its parent's proof swapped for the proof of another
	// transaction: a path that does not lead from the transaction it is
	// attached to. And one that claims a proof of its own, which a carrier
	// never has.
	other := f.mint(f.w2, payload(f.id2, 5), 0)
	borrowed := parse(f.bare(good))
	parent := *good.Inputs[0].SourceTransaction
	parent.MerklePath = other.Inputs[0].SourceTransaction.MerklePath
	borrowed.Inputs[0].SourceTransaction = &parent
	selfProven := parse(f.bare(good))
	selfProven.MerklePath = good.Inputs[0].SourceTransaction.MerklePath
	down := &downTracker{}
	for _, c := range []struct {
		name    string
		tx      *transaction.Transaction
		tracker chaintracker.ChainTracker
		want    verify.Verdict
		is      error
	}{
		{"control: proven through its parent", good, f.tracker, verify.Passed, nil},
		{"the parent's root is unknown", good, &goldentest.Tracker{Roots: map[uint32]string{}}, verify.ProofRefused, spv.ErrInvalidMerklePath},
		{"the input does not unlock", stolen, f.tracker, verify.ScriptRefused, spv.ErrScriptVerificationFailed},
		{"the parent is not in the answer", parse(f.bare(good)), f.tracker, verify.AncestryMissing, spv.ErrMissingSourceTransaction},
		{"the parent's proof is another transaction's", borrowed, f.tracker, verify.ProofRefused, spv.ErrInvalidMerklePath},
		{"a proof that does not lead from the transaction", selfProven, f.tracker, verify.ProofRefused, spv.ErrInvalidMerklePath},
		{"a forged proof with the header source down is still a refusal", borrowed, &downTracker{}, verify.ProofRefused, spv.ErrInvalidMerklePath},
		{"the header source is down", good, down, verify.Transport, nil},
		{"no header source", good, nil, verify.Transport, verify.ErrNoTracker},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, err := verify.Check(f.ctx, c.tx, c.tracker)
			if k != c.want {
				t.Fatalf("verdict %d (%v), want %d", k, err, c.want)
			}
			if (k == verify.Passed) != (err == nil) {
				t.Fatalf("verdict %d with error %v", k, err)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("error %v, want %v", err, c.is)
			}
		})
	}
	if down.calls == 0 {
		t.Error("the down header source was never asked")
	}
}

// cancelling is a tracker that holds every root and cancels the caller's
// context as it answers, so the proofs pass and the scripts are checked
// under a context that is already done.
type cancelling struct {
	chaintracker.ChainTracker
	cancel context.CancelFunc
}

func (c cancelling) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	ok, err := c.ChainTracker.IsValidRootForHeight(ctx, root, height)
	c.cancel()
	return ok, err
}

// A check that was cut off is no verdict. It must never read as the
// transaction refused.
func TestCheckCancelledIsNoVerdict(t *testing.T) {
	f := newFixture(t)
	good := f.mint(f.w1, payload(f.id1, 5), 0)
	ctx, cancel := context.WithCancel(f.ctx)
	k, err := verify.Check(ctx, good, cancelling{f.tracker, cancel})
	if k != verify.Transport || !errors.Is(err, context.Canceled) {
		t.Fatalf("verdict %d (%v), want no verdict and context.Canceled", k, err)
	}
	if k, err := verify.Check(f.ctx, good, f.tracker); k != verify.Passed {
		t.Fatalf("the same carrier under a live context: %d (%v)", k, err)
	}
}
