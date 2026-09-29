package hostset

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func says(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func get(h Host) (*http.Request, error) {
	return http.NewRequest(http.MethodGet, h.Base+"/x", nil)
}

// sourceFunc lets a test hand the client an exact Host, address included.
type sourceFunc func(ctx context.Context, base string) ([]Host, error)

func (f sourceFunc) Hosts(ctx context.Context, base string) ([]Host, error) { return f(ctx, base) }

func TestFirstFailsOverFromA503(t *testing.T) {
	down := serve(t, status(http.StatusServiceUnavailable))
	up := serve(t, says("two"))
	c := &Client{Source: Static{Bases: []string{down.URL, up.URL}}, Policy: PolicyFirst}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("%d attempts, want 2", len(results))
	}
	if results[0].Err == nil || results[0].Status != 503 {
		t.Errorf("the 503 was not a failure: %+v", results[0])
	}
	if results[1].Err != nil || string(results[1].Body) != "two" {
		t.Errorf("the second host did not win: %+v", results[1])
	}
}

// A 4xx is an answer, not a failure: the host was up and said no.
func TestFirstStopsAtA4xx(t *testing.T) {
	no := serve(t, status(http.StatusNotFound))
	yes := serve(t, says("yes"))
	c := &Client{Source: Static{Bases: []string{no.URL, yes.URL}}}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != 404 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
}

func TestAllQueriesEveryHost(t *testing.T) {
	one := serve(t, says("one"))
	two := serve(t, says("two"))
	c := &Client{Source: Static{Bases: []string{one.URL, two.URL}}, Policy: PolicyAll}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || string(results[0].Body) != "one" || string(results[1].Body) != "two" {
		t.Fatalf("results = %+v", results)
	}
	for _, r := range results {
		if r.Elapsed <= 0 {
			t.Errorf("Elapsed not recorded for %s", r.Host.Addr)
		}
	}
}

func TestQuorumShortfallIsNamed(t *testing.T) {
	up := serve(t, says("one"))
	dead := httptest.NewServer(says("never"))
	deadURL := dead.URL
	dead.Close()
	c := &Client{Source: Static{Bases: []string{up.URL, deadURL}}, Quorum: 2}
	results, err := c.Do(context.Background(), "", get)
	if err == nil {
		t.Fatal("quorum 2 with one host down succeeded")
	}
	if len(results) != 2 {
		t.Fatalf("%d attempts, want 2", len(results))
	}
	msg := err.Error()
	for _, want := range []string{"1 of 2", "need 2", strings.TrimPrefix(deadURL, "http://")} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not say %q", msg, want)
		}
	}
}

// The shortfall is one line naming every failed host in the order tried, so
// the whole text is pinned here: a caller that prints it prints this.
func TestShortfallTextNamesEveryFailedHost(t *testing.T) {
	a := serve(t, status(http.StatusServiceUnavailable))
	b := serve(t, status(http.StatusServiceUnavailable))
	c := &Client{Source: Static{Bases: []string{a.URL, b.URL}}}
	_, err := c.Do(context.Background(), "http://overlay.example", get)
	if err == nil {
		t.Fatal("two hosts down succeeded")
	}
	ha, hb := strings.TrimPrefix(a.URL, "http://"), strings.TrimPrefix(b.URL, "http://")
	want := "hostset: http://overlay.example: 0 of 2 host(s) answered, need 1: 127.0.0.1@" + ha + ": status 503; 127.0.0.1@" + hb + ": status 503"
	if err.Error() != want {
		t.Fatalf("error\n%q\nwant\n%q", err.Error(), want)
	}
}

func TestQuorumMet(t *testing.T) {
	one := serve(t, says("one"))
	two := serve(t, says("two"))
	three := serve(t, says("three"))
	c := &Client{Source: Static{Bases: []string{one.URL, two.URL, three.URL}}, Quorum: 2}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatal(err)
	}
	// Bounded to the quorum: the third host is not asked.
	if len(results) != 2 {
		t.Fatalf("%d attempts, want 2", len(results))
	}
}

func TestQuorumBeyondTheHostsFailsBeforeDialing(t *testing.T) {
	var hits atomic.Int32
	one := serve(t, func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	c := &Client{Source: Static{Bases: []string{one.URL}}, Quorum: 2}
	if _, err := c.Do(context.Background(), "", get); err == nil || !strings.Contains(err.Error(), "quorum 2 exceeds") {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a quorum that cannot be met still dialed")
	}
}

func TestRandomStillFindsTheLiveHost(t *testing.T) {
	down := serve(t, status(http.StatusBadGateway))
	up := serve(t, says("up"))
	c := &Client{Source: Static{Bases: []string{down.URL, up.URL}}, Policy: PolicyRandom}
	for i := 0; i < 8; i++ {
		results, err := c.Do(context.Background(), "", get)
		if err != nil {
			t.Fatal(err)
		}
		if last := results[len(results)-1]; last.Err != nil || string(last.Body) != "up" {
			t.Fatalf("results = %+v", results)
		}
	}
}

func TestAllWithEveryHostDownIsAnError(t *testing.T) {
	a := serve(t, status(http.StatusInternalServerError))
	b := serve(t, status(http.StatusInternalServerError))
	c := &Client{Source: Static{Bases: []string{a.URL, b.URL}}, Policy: PolicyAll}
	results, err := c.Do(context.Background(), "", get)
	if err == nil || !strings.Contains(err.Error(), "0 of 2") {
		t.Fatalf("got %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("%d attempts, want 2", len(results))
	}
}

func TestDNSSource(t *testing.T) {
	hosts, err := (DNS{}).Hosts(context.Background(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) == 0 {
		t.Fatal("localhost resolved to nothing")
	}
	for _, h := range hosts {
		if h.Name != "localhost" || h.Base != "http://localhost:8080" {
			t.Errorf("host = %+v", h)
		}
		ip, port, err := net.SplitHostPort(h.Addr)
		if err != nil || port != "8080" || net.ParseIP(ip) == nil {
			t.Errorf("Addr %q is not ip:8080", h.Addr)
		}
	}

	for base, want := range map[string]string{
		"https://192.0.2.7":             "192.0.2.7:443",
		"http://192.0.2.7":              "192.0.2.7:80",
		"http://[2001:db8::1]:8080":     "[2001:db8::1]:8080",
		"https://[2001:db8::1]/overlay": "[2001:db8::1]:443",
	} {
		hosts, err := (DNS{}).Hosts(context.Background(), base)
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if len(hosts) != 1 || hosts[0].Addr != want || hosts[0].Base != base {
			t.Errorf("%s: hosts = %+v, want Addr %s", base, hosts, want)
		}
	}

	for _, bad := range []string{"ftp://example.com", "example.com", "http://"} {
		if _, err := (DNS{}).Hosts(context.Background(), bad); err == nil {
			t.Errorf("%q was accepted as a base", bad)
		}
	}

	// A base that breaks both rules is refused for its scheme: the scheme
	// is checked first.
	for base, want := range map[string]string{
		"http://": `base "http://" has no host`,
		"ftp://":  `base "ftp://": scheme "ftp" is not http or https`,
	} {
		if _, err := (DNS{}).Hosts(context.Background(), base); err == nil || err.Error() != want {
			t.Errorf("%q: error %v, want %q", base, err, want)
		}
	}
}

// The transport takes no proxy, whether it is built from the default or
// cloned from a caller's: a proxy would choose the address, and choosing it
// is this package's job. A caller's transport is cloned, so the caller's
// own Proxy is left alone and the clone has none.
func TestTransportTakesNoProxy(t *testing.T) {
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	for name, c := range map[string]*Client{
		"default":  {},
		"caller's": {HTTP: proxied},
	} {
		tr, ok := c.transport().Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: the client has no *http.Transport of its own", name)
		}
		if tr.Proxy != nil {
			t.Errorf("%s: the transport asks a proxy function where to send a request", name)
		}
	}
	if proxied.Transport.(*http.Transport).Proxy == nil {
		t.Error("the caller's own transport was changed rather than cloned")
	}
}

func TestStaticSource(t *testing.T) {
	hosts, err := Static{Bases: []string{"https://a.example.com/overlay", "http://b.example.com:8080"}}.Hosts(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].Addr != "a.example.com:443" || hosts[1].Addr != "b.example.com:8080" || hosts[0].Base != "https://a.example.com/overlay" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

// The mechanism: the URL, and so the Host header, carries the name; the
// dialer goes to the address. name.example does not resolve anywhere, so a
// success proves the override, not the resolver.
func TestHostHeaderCarriesTheNameNotTheAddress(t *testing.T) {
	var seen atomic.Value
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Host)
		_, _ = w.Write([]byte("ok"))
	})
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	src := sourceFunc(func(context.Context, string) ([]Host, error) {
		return []Host{{Base: "http://name.example:" + port, Name: "name.example", Addr: srv.Listener.Addr().String()}}, nil
	})
	c := &Client{Source: src}
	results, err := c.Do(context.Background(), "http://name.example:"+port, get)
	if err != nil {
		t.Fatal(err)
	}
	if string(results[0].Body) != "ok" {
		t.Fatalf("results = %+v", results)
	}
	if seen.Load() != "name.example:"+port {
		t.Fatalf("Host header was %v, want name.example:%s", seen.Load(), port)
	}
}

// TLS is verified against the NAME. The test certificate is issued for
// example.com and *.example.com; the dial goes to 127.0.0.1 either way, so
// the one that fails fails on the certificate, not the connection.
func TestTLSVerifiesTheNameNotTheAddress(t *testing.T) {
	srv := httptest.NewTLSServer(says("tls"))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	for name, wantOK := range map[string]bool{"example.com": true, "api.example.com": true, "wrong.example.net": false} {
		src := sourceFunc(func(context.Context, string) ([]Host, error) {
			return []Host{{Base: "https://" + name + ":" + port, Name: name, Addr: addr}}, nil
		})
		c := &Client{Source: src, HTTP: srv.Client()}
		results, err := c.Do(context.Background(), "", get)
		if wantOK {
			if err != nil || string(results[0].Body) != "tls" {
				t.Errorf("%s: %v %+v", name, err, results)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: a certificate for another name verified", name)
		}
	}
}

func TestBodyIsBounded(t *testing.T) {
	big := serve(t, says(strings.Repeat("x", 100)))
	c := &Client{Source: Static{Bases: []string{big.URL}}, MaxBody: 8}
	results, err := c.Do(context.Background(), "", get)
	if err == nil || !errors.Is(results[0].Err, ErrBodyTooLarge) {
		t.Fatalf("a 100-byte body passed an 8-byte bound: %v %+v", err, results)
	}
}

// The bound is checked before the status, so a 5xx whose page is over the
// bound fails as the size. Pinned whole, since the order decides the text.
func TestBodyOverTheBoundIsNamedBeforeA5xx(t *testing.T) {
	big := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("x", 32)))
	})
	c := &Client{Source: Static{Bases: []string{big.URL}}, MaxBody: 16}
	_, err := c.Do(context.Background(), "http://overlay.example", get)
	if err == nil {
		t.Fatal("a 503 over the bound succeeded")
	}
	want := "hostset: http://overlay.example: 0 of 1 host(s) answered, need 1: 127.0.0.1@" + strings.TrimPrefix(big.URL, "http://") + ": hostset: response body exceeds the bound (16 bytes)"
	if err.Error() != want {
		t.Fatalf("error\n%q\nwant\n%q", err.Error(), want)
	}
}

func TestCrossOriginRedirectIsRefusedByDefault(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.example/x", http.StatusFound)
	})
	c := &Client{Source: Static{Bases: []string{srv.URL}}}
	_, err := c.Do(context.Background(), "", get)
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("got %v", err)
	}
}

// The origin is the scheme as well as the host, so the same host under
// another scheme is refused as another origin and never dialed.
func TestSchemeChangeRedirectIsRefusedByDefault(t *testing.T) {
	var asked atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, "https://"+r.Host+"/x", http.StatusFound)
	})
	hostport := strings.TrimPrefix(srv.URL, "http://")
	c := &Client{Source: Static{Bases: []string{srv.URL}}}
	results, err := c.Do(context.Background(), "", get)
	if err == nil {
		t.Fatal("a redirect to another scheme was followed")
	}
	want := `Get "https://` + hostport + `/x": hostset: redirect to another origin refused: http://` + hostport + ` -> https://` + hostport
	if got := results[0].Err; got == nil || got.Error() != want {
		t.Fatalf("got %v, want\n%q", got, want)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the host was asked %d time(s), want 1", n)
	}
}

func TestUnknownPolicyIsRefused(t *testing.T) {
	c := &Client{Source: Static{Bases: []string{"http://example.com"}}, Policy: "fastest"}
	if _, err := c.Do(context.Background(), "", get); err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("got %v", err)
	}
}

// A host that answered is left out of the shortfall text: the line names
// only the hosts that failed, after the count of those that did not.
func TestShortfallTextSkipsTheHostsThatAnswered(t *testing.T) {
	up := serve(t, says("one"))
	down := serve(t, status(http.StatusServiceUnavailable))
	c := &Client{Source: Static{Bases: []string{up.URL, down.URL}}, Quorum: 2}
	_, err := c.Do(context.Background(), "http://overlay.example", get)
	if err == nil {
		t.Fatal("quorum 2 with one host down succeeded")
	}
	want := "hostset: http://overlay.example: 1 of 2 host(s) answered, need 2: 127.0.0.1@" + strings.TrimPrefix(down.URL, "http://") + ": status 503"
	if err.Error() != want {
		t.Fatalf("error\n%q\nwant\n%q", err.Error(), want)
	}
}

// Host names compare without case, so a redirect to the same host in
// capitals stays within the origin and is followed.
func TestSameOriginRedirectIgnoresHostCase(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			// r.Host is localhost:port, the name the request was made to.
			http.Redirect(w, r, "http://"+strings.ToUpper(r.Host)+"/y", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("moved"))
	})
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := &Client{Source: Static{Bases: []string{"http://localhost:" + port}}}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatalf("a redirect to the same host in capitals was refused: %v", err)
	}
	if string(results[0].Body) != "moved" {
		t.Fatalf("results = %+v", results)
	}
}

// A redirect to the same host in capitals is dialed at the attempt's address
// and not looked up again: the dialer matches the name without case, as the
// redirect rule does, so the hop cannot land on another replica.
// replicas.example resolves nowhere, so the body proves the override.
func TestARedirectInOtherCaseStaysOnTheReplica(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			http.Redirect(w, r, "http://REPLICAS.EXAMPLE/y", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("moved"))
	})
	src := sourceFunc(func(context.Context, string) ([]Host, error) {
		return []Host{{Base: "http://replicas.example", Name: "replicas.example", Addr: srv.Listener.Addr().String()}}, nil
	})
	results, err := (&Client{Source: src}).Do(context.Background(), "http://replicas.example", get)
	if err != nil {
		t.Fatalf("the redirect in capitals did not reach the same replica: %v", err)
	}
	if string(results[0].Body) != "moved" {
		t.Fatalf("results = %+v", results)
	}
}

// A body of exactly the bound is read whole; only a longer one is refused.
func TestBodyAtTheBoundIsRead(t *testing.T) {
	srv := serve(t, says(strings.Repeat("x", 16)))
	c := &Client{Source: Static{Bases: []string{srv.URL}}, MaxBody: 16}
	results, err := c.Do(context.Background(), "", get)
	if err != nil {
		t.Fatalf("a 16-byte body was refused by a 16-byte bound: %v", err)
	}
	if len(results[0].Body) != 16 {
		t.Fatalf("read %d bytes, want 16", len(results[0].Body))
	}
}

// A redirect chain is followed for five hops and refused at the sixth, and
// the count is checked before the origin, so a sixth hop that also leaves the
// origin is refused for the length of the chain. The requests are counted,
// because a bound moved by one changes how many are made and not the words.
func TestRedirectsStopAtTheSixthHopBeforeTheOriginIsChecked(t *testing.T) {
	for name, out := range map[string]bool{"within the origin": false, "out of the origin": true} {
		var asked atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			to := "http://" + r.Host + "/x"
			if asked.Add(1) == 5 && out {
				to = "http://other.example/x"
			}
			http.Redirect(w, r, to, http.StatusFound)
		})
		sixth := srv.URL + "/x"
		if out {
			sixth = "http://other.example/x"
		}
		c := &Client{Source: Static{Bases: []string{srv.URL}}}
		results, err := c.Do(context.Background(), "", get)
		if err == nil || len(results) != 1 {
			t.Fatalf("%s: %v %+v", name, err, results)
		}
		if want := `Get "` + sixth + `": hostset: more than 5 redirects`; results[0].Err == nil || results[0].Err.Error() != want {
			t.Errorf("%s: got %v, want\n%q", name, results[0].Err, want)
		}
		if n := asked.Load(); n != 5 {
			t.Errorf("%s: the host was asked %d time(s), want 5", name, n)
		}
	}
}

// One name at two addresses is two replicas, and each is asked on its own
// connection. Were connections kept alive, the transport would pool them by
// the name and the second attempt would reuse the first address's
// connection, so PolicyAll and a quorum would count one replica twice.
func TestOneNameAtTwoAddressesIsTwoReplicas(t *testing.T) {
	a, b := serve(t, says("a")), serve(t, says("b"))
	src := sourceFunc(func(context.Context, string) ([]Host, error) {
		return []Host{
			{Base: "http://replicas.example", Name: "replicas.example", Addr: a.Listener.Addr().String()},
			{Base: "http://replicas.example", Name: "replicas.example", Addr: b.Listener.Addr().String()},
		}, nil
	})
	for name, c := range map[string]*Client{
		"all":      {Source: src, Policy: PolicyAll},
		"quorum 2": {Source: src, Quorum: 2},
	} {
		results, err := c.Do(context.Background(), "http://replicas.example", get)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(results) != 2 || string(results[0].Body) != "a" || string(results[1].Body) != "b" {
			t.Errorf("%s: results = %+v, want one answer from each address", name, results)
		}
	}
}

// Once the caller's context is done the fan-out stops: every later attempt
// would fail the same way, so the report names the one host tried and no
// other is asked.
func TestADoneContextEndsTheFanOut(t *testing.T) {
	var asked atomic.Int32
	count := func(w http.ResponseWriter, _ *http.Request) { asked.Add(1) }
	one, two, three := serve(t, count), serve(t, count), serve(t, count)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{Source: Static{Bases: []string{one.URL, two.URL, three.URL}}, Policy: PolicyAll}
	results, err := c.Do(ctx, "http://overlay.example", func(h Host) (*http.Request, error) {
		cancel()
		return get(h)
	})
	if len(results) != 1 || !errors.Is(results[0].Err, context.Canceled) {
		t.Fatalf("results = %+v, want the one attempt the context ended", results)
	}
	host := strings.TrimPrefix(one.URL, "http://")
	if want := `hostset: http://overlay.example: 0 of 1 host(s) answered, need 1: 127.0.0.1@` + host + `: Get "` + one.URL + `/x": context canceled`; err == nil || err.Error() != want {
		t.Errorf("got %v, want\n%q", err, want)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("%d host(s) were asked after the context was done", n)
	}
}

// Each attempt is bounded by Timeout, 15s when it is zero, and a caller's
// redirect rule replaces the same-origin default. The rule sees the
// attempt's deadline on the request it is asked about, and its refusal is
// the attempt's error.
func TestAttemptDeadlineAndTheCallersRedirectRule(t *testing.T) {
	errCaller := errors.New("the caller's rule")
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			http.Redirect(w, r, "/y", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("followed"))
	})
	for _, tc := range []struct{ timeout, want time.Duration }{{0, 15 * time.Second}, {3 * time.Second, 3 * time.Second}} {
		var left time.Duration
		rule := func(req *http.Request, _ []*http.Request) error {
			if d, ok := req.Context().Deadline(); ok {
				left = time.Until(d)
			}
			return errCaller
		}
		c := &Client{Source: Static{Bases: []string{srv.URL}}, HTTP: &http.Client{CheckRedirect: rule}, Timeout: tc.timeout}
		results, _ := c.Do(context.Background(), "", get)
		if len(results) != 1 || !errors.Is(results[0].Err, errCaller) {
			t.Fatalf("timeout %v: results = %+v, want the caller's rule to refuse the redirect", tc.timeout, results)
		}
		if left <= tc.want-time.Second || left > tc.want {
			t.Errorf("timeout %v: the attempt had %v left, want just under %v", tc.timeout, left, tc.want)
		}
	}
}

// A caller's overall timeout is kept on the client the attempts run on, and
// with no caller client there is none: each attempt has its own bound.
func TestTheCallersOverallTimeoutIsKept(t *testing.T) {
	if got := (&Client{HTTP: &http.Client{Timeout: 2 * time.Second}}).transport().Timeout; got != 2*time.Second {
		t.Errorf("the caller's timeout became %v", got)
	}
	if got := (&Client{}).transport().Timeout; got != 0 {
		t.Errorf("the default client has an overall timeout of %v", got)
	}
}

// PolicyRandom shuffles before it tries: over many runs each of two live
// hosts is asked first. Not shuffling would ask the first one every time.
func TestRandomVariesTheFirstHost(t *testing.T) {
	a, b := serve(t, says("a")), serve(t, says("b"))
	c := &Client{Source: Static{Bases: []string{a.URL, b.URL}}, Policy: PolicyRandom}
	first := map[string]int{}
	for i := 0; i < 64; i++ {
		results, err := c.Do(context.Background(), "", get)
		if err != nil || len(results) != 1 {
			t.Fatalf("run %d: %v %+v", i, err, results)
		}
		first[string(results[0].Body)]++
	}
	if first["a"] == 0 || first["b"] == 0 {
		t.Fatalf("first answers over 64 runs: %v; both hosts should lead some of them", first)
	}
}
