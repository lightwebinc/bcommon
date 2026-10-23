package termsroute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/auth"
	"github.com/bsv-blockchain/go-sdk/auth/authpayload"
	"github.com/bsv-blockchain/go-sdk/auth/brc104"
	"github.com/bsv-blockchain/go-sdk/auth/utils"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"
)

// MaxBody bounds a request body: a question and its authentication are
// small, and a payment rides in a header. MaxHeaders bounds the headers.
const (
	MaxBody    = 64 << 10
	MaxHeaders = 256 << 10
	// AuthTimeout bounds a Peer's work on one message.
	AuthTimeout = 30 * time.Second
)

// RequestResults are <app>_requests_total's results: authenticated
// requests refused before any signature.
var RequestResults = []string{"replayed", "limited"}

// HandshakeResults are <app>_handshakes_total's results.
var HandshakeResults = []string{"accepted", "failed", "limited_address", "limited_global"}

// Question is a question as the route reads it: a JSON object, its members
// unchecked.
type Question struct {
	Service string          `json:"service"`
	Query   json.RawMessage `json:"query"`
}

// Answerer is the lookup service behind the route. Classify names a
// question's class or errs (answered 400 with the error's text); Answer is
// the JSON a 200 carries (an error is answered 400 with its text), with the
// asker's identity in ctx (Caller) when the request was authenticated;
// Restored is false until the service can answer (503).
type Answerer interface {
	Restored() bool
	Classify(q Question) (string, error)
	Answer(ctx context.Context, q Question) (any, error)
}

type callerKey struct{}

// WithCaller carries an asker's identity key (compressed hex) in ctx.
func WithCaller(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, callerKey{}, identity)
}

// Caller is the identity key the request being answered was verified
// under, when the route verified one.
func Caller(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(callerKey{}).(string)
	return s, ok && s != ""
}

// SessionBound is the bound on BRC-104 sessions: the most kept, and how
// long one idle is kept.
type SessionBound struct {
	Max int
	TTL time.Duration
}

// Options builds a Front.
type Options struct {
	// App is the application's name: the prefix of the route's metrics and
	// log lines. Service is the lookup service's name.
	App     string
	Service string
	// Classes are the classes a question can be, in the terms document's
	// order.
	Classes []PricedClass
	// PaymentProtocol is BRC-29's derivation protocol, under which a priced
	// question's payment pays the host.
	PaymentProtocol wallet.Protocol
	LS              Answerer
	Host            bmodule.Host
	// Prices by priceable class; empty when the host prices nothing.
	Prices map[string]uint64
	// Payee is the payee's key, whose identity key is the server's: needed
	// for BRC-104 and priced classes.
	Payee *ec.PrivateKey
	// Receiver is where accepted payments go.
	Receiver Receiver
	// Headers are the host's block headers, which a payment verifies against.
	Headers chaintracker.ChainTracker
	// Sessions bounds BRC-104 sessions; the defaults when zero.
	Sessions SessionBound
	// Gate is payment acceptance; without one, and with a price and
	// headers, every payment is held for confirmation.
	Gate *Gate
	// Budget is the handshake budget, DefaultBudget when zero; Responses
	// the budget of signed responses for each session,
	// DefaultResponseBudget when zero.
	Budget    BudgetConfig
	Responses ResponseBudgetConfig
	// ResponseBudgetBeforeVerify takes a session's budget by every request
	// that names the session, before verification, so forged requests on
	// one session cost its budget and not the host's CPU; false (the
	// default) takes it only by a request whose signature verified.
	ResponseBudgetBeforeVerify bool
	Now                        func() time.Time
}

type reply struct {
	status  int
	headers http.Header
	body    []byte
}

func jsonReply(status int, v any, headers http.Header) reply {
	b, _ := json.Marshal(v)
	h := http.Header{"Content-Type": {"application/json"}}
	for k, vs := range headers {
		h[k] = vs
	}
	return reply{status: status, headers: h, body: b}
}

func failure(status int, code, description string) reply {
	return jsonReply(status, map[string]string{"status": "error", "code": code, "description": description}, nil)
}

// serverTransport is BRC-104 over HTTP, server side: it hands requests to a
// Peer and catches what the Peer sends back, keyed by the request they
// answer.
type serverTransport struct {
	mu      sync.Mutex
	onData  func(context.Context, *auth.AuthMessage) error
	waiting map[string]chan *auth.AuthMessage
}

func (t *serverTransport) Send(_ context.Context, m *auth.AuthMessage) error {
	var key string
	if m.MessageType == auth.MessageTypeGeneral {
		if len(m.Payload) < 32 {
			return errors.New("general message without a request id")
		}
		key = "g:" + base64.StdEncoding.EncodeToString(m.Payload[:32])
	} else {
		key = "h:" + m.YourNonce
	}
	t.mu.Lock()
	ch, ok := t.waiting[key]
	delete(t.waiting, key)
	t.mu.Unlock()
	if !ok {
		return errors.New("no open request for this message")
	}
	ch <- m
	return nil
}

func (t *serverTransport) OnData(cb func(context.Context, *auth.AuthMessage) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onData = cb
	return nil
}

func (t *serverTransport) GetRegisteredOnData() (func(context.Context, *auth.AuthMessage) error, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onData == nil {
		return nil, errors.New("no handler")
	}
	return t.onData, nil
}

func (t *serverTransport) expect(key string) chan *auth.AuthMessage {
	ch := make(chan *auth.AuthMessage, 1)
	t.mu.Lock()
	t.waiting[key] = ch
	t.mu.Unlock()
	return ch
}

func (t *serverTransport) forget(key string) {
	t.mu.Lock()
	delete(t.waiting, key)
	t.mu.Unlock()
}

func (t *serverTransport) deliver(ctx context.Context, m *auth.AuthMessage) error {
	t.mu.Lock()
	cb := t.onData
	t.mu.Unlock()
	if cb == nil {
		return errors.New("no peer")
	}
	return cb(ctx, m)
}

// payeeWallet is the payee's ProtoWallet as the wallet.Interface a Peer
// takes; a Peer uses only the key and signature methods.
type payeeWallet struct{ *wallet.ProtoWallet }

var errNotSpending = errors.New("termsroute: the payee wallet signs and derives; it never spends")

func (payeeWallet) CreateAction(context.Context, wallet.CreateActionArgs, string) (*wallet.CreateActionResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) SignAction(context.Context, wallet.SignActionArgs, string) (*wallet.SignActionResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) AbortAction(context.Context, wallet.AbortActionArgs, string) (*wallet.AbortActionResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) ListActions(context.Context, wallet.ListActionsArgs, string) (*wallet.ListActionsResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) InternalizeAction(context.Context, wallet.InternalizeActionArgs, string) (*wallet.InternalizeActionResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) ListOutputs(context.Context, wallet.ListOutputsArgs, string) (*wallet.ListOutputsResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) RelinquishOutput(context.Context, wallet.RelinquishOutputArgs, string) (*wallet.RelinquishOutputResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) AcquireCertificate(context.Context, wallet.AcquireCertificateArgs, string) (*wallet.Certificate, error) {
	return nil, errNotSpending
}
func (payeeWallet) ListCertificates(context.Context, wallet.ListCertificatesArgs, string) (*wallet.ListCertificatesResult, error) {
	return &wallet.ListCertificatesResult{}, nil
}
func (payeeWallet) ProveCertificate(context.Context, wallet.ProveCertificateArgs, string) (*wallet.ProveCertificateResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) RelinquishCertificate(context.Context, wallet.RelinquishCertificateArgs, string) (*wallet.RelinquishCertificateResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) DiscoverByIdentityKey(context.Context, wallet.DiscoverByIdentityKeyArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) DiscoverByAttributes(context.Context, wallet.DiscoverByAttributesArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) IsAuthenticated(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return &wallet.AuthenticatedResult{Authenticated: true}, nil
}
func (payeeWallet) WaitForAuthentication(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return &wallet.AuthenticatedResult{Authenticated: true}, nil
}
func (payeeWallet) GetHeight(context.Context, any, string) (*wallet.GetHeightResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) GetHeaderForHeight(context.Context, wallet.GetHeaderArgs, string) (*wallet.GetHeaderResult, error) {
	return nil, errNotSpending
}
func (payeeWallet) GetNetwork(context.Context, any, string) (*wallet.GetNetworkResult, error) {
	return &wallet.GetNetworkResult{Network: wallet.NetworkMainnet}, nil
}
func (payeeWallet) GetVersion(context.Context, any, string) (*wallet.GetVersionResult, error) {
	return &wallet.GetVersionResult{Version: "termsroute-1"}, nil
}

// Front is the terms route. It is an http.Handler.
type Front struct {
	o         Options
	wallet    *wallet.ProtoWallet
	identity  string
	peer      *auth.Peer
	transport *serverTransport
	// Sessions are the BRC-104 sessions the route keeps.
	Sessions *Sessions
	// Budget is what handshakes the route answers; Responses what signed
	// responses each session is given; Seen the authenticated requests
	// answered lately.
	Budget    *HandshakeBudget
	Responses *KeyedBudget
	Seen      *SeenRequests
	// Gate is payment acceptance, when the host prices anything.
	Gate *Gate

	mu        sync.Mutex
	verifying map[string]bool
	started   bool
}

// New builds a route. A price needs a payee key, a receiver and headers.
func New(o Options) (*Front, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	for name, price := range o.Prices {
		if price > 0 && (o.Payee == nil || o.Receiver == nil || o.Headers == nil) {
			return nil, fmt.Errorf("%s: %s has a price, which needs a payee wallet, a payment receiver and a header source", o.App, name)
		}
	}
	if o.LS == nil || o.Host == nil {
		return nil, fmt.Errorf("%s: a lookup service and a host are required", o.App)
	}
	bound := o.Sessions
	if bound.Max <= 0 {
		bound.Max = DefaultMaxSessions
	}
	if bound.TTL <= 0 {
		bound.TTL = DefaultSessionTTL * time.Second
	}
	if o.Budget == (BudgetConfig{}) {
		o.Budget = DefaultBudget
	}
	if o.Responses == (ResponseBudgetConfig{}) {
		o.Responses = DefaultResponseBudget
	}
	f := &Front{o: o, transport: &serverTransport{waiting: map[string]chan *auth.AuthMessage{}}, verifying: map[string]bool{}, Seen: NewSeenRequests(MaxSeenRequests)}
	var err error
	if f.Sessions, err = NewSessions(bound.Max, bound.TTL, o.Now, func(why string) {
		o.Host.Metrics().Inc(o.App+"_sessions_evicted_total", map[string]string{"why": why}, 1)
	}); err != nil {
		return nil, err
	}
	for _, why := range []string{"cap", "idle"} {
		o.Host.Metrics().Preset(o.App+"_sessions_evicted_total", map[string]string{"why": why})
	}
	o.Host.Metrics().Gauge(o.App+"_sessions", func() float64 { return float64(f.Sessions.Size()) })
	if f.Budget, err = NewHandshakeBudget(o.Budget, o.Now); err != nil {
		return nil, err
	}
	if f.Responses, err = NewKeyedBudget(o.Responses, bound.Max, o.Now); err != nil {
		return nil, err
	}
	for _, r := range RequestResults {
		o.Host.Metrics().Preset(o.App+"_requests_total", map[string]string{"result": r})
	}
	for _, r := range HandshakeResults {
		o.Host.Metrics().Preset(o.App+"_handshakes_total", map[string]string{"result": r})
	}
	priced := false
	for _, p := range o.Prices {
		priced = priced || p > 0
	}
	f.Gate = o.Gate
	if f.Gate == nil && o.Headers != nil && priced {
		f.Gate = NewGate(GateOptions{App: o.App, Host: o.Host, Headers: o.Headers, Policy: defaultPolicy(), Now: o.Now})
	}
	if o.Payee != nil {
		w, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: o.Payee})
		if err != nil {
			return nil, err
		}
		f.wallet = w
		f.identity = o.Payee.PubKey().ToDERHex()
		f.peer = auth.NewPeer(&auth.PeerOptions{Wallet: payeeWallet{w}, Transport: f.transport, SessionManager: f.Sessions})
	}
	return f, nil
}

// Identity is the route's BRC-104 identity key, compressed hex; empty
// without a payee.
func (f *Front) Identity() string { return f.identity }

func (f *Front) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started || f.peer == nil {
		return nil
	}
	if err := f.peer.Start(); err != nil {
		return err
	}
	f.started = true
	return nil
}

// Listen serves the route on addr until ctx ends.
func (f *Front) Listen(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return f.Serve(ctx, l)
}

// Serve serves the route on l until ctx ends.
func (f *Front) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: MaxHeaders}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func send(w http.ResponseWriter, r reply) {
	for k, vs := range r.headers {
		w.Header()[k] = vs
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

func (f *Front) inc(name string, labels map[string]string) {
	f.o.Host.Metrics().Inc(f.o.App+name, labels, 1)
}

// ServeHTTP implements http.Handler.
func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f.start(); err != nil {
		f.o.Host.Log(f.o.App+" terms route failed", map[string]any{"err": err.Error()})
		send(w, failure(http.StatusInternalServerError, "ERR_INTERNAL", "the request could not be served"))
		return
	}
	handshake := r.Method == http.MethodPost && r.URL.Path == "/.well-known/auth"
	if handshake && f.peer != nil {
		// Before the body is read and before any signature work.
		if v := f.Budget.Take(r.RemoteAddr); !v.OK {
			f.inc("_handshakes_total", map[string]string{"result": "limited_" + v.Limit})
			send(w, jsonReply(http.StatusTooManyRequests, map[string]string{"status": "error", "code": "ERR_RATE_LIMITED", "description": "too many handshakes; try again later"},
				http.Header{"Retry-After": {strconv.Itoa(v.RetryAfter)}}))
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		send(w, failure(http.StatusRequestEntityTooLarge, "ERR_TOO_LARGE", "the request body is too large"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), AuthTimeout)
	defer cancel()
	if handshake {
		status := f.handshake(ctx, w, body)
		if f.peer != nil {
			result := "failed"
			if status == http.StatusOK {
				result = "accepted"
			}
			f.inc("_handshakes_total", map[string]string{"result": result})
		}
		return
	}
	requestID := r.Header.Get(brc104.HeaderRequestID)
	if requestID == "" {
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), brc104.AuthHeaderPrefix+"-") {
				send(w, failure(http.StatusBadRequest, "ERR_AUTH_PARTIAL", "partial BRC-104 authentication headers"))
				return
			}
		}
		send(w, f.route(ctx, r, body, ""))
		return
	}
	f.authenticated(ctx, w, r, body, requestID)
}

func authHeaders(m *auth.AuthMessage) http.Header {
	h := http.Header{}
	h.Set(brc104.HeaderVersion, m.Version)
	h.Set(brc104.HeaderMessageType, string(m.MessageType))
	if m.IdentityKey != nil {
		h.Set(brc104.HeaderIdentityKey, m.IdentityKey.ToDERHex())
	}
	if m.YourNonce != "" {
		h.Set(brc104.HeaderYourNonce, m.YourNonce)
	}
	if m.Nonce != "" {
		h.Set(brc104.HeaderNonce, m.Nonce)
	}
	if len(m.Signature) > 0 {
		h.Set(brc104.HeaderSignature, hex.EncodeToString(m.Signature))
	}
	return h
}

// handshake answers a handshake and returns the status answered.
func (f *Front) handshake(ctx context.Context, w http.ResponseWriter, body []byte) int {
	if f.peer == nil {
		send(w, failure(http.StatusNotFound, "ERR_NOT_FOUND", "this host offers no BRC-104 authentication"))
		return http.StatusNotFound
	}
	var m auth.AuthMessage
	if err := json.Unmarshal(body, &m); err != nil {
		send(w, failure(http.StatusBadRequest, "ERR_AUTH_MALFORMED", "the handshake is not JSON"))
		return http.StatusBadRequest
	}
	if m.MessageType != auth.MessageTypeInitialRequest || m.InitialNonce == "" {
		send(w, failure(http.StatusBadRequest, "ERR_AUTH_MALFORMED", "only an initial request is accepted here"))
		return http.StatusBadRequest
	}
	key := "h:" + m.InitialNonce
	ch := f.transport.expect(key)
	if err := f.transport.deliver(ctx, &m); err != nil {
		f.transport.forget(key)
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", err.Error()))
		return http.StatusUnauthorized
	}
	var r *auth.AuthMessage
	select {
	case r = <-ch:
	case <-ctx.Done():
		f.transport.forget(key)
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", "authentication timed out"))
		return http.StatusUnauthorized
	}
	b, err := json.Marshal(r)
	if err != nil {
		send(w, failure(http.StatusInternalServerError, "ERR_INTERNAL", "the handshake could not be answered"))
		return http.StatusInternalServerError
	}
	h := authHeaders(r)
	h.Set("Content-Type", "application/json")
	send(w, reply{status: http.StatusOK, headers: h, body: b})
	return http.StatusOK
}

var hexRE = regexp.MustCompile(`^([0-9a-f]{2})+$`)

// strictBase64 decodes s when it is non-empty standard base64 with padding
// and zero pad bits, the one spelling of its bytes.
func strictBase64(s string) ([]byte, bool) {
	if s == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

func (f *Front) authenticated(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, requestID string) {
	h := func(n string) string { return r.Header.Get(n) }
	version, idHex, nonce, yourNonce, sigHex := h(brc104.HeaderVersion), h(brc104.HeaderIdentityKey), h(brc104.HeaderNonce), h(brc104.HeaderYourNonce), h(brc104.HeaderSignature)
	id, ok := strictBase64(requestID)
	if f.peer == nil || version == "" || idHex == "" || nonce == "" || yourNonce == "" || sigHex == "" || !ok || len(id) != brc104.RequestIDLength || !hexRE.MatchString(sigHex) {
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", "the BRC-104 authentication headers are incomplete"))
		return
	}
	identity, err := ec.PublicKeyFromString(idHex)
	if err != nil {
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", "the BRC-104 authentication headers are incomplete"))
		return
	}
	// A request already answered, or being answered, is refused before any
	// work: sent again byte for byte it would otherwise cost the host a
	// signature each time and its sender nothing.
	once := idHex + ":" + requestID
	f.mu.Lock()
	seen := f.Seen.Has(once) || f.verifying[once]
	if !seen {
		f.verifying[once] = true
	}
	f.mu.Unlock()
	if seen {
		f.inc("_requests_total", map[string]string{"result": "replayed"})
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_REPLAYED", "this request was already answered"))
		return
	}
	defer func() {
		f.mu.Lock()
		delete(f.verifying, once)
		f.mu.Unlock()
	}()
	// before-verify: the session's budget is taken by every request that
	// names the session, so a stream of forged requests on one session
	// costs its own budget and not the host's CPU.
	if f.o.ResponseBudgetBeforeVerify && !f.allowed(w, yourNonce) {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	payload, err := authpayload.FromHTTPRequest(id, r)
	if err != nil {
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", "the request does not verify"))
		return
	}
	sig, _ := hex.DecodeString(sigHex)
	m := &auth.AuthMessage{Version: version, MessageType: auth.MessageTypeGeneral, IdentityKey: identity, Nonce: nonce, YourNonce: yourNonce, Payload: payload, Signature: sig}
	if err := f.transport.deliver(ctx, m); err != nil {
		send(w, failure(http.StatusUnauthorized, "ERR_AUTH_FAILED", "the request does not verify"))
		return
	}
	// Remembered only once it verified, so nobody fills the memory with
	// requests it cannot sign.
	f.Seen.Add(once)
	if !f.o.ResponseBudgetBeforeVerify && !f.allowed(w, yourNonce) {
		return
	}
	rep := f.route(WithCaller(ctx, idHex), r, body, idHex)
	signed, err := f.sign(ctx, id, rep, yourNonce, identity)
	if err != nil {
		f.o.Host.Log(f.o.App+" terms route failed", map[string]any{"err": err.Error()})
		send(w, failure(http.StatusInternalServerError, "ERR_INTERNAL", "the request could not be served"))
		return
	}
	for k, vs := range authHeaders(signed) {
		if k == http.CanonicalHeaderKey(brc104.HeaderMessageType) {
			continue
		}
		rep.headers[k] = vs
	}
	rep.headers.Set(brc104.HeaderRequestID, requestID)
	send(w, rep)
}

// sign signs rep for the session yourNonce names, as the SDK's Peer signs a
// general message: the response payload under the auth protocol, keyed by
// a fresh nonce and the session's, for the peer.
func (f *Front) sign(ctx context.Context, requestID []byte, rep reply, yourNonce string, peer *ec.PublicKey) (*auth.AuthMessage, error) {
	session, err := f.Sessions.GetSession(yourNonce)
	if err != nil || session == nil || !session.IsAuthenticated {
		return nil, errors.New("no authenticated session for the response")
	}
	payload, err := authpayload.FromResponse(requestID, authpayload.SimplifiedHttpResponse{StatusCode: rep.status, Header: rep.headers, Body: rep.body})
	if err != nil {
		return nil, err
	}
	nonce := string(utils.RandomBase64(32))
	sig, err := f.wallet.CreateSignature(ctx, wallet.CreateSignatureArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: auth.AUTH_PROTOCOL_ID},
			KeyID:        nonce + " " + session.PeerNonce,
			Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: peer},
		},
		Data: payload,
	}, "auth-peer")
	if err != nil {
		return nil, err
	}
	session.TouchLastUpdate()
	f.Sessions.UpdateSession(session)
	return &auth.AuthMessage{Version: auth.AUTH_VERSION, MessageType: auth.MessageTypeGeneral, IdentityKey: f.o.Payee.PubKey(),
		Nonce: nonce, YourNonce: session.PeerNonce, Payload: payload, Signature: sig.Signature.Serialize()}, nil
}

// allowed takes one signed response from session's budget, or answers 429
// and says so.
func (f *Front) allowed(w http.ResponseWriter, session string) bool {
	v := f.Responses.Take(session)
	if v.OK {
		return true
	}
	f.inc("_requests_total", map[string]string{"result": "limited"})
	send(w, jsonReply(http.StatusTooManyRequests, map[string]string{"status": "error", "code": "ERR_RATE_LIMITED", "description": "too many requests on this session; try again later"},
		http.Header{"Retry-After": {strconv.Itoa(v.RetryAfter)}}))
	return false
}

// route is the application: terms and lookups. identity is the asker's key
// when authenticated.
func (f *Front) route(ctx context.Context, r *http.Request, body []byte, identity string) reply {
	if r.URL.Path == TermsPath(f.o.Service) && r.Method == http.MethodGet {
		if len(f.o.Prices) == 0 {
			return failure(http.StatusNotFound, "ERR_NO_TERMS", "this host prices nothing")
		}
		return jsonReply(http.StatusOK, TermsDocument(f.o.Service, f.o.Classes, f.o.Prices), nil)
	}
	if r.URL.Path != "/lookup" || r.Method != http.MethodPost {
		return failure(http.StatusNotFound, "ERR_NOT_FOUND", "no such route")
	}
	var q Question
	if err := json.Unmarshal(body, &q); err != nil || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return jsonReply(http.StatusBadRequest, map[string]string{"error": "the question is not a JSON object"}, nil)
	}
	cls, err := f.o.LS.Classify(q)
	if err != nil {
		return jsonReply(http.StatusBadRequest, map[string]string{"error": err.Error()}, nil)
	}
	if !f.o.LS.Restored() {
		return failure(http.StatusServiceUnavailable, "ERR_NOT_READY", "the lookup service is not restored yet")
	}
	extra := http.Header{}
	if price := f.o.Prices[cls]; price > 0 {
		if identity == "" {
			return failure(http.StatusUnauthorized, "ERR_AUTH_REQUIRED", cls+" is priced; ask it over BRC-104 authentication")
		}
		raw := r.Header.Get("x-bsv-payment")
		if raw == "" {
			if f.Gate != nil {
				f.Gate.Count("requested", "no-payment")
			}
			prefix, err := utils.CreateNonce(ctx, f.wallet, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf})
			if err != nil {
				return failure(http.StatusInternalServerError, "ERR_INTERNAL", "no derivation prefix")
			}
			return jsonReply(http.StatusPaymentRequired,
				map[string]any{"status": "error", "code": "ERR_PAYMENT_REQUIRED", "satoshisRequired": price, "description": fmt.Sprintf("%s costs %d satoshis a question", cls, price)},
				http.Header{"X-Bsv-Payment-Version": {PaymentVersion}, "X-Bsv-Payment-Satoshis-Required": {strconv.FormatUint(price, 10)}, "X-Bsv-Payment-Derivation-Prefix": {prefix}})
		}
		sats, rep, ok := f.pay(ctx, raw, identity, price, cls)
		if !ok {
			return rep
		}
		extra.Set("X-Bsv-Payment-Satoshis-Paid", strconv.FormatUint(sats, 10))
	}
	ans, err := f.o.LS.Answer(ctx, q)
	if err != nil {
		return jsonReply(http.StatusBadRequest, map[string]string{"error": err.Error()}, nil)
	}
	return jsonReply(http.StatusOK, ans, extra)
}

// pay checks a BRC-105 payment of price for one question of cls from
// identity: a prefix this server issued, an Atomic BEEF whose output 0
// holds at least the price and pays the key BRC-29 derives for the prefix,
// the suffix and the asker, a transaction that verifies against the host's
// headers, a txid never claimed before, and no input an accepted payment
// already spent; then acceptance.
func (f *Front) pay(ctx context.Context, raw, identity string, price uint64, cls string) (uint64, reply, bool) {
	bad := func(status int, code, d, reason string) (uint64, reply, bool) {
		if f.Gate != nil {
			f.Gate.Count("refuse", reason)
		}
		return 0, failure(status, code, d), false
	}
	var p struct {
		Prefix json.RawMessage `json:"derivationPrefix"`
		Suffix json.RawMessage `json:"derivationSuffix"`
		Tx     json.RawMessage `json:"transaction"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return bad(http.StatusBadRequest, "ERR_MALFORMED_PAYMENT", "the payment header is not JSON", "malformed")
	}
	var prefix, suffix, txB64 string
	if json.Unmarshal(p.Prefix, &prefix) != nil || json.Unmarshal(p.Suffix, &suffix) != nil || json.Unmarshal(p.Tx, &txB64) != nil {
		return bad(http.StatusBadRequest, "ERR_MALFORMED_PAYMENT", "the payment header is malformed", "malformed")
	}
	if _, ok := strictBase64(prefix); !ok {
		return bad(http.StatusBadRequest, "ERR_MALFORMED_PAYMENT", "the payment header is malformed", "malformed")
	}
	if _, ok := strictBase64(suffix); !ok {
		return bad(http.StatusBadRequest, "ERR_MALFORMED_PAYMENT", "the payment header is malformed", "malformed")
	}
	beef, ok := strictBase64(txB64)
	if !ok {
		return bad(http.StatusBadRequest, "ERR_MALFORMED_PAYMENT", "the payment transaction is not standard base64", "malformed")
	}
	issued, err := utils.VerifyNonce(ctx, prefix, f.wallet, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf})
	if err != nil || !issued {
		return bad(http.StatusBadRequest, "ERR_INVALID_DERIVATION_PREFIX", "this server did not issue that derivation prefix", "malformed")
	}
	_, tx, _, err := guard.ParseBEEF(beef, guard.DefaultBound)
	if err != nil || tx == nil {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", "the payment is not Atomic BEEF", "malformed")
	}
	if len(tx.Outputs) == 0 || tx.Outputs[0].Satoshis < price {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", fmt.Sprintf("output 0 does not hold %d satoshis", price), "malformed")
	}
	payer, err := ec.PublicKeyFromString(identity)
	if err != nil {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", "the asker's key does not parse", "malformed")
	}
	forSelf := true
	k, err := f.wallet.GetPublicKey(ctx, wallet.GetPublicKeyArgs{EncryptionArgs: wallet.EncryptionArgs{
		ProtocolID: f.o.PaymentProtocol, KeyID: prefix + " " + suffix,
		Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: payer}}, ForSelf: &forSelf}, "")
	if err != nil {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", "no key derives for this prefix, suffix and asker", "malformed")
	}
	addr, err := script.NewAddressFromPublicKey(k.PublicKey, true)
	if err != nil {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", "no key derives for this prefix, suffix and asker", "malformed")
	}
	want, _ := p2pkh.Lock(addr)
	if tx.Outputs[0].LockingScript == nil || !bytes.Equal(*tx.Outputs[0].LockingScript, *want) {
		return bad(http.StatusBadRequest, "ERR_INVALID_PAYMENT", "output 0 does not pay the key derived for this prefix, suffix and asker", "wrong-script")
	}
	if ok, err := spv.Verify(ctx, tx, f.o.Headers, nil); err != nil || !ok {
		return bad(http.StatusBadRequest, "ERR_PAYMENT_SPV", "the payment does not verify against this host's headers", "spv-failed")
	}
	accepted := &Received{Txid: tx.TxID().String(), Beef: beef, OutputIndex: 0, Satoshis: tx.Outputs[0].Satoshis,
		DerivationPrefix: prefix, DerivationSuffix: suffix, SenderIdentityKey: identity, Class: cls, Inputs: SpentOutpoints(tx)}
	replayed := func() (uint64, reply, bool) {
		return bad(http.StatusConflict, "ERR_PAYMENT_REPLAYED", "this payment was already used", "replayed")
	}
	conflict := func() (uint64, reply, bool) {
		return bad(http.StatusConflict, "ERR_PAYMENT_CONFLICT", "this payment spends a coin an earlier payment spent", "conflict")
	}
	// Refused before the host broadcasts it: a conflicting payment is never sent.
	switch f.o.Receiver.Refusal(accepted.Txid, accepted.Inputs) {
	case Replayed:
		return replayed()
	case Conflict:
		return conflict()
	}
	if f.Gate == nil {
		return bad(http.StatusInternalServerError, "ERR_INTERNAL", errNoGate.Error(), "malformed")
	}
	v := f.Gate.Accept(ctx, tx, identity, *want, price)
	if v.Decision == "refuse" {
		f.o.Host.Log(f.o.App+" payment refused", map[string]any{"txid": accepted.Txid, "reason": v.Reason, "detail": v.Detail})
		d := fmt.Sprintf("the payment is refused (%s)", v.Reason)
		if v.Detail != "" {
			d += ": " + v.Detail
		}
		return 0, failure(http.StatusBadRequest, "ERR_PAYMENT_REFUSED", d), false
	}
	accepted.Decision = "fast"
	if v.Decision == "hold" {
		accepted.Decision = "hold"
	} else if v.Reason == "mined" {
		accepted.Decision = "mined"
	}
	accepted.Reason = v.Reason
	claim := f.o.Receiver.Claim(accepted)
	// Taken fast and then not taken: nothing is owed, so nothing is charged or watched.
	if claim != Accepted && v.Decision == "fast" {
		f.Gate.Forget(accepted.Txid)
	}
	switch claim {
	case Replayed:
		return replayed()
	case Conflict:
		return conflict()
	}
	if v.Decision == "hold" {
		f.o.Host.Log(f.o.App+" payment held for confirmation", map[string]any{"txid": accepted.Txid, "satoshis": accepted.Satoshis, "class": cls, "reason": v.Reason})
		// 202, not 402: a BRC-105 client takes every 402 for a price to
		// pay, so a 402 without the BRC-105 headers fails inside the
		// client and never reaches its caller, and one with them would be
		// paid a second time.
		return 0, jsonReply(http.StatusAccepted, map[string]any{"status": "error", "code": "ERR_PAYMENT_HELD", "txid": accepted.Txid, "reason": v.Reason,
			"description": fmt.Sprintf("the payment is held for confirmation (%s); send it again once it is mined", v.Reason)}, nil), false
	}
	f.o.Host.Log(f.o.App+" payment accepted", map[string]any{"txid": accepted.Txid, "satoshis": accepted.Satoshis, "class": cls, "decision": accepted.Decision})
	return accepted.Satoshis, reply{}, true
}

// RestoreFast watches again the fast payments a ledger read at start,
// charged as they were.
func (f *Front) RestoreFast(lines []FastLine) {
	if f.Gate == nil {
		return
	}
	for _, l := range lines {
		_, tx, _, err := guard.ParseBEEF(l.Beef, guard.DefaultBound)
		if err != nil || tx == nil {
			continue
		}
		f.Gate.Restore(tx, l.SenderIdentityKey, l.Satoshis, l.At)
	}
}

// Tick is the background work: one sweep of the payment watch. Run it
// every 30 seconds.
func (f *Front) Tick(ctx context.Context) {
	if f.Gate != nil {
		f.Gate.Sweep(ctx)
	}
}
