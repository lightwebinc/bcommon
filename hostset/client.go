package hostset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Policy says how the hosts a Source returns are tried.
type Policy string

const (
	// PolicyFirst tries hosts in the order given and stops at the first
	// success. A failure is a connect error, a timeout, a body over the
	// bound or a 5xx. A 4xx is an answer: the host was up and said no, and
	// asking the next replica for a second opinion would only hide that.
	PolicyFirst Policy = "first"
	// PolicyRandom shuffles, then behaves as PolicyFirst. It spreads a
	// fleet of readers over a fleet of replicas without any coordination.
	PolicyRandom Policy = "random"
	// PolicyAll queries every host. It is what a fork check needs: a reader
	// that refuses a name whose replicas disagree can only see disagreement
	// if it asks everyone.
	PolicyAll Policy = "all"
)

// DefaultMaxBody bounds a response body when Client.MaxBody is zero. Lookup
// answers carry BEEF, and a BEEF with its ancestry can run to megabytes, so
// the bound is large. It is still a bound.
const DefaultMaxBody int64 = 64 << 20

// ErrBodyTooLarge refuses a response above the bound.
var ErrBodyTooLarge = errors.New("hostset: response body exceeds the bound")

// Result is one attempt against one host. Err is non-nil for every failure
// as PolicyFirst defines it, including a 5xx, so Err == nil is the single
// test for "this host answered"; Status and Body are kept alongside so the
// caller can see what a failing host said.
type Result struct {
	Host    Host
	Status  int
	Body    []byte
	Err     error
	Elapsed time.Duration
}

// Client runs one request against the hosts behind a base URL.
type Client struct {
	// Source lists the hosts; nil means DNS{}.
	Source Source
	// Policy defaults to PolicyFirst.
	Policy Policy
	// Quorum is how many hosts must answer. Zero and one mean one. A larger
	// value collects that many successful answers from distinct hosts, in
	// policy order, and fails naming the shortfall if fewer answer. The
	// caller compares the bodies; whether they must agree is its rule, not
	// this package's.
	Quorum int
	// HTTP supplies the TLS configuration, the overall timeout and the
	// redirect policy. Its Transport is cloned, never used directly; see
	// transport.
	HTTP *http.Client
	// Timeout bounds each attempt. Defaults to 15s.
	Timeout time.Duration
	// MaxBody bounds each response body. Defaults to DefaultMaxBody.
	MaxBody int64

	once   sync.Once
	client *http.Client
}

// targetKey carries the Host an attempt is for, from Do to the dialer.
type targetKey struct{}

// transport builds the one http.Client this Client uses.
//
// The address is chosen in the dialer and not by rewriting the URL, because
// the URL is what TLS verifies against: its hostname becomes the SNI, the
// name the certificate must match, and the Host header. Rewriting req.URL.Host
// to an IP would make the certificate check fail or, worse, pass against a
// certificate for the wrong name. So the request keeps the name, and the
// dialer, told the target through the context, connects to the address
// instead. It does so only when the requested host IS the target's name, so a
// same-origin redirect still lands on the same replica and any other host
// dials normally.
//
// Keep-alives are off because the transport pools connections by URL host,
// and two attempts against the same name but different addresses would
// otherwise share one connection to whichever address dialed first. That
// would make PolicyAll ask one replica twice and report two answers, which
// is precisely the blind spot a fork check must not have. No proxy for the
// same reason: a proxy chooses the address, and then this package does not.
func (c *Client) transport() *http.Client {
	c.once.Do(func() {
		var t *http.Transport
		if c.HTTP != nil {
			if bt, ok := c.HTTP.Transport.(*http.Transport); ok {
				t = bt.Clone()
			}
		}
		if t == nil {
			t = http.DefaultTransport.(*http.Transport).Clone()
		}
		inner := t.DialContext
		if inner == nil {
			inner = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		}
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if h, ok := ctx.Value(targetKey{}).(Host); ok {
				if name, _, err := net.SplitHostPort(addr); err == nil && strings.EqualFold(name, h.Name) {
					addr = h.Addr
				}
			}
			return inner(ctx, network, addr)
		}
		t.DisableKeepAlives = true
		t.Proxy = nil
		cl := &http.Client{Transport: t, CheckRedirect: sameOriginRedirect}
		if c.HTTP != nil {
			cl.Timeout = c.HTTP.Timeout
			if c.HTTP.CheckRedirect != nil {
				cl.CheckRedirect = c.HTTP.CheckRedirect
			}
		}
		c.client = cl
	})
	return c.client
}

// sameOriginRedirect is the redirect policy when HTTP supplies none: within
// the origin, at most five hops. It repeats resolve's rule rather than
// importing it, so that this package stays below resolve in the dependency
// order and neither needs the other.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("hostset: more than 5 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		return fmt.Errorf("hostset: redirect to another origin refused: %s://%s -> %s://%s", first.Scheme, first.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// Do resolves base to hosts, runs build against each in policy order and
// returns every attempt made, in that order. The error, when there is one,
// names each host that failed and why.
//
// build is called once per attempt so that each request has a fresh body; a
// request built once and reused would arrive at the second host with its
// body already consumed by the first. Build the request against h.Base.
func (c *Client) Do(ctx context.Context, base string, build func(h Host) (*http.Request, error)) ([]Result, error) {
	src := c.Source
	if src == nil {
		src = DNS{}
	}
	hosts, err := src.Hosts(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("hostset: %s: %w", base, err)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("hostset: %s: no hosts", base)
	}
	switch c.Policy {
	case "", PolicyFirst, PolicyAll:
	case PolicyRandom:
		rand.Shuffle(len(hosts), func(i, j int) { hosts[i], hosts[j] = hosts[j], hosts[i] })
	default:
		return nil, fmt.Errorf("hostset: unknown policy %q", c.Policy)
	}
	need := max(c.Quorum, 1)
	if need > len(hosts) {
		return nil, fmt.Errorf("hostset: %s: quorum %d exceeds the %d host(s) the name resolves to", base, need, len(hosts))
	}
	all := c.Policy == PolicyAll
	var results []Result
	got := 0
	for _, h := range hosts {
		r := c.attempt(ctx, h, build)
		results = append(results, r)
		if r.Err == nil {
			got++
			if !all && got >= need {
				break
			}
		}
		if ctx.Err() != nil {
			// The caller's context is done; every further attempt would
			// fail the same way and only pad the report.
			break
		}
	}
	if got < need {
		return results, shortfall(base, results, got, need)
	}
	return results, nil
}

func (c *Client) attempt(ctx context.Context, h Host, build func(Host) (*http.Request, error)) (r Result) {
	r.Host = h
	start := time.Now()
	defer func() { r.Elapsed = time.Since(start) }()
	req, err := build(h)
	if err != nil {
		r.Err = err
		return r
	}
	t := c.Timeout
	if t <= 0 {
		t = 15 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	req = req.WithContext(context.WithValue(actx, targetKey{}, h))
	resp, err := c.transport().Do(req)
	if err != nil {
		r.Err = err
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	limit := c.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		r.Err = err
		return r
	}
	r.Body = body
	if resp.StatusCode >= 500 {
		r.Err = fmt.Errorf("status %d", resp.StatusCode)
	}
	return r
}

// shortfall names every host that failed. A count alone would send the
// operator back to repeat the call, traced, to learn which replica is down.
func shortfall(base string, results []Result, got, need int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostset: %s: %d of %d host(s) answered, need %d", base, got, len(results), need)
	sep := ": "
	for _, r := range results {
		if r.Err == nil {
			continue
		}
		fmt.Fprintf(&b, "%s%s@%s: %v", sep, r.Host.Name, r.Host.Addr, r.Err)
		sep = "; "
	}
	return errors.New(b.String())
}

// readBounded reads at most limit bytes and refuses a longer body rather than
// truncating it, since a truncated BEEF fails to parse with an error that
// names the wrong problem.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, limit)
	}
	return b, nil
}
