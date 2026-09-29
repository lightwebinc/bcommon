package headers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// sourceFixture reads a header answer captured from a public source,
// verbatim, so the decoding is tested against what those services send.
func sourceFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sources", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serveAt answers one path with a body; every other path is a 404.
func serveAt(t *testing.T, path string, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustHash(t *testing.T, s string) *chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const root900000 = "62272ce3662923219acd98587fdb5c0b01557597036d8635207bda8a3fa72a7e"

func client(srv *httptest.Server, kind Kind, network string) *Client {
	return &Client{Base: srv.URL, Kind: kind, Network: network, HTTP: srv.Client()}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		kind    Kind
		base    string
		network string
	}{
		{"woc:main", WhatsOnChain, "https://api.whatsonchain.com/v1/bsv/main", Mainnet},
		{"woc:test", WhatsOnChain, "https://api.whatsonchain.com/v1/bsv/test", Testnet},
		{"chaintracks:https://ct.example.com/v2/", Chaintracks, "https://ct.example.com/v2", Mainnet},
		{"http://bridge.example.com:9178/", Native, "http://bridge.example.com:9178", ""},
	} {
		kind, base, network, err := Parse(tc.spec)
		if err != nil || kind != tc.kind || base != tc.base || network != tc.network {
			t.Errorf("Parse(%q) = %v %q %q %v", tc.spec, kind, base, network, err)
		}
	}
	for _, bad := range []string{"", "woc:", "woc:regtest", "chaintracks:", "chaintracks:ftp://x", "bridge.example.com", "file:///etc"} {
		if _, _, _, err := Parse(bad); !errors.Is(err, ErrSource) {
			t.Errorf("Parse(%q) = %v, want ErrSource", bad, err)
		}
	}
}

func TestABadSpecRefusesEveryCall(t *testing.T) {
	c := New("woc:regtest")
	if _, err := c.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 1); !errors.Is(err, ErrSource) {
		t.Fatalf("root: %v", err)
	}
	if _, err := c.CurrentHeight(context.Background()); !errors.Is(err, ErrSource) {
		t.Fatalf("tip: %v", err)
	}
}

func TestWhatsOnChainHeaderValidates(t *testing.T) {
	srv := serveAt(t, "/block/900000/header", 200, sourceFixture(t, "woc-main-900000"))
	c := client(srv, WhatsOnChain, Mainnet)
	ok, err := c.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000)
	if err != nil || !ok {
		t.Fatalf("real mainnet header: %v %v", ok, err)
	}
	wrong := mustHash(t, strings.Repeat("11", 32))
	if ok, err := c.IsValidRootForHeight(context.Background(), wrong, 900000); err != nil || ok {
		t.Fatalf("wrong root must be false, not an error: %v %v", ok, err)
	}
}

func TestChaintracksHeaderValidates(t *testing.T) {
	srv := serveAt(t, "/header/height/900000", 200, sourceFixture(t, "chaintracks-900000"))
	ok, err := client(srv, Chaintracks, Mainnet).IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000)
	if err != nil || !ok {
		t.Fatalf("real chaintracks header: %v %v", ok, err)
	}
}

// tamper rewrites one field of a captured WhatsOnChain answer.
func tamper(t *testing.T, field string, value any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sourceFixture(t, "woc-main-900000"), &m); err != nil {
		t.Fatal(err)
	}
	m[field] = value
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A source that swaps in the root it wants the caller to accept, keeping the
// real block's hash, is caught because the fields no longer hash to it.
func TestAForgedRootIsAnErrorNotAMatch(t *testing.T) {
	forged := strings.Repeat("ab", 32)
	for name, body := range map[string][]byte{
		"root":   tamper(t, "merkleroot", forged),
		"nonce":  tamper(t, "nonce", 1),
		"height": tamper(t, "height", 900001),
	} {
		t.Run(name, func(t *testing.T) {
			srv := serveAt(t, "/block/900000/header", 200, body)
			ok, err := client(srv, WhatsOnChain, Mainnet).IsValidRootForHeight(context.Background(), mustHash(t, forged), 900000)
			if ok || !errors.Is(err, ErrProofOfWork) {
				t.Fatalf("got %v %v, want ErrProofOfWork", ok, err)
			}
		})
	}
}

// mine builds a header at the easiest target a regtest chain allows, with a
// root of the caller's choosing: exactly what a lying source can make for
// free.
func mine(t *testing.T, height uint32, root *chainhash.Hash) []byte {
	t.Helper()
	h := header{Height: height, Version: 0x20000000, Root: *root, Time: 1790000000, Bits: 0x207fffff}
	for ; ; h.Nonce++ {
		got := chainhash.DoubleHashH(h.serialize())
		if hashToBig(got).Cmp(compactToBig(h.Bits)) <= 0 {
			h.Hash = got
			break
		}
	}
	b, _ := json.Marshal(map[string]any{
		"height": h.Height, "version": h.Version, "hash": h.Hash.String(),
		"merkleroot": h.Root.String(), "previousblockhash": h.Prev.String(),
		"time": h.Time, "bits": "207fffff", "nonce": h.Nonce,
	})
	return b
}

func TestACheaplyMinedHeaderFailsTheMainnetFloor(t *testing.T) {
	forged := mustHash(t, strings.Repeat("cd", 32))
	srv := serveAt(t, "/block/900000/header", 200, mine(t, 900000, forged))

	ok, err := client(srv, WhatsOnChain, Mainnet).IsValidRootForHeight(context.Background(), forged, 900000)
	if ok || !errors.Is(err, ErrProofOfWork) {
		t.Fatalf("mainnet: got %v %v, want ErrProofOfWork", ok, err)
	}
	// The same header is a valid header on a chain with no floor.
	ok, err = client(srv, WhatsOnChain, Regtest).IsValidRootForHeight(context.Background(), forged, 900000)
	if err != nil || !ok {
		t.Fatalf("regtest: got %v %v", ok, err)
	}
}

func TestTestnetHeaderValidatesWithoutAFloor(t *testing.T) {
	var m struct {
		Root string `json:"merkleroot"`
	}
	body := sourceFixture(t, "woc-test-1")
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	srv := serveAt(t, "/block/1/header", 200, body)
	ok, err := client(srv, WhatsOnChain, Testnet).IsValidRootForHeight(context.Background(), mustHash(t, m.Root), 1)
	if err != nil || !ok {
		t.Fatalf("testnet block 1: %v %v", ok, err)
	}
	// Difficulty 1 is far under the mainnet floor.
	if _, err := client(srv, WhatsOnChain, Mainnet).IsValidRootForHeight(context.Background(), mustHash(t, m.Root), 1); !errors.Is(err, ErrProofOfWork) {
		t.Fatalf("testnet header on mainnet: %v", err)
	}
}

func TestUnknownHeightAndBrokenSourceStayDistinct(t *testing.T) {
	root := mustHash(t, root900000)
	notFound := serveAt(t, "/nothing", 200, nil)
	for _, kind := range []Kind{WhatsOnChain, Chaintracks} {
		if ok, err := client(notFound, kind, Mainnet).IsValidRootForHeight(context.Background(), root, 900000); ok || err != nil {
			t.Errorf("%s 404: %v %v, want false nil", kind, ok, err)
		}
	}
	broken := serveAt(t, "/header/height/900000", 500, []byte(`{"status":"error","code":"ERR_INTERNAL"}`))
	if _, err := client(broken, Chaintracks, Mainnet).IsValidRootForHeight(context.Background(), root, 900000); err == nil {
		t.Error("chaintracks 500 must be an error")
	}
	odd := serveAt(t, "/header/height/900000", 200, []byte(`{"status":"error","code":"ERR_INTERNAL"}`))
	if _, err := client(odd, Chaintracks, Mainnet).IsValidRootForHeight(context.Background(), root, 900000); err == nil {
		t.Error("chaintracks 200 without success must be an error")
	}
}

func TestCurrentHeightPerKind(t *testing.T) {
	woc := serveAt(t, "/chain/info", 200, []byte(`{"chain":"main","blocks":968932}`))
	if h, err := client(woc, WhatsOnChain, Mainnet).CurrentHeight(context.Background()); err != nil || h != 968932 {
		t.Errorf("woc: %d %v", h, err)
	}
	ct := serveAt(t, "/height", 200, []byte(`{"status":"success","value":{"height":968932}}`))
	if h, err := client(ct, Chaintracks, Mainnet).CurrentHeight(context.Background()); err != nil || h != 968932 {
		t.Errorf("chaintracks: %d %v", h, err)
	}
}

func TestTheFloorSitsUnderEveryRealHeader(t *testing.T) {
	// 0x181399a4 is block 900000's bits, difficulty about 5.6e10.
	if compactToBig(0x181399a4).Cmp(floorTarget(MainnetMinDifficulty)) > 0 {
		t.Fatal("a real mainnet header is above the floor target")
	}
	if compactToBig(0x1d00ffff).Cmp(floorTarget(MainnetMinDifficulty)) <= 0 {
		t.Fatal("difficulty 1 passes the mainnet floor")
	}
}

// A rate-limited service is retried, not read as a failed proof, and a root
// proven once is not asked for again.
func TestA429IsRetriedAndAProvenRootIsReused(t *testing.T) {
	calls := 0
	body := sourceFixture(t, "woc-main-900000")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c := client(srv, WhatsOnChain, Mainnet)
	if ok, err := c.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000); err != nil || !ok {
		t.Fatalf("after a 429: %v %v", ok, err)
	}
	if ok, err := c.IsValidRootForHeight(context.Background(), mustHash(t, strings.Repeat("11", 32)), 900000); err != nil || ok {
		t.Fatalf("cached, wrong root: %v %v", ok, err)
	}
	if calls != 2 {
		t.Fatalf("service asked %d times, want 2", calls)
	}
}
