package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
)

// holds reports whether the pool holds exactly the outpoints named, in any
// order.
func holds(t *testing.T, pool *bwallet.Pool, outpoints ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, o := range pool.Outputs() {
		got[o.Outpoint()] = true
	}
	if len(got) != len(outpoints) {
		t.Fatalf("pool holds %v, want %v", got, outpoints)
	}
	for _, op := range outpoints {
		if !got[op] {
			t.Fatalf("pool holds %v, want %v", got, outpoints)
		}
	}
}

// Take leaves a coin below the fee floor in the pool, since it could pay
// for nothing, and TakeAtLeast one below what the caller asks.
func TestTakeLeavesACoinTooSmallToPay(t *testing.T) {
	ctx := context.Background()
	l := newTestChain(t)
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	small := fund(t, pool, own, 100, 0x61) // older than the next, and under the 250 sat floor
	enough := fund(t, pool, own, 5000, 0x62)
	p := payerFor(l, pool, own, &notes{})

	in, err := p.Take(ctx)
	if err != nil || in.Tx.TxID().String() != enough.TxID().String() {
		t.Fatalf("took %v: %v, want the 5000 sat coin", in.Tx, err)
	}
	_, err = p.Take(ctx)
	var nc *producer.NoCoinError
	if !errors.As(err, &nc) || !errors.Is(err, bwallet.ErrNoSpendable) ||
		err.Error() != "fee input: bwallet: no spendable output in the wallet of at least 250 sat" {
		t.Fatalf("only a coin under the floor left: %v", err)
	}
	holds(t, pool, small.TxID().String()+".0")
	p.GiveBack()
	if _, err := p.TakeAtLeast(ctx, 5001); !errors.As(err, &nc) {
		t.Fatalf("no coin of 5001 sat: %v", err)
	}
	if in, err := p.TakeAtLeast(ctx, 5000); err != nil || in.Tx.TxID().String() != enough.TxID().String() {
		t.Fatalf("a coin of exactly the minimum: %v", err)
	}
}

// A coin Take cannot sign for is back in the pool when the error returns,
// with no GiveBack.
func TestTakeReturnsACoinItCannotSignFor(t *testing.T) {
	l := newTestChain(t)
	pool := poolIn(t)
	own, stranger := signerOf(t, newKey(t)), signerOf(t, newKey(t))
	coin := fund(t, pool, stranger, 5000, 0x63)
	p := payerFor(l, pool, own, &notes{})
	var nk *producer.NoKeyError
	if _, err := p.Take(context.Background()); !errors.As(err, &nk) {
		t.Fatalf("got %v", err)
	}
	holds(t, pool, coin.TxID().String()+".0")
	p.GiveBack()
	holds(t, pool, coin.TxID().String()+".0")
}

// A tree the pool pays for that fails before it reaches the settlement leg
// puts its coin back before Spend returns, whichever step failed, and a
// GiveBack after it does not add it twice.
func TestSpendReturnsTheCoinOnEveryFailureBeforeTheLeg(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name  string
		sats  uint64 // the one coin's value; zero is treesFor's 50000
		setup func(tr *producer.Trees, l *testChain)
		want  func(error) bool
	}{
		{"the coin cannot pay the fee", 300, nil, func(err error) bool { return errors.Is(err, mint.ErrInsufficient) }},
		{"no change script", 0, func(tr *producer.Trees, _ *testChain) { tr.Change = nil },
			func(err error) bool { return err.Error() == "producer: Trees needs a Change script" }},
		{"the change script fails", 0, func(tr *producer.Trees, _ *testChain) {
			tr.Change = func() (*script.Script, error) { return nil, boom }
		}, func(err error) bool { return errors.Is(err, boom) }},
		{"the lock fails", 0, func(tr *producer.Trees, _ *testChain) {
			tr.Lock = func(context.Context) (*script.Script, error) { return nil, boom }
		}, func(err error) bool { return errors.Is(err, boom) }},
		{"a bad value", 0, func(tr *producer.Trees, _ *testChain) { tr.Sats = 0 },
			func(err error) bool { return strings.HasPrefix(err.Error(), "mint: a funding tree needs") }},
		{"no settlement leg", 0, func(tr *producer.Trees, _ *testChain) { tr.Payer.Settler = nil },
			func(err error) bool { return err.Error() == "funding tree: settle: no settlement leg" }},
		{"the leg refuses the tree", 0, func(tr *producer.Trees, _ *testChain) {
			leg := &failingLeg{Settler: tr.Payer.Settler}
			leg.fails.Store(1)
			tr.Payer.Settler = leg
		}, func(err error) bool { return err.Error() == "funding tree: settle: leg down" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, _ := treesFor(t)
			l.mineOnSubmit = true
			if c.sats != 0 {
				tr.Payer.Pool = poolIn(t)
				fund(t, tr.Payer.Pool, signerFor(t, tr), c.sats, 0x64)
			}
			coin := tr.Payer.Pool.Outputs()[0].Outpoint()
			if c.setup != nil {
				c.setup(tr, l)
			}
			_, _, err := tr.Spend(context.Background(), 1)
			if err == nil || !c.want(err) {
				t.Fatalf("got %v", err)
			}
			holds(t, tr.Payer.Pool, coin)
			tr.Payer.GiveBack()
			holds(t, tr.Payer.Pool, coin)
			if st.adopted != 0 {
				t.Fatal("a tree that failed was recorded")
			}
		})
	}
}

// Once the tree has reached the leg its coin is spent: a failure after that
// does not return it, and neither does a GiveBack, even after a tree that
// settled and was published.
func TestSpendReleasesTheCoinOnceTheTreeIsOnTheLeg(t *testing.T) {
	t.Run("the proof never comes", func(t *testing.T) {
		tr, _, _, _ := treesFor(t)
		tr.Payer.Timeout = 50 * time.Millisecond
		if _, _, err := tr.Spend(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "waiting for a proof") {
			t.Fatalf("got %v", err)
		}
		tr.Payer.GiveBack()
		if n := tr.Payer.Pool.Count(); n != 0 {
			t.Fatalf("a coin spent by a tree on the leg was returned: %d held", n)
		}
	})
	t.Run("settled and published", func(t *testing.T) {
		tr, l, _, _ := treesFor(t)
		l.mineOnSubmit = true
		coin := tr.Payer.Pool.Outputs()[0].Outpoint()
		tree, _, err := tr.Spend(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		tr.Payer.GiveBack()
		held := tr.Payer.Pool.Outputs()
		if len(held) != 1 || held[0].Outpoint() == coin || held[0].TxID != tree.TxID().String() {
			t.Fatalf("pool holds %+v, want the tree's change alone", held)
		}
	})
}

// A tree is paid for by a coin that can cover its outputs and the fee
// floor: an older coin too small for it stays in the pool.
func TestSpendSkipsACoinTooSmallForTheTree(t *testing.T) {
	tr, l, _, _ := treesFor(t)
	l.mineOnSubmit = true
	pool := poolIn(t)
	own := signerFor(t, tr)
	small := fund(t, pool, own, 200, 0x65)
	fund(t, pool, own, 50000, 0x66)
	tr.Payer.Pool = pool
	tree, _, err := tr.Spend(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	holds(t, pool, small.TxID().String()+".0", tree.TxID().String()+".4")
}

// A mint ahead takes only a coin that can pay for the tree it mints; with
// none, nothing is minted ahead and the pool is untouched.
func TestAheadTakesOnlyACoinThatCanPay(t *testing.T) {
	tr, _, st, n := aheadFor(t)
	spend(t, tr, st, 1) // mints the first tree from the one coin; its change is the pool
	pool := poolIn(t)
	small := fund(t, pool, signerFor(t, tr), 200, 0x67)
	tr.Payer.Pool = pool
	spend(t, tr, st, 1) // leaves 2: a mint ahead is due
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.Prepared() != nil || !strings.Contains(n.String(), "of at least 254 sat") {
		t.Fatalf("minted ahead from a coin too small:\n%s", n)
	}
	holds(t, pool, small.TxID().String()+".0")
}
