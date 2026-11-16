package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

type countingTracker struct {
	calls int
	err   error
}

func (c *countingTracker) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	c.calls++
	return c.err == nil, c.err
}

func (c *countingTracker) CurrentHeight(context.Context) (uint32, error) { return 0, nil }

// A root is asked about once in a check, at each height it is claimed for,
// and an error is asked again rather than remembered.
func TestRootIsAskedOnce(t *testing.T) {
	ctx := context.Background()
	under := &countingTracker{}
	a := &askedOnce{t: under, roots: map[rootAt]bool{}}
	r1, r2 := chainhash.Hash{1}, chainhash.Hash{2}
	for range 3 {
		if ok, err := a.IsValidRootForHeight(ctx, &r1, 100); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if under.calls != 1 {
		t.Fatalf("%d calls for one root at one height", under.calls)
	}
	_, _ = a.IsValidRootForHeight(ctx, &r1, 101)
	_, _ = a.IsValidRootForHeight(ctx, &r2, 100)
	if under.calls != 3 {
		t.Fatalf("%d calls for three distinct questions", under.calls)
	}
	under.err = errors.New("down")
	r3 := chainhash.Hash{3}
	for range 2 {
		if _, err := a.IsValidRootForHeight(ctx, &r3, 100); err == nil {
			t.Fatal("an error was swallowed")
		}
	}
	if under.calls != 5 {
		t.Fatalf("%d calls: an error must not be remembered", under.calls)
	}
}
