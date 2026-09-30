package producer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
)

// coinbaseIn adds a coin of sats paying s's fund key to pool the way a
// coinbase reaches it: its parent's bytes are not held, only its output.
// The coin is also on the test chain, mined, so the node serves it.
func coinbaseIn(t *testing.T, l *testChain, pool *bwallet.Pool, s *bwallet.Signer, sats uint64, salt byte) *transaction.Transaction {
	t.Helper()
	lock, err := s.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	coin := coinFor(lock, sats, salt)
	coin.MerklePath = nil
	if _, err := pool.Add(bwallet.Output{TxID: coin.TxID().String(), Vout: 0, Satoshis: sats,
		LockingScript: lock.String(), Height: 90}); err != nil {
		t.Fatal(err)
	}
	l.know(coin)
	l.mine(coin.TxID().String())
	return coin
}

// A transaction paid from a coinbase coin, kept as BEEF before it mines,
// reads back through every guarded path: the BEEF carries the coin's real
// parent with its proof, never a placeholder of no inputs.
func TestCoinbaseFeeKeptBEEFReadsBack(t *testing.T) {
	for _, async := range []bool{false, true} {
		ctx := context.Background()
		l, n := newTestChain(t), &notes{}
		pool := poolIn(t)
		own := signerOf(t, newKey(t))
		coin := coinbaseIn(t, l, pool, own, 5000, 0x51)
		p := payerFor(l, pool, own, n)
		p.Async = async

		in, err := p.Take(ctx)
		if err != nil {
			t.Fatalf("async %v: %v", async, err)
		}
		change, err := own.FundScript()
		if err != nil {
			t.Fatal(err)
		}
		tx, err := mint.Payment(ctx, &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
		if err != nil {
			t.Fatal(err)
		}
		beef, err := funding.KeepBEEF(tx, nil)
		if err != nil {
			t.Fatalf("async %v: keeping the unmined spender: %v", async, err)
		}

		back, err := funding.Rebuild(tx.Hex(), "", beef)
		if err != nil {
			t.Fatalf("async %v: the kept BEEF does not read back: %v", async, err)
		}
		if back.TxID().String() != tx.TxID().String() {
			t.Fatalf("async %v: read back %s, kept %s", async, back.TxID(), tx.TxID())
		}
		src := back.Inputs[0].SourceTransaction
		if src == nil || src.TxID().String() != coin.TxID().String() || src.MerklePath == nil || len(src.Inputs) == 0 {
			t.Fatalf("async %v: the fee input's parent is not the real, proven coin", async)
		}

		// Kept rebuilds it the same way, so its change is spendable before
		// it mines.
		k := &producer.Kept{Load: func(string) (*transaction.Transaction, error) {
			return funding.Rebuild(tx.Hex(), "", beef)
		}}
		if _, err := k.Tx(tx.TxID().String()); err != nil {
			t.Fatalf("async %v: Kept.Load: %v", async, err)
		}
	}
}

// Without a node to fetch a coinbase coin's parent from, the Payer can only
// sign against a placeholder. Keeping a spender of it as BEEF is refused at
// build time, naming the cause, rather than written and refused on reading
// back.
func TestPlaceholderParentIsNeverKeptAsBEEF(t *testing.T) {
	ctx := context.Background()
	l, n := newTestChain(t), &notes{}
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	coinbaseIn(t, l, pool, own, 5000, 0x52)
	p := payerFor(l, pool, own, n)
	p.Asset = nil

	in, err := p.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	change, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := mint.Payment(ctx, &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
	if err != nil {
		t.Fatalf("a placeholder is still enough to sign against: %v", err)
	}
	beef, err := funding.KeepBEEF(tx, nil)
	if !errors.Is(err, funding.ErrPlaceholder) || beef != "" || !strings.Contains(err.Error(), in.Tx.TxID().String()) {
		t.Fatalf("a placeholder parent was kept: %q, %v", beef, err)
	}
	if _, err := funding.BEEF(tx); !errors.Is(err, funding.ErrPlaceholder) {
		t.Fatalf("funding.BEEF wrote a placeholder: %v", err)
	}
	// Once the spender mines, its BEEF carries no ancestry, and nothing is
	// kept.
	tx.MerklePath = proofAt(tx.TxID(), 101)
	if _, err := funding.BEEF(tx); err != nil {
		t.Fatalf("a mined spender of a placeholder: %v", err)
	}
	if s, err := funding.KeepBEEF(tx, tx.MerklePath); err != nil || s != "" {
		t.Fatalf("a mined spender is kept: %q, %v", s, err)
	}
}
