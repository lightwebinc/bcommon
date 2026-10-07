package nodeapi

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
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// WoC is the public WhatsOnChain API for one network, as a TxSource,
// ProofSource, SpendSource and KnownSource: a chain view that needs no
// node.
//
// What it answers is held to the same rules as a node's: a transaction
// must hash to the txid asked for, a proof must name it (and should be
// checked against the caller's headers, Checked), and a spender must be a
// txid. Its absence answers are its word: "unspent" is a 404 from the
// spent endpoint, which WhatsOnChain documents as "known but spent details
// are not found", and a 400, which it documents as an unknown output (and
// answers for an unspendable one), is ErrSpendUnknown, never unspent.
//
// The free tier allows 3 requests a second; Rate paces requests to it,
// and a 429 is retried with backoff. A key (Key) raises the limit on a
// paid plan; set Rate to match it.
//
// Safe for concurrent use.
type WoC struct {
	// Network is "main" or "test".
	Network string
	// Base overrides the API root (default
	// https://api.whatsonchain.com/v1/bsv/<Network>), for a mirror or a
	// test.
	Base string
	// Key, when set, is sent as the Authorization header, as WhatsOnChain
	// documents for an API key.
	Key string
	// Client is optional; the default has a 30 s timeout and takes no
	// proxy from the environment.
	Client *http.Client
	// Rate is requests a second, shared by every call on this value; zero
	// is WoCFreeRate.
	Rate float64
	// MaxTx bounds a transaction's size in bytes; zero is
	// guard.DefaultBound.
	MaxTx int

	mu   sync.Mutex
	next time.Time
}

// WoCFreeRate is WhatsOnChain's free tier: "Up to 3 requests/sec".
const WoCFreeRate = 3

// WoCBase is the WhatsOnChain API root, before the network.
const WoCBase = "https://api.whatsonchain.com/v1/bsv/"

var (
	_ Chain       = (*WoC)(nil)
	_ KnownSource = (*WoC)(nil)
)

// NewWoC returns a WhatsOnChain view of network, "main" or "test".
func NewWoC(network, key string) (*WoC, error) {
	if network != "main" && network != "test" {
		return nil, fmt.Errorf("nodeapi: WhatsOnChain serves main or test, not %q", network)
	}
	return &WoC{Network: network, Key: key}, nil
}

func (w *WoC) base() string {
	if w.Base != "" {
		return strings.TrimRight(w.Base, "/")
	}
	return WoCBase + w.Network
}

func (w *WoC) maxTx() int {
	if w.MaxTx > 0 {
		return w.MaxTx
	}
	return guard.DefaultBound
}

// wait paces a request to Rate: each takes the next slot, one interval
// after the last, and waits for it.
func (w *WoC) wait(ctx context.Context) error {
	rate := w.Rate
	if rate <= 0 {
		rate = WoCFreeRate
	}
	gap := time.Duration(float64(time.Second) / rate)
	w.mu.Lock()
	now := time.Now()
	slot := w.next
	if slot.Before(now) {
		slot = now
	}
	w.next = slot.Add(gap)
	w.mu.Unlock()
	if d := time.Until(slot); d > 0 {
		return sleep(ctx, d)
	}
	return nil
}

// get is one paced GET with the 429 ladder; it returns the status and up
// to limit bytes of the body, refusing a 200 body over the limit.
func (w *WoC) get(ctx context.Context, path string, limit int) (int, []byte, error) {
	client := clientOr(w.Client, 30*time.Second)
	for attempt := 0; ; attempt++ {
		if err := w.wait(ctx); err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base()+path, nil)
		if err != nil {
			return 0, nil, err
		}
		if w.Key != "" {
			req.Header.Set("Authorization", w.Key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
		resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < len(backoff429) {
			wait := backoff429[attempt]
			if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 && s <= 10 {
				wait = time.Duration(s) * time.Second
			}
			if err := sleep(ctx, wait); err != nil {
				return 0, nil, err
			}
			continue
		}
		if resp.StatusCode == http.StatusOK && len(body) > limit {
			return 0, nil, fmt.Errorf("GET %s: %w (%d bytes)", path, ErrBodyTooLarge, limit)
		}
		return resp.StatusCode, body, nil
	}
}

func (w *WoC) failed(path string, status int, body []byte) error {
	return &HTTPError{Method: http.MethodGet, Path: path, Status: status, Body: trim(body)}
}

func checkTxid(txid string) error {
	if !isTxid(txid) {
		return fmt.Errorf("nodeapi: %q is not a txid", txid)
	}
	return nil
}

// TxRaw is GET /tx/{txid}/hex: the raw transaction, which must hash to
// txid. A 404 is ErrTxNotFound.
func (w *WoC) TxRaw(ctx context.Context, txid string) ([]byte, error) {
	if err := checkTxid(txid); err != nil {
		return nil, err
	}
	path := "/tx/" + txid + "/hex"
	status, body, err := w.get(ctx, path, 2*w.maxTx()+2)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrTxNotFound, txid)
	default:
		return nil, w.failed(path, status, body)
	}
	raw, err := hex.DecodeString(string(bytes.TrimSpace(body)))
	if err != nil {
		return nil, fmt.Errorf("tx %s: not hex: %w", txid, err)
	}
	if err := sameTxWithin(raw, txid, w.maxTx()); err != nil {
		return nil, err
	}
	return raw, nil
}

func sameTxWithin(raw []byte, txid string, bound int) error {
	tx, err := guard.ParseTransaction(raw, bound)
	if err != nil {
		return fmt.Errorf("tx %s: %w", txid, err)
	}
	if got := tx.TxID().String(); !strings.EqualFold(got, txid) {
		return fmt.Errorf("asked for transaction %s, answered %s", txid, got)
	}
	return nil
}

// TxBEEF is GET /tx/{txid}/beef, which WhatsOnChain serves but does not
// document: the transaction as BEEF, with its proof when mined. A 422 (it
// declines some unmined transactions) is ErrNotMined; a 404, or a 500
// saying the transaction is unknown, is ErrTxNotFound. The bytes are
// returned as served; ParseBEEF them through guard before use.
func (w *WoC) TxBEEF(ctx context.Context, txid string) ([]byte, error) {
	if err := checkTxid(txid); err != nil {
		return nil, err
	}
	path := "/tx/" + txid + "/beef"
	status, body, err := w.get(ctx, path, 2*guard.DefaultBound+2)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("%w: %s: %s", ErrNotMined, txid, trim(body))
	case status == http.StatusNotFound,
		status == http.StatusInternalServerError && bytes.Contains(body, []byte("No such mempool or blockchain transaction")):
		return nil, fmt.Errorf("%w: %s", ErrTxNotFound, txid)
	default:
		return nil, w.failed(path, status, body)
	}
	b, err := hex.DecodeString(string(bytes.TrimSpace(body)))
	if err != nil {
		return nil, fmt.Errorf("beef %s: not hex: %w", txid, err)
	}
	return b, nil
}

// Proof is txid's proof and height, or ErrNotMined while WhatsOnChain does
// not hold it mined. It reads the BEEF first and falls back to the raw
// transaction's TSC proof (/tx/{txid}/proof/tsc) when the BEEF endpoint
// fails for any other reason, since that endpoint is undocumented. The
// proof names txid; check it against your headers (Checked).
func (w *WoC) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	mp, err := w.beefProof(ctx, txid)
	switch {
	case err == nil:
		return mp, mp.BlockHeight, nil
	case errors.Is(err, ErrNotMined), errors.Is(err, ErrTxNotFound):
		return nil, 0, fmt.Errorf("%w: %v", ErrNotMined, err)
	}
	mp, terr := w.tscProof(ctx, txid)
	if terr != nil {
		if errors.Is(terr, ErrNotMined) {
			return nil, 0, terr
		}
		return nil, 0, fmt.Errorf("WhatsOnChain proof of %s: beef: %v; tsc: %w", txid, err, terr)
	}
	return mp, mp.BlockHeight, nil
}

func (w *WoC) beefProof(ctx context.Context, txid string) (*transaction.MerklePath, error) {
	b, err := w.TxBEEF(ctx, txid)
	if err != nil {
		return nil, err
	}
	_, tx, _, err := guard.ParseBEEF(b, guard.DefaultBound)
	if err != nil {
		return nil, fmt.Errorf("beef %s: %w", txid, err)
	}
	if tx == nil {
		return nil, fmt.Errorf("beef %s: names no transaction", txid)
	}
	if got := tx.TxID().String(); !strings.EqualFold(got, txid) {
		return nil, fmt.Errorf("beef %s: is transaction %s", txid, got)
	}
	if tx.MerklePath == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotMined, txid)
	}
	if _, err := ProofFor(tx.MerklePath.Bytes(), txid); err != nil {
		return nil, fmt.Errorf("beef %s: %w", txid, err)
	}
	return tx.MerklePath, nil
}

// TSCProof is one entry of WhatsOnChain's /tx/{txid}/proof/tsc answer: a
// TSC merkle proof (the transaction's index in its block, the block hash as
// target, and the sibling at each level, "*" for a duplicate).
type TSCProof struct {
	Index  uint64   `json:"index"`
	TxOrID string   `json:"txOrId"`
	Target string   `json:"target"`
	Nodes  []string `json:"nodes"`
}

// maxTSCDepth bounds a TSC path: a block of 2^64 transactions has 64
// levels.
const maxTSCDepth = 64

// MerklePath converts a TSC proof of txid into a BRC-74 BUMP at height.
func (p *TSCProof) MerklePath(txid string, height uint32) (*transaction.MerklePath, error) {
	if !strings.EqualFold(p.TxOrID, txid) {
		return nil, fmt.Errorf("tsc proof is for %q, not %s", p.TxOrID, txid)
	}
	if len(p.Nodes) > maxTSCDepth {
		return nil, fmt.Errorf("tsc proof of %d levels", len(p.Nodes))
	}
	if len(p.Nodes) < 64 && p.Index>>len(p.Nodes) != 0 {
		return nil, fmt.Errorf("tsc proof index %d does not fit %d levels", p.Index, len(p.Nodes))
	}
	leaf, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, err
	}
	yes := true
	path := make([][]*transaction.PathElement, max(len(p.Nodes), 1))
	path[0] = []*transaction.PathElement{{Offset: p.Index, Hash: leaf, Txid: &yes}}
	for level, n := range p.Nodes {
		el := &transaction.PathElement{Offset: (p.Index >> level) ^ 1}
		if n == "*" {
			dup := true
			el.Duplicate = &dup
		} else {
			h, err := chainhash.NewHashFromHex(n)
			if err != nil {
				return nil, fmt.Errorf("tsc node %d: %w", level, err)
			}
			el.Hash = h
		}
		path[level] = append(path[level], el)
	}
	for _, lv := range path {
		if len(lv) == 2 && lv[1].Offset < lv[0].Offset {
			lv[0], lv[1] = lv[1], lv[0]
		}
	}
	return transaction.NewMerklePath(height, path), nil
}

func (w *WoC) tscProof(ctx context.Context, txid string) (*transaction.MerklePath, error) {
	if err := checkTxid(txid); err != nil {
		return nil, err
	}
	path := "/tx/" + txid + "/proof/tsc"
	status, body, err := w.get(ctx, path, maxBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, w.failed(path, status, body)
	}
	if string(bytes.TrimSpace(body)) == "null" {
		return nil, fmt.Errorf("%w: %s", ErrNotMined, txid)
	}
	var proofs []TSCProof
	if err := json.Unmarshal(body, &proofs); err != nil {
		var one TSCProof
		if json.Unmarshal(body, &one) != nil {
			return nil, fmt.Errorf("tsc %s: %w", txid, err)
		}
		proofs = []TSCProof{one}
	}
	for _, p := range proofs {
		if !strings.EqualFold(p.TxOrID, txid) {
			continue
		}
		if !isTxid(p.Target) {
			return nil, fmt.Errorf("tsc %s: target %q is not a block hash", txid, p.Target)
		}
		height, err := w.heightOf(ctx, p.Target)
		if err != nil {
			return nil, err
		}
		mp, err := p.MerklePath(txid, height)
		if err != nil {
			return nil, err
		}
		if _, err := ProofFor(mp.Bytes(), txid); err != nil {
			return nil, fmt.Errorf("tsc %s: %w", txid, err)
		}
		return mp, nil
	}
	return nil, fmt.Errorf("tsc %s: no proof names the transaction", txid)
}

// heightOf reads a block's height by hash (/block/{hash}/header). The
// height is WhatsOnChain's word until the proof built at it is checked
// against the caller's headers: a wrong height finds a root that does not
// match.
func (w *WoC) heightOf(ctx context.Context, hash string) (uint32, error) {
	path := "/block/" + hash + "/header"
	status, body, err := w.get(ctx, path, maxBody)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, w.failed(path, status, body)
	}
	var h Header
	if err := json.Unmarshal(body, &h); err != nil {
		return 0, fmt.Errorf("block %s: %w", hash, err)
	}
	if !strings.EqualFold(h.Hash, hash) {
		return 0, fmt.Errorf("asked for block %s, answered %s", hash, h.Hash)
	}
	return h.Height, nil
}

// Spender is GET /tx/{txid}/{vout}/spent. A 200 names the spender, mined
// or not; a 404 is unspent ("" and nil), which WhatsOnChain documents as an
// output it knows with no spend; a 400, its "UTXO is unknown" (also its
// answer for an unspendable output), and every other answer is an error
// wrapping ErrSpendUnknown.
func (w *WoC) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	by, err := w.spender(ctx, txid, vout)
	if err != nil {
		return "", fmt.Errorf("%w: output %d of %s: %w", ErrSpendUnknown, vout, txid, err)
	}
	return by, nil
}

func (w *WoC) spender(ctx context.Context, txid string, vout uint32) (string, error) {
	if err := checkTxid(txid); err != nil {
		return "", err
	}
	path := "/tx/" + txid + "/" + strconv.FormatUint(uint64(vout), 10) + "/spent"
	status, body, err := w.get(ctx, path, maxBody)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", nil
	case http.StatusBadRequest:
		return "", errors.New("WhatsOnChain does not know the output")
	default:
		return "", w.failed(path, status, body)
	}
	var s struct {
		TxID   string `json:"txid"`
		Vin    *int   `json:"vin"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", fmt.Errorf("the answer does not decode: %w", err)
	}
	if !isTxid(s.TxID) {
		return "", fmt.Errorf("the spender %q is not a txid", s.TxID)
	}
	return strings.ToLower(s.TxID), nil
}

// SpentElsewhere is SpentElsewhereIn over WhatsOnChain's spends.
func (w *WoC) SpentElsewhere(ctx context.Context, tx *transaction.Transaction) error {
	return SpentElsewhereIn(ctx, w, tx)
}

// Known is GET /tx/hash/{txid}: true for a transaction WhatsOnChain holds,
// mined or in its mempool, false for a 404.
func (w *WoC) Known(ctx context.Context, txid string) (bool, error) {
	if err := checkTxid(txid); err != nil {
		return false, err
	}
	path := "/tx/hash/" + txid
	status, body, err := w.get(ctx, path, 2*w.maxTx()+maxBody)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, w.failed(path, status, body)
	}
	var t struct {
		TxID string `json:"txid"`
	}
	if err := json.Unmarshal(body, &t); err != nil || !strings.EqualFold(t.TxID, txid) {
		return false, fmt.Errorf("tx/hash %s: the answer is not that transaction", txid)
	}
	return true, nil
}
