package producer_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
)

// aheadFor is treesFor minting ahead once two outputs or fewer are left,
// over a chain that mines what it settles and a Kept that serves every tree
// the test chain has seen.
func aheadFor(t *testing.T) (*producer.Trees, *testChain, *memTrees, *notes) {
	t.Helper()
	tr, l, st, n := treesFor(t)
	tr.Ahead = 2
	l.mineOnSubmit = true
	tr.Payer.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if tx, ok := l.known[txid]; ok {
			return tx, nil
		}
		return nil, errors.New("unknown tree " + txid)
	}
	return tr, l, st, n
}

// spend is one Spend of need outputs, with the application's half: the
// current tree's Next advanced past them.
func spend(t *testing.T, tr *producer.Trees, st *memTrees, need uint32) *transaction.Transaction {
	t.Helper()
	tree, first, err := tr.Spend(context.Background(), need)
	if err != nil {
		t.Fatal(err)
	}
	if st.cur == nil || st.cur.Txid != tree.TxID().String() || first != st.cur.Next {
		t.Fatalf("spent from %s at %d, current %+v", tree.TxID(), first, st.cur)
	}
	st.cur.Next += need
	return tree
}

func settled(l *testChain) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.accepted)
}

// A spend that leaves the current tree low mints the next one in the
// background, and the spend that finds the current tree exhausted switches
// to it with nothing settled or waited for: the chain has stopped mining,
// so a mint then would fail.
func TestAheadRollsOverWithoutAStall(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	first := spend(t, tr, st, 1) // minted on demand; 3 left
	if settled(l) != 1 || tr.Prepared() != nil {
		t.Fatalf("minted ahead with 3 left:\n%s", n)
	}
	spend(t, tr, st, 1) // 2 left: the next is minted ahead
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	prep := tr.Prepared()
	if prep == nil || prep.Count != 4 || prep.Next != 0 || prep.IdentityKeyHex != tr.Identity || prep.BumpHex == "" {
		t.Fatalf("prepared %+v\n%s", prep, n)
	}
	if st.adopted != 1 || len(l.submissions()) != 1 || settled(l) != 2 {
		t.Fatal("the tree minted ahead was adopted or published before it was used")
	}
	// Its change is back in the pool, proven, the moment it is collected.
	held := tr.Payer.Pool.Outputs()
	if len(held) != 1 || held[0].TxID != prep.Txid || held[0].Unproven {
		t.Fatalf("change: %+v", held)
	}

	l.mu.Lock()
	l.mineOnSubmit = false
	l.mu.Unlock()
	tr.Payer.Timeout = 50 * time.Millisecond
	spend(t, tr, st, 2) // exactly the 2 left
	next := spend(t, tr, st, 1)
	if next.TxID().String() != prep.Txid || next == first {
		t.Fatal("the spend did not switch to the tree minted ahead")
	}
	if st.adopted != 2 || st.all[1].Txid != prep.Txid || st.all[1].Next != 0 {
		t.Fatalf("adopted %d: %+v", st.adopted, st.all)
	}
	subs := l.submissions()
	if len(subs) != 2 || subjectOf(t, subs[1].beef).TxID().String() != prep.Txid {
		t.Fatalf("the switched tree was not published: %d submission(s)", len(subs))
	}
	if settled(l) != 2 {
		t.Fatal("the switch settled a tree")
	}
	lines := n.String()
	for _, want := range []string{
		"funding tree " + first.TxID().String() + " has 2 output(s) left, so the next is minted ahead",
		"funding tree " + prep.Txid + ": 4 output(s) of 1 sat",
		"funding tree " + prep.Txid + ": mined at height 702",
		"funding tree " + prep.Txid + " is minted ahead and waits for the switch",
		"switching to funding tree " + prep.Txid + ", minted ahead",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("no %q in:\n%s", want, lines)
		}
	}
}

// However many spends run while the mint is in flight, one tree is minted
// ahead and one coin taken; a spend the current tree cannot cover waits for
// that mint rather than starting another.
func TestAheadOneMintInFlight(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	fund(t, tr.Payer.Pool, signerFor(t, tr), 50000, 0x83)
	spend(t, tr, st, 1)
	l.mu.Lock()
	l.mineOnSubmit = false
	l.mu.Unlock()
	coins := tr.Payer.Pool.Count() // the unused coin and the first tree's change
	spend(t, tr, st, 1)            // 2 left: minted ahead, not yet mined
	spend(t, tr, st, 1)
	spend(t, tr, st, 1)
	if got := tr.Payer.Pool.Count(); got != coins-1 {
		t.Fatalf("%d coin(s) taken while one mint was in flight", coins-got)
	}
	// The mint's settlement reaches the leg in the background.
	var ahead string
	for deadline := time.Now().Add(5 * time.Second); ahead == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		l.mu.Lock()
		for id := range l.accepted {
			if _, ok := l.mined[id]; !ok {
				ahead = id
			}
		}
		l.mu.Unlock()
	}
	if ahead == "" || settled(l) != 2 || tr.Prepared() != nil {
		t.Fatalf("settled %d\n%s", settled(l), n)
	}
	// The current tree is exhausted: the next spend waits for the one mint,
	// which mines a moment later.
	go func() {
		time.Sleep(50 * time.Millisecond)
		l.mine(ahead)
	}()
	next := spend(t, tr, st, 1)
	if next.TxID().String() != ahead || settled(l) != 2 || st.adopted != 2 {
		t.Fatalf("switched to %s, settled %d, adopted %d\n%s", next.TxID(), settled(l), st.adopted, n)
	}
}

// signerFor is the one key tr's Payer holds.
func signerFor(t *testing.T, tr *producer.Trees) *bwallet.Signer {
	t.Helper()
	for _, s := range tr.Payer.Keys {
		return s
	}
	t.Fatal("no key")
	return nil
}

// failingLeg refuses its first fails submissions and passes the rest on.
type failingLeg struct {
	publish.Settler
	fails atomic.Int32
}

func (f *failingLeg) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if f.fails.Add(-1) >= 0 {
		return errors.New("leg down")
	}
	return f.Settler.Submit(ctx, tx)
}

// A mint ahead that fails is reported, its coin goes back to the pool, and
// the tree is minted when it is needed, as it is with Ahead zero.
func TestAheadFailureFallsBackToMintOnDemand(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	spend(t, tr, st, 1)
	leg := &failingLeg{Settler: tr.Payer.Settler}
	leg.fails.Store(1)
	tr.Payer.Settler = leg
	coins := tr.Payer.Pool.Count()
	spend(t, tr, st, 1) // minted ahead; the leg refuses it
	err := tr.Wait(context.Background())
	if err == nil || err.Error() != "funding tree: settle: leg down" {
		t.Fatalf("got %v", err)
	}
	if tr.Payer.Pool.Count() != coins || tr.Prepared() != nil {
		t.Fatalf("the unspent coin was not put back: %d of %d", tr.Payer.Pool.Count(), coins)
	}
	if !strings.Contains(n.String(), "the next funding tree was not minted ahead (funding tree: settle: leg down); it is minted when it is needed") {
		t.Fatalf("notes:\n%s", n)
	}
	// Not retried on every spend from the same tree.
	spend(t, tr, st, 2)
	if leg.fails.Load() != 0 || tr.Wait(context.Background()) != nil {
		t.Fatal("the failed mint was retried for the same tree")
	}
	// Exhausted: the next tree is minted on demand.
	next := spend(t, tr, st, 1)
	if st.adopted != 2 || settled(l) != 2 || next.TxID().String() != st.all[1].Txid {
		t.Fatalf("adopted %d, settled %d\n%s", st.adopted, settled(l), n)
	}
}

// A dry run never mints ahead: nothing is taken, settled or started.
func TestAheadDryRun(t *testing.T) {
	tr, l, st, _ := aheadFor(t)
	tree := coinFor(&script.Script{script.OpTRUE}, 1, 0x84)
	l.know(tree)
	st.cur = &funding.Tree{IdentityKeyHex: tr.Identity, Txid: tree.TxID().String(), Count: 4, Next: 3}
	tr.DryRun = true
	if _, _, err := tr.Spend(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := tr.Wait(context.Background()); err != nil || tr.Prepared() != nil {
		t.Fatalf("a dry run minted ahead: %v", err)
	}
	if tr.Payer.Pool.Count() != 1 || settled(l) != 0 || len(l.submissions()) != 0 {
		t.Fatal("a dry run took, settled or published")
	}
}

// With Fund set, the tree minted ahead comes from Fund, called once in the
// background with Count, and the pool is not touched.
func TestAheadThroughFund(t *testing.T) {
	tr, l, st, n := aheadFor(t)
	tr.Lock, tr.Change = nil, nil
	var calls atomic.Int32
	tr.Fund = func(_ context.Context, count int) (*transaction.Transaction, *transaction.MerklePath, uint32, error) {
		i := calls.Add(1)
		tx := coinFor(&script.Script{script.OpTRUE}, 1, byte(0x90+i))
		for j := 1; j < count; j++ {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
		}
		tx.MerklePath = proofAt(tx.TxID(), 800)
		l.know(tx)
		return tx, tx.MerklePath, 800, nil
	}
	spend(t, tr, st, 2) // on demand; 2 left: minted ahead
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	prep := tr.Prepared()
	if calls.Load() != 2 || prep == nil || prep.Count != 4 || prep.Height != 800 || tr.Payer.Pool.Count() != 1 {
		t.Fatalf("calls %d, prepared %+v, pool %d", calls.Load(), prep, tr.Payer.Pool.Count())
	}
	spend(t, tr, st, 2)
	if next := spend(t, tr, st, 1); next.TxID().String() != prep.Txid || calls.Load() != 2 {
		t.Fatal("the spend did not switch to the funded tree")
	}
}

// A restart forgets a tree minted ahead and not used. The state and the
// plane never knew it, its change is already in the pool, and the restarted
// producer mints the next tree when it needs one.
func TestAheadRestartWithAnUnusedTree(t *testing.T) {
	tr, l, st, _ := aheadFor(t)
	spend(t, tr, st, 2)
	if err := tr.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	prep := tr.Prepared()
	if prep == nil {
		t.Fatal("nothing minted ahead")
	}
	// The process stops here: a new Trees over the same state and pool.
	after := *tr.Payer
	after.Kept = &producer.Kept{Load: tr.Payer.Kept.Load}
	restarted := &producer.Trees{Payer: &after, State: st, Identity: tr.Identity, Count: 4, Sats: 1, Funder: "pool",
		Lock: tr.Lock, Change: tr.Change, Facade: tr.Facade, Topic: tr.Topic}
	if restarted.Prepared() != nil {
		t.Fatal("a restarted Trees knows a tree minted ahead")
	}
	for _, s := range l.submissions() {
		if subjectOf(t, s.beef).TxID().String() == prep.Txid {
			t.Fatal("a tree the state does not know was published")
		}
	}
	if st.adopted != 1 {
		t.Fatal("the tree minted ahead was adopted")
	}
	held := restarted.Payer.Pool.Outputs()
	if len(held) != 1 || held[0].TxID != prep.Txid {
		t.Fatalf("the unused tree's change is not in the pool: %+v", held)
	}
	spend(t, restarted, st, 2)
	next := spend(t, restarted, st, 1)
	if next.TxID().String() == prep.Txid || st.adopted != 2 || settled(l) != 3 {
		t.Fatalf("the restarted producer did not mint its own tree: adopted %d, settled %d", st.adopted, settled(l))
	}
}
