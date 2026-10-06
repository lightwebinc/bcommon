package guard

import (
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Limits bounds what a walk admits beyond the bytes present: the bytes bound
// what the SDK can be asked to allocate, and Limits bound how much work a
// well-formed object can ask of whoever reads it after the parse (SPV over
// every transaction, a signature check per input, a script run per output).
// A zero field sets no limit beyond the bytes.
//
// Every walk of this package that takes no Limits uses DefaultLimits.
type Limits struct {
	// Transactions is the most transactions one BEEF carries, txid-only
	// entries included.
	Transactions int
	// BUMPs is the most BUMPs one BEEF carries.
	BUMPs int
	// Inputs and Outputs are the most of each one transaction has.
	Inputs  int
	Outputs int
	// Script is the longest unlocking or locking script, in bytes, and the
	// longest previous locking script an Extended Format input carries.
	Script int
	// BUMPBytes is the longest one BUMP's encoding, in bytes.
	BUMPBytes int
}

// DefaultLimits are the limits every walk without its own applies. Each is
// far above what any producer of this module writes (a funding tree has at
// most 1023 outputs, a sweep at most 1024 inputs, a carrier one input) and
// above what an ordinary payment's ancestry holds, and each is below what
// DefaultBound alone would admit.
func DefaultLimits() Limits {
	return Limits{
		Transactions: 100_000,
		BUMPs:        100_000,
		Inputs:       100_000,
		Outputs:      100_000,
		Script:       16 << 20,
		BUMPBytes:    8 << 20,
	}
}

// over reports whether n exceeds a limit, zero being none.
func over(n uint64, limit int) bool {
	return limit > 0 && n > uint64(limit)
}

// CheckBEEFWithin is CheckBEEF under l in place of DefaultLimits.
func CheckBEEFWithin(b []byte, bound int, l Limits) error {
	return checkBEEF(b, bound, l)
}

// ParseBEEFWithin is ParseBEEF under l in place of DefaultLimits.
func ParseBEEFWithin(b []byte, bound int, l Limits) (*transaction.Beef, *transaction.Transaction, *chainhash.Hash, error) {
	return parseBEEF(b, bound, l)
}

// ParseTransactionWithin is ParseTransaction under l in place of
// DefaultLimits.
func ParseTransactionWithin(b []byte, bound int, l Limits) (*transaction.Transaction, error) {
	return parseTransaction(b, bound, l)
}

// ParseBUMPWithin is ParseBUMP under l in place of DefaultLimits; of l only
// BUMPBytes applies.
func ParseBUMPWithin(b []byte, bound int, l Limits) (*transaction.MerklePath, error) {
	return parseBUMP(b, bound, l)
}

// limitErr words a count over its limit.
func limitErr(what string, n uint64, limit int) error {
	return fmt.Errorf("declares %d %s, the limit is %d", n, what, limit)
}
