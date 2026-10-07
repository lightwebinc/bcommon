package mint_test

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/lightwebinc/bcommon/mint"
)

// bigFee is ceil(size*sats/bytes) in big.Int, the reference Rate.Fee is
// held to.
func bigFee(size, sats, bytes uint64) *big.Int {
	n := new(big.Int).Mul(new(big.Int).SetUint64(size), new(big.Int).SetUint64(sats))
	d := new(big.Int).SetUint64(bytes)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func TestRateFeeMatchesBigInt(t *testing.T) {
	sizes := []uint64{0, 1, 225, 999, 1000, 1001, 10 << 20}
	rates := []mint.Rate{{Sats: 1, Bytes: 1}, {Sats: 100, Bytes: 1000}, {Sats: 1, Bytes: 1000000}, {Sats: 7, Bytes: 3}}
	for _, r := range rates {
		for _, size := range sizes {
			got, err := r.Fee(size)
			if err != nil {
				t.Fatalf("%s at %d: %v", r, size, err)
			}
			if want := bigFee(size, r.Sats, r.Bytes); want.Cmp(new(big.Int).SetUint64(got)) != 0 {
				t.Fatalf("%s at %d bytes: %d, want %s", r, size, got, want)
			}
		}
	}
	// 225 bytes at the network rate is 23 satoshis: 22.5 rounded up.
	if fee, _ := (mint.Rate{Sats: 100, Bytes: 1000}).Fee(225); fee != 23 {
		t.Fatalf("225 bytes at 100/1000: %d, want 23", fee)
	}
}

func TestRateFeeRefusesOverflowAndZeroBytes(t *testing.T) {
	if _, err := (mint.Rate{Sats: math.MaxUint64, Bytes: 1}).Fee(2); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("overflow: %v", err)
	}
	// The exact maximum fits; one rounding step past it does not.
	if fee, err := (mint.Rate{Sats: math.MaxUint64, Bytes: 1}).Fee(1); err != nil || fee != math.MaxUint64 {
		t.Fatalf("max: %d %v", fee, err)
	}
	if _, err := (mint.Rate{Sats: math.MaxUint64, Bytes: 2}).Fee(3); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("overflow past the quotient: %v", err)
	}
	if _, err := (mint.Rate{Sats: 1}).Fee(10); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("zero bytes: %v", err)
	}
}

func TestRateCmpClampParse(t *testing.T) {
	a, b := mint.Rate{Sats: 100, Bytes: 1000}, mint.Rate{Sats: 1, Bytes: 10}
	if a.Cmp(b) != 0 || a.Cmp(mint.Rate{Sats: 1, Bytes: 1}) != -1 || (mint.Rate{Sats: 1, Bytes: 1}).Cmp(a) != 1 {
		t.Fatal("Cmp compares by value")
	}
	huge := mint.Rate{Sats: math.MaxUint64, Bytes: 1}
	if huge.Cmp(mint.Rate{Sats: math.MaxUint64 - 1, Bytes: 1}) != 1 {
		t.Fatal("Cmp near the top")
	}
	if (mint.Rate{Sats: 5}).Cmp(huge) != 1 {
		t.Fatal("a rate over zero bytes is the highest")
	}
	lo, hi := mint.Rate{Sats: 100, Bytes: 1000}, mint.Rate{Sats: 1, Bytes: 1}
	if got := (mint.Rate{Sats: 1, Bytes: 1000000}).Clamp(lo, hi); got != lo {
		t.Fatalf("below: %s", got)
	}
	if got := (mint.Rate{Sats: 5, Bytes: 1}).Clamp(lo, hi); got != hi {
		t.Fatalf("above: %s", got)
	}
	if got := (mint.Rate{Sats: 5, Bytes: 1}).Clamp(mint.Rate{}, mint.Rate{}); got != (mint.Rate{Sats: 5, Bytes: 1}) {
		t.Fatalf("no bounds: %s", got)
	}
	for in, want := range map[string]mint.Rate{"100/1000": {Sats: 100, Bytes: 1000}, " 1 ": {Sats: 1, Bytes: 1}, "1/1000000": {Sats: 1, Bytes: 1000000}} {
		if got, err := mint.ParseRate(in); err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "1/0", "1/", "-1/2", "1/2/3"} {
		if _, err := mint.ParseRate(bad); !errors.Is(err, mint.ErrRate) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if s := lo.String(); s != "100/1000" {
		t.Fatal(s)
	}
}

func TestFeesFor(t *testing.T) {
	cases := []struct {
		name string
		fees mint.Fees
		size int
		want uint64
	}{
		{"legacy under the floor", mint.LegacyFees, 225, 250},
		{"legacy above the floor", mint.LegacyFees, 1000, 1000},
		{"default under the floor", mint.DefaultFees, 1000, 250},
		{"default above the floor", mint.DefaultFees, 10000, 1000},
		{"network, no padding", mint.NetworkFees, 225, 23},
		{"network, empty", mint.NetworkFees, 0, 1},
		{"rate wins over SatPerByte", mint.Fees{SatPerByte: 9, Rate: mint.Rate{Sats: 1, Bytes: 1}}, 10, 10},
		{"max rate lowers the rate", mint.Fees{Rate: mint.Rate{Sats: 10, Bytes: 1}, MaxRate: mint.Rate{Sats: 1, Bytes: 1}}, 300, 300},
		{"max rate leaves a lower rate", mint.Fees{Rate: mint.Rate{Sats: 100, Bytes: 1000}, MaxRate: mint.Rate{Sats: 1, Bytes: 1}}, 300, 30},
		{"max rate caps SatPerByte too", mint.Fees{SatPerByte: 5, MaxRate: mint.Rate{Sats: 1, Bytes: 1}}, 300, 300},
		{"at the max", mint.Fees{SatPerByte: 1, Max: 300}, 300, 300},
	}
	for _, c := range cases {
		got, err := c.fees.For(c.size)
		if err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, got, err, c.want)
		}
	}
	if _, err := (mint.Fees{SatPerByte: 1, Max: 299}).For(300); !errors.Is(err, mint.ErrFeeTooHigh) {
		t.Fatalf("above the max: %v", err)
	}
	if _, err := (mint.Fees{Rate: mint.Rate{Sats: 1}}).For(1); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("zero bytes: %v", err)
	}
	if _, err := (mint.Fees{SatPerByte: 1, MaxRate: mint.Rate{Sats: 1}}).For(1); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("max over zero bytes: %v", err)
	}
	if _, err := mint.LegacyFees.For(-1); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("negative size: %v", err)
	}
}

// At a rate under a satoshi a byte the loop converges as it does at one:
// every transaction pays at least what its measured size costs, never more
// than the margin allows (two bytes per input, plus the two a signature may
// have shrunk by since the pass that set the fee), and verifies.
func TestFeeConvergesAtFractionalRates(t *testing.T) {
	holder, payer := parties(t)
	for _, r := range []mint.Rate{{Sats: 100, Bytes: 1000}, {Sats: 1, Bytes: 1000000}, {Sats: 7, Bytes: 3}, {Sats: 1, Bytes: 1}} {
		fees := mint.Fees{Rate: r, Floor: 1, Dust: 1}
		created, err := mint.Transition(holder.lock, 1, nil, payer.coin(50000), payer.lock, fees)
		if err != nil {
			t.Fatal(err)
		}
		prev := &mint.Input{Tx: created, Vout: 0, Unlocker: holder.unlock}
		for i := 0; i < 30; i++ {
			tx, err := mint.Transition(holder.lock, 1, prev, payer.coin(3000+uint64(i)*137), payer.lock, fees)
			if err != nil {
				t.Fatalf("%s amount %d: %v", r, i, err)
			}
			need, _ := fees.For(tx.Size())
			most, _ := fees.For(tx.Size() + 4*len(tx.Inputs))
			if got := paid(tx); got < need || got > most {
				t.Fatalf("%s amount %d paid %d for %d bytes, want %d to %d", r, i, got, tx.Size(), need, most)
			}
			verifies(t, tx)
		}
		tree, err := mint.FundingTree(holder.lock, mint.MaxFundingOutputs, 1, payer.coin(100000), payer.lock, fees)
		if err != nil {
			t.Fatalf("%s: full tree: %v", r, err)
		}
		if need, _ := fees.For(tree.Size()); paid(tree) < need {
			t.Fatalf("%s: full tree paid %d for %d bytes", r, paid(tree), tree.Size())
		}
	}
}

// Dust is its own threshold: with a floor of one satoshi, change of one
// satoshi is kept, and with Dust above the floor, change under Dust is
// paid to the fee.
func TestDustIsSeparateFromTheFloor(t *testing.T) {
	ctx := context.Background()
	holder, payer := parties(t)
	probe, err := mint.Payment(ctx, holder.lock, 1000, payer.coin(5000), payer.lock, mint.NetworkFees)
	if err != nil {
		t.Fatal(err)
	}
	fee := paid(probe)
	if len(probe.Outputs) != 2 || fee >= 250 {
		t.Fatalf("network fees: %d outputs, fee %d", len(probe.Outputs), fee)
	}
	// Leave exactly one satoshi of change.
	one, err := mint.Payment(ctx, holder.lock, 5000-fee-1, payer.coin(5000), payer.lock, mint.NetworkFees)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Outputs) != 2 || one.Outputs[1].Satoshis != 1 {
		t.Fatalf("one satoshi of change is kept at dust 1: %d outputs", len(one.Outputs))
	}
	verifies(t, one)
	high := mint.NetworkFees
	high.Dust = 100
	dropped, err := mint.Payment(ctx, holder.lock, 5000-fee-99, payer.coin(5000), payer.lock, high)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped.Outputs) != 1 || paid(dropped) != fee+99 {
		t.Fatalf("change under dust is paid to the fee: %d outputs, paid %d", len(dropped.Outputs), paid(dropped))
	}
}

func TestMaxRefusesTheBuild(t *testing.T) {
	holder, payer := parties(t)
	fees := mint.Fees{SatPerByte: 1, Floor: 1, Max: 100}
	if _, err := mint.FundingTree(holder.lock, 8, 1, payer.coin(5000), payer.lock, fees); !errors.Is(err, mint.ErrFeeTooHigh) {
		t.Fatalf("a fee above Max must be refused: %v", err)
	}
	if _, err := mint.FundingTree(holder.lock, 8, 1, payer.coin(5000), payer.lock, mint.Fees{Floor: 300, Max: 200}); !errors.Is(err, mint.ErrFeeTooHigh) {
		t.Fatalf("a floor above Max is refused before signing: %v", err)
	}
}
