package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
)

// doubleSpent sets up the case arcade gets wrong: a payment arcade answers
// ACCEPTED_BY_NETWORK for while the node shows its fee coin spent by another
// transaction. It never mines.
func doubleSpent(t *testing.T) (*testChain, *producer.Payer, *transaction.Transaction, string) {
	t.Helper()
	l := newTestChain(t)
	l.verdict = "ACCEPTED_BY_NETWORK"
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	parent := fund(t, pool, own, 5000, 0x61)
	p := payerFor(l, pool, own, &notes{})
	tx := paymentFrom(t, p, own)
	other := strings.Repeat("e7", 32)
	l.know(parent)
	l.spend(parent.TxID().String(), 0, other)
	return l, p, tx, other
}

func wantDoubleSpent(t *testing.T, what string, err error, other string) {
	t.Helper()
	if !errors.Is(err, producer.ErrRefused) || !errors.Is(err, nodeapi.ErrDoubleSpent) || !strings.Contains(err.Error(), "spent by "+other) {
		t.Fatalf("%s: %v, want a refusal naming the spender", what, err)
	}
	var se *nodeapi.SpentError
	if !errors.As(err, &se) || se.By != other || se.Input != 0 {
		t.Fatalf("%s: %v is not a SpentError for input 0", what, err)
	}
}

// Waiting for a proof stops on the node's word, not on the timeout, whatever
// arcade says.
func TestSettleRefusesADoubleSpendPromptly(t *testing.T) {
	ctx := context.Background()
	for _, async := range []bool{false, true} {
		_, p, tx, other := doubleSpent(t)
		p.Async = async
		p.Timeout = 30 * time.Second
		start := time.Now()
		_, _, err := p.Settle(ctx, "payment", tx)
		wantDoubleSpent(t, "settle", err, other)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("async=%v: refused after %s, which is the timeout and not the node's word", async, d)
		}
	}
	_, p, tx, other := doubleSpent(t)
	p.Timeout = 30 * time.Second
	start := time.Now()
	_, _, err := p.Await(ctx, "payment", tx)
	wantDoubleSpent(t, "await", err, other)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("await refused after %s", d)
	}
}

// The publish leg held to the node refuses at Submit.
func TestArcadeWithANodeRefusesADoubleSpend(t *testing.T) {
	l, _, tx, other := doubleSpent(t)
	a := l.arcade()
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatalf("an arcade not held to the node: %v", err)
	}
	a.Asset = l.asset()
	err := a.Submit(context.Background(), tx)
	if !errors.Is(err, nodeapi.ErrDoubleSpent) || !strings.Contains(err.Error(), "the network refused") || !strings.Contains(err.Error(), other) {
		t.Fatalf("held to the node: %v", err)
	}
}

// Proofs refuses an unmined transaction the node shows double spent, given
// the transaction (OfTx, or Of through Tx), and a collection reports it as
// a refusal.
func TestProofsRefuseADoubleSpend(t *testing.T) {
	ctx := context.Background()
	l, p, tx, other := doubleSpent(t)
	if err := p.Settler.Submit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	id := tx.TxID().String()
	pr := producer.Proofs{Arcade: l.arcade(), Asset: l.asset()}
	if _, _, err := pr.Of(ctx, id); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("by txid alone: %v", err)
	}
	_, _, err := pr.OfTx(ctx, tx)
	wantDoubleSpent(t, "OfTx", err, other)
	pr.Tx = func(string) (*transaction.Transaction, error) { return tx, nil }
	_, _, err = pr.Of(ctx, id)
	wantDoubleSpent(t, "Of with Tx", err, other)

	var refused error
	c := producer.Collector{Proofs: producer.Proofs{Arcade: l.arcade(), Asset: l.asset()}}
	c.Collect(ctx, []producer.Pending{{What: "payment", Txid: id, RawHex: tx.Hex(), Refused: func(err error) { refused = err }}})
	wantDoubleSpent(t, "collect", refused, other)

	// Once it mines (the node's view was wrong, or a reorg), it is proven.
	l.mine(id)
	if mp, _, err := pr.OfTx(ctx, tx); err != nil || mp == nil {
		t.Fatalf("mined: %v", err)
	}
}

// An input spent by the transaction itself is the ordinary case, and an
// input the node does not know is passed over.
func TestSettleIsNotRefusedForItsOwnSpend(t *testing.T) {
	ctx := context.Background()
	l := newTestChain(t)
	l.verdict = "ACCEPTED_BY_NETWORK"
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	parent := fund(t, pool, own, 5000, 0x62)
	p := payerFor(l, pool, own, &notes{})
	p.Async = true
	tx := paymentFrom(t, p, own)
	l.know(parent)
	l.spend(parent.TxID().String(), 0, tx.TxID().String())
	if _, _, err := p.Settle(ctx, "payment", tx); err != nil {
		t.Fatalf("own spend: %v", err)
	}

	unknown := poolIn(t)
	fund(t, unknown, own, 5000, 0x63)
	p2 := payerFor(l, unknown, own, &notes{})
	p2.Async = true
	if _, _, err := p2.Settle(ctx, "payment", paymentFrom(t, p2, own)); err != nil {
		t.Fatalf("parent unknown to the node: %v", err)
	}
}
