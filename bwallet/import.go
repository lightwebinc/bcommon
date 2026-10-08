package bwallet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/nodeapi"
)

// Funding a wallet on mainnet or testnet: the user pays the fund address
// from a wallet of their own, and the application imports that payment
// once. ImportBEEF takes the payment as the BEEF the user's wallet hands
// over (a BRC-100 createAction answers one), which needs no lookup at all;
// ImportTxid takes its txid and fetches it with its proof from a chain view
// (a node, WhatsOnChain). Either way the payment is checked, never trusted:
// its proofs against the caller's headers, and an unmined payment's
// scripts against its proven parents.

// ImportOptions are an import's choices.
type ImportOptions struct {
	// RefuseUnmined refuses a payment that has not mined yet. The zero
	// value accepts one whose every parent is proven: its outputs go into
	// the pool held back (Output.Unproven) until its proof is collected
	// (producer.Collector with Pool set, which asks the chain view by txid),
	// and the BEEF to keep for it meanwhile is Import.BeefHex.
	RefuseUnmined bool
	// Bound bounds the BEEF in bytes; zero is guard.DefaultBound.
	Bound int
}

// Import is a funding payment checked and read for the pool.
type Import struct {
	// Tx is the payment, with its proof when mined, its proven parents when
	// not.
	Tx   *transaction.Transaction
	Txid string
	// Outputs are the payment's outputs that pay the fund script, ready for
	// Pool.Add; Sats is their total.
	Outputs []Output
	Sats    uint64
	// Mined reports a payment with a proof the headers hold, at Height.
	Mined  bool
	Height uint32
	// BeefHex is the BEEF to keep for an unmined payment
	// (funding.KeepBEEF), from which a kept-transaction loader rebuilds it
	// (funding.Rebuild); empty once mined.
	BeefHex string
}

var (
	// ErrUnmined is a payment that has not mined, where only a mined one
	// is taken (ImportOptions.RefuseUnmined, ImportTxid).
	ErrUnmined = errors.New("bwallet: the payment has not mined yet")
	// ErrPaysNothing is a payment with no output to the fund script.
	ErrPaysNothing = errors.New("bwallet: the payment pays nothing to this wallet's fund address")
	// ErrUnprovenParent is an unmined payment one of whose parents carries
	// no proof: only one unmined hop is taken.
	ErrUnprovenParent = errors.New("bwallet: the payment spends a parent with no proof")
	// ErrSpent is a payment every output of which to the fund script the
	// chain view shows spent already (ImportTxid).
	ErrSpent = errors.New("bwallet: the payment's outputs to this wallet are spent already")
)

// ImportBEEF checks a funding payment handed over as BEEF (or Atomic BEEF)
// and reads the outputs that pay fund.
//
// A mined payment's proof must verify against headers. An unmined one is
// accepted unless opt.RefuseUnmined, and then must be final, carry every
// parent with a proof the headers hold, and verify (its scripts against
// those parents); its outputs are marked Unproven. headers is required.
func ImportBEEF(ctx context.Context, beef []byte, fund *script.Script, headers chaintracker.ChainTracker, opt ImportOptions) (*Import, error) {
	if headers == nil {
		return nil, errors.New("bwallet: import: no headers to check the payment against")
	}
	bound := opt.Bound
	if bound <= 0 {
		bound = guard.DefaultBound
	}
	_, tx, _, err := guard.ParseBEEF(beef, bound)
	if err != nil {
		return nil, fmt.Errorf("bwallet: import: %w", err)
	}
	if tx == nil {
		return nil, errors.New("bwallet: import: the BEEF names no payment")
	}
	txid := tx.TxID().String()
	if tx.MerklePath != nil {
		if err := nodeapi.CheckProof(ctx, tx.MerklePath, txid, headers); err != nil {
			return nil, fmt.Errorf("bwallet: import %s: %w", txid, err)
		}
		return read(tx, fund, true)
	}
	if opt.RefuseUnmined {
		return nil, fmt.Errorf("%w: %s", ErrUnmined, txid)
	}
	if !final(tx) {
		return nil, fmt.Errorf("bwallet: import %s: not final, so it cannot mine as it stands", txid)
	}
	for i, in := range tx.Inputs {
		parent := in.SourceTransaction
		if parent == nil || parent.MerklePath == nil {
			return nil, fmt.Errorf("%w: input %d of %s", ErrUnprovenParent, i, txid)
		}
		if err := nodeapi.CheckProof(ctx, parent.MerklePath, parent.TxID().String(), headers); err != nil {
			return nil, fmt.Errorf("bwallet: import %s: input %d: %w", txid, i, err)
		}
	}
	if ok, err := spv.Verify(ctx, tx, headers, nil); err != nil || !ok {
		return nil, fmt.Errorf("bwallet: import %s: does not verify against its parents: %v", txid, err)
	}
	im, err := read(tx, fund, false)
	if err != nil {
		return nil, err
	}
	if im.BeefHex, err = funding.KeepBEEF(tx, nil); err != nil {
		return nil, fmt.Errorf("bwallet: import %s: %w", txid, err)
	}
	return im, nil
}

// ImportTxid fetches a mined funding payment by txid from a chain view (a
// node, WhatsOnChain), checks its proof against headers, and reads the
// outputs that pay fund. A payment not mined yet is ErrUnmined: try again
// once it has a block, or import the wallet's BEEF instead.
//
// When src is also a nodeapi.SpendSource (a node, WhatsOnChain, the
// Sources nodeapi.ParseChain builds), each output paying fund is checked
// for a spend first: one shown spent is left out, and when every one is,
// the import is ErrSpent. An output the view cannot answer for
// (nodeapi.ErrSpendUnknown) fails the import. "Unspent" is the view's
// word and is trusted: no proof of absence exists.
func ImportTxid(ctx context.Context, txid string, fund *script.Script, src interface {
	nodeapi.TxSource
	nodeapi.ProofSource
}, headers chaintracker.ChainTracker) (*Import, error) {
	if headers == nil || src == nil {
		return nil, errors.New("bwallet: import: needs a chain view and headers")
	}
	txid = strings.ToLower(strings.TrimSpace(txid))
	raw, err := src.TxRaw(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("bwallet: import %s: %w", txid, err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return nil, fmt.Errorf("bwallet: import %s: %w", txid, err)
	}
	if got := tx.TxID().String(); got != txid {
		return nil, fmt.Errorf("bwallet: import %s: the source answered transaction %s", txid, got)
	}
	mp, _, err := src.Proof(ctx, txid)
	if errors.Is(err, nodeapi.ErrNotMined) {
		return nil, fmt.Errorf("%w: %s", ErrUnmined, txid)
	}
	if err != nil {
		return nil, fmt.Errorf("bwallet: import %s: its proof: %w", txid, err)
	}
	if err := nodeapi.CheckProof(ctx, mp, txid, headers); err != nil {
		return nil, fmt.Errorf("bwallet: import %s: %w", txid, err)
	}
	tx.MerklePath = mp
	im, err := read(tx, fund, true)
	if err != nil {
		return nil, err
	}
	if sp, ok := src.(nodeapi.SpendSource); ok {
		return unspent(ctx, im, sp)
	}
	return im, nil
}

// unspent leaves out of im the outputs sp shows spent.
func unspent(ctx context.Context, im *Import, sp nodeapi.SpendSource) (*Import, error) {
	kept := im.Outputs[:0]
	var sats uint64
	for _, o := range im.Outputs {
		by, err := sp.Spender(ctx, o.TxID, o.Vout)
		if err != nil {
			return nil, fmt.Errorf("bwallet: import %s: %w", im.Txid, err)
		}
		if by != "" {
			continue
		}
		kept = append(kept, o)
		sats += o.Satoshis
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSpent, im.Txid)
	}
	im.Outputs, im.Sats = kept, sats
	return im, nil
}

// final is a transaction that can mine as it stands: lock time zero, or
// every input's sequence final.
func final(tx *transaction.Transaction) bool {
	if tx.LockTime == 0 {
		return true
	}
	for _, in := range tx.Inputs {
		if in.SequenceNumber != 0xffffffff {
			return false
		}
	}
	return true
}

func read(tx *transaction.Transaction, fund *script.Script, mined bool) (*Import, error) {
	if fund == nil {
		return nil, errors.New("bwallet: import: no fund script")
	}
	txid := tx.TxID().String()
	im := &Import{Tx: tx, Txid: txid, Mined: mined}
	if mined {
		im.Height = tx.MerklePath.BlockHeight
	}
	for i, o := range tx.Outputs {
		if o.LockingScript == nil || !bytes.Equal(*o.LockingScript, *fund) || o.Satoshis == 0 {
			continue
		}
		out := Output{TxID: txid, Vout: uint32(i), Satoshis: o.Satoshis, //nolint:gosec // an output index
			LockingScript: o.LockingScript.String(), Raw: tx.Hex()}
		if mined {
			out.Height, out.Bump = im.Height, funding.BumpHex(tx.MerklePath)
		} else {
			out.Unproven = true
		}
		im.Outputs = append(im.Outputs, out)
		im.Sats += o.Satoshis
	}
	if len(im.Outputs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrPaysNothing, txid)
	}
	return im, nil
}
