package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// signedTx is a P2PKH spend of a parent held in memory, so EF and BEEF both
// encode. The key is throwaway; nothing here touches a wallet.
func signedTx(t *testing.T) *transaction.Transaction {
	t.Helper()
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	parent := transaction.NewTransaction()
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
	unlock, err := p2pkh.Unlock(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(parent, 0, unlock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

// recorder accepts connections and keeps everything each one wrote.
type recorder struct {
	ln    net.Listener
	mu    sync.Mutex
	conns [][]byte
	wg    sync.WaitGroup
}

func listen(t *testing.T) *recorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer c.Close()
				b, _ := io.ReadAll(c)
				r.mu.Lock()
				r.conns = append(r.conns, b)
				r.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return r
}

// seen waits for the client side to close, then returns what arrived.
func (r *recorder) seen() [][]byte {
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.conns))
	copy(out, r.conns)
	return out
}

// countingConn observes the client's writes and close.
type countingConn struct {
	net.Conn
	writes int
	closed bool
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.writes++
	return c.Conn.Write(b)
}

func (c *countingConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestTCPIngressWritesEFOnceThenCloses(t *testing.T) {
	rec := listen(t)
	tx := signedTx(t)
	ef, err := tx.EF()
	if err != nil {
		t.Fatal(err)
	}
	var cc *countingConn
	ingress := &TCPIngress{Addr: rec.ln.Addr().String()}
	ingress.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		cc = &countingConn{Conn: c}
		return cc, nil
	}
	if err := ingress.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if cc.writes != 1 {
		t.Fatalf("%d writes, want exactly one: the stream is self-delimiting", cc.writes)
	}
	if !cc.closed {
		t.Fatal("connection left open after Submit")
	}
	// Give the accept loop a moment to see EOF, then read.
	time.Sleep(10 * time.Millisecond)
	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("%d connections, want 1", len(got))
	}
	if !bytes.Equal(got[0], ef) {
		t.Fatalf("ingress received %d bytes, want the %d EF bytes", len(got[0]), len(ef))
	}
	// EF, not BEEF, not framed: version 1 then the six-byte BRC-30 marker
	// 0000000000EF.
	if !bytes.HasPrefix(got[0], []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0xEF}) {
		t.Fatalf("bytes do not start as an EF transaction: % x", got[0][:12])
	}
	// A second Submit is a second connection: no reuse.
	if err := ingress.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if n := len(rec.seen()); n != 2 {
		t.Fatalf("%d connections after two Submits, want 2", n)
	}
}

// failingConn fails part-way through a write.
type failingConn struct {
	net.Conn
	closed bool
}

func (c *failingConn) Write(b []byte) (int, error) { return 3, errors.New("peer went away") }
func (c *failingConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestTCPIngressClosesOnWriteError(t *testing.T) {
	rec := listen(t)
	var fc *failingConn
	ingress := &TCPIngress{Addr: rec.ln.Addr().String()}
	ingress.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		fc = &failingConn{Conn: c}
		return fc, nil
	}
	err := ingress.Submit(context.Background(), signedTx(t))
	if err == nil || !strings.Contains(err.Error(), "peer went away") || !strings.Contains(err.Error(), "write 3 of") {
		t.Fatalf("err %v, want the write error with the byte count", err)
	}
	if !fc.closed {
		t.Fatal("a failed write must close the connection: a partial transaction poisons the stream")
	}
}

func TestTCPIngressRefusesWhatItCannotEncode(t *testing.T) {
	dialed := false
	ingress := &TCPIngress{Addr: "127.0.0.1:1"}
	ingress.dial = func(context.Context, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not be reached")
	}
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{}) // no source output: EF impossible
	if err := ingress.Submit(context.Background(), tx); err == nil {
		t.Fatal("a transaction with no source outputs cannot be EF-encoded")
	}
	if dialed {
		t.Fatal("dialed before encoding: an unencodable transaction must not open a connection")
	}
	if err := ingress.Submit(context.Background(), nil); err == nil {
		t.Fatal("nil transaction must be refused")
	}
}

func TestTCPIngressDialFailureIsReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now
	err = (&TCPIngress{Addr: addr, DialTimeout: time.Second}).Submit(context.Background(), signedTx(t))
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err %v, want a dial error", err)
	}
}

func rpcStub(t *testing.T, reply func(method string, params []any) (any, *string)) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)
		result, rpcErr := reply(req.Method, req.Params)
		if rpcErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"code": -25, "message": *rpcErr}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestRPCSettlerSendsStandardHexAndPropagatesTheNodesError(t *testing.T) {
	tx := signedTx(t)
	msg := "TX rejected: bad-txns-inputs-missingorspent"
	srv, bodies := rpcStub(t, func(method string, params []any) (any, *string) {
		if method != "sendrawtransaction" {
			t.Errorf("method %q", method)
		}
		if params[0] != tx.Hex() {
			t.Errorf("param is not the standard hex")
		}
		return nil, &msg
	})
	s := &RPCSettler{RPC: &nodeapi.RPC{URL: srv.URL, ID: "app-under-test"}}
	err := s.Submit(context.Background(), tx)
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("err %v, want the node's message", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("%d requests", len(*bodies))
	}
	// The EF marker never appears in what the RPC leg sends.
	ef, _ := tx.EF()
	if strings.Contains((*bodies)[0], strings.ToLower(hexOf(ef[:12]))) {
		t.Fatal("the RPC leg sent EF bytes")
	}
	if !strings.HasPrefix(s.Name(), "rpc:") {
		t.Fatalf("name %q", s.Name())
	}
}

func TestRPCSettlerAcceptsAMatchingAck(t *testing.T) {
	tx := signedTx(t)
	srv, _ := rpcStub(t, func(string, []any) (any, *string) { return tx.TxID().String(), nil })
	if err := (&RPCSettler{RPC: &nodeapi.RPC{URL: srv.URL, ID: "app-under-test"}}).Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	other, _ := rpcStub(t, func(string, []any) (any, *string) { return strings.Repeat("00", 32), nil })
	err := (&RPCSettler{RPC: &nodeapi.RPC{URL: other.URL, ID: "app-under-test"}}).Submit(context.Background(), tx)
	if err == nil || !strings.Contains(err.Error(), "acknowledged") {
		t.Fatalf("a different txid in the ack must be an error, got %v", err)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&0xf])
	}
	return string(out)
}
