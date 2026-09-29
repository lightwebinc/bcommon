package resolve

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Handle is a resolved handle: what the domain says the identity key is.
//
// CertificateChecked is false in every Handle this build returns. The field
// exists so that a caller cannot mistake "the domain said so" for "the
// certifier signed it"; see VerifyHandleCertificate.
type Handle struct {
	Acct
	IdentityKey [33]byte
	// Certificate is the BRC-52 handle certificate exactly as served, and
	// unverified.
	Certificate json.RawMessage
	Messagebox  string
	// TTL is the caching bound in seconds (section 5.4): binding upward,
	// advisory downward.
	TTL     int
	Revoked bool
	// CertificateChecked reports whether Certificate has been verified
	// against the domain's certifier key. Never true in this build.
	CertificateChecked bool
}

// RevokedError is ErrRevoked with the forwarding record, if the domain served
// one (section 4.3). errors.Is(err, ErrRevoked) matches it.
type RevokedError struct {
	Acct       Acct
	Status     int
	Forwarding json.RawMessage
}

func (e *RevokedError) Error() string {
	if len(e.Forwarding) > 0 {
		return fmt.Sprintf("resolve: %s: handle revoked (status %d), a forwarding record was served", e.Acct, e.Status)
	}
	return fmt.Sprintf("resolve: %s: handle revoked (status %d)", e.Acct, e.Status)
}

// Is makes errors.Is(err, ErrRevoked) true for a RevokedError.
func (e *RevokedError) Is(target error) bool { return target == ErrRevoked }

// apiError is the error object of section 5.3.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) suffix() string {
	if e == nil || e.Code == "" {
		return ""
	}
	return " (" + e.Code + ")"
}

const wellKnownResolve = "/.well-known/metanet-handles/resolve"

// ResolveHandle queries the domain's handle-resolution endpoint (section 5.2)
// and returns what it claims about a.
//
// The echo check is the one that matters. The endpoint must echo the handle
// and domain it resolved, and a static file that returns the same document
// for every handle passes every other check in this function: valid JSON,
// valid key, valid version. Requiring the echo to equal what was asked is how
// that file is caught here rather than trusted.
func ResolveHandle(ctx context.Context, c *http.Client, m *Manifest, a Acct) (*Handle, error) {
	if m == nil || m.Handles == nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, ErrNoHandles)
	}
	if err := checkMajor(m.Handles.Version, "metanet.handles.version"); err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, err)
	}
	u, err := resolveURL(m, a)
	if err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, err)
	}
	status, body, err := get(ctx, c, u)
	if err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, err)
	}
	var doc struct {
		MetanetHandles string          `json:"metanetHandles"`
		Handle         string          `json:"handle"`
		Domain         string          `json:"domain"`
		IdentityKey    string          `json:"identityKey"`
		Certificate    json.RawMessage `json:"certificate"`
		Messagebox     string          `json:"messagebox"`
		TTL            int             `json:"ttl"`
		Revoked        bool            `json:"revoked"`
		Forwarding     json.RawMessage `json:"forwarding"`
		Error          *apiError       `json:"error"`
	}
	// A non-JSON body on a non-200 is still a usable outcome: the status
	// names it (section 5.3) and the body only refines the name.
	jsonErr := json.Unmarshal(body, &doc)
	switch status {
	case http.StatusOK:
	case http.StatusGone:
		return nil, &RevokedError{Acct: a, Status: status, Forwarding: doc.Forwarding}
	case http.StatusNotFound:
		return nil, fmt.Errorf("resolve: %s: status 404%s: %w", a, doc.Error.suffix(), ErrNotFound)
	default:
		return nil, fmt.Errorf("resolve: %s: status %d%s", a, status, doc.Error.suffix())
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("resolve: %s: answer is not the expected JSON: %w", a, jsonErr)
	}
	if err := checkMajor(doc.MetanetHandles, "metanetHandles"); err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, err)
	}
	if doc.Handle != a.Handle || doc.Domain != a.Domain {
		return nil, fmt.Errorf("resolve: %s: endpoint echoed %q at %q, not the handle asked for; refusing an answer that was not for this handle", a, doc.Handle, doc.Domain)
	}
	if doc.Revoked {
		// A 200 MUST carry revoked:false (section 5.2). A true is still a
		// revocation, whatever status it arrived under.
		return nil, &RevokedError{Acct: a, Status: status, Forwarding: doc.Forwarding}
	}
	key, err := parseIdentityKey(doc.IdentityKey)
	if err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", a, err)
	}
	return &Handle{
		Acct:        a,
		IdentityKey: key,
		Certificate: doc.Certificate,
		Messagebox:  doc.Messagebox,
		TTL:         doc.TTL,
	}, nil
}

// resolveURL is section 5.1's rule: the manifest's resolve URL when present,
// else the well-known path at the domain, in both cases HTTPS.
func resolveURL(m *Manifest, a Acct) (*url.URL, error) {
	var u *url.URL
	if m.Handles.Resolve != "" {
		var err error
		u, err = url.Parse(m.Handles.Resolve)
		if err != nil {
			return nil, fmt.Errorf("metanet.handles.resolve %q: %w", m.Handles.Resolve, err)
		}
		if u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("%w: metanet.handles.resolve %q", ErrNotHTTPS, m.Handles.Resolve)
		}
	} else {
		u = &url.URL{Scheme: "https", Host: a.Domain, Path: wellKnownResolve}
	}
	q := u.Query()
	q.Set("handle", a.Handle)
	u.RawQuery = q.Encode()
	return u, nil
}

// checkMajor is section 5.1's versioning rule: a major this build does not
// implement is refused rather than guessed at; a minor it does not know is
// not.
func checkMajor(v, what string) error {
	major, _, _ := strings.Cut(v, ".")
	if major != "1" {
		return fmt.Errorf("%s %q: major version is not 1, the only one this build implements", what, v)
	}
	return nil
}

// parseIdentityKey requires the 66-hex-character compressed secp256k1 form:
// 33 bytes with a 02 or 03 prefix. Whether the point is on the curve is for
// whoever uses the key to verify a signature; here the point is to refuse an
// uncompressed or truncated key before it can reach a pin store.
func parseIdentityKey(s string) ([33]byte, error) {
	var key [33]byte
	if len(s) != 66 {
		return key, fmt.Errorf("identityKey %q is %d characters, want 66 (33-byte compressed key, hex)", s, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return key, fmt.Errorf("identityKey %q is not hex: %w", s, err)
	}
	if b[0] != 0x02 && b[0] != 0x03 {
		return key, fmt.Errorf("identityKey %q is not a compressed key (prefix %02x, want 02 or 03)", s, b[0])
	}
	copy(key[:], b)
	return key, nil
}

// VerifyHandleCertificate is the seam for BRC-169 section 4.1. It would check
// that h.Certificate is a BRC-52 certificate whose subject equals
// h.IdentityKey, whose certifier is m.Trust.PublicKey, whose type is the
// handle certificate type of section 4.5, whose fields bind the handle and
// domain, and whose revocation outpoint is unspent (section 4.2) at the
// freshness the pending action needs; on success it would set
// h.CertificateChecked.
//
// It is the seam the BRC-52 verifier fills, exported so that a caller can
// gate an action on it and gain the check without changing its own code.
// Until the verifier is in place it always refuses: it returns
// ErrCertificateUnverified unconditionally and never sets
// CertificateChecked. Meanwhile an identity key from ResolveHandle is
// attested by the domain's resolution endpoint alone, which is exactly the
// trust a key pin is there to bound.
func VerifyHandleCertificate(h *Handle, m *Manifest) error {
	return ErrCertificateUnverified
}
