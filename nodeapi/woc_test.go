package nodeapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/nodeapi"
)

// The fixtures are WhatsOnChain's answers about one mainnet transaction in
// block 900000, captured verbatim on 2026-10-07.
const (
	wocTx      = "52854619b1c2e78d6c1e9a91fdb14c4bef1b8d1897de3253a538e152bd055be6"
	wocParent  = "4965bef51816db722c4c76b91dd753a2e5eef8abe6a846d888f2b5432aae86ae"
	wocBlock   = "000000000000000002feb6a36e1b8bf81409d0252e285449e3d0ef2388c5506a"
	wocRoot    = "62272ce3662923219acd98587fdb5c0b01557597036d8635207bda8a3fa72a7e"
	wocUnknown = "0000000000000000000000000000000000000000000000000000000000000001"
	wocUnmined = "c4102d6c8d2fbb948d87a442e726280a3c6736e9c28e1608879570b2d1e46011"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sources", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type answer struct {
	status int
	body   []byte
}

// wocServer answers each path from a table, as WhatsOnChain answered it;
// any other path is WhatsOnChain's plain 404.
type wocServer struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]answer
	hits   map[string]int
	auth   atomic.Value
	busy   atomic.Int32
}

func newWoC(t *testing.T, routes map[string]answer) (*wocServer, *nodeapi.WoC) {
	t.Helper()
	s := &wocServer{routes: routes, hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.auth.Store(r.Header.Get("Authorization"))
		if s.busy.Load() > 0 {
			s.busy.Add(-1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		s.mu.Lock()
		a, ok := s.routes[r.URL.Path]
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Not Found"))
			return
		}
		w.WriteHeader(a.status)
		_, _ = w.Write(a.body)
	}))
	t.Cleanup(s.Close)
	return s, &nodeapi.WoC{Network: "main", Base: s.URL, Client: s.Client(), Rate: 1000}
}

func (s *wocServer) set(path string, a answer) {
	s.mu.Lock()
	s.routes[path] = a
	s.mu.Unlock()
}

func (s *wocServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func headersAt900000() *goldentest.Tracker {
	return &goldentest.Tracker{Roots: map[uint32]string{900000: wocRoot}, Tip: 970000}
}

func realRoutes(t *testing.T) map[string]answer {
	return map[string]answer{
		"/tx/" + wocTx + "/hex":            {200, fixture(t, "woc-main-tx-hex.txt")},
		"/tx/" + wocTx + "/beef":           {200, fixture(t, "woc-main-tx-beef.txt")},
		"/tx/" + wocTx + "/proof/tsc":      {200, fixture(t, "woc-main-tx-tsc.json")},
		"/tx/hash/" + wocTx:                {200, fixture(t, "woc-main-tx-hash.json")},
		"/block/" + wocBlock + "/header":   {200, fixture(t, "woc-main-block-hash-header.json")},
		"/tx/" + wocParent + "/4/spent":    {200, fixture(t, "woc-main-spent-confirmed.json")},
		"/tx/" + wocTx + "/1/spent":        {400, []byte("Bad Request")},
		"/tx/" + wocUnknown + "/beef":      {500, fixture(t, "woc-main-beef-unknown.txt")},
		"/tx/" + wocUnknown + "/proof/tsc": {200, []byte("null")},
		"/tx/" + wocUnknown + "/0/spent":   {400, []byte("Bad Request")},
		"/tx/" + wocUnmined + "/beef":      {422, fixture(t, "woc-main-beef-unmined.txt")},
		"/tx/" + wocUnmined + "/proof/tsc": {200, []byte("null")},
	}
}

func TestWoCTxRaw(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	raw, err := w.TxRaw(ctx, wocTx)
	if err != nil || len(raw) != 351 {
		t.Fatalf("%d %v", len(raw), err)
	}
	if _, err := w.TxRaw(ctx, wocUnknown); !nodeapi.IsNotFound(err) || !errors.Is(err, nodeapi.ErrTxNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Another transaction's bytes under this txid are refused.
	srv.set("/tx/"+wocParent+"/hex", answer{200, fixture(t, "woc-main-tx-hex.txt")})
	if _, err := w.TxRaw(ctx, wocParent); err == nil || !strings.Contains(err.Error(), "answered "+wocTx) {
		t.Fatalf("wrong tx: %v", err)
	}
	if _, err := w.TxRaw(ctx, "../x"); err == nil {
		t.Fatal("a txid that is not one must be refused before a request")
	}
	srv.set("/tx/"+wocParent+"/hex", answer{200, []byte("zz")})
	if _, err := w.TxRaw(ctx, wocParent); err == nil {
		t.Fatal("not hex")
	}
}

func TestWoCProofFromBEEFVerifiesAgainstHeaders(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	mp, height, err := w.Proof(ctx, wocTx)
	if err != nil || height != 900000 || mp.BlockHeight != 900000 {
		t.Fatalf("%v %d %v", mp, height, err)
	}
	if srv.count("/tx/"+wocTx+"/proof/tsc") != 0 {
		t.Fatal("the BEEF answered; the TSC proof must not be read")
	}
	if err := nodeapi.CheckProof(ctx, mp, wocTx, headersAt900000()); err != nil {
		t.Fatal(err)
	}
	checked := nodeapi.Checked{Source: w, Headers: headersAt900000()}
	if _, _, err := checked.Proof(ctx, wocTx); err != nil {
		t.Fatal(err)
	}
	// Headers that hold another root at the height refuse it.
	other := &goldentest.Tracker{Roots: map[uint32]string{900000: strings.Repeat("11", 32)}}
	if _, _, err := (nodeapi.Checked{Source: w, Headers: other}).Proof(ctx, wocTx); !errors.Is(err, nodeapi.ErrProofRefused) {
		t.Fatalf("wrong root: %v", err)
	}
}

func TestWoCProofFallsBackToTSC(t *testing.T) {
	ctx := context.Background()
	routes := realRoutes(t)
	routes["/tx/"+wocTx+"/beef"] = answer{502, []byte("bad gateway")}
	srv, w := newWoC(t, routes)
	mp, height, err := w.Proof(ctx, wocTx)
	if err != nil || height != 900000 {
		t.Fatalf("%d %v", height, err)
	}
	if srv.count("/tx/"+wocTx+"/proof/tsc") != 1 {
		t.Fatal("the TSC proof was not read")
	}
	if err := nodeapi.CheckProof(ctx, mp, wocTx, headersAt900000()); err != nil {
		t.Fatalf("the TSC proof, converted, must prove against the real header: %v", err)
	}
	// The same BUMP the BEEF carries, up to the path's form.
	_, w2 := newWoC(t, realRoutes(t))
	fromBEEF, _, _ := w2.Proof(ctx, wocTx)
	txid, _ := chainhash.NewHashFromHex(wocTx)
	r1, err1 := mp.ComputeRoot(txid)
	r2, err2 := fromBEEF.ComputeRoot(txid)
	if err1 != nil || err2 != nil || !r1.IsEqual(r2) || r1.String() != wocRoot {
		t.Fatalf("roots %v %v (%v %v)", r1, r2, err1, err2)
	}
}

func TestWoCProofNotMined(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	for _, id := range []string{wocUnknown, wocUnmined} {
		if _, _, err := w.Proof(ctx, id); !errors.Is(err, nodeapi.ErrNotMined) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	srv.set("/tx/"+wocTx+"/beef", answer{200, []byte("zz")})
	srv.set("/tx/"+wocTx+"/proof/tsc", answer{200, []byte("null")})
	if _, _, err := w.Proof(ctx, wocTx); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("broken BEEF, TSC null: %v", err)
	}
	srv.set("/tx/"+wocTx+"/proof/tsc", answer{500, []byte("down")})
	if _, _, err := w.Proof(ctx, wocTx); err == nil || errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("both down is an error, not 'not mined': %v", err)
	}
}

func TestWoCSpender(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	by, err := w.Spender(ctx, wocParent, 4)
	if err != nil || by != wocTx {
		t.Fatalf("spent: %q %v", by, err)
	}
	// 404 is WhatsOnChain's "known, not spent".
	by, err = w.Spender(ctx, wocTx, 0)
	if err != nil || by != "" {
		t.Fatalf("unspent: %q %v", by, err)
	}
	// 400 is "unknown", never unspent.
	for _, c := range []struct {
		txid string
		vout uint32
	}{{wocTx, 1}, {wocUnknown, 0}} {
		if by, err := w.Spender(ctx, c.txid, c.vout); !errors.Is(err, nodeapi.ErrSpendUnknown) || by != "" {
			t.Fatalf("%s.%d: %q %v", c.txid, c.vout, by, err)
		}
	}
	for name, a := range map[string]answer{
		"server error": {500, []byte("x")},
		"not json":     {200, []byte("x")},
		"no txid":      {200, []byte(`{"vin":0,"status":"confirmed"}`)},
		"not a txid":   {200, []byte(`{"txid":"zz","vin":0,"status":"confirmed"}`)},
	} {
		srv.set("/tx/"+wocTx+"/2/spent", a)
		if by, err := w.Spender(ctx, wocTx, 2); !errors.Is(err, nodeapi.ErrSpendUnknown) || by != "" {
			t.Fatalf("%s: %q %v", name, by, err)
		}
	}
}

func TestWoCSpentElsewhere(t *testing.T) {
	ctx := context.Background()
	_, w := newWoC(t, realRoutes(t))
	// A transaction spending the same outpoint as wocTx.
	parent, _ := chainhash.NewHashFromHex(wocParent)
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: parent, SourceTxOutIndex: 4, SequenceNumber: 0xffffffff})
	err := w.SpentElsewhere(ctx, tx)
	var se *nodeapi.SpentError
	if !errors.As(err, &se) || se.By != wocTx || !errors.Is(err, nodeapi.ErrDoubleSpent) {
		t.Fatalf("%v", err)
	}
	// An input WhatsOnChain cannot answer for is passed over.
	unknown, _ := chainhash.NewHashFromHex(wocUnknown)
	tx2 := transaction.NewTransaction()
	tx2.AddInput(&transaction.TransactionInput{SourceTXID: unknown, SequenceNumber: 0xffffffff})
	if err := w.SpentElsewhere(ctx, tx2); err != nil {
		t.Fatal(err)
	}
}

func TestWoCKnown(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	if ok, err := w.Known(ctx, wocTx); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := w.Known(ctx, wocUnknown); err != nil || ok {
		t.Fatalf("unknown: %v %v", ok, err)
	}
	srv.set("/tx/hash/"+wocParent, answer{200, fixture(t, "woc-main-tx-hash.json")})
	if _, err := w.Known(ctx, wocParent); err == nil {
		t.Fatal("an answer for another transaction must be refused")
	}
	srv.set("/tx/hash/"+wocParent, answer{503, nil})
	if _, err := w.Known(ctx, wocParent); err == nil {
		t.Fatal("a failed read is not 'not known'")
	}
}

func TestWoCKeyRateAnd429(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	w.Key = "mainnet_abc"
	srv.busy.Store(2)
	if _, err := w.TxRaw(ctx, wocTx); err != nil {
		t.Fatalf("429 twice then 200: %v", err)
	}
	if got, _ := srv.auth.Load().(string); got != "mainnet_abc" {
		t.Fatalf("authorization %q", got)
	}
	// Pacing: four calls at 20 a second take at least 150 ms.
	w.Rate = 20
	start := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := w.Known(ctx, wocTx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 140*time.Millisecond {
		t.Fatalf("four calls in %s at 20/s", d)
	}
	// A cancelled context ends the wait for a slot.
	w.Rate = 0.5
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, _ = w.Known(cctx, wocTx)
	if _, err := w.Known(cctx, wocTx); err == nil {
		t.Fatal("the pacing wait must end with the context")
	}
}

func TestNewWoC(t *testing.T) {
	if w, err := nodeapi.NewWoC("test", ""); err != nil || w.Network != "test" {
		t.Fatal(err)
	}
	if _, err := nodeapi.NewWoC("regtest", ""); err == nil {
		t.Fatal("regtest has no WhatsOnChain")
	}
}

// A TSC path with duplicates converts to a BUMP whose root is the block's:
// a block of three transactions, the last proven, so its level-0 sibling
// is itself ("*").
func TestTSCDuplicates(t *testing.T) {
	h := func(b byte) *chainhash.Hash { var x chainhash.Hash; x[0] = b; return &x }
	pair := func(a, b *chainhash.Hash) *chainhash.Hash {
		var buf [64]byte
		copy(buf[:32], a[:])
		copy(buf[32:], b[:])
		r := chainhash.DoubleHashH(buf[:])
		return &r
	}
	t0, t1, t2 := h(1), h(2), h(3)
	root := pair(pair(t0, t1), pair(t2, t2))
	p := nodeapi.TSCProof{Index: 2, TxOrID: t2.String(), Nodes: []string{"*", pair(t0, t1).String()}}
	mp, err := p.MerklePath(t2.String(), 7)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mp.ComputeRoot(t2)
	if err != nil || !got.IsEqual(root) {
		t.Fatalf("%v %v, want %v", got, err, root)
	}
	for _, bad := range []nodeapi.TSCProof{
		{Index: 4, TxOrID: t2.String(), Nodes: []string{"*", "*"}},
		{Index: 0, TxOrID: t0.String(), Nodes: []string{"zz"}},
		{Index: 0, TxOrID: t1.String(), Nodes: nil},
	} {
		if _, err := bad.MerklePath(t0.String(), 1); err == nil {
			t.Errorf("%+v converted", bad)
		}
	}
}

func TestSources(t *testing.T) {
	ctx := context.Background()
	_, w := newWoC(t, realRoutes(t))
	empty := httptest.NewServer(http.NotFoundHandler())
	defer empty.Close()
	node := &nodeapi.Asset{Base: empty.URL}
	s := &nodeapi.Sources{Tx: []nodeapi.TxSource{node, w}, Proofs: []nodeapi.ProofSource{node, w}, Spends: w, Knows: w}
	if raw, err := s.TxRaw(ctx, wocTx); err != nil || len(raw) != 351 {
		t.Fatalf("falls through to WoC: %v", err)
	}
	if _, err := s.TxRaw(ctx, wocUnknown); !nodeapi.IsNotFound(err) {
		t.Fatalf("not found everywhere: %v", err)
	}
	if _, h, err := s.Proof(ctx, wocTx); err != nil || h != 900000 {
		t.Fatalf("proof: %v", err)
	}
	if _, _, err := s.Proof(ctx, wocUnmined); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("not mined: %v", err)
	}
	if by, err := s.Spender(ctx, wocParent, 4); err != nil || by != wocTx {
		t.Fatalf("spender: %v", err)
	}
	if ok, err := s.Known(ctx, wocTx); !ok || err != nil {
		t.Fatalf("known: %v", err)
	}
	var none nodeapi.Sources
	if _, err := none.Spender(ctx, wocTx, 0); !errors.Is(err, nodeapi.ErrSpendUnknown) {
		t.Fatalf("no spends: %v", err)
	}
	if _, _, err := none.Proof(ctx, wocTx); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("no proofs: %v", err)
	}
	if _, err := none.TxRaw(ctx, wocTx); err == nil {
		t.Fatal("no tx source")
	}
	if _, err := none.Known(ctx, wocTx); err == nil {
		t.Fatal("no known source")
	}
	if err := none.SpentElsewhere(ctx, transaction.NewTransaction()); err != nil {
		t.Fatal(err)
	}
}

func TestWaitOn(t *testing.T) {
	ctx := context.Background()
	srv, w := newWoC(t, realRoutes(t))
	if _, h, err := nodeapi.WaitMinedOn(ctx, w, wocTx, time.Millisecond); err != nil || h != 900000 {
		t.Fatal(err)
	}
	// Not mined, and an input spent elsewhere: the wait ends at once.
	parent, _ := chainhash.NewHashFromHex(wocParent)
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: parent, SourceTxOutIndex: 4, SequenceNumber: 0xffffffff})
	id := tx.TxID().String()
	srv.set("/tx/"+id+"/beef", answer{422, nil})
	srv.set("/tx/"+id+"/proof/tsc", answer{200, []byte("null")})
	if _, _, err := nodeapi.WaitSettledOn(ctx, w, w, tx, time.Millisecond); !errors.Is(err, nodeapi.ErrDoubleSpent) {
		t.Fatalf("%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, _, err := nodeapi.WaitMinedOn(cctx, w, id, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if _, _, err := nodeapi.WaitMinedOn(ctx, nil, id, 0); err == nil {
		t.Fatal("no source")
	}
}

func TestParseChain(t *testing.T) {
	s, err := nodeapi.ParseChain("woc:main", nodeapi.ChainOptions{WoCKey: "k", WoCRate: 10, Headers: headersAt900000()})
	if err != nil || len(s.Tx) != 1 || len(s.Proofs) != 1 {
		t.Fatal(err)
	}
	w, ok := s.Spends.(*nodeapi.WoC)
	if !ok || w.Network != "main" || w.Key != "k" || w.Rate != 10 || s.Knows != nodeapi.KnownSource(w) {
		t.Fatalf("%+v", s)
	}
	s, err = nodeapi.ParseChain(" asset:http://node:8090/ , woc:test ", nodeapi.ChainOptions{Headers: headersAt900000()})
	if err != nil || len(s.Tx) != 2 || len(s.Proofs) != 2 {
		t.Fatal(err)
	}
	if a, ok := s.Spends.(*nodeapi.Asset); !ok || a.Base != "http://node:8090" {
		t.Fatalf("the first backend answers spends: %T", s.Spends)
	}
	s, err = nodeapi.ParseChain("woc:test,spend=asset:https://node,known=asset:https://node2,proof=woc:main,tx=woc:main", nodeapi.ChainOptions{Headers: headersAt900000()})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := s.Spends.(*nodeapi.Asset); !ok || a.Base != "https://node" {
		t.Fatalf("spend= overrides: %T", s.Spends)
	}
	if a, ok := s.Knows.(*nodeapi.Asset); !ok || a.Base != "https://node2" {
		t.Fatalf("known= overrides: %T", s.Knows)
	}
	if len(s.Tx) != 2 || len(s.Proofs) != 2 {
		t.Fatalf("%d %d", len(s.Tx), len(s.Proofs))
	}
	for _, bad := range []string{"", " , ", "woc:regtest", "woc:", "asset:", "asset:ftp://x", "node:8090", "gp:main", "spends=woc:main", "http://node"} {
		if _, err := nodeapi.ParseChain(bad, nodeapi.ChainOptions{Headers: headersAt900000()}); !errors.Is(err, nodeapi.ErrChainSpec) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := nodeapi.ParseChain("woc:main", nodeapi.ChainOptions{}); !errors.Is(err, nodeapi.ErrChainSpec) {
		t.Fatalf("no headers: %v", err)
	}
	for _, p := range s.Proofs {
		if _, ok := p.(nodeapi.Checked); !ok {
			t.Fatalf("an unchecked proof source: %T", p)
		}
	}
}

// End to end with no node: ParseChain over WhatsOnChain answers a proof
// only once the headers hold its root.
func TestParseChainChecksProofs(t *testing.T) {
	ctx := context.Background()
	srv, _ := newWoC(t, realRoutes(t))
	s, err := nodeapi.ParseChain("woc:main", nodeapi.ChainOptions{Client: srv.Client(), WoCRate: 1000, Headers: headersAt900000()})
	if err != nil {
		t.Fatal(err)
	}
	s.Spends.(*nodeapi.WoC).Base = srv.URL
	if _, h, err := s.Proof(ctx, wocTx); err != nil || h != 900000 {
		t.Fatalf("%d %v", h, err)
	}
	bad, _ := nodeapi.ParseChain("woc:main", nodeapi.ChainOptions{Client: srv.Client(), WoCRate: 1000,
		Headers: &goldentest.Tracker{Roots: map[uint32]string{900000: strings.Repeat("22", 32)}}})
	bad.Spends.(*nodeapi.WoC).Base = srv.URL
	if _, _, err := bad.Proof(ctx, wocTx); !errors.Is(err, nodeapi.ErrProofRefused) {
		t.Fatalf("a root the headers do not hold: %v", err)
	}
}
