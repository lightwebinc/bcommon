package acceptance

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Decision is what a receiver does with a payment.
type Decision int

const (
	// Refuse is a payment that fails a check: the receiver does not act on
	// it. It is the zero Decision, so a Verdict nobody filled in refuses.
	Refuse Decision = iota
	// Fast is a payment the receiver acts on now, on the fast path's
	// evidence, and watches until it mines.
	Fast
	// Hold is a payment the receiver acts on only once it is mined with a
	// proof against its headers.
	Hold
)

func (d Decision) String() string {
	switch d {
	case Fast:
		return "fast"
	case Hold:
		return "hold"
	}
	return "refuse"
}

// Reason is why a Verdict is what it is: a short fixed label, fit for a
// metric.
type Reason string

// The reasons.
const (
	// Fast.
	ReasonAtOrBelow Reason = "at-or-below-threshold"
	ReasonMined     Reason = "mined"

	// Hold.
	ReasonAboveThreshold   Reason = "above-threshold"
	ReasonPriceUnknown     Reason = "price-unknown"
	ReasonPriceStale       Reason = "price-stale"
	ReasonPayerLimit       Reason = "payer-limit"
	ReasonTotalLimit       Reason = "total-limit"
	ReasonFlagged          Reason = "payer-flagged"
	ReasonAskedHold        Reason = "payer-asked-hold"
	ReasonNoExposure       Reason = "no-exposure-ledger"
	ReasonNoBroadcast      Reason = "no-broadcast-leg"
	ReasonBroadcastUnknown Reason = "broadcast-unconfirmed"
	ReasonNoVerdict        Reason = "no-network-verdict"
	ReasonConflict         Reason = "double-spend-attempted"
	ReasonSpendUnknown     Reason = "spend-view-unknown"
	ReasonNoTxid           Reason = "no-txid"
	ReasonAlreadyCharged   Reason = "already-charged"

	// Refuse.
	ReasonNothing        Reason = "pays-nothing"
	ReasonMalformed      Reason = "malformed"
	ReasonUnderpaid      Reason = "underpaid"
	ReasonWrongScript    Reason = "wrong-script"
	ReasonNotFinal       Reason = "not-final"
	ReasonOverspends     Reason = "outputs-exceed-inputs"
	ReasonSPV            Reason = "spv-failed"
	ReasonNetworkRefused Reason = "network-refused"
	ReasonDoubleSpent    Reason = "double-spent"
)

// Verdict is a decision on one payment and why.
type Verdict struct {
	Decision Decision
	Reason   Reason
	// Detail is the reason in words, for a log; it may hold text another
	// party wrote (a broadcaster's answer), so filter it before a terminal.
	Detail string
	// Sats is what the payment pays the receiver, and Threshold the
	// threshold it was held to (zero when it was never reached).
	Sats      uint64
	Threshold uint64
}

func (v Verdict) String() string {
	s := fmt.Sprintf("%s (%s): %d sat, threshold %d sat", v.Decision, v.Reason, v.Sats, v.Threshold)
	if v.Detail != "" {
		s += ": " + v.Detail
	}
	return s
}

// Defaults DefaultPolicy uses. DefaultThresholdSats is 25 US dollars at a
// price of 100 US dollars a coin, set high on purpose: if the price is
// lower, the threshold is worth less than 25 dollars, which is the safe
// side. Review it against the price on a schedule.
const (
	DefaultThresholdSats  uint64 = 25_000_000
	DefaultThresholdCents uint64 = 2_500
	DefaultWindow                = time.Hour
	DefaultTotalFactor    uint64 = 10
	DefaultWait                  = 10 * time.Second
	DefaultPoll                  = 500 * time.Millisecond
	DefaultMaxPriceAge           = time.Hour
	// SatsPerCoin is satoshis in one coin.
	SatsPerCoin uint64 = 100_000_000
)

// Policy is a receiver's rule for which payments are fast. The zero Policy
// holds every payment.
type Policy struct {
	// ThresholdSats is the largest payment, in satoshis, the fast path
	// takes. With ThresholdCents set too, the smaller of the two applies.
	ThresholdSats uint64
	// ThresholdCents is the threshold in US cents, converted through
	// Price. A price that is unknown, zero or older than MaxPriceAge makes
	// the threshold zero: every payment is held.
	ThresholdCents uint64
	Price          PriceSource
	MaxPriceAge    time.Duration
	// Window is how long a fast payment counts against its payer and the
	// total once taken, unless it mines first; zero counts it until it
	// mines or is released.
	Window time.Duration
	// PayerLimit bounds the satoshis one payer has on the fast path and
	// not yet mined within Window; TotalLimit bounds them across every
	// payer. A payment that would pass either is held. Zero allows
	// nothing.
	PayerLimit uint64
	TotalLimit uint64
	// Agree is how many status sources must answer a fast payment
	// accepted; zero is one.
	Agree int
	// Wait bounds how long the fast path waits for that acceptance; zero
	// is DefaultWait. Watch is how long it keeps watching for a conflict
	// before it answers fast, zero for none; a Watch longer than Wait
	// extends Wait. Poll paces both; zero is DefaultPoll.
	Wait  time.Duration
	Watch time.Duration
	Poll  time.Duration
}

// DefaultPolicy is the recommended policy over a static threshold: 25
// dollars at 100 dollars a coin, each payer bounded to one threshold and
// all payers to ten, within an hour, no watch window.
func DefaultPolicy() Policy {
	return Policy{
		ThresholdSats: DefaultThresholdSats,
		Window:        DefaultWindow,
		PayerLimit:    DefaultThresholdSats,
		TotalLimit:    DefaultTotalFactor * DefaultThresholdSats,
		MaxPriceAge:   DefaultMaxPriceAge,
	}
}

// Threshold is the threshold in satoshis now, with the reason it is zero
// when a price was needed and not known.
func (p Policy) Threshold(ctx context.Context) (uint64, Reason) {
	if p.ThresholdCents == 0 {
		return p.ThresholdSats, ""
	}
	if p.Price == nil {
		return 0, ReasonPriceUnknown
	}
	pr, err := p.Price.Price(ctx)
	if err != nil || pr.CentsPerCoin == 0 {
		return 0, ReasonPriceUnknown
	}
	if p.MaxPriceAge > 0 && !pr.At.IsZero() && time.Since(pr.At) > p.MaxPriceAge {
		return 0, ReasonPriceStale
	}
	sats := convert(p.ThresholdCents, pr.CentsPerCoin)
	if p.ThresholdSats > 0 && p.ThresholdSats < sats {
		sats = p.ThresholdSats
	}
	return sats, ""
}

// convert is cents in satoshis at a price, rounded down; a product too
// large to hold saturates, which a caller bounds with ThresholdSats.
func convert(cents, centsPerCoin uint64) uint64 {
	if cents > ^uint64(0)/SatsPerCoin {
		return ^uint64(0) / centsPerCoin
	}
	return cents * SatsPerCoin / centsPerCoin
}

// Ask is what a payer asks of the receiver. A payer may ask to be held; it
// may ask to be fast, which changes nothing: the receiver decides.
type Ask int

const (
	AskNone Ask = iota
	AskFast
	AskHold
)

// Request is a payment as Decide weighs it: who pays, how much, and its
// txid, by which a fast payment is charged to its payer.
type Request struct {
	Payer string
	Txid  string
	Sats  uint64
	Ask   Ask
}

// Decide weighs a payment's value against the policy and the exposure
// already taken. On Fast it charges the payment to x at once, so two
// payments decided together cannot both pass a limit; a caller that then
// does not act on it calls x.Release. It looks at nothing but value: the
// checks are the Verifier's.
func (p Policy) Decide(ctx context.Context, x *Exposure, r Request) Verdict {
	v := Verdict{Sats: r.Sats}
	if r.Sats == 0 {
		v.Decision, v.Reason = Refuse, ReasonNothing
		return v
	}
	threshold, why := p.Threshold(ctx)
	v.Threshold = threshold
	hold := func(r Reason, detail string) Verdict {
		v.Decision, v.Reason, v.Detail = Hold, r, detail
		return v
	}
	switch {
	case why != "":
		return hold(why, "the threshold needs a price and has none, so every payment is held")
	case r.Ask == AskHold:
		return hold(ReasonAskedHold, "")
	case r.Sats > threshold:
		return hold(ReasonAboveThreshold, "")
	case x == nil:
		return hold(ReasonNoExposure, "nothing counts what the fast path has taken")
	case r.Txid == "":
		return hold(ReasonNoTxid, "")
	}
	if reason, detail := x.reserve(p, r); reason != "" {
		return hold(reason, detail)
	}
	v.Decision, v.Reason = Fast, ReasonAtOrBelow
	return v
}

// Exposure counts what the fast path has taken and not seen mined, per
// payer and in total, and the payers whose fast payment was double spent.
// It is safe for concurrent use. It is memory only: an application that
// restarts restores what it still watches with Charge and Flag.
type Exposure struct {
	mu      sync.Mutex
	charges map[string]charge
	flagged map[string]string
	now     func() time.Time
}

type charge struct {
	payer string
	sats  uint64
	at    time.Time
}

// NewExposure is an empty Exposure.
func NewExposure() *Exposure {
	return &Exposure{charges: map[string]charge{}, flagged: map[string]string{}, now: time.Now}
}

func (x *Exposure) init() {
	if x.charges == nil {
		x.charges = map[string]charge{}
	}
	if x.flagged == nil {
		x.flagged = map[string]string{}
	}
	if x.now == nil {
		x.now = time.Now
	}
}

// reserve charges r if the limits allow it, or says which does not.
func (x *Exposure) reserve(p Policy, r Request) (Reason, string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
	now := x.now()
	if why, ok := x.flagged[r.Payer]; ok {
		return ReasonFlagged, why
	}
	if _, ok := x.charges[r.Txid]; ok {
		return ReasonAlreadyCharged, "this payment is already on the fast path"
	}
	var payer, total uint64
	for txid, c := range x.charges {
		if p.Window > 0 && now.Sub(c.at) > p.Window {
			delete(x.charges, txid)
			continue
		}
		total += c.sats
		if c.payer == r.Payer {
			payer += c.sats
		}
	}
	if payer+r.Sats > p.PayerLimit || payer+r.Sats < payer {
		return ReasonPayerLimit, fmt.Sprintf("the payer has %d sat unmined on the fast path, limit %d", payer, p.PayerLimit)
	}
	if total+r.Sats > p.TotalLimit || total+r.Sats < total {
		return ReasonTotalLimit, fmt.Sprintf("%d sat unmined on the fast path in all, limit %d", total, p.TotalLimit)
	}
	x.charges[r.Txid] = charge{payer: r.Payer, sats: r.Sats, at: now}
	return "", ""
}

// Charge counts a fast payment taken earlier, at the time it was taken:
// what an application restores after a restart.
func (x *Exposure) Charge(payer, txid string, sats uint64, at time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
	x.charges[txid] = charge{payer: payer, sats: sats, at: at}
}

// Release stops counting a payment: one decided fast and then not acted
// on, or one that has mined.
func (x *Exposure) Release(txid string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.charges, txid)
}

// Flag marks a payer whose fast payment was double spent or refused: every
// later payment of theirs is held. why is kept for the verdict.
func (x *Exposure) Flag(payer, why string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
	x.flagged[payer] = why
}

// Unflag clears a payer's flag: an operator's decision.
func (x *Exposure) Unflag(payer string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.flagged, payer)
}

// Flagged reports whether a payer is flagged, and why.
func (x *Exposure) Flagged(payer string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	why, ok := x.flagged[payer]
	return why, ok
}

// Unmined is what the fast path has taken and not seen mined: for one
// payer, and in total. Charges older than window are not counted; zero
// counts them all.
func (x *Exposure) Unmined(payer string, window time.Duration) (forPayer, total uint64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
	now := x.now()
	for _, c := range x.charges {
		if window > 0 && now.Sub(c.at) > window {
			continue
		}
		total += c.sats
		if c.payer == payer {
			forPayer += c.sats
		}
	}
	return forPayer, total
}
