package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
)

// noEvidence is every status the node's UTXO view answers that says
// neither "unspent" nor who spent the output, and one it does not answer.
var noEvidence = []string{"NOT_FOUND", "IMMATURE", "FROZEN", "CONFLICTING", "LOCKED", "UNSPENT"}

// A tree that never reached the node, whose fee coin the node answers for
// with a status that is no evidence, such as NOT_FOUND for an output it has
// pruned: Recover does not take that for "unspent". It answers an error
// wrapping nodeapi.ErrSpendUnknown and no outcome, the coin stays out of
// the pool and the record stays, and once the node shows the coin unspent
// the next recovery answers CoinReturned.
func TestRecoverReturnsNoCoinOnAStatusThatIsNoEvidence(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	tr = restart(t, tr, l)
	rec := ps.only(t)
	ctx := context.Background()

	for _, status := range noEvidence {
		l.answer(rec.coin.TxID, rec.coin.Vout, status)
		got, err := tr.Recover(ctx, rec.tree, rec.coin)
		if got != (producer.Recovery{}) || !errors.Is(err, nodeapi.ErrSpendUnknown) ||
			!strings.Contains(err.Error(), "whether its fee coin "+rec.coin.Outpoint()+" is spent") {
			t.Fatalf("%s: %v, %v\n%s", status, got.Outcome, err, n)
		}
		if tr.Payer.Pool.Count() != 0 || len(onDisk(t, tr)) != 0 || st.adopted != 0 || len(ps.recs) != 1 {
			t.Fatalf("%s: the pool, the state or the record changed", status)
		}
	}
	if strings.Contains(n.String(), "back in the pool") {
		t.Fatalf("notes:\n%s", n)
	}
	l.answer(rec.coin.TxID, rec.coin.Vout, "")
	if got, err := tr.Recover(ctx, rec.tree, rec.coin); err != nil || got.Outcome != producer.CoinReturned {
		t.Fatalf("unspent: %v, %v", got.Outcome, err)
	}
}

// The same with Async for a tree the node serves without a proof: the coin
// is spent, by the tree or by another transaction, and only the node's word
// on the spender decides which, so a status that is no evidence decides
// nothing.
func TestRecoverAsyncDecidesNothingOnAStatusThatIsNoEvidence(t *testing.T) {
	tr, l, st, n := treesFor(t)
	tr.Payer.Async = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	tr = restart(t, tr, l)
	rec := ps.only(t)

	for _, status := range noEvidence {
		l.answer(rec.coin.TxID, rec.coin.Vout, status)
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if got != (producer.Recovery{}) || !errors.Is(err, nodeapi.ErrSpendUnknown) {
			t.Fatalf("%s: %v, %v\n%s", status, got.Outcome, err, n)
		}
		nothingAdopted(t, tr, l, st, n)
	}
}

// A mined tree's change is taken back only where the node shows it
// unspent: change the node answers for with a status that is no evidence
// is an error, nothing is adopted, and the next recovery, once the node
// answers, adopts the tree.
func TestRecoverTakesNoChangeOnAStatusThatIsNoEvidence(t *testing.T) {
	tr, l, st, n := treesFor(t)
	l.mineOnSubmit = true
	ps := preparing(t, tr, l, st)
	tr.Payer.Settler = &crashLeg{Settler: tr.Payer.Settler, after: true}
	crashing(t, func() { _, _, _ = tr.Spend(context.Background(), 1) })
	tr = restart(t, tr, l)
	rec := ps.only(t)
	tree, err := transaction.NewTransactionFromHex(rec.tree.RawHex)
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range noEvidence {
		for vout := range tree.Outputs {
			l.answer(rec.tree.Txid, uint32(vout), status) //nolint:gosec // output index
		}
		got, err := tr.Recover(context.Background(), rec.tree, rec.coin)
		if got != (producer.Recovery{}) || !errors.Is(err, nodeapi.ErrSpendUnknown) ||
			!strings.Contains(err.Error(), "whether its change, output ") {
			t.Fatalf("%s: %v, %v\n%s", status, got.Outcome, err, n)
		}
		if st.adopted != 0 || tr.Payer.Pool.Count() != 0 || len(onDisk(t, tr)) != 0 {
			t.Fatalf("%s: adopted %d, pool %+v", status, st.adopted, tr.Payer.Pool.Outputs())
		}
	}
	for vout := range tree.Outputs {
		l.answer(rec.tree.Txid, uint32(vout), "") //nolint:gosec // output index
	}
	if got, err := tr.Recover(context.Background(), rec.tree, rec.coin); err != nil || got.Outcome != producer.TreeAdopted || st.adopted != 1 {
		t.Fatalf("answered: %v, %v\n%s", got.Outcome, err, n)
	}
}
