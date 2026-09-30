package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
)

// CarrierSpec is what makes a carrier an application's: the carrier's own
// parameters and classifier, where the payload names its identity, and the
// application's expectations of the payload at the position it was asked
// for. Every field is required.
type CarrierSpec struct {
	Params   carrier.Params
	Classify carrier.Classify
	// IdentityOf returns the identity key bytes the payload names. It runs
	// just before Validate, which parses them only after the finality
	// checks, so it reads the bytes and does not judge them. An error
	// refuses the carrier as REFUSED-DECODE with the label and the error.
	IdentityOf func(payload []byte) ([]byte, error)
	// Expect runs after Validate and before Check: the caller's own rules
	// (for example a kind, then an identity). A non-empty Code refuses with
	// that code and reason, before any header-source call. The reason is
	// used as given, so a caller that wants the label in it writes it.
	Expect func(payload []byte) (Code, string)
}

// errIncompleteSpec is a CarrierSpec with a hook missing. It is the
// caller's fault, not the carrier's, so it is no verdict.
var errIncompleteSpec = errors.New("verify: incomplete carrier spec")

// VerifyCarrier is the check every carrier a reader is handed gets: the host
// answered one output, it decodes as a carrier, its commitment is the one
// asked for, it validates (the payload's rules, unmineable shape, the one
// canonical unlocking script, lock derivation, field signature), it meets
// the caller's expectations, and its funding parent is proven in the
// reader's own header source. The unlocking script is checked on the
// carrier transaction the BEEF answers, before SPV runs it.
//
// The commitment is checked before anything the carrier says about itself:
// a host answering a different carrier than the one asked for is the case
// this exists for, and every later check would pass on a valid carrier that
// is simply not the one wanted. The expectations come after Validate and
// before the proof, so a carrier refused on its content costs the reader no
// header lookup.
//
// what labels the carrier in every reason and names the step a pass records.
// On a pass c is the carrier, code is Verified and steps holds that one
// step. A refusal returns its code and reason with one step named after the
// code. Error is no verdict: the reader could not decide, and it records no
// step.
func VerifyCarrier(ctx context.Context, items []Item, want [32]byte, what string, spec CarrierSpec, t chaintracker.ChainTracker) (c *carrier.Carrier, code Code, reason string, steps []Step) {
	refuse := func(rc Code, why string) (*carrier.Carrier, Code, string, []Step) {
		return nil, rc, why, []Step{{Name: string(rc), OK: false, Detail: why}}
	}
	if spec.Classify == nil || spec.IdentityOf == nil || spec.Expect == nil || spec.Params.ValidatePayload == nil {
		return nil, Error, errIncompleteSpec.Error(), nil
	}
	switch len(items) {
	case 0:
		return refuse(NoToken, fmt.Sprintf("the host holds no carrier %s for %s", displayHex(want), what))
	case 1:
	default:
		return refuse(RefusedFork, fmt.Sprintf("%s: %d outputs answered for one carrier", what, len(items)))
	}
	it := items[0]
	_, tx, txid, err := guard.ParseBEEF(it.Beef, guard.DefaultBound)
	if err != nil || tx == nil {
		return refuse(RefusedDecode, fmt.Sprintf("%s: BEEF does not parse: %v", what, err))
	}
	c, err = carrier.Decode(tx, spec.Classify)
	if err != nil {
		return refuse(RefusedDecode, what+": not a carrier: "+err.Error())
	}
	if c.OutputIndex != it.OutputIndex {
		return refuse(RefusedDecode, fmt.Sprintf("%s: the answered output %d is not the record output %d", what, it.OutputIndex, c.OutputIndex))
	}
	if carrier.Commitment(tx) != want {
		return refuse(RefusedCommit, fmt.Sprintf("%s: the host answered carrier %s, not %s", what, txid, displayHex(want)))
	}
	identity, err := spec.IdentityOf(c.Payload)
	if err != nil {
		return refuse(RefusedDecode, what+": "+err.Error())
	}
	if err := c.Validate(spec.Params, identity); err != nil {
		switch {
		case errors.Is(err, carrier.ErrMineable):
			return refuse(RefusedMineable, what+": "+err.Error())
		case errors.Is(err, carrier.ErrUnlocking):
			return refuse(RefusedUnlocking, what+": "+err.Error())
		case errors.Is(err, carrier.ErrLock):
			return refuse(RefusedKeyDerive, what+": "+err.Error())
		case errors.Is(err, carrier.ErrSignature):
			return refuse(RefusedSig, what+": "+err.Error())
		default:
			return refuse(RefusedDecode, what+": "+err.Error())
		}
	}
	if expected, why := spec.Expect(c.Payload); expected != "" {
		return refuse(expected, why)
	}
	// A carrier is never mined, so it proves only through its funding
	// parent, and a parent missing from the answer is the same refusal as
	// one the header source does not hold: nothing proves this carrier.
	switch k, err := Check(ctx, tx, t); k {
	case ProofRefused, AncestryMissing:
		return refuse(RefusedBump, what+": the funding parent is not proven in the header source")
	case ScriptRefused:
		return refuse(RefusedSig, what+": the input does not satisfy its funding output")
	case Transport:
		return nil, Error, what + ": could not verify: " + err.Error(), nil
	}
	return c, Verified, "", []Step{{Name: what, OK: true, Detail: txid.String()}}
}

// displayHex is a commitment as a person reads a txid: the SDK's display
// order, the reverse of the hash byte order a commitment is carried in.
func displayHex(c [32]byte) string {
	h := chainhash.Hash(c)
	return h.String()
}
