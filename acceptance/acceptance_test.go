package acceptance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
)

// rig is a test chain with mature coins to the test key, and a receiver
// script to pay.
type rig struct {
	t     *testing.T
	chain *testchain.Chain
	key   *ec.PrivateKey
	lock  *script.Script
	payee *script.Script
	coins []*transaction.Transaction
	next  int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	key := goldentest.FixedKey()
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ec.NewPrivateKey()
	paddr, _ := script.NewAddressFromPublicKey(other.PubKey(), false)
	payee, _ := p2pkh.Lock(paddr)
	c := testchain.New(800)
	if _, err := c.Generate(testchain.Maturity+100, addr.AddressString); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, chain: c, key: key, lock: lock, payee: payee}
	heights := map[*transaction.Transaction]uint32{}
	for _, id := range c.Txids() {
		tx := c.Tx(id)
		mp, h, _ := c.Proof(id)
		tx.MerklePath = mp
		heights[tx] = h
		r.coins = append(r.coins, tx)
	}
	// Oldest first: those are mature, for a test that mines a payment.
	sort.Slice(r.coins, func(i, j int) bool { return heights[r.coins[i]] < heights[r.coins[j]] })
	return r
}

// pay is a payment of sats to the payee from the next mined coin, with
// change back to the test key.
func (r *rig) pay(sats uint64) *transaction.Transaction {
	r.t.Helper()
	src := r.coins[r.next]
	r.next++
	return r.payFrom(src, sats, 0)
}

func (r *rig) payFrom(src *transaction.Transaction, sats uint64, lockTime uint32) *transaction.Transaction {
	r.t.Helper()
	unlock, err := p2pkh.Unlock(r.key, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(src, 0, unlock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: r.payee})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: src.Outputs[0].Satoshis - sats - 500, LockingScript: r.lock})
	tx.LockTime = lockTime
	if lockTime != 0 {
		tx.Inputs[0].SequenceNumber = 0
	}
	if err := tx.Sign(); err != nil {
		r.t.Fatal(err)
	}
	return tx
}

func (r *rig) payment(tx *transaction.Transaction, payer string) Payment {
	return Payment{Tx: tx, Payer: payer, Pays: []Output{{Vout: 0, Script: *r.payee, Sats: tx.Outputs[0].Satoshis}}}
}

type settler struct {
	mu   sync.Mutex
	err  error
	sent []string
}

func (s *settler) Submit(_ context.Context, tx *transaction.Transaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, tx.TxID().String())
	return s.err
}
func (s *settler) Name() string { return "test" }

// status answers every txid with the next of its statuses, the last one
// for good.
type status struct {
	mu   sync.Mutex
	seq  []publish.ArcadeStatus
	err  error
	asks int
}

func (s *status) Status(_ context.Context, txid string) (*publish.ArcadeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	i := min(s.asks, len(s.seq)-1)
	s.asks++
	st := s.seq[i]
	st.TxID = txid
	return &st, nil
}

func says(st string, competing ...string) *status {
	return &status{seq: []publish.ArcadeStatus{{TxStatus: st, CompetingTxs: competing}}}
}

// spends answers every outpoint unspent, or spent by by, or err.
type spends struct {
	by  string
	err error
}

func (s *spends) Spender(context.Context, string, uint32) (string, error) { return s.by, s.err }

type proofs struct{ c *testchain.Chain }

func (p proofs) Proof(_ context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	mp, h, ok := p.c.Proof(txid)
	if !ok {
		return nil, 0, nodeapi.ErrNotMined
	}
	return mp, h, nil
}

func (r *rig) verifier(pol Policy, st ...StatusSource) (*Verifier, *settler) {
	s := &settler{}
	if len(st) == 0 {
		st = []StatusSource{says("SEEN_ON_NETWORK")}
	}
	return &Verifier{Policy: pol, Exposure: NewExposure(), Headers: r.chain, Settler: s,
		Status: st, Spends: &spends{}, Proofs: proofs{r.chain}}, s
}

func fastPolicy() Policy {
	p := DefaultPolicy()
	p.Poll = time.Millisecond
	p.Wait = 20 * time.Millisecond
	return p
}

func accept(t *testing.T, v *Verifier, p Payment) Verdict {
	t.Helper()
	vd, err := v.Accept(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return vd
}

func want(t *testing.T, vd Verdict, d Decision, r Reason) {
	t.Helper()
	if vd.Decision != d || vd.Reason != r {
		t.Fatalf("verdict %v, want %s (%s)", vd, d, r)
	}
}

func TestSmallPaymentIsFast(t *testing.T) {
	r := newRig(t)
	v, s := r.verifier(fastPolicy())
	tx := r.pay(1_000)
	vd := accept(t, v, r.payment(tx, "alice"))
	want(t, vd, Fast, ReasonAtOrBelow)
	if vd.Sats != 1_000 || vd.Threshold != DefaultThresholdSats {
		t.Fatalf("verdict %v", vd)
	}
	if len(s.sent) != 1 || s.sent[0] != tx.TxID().String() {
		t.Fatalf("the receiver did not broadcast the payment: %v", s.sent)
	}
	if p, total := v.Exposure.Unmined("alice", 0); p != 1_000 || total != 1_000 {
		t.Fatalf("exposure %d, %d", p, total)
	}
}

func TestThresholdIsInclusive(t *testing.T) {
	r := newRig(t)
	pol := fastPolicy()
	pol.ThresholdSats, pol.PayerLimit, pol.TotalLimit = 5_000, 5_000, 5_000
	v, _ := r.verifier(pol)
	want(t, accept(t, v, r.payment(r.pay(5_000), "a")), Fast, ReasonAtOrBelow)
	want(t, accept(t, v, r.payment(r.pay(5_001), "b")), Hold, ReasonAboveThreshold)
}

func TestLargePaymentIsHeldAndBroadcast(t *testing.T) {
	r := newRig(t)
	v, s := r.verifier(fastPolicy())
	tx := r.pay(DefaultThresholdSats + 1)
	want(t, accept(t, v, r.payment(tx, "alice")), Hold, ReasonAboveThreshold)
	if len(s.sent) != 1 {
		t.Fatal("a held payment is broadcast too, so it mines")
	}
	if _, total := v.Exposure.Unmined("alice", 0); total != 0 {
		t.Fatal("a held payment is not charged to the fast path")
	}
	// It is taken once it mines, with a proof against the headers.
	if err := r.chain.Send(tx); err != nil {
		t.Fatal(err)
	}
	r.chain.Mine()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mp, h, err := v.Confirm(ctx, tx)
	if err != nil || mp == nil || h == 0 {
		t.Fatalf("confirm: %v %d %v", mp, h, err)
	}
}

func TestAskCannotForceFast(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(fastPolicy())
	p := r.payment(r.pay(DefaultThresholdSats+1), "a")
	p.Ask = AskFast
	want(t, accept(t, v, p), Hold, ReasonAboveThreshold)
	p = r.payment(r.pay(10), "a")
	p.Ask = AskHold
	want(t, accept(t, v, p), Hold, ReasonAskedHold)
}

func TestAggregationCrossesTheLimit(t *testing.T) {
	r := newRig(t)
	pol := fastPolicy()
	pol.ThresholdSats, pol.PayerLimit, pol.TotalLimit = 1_000, 2_500, 3_500
	v, _ := r.verifier(pol)
	want(t, accept(t, v, r.payment(r.pay(1_000), "alice")), Fast, ReasonAtOrBelow)
	want(t, accept(t, v, r.payment(r.pay(1_000), "alice")), Fast, ReasonAtOrBelow)
	want(t, accept(t, v, r.payment(r.pay(1_000), "alice")), Hold, ReasonPayerLimit)
	want(t, accept(t, v, r.payment(r.pay(1_000), "bob")), Fast, ReasonAtOrBelow)
	// alice 2000 + bob 1000 = 3000 of 3500: bob's next is within his own
	// limit and passes the total's, and so does carol's.
	want(t, accept(t, v, r.payment(r.pay(1_000), "bob")), Hold, ReasonTotalLimit)
	want(t, accept(t, v, r.payment(r.pay(1_000), "carol")), Hold, ReasonTotalLimit)
	want(t, accept(t, v, r.payment(r.pay(500), "carol")), Fast, ReasonAtOrBelow)
}

func TestAggregationWindow(t *testing.T) {
	x := NewExposure()
	now := time.Unix(1_000_000, 0)
	x.now = func() time.Time { return now }
	pol := Policy{ThresholdSats: 100, PayerLimit: 100, TotalLimit: 1_000, Window: time.Minute}
	ctx := context.Background()
	if d := pol.Decide(ctx, x, Request{Payer: "a", Txid: "1", Sats: 100}); d.Decision != Fast {
		t.Fatal(d)
	}
	if d := pol.Decide(ctx, x, Request{Payer: "a", Txid: "2", Sats: 1}); d.Reason != ReasonPayerLimit {
		t.Fatal(d)
	}
	now = now.Add(2 * time.Minute)
	if d := pol.Decide(ctx, x, Request{Payer: "a", Txid: "2", Sats: 1}); d.Decision != Fast {
		t.Fatalf("a charge older than the window still counts: %v", d)
	}
	if d := pol.Decide(ctx, x, Request{Payer: "a", Txid: "2", Sats: 1}); d.Reason != ReasonAlreadyCharged {
		t.Fatal(d)
	}
	x.Release("2")
	if p, _ := x.Unmined("a", 0); p != 0 {
		t.Fatal(p)
	}
}

func TestZeroPolicyHoldsEverything(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(Policy{})
	want(t, accept(t, v, r.payment(r.pay(1), "a")), Hold, ReasonAboveThreshold)
	if d := (Policy{ThresholdSats: 10}).Decide(context.Background(), nil, Request{Payer: "a", Txid: "x", Sats: 1}); d.Reason != ReasonNoExposure {
		t.Fatal(d)
	}
}

type failingPrice struct{}

func (failingPrice) Price(context.Context) (Price, error) { return Price{}, errors.New("down") }

func TestUnknownPriceIsHeld(t *testing.T) {
	ctx := context.Background()
	x := NewExposure()
	base := Policy{ThresholdCents: 2_500, PayerLimit: 1 << 40, TotalLimit: 1 << 40, MaxPriceAge: time.Hour}
	cases := []struct {
		name  string
		price PriceSource
		want  Reason
	}{
		{"none", nil, ReasonPriceUnknown},
		{"failing", failingPrice{}, ReasonPriceUnknown},
		{"zero", StaticPrice(0), ReasonPriceUnknown},
		{"stale", staticAt{Price{CentsPerCoin: 10_000, At: time.Now().Add(-2 * time.Hour)}}, ReasonPriceStale},
	}
	for i, c := range cases {
		p := base
		p.Price = c.price
		d := p.Decide(ctx, x, Request{Payer: "a", Txid: fmt.Sprint(i), Sats: 1})
		if d.Decision != Hold || d.Reason != c.want || d.Threshold != 0 {
			t.Errorf("%s: %v", c.name, d)
		}
	}
	// A known price converts: 25 dollars at 100 dollars a coin is a
	// quarter coin, and the static threshold caps it.
	p := base
	p.Price = StaticPrice(10_000)
	if th, why := p.Threshold(ctx); th != 25_000_000 || why != "" {
		t.Fatal(th, why)
	}
	p.ThresholdSats = 1_000
	if th, _ := p.Threshold(ctx); th != 1_000 {
		t.Fatal(th)
	}
	if d := p.Decide(ctx, x, Request{Payer: "a", Txid: "ok", Sats: 1_000}); d.Decision != Fast {
		t.Fatal(d)
	}
}

type staticAt struct{ p Price }

func (s staticAt) Price(context.Context) (Price, error) { return s.p, nil }

type countingPrice struct {
	n   int
	err error
	p   uint64
}

func (c *countingPrice) Price(context.Context) (Price, error) {
	c.n++
	return Price{CentsPerCoin: c.p}, c.err
}

func TestCachedPrice(t *testing.T) {
	ctx := context.Background()
	src := &countingPrice{p: 5_000}
	now := time.Unix(2_000_000, 0)
	c := &CachedPrice{Source: src, TTL: time.Minute, now: func() time.Time { return now }}
	for range 3 {
		if p, err := c.Price(ctx); err != nil || p.CentsPerCoin != 5_000 || !p.At.Equal(now) {
			t.Fatal(p, err)
		}
	}
	if src.n != 1 {
		t.Fatalf("asked %d times within the TTL", src.n)
	}
	// A failed ask answers the last good price with its own date, so the
	// policy's age limit ends it.
	now = now.Add(2 * time.Minute)
	src.err = errors.New("down")
	if p, err := c.Price(ctx); err != nil || !p.At.Equal(now.Add(-2*time.Minute)) {
		t.Fatal(p, err)
	}
	empty := &CachedPrice{Source: &countingPrice{err: errors.New("down")}, TTL: time.Minute}
	if _, err := empty.Price(ctx); err == nil {
		t.Fatal("no good price yet must be an error")
	}
}

func TestArcadeStatuses(t *testing.T) {
	cases := []struct {
		name   string
		st     StatusSource
		spend  *spends
		send   error
		d      Decision
		reason Reason
	}{
		{"seen", says("SEEN_ON_NETWORK"), &spends{}, nil, Fast, ReasonAtOrBelow},
		{"accepted", says("ACCEPTED_BY_NETWORK"), &spends{}, nil, Fast, ReasonAtOrBelow},
		{"double spend attempted", says("DOUBLE_SPEND_ATTEMPTED"), &spends{}, nil, Hold, ReasonConflict},
		{"competing", says("SEEN_ON_NETWORK", "ab"), &spends{}, nil, Hold, ReasonConflict},
		{"rejected", says("REJECTED"), &spends{}, nil, Refuse, ReasonNetworkRefused},
		{"no verdict", says("RECEIVED"), &spends{}, nil, Hold, ReasonNoVerdict},
		{"status unreachable", &status{err: errors.New("down")}, &spends{}, nil, Hold, ReasonNoVerdict},
		// The case seen in practice: arcade answered ACCEPTED_BY_NETWORK for a payment
		// whose input another transaction had spent.
		{"accepted but spent", says("ACCEPTED_BY_NETWORK"), &spends{by: fmt.Sprintf("%064x", 7)}, nil, Refuse, ReasonDoubleSpent},
		{"spend view unknown", says("ACCEPTED_BY_NETWORK"), &spends{err: nodeapi.ErrSpendUnknown}, nil, Hold, ReasonSpendUnknown},
		{"broadcast refused", says("SEEN_ON_NETWORK"), &spends{}, errors.New("publish: arcade refused x: DOUBLE_SPEND_ATTEMPTED"), Refuse, ReasonNetworkRefused},
		{"broadcast down", says("SEEN_ON_NETWORK"), &spends{}, errors.New("publish: arcade: connection refused"), Hold, ReasonBroadcastUnknown},
		{"already known", says("SEEN_ON_NETWORK"), &spends{}, errors.New("txn-already-known"), Fast, ReasonAtOrBelow},
	}
	r := newRig(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, s := r.verifier(fastPolicy(), c.st)
			v.Spends = c.spend
			s.err = c.send
			vd := accept(t, v, r.payment(r.pay(1_000), "alice"))
			want(t, vd, c.d, c.reason)
			p, _ := v.Exposure.Unmined("alice", 0)
			if (c.d == Fast) != (p == 1_000) {
				t.Fatalf("exposure %d after %s", p, vd.Decision)
			}
		})
	}
}

func TestAgreementOfSeveralSources(t *testing.T) {
	r := newRig(t)
	pol := fastPolicy()
	pol.Agree = 2
	v, _ := r.verifier(pol, says("SEEN_ON_NETWORK"), says("RECEIVED"))
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Hold, ReasonNoVerdict)
	v, _ = r.verifier(pol, says("SEEN_ON_NETWORK"), says("ACCEPTED_BY_NETWORK"))
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Fast, ReasonAtOrBelow)
	// One source that sees a conflict is enough to hold it.
	v, _ = r.verifier(pol, says("SEEN_ON_NETWORK"), says("DOUBLE_SPEND_ATTEMPTED"), says("SEEN_ON_NETWORK"))
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Hold, ReasonConflict)
}

func TestWatchWindowCatchesALateConflict(t *testing.T) {
	r := newRig(t)
	pol := fastPolicy()
	pol.Watch = 30 * time.Millisecond
	st := &status{seq: []publish.ArcadeStatus{{TxStatus: "SEEN_ON_NETWORK"}, {TxStatus: "SEEN_ON_NETWORK"}, {TxStatus: "DOUBLE_SPEND_ATTEMPTED"}}}
	v, _ := r.verifier(pol, st)
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Hold, ReasonConflict)
	// Without a conflict it answers fast once the window has passed.
	v, _ = r.verifier(pol)
	start := time.Now()
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Fast, ReasonAtOrBelow)
	if time.Since(start) < pol.Watch {
		t.Fatal("answered before the watch window passed")
	}
}

type noRoots struct{}

func (noRoots) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return false, nil
}
func (noRoots) CurrentHeight(context.Context) (uint32, error) { return 0, nil }

func TestChecksRefuse(t *testing.T) {
	r := newRig(t)
	v, s := r.verifier(fastPolicy())
	// SPV that fails: headers that know no root.
	v.Headers = noRoots{}
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Refuse, ReasonSPV)
	if len(s.sent) != 0 {
		t.Fatal("a payment that fails its checks is never broadcast")
	}
	v.Headers = r.chain

	tx := r.pay(1_000)
	p := r.payment(tx, "a")
	p.Pays[0].Sats = 1_001
	want(t, accept(t, v, p), Refuse, ReasonUnderpaid)
	p = r.payment(tx, "a")
	p.Pays[0].Script = *r.lock
	want(t, accept(t, v, p), Refuse, ReasonWrongScript)
	p = r.payment(tx, "a")
	p.Pays[0].Vout = 9
	want(t, accept(t, v, p), Refuse, ReasonMalformed)
	want(t, accept(t, v, Payment{Tx: tx, Payer: "a"}), Refuse, ReasonMalformed)

	// A signature that does not verify fails SPV.
	bad := r.pay(1_000)
	bad.Outputs[1].Satoshis--
	want(t, accept(t, v, r.payment(bad, "a")), Refuse, ReasonSPV)

	// Non-final.
	nf := r.payFrom(r.coins[r.next], 1_000, 900_000)
	r.next++
	want(t, accept(t, v, r.payment(nf, "a")), Refuse, ReasonNotFinal)

	// No source transaction to verify against.
	bare := r.pay(1_000)
	bare.Inputs[0].SourceTransaction = nil
	want(t, accept(t, v, r.payment(bare, "a")), Refuse, ReasonSPV)

	if _, err := (&Verifier{}).Accept(context.Background(), p); !errors.Is(err, ErrNoHeaders) {
		t.Fatal(err)
	}
}

func TestMinedPaymentIsTakenAtAnyValue(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(fastPolicy())
	tx := r.pay(DefaultThresholdSats * 4)
	if err := r.chain.Send(tx); err != nil {
		t.Fatal(err)
	}
	r.chain.Mine()
	mp, _, _ := r.chain.Proof(tx.TxID().String())
	tx.MerklePath = mp
	want(t, accept(t, v, r.payment(tx, "a")), Fast, ReasonMined)
}

func TestNoBroadcastLegIsNeverFast(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(fastPolicy())
	v.Settler = nil
	want(t, accept(t, v, r.payment(r.pay(1_000), "a")), Hold, ReasonNoBroadcast)
	if _, total := v.Exposure.Unmined("a", 0); total != 0 {
		t.Fatal(total)
	}
}

func TestMonitor(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(fastPolicy())
	var got []Event
	m := &Monitor{Verifier: v, Hook: func(e Event) { got = append(got, e) }}
	ctx := context.Background()

	good := r.pay(1_000)
	want(t, accept(t, v, r.payment(good, "alice")), Fast, ReasonAtOrBelow)
	m.Watch(good, "alice", 1_000)
	if ev := m.Sweep(ctx); len(ev) != 0 {
		t.Fatalf("unmined and unspent is no event yet: %v", ev)
	}
	if err := r.chain.Send(good); err != nil {
		t.Fatal(err)
	}
	r.chain.Mine()
	if ev := m.Sweep(ctx); len(ev) != 1 || ev[0].Kind != Confirmed || ev[0].Height == 0 {
		t.Fatalf("events %v", ev)
	}
	if p, _ := v.Exposure.Unmined("alice", 0); p != 0 {
		t.Fatal("a confirmed payment still counts")
	}

	// A fast payment double spent after it was taken: reported, its payer
	// flagged, and the payer's next payment held.
	lost := r.pay(1_000)
	want(t, accept(t, v, r.payment(lost, "mallory")), Fast, ReasonAtOrBelow)
	m.Watch(lost, "mallory", 1_000)
	v.Spends = &spends{by: fmt.Sprintf("%064x", 9)}
	ev := m.Sweep(ctx)
	if len(ev) != 1 || ev[0].Kind != DoubleSpent || ev[0].Payer != "mallory" {
		t.Fatalf("events %v", ev)
	}
	if len(got) != 2 || len(m.Watching()) != 0 {
		t.Fatalf("hook %v, watching %v", got, m.Watching())
	}
	v.Spends = &spends{}
	want(t, accept(t, v, r.payment(r.pay(10), "mallory")), Hold, ReasonFlagged)
	v.Exposure.Unflag("mallory")
	want(t, accept(t, v, r.payment(r.pay(10), "mallory")), Fast, ReasonAtOrBelow)

	// Refused by a broadcaster, and too old.
	rej := r.pay(1_000)
	m.Watch(rej, "eve", 1_000)
	v.Status = []StatusSource{says("REJECTED")}
	if ev := m.Sweep(ctx); len(ev) != 1 || ev[0].Kind != Refused {
		t.Fatalf("events %v", ev)
	}
	v.Status = []StatusSource{says("SEEN_ON_NETWORK")}
	old := r.pay(1_000)
	now := time.Now()
	m.now = func() time.Time { return now }
	m.MaxAge = time.Minute
	m.Watch(old, "olga", 1_000)
	now = now.Add(2 * time.Minute)
	if ev := m.Sweep(ctx); len(ev) != 1 || ev[0].Kind != Unmined {
		t.Fatalf("events %v", ev)
	}
	if _, ok := v.Exposure.Flagged("olga"); !ok {
		t.Fatal("an unmined payment flags its payer")
	}
}

func TestConfirmStopsOnDoubleSpend(t *testing.T) {
	r := newRig(t)
	v, _ := r.verifier(fastPolicy())
	v.Spends = &spends{by: fmt.Sprintf("%064x", 3)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := v.Confirm(ctx, r.pay(1_000)); !errors.Is(err, nodeapi.ErrDoubleSpent) {
		t.Fatal(err)
	}
}

func TestConcurrentDecisionsHoldTheLimit(t *testing.T) {
	pol := Policy{ThresholdSats: 10, PayerLimit: 50, TotalLimit: 50}
	x := NewExposure()
	var wg sync.WaitGroup
	var mu sync.Mutex
	fast := 0
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pol.Decide(context.Background(), x, Request{Payer: "a", Txid: fmt.Sprint(i), Sats: 10}).Decision == Fast {
				mu.Lock()
				fast++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fast != 5 {
		t.Fatalf("%d fast, want 5", fast)
	}
}
