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

// ErrSpendUnknown is the node's UTXO view saying neither that an output is
// unspent nor who spent it. Every error Spender returns wraps it, so a
// caller that must not mistake "no answer" for "unspent" tests one error.
// An output the answer leaves out, a status other than OK or SPENT, a SPENT
// with no spender, an answer that does not decode, a transaction the node
// does not serve (a 404, which a node also answers for a fully spent
// transaction it has pruned) and a failed read are all this. The status
// NOT_FOUND is one of them: a node answers it for an output it holds no
// record of, which it has pruned or could not read, as well as for one it
// never stored.
var ErrSpendUnknown = errors.New("the node does not say whether the output is spent")

// Spender reads the node's UTXO view of txid (/api/v1/utxos/{txid}/json) and
// names the transaction that spent output vout, or "" with a nil error when
// the node shows the output unspent (status OK). Only those two answers are
// evidence. Anything else is an error wrapping ErrSpendUnknown, never "":
// see ErrSpendUnknown for what the node answers that is not evidence. A
// transaction the node does not serve is also an *HTTPError with status 404
// (IsHTTP).
//
// A caller that acts on "unspent", such as returning a coin to a pool,
// acts only on a nil error and "", and treats an error as undecided: it
// changes nothing and asks again later.
func (a *Asset) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	by, err := a.spender(ctx, txid, vout)
	if err != nil {
		return "", fmt.Errorf("%w: output %d of %s: %w", ErrSpendUnknown, vout, txid, err)
	}
	return by, nil
}

// The statuses of the node's UTXO view that are evidence. Every other
// status the node answers (NOT_FOUND, IMMATURE, FROZEN, CONFLICTING,
// LOCKED) and any it may add are not.
const (
	statusUnspent = "OK"
	statusSpent   = "SPENT"
)

func (a *Asset) spender(ctx context.Context, txid string, vout uint32) (string, error) {
	outs, err := getJSON[[]*struct {
		TxID     string  `json:"txid"`
		Vout     *uint32 `json:"vout"`
		Status   string  `json:"status"`
		Spending *struct {
			TxID string `json:"txId"`
		} `json:"spendingData"`
	}](ctx, a, "/api/v1/utxos/"+txid+"/json")
	if err != nil {
		return "", err
	}
	var found bool
	var by string
	for _, o := range *outs {
		if o == nil || o.Vout == nil {
			return "", errors.New("the answer has an entry with no output index")
		}
		if *o.Vout != vout {
			continue
		}
		if found {
			return "", errors.New("the answer names the output twice")
		}
		found = true
		if o.TxID != "" && !strings.EqualFold(o.TxID, txid) {
			return "", fmt.Errorf("the answer is for transaction %s", o.TxID)
		}
		switch {
		case strings.EqualFold(o.Status, statusUnspent):
			by = ""
		case strings.EqualFold(o.Status, statusSpent):
			if o.Spending == nil || o.Spending.TxID == "" {
				return "", errors.New("the node says it is spent and names no spender")
			}
			if !isTxid(o.Spending.TxID) {
				return "", fmt.Errorf("the node names the spender %q, which is not a txid", o.Spending.TxID)
			}
			by = o.Spending.TxID
		default:
			return "", fmt.Errorf("the node answers status %q", o.Status)
		}
	}
	if !found {
		return "", fmt.Errorf("the answer has no output %d", vout)
	}
	return by, nil
}

// isTxid is 64 hex digits.
func isTxid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// SpentElsewhere returns a *SpentError for the first input of tx the node
// shows spent by another transaction, or nil when there is none: then tx can
// still mine as far as its inputs go. An input the node cannot answer for
// (Spender's ErrSpendUnknown: a parent it does not serve, a status that is
// neither OK nor SPENT, a read that failed) is passed over, so this refuses
// only on the node's positive word. nil is therefore not evidence that tx's
// inputs are unspent; a caller that needs that asks Spender per input.
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
