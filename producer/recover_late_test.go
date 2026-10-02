package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/producer"
)

// unseen takes a transaction the leg accepted out of the node's view, as
// if it had not reached the node yet, and returns it.
func (l *testChain) unseen(txid string) *transaction.Transaction {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx := l.known[txid]
	delete(l.known, txid)
	delete(l.mined, txid)
	return tx
}

// lands puts a tree the node had not seen in a block, spending its coin.
func (l *testChain) lands(tree *transaction.Transaction, coin bwallet.Output) {
	l.know(tree)
	l.mine(tree.TxID().String())
	l.spend(coin.TxID, coin.Vout, tree.TxID().String())
}

// onDisk is the pool as a restart would load it.
func onDisk(t *testing.T, tr *producer.Trees) []bwallet.Output {
	t.Helper()
	pool, err := bwallet.LoadPool(tr.Payer.Pool.Path())
	if err != nil {
		t.Fatal(err)
	}
	return pool.Outputs()
}

// A tree reaches the leg an instant before the run stops and has not
// reached the node when the next start asks: Recover answers CoinReturned
// and the coin is back in the pool. The tree lands afterwards. The
// application kept the record, so the start after that finds the tree on
// the chain: the coin it spent leaves the pool, in memory and on disk, its
// change comes in, and the tree is adopted, or held while the current tree
// has outputs.
func TestRecoverATreeThatLandsAfterCoinReturned(t *testing.T) {
	for _, c := range []struct {
		name    string
		current bool
		want    producer.RecoveryOutcome
	}{
		{"no current tree", false, producer.TreeAdopted},
		{"the current tree has outputs", true, producer.TreeHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := treesFor(t)
			l.mineOnSubmit = true
			ps := preparing(t, tr, l, st)
			need, adopted, published := uint32(1), 0, 0
			if c.current {
				// A first tree is current, with three outputs left, and the
				// next spend needs more than it has.
				spend(t, tr, st, 1)
				need, adopted, published = 5, 1, 1
			}
			tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
			crashing(t, func() { _, _, _ = tr.Spend(context.Background(), need) })
			rec := ps.only(t)
			tree := l.unseen(rec.tree.Txid)

			// The next start: the node knows no tree and shows the coin unspent.
			tr = restart(t, tr, l)
			got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || got.Outcome != producer.CoinReturned {
				t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
			}
			if held := onDisk(t, tr); len(held) != 1 || held[0] != rec.coin {
				t.Fatalf("pool after CoinReturned: %+v", held)
			}
			// The application keeps the record, and the tree lands.
			if len(ps.recs) != 1 || st.adopted != adopted {
				t.Fatalf("records %d, adopted %d", len(ps.recs), st.adopted)
			}
			l.lands(tree, rec.coin)

			// The start after: the tree is on the chain.
			tr = restart(t, tr, l)
			got, err = tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || got.Outcome != c.want || got.Tree.Txid != rec.tree.Txid || got.Tree.BumpHex == "" {
				t.Fatalf("got %v, %+v, %v\n%s", got.Outcome, got.Tree, err, n)
			}
			for where, held := range map[string][]bwallet.Output{"memory": tr.Payer.Pool.Outputs(), "disk": onDisk(t, tr)} {
				if len(held) != 1 || held[0].TxID != rec.tree.Txid || held[0].Vout != rec.tree.Count {
					t.Fatalf("pool in %s: %+v, want the tree's change alone", where, held)
				}
			}
			if !strings.Contains(n.String(), "funding tree "+rec.tree.Txid+": its fee coin "+rec.coin.Outpoint()+" is spent and is taken out of the pool") {
				t.Fatalf("notes:\n%s", n)
			}
			if c.current {
				if st.adopted != 1 || len(ps.recs) != 1 || len(l.submissions()) != 1 {
					t.Fatal("a held tree was adopted or published, or its record dropped")
				}
				if held := tr.Held(); len(held) != 1 || held[0] != got.Tree {
					t.Fatalf("held: %+v", held)
				}
				spend(t, tr, st, 3) // the current tree's last outputs: none stranded
			}
			// The recovered tree is the one carriers spend from, with nothing
			// minted for it.
			if next := spend(t, tr, st, need); next.TxID().String() != rec.tree.Txid {
				t.Fatalf("spent from %s\n%s", next.TxID(), n)
			}
			if st.adopted != adopted+1 || len(ps.recs) != 0 || len(l.submissions()) != published+1 || settled(l) != adopted+1 {
				t.Fatalf("adopted %d, records %d, published %d, settled %d", st.adopted, len(ps.recs), len(l.submissions()), settled(l))
			}
			// Asked once more with the dropped record, nothing changes.
			again, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || again.Outcome != producer.TreeAdopted || tr.Payer.Pool.Count() != 1 || st.adopted != adopted+1 {
				t.Fatalf("a later recovery: %v, %v", again.Outcome, err)
			}
		})
	}
}

// A coin that CoinReturned put back and that another transaction then
// spent is taken out of the pool by the recovery that answers CoinSpent.
func TestRecoverRemovesACoinSpentElsewhere(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	rec := ps.only(t)
	tr = restart(t, tr, l)
	if got, err := tr.Recover(context.Background(), rec.tree, rec.coin); err != nil || got.Outcome != producer.CoinReturned || tr.Payer.Pool.Count() != 1 {
		t.Fatalf("got %v, %v", got.Outcome, err)
	}
	other := strings.Repeat("ef", 32)
	l.spend(rec.coin.TxID, rec.coin.Vout, other)
	tr = restart(t, tr, l)
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.CoinSpent || got.By != other {
		t.Fatalf("got %v by %q, %v\n%s", got.Outcome, got.By, err, n)
	}
	if tr.Payer.Pool.Count() != 0 || len(onDisk(t, tr)) != 0 || st.adopted != 0 {
		t.Fatalf("a spent coin is still in the pool: %+v", tr.Payer.Pool.Outputs())
	}
}

// Adopt takes the recovered tree and the publish then fails. Recover says
// so with the outcome and a *PublishError: the tree is adopted, its record
// is dropped, and Publish alone repeats what is left.
func TestRecoverPublishFailureAfterAdopt(t *testing.T) {
	for _, c := range []struct {
		name  string
		async bool
	}{{"mined", false}, {"unmined", true}} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := treesFor(t)
			tr.Payer.Async = c.async
			l.mineOnSubmit = !c.async
			ps := preparing(t, tr, l, st)
			tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
			crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
			rec := ps.only(t)
			tr = restart(t, tr, l)
			l.mu.Lock()
			l.facadeDown = true
			l.mu.Unlock()

			got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			var pe *producer.PublishError
			if got.Outcome != producer.TreeAdopted || !errors.Is(err, producer.ErrPublish) || !errors.As(err, &pe) {
				t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
			}
			if !strings.HasPrefix(err.Error(), "publish funding tree: ") || errors.Unwrap(err) == nil {
				t.Fatalf("the error does not carry its cause: %v", err)
			}
			if pe.Tree != got.Tree || got.Tree.Txid != rec.tree.Txid || st.adopted != 1 || *st.cur != got.Tree || len(ps.recs) != 0 {
				t.Fatalf("adopted %d, records %d, tree %+v", st.adopted, len(ps.recs), got.Tree)
			}
			if len(l.submissions()) != 0 || tr.Payer.Pool.Count() != 1 {
				t.Fatalf("published %d, pool %d", len(l.submissions()), tr.Payer.Pool.Count())
			}
			// Recover does not repeat the publish: the tree is current.
			again, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || again.Outcome != producer.TreeAdopted || len(l.submissions()) != 0 {
				t.Fatalf("a second recovery: %v, %v", again.Outcome, err)
			}
			// Publish does, and says so while the hosts are still down.
			if err := tr.Publish(context.Background(), got.Tree); !errors.Is(err, producer.ErrPublish) || len(l.submissions()) != 0 {
				t.Fatalf("a publish to hosts that are down: %v", err)
			}
			l.mu.Lock()
			l.facadeDown = false
			l.mu.Unlock()
			// On a later start too: Publish needs only the record.
			tr = restart(t, tr, l)
			if err := tr.Publish(context.Background(), got.Tree); err != nil {
				t.Fatalf("%v\n%s", err, n)
			}
			subs := l.submissions()
			if len(subs) != 1 || subs[0].topic != testTopic || subjectOf(t, subs[0].beef).TxID().String() != rec.tree.Txid {
				t.Fatalf("the adopted tree was not published: %d submission(s)", len(subs))
			}
			if st.adopted != 1 {
				t.Fatal("Publish adopted a tree")
			}
		})
	}
}

// Spend treats the same failure the same way: the new tree is adopted, the
// error is a *PublishError naming it, and after Publish the next Spend
// answers the tree with nothing minted.
func TestSpendPublishFailureAfterAdopt(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	l.facadeDown = true
	tree, _, err := tr.Spend(context.Background(), 1)
	var pe *producer.PublishError
	if tree != nil || !errors.Is(err, producer.ErrPublish) || !errors.As(err, &pe) || !strings.HasPrefix(err.Error(), "publish funding tree: ") {
		t.Fatalf("got %v", err)
	}
	if st.adopted != 1 || pe.Tree != *st.cur || len(ps.recs) != 0 || len(l.submissions()) != 0 {
		t.Fatalf("adopted %d, records %d", st.adopted, len(ps.recs))
	}
	l.mu.Lock()
	l.facadeDown = false
	l.mu.Unlock()
	if err := tr.Publish(context.Background(), pe.Tree); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	// A second Publish is answered as a duplicate would be: no error.
	if err := tr.Publish(context.Background(), pe.Tree); err != nil {
		t.Fatal(err)
	}
	if subs := l.submissions(); len(subs) != 2 || subjectOf(t, subs[0].beef).TxID().String() != pe.Tree.Txid {
		t.Fatalf("%d submission(s)", len(subs))
	}
	if next := spend(t, tr, st, 1); next.TxID().String() != pe.Tree.Txid || st.adopted != 1 || settled(l) != 1 {
		t.Fatal("the next spend minted a tree")
	}

	// An error before Adopt is not a publish error: no tree is adopted.
	tr2, l2, st2, _ := treesFor(t)
	l2.mineOnSubmit = true
	st2.failing = errors.New("disk full")
	if _, _, err := tr2.Spend(context.Background(), 1); errors.Is(err, producer.ErrPublish) || st2.adopted != 0 {
		t.Fatalf("a state that cannot be saved: %v", err)
	}
	// A record that does not rebuild, or is another transaction's, is not
	// published.
	bad := pe.Tree
	bad.RawHex = "00"
	if err := tr.Publish(context.Background(), bad); !errors.Is(err, producer.ErrPublish) || !strings.Contains(err.Error(), "publish funding tree "+bad.Txid) {
		t.Fatalf("an unreadable record: %v", err)
	}
	bad = pe.Tree
	bad.Txid = strings.Repeat("12", 32)
	if err := tr.Publish(context.Background(), bad); !errors.Is(err, producer.ErrPublish) || !strings.Contains(err.Error(), "the record's bytes are transaction "+pe.Tree.Txid) {
		t.Fatalf("another transaction's record: %v", err)
	}
	if err := (&producer.Trees{}).Publish(context.Background(), pe.Tree); err == nil || errors.Is(err, producer.ErrPublish) {
		t.Fatalf("a Trees with no Payer published: %v", err)
	}
	if len(l.submissions()) != 2 {
		t.Fatal("a bad record was published")
	}
}

// Two records are on the chain at the next start while the current tree
// still has outputs: a tree minted ahead, and a larger tree minted on
// demand that the run stopped short of adopting. Both are held, in the
// order they were recovered, and Spend switches to each in turn, so the
// current tree and the first held tree are spent to their last output and
// nothing is minted.
func TestRecoverHoldsASecondTreeInOrder(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	ps := preparing(t, tr, l, st)
	spend(t, tr, st, 2) // 2 left: tree A is minted ahead
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	a := ps.only(t)
	// A spend of 5 fits neither the current tree nor A, so tree B is minted
	// on demand, paid for by A's change, and the run stops before Adopt.
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 5) })
	if len(ps.recs) != 2 || st.adopted != 1 || settled(l) != 3 {
		t.Fatalf("records %d, adopted %d, settled %d", len(ps.recs), st.adopted, settled(l))
	}
	var b prepared
	for txid, r := range ps.recs {
		if txid != a.tree.Txid {
			b = r
		}
	}
	if b.coin.TxID != a.tree.Txid || b.tree.Count != 5 {
		t.Fatalf("tree B: %+v paid by %s", b.tree, b.coin.Outpoint())
	}
	l.spend(b.coin.TxID, b.coin.Vout, b.tree.Txid)

	tr = restart(t, tr, l)
	current := st.cur.Txid
	for i, r := range []prepared{a, b, a, b} {
		got, err := tr.Recover(context.Background(), r.tree, r.coin)
		if err != nil || got.Outcome != producer.TreeHeld || got.Tree.Txid != r.tree.Txid {
			t.Fatalf("recovery %d: %v, %v\n%s", i, got.Outcome, err, n)
		}
	}
	held := tr.Held()
	if len(held) != 2 || held[0].Txid != a.tree.Txid || held[1].Txid != b.tree.Txid || tr.Prepared().Txid != a.tree.Txid {
		t.Fatalf("held: %+v", held)
	}
	if st.adopted != 1 || st.cur.Txid != current || st.cur.Remaining() != 2 || len(ps.recs) != 2 || len(l.submissions()) != 1 {
		t.Fatalf("a held tree was adopted or published: adopted %d, records %d", st.adopted, len(ps.recs))
	}
	// A's change paid for B, so only B's change is in the pool.
	if coins := tr.Payer.Pool.Outputs(); len(coins) != 1 || coins[0].TxID != b.tree.Txid {
		t.Fatalf("pool: %+v", coins)
	}

	// The current tree is spent out, then A, then B.
	if got := spend(t, tr, st, 2); got.TxID().String() != current || st.cur.Remaining() != 0 {
		t.Fatal("the current tree's outputs were stranded")
	}
	if got := spend(t, tr, st, 1); got.TxID().String() != a.tree.Txid || st.adopted != 2 || len(tr.Held()) != 1 || tr.Prepared().Txid != b.tree.Txid {
		t.Fatalf("the first switch went to %s\n%s", got.TxID(), n)
	}
	if _, kept := ps.recs[b.tree.Txid]; !kept || len(ps.recs) != 1 {
		t.Fatal("the record of the tree still held was dropped")
	}
	if got := spend(t, tr, st, 3); got.TxID().String() != a.tree.Txid || st.cur.Remaining() != 0 {
		t.Fatal("tree A's outputs were stranded")
	}
	// Nothing was minted ahead while a tree was held, though each of these
	// spends left its tree at or below Ahead.
	if err := tr.Wait(context.Background()); err != nil || settled(l) != 3 || len(ps.recs) != 1 {
		t.Fatalf("a tree was minted while one was held: settled %d, records %d, %v", settled(l), len(ps.recs), err)
	}
	if got := spend(t, tr, st, 5); got.TxID().String() != b.tree.Txid || st.adopted != 3 || len(tr.Held()) != 0 || tr.Prepared() != nil {
		t.Fatalf("the second switch went to %s\n%s", got.TxID(), n)
	}
	if _, kept := ps.recs[b.tree.Txid]; kept || len(l.submissions()) != 3 {
		t.Fatalf("the second switch kept the record or did not publish: %d submission(s)", len(l.submissions()))
	}
	// With nothing held, minting ahead resumes.
	if err := tr.Wait(context.Background()); err != nil || settled(l) != 4 || tr.Prepared() == nil {
		t.Fatalf("minting ahead did not resume: settled %d, %v", settled(l), err)
	}
	var order []string
	for _, tree := range st.all {
		order = append(order, tree.Txid)
	}
	if want := []string{current, a.tree.Txid, b.tree.Txid}; strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("adopted in the order %v", order)
	}
}
