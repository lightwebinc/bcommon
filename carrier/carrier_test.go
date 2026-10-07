package carrier_test

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
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// A derivation, tag and payload format that belong to no application, so
// what these tests establish is the mechanism's and not any one
// application's. A sample payload is "smp", a version byte of 1, then
// anything; a payload with the prefix and a version other than 1 is the
// sample's own and breaks its rules.
var (
	sample = pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "sample"},
		KeyID:    "object",
	}
	fundingTag   = []byte{'z', 'z', 0x02}
	errBadSample = errors.New("sample: version is not 1")
	errShort     = errors.New("sample: too short")
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

// classify takes every output that starts "smp" and refuses one too short to
// hold the version byte.
func classify(p []byte) (bool, error) {
	if !bytes.HasPrefix(p, []byte("smp")) {
		return false, nil
	}
	if len(p) < 4 {
		return true, errShort
	}
	return true, nil
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	w        *wallet.CompletedProtoWallet
	identity []byte
	funding  *transaction.Transaction
	tracker  *goldentest.Tracker
}

// newFixture builds a fake mined funding tree under the sample derivation:
// one input nobody can spend and outputs of 1000, 1 and 5000 satoshis. Its
// proof places it at offset 1 of a two-leaf block at height 100, which the
// fixture's tracker knows.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := carrier.FundingLock(ctx, w, originator, params())
	if err != nil {
		t.Fatal(err)
	}
	funding := transaction.NewTransaction()
	fake := chainhash.Hash(goldentest.Fill(0x11))
	funding.AddInput(&transaction.TransactionInput{SourceTXID: &fake, SourceTxOutIndex: 0,
		UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for _, sats := range []uint64{1000, 1, 5000} {
		funding.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
	}
	dummy := chainhash.Hash(goldentest.Fill(0x33))
	yes := true
	funding.MerklePath = transaction.NewMerklePath(100, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &dummy},
		{Offset: 1, Hash: funding.TxID(), Txid: &yes},
	}})
	root, err := funding.MerklePath.ComputeRoot(funding.TxID())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t: t, ctx: ctx, w: w,
		identity: goldentest.FixedKey().PubKey().Compressed(),
		funding:  funding,
		tracker:  &goldentest.Tracker{Roots: map[uint32]string{100: root.String()}, Tip: 110},
	}
}

func (f *fixture) mint(payload []byte, vout uint32) *transaction.Transaction {
	f.t.Helper()
	tx, err := carrier.Mint(f.ctx, f.w, originator, params(), payload, f.funding, vout)
	if err != nil {
		f.t.Fatal(err)
	}
	return tx
}

func (f *fixture) decode(tx *transaction.Transaction) *carrier.Carrier {
	f.t.Helper()
	c, err := carrier.Decode(tx, classify)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) spv(tx *transaction.Transaction) bool {
	f.t.Helper()
	for _, in := range tx.Inputs {
		in.SourceTransaction = f.funding
	}
	ok, err := spv.Verify(f.ctx, tx, f.tracker, nil)
	if err != nil {
		f.t.Fatalf("spv: %v", err)
	}
	return ok
}

func TestSentinelTexts(t *testing.T) {
	for err, want := range map[error]string{
		carrier.ErrNotCarrier: "carrier: not a carrier",
		carrier.ErrShape:      "carrier: record output has the wrong shape",
		carrier.ErrMineable:   "carrier: mineable; the record could reach the chain",
		carrier.ErrLock:       "carrier: locking key is not the identity's record key",
		carrier.ErrSignature:  "carrier: field signature does not verify",
		carrier.ErrIdentity:   "carrier: identity key does not parse",
		carrier.ErrUnlocking:  "carrier: unlocking script is not one canonical signature push",
	} {
		if err.Error() != want {
			t.Errorf("%q, want %q", err, want)
		}
	}
}

// A minted carrier decodes to what was minted, validates, and spends its
// funding output under the interpreter through the proven tree.
func TestMintDecodeValidate(t *testing.T) {
	f := newFixture(t)
	payload := []byte("smp\x01an object")
	tx := f.mint(payload, 0)

	if tx.LockTime != carrier.LockTime || len(tx.Inputs) != 1 || tx.Inputs[0].SequenceNumber != carrier.Sequence {
		t.Fatalf("locktime %d, %d inputs", tx.LockTime, len(tx.Inputs))
	}
	if len(tx.Outputs) != 1 || tx.Outputs[0].Satoshis != 1000 {
		t.Fatal("output 0 must carry the whole funding output back")
	}
	c := f.decode(tx)
	if c.Tx != tx || c.OutputIndex != 0 || !bytes.Equal(c.Payload, payload) {
		t.Fatalf("decoded output %d payload %q", c.OutputIndex, c.Payload)
	}
	want, err := sample.ExpectedLockingKey(goldentest.FixedKey().PubKey())
	if err != nil {
		t.Fatal(err)
	}
	if !want.IsEqual(c.LockingKey) {
		t.Fatal("the record output is not locked to the derivation's key")
	}
	if err := c.Validate(params(), f.identity); err != nil {
		t.Fatal(err)
	}
	if !f.spv(tx) {
		t.Fatal("the carrier does not verify through its funding parent")
	}

	// The commitment is the txid in hash order, not display order.
	C := carrier.Commitment(tx)
	if C != *tx.TxID() || hex.EncodeToString(C[:]) == tx.TxID().String() {
		t.Fatal("commitment is not the txid in hash byte order")
	}

	// Mint checks the wallet and the funding output, and leaves the
	// payload's rules to its caller: an invalid payload mints.
	if _, err := carrier.Mint(f.ctx, nil, originator, params(), payload, f.funding, 0); err == nil || err.Error() != "carrier: nil wallet" {
		t.Fatalf("nil wallet: %v", err)
	}
	if _, err := carrier.Mint(f.ctx, f.w, originator, params(), payload, f.funding, 3); err == nil || err.Error() != "carrier: funding output out of range" {
		t.Fatalf("vout beyond the tree: %v", err)
	}
	if _, err := carrier.Mint(f.ctx, f.w, originator, params(), payload, nil, 0); err == nil || err.Error() != "carrier: funding output out of range" {
		t.Fatalf("nil funding: %v", err)
	}
	// 1<<31 is negative as a 32-bit int, so a check that converted the
	// index to int would pass it there and the index would panic.
	if _, err := carrier.Mint(f.ctx, f.w, originator, params(), payload, f.funding, 1<<31); err == nil || err.Error() != "carrier: funding output out of range" {
		t.Fatalf("vout 1<<31: %v", err)
	}
	f.mint([]byte("smp\x07"), 1)
}

// Decode takes exactly one output the classifier takes, skips the rest, and
// refuses in the order it meets them: an output the classifier refuses is
// reported even after a good one, ahead of the duplicate.
func TestDecodeRefusals(t *testing.T) {
	f := newFixture(t)
	good := f.mint([]byte("smp\x01one"), 0)
	goodLock := good.Outputs[0].LockingScript
	lockFor := func(payload []byte) *script.Script {
		s, err := sample.Lock(f.ctx, f.w, originator, [][]byte{payload}, true)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	with := func(scripts ...*script.Script) *transaction.Transaction {
		tx := transaction.NewTransaction()
		for _, s := range scripts {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: s})
		}
		return tx
	}

	// Someone else's PushDrop ahead of the record output is skipped, as is
	// the funding lock (one field, no signature).
	c, err := carrier.Decode(with(lockFor([]byte("other")), f.funding.Outputs[0].LockingScript, goodLock), classify)
	if err != nil || c.OutputIndex != 2 {
		t.Fatalf("skip: %v", err)
	}

	// A record output is the payload and its signature and nothing more. A
	// PushDrop with a field more, or with the payload alone and unsigned,
	// has no signature where Validate would read one, so it is skipped
	// even when the classifier would take its first field: on either side
	// of a good one it is neither the record nor a second one.
	extra, err := sample.Lock(f.ctx, f.w, originator, [][]byte{[]byte("smp\x01x"), []byte("extra")}, true)
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := sample.Lock(f.ctx, f.w, originator, [][]byte{[]byte("smp\x01x")}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*script.Script{extra, unsigned} {
		for _, side := range []struct {
			tx   *transaction.Transaction
			good uint32
		}{{with(s, goodLock), 1}, {with(goodLock, s), 0}} {
			c, err := carrier.Decode(side.tx, classify)
			if err != nil || c.OutputIndex != side.good || !bytes.Equal(c.Payload, []byte("smp\x01one")) {
				t.Fatalf("beside a good one at output %d: %v", side.good, err)
			}
		}
	}

	for _, row := range []struct {
		name string
		tx   *transaction.Transaction
		want error
		text string
	}{
		{"nil transaction", nil, carrier.ErrNotCarrier, "carrier: not a carrier: nil transaction"},
		{"the funding tree", f.funding, carrier.ErrNotCarrier, "carrier: not a carrier: no record output"},
		{"two record outputs", with(goodLock, goodLock), carrier.ErrNotCarrier, "carrier: not a carrier: more than one record output"},
		{"a field more than a record output", with(extra), carrier.ErrNotCarrier, "carrier: not a carrier: no record output"},
		{"the payload unsigned", with(unsigned), carrier.ErrNotCarrier, "carrier: not a carrier: no record output"},
		{"refused by the classifier", with(lockFor([]byte("smp"))), carrier.ErrShape, "carrier: record output has the wrong shape: output 0: sample: too short"},
		{"refused after a good one", with(goodLock, lockFor([]byte("smp"))), carrier.ErrShape, "carrier: record output has the wrong shape: output 1: sample: too short"},
	} {
		_, err := carrier.Decode(row.tx, classify)
		if !errors.Is(err, row.want) || err.Error() != row.text {
			t.Errorf("%s: %v", row.name, err)
		}
	}
	if _, err := carrier.Decode(good, nil); err == nil {
		t.Fatal("decoded with no classifier")
	}
}

// A classifier's (false, err) is a skip like (false, nil): its error is not
// read, so it neither refuses the transaction nor hides the record output
// beside it. On every output it leaves no record output, as a classifier
// that takes nothing does.
func TestDecodeSkipIgnoresClassifierError(t *testing.T) {
	f := newFixture(t)
	good := f.mint([]byte("smp\x01one"), 0).Outputs[0].LockingScript
	other := func(payload string) *script.Script {
		s, err := sample.Lock(f.ctx, f.w, originator, [][]byte{[]byte(payload)}, true)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	with := func(scripts ...*script.Script) *transaction.Transaction {
		tx := transaction.NewTransaction()
		for _, s := range scripts {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: s})
		}
		return tx
	}
	// skipWithError takes the sample's payloads and answers (false, err) for
	// anything else.
	skipWithError := func(p []byte) (bool, error) {
		if bytes.HasPrefix(p, []byte("smp")) {
			return true, nil
		}
		return false, errors.New("not a sample")
	}

	for _, row := range []struct {
		name string
		tx   *transaction.Transaction
		at   uint32
	}{
		{"skipped before the record output", with(other("first"), good), 1},
		{"skipped after the record output", with(good, other("last")), 0},
		{"skipped on both sides", with(other("first"), good, other("last")), 1},
	} {
		c, err := carrier.Decode(row.tx, skipWithError)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if c.OutputIndex != row.at || !bytes.Equal(c.Payload, []byte("smp\x01one")) {
			t.Fatalf("%s: took output %d %q, want %d", row.name, c.OutputIndex, c.Payload, row.at)
		}
	}

	_, err := carrier.Decode(with(other("first"), other("second")), skipWithError)
	if want := "carrier: not a carrier: no record output"; !errors.Is(err, carrier.ErrNotCarrier) || err.Error() != want {
		t.Fatalf("every output skipped with an error: %v, want %q", err, want)
	}
}

// Validate's order is its contract: the payload's rules first, then the
// finality checks, then the unlocking script, and the identity key only
// after them. Each row breaks what its name says and, where it breaks two
// rules, pins which is reported.
func TestValidateOrder(t *testing.T) {
	f := newFixture(t)
	offCurve := make([]byte, 33)
	offCurve[0], offCurve[32] = 0x02, 0x05
	other := goldentest.Fill(0x43)
	foreign, _ := ec.PrivateKeyFromBytes(other[:])
	errOwn := errors.New("sample: field is wrong")

	fresh := func(payload []byte) *carrier.Carrier { return f.decode(f.mint(payload, 0)) }
	lockTime := func(lt uint32) func(*carrier.Carrier) {
		return func(c *carrier.Carrier) { c.Tx.LockTime = lt }
	}
	final := func(c *carrier.Carrier) { c.Tx.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum }
	mangle := func(c *carrier.Carrier) { c.Signature = append([]byte{0x31}, c.Signature[1:]...) }
	both := func(a, b func(*carrier.Carrier)) func(*carrier.Carrier) {
		return func(c *carrier.Carrier) { a(c); b(c) }
	}
	noInputs := func(c *carrier.Carrier) { c.Tx.Inputs = nil }
	// A final input behind a non-final one: the safe shape asks every input
	// to be non-final, not only the first.
	finalSecond := func(c *carrier.Carrier) {
		c.Tx.Inputs = append(c.Tx.Inputs, &transaction.TransactionInput{SourceTXID: c.Tx.Inputs[0].SourceTXID,
			SourceTxOutIndex: 1, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	}
	// A second input as non-final as the first, spent by the same unlocking
	// script: nothing but the count is wrong.
	secondInput := func(c *carrier.Carrier) {
		c.Tx.Inputs = append(c.Tx.Inputs, &transaction.TransactionInput{SourceTXID: c.Tx.Inputs[0].SourceTXID,
			SourceTxOutIndex: 1, UnlockingScript: c.Tx.Inputs[0].UnlockingScript, SequenceNumber: carrier.Sequence})
	}
	highS := func(c *carrier.Carrier) {
		s := script.Script(flipS(t, *c.Tx.Inputs[0].UnlockingScript))
		c.Tx.Inputs[0].UnlockingScript = &s
	}

	for _, row := range []struct {
		name     string
		payload  string
		mutate   func(*carrier.Carrier)
		p        func(carrier.Params) carrier.Params
		identity []byte
		want     error
		text     string
	}{
		{name: "valid"},
		{name: "invalid payload", payload: "smp\x02", want: errBadSample, text: "sample: version is not 1"},
		{name: "invalid payload and mineable", payload: "smp\x02", mutate: lockTime(0), want: errBadSample, text: "sample: version is not 1"},
		{name: "invalid payload and a final input", payload: "smp\x02", mutate: final, want: errBadSample, text: "sample: version is not 1"},
		{name: "low locktime", mutate: lockTime(carrier.LockTime - 1), want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: nLockTime 4102444799"},
		{name: "a final input", mutate: final, want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: input 0 is final"},
		{name: "a second input final", mutate: finalSecond, want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: input 1 is final"},
		{name: "a final input and a low locktime", mutate: both(final, lockTime(0)), want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: nLockTime 0"},
		{name: "no inputs", mutate: noInputs, want: carrier.ErrNotCarrier,
			text: "carrier: not a carrier: no inputs"},
		{name: "no inputs and a low locktime", mutate: both(noInputs, lockTime(0)), want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: nLockTime 0"},
		{name: "high S", mutate: highS, want: carrier.ErrUnlocking,
			text: "carrier: unlocking script is not one canonical signature push: S is high"},
		{name: "two non-final inputs", mutate: secondInput, want: carrier.ErrUnlocking,
			text: "carrier: unlocking script is not one canonical signature push: 2 inputs, want 1"},
		{name: "invalid payload and high S", payload: "smp\x02", mutate: highS, want: errBadSample, text: "sample: version is not 1"},
		{name: "high S and mineable", mutate: both(highS, lockTime(0)), want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: nLockTime 0"},
		{name: "off curve and high S", identity: offCurve, mutate: highS, want: carrier.ErrUnlocking,
			text: "carrier: unlocking script is not one canonical signature push: S is high"},
		{name: "foreign identity and high S", identity: foreign.PubKey().Compressed(), mutate: highS, want: carrier.ErrUnlocking,
			text: "carrier: unlocking script is not one canonical signature push: S is high"},
		{name: "off curve", identity: offCurve, want: carrier.ErrIdentity,
			text: "carrier: identity key does not parse: guard: public key refused: invalid square root"},
		{name: "off curve, the application's sentinel", identity: offCurve, want: errOwn,
			p:    func(p carrier.Params) carrier.Params { p.ErrIdentity = errOwn; return p },
			text: "sample: field is wrong: identity key: guard: public key refused: invalid square root"},
		{name: "off curve and mineable", identity: offCurve, mutate: lockTime(0), want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: nLockTime 0"},
		{name: "off curve and a final input", identity: offCurve, mutate: final, want: carrier.ErrMineable,
			text: "carrier: mineable; the record could reach the chain: input 0 is final"},
		{name: "off curve and no inputs", identity: offCurve, mutate: noInputs, want: carrier.ErrNotCarrier,
			text: "carrier: not a carrier: no inputs"},
		{name: "invalid payload and off curve", payload: "smp\x02", identity: offCurve, want: errBadSample, text: "sample: version is not 1"},
		{name: "foreign identity", identity: foreign.PubKey().Compressed(), want: carrier.ErrLock, text: carrier.ErrLock.Error()},
		{name: "foreign identity and a mangled signature", identity: foreign.PubKey().Compressed(), mutate: mangle,
			want: carrier.ErrLock, text: carrier.ErrLock.Error()},
		{name: "another derivation", want: carrier.ErrLock, text: carrier.ErrLock.Error(),
			p: func(p carrier.Params) carrier.Params { p.Derivation.KeyID = "elsewhere"; return p }},
		{name: "signature over other bytes", want: carrier.ErrSignature, text: carrier.ErrSignature.Error(),
			mutate: func(c *carrier.Carrier) { c.Payload = []byte("smp\x01an objecu") }},
		{name: "signature mangled", want: carrier.ErrSignature, text: carrier.ErrSignature.Error(), mutate: mangle},
		{name: "no payload validator", text: "carrier: no payload validator",
			p: func(p carrier.Params) carrier.Params { p.ValidatePayload = nil; return p }},
	} {
		t.Run(row.name, func(t *testing.T) {
			payload := row.payload
			if payload == "" {
				payload = "smp\x01an object"
			}
			c := fresh([]byte(payload))
			if row.mutate != nil {
				row.mutate(c)
			}
			p := params()
			if row.p != nil {
				p = row.p(p)
			}
			id := row.identity
			if id == nil {
				id = f.identity
			}
			err := c.Validate(p, id)
			switch {
			case row.text == "":
				if err != nil {
					t.Fatal(err)
				}
			case err == nil || err.Error() != row.text:
				t.Fatalf("%v, want %q", err, row.text)
			case row.want != nil && !errors.Is(err, row.want):
				t.Fatalf("%v does not match its sentinel", err)
			}
		})
	}
}

// The funding lock is `<key> OP_CHECKSIG <tag> OP_DROP` under the
// derivation, and only a script of that shape under the same tag decodes.
func TestFundingLockAndDecode(t *testing.T) {
	f := newFixture(t)
	lock := f.funding.Outputs[0].LockingScript
	want, err := sample.ExpectedLockingKey(goldentest.FixedKey().PubKey())
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := lock.Chunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 || !bytes.Equal(chunks[0].Data, want.Compressed()) || chunks[1].Op != script.OpCHECKSIG ||
		!bytes.Equal(chunks[2].Data, fundingTag) || chunks[3].Op != script.OpDROP {
		t.Fatalf("funding lock %s", lock)
	}
	key, ok := carrier.DecodeFunding(lock, fundingTag)
	if !ok || !key.IsEqual(want) {
		t.Fatal("the funding lock does not decode to the derivation's key")
	}

	carrierOut := f.mint([]byte("smp\x01x"), 0).Outputs[0].LockingScript
	for _, row := range []struct {
		name string
		s    *script.Script
		tag  []byte
	}{
		{"another tag", lock, []byte{'z', 'z', 0x03}},
		{"an empty tag", lock, nil},
		{"a nil script", nil, fundingTag},
		{"a carrier output", carrierOut, fundingTag},
	} {
		if _, ok := carrier.DecodeFunding(row.s, row.tag); ok {
			t.Errorf("%s decoded as funding", row.name)
		}
	}

	p := params()
	p.FundingTag = nil
	if _, err := carrier.FundingLock(f.ctx, f.w, originator, p); err == nil || err.Error() != "carrier: empty funding tag" {
		t.Fatalf("no tag: %v", err)
	}
	// Why: an empty field goes out as OP_0 and comes back as 0x00, a tag
	// nobody named, which the empty tag it was locked with does not match.
	emptyField, err := sample.Lock(f.ctx, f.w, originator, [][]byte{{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := carrier.DecodeFunding(emptyField, nil); ok {
		t.Fatal("an empty field decoded under the empty tag")
	}
	if _, ok := carrier.DecodeFunding(emptyField, []byte{0}); !ok {
		t.Fatal("the SDK no longer reads an empty field back as 0x00; revisit FundingLock's refusal")
	}
}

// Both sweep modes: the tree paying for itself, and a fee input paying with
// its remainder as change. Output 0 is the funding-shaped tombstone either
// way, the fee meets the rate and the floor, and the sweep verifies through
// the proven tree.
func TestSweep(t *testing.T) {
	f := newFixture(t)
	change, _ := script.NewFromHex("76a914" + "00000000000000000000000000000000000000ff" + "88ac")
	const rate, floor = 1, 250
	paid := func(tx *transaction.Transaction) uint64 {
		var in, out uint64
		for _, i := range tx.Inputs {
			in += f.funding.Outputs[i.SourceTxOutIndex].Satoshis
		}
		for _, o := range tx.Outputs {
			out += o.Satoshis
		}
		return in - out
	}

	tree, err := carrier.Sweep(f.ctx, f.w, originator, params(), f.funding, []uint32{0, 1}, nil, 0, nil, change, rate, floor)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Inputs) != 2 || len(tree.Outputs) != 1 {
		t.Fatalf("%d inputs, %d outputs", len(tree.Inputs), len(tree.Outputs))
	}
	if _, ok := carrier.DecodeFunding(tree.Outputs[0].LockingScript, fundingTag); !ok {
		t.Fatal("the tombstone is not funding-shaped")
	}
	if fee := paid(tree); fee < floor || fee < uint64(tree.Size())*rate || tree.Outputs[0].Satoshis != 1001-fee {
		t.Fatalf("fee %d for %d bytes, tombstone %d", fee, tree.Size(), tree.Outputs[0].Satoshis)
	}
	if !f.spv(tree) {
		t.Fatal("the tree-paid sweep does not verify")
	}

	withFee, err := carrier.Sweep(f.ctx, f.w, originator, params(), f.funding, []uint32{0, 1}, f.funding, 2,
		sample.Unlocker(f.ctx, f.w, originator), change, rate, floor)
	if err != nil {
		t.Fatal(err)
	}
	if len(withFee.Outputs) != 2 || withFee.Outputs[0].Satoshis != 1001 || !withFee.Outputs[1].LockingScript.Equals(change) {
		t.Fatalf("outputs %d, tombstone %d", len(withFee.Outputs), withFee.Outputs[0].Satoshis)
	}
	if fee := paid(withFee); fee < floor || fee < uint64(withFee.Size())*rate {
		t.Fatalf("fee %d for %d bytes", fee, withFee.Size())
	}
	if !f.spv(withFee) {
		t.Fatal("the fee-paid sweep does not verify")
	}

	// One output of sats under the funding lock, to pay a sweep's fee or to
	// be swept.
	single := func(sats uint64) *transaction.Transaction {
		tx := transaction.NewTransaction()
		src := chainhash.Hash(goldentest.Fill(0x22))
		tx.AddInput(&transaction.TransactionInput{SourceTXID: &src, SourceTxOutIndex: 0,
			UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: f.funding.Outputs[0].LockingScript})
		return tx
	}

	// The change floor is inclusive: a fee input left with exactly the floor
	// once the fee is paid keeps it as change, and one satoshi less gives it
	// to the fee. The floor here is above what the sweep's size asks at the
	// rate, so the converged fee is the floor on every fee input and does
	// not move with the signatures' DER lengths, which change with what the
	// fee input is.
	const high = 1000
	sweepHigh := func(fee *transaction.Transaction, vout uint32) *transaction.Transaction {
		t.Helper()
		tx, err := carrier.Sweep(f.ctx, f.w, originator, params(), f.funding, []uint32{0, 1}, fee, vout,
			sample.Unlocker(f.ctx, f.w, originator), change, rate, high)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	converged := paid(sweepHigh(f.funding, 2))
	if converged != high {
		t.Fatalf("fee %d on a large fee input; the floor %d no longer sets it", converged, high)
	}
	at := sweepHigh(single(converged+high), 0)
	if len(at.Outputs) != 2 || at.Outputs[0].Satoshis != 1001 || at.Outputs[1].Satoshis != high || !at.Outputs[1].LockingScript.Equals(change) {
		t.Fatalf("a remainder of exactly the floor: %d outputs", len(at.Outputs))
	}
	if below := sweepHigh(single(converged+high-1), 0); len(below.Outputs) != 1 || below.Outputs[0].Satoshis != 1001 {
		t.Fatalf("a remainder under the floor: %d outputs", len(below.Outputs))
	}

	unlocker := sample.Unlocker(f.ctx, f.w, originator)
	for _, row := range []struct {
		name    string
		tree    *transaction.Transaction
		vouts   []uint32
		fee     *transaction.Transaction
		feeVout uint32
		feeUnl  transaction.UnlockingScriptTemplate
		chg     *script.Script
		text    string
	}{
		{"no outputs", nil, nil, nil, 0, nil, change, "carrier: a sweep needs a tree, outputs and a change script"},
		{"no change script", nil, []uint32{0}, nil, 0, nil, nil, "carrier: a sweep needs a tree, outputs and a change script"},
		{"an output beyond the tree", nil, []uint32{0, 9}, nil, 0, nil, change, "carrier: sweep output 9 out of range"},
		{"a tree too small to pay", nil, []uint32{1}, nil, 0, nil, change, "carrier: sweep inputs 1 cannot pay fee 250"},
		// A fee equal to what is there leaves nothing, and is refused.
		{"a tree of exactly the fee", single(floor), []uint32{0}, nil, 0, nil, change, "carrier: sweep inputs 250 cannot pay fee 250"},
		{"a fee input of exactly the fee", nil, []uint32{0}, single(floor), 0, unlocker, change, "carrier: fee input 250 cannot pay fee 250"},
		// A fee input is refused before the fee loop reads it: an index past
		// its outputs would otherwise panic, and a missing unlocker would
		// leave the fee input unsigned.
		{"the first index past the fee input's outputs", nil, []uint32{0}, single(high), 1, unlocker, change, "carrier: fee input out of range or unsigned"},
		{"a fee input with no unlocker", nil, []uint32{0}, f.funding, 2, nil, change, "carrier: fee input out of range or unsigned"},
		// 1<<31 is negative as a 32-bit int; both checks refuse it there too.
		{"a fee index of 1<<31", nil, []uint32{0}, single(high), 1 << 31, unlocker, change, "carrier: fee input out of range or unsigned"},
		{"a tree index of 1<<31", nil, []uint32{0, 1 << 31}, nil, 0, nil, change, "carrier: sweep output 2147483648 out of range"},
	} {
		tree := row.tree
		if tree == nil {
			tree = f.funding
		}
		_, err := carrier.Sweep(f.ctx, f.w, originator, params(), tree, row.vouts, row.fee, row.feeVout, row.feeUnl, row.chg, rate, floor)
		if err == nil || err.Error() != row.text {
			t.Errorf("%s: %v", row.name, err)
		}
	}
}

// SweepAt pays a fractional rate exactly: at the network rate a sweep pays
// its size at 100 satoshis per 1000 bytes rounded up, a tenth of what the
// whole-satoshi rate asks, and a fee above Max is refused rather than paid.
func TestSweepAt(t *testing.T) {
	f := newFixture(t)
	change, _ := script.NewFromHex("76a914" + "00000000000000000000000000000000000000ff" + "88ac")
	tx, err := carrier.SweepAt(f.ctx, f.w, originator, params(), f.funding, []uint32{0, 1}, nil, 0, nil, change, mint.NetworkFees)
	if err != nil {
		t.Fatal(err)
	}
	var in uint64
	for _, i := range tx.Inputs {
		in += f.funding.Outputs[i.SourceTxOutIndex].Satoshis
	}
	fee := in - tx.Outputs[0].Satoshis
	if least := (uint64(tx.Size())*100 + 999) / 1000; fee < least || fee >= uint64(tx.Size()) {
		t.Fatalf("fee %d for %d bytes, want at least %d and under the whole-satoshi rate", fee, tx.Size(), least)
	}
	if !f.spv(tx) {
		t.Fatal("the sweep does not verify")
	}
	capped := mint.NetworkFees
	capped.Max = 1
	if _, err := carrier.SweepAt(f.ctx, f.w, originator, params(), f.funding, []uint32{0, 1}, nil, 0, nil, change, capped); !errors.Is(err, mint.ErrFeeTooHigh) {
		t.Fatalf("a fee above Max: %v", err)
	}
}
