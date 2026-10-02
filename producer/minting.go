package producer

import (
	"context"
	"errors"
	"sync"
	"weak"

	"github.com/lightwebinc/bcommon/bwallet"
)

// mints is every funding tree minted ahead that took a pool coin and whose
// result nothing has collected yet, by the pool the coin came from. It is
// how a Payer finds a mint to wait for: an application takes its fees
// through Payers of its own over the pool its Trees pay from, so the pool
// is the one thing the two have in common.
//
// Both sides are held weakly. A Trees dropped with its mint uncollected,
// and its pool, are reclaimed as they were before this list existed, and
// their entries go the next time a mint is listed.
var mints struct {
	sync.Mutex
	on map[weak.Pointer[bwallet.Pool]][]weak.Pointer[Trees]
}

// minting lists t as holding a coin of pool until its mint is collected.
func (t *Trees) minting(pool *bwallet.Pool) {
	mints.Lock()
	defer mints.Unlock()
	if mints.on == nil {
		mints.on = map[weak.Pointer[bwallet.Pool]][]weak.Pointer[Trees]{}
	}
	for k, list := range mints.on {
		if k.Value() == nil {
			delete(mints.on, k)
			continue
		}
		live := list[:0]
		for _, w := range list {
			if w.Value() != nil {
				live = append(live, w)
			}
		}
		if len(live) == 0 {
			delete(mints.on, k)
		} else {
			mints.on[k] = live
		}
	}
	t.mintPool = weak.Make(pool)
	mints.on[t.mintPool] = append(mints.on[t.mintPool], weak.Make(t))
}

// minted takes t off the list: its mint is collected.
func (t *Trees) minted() {
	mints.Lock()
	defer mints.Unlock()
	list := mints.on[t.mintPool]
	for i, w := range list {
		if w.Value() == t {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(mints.on, t.mintPool)
	} else {
		mints.on[t.mintPool] = list
	}
	t.mintPool = weak.Pointer[bwallet.Pool]{}
}

// mintsOn is every Trees with an uncollected mint that took a coin of pool,
// in the order the mints started.
func mintsOn(pool *bwallet.Pool) []*Trees {
	mints.Lock()
	defer mints.Unlock()
	var out []*Trees
	for _, w := range mints.on[weak.Make(pool)] {
		if t := w.Value(); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// takeAfterMints is the second half of a take the pool could not cover,
// which failed with err: while a funding tree minted ahead holds a coin of
// the pool, the mint is waited for and collected, and the take is tried
// again. The wait is bounded by ctx and the Payer's Timeout, over every
// mint together. It returns the coin, or the pool's last answer with the
// number of mints still in flight.
//
// The list is read under its lock and each mint collected outside it, on
// this goroutine, which owns the Trees: collect takes no coin and waits
// for nothing but the mint's result, so a take inside it cannot recur.
func (p *Payer) takeAfterMints(ctx context.Context, sats uint64, allow []string, err error) (bwallet.Output, int, error) {
	flights := mintsOn(p.Pool)
	if len(flights) == 0 {
		return bwallet.Output{}, 0, err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p.note("no coin is spendable and %d funding tree(s) minted ahead hold one: waiting for the change", len(flights))
	for _, t := range flights {
		if t.ahead == nil {
			continue
		}
		_ = t.collect(wctx, true)
		if t.ahead != nil {
			// The wait ended first: the mint is still in flight.
			continue
		}
		o, terr := p.Pool.TakeAtLeast(p.Tip, sats, allow)
		if !errors.Is(terr, bwallet.ErrNoSpendable) {
			return o, 0, terr
		}
		err = terr
	}
	return bwallet.Output{}, len(mintsOn(p.Pool)), err
}
