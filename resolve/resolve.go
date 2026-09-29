// Package resolve turns a user@domain into an identity key and the overlay
// host that serves it, using only the two documents a domain publishes about
// itself: its BRC-180 manifest and its BRC-169 handle-resolution endpoint.
//
// The domain is the trust anchor and nothing else is. A manifest says what a
// domain CLAIMS to host and a resolution answer says what it CLAIMS a handle's
// key is; both are secured by control of the domain and by nothing stronger.
// Every later step a reader takes (signature, sequence, proof, pin) exists
// because this step proves so little, so this package is deliberately literal:
// it fetches exactly the documents the specifications name, refuses anything
// that does not match what was asked, and never probes.
//
// HTTP here is the one place a reader reaches beyond the host and header
// service it was configured with, so the client policy is part of the security
// posture rather than a convenience: system roots only, HTTPS only, no
// cross-origin redirects, bounded bodies. NewHTTPClient is that policy in one
// place.
package resolve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrNotHTTPS refuses a URL that is not https. RFC 7033 and BRC-169 both
	// require HTTPS for discovery, and a manifest that points resolution at
	// http:// has asked the client to accept a key that anyone on the path
	// could have written.
	ErrNotHTTPS = errors.New("not an https URL")
	// ErrCrossOriginRedirect refuses a redirect that leaves the origin the
	// request was made to. Following one would let a domain hand its
	// authority to a host the user never named.
	ErrCrossOriginRedirect = errors.New("redirect to another origin refused")
	// ErrBodyTooLarge refuses a response above the bound.
	ErrBodyTooLarge = errors.New("response body exceeds the bound")
	// ErrNoHandles means the manifest has no metanet.handles object, so the
	// domain does not offer handle resolution. BRC-169 section 5.1 forbids
	// probing the well-known path in that case.
	ErrNoHandles = errors.New("domain does not offer handle resolution (no metanet.handles in its manifest)")
	// ErrNotFound is a 404: no such handle is registered at the domain.
	ErrNotFound = errors.New("handle is not registered at this domain")
	// ErrRevoked means the handle was registered and has been released or
	// revoked. The error may carry a forwarding record; see RevokedError.
	ErrRevoked = errors.New("handle revoked")
	// ErrCertificateUnverified is what VerifyHandleCertificate returns until
	// BRC-52 verification is implemented.
	ErrCertificateUnverified = errors.New("handle certificate not verified (verification is not implemented in this build)")
)

// maxBody bounds every response before it is parsed. A manifest is a few
// hundred bytes and a resolution answer with its certificate a few KiB; the
// bound is generous for both and small enough that a hostile domain cannot
// make the reader allocate on demand.
const maxBody = 256 << 10

// maxRedirects bounds a same-origin redirect chain.
const maxRedirects = 5

// NewHTTPClient returns a client that implements the discovery policy: system
// TLS roots only, no way to skip verification, redirects followed only within
// the origin of the first request, and no proxy.
//
// No proxy is a choice, not an omission. HTTP_PROXY in the environment would
// route discovery through a host that appears nowhere in the caller's
// configuration or its trace, and a run should reach exactly what it was
// configured to reach.
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	t := &http.Transport{
		Proxy:               nil, // see the doc comment
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: timeout,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{
		Transport:     t,
		Timeout:       timeout,
		CheckRedirect: sameOriginRedirect,
	}
}

// sameOriginRedirect is the CheckRedirect policy: a redirect may move within
// the origin it started at and nowhere else.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("more than %d redirects", maxRedirects)
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		return fmt.Errorf("%w: %s://%s -> %s://%s", ErrCrossOriginRedirect, first.Scheme, first.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// get performs one bounded HTTPS GET and returns the status and body. Status
// handling belongs to the caller: which non-200s mean what is specified per
// endpoint, not per transport.
func get(ctx context.Context, c *http.Client, u *url.URL) (int, []byte, error) {
	if u.Scheme != "https" {
		return 0, nil, fmt.Errorf("%w: %s", ErrNotHTTPS, u.Redacted())
	}
	if c == nil {
		c = NewHTTPClient(0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body, maxBody)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// readBounded reads at most limit bytes and refuses a body that is longer,
// rather than silently truncating it: a truncated JSON document fails to parse
// with an error that names the wrong problem.
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
