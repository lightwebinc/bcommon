package devchain

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/testchain"
)

func rpc(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body)))
	return rec.Body.String()
}

func TestTheJournalReplaysTheChain(t *testing.T) {
	key, _ := ec.PrivateKeyFromBytes([]byte(strings.Repeat("\x42", 32)))
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "journal")
	c1 := testchain.New(700)
	j1, err := Journaled(c1, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := rpc(t, j1, `{"id":1,"method":"generatetoaddress","params":[3,"`+addr.AddressString+`"]}`); !strings.Contains(out, `"error":null`) {
		t.Fatal(out)
	}
	rpc(t, j1, `{"id":2,"method":"getinfo","params":[]}`)
	rpc(t, j1, `{"id":3,"method":"sendrawtransaction","params":["00"]}`)
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "\n"); n != 1 {
		t.Fatalf("journal holds %d line(s), want only the accepted mining call:\n%s", n, raw)
	}
	c2 := testchain.New(700)
	if _, err := Journaled(c2, path, nil); err != nil {
		t.Fatal(err)
	}
	if c2.Height() != 703 || c2.Height() != c1.Height() {
		t.Fatalf("replayed tip %d, want %d", c2.Height(), c1.Height())
	}
	// A journal another chain wrote (a different start) is refused, not
	// half applied.
	if err := os.WriteFile(path, []byte(`{"id":1,"method":"sendrawtransaction","params":["00"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Journaled(testchain.New(700), path, nil); err == nil {
		t.Fatal("a journal line the chain refuses was accepted")
	}
}

// post sends body to h's /rpc through a real server, so the reply's header
// lines are as a browser receives them.
func post(t *testing.T, h http.Handler, body string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// notAPage asserts that a reply carries exactly one Content-Type, wantType,
// and exactly one X-Content-Type-Options, nosniff.
func notAPage(t *testing.T, name string, resp *http.Response, wantType string) {
	t.Helper()
	if ct, xo := resp.Header.Values("Content-Type"), resp.Header.Values("X-Content-Type-Options"); len(ct) != 1 || ct[0] != wantType || len(xo) != 1 || xo[0] != "nosniff" {
		t.Errorf("%s: served with Content-Type %q and X-Content-Type-Options %q, want %q and nosniff alone", name, ct, xo, wantType)
	}
}

// A reply the journal passes on is never served as a page, whatever the
// handler behind it wrote: JSON stays JSON, anything else is plain text,
// and neither is sniffed. The chain's reply to a method it does not know
// names the method, so a request can choose part of what comes back.
func TestJournaledRepliesAreNeverPages(t *testing.T) {
	const page = `<html><script>alert(1)</script></html>`

	// The chain itself: an unknown method is named in a JSON error.
	j, err := Journaled(testchain.New(700), filepath.Join(t.TempDir(), "journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, j, `{"id":1,"method":"`+strings.ReplaceAll(page, `"`, `\"`)+`","params":[]}`)
	notAPage(t, "an unknown method", resp, "application/json")
	if b, _ := io.ReadAll(resp.Body); bytes.Contains(b, []byte("<script>")) || !bytes.Contains(b, []byte("not found")) {
		t.Errorf("the reply carries the method as markup: %s", b)
	}

	// A handler that echoes what it is sent, with no type of its own.
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	})
	j, err = Journaled(echo, filepath.Join(t.TempDir(), "journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	notAPage(t, "an echo", post(t, j, page), "text/plain; charset=utf-8")
}

// The type headers a handler sets never reach the reply, under a key in any
// case: a header map keeps "content-type" apart from "Content-Type", and a
// browser takes either. The reply is JSON only when every type the handler
// gave is application/json as a media type, which ignores case and takes no
// prefix.
func TestJournaledReplyTypeIsTheJournals(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(h http.Header)
		want string
	}{
		{"a page under lowercase keys", func(h http.Header) {
			h["content-type"] = []string{"text/html"}
			h["x-content-type-options"] = []string{"none"}
		}, "text/plain; charset=utf-8"},
		{"JSON and a page under two keys", func(h http.Header) {
			h.Set("Content-Type", "application/json")
			h["content-type"] = []string{"text/html"}
		}, "text/plain; charset=utf-8"},
		{"JSON in capitals with a parameter", func(h http.Header) { h.Set("Content-Type", "Application/JSON; charset=utf-8") }, "application/json"},
		{"a type that only starts like JSON", func(h http.Header) { h.Set("Content-Type", "application/jsonx") }, "text/plain; charset=utf-8"},
	} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.set(w.Header())
			_, _ = io.WriteString(w, `{"result":"<html><script>alert(1)</script></html>"}`)
		})
		j, err := Journaled(h, filepath.Join(t.TempDir(), "journal"), nil)
		if err != nil {
			t.Fatal(err)
		}
		notAPage(t, c.name, post(t, j, `{"id":1,"method":"getinfo","params":[]}`), c.want)
	}
}

func TestCertsVerifyAgainstTheirCA(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCerts(dir, "sample", []string{"provider-a", "127.0.0.1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem")
	}
	for _, n := range []string{"provider-a", "127.0.0.1"} {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, n+".pem"), filepath.Join(dir, n+"-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(pair.Certificate[0])
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: n}); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, n+"-key.pem")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s key mode %v", n, fi.Mode())
		}
	}
	if err := WriteCerts(dir, "sample", []string{"../x"}, io.Discard); err == nil {
		t.Fatal("a path in a name was accepted")
	}
}

// The command's usage errors, -version, and certs only where the
// application asked for it.
func TestMain(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(Options{App: "sample", Version: "v1"}, []string{"-version"}, &out, &errb); code != 0 || out.String() != "devchain v1\n" {
		t.Fatalf("-version: %d %q", code, out.String())
	}
	if code := Main(Options{App: "sample"}, []string{"-nope"}, &out, &errb); code != 2 {
		t.Fatalf("a bad flag: %d", code)
	}
	errb.Reset()
	if code := Main(Options{App: "sample"}, []string{"-h"}, &out, &errb); code != 0 || !strings.Contains(errb.String(), "for trying sample") || strings.Contains(errb.String(), "certs") {
		t.Fatalf("-h: %d %s", code, errb.String())
	}
	dir := t.TempDir()
	out.Reset()
	if code := Main(Options{App: "sample", Certs: true}, []string{"certs", "-dir", dir, "-names", "a.test"}, &out, &errb); code != 0 || out.String() != "certificate a.test\n" {
		t.Fatalf("certs: %d %q %s", code, out.String(), errb.String())
	}
	if code := Main(Options{App: "sample", Certs: true}, []string{"certs", "-dir", dir}, &out, &errb); code != 2 {
		t.Fatalf("certs with no names: %d", code)
	}
}
