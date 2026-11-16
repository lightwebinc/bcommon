package main

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
)

// overlaySPV is SPV as an overlay reader runs it, written here from go-sdk's
// parts so the generator does not rest on the library it checks: every proof
// reached is verified against the tracker, then the SDK checks the unmined
// transactions' scripts, amounts and shape. There is no finality rule. A
// carrier is not final by design, and go-sdk's spv.Verify refuses one.
func overlaySPV(ctx context.Context, tx *transaction.Transaction, tracker chaintracker.ChainTracker) (bool, error) {
	seen := map[*transaction.Transaction]bool{}
	var proofs func(t *transaction.Transaction) error
	proofs = func(t *transaction.Transaction) error {
		if t == nil || seen[t] {
			return nil
		}
		seen[t] = true
		if t.MerklePath != nil {
			ok, err := t.MerklePath.Verify(ctx, t.TxID(), tracker)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w for transaction %s", spv.ErrInvalidMerklePath, t.TxID())
			}
			return nil
		}
		for _, in := range t.Inputs {
			if err := proofs(in.SourceTransaction); err != nil {
				return err
			}
		}
		return nil
	}
	if err := proofs(tx); err != nil {
		return false, err
	}
	return spv.VerifyScripts(ctx, tx)
}
