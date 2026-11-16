// Package verify is what a reader shares with every other reader: the
// outcome vocabulary it reports (codes.go), the verdict on one
// transaction against the reader's own headers (Check), and the check every
// carrier a host answers gets before a reader believes it (VerifyCarrier).
//
// An application's own algorithm (which outputs it asks for, what its records
// say about each other, what it pins) stays with the application. What it
// hands this package is the carrier's parameters and its own expectations of
// the payload, through CarrierSpec.
//
// Nothing here reaches the network except through the chain tracker it is
// handed, and a nil tracker is refused rather than replaced by a default.
package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
)

// Item is one output a host answered: its BEEF and the output index.
type Item struct {
	Beef        []byte
	OutputIndex uint32
}

// Step is one check and its verdict, for the verbose trace.
type Step struct {
	Name   string
	OK     bool
	Detail string
}

// ErrNoTracker is Check handed no chain tracker. The SDK would dial a public
// service in its place, so a reader that forgot its own header source would
// quietly trust someone else's.
var ErrNoTracker = errors.New("verify: no chain tracker; refusing to verify against a default")

// Verdict classifies a verification. A proof the tracker rejected, a
// failing script and a missing ancestor are refusals; a tracker that could
// not answer is no verdict, and only that is a reason to stop without one,
// so they are told apart here.
type Verdict int

const (
	// Passed is a transaction proven against the tracker, through its own
	// proof or its ancestry.
	Passed Verdict = iota
	// ProofRefused is a proof, the transaction's or an ancestor's, that the
	// tracker does not hold.
	ProofRefused
	// ScriptRefused is an input that does not satisfy the output it spends.
	ScriptRefused
	// AncestryMissing is an unproven transaction whose BEEF lacks a parent it
	// spends from.
	AncestryMissing
	// Transport is no verdict: the tracker could not answer, or there was
	// none to ask.
	Transport
)

// Check runs SPV over tx against the tracker and says which way it went.
// The error carries the detail for every verdict but Passed.
//
// It is the overlay plane's SPV, not a node's: a transaction that is not
// final is admitted. A carrier is not final by design (a lock time in 2100
// and a non-final input keep it off the chain), and from v1.7.2 go-sdk's
// spv.Verify refuses every such transaction as a node would refuse to mine
// it. So Check does the two halves of SPV itself, from the SDK's parts:
// every proof reached is verified against the tracker, and then the SDK
// checks every unmined transaction's scripts, amounts and shape
// (spv.VerifyScripts), which leaves finality and the maturity of coinbase
// and confiscation outputs out. A payment, which must be mineable, is not
// verified here: its readers call spv.Verify.
func Check(ctx context.Context, tx *transaction.Transaction, tracker chaintracker.ChainTracker) (Verdict, error) {
	if tracker == nil {
		return Transport, ErrNoTracker
	}
	// A proof that does not lead from the transaction it is attached to is
	// refused here, by arithmetic, so that what the tracker is asked below
	// can fail only because the tracker could not answer.
	if err := pathsLead(tx); err != nil {
		return ProofRefused, err
	}
	if k, err := proofsHold(ctx, tx, tracker); k != Passed {
		return k, err
	}
	ok, err := spv.VerifyScripts(ctx, tx)
	switch {
	case err == nil && ok:
		return Passed, nil
	case err == nil:
		return ScriptRefused, errors.New("not verified")
	case errors.Is(err, spv.ErrInvalidMerklePath):
		return ProofRefused, err
	case errors.Is(err, spv.ErrMissingSourceTransaction):
		return AncestryMissing, err
	default:
		// No tracker is asked in this half, so every other error is the
		// transaction's own: a script, an amount, a shape no node takes.
		return ScriptRefused, err
	}
}

// proofsHold asks the tracker about every Merkle path SPV rests on, tx's
// own or its ancestors' down to the first proof on each line.
func proofsHold(ctx context.Context, tx *transaction.Transaction, tracker chaintracker.ChainTracker) (Verdict, error) {
	seen := map[*transaction.Transaction]bool{}
	var walk func(t *transaction.Transaction) (Verdict, error)
	walk = func(t *transaction.Transaction) (Verdict, error) {
		if t == nil || seen[t] {
			return Passed, nil
		}
		seen[t] = true
		if t.MerklePath != nil {
			ok, err := t.MerklePath.Verify(ctx, t.TxID(), tracker)
			if err != nil {
				return Transport, err
			}
			if !ok {
				return ProofRefused, fmt.Errorf("%w for transaction %s", spv.ErrInvalidMerklePath, t.TxID())
			}
			return Passed, nil
		}
		for _, in := range t.Inputs {
			if k, err := walk(in.SourceTransaction); k != Passed {
				return k, err
			}
		}
		return Passed, nil
	}
	return walk(tx)
}

// pathsLead checks every Merkle path SPV would rest on, tx's own or its
// ancestors' down to the first proof on each line: each must lead from the
// transaction it is attached to up to a root. Whether that root is the
// chain's is the tracker's to say.
func pathsLead(tx *transaction.Transaction) error {
	seen := map[*transaction.Transaction]bool{}
	var walk func(t *transaction.Transaction) error
	walk = func(t *transaction.Transaction) error {
		if t == nil || seen[t] {
			return nil
		}
		seen[t] = true
		if t.MerklePath != nil {
			if _, err := t.MerklePath.ComputeRoot(t.TxID()); err != nil {
				return fmt.Errorf("%w: the proof attached to %s does not lead from it: %v", spv.ErrInvalidMerklePath, t.TxID(), err)
			}
			return nil
		}
		for _, in := range t.Inputs {
			if err := walk(in.SourceTransaction); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(tx)
}
