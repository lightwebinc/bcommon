// Package heldpay is a BRC-105 client's side of a payment the server holds
// for confirmation. A server that takes a payment it cannot yet trust
// broadcasts it and answers 202 with the code ERR_PAYMENT_HELD; the same
// payment, sent again once it is mined, buys the answer. (Servers built
// before that answered 402 with the same code and no BRC-105 headers, which
// a BRC-105 client such as go-sdk's AuthFetch takes for a price it cannot
// read; Tap recognizes both.)
//
// A client wraps its HTTP transport in a Tap, makes its paid request, and
// when Tap.Held reports a hold, waits for the payment to mine (Mined) and
// sends the request again with Tap.Payment as its x-bsv-payment header,
// paying nothing more.
package heldpay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// Code is the error code of a held payment's answer.
const Code = "ERR_PAYMENT_HELD"

// maxHeldBody bounds how much of a held answer is read to find its code.
const maxHeldBody = 4 << 10

// Tap is an http.RoundTripper over Next (nil is http.DefaultTransport) that
// keeps the x-bsv-payment header of the last request that sent one, and
// whether, and why, the server held a payment. Its methods are safe for
// concurrent use; a client makes one paid call at a time through it.
type Tap struct {
	Next http.RoundTripper

	mu      sync.Mutex
	payment string
	held    bool
	why     string
}

// RoundTrip sends req, noting a payment it carries and a held answer.
func (t *Tap) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	if v := req.Header.Get("x-bsv-payment"); v != "" {
		t.mu.Lock()
		t.payment = v
		t.mu.Unlock()
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	legacy := resp.StatusCode == http.StatusPaymentRequired && resp.Header.Get("x-bsv-payment-version") == ""
	if resp.StatusCode != http.StatusAccepted && !legacy {
		return resp, nil
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxHeldBody+1))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		return resp, nil
	}
	var e struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	}
	if json.Unmarshal(body, &e) == nil && e.Code == Code {
		t.mu.Lock()
		t.held, t.why = true, e.Description
		t.mu.Unlock()
	}
	return resp, nil
}

// Payment is the x-bsv-payment header last sent, "" when none was.
func (t *Tap) Payment() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.payment
}

// Held reports whether the server held a payment since the last Reset, and
// the reason it gave.
func (t *Tap) Held() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.why, t.held
}

// Reset forgets the hold seen, keeping the payment: what a client calls
// before it sends the held payment again.
func (t *Tap) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held, t.why = false, ""
}

// Mined waits, up to wait, for tx to be mined on chain, asking every poll
// (zero is nodeapi's default). An error is a payment not mined in time, or
// one the chain shows spent elsewhere.
func Mined(ctx context.Context, chain nodeapi.Chain, tx *transaction.Transaction, wait, poll time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	_, _, err := nodeapi.WaitSettledOn(wctx, chain, chain, tx, poll)
	return err
}
