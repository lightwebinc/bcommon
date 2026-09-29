// Package nodeapi talks to a Teranode node over HTTP: JSON-RPC for
// mining and direct submission, the asset API for reading what got mined.
//
// It is the funding and settlement side of a producer, and nothing a reader
// depends on: a reader verifies against a header source, never against a node
// it would have to trust.
//
// Every response body is bounded before it is parsed and every declared count
// in a binary proof is checked against the bytes present before the SDK is
// allowed to allocate for it. The node is trusted to be honest about the
// chain, which is not the same as trusting it to be well behaved about
// response size (the same rule the headers package states).
//
// The JSON shapes decoded here are the ones a Teranode node's RPC and asset
// API answer. The tests' response bodies are written to those shapes, not
// captured from a live node.
package nodeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxBody bounds any answer before it is parsed.
const maxBody = 1 << 20

// ErrBodyTooLarge refuses a response above the bound.
var ErrBodyTooLarge = errors.New("nodeapi: response body exceeds the bound")

// backoff429 is the retry ladder for a 429 from either API. The asset API
// rate-limits bulk reads (a rescan over a hundred blocks), and the ladder is
// short because a caller with a deadline should hit it, not this.
var backoff429 = []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second}

// ErrNoID refuses a call on an RPC with no request id. The library has no id
// of its own to fall back on, and any default would put one application's
// name on another's requests.
var ErrNoID = errors.New("nodeapi: rpc: no request id")

// ErrNotMined reports a transaction the node has not placed in a main-chain
// block yet, or a proof it cannot build yet. It is a state, not a failure:
// callers poll again after the next block.
var ErrNotMined = errors.New("nodeapi: not mined")

// HTTPError is a non-2xx answer, kept so callers can distinguish a 404 from a
// transport failure.
type HTTPError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: http %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsHTTP reports whether err is an HTTPError with the given status.
func IsHTTP(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

func trim(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

// readAtMost reads up to limit+1 bytes WITHOUT judging the length, so the
// caller can act on the status code first. Refusing an oversized body before
// the status is read turns every padded error page into a size error and
// throws away the distinction the status carries, which is exactly the
// distinction this package is careful about elsewhere.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit+1))
}

// overBound reports a body that exceeded the bound. It is called once the
// status has been handled. Refusing rather than truncating is the point: a
// truncated answer parses as the wrong thing, or fails to parse with an
// error that names the wrong problem.
func overBound(b []byte, limit int64) error {
	if int64(len(b)) > limit {
		return fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, limit)
	}
	return nil
}

// noProxyTransport is the transport behind the default client, and it takes no
// proxy. This leg carries the publisher's funding and its signed transactions,
// so HTTP_PROXY in the environment would hand them to a host that appears
// nowhere in the application's configuration or in its traces, and would let
// that host answer for the node about what was mined. resolve states the rule
// for discovery and hostset repeats it for host reads; a node is no place to
// relax it. A deployment that needs a proxy supplies its own client, where the
// choice is visible.
//
// One transport for the package, as http.DefaultTransport is, so the poll loop
// in WaitMined reuses one connection pool.
var noProxyTransport = &http.Transport{
	Proxy: nil, // see the doc comment
	// Setting TLSClientConfig or DialContext disables automatic HTTP/2 unless
	// this is set, and the http.DefaultTransport these replaced had it on.
	// Losing a protocol version as a side effect of pinning a proxy policy is
	// the kind of change nobody notices until a deployment is slower.
	ForceAttemptHTTP2:   true,
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	TLSHandshakeTimeout: 10 * time.Second,
	MaxIdleConns:        4,
	IdleConnTimeout:     30 * time.Second,
}

// clientOr returns the caller's client when there is one, so a test or a
// deployment can supply its own policy, and otherwise the default.
func clientOr(c *http.Client, d time.Duration) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Transport: noProxyTransport, Timeout: d}
}

// RPC is a Teranode JSON-RPC client.
type RPC struct {
	// URL is the RPC endpoint, e.g. http://node.example.com:21292/.
	URL  string
	User string
	Pass string
	// ID is the JSON-RPC request id, sent verbatim on every call. It is the
	// application's to choose and it is required: Call refuses an empty one
	// before any request is sent rather than choosing one itself.
	ID string
	// Client is optional; the default has a 60s timeout, which is long
	// because generatetoaddress mines inline. The lever for a slow node is
	// the batch size, not this.
	Client *http.Client
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call issues one JSON-RPC call and unmarshals the result into out, which
// may be nil. An RPC-level error is returned as the node phrased it. An RPC
// with no ID is refused with ErrNoID before anything is sent.
func (r *RPC) Call(ctx context.Context, method string, params []any, out any) error {
	if r.ID == "" {
		return ErrNoID
	}
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "1.0", ID: r.ID, Method: method, Params: params})
	if err != nil {
		return err
	}
	client := clientOr(r.Client, 60*time.Second)
	var raw []byte
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(r.User, r.Pass)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		raw, err = readAtMost(resp.Body, maxBody)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < len(backoff429) {
			if err := sleep(ctx, backoff429[attempt]); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return &HTTPError{Method: method, Path: r.URL, Status: resp.StatusCode, Body: trim(raw)}
		}
		// The size verdict comes last, so a padded 429 still retries and a
		// padded error page still reports its status. Asset.get holds the
		// same order for the same reason.
		if err := overBound(raw, maxBody); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		break
	}
	var res rpcResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("%s: decode: %w (body %s)", method, err, trim(raw))
	}
	if res.Error != nil {
		return fmt.Errorf("%s: rpc error %d: %s", method, res.Error.Code, res.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Result, out)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Info is the subset of getinfo used here. Teranode registers getblockcount
// but answers "Command unimplemented", so the height comes from here.
type Info struct {
	Blocks      int64 `json:"blocks"`
	Connections int64 `json:"connections"`
	Version     int64 `json:"version"`
}

// GetInfo reads the node's view of the tip.
func (r *RPC) GetInfo(ctx context.Context) (*Info, error) {
	var i Info
	if err := r.Call(ctx, "getinfo", nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// GenerateToAddress mines n blocks paying the coinbase to addr and returns
// the block hashes. Keep n small: a large call can outlast the RPC timeout,
// which is why FundFromCoinbase mines in batches.
func (r *RPC) GenerateToAddress(ctx context.Context, n int, addr string) ([]string, error) {
	var hashes []string
	if err := r.Call(ctx, "generatetoaddress", []any{n, addr}, &hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

// SendRawTransaction submits standard-format hex straight to the node and
// returns the txid it acknowledged. This is the settlement leg with an ack;
// the TCP ingress has none.
func (r *RPC) SendRawTransaction(ctx context.Context, rawHex string) (string, error) {
	var txid string
	if err := r.Call(ctx, "sendrawtransaction", []any{rawHex}, &txid); err != nil {
		return "", err
	}
	return txid, nil
}
