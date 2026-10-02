package testchain_test

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
)

const start = 700

// rig is a chain with mature coinbase paying the fixed test key.
type rig struct {
	t     *testing.T
	chain *testchain.Chain
	key   *ec.PrivateKey
	lock  *script.Script
	coins []*transaction.Transaction
	next  int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	key := goldentest.FixedKey()
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	c := testchain.New(start)
	const blocks = testchain.Maturity + 20
	if hashes, err := c.Generate(blocks, addr.AddressString); err != nil || len(hashes) != blocks {
		t.Fatalf("generate: %d hashes, %v", len(hashes), err)
	}
	if c.Height() != start+blocks {
		t.Fatalf("tip %d", c.Height())
	}
	r := &rig{t: t, chain: c, key: key, lock: lock}
	for _, id := range c.Txids() {
		r.coins = append(r.coins, c.Tx(id))
	}
	// Oldest first: only those are mature.
	for i := range r.coins {
		for j := i + 1; j < len(r.coins); j++ {
			_, hi, _ := c.Proof(r.coins[i].TxID().String())
			_, hj, _ := c.Proof(r.coins[j].TxID().String())
			if hj < hi {
				r.coins[i], r.coins[j] = r.coins[j], r.coins[i]
			}
		}
	}
	return r
}

// spend is a signed transaction spending output vout of src to the test
// key, less a fee.
func (r *rig) spend(src *transaction.Transaction, vout uint32) *transaction.Transaction {
	r.t.Helper()
	unlock, err := p2pkh.Unlock(r.key, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(src, vout, unlock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: src.Outputs[vout].Satoshis - 500, LockingScript: r.lock})
	if err := tx.Sign(); err != nil {
		r.t.Fatal(err)
	}
	return tx
}

// coin is a transaction spending the next mature coinbase.
func (r *rig) coin() *transaction.Transaction {
	r.t.Helper()
	r.next++
	return r.spend(r.coins[r.next-1], 0)
}

func TestSendMinesAndProves(t *testing.T) {
	r := newRig(t)
	c := r.chain
	ctx := context.Background()
	tx := r.coin()
	id := tx.TxID().String()
	tip := c.Height()
	if err := c.Send(tx); err != nil {
		t.Fatal(err)
	}
	mp, h, ok := c.Proof(id)
	if !ok || h != tip+1 || !c.Mined(id) || c.Tx(id) == nil || c.Sent != 1 {
		t.Fatalf("mined %v at %d, sent %d", ok, h, c.Sent)
	}
	if valid, err := mp.Verify(ctx, tx.TxID(), c); err != nil || !valid {
		t.Fatalf("the proof does not verify against the chain as a tracker: %v", err)
	}
	if got, _ := c.CurrentHeight(ctx); got != tip+1 {
		t.Errorf("CurrentHeight %d", got)
	}
	if valid, _ := c.IsValidRootForHeight(ctx, tx.TxID(), h); valid {
		t.Error("any hash is a root")
	}
	if valid, _ := c.IsValidRootForHeight(ctx, nil, h); valid {
		t.Error("a nil root is valid")
	}
	// Sent again, it is known: no error, nothing counted twice.
	if err := c.Send(tx); err != nil || c.Sent != 1 {
		t.Errorf("again: %v, sent %d", err, c.Sent)
	}
	// Its output is spendable at once, and only once.
	child := r.spend(tx, 0)
	if err := c.Send(child); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(r.spendTo(tx, 0, 400)); err == nil || !strings.Contains(err.Error(), "missing or spent") {
		t.Errorf("a second spend of one output: %v", err)
	}
}

// spendTo is spend with another fee, so that the same output is spent by
// other bytes.
func (r *rig) spendTo(src *transaction.Transaction, vout uint32, fee uint64) *transaction.Transaction {
	r.t.Helper()
	unlock, err := p2pkh.Unlock(r.key, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(src, vout, unlock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: src.Outputs[vout].Satoshis - fee, LockingScript: r.lock})
	if err := tx.Sign(); err != nil {
		r.t.Fatal(err)
	}
	return tx
}

func TestSendRefusesWhatANodeRefuses(t *testing.T) {
	r := newRig(t)
	c := r.chain
	// Immature coinbase: the newest block's.
	young := r.spend(r.coins[len(r.coins)-1], 0)
	if err := c.Send(young); err == nil || !strings.Contains(err.Error(), "immature") {
		t.Errorf("immature coinbase: %v", err)
	}
	// A script that does not verify: signed by another key.
	other, _ := ec.NewPrivateKey()
	unlock, _ := p2pkh.Unlock(other, nil)
	bad := transaction.NewTransaction()
	bad.AddInputFromTx(r.coins[0], 0, unlock)
	bad.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: r.lock})
	if err := bad.Sign(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(bad); err == nil || !strings.Contains(err.Error(), "script") {
		t.Errorf("another key's signature: %v", err)
	}
	// Non-final: a locktime with an input that is not final.
	unlock, _ = p2pkh.Unlock(r.key, nil)
	late := transaction.NewTransaction()
	late.LockTime = 4102444800
	late.AddInputFromTx(r.coins[0], 0, unlock)
	late.Inputs[0].SequenceNumber = 0
	late.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: r.lock})
	if err := late.Sign(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(late); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Errorf("a non-final transaction: %v", err)
	}
	// The caller's refusal.
	c.Refuse = func(*transaction.Transaction) string { return "policy" }
	if err := c.Send(r.coin()); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Errorf("Refuse: %v", err)
	}
	if c.Sent != 0 {
		t.Errorf("%d refused transactions counted as sent", c.Sent)
	}
}

func TestHoldAndMine(t *testing.T) {
	r := newRig(t)
	c := r.chain
	c.SetHold(true)
	a, b := r.coin(), r.coin()
	for _, tx := range []*transaction.Transaction{a, b} {
		if err := c.Send(tx); err != nil {
			t.Fatal(err)
		}
	}
	tip := c.Height()
	if c.Waiting() != 2 || c.Mined(a.TxID().String()) {
		t.Fatalf("%d waiting", c.Waiting())
	}
	// A held transaction's output is spendable before it mines.
	child := r.spend(a, 0)
	if err := c.Send(child); err != nil {
		t.Fatalf("spending a held output: %v", err)
	}
	if n := c.Mine(); n != 3 || c.Waiting() != 0 || c.Height() != tip+3 {
		t.Fatalf("mined %d, tip %d", n, c.Height())
	}
	_, ha, _ := c.Proof(a.TxID().String())
	_, hc, _ := c.Proof(child.TxID().String())
	if hc <= ha {
		t.Errorf("a child mined at %d, its parent at %d", hc, ha)
	}
	c.SetHold(false)
	c.SetHoldIf(func(tx *transaction.Transaction) bool { return len(tx.Outputs) == 1 && tx.Outputs[0].Satoshis%2 == 1 })
	free := r.coin()
	if err := c.Send(free); err != nil || !c.Mined(free.TxID().String()) {
		t.Fatalf("a transaction HoldIf passes: %v", err)
	}
	held := r.spendTo(r.coins[r.next], 0, 501)
	r.next++
	if err := c.Send(held); err != nil || c.Mined(held.TxID().String()) || c.Waiting() != 1 {
		t.Fatalf("a transaction HoldIf holds: %v, waiting %d", err, c.Waiting())
	}
}

// The chain over HTTP, through the clients this module ships: a node's
// RPC and asset API, a header source, an ARC-compatible broadcaster and a
// fabric ingress.
func TestServedThroughTheClients(t *testing.T) {
	r := newRig(t)
	c := r.chain
	ctx := context.Background()
	srv := httptest.NewServer(c)
	defer srv.Close()
	rpc := &nodeapi.RPC{URL: srv.URL + "/rpc", ID: "t"}
	asset := &nodeapi.Asset{Base: srv.URL}

	info, err := rpc.GetInfo(ctx)
	if err != nil || info.Blocks != int64(c.Height()) {
		t.Fatalf("getinfo: %+v, %v", info, err)
	}

	// The RPC settlement leg.
	viaRPC := r.coin()
	if err := (&publish.RPCSettler{RPC: rpc}).Submit(ctx, viaRPC); err != nil {
		t.Fatal(err)
	}
	mp, h, err := asset.Proof(ctx, viaRPC.TxID().String())
	if err != nil || h != c.Height() {
		t.Fatalf("the asset API's proof: %v", err)
	}
	hc := headers.New(srv.URL)
	if valid, err := mp.Verify(ctx, viaRPC.TxID(), hc); err != nil || !valid {
		t.Fatalf("the proof against the header source: %v", err)
	}
	if tip, err := hc.CurrentHeight(ctx); err != nil || tip != c.Height() {
		t.Fatalf("the header source's tip: %d, %v", tip, err)
	}
	raw, err := asset.TxRaw(ctx, viaRPC.TxID().String())
	if err != nil || string(raw) != string(viaRPC.Bytes()) {
		t.Fatalf("the asset API's transaction: %v", err)
	}

	// The broadcaster.
	arcade := &publish.Arcade{Base: srv.URL + "/arcade", Poll: 5 * time.Millisecond, Verdict: 2 * time.Second}
	viaArcade := r.coin()
	if err := arcade.Submit(ctx, viaArcade); err != nil {
		t.Fatal(err)
	}
	st, err := arcade.Status(ctx, viaArcade.TxID().String())
	if err != nil || !st.Mined() {
		t.Fatalf("arcade status: %+v, %v", st, err)
	}
	if err := arcade.Submit(ctx, r.spendTo(r.coins[r.next-1], 0, 400)); err == nil {
		t.Fatal("arcade took a second spend of one output")
	}
	busy := true
	c.Busy = func(*transaction.Transaction) bool { return busy }
	waiting := r.coin()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err = (&publish.RPCSettler{RPC: rpc}).Submit(short, waiting)
	cancel()
	if err == nil {
		t.Fatal("a busy chain took a transaction")
	}
	if c.Tx(waiting.TxID().String()) != nil {
		t.Fatal("a busy answer recorded the transaction")
	}
	busy = false

	// A competing spend the chain took from someone else.
	spentBy := strings.Repeat("ab", 32)
	c.SpendElsewhere(viaArcade.TxID().String(), 0, spentBy)
	if by, err := asset.Spender(ctx, viaArcade.TxID().String(), 0); err != nil || by != spentBy {
		t.Fatalf("the spender: %q, %v", by, err)
	}
	if err := asset.SpentElsewhere(ctx, r.spend(viaArcade, 0)); !errors.Is(err, nodeapi.ErrDoubleSpent) {
		t.Fatalf("a spend of an output spent elsewhere: %v", err)
	}

	// The ingress: Extended Format in, nothing back.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c.Ingress(l)
	viaIngress := r.spend(viaRPC, 0)
	if err := (&publish.TCPIngress{Addr: l.Addr().String()}).Submit(ctx, viaIngress); err != nil {
		t.Fatal(err)
	}
	until(t, "the ingress to mine", func() bool { return c.Mined(viaIngress.TxID().String()) })
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(r.coin().Bytes()); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	until(t, "the ingress to refuse a raw transaction", func() bool { return c.IngressRefused() == 1 })
}

// With RefuseKnown a transaction the chain holds is refused when it is sent
// again, mined, waiting or displaced, by Send and through the RPC; the
// broadcaster still answers its status. Without it nothing changes.
func TestRefuseKnown(t *testing.T) {
	r := newRig(t)
	c := r.chain
	ctx := context.Background()
	srv := httptest.NewServer(c)
	defer srv.Close()
	leg := &publish.RPCSettler{RPC: &nodeapi.RPC{URL: srv.URL + "/rpc", ID: "t"}}
	arcade := &publish.Arcade{Base: srv.URL + "/arcade", Poll: 5 * time.Millisecond, Verdict: 2 * time.Second}

	mined := r.coin()
	if err := c.Send(mined); err != nil {
		t.Fatal(err)
	}
	c.SetHold(true)
	waiting, displaced := r.coin(), r.coin()
	for _, tx := range []*transaction.Transaction{waiting, displaced} {
		if err := c.Send(tx); err != nil {
			t.Fatal(err)
		}
	}
	// A competing spend takes the third one's coin; the chain keeps and
	// serves the transaction that lost.
	c.SpendElsewhere(displaced.Inputs[0].SourceTXID.String(), 0, strings.Repeat("cd", 32))
	if c.Tx(displaced.TxID().String()) == nil || c.Sent != 3 {
		t.Fatalf("sent %d", c.Sent)
	}
	all := map[string]*transaction.Transaction{"mined": mined, "waiting": waiting, "displaced": displaced}

	// The default: each is taken again with nothing done.
	for what, tx := range all {
		if err := c.Send(tx); err != nil {
			t.Fatalf("%s, sent again: %v", what, err)
		}
		if err := leg.Submit(ctx, tx); err != nil {
			t.Fatalf("%s, sent again through the RPC: %v", what, err)
		}
	}
	if c.Sent != 3 || c.Waiting() != 2 {
		t.Fatalf("sent %d, waiting %d", c.Sent, c.Waiting())
	}

	c.SetRefuseKnown(true)
	for what, tx := range all {
		if err := c.Send(tx); !errors.Is(err, testchain.ErrAlreadyKnown) {
			t.Fatalf("%s, sent again: %v", what, err)
		}
		if err := leg.Submit(ctx, tx); err == nil || !strings.Contains(err.Error(), "txn-already-known") {
			t.Fatalf("%s, sent again through the RPC: %v", what, err)
		}
		// The broadcaster answers the status of what it holds.
		if err := arcade.Submit(ctx, tx); err != nil {
			t.Fatalf("%s, sent again through the broadcaster: %v", what, err)
		}
		if st, err := arcade.Status(ctx, tx.TxID().String()); err != nil || st.Mined() != (what == "mined") {
			t.Fatalf("%s: the broadcaster's status %+v, %v", what, st, err)
		}
	}
	if c.Sent != 3 || c.Waiting() != 2 {
		t.Fatalf("sent %d, waiting %d", c.Sent, c.Waiting())
	}
	// A transaction the chain does not hold is taken as before.
	fresh := r.coin()
	if err := leg.Submit(ctx, fresh); err != nil || c.Sent != 4 {
		t.Fatalf("a new transaction: %v, sent %d", err, c.Sent)
	}
	c.SetRefuseKnown(false)
	if err := c.Send(fresh); err != nil || c.Sent != 4 {
		t.Fatalf("with the refusal off again: %v, sent %d", err, c.Sent)
	}
}

func until(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
