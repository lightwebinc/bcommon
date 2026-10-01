package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// A broadcaster's verdict is not the node's view. Arcade has answered
// ACCEPTED_BY_NETWORK for a transaction whose input another transaction had
// already spent and mined; such a transaction never mines, and a caller
// waiting for its proof would wait out every timeout. The node's view of the
// outputs a transaction spends is the evidence that settles it: an input the
// node shows spent by some other transaction means this one cannot mine.

// ErrDoubleSpent is a transaction one of whose inputs the node shows spent by
// another transaction. It never mines. Every error this package and the
// packages built on it return for that case wraps it, so errors.Is finds it
// whatever the broadcaster said.
var ErrDoubleSpent = errors.New("an input is spent by another transaction")

// SpentError names the input of Txid that the node shows spent by By.
type SpentError struct {
	Txid string
	// Input is the index of the input in Txid; Outpoint is what it spends,
	// txid.vout.
	Input    int
	Outpoint string
	By       string
}

func (e *SpentError) Error() string {
	return fmt.Sprintf("%v: input %d of %s (%s) is spent by %s", ErrDoubleSpent, e.Input, e.Txid, e.Outpoint, e.By)
}

// Unwrap makes a *SpentError errors.Is ErrDoubleSpent.
func (e *SpentError) Unwrap() error { return ErrDoubleSpent }

// Spender reads the node's UTXO view of txid (/api/v1/utxos/{txid}/json) and
// names the transaction that spent output vout, or "" while it is unspent. A
// transaction the node does not know is an *HTTPError with status 404.
func (a *Asset) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	outs, err := getJSON[[]struct {
		Vout     uint32 `json:"vout"`
		Status   string `json:"status"`
		Spending *struct {
			TxID string `json:"txId"`
		} `json:"spendingData"`
	}](ctx, a, "/api/v1/utxos/"+txid+"/json")
	if err != nil {
		return "", err
	}
	for _, o := range *outs {
		if o.Vout != vout {
			continue
		}
		if !strings.EqualFold(o.Status, "SPENT") {
			return "", nil
		}
		if o.Spending == nil || o.Spending.TxID == "" {
			return "", fmt.Errorf("the node's UTXO view of %s says output %d is spent and names no spender", txid, vout)
		}
		return o.Spending.TxID, nil
	}
	return "", fmt.Errorf("the node's UTXO view of %s has no output %d", txid, vout)
}

// SpentElsewhere returns a *SpentError for the first input of tx the node
// shows spent by another transaction, or nil when there is none: then tx can
// still mine as far as its inputs go. An input the node cannot answer for (a
// parent it does not know, a read that failed) is passed over, so this
// refuses only on the node's positive word.
func (a *Asset) SpentElsewhere(ctx context.Context, tx *transaction.Transaction) error {
	if a == nil || tx == nil {
		return nil
	}
	txid := tx.TxID().String()
	for i, in := range tx.Inputs {
		if in.SourceTXID == nil {
			continue
		}
		src := in.SourceTXID.String()
		by, err := a.Spender(ctx, src, in.SourceTxOutIndex)
		if err == nil && by != "" && !strings.EqualFold(by, txid) {
			return &SpentError{Txid: txid, Input: i, Outpoint: fmt.Sprintf("%s.%d", src, in.SourceTxOutIndex), By: by}
		}
	}
	return nil
}

// WaitSettled is WaitMined for a transaction the caller holds: while tx has
// not mined, each poll also asks the node whether one of its inputs is spent
// by another transaction, and returns that *SpentError (errors.Is
// ErrDoubleSpent) at once rather than waiting until ctx ends.
func WaitSettled(ctx context.Context, asset *Asset, tx *transaction.Transaction, poll time.Duration) (*transaction.MerklePath, uint32, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	txid := tx.TxID().String()
	for {
		mp, height, err := tryMined(ctx, asset, txid)
		if err == nil {
			return mp, height, nil
		}
		if err != ErrNotMined {
			return nil, 0, err
		}
		if serr := asset.SpentElsewhere(ctx, tx); serr != nil {
			return nil, 0, serr
		}
		if err := sleep(ctx, poll); err != nil {
			return nil, 0, fmt.Errorf("waiting for %s to mine: %w", txid, err)
		}
	}
}
