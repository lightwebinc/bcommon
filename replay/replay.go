// Package replay is the walk behind a module.Replayer: how a chain
// application's Go module asks a peer, with the application's own lookup
// questions, for what its host lacks, and answers the objects to submit in a
// valid order, bounded per call.
//
// A walk is a list of phases run in order, each a list of items in
// ascending key order (Phase). The items are computed afresh at every call
// from what the host holds, since the host admits what one call answered
// before it makes the next; a phase that answered anything ends the call, so
// the next phase reads an index that holds it. The cursor (Cursor) carries
// the phase, the last item finished, a page inside the next item, and what
// the walk has answered, so that nothing is answered twice in one walk and
// a walk always ends.
//
// Two kinds of work fill the phases. Tokens repairs one chain of tokens:
// every seq up to the peer's latest at which the host holds no token,
// ascending, each token in the BEEF of the token and its parent (TokenBEEF),
// the parent read from the host, from the peer's answer, or for a chain's
// first token from a chain source, never a token before its parent. Each
// answers what one question finds as the peer gave it: the carriers,
// sweeps and other records a chain's readers need besides the tokens.
//
// This package decides no admission and logs nothing: it answers objects
// and counts what it could not answer, and the host admits each through
// its ordinary path.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"
)

// ErrBudget is a call's question or object bound reached: the phase stops
// where it is and the walk goes on at the next call.
var ErrBudget = errors.New("replay: the call's bound is reached")

// MaxNotes bounds the notes one answer carries.
const MaxNotes = 16

// Cursor is a walk's position, carried from one call to the next in
// module.ReplayAnswer.Cursor. A host treats it as opaque.
type Cursor struct {
	// Phase is the index of the phase the walk is in.
	Phase int
	// Item is the key of the last item the phase finished, "" for none.
	Item string
	// Page is where the next item resumes, its own page cursor.
	Page string
	// Sent are the txids the walk has answered, so that nothing is
	// answered twice in one walk: an object the host did not admit waits
	// for the next walk.
	Sent map[chainhash.Hash]bool
	// Found are keys a phase found for a later one (Find): what the peer
	// lists that the host holds nothing of yet, say.
	Found map[string]bool
}

// Output is one output a peer answered: the BEEF it gave, the subject
// transaction parsed from it (with its proof when mined), the output's
// index and its context.
type Output struct {
	Txid    chainhash.Hash
	Tx      *transaction.Transaction
	Vout    uint32
	BEEF    []byte
	Context []byte
}

// Walk is one call of a replayer: the request and its bounds, the cursor it
// continues, and the objects found, in order.
type Walk struct {
	req        *module.ReplayRequest
	cur        *Cursor
	maxQ, maxO int
	// MaxBEEF bounds the BEEF of each answered output; guard.DefaultBound
	// when zero.
	MaxBEEF int

	asked, refused int
	objects        []module.ReplayObject
	now            map[chainhash.Hash]bool
	gaps           int
	notes          []string
	trees          map[chainhash.Hash]*transaction.Transaction
	gapped         map[chainhash.Hash]bool
}

// New starts a call on r, continuing the walk its cursor names.
func New(r *module.ReplayRequest) *Walk {
	cur, _ := r.Cursor.(*Cursor)
	if cur == nil {
		cur = &Cursor{}
	}
	if cur.Sent == nil {
		cur.Sent = map[chainhash.Hash]bool{}
	}
	if cur.Found == nil {
		cur.Found = map[string]bool{}
	}
	w := &Walk{req: r, cur: cur, maxQ: r.MaxQuestions, maxO: r.MaxObjects, now: map[chainhash.Hash]bool{}, trees: map[chainhash.Hash]*transaction.Transaction{},
		gapped: map[chainhash.Hash]bool{}}
	if w.maxQ <= 0 {
		w.maxQ = module.DefaultReplayQuestions
	}
	if w.maxO <= 0 {
		w.maxO = module.DefaultReplayObjects
	}
	return w
}

// Request is the call's request.
func (w *Walk) Request() *module.ReplayRequest { return w.req }

// Room reports whether the call can ask one more question and answer one
// more object.
func (w *Walk) Room() bool { return w.asked < w.maxQ && len(w.objects) < w.maxO }

// Gap counts something the peer holds that this host lacks and the walk
// cannot answer now, and why.
func (w *Walk) Gap(note string) {
	w.gaps++
	w.Note(note)
}

// Note keeps a line for the host's log, at most MaxNotes a call.
func (w *Walk) Note(note string) {
	if len(w.notes) < MaxNotes {
		w.notes = append(w.notes, note)
	}
}

// Ask asks the peer one question of the application's lookup service. A
// refusal is counted and answered as nothing (the item has nothing the walk
// can use); a peer that does not answer is an error that ends the call;
// ErrBudget when the call has no question left. An output whose BEEF does
// not parse is left out, with a note.
func (w *Walk) Ask(ctx context.Context, service string, query map[string]string) ([]*Output, error) {
	if w.asked >= w.maxQ {
		return nil, ErrBudget
	}
	q, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	w.asked++
	ans, err := w.req.Ask(ctx, &lookup.LookupQuestion{Service: service, Query: q})
	if err != nil {
		if errors.Is(err, module.ErrRefused) {
			w.refused++
			w.Note(fmt.Sprintf("%s %s: the peer refused: %v", service, q, err))
			return nil, nil
		}
		return nil, fmt.Errorf("replay: asking %s %s: %w", service, q, err)
	}
	return w.outputs(ans), nil
}

func (w *Walk) outputs(ans *lookup.LookupAnswer) []*Output {
	if ans == nil || ans.Type != lookup.AnswerTypeOutputList {
		return nil
	}
	bound := w.MaxBEEF
	if bound <= 0 {
		bound = guard.DefaultBound
	}
	var contexts []module.HydratedOutput
	if h, ok := ans.Result.([]module.HydratedOutput); ok && len(h) == len(ans.Outputs) {
		contexts = h
	}
	out := make([]*Output, 0, len(ans.Outputs))
	for i, o := range ans.Outputs {
		if o == nil {
			continue
		}
		beef, tx, txid, err := guard.ParseBEEF(o.Beef, bound)
		if err != nil || tx == nil || txid == nil {
			w.Note(fmt.Sprintf("the peer answered an output whose BEEF does not parse: %v", err))
			continue
		}
		if t := beef.FindAtomicTransactionByHash(txid); t != nil {
			tx = t
		}
		if int(o.OutputIndex) >= len(tx.Outputs) {
			w.Note(fmt.Sprintf("the peer answered output %d of %s, which it does not have", o.OutputIndex, txid))
			continue
		}
		x := &Output{Txid: *txid, Tx: tx, Vout: o.OutputIndex, BEEF: o.Beef}
		if contexts != nil {
			x.Context = contexts[i].Context
		}
		out = append(out, x)
	}
	return out
}

// Wanted reports whether txid is one to answer: not answered in this walk
// and not applied by the host's topic.
func (w *Walk) Wanted(ctx context.Context, txid chainhash.Hash) (bool, error) {
	if w.cur.Sent[txid] {
		return false, nil
	}
	if w.req.Applied == nil {
		return true, nil
	}
	applied, err := w.req.Applied(ctx, txid)
	if err != nil {
		return false, fmt.Errorf("replay: is %s applied: %w", txid, err)
	}
	return !applied, nil
}

// Find keeps a key for a later phase of the walk (Found).
func (w *Walk) Find(key string) { w.cur.Found[key] = true }

// Found are the keys the walk found that begin with prefix, sorted.
func (w *Walk) Found(prefix string) []string {
	var out []string
	for k := range w.cur.Found {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Sent reports whether the walk answered txid, in this call or an earlier
// one.
func (w *Walk) Sent(txid chainhash.Hash) bool { return w.cur.Sent[txid] }

// SentNow reports whether this call answered txid.
func (w *Walk) SentNow(txid chainhash.Hash) bool { return w.now[txid] }

// Emit answers one object; ErrBudget when the call has no object left.
func (w *Walk) Emit(o module.ReplayObject) error {
	if len(w.objects) >= w.maxO {
		return ErrBudget
	}
	w.objects = append(w.objects, o)
	w.cur.Sent[o.Txid] = true
	w.now[o.Txid] = true
	return nil
}

// Each asks one question and answers every output's transaction the walk
// wants (Wanted), as the peer gave it, in the peer's order, kind naming
// them; keep, when not nil, leaves out what it refuses. It returns every
// output the peer answered, for a caller that pages.
func (w *Walk) Each(ctx context.Context, kind, service string, query map[string]string, keep func(*Output) bool) ([]*Output, error) {
	outs, err := w.Ask(ctx, service, query)
	if err != nil {
		return nil, err
	}
	for _, o := range outs {
		if keep != nil && !keep(o) {
			continue
		}
		want, err := w.Wanted(ctx, o.Txid)
		if err != nil {
			return nil, err
		}
		if !want {
			continue
		}
		if err := w.Emit(module.ReplayObject{Txid: o.Txid, BEEF: o.BEEF, Kind: kind}); err != nil {
			return nil, err
		}
	}
	return outs, nil
}

// Phase is one phase of a walk: its items, by key in ascending order,
// computed at each call from what the host holds, and the work on one.
type Phase struct {
	Name string
	// Items lists the phase's item keys, from what the host holds and what
	// the walk found (Found).
	Items func(ctx context.Context, w *Walk) ([]string, error)
	// Do works on one item. It answers done when the item is finished;
	// ErrBudget (or false) stops the call where it is, and *page says where
	// the item resumes at the next call ("" from its start).
	Do func(ctx context.Context, w *Walk, item string, page *string) (done bool, err error)
}

// Run runs a walk's phases from the request's cursor and answers the call.
// A phase that answered objects ends the call when it finishes, so that the
// host admits them before the next phase reads the index; the walk is Done
// once every phase finished.
func Run(ctx context.Context, r *module.ReplayRequest, phases ...Phase) (*module.ReplayAnswer, error) {
	w := New(r)
	return w.Run(ctx, phases...)
}

// Run is the package's Run on this walk.
func (w *Walk) Run(ctx context.Context, phases ...Phase) (*module.ReplayAnswer, error) {
	for w.cur.Phase < len(phases) {
		p := phases[w.cur.Phase]
		items, err := p.Items(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("replay: %s: %w", p.Name, err)
		}
		sort.Strings(items)
		for i, item := range items {
			if (i > 0 && items[i-1] == item) || (w.cur.Item != "" && item <= w.cur.Item) {
				continue
			}
			page := w.cur.Page
			done, err := p.Do(ctx, w, item, &page)
			switch {
			case errors.Is(err, ErrBudget):
				w.cur.Page = page
				return w.answer(false), nil
			case err != nil:
				return nil, fmt.Errorf("replay: %s %s: %w", p.Name, item, err)
			case !done:
				w.cur.Page = page
				return w.answer(false), nil
			}
			w.cur.Item, w.cur.Page = item, ""
			if !w.Room() {
				return w.answer(false), nil
			}
		}
		w.cur.Phase++
		w.cur.Item, w.cur.Page = "", ""
		if len(w.objects) > 0 && w.cur.Phase < len(phases) {
			return w.answer(false), nil
		}
	}
	return w.answer(true), nil
}

func (w *Walk) answer(done bool) *module.ReplayAnswer {
	a := &module.ReplayAnswer{Objects: w.objects, Done: done, Questions: w.asked, Refused: w.refused, Gaps: w.gaps, Notes: w.notes}
	if !done {
		a.Cursor = w.cur
	}
	return a
}
