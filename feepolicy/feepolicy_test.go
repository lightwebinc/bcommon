package feepolicy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
)

// source reads a policy answer captured verbatim from a public broadcaster
// on 2026-10-07.
func source(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sources", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// policyServer answers GET /v1/policy with body and status, counting calls.
type policyServer struct {
	*httptest.Server
	mu     sync.Mutex
	body   []byte
	status int
	calls  atomic.Int32
	auth   string
}

func newPolicy(t *testing.T, body []byte) *policyServer {
	t.Helper()
	p := &policyServer{body: body, status: 200}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		if r.URL.Path != "/v1/policy" {
			http.NotFound(w, r)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.auth = r.Header.Get("Authorization")
		w.WriteHeader(p.status)
		_, _ = w.Write(p.body)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *policyServer) set(status int, body []byte) {
	p.mu.Lock()
	p.status, p.body = status, body
	p.mu.Unlock()
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestParsePolicyRealAnswers(t *testing.T) {
	want := map[string]feepolicy.Policy{
		"arc-gorillapool-policy":            {Rate: mint.Rate{Sats: 100, Bytes: 1000}, MaxTxSize: 100000000, MaxScriptSize: 100000000},
		"arc-taal-policy":                   {Rate: mint.Rate{Sats: 100, Bytes: 1000}, MaxTxSize: 100000000, MaxScriptSize: 100000000},
		"arcade-gorillapool-policy":         {Rate: mint.Rate{Sats: 100, Bytes: 1000}, MaxTxSize: 10485760, MaxScriptSize: 500000},
		"arcade-gorillapool-testnet-policy": {Rate: mint.Rate{Sats: 100, Bytes: 1000}, MaxTxSize: 10485760, MaxScriptSize: 500000},
	}
	for name, w := range want {
		p, err := feepolicy.ParsePolicy(source(t, name))
		if err != nil || *p != w {
			t.Errorf("%s: %+v %v", name, p, err)
		}
	}
	// GorillaPool's testnet ARC publishes 0 satoshis, which is refused.
	if _, err := feepolicy.ParsePolicy(source(t, "arc-gorillapool-testnet-policy")); !errors.Is(err, feepolicy.ErrPolicy) {
		t.Fatalf("satoshis 0: %v", err)
	}
	for _, bad := range []string{
		``, `{}`, `{"policy":{}}`, `[]`, `{"policy":{"miningFee":{"satoshis":1}}}`,
		`{"policy":{"miningFee":{"satoshis":1.5,"bytes":1000}}}`,
		`{"policy":{"miningFee":{"satoshis":-1,"bytes":1000}}}`,
		`{"policy":{"miningFee":{"satoshis":"1","bytes":1000}}}`,
		`{"policy":{"miningFee":{"satoshis":1,"bytes":0}}}`,
		`{"policy":{"miningFee":{"satoshis":1,"bytes":1000000001}}}`,
		`{"policy":{"miningFee":{"satoshis":1e3,"bytes":1000}}}`,
		`{"policy":{"miningFee":{"satoshis":18446744073709551616,"bytes":1000}}}`,
		`{"policy":{"miningFee":{"satoshis":1,"bytes":1000},"maxtxsizepolicy":-5}}`,
	} {
		if _, err := feepolicy.ParsePolicy([]byte(bad)); !errors.Is(err, feepolicy.ErrPolicy) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestStatic(t *testing.T) {
	f, err := feepolicy.Static(mint.DefaultFees).Fees(context.Background())
	if err != nil || f != mint.DefaultFees {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := feepolicy.Static(mint.Fees{Rate: mint.Rate{Sats: 1}}).Fees(context.Background()); !errors.Is(err, mint.ErrRate) {
		t.Fatalf("zero bytes: %v", err)
	}
}

func arc(c *clock, base mint.Fees, urls ...string) *feepolicy.ARC {
	return &feepolicy.ARC{URLs: urls, Base: base, Now: c.now}
}

func TestARCUsesTheLiveRate(t *testing.T) {
	srv := newPolicy(t, source(t, "arcade-gorillapool-policy"))
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := arc(c, mint.LegacyFees, srv.URL)
	a.Key = "k"
	f, err := a.Fees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.Rate != (mint.Rate{Sats: 100, Bytes: 1000}) || f.Floor != 250 || f.MaxRate != feepolicy.DefaultMaxRate {
		t.Fatalf("%+v", f)
	}
	if st := a.Status(); st.Source != feepolicy.SourceARC || st.Err != nil {
		t.Fatalf("%+v", st)
	}
	if srv.auth != "Bearer k" {
		t.Fatalf("auth %q", srv.auth)
	}
	if p := a.Policy(context.Background()); p == nil || p.MaxTxSize != 10485760 {
		t.Fatalf("policy %+v", p)
	}
	// Cached within the TTL: one fetch for all of the above.
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("%d fetches", n)
	}
	c.add(feepolicy.DefaultTTL)
	if _, err := a.Fees(context.Background()); err != nil || srv.calls.Load() != 2 {
		t.Fatalf("after the TTL: %v, %d fetches", err, srv.calls.Load())
	}
}

func TestARCTakesTheHighestRateAndLowestLimits(t *testing.T) {
	low := newPolicy(t, source(t, "arc-gorillapool-policy"))
	high := newPolicy(t, []byte(`{"policy":{"miningFee":{"satoshis":250,"bytes":1000},"maxtxsizepolicy":200000000,"maxscriptsizepolicy":400000}}`))
	down := newPolicy(t, nil)
	down.set(500, []byte("down"))
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := arc(c, mint.DefaultFees, low.URL, high.URL, down.URL)
	f, err := a.Fees(context.Background())
	if err != nil || f.Rate != (mint.Rate{Sats: 250, Bytes: 1000}) {
		t.Fatalf("%+v %v", f, err)
	}
	if p := a.Policy(context.Background()); p.MaxTxSize != 100000000 || p.MaxScriptSize != 400000 {
		t.Fatalf("%+v", p)
	}
}

func TestARCGuards(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	// A huge rate is held to the maximum: one satoshi a byte by default.
	huge := newPolicy(t, []byte(`{"policy":{"miningFee":{"satoshis":18446744073709551615,"bytes":1}}}`))
	f, err := arc(c, mint.DefaultFees, huge.URL).Fees(ctx)
	if err != nil || f.Rate != feepolicy.DefaultMaxRate {
		t.Fatalf("huge: %+v %v", f, err)
	}
	// A rate under the minimum is raised to it.
	tiny := newPolicy(t, []byte(`{"policy":{"miningFee":{"satoshis":1,"bytes":1000000}}}`))
	f, err = arc(c, mint.DefaultFees, tiny.URL).Fees(ctx)
	if err != nil || f.Rate != feepolicy.DefaultMinRate {
		t.Fatalf("tiny: %+v %v", f, err)
	}
	a := arc(c, mint.DefaultFees, tiny.URL)
	a.Min, a.Max = mint.Rate{Sats: 1, Bytes: 1000000}, mint.Rate{Sats: 1, Bytes: 1000}
	if f, _ = a.Fees(ctx); f.Rate != (mint.Rate{Sats: 1, Bytes: 1000000}) || f.MaxRate != a.Max {
		t.Fatalf("configured bounds: %+v", f)
	}
	// A per-transaction maximum in Base is refused at build time.
	base := mint.DefaultFees
	base.Max = 100
	f, _ = arc(c, base, huge.URL).Fees(ctx)
	if _, err := f.For(10000); !errors.Is(err, mint.ErrFeeTooHigh) {
		t.Fatalf("max_tx: %v", err)
	}
	// Zero satoshis (GorillaPool's testnet ARC) is refused and Base used.
	zero := newPolicy(t, source(t, "arc-gorillapool-testnet-policy"))
	z := arc(c, mint.DefaultFees, zero.URL)
	if f, err = z.Fees(ctx); err != nil || f.Rate != mint.DefaultFees.Rate {
		t.Fatalf("zero: %+v %v", f, err)
	}
	if st := z.Status(); st.Source != feepolicy.SourceStatic || !errors.Is(st.Err, feepolicy.ErrPolicy) {
		t.Fatalf("zero status: %+v", st)
	}
}

func TestARCDegradesToCacheThenStatic(t *testing.T) {
	ctx := context.Background()
	srv := newPolicy(t, []byte(`{"policy":{"miningFee":{"satoshis":200,"bytes":1000}}}`))
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	var notes []string
	a := arc(c, mint.DefaultFees, srv.URL)
	a.Note = func(f string, args ...any) { notes = append(notes, f) }
	if f, _ := a.Fees(ctx); f.Rate.Sats != 200 {
		t.Fatalf("%+v", f)
	}
	srv.set(503, []byte("busy"))
	c.add(feepolicy.DefaultTTL)
	f, err := a.Fees(ctx)
	if err != nil || f.Rate.Sats != 200 {
		t.Fatalf("cache: %+v %v", f, err)
	}
	if st := a.Status(); st.Source != feepolicy.SourceCache || st.Age != feepolicy.DefaultTTL || st.Err == nil {
		t.Fatalf("%+v", st)
	}
	if len(notes) != 1 {
		t.Fatalf("notes %v", notes)
	}
	// A failed fetch is not retried until the TTL passes again.
	calls := srv.calls.Load()
	_, _ = a.Fees(ctx)
	if srv.calls.Load() != calls {
		t.Fatal("refetched within the TTL after a failure")
	}
	c.add(feepolicy.DefaultStale)
	if f, _ = a.Fees(ctx); f.Rate != mint.DefaultFees.Rate {
		t.Fatalf("stale: %+v", f)
	}
	if st := a.Status(); st.Source != feepolicy.SourceStatic {
		t.Fatalf("%+v", st)
	}
	// Recovery.
	srv.set(200, []byte(`{"policy":{"miningFee":{"satoshis":150,"bytes":1000}}}`))
	c.add(feepolicy.DefaultTTL)
	if f, _ = a.Fees(ctx); f.Rate.Sats != 150 {
		t.Fatalf("recovered: %+v", f)
	}
}

func TestARCTimeout(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := arc(c, mint.DefaultFees, slow.URL)
	a.Timeout = 50 * time.Millisecond
	start := time.Now()
	f, err := a.Fees(context.Background())
	if err != nil || f.Rate != mint.DefaultFees.Rate || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v %v after %s", f, err, time.Since(start))
	}
}

func TestPolicyURLs(t *testing.T) {
	var got []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"policy":{"miningFee":{"satoshis":100,"bytes":1000}}}`))
	}))
	defer srv.Close()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	for _, u := range []string{srv.URL, srv.URL + "/", srv.URL + "/v1", srv.URL + "/arc/v1/"} {
		if _, err := arc(c, mint.DefaultFees, u).Fees(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, " ") != "/v1/policy /v1/policy /v1/policy /arc/v1/policy" {
		t.Fatalf("%v", got)
	}
}

func u64(v uint64) *uint64 { return &v }

func TestConfig(t *testing.T) {
	c, err := feepolicy.ParseConfig([]byte(`{
	  "dust": 1,
	  "floor": 1,
	  "max_rate": {"bytes": 1, "satoshis": 1},
	  "max_tx": 5000,
	  "min_rate": {"bytes": 1000, "satoshis": 100},
	  "policy_urls": ["https://arcade.gorillapool.io"],
	  "rate": {"bytes": 1000, "satoshis": 100},
	  "source": "static"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Fees(mint.DefaultFees)
	want := mint.Fees{Rate: mint.Rate{Sats: 100, Bytes: 1000}, Floor: 1, Dust: 1, Max: 5000, MaxRate: mint.Rate{Sats: 1, Bytes: 1}}
	if err != nil || f != want {
		t.Fatalf("%+v %v", f, err)
	}
	src, err := c.Build(mint.DefaultFees)
	if _, ok := src.(feepolicy.Static); err != nil || !ok {
		t.Fatalf("%T %v", src, err)
	}
	c.Source = "arc"
	src, err = c.Build(mint.DefaultFees)
	if a, ok := src.(*feepolicy.ARC); err != nil || !ok || a.Base != want || a.URLs[0] != "https://arcade.gorillapool.io" {
		t.Fatalf("%T %v", src, err)
	}
	// Empty: the defaults, static.
	empty := &feepolicy.Config{}
	if f, err := empty.Fees(mint.DefaultFees); err != nil || f != mint.DefaultFees {
		t.Fatalf("%+v %v", f, err)
	}
	var none *feepolicy.Config
	if s, err := none.Build(mint.NetworkFees); err != nil || s.(feepolicy.Static) != feepolicy.Static(mint.NetworkFees) {
		t.Fatalf("%v %v", s, err)
	}
	for name, bad := range map[string]string{
		"unknown key":   `{"fee_rate": 1}`,
		"zero bytes":    `{"rate": {"satoshis": 1, "bytes": 0}}`,
		"zero sats":     `{"rate": {"satoshis": 0, "bytes": 1}}`,
		"bad source":    `{"source": "magic"}`,
		"arc, no urls":  `{"source": "arc"}`,
		"bad url":       `{"source": "arc", "policy_urls": ["ftp://x"]}`,
		"min above max": `{"source": "arc", "policy_urls": ["https://a"], "min_rate": {"satoshis": 2, "bytes": 1}}`,
		"floor > max":   `{"floor": 300, "max_tx": 200}`,
		"max zero b":    `{"max_rate": {"satoshis": 1, "bytes": 0}}`,
	} {
		c, err := feepolicy.ParseConfig([]byte(bad))
		if err == nil {
			_, err = c.Build(mint.DefaultFees)
		}
		if !errors.Is(err, feepolicy.ErrConfig) && !errors.Is(err, mint.ErrRate) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMergeLegacy(t *testing.T) {
	c, err := feepolicy.MergeLegacy(nil, u64(1), u64(250))
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := c.Fees(mint.DefaultFees); f.Rate != (mint.Rate{Sats: 1, Bytes: 1}) || f.Floor != 250 {
		t.Fatalf("%+v", f)
	}
	if f, _ := c.Fees(mint.DefaultFees); f.Dust != mint.DefaultFees.Dust {
		t.Fatalf("dust %+v", f)
	}
	if _, err := feepolicy.MergeLegacy(&feepolicy.Config{Rate: &mint.Rate{Sats: 1, Bytes: 1}}, u64(1), nil); !errors.Is(err, feepolicy.ErrConfig) {
		t.Fatalf("both rates: %v", err)
	}
	if _, err := feepolicy.MergeLegacy(&feepolicy.Config{Floor: u64(1)}, nil, u64(250)); !errors.Is(err, feepolicy.ErrConfig) {
		t.Fatalf("both floors: %v", err)
	}
	if _, err := feepolicy.MergeLegacy(nil, u64(0), nil); !errors.Is(err, feepolicy.ErrConfig) {
		t.Fatalf("zero: %v", err)
	}
	if c, err := feepolicy.MergeLegacy(nil, nil, nil); err != nil || c.Rate != nil {
		t.Fatalf("none: %v", err)
	}
}

func TestConcurrentFees(t *testing.T) {
	srv := newPolicy(t, source(t, "arc-taal-policy"))
	a := &feepolicy.ARC{URLs: []string{srv.URL}, Base: mint.DefaultFees}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Fees(context.Background()); err != nil {
				t.Error(err)
			}
			_ = a.Status()
		}()
	}
	wg.Wait()
}
