package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
)

// Reserved answers the coin a take reserved, exactly as the pool held it,
// until it is given back, and nothing for an input the Payer did not take.
func TestReservedIsTheCoinTaken(t *testing.T) {
	l := newTestChain(t)
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 5000, 0x71)
	want := pool.Outputs()[0]
	p := payerFor(l, pool, own, &notes{})

	in, err := p.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.Reserved(in)
	if !ok || got != want {
		t.Fatalf("reserved %+v, %v; want %+v", got, ok, want)
	}
	other := coinFor(&script.Script{script.OpTRUE}, 1, 0x72)
	if _, ok := p.Reserved(mint.Input{Tx: other}); ok {
		t.Fatal("an input the Payer did not take is reserved")
	}
	if _, ok := p.Reserved(mint.Input{}); ok {
		t.Fatal("an input with no transaction is reserved")
	}
	p.GiveBack()
	if _, ok := p.Reserved(in); ok {
		t.Fatal("a coin given back is still reserved")
	}
}

// Without Async a tree minted ahead waits for its proof under its Payer's
// Timeout. When the chain does not mine in time, the take that waited for
// the mint finds no coin, and says why: the tree reached the leg and its
// change is not in the pool, which is not an empty wallet. Every later take
// over the pool says the same until Recover settles the tree, and then the
// take is paid from its change.
func TestAFeeAfterAMintThatTimedOutIsNotAnEmptyWallet(t *testing.T) {
	ctx := context.Background()
	h := homeOnChain(t, 1)
	tr, st := h.trees()
	spend(t, tr, st, 1) // minted on demand from the one coin
	var rec funding.Tree
	var coin bwallet.Output
	tr.Prepare = func(tree funding.Tree, c bwallet.Output) error { rec, coin = tree, c; return nil }
	h.chain.SetHold(true)
	tr.Payer.Timeout = 300 * time.Millisecond
	spend(t, tr, st, 1) // 2 left: the next is minted ahead from the one coin
	if h.pool.Count() != 0 || rec.Txid == "" {
		t.Fatalf("the test did not reach the case: pool %d, record %q\n%s", h.pool.Count(), rec.Txid, h.n)
	}

	for i := 0; i < 2; i++ {
		_, err := h.payer().Take(ctx)
		var nc *producer.NoCoinError
		if !errors.As(err, &nc) || !errors.Is(err, bwallet.ErrNoSpendable) || nc.Unsettled != 1 || nc.Minting != 0 || nc.Held != 0 ||
			!strings.Contains(err.Error(), "1 funding tree(s) minted ahead reached the settlement leg and were not proven in time") {
			t.Fatalf("take %d: %v\n%s", i, err, h.n)
		}
	}
	if tr.Prepared() != nil {
		t.Fatal("a tree that did not mine is held for the switch")
	}

	h.chain.SetHold(false)
	h.chain.Mine()
	got, err := tr.Recover(ctx, rec, coin)
	if err != nil || got.Outcome != producer.TreeHeld {
		t.Fatalf("recover: %v, %v\n%s", got.Outcome, err, h.n)
	}
	in, err := h.payer().Take(ctx)
	if err != nil || in.Tx.TxID().String() != rec.Txid {
		t.Fatalf("after recovery: %v", err)
	}
}

// A mint ahead that is refused after the leg (its coin spent by another
// transaction) leaves no tree that may mine, so it is not counted.
func TestARefusedMintAheadIsNotUnsettled(t *testing.T) {
	h := homeOnChain(t, 1)
	tr, st := h.trees()
	spend(t, tr, st, 1)
	h.chain.SetHold(true)
	tr.Payer.Settler = &displacingLeg{Settler: tr.Payer.Settler, chain: h.chain, by: strings.Repeat("c3", 32)}
	spend(t, tr, st, 1)
	_, err := h.payer().Take(context.Background())
	var nc *producer.NoCoinError
	if !errors.As(err, &nc) || nc.Unsettled != 0 || !strings.Contains(h.n.String(), "refused by the network") {
		t.Fatalf("%v\n%s", err, h.n)
	}
}

// unprovenTree is a current tree adopted with no proof, as an earlier
// version adopted one under Async, spending output 0 of a parent the test
// chain knows.
func unprovenTree(t *testing.T, tr *producer.Trees, l *testChain, st *memTrees, salt byte) (*transaction.Transaction, *transaction.Transaction) {
	t.Helper()
	parent := coinFor(&script.Script{script.OpTRUE}, 10, salt)
	tree := transaction.NewTransaction()
	tree.AddInput(&transaction.TransactionInput{SourceTXID: parent.TxID(), SourceTxOutIndex: 0,
		UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for i := 0; i < 4; i++ {
		tree.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	}
	l.know(parent)
	st.cur = &funding.Tree{IdentityKeyHex: tr.Identity, Txid: tree.TxID().String(), RawHex: tree.Hex(), Sats: 1, Count: 4, Next: 1}
	tr.Payer.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		if txid == tree.TxID().String() {
			return tree, nil
		}
		return nil, errors.New("unknown tree " + txid)
	}
	return parent, tree
}

// A current tree with no proof whose input the node shows spent by another
// transaction never mines. A spend does not use it: it is reported, and a
// new tree is minted and adopted in its place.
func TestSpendReplacesACurrentTreeThatNeverMines(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	parent, old := unprovenTree(t, tr, l, st, 0x73)
	other := strings.Repeat("c4", 32)
	l.spend(parent.TxID().String(), 0, other)
	var displaced funding.Tree
	var by string
	tr.Displaced = func(tree funding.Tree, b string) { displaced, by = tree, b }

	tree, vout, err := tr.Spend(context.Background(), 1)
	if err != nil {
		t.Fatalf("%v\n%s", err, n)
	}
	if tree.TxID().String() == old.TxID().String() || vout != 0 || st.adopted != 1 || st.cur.Txid != tree.TxID().String() {
		t.Fatalf("spent from %s at %d, adopted %d\n%s", tree.TxID(), vout, st.adopted, n)
	}
	if displaced.Txid != old.TxID().String() || by != other {
		t.Fatalf("displaced %q by %q", displaced.Txid, by)
	}
	want := "funding tree " + old.TxID().String() + ", the current tree, has no proof and its input " +
		parent.TxID().String() + ".0 is spent by " + other + ": it never mines"
	if !strings.Contains(n.String(), want) {
		t.Fatalf("notes:\n%s", n)
	}
}

// A current tree with no proof whose input the node shows spent by the tree
// itself, or cannot answer for, is used; the node is asked once a run, and
// never for a tree this Trees adopted itself.
func TestSpendKeepsACurrentTreeThatMayMine(t *testing.T) {
	tr, l, st, n := treesFor(t)
	parent, old := unprovenTree(t, tr, l, st, 0x74)
	l.spend(parent.TxID().String(), 0, old.TxID().String())
	tr.Displaced = func(funding.Tree, string) { t.Fatal("a tree that may mine was displaced") }

	tree, vout, err := tr.Spend(context.Background(), 1)
	if err != nil || tree != old || vout != 1 || st.adopted != 0 {
		t.Fatalf("got %v at %d, adopted %d, %v\n%s", tree == old, vout, st.adopted, err, n)
	}
	st.cur.Next++
	// Asked once: a later spend in the run uses the tree whatever the node
	// says by then.
	l.spend(parent.TxID().String(), 0, strings.Repeat("c5", 32))
	if tree, _, err := tr.Spend(context.Background(), 1); err != nil || tree != old {
		t.Fatalf("second spend: %v", err)
	}

	// An input the node does not know is not evidence.
	tr2, l2, st2, _ := treesFor(t)
	_, old2 := unprovenTree(t, tr2, l2, st2, 0x75)
	l2.mu.Lock()
	l2.known = map[string]*transaction.Transaction{}
	l2.mu.Unlock()
	if tree, _, err := tr2.Spend(context.Background(), 1); err != nil || tree != old2 {
		t.Fatalf("unknown input: %v", err)
	}
}
