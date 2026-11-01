package producer_test

import (
	"context"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/producer"
)

// An Ahead above Count keeps a pool of trees: a spend that leaves Ahead
// outputs or fewer ready, across the current tree, the held trees and the
// mints in flight, mints several trees at once, and the spends switch
// through them without minting more while enough are ready.
func TestAheadKeepsAPoolOfTrees(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	tr.Ahead = 10
	for i := byte(0); i < 6; i++ {
		fund(t, tr.Payer.Pool, signerFor(t, tr), 50000, 0xa0+i)
	}
	spend(t, tr, st, 1) // on demand; 3 left: 3 ready, so two trees are minted ahead (3 + 4 + 4 > 10)
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	held := tr.Held()
	if len(held) != 2 || settled(l) != 3 {
		t.Fatalf("held %d tree(s), settled %d, want 2 held of 3 settled\n%s", len(held), settled(l), n)
	}
	spend(t, tr, st, 3) // the current tree is spent: 8 ready, so one more is minted
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	if got := tr.Held(); len(got) != 3 || settled(l) != 4 {
		t.Fatalf("held %d tree(s), settled %d, want 3 of 4\n%s", len(got), settled(l), n)
	}
	// The switch takes the first tree held, and with 11 ready mints nothing.
	next := spend(t, tr, st, 1)
	if next.TxID().String() != held[0].Txid || st.adopted != 2 {
		t.Fatalf("switched to %s, adopted %d; want %s\n%s", next.TxID(), st.adopted, held[0].Txid, n)
	}
	if err := tr.Wait(context.Background()); err != nil || settled(l) != 4 || len(tr.Held()) != 2 {
		t.Fatalf("after the switch: %v, settled %d, held %d\n%s", err, settled(l), len(tr.Held()), n)
	}
}

// A take over the pool and a spend that both wait for the same mint: the
// one that sees the result applies it, the other wakes, and neither hangs.
// Before the Trees had a lock, the take collected the result on its own
// goroutine and the spend waited for its context.
func TestATakeAndASpendWaitForTheSameMint(t *testing.T) {
	ctx := context.Background()
	h, tr, st := oneCoinMintingAhead(t)
	spend(t, tr, st, 2) // the 2 left: the current tree is spent
	done := make(chan error, 2)
	go func() {
		_, _, err := tr.Spend(ctx, 1) // waits for the mint, then switches to it
		done <- err
	}()
	go func() {
		_, err := h.payer().Take(ctx) // waits for the tree's change
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	h.chain.Mine()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%v\n%s", err, h.n)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("a waiter hung\n%s", h.n)
		}
	}
	if st.adopted != 2 || h.pool.Count() != 0 {
		t.Fatalf("adopted %d, pool %d: the spend did not switch, or the take did not get the change\n%s", st.adopted, h.pool.Count(), h.n)
	}
}

// A tree an earlier run minted ahead and did not use is given to the next
// run's Trees with Hold: it counts as ready, so nothing is minted ahead
// while it lasts, and the spend that exhausts the current tree switches to
// it, settling nothing.
func TestHoldOffersATreeForTheSwitch(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	spend(t, tr, st, 2) // on demand; 2 left: one minted ahead
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	prep := tr.Prepared()
	if prep == nil {
		t.Fatalf("nothing minted ahead\n%s", n)
	}
	// The process stops here: a new Trees over the same state and pool,
	// given the record the application kept.
	after := *tr.Payer
	after.Kept = &producer.Kept{Load: tr.Payer.Kept.Load}
	restarted := &producer.Trees{Payer: &after, State: st, Identity: tr.Identity, Count: 4, Sats: 1, Funder: "pool",
		Lock: tr.Lock, Change: tr.Change, Facade: tr.Facade, Topic: tr.Topic, Ahead: 2}
	if err := restarted.Hold(*prep); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Hold(*prep); err != nil {
		t.Fatal(err)
	}
	if held := restarted.Held(); len(held) != 1 || held[0].Txid != prep.Txid {
		t.Fatalf("held %+v, want the one tree once", held)
	}
	before := settled(l)
	spend(t, restarted, st, 2) // the 2 left: 4 ready, so nothing is minted ahead
	next := spend(t, restarted, st, 1)
	if next.TxID().String() != prep.Txid || settled(l) != before || restarted.Prepared() != nil {
		t.Fatalf("switched to %s, settled %d (was %d)\n%s", next.TxID(), settled(l), before, n)
	}
}
