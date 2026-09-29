package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testID is the request id every client here is built with. It stands for
// whatever id an application chooses; the library has none of its own.
const testID = "app-under-test"

// rpcServer answers JSON-RPC the way a Teranode node does. Its bodies are
// written to the node's shapes, not captured from a node.
func rpcServer(t *testing.T, handle func(method string, params []any) (any, *string)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type %q", ct)
		}
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			ID      string `json:"id"`
			Method  string `json:"method"`
			Params  []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Params == nil {
			t.Error("params must be an array, never null")
		}
		if req.ID != testID {
			t.Errorf("request id %q, want the client's %q", req.ID, testID)
		}
		result, rpcErr := handle(req.Method, req.Params)
		w.Header().Set("content-type", "application/json")
		if rpcErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": nil,
				"error":  map[string]any{"code": -26, "message": *rpcErr},
				"id":     req.ID,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": req.ID})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestGetInfoGenerateAndSend(t *testing.T) {
	srv, _ := rpcServer(t, func(method string, params []any) (any, *string) {
		switch method {
		case "getinfo":
			return map[string]any{"blocks": 1234, "connections": 3, "version": 100}, nil
		case "generatetoaddress":
			n := int(params[0].(float64))
			if params[1] != "mhkGqU8gsK9V6xBf7fGN7AmmJ6R9WfHMdV" {
				t.Errorf("address param %v", params[1])
			}
			hashes := make([]string, n)
			for i := range hashes {
				hashes[i] = strings.Repeat("ab", 32)
			}
			return hashes, nil
		case "sendrawtransaction":
			if params[0] != "0100000000" {
				t.Errorf("raw param %v", params[0])
			}
			return strings.Repeat("cd", 32), nil
		}
		msg := "unknown method"
		return nil, &msg
	})
	rpc := &RPC{URL: srv.URL, User: "alice", Pass: "s3cret", ID: testID}
	ctx := context.Background()

	info, err := rpc.GetInfo(ctx)
	if err != nil || info.Blocks != 1234 {
		t.Fatalf("getinfo: %+v %v", info, err)
	}
	hashes, err := rpc.GenerateToAddress(ctx, 3, "mhkGqU8gsK9V6xBf7fGN7AmmJ6R9WfHMdV")
	if err != nil || len(hashes) != 3 {
		t.Fatalf("generatetoaddress: %v %v", hashes, err)
	}
	txid, err := rpc.SendRawTransaction(ctx, "0100000000")
	if err != nil || txid != strings.Repeat("cd", 32) {
		t.Fatalf("sendrawtransaction: %q %v", txid, err)
	}
}

func TestRPCErrorIsReturnedAsTheNodePhrasedIt(t *testing.T) {
	srv, _ := rpcServer(t, func(string, []any) (any, *string) {
		msg := "TX rejected: missing inputs"
		return nil, &msg
	})
	rpc := &RPC{URL: srv.URL, User: "alice", Pass: "s3cret", ID: testID}
	_, err := rpc.SendRawTransaction(context.Background(), "00")
	if err == nil || !strings.Contains(err.Error(), "missing inputs") || !strings.Contains(err.Error(), "-26") {
		t.Fatalf("err %v, want the node's code and message", err)
	}
}

func TestBadAuthIsAnHTTPError(t *testing.T) {
	srv, _ := rpcServer(t, func(string, []any) (any, *string) { return nil, nil })
	rpc := &RPC{URL: srv.URL, User: "alice", Pass: "wrong", ID: testID}
	err := rpc.Call(context.Background(), "getinfo", nil, nil)
	if !IsHTTP(err, http.StatusUnauthorized) {
		t.Fatalf("err %v, want http 401", err)
	}
}

func Test429IsRetriedOnce(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"result":{"blocks":7},"error":null,"id":"app-under-test"}`))
	}))
	t.Cleanup(srv.Close)
	rpc := &RPC{URL: srv.URL, ID: testID}
	info, err := rpc.GetInfo(context.Background())
	if err != nil || info.Blocks != 7 {
		t.Fatalf("after a 429: %+v %v", info, err)
	}
	if hits != 2 {
		t.Fatalf("%d requests, want 2", hits)
	}
}

func Test429BackoffHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&RPC{URL: srv.URL, ID: testID}).Call(ctx, "getinfo", nil, nil)
	if err == nil {
		t.Fatal("a cancelled context must end the backoff")
	}
}

// A body one byte over the bound is refused rather than truncated. A clipped
// JSON-RPC answer fails to parse, and the decode error then blames the node's
// JSON for a cut this client made.
func TestRPCBodyOverTheBoundIsRefusedNotTruncated(t *testing.T) {
	reply := func(size int) []byte {
		head, tail := []byte(`{"result":{"blocks":7},"error":null,"id":"app-under-test","pad":"`), []byte(`"}`)
		pad := size - len(head) - len(tail)
		if pad < 0 {
			t.Fatalf("an rpc answer cannot be shaped to %d bytes", size)
		}
		return append(append(head, bytes.Repeat([]byte("a"), pad)...), tail...)
	}
	serveBody := func(b []byte) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(b)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	info, err := (&RPC{URL: serveBody(reply(maxBody)).URL, ID: testID}).GetInfo(context.Background())
	if err != nil {
		t.Fatalf("a body of exactly %d bytes was refused: %v", maxBody, err)
	}
	if info.Blocks != 7 {
		t.Fatalf("blocks = %d, want 7", info.Blocks)
	}

	_, err = (&RPC{URL: serveBody(reply(maxBody + 1)).URL, ID: testID}).GetInfo(context.Background())
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("one byte over the bound: %v, want ErrBodyTooLarge", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxBody)) {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}
}

// The default client for both node legs takes no proxy: they carry the
// publisher's funding and its signed transactions, and a host named only by
// HTTP_PROXY appears in no configuration. A caller may still supply its own.
func TestDefaultNodeClientTakesNoProxyAndTheCallersClientWins(t *testing.T) {
	tr, ok := clientOr(nil, time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatal("the default client has no transport of its own, so it follows HTTP_PROXY from the environment")
	}
	if tr.Proxy != nil {
		t.Fatal("the default transport asks a proxy function where to send a node call")
	}
	mine := &http.Client{Timeout: time.Second}
	if got := clientOr(mine, time.Second); got != mine {
		t.Fatal("a caller-supplied client was replaced")
	}
}

// The id is the caller's and reaches the node exactly as given, whatever it
// is: the library neither defaults, trims nor rewrites it.
func TestRPCSendsTheCallersIDVerbatim(t *testing.T) {
	for _, id := range []string{testID, " spaced id ", `quoted "id"`, "\u00e9t\u00e9"} {
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
			}
			var s string
			if err := json.Unmarshal(req["id"], &s); err != nil {
				t.Errorf("id %s is not a JSON string: %v", req["id"], err)
			}
			got = append(got, s)
			_, _ = w.Write([]byte(`{"result":null,"error":null,"id":"not-the-request-id"}`))
		}))
		err := (&RPC{URL: srv.URL, ID: id}).Call(context.Background(), "getinfo", nil, nil)
		srv.Close()
		if err != nil {
			t.Fatalf("id %q: %v", id, err)
		}
		if len(got) != 1 || got[0] != id {
			t.Errorf("sent ids %q, want exactly [%q]", got, id)
		}
	}
}

// An RPC with no id is refused before anything is sent: a node never sees a
// request whose id the library would have had to make up.
func TestRPCRefusesAnEmptyIDBeforeSending(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"result":{"blocks":7},"error":null}`))
	}))
	t.Cleanup(srv.Close)
	rpc := &RPC{URL: srv.URL, User: "alice", Pass: "s3cret"}
	ctx := context.Background()
	calls := map[string]func() error{
		"Call":               func() error { return rpc.Call(ctx, "getinfo", nil, nil) },
		"GetInfo":            func() error { _, err := rpc.GetInfo(ctx); return err },
		"GenerateToAddress":  func() error { _, err := rpc.GenerateToAddress(ctx, 1, "addr"); return err },
		"SendRawTransaction": func() error { _, err := rpc.SendRawTransaction(ctx, "00"); return err },
	}
	for name, call := range calls {
		err := call()
		if !errors.Is(err, ErrNoID) {
			t.Errorf("%s: err %v, want ErrNoID", name, err)
		}
		if err != nil && err.Error() != "nodeapi: rpc: no request id" {
			t.Errorf("%s: text %q", name, err.Error())
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("%d requests reached the node with no id", n)
	}
}
