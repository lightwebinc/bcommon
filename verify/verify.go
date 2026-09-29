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

// Verdict classifies what spv.Verify reported. The SDK reports a proof the
// tracker rejected, a failing script and a missing ancestor all as errors,
// and a tracker that could not answer as an error too; only the last is a
// reason to stop without a verdict, so they are told apart here.
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
// The error carries the SDK's detail for every verdict but Passed.
func Check(ctx context.Context, tx *transaction.Transaction, tracker chaintracker.ChainTracker) (Verdict, error) {
	if tracker == nil {
		return Transport, ErrNoTracker
	}
	ok, err := spv.Verify(ctx, tx, tracker, nil)
	switch {
	case err == nil && ok:
		return Passed, nil
	case err == nil:
		return ProofRefused, errors.New("not verified")
	case errors.Is(err, spv.ErrInvalidMerklePath):
		return ProofRefused, err
	case errors.Is(err, spv.ErrScriptVerificationFailed):
		return ScriptRefused, err
	case errors.Is(err, spv.ErrMissingSourceTransaction):
		return AncestryMissing, err
	default:
		return Transport, err
	}
}
