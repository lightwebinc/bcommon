package publish

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// Arcade settles through an arcade installation's ARC-compatible API: the
// broadcaster a publisher reaches when it runs no node of its own.
//
// Unlike the bare EF ingress it answers. POST /tx validates policy
// synchronously and says so, and GET /tx/{txid} reports the network's verdict
// and, once mined, the merkle path. That answer is what lets a publisher stop
// waiting for a block: the network's acceptance is the evidence a transaction
// will mine, and the proof can be collected later.
//
// Only a transaction meant to be mined is ever given to a Settler. A carrier
// is non-final by construction and arcade would refuse it as such; nothing
// here ever sees one.
type Arcade struct {
	Base string
	// Key, when set, is sent as a bearer token; public arcade installations
	// generally require one.
	Key    string
	Client *http.Client
	// Verdict bounds how long Submit waits for the network to accept or
	// refuse a transaction arcade has taken. Zero means DefaultVerdict.
	Verdict time.Duration
	// Poll is the interval between status checks while waiting. Zero means
	// one second.
	Poll time.Duration
	// Note, when set, receives a line a caller may want to show: a verdict
	// that did not arrive inside the bound, or a backoff.
	Note func(format string, args ...any)
	// Asset, when set, is the node arcade's verdict is held to. Arcade has
	// answered ACCEPTED_BY_NETWORK for a transaction whose input was already
	// spent and mined; with Asset set, an input the node shows spent by
	// another transaction is a refusal (errors.Is nodeapi.ErrDoubleSpent)
	// whatever arcade says, and is checked while Submit waits too.
	Asset *nodeapi.Asset
	// Spends, when set, is that view in Asset's place: a spend view that
	// needs no node, such as WhatsOnChain (nodeapi.WoC), so that the
	// ACCEPTED-but-spent case stays refused without one.
	Spends nodeapi.SpendSource
}

// DefaultVerdict is long enough for a network to see a transaction in the
// ordinary case and short enough that a publisher never waits long for it.
const DefaultVerdict = 15 * time.Second

var _ Settler = (*Arcade)(nil)

// ErrArcadeUnknown is a txid arcade does not hold. A transaction broadcast
// through some other path is not arcade's to report on.
var ErrArcadeUnknown = errors.New("arcade: transaction not known")

// ArcadeStatus is arcade's answer for one transaction.
type ArcadeStatus struct {
	TxID         string   `json:"txid"`
	TxStatus     string   `json:"txStatus"`
	BlockHash    string   `json:"blockHash"`
	BlockHeight  uint32   `json:"blockHeight"`
	MerklePath   string   `json:"merklePath"`
	ExtraInfo    string   `json:"extraInfo"`
	CompetingTxs []string `json:"competingTxs"`
}

// Refused reports a terminal refusal: the network will not mine this
// transaction as it stands.
func (s *ArcadeStatus) Refused() bool {
	return s.TxStatus == "REJECTED" || s.TxStatus == "DOUBLE_SPEND_ATTEMPTED"
}

// Accepted reports that the network, not only arcade's policy check, has
// taken the transaction.
func (s *ArcadeStatus) Accepted() bool {
	switch s.TxStatus {
	case "ACCEPTED_BY_NETWORK", "SEEN_ON_NETWORK", "SEEN_MULTIPLE_NODES", "STUMP_PROCESSING", "MINED", "IMMUTABLE":
		return true
	}
	return false
}

// Mined reports a transaction arcade can prove.
func (s *ArcadeStatus) Mined() bool {
	return (s.TxStatus == "MINED" || s.TxStatus == "IMMUTABLE") && s.MerklePath != ""
}

// Why is the refusal in words, with whatever arcade said about it.
func (s *ArcadeStatus) Why() string {
	w := s.TxStatus
	if s.ExtraInfo != "" {
		w += ": " + s.ExtraInfo
	}
	if len(s.CompetingTxs) > 0 {
		w += " (competing " + strings.Join(s.CompetingTxs, ", ") + ")"
	}
	return w
}

// Name identifies the leg in the journal.
func (a *Arcade) Name() string { return "arcade:" + a.Base }

func (a *Arcade) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	// The package's shared transport, which ignores HTTP_PROXY: a proxy
	// from the environment would put a third party between a publisher and
	// the broadcaster it named, for every transaction it settles.
	return &http.Client{Transport: noProxyTransport, Timeout: 30 * time.Second}
}

func (a *Arcade) note(format string, args ...any) {
	if a.Note != nil {
		a.Note(format, args...)
	}
}

// Submit gives arcade the transaction as EF and waits, briefly, for the
// network's verdict.
//
// A 202 means arcade's policy check passed; scripts and fees are the
// network's to judge, so RECEIVED alone is not yet acceptance. Submit waits
// up to Verdict for the network to accept or refuse. A refusal is an error,
// which keeps a transaction the network will not mine from being published
// to anyone. No verdict inside the bound is not an error: the transaction is
// in arcade's hands and its later status will say, and a publisher that
// waited for it would be back to waiting for a block.
//
// With Asset set, acceptance is not taken at arcade's word: an input the node
// shows spent by another transaction is the network's refusal, an error that
// wraps a *nodeapi.SpentError.
func (a *Arcade) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("publish: nil transaction")
	}
	ef, err := tx.EF()
	if err != nil {
		return fmt.Errorf("publish: ef: %w", err)
	}
	want := tx.TxID().String()
	st, err := a.post(ctx, ef)
	if err != nil {
		return err
	}
	if st.TxID != "" && st.TxID != want {
		return fmt.Errorf("publish: arcade acknowledged %s, we sent %s", st.TxID, want)
	}
	if st.Refused() {
		return fmt.Errorf("publish: arcade refused %s: %s", want, st.Why())
	}
	if st.Accepted() {
		return a.spentElsewhere(ctx, tx)
	}
	return a.awaitVerdict(ctx, tx)
}

// spentElsewhere is the node's word on tx's inputs, as a refusal, or nil
// with no Asset or when the node shows none spent by another transaction.
func (a *Arcade) spentElsewhere(ctx context.Context, tx *transaction.Transaction) error {
	var s nodeapi.SpendSource
	switch {
	case a.Spends != nil:
		s = a.Spends
	case a.Asset != nil:
		s = a.Asset
	default:
		return nil
	}
	if err := nodeapi.SpentElsewhereIn(ctx, s, tx); err != nil {
		return fmt.Errorf("publish: the network refused %s: %w", tx.TxID(), err)
	}
	return nil
}

// post is POST /tx, retrying the one answer arcade documents as safe to
// retry: 503 under backpressure, where the transaction was not queued.
func (a *Arcade) post(ctx context.Context, ef []byte) (*ArcadeStatus, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.Base, "/")+"/tx", bytes.NewReader(ef))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		a.auth(req)
		resp, err := a.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("publish: arcade: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
			var st ArcadeStatus
			if err := json.Unmarshal(body, &st); err != nil {
				return nil, fmt.Errorf("publish: arcade answered %d with a body that does not parse: %w", resp.StatusCode, err)
			}
			return &st, nil
		case resp.StatusCode == http.StatusServiceUnavailable && attempt < 3:
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 10 {
				wait = time.Duration(s) * time.Second
			}
			a.note("arcade is under backpressure; retrying in %s", wait)
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		default:
			return nil, fmt.Errorf("publish: arcade answered %d: %s", resp.StatusCode, reason(body))
		}
	}
}

// awaitVerdict polls the transaction's status until the network accepts or
// refuses it, or the bound passes. With Asset set, each poll also asks the
// node whether an input is spent by another transaction.
func (a *Arcade) awaitVerdict(ctx context.Context, tx *transaction.Transaction) error {
	txid := tx.TxID().String()
	bound, poll := a.Verdict, a.Poll
	if bound <= 0 {
		bound = DefaultVerdict
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(bound)
	last := "RECEIVED"
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
		st, err := a.Status(ctx, txid)
		if err != nil {
			// A status read that failed is not a verdict. Keep asking until
			// the bound, then say what is known.
			continue
		}
		last = st.TxStatus
		if st.Refused() {
			return fmt.Errorf("publish: the network refused %s: %s", txid, st.Why())
		}
		if err := a.spentElsewhere(ctx, tx); err != nil || st.Accepted() {
			return err
		}
	}
	a.note("arcade holds %s as %s; the network's verdict had not arrived after %s", txid, last, bound)
	return nil
}

// Status is GET /tx/{txid}.
func (a *Arcade) Status(ctx context.Context, txid string) (*ArcadeStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.Base, "/")+"/tx/"+txid, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	a.auth(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("arcade: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var st ArcadeStatus
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, fmt.Errorf("arcade: status for %s does not parse: %w", txid, err)
		}
		return &st, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrArcadeUnknown, txid)
	default:
		return nil, fmt.Errorf("arcade: status for %s answered %d: %s", txid, resp.StatusCode, reason(body))
	}
}

// Ping reads GET /policy, which answers without a transaction, to show the
// installation is reachable and speaks ARC.
func (a *Arcade) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.Base, "/")+"/policy", nil)
	if err != nil {
		return err
	}
	a.auth(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("policy answered %d: %s", resp.StatusCode, reason(body))
	}
	return nil
}

func (a *Arcade) auth(req *http.Request) {
	if a.Key != "" {
		req.Header.Set("Authorization", "Bearer "+a.Key)
	}
}

// reason pulls arcade's error text out of a failure body, or quotes the start
// of whatever came back.
func reason(body []byte) string {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil {
		for _, s := range []string{e.Reason, e.Detail, e.Error} {
			if s != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrArcadeRefused is a transaction arcade reports the network refused
// (REJECTED, DOUBLE_SPEND_ATTEMPTED). It never mines.
var ErrArcadeRefused = errors.New("arcade: the network refused the transaction")

var (
	_ nodeapi.ProofSource = (*Arcade)(nil)
	_ nodeapi.KnownSource = (*Arcade)(nil)
)

// Proof is arcade's proof of a transaction it was sent, as a
// nodeapi.ProofSource: the merkle path once MINED, held to the checks a
// node's proof is (it parses through the BUMP guard, names txid, and
// agrees with the height arcade reports). A transaction not mined yet, or
// one arcade does not hold (ErrArcadeUnknown: it was broadcast some other
// way), is nodeapi.ErrNotMined, so that a caller asks another source; one
// the network refused is ErrArcadeRefused. Check the proof against your
// headers (nodeapi.Checked).
func (a *Arcade) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	st, err := a.Status(ctx, txid)
	switch {
	case errors.Is(err, ErrArcadeUnknown):
		return nil, 0, fmt.Errorf("%w: %w", nodeapi.ErrNotMined, err)
	case err != nil:
		return nil, 0, err
	case st.Refused():
		return nil, 0, fmt.Errorf("%w: %s: %s", ErrArcadeRefused, txid, st.Why())
	case !st.Mined():
		return nil, 0, fmt.Errorf("%w: arcade holds %s as %s", nodeapi.ErrNotMined, txid, st.TxStatus)
	}
	raw, err := hex.DecodeString(st.MerklePath)
	if err != nil {
		return nil, 0, fmt.Errorf("arcade's proof for %s is not hex: %w", txid, err)
	}
	mp, err := nodeapi.ProofFor(raw, txid)
	if err != nil {
		return nil, 0, fmt.Errorf("arcade's proof for %s: %w", txid, err)
	}
	if st.BlockHeight != 0 && st.BlockHeight != mp.BlockHeight {
		return nil, 0, fmt.Errorf("arcade's proof for %s: proof height %d, reported height %d", txid, mp.BlockHeight, st.BlockHeight)
	}
	return mp, mp.BlockHeight, nil
}

// Known reports whether arcade holds txid, in any status: true for a
// transaction it was sent, false for ErrArcadeUnknown.
func (a *Arcade) Known(ctx context.Context, txid string) (bool, error) {
	_, err := a.Status(ctx, txid)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrArcadeUnknown):
		return false, nil
	}
	return false, err
}
