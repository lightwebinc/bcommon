package producer

import (
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
)

// minimalProof is mp cut to tx's minimal path (chaintoken.Minimal), or mp
// as it came when it cannot be cut. A node's proof may flag the sibling
// leaf as a transaction id too; every proof a producer attaches reaches a
// host inside a BEEF, and a host holds each path to the minimal-path rule,
// so a carrier built on such a proof is refused everywhere (bstore
// providers refused every piece of a recovered funding tree, 2026-11-01).
func minimalProof(mp *transaction.MerklePath, tx *transaction.Transaction) *transaction.MerklePath {
	if mp == nil || tx == nil {
		return mp
	}
	min, err := chaintoken.Minimal(mp, *tx.TxID())
	if err != nil {
		return mp
	}
	return min
}
