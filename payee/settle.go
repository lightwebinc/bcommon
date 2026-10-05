package payee

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/termsafe"
)

// Settling.
const (
	// DefaultInFlight is how many payments a run has broadcast and not yet
	// mined at once, unless told otherwise.
	DefaultInFlight = 16
	// MaxInFlight is the most it may.
	MaxInFlight = 64
)

// Payer is the payee leg of the purse (*purse.Purse): Check, Broadcast,
// Await and Take, the halves of InternalizeAction, run apart so that every
// payment is broadcast before any is waited for. Broadcast and Await answer
// a *purse.RefusedError for a payment the network will never mine: the
// leg refused it for good, or the node shows an input spent by another
// transaction (package chainview).
type Payer interface {
	Check(ctx context.Context, args wallet.InternalizeActionArgs) (*purse.Incoming, error)
	Broadcast(ctx context.Context, in *purse.Incoming) error
	Await(ctx context.Context, in *purse.Incoming) error
	Take(in *purse.Incoming) error
}

// Pool is the payee's coin pool as the report reads it (*bwallet.Pool).
type Pool interface {
	Count() int
	Balance() uint64
}

// Settler settles a host's payments into a payee's pool.
type Settler struct {
	// App is the application's name: a payment is internalized with the
	// description "<App> priced question <class>" and the labels App and
	// "payee".
	App string
	// Payer is the payee's purse, and Record its record of what is
	// settled. Pool, when set, is reported.
	Payer  Payer
	Record Record
	Pool   Pool
	// InFlight bounds the payments broadcast and not yet mined at once,
	// 1 to MaxInFlight; zero is DefaultInFlight.
	InFlight int
	// Words puts a refusal of the purse in the application's words (which
	// setting names the node or the leg); nil is Words.
	Words func(error) error
	// Out takes a line for each payment settled and the report; Warn a
	// line for each payment not settled or refused. Either may be nil.
	Out, Warn io.Writer
}

// Report counts a run.
type Report struct {
	// Settled payments this run, and the satoshis they paid.
	Settled int
	Sats    uint64
	// Before were settled by an earlier run.
	Before int
	// NotSettled failed a check, or did not mine in time: a later run
	// tries them again, and until one settles, its payer can spend the
	// coins elsewhere.
	NotSettled int
	// Refused were refused by the network this run, and RefusedBefore by
	// an earlier one: they will never settle.
	Refused       int
	RefusedBefore int
	// PoolOutputs and PoolSats are the pool after the run.
	PoolOutputs int
	PoolSats    uint64
}

// String is the run's report line.
func (r Report) String() string {
	return fmt.Sprintf("%d payment(s) settled, %d sat; %d settled before; %d not settled; %d refused (%d before); pool %d output(s), %d sat",
		r.Settled, r.Sats, r.Before, r.NotSettled, r.Refused, r.RefusedBefore, r.PoolOutputs, r.PoolSats)
}

// Problem is what a run that did not settle everything says, or "" for one
// that did: a payment refused this run first, then one not settled. An
// application ends such a run with its refusal status.
func (r Report) Problem() string {
	switch {
	case r.Refused > 0:
		return fmt.Sprintf("%d payment(s) refused by the network: their payers spent the coins elsewhere, and they will never settle", r.Refused)
	case r.NotSettled > 0:
		return fmt.Sprintf("%d payment(s) not settled: until one is, its payer can spend the coins elsewhere", r.NotSettled)
	}
	return ""
}

// Words puts the purse's refusals that settle can meet in a command's
// words: the node and the settlement leg by their config keys (asset,
// settle), and a payment not mined yet as one the next run takes.
func Words(err error) error {
	switch {
	case errors.Is(err, purse.ErrNoNode):
		return fmt.Errorf("%w (config key asset)", err)
	case errors.Is(err, purse.ErrNoSettler):
		return fmt.Errorf("%w (config key settle)", err)
	case errors.Is(err, purse.ErrNotMined):
		return fmt.Errorf("%w; run the command again to take it into the pool once it is", err)
	}
	return err
}

// Field is someone else's text that fills one field of a line (a class
// name, a network's reason): held to that line, every line break a space,
// and filtered for the terminal. A line break in it would otherwise write
// lines of its own in a report, in the command's voice.
func Field(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f', 0x85, 0x2028, 0x2029:
			return ' '
		}
		return r
	}, s)
	return termsafe.Text(s)
}

// ErrNewerLedger is a ledger line written by a later version than this
// package reads: it is not settled, and stays in the ledger for a reader
// that knows it.
var ErrNewerLedger = errors.New("payee: the line is of a later ledger version than this reader")

// Settle takes into the pool every payment in ps the record holds neither
// as settled nor as refused, and reports the run. It returns an error only
// when the run cannot go on (a record that does not persist, a context
// ended, a Settler not set up); a payment that is not settled is counted,
// named on Warn, and left for the next run.
func (s *Settler) Settle(ctx context.Context, ps []Payment) (Report, error) {
	var rep Report
	inFlight := s.InFlight
	if inFlight == 0 {
		inFlight = DefaultInFlight
	}
	switch {
	case s.App == "":
		return rep, errors.New("payee: Settler.App is empty")
	case s.Payer == nil || s.Record == nil:
		return rep, errors.New("payee: Settler needs a Payer and a Record")
	case inFlight < 1 || inFlight > MaxInFlight:
		return rep, fmt.Errorf("payee: in-flight %d is outside 1 to %d", inFlight, MaxInFlight)
	}
	words := s.Words
	if words == nil {
		words = Words
	}
	notSettled := func(p Payment, err error) {
		rep.NotSettled++
		say(s.Warn, fmt.Sprintf("payment %s (%d sat, %s): NOT SETTLED: %s", p.Txid, p.Satoshis, Field(p.Class), Field(err.Error())))
	}
	// Every payment is checked first, then broadcast, and the proofs are
	// awaited together: at most inFlight are broadcast and not yet mined
	// at once, so a run takes about one block, not one block a payment.
	type job struct {
		p  Payment
		in *purse.Incoming
	}
	var jobs []job
	for _, p := range ps {
		if s.Record.IsSettled(p.Txid) {
			rep.Before++
			continue
		}
		if s.Record.IsUnsettleable(p.Txid) {
			rep.RefusedBefore++
			continue
		}
		in, err := s.check(ctx, p)
		if err != nil {
			notSettled(p, err)
			continue
		}
		jobs = append(jobs, job{p, in})
	}
	type result struct {
		job
		err error
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result)
	sem := make(chan struct{}, inFlight)
	go func() {
		var wg sync.WaitGroup
		for _, j := range jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				err := s.Payer.Broadcast(rctx, j.in)
				if err == nil {
					err = s.Payer.Await(rctx, j.in)
				}
				results <- result{j, err}
			}()
		}
		wg.Wait()
		close(results)
	}()
	// stop ends the run early: the payments still out are let go, and
	// their results drained so that nothing is left waiting to send one.
	stop := func(err error) (Report, error) {
		cancel()
		go func() {
			for range results {
			}
		}()
		return rep, err
	}
	for r := range results {
		err := r.err
		if err == nil {
			err = s.Payer.Take(r.in)
		}
		var re *purse.RefusedError
		if errors.As(err, &re) {
			// It will never mine: reported once, recorded, and passed over
			// by every later run.
			rep.Refused++
			say(s.Warn, fmt.Sprintf("payment %s (%d sat, %s): REFUSED, NEVER SETTLES: %s; the payer took the coins back after the question was answered",
				r.p.Txid, r.p.Satoshis, Field(r.p.Class), Field(re.Why)))
			if err := s.Record.RecordUnsettleable(r.p.Txid, re.Why); err != nil {
				return stop(err)
			}
			continue
		}
		if err != nil {
			notSettled(r.p, words(err))
			continue
		}
		if err := s.Record.RecordSettled(r.p.Txid); err != nil {
			return stop(err)
		}
		rep.Settled++
		rep.Sats += r.p.Satoshis
		if s.Out != nil {
			fmt.Fprintf(s.Out, "settled %s: %d sat for %s from %s\n", r.p.Txid, r.p.Satoshis, Field(r.p.Class), termsafe.Abbrev(r.p.SenderIdentityKey))
		}
	}
	if s.Pool != nil {
		rep.PoolOutputs, rep.PoolSats = s.Pool.Count(), s.Pool.Balance()
	}
	if s.Out != nil {
		fmt.Fprintln(s.Out, rep.String())
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	return rep, nil
}

// check is a payment's checks: a line this package reads, BEEF that holds
// the txid the line names, paying the key the payee derives for its
// remittance and payer, and verifying against the headers.
func (s *Settler) check(ctx context.Context, p Payment) (*purse.Incoming, error) {
	if v := p.Version(); v > LedgerVersion {
		return nil, fmt.Errorf("%w: version %d, this reader reads up to %d", ErrNewerLedger, v, LedgerVersion)
	}
	beef, err := base64.StdEncoding.DecodeString(p.Beef)
	if err != nil {
		return nil, fmt.Errorf("the ledger's BEEF is not base64: %w", err)
	}
	r, err := purse.Remittance(p.DerivationPrefix, p.DerivationSuffix, p.SenderIdentityKey)
	if err != nil {
		return nil, err
	}
	in, err := s.Payer.Check(ctx, wallet.InternalizeActionArgs{Tx: beef, Description: s.App + " priced question " + p.Class,
		Labels: []string{s.App, "payee"}, Outputs: []wallet.InternalizeOutput{{OutputIndex: p.OutputIndex,
			Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: r}}})
	if err != nil {
		return nil, err
	}
	if in.Txid != p.Txid {
		return nil, fmt.Errorf("the ledger names %s and its BEEF holds %s", p.Txid, in.Txid)
	}
	return in, nil
}
