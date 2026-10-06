package nodeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// Fixture bodies are written to the JSON shapes a Teranode node answers,
// not captured from a live node.
const (
	fixtureBestHeader = `{"hash":"000000000000000000a1b2c3d4e5f60718293a4b5c6d7e8f9011223344556677",` +
		`"previousblockhash":"0000000000000000009988776655443322110ffeeddccbbaa9988776655443322","merkleroot":"aa11bb22cc33dd44ee55ff6600778899aabbccddeeff00112233445566778899","height":1200}`
	fixtureTxMeta = `{"blockHashes":["000000000000000000a1b2c3d4e5f60718293a4b5c6d7e8f9011223344556677"],"blockHeights":[1200],"subtreeIdxs":[0],"mainChainIndex":0,"isCoinbase":false}`
	fixtureBlock  = `{"hash":"000000000000000000a1b2c3d4e5f60718293a4b5c6d7e8f9011223344556677","height":1200,"coinbase_tx":{"txid":"cbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcb",` +
		`"outputs":[{"satoshis":5000000000,"lockingScript":"76a914000000000000000000000000000000000000000088ac"},{"satoshis":0,"lockingScript":"6a"}]}}`
)

func route(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func text(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// testBump is a one-level BUMP placing txid at offset 0 beside one sibling.
func testBump(t *testing.T, txid *chainhash.Hash, height uint32) *transaction.MerklePath {
	t.Helper()
	sibling, err := chainhash.NewHashFromHex(strings.Repeat("5a", 32))
	if err != nil {
		t.Fatal(err)
	}
	isTxid := true
	return &transaction.MerklePath{
		BlockHeight: height,
		Path: [][]*transaction.PathElement{{
			{Offset: 0, Hash: txid, Txid: &isTxid},
			{Offset: 1, Hash: sibling},
		}},
	}
}

const txidHex = "1111111111111111111111111111111111111111111111111111111111111111"

func TestHeaders(t *testing.T) {
	srv := route(t, map[string]http.HandlerFunc{
		"/api/v1/bestblockheader/json": text(fixtureBestHeader),
		"/api/v1/header/000000000000000000a1b2c3d4e5f60718293a4b5c6d7e8f9011223344556677/json": text(fixtureBestHeader),
	})
	a := &Asset{Base: srv.URL + "/"}
	best, err := a.BestHeader(context.Background())
	if err != nil || best.Height != 1200 || best.MerkleRoot == "" || best.Prev == "" {
		t.Fatalf("best: %+v %v", best, err)
	}
	h, err := a.Header(context.Background(), best.Hash)
	if err != nil || h.Hash != best.Hash {
		t.Fatalf("header: %+v %v", h, err)
	}
}

func TestTxMetaPlacementAndNotMined(t *testing.T) {
	srv := route(t, map[string]http.HandlerFunc{
		"/api/v1/txmeta/" + txidHex + "/json":                  text(fixtureTxMeta),
		"/api/v1/txmeta/" + strings.Repeat("22", 32) + "/json": text(`{"blockHashes":[],"blockHeights":[],"subtreeIdxs":[],"mainChainIndex":0}`),
	})
	a := &Asset{Base: srv.URL}
	m, err := a.TxMeta(context.Background(), txidHex)
	if err != nil {
		t.Fatal(err)
	}
	hash, height, err := m.Placement()
	if err != nil || height != 1200 || !strings.HasPrefix(hash, "0000") {
		t.Fatalf("placement: %s %d %v", hash, height, err)
	}
	// Known but unplaced: the asset API answers with empty slices.
	m, err = a.TxMeta(context.Background(), strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Placement(); !errors.Is(err, ErrNotMined) {
		t.Fatalf("unplaced: %v, want ErrNotMined", err)
	}
	// Unknown: 404 is the same state to a caller waiting for a submission.
	if _, err := a.TxMeta(context.Background(), strings.Repeat("33", 32)); !errors.Is(err, ErrNotMined) {
		t.Fatalf("404: %v, want ErrNotMined", err)
	}
}

func TestMerkleProofParsesGuardsAndChecksTheTxid(t *testing.T) {
	txid, err := chainhash.NewHashFromHex(txidHex)
	if err != nil {
		t.Fatal(err)
	}
	other, err := chainhash.NewHashFromHex(strings.Repeat("44", 32))
	if err != nil {
		t.Fatal(err)
	}
	good := testBump(t, txid, 1200).Bytes()
	wrong := testBump(t, other, 1200).Bytes()
	big := make([]byte, maxBody+1)
	srv := route(t, map[string]http.HandlerFunc{
		"/api/v1/merkle_proof/" + txidHex:                  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(good) },
		"/api/v1/merkle_proof/" + strings.Repeat("aa", 32): status(http.StatusNotFound),
		"/api/v1/merkle_proof/" + strings.Repeat("bb", 32): status(http.StatusInternalServerError),
		"/api/v1/merkle_proof/" + strings.Repeat("cc", 32): func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(wrong) },
		"/api/v1/merkle_proof/" + strings.Repeat("dd", 32): func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(big) },
		"/api/v1/merkle_proof/" + strings.Repeat("ee", 32): func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(good[:20]) },
		"/api/v1/merkle_proof/" + strings.Repeat("ff", 32): status(http.StatusForbidden),
	})
	a := &Asset{Base: srv.URL}
	ctx := context.Background()

	mp, err := a.MerkleProof(ctx, txidHex)
	if err != nil {
		t.Fatal(err)
	}
	if mp.BlockHeight != 1200 || !bytes.Equal(mp.Bytes(), good) {
		t.Fatalf("proof did not round-trip: height %d", mp.BlockHeight)
	}
	root, err := mp.ComputeRoot(txid)
	if err != nil || root == nil {
		t.Fatalf("root: %v %v", root, err)
	}
	for _, c := range []struct{ id, want string }{
		{strings.Repeat("aa", 32), "not mined"},
		{strings.Repeat("bb", 32), "not mined"},
		{strings.Repeat("cc", 32), "does not contain the txid"},
		{strings.Repeat("dd", 32), "exceeds"},
		{strings.Repeat("ee", 32), "mid-structure"},
		{strings.Repeat("ff", 32), "http 403"},
	} {
		_, err := a.MerkleProof(ctx, c.id)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.id[:4], err, c.want)
		}
		if c.want == "not mined" && !errors.Is(err, ErrNotMined) {
			t.Errorf("%s: %v is not ErrNotMined", c.id[:4], err)
		}
		// One bound, one refusal: the asset API answers with the same
		// sentinel the RPC leg does, so a caller matches on one error.
		if c.want == "exceeds" && !errors.Is(err, ErrBodyTooLarge) {
			t.Errorf("%s: %v is not ErrBodyTooLarge", c.id[:4], err)
		}
	}
}

// ProofFor may be handed bytes from any source, and some of them never passed
// Asset.get's bound, so the bound it hands the guard is the size check the
// library guarantees for them. It must be the same 1 MiB every answer here is
// held to: a larger one, or none, admits proofs no caller budgeted for, and a
// much smaller one refuses real proofs. The texts are literal so a change to
// the bound shows up as a changed number, from either side.
func TestProofForHoldsTheProofToTheBodyBound(t *testing.T) {
	_, err := ProofFor(make([]byte, maxBody+1), txidHex)
	if err == nil || err.Error() != "bump is 1048577 bytes, max 1048576" {
		t.Fatalf("one byte over: err %v, want exactly %q", err, "bump is 1048577 bytes, max 1048576")
	}
	// Exactly at the bound is not a size refusal. All zeros is a block height
	// of 0 and an empty tree, so what the guard objects to is the rest.
	_, err = ProofFor(make([]byte, maxBody), txidHex)
	if err == nil || err.Error() != "bump has 1048574 trailing bytes" {
		t.Fatalf("at the bound: err %v, want exactly %q", err, "bump has 1048574 trailing bytes")
	}
	// One byte over and malformed from its first level: the size refuses it
	// before the walk reaches the absurd leaf count, so an over-bound proof is
	// never walked.
	absurd := make([]byte, maxBody+1)
	copy(absurd, []byte{0x65, 0x01, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF})
	_, err = ProofFor(absurd, txidHex)
	if err == nil || err.Error() != "bump is 1048577 bytes, max 1048576" {
		t.Fatalf("over and malformed: err %v, want exactly %q", err, "bump is 1048577 bytes, max 1048576")
	}
}

// The bytes are judged before the txid is read: a caller handed a bad proof
// for a bad txid hears about the proof, the part a service supplied.
func TestProofForGuardsTheProofBeforeTheTxid(t *testing.T) {
	_, err := ProofFor(nil, "zz")
	if err == nil || err.Error() != "bump ends mid-structure" {
		t.Fatalf("bad proof, bad txid: err %v, want exactly %q", err, "bump ends mid-structure")
	}
}

func TestBlockAndTxRaw(t *testing.T) {
	srv := route(t, map[string]http.HandlerFunc{
		"/api/v1/block/abc/json": text(fixtureBlock),
		"/api/v1/tx/" + txidHex:  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(coinbase.Bytes()) },
	})
	a := &Asset{Base: srv.URL}
	b, err := a.Block(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if b.Height != 1200 || len(b.CoinbaseTx.Outputs) != 2 || b.CoinbaseTx.Outputs[0].Satoshis != 5000000000 ||
		!strings.HasPrefix(b.CoinbaseTx.Outputs[0].LockingScript, "76a914") || b.CoinbaseTx.TxID == "" {
		t.Fatalf("block: %+v", b)
	}
	raw, err := a.TxRaw(context.Background(), txidHex)
	if err != nil || !bytes.Equal(raw, coinbase.Bytes()) {
		t.Fatalf("txraw: %x %v", raw, err)
	}
}

// coinbase is a coinbase as a node holds it: its one input spends nothing,
// and in Extended Format carries a zero previous output.
var coinbase = func() *transaction.Transaction {
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{}, SourceTxOutIndex: 0xffffffff,
		UnlockingScript: &script.Script{0x02, 0x01, 0x02}, SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{LockingScript: &script.Script{}})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: &script.Script{script.OpTRUE}})
	return tx
}()

// A node answering in Extended Format, as a Teranode asset API does, is
// read as the raw transaction it extends; a malformed answer in either
// form is the guard's refusal.
func TestTxRawReadsExtendedFormat(t *testing.T) {
	ef, err := coinbase.EF()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body []byte
		ok   bool
	}{
		"extended format":        {ef, true},
		"raw":                    {coinbase.Bytes(), true},
		"truncated EF":           {ef[:len(ef)-1], false},
		"EF of 2^32-1 inputs":    {append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0xef, 0xfe, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)...), false},
		"raw with trailing byte": {append(coinbase.Bytes(), 0), false},
	} {
		srv := route(t, map[string]http.HandlerFunc{
			"/api/v1/tx/" + txidHex: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(c.body) },
		})
		raw, err := (&Asset{Base: srv.URL}).TxRaw(context.Background(), txidHex)
		if !c.ok {
			if !errors.Is(err, guard.ErrTransaction) || raw != nil {
				t.Fatalf("%s: %x %v, want the guard's refusal", name, raw, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(raw, coinbase.Bytes()) {
			t.Fatalf("%s: %x %v", name, raw, err)
		}
		tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
		if err != nil || !tx.TxID().IsEqual(coinbase.TxID()) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestHashAtHeightReadsBackFromTheTip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/bestblockheader/json":
			_, _ = w.Write([]byte(`{"hash":"tip","height":10}`))
		case r.URL.Path == "/api/v1/blocks" && r.URL.RawQuery == "limit=1&offset=3":
			_, _ = w.Write([]byte(`{"data":[{"height":7,"hash":"h7"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := &Asset{Base: srv.URL}
	h, err := a.HashAtHeight(context.Background(), 7)
	if err != nil || h != "h7" {
		t.Fatalf("hash at 7: %q %v", h, err)
	}
	if _, err := a.HashAtHeight(context.Background(), 11); err == nil {
		t.Fatal("above the tip must be an error")
	}
}

func TestAsset429IsRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(fixtureBestHeader))
	}))
	t.Cleanup(srv.Close)
	best, err := (&Asset{Base: srv.URL}).BestHeader(context.Background())
	if err != nil || best.Height != 1200 || hits != 2 {
		t.Fatalf("after 429: %+v %v hits %d", best, err, hits)
	}
}

// The genesis block's header, as a node answers it: public, and its fields
// hash to its hash.
const genesisHash = "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"

func genesisHeader(timeField uint32) string {
	return fmt.Sprintf(`{"hash":%q,"version":1,"previousblockhash":"0000000000000000000000000000000000000000000000000000000000000000",`+
		`"merkleroot":"4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b","time":%d,"bits":"1d00ffff","nonce":2083236893,"height":0}`, genesisHash, timeField)
}

func TestBlockTime(t *testing.T) {
	for _, c := range []struct {
		name   string
		header string
		want   error
	}{
		{"the header hashes to its hash", genesisHeader(1231006505), nil},
		{"a time the hash does not commit to", genesisHeader(1231006506), ErrHeader},
		{"bits that are not hex", strings.Replace(genesisHeader(1231006505), "1d00ffff", "zz", 1), ErrHeader},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := route(t, map[string]http.HandlerFunc{
				"/api/v1/bestblockheader/json":            text(`{"hash":"` + strings.Repeat("ab", 32) + `","height":5}`),
				"/api/v1/blocks":                          text(`{"data":[{"height":0,"hash":"` + genesisHash + `"}]}`),
				"/api/v1/header/" + genesisHash + "/json": text(c.header),
			})
			at, err := (&Asset{Base: srv.URL}).BlockTime(context.Background(), 0)
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("BlockTime: %v, want %v", err, c.want)
				}
				return
			}
			if err != nil || !at.Equal(time.Date(2009, 1, 3, 18, 15, 5, 0, time.UTC)) {
				t.Fatalf("BlockTime: %v %v", at, err)
			}
		})
	}
	// A node that answers another height for the hash it named is refused.
	srv := route(t, map[string]http.HandlerFunc{
		"/api/v1/bestblockheader/json":            text(`{"hash":"` + strings.Repeat("ab", 32) + `","height":5}`),
		"/api/v1/blocks":                          text(`{"data":[{"height":0,"hash":"` + genesisHash + `"}]}`),
		"/api/v1/header/" + genesisHash + "/json": text(strings.Replace(genesisHeader(1231006505), `"height":0`, `"height":1`, 1)),
	})
	if _, err := (&Asset{Base: srv.URL}).BlockTime(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("BlockTime: %v", err)
	}
}
