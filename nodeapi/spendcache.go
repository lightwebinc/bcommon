package nodeapi

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultSpendTTL is how long a SpendCache answers from one read when TTL is
// not set.
const DefaultSpendTTL = 2 * time.Second

// maxSpendReads bounds the transactions a SpendCache remembers.
const maxSpendReads = 4096

// SpendCache is a SpendSource that answers for the outputs of a transaction
// from one read of it, for TTL. A node's UTXO view of a transaction names all
// its outputs and costs the node a lookup of each: asked once per output of a
// transaction of a thousand outputs, it costs the node a million lookups to
// answer a thousand questions that one read answers. Callers asking at the
// same moment share the read in flight.
//
// An answer may be TTL old. An output shown unspent may have been spent since,
// as it may be the moment after any read; a caller that must not act on that
// keeps asking the source itself, or sets a TTL it can live with. A source
// that cannot answer for a whole transaction is asked as it would be without
// the cache. A failed read is not remembered.
type SpendCache struct {
	Source SpendSource
	TTL    time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu    sync.Mutex
	reads map[string]*spendRead
}

// spendRead is one read of a transaction's outputs: done closes when answers
// and err are set.
type spendRead struct {
	done    chan struct{}
	at      time.Time
	answers map[uint32]spendAnswer
	err     error
}

// wholeTxSource answers for every output of a transaction at once.
type wholeTxSource interface {
	spendAnswers(ctx context.Context, txid string) (map[uint32]spendAnswer, error)
}

func (c *SpendCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SpendCache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultSpendTTL
}

// Spender is the source's answer for output vout of txid, as Asset.Spender
// gives it: "" only for an output shown unspent, and an error wrapping
// ErrSpendUnknown for anything that is not evidence.
func (c *SpendCache) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	whole, ok := c.Source.(wholeTxSource)
	if !ok {
		return c.Source.Spender(ctx, txid, vout)
	}
	r, first := c.read(txid)
	if first {
		r.answers, r.err = whole.spendAnswers(ctx, txid)
		close(r.done)
		if r.err != nil {
			c.forget(txid, r)
		}
	} else {
		select {
		case <-r.done:
		case <-ctx.Done():
			return "", fmt.Errorf("%w: output %d of %s: %w", ErrSpendUnknown, vout, txid, ctx.Err())
		}
	}
	if r.err != nil {
		return "", fmt.Errorf("%w: output %d of %s: %w", ErrSpendUnknown, vout, txid, r.err)
	}
	by, err := answerFor(r.answers, vout)
	if err != nil {
		return "", fmt.Errorf("%w: output %d of %s: %w", ErrSpendUnknown, vout, txid, err)
	}
	return by, nil
}

// read is the read that answers for txid now, and whether the caller is the
// one to make it.
func (c *SpendCache) read(txid string) (*spendRead, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if r := c.reads[txid]; r != nil && now.Sub(r.at) < c.ttl() {
		return r, false
	}
	if c.reads == nil {
		c.reads = map[string]*spendRead{}
	}
	if len(c.reads) >= maxSpendReads {
		for id, r := range c.reads {
			if now.Sub(r.at) >= c.ttl() {
				delete(c.reads, id)
			}
		}
		if len(c.reads) >= maxSpendReads {
			clear(c.reads)
		}
	}
	r := &spendRead{done: make(chan struct{}), at: now}
	c.reads[txid] = r
	return r, true
}

// forget drops a failed read, unless a later one has taken its place.
func (c *SpendCache) forget(txid string, r *spendRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads[txid] == r {
		delete(c.reads, txid)
	}
}
