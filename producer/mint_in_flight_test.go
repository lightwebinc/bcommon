package producer_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
)

// chainHome is one wallet on a chain that checks what it is sent: a pool of
// mature coinbase coins, the node's asset API and RPC, and the trees and
// payers an application builds over them.
type chainHome struct {
	chain  *testchain.Chain
	base   string
	asset  *nodeapi.Asset
	rpc    *nodeapi.RPC
	own    *bwallet.Signer
	pool   *bwallet.Pool
	n      *notes
	kept   *producer.Kept
	states []*memTrees
	facade *publish.Facade
}

// homeOnChain is a wallet holding exactly coins coins, each spendable.
func homeOnChain(t *testing.T, coins int) *chainHome {
	t.Helper()
	ctx := context.Background()
	chain := testchain.New(700)
	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)
	h := &chainHome{chain: chain, base: srv.URL, asset: &nodeapi.Asset{Base: srv.URL},
		rpc: &nodeapi.RPC{URL: srv.URL + "/rpc", ID: "t"}, own: signerOf(t, newKey(t)), pool: poolIn(t), n: &notes{},
		facade: newTestChain(t).facade()}
	if _, _, err := bwallet.FundFromCoinbase(ctx, h.own, h.pool, h.rpc, h.asset, coins, 0); err != nil {
		t.Fatal(err)
	}
	// The blocks that bury the coins pay someone else, so the wallet holds
	// what the test says it does.
	burier, err := script.NewAddressFromPublicKey(newKey(t).PubKey(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Generate(testchain.Maturity, burier.AddressString); err != nil {
		t.Fatal(err)
	}
	if h.pool.Count() != coins || len(h.pool.Immature(chain.Height())) != 0 {
		t.Fatalf("the wallet holds %d coin(s), want %d", h.pool.Count(), coins)
	}
	h.kept = &producer.Kept{Load: func(txid string) (*transaction.Transaction, error) {
		for _, st := range h.states {
			for _, rec := range st.all {
				if rec.Txid == txid {
					return funding.Rebuild(rec.RawHex, rec.BumpHex, rec.BeefHex)
				}
			}
		}
		return nil, errors.New("unknown tree " + txid)
	}}
	return h
}

// payer is a Payer of its own over the home's pool, as an application makes
// one for each transaction it pays for.
func (h *chainHome) payer() *producer.Payer {
	return &producer.Payer{Pool: h.pool, Tip: h.chain.Height(), Keys: map[string]*bwallet.Signer{h.own.IdentityHex(): h.own},
		Kept: h.kept, Settler: &publish.RPCSettler{RPC: h.rpc}, Asset: h.asset, Fees: mint.DefaultFees,
		Poll: 10 * time.Millisecond, Timeout: 30 * time.Second, Note: h.n.note}
}

// trees is a Trees of four outputs minting ahead once two or fewer are
// left, with a state and a Payer of its own.
func (h *chainHome) trees() (*producer.Trees, *memTrees) {
	st := &memTrees{}
	h.states = append(h.states, st)
	return &producer.Trees{Payer: h.payer(), State: st, Identity: h.own.IdentityHex(), Count: 4, Sats: 1, Funder: "pool",
		Lock: fundingLock(h.own), Change: h.own.FundScript, Facade: h.facade, Topic: testTopic, Ahead: 2}, st
}

// mining mines whatever the chain holds, every 50 ms, until the test ends.
func (h *chainHome) mining(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				h.chain.Mine()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// pay builds a payment from the fee input in, settles it and takes its
// change: what a fee is taken for.
func (h *chainHome) pay(t *testing.T, p *producer.Payer, in mint.Input) *transaction.Transaction {
	t.Helper()
	change, err := h.own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := mint.Payment(context.Background(), &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	mp, height, err := p.Settle(context.Background(), "payment", tx)
	if err != nil {
		t.Fatalf("%v\n%s", err, h.n)
	}
	p.Change(tx, height, mp)
	return tx
}

// oneCoinMintingAhead is a wallet of one coin whose tree has just started
// minting the next one ahead, on a chain that holds what it is sent: the
// mint took the one coin, and its change comes back when it mines.
func oneCoinMintingAhead(t *testing.T) (*chainHome, *producer.Trees, *memTrees) {
	t.Helper()
	h := homeOnChain(t, 1)
	tr, st := h.trees()
	spend(t, tr, st, 1) // minted on demand from the one coin; its change is the one coin now
	h.chain.SetHold(true)
	spend(t, tr, st, 1) // 2 left: the next is minted ahead, and takes the one coin
	if h.pool.Count() != 0 || !strings.Contains(h.n.String(), "so the next is minted ahead") {
		t.Fatalf("the test did not reach the case: pool %d\n%s", h.pool.Count(), h.n)
	}
	return h, tr, st
}

const waitingNote = "no coin is spendable and 1 funding tree(s) minted ahead hold one: waiting for the change"

// A wallet with one coin has none while a tree is minted ahead. A fee taken
// in that time, through a Payer of its own as an application takes one,
// waits for the mint, collects it and is paid from the tree's change: the
// operation that needs the fee goes on, where it stopped before at an empty
// wallet.
func TestAFeeWaitsForATreeMintedAhead(t *testing.T) {
	ctx := context.Background()
	h, tr, _ := oneCoinMintingAhead(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		h.chain.Mine()
	}()
	p := h.payer()
	began := time.Now()
	in, err := p.Take(ctx)
	if err != nil {
		t.Fatalf("a fee while the tree ahead held the one coin: %v\n%s", err, h.n)
	}
	if d := time.Since(began); d < 200*time.Millisecond || d > 10*time.Second {
		t.Fatalf("the fee was taken after %s: it did not wait for the mint", d)
	}
	// The take collected the mint: the tree is held for the switch, and the
	// fee input is its change, proven.
	prep := tr.Prepared()
	if prep == nil || prep.BumpHex == "" || in.Tx.TxID().String() != prep.Txid || in.Tx.MerklePath == nil {
		t.Fatalf("prepared %+v, fee from %s\n%s", prep, in.Tx.TxID(), h.n)
	}
	for _, want := range []string{waitingNote, "funding tree " + prep.Txid + " is minted ahead and waits for the switch"} {
		if !strings.Contains(h.n.String(), want) {
			t.Fatalf("no %q in:\n%s", want, h.n)
		}
	}
	// The coin pays: the chain takes the payment, and its change is the
	// wallet's one coin again.
	h.chain.SetHold(false)
	tx := h.pay(t, p, in)
	if !h.chain.Mined(tx.TxID().String()) {
		t.Fatal("the payment did not mine")
	}
	holds(t, h.pool, tx.TxID().String()+".1")
	if err := tr.Wait(ctx); err != nil {
		t.Fatalf("nothing is in flight: %v", err)
	}
}

// A wallet that has a coin for the fee does not wait: the take answers at
// once, and the mint is still in flight, uncollected, afterwards.
func TestAFeeDoesNotWaitWhenThePoolCanPay(t *testing.T) {
	h := homeOnChain(t, 2)
	tr, st := h.trees()
	spend(t, tr, st, 1)
	h.chain.SetHold(true)
	spend(t, tr, st, 1) // minted ahead from one coin; the other stays
	if h.pool.Count() != 1 {
		t.Fatalf("pool %d\n%s", h.pool.Count(), h.n)
	}
	p := h.payer()
	began := time.Now()
	if _, err := p.Take(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("a take the pool could cover took %s", d)
	}
	if tr.Prepared() != nil || strings.Contains(h.n.String(), "waiting for the change") {
		t.Fatalf("a take the pool could cover collected the mint:\n%s", h.n)
	}
	h.chain.Mine()
	if err := tr.Wait(context.Background()); err != nil || tr.Prepared() == nil {
		t.Fatalf("the mint: %v", err)
	}
}

// The wait is bounded by the Payer's Timeout and by the context. When it
// ends first the mint is still in flight, the error says so, and a later
// take, once the mint has settled, is paid.
func TestAFeeWaitIsBounded(t *testing.T) {
	h, tr, _ := oneCoinMintingAhead(t)
	for _, c := range []struct {
		name    string
		timeout time.Duration
		ctx     time.Duration
	}{{"by the Payer's Timeout", 150 * time.Millisecond, time.Minute}, {"by the context", time.Minute, 150 * time.Millisecond}} {
		p := h.payer()
		p.Timeout = c.timeout
		ctx, cancel := context.WithTimeout(context.Background(), c.ctx)
		began := time.Now()
		_, err := p.Take(ctx)
		cancel()
		var nc *producer.NoCoinError
		if !errors.As(err, &nc) || !errors.Is(err, bwallet.ErrNoSpendable) || nc.Minting != 1 || nc.Held != 0 ||
			!strings.Contains(err.Error(), "1 funding tree(s) minted ahead hold a coin and have not settled") {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d := time.Since(began); d < 150*time.Millisecond || d > 10*time.Second {
			t.Fatalf("%s: answered after %s", c.name, d)
		}
		if tr.Prepared() != nil || h.pool.Count() != 0 {
			t.Fatalf("%s: the mint was collected", c.name)
		}
	}
	h.chain.Mine()
	p := h.payer()
	if _, err := p.Take(context.Background()); err != nil || tr.Prepared() == nil {
		t.Fatalf("after the mint settled: %v\n%s", err, h.n)
	}
}

// With Async the tree ahead settles on the leg's acceptance, so its change
// is unproven when the take collects it: the take then answers what it
// answers for any change waiting for a block, a NoCoinError whose Held
// counts it, and is paid once the block arrives.
func TestAFeeAfterAnAsyncMintWaitsForABlock(t *testing.T) {
	ctx := context.Background()
	h := homeOnChain(t, 1)
	tr, st := h.trees()
	tr.Payer.Async = true
	first := spend(t, tr, st, 1)
	prove := func(txid string) {
		t.Helper()
		mp, height, ok := h.chain.Proof(txid)
		if !ok {
			t.Fatalf("%s has not mined", txid)
		}
		if n, err := h.pool.Prove(txid, mp.Hex(), height); err != nil || n != 1 {
			t.Fatalf("prove %s: %d, %v", txid, n, err)
		}
	}
	prove(first.TxID().String())
	h.chain.SetHold(true)
	spend(t, tr, st, 1)
	if h.pool.Count() != 0 {
		t.Fatalf("pool %d\n%s", h.pool.Count(), h.n)
	}
	p := h.payer()
	_, err := p.Take(ctx)
	var nc *producer.NoCoinError
	prep := tr.Prepared()
	if !errors.As(err, &nc) || nc.Held != 1 || nc.Minting != 0 || prep == nil {
		t.Fatalf("got %v, prepared %v\n%s", err, prep, h.n)
	}
	if held := h.pool.Outputs(); len(held) != 1 || held[0].TxID != prep.Txid || !held[0].Unproven {
		t.Fatalf("pool %+v", held)
	}
	h.chain.Mine()
	prove(prep.Txid)
	if in, err := p.Take(ctx); err != nil || in.Tx.TxID().String() != prep.Txid {
		t.Fatalf("after the block: %v", err)
	}
}

// Two Trees pay from one pool of one coin. A mint ahead that finds no coin
// for itself, because the other's mint holds it, is skipped at once and
// waits for nothing. A tree minted on demand, which a spend cannot go on
// without, waits for the other's mint and is paid from its change. Nothing
// deadlocks.
func TestAMintAheadNeverWaitsAndAMintOnDemandDoes(t *testing.T) {
	ctx := context.Background()
	h := homeOnChain(t, 1)
	a, sta := h.trees()
	b, stb := h.trees()
	spend(t, a, sta, 1) // a's first tree, from the one coin
	spend(t, b, stb, 1) // b's first tree, from a's change; 3 left
	h.chain.SetHold(true)
	spend(t, a, sta, 1) // a mints ahead, and takes the one coin
	if h.pool.Count() != 0 {
		t.Fatalf("pool %d\n%s", h.pool.Count(), h.n)
	}
	began := time.Now()
	spend(t, b, stb, 1) // 2 left: b's mint ahead is due, and finds no coin
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("a mint ahead with no coin took %s", d)
	}
	if a.Prepared() != nil || !strings.Contains(h.n.String(), "the next funding tree was not minted ahead (fee input: bwallet: no spendable output in the wallet); it is minted when it is needed") {
		t.Fatalf("b's mint ahead waited for a's, or was not skipped:\n%s", h.n)
	}
	if strings.Contains(h.n.String(), "waiting for the change") {
		t.Fatalf("a mint ahead waited:\n%s", h.n)
	}
	spend(t, b, stb, 2) // b's first tree is spent
	h.mining(t)
	next, _, err := b.Spend(ctx, 1) // minted on demand: waits for a's mint
	if err != nil {
		t.Fatalf("%v\n%s", err, h.n)
	}
	prep := a.Prepared()
	if prep == nil || stb.adopted != 2 || stb.cur.Txid != next.TxID().String() || !strings.Contains(h.n.String(), waitingNote) {
		t.Fatalf("a prepared %v, b adopted %d\n%s", prep, stb.adopted, h.n)
	}
	// b's tree was paid for by the change of the tree a minted ahead.
	if got := next.Inputs[0].SourceTXID.String(); got != prep.Txid {
		t.Fatalf("b's tree spends %s, want the change of %s", got, prep.Txid)
	}
}

// displacingLeg is the case a broadcaster gets wrong: the network takes the
// transaction, another spend of its first input displaces it, and the leg
// still answers through inner, which may or may not know.
type displacingLeg struct {
	publish.Settler
	chain *testchain.Chain
	by    string
}

func (d *displacingLeg) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if err := d.chain.Send(tx); err != nil {
		return err
	}
	in := tx.Inputs[0]
	d.chain.SpendElsewhere(in.SourceTXID.String(), in.SourceTxOutIndex, d.by)
	return d.Settler.Submit(ctx, tx)
}

// landingLeg sends the transaction to the chain and then fails, as a leg
// whose answer was lost does.
type landingLeg struct {
	publish.Settler
	chain *testchain.Chain
}

func (l *landingLeg) Submit(_ context.Context, tx *transaction.Transaction) error {
	if err := l.chain.Send(tx); err != nil {
		return err
	}
	return errors.New("the answer was lost")
}

// accepted is a leg that has nothing more to say.
type accepted struct{}

func (accepted) Name() string                                           { return "accepted" }
func (accepted) Submit(context.Context, *transaction.Transaction) error { return nil }

// refusals are the ways a settlement leg refuses a transaction at Submit,
// set up for the fee coin it spends, and what becomes of the coin.
var refusals = []struct {
	name string
	// dropped says the coin is spent by another transaction, by the leg's
	// word or the node's, and so leaves the pool; otherwise it goes back.
	dropped bool
	setup   func(h *chainHome, p *producer.Payer, coin bwallet.Output, other string)
	want    func(err error, other string) bool
}{
	{"the refusal names another spender", true, func(h *chainHome, p *producer.Payer, _ bwallet.Output, other string) {
		// An ARC-compatible broadcaster held to the node: it took the
		// transaction, and the node shows the coin spent by another. The
		// Payer has no node of its own to ask, so the leg's word decides.
		h.chain.SetHold(true)
		p.Settler = &displacingLeg{chain: h.chain, by: other, Settler: &publish.Arcade{Base: h.base + "/arcade", Asset: h.asset,
			Poll: 10 * time.Millisecond, Verdict: 200 * time.Millisecond}}
		p.Asset = nil
	}, func(err error, other string) bool {
		var se *nodeapi.SpentError
		return errors.As(err, &se) && se.By == other && strings.Contains(err.Error(), "the network refused")
	}},
	{"the node shows the coin spent by another transaction", true, func(h *chainHome, _ *producer.Payer, coin bwallet.Output, other string) {
		// The node's sendrawtransaction refuses an input that is gone and
		// names nobody; its UTXO view names the spender.
		h.chain.SpendElsewhere(coin.TxID, coin.Vout, other)
	}, func(err error, _ string) bool {
		return strings.Contains(err.Error(), "missing or spent input 0") && !errors.Is(err, nodeapi.ErrDoubleSpent)
	}},
	{"the refusal has another cause", false, func(h *chainHome, _ *producer.Payer, _ bwallet.Output, _ string) {
		h.chain.Refuse = func(*transaction.Transaction) string { return "policy" }
	}, func(err error, _ string) bool { return strings.Contains(err.Error(), "refused: policy") }},
	{"the coin is spent and nothing says by whom", false, func(h *chainHome, p *producer.Payer, coin bwallet.Output, other string) {
		h.chain.SpendElsewhere(coin.TxID, coin.Vout, other)
		p.Asset = nil
	}, func(err error, _ string) bool { return strings.Contains(err.Error(), "missing or spent input 0") }},
	{"the coin is spent by the transaction itself", false, func(h *chainHome, p *producer.Payer, _ bwallet.Output, _ string) {
		h.chain.SetHold(true)
		p.Settler = &landingLeg{Settler: p.Settler, chain: h.chain}
	}, func(err error, _ string) bool { return strings.Contains(err.Error(), "the answer was lost") }},
}

const (
	refusedNow  = "after the refusal"
	treeNote    = "funding tree "
	paymentNote = "payment "
)

// afterRefusal fails unless the pool, in memory and on disk, holds the coin
// exactly when it should, before a GiveBack and after one.
func afterRefusal(t *testing.T, h *chainHome, p *producer.Payer, coin bwallet.Output, other string, dropped bool, what string) {
	t.Helper()
	check := func(when string) {
		t.Helper()
		if !dropped && what == paymentNote && when == refusedNow {
			// A payment's coin stays reserved until the caller's GiveBack.
			if h.pool.Count() != 0 {
				t.Fatalf("%s: the pool holds %+v before the GiveBack", when, h.pool.Outputs())
			}
			return
		}
		disk, err := bwallet.LoadPool(h.pool.Path())
		if err != nil {
			t.Fatal(err)
		}
		for where, pool := range map[string]*bwallet.Pool{"memory": h.pool, "disk": disk} {
			held := false
			for _, o := range pool.Outputs() {
				held = held || o.Outpoint() == coin.Outpoint()
			}
			switch {
			case dropped && held:
				t.Fatalf("%s, %s: the spent coin %s is in the pool\n%s", when, where, coin.Outpoint(), h.n)
			case !dropped && (!held || pool.Count() != 1):
				t.Fatalf("%s, %s: the pool holds %+v, want the coin %s alone\n%s", when, where, pool.Outputs(), coin.Outpoint(), h.n)
			}
		}
	}
	check(refusedNow)
	p.GiveBack()
	check("after a GiveBack")
	note := "its fee coin " + coin.Outpoint() + " is spent by " + other + ", so it is out of the pool and is not returned"
	want := 0
	if dropped {
		want = 1 // said once: the coin is settled in one place
	}
	if got := strings.Count(h.n.String(), note); got != want || !strings.Contains(h.n.String(), what) {
		t.Fatalf("dropped %v, noted %d time(s):\n%s", dropped, got, h.n)
	}
}

// A funding tree the leg refuses at Submit because its fee coin is already
// spent does not put the coin back: where the refusal names another
// spender, or the node shows one, the coin leaves the pool. A refusal for
// another reason, a coin nothing names a spender for, and a coin the tree
// itself spent all put it back, as before.
func TestARefusedTreeDropsACoinSpentElsewhere(t *testing.T) {
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			h := homeOnChain(t, 1)
			tr, st := h.trees()
			tr.Ahead = 0
			coin := h.pool.Outputs()[0]
			other := strings.Repeat("c4", 32)
			c.setup(h, tr.Payer, coin, other)
			_, _, err := tr.Spend(context.Background(), 1)
			if err == nil || !c.want(err, other) {
				t.Fatalf("got %v\n%s", err, h.n)
			}
			if st.adopted != 0 {
				t.Fatal("a refused tree was adopted")
			}
			afterRefusal(t, h, tr.Payer, coin, other, c.dropped, treeNote)
		})
	}
}

// The same for a tree minted ahead, whose refusal is settled when the mint
// is collected.
func TestARefusedTreeMintedAheadDropsACoinSpentElsewhere(t *testing.T) {
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			h := homeOnChain(t, 1)
			tr, st := h.trees()
			spend(t, tr, st, 1)
			coin := h.pool.Outputs()[0] // the first tree's change
			other := strings.Repeat("c5", 32)
			c.setup(h, tr.Payer, coin, other)
			spend(t, tr, st, 1) // 2 left: minted ahead with that coin, and refused
			err := tr.Wait(context.Background())
			if err == nil || !c.want(err, other) {
				t.Fatalf("got %v\n%s", err, h.n)
			}
			if tr.Prepared() != nil || st.adopted != 1 {
				t.Fatal("a refused tree was held or adopted")
			}
			afterRefusal(t, h, tr.Payer, coin, other, c.dropped, treeNote)
		})
	}
}

// A fee payment is the same: the coin Take reserved for a transaction the
// leg refuses is dropped when another transaction spent it, and the
// GiveBack an application calls after the refusal returns only a coin that
// can still be spent.
func TestARefusedPaymentDropsACoinSpentElsewhere(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, c := range refusals {
			name := c.name
			if async {
				name += ", collected later"
			}
			t.Run(name, func(t *testing.T) {
				h := homeOnChain(t, 1)
				p := h.payer()
				p.Async = async
				coin := h.pool.Outputs()[0]
				other := strings.Repeat("c6", 32)
				// The fee input is signed against the coin's real parent,
				// from the node, before the node is taken away.
				in, err := p.Take(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				c.setup(h, p, coin, other)
				change, _ := h.own.FundScript()
				tx, err := mint.Payment(context.Background(), &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = p.Settle(context.Background(), "payment", tx); err == nil || !c.want(err, other) {
					t.Fatalf("got %v\n%s", err, h.n)
				}
				afterRefusal(t, h, p, coin, other, c.dropped, paymentNote)
			})
		}
	}
}

// A payment the leg takes and the node then refuses, at once with Async or
// while its proof is waited for, drops its coin too: the refusal names the
// spender.
func TestAPaymentRefusedAfterTheLegDropsItsCoin(t *testing.T) {
	for _, async := range []bool{false, true} {
		h := homeOnChain(t, 1)
		h.chain.SetHold(true)
		p := h.payer()
		p.Async = async
		coin := h.pool.Outputs()[0]
		other := strings.Repeat("c7", 32)
		p.Settler = &displacingLeg{chain: h.chain, by: other, Settler: accepted{}}
		in, err := p.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		change, _ := h.own.FundScript()
		tx, err := mint.Payment(context.Background(), &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
		if err != nil {
			t.Fatal(err)
		}
		// The pool can hold a coin that is also reserved: a Recover that
		// answers CoinReturned for a record naming it puts it there. The
		// refusal takes it out of the pool as well as out of the
		// reservation.
		if err := h.pool.Return(coin); err != nil {
			t.Fatal(err)
		}
		_, _, err = p.Settle(context.Background(), "payment", tx)
		if !errors.Is(err, producer.ErrRefused) || !errors.Is(err, nodeapi.ErrDoubleSpent) {
			t.Fatalf("async=%v: %v", async, err)
		}
		afterRefusal(t, h, p, coin, other, true, paymentNote)
	}
}
