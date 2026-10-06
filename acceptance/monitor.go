package acceptance

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// EventKind is what became of a payment taken on the fast path.
type EventKind int

const (
	// Confirmed is a payment mined with a proof against the headers: it is
	// money, and no longer counts against its payer.
	Confirmed EventKind = iota + 1
	// DoubleSpent is a payment whose input the node shows spent by another
	// transaction: it will never mine.
	DoubleSpent
	// Refused is a payment a broadcaster answers REJECTED.
	Refused
	// Unmined is a payment not mined within the Monitor's MaxAge. It may
	// still mine; it is reported once and no longer watched.
	Unmined
)

func (k EventKind) String() string {
	switch k {
	case Confirmed:
		return "confirmed"
	case DoubleSpent:
		return "double-spent"
	case Refused:
		return "refused"
	case Unmined:
		return "unmined"
	}
	return "unknown"
}

// Event is a payment taken on the fast path that the Monitor stopped
// watching, and why.
type Event struct {
	Kind   EventKind
	Txid   string
	Payer  string
	Sats   uint64
	Height uint32
	// Why is the evidence in words; it may hold text another party wrote.
	Why string
}

// Monitor watches the payments the fast path took until each mines or is
// lost. A lost payment (DoubleSpent, Refused, Unmined) flags its payer in
// the Verifier's Exposure, so the payer's later payments are held, and is
// reported to Hook: revoking what it bought, or telling an operator, is
// the application's. Sweep is one pass and is idempotent, so it runs on a
// timer; Run runs it on one.
type Monitor struct {
	Verifier *Verifier
	// MaxAge is how long a payment may stay unmined before it is reported
	// Unmined; zero never reports it.
	MaxAge time.Duration
	// Hook receives each event, outside the Monitor's lock.
	Hook func(Event)

	mu      sync.Mutex
	watched map[string]*watch
	now     func() time.Time
}

type watch struct {
	tx    *transaction.Transaction
	payer string
	sats  uint64
	since time.Time
}

// Watch starts watching a payment the fast path took.
func (m *Monitor) Watch(tx *transaction.Transaction, payer string, sats uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watched == nil {
		m.watched = map[string]*watch{}
	}
	id := tx.TxID().String()
	if _, ok := m.watched[id]; !ok {
		m.watched[id] = &watch{tx: tx, payer: payer, sats: sats, since: m.clock()}
	}
}

func (m *Monitor) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// Watching is the txids still watched, sorted.
func (m *Monitor) Watching() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.watched))
	for id := range m.watched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Sweep asks once after every watched payment and reports each that mined
// or was lost. It answers the events it reported.
func (m *Monitor) Sweep(ctx context.Context) []Event {
	v := m.Verifier
	m.mu.Lock()
	todo := make(map[string]*watch, len(m.watched))
	for id, w := range m.watched {
		todo[id] = w
	}
	m.mu.Unlock()
	var events []Event
	for id, w := range todo {
		if ctx.Err() != nil {
			break
		}
		ev, ok := m.check(ctx, v, id, w)
		if !ok {
			continue
		}
		m.mu.Lock()
		delete(m.watched, id)
		m.mu.Unlock()
		if v.Exposure != nil {
			if ev.Kind == Confirmed {
				v.Exposure.Release(id)
			} else {
				v.Exposure.Flag(w.payer, ev.Kind.String()+": "+id)
			}
		}
		events = append(events, ev)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Txid < events[j].Txid })
	if m.Hook != nil {
		for _, e := range events {
			m.Hook(e)
		}
	}
	return events
}

// check is one payment's evidence: an event, or none yet.
func (m *Monitor) check(ctx context.Context, v *Verifier, id string, w *watch) (Event, bool) {
	ev := Event{Txid: id, Payer: w.payer, Sats: w.sats}
	if v.Proofs != nil && v.Headers != nil {
		_, h, done, err := v.confirmed(ctx, w.tx)
		var se *nodeapi.SpentError
		switch {
		case done:
			ev.Kind, ev.Height = Confirmed, h
			return ev, true
		case errors.As(err, &se):
			ev.Kind, ev.Why = DoubleSpent, se.Error()
			return ev, true
		}
	}
	for _, s := range v.Status {
		st, err := s.Status(ctx, id)
		if err == nil && st != nil && st.TxStatus == "REJECTED" {
			ev.Kind, ev.Why = Refused, st.Why()
			return ev, true
		}
	}
	if m.MaxAge > 0 && m.clock().Sub(w.since) > m.MaxAge {
		ev.Kind, ev.Why = Unmined, "not mined within the watch's age limit"
		return ev, true
	}
	return ev, false
}

// Run sweeps every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		m.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
