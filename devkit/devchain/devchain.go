// Package devchain is an application's local stand-in chain command:
// bcommon's testchain served over HTTP, a node's JSON-RPC at /rpc
// (generatetoaddress, sendrawtransaction, getinfo), its asset API under
// /api/v1/ and a header source at /v1/root/<height> and /v1/tip. It checks
// what it is sent the way a node would (inputs exist and are unspent,
// coinbase is mature, scripts verify, nothing non-final is mined) but it has
// no proof of work and no peers: its proofs mean nothing outside it, and
// nothing it mines is money. Coinbase: only on a regtest chain you run, for
// development and tests. It is for a laptop, never for a network.
//
// With -journal every accepted RPC call is appended to a file and replayed
// at start, so a restarted chain is the same chain: the same blocks, and
// the hosts' and the homes' state still matches it.
//
// An application's cmd/devchain is one call:
//
//	func main() {
//		os.Exit(devchain.Main(devchain.Options{App: "app", Version: version}, os.Args[1:], os.Stdout, os.Stderr))
//	}
package devchain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/bcommon/testchain"
)

// Options are what differ between applications.
type Options struct {
	// App is the application the chain is for, as its help names it.
	App string
	// Version is the command's stamped version.
	Version string
	// Certs adds `devchain certs -dir DIR -names NAME,...`, which writes a
	// throwaway CA and server certificates (WriteCerts), for an application
	// whose servers must be https on a laptop.
	Certs bool
}

func (o Options) usage() string {
	u := "usage: devchain [-listen ADDR] [-height N] [-journal FILE]\n"
	if o.Certs {
		u += "       devchain certs -dir DIR -names NAME,...\n"
	}
	u += fmt.Sprintf(`
A local stand-in chain for trying %s: a node's RPC at /rpc, its asset
API under /api/v1/, and a header source at /v1/root/<height> and /v1/tip.
Every transaction is mined at once, and generatetoaddress mines coinbase
(coinbase: only on a regtest chain you run, for development and tests).
No proof of work, no peers: for a laptop, never for a network.
`, o.App)
	if o.Certs {
		u += `
certs writes a throwaway certificate authority and a server certificate for
each name into DIR, and exits; the CA's key is not kept. Never for a
network.
`
	}
	return u + "\n"
}

// Main runs the command on args (without the program name) and returns
// its exit status: 0, 1 when the chain cannot start or stops with an
// error, 2 for a usage error. It serves until SIGINT or SIGTERM.
func Main(o Options, args []string, stdout, stderr io.Writer) int {
	logger := log.New(stderr, "", log.LstdFlags)
	if o.Certs && len(args) > 0 && args[0] == "certs" {
		fs := flag.NewFlagSet("certs", flag.ContinueOnError)
		fs.SetOutput(stderr)
		dir := fs.String("dir", "", "the directory to write ca.pem and <name>.pem, <name>-key.pem into")
		names := fs.String("names", "", "the server names, comma separated")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *dir == "" || *names == "" {
			fmt.Fprintln(stderr, "devchain certs: -dir and -names are required")
			return 2
		}
		if err := WriteCerts(*dir, o.App, strings.Split(*names, ","), stdout); err != nil {
			fmt.Fprintf(stderr, "devchain certs: %v\n", err)
			return 1
		}
		return 0
	}
	fs := flag.NewFlagSet("devchain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8080", "address to serve on")
	start := fs.Uint("height", 700, "the height of the tip before anything is mined")
	journal := fs.String("journal", "", "append every accepted RPC call to this file and replay it at start")
	showVer := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprint(stderr, o.usage())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVer {
		fmt.Fprintln(stdout, "devchain", o.Version)
		return 0
	}
	chain := testchain.New(uint32(*start)) //nolint:gosec // a flag value
	h, err := Journaled(chain, *journal, logger.Printf)
	if err != nil {
		logger.Printf("devchain: %v", err)
		return 1
	}
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Printf("devchain %s: serving on %s, tip %d (a local stand-in chain; nothing here is money)", o.Version, *listen, chain.Height())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("devchain: %v", err)
		return 1
	}
	return 0
}

// Journaled is chain served with every accepted RPC call appended to the
// file at path, after replaying the file's calls into chain; an empty path
// serves chain as it is. logf takes a line about the replay.
func Journaled(chain http.Handler, path string, logf func(string, ...any)) (http.Handler, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return newJournaled(chain, path, logf)
}

// journaled serves the chain and appends each RPC call it accepted to a
// file, so a restart replays the same calls into the same chain.
type journaled struct {
	chain http.Handler
	logf  func(string, ...any)
	mu    sync.Mutex
	f     *os.File
}

func newJournaled(chain http.Handler, path string, logf func(string, ...any)) (*journaled, error) {
	j := &journaled{chain: chain, logf: logf}
	if path == "" {
		return j, nil
	}
	if err := j.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

// replay sends each journal line to the chain again, in order. A line the
// chain refuses now means the journal is not this chain's.
func (j *journaled) replay(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		n++
		rec := httptest.NewRecorder()
		j.chain.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(sc.Bytes())))
		if !accepted(rec.Body.Bytes()) {
			return fmt.Errorf("journal %s line %d: the chain refuses it now: %s", path, n, rec.Body.String())
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n > 0 {
		j.logf("devchain: replayed %d call(s) from %s", n, path)
	}
	return nil
}

func accepted(body []byte) bool {
	var r struct {
		Error any `json:"error"`
	}
	return json.Unmarshal(body, &r) == nil && r.Error == nil
}

func (j *journaled) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if j.f == nil || r.Method != http.MethodPost || r.URL.Path != "/rpc" {
		j.chain.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var call struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &call)
	// One call at a time, so the journal's order is the chain's.
	j.mu.Lock()
	defer j.mu.Unlock()
	rec := httptest.NewRecorder()
	r.Body = io.NopCloser(bytes.NewReader(body))
	j.chain.ServeHTTP(rec, r)
	if call.Method != "getinfo" && accepted(rec.Body.Bytes()) {
		var line bytes.Buffer
		if err := json.Compact(&line, body); err != nil {
			http.Error(w, "journal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		line.WriteByte('\n')
		if _, err := j.f.Write(line.Bytes()); err != nil {
			http.Error(w, "journal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := j.f.Sync(); err != nil {
			http.Error(w, "journal: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

var certName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62})$`)

// WriteCerts writes a throwaway CA and a server certificate for each name
// (a DNS name, or an IP address) into dir: ca.pem, <name>.pem and
// <name>-key.pem, keys mode 0600, and a line per certificate to out. The
// CA's key is discarded, so nothing more can be signed by it. app names the
// CA. For a laptop's quickstart, never a network.
func WriteCerts(dir, app string, names []string, out io.Writer) error {
	if len(names) == 0 {
		return errors.New("no names")
	}
	for _, n := range names {
		if !certName.MatchString(n) {
			return fmt.Errorf("name %q: lowercase letters, digits, dots and hyphens", n)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: app + " quickstart CA (throwaway)"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}
	for i, n := range names {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i) + 2),
			Subject:      pkix.Name{CommonName: n},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.AddDate(1, 0, 0),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if ip := net.ParseIP(n); ip != nil {
			leaf.IPAddresses = []net.IP{ip}
		} else {
			leaf.DNSNames = []string{n}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		kder, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, n+".pem"), "CERTIFICATE", der, 0o644); err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, n+"-key.pem"), "EC PRIVATE KEY", kder, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "certificate %s\n", n)
	}
	return nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}
