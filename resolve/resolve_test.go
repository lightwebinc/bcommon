package resolve

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The samples are the specifications' own. BRC-180's is verbatim; BRC-169's
// have their names replaced by documentation names and are otherwise as
// written. Parsing them proves the struct tags agree with the documents
// rather than with this package.
const brc180Sample = `{
  "metanet": {
    "overlays": {
      "tm_example": "https://api.example.com",
      "ls_example": "https://api.example.com",
      "tm_other": "https://other.example.net/overlay"
    }
  }
}`

const brc169Sample = `{
  "name": "Example",
  "metanet": {
    "trust": {
      "name": "Example",
      "note": "Handle registry and messagebox for the Example ecosystem",
      "icon": "https://example.com/icon.png",
      "publicKey": "0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9"
    },
    "handles": {
      "version": "1.0",
      "resolve": "https://example.com/.well-known/metanet-handles/resolve",
      "search": "https://example.com/.well-known/metanet-handles/search",
      "reverse": "https://example.com/.well-known/metanet-handles/reverse",
      "messagebox": "https://messagebox.example.com",
      "aliases": ["example"],
      "commands": []
    }
  }
}`

// identityKeyHex is the A.4 sample key.
const identityKeyHex = "0359c5f3bfe249f6c0ca99d0e9cc1517da51a511f3d04f18e47a5d7ae55f04008c"

// forwardingSample is A.8, renamed as above. Its signature was made over the
// original names and no longer verifies; nothing in this package checks it.
const forwardingSample = `{"from":"@bob@example.com","toIdentityKey":"033cef496cd596a9dab13b36335cf51383322056f118c91e71d80be69b87e5db66","toHandle":"@bob@nexus.example","created":"2027-01-15T00:00:00Z","signature":"3045022100f4c27b2052e40565134c91e14e530fd0831c2043b5e698284f2a76d4383c509602201598a61ea25dd46f4e5f6f64d9ca54fdeac71bc34df048e685c5d8f77bf69d83"}`

func TestParseAcct(t *testing.T) {
	long := strings.Repeat("a", 64)
	cases := []struct {
		in   string
		want Acct
		bad  string // substring of the error, or "" for success
	}{
		{"alice@example.com", Acct{"alice", "example.com", ""}, ""},
		{"@alice@example.com", Acct{"alice", "example.com", ""}, ""},
		{"acct:alice@example.com", Acct{"alice", "example.com", ""}, ""},
		{"  Alice@Example.COM ", Acct{"alice", "example.com", ""}, ""},
		{"@bob+conf2036@example.org", Acct{"bob", "example.org", "conf2036"}, ""},
		{"alice+work@example.com", Acct{"alice", "example.com", "work"}, ""},
		{"a.b_c-d@sub.example.com", Acct{"a.b_c-d", "sub.example.com", ""}, ""},
		{"a@example.com", Acct{"a", "example.com", ""}, ""},
		{long + "@example.com", Acct{long, "example.com", ""}, ""},
		{long + "a@example.com", Acct{}, "limit is 64"},
		{"alice@example", Acct{}, "aliases are not supported"},
		{"@alice", Acct{}, "no @domain"},
		{"alice", Acct{}, "no @domain"},
		{"", Acct{}, "no @domain"},
		{"alice@", Acct{}, "no @domain"},
		{"alice@@example.com", Acct{}, "more than one @"},
		// "acct:" comes off before "@", and the tag is split at the first "+".
		{"acct:@alice@example.com", Acct{"alice", "example.com", ""}, ""},
		{"@acct:alice@example.com", Acct{}, `handle "acct:alice" contains`},
		{"alice+a+b@example.com", Acct{}, `tag "a+b" contains`},
		{".alice@example.com", Acct{}, "begin and end"},
		{"alice.@example.com", Acct{}, "begin and end"},
		{"al ice@example.com", Acct{}, "contains"},
		{"alice+@example.com", Acct{}, "tag is empty"},
		{"alice+" + strings.Repeat("t", 33) + "@example.com", Acct{}, "limit is 32"},
		{"alice@example.com:8443", Acct{}, "contains"},
		{"alice@-bad.example.com", Acct{}, "hyphen"},
		{"alice@exa mple.com", Acct{}, "contains"},
		{"alice@example..com", Acct{}, "empty label"},
		{"alice@example.com.", Acct{}, "empty label"},
		// Two rules broken at once: the check that runs first names it.
		{"al!ce", Acct{}, "no @domain"},
		{"al!ce@@example.com", Acct{}, "more than one @"},
		{"alice+w!rk@@example.com", Acct{}, "more than one @"},
		{"alice@@example", Acct{}, "more than one @"},
		{"al!ce+w!rk@example.com", Acct{}, `handle "al!ce" contains`},
		{"al!ce@example", Acct{}, `handle "al!ce" contains`},
		{"al!ce@-bad.example.com", Acct{}, `handle "al!ce" contains`},
		{"alice+w!rk@example", Acct{}, `tag "w!rk" contains`},
		{"alice+w!rk@-bad.example.com", Acct{}, `tag "w!rk" contains`},
		{"alice@exam!ple", Acct{}, "aliases are not supported"},
		{strings.Repeat("!", 65) + "@example.com", Acct{}, "handle is 65 characters"},
		{"alice@" + strings.Repeat("a.", 126) + ".ab", Acct{}, "domain is 255 characters"},
		{"alice@-" + strings.Repeat("l", 63) + ".com", Acct{}, "exceeds 63 characters"},
		{"alice@" + strings.Repeat("l", 63) + "!.com", Acct{}, "exceeds 63 characters"},
		{"alice@-b!d.example.com", Acct{}, "hyphen"},
		{"alice@b!d.-bad.com", Acct{}, `contains "!"`},
	}
	for _, c := range cases {
		got, err := ParseAcct(c.in)
		if c.bad == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("%q: got %+v, want %+v", c.in, got, c.want)
			}
			if got.String() != c.want.Handle+"@"+c.want.Domain {
				t.Errorf("%q: String() = %q", c.in, got.String())
			}
			continue
		}
		if err == nil {
			t.Errorf("%q: accepted as %+v, want an error mentioning %q", c.in, got, c.bad)
		} else if !strings.Contains(err.Error(), c.bad) {
			t.Errorf("%q: error %q does not mention %q", c.in, err, c.bad)
		}
	}
}

func TestManifestParsesTheBRC180Sample(t *testing.T) {
	m, err := ParseManifest([]byte(brc180Sample))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"tm_example": "https://api.example.com",
		"ls_example": "https://api.example.com",
		"tm_other":   "https://other.example.net/overlay",
	} {
		got, ok := m.Overlay(name)
		if !ok || got != want {
			t.Errorf("Overlay(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	if _, ok := m.Overlay("ls_undeclared"); ok {
		t.Error("an undeclared service resolved")
	}
	if m.Handles != nil || m.Trust != nil {
		t.Error("a BRC-180-only manifest grew handles or trust")
	}
}

func TestManifestParsesTheBRC169Sample(t *testing.T) {
	m, err := ParseManifest([]byte(brc169Sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.Overlays != nil {
		t.Errorf("Overlays = %v, want nil for a manifest without metanet.overlays", m.Overlays)
	}
	if m.Trust == nil || m.Trust.PublicKey != "0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9" || m.Trust.Name != "Example" {
		t.Errorf("Trust = %+v", m.Trust)
	}
	h := m.Handles
	if h == nil {
		t.Fatal("no Handles")
	}
	if h.Version != "1.0" ||
		h.Resolve != "https://example.com/.well-known/metanet-handles/resolve" ||
		h.Search != "https://example.com/.well-known/metanet-handles/search" ||
		h.Reverse != "https://example.com/.well-known/metanet-handles/reverse" ||
		h.Messagebox != "https://messagebox.example.com" ||
		len(h.Aliases) != 1 || h.Aliases[0] != "example" {
		t.Errorf("Handles = %+v", h)
	}
	if len(m.Raw) != len(brc169Sample) {
		t.Error("Raw is not the document as served")
	}
}

// tlsServer serves routes over TLS and returns a client that trusts its
// certificate and dials it for every name. The test certificate is issued for
// example.com, so tests speak to "example.com" and hostname verification is
// exercised for real rather than bypassed.
func tlsServer(t *testing.T, routes map[string]http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("%s was asked with Accept %q, want application/json", r.URL.Path, got)
		}
		h, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("content-type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewHTTPClient(5 * time.Second)
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return c
}

func manifestWith(handles string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metanet":{"trust":{"name":"Example","publicKey":"0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9"},"handles":` + handles + `}}`))
	}
}

// echo answers the A.4 document for whatever handle was asked, at the
// domain given, which is what a conforming endpoint does.
func echo(domain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"metanetHandles":"1.0","handle":"` + r.URL.Query().Get("handle") + `","domain":"` + domain +
			`","identityKey":"` + identityKeyHex + `","certificate":{"type":"handle"},"messagebox":"https://messagebox.example.com","ttl":3600,"revoked":false}`))
	}
}

func alice() Acct { return Acct{Handle: "alice", Domain: "example.com"} }

func TestResolveHappyPath(t *testing.T) {
	var asked atomic.Value
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0","resolve":"https://example.com/handles/resolve"}`),
		"/handles/resolve": func(w http.ResponseWriter, r *http.Request) {
			asked.Store(r.URL.Query().Get("handle"))
			echo("example.com")(w, r)
		},
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := ResolveHandle(context.Background(), c, m, alice())
	if err != nil {
		t.Fatal(err)
	}
	if asked.Load() != "alice" {
		t.Errorf("endpoint was asked for %v, want alice", asked.Load())
	}
	if hex.EncodeToString(h.IdentityKey[:]) != identityKeyHex {
		t.Errorf("IdentityKey = %x", h.IdentityKey)
	}
	if h.Messagebox != "https://messagebox.example.com" || h.TTL != 3600 || h.Revoked {
		t.Errorf("Handle = %+v", h)
	}
	if string(h.Certificate) != `{"type":"handle"}` {
		t.Errorf("Certificate = %s, want it raw", h.Certificate)
	}
	if h.CertificateChecked {
		t.Fatal("CertificateChecked is true and nothing checked it")
	}
	if err := VerifyHandleCertificate(h, m); !errors.Is(err, ErrCertificateUnverified) {
		t.Fatalf("VerifyHandleCertificate = %v, want ErrCertificateUnverified", err)
	}
	if h.CertificateChecked {
		t.Fatal("the seam set CertificateChecked")
	}
}

func TestResolveDefaultsToTheWellKnownPath(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"1.2"}`),
		"/.well-known/metanet-handles/resolve": echo("example.com"),
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveHandle(context.Background(), c, m, alice()); err != nil {
		t.Fatalf("a manifest without resolve, minor version 1.2: %v", err)
	}
}

// A static file that answers the same document for every handle passes every
// check but this one.
func TestResolveRefusesAnEchoMismatch(t *testing.T) {
	static := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metanetHandles":"1.0","handle":"bob","domain":"example.com","identityKey":"` + identityKeyHex + `","certificate":{},"messagebox":"https://mb.example.com","ttl":60,"revoked":false}`))
	}
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": static,
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	_, err := ResolveHandle(context.Background(), c, m, alice())
	if err == nil || !strings.Contains(err.Error(), `echoed "bob"`) {
		t.Fatalf("a document for another handle was accepted: %v", err)
	}

	// The domain is checked the same way.
	c = tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": echo("other.example"),
	})
	m, _ = FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); err == nil || !strings.Contains(err.Error(), `"other.example"`) {
		t.Fatalf("a document for another domain was accepted: %v", err)
	}
}

func TestResolveRefusesRevoked(t *testing.T) {
	gone := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"metanetHandles":"1.0","error":{"code":"handle-revoked","message":"released"},"forwarding":` + forwardingSample + `}`))
	}
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": gone,
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	_, err := ResolveHandle(context.Background(), c, m, alice())
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("410 was not ErrRevoked: %v", err)
	}
	var re *RevokedError
	if !errors.As(err, &re) || !strings.Contains(string(re.Forwarding), `"toHandle":"@bob@nexus.example"`) {
		t.Fatalf("the forwarding record did not travel with the error: %v", err)
	}

	// revoked:true under a 200 is non-conforming, and still a revocation.
	revokedOK := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"metanetHandles":"1.0","handle":"alice","domain":"example.com","identityKey":"` + identityKeyHex + `","certificate":{},"messagebox":"","ttl":0,"revoked":true}`))
	}
	c = tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": revokedOK,
	})
	m, _ = FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked:true under 200 was accepted: %v", err)
	}
}

func TestResolveRefusesAnHTTPResolveURL(t *testing.T) {
	var hits atomic.Int32
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0","resolve":"http://example.com/handles/resolve"}`),
		"/handles/resolve": func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			echo("example.com")(w, r)
		},
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveHandle(context.Background(), c, m, alice())
	if !errors.Is(err, ErrNotHTTPS) {
		t.Fatalf("an http:// resolve URL was not refused: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the http:// endpoint was contacted before being refused")
	}
}

func TestResolveRefusesACrossOriginRedirect(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://other.example/resolve?"+r.URL.RawQuery, http.StatusFound)
		},
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); !errors.Is(err, ErrCrossOriginRedirect) {
		t.Fatalf("a redirect to another origin was followed: %v", err)
	}
}

// The origin is the scheme as well as the host, so a downgrade to http on the
// same host is refused as another origin, for the manifest and the answer
// alike, and not followed to fail some other way.
func TestSchemeChangeRedirectIsRefused(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/manifest.json", http.StatusFound)
		},
	})
	_, err := FetchManifest(context.Background(), c, "example.com")
	want := `resolve: manifest for example.com: Get "http://example.com/manifest.json": redirect to another origin refused: https://example.com -> http://example.com`
	if !errors.Is(err, ErrCrossOriginRedirect) || err.Error() != want {
		t.Fatalf("manifest: got %v, want\n%q", err, want)
	}

	c = tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/resolve?"+r.URL.RawQuery, http.StatusFound)
		},
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveHandle(context.Background(), c, m, alice())
	want = `resolve: alice@example.com: Get "http://example.com/resolve?handle=alice": redirect to another origin refused: https://example.com -> http://example.com`
	if !errors.Is(err, ErrCrossOriginRedirect) || err.Error() != want {
		t.Fatalf("answer: got %v, want\n%q", err, want)
	}
}

func TestResolveFollowsASameOriginRedirect(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0","resolve":"https://example.com/old"}`),
		"/old": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/new?"+r.URL.RawQuery, http.StatusMovedPermanently)
		},
		"/new": echo("example.com"),
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"metanet":{"overlays":{"x":"` + strings.Repeat("a", maxBody) + `"}}}`))
		},
	})
	_, err := FetchManifest(context.Background(), c, "example.com")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("a %d+ byte manifest was read: %v", maxBody, err)
	}
}

func TestNoHandlesIsAnErrorNotAProbe(t *testing.T) {
	var probed atomic.Int32
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(brc180Sample)) },
		"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, r *http.Request) {
			probed.Add(1)
			echo("example.com")(w, r)
		},
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveHandle(context.Background(), c, m, alice()); !errors.Is(err, ErrNoHandles) {
		t.Fatalf("got %v, want ErrNoHandles", err)
	}
	if probed.Load() != 0 {
		t.Fatal("the well-known path was probed for a domain that does not offer handles")
	}
}

func TestUnsupportedMajorVersionIsRefused(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json":                       manifestWith(`{"version":"2.0"}`),
		"/.well-known/metanet-handles/resolve": echo("example.com"),
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); err == nil || !strings.Contains(err.Error(), "major version") {
		t.Fatalf("version 2.0 was accepted: %v", err)
	}
}

func TestBadIdentityKeysAreRefused(t *testing.T) {
	for _, bad := range []string{
		"04" + identityKeyHex[2:], // uncompressed prefix
		identityKeyHex[:64],       // 32 bytes
		"zz" + identityKeyHex[2:], // not hex
		"",
	} {
		answer := func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"metanetHandles":"1.0","handle":"alice","domain":"example.com","identityKey":"` + bad + `","certificate":{},"messagebox":"","ttl":0,"revoked":false}`))
		}
		c := tlsServer(t, map[string]http.HandlerFunc{
			"/manifest.json":                       manifestWith(`{"version":"1.0"}`),
			"/.well-known/metanet-handles/resolve": answer,
		})
		m, _ := FetchManifest(context.Background(), c, "example.com")
		if _, err := ResolveHandle(context.Background(), c, m, alice()); err == nil || !strings.Contains(err.Error(), "identityKey") {
			t.Errorf("identityKey %q was accepted: %v", bad, err)
		}
	}
}

func TestNon200NamesTheStatusAndCode(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"metanetHandles":"1.0","error":{"code":"handle-not-found","message":"No handle 'alice' is registered at example.com."}}`))
		},
	})
	m, _ := FetchManifest(context.Background(), c, "example.com")
	_, err := ResolveHandle(context.Background(), c, m, alice())
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "handle-not-found") {
		t.Fatalf("got %v", err)
	}

	c = tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0"}`),
		"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	})
	m, _ = FetchManifest(context.Background(), c, "example.com")
	if _, err := ResolveHandle(context.Background(), c, m, alice()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a 503 with no body did not name its status: %v", err)
	}
}

// The edges of the status and version rules, pinned whole because each one
// decides the text a caller shows: only a 200 is a manifest, a missing
// version is not version 1 and neither is a major that only begins with a 1,
// a message never stands in for a code, and a forwarding field that is
// present counts even when it is null.
func TestStatusAndVersionEdgesKeepTheirWords(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
	_, err := FetchManifest(context.Background(), c, "example.com")
	if want := "resolve: manifest for example.com: status 204"; err == nil || err.Error() != want {
		t.Fatalf("manifest 204: got %v, want\n%q", err, want)
	}

	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	for _, tc := range []struct {
		name, handles string
		answer        http.HandlerFunc
		want          string
	}{
		{"handles with no version", `{}`, echo("example.com"),
			`resolve: alice@example.com: metanet.handles.version "": major version is not 1, the only one this build implements`},
		{"answer with no version", `{"version":"1.0"}`,
			answer(http.StatusOK, `{"handle":"alice","domain":"example.com","identityKey":"`+identityKeyHex+`","revoked":false}`),
			`resolve: alice@example.com: metanetHandles "": major version is not 1, the only one this build implements`},
		{"handles with a two-digit major", `{"version":"10.0"}`, echo("example.com"),
			`resolve: alice@example.com: metanet.handles.version "10.0": major version is not 1, the only one this build implements`},
		{"answer with a two-digit major", `{"version":"1.0"}`,
			answer(http.StatusOK, `{"metanetHandles":"12.0","handle":"alice","domain":"example.com","identityKey":"`+identityKeyHex+`","revoked":false}`),
			`resolve: alice@example.com: metanetHandles "12.0": major version is not 1, the only one this build implements`},
		{"a message and no code", `{"version":"1.0"}`, answer(http.StatusServiceUnavailable, `{"error":{"message":"later"}}`),
			"resolve: alice@example.com: status 503"},
		{"a null forwarding", `{"version":"1.0"}`, answer(http.StatusGone, `{"forwarding":null}`),
			"resolve: alice@example.com: handle revoked (status 410), a forwarding record was served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tlsServer(t, map[string]http.HandlerFunc{
				"/manifest.json":                       manifestWith(tc.handles),
				"/.well-known/metanet-handles/resolve": tc.answer,
			})
			m, err := FetchManifest(context.Background(), c, "example.com")
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveHandle(context.Background(), c, m, alice())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want\n%q", err, tc.want)
			}
		})
	}
}

func TestFetchManifestRefusesANon200(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{})
	if _, err := FetchManifest(context.Background(), c, "example.com"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

// An https URL with no host is refused as not https, in the same words as an
// http one, and nothing is asked.
func TestResolveRefusesAnHTTPSURLWithNoHost(t *testing.T) {
	var hits atomic.Int32
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": manifestWith(`{"version":"1.0","resolve":"https:///resolve"}`),
		"/resolve": func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			echo("example.com")(w, r)
		},
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveHandle(context.Background(), c, m, alice())
	want := `resolve: alice@example.com: not an https URL: metanet.handles.resolve "https:///resolve"`
	if !errors.Is(err, ErrNotHTTPS) || err.Error() != want {
		t.Fatalf("got %v, want\n%q", err, want)
	}
	if hits.Load() != 0 {
		t.Fatal("a resolve URL with no host was asked")
	}
}

// An empty overlay entry names no base URL, so it reads as an absent one:
// the domain does not offer that service.
func TestAnEmptyOverlayIsNoOverlay(t *testing.T) {
	m, err := ParseManifest([]byte(`{"metanet":{"overlays":{"ls_example":"","tm_example":"https://api.example.com"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if base, ok := m.Overlay("ls_example"); ok || base != "" {
		t.Errorf("an empty entry resolved to %q, %v", base, ok)
	}
	if base, ok := m.Overlay("tm_example"); !ok || base != "https://api.example.com" {
		t.Errorf("the entry beside it resolved to %q, %v", base, ok)
	}
}

// Host names compare without case, so a redirect to the same host in
// capitals stays within the origin and is followed.
func TestSameOriginRedirectIgnoresHostCase(t *testing.T) {
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://EXAMPLE.COM/m2.json", http.StatusFound)
		},
		"/m2.json": manifestWith(`{"version":"1.0"}`),
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatalf("a redirect to the same host in capitals was refused: %v", err)
	}
	if m.Handles == nil || m.Handles.Version != "1.0" {
		t.Fatalf("the manifest redirected to was not read: %+v", m)
	}
}

// A body of exactly the bound is read and parsed; only a longer one is
// refused.
func TestBodyAtTheBoundIsRead(t *testing.T) {
	const head, tail = `{"metanet":{"overlays":{"x":"`, `"}}}`
	pad := strings.Repeat("a", maxBody-len(head)-len(tail))
	c := tlsServer(t, map[string]http.HandlerFunc{
		"/manifest.json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(head + pad + tail)) },
	})
	m, err := FetchManifest(context.Background(), c, "example.com")
	if err != nil {
		t.Fatalf("a manifest of exactly %d bytes was refused: %v", maxBody, err)
	}
	if base, ok := m.Overlay("x"); !ok || base != pad {
		t.Fatalf("the manifest at the bound lost its overlay: %d bytes, %v", len(base), ok)
	}
}

// roundTripFunc lets a test see whether a client was asked anything at all.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A domain carrying a character that would change the URL's meaning is
// refused as not a domain before any request is made.
func TestFetchManifestRefusesANonDomainBeforeAsking(t *testing.T) {
	var asked atomic.Int32
	c := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		asked.Add(1)
		return nil, errors.New("asked")
	})}
	for _, d := range []string{"", "example.com/x", "example.com?x", "example.com#x", "a@example.com", "example .com"} {
		_, err := FetchManifest(context.Background(), c, d)
		if want := `resolve: "` + d + `" is not a domain`; err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want\n%q", d, err, want)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the client was asked %d time(s)", n)
	}
}

// An answer that breaks two rules is refused for the one checked first, and
// that order decides the text a caller shows: the manifest's version before
// its resolve URL, then the answer's version before the echo, the echo before
// a revocation, a revocation before the key, and a key's length before its
// hex.
func TestAnAnswerBreakingTwoRulesNamesTheFirstCheck(t *testing.T) {
	doc := func(version, handle, key string, revoked bool) string {
		return fmt.Sprintf(`{"metanetHandles":%q,"handle":%q,"domain":"example.com","identityKey":%q,"certificate":{},"messagebox":"","ttl":60,"revoked":%v}`, version, handle, key, revoked)
	}
	notHex := strings.Repeat("z", 64)
	for _, tc := range []struct {
		name, handles, answer, want string
	}{
		{"the handles version before the resolve URL", `{"version":"2.0","resolve":"http://example.com/handles/resolve"}`, doc("1.0", "alice", identityKeyHex, false),
			`resolve: alice@example.com: metanet.handles.version "2.0": major version is not 1, the only one this build implements`},
		{"the answer's version before the echo", `{"version":"1.0"}`, doc("2.0", "bob", identityKeyHex, false),
			`resolve: alice@example.com: metanetHandles "2.0": major version is not 1, the only one this build implements`},
		{"the echo before a revocation", `{"version":"1.0"}`, doc("1.0", "bob", identityKeyHex, true),
			`resolve: alice@example.com: endpoint echoed "bob" at "example.com", not the handle asked for; refusing an answer that was not for this handle`},
		{"a revocation before the key", `{"version":"1.0"}`, doc("1.0", "alice", identityKeyHex[:64], true),
			"resolve: alice@example.com: handle revoked (status 200)"},
		{"the key's length before its hex", `{"version":"1.0"}`, doc("1.0", "alice", notHex, false),
			`resolve: alice@example.com: identityKey "` + notHex + `" is 64 characters, want 66 (33-byte compressed key, hex)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tlsServer(t, map[string]http.HandlerFunc{
				"/manifest.json": manifestWith(tc.handles),
				"/.well-known/metanet-handles/resolve": func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(tc.answer))
				},
			})
			m, err := FetchManifest(context.Background(), c, "example.com")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveHandle(context.Background(), c, m, alice()); err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want\n%q", err, tc.want)
			}
		})
	}
}

// A redirect chain is followed for five hops and refused at the sixth, and
// the count is checked before the origin, so a sixth hop that also leaves the
// origin is refused for the length of the chain. The requests are counted,
// because a bound moved by one changes how many are made and not the words.
func TestRedirectsStopAtTheSixthHopBeforeTheOriginIsChecked(t *testing.T) {
	for name, sixth := range map[string]string{
		"within the origin": "https://example.com/manifest.json",
		"out of the origin": "https://other.example/manifest.json",
	} {
		var asked atomic.Int32
		c := tlsServer(t, map[string]http.HandlerFunc{
			"/manifest.json": func(w http.ResponseWriter, r *http.Request) {
				to := "https://example.com/manifest.json"
				if asked.Add(1) == 5 {
					to = sixth
				}
				http.Redirect(w, r, to, http.StatusFound)
			},
		})
		_, err := FetchManifest(context.Background(), c, "example.com")
		if want := `resolve: manifest for example.com: Get "` + sixth + `": more than 5 redirects`; err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want\n%q", name, err, want)
		}
		if n := asked.Load(); n != 5 {
			t.Errorf("%s: the manifest was asked %d time(s), want 5", name, n)
		}
	}
}

// NewHTTPClient is the discovery policy, field by field: system roots with
// verification on and TLS 1.2 at the least, no proxy, the same-origin
// redirect rule, a small idle pool, and the timeout given, or 15s when none
// is.
func TestNewHTTPClientIsTheDiscoveryPolicy(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "https://example.com/manifest.json", nil)
	away := httptest.NewRequest(http.MethodGet, "https://other.example/manifest.json", nil)
	for _, tc := range []struct{ in, want time.Duration }{
		{0, 15 * time.Second},
		{-time.Second, 15 * time.Second},
		{7 * time.Second, 7 * time.Second},
	} {
		c := NewHTTPClient(tc.in)
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%v: transport is %T, want an *http.Transport of its own", tc.in, c.Transport)
		}
		if c.Timeout != tc.want || tr.TLSHandshakeTimeout != tc.want {
			t.Errorf("%v: timeout %v, handshake %v, want %v for both", tc.in, c.Timeout, tr.TLSHandshakeTimeout, tc.want)
		}
		if cfg := tr.TLSClientConfig; cfg == nil || cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify || cfg.RootCAs != nil {
			t.Errorf("%v: TLS config %+v, want system roots, verification on and TLS 1.2 at the least", tc.in, cfg)
		}
		if tr.Proxy != nil {
			t.Errorf("%v: the transport asks a proxy function where to send a request", tc.in)
		}
		if tr.MaxIdleConns != 4 || tr.IdleConnTimeout != 30*time.Second {
			t.Errorf("%v: idle pool of %d for %v, want 4 for 30s", tc.in, tr.MaxIdleConns, tr.IdleConnTimeout)
		}
		if c.CheckRedirect == nil {
			t.Fatalf("%v: the client follows any redirect", tc.in)
		}
		if err := c.CheckRedirect(away, []*http.Request{first}); !errors.Is(err, ErrCrossOriginRedirect) {
			t.Errorf("%v: a redirect to another origin gave %v", tc.in, err)
		}
	}
}

// A nil client is not a nil pointer: each fetch runs and honours the
// context, which here is already done, so it fails on the context and names
// the URL it would have asked.
func TestANilClientIsNotANilPointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FetchManifest(ctx, nil, "example.com")
	if want := `resolve: manifest for example.com: Get "https://example.com/manifest.json": context canceled`; !errors.Is(err, context.Canceled) || err.Error() != want {
		t.Errorf("manifest: got %v, want\n%q", err, want)
	}
	m := &Manifest{Handles: &Handles{Version: "1.0"}}
	_, err = ResolveHandle(ctx, nil, m, alice())
	if want := `resolve: alice@example.com: Get "https://example.com/.well-known/metanet-handles/resolve?handle=alice": context canceled`; !errors.Is(err, context.Canceled) || err.Error() != want {
		t.Errorf("answer: got %v, want\n%q", err, want)
	}
}
