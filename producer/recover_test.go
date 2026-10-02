package producer_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
)

// prepared is what Prepare hands an application: the tree's record and the
// fee coin it spends.
type prepared struct {
	tree funding.Tree
	coin bwallet.Output
}

// prepState is a memTrees that also keeps what Prepare hands it, by txid,
// until Adopt names the tree, as an application's state file would.
type prepState struct {
	*memTrees
	recs    map[string]prepared
	calls   int
	failing error
	// during, when set, runs inside Prepare, after the record is kept.
	during func(tree funding.Tree, coin bwallet.Output)
}

func (s *prepState) Prepare(tree funding.Tree, coin bwallet.Output) error {
	s.calls++
	if s.failing != nil {
		return s.failing
	}
	s.recs[tree.Txid] = prepared{tree, coin}
	if s.during != nil {
		s.during(tree, coin)
	}
	return nil
}

func (s *prepState) Adopt(t funding.Tree) error {
	if err := s.memTrees.Adopt(t); err != nil {
		return err
	}
	delete(s.recs, t.Txid)
	return nil
}

// only is the one record the state holds.
func (s *prepState) only(t *testing.T) prepared {
	t.Helper()
	if len(s.recs) != 1 {
		t.Fatalf("%d prepared record(s), want one", len(s.recs))
	}
	for _, r := range s.recs {
		return r
	}
	return prepared{}
}

// preparing gives tr a state that keeps what Prepare hands it, and teaches
// the test chain the pool's coins, so its node can answer for them.
func preparing(t *testing.T, tr *producer.Trees, l *testChain, st *memTrees) *prepState {
	t.Helper()
	ps := &prepState{memTrees: st, recs: map[string]prepared{}}
	tr.State, tr.Prepare = ps, ps.Prepare
	for _, o := range tr.Payer.Pool.Outputs() {
		tx, err := funding.Rebuild(o.Raw, "", "")
		if err != nil {
			t.Fatal(err)
		}
		l.know(tx)
	}
	// Every tree the test chain has seen is one the application keeps.
	tr.Payer.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if tx, ok := l.known[txid]; ok {
			return tx, nil
		}
		return nil, errors.New("unknown tree " + txid)
	}
	return ps
}

// errCrash is the run stopping: a panic the test recovers, which leaves
// everything after it undone, as a killed process does.
var errCrash = errors.New("the run stops here")

// crashing runs f, which must stop with errCrash.
func crashing(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != errCrash {
			t.Fatalf("the run did not stop where the test stops it: %v", r)
		}
	}()
	f()
}

// crashLeg stops the run at Submit: before the tree reaches the leg, or
// after the leg has taken it.
type crashLeg struct {
	publish.Settler
	after bool
}

func (c *crashLeg) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if c.after {
		if err := c.Settler.Submit(ctx, tx); err != nil {
			return err
		}
	}
	panic(errCrash)
}

// restart is the next start of tr's application: a new Trees over the same
// state, the pool as it is on disk, and nothing held in memory.
func restart(t *testing.T, tr *producer.Trees, l *testChain) *producer.Trees {
	t.Helper()
	pool, err := bwallet.LoadPool(tr.Payer.Pool.Path())
	if err != nil {
		t.Fatal(err)
	}
	old := tr.Payer
	p := &producer.Payer{Pool: pool, Tip: old.Tip, Keys: old.Keys, Kept: &producer.Kept{Load: old.Kept.Load},
		Settler: l.arcade(), Asset: l.asset(), Async: old.Async, Fees: old.Fees, Poll: old.Poll, Timeout: old.Timeout, Note: old.Note}
	return &producer.Trees{Payer: p, State: tr.State, Identity: tr.Identity, Count: tr.Count, Sats: tr.Sats, Funder: tr.Funder,
		Lock: tr.Lock, Change: tr.Change, Facade: tr.Facade, Topic: tr.Topic, Ahead: tr.Ahead, Prepare: tr.Prepare}
}

// Prepare is given the record the tree is adopted as and the coin it
// spends, while the coin is out of the pool and before the leg has the
// tree; Adopt then drops the record.
func TestPrepareIsCalledBeforeTheLeg(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	coin := tr.Payer.Pool.Outputs()[0]
	ps.during = func(tree funding.Tree, got bwallet.Output) {
		if settled(l) != 0 || tr.Payer.Pool.Count() != 0 || st.adopted != 0 {
			t.Errorf("Prepare ran with %d tree(s) on the leg, %d coin(s) in the pool, %d adopted", settled(l), tr.Payer.Pool.Count(), st.adopted)
		}
		if got.Outpoint() != coin.Outpoint() || got.Satoshis != coin.Satoshis || got.Raw != coin.Raw || got.Bump != coin.Bump {
			t.Errorf("coin %+v, want %+v", got, coin)
		}
		tx, err := funding.Rebuild(tree.RawHex, "", "")
		if err != nil || tx.TxID().String() != tree.Txid || len(tx.Inputs) != 1 ||
			tx.Inputs[0].SourceTXID.String() != coin.TxID || tx.Inputs[0].SourceTxOutIndex != coin.Vout {
			t.Errorf("the record's bytes are not the tree spending the coin: %v", err)
		}
		want := funding.Tree{IdentityKeyHex: tr.Identity, Txid: tree.Txid, RawHex: tree.RawHex, Sats: 1, Count: 4, Funder: "pool"}
		if tree != want {
			t.Errorf("record\n%+v\nwant\n%+v", tree, want)
		}
	}
	tree, _, err := tr.Spend(context.Background(), 1)
	if err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	if ps.calls != 1 || len(ps.recs) != 0 || st.cur.Txid != tree.TxID().String() || st.cur.RawHex != tree.Hex() {
		t.Fatalf("calls %d, records left %d", ps.calls, len(ps.recs))
	}
}

// A tree minted ahead is handed to Prepare inside the Spend that starts it,
// before its background half, and its record lasts until the switch.
func TestPrepareIsCalledForATreeMintedAhead(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	ps := preparing(t, tr, l, st)
	spend(t, tr, st, 1)
	ps.during = func(funding.Tree, bwallet.Output) {
		if settled(l) != 1 {
			t.Errorf("Prepare ran with the tree minted ahead already on the leg")
		}
	}
	spend(t, tr, st, 1) // 2 left: the next is minted ahead
	if ps.calls != 2 {
		t.Fatalf("Prepare was called %d time(s) by the time Spend returned", ps.calls)
	}
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	prep := tr.Prepared()
	if rec := ps.only(t); prep == nil || rec.tree.Txid != prep.Txid || rec.tree.RawHex != prep.RawHex {
		t.Fatalf("the record is not the tree minted ahead: %+v", prep)
	}
	spend(t, tr, st, 2)
	spend(t, tr, st, 1) // the switch adopts it
	if len(ps.recs) != 0 || st.cur.Txid != prep.Txid {
		t.Fatal("the switch did not drop the record")
	}
}

// Prepare's error aborts the mint with the coin back in the pool and
// nothing on the leg, on demand and ahead.
func TestPrepareErrorAbortsTheMint(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	ps.failing = errors.New("disk full")
	_, _, err := tr.Spend(context.Background(), 1)
	if err == nil || err.Error() != "funding tree: prepare: disk full" || !errors.Is(err, ps.failing) {
		t.Fatalf("got %v", err)
	}
	if tr.Payer.Pool.Count() != 1 || settled(l) != 0 || st.adopted != 0 {
		t.Fatalf("pool %d, settled %d, adopted %d", tr.Payer.Pool.Count(), settled(l), st.adopted)
	}
	tr.Payer.GiveBack()
	if tr.Payer.Pool.Count() != 1 {
		t.Fatal("GiveBack returned the coin a second time")
	}

	tr, l, st, n := aheadFor(t)
	ps = preparing(t, tr, l, st)
	spend(t, tr, st, 1)
	coins := tr.Payer.Pool.Count()
	ps.failing = errors.New("disk full")
	spend(t, tr, st, 1) // 2 left: the mint ahead is refused by Prepare
	if err := tr.Wait(context.Background()); err != nil || tr.Prepared() != nil {
		t.Fatalf("a mint Prepare refused is in flight or held: %v", err)
	}
	if tr.Payer.Pool.Count() != coins || settled(l) != 1 {
		t.Fatalf("pool %d of %d, settled %d", tr.Payer.Pool.Count(), coins, settled(l))
	}
	if !strings.Contains(n.String(), "the next funding tree was not minted ahead (funding tree: prepare: disk full); it is minted when it is needed") {
		t.Fatalf("notes:\n%s", n)
	}
}

// Prepare is for a tree the pool pays for and settles: a dry run settles
// nothing, and a tree Fund pays for is signed and broadcast by the wallet.
func TestPrepareIsNotCalledForADryRunOrFund(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	ps := preparing(t, tr, l, st)
	tr.DryRun = true
	if _, _, err := tr.Spend(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	tr.DryRun = false
	tr.Payer.GiveBack()
	tr.Fund = func(context.Context, int) (*transaction.Transaction, *transaction.MerklePath, uint32, error) {
		tx := coinFor(&script.Script{script.OpTRUE}, 4, 0x62)
		return tx, tx.MerklePath, 90, nil
	}
	if _, _, err := tr.Spend(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if ps.calls != 0 || st.adopted != 1 {
		t.Fatalf("Prepare was called %d time(s), adopted %d", ps.calls, st.adopted)
	}
}

// The run stops after Prepare and before the leg has the tree. The pool on
// disk is short the coin and the state knows no tree; Recover finds the
// coin unspent and puts it back, as often as it is asked.
func TestRecoverACrashBetweenPrepareAndSettle(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })

	tr = restart(t, tr, l)
	rec := ps.only(t)
	if tr.Payer.Pool.Count() != 0 || st.adopted != 0 || settled(l) != 0 {
		t.Fatalf("after the crash: pool %d, adopted %d, settled %d", tr.Payer.Pool.Count(), st.adopted, settled(l))
	}
	for i := 0; i < 2; i++ {
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if err != nil || got.Outcome != producer.CoinReturned {
			t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
		}
		held := tr.Payer.Pool.Outputs()
		if len(held) != 1 || held[0] != rec.coin {
			t.Fatalf("pool after recovery %d: %+v", i, held)
		}
	}
	delete(ps.recs, rec.tree.Txid) // the application's half
	if st.adopted != 0 || len(l.submissions()) != 0 {
		t.Fatal("a tree that never reached the chain was adopted or published")
	}
	if !strings.Contains(n.String(), "funding tree "+rec.tree.Txid+" never reached the chain: its fee coin "+rec.coin.Outpoint()+" is unspent and back in the pool") {
		t.Fatalf("notes:\n%s", n)
	}
	// The coin pays for the next tree.
	spend(t, tr, st, 1)
	if st.adopted != 1 || len(ps.recs) != 0 {
		t.Fatalf("adopted %d, records %d", st.adopted, len(ps.recs))
	}
}

// The run stops once the leg has the tree and before it is adopted. The
// coin is spent and the state knows no tree; Recover finds the tree on the
// chain, takes its change, and adopts and publishes it.
func TestRecoverACrashBetweenSettleAndAdopt(t *testing.T) {
	for _, c := range []struct {
		name         string
		async, mined bool
	}{
		{"mined", false, true},
		{"mined after the restart", false, false},
		{"collected later", true, false},
		{"collected later, mined", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := treesFor(t)
			tr.Payer.Async = c.async
			l.mineOnSubmit = c.mined
			ps := preparing(t, tr, l, st)
			tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
			crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })

			tr = restart(t, tr, l)
			rec := ps.only(t)
			if tr.Payer.Pool.Count() != 0 || st.adopted != 0 || settled(l) != 1 {
				t.Fatalf("after the crash: pool %d, adopted %d, settled %d", tr.Payer.Pool.Count(), st.adopted, settled(l))
			}
			if !c.async && !c.mined {
				go func() {
					time.Sleep(50 * time.Millisecond)
					l.mine(rec.tree.Txid)
				}()
			}
			got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || got.Outcome != producer.TreeAdopted {
				t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
			}
			want := rec.tree
			if c.async && !c.mined {
				if got.Tree.BeefHex == "" {
					t.Fatal("an unmined tree was adopted without the BEEF it is kept as")
				}
				kept, err := funding.Rebuild(got.Tree.RawHex, "", got.Tree.BeefHex)
				if err != nil || kept.TxID().String() != rec.tree.Txid || kept.Inputs[0].SourceTransaction == nil ||
					kept.Inputs[0].SourceTransaction.MerklePath == nil {
					t.Fatalf("the kept BEEF does not carry the tree back to a proof: %v", err)
				}
				want.BeefHex = got.Tree.BeefHex
			} else {
				bump, _ := hex.DecodeString(got.Tree.BumpHex)
				mp, err := guard.ParseBUMP(bump, guard.DefaultBound)
				if err != nil || mp.BlockHeight != 701 {
					t.Fatalf("proof: %v", err)
				}
				want.BumpHex, want.Height = got.Tree.BumpHex, 701
			}
			if got.Tree != want || st.adopted != 1 || *st.cur != want || len(ps.recs) != 0 {
				t.Fatalf("adopted %d\n%+v\nwant\n%+v", st.adopted, got.Tree, want)
			}
			subs := l.submissions()
			if len(subs) != 1 || subs[0].topic != testTopic || subjectOf(t, subs[0].beef).TxID().String() != rec.tree.Txid {
				t.Fatalf("the recovered tree was not published: %d submission(s)", len(subs))
			}
			held := tr.Payer.Pool.Outputs()
			if len(held) != 1 || held[0].TxID != rec.tree.Txid || held[0].Unproven != (c.async && !c.mined) {
				t.Fatalf("change: %+v", held)
			}
			// Asked again, with the record the application should have
			// dropped, it does nothing.
			again, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || again.Outcome != producer.TreeAdopted || again.Tree != want || st.adopted != 1 || tr.Payer.Pool.Count() != 1 {
				t.Fatalf("a second recovery: %v, %v, adopted %d", again.Outcome, err, st.adopted)
			}
			// The recovered tree is the one carriers spend from.
			if tree := spend(t, tr, st, 1); tree.TxID().String() != rec.tree.Txid || settled(l) != 1 {
				t.Fatal("the next spend minted a tree")
			}
		})
	}
}

// The run stops with a tree minted ahead, settled and collected but not yet
// switched to. Recover holds it as the tree minted ahead again, since the
// current tree still has outputs, and takes only change that is unspent.
func TestRecoverACrashWithATreeMintedAhead(t *testing.T) {
	for _, c := range []struct {
		name        string
		changeSpent bool
	}{
		{"change in the pool", false},
		{"change already spent", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := aheadFor(t)
			ps := preparing(t, tr, l, st)
			spend(t, tr, st, 2) // 2 left: the next is minted ahead
			if err := tr.Wait(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, n)
			}
			prep := tr.Prepared()
			coins := 1
			if c.changeSpent {
				// The change paid a fee before the run stopped.
				o, err := tr.Payer.Pool.Take(tr.Payer.Tip)
				if err != nil || o.TxID != prep.Txid {
					t.Fatalf("change: %+v, %v", o, err)
				}
				l.spend(o.TxID, o.Vout, strings.Repeat("cd", 32))
				coins = 0
			}

			tr = restart(t, tr, l)
			rec := ps.only(t)
			if tr.Prepared() != nil || rec.tree.Txid != prep.Txid || st.adopted != 1 {
				t.Fatal("the restarted producer knows the tree minted ahead")
			}
			got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
			if err != nil || got.Outcome != producer.TreeHeld {
				t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
			}
			if held := tr.Prepared(); held == nil || *held != *prep || got.Tree != *prep {
				t.Fatalf("held\n%+v\nwant\n%+v", held, prep)
			}
			if st.adopted != 1 || len(l.submissions()) != 1 || len(ps.recs) != 1 {
				t.Fatal("a held tree was adopted or published, or its record dropped")
			}
			if tr.Payer.Pool.Count() != coins {
				t.Fatalf("%d coin(s) in the pool, want %d", tr.Payer.Pool.Count(), coins)
			}
			// The switch adopts it, with nothing minted or settled.
			spend(t, tr, st, 2)
			next := spend(t, tr, st, 1)
			if next.TxID().String() != prep.Txid || st.adopted != 2 || settled(l) != 2 || len(ps.recs) != 0 {
				t.Fatalf("switched to %s: adopted %d, settled %d, records %d\n%s", next.TxID(), st.adopted, settled(l), len(ps.recs), n)
			}
			if subs := l.submissions(); len(subs) != 2 || subjectOf(t, subs[1].beef).TxID().String() != prep.Txid {
				t.Fatal("the switched tree was not published")
			}
		})
	}
}

// The run stops while the tree minted ahead waits for its block: the leg
// has it, and nothing collected it. Recover waits for the proof, takes the
// change the stopped run never took, and holds the tree.
func TestRecoverACrashWithAMintAheadInFlight(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	ps := preparing(t, tr, l, st)
	spend(t, tr, st, 1)
	l.mu.Lock()
	l.mineOnSubmit = false
	l.mu.Unlock()
	run, stop := context.WithCancel(context.Background())
	defer stop()
	if _, _, err := tr.Spend(run, 1); err != nil { // 2 left: minted ahead, never mined in this run
		t.Fatal(err)
	}
	st.cur.Next++
	rec := ps.only(t)
	for deadline := time.Now().Add(5 * time.Second); settled(l) != 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if err := tr.Wait(context.Background()); err == nil || settled(l) != 2 {
		t.Fatalf("the run stopped with the tree mined or never sent: %v, settled %d", err, settled(l))
	}

	tr = restart(t, tr, l)
	if tr.Payer.Pool.Count() != 0 {
		t.Fatalf("pool after the crash: %+v", tr.Payer.Pool.Outputs())
	}
	l.mine(rec.tree.Txid)
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.TreeHeld || got.Tree.BumpHex == "" || got.Tree.Height != 702 {
		t.Fatalf("got %v, %+v, %v\n%s", got.Outcome, got.Tree, err, n)
	}
	held := tr.Payer.Pool.Outputs()
	if len(held) != 1 || held[0].TxID != rec.tree.Txid || held[0].Unproven {
		t.Fatalf("change: %+v", held)
	}
	spend(t, tr, st, 2)
	if next := spend(t, tr, st, 1); next.TxID().String() != rec.tree.Txid || st.adopted != 2 || settled(l) != 2 || len(ps.recs) != 0 {
		t.Fatalf("adopted %d, settled %d, records %d", st.adopted, settled(l), len(ps.recs))
	}
}

// A record outlives a mint that failed after Prepare. The coin went back
// when the mint failed, so Recover has nothing to return twice; and once
// the coin has paid for something else, there is nothing to recover.
func TestRecoverAfterAMintThatFailed(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	leg := &failingLeg{Settler: tr.Payer.Settler}
	leg.fails.Store(1)
	tr.Payer.Settler = leg
	if _, _, err := tr.Spend(context.Background(), 1); err == nil || err.Error() != "funding tree: settle: leg down" {
		t.Fatalf("got %v", err)
	}
	rec := ps.only(t)
	if tr.Payer.Pool.Count() != 1 {
		t.Fatal("the coin of a tree the leg refused was not put back")
	}
	tr = restart(t, tr, l)
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.CoinReturned || tr.Payer.Pool.Count() != 1 {
		t.Fatalf("got %v, %v, pool %d", got.Outcome, err, tr.Payer.Pool.Count())
	}

	other := strings.Repeat("ef", 32)
	if _, err := tr.Payer.Pool.Take(tr.Payer.Tip); err != nil {
		t.Fatal(err)
	}
	l.spend(rec.coin.TxID, rec.coin.Vout, other)
	got, err = tr.Recover(context.Background(), rec.tree, rec.coin)
	if err != nil || got.Outcome != producer.CoinSpent || got.By != other || tr.Payer.Pool.Count() != 0 {
		t.Fatalf("got %v by %q, %v, pool %d", got.Outcome, got.By, err, tr.Payer.Pool.Count())
	}
	if !strings.Contains(n.String(), "its fee coin "+rec.coin.Outpoint()+" is spent by "+other) || st.adopted != 0 {
		t.Fatalf("notes:\n%s", n)
	}
}

// The node shows the coin spent by the tree though it does not serve the
// tree: that is the tree on the chain.
func TestRecoverByTheCoinsSpender(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	tr = restart(t, tr, l)
	rec := ps.only(t)
	// The node holds the tree's block and its spends, and not its bytes.
	l.mu.Lock()
	tree := l.known[rec.tree.Txid]
	delete(l.known, rec.tree.Txid)
	l.mu.Unlock()
	l.spend(rec.coin.TxID, rec.coin.Vout, rec.tree.Txid)
	got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
	if err == nil || !strings.Contains(err.Error(), "whether its change, output 4, is spent") {
		t.Fatalf("change the node cannot answer for: %v, %v", got.Outcome, err)
	}
	if st.adopted != 0 || tr.Payer.Pool.Count() != 0 || len(ps.recs) != 1 {
		t.Fatal("an undecided recovery adopted the tree, took change or dropped the record")
	}
	l.know(tree)
	if got, err = tr.Recover(context.Background(), rec.tree, rec.coin); err != nil || got.Outcome != producer.TreeAdopted {
		t.Fatalf("got %v, %v\n%s", got.Outcome, err, n)
	}
}

// What Recover cannot decide it leaves alone: the record stays, the pool
// and the state are untouched, and the next start asks again.
func TestRecoverDecidesNothingWithoutAnAnswer(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	tr = restart(t, tr, l)
	rec := ps.only(t)
	ctx := context.Background()
	undecided := func(why string, tree funding.Tree, coin bwallet.Output, want string) {
		t.Helper()
		got, err := tr.Recover(ctx, tree, coin)
		if err == nil || got != (producer.Recovery{}) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %+v, %v", why, got.Outcome, err)
		}
		if tr.Payer.Pool.Count() != 0 || st.adopted != 0 {
			t.Fatalf("%s: the pool or the state changed", why)
		}
	}

	asset := tr.Payer.Asset
	tr.Payer.Asset = nil
	undecided("no node", rec.tree, rec.coin, "no node to ask")
	tr.Payer.Asset = asset

	bad := rec.tree
	bad.RawHex = "00"
	undecided("unreadable bytes", bad, rec.coin, "recover: ")
	bad = rec.tree
	bad.Txid = strings.Repeat("12", 32)
	undecided("another transaction's bytes", bad, rec.coin, "the record's bytes are transaction "+rec.tree.Txid)
	elsewhere := rec.coin
	elsewhere.Vout = 7
	undecided("another coin", rec.tree, elsewhere, "it does not spend the coin "+elsewhere.Outpoint())

	// A node that does not know the coin's parent cannot say it is unspent.
	l.mu.Lock()
	parent := l.known[rec.coin.TxID]
	delete(l.known, rec.coin.TxID)
	l.mu.Unlock()
	undecided("an unknown coin", rec.tree, rec.coin, "whether its fee coin "+rec.coin.Outpoint()+" is spent")
	l.know(parent)

	if _, err := (&producer.Trees{}).Recover(ctx, rec.tree, rec.coin); err == nil {
		t.Fatal("a Trees with no Payer recovered")
	}
	if got := producer.RecoveryOutcome(0).String() + producer.CoinReturned.String() + producer.CoinSpent.String() +
		producer.TreeAdopted.String() + producer.TreeHeld.String(); got != "unknowncoin returnedcoin spenttree adoptedtree held" {
		t.Fatal(got)
	}
	if got, err := tr.Recover(ctx, rec.tree, rec.coin); err != nil || got.Outcome != producer.CoinReturned {
		t.Fatalf("got %v, %v", got.Outcome, err)
	}
}
