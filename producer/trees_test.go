package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/producer"
)

// treesFor is a Trees over a fresh test chain, pool and memory state, with
// one proven coin in the pool.
func treesFor(t *testing.T) (*producer.Trees, *testChain, *memTrees, *notes) {
	t.Helper()
	l, n := newTestChain(t), &notes{}
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 50000, 0x81)
	st := &memTrees{}
	p := payerFor(l, pool, own, n)
	return &producer.Trees{
		Payer: p, State: st, Identity: own.IdentityHex(), Count: 4, Sats: 1, Funder: "pool",
		Lock: fundingLock(own), Change: own.FundScript, Facade: l.facade(), Topic: testTopic,
	}, l, st, n
}

// A spend the current tree can cover takes its next output and mints
// nothing.
func TestSpendUsesTheCurrentTreeWhileItFits(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	tree := coinFor(&script.Script{script.OpTRUE}, 1, 0x82)
	st.cur = &funding.Tree{IdentityKeyHex: tr.Identity, Txid: tree.TxID().String(), Count: 4, Next: 1}
	tr.Payer.Kept.Load = func(string) (*transaction.Transaction, error) { return tree, nil }
	lockAsked := false
	lock := tr.Lock
	tr.Lock = func(ctx context.Context) (*script.Script, error) { lockAsked = true; return lock(ctx) }

	got, vout, err := tr.Spend(context.Background(), 3)
	if err != nil || got != tree || vout != 1 {
		t.Fatalf("got %v vout %d err %v", got == tree, vout, err)
	}
	if st.adopted != 0 || len(l.submissions()) != 0 || lockAsked || tr.Payer.Pool.Count() != 1 {
		t.Fatal("a tree that fits was replaced, or the wallet was asked for a lock")
	}
	// A current tree whose kept copy cannot be rebuilt is an error, not a
	// silent replacement.
	tr.Payer.Kept = &producer.Kept{Load: func(string) (*transaction.Transaction, error) { return nil, errors.New("gone") }}
	if _, _, err := tr.Spend(context.Background(), 3); err == nil || err.Error() != "funding tree: gone" {
		t.Fatalf("an unreadable tree: %v", err)
	}
}

// A tree with too few outputs left, or locked to another identity, is
// replaced by one minted from the pool, settled, recorded before it is
// published, and published to the topic; its change goes back into the pool.
func TestSpendMintsTheNextTree(t *testing.T) {
	for _, c := range []struct {
		name  string
		async bool
		cur   func(id string) *funding.Tree
	}{
		{"no current tree", false, func(string) *funding.Tree { return nil }},
		{"too few left", false, func(id string) *funding.Tree {
			return &funding.Tree{IdentityKeyHex: id, Txid: "old", Count: 4, Next: 3}
		}},
		{"another identity's", false, func(string) *funding.Tree {
			return &funding.Tree{IdentityKeyHex: "predecessor", Txid: "old", Count: 16}
		}},
		{"collected later", true, func(string) *funding.Tree { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, l, st, n := treesFor(t)
			tr.Payer.Async = c.async
			l.mineOnSubmit = !c.async
			st.cur = c.cur(tr.Identity)

			tree, vout, err := tr.Spend(context.Background(), 2)
			if err != nil {
				t.Fatalf("%v\n%s", err, n)
			}
			if vout != 0 || st.adopted != 1 || st.cur.Txid != tree.TxID().String() {
				t.Fatalf("vout %d, adopted %d", vout, st.adopted)
			}
			want := funding.Tree{IdentityKeyHex: tr.Identity, Txid: tree.TxID().String(), RawHex: tree.Hex(), Sats: 1, Count: 4, Funder: "pool"}
			if c.async {
				beef, err := funding.KeepBEEF(tree, nil)
				if err != nil {
					t.Fatal(err)
				}
				want.BeefHex = beef
			} else {
				want.BumpHex, want.Height = tree.MerklePath.Hex(), 701
			}
			if *st.cur != want {
				t.Fatalf("adopted\n%+v\nwant\n%+v", *st.cur, want)
			}
			for i := 0; i < 4; i++ {
				if _, ok := carrier.DecodeFunding(tree.Outputs[i].LockingScript, testParams.FundingTag); !ok || tree.Outputs[i].Satoshis != 1 {
					t.Fatalf("output %d is not a funding output", i)
				}
			}
			subs := l.submissions()
			if len(subs) != 1 || subs[0].topic != testTopic || subjectOf(t, subs[0].beef).TxID().String() != tree.TxID().String() {
				t.Fatalf("the tree was not published to the topic: %d submission(s)", len(subs))
			}
			held := tr.Payer.Pool.Outputs()
			if len(held) != 1 || held[0].TxID != tree.TxID().String() || held[0].Unproven != c.async {
				t.Fatalf("change: %+v", held)
			}
			id := tree.TxID().String()
			last := n.all()[len(n.all())-1]
			if !strings.HasPrefix(n.all()[0], "funding tree "+id+": 4 output(s) of 1 sat") || last != "funding tree published: admitted 1 output(s)" {
				t.Fatalf("notes:\n%s", n)
			}
		})
	}
}

// A spend larger than the configured count gets a tree that large, and says
// so.
func TestSpendSizesTheTreeToTheSpend(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	tree, _, err := tr.Spend(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	if st.cur.Count != 6 || len(tree.Outputs) < 6 {
		t.Fatalf("count %d", st.cur.Count)
	}
	if n.all()[0] != "this transition spends 6 outputs, so the tree is minted with 6 rather than 4" {
		t.Fatalf("notes:\n%s", n)
	}
}

// A dry run builds the tree and records, settles and publishes nothing; the
// fee input stays reserved until GiveBack.
func TestSpendDryRunRecordsNothing(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	tr.DryRun = true
	tree, vout, err := tr.Spend(context.Background(), 1)
	if err != nil || tree == nil || vout != 0 {
		t.Fatal(err)
	}
	l.mu.Lock()
	settled := len(l.accepted)
	l.mu.Unlock()
	if st.adopted != 0 || settled != 0 || len(l.submissions()) != 0 {
		t.Fatal("a dry run recorded, settled or published")
	}
	if tr.Payer.Pool.Count() != 0 {
		t.Fatal("the fee input was given back before GiveBack")
	}
	tr.Payer.GiveBack()
	if tr.Payer.Pool.Count() != 1 {
		t.Fatal("GiveBack did not return the fee input")
	}
}

// With Fund set, something else pays for and settles the tree: the pool,
// the lock and the change script are not touched, and DryRun does not
// apply.
func TestSpendThroughFund(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	tr.DryRun = true
	tr.Funder = "wallet"
	tr.Lock, tr.Change = nil, nil
	var asked int
	built := transaction.NewTransaction()
	for i := 0; i < 5; i++ {
		built.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	}
	tr.Fund = func(_ context.Context, count int) (*transaction.Transaction, *transaction.MerklePath, uint32, error) {
		asked = count
		return built, nil, 0, nil
	}
	tree, _, err := tr.Spend(context.Background(), 5)
	if err != nil || tree != built || asked != 5 {
		t.Fatalf("asked %d: %v", asked, err)
	}
	if st.cur.Funder != "wallet" || st.cur.BeefHex == "" || tr.Payer.Pool.Count() != 1 || len(l.submissions()) != 1 {
		t.Fatalf("state %+v, pool %d", st.cur, tr.Payer.Pool.Count())
	}
	tr.Fund = func(context.Context, int) (*transaction.Transaction, *transaction.MerklePath, uint32, error) {
		return nil, nil, 0, errors.New("the wallet refused")
	}
	st.cur = nil
	if _, _, err := tr.Spend(context.Background(), 1); err == nil || err.Error() != "the wallet refused" {
		t.Fatalf("Fund's error: %v", err)
	}
}

// A bad count or value is refused before the wallet is asked for a lock, in
// mint's words, and after the fee input is reserved.
func TestSpendRefusesABadValueBeforeAskingForTheLock(t *testing.T) {
	tr, _, _, _ := treesFor(t)
	tr.Sats = 0
	tr.Lock = func(context.Context) (*script.Script, error) { t.Fatal("the lock was asked for"); return nil, nil }
	_, _, err := tr.Spend(context.Background(), 1)
	if err == nil || err.Error() != "mint: a funding tree needs at least one output of at least one satoshi" {
		t.Fatalf("got %v", err)
	}
}

// Failures after the tree is settled leave it recorded: the state knows
// every tree that reached the chain.
func TestSpendRecordsTheTreeBeforePublishing(t *testing.T) {
	tr, l, st, _ := treesFor(t)
	l.mineOnSubmit = true
	l.facadeDown = true
	_, _, err := tr.Spend(context.Background(), 1)
	if err == nil || !strings.HasPrefix(err.Error(), "publish funding tree: ") {
		t.Fatalf("got %v", err)
	}
	if st.adopted != 1 {
		t.Fatal("a settled tree was not recorded")
	}

	tr2, l2, st2, _ := treesFor(t)
	l2.mineOnSubmit = true
	st2.failing = errors.New("disk full")
	if _, _, err := tr2.Spend(context.Background(), 1); err == nil || err.Error() != "disk full" {
		t.Fatalf("a state that cannot be saved: %v", err)
	}
	if len(l2.submissions()) != 0 {
		t.Fatal("a tree the state does not know was published")
	}

	tr3, _, _, _ := treesFor(t)
	tr3.Payer.Pool = poolIn(t)
	var nc *producer.NoCoinError
	if _, _, err := tr3.Spend(context.Background(), 1); !errors.As(err, &nc) {
		t.Fatalf("an empty pool: %#v", err)
	}
	if _, _, err := (&producer.Trees{}).Spend(context.Background(), 1); err == nil {
		t.Fatal("a Trees with no Payer spent")
	}
}

func TestIndexKeepsTheHistoryInStep(t *testing.T) {
	all := []funding.Tree{{Txid: "a", Next: 1}, {Txid: "b", Next: 2}}
	cur := &funding.Tree{Txid: "b", Next: 5}
	all = producer.Index(all, cur)
	if len(all) != 2 || all[1].Next != 5 || all[0].Next != 1 {
		t.Fatalf("%+v", all)
	}
	all = producer.Index(all, &funding.Tree{Txid: "c", Next: 1})
	if len(all) != 3 || all[2].Txid != "c" {
		t.Fatalf("%+v", all)
	}
	if got := producer.Index(all, nil); len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	var none []funding.Tree
	if got := producer.Index(none, cur); len(got) != 1 || got[0] != *cur {
		t.Fatalf("%+v", got)
	}
}
