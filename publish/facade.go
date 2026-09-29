package publish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Facade is the object leg: POST <Base>/submit with a raw BEEF body and one
// topic. It owns its own *http.Client and touches no net.Conn of the
// settlement leg.
//
// The POST is hand-rolled rather than go-sdk's HTTPSOverlayBroadcastFacilitator
// because that one sets x-topics to a JSON array, which not every overlay
// server reads: go-overlay-services before v1.3.6 splits the header on commas
// without trimming, so the brackets and quotes stay in the name and no topic
// matches. It also mis-decodes the wrapped answer. One bare name reads the
// same on every server.
type Facade struct {
	// Base is the facade's root with no path suffix.
	Base string
	// HTTP is optional; the default has a 30s timeout.
	HTTP *http.Client
}

// Result is what the host answered for the one topic submitted.
type Result struct {
	// Admitted are the output indexes the topic manager admitted.
	Admitted []uint32
	// Retained are the input outpoints (as indexes) the manager kept.
	Retained []uint32
	// Duplicate is HTTP 200 with nothing admitted: the engine's answer for
	// an object it already holds. It is a result, not an error, so a
	// re-POST after a crash is idempotent.
	Duplicate bool
	// Raw is the whole response body, for the journal.
	Raw json.RawMessage
}

// ErrNotBEEF is returned by Submit for a body that does not start with a
// BEEF marker. Nothing else may go down the object leg.
var ErrNotBEEF = errors.New("publish: body is not a BEEF")

// IsBEEF reports whether b begins with a BEEF V1, V2 or Atomic BEEF marker
// (the SDK's constants, little-endian on the wire).
func IsBEEF(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	switch binary.LittleEndian.Uint32(b[:4]) {
	case transaction.BEEF_V1, transaction.BEEF_V2, transaction.ATOMIC_BEEF:
		return true
	}
	return false
}

// maxSteak bounds the answer before it is parsed.
const maxSteak = 1 << 20

// ErrBodyTooLarge refuses a response above the bound.
var ErrBodyTooLarge = errors.New("publish: response body exceeds the bound")

// readAtMost reads up to limit+1 bytes WITHOUT judging the length, so the
// caller can act on the status code first. Refusing an oversized body before
// the status is read turns every padded error page into a size error and
// throws away the distinction the status carries, which is exactly the
// distinction this package is careful about elsewhere.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit+1))
}

// overBound reports a body that exceeded the bound. It is called once the
// status has been handled. Refusing rather than truncating is the point: a
// truncated answer parses as the wrong thing, or fails to parse with an
// error that names the wrong problem.
func overBound(b []byte, limit int64) error {
	if int64(len(b)) > limit {
		return fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, limit)
	}
	return nil
}

// noProxyTransport is the transport behind the default client, and it takes no
// proxy. The object leg POSTs the publisher's BEEF and believes what comes
// back about what was admitted, so HTTP_PROXY in the environment would put a
// host that appears nowhere in the caller's configuration in both halves of
// that exchange. resolve states the rule for discovery and hostset repeats it
// for host reads; the leg that carries transactions does not get the looser
// one. A deployment that needs a proxy sets Facade.HTTP, where the choice is
// visible.
//
// One transport for the package, as http.DefaultTransport is, so a republish
// run reuses one connection pool.
var noProxyTransport = &http.Transport{
	Proxy: nil, // see the doc comment
	// Setting TLSClientConfig or DialContext disables automatic HTTP/2 unless
	// this is set, and the http.DefaultTransport these replaced had it on.
	// Losing a protocol version as a side effect of pinning a proxy policy is
	// the kind of change nobody notices until a deployment is slower.
	ForceAttemptHTTP2:   true,
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	TLSHandshakeTimeout: 10 * time.Second,
	MaxIdleConns:        4,
	IdleConnTimeout:     30 * time.Second,
}

// httpClient returns the caller's client when there is one, so a test or a
// deployment can supply its own policy, and otherwise the default.
func (f *Facade) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Transport: noProxyTransport, Timeout: 30 * time.Second}
}

type steakEntry struct {
	OutputsToAdmit []uint32 `json:"outputsToAdmit"`
	CoinsToRetain  []uint32 `json:"coinsToRetain"`
}

// Submit posts beef to the facade for exactly one topic.
//
// One topic per call, because a Result is one topic's answer. Submit refuses
// an empty topic, or one containing a comma, space, tab, CR or LF, before it
// sends anything: a server splits x-topics on commas, and servers differ on
// whether they trim the parts, so "a, b" can reach the topic lookup as "a"
// and " b". The header is sent exactly as given.
func (f *Facade) Submit(ctx context.Context, topic string, beef []byte) (Result, error) {
	if !IsBEEF(beef) {
		return Result{}, ErrNotBEEF
	}
	if topic == "" || strings.ContainsAny(topic, ", \t\r\n") {
		return Result{}, fmt.Errorf("publish: topic %q must be one name with no separators", topic)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.Base, "/")+"/submit", bytes.NewReader(beef))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-topics", topic)

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("publish: facade: %w", err)
	}
	defer resp.Body.Close()
	body, err := readAtMost(resp.Body, maxSteak)
	if err != nil {
		return Result{}, fmt.Errorf("publish: facade: read: %w", err)
	}
	// The named status branches come first so an operator reads "refused with
	// 401" rather than a size error, whatever the size of the error page.
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Auth is a transport matter until the facade's contract says
		// otherwise; named so the operator sees which.
		return Result{}, fmt.Errorf("publish: facade refused with %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), trim(body))
	case resp.StatusCode/100 == 5:
		return Result{}, fmt.Errorf("publish: facade failed with %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), trim(body))
	case resp.StatusCode != http.StatusOK:
		return Result{}, fmt.Errorf("publish: facade answered %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), trim(body))
	}
	if err := overBound(body, maxSteak); err != nil {
		return Result{}, fmt.Errorf("publish: facade: %w", err)
	}
	entry, err := decodeSteak(body, topic)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Admitted:  entry.OutputsToAdmit,
		Retained:  entry.CoinsToRetain,
		Duplicate: len(entry.OutputsToAdmit) == 0,
		Raw:       json.RawMessage(body),
	}
	return res, nil
}

// decodeSteak reads the wrapped form first ({"STEAK":{topic:{...}}}) and then
// the bare form ({topic:{...}}). The Go server wraps the answer in STEAK and
// the TypeScript host does not, so both forms are read.
func decodeSteak(body []byte, topic string) (steakEntry, error) {
	var wrapped struct {
		STEAK map[string]steakEntry `json:"STEAK"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.STEAK != nil {
		if e, ok := wrapped.STEAK[topic]; ok {
			return e, nil
		}
		return steakEntry{}, fmt.Errorf("publish: STEAK has no entry for topic %q: %s", topic, trim(body))
	}
	var bare map[string]json.RawMessage
	if err := json.Unmarshal(body, &bare); err != nil {
		return steakEntry{}, fmt.Errorf("publish: facade answered 200 with a body that is not a STEAK: %w: %s", err, trim(body))
	}
	raw, ok := bare[topic]
	if !ok {
		return steakEntry{}, fmt.Errorf("publish: STEAK has no entry for topic %q: %s", topic, trim(body))
	}
	var e steakEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return steakEntry{}, fmt.Errorf("publish: STEAK entry for %q: %w", topic, err)
	}
	return e, nil
}

func trim(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
