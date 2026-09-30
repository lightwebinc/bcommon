package bwallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// fakeNode is one server speaking both the JSON-RPC and asset APIs, with
// bodies shaped like the ones a Teranode node answers.
// They are not captured from a live node.
type fakeNode struct {
	mu       sync.Mutex
	fundLock string // hex script the coinbase pays to
	blocks   []string
	rpcCalls int
	srv      *httptest.Server
	// empty is the heights whose coinbase pays the fund script nothing: a
	// block of no fees once the subsidy is gone.
	empty map[int]bool
}

func newFakeNode(t *testing.T, fundLock string) *fakeNode {
	t.Helper()
	n := &fakeNode{fundLock: fundLock}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) hashAt(height int) string {
	return fmt.Sprintf("%064x", uint64(0xb10c)<<16|uint64(height))
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	if r.Method == http.MethodPost {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "generatetoaddress" {
			http.Error(w, "bad rpc", http.StatusBadRequest)
			return
		}
		n.rpcCalls++
		count := int(req.Params[0].(float64))
		var hashes []string
		for i := 0; i < count; i++ {
			n.blocks = append(n.blocks, n.hashAt(len(n.blocks)+1))
			hashes = append(hashes, n.blocks[len(n.blocks)-1])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": hashes, "error": nil})
		return
	}
	switch {
	case r.URL.Path == "/api/v1/bestblockheader/json":
		fmt.Fprintf(w, `{"hash":%q,"height":%d}`, n.hashAt(len(n.blocks)), len(n.blocks))
	case r.URL.Path == "/api/v1/blocks":
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		h := len(n.blocks) - off
		fmt.Fprintf(w, `{"data":[{"height":%d,"hash":%q}]}`, h, n.hashAt(h))
	case strings.HasPrefix(r.URL.Path, "/api/v1/block/"):
		hash := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/block/"), "/json")
		for i, b := range n.blocks {
			if b != hash {
				continue
			}
			height := i + 1
			sats := uint64(5000000000)
			if n.empty[height] {
				sats = 0
			}
			fmt.Fprintf(w, `{"hash":%q,"height":%d,"coinbase_tx":{"txid":%q,"outputs":[`+
				`{"satoshis":%d,"lockingScript":%q},`+
				`{"satoshis":1,"lockingScript":"6a"}]}}`,
				hash, height, fmt.Sprintf("%064x", height), sats, n.fundLock)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func TestFundFromCoinbaseBatchesAndAddsMatureLater(t *testing.T) {
	e := newWallet(t)
	lock, err := e.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	node := newFakeNode(t, hex.EncodeToString(*lock))
	rpc := &nodeapi.RPC{URL: node.srv.URL + "/", User: "u", Pass: "p", ID: "app-under-test"}
	asset := &nodeapi.Asset{Base: node.srv.URL}

	added, hashes, err := FundFromCoinbase(context.Background(), e.Signer(), e.Pool, rpc, asset, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if added != 5 || len(hashes) != 5 {
		t.Fatalf("added %d hashes %d, want 5 and 5", added, len(hashes))
	}
	if node.rpcCalls != 3 {
		t.Fatalf("generatetoaddress called %d times, want 3 (batches of 2,2,1)", node.rpcCalls)
	}
	outs := e.Pool.Outputs()
	for i, o := range outs {
		if !o.Coinbase || o.Height != uint32(i+1) || o.Satoshis != 5000000000 || o.Vout != 0 {
			t.Fatalf("output %d: %+v", i, o)
		}
	}
	// The other coinbase output (OP_RETURN) was not ours and was not added.
	if e.Pool.Count() != 5 {
		t.Fatalf("pool holds %d, want 5", e.Pool.Count())
	}
	if _, err := e.Pool.Take(100); !errors.Is(err, ErrNoSpendable) {
		t.Fatal("fresh coinbase must not be spendable at tip 100")
	}
	if o, err := e.Pool.Take(101); err != nil || o.Height != 1 {
		t.Fatalf("Take at 101: %+v %v, want the height-1 coinbase", o, err)
	}

	if _, _, err := FundFromCoinbase(context.Background(), e.Signer(), e.Pool, rpc, asset, 0, 0); err == nil {
		t.Fatal("blocks=0 must be refused")
	}
}

// A coinbase output of zero satoshis is no coin: funding and a rescan both
// leave it out of the pool, where it would only be taken to fail.
func TestFundSkipsAZeroValueCoinbase(t *testing.T) {
	e := newWallet(t)
	lock, err := e.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	node := newFakeNode(t, hex.EncodeToString(*lock))
	node.empty = map[int]bool{2: true, 3: true}
	rpc := &nodeapi.RPC{URL: node.srv.URL + "/", User: "u", Pass: "p", ID: "app-under-test"}
	asset := &nodeapi.Asset{Base: node.srv.URL}

	added, hashes, err := FundFromCoinbase(context.Background(), e.Signer(), e.Pool, rpc, asset, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || len(hashes) != 4 {
		t.Fatalf("added %d of %d blocks, want 2 of 4", added, len(hashes))
	}
	for _, o := range e.Pool.Outputs() {
		if o.Satoshis == 0 || o.Height == 2 || o.Height == 3 {
			t.Fatalf("a zero-value coinbase was pooled: %+v", o)
		}
	}
	if n, err := Rescan(context.Background(), e.Signer(), e.Pool, asset, 1, 4); err != nil || n != 0 {
		t.Fatalf("a rescan added %d: %v", n, err)
	}
}

func TestRescanRecoversWhatFundingDidNotRecord(t *testing.T) {
	e := newWallet(t)
	lock, err := e.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	node := newFakeNode(t, hex.EncodeToString(*lock))
	rpc := &nodeapi.RPC{URL: node.srv.URL + "/", User: "u", Pass: "p", ID: "app-under-test"}
	asset := &nodeapi.Asset{Base: node.srv.URL}
	if _, _, err := FundFromCoinbase(context.Background(), e.Signer(), e.Pool, rpc, asset, 4, 0); err != nil {
		t.Fatal(err)
	}
	// Lose the pool file: the blocks were mined but nothing was recorded.
	if err := os.Remove(filepath.Join(e.Dir(), walletFile)); err != nil {
		t.Fatal(err)
	}
	e, err = Open(e.Dir(), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if e.Pool.Count() != 0 {
		t.Fatal("pool should be empty after losing the file")
	}
	added, err := Rescan(context.Background(), e.Signer(), e.Pool, asset, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 || e.Pool.Count() != 3 {
		t.Fatalf("rescan 2..4 added %d, pool %d, want 3 and 3", added, e.Pool.Count())
	}
	// Idempotent: a second pass over the same heights adds nothing.
	added, err = Rescan(context.Background(), e.Signer(), e.Pool, asset, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || e.Pool.Count() != 4 {
		t.Fatalf("rescan 1..4 added %d, pool %d, want 1 (height 1 only) and 4", added, e.Pool.Count())
	}
	if _, err := Rescan(context.Background(), e.Signer(), e.Pool, asset, 4, 2); err == nil {
		t.Fatal("inverted range must be refused")
	}
	if _, err := Rescan(context.Background(), e.Signer(), e.Pool, asset, 1, 40); err == nil {
		t.Fatal("a height above the tip must be an error, not a silent stop")
	}
}
