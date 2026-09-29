package verify

// Code is the one-word outcome of a verification. One uppercase token per
// outcome, so a caller or script can branch on it without parsing prose, and
// so two independent readers can be compared line for line.
type Code string

const (
	// Verified is every step passed with a proof against the reader's own
	// headers.
	Verified Code = "VERIFIED"
	// VerifiedUnmined is every step passed but the token carries no proof
	// yet; its ancestry verified instead. OK counts it as a pass, so a caller
	// that does not accept unmined state checks for it itself. An
	// application's verification algorithm produces it; this package never
	// does.
	VerifiedUnmined Code = "VERIFIED-UNMINED"
	// RecordPending is a verified token whose carrier the host has not
	// served: the commitment is on the chain and the record is not here yet.
	// An application's verification algorithm produces it; this package
	// never does.
	RecordPending Code = "RECORD-PENDING"

	// Unsupported is a store whose entry uses a feature this build does not
	// implement. It is never the record's verdict: the record verified, and
	// one store is unreadable here. An application's verification algorithm
	// produces it; this package never does.
	Unsupported Code = "UNSUPPORTED"

	// NoToken is a host answer that holds nothing to verify.
	NoToken Code = "NO-TOKEN"
	// RefusedDecode is an answer that does not decode, or breaks its payload's rules.
	RefusedDecode Code = "REFUSED-DECODE"
	// RefusedKeyDerive is a lock that is not the key the identity derives.
	RefusedKeyDerive Code = "REFUSED-KEY-DERIVE"
	// RefusedSig is a field signature or an input script that does not verify.
	RefusedSig Code = "REFUSED-SIG"
	// RefusedKey is an unexpected identity; an application's verification algorithm produces it.
	RefusedKey Code = "REFUSED-KEY"
	// RefusedSeq is a sequence out of order; an application's verification algorithm produces it.
	RefusedSeq Code = "REFUSED-SEQ"
	// RefusedFork is more than one answer where the question allows one.
	RefusedFork Code = "REFUSED-FORK"
	// RefusedExpired is a record not valid now; an application's verification algorithm produces it.
	RefusedExpired Code = "REFUSED-EXPIRED"
	// RefusedBump is a transaction with no proof the reader's header source holds.
	RefusedBump Code = "REFUSED-BUMP"
	// RefusedCommit is an answer that does not match its commitment.
	RefusedCommit Code = "REFUSED-COMMIT"
	// RefusedWitness is a mismatched witness; an application's verification algorithm produces it.
	RefusedWitness Code = "REFUSED-WITNESS"
	// RefusedMineable is a carrier that could be mined.
	RefusedMineable Code = "REFUSED-MINEABLE"
	// RefusedUnlocking is a carrier whose input is not spent by exactly one
	// canonical signature push (carrier.CheckUnlocking): a spend anyone can
	// rewrite without the key under another txid.
	RefusedUnlocking Code = "REFUSED-UNLOCKING"
	// RefusedRetired is a retired identity; an application's verification algorithm produces it.
	RefusedRetired Code = "REFUSED-RETIRED"
	// Error is not a verdict: the reader could not decide, typically because
	// the header source did not answer. A caller reports it apart from every
	// REFUSED-* code, so an outage is never read as a forgery.
	Error Code = "ERROR"
)

// OK reports whether the code is a pass.
func (c Code) OK() bool { return c == Verified || c == VerifiedUnmined }
