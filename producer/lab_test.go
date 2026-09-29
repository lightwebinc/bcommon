package producer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// testProfile is a neutral wallet profile under the protocol reserved for
// tests.
var testProfile = bwallet.Profile{
	FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
	FundKeyID:      "coin",
	FundBasket:     "vector sample coin",
	Version:        "vector-sample-1",
	LegacyPoolFile: "coins.json",
}

// testParams are the carrier parameters reserved for tests.
var testParams = carrier.Params{
	Derivation: pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
		KeyID:    "object",
	},
	FundingTag:      []byte{'v', 'x', 0x02},
	ValidatePayload: func([]byte) error { return nil },
}

const testTopic = "tm_vector_sample"

// signerOf is a Signer over an in-process wallet holding key, under the test
// profile.
func signerOf(t testing.TB, key *ec.PrivateKey) *bwallet.Signer {
	t.Helper()
	w, err := wallet.NewCompletedProtoWallet(key)
	if err != nil {
		t.Fatal(err)
	}
	return &bwallet.Signer{Interface: w, Identity: key.PubKey(), Originator: "example.com", Profile: testProfile}
}

func newKey(t testing.TB) *ec.PrivateKey {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func poolIn(t testing.TB) *bwallet.Pool {
	t.Helper()
	p, err := bwallet.LoadPool(filepath.Join(t.TempDir(), "wallet.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// proofAt is a stand-in proof placing txid at offset 1 of a two-transaction
// block at height.
func proofAt(txid *chainhash.Hash, height uint32) *transaction.MerklePath {
	sibling := chainhash.Hash(goldentest.Fill(0x33))
	isTxid := true
	return transaction.NewMerklePath(height, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: txid, Txid: &isTxid},
	}})
}

// coinFor is a proven transaction paying sats to lock at height 90, whose
// one input spends nothing real.
func coinFor(lock *script.Script, sats uint64, salt byte) *transaction.Transaction {
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(goldentest.Fill(salt))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
	coin.MerklePath = proofAt(coin.TxID(), 90)
	return coin
}

// fund adds a proven coin of sats paying s's fund key to pool, and returns
// its parent.
func fund(t testing.TB, pool *bwallet.Pool, s *bwallet.Signer, sats uint64, salt byte) *transaction.Transaction {
	t.Helper()
	lock, err := s.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	coin := coinFor(lock, sats, salt)
	if _, err := pool.Add(bwallet.Output{TxID: coin.TxID().String(), Vout: 0, Satoshis: sats,
		LockingScript: lock.String(), Height: 90, Raw: coin.Hex(), Bump: coin.MerklePath.Hex()}); err != nil {
		t.Fatal(err)
	}
	return coin
}

// notes collects Note lines.
type notes struct {
	mu    sync.Mutex
	lines []string
}

func (n *notes) note(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, fmt.Sprintf(format, args...))
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.lines...)
}

func (n *notes) String() string { return strings.Join(n.all(), "\n") }

// lab is one in-process server speaking a node's asset API, arcade's API and
// an overlay host's submit route, over a chain that mines what a test tells
// it to.
type lab struct {
	srv *httptest.Server

	mu sync.Mutex
	// known is every transaction the lab can serve raw, by txid.
	known map[string]*transaction.Transaction
	// accepted is what arcade took; mined what is in a block, at a height;
	// refused what the network refused, with why.
	accepted map[string]bool
	mined    map[string]uint32
	refused  map[string]string
	// mineOnSubmit mines everything arcade takes at the next height.
	mineOnSubmit bool
	height       uint32
	// submitted is every body the overlay host was given, with its topic.
	submitted []submission
	// facadeDown answers every submit with a 503.
	facadeDown bool
	// arcadeCalls counts status requests.
	arcadeCalls int
}

type submission struct {
	topic string
	beef  []byte
}

func newLab(t testing.TB) *lab {
	t.Helper()
	l := &lab{known: map[string]*transaction.Transaction{}, accepted: map[string]bool{},
		mined: map[string]uint32{}, refused: map[string]string{}, height: 700}
	l.srv = httptest.NewServer(http.HandlerFunc(l.serve))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *lab) arcade() *publish.Arcade {
	return &publish.Arcade{Base: l.srv.URL + "/arc", Poll: 10 * time.Millisecond, Verdict: 200 * time.Millisecond}
}

func (l *lab) asset() *nodeapi.Asset { return &nodeapi.Asset{Base: l.srv.URL + "/node"} }

func (l *lab) facade() *publish.Facade { return &publish.Facade{Base: l.srv.URL + "/host"} }

// mine puts each transaction in a block of its own at the next height.
func (l *lab) mine(txids ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range txids {
		l.height++
		l.mined[id] = l.height
	}
}

func (l *lab) know(txs ...*transaction.Transaction) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, tx := range txs {
		l.known[tx.TxID().String()] = tx
	}
}

func (l *lab) submissions() []submission {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]submission(nil), l.submitted...)
}

func (l *lab) proof(txid string) (*transaction.MerklePath, uint32, bool) {
	h, ok := l.mined[txid]
	if !ok {
		return nil, 0, false
	}
	id, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, 0, false
	}
	return proofAt(id, h), h, true
}

func (l *lab) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/arc/tx":
		body, _ := io.ReadAll(r.Body)
		tx, err := transaction.NewTransactionFromBytes(body)
		if err != nil {
			http.Error(w, `{"reason":"not a transaction"}`, http.StatusBadRequest)
			return
		}
		id := tx.TxID().String()
		l.known[id] = tx
		if why, ok := l.refused[id]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": id, "txStatus": "REJECTED", "extraInfo": why})
			return
		}
		l.accepted[id] = true
		if l.mineOnSubmit {
			l.height++
			l.mined[id] = l.height
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"txid": id, "txStatus": "SEEN_ON_NETWORK"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/arc/tx/"):
		l.arcadeCalls++
		id := strings.TrimPrefix(path, "/arc/tx/")
		if why, ok := l.refused[id]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": id, "txStatus": "REJECTED", "extraInfo": why})
			return
		}
		if mp, h, ok := l.proof(id); ok && l.accepted[id] {
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": id, "txStatus": "MINED", "merklePath": mp.Hex(), "blockHeight": h})
			return
		}
		if l.accepted[id] {
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": id, "txStatus": "SEEN_ON_NETWORK"})
			return
		}
		http.NotFound(w, r)
	case strings.HasPrefix(path, "/node/api/v1/txmeta/"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/node/api/v1/txmeta/"), "/json")
		h, ok := l.mined[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"blockHashes":[%q],"blockHeights":[%d],"subtreeIdxs":[0],"mainChainIndex":0}`, fmt.Sprintf("%064x", h), h)
	case strings.HasPrefix(path, "/node/api/v1/merkle_proof/"):
		mp, _, ok := l.proof(strings.TrimPrefix(path, "/node/api/v1/merkle_proof/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(mp.Bytes())
	case strings.HasPrefix(path, "/node/api/v1/tx/"):
		tx, ok := l.known[strings.TrimPrefix(path, "/node/api/v1/tx/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(tx.Bytes())
	case r.Method == http.MethodPost && path == "/host/submit":
		if l.facadeDown {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		topic := r.Header.Get("x-topics")
		l.submitted = append(l.submitted, submission{topic, body})
		_ = json.NewEncoder(w).Encode(map[string]any{"STEAK": map[string]any{topic: map[string]any{"outputsToAdmit": []int{0}, "coinsToRetain": []int{}}}})
	default:
		http.NotFound(w, r)
	}
}

// subjectOf parses a submitted BEEF's subject transaction.
func subjectOf(t testing.TB, beef []byte) *transaction.Transaction {
	t.Helper()
	_, tx, _, err := transaction.ParseBeef(beef)
	if err != nil || tx == nil {
		t.Fatalf("submitted body is not a BEEF with a subject: %v", err)
	}
	return tx
}

// memTrees is a TreeState in memory.
type memTrees struct {
	cur     *funding.Tree
	all     []funding.Tree
	adopted int
	failing error
}

func (m *memTrees) Current() *funding.Tree { return m.cur }

func (m *memTrees) Adopt(t funding.Tree) error {
	if m.failing != nil {
		return m.failing
	}
	m.cur = &t
	m.all = append(m.all, t)
	m.adopted++
	return nil
}

// payerFor is a Payer over pool with s as its one key, settling to the lab.
func payerFor(l *lab, pool *bwallet.Pool, s *bwallet.Signer, n *notes) *producer.Payer {
	return &producer.Payer{
		Pool: pool, Tip: 100, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
		Kept: &producer.Kept{}, Settler: l.arcade(), Asset: l.asset(), Fees: mint.DefaultFees,
		Poll: 10 * time.Millisecond, Timeout: 5 * time.Second, Note: n.note,
	}
}

// fundingLock is s's funding lock under the test parameters.
func fundingLock(s *bwallet.Signer) func(context.Context) (*script.Script, error) {
	return func(ctx context.Context) (*script.Script, error) {
		return carrier.FundingLock(ctx, s, s.Originator, testParams)
	}
}
