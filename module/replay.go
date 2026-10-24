package module

import (
	"context"
	"errors"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
)

// Ordered replay (Go only).
//
// A chain application's specification may join a host to its topics by
// replay alone: each token submitted again in a BEEF of the token and its
// parent, which the replayer assembles, in chain order, and the records that
// hang off the chain after it; an unordered synchronization of unspent
// outputs is never enabled for those topics, since it never carries a
// chain's spent tokens and drops what a topic manager refuses without
// retrying it. A Replayer is such an application's lookup service repairing
// its own host's gaps that way. Given a peer it can ask (the peer's lookup
// service, through Ask, with the application's own questions) and what this
// host has applied (Applied), it answers the objects to submit, in the order
// to submit them, bounded per call. The host submits each through its
// ordinary admission path, never one whose After it did not admit, and
// calls again with the cursor the answer carries until an answer is Done.
//
// The replayer has no TypeScript twin: the TypeScript host is frozen, and
// the twins table (the package documentation and its test) is held to
// engine-types.ts, where nothing names a replayer. Package
// github.com/lightwebinc/bcommon/replay is the walk a replayer is built on.

// Replayer is a LookupService that repairs its host's gaps on the topics
// its module manages by ordered replay. It is optional: a host asserts for
// it on the lookup services of the module that manages a topic, and repairs
// by replay only the topics whose application forbids an unordered
// synchronization.
type Replayer interface {
	// Replay answers the next objects to submit to r.Topic from what the
	// peer holds and this host lacks. A topic the replayer does not repair
	// is ErrNoReplay.
	Replay(ctx context.Context, r *ReplayRequest) (*ReplayAnswer, error)
}

// Ask asks one question of a peer's lookup service and answers as the
// peer's BRC-24 route does: an output-list answer's outputs with their
// BEEF, and, when the peer gives contexts, Result set to a
// []HydratedOutput. A question the peer answers with a refusal is an error
// wrapping ErrRefused; any other error is the peer not answering.
type Ask func(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error)

// ErrRefused wraps a peer's refusal of a question: the peer answered, and
// the answer was an error.
var ErrRefused = errors.New("module: the peer refused the question")

// ErrNoReplay is a replayer's answer for a topic it does not repair.
var ErrNoReplay = errors.New("module: the topic is not repaired by replay")

// The bounds of one call when a request names none.
const (
	DefaultReplayQuestions = 256
	DefaultReplayObjects   = 256
)

// ReplayRequest is one call of a Replayer.
type ReplayRequest struct {
	// Topic is the topic to repair, one the module manages.
	Topic string
	// Peer names the peer Ask reaches, for the replayer's notes.
	Peer string
	// Ask reaches the peer's lookup service.
	Ask Ask
	// Applied reports whether this host's topic has applied a transaction:
	// admitted it, or decided it and kept that record. A replayer offers
	// nothing the topic applied.
	Applied func(ctx context.Context, txid chainhash.Hash) (bool, error)
	// Cursor is the previous answer's cursor of the same walk, nil to start
	// one. It is the replayer's own: the host passes it back unchanged, in
	// the same process, and starts a new walk after an answer that is Done.
	Cursor any
	// MaxQuestions and MaxObjects bound this call: at most that many
	// questions asked of the peer and that many objects answered. Zero or
	// less takes DefaultReplayQuestions and DefaultReplayObjects.
	MaxQuestions, MaxObjects int
}

// ReplayObject is one object to submit to the topic.
type ReplayObject struct {
	// Txid is the subject's.
	Txid chainhash.Hash
	// BEEF is the submission, exactly as the host admits it: a token in
	// the BEEF of the token and its parent, anything else as the peer
	// answered it.
	BEEF []byte
	// Kind says what it is ("token", "entry", "sweep" ...), for the host's
	// counts.
	Kind string
	// After are objects earlier in the same answer that must be admitted
	// (or found already applied) before this one is submitted: a token's
	// parent token. The host submits nothing whose After it did not admit.
	After []chainhash.Hash
}

// ReplayAnswer is one call's answer.
type ReplayAnswer struct {
	// Objects are what to submit, in order.
	Objects []ReplayObject
	// Cursor continues the walk; the host passes it to the next call.
	Cursor any
	// Done says the walk is over: everything the peer answered is applied
	// here, answered in this walk, or counted in Gaps.
	Done bool
	// Questions is how many questions this call asked of the peer, and
	// Refused how many of them the peer refused.
	Questions, Refused int
	// Gaps counts what the peer holds and this host lacks that the replayer
	// could not answer now: a chain's first token whose parent no chain
	// source gives, a token whose parent the peer did not answer, a token
	// whose parent was offered and is not held. A later walk tries again;
	// until then only the application's own fill repairs it.
	Gaps int
	// Notes say why, a line each, for the host's log.
	Notes []string
}
