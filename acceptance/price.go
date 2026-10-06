package acceptance

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Price is a coin's price in US cents, and when it was read. A zero At is a
// price with no date, which never goes stale: a static price an operator
// set and reviews.
type Price struct {
	CentsPerCoin uint64
	At           time.Time
}

// PriceSource is where a threshold set in cents gets its price. The library
// names no live price service: an application wraps the one it trusts.
// Any error, or a zero price, makes the threshold zero, so every payment is
// held.
type PriceSource interface {
	Price(ctx context.Context) (Price, error)
}

// StaticPrice is a fixed price, reviewed by its operator.
type StaticPrice uint64

// Price is the fixed price, undated.
func (s StaticPrice) Price(context.Context) (Price, error) {
	if s == 0 {
		return Price{}, ErrNoPrice
	}
	return Price{CentsPerCoin: uint64(s)}, nil
}

// ErrNoPrice is a source with no price to give.
var ErrNoPrice = errors.New("acceptance: no price")

// CachedPrice asks Source at most once per TTL and answers its last good
// price between asks. A failed ask is not cached: the last good price is
// answered, with its own date, so a Policy's MaxPriceAge still ends it,
// and with no good price yet the error is answered.
type CachedPrice struct {
	Source PriceSource
	TTL    time.Duration

	mu    sync.Mutex
	last  Price
	good  bool
	asked time.Time
	err   error
	now   func() time.Time
}

// Price is the cached price, asking Source when the cache is older than TTL.
func (c *CachedPrice) Price(ctx context.Context) (Price, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	if c.Source == nil {
		return Price{}, ErrNoPrice
	}
	if c.asked.IsZero() || now().Sub(c.asked) >= c.TTL {
		c.asked = now()
		p, err := c.Source.Price(ctx)
		switch {
		case err == nil && p.CentsPerCoin > 0:
			if p.At.IsZero() {
				p.At = c.asked
			}
			c.last, c.good, c.err = p, true, nil
		case err == nil:
			c.err = ErrNoPrice
		default:
			c.err = err
		}
	}
	if c.good {
		return c.last, nil
	}
	return Price{}, c.err
}
