package bwallet

import (
	"errors"
	"strings"
	"testing"
)

// TakeFitting takes the smallest spendable coin that is large enough, the
// oldest on a tie, and leaves the larger ones for what needs them.
func TestTakeFittingTakesTheSmallestCoinThatPays(t *testing.T) {
	p := newPool(t)
	big, mid, twin, small := out(1, 0, 5000, 1, false), out(2, 0, 400, 3, false), out(4, 0, 400, 2, false), out(3, 0, 100, 2, false)
	if _, err := p.Add(big, mid, twin, small); err != nil {
		t.Fatal(err)
	}
	o, err := p.TakeFitting(0, 300, nil)
	if err != nil || o.Outpoint() != twin.Outpoint() {
		t.Fatalf("took %+v %v, want the older 400 sat coin", o, err)
	}
	o, err = p.TakeFitting(0, 300, nil)
	if err != nil || o.Outpoint() != mid.Outpoint() {
		t.Fatalf("took %+v %v, want the other 400 sat coin", o, err)
	}
	o, err = p.TakeFitting(0, 300, nil)
	if err != nil || o.Outpoint() != big.Outpoint() {
		t.Fatalf("took %+v %v, want the 5000 sat coin", o, err)
	}
	_, err = p.TakeFitting(0, 300, nil)
	if !errors.Is(err, ErrNoSpendable) || !strings.Contains(err.Error(), "of at least 300 sat") {
		t.Fatalf("only a small coin left: %v", err)
	}
	if p.Count() != 1 {
		t.Fatalf("a refused take changed the pool: %d held", p.Count())
	}
	o, err = p.TakeFitting(0, 1, nil)
	if err != nil || o.Outpoint() != small.Outpoint() {
		t.Fatalf("took %+v %v, want the 100 sat coin", o, err)
	}
	// An unproven coin is taken only when allowed, and then the smallest.
	late, later := out(5, 0, 900, 9, false), out(6, 0, 700, 9, false)
	late.Unproven, later.Unproven = true, true
	if _, err := p.Add(late, later); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TakeFitting(0, 300, nil); !errors.Is(err, ErrNoSpendable) {
		t.Fatalf("an unproven coin was taken: %v", err)
	}
	o, err = p.TakeFitting(0, 300, []string{late.TxID, later.TxID})
	if err != nil || o.Outpoint() != later.Outpoint() {
		t.Fatalf("took %+v %v, want the 700 sat unproven coin", o, err)
	}
}
