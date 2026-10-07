package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/guard"
)

// The narrow views of the chain an application reads, each a method a
// node's asset API (*Asset) answers and other backends can answer too:
// WhatsOnChain (*WoC) all of them, a broadcaster (publish.Arcade) proofs and
// knowledge of what it was sent. An application holds each view as the
// interface, so a backend is configuration, not code.
//
// Presence answers are checked, never trusted: a transaction must hash to
// the txid asked for, and a proof must name the txid and verify against
// the caller's own headers (Checked). Absence answers ("unspent", "not
// known", "not mined") cannot be checked and are the backend's word; only
// a backend that tells "unknown" from "unspent" may give them.

// TxSource returns a transaction's raw bytes. A transaction the source does
// not hold is an error for which IsNotFound is true.
type TxSource interface {
	TxRaw(ctx context.Context, txid string) ([]byte, error)
}

// ProofSource returns a mined transaction's proof and block height, or
// ErrNotMined while it has not mined (or the source does not know it).
type ProofSource interface {
	Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error)
}

// SpendSource names the transaction that spent an output, "" with a nil
// error only when the source shows it unspent, and an error wrapping
// ErrSpendUnknown for every other answer.
type SpendSource interface {
	Spender(ctx context.Context, txid string, vout uint32) (string, error)
}

// KnownSource reports whether the source holds a transaction at all, mined
// or not: true for "known", false with a nil error for "not known", an
// error when it cannot say. A transaction known and not mined is one to
// wait for; one not known after it was sent is one to send again.
type KnownSource interface {
	Known(ctx context.Context, txid string) (bool, error)
}

// Chain is the three views a producer needs to fund, prove and refuse.
type Chain interface {
	TxSource
	ProofSource
	SpendSource
}

var (
	_ Chain       = (*Asset)(nil)
	_ KnownSource = (*Asset)(nil)
)

// ErrTxNotFound is a transaction a source does not hold. *Asset answers it
// as an *HTTPError with status 404; IsNotFound reads both.
var ErrTxNotFound = errors.New("nodeapi: transaction not known")

// IsNotFound reports a TxSource's "not known": ErrTxNotFound, or a 404.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrTxNotFound) || IsHTTP(err, http.StatusNotFound)
}

// Known reports whether the node holds txid, mined or not
// (/api/v1/txmeta/{txid}/json answers it; a 404 is not known).
func (a *Asset) Known(ctx context.Context, txid string) (bool, error) {
	_, err := a.TxMeta(ctx, txid)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotMined):
		return false, nil
	}
	return false, err
}

// SpentElsewhereIn is Asset.SpentElsewhere over any SpendSource: a
// *SpentError for the first input of tx the source shows spent by another
// transaction, or nil. An input the source cannot answer for is passed
// over, so nil is not evidence that the inputs are unspent.
func SpentElsewhereIn(ctx context.Context, s SpendSource, tx *transaction.Transaction) error {
	if s == nil || tx == nil {
		return nil
	}
	txid := tx.TxID().String()
	for i, in := range tx.Inputs {
		if in.SourceTXID == nil {
			continue
		}
		src := in.SourceTXID.String()
		by, err := s.Spender(ctx, src, in.SourceTxOutIndex)
		if err == nil && by != "" && !strings.EqualFold(by, txid) {
			return &SpentError{Txid: txid, Input: i, Outpoint: fmt.Sprintf("%s.%d", src, in.SourceTxOutIndex), By: by}
		}
	}
	return nil
}

// WaitMinedOn is WaitMined over any ProofSource: it polls until txid's
// proof is served, or ctx ends.
func WaitMinedOn(ctx context.Context, p ProofSource, txid string, poll time.Duration) (*transaction.MerklePath, uint32, error) {
	return waitOn(ctx, p, nil, txid, nil, poll)
}

// WaitSettledOn is WaitSettled over any ProofSource and SpendSource: while
// tx has not mined, each poll also asks spends (when not nil) whether one
// of its inputs is spent by another transaction, and returns that
// *SpentError (errors.Is ErrDoubleSpent) at once.
func WaitSettledOn(ctx context.Context, p ProofSource, spends SpendSource, tx *transaction.Transaction, poll time.Duration) (*transaction.MerklePath, uint32, error) {
	if tx == nil {
		return nil, 0, errors.New("nodeapi: no transaction to wait on")
	}
	return waitOn(ctx, p, spends, tx.TxID().String(), tx, poll)
}

func waitOn(ctx context.Context, p ProofSource, spends SpendSource, txid string, tx *transaction.Transaction, poll time.Duration) (*transaction.MerklePath, uint32, error) {
	if p == nil {
		return nil, 0, errors.New("nodeapi: no proof source to wait on")
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		mp, height, err := p.Proof(ctx, txid)
		if err == nil {
			return mp, height, nil
		}
		if !errors.Is(err, ErrNotMined) {
			return nil, 0, err
		}
		if tx != nil && spends != nil {
			if serr := SpentElsewhereIn(ctx, spends, tx); serr != nil {
				return nil, 0, serr
			}
		}
		if err := sleep(ctx, poll); err != nil {
			return nil, 0, fmt.Errorf("waiting for %s to mine: %w", txid, err)
		}
	}
}

// ErrProofRefused is a proof that does not verify against the caller's
// headers. It is the source lying or on another chain, never "not yet".
var ErrProofRefused = errors.New("nodeapi: proof does not verify against the headers")

// Checked is a ProofSource whose every proof is verified against Headers
// before it is returned: it must name txid at its leaf level (ProofFor's
// rule) and its root must be the one Headers holds at its height. Wrap any
// third party's proofs in it; a node's own are checked the same way.
type Checked struct {
	Source  ProofSource
	Headers chaintracker.ChainTracker
}

// Proof is Source's proof once Headers confirm it, ErrNotMined as Source
// answers it, and an error wrapping ErrProofRefused for a proof Headers do
// not hold. A header source that cannot answer is an error, not a refusal.
func (c Checked) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	if c.Source == nil || c.Headers == nil {
		return nil, 0, errors.New("nodeapi: a checked proof source needs a source and headers")
	}
	mp, height, err := c.Source.Proof(ctx, txid)
	if err != nil {
		return nil, 0, err
	}
	if err := CheckProof(ctx, mp, txid, c.Headers); err != nil {
		return nil, 0, err
	}
	if height != mp.BlockHeight {
		return nil, 0, fmt.Errorf("%w: %s: answered height %d, proof height %d", ErrProofRefused, txid, height, mp.BlockHeight)
	}
	return mp, height, nil
}

// CheckProof verifies that mp proves txid against headers: txid at the
// leaf level, and the root it computes is the one headers hold at its
// height.
func CheckProof(ctx context.Context, mp *transaction.MerklePath, txid string, headers chaintracker.ChainTracker) error {
	if mp == nil {
		return fmt.Errorf("%w: %s: no proof", ErrProofRefused, txid)
	}
	if _, err := ProofFor(mp.Bytes(), txid); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrProofRefused, txid, err)
	}
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return err
	}
	ok, err := mp.Verify(ctx, h, headers)
	if err != nil {
		return fmt.Errorf("%s: checking its proof against the headers: %w", txid, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s at height %d", ErrProofRefused, txid, mp.BlockHeight)
	}
	return nil
}

// Sources is a Chain built from a backend per method, which is how an
// application picks them in configuration (ParseChain). Presence answers may
// come from any backend in a list, tried in order, since each is checked:
// a transaction must hash to its txid, and a proof should be wrapped in
// Checked by the caller who holds the headers. Absence answers come from
// the one Spends and the one Knows backend only.
type Sources struct {
	Tx     []TxSource
	Proofs []ProofSource
	Spends SpendSource
	Knows  KnownSource
}

var (
	_ Chain       = (*Sources)(nil)
	_ KnownSource = (*Sources)(nil)
)

// TxRaw asks each TxSource in order and returns the first answer whose
// bytes are the transaction txid names. Every source not holding it is
// IsNotFound; the error otherwise joins what each said.
func (s *Sources) TxRaw(ctx context.Context, txid string) ([]byte, error) {
	if len(s.Tx) == 0 {
		return nil, errors.New("nodeapi: no transaction source configured")
	}
	var errs []error
	notFound := true
	for _, src := range s.Tx {
		raw, err := src.TxRaw(ctx, txid)
		if err == nil {
			err = sameTx(raw, txid)
		}
		if err == nil {
			return raw, nil
		}
		notFound = notFound && IsNotFound(err)
		errs = append(errs, err)
	}
	if notFound {
		return nil, fmt.Errorf("%w: %s: %w", ErrTxNotFound, txid, errors.Join(errs...))
	}
	return nil, errors.Join(errs...)
}

// sameTx refuses raw bytes that are not the transaction txid.
func sameTx(raw []byte, txid string) error {
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return fmt.Errorf("tx %s: %w", txid, err)
	}
	if got := tx.TxID().String(); !strings.EqualFold(got, txid) {
		return fmt.Errorf("asked for transaction %s, answered %s", txid, got)
	}
	return nil
}

// Proof asks each ProofSource in order and returns the first proof. When
// every source answers ErrNotMined, so does Proof; a source that failed
// otherwise does not stop the next from being asked.
func (s *Sources) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	if len(s.Proofs) == 0 {
		return nil, 0, ErrNotMined
	}
	var errs []error
	for _, src := range s.Proofs {
		mp, h, err := src.Proof(ctx, txid)
		if err == nil {
			return mp, h, nil
		}
		if !errors.Is(err, ErrNotMined) {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil, 0, ErrNotMined
	}
	return nil, 0, errors.Join(errs...)
}

// Spender is the Spends backend's answer; with none configured, every
// answer is ErrSpendUnknown.
func (s *Sources) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	if s.Spends == nil {
		return "", fmt.Errorf("%w: no spend source configured", ErrSpendUnknown)
	}
	return s.Spends.Spender(ctx, txid, vout)
}

// Known is the Knows backend's answer; with none configured, an error.
func (s *Sources) Known(ctx context.Context, txid string) (bool, error) {
	if s.Knows == nil {
		return false, errors.New("nodeapi: no source configured to say whether a transaction is known")
	}
	return s.Knows.Known(ctx, txid)
}

// SpentElsewhere is SpentElsewhereIn over the Spends backend.
func (s *Sources) SpentElsewhere(ctx context.Context, tx *transaction.Transaction) error {
	if s.Spends == nil {
		return nil
	}
	return SpentElsewhereIn(ctx, s.Spends, tx)
}
