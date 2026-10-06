package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/chainview"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// StatusSource is a broadcaster's view of one transaction, as arcade's
// GET /tx/{txid} answers it (*publish.Arcade).
type StatusSource interface {
	Status(ctx context.Context, txid string) (*publish.ArcadeStatus, error)
}

// SpendView is the node's view of an output (*nodeapi.Asset): the txid
// that spent it, "" with a nil error only when the node says it is
// unspent, and an error for any other answer.
type SpendView interface {
	Spender(ctx context.Context, txid string, vout uint32) (string, error)
}

// ProofSource is where a mined transaction's proof is read
// (*nodeapi.Asset); an error is not mined yet, or not known.
type ProofSource interface {
	Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error)
}

var (
	_ StatusSource = (*publish.Arcade)(nil)
	_ SpendView    = (*nodeapi.Asset)(nil)
	_ ProofSource  = (*nodeapi.Asset)(nil)
)

// Output is one output a payment must hold: at index Vout, locked by
// Script (the key the receiver derived for the payment), at least Sats.
type Output struct {
	Vout   uint32
	Script []byte
	Sats   uint64
}

// Payment is a payment offered to the receiver: the transaction, read from
// its BEEF so that its inputs carry their source transactions (guard
// first), the payer it is charged to, the outputs it must hold, and what
// the payer asked.
type Payment struct {
	Tx    *transaction.Transaction
	Payer string
	Pays  []Output
	Ask   Ask
}

// Verifier gathers a payment's evidence under a Policy.
type Verifier struct {
	Policy   Policy
	Exposure *Exposure
	// Headers are the receiver's own headers, which every proof is checked
	// against.
	Headers chaintracker.ChainTracker
	// Settler is the leg the receiver broadcasts on; without one, no
	// payment is fast.
	Settler publish.Settler
	// Status are the broadcasters asked for the network's verdict; Policy
	// .Agree of them must answer accepted. Spends, when set, is the node's
	// spend view, which must name no other spender and must answer for
	// every input. Proofs is where Confirm reads a proof.
	Status []StatusSource
	Spends SpendView
	Proofs ProofSource
}

// ErrNoHeaders is a Verifier with no headers to check a payment against.
var ErrNoHeaders = errors.New("acceptance: no headers to verify the payment against")

// Accept checks a payment and decides on it:
//
//  1. the checks every payment passes (Check): well formed, final, pays
//     every Output, spends no more than it has, and its ancestry verifies
//     to mined proofs against Headers;
//  2. Decide on its value; a mined payment that verifies is Fast at once;
//  3. the receiver broadcasts it, its unmined ancestors first, whatever
//     the decision, so that a held payment mines too;
//  4. for a Fast decision, the network's verdict: Agree status sources
//     answer it accepted with no double-spend status and no competing
//     transaction, and the spend view names no other spender of any
//     input, through Policy.Watch.
//
// A payment that loses its fast evidence for a reason that may pass (no
// verdict yet, a spend view that does not answer) is held; one the network
// refuses, or whose input is spent by another transaction, is refused. The
// error is only for a Verifier that cannot decide at all, or a context
// that ended.
func (v *Verifier) Accept(ctx context.Context, p Payment) (Verdict, error) {
	if v.Headers == nil {
		return Verdict{}, ErrNoHeaders
	}
	sats, vd := v.Check(ctx, p)
	if vd != nil {
		return *vd, nil
	}
	txid := p.Tx.TxID().String()
	if p.Tx.MerklePath != nil {
		return Verdict{Decision: Fast, Reason: ReasonMined, Sats: sats}, nil
	}
	x := v.Exposure
	d := v.Policy.Decide(ctx, x, Request{Payer: p.Payer, Txid: txid, Sats: sats, Ask: p.Ask})
	if d.Decision == Refuse {
		return d, nil
	}
	fast := d.Decision == Fast
	demote := func(r Reason, detail string) (Verdict, error) {
		if fast {
			x.Release(txid)
		}
		d.Decision, d.Reason, d.Detail = Hold, r, detail
		return d, nil
	}
	refuse := func(r Reason, detail string) (Verdict, error) {
		if fast {
			x.Release(txid)
		}
		d.Decision, d.Reason, d.Detail = Refuse, r, detail
		return d, nil
	}
	if v.Settler == nil {
		if fast {
			return demote(ReasonNoBroadcast, "no leg to broadcast the payment on")
		}
		return d, nil
	}
	if err := v.broadcast(ctx, p.Tx); err != nil {
		if why, ok := chainview.RefusedAnswer(err); ok {
			return refuse(ReasonNetworkRefused, why)
		}
		if errors.Is(err, nodeapi.ErrDoubleSpent) {
			return refuse(ReasonDoubleSpent, err.Error())
		}
		if ctx.Err() != nil {
			if fast {
				x.Release(txid)
			}
			return Verdict{}, ctx.Err()
		}
		if fast {
			return demote(ReasonBroadcastUnknown, err.Error())
		}
		return d, nil
	}
	if !fast {
		return d, nil
	}
	ev, err := v.evidence(ctx, p.Tx, txid)
	if err != nil {
		x.Release(txid)
		return Verdict{}, err
	}
	switch ev.Decision {
	case Refuse:
		return refuse(ev.Reason, ev.Detail)
	case Hold:
		return demote(ev.Reason, ev.Detail)
	}
	return d, nil
}

// Check is the checks every payment passes before its value is weighed. It
// answers what the payment pays the receiver, and a refusal, or nil when
// it passes.
func (v *Verifier) Check(ctx context.Context, p Payment) (uint64, *Verdict) {
	refuse := func(sats uint64, r Reason, detail string) (uint64, *Verdict) {
		return sats, &Verdict{Decision: Refuse, Reason: r, Detail: detail, Sats: sats}
	}
	tx := p.Tx
	if tx == nil || len(tx.Inputs) == 0 || len(tx.Outputs) == 0 {
		return refuse(0, ReasonMalformed, "no transaction, or one with no inputs or outputs")
	}
	if len(p.Pays) == 0 {
		return refuse(0, ReasonMalformed, "no output named to pay the receiver")
	}
	var sats uint64
	seen := map[uint32]bool{}
	for _, o := range p.Pays {
		if uint64(o.Vout) >= uint64(len(tx.Outputs)) || seen[o.Vout] {
			return refuse(0, ReasonMalformed, fmt.Sprintf("output %d is not in the transaction, or is named twice", o.Vout))
		}
		seen[o.Vout] = true
		out := tx.Outputs[o.Vout]
		if out.LockingScript == nil || len(o.Script) == 0 || !bytes.Equal(*out.LockingScript, o.Script) {
			return refuse(0, ReasonWrongScript, fmt.Sprintf("output %d does not pay the key derived for it", o.Vout))
		}
		if out.Satoshis < o.Sats {
			return refuse(0, ReasonUnderpaid, fmt.Sprintf("output %d holds %d sat of %d", o.Vout, out.Satoshis, o.Sats))
		}
		if sats+out.Satoshis < sats {
			return refuse(0, ReasonMalformed, "the outputs overflow")
		}
		sats += out.Satoshis
	}
	if sats == 0 {
		return refuse(0, ReasonNothing, "")
	}
	if tx.MerklePath == nil {
		if !final(tx) {
			return refuse(sats, ReasonNotFinal, "the payment is not final: it can be replaced before it mines")
		}
		var in, out uint64
		for i, txin := range tx.Inputs {
			src := txin.SourceTxOutput()
			if src == nil {
				return refuse(sats, ReasonSPV, fmt.Sprintf("input %d carries no source transaction", i))
			}
			in += src.Satoshis
		}
		for _, o := range tx.Outputs {
			out += o.Satoshis
		}
		if out > in {
			return refuse(sats, ReasonOverspends, fmt.Sprintf("outputs %d sat, inputs %d sat", out, in))
		}
	}
	if ok, err := spv.Verify(ctx, tx, v.Headers, nil); err != nil || !ok {
		return refuse(sats, ReasonSPV, fmt.Sprintf("the payment does not verify against the headers: %v", err))
	}
	return sats, nil
}

// final is a transaction that cannot be replaced: no lock time, or every
// input's sequence final.
func final(tx *transaction.Transaction) bool {
	if tx.LockTime == 0 {
		return true
	}
	for _, in := range tx.Inputs {
		if in.SequenceNumber != transaction.MaxTxInSequenceNum {
			return false
		}
	}
	return true
}

// broadcast submits tx's unmined ancestors, oldest first, then tx. An
// ancestor's answer is not the payment's; one the network already holds
// is not an error.
func (v *Verifier) broadcast(ctx context.Context, tx *transaction.Transaction) error {
	for _, anc := range unmined(tx) {
		_ = v.Settler.Submit(ctx, anc)
	}
	if err := v.Settler.Submit(ctx, tx); err != nil && !alreadyKnown(err) {
		return err
	}
	return nil
}

// unmined are the ancestors of tx carried without a proof, each after its
// own.
func unmined(tx *transaction.Transaction) []*transaction.Transaction {
	var out []*transaction.Transaction
	seen := map[*transaction.Transaction]bool{}
	var walk func(t *transaction.Transaction)
	walk = func(t *transaction.Transaction) {
		for _, in := range t.Inputs {
			src := in.SourceTransaction
			if src == nil || src.MerklePath != nil || seen[src] {
				continue
			}
			seen[src] = true
			walk(src)
			out = append(out, src)
		}
	}
	walk(tx)
	return out
}

func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "txn-already-known")
}

// evidence is the fast path's network evidence, polled until Agree sources
// accept and Watch has passed, or Wait ends: Fast, Hold or Refuse.
func (v *Verifier) evidence(ctx context.Context, tx *transaction.Transaction, txid string) (Verdict, error) {
	pol := v.Policy
	wait, poll, watch := pol.Wait, pol.Poll, pol.Watch
	if wait <= 0 {
		wait = DefaultWait
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	if watch > wait {
		wait = watch
	}
	start := time.Now()
	for {
		ev := v.round(ctx, tx, txid)
		elapsed := time.Since(start)
		switch {
		case ev.Decision == Refuse:
			return ev, nil
		case ev.Decision == Fast && elapsed >= watch:
			return ev, nil
		case ev.Decision == Hold && ev.Reason != ReasonNoVerdict:
			// A conflict, or a spend view that does not answer, is not
			// waited out: the payment is held for its block.
			return ev, nil
		case elapsed >= wait:
			return ev, nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return Verdict{}, ctx.Err()
		case <-t.C:
		}
	}
}

// round asks every status source and the spend view once.
func (v *Verifier) round(ctx context.Context, tx *transaction.Transaction, txid string) Verdict {
	agree := v.Policy.Agree
	if agree <= 0 {
		agree = 1
	}
	accepted := 0
	for _, s := range v.Status {
		st, err := s.Status(ctx, txid)
		if err != nil || st == nil {
			continue
		}
		switch {
		case st.TxStatus == "REJECTED":
			return Verdict{Decision: Refuse, Reason: ReasonNetworkRefused, Detail: st.Why()}
		case st.TxStatus == "DOUBLE_SPEND_ATTEMPTED" || len(st.CompetingTxs) > 0:
			// Two transactions spend one coin and either may mine: only a
			// block settles which.
			return Verdict{Decision: Hold, Reason: ReasonConflict, Detail: st.Why()}
		case st.Accepted():
			accepted++
		}
	}
	if v.Spends != nil {
		for i, in := range tx.Inputs {
			if in.SourceTXID == nil {
				return Verdict{Decision: Hold, Reason: ReasonSpendUnknown, Detail: fmt.Sprintf("input %d names no source", i)}
			}
			src := in.SourceTXID.String()
			by, err := v.Spends.Spender(ctx, src, in.SourceTxOutIndex)
			if err != nil {
				return Verdict{Decision: Hold, Reason: ReasonSpendUnknown, Detail: err.Error()}
			}
			if by != "" && !strings.EqualFold(by, txid) {
				return Verdict{Decision: Refuse, Reason: ReasonDoubleSpent,
					Detail: fmt.Sprintf("input %d (%s.%d) is spent by %s", i, src, in.SourceTxOutIndex, by)}
			}
		}
	}
	if accepted < agree {
		return Verdict{Decision: Hold, Reason: ReasonNoVerdict,
			Detail: fmt.Sprintf("%d of %d status sources answer it accepted, %d needed", accepted, len(v.Status), agree)}
	}
	return Verdict{Decision: Fast, Reason: ReasonAtOrBelow}
}

// Confirm waits for tx to mine and answers its proof, checked against
// Headers. While it waits, an input the spend view shows spent by another
// transaction ends the wait with a *nodeapi.SpentError (errors.Is
// nodeapi.ErrDoubleSpent). ctx bounds the wait.
func (v *Verifier) Confirm(ctx context.Context, tx *transaction.Transaction) (*transaction.MerklePath, uint32, error) {
	if v.Headers == nil {
		return nil, 0, ErrNoHeaders
	}
	if v.Proofs == nil {
		return nil, 0, errors.New("acceptance: no source to read a proof from")
	}
	poll := v.Policy.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	for {
		mp, h, done, err := v.confirmed(ctx, tx)
		if done || err != nil {
			return mp, h, err
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, 0, ctx.Err()
		case <-t.C:
		}
	}
}

// confirmed asks once: the proof when tx has mined and it verifies, an
// error when an input is spent elsewhere or a proof does not verify, or
// neither.
func (v *Verifier) confirmed(ctx context.Context, tx *transaction.Transaction) (*transaction.MerklePath, uint32, bool, error) {
	txid := tx.TxID()
	if mp, h, err := v.Proofs.Proof(ctx, txid.String()); err == nil && mp != nil {
		ok, err := mp.Verify(ctx, txid, v.Headers)
		if err == nil && ok {
			return mp, h, true, nil
		}
		return nil, 0, false, fmt.Errorf("acceptance: the proof of %s does not verify against the headers: %v", txid, err)
	}
	if v.Spends != nil {
		id := txid.String()
		for i, in := range tx.Inputs {
			if in.SourceTXID == nil {
				continue
			}
			src := in.SourceTXID.String()
			if by, err := v.Spends.Spender(ctx, src, in.SourceTxOutIndex); err == nil && by != "" && !strings.EqualFold(by, id) {
				return nil, 0, false, &nodeapi.SpentError{Txid: id, Input: i, Outpoint: fmt.Sprintf("%s.%d", src, in.SourceTxOutIndex), By: by}
			}
		}
	}
	return nil, 0, false, nil
}
