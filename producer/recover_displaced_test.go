package producer_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
)

// displaced is the note Recover leaves for a tree the node still serves
// whose fee coin another transaction spent.
func displaced(rec prepared, by string) string {
	return "funding tree " + rec.tree.Txid + " lost a double spend: the node still serves it, without a proof, and its fee coin " + rec.coin.Outpoint() + " is spent by " + by
}

// nothingAdopted fails when a displaced tree left anything behind: a coin
// in the pool, in memory or on disk, a tree adopted, held or published.
func nothingAdopted(t *testing.T, tr *producer.Trees, l *testChain, st *memTrees, n *notes) {
	t.Helper()
	if tr.Payer.Pool.Count() != 0 || len(onDisk(t, tr)) != 0 {
		t.Fatalf("the spent coin is in the pool: memory %+v, disk %+v\n%s", tr.Payer.Pool.Outputs(), onDisk(t, tr), n)
	}
	if st.adopted != 0 || st.cur != nil || len(tr.Held()) != 0 || tr.Prepared() != nil || len(l.submissions()) != 0 {
		t.Fatalf("a displaced tree was adopted, held or published: adopted %d, held %d, published %d\n%s", st.adopted, len(tr.Held()), len(l.submissions()), n)
	}
}

// A tree reached the node and lost a double spend: the node still serves
// its transaction, with no proof, and shows the fee coin spent by another
// transaction. Recover answers CoinSpent naming that transaction, takes the
// coin out of the pool, in memory and on disk, and adopts nothing, with
// Async and without, as often as it is asked and on every start.
func TestRecoverADisplacedTree(t *testing.T) {
	for _, c := range []struct {
		name  string
		async bool
	}{{"waiting for the proof", false}, {"collected later", true}} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := treesFor(t)
			tr.Payer.Async = c.async
			tr.Payer.Timeout = 30 * time.Second
			ps := preparing(t, tr, l, st)
			tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
			crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
			rec := ps.only(t)

			// The first start asks before the tree has reached the node, so
			// the coin is back in the pool, in memory and on disk.
			tree := l.unseen(rec.tree.Txid)
			tr = restart(t, tr, l)
			if got, err := tr.Recover(context.Background(), rec.tree, rec.coin); err != nil || got.Outcome != producer.CoinReturned {
				t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
			}
			if held := onDisk(t, tr); len(held) != 1 || held[0] != rec.coin {
				t.Fatalf("pool after CoinReturned: %+v", held)
			}

			// The tree reaches the node and loses to another spend of its
			// coin. The node keeps serving it; it has no proof.
			other := strings.Repeat("d5", 32)
			l.know(tree)
			l.spend(rec.coin.TxID, rec.coin.Vout, other)

			for start := 0; start < 2; start++ {
				tr = restart(t, tr, l)
				for i := 0; i < 2; i++ {
					began := time.Now()
					got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
					if err != nil || got.Outcome != producer.CoinSpent || got.By != other || got.Tree.Txid != "" {
						t.Fatalf("start %d, recovery %d: %v by %q, %+v, %v\n%s", start, i, got.Outcome, got.By, got.Tree, err, n)
					}
					if d := time.Since(began); d > 5*time.Second {
						t.Fatalf("answered after %s, which is the wait for a proof and not the node's word", d)
					}
					nothingAdopted(t, tr, l, st, n)
				}
			}
			if !strings.Contains(n.String(), displaced(rec, other)) ||
				!strings.Contains(n.String(), "funding tree "+rec.tree.Txid+": its fee coin "+rec.coin.Outpoint()+" is spent and is taken out of the pool") {
				t.Fatalf("notes:\n%s", n)
			}
			// The application drops the record, and the next tree is minted
			// from a coin of its own.
			if len(ps.recs) != 1 {
				t.Fatalf("records %d: Recover dropped the record, which is the application's to drop", len(ps.recs))
			}
		})
	}
}

// The same tree while a current tree has outputs left is not held either.
func TestRecoverADisplacedTreeIsNotHeld(t *testing.T) {
	for _, async := range []bool{false, true} {
		tr, l, st, n := treesFor(t)
		tr.Payer.Async = async
		l.mineOnSubmit = !async
		// A second proven coin, since the first tree's change is unproven
		// while its proof is collected later.
		for _, own := range tr.Payer.Keys {
			fund(t, tr.Payer.Pool, own, 50000, 0x83)
		}
		ps := preparing(t, tr, l, st)
		spend(t, tr, st, 1) // a current tree with three outputs left
		l.mu.Lock()
		l.mineOnSubmit = false
		l.mu.Unlock()
		tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
		crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 5) })
		rec := ps.only(t)
		other := strings.Repeat("d6", 32)
		l.spend(rec.coin.TxID, rec.coin.Vout, other)

		tr = restart(t, tr, l)
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if err != nil || got.Outcome != producer.CoinSpent || got.By != other {
			t.Fatalf("async=%v: %v by %q, %v\n%s", async, got.Outcome, got.By, err, n)
		}
		if len(tr.Held()) != 0 || st.adopted != 1 || len(l.submissions()) != 1 || len(ps.recs) != 1 {
			t.Fatalf("async=%v: held %d, adopted %d, published %d, records %d", async, len(tr.Held()), st.adopted, len(l.submissions()), len(ps.recs))
		}
		for _, o := range append(tr.Payer.Pool.Outputs(), onDisk(t, tr)...) {
			if o.Outpoint() == rec.coin.Outpoint() || o.TxID == rec.tree.Txid {
				t.Fatalf("async=%v: the pool holds %s", async, o.Outpoint())
			}
		}
	}
}

// A proof settles it. A mined tree is on the chain whatever the node's
// UTXO view says of its coin, so it is adopted with its proof and the
// coin's spender is not what decides.
func TestRecoverAMinedTreeWhateverTheSpender(t *testing.T) {
	for _, async := range []bool{false, true} {
		tr, l, st, n := treesFor(t)
		tr.Payer.Async = async
		l.mineOnSubmit = true
		ps := preparing(t, tr, l, st)
		tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
		crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
		rec := ps.only(t)
		l.spend(rec.coin.TxID, rec.coin.Vout, strings.Repeat("d7", 32))

		tr = restart(t, tr, l)
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if err != nil || got.Outcome != producer.TreeAdopted || got.Tree.BumpHex == "" || got.Tree.Height != 701 || got.By != "" {
			t.Fatalf("async=%v: %v, %+v, %v\n%s", async, got.Outcome, got.Tree, err, n)
		}
		if st.adopted != 1 || len(ps.recs) != 0 || len(l.submissions()) != 1 {
			t.Fatalf("async=%v: adopted %d, records %d, published %d", async, st.adopted, len(ps.recs), len(l.submissions()))
		}
		if mined := strings.Contains(n.String(), "funding tree "+rec.tree.Txid+": mined at height 701"); mined == async {
			t.Fatalf("async=%v: notes:\n%s", async, n)
		}
	}
}

// With Async a tree the node serves with no proof, whose coin the node
// shows spent by the tree itself, is adopted as it was: its proof is
// collected later.
func TestRecoverAnUnminedTreeThatSpentItsCoin(t *testing.T) {
	tr, l, st, n := treesFor(t)
	tr.Payer.Async = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	rec := ps.only(t)
	l.spend(rec.coin.TxID, rec.coin.Vout, strings.ToUpper(rec.tree.Txid))

	tr = restart(t, tr, l)
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.TreeAdopted || got.Tree.BumpHex != "" || got.Tree.BeefHex == "" || got.By != "" {
		t.Fatalf("got %v, %+v, %v\n%s", got.Outcome, got.Tree, err, n)
	}
	if st.adopted != 1 || len(ps.recs) != 0 || len(l.submissions()) != 1 {
		t.Fatalf("adopted %d, records %d, published %d", st.adopted, len(ps.recs), len(l.submissions()))
	}
}

// The coin is spent by another transaction while Recover waits for the
// tree's proof: the wait says so, and Recover answers CoinSpent.
func TestRecoverATreeDisplacedDuringTheWait(t *testing.T) {
	tr, l, st, n := treesFor(t)
	tr.Payer.Timeout = 30 * time.Second
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	rec := ps.only(t)
	l.spend(rec.coin.TxID, rec.coin.Vout, rec.tree.Txid)

	tr = restart(t, tr, l)
	other := strings.Repeat("d8", 32)
	go func() {
		time.Sleep(100 * time.Millisecond)
		l.spend(rec.coin.TxID, rec.coin.Vout, other)
	}()
	began := time.Now()
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.CoinSpent || got.By != other {
		t.Fatalf("got %v by %q, %v\n%s", got.Outcome, got.By, err, n)
	}
	if d := time.Since(began); d < 100*time.Millisecond || d > 5*time.Second {
		t.Fatalf("answered after %s", d)
	}
	nothingAdopted(t, tr, l, st, n)
	if !strings.Contains(n.String(), displaced(rec, other)) {
		t.Fatalf("notes:\n%s", n)
	}
}

// The node serves the tree, has no proof for it, and cannot answer for the
// coin. With Async nothing is adopted on that: the error has no outcome and
// the next start asks again. Without Async the wait for the proof is the
// answer, and a tree that then mines is adopted.
func TestRecoverWhenTheNodeCannotAnswerForTheCoin(t *testing.T) {
	for _, async := range []bool{false, true} {
		tr, l, st, n := treesFor(t)
		tr.Payer.Async = async
		ps := preparing(t, tr, l, st)
		tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
		crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
		rec := ps.only(t)
		l.unseen(rec.coin.TxID) // the node no longer serves the coin's parent

		tr = restart(t, tr, l)
		if !async {
			go func() {
				time.Sleep(50 * time.Millisecond)
				l.mine(rec.tree.Txid)
			}()
		}
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if !async {
			if err != nil || got.Outcome != producer.TreeAdopted || got.Tree.BumpHex == "" || st.adopted != 1 {
				t.Fatalf("waiting: %v, %v\n%s", got.Outcome, err, n)
			}
			continue
		}
		if err == nil || got.Outcome != 0 || !nodeapi.IsHTTP(err, 404) ||
			!strings.Contains(err.Error(), "funding tree "+rec.tree.Txid+": recover: whether its fee coin "+rec.coin.Outpoint()+" is spent: ") {
			t.Fatalf("collected later: %v, %v\n%s", got.Outcome, err, n)
		}
		if errors.Is(err, producer.ErrRefused) || errors.Is(err, producer.ErrPublish) {
			t.Fatalf("the error claims more than the node said: %v", err)
		}
		nothingAdopted(t, tr, l, st, n)
		if len(ps.recs) != 1 {
			t.Fatal("the record was dropped")
		}
	}
}

// The same, on a chain that checks what it is sent: the tree is accepted
// and held unmined, another spend of its coin displaces it, and the chain
// goes on serving the tree's transaction. This is the case as an
// application meets it.
func TestRecoverADisplacedTreeOnAChain(t *testing.T) {
	for _, c := range []struct {
		name  string
		async bool
	}{{"waiting for the proof", false}, {"collected later", true}} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			chain := testchain.New(700)
			chain.Hold = true
			srv := httptest.NewServer(chain)
			t.Cleanup(srv.Close)
			asset := &nodeapi.Asset{Base: srv.URL}
			rpc := &nodeapi.RPC{URL: srv.URL + "/rpc", ID: "t"}

			own := signerOf(t, newKey(t))
			pool := poolIn(t)
			if _, _, err := bwallet.FundFromCoinbase(ctx, own, pool, rpc, asset, 101, 0); err != nil {
				t.Fatal(err)
			}
			n, st := &notes{}, &memTrees{}
			ps := &prepState{memTrees: st, recs: map[string]prepared{}}
			trees := func(pool *bwallet.Pool) *producer.Trees {
				p := &producer.Payer{Pool: pool, Tip: chain.Height(), Keys: map[string]*bwallet.Signer{own.IdentityHex(): own},
					Kept: &producer.Kept{}, Settler: &publish.RPCSettler{RPC: rpc}, Asset: asset, Async: c.async, Fees: mint.DefaultFees,
					Poll: 10 * time.Millisecond, Timeout: 30 * time.Second, Note: n.note}
				return &producer.Trees{Payer: p, State: ps, Identity: own.IdentityHex(), Count: 4, Sats: 1, Funder: "pool",
					Lock: fundingLock(own), Change: own.FundScript, Topic: testTopic, Prepare: ps.Prepare}
			}
			tr := trees(pool)
			tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
			crashing(t, func() { _, _, _ = tr.Spend(ctx, 1) })
			rec := ps.only(t)
			if chain.Tx(rec.tree.Txid) == nil || chain.Mined(rec.tree.Txid) || chain.Waiting() != 1 {
				t.Fatal("the chain does not hold the tree unmined")
			}
			other := strings.Repeat("d9", 32)
			chain.SpendElsewhere(rec.coin.TxID, rec.coin.Vout, other)

			for start := 0; start < 2; start++ {
				loaded, err := bwallet.LoadPool(pool.Path())
				if err != nil {
					t.Fatal(err)
				}
				tr = trees(loaded)
				began := time.Now()
				got, err := tr.Recover(ctx, rec.tree, rec.coin)
				if err != nil || got.Outcome != producer.CoinSpent || got.By != other {
					t.Fatalf("start %d: %v by %q, %v\n%s", start, got.Outcome, got.By, err, n)
				}
				if d := time.Since(began); d > 5*time.Second {
					t.Fatalf("start %d: answered after %s", start, d)
				}
				for _, o := range append(tr.Payer.Pool.Outputs(), onDisk(t, tr)...) {
					if o.Outpoint() == rec.coin.Outpoint() || o.TxID == rec.tree.Txid {
						t.Fatalf("start %d: the pool holds %s", start, o.Outpoint())
					}
				}
				if st.adopted != 0 || len(tr.Held()) != 0 {
					t.Fatalf("start %d: adopted %d, held %d", start, st.adopted, len(tr.Held()))
				}
			}
			if !strings.Contains(n.String(), displaced(rec, other)) {
				t.Fatalf("notes:\n%s", n)
			}
		})
	}
}
