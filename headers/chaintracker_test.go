package headers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// fixture reads one of the bridge's OWN generated response bodies, vendored
// verbatim into testdata/fixtures. Testing against a hand-written body would
// only prove this client agrees with itself.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureField(t *testing.T, name, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m[field].(string)
	return s
}

func serve(t *testing.T, routes map[string]func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("%s was asked with Accept %q, want application/json", r.URL.Path, got)
		}
		h, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("content-type", "application/json")
		h(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidRootAgainstTheBridgesOwnBytes(t *testing.T) {
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) { _, _ = w.Write(fixture(t, "v1_root_known")) },
	})
	root, err := chainhash.NewHashFromHex(fixtureField(t, "v1_root_known", "merkleRoot"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := New(srv.URL).IsValidRootForHeight(context.Background(), root, 101)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the bridge's own root did not validate")
	}
}

func TestWrongRootIsFalseNotAnError(t *testing.T) {
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) { _, _ = w.Write(fixture(t, "v1_root_known")) },
	})
	other, err := chainhash.NewHashFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := New(srv.URL).IsValidRootForHeight(context.Background(), other, 101)
	if err != nil {
		t.Fatalf("a mismatching root should be a plain false, got err %v", err)
	}
	if ok {
		t.Fatal("a root the chain does not commit to validated")
	}
}

// The distinction this whole client exists to get right. A height the bridge
// does not hold is "cannot check yet". A broken service is "something is
// wrong". A verifier that cannot tell them apart either accepts forgeries
// during an outage or rejects good proofs during one.
func TestNotYetAndBrokenAreDifferent(t *testing.T) {
	notYet := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/999999": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(fixture(t, "v1_root_unknown"))
		},
	})
	root, _ := chainhash.NewHashFromHex(strings.Repeat("ab", 32))
	ok, err := New(notYet.URL).IsValidRootForHeight(context.Background(), root, 999999)
	if err != nil {
		t.Fatalf("404 must be (false, nil), got err %v", err)
	}
	if ok {
		t.Fatal("404 validated a root")
	}

	broken := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
	})
	if _, err := New(broken.URL).IsValidRootForHeight(context.Background(), root, 101); err == nil {
		t.Fatal("a 500 from the header service was read as a failed proof")
	}
}

// Only a 404 is "not held yet" and only a 200 is an answer: a 4xx that is
// not 404 is an error like any other, and so is a 2xx that is not 200.
func TestOnlyA404IsNotYetAndOnlyA200IsAnAnswer(t *testing.T) {
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/7": func(w http.ResponseWriter) { w.WriteHeader(http.StatusGone) },
		"/v1/tip":    func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	})
	root, _ := chainhash.NewHashFromHex(strings.Repeat("ab", 32))
	_, err := New(srv.URL).IsValidRootForHeight(context.Background(), root, 7)
	if want := "header service: root for height 7: status 410"; err == nil || err.Error() != want {
		t.Errorf("root 410: got %v, want %q", err, want)
	}
	_, err = New(srv.URL).CurrentHeight(context.Background())
	if want := "header service: tip: status 204"; err == nil || err.Error() != want {
		t.Errorf("tip 204: got %v, want %q", err, want)
	}
}

func TestCurrentHeightFromTheBridgesOwnBytes(t *testing.T) {
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/tip": func(w http.ResponseWriter) { _, _ = w.Write(fixture(t, "v1_tip")) },
	})
	h, err := New(srv.URL).CurrentHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h != 101 {
		t.Fatalf("height = %d, want 101 (the fixture's)", h)
	}
}

// A byte-reversed root is the failure with no symptom: the service answers
// 200 throughout and every comparison fails. Pin it as a mismatch rather than
// an error so the reading is unambiguous.
func TestReversedRootDoesNotValidate(t *testing.T) {
	display := fixtureField(t, "v1_root_known", "merkleRoot")
	root, err := chainhash.NewHashFromHex(display)
	if err != nil {
		t.Fatal(err)
	}
	reversed := make([]byte, len(root))
	for i := range root {
		reversed[i] = root[len(root)-1-i]
	}
	var m map[string]any
	if err := json.Unmarshal(fixture(t, "v1_root_known"), &m); err != nil {
		t.Fatal(err)
	}
	m["merkleRoot"] = chainhash.Hash(reversed).String()
	bad, _ := json.Marshal(m)

	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) { _, _ = w.Write(bad) },
	})
	ok, err := New(srv.URL).IsValidRootForHeight(context.Background(), root, 101)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a reversed root validated")
	}
}

// The root is compared without case, so a service that renders it in
// capitals still validates a proof. The fixture's root is all digits, so the
// bridge's answer is given a root with letters in it.
func TestUpperCaseRootValidates(t *testing.T) {
	const display = "00000000000000000000000000000000000000000000000000000000deadbeef"
	upper := strings.ToUpper(display)
	root, err := chainhash.NewHashFromHex(display)
	if err != nil {
		t.Fatal(err)
	}
	if root.String() != display {
		t.Fatalf("the root renders as %s, not %s", root, display)
	}
	var m map[string]any
	if err := json.Unmarshal(fixture(t, "v1_root_known"), &m); err != nil {
		t.Fatal(err)
	}
	m["merkleRoot"] = upper
	body, _ := json.Marshal(m)
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) { _, _ = w.Write(body) },
	})
	ok, err := New(srv.URL).IsValidRootForHeight(context.Background(), root, 101)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the root in capitals did not validate")
	}
}

func TestBaseWithTrailingSlash(t *testing.T) {
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/tip": func(w http.ResponseWriter) { _, _ = w.Write(fixture(t, "v1_tip")) },
	})
	c := &Client{Base: srv.URL + "/"}
	if _, err := c.CurrentHeight(context.Background()); err != nil {
		t.Fatalf("a base with a trailing slash produced a double slash: %v", err)
	}
}

// A body one byte over the bound is refused rather than truncated. Truncation
// is the worse of the two failures: the prefix that survives the cut still
// parses often enough to be believed, and nothing downstream can tell a
// clipped answer about the chain from a complete one.
func TestBodyOverTheBoundIsRefusedNotTruncated(t *testing.T) {
	tip := func(size int) []byte {
		head, tail := []byte(`{"height":101,"hash":"`), []byte(`"}`)
		pad := size - len(head) - len(tail)
		if pad < 0 {
			t.Fatalf("a tip answer cannot be shaped to %d bytes", size)
		}
		return append(append(head, bytes.Repeat([]byte("a"), pad)...), tail...)
	}

	atBound := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/tip": func(w http.ResponseWriter) { _, _ = w.Write(tip(maxBody)) },
	})
	h, err := New(atBound.URL).CurrentHeight(context.Background())
	if err != nil {
		t.Fatalf("a body of exactly %d bytes was refused: %v", maxBody, err)
	}
	if h != 101 {
		t.Fatalf("height = %d, want 101", h)
	}

	over := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/tip": func(w http.ResponseWriter) { _, _ = w.Write(tip(maxBody + 1)) },
	})
	_, err = New(over.URL).CurrentHeight(context.Background())
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("one byte over the bound: %v, want ErrBodyTooLarge", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxBody)) {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}
}

// The default client takes no proxy. A header read is the root of trust for
// every proof, so a host named only by HTTP_PROXY must not get to answer for
// the chain. A caller that needs one supplies its own client, where the
// configuration can see it.
func TestDefaultClientTakesNoProxyAndTheCallersClientWins(t *testing.T) {
	tr, ok := New("https://headers.example").httpClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the default client has no transport of its own, so it follows HTTP_PROXY from the environment")
	}
	if tr.Proxy != nil {
		t.Fatal("the default transport asks a proxy function where to send a header read")
	}
	mine := &http.Client{Timeout: time.Second}
	if got := (&Client{Base: "https://headers.example", HTTP: mine}).httpClient(); got != mine {
		t.Fatal("a caller-supplied client was replaced")
	}
}

// A nil root is no root the chain commits to: the answer is false, and the
// header service is not asked.
func TestANilRootIsFalseAndAsksNothing(t *testing.T) {
	var asked atomic.Int32
	srv := serve(t, map[string]func(w http.ResponseWriter){
		"/v1/root/101": func(w http.ResponseWriter) {
			asked.Add(1)
			_, _ = w.Write(fixture(t, "v1_root_known"))
		},
	})
	ok, err := New(srv.URL).IsValidRootForHeight(context.Background(), nil, 101)
	if ok || err != nil {
		t.Fatalf("a nil root gave %v, %v; want false and no error", ok, err)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the header service was asked %d time(s) about a nil root", n)
	}
}

// The default client: the Timeout given, or 10s when none is; a 10s bound on
// the TLS handshake either way; verification on and TLS 1.2 at the least;
// and HTTP/2 still attempted, which a transport that sets its own TLS config
// and dialer loses unless it asks for it.
func TestDefaultClientPolicy(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{{0, 10 * time.Second}, {3 * time.Second, 3 * time.Second}} {
		c := (&Client{Base: "https://headers.example", Timeout: tc.in}).httpClient()
		if c.Timeout != tc.want {
			t.Errorf("Timeout %v: the client's timeout is %v, want %v", tc.in, c.Timeout, tc.want)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Timeout %v: transport is %T", tc.in, c.Transport)
		}
		if cfg := tr.TLSClientConfig; cfg == nil || cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify {
			t.Errorf("Timeout %v: TLS config %+v, want verification on and TLS 1.2 at the least", tc.in, cfg)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Errorf("Timeout %v: the transport no longer attempts HTTP/2", tc.in)
		}
		if tr.TLSHandshakeTimeout != 10*time.Second {
			t.Errorf("Timeout %v: the TLS handshake is bounded at %v, want 10s", tc.in, tr.TLSHandshakeTimeout)
		}
	}
}
