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
