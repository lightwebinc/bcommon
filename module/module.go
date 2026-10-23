// Package module is the engine-neutral contract an application's overlay
// module satisfies in Go: the topic managers and lookup services an overlay
// host mounts, the payloads the host hands them, and the host they are
// built against. It is the Go twin of the TypeScript package's
// engine-types (ts/src/engine-types.ts).
//
// A module never imports an engine. It is written against these shapes,
// expressed in go-sdk's own types (a parsed BEEF, an outpoint, admittance
// instructions, a lookup question and answer), and an engine's host adapts
// them to whatever that engine calls. So the same module runs on any Go
// overlay host that carries the adapter, and a widening of an engine's
// interface cannot break a module's build.
//
// # The twins
//
// The TypeScript contract is declared structurally and marks some members
// optional. Go has neither, so an optional member is an optional interface a
// host asserts for, and a union payload is one struct with a Mode field that
// says which members are set.
//
//	TypeScript (engine-types.ts)       Go (this package)
//	AdmittanceInstructions             overlay.AdmittanceInstructions (go-sdk)
//	TopicManager                       TopicManager
//	  identifyAdmissibleOutputs          IdentifyAdmissibleOutputs
//	  identifyNeededInputs (optional)    InputIdentifier.IdentifyNeededInputs
//	  getDocumentation                   GetDocumentation
//	  getMetaData                        GetMetaData (*overlay.MetaData)
//	OutputAdmittedByTopic              OutputAdmittedByTopic
//	  mode                               Mode (AdmissionMode)
//	  txid, outputIndex                  Outpoint (transaction.Outpoint)
//	  topic                              Topic
//	  satoshis                           Satoshis
//	  lockingScript                      LockingScript
//	  atomicBEEF                         AtomicBEEF
//	  offChainValues (optional)          OffChainValues (nil when absent)
//	OutputSpent                        OutputSpent
//	  mode                               Mode (SpendNotificationMode)
//	  txid, outputIndex                  Outpoint
//	  topic                              Topic
//	  spendingTxid                       SpendingTxid
//	  inputIndex                         InputIndex
//	  unlockingScript                    UnlockingScript
//	  sequenceNumber                     SequenceNumber
//	  spendingAtomicBEEF                 SpendingAtomicBEEF
//	  offChainValues (optional)          OffChainValues (nil when absent)
//	LookupFormula                      []lookup.LookupFormula (go-sdk)
//	LookupQuestion                     *lookup.LookupQuestion (go-sdk)
//	LookupServiceMetaData              *overlay.MetaData (go-sdk)
//	LookupService                      LookupService
//	  admissionMode                      AdmissionMode()
//	  spendNotificationMode              SpendNotificationMode()
//	  outputAdmittedByTopic              OutputAdmittedByTopic
//	  outputSpent (optional)             SpendListener.OutputSpent
//	  outputNoLongerRetainedInHistory    RetentionListener.OutputNoLongerRetainedInHistory
//	    (optional)
//	  outputEvicted                      OutputEvicted
//	  lookup                             Lookup (*lookup.LookupAnswer)
//	  getDocumentation                   GetDocumentation
//	  getMetaData                        GetMetaData
//	  restore (optional)                 Restorer.Restore
//	Output                             Output
//	  txid, outputIndex                  Outpoint
//	  outputScript                       OutputScript
//	  satoshis                           Satoshis
//	  topic                              Topic
//	  spent                              Spent
//	  outputsConsumed                    OutputsConsumed
//	  consumedBy                         ConsumedBy
//	  beef (optional)                    Beef (nil when absent)
//	  blockHeight (optional)             BlockHeight (0 when unmined)
//	  score (optional)                   Score
//	ModuleHost                         Host
//	  log                                Log
//	  metrics.inc                        Metrics().Inc
//	  metrics.preset                     Metrics().Preset
//	  metrics.gauge                      Metrics().Gauge
//	RestoreStorage                     RestoreStorage
//	  findOutput                         FindOutput
//	Module                             Module
//	  topics                             Topics
//	  lookups                            Lookups
//	default export (host) => Module    Factory
//
// Two TypeScript arguments have no Go member. identifyAdmissibleOutputs
// takes offChainValues and a mode on the TypeScript side; a Go engine hands
// a manager neither, so a module that admits on either is not portable and
// does not exist. The TypeScript lookupFormula's context has no go-sdk
// field either; no module sets one.
//
// A lookup answer's metadata (name, shortDescription, iconURL, version,
// informationURL) and admittance instructions (outputsToAdmit,
// coinsToRetain, coinsRemoved) cross the wire as JSON, and go-sdk's types
// carry exactly the TypeScript field names as their JSON tags; the package's
// test holds them to it, and holds this table to the TypeScript source.
package module

import (
	"context"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// AdmissionMode is the shape a lookup service wants an admitted output in.
type AdmissionMode string

// The admission modes.
const (
	// AdmitLockingScript: the host hands the output's locking script,
	// satoshis and outpoint.
	AdmitLockingScript AdmissionMode = "locking-script"
	// AdmitWholeTx: the host hands the whole transaction as Atomic BEEF.
	AdmitWholeTx AdmissionMode = "whole-tx"
)

// SpendNotificationMode is how much a lookup service wants to hear about a
// spend of an output it indexed.
type SpendNotificationMode string

// The spend notification modes.
const (
	// SpendNone: the service is not told of spends.
	SpendNone SpendNotificationMode = "none"
	// SpendTxid: the spending transaction's id.
	SpendTxid SpendNotificationMode = "txid"
	// SpendScript: the spending input's script, index and sequence.
	SpendScript SpendNotificationMode = "script"
	// SpendWholeTx: the spending transaction as Atomic BEEF.
	SpendWholeTx SpendNotificationMode = "whole-tx"
)

// TopicManager decides which outputs of a transaction a topic admits.
type TopicManager interface {
	// IdentifyAdmissibleOutputs answers which outputs of the transaction
	// txid names in beef the topic admits, and which of the previous coins
	// (the inputs' outpoints the host already holds on this topic, by input
	// index) it retains. A transaction that admits nothing is an empty
	// answer, never an error: an error is the manager failing, not the
	// transaction.
	IdentifyAdmissibleOutputs(ctx context.Context, beef *transaction.Beef, txid *chainhash.Hash, previousCoins []uint32) (overlay.AdmittanceInstructions, error)
	// GetDocumentation is the topic's documentation, Markdown.
	GetDocumentation() string
	// GetMetaData names the topic.
	GetMetaData() *overlay.MetaData
}

// InputIdentifier is a TopicManager that can name inputs the host must
// fetch before admission (TypeScript identifyNeededInputs). A manager that
// needs none leaves it out.
type InputIdentifier interface {
	IdentifyNeededInputs(ctx context.Context, beef *transaction.Beef, txid *chainhash.Hash) ([]*transaction.Outpoint, error)
}

// OutputAdmittedByTopic is what a lookup service is told when a topic
// manager admits an output. Mode says which members are set: under
// AdmitLockingScript the outpoint, satoshis and locking script; under
// AdmitWholeTx the outpoint and the Atomic BEEF. Topic and OffChainValues
// are set under both.
type OutputAdmittedByTopic struct {
	Mode           AdmissionMode
	Outpoint       transaction.Outpoint
	Topic          string
	Satoshis       uint64
	LockingScript  *script.Script
	AtomicBEEF     []byte
	OffChainValues []byte
}

// OutputSpent is what a lookup service is told when an output it indexed is
// spent. Mode says which members are set: the outpoint and topic always;
// SpendingTxid under SpendTxid, SpendScript and SpendWholeTx; InputIndex,
// UnlockingScript and SequenceNumber under SpendScript; SpendingAtomicBEEF
// under SpendWholeTx.
type OutputSpent struct {
	Mode               SpendNotificationMode
	Outpoint           transaction.Outpoint
	Topic              string
	SpendingTxid       *chainhash.Hash
	InputIndex         uint32
	UnlockingScript    *script.Script
	SequenceNumber     uint32
	SpendingAtomicBEEF []byte
	OffChainValues     []byte
}

// LookupService indexes admitted outputs and answers questions about them.
type LookupService interface {
	// AdmissionMode is the shape OutputAdmittedByTopic arrives in.
	AdmissionMode() AdmissionMode
	// SpendNotificationMode is how much OutputSpent carries. A service
	// that answers SpendNone need not be a SpendListener.
	SpendNotificationMode() SpendNotificationMode
	// OutputAdmittedByTopic indexes one admitted output.
	OutputAdmittedByTopic(ctx context.Context, payload *OutputAdmittedByTopic) error
	// OutputEvicted removes an output from every index; it must never be
	// answered again.
	OutputEvicted(ctx context.Context, outpoint *transaction.Outpoint) error
	// Lookup answers one question.
	Lookup(ctx context.Context, question *lookup.LookupQuestion) (*lookup.LookupAnswer, error)
	// GetDocumentation is the service's documentation, Markdown.
	GetDocumentation() string
	// GetMetaData names the service.
	GetMetaData() *overlay.MetaData
}

// SpendListener is a LookupService told of spends (TypeScript outputSpent).
type SpendListener interface {
	OutputSpent(ctx context.Context, payload *OutputSpent) error
}

// RetentionListener is a LookupService told when a topic no longer retains
// an output's history (TypeScript outputNoLongerRetainedInHistory).
type RetentionListener interface {
	OutputNoLongerRetainedInHistory(ctx context.Context, outpoint *transaction.Outpoint, topic string) error
}

// Output is one row of a host's outputs table, as the host hands it to a
// restore. Beef is nil when the host did not include it; BlockHeight is
// zero while the output's transaction is unmined.
type Output struct {
	Outpoint        transaction.Outpoint
	OutputScript    *script.Script
	Satoshis        uint64
	Topic           string
	Spent           bool
	OutputsConsumed []*transaction.Outpoint
	ConsumedBy      []*transaction.Outpoint
	Beef            []byte
	BlockHeight     uint32
	Score           float64
}

// RestoreStorage is the one storage call a restore may make: the host's own
// lookup of one output, spent or not. It is what lets a lookup service read
// an output that is not among the unspent rows it was handed: a spent
// funding output, with Spent and ConsumedBy saying who spent it. An empty
// topic means any topic. An output the host does not hold is (nil, nil).
type RestoreStorage interface {
	FindOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) (*Output, error)
}

// Restorer is a LookupService that rebuilds its index from the host's
// unspent rows at startup (TypeScript restore). It answers how many it
// indexed.
type Restorer interface {
	Restore(ctx context.Context, outputs []Output, storage RestoreStorage) (int, error)
}

// Metrics is a module's view of the host's metrics. Names are the module's
// own, under its own prefix; the host owns the registry.
type Metrics interface {
	// Inc adds by to a counter, creating it on first use.
	Inc(name string, labels map[string]string, by float64)
	// Preset creates a counter series at zero, so an alert over it finds a
	// series before the first event.
	Preset(name string, labels map[string]string)
	// Gauge registers a gauge read from fn at each scrape.
	Gauge(name string, fn func() float64)
}

// Host is what a module is built against: the host's log and metrics. A
// module keeps no logger or registry of its own.
type Host interface {
	Log(msg string, extra map[string]any)
	Metrics() Metrics
}

// Module is what a factory returns: topic managers keyed by topic name and
// lookup services keyed by service name. Either map may be empty.
type Module struct {
	Topics  map[string]TopicManager
	Lookups map[string]LookupService
}

// Factory builds a module for one host. It is the TypeScript module's
// default export.
type Factory func(h Host) (Module, error)

// HydratedOutput is one output of an answer a lookup service hydrated
// itself: its BEEF (Atomic BEEF), its index, and the context the class
// gives it (nil when none). A service that must put a context on an output
// (the TypeScript engine carries a formula's context to the output; go-sdk's
// OutputListItem has no such member) answers a LookupAnswer of type
// output-list with Outputs filled as usual and Result set to a
// []HydratedOutput; a host that serves the answer over HTTP renders Result
// when it is one, and Outputs otherwise. An engine passes an output-list
// answer through untouched, so the convention costs it nothing.
type HydratedOutput struct {
	Beef        []byte
	OutputIndex uint32
	Context     []byte
}
