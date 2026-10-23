package termsroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/auth"
	clients "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/acceptance"
	"github.com/lightwebinc/bcommon/bwallet"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/payee"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
)

// fakeHost records what the route counts and logs.
type fakeHost struct {
	mu       sync.Mutex
	counters map[string]float64
	lines    []string
}

func newFakeHost() *fakeHost { return &fakeHost{counters: map[string]float64{}} }

func key(name string, labels map[string]string) string {
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func (h *fakeHost) Log(msg string, extra map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, msg)
}
func (h *fakeHost) Metrics() bmodule.Metrics { return h }
func (h *fakeHost) Inc(name string, labels map[string]string, by float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[key(name, labels)] += by
}
func (h *fakeHost) Preset(name string, labels map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.counters[key(name, labels)]; !ok {
		h.counters[key(name, labels)] = 0
	}
}
func (h *fakeHost) Gauge(string, func() float64) {}
func (h *fakeHost) count(name string, labels map[string]string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counters[key(name, labels)]
}

var classes = []PricedClass{{"list", true}, {"history", false}, {"history-after", false}}

// answerer answers {type:"output-list", outputs:[], asked:<class>}.
type answerer struct{ restored bool }

func (a *answerer) Restored() bool { return a.restored }
func (a *answerer) Classify(_ context.Context, q Question) (string, error) {
	if q.Service != "ls_example" {
		return "", errors.New("not ls_example")
	}
	var m struct {
		Class string `json:"class"`
	}
	if err := json.Unmarshal(q.Query, &m); err != nil {
		return "", errors.New("no such question")
	}
	for _, c := range classes {
		if c.Name == m.Class {
			return m.Class, nil
		}
	}
	return "", errors.New("no such question")
}
func (a *answerer) Answer(ctx context.Context, q Question) (any, error) {
	cls, _ := a.Classify(ctx, q)
	caller, _ := Caller(ctx)
	return map[string]any{"type": "output-list", "outputs": []any{}, "asked": cls, "caller": caller}, nil
}

var payeeKey, _ = ec.PrivateKeyFromBytes(bytes.Repeat([]byte{7}, 32))
var clientKey, _ = ec.PrivateKeyFromBytes(bytes.Repeat([]byte{9}, 32))

type rig struct {
	t      *testing.T
	front  *Front
	srv    *httptest.Server
	host   *fakeHost
	ls     *answerer
	recv   *MemoryReceiver
	chain  *testchain.Chain
	node   *httptest.Server
	client *clientWallet
}

type rigOptions struct {
	prices    string
	budget    BudgetConfig
	sessions  SessionBound
	noNetwork bool
	now       func() time.Time
}

func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	chain := testchain.New(700)
	node := httptest.NewServer(chain)
	t.Cleanup(node.Close)
	host := newFakeHost()
	if o.prices == "" && !strings.Contains(o.prices, "=") {
		o.prices = "history=5,history-after=5"
	}
	if o.prices == "none" {
		o.prices = ""
	}
	prices, err := ParsePrices(o.prices, classes, "EXAMPLE_PRICES")
	if err != nil {
		t.Fatal(err)
	}
	ls := &answerer{restored: true}
	recv := &MemoryReceiver{}
	g := GateOptions{App: "example", Host: host, Headers: chain, Policy: acceptance.DefaultPolicy(), Now: o.now}
	if !o.noNetwork {
		g.Arcade = &publish.Arcade{Base: node.URL + "/arcade", Verdict: 2 * time.Second, Poll: 50 * time.Millisecond}
		g.Asset = &nodeapi.Asset{Base: node.URL}
		g.Arcade.Asset = g.Asset
	}
	g.Policy.Wait = 2 * time.Second
	g.Policy.Poll = 50 * time.Millisecond
	f, err := New(Options{App: "example", Service: "ls_example", Classes: classes, PaymentProtocol: bwallet.PaymentProtocol, LS: ls, Host: host,
		Prices: prices, Payee: payeeKey, Receiver: recv, Headers: chain, Gate: NewGate(g), Budget: o.budget, Sessions: o.sessions, Now: o.now})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &rig{t: t, front: f, srv: srv, host: host, ls: ls, recv: recv, chain: chain, node: node, client: newClientWallet(t, chain)}
}

func ask(class string) []byte {
	b, _ := json.Marshal(map[string]any{"service": "ls_example", "query": map[string]string{"class": class}})
	return b
}

func post(t *testing.T, url string, body []byte, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// clientWallet is a paying client: the key's ProtoWallet, and CreateAction
// paying from a mature coinbase of the chain.
type clientWallet struct {
	payeeWallet
	t     *testing.T
	chain *testchain.Chain
	key   *ec.PrivateKey
	mu    sync.Mutex
	spent map[string]bool
	// paid is every payment made, newest last.
	paid []*transaction.Transaction
	// fail refuses to pay.
	fail bool
}

func newClientWallet(t *testing.T, chain *testchain.Chain) *clientWallet {
	t.Helper()
	w, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: clientKey})
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := script.NewAddressFromPublicKey(clientKey.PubKey(), true)
	if _, err := chain.Generate(testchain.Maturity+3, addr.AddressString); err != nil {
		t.Fatal(err)
	}
	return &clientWallet{payeeWallet: payeeWallet{w}, t: t, chain: chain, key: clientKey, spent: map[string]bool{}}
}

// coin is a mature, unspent coinbase of the client's.
func (c *clientWallet) coin() *transaction.Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr, _ := script.NewAddressFromPublicKey(c.key.PubKey(), true)
	lock, _ := p2pkh.Lock(addr)
	tip := c.chain.Height()
	for _, id := range c.chain.Txids() {
		if c.spent[id] {
			continue
		}
		tx := c.chain.Tx(id)
		if tx == nil || len(tx.Inputs) != 1 || tx.Inputs[0].SourceTXID == nil || *tx.Inputs[0].SourceTXID != (chainhash.Hash{}) {
			continue
		}
		mp, h, ok := c.chain.Proof(id)
		if !ok || tip+1-h < testchain.Maturity || !bytes.Equal(*tx.Outputs[0].LockingScript, *lock) {
			continue
		}
		tx.MerklePath = mp
		c.spent[id] = true
		return tx
	}
	c.t.Fatal("no mature coin")
	return nil
}

// payment is a transaction paying sats to lock from a coin, signed.
func (c *clientWallet) payment(lock []byte, sats uint64) *transaction.Transaction {
	src := c.coin()
	tx := transaction.NewTransaction()
	unlock, _ := p2pkh.Unlock(c.key, nil)
	if err := tx.AddInputFrom(src.TxID().String(), 0, src.Outputs[0].LockingScript.String(), src.Outputs[0].Satoshis, unlock); err != nil {
		c.t.Fatal(err)
	}
	tx.Inputs[0].SourceTransaction = src
	ls := script.Script(lock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: &ls})
	addr, _ := script.NewAddressFromPublicKey(c.key.PubKey(), true)
	change, _ := p2pkh.Lock(addr)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: src.Outputs[0].Satoshis - sats - 500, LockingScript: change})
	if err := tx.Sign(); err != nil {
		c.t.Fatal(err)
	}
	return tx
}

func (c *clientWallet) CreateAction(_ context.Context, args wallet.CreateActionArgs, _ string) (*wallet.CreateActionResult, error) {
	if c.fail {
		return nil, errors.New("the wallet pays nothing")
	}
	tx := c.payment(args.Outputs[0].LockingScript, args.Outputs[0].Satoshis)
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.paid = append(c.paid, tx)
	c.mu.Unlock()
	return &wallet.CreateActionResult{Txid: *tx.TxID(), Tx: beef}, nil
}

func TestPricesAndTerms(t *testing.T) {
	p, err := ParsePrices("history-after=7, history=5", classes, "EXAMPLE_PRICES")
	if err != nil || p["history"] != 5 || p["history-after"] != 7 || len(p) != 2 {
		t.Fatalf("%v %v", p, err)
	}
	for bad, want := range map[string]string{
		"list=1":                    "EXAMPLE_PRICES: list is a free class and never has a price",
		"history":                   `EXAMPLE_PRICES: "history" is not <class>=<satoshis>`,
		"nosuch=1":                  `EXAMPLE_PRICES: "nosuch=1" is not <class>=<satoshis>`,
		"history=-1":                `EXAMPLE_PRICES: history: "-1" is not an integer from 0 to 2^53 - 1`,
		"history=01":                `EXAMPLE_PRICES: history: "01" is not an integer from 0 to 2^53 - 1`,
		"history=9007199254740992":  `EXAMPLE_PRICES: history: "9007199254740992" is not an integer from 0 to 2^53 - 1`,
		"history=1,history=2":       "EXAMPLE_PRICES names history twice",
		"history=5":                 "EXAMPLE_PRICES: history and history-after are priced together or not at all",
		"history=0,history-after=5": "EXAMPLE_PRICES: history and history-after are priced together or not at all",
	} {
		if _, err := ParsePrices(bad, classes, "EXAMPLE_PRICES"); err == nil || err.Error() != want {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, good := range []string{"history=5,history-after=6", "history=0", "history-after=0", "", " "} {
		if _, err := ParsePrices(good, classes, "EXAMPLE_PRICES"); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	b, _ := json.Marshal(TermsDocument("ls_example", classes, p))
	if string(b) != `{"service":"ls_example","terms":1,"classes":[{"class":"history","satoshis":5},{"class":"history-after","satoshis":7}]}` {
		t.Errorf("terms: %s", b)
	}
	if TermsPath("ls_example") != "/ls_example/terms" {
		t.Error(TermsPath("ls_example"))
	}
}

func TestTermsAndFreeQuestions(t *testing.T) {
	r := newRig(t, rigOptions{})
	code, body := get(t, r.srv.URL+"/ls_example/terms")
	if code != 200 || string(body) != `{"service":"ls_example","terms":1,"classes":[{"class":"history","satoshis":5},{"class":"history-after","satoshis":5}]}` {
		t.Fatalf("terms: %d %s", code, body)
	}
	code, _, body = post(t, r.srv.URL+"/lookup", ask("list"), nil)
	if code != 200 || !strings.Contains(string(body), `"asked":"list"`) || !strings.Contains(string(body), `"caller":""`) {
		t.Fatalf("free: %d %s", code, body)
	}
	if code, _, body = post(t, r.srv.URL+"/lookup", ask("nosuch"), nil); code != 400 || string(body) != `{"error":"no such question"}` {
		t.Fatalf("bad: %d %s", code, body)
	}
	if code, _, _ = post(t, r.srv.URL+"/lookup", []byte("nope"), nil); code != 400 {
		t.Fatal("not JSON")
	}
	if code, _, _ = post(t, r.srv.URL+"/lookup", []byte("null"), nil); code != 400 {
		t.Fatal("null")
	}
	if code, _ = get(t, r.srv.URL+"/nosuch"); code != 404 {
		t.Fatal("no such route")
	}
	if code, _, body = post(t, r.srv.URL+"/lookup", ask("history"), nil); code != 401 || !strings.Contains(string(body), "ERR_AUTH_REQUIRED") {
		t.Fatalf("priced without auth: %d %s", code, body)
	}
	if code, _, _ = post(t, r.srv.URL+"/lookup", ask("list"), map[string]string{"x-bsv-auth-nonce": "x"}); code != 400 {
		t.Fatal("partial BRC-104 headers")
	}
	if code, _, _ = post(t, r.srv.URL+"/lookup", bytes.Repeat([]byte("x"), MaxBody+1), nil); code != 413 {
		t.Fatal("too large")
	}
	r.ls.restored = false
	if code, _, _ = post(t, r.srv.URL+"/lookup", ask("list"), nil); code != 503 {
		t.Fatal("not restored")
	}
	if code, _, _ = post(t, r.srv.URL+"/lookup", ask("nosuch"), nil); code != 400 {
		t.Fatal("a question is classified before readiness is asked")
	}
	none := newRig(t, rigOptions{prices: "none"})
	if code, _ = get(t, none.srv.URL+"/ls_example/terms"); code != 404 {
		t.Fatal("a host that prices nothing serves no terms")
	}
	if code, _, _ = post(t, none.srv.URL+"/lookup", ask("history"), nil); code != 200 {
		t.Fatal("unpriced history")
	}
}

func TestPriceNeedsPayeeReceiverHeaders(t *testing.T) {
	prices, _ := ParsePrices("history=5,history-after=5", classes, "EXAMPLE_PRICES")
	_, err := New(Options{App: "example", Service: "ls_example", Classes: classes, LS: &answerer{}, Host: newFakeHost(), Prices: prices})
	if err == nil || !strings.Contains(err.Error(), "has a price, which needs a payee wallet, a payment receiver and a header source") {
		t.Fatal(err)
	}
}

// recorder keeps the last request's bytes, to send them again.
type recorder struct {
	mu   sync.Mutex
	last *http.Request
	body []byte
}

func (rc *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	rc.mu.Lock()
	rc.last, rc.body = req.Clone(context.Background()), body
	rc.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func TestAuthenticatedFreeAndPaid(t *testing.T) {
	r := newRig(t, rigOptions{})
	rc := &recorder{}
	af := clients.New(r.client, clients.WithoutLogging(), clients.WithHttpClient(&http.Client{Transport: rc}))
	ctx := context.Background()
	res, err := af.Fetch(ctx, r.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: ask("list")})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("x-bsv-auth-identity-key") != payeeKey.PubKey().ToDERHex() || !strings.Contains(string(body), `"caller":"`+clientKey.PubKey().ToDERHex()+`"`) {
		t.Fatalf("free over BRC-104: %d %s %v", res.StatusCode, body, res.Header)
	}
	// The same signed request sent again byte for byte is refused before any signature.
	rc.mu.Lock()
	again := rc.last
	again.Body = io.NopCloser(bytes.NewReader(rc.body))
	rc.mu.Unlock()
	res2, err := http.DefaultTransport.RoundTrip(again)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != 401 || !strings.Contains(string(b2), "ERR_AUTH_REPLAYED") {
		t.Fatalf("replay: %d %s", res2.StatusCode, b2)
	}
	if r.host.count("example_requests_total", map[string]string{"result": "replayed"}) != 1 {
		t.Error("replay not counted")
	}
	// A priced question: 402, paid, answered fast (the chain's arcade took it, the node shows no other spender).
	res, err = af.Fetch(ctx, r.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: ask("history")})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("x-bsv-payment-satoshis-paid") != "5" || !strings.Contains(string(body), `"asked":"history"`) {
		t.Fatalf("paid: %d %s %v", res.StatusCode, body, res.Header)
	}
	if len(r.recv.Payments) != 1 {
		t.Fatalf("%d payments", len(r.recv.Payments))
	}
	p := r.recv.Payments[0]
	if p.SenderIdentityKey != clientKey.PubKey().ToDERHex() || p.Class != "history" || p.Satoshis != 5 || p.Decision != "fast" || p.Reason != "at-or-below-threshold" || len(p.Inputs) != 1 {
		t.Fatalf("%+v", p)
	}
	if r.host.count("example_payments_total", map[string]string{"decision": "fast", "reason": "at-or-below-threshold"}) != 1 ||
		r.host.count("example_payments_total", map[string]string{"decision": "requested", "reason": "no-payment"}) != 1 {
		t.Error("payments not counted")
	}
	if !r.chain.Mined(p.Txid) && r.chain.Waiting() == 0 {
		t.Error("the host did not broadcast the payment")
	}
	// The watch releases it once mined.
	r.chain.Mine()
	r.front.Tick(ctx)
	if len(r.front.Gate.Watching()) != 0 || r.host.count("example_payment_events_total", map[string]string{"kind": "confirmed"}) != 1 {
		t.Errorf("watching %v", r.front.Gate.Watching())
	}
	// A line as the payee reads it, byte for byte with the Go payee's line once the decision is removed.
	line, _ := LedgerLine(p, 1700000000, LayoutSpread)
	pp, err := payee.ParseLine(line)
	if err != nil || pp.Txid != p.Txid || len(pp.Inputs) != 1 || pp.At != 1700000000 {
		t.Fatalf("the payee cannot read the line: %v %+v", err, pp)
	}
	plain := *p
	plain.Decision, plain.Reason = "", ""
	ours, _ := LedgerLine(&plain, 1700000000, LayoutPayee)
	theirs, _ := payee.Payment{Txid: p.Txid, Beef: pp.Beef, OutputIndex: 0, Satoshis: 5, DerivationPrefix: p.DerivationPrefix, DerivationSuffix: p.DerivationSuffix,
		SenderIdentityKey: p.SenderIdentityKey, Class: "history", Inputs: p.Inputs, At: 1700000000}.Line()
	if !bytes.Equal(ours, theirs) {
		t.Errorf("line differs from the payee's:\n%s%s", ours, theirs)
	}
}

func TestPaymentRefusals(t *testing.T) {
	r := newRig(t, rigOptions{})
	af := clients.New(r.client, clients.WithoutLogging())
	ctx := context.Background()
	// A payment whose prefix is not the server's.
	send := func(payment string) (int, string) {
		res, err := af.Fetch(ctx, r.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost,
			Headers: map[string]string{"content-type": "application/json", "x-bsv-payment": payment}, Body: ask("history")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		var v struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &v)
		return res.StatusCode, v.Code
	}
	if code, c := send("nope"); code != 400 || c != "ERR_MALFORMED_PAYMENT" {
		t.Errorf("not JSON: %d %s", code, c)
	}
	if code, c := send(`{"derivationPrefix":"AA==","derivationSuffix":"AQ==","transaction":"AAAA"}`); code != 400 || c != "ERR_INVALID_DERIVATION_PREFIX" {
		t.Errorf("foreign prefix: %d %s", code, c)
	}
	// A paid question, then the same payment again: replayed.
	res, err := af.Fetch(ctx, r.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: ask("history")})
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", err, res)
	}
	res.Body.Close()
	p := r.recv.Payments[0]
	raw, _ := json.Marshal(map[string]string{"derivationPrefix": p.DerivationPrefix, "derivationSuffix": p.DerivationSuffix, "transaction": base64Of(p.Beef)})
	if code, c := send(string(raw)); code != 409 || c != "ERR_PAYMENT_REPLAYED" {
		t.Errorf("replayed: %d %s", code, c)
	}
	if len(r.recv.Payments) != 1 {
		t.Error("a replay was recorded")
	}
	// A host with no arcade and no node holds every payment: 202, held, one line.
	held := newRig(t, rigOptions{noNetwork: true})
	af2 := clients.New(held.client, clients.WithoutLogging())
	res, err = af2.Fetch(ctx, held.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: ask("history")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 202 || !strings.Contains(string(b), "ERR_PAYMENT_HELD") || len(held.recv.Payments) != 1 || held.recv.Payments[0].Decision != "hold" || held.recv.Payments[0].Reason != "no-broadcast-leg" {
		t.Fatalf("held: %d %s %+v", res.StatusCode, b, held.recv.Payments)
	}
}

func base64Of(b []byte) string {
	var s strings.Builder
	enc := json.NewEncoder(&s)
	_ = enc.Encode(b)
	return strings.Trim(strings.TrimSpace(s.String()), `"`)
}

func TestHandshakeBudget(t *testing.T) {
	r := newRig(t, rigOptions{budget: BudgetConfig{PerSec: 1, Burst: 2, PerAddressPerSec: 1, AddressBurst: 2}})
	for i := 0; i < 2; i++ {
		if code, _, _ := post(t, r.srv.URL+"/.well-known/auth", []byte("{}"), nil); code != 400 {
			t.Fatalf("handshake %d: %d", i, code)
		}
	}
	code, h, body := post(t, r.srv.URL+"/.well-known/auth", []byte("{}"), nil)
	if code != 429 || h.Get("Retry-After") == "" || !strings.Contains(string(body), "ERR_RATE_LIMITED") {
		t.Fatalf("over budget: %d %s", code, body)
	}
	if r.host.count("example_handshakes_total", map[string]string{"result": "failed"}) != 2 || r.host.count("example_handshakes_total", map[string]string{"result": "limited_address"}) != 1 {
		t.Error("handshakes not counted")
	}
}

func TestSessionsBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	evicted := map[string]int{}
	s, err := NewSessions(2, 10*time.Second, clock, func(why string) { evicted[why]++ })
	if err != nil {
		t.Fatal(err)
	}
	k := clientKey.PubKey()
	for _, n := range []string{"a", "b", "c"} {
		_ = s.AddSession(&auth.PeerSession{IsAuthenticated: true, SessionNonce: n, PeerNonce: "p" + n, PeerIdentityKey: k})
	}
	if s.Size() != 2 || evicted["cap"] != 1 || s.HasSession("a") || !s.HasSession("c") {
		t.Fatalf("cap: size %d evicted %v", s.Size(), evicted)
	}
	now = now.Add(11 * time.Second)
	if s.HasSession("b") || s.Size() != 0 || evicted["idle"] != 2 {
		t.Fatalf("idle: size %d evicted %v", s.Size(), evicted)
	}
}

func TestLedgerFile(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(dir, LayoutSpread, func() time.Time { return time.Unix(7, 0) })
	if err != nil {
		t.Fatal(err)
	}
	p := &Received{Txid: strings.Repeat("ab", 32), Beef: []byte{1, 2, 3}, Satoshis: 5, DerivationPrefix: "AA==", DerivationSuffix: "AQ==", SenderIdentityKey: "02aa", Class: "history",
		Inputs: []string{strings.Repeat("dd", 32) + ".0"}, Decision: "fast", Reason: "at-or-below-threshold"}
	if l.Claim(p) != Accepted || l.Claim(p) != Replayed {
		t.Fatal("claim")
	}
	q := *p
	q.Txid = strings.Repeat("cd", 32)
	if l.Claim(&q) != Conflict {
		t.Fatal("a coin pays once")
	}
	h := *p
	h.Txid, h.Inputs, h.Decision = strings.Repeat("ef", 32), []string{strings.Repeat("ee", 32) + ".1"}, "hold"
	if l.Claim(&h) != Accepted || l.Claim(&h) != Accepted {
		t.Fatal("a held payment is accepted again")
	}
	h.Decision = "mined"
	if l.Claim(&h) != Accepted || l.Claim(&h) != Replayed {
		t.Fatal("a held payment is answered once")
	}
	_ = l.Close()
	text, _ := os.ReadFile(filepath.Join(dir, LedgerFile))
	if strings.Count(string(text), "\n") != 3 {
		t.Fatalf("lines:\n%s", text)
	}
	if !strings.HasPrefix(string(text), `{"txid":"`+p.Txid+`","beef":"AQID","outputIndex":0,"satoshis":5,"derivationPrefix":"AA==","derivationSuffix":"AQ==","senderIdentityKey":"02aa","class":"history","inputs":["`+p.Inputs[0]+`"],"decision":"fast","reason":"at-or-below-threshold","at":7}`+"\n") {
		t.Errorf("spread line:\n%s", text)
	}
	// Across a restart: the txids and coins are claimed again, the fast line read back, a cut line ended.
	_ = os.WriteFile(filepath.Join(dir, LedgerFile), append(text, []byte(`{"txid":"cut`)...), 0o600)
	l2, err := OpenLedger(dir, LayoutSpread, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Claim(p) != Replayed || l2.Claim(&q) != Conflict || len(l2.Fast) != 1 || l2.Fast[0].SenderIdentityKey != "02aa" {
		t.Fatalf("restart: fast %+v", l2.Fast)
	}
	text, _ = os.ReadFile(filepath.Join(dir, LedgerFile))
	if !bytes.HasSuffix(text, []byte("\n")) {
		t.Error("the cut line is not ended")
	}
	_ = l2.Close()
}

func TestAcceptConfig(t *testing.T) {
	env := map[string]string{"X_ACCEPT_THRESHOLD_SATS": "1000", "X_ACCEPT_WINDOW": "2m", "X_ARCADE_URL": "http://a.example", "X_ASSET_URL": "http://n.example"}
	c, err := ParseAcceptConfig("X", func(k string) string { return env[k] })
	if err != nil || c.Policy.ThresholdSats != 1000 || c.Policy.PayerLimit != 1000 || c.Policy.TotalLimit != 10000 || c.Policy.Window != 2*time.Minute || c.ArcadeURL != "http://a.example" {
		t.Fatalf("%+v %v", c, err)
	}
	for k, v := range map[string]string{"X_ACCEPT_THRESHOLD_SATS": "1e3", "X_ACCEPT_WATCH": "3 days", "X_ARCADE_URL": "ftp://x"} {
		if _, err := ParseAcceptConfig("X", func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

// TestAcceptConfigChain: <P>_CHAIN picks WhatsOnChain as the node view, as
// the TypeScript package's parseAcceptConfig does, never beside an asset
// URL; the gate it makes is networked with an arcade, and reads proofs from
// it.
func TestAcceptConfigChain(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := ParseAcceptConfig("X", get(map[string]string{"X_CHAIN": "woc:test", "X_WOC_KEY": " key ", "X_ARCADE_URL": "http://a.example"}))
	if err != nil || c.Chain != "test" || c.WocKey != "key" || c.AssetURL != "" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, m := range []map[string]string{{"X_CHAIN": "woc:regtest"}, {"X_CHAIN": "main"}, {"X_CHAIN": "woc:main", "X_ASSET_URL": "http://n.example"}} {
		if _, err := ParseAcceptConfig("X", get(m)); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
	host := newFakeHost()
	g := GateFor("example", host, testchain.New(10), c, "")
	if !g.Networked() || g.o.Node == nil || g.o.Asset != nil || g.o.Arcade == nil || g.o.Arcade.Spends == nil {
		t.Fatalf("not networked over WhatsOnChain: %+v", g.o)
	}
	if n, ok := g.o.Node.(*nodeapi.WoC); !ok || n.Network != "test" || n.Key != "key" {
		t.Fatalf("%+v", g.o.Node)
	}
	none, _ := ParseAcceptConfig("X", get(map[string]string{"X_CHAIN": "woc:main"}))
	if GateFor("example", newFakeHost(), testchain.New(10), none, "").Networked() {
		t.Fatal("networked without an arcade")
	}
	// A node view in Asset's place: a proof it serves makes a payment mined.
	chain := testchain.New(700)
	node := httptest.NewServer(chain)
	t.Cleanup(node.Close)
	view := &nodeapi.Asset{Base: node.URL}
	gate := NewGate(GateOptions{App: "example", Host: newFakeHost(), Headers: chain, Policy: acceptance.DefaultPolicy(), Node: view, Arcade: &publish.Arcade{Base: node.URL + "/arcade"}})
	if !gate.Networked() {
		t.Fatal("a node view in Asset's place is not networked")
	}
	if _, err := chain.Generate(1, "1BitcoinEaterAddressDontSendf59kuE"); err != nil {
		t.Fatal(err)
	}
	ids := chain.Txids()
	if tx := chain.Tx(ids[len(ids)-1]); tx == nil || !gate.mined(context.Background(), tx) {
		t.Fatal("the node view's proof is not read")
	}
}
