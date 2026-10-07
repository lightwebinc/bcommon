package feepolicy

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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/mint"
)

// Policy is what a broadcaster's GET /v1/policy says, held to the guards'
// validation: the mining fee rate and the size limits it states.
type Policy struct {
	Rate mint.Rate
	// MaxTxSize and MaxScriptSize are maxtxsizepolicy and
	// maxscriptsizepolicy; zero when not stated. Across several answers,
	// the lowest stated.
	MaxTxSize     uint64
	MaxScriptSize uint64
}

// Status is what an ARC source last used, for a metric or a status line.
type Status struct {
	// Source is SourceARC (a fresh answer), SourceCache (the last good
	// answer, the endpoints failing) or SourceStatic (the static rate:
	// never answered, or the last answer is older than Stale).
	Source string
	// Age is how old the policy in use is; zero for static.
	Age  time.Duration
	Rate mint.Rate
	// Err is the last fetch's failure, nil after a good one.
	Err error
}

// Defaults for an ARC source's zero fields.
const (
	DefaultTTL     = 5 * time.Minute
	DefaultStale   = 24 * time.Hour
	DefaultTimeout = 5 * time.Second
)

// maxPolicyBody bounds a policy answer, which is a few hundred bytes.
const maxPolicyBody = 64 << 10

// ARC is a Source over broadcasters' published policy. It asks each URL's
// GET /v1/policy (a URL with a path, such as https://arc.example.com/v1,
// is asked at that path plus /policy), takes the highest rate of those that
// answered (a transaction must be accepted by whichever is used), holds it
// to [Min, Max], and returns Base with that rate. One fetch per TTL; a
// failed fetch answers the last good policy while it is younger than
// Stale, and Base itself after that. Fees never fails on the network: a
// mint is never blocked on a policy fetch.
//
// Safe for concurrent use.
type ARC struct {
	URLs []string
	// Key, when set, is sent as a bearer token.
	Key string
	// Base is the fees returned with the policy's rate in place of its
	// own: floor, dust and per-transaction maximum, and the rate used when
	// no policy is at hand.
	Base mint.Fees
	// Min and Max bound the policy's rate; zero is DefaultMinRate and
	// DefaultMaxRate.
	Min, Max mint.Rate
	// TTL, Stale and Timeout default to DefaultTTL, DefaultStale and
	// DefaultTimeout.
	TTL, Stale, Timeout time.Duration
	// Client is optional; the default takes no proxy from the environment.
	Client *http.Client
	// Note, when set, receives a line on a failed fetch or a clamped rate.
	Note func(format string, args ...any)
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	last    *Policy
	lastAt  time.Time
	tried   time.Time
	lastErr error
	status  Status
}

var _ Source = (*ARC)(nil)

func (a *ARC) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *ARC) note(format string, args ...any) {
	if a.Note != nil {
		a.Note(format, args...)
	}
}

func orDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (a *ARC) bounds() (mint.Rate, mint.Rate) {
	lo, hi := a.Min, a.Max
	if lo.IsZero() {
		lo = DefaultMinRate
	}
	if hi.IsZero() {
		hi = DefaultMaxRate
	}
	return lo, hi
}

// Fees returns Base with the policy's rate, guarded. Its error is only a
// Base that cannot be used.
func (a *ARC) Fees(ctx context.Context) (mint.Fees, error) {
	f := a.Base
	if _, err := f.For(0); err != nil && !errors.Is(err, mint.ErrFeeTooHigh) {
		return mint.Fees{}, err
	}
	p, src, age := a.current(ctx)
	lo, hi := a.bounds()
	if p != nil {
		r := p.Rate.Clamp(lo, hi)
		if r != p.Rate {
			a.note("fee policy: rate %s held to %s (bounds %s to %s)", p.Rate, r, lo, hi)
		}
		f.Rate, f.SatPerByte = r, 0
	}
	// The maximum also bounds a static rate, and the build applies it.
	if f.MaxRate.IsZero() || f.MaxRate.Cmp(hi) > 0 {
		f.MaxRate = hi
	}
	a.mu.Lock()
	a.status = Status{Source: src, Age: age, Rate: f.Rate, Err: a.lastErr}
	if f.Rate.IsZero() {
		a.status.Rate = mint.Rate{Sats: f.SatPerByte, Bytes: 1}
	}
	a.mu.Unlock()
	return f, nil
}

// Status is what the last Fees used.
func (a *ARC) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// Policy is the policy in use: fresh, cached, or nil when there is none
// younger than Stale (Base's rate is then in force).
func (a *ARC) Policy(ctx context.Context) *Policy {
	p, _, _ := a.current(ctx)
	return p
}

// current returns the policy to use, fetching when the cached one is older
// than TTL, and the source label and age.
func (a *ARC) current(ctx context.Context) (*Policy, string, time.Duration) {
	ttl, stale := orDur(a.TTL, DefaultTTL), orDur(a.Stale, DefaultStale)
	a.mu.Lock()
	now := a.now()
	if a.last != nil && now.Sub(a.lastAt) < ttl {
		p, age := *a.last, now.Sub(a.lastAt)
		a.mu.Unlock()
		return &p, SourceARC, age
	}
	// One fetch per TTL even when it fails, so a dead endpoint costs one
	// timeout per TTL, not one per mint.
	if !a.tried.IsZero() && now.Sub(a.tried) < ttl {
		p, src, age := a.fallbackLocked(now, stale)
		a.mu.Unlock()
		return p, src, age
	}
	a.tried = now
	a.mu.Unlock()

	p, err := a.fetch(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	now = a.now()
	if err == nil {
		a.last, a.lastAt, a.lastErr = p, now, nil
		cp := *p
		return &cp, SourceARC, 0
	}
	a.lastErr = err
	a.note("fee policy: %v", err)
	return a.fallbackLocked(now, stale)
}

func (a *ARC) fallbackLocked(now time.Time, stale time.Duration) (*Policy, string, time.Duration) {
	if a.last != nil && now.Sub(a.lastAt) < stale {
		p := *a.last
		return &p, SourceCache, now.Sub(a.lastAt)
	}
	return nil, SourceStatic, 0
}

// fetch asks every URL at once and combines the answers that validate:
// the highest rate, the lowest stated size limits. It fails only when none
// does.
func (a *ARC) fetch(ctx context.Context) (*Policy, error) {
	if len(a.URLs) == 0 {
		return nil, errors.New("no policy URL")
	}
	ctx, cancel := context.WithTimeout(ctx, orDur(a.Timeout, DefaultTimeout))
	defer cancel()
	type answer struct {
		p   *Policy
		err error
	}
	answers := make([]answer, len(a.URLs))
	var wg sync.WaitGroup
	for i, u := range a.URLs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := a.fetchOne(ctx, u)
			answers[i] = answer{p, err}
		}()
	}
	wg.Wait()
	var out *Policy
	var errs []error
	for _, an := range answers {
		if an.err != nil {
			errs = append(errs, an.err)
			continue
		}
		if out == nil {
			cp := *an.p
			out = &cp
			continue
		}
		if an.p.Rate.Cmp(out.Rate) > 0 {
			out.Rate = an.p.Rate
		}
		out.MaxTxSize = lowestStated(out.MaxTxSize, an.p.MaxTxSize)
		out.MaxScriptSize = lowestStated(out.MaxScriptSize, an.p.MaxScriptSize)
	}
	if out == nil {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func lowestStated(a, b uint64) uint64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// ErrPolicy is a policy answer that does not hold to the validation.
var ErrPolicy = errors.New("feepolicy: policy answer refused")

// The range a published rate must fall in: whole numbers, at least one
// satoshi, and 1 to 1e9 bytes.
const maxPolicyBytes = 1_000_000_000

// ParsePolicy reads a GET /v1/policy answer: policy.miningFee as whole
// numbers, satoshis at least 1 and bytes 1 to 1e9, and the size limits
// when stated. Anything else is ErrPolicy.
func ParsePolicy(b []byte) (*Policy, error) {
	var w struct {
		Policy *struct {
			MiningFee *struct {
				Satoshis json.RawMessage `json:"satoshis"`
				Bytes    json.RawMessage `json:"bytes"`
			} `json:"miningFee"`
			MaxTxSize     json.RawMessage `json:"maxtxsizepolicy"`
			MaxScriptSize json.RawMessage `json:"maxscriptsizepolicy"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicy, err)
	}
	if w.Policy == nil || w.Policy.MiningFee == nil {
		return nil, fmt.Errorf("%w: no policy.miningFee", ErrPolicy)
	}
	sats, err1 := whole(w.Policy.MiningFee.Satoshis)
	byts, err2 := whole(w.Policy.MiningFee.Bytes)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("%w: miningFee: %v", ErrPolicy, err)
	}
	if sats < 1 || byts < 1 || byts > maxPolicyBytes {
		return nil, fmt.Errorf("%w: miningFee %d/%d is out of range (satoshis at least 1, bytes 1 to 1e9)", ErrPolicy, sats, byts)
	}
	p := &Policy{Rate: mint.Rate{Sats: sats, Bytes: byts}}
	var err error
	if len(w.Policy.MaxTxSize) > 0 && string(w.Policy.MaxTxSize) != "null" {
		if p.MaxTxSize, err = whole(w.Policy.MaxTxSize); err != nil {
			return nil, fmt.Errorf("%w: maxtxsizepolicy: %v", ErrPolicy, err)
		}
	}
	if len(w.Policy.MaxScriptSize) > 0 && string(w.Policy.MaxScriptSize) != "null" {
		if p.MaxScriptSize, err = whole(w.Policy.MaxScriptSize); err != nil {
			return nil, fmt.Errorf("%w: maxscriptsizepolicy: %v", ErrPolicy, err)
		}
	}
	return p, nil
}

// whole reads a JSON number that must be a non-negative integer written
// in digits: a string, a fraction, an exponent or a sign is refused.
func whole(n json.RawMessage) (uint64, error) {
	s := string(n)
	if s == "" || s == "null" {
		return 0, errors.New("missing")
	}
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q is not a whole number", s)
		}
		d := uint64(c - '0')
		if v > (1<<64-1-d)/10 {
			return 0, fmt.Errorf("%q overflows", s)
		}
		v = v*10 + d
	}
	return v, nil
}

// policyURL is where a broadcaster URL's policy is read: /v1/policy on a
// bare host, the URL's path plus /policy otherwise.
func policyURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%w: policy URL %q is not an http or https URL", ErrConfig, base)
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		p = "/v1"
	}
	u.Path = p + "/policy"
	return u.String(), nil
}

// noProxyTransport takes no proxy from the environment: the policy decides
// what every transaction pays, and HTTP_PROXY would let whoever set it
// answer.
var noProxyTransport = &http.Transport{
	Proxy:               nil,
	ForceAttemptHTTP2:   true,
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	TLSHandshakeTimeout: 5 * time.Second,
	MaxIdleConns:        4,
	IdleConnTimeout:     30 * time.Second,
}

func (a *ARC) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Transport: noProxyTransport, Timeout: orDur(a.Timeout, DefaultTimeout)}
}

func (a *ARC) fetchOne(ctx context.Context, base string) (*Policy, error) {
	u, err := policyURL(base)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if a.Key != "" {
		req.Header.Set("Authorization", "Bearer "+a.Key)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPolicyBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	if len(body) > maxPolicyBody {
		return nil, fmt.Errorf("%s: %w: answer over %d bytes", u, ErrPolicy, maxPolicyBody)
	}
	p, err := ParsePolicy(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	return p, nil
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
