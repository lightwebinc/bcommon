package payee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/termsafe"
	"github.com/lightwebinc/bcommon/testchain"
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

// rig is a local chain, a funded payer and a payee home.
type rig struct {
	chain *testchain.Chain
	asset *nodeapi.Asset
	rpc   *nodeapi.RPC
	payer *bwallet.Embedded
	payee *bwallet.Embedded
	home  string
	n     int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c := testchain.New(700)
	s := httptest.NewServer(c)
	t.Cleanup(s.Close)
	r := &rig{chain: c, asset: &nodeapi.Asset{Base: s.URL}, rpc: &nodeapi.RPC{URL: s.URL + "/rpc", ID: "t"}, home: t.TempDir()}
	var err error
	if r.payer, err = bwallet.Create(t.TempDir(), testProfile); err != nil {
		t.Fatal(err)
	}
	if r.payee, err = bwallet.Create(r.home, testProfile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(context.Background(), r.payer.Signer(), r.payer.Pool, r.rpc, r.asset, 110, 0); err != nil {
		t.Fatal(err)
	}
	return r
}

// purse is the payee's purse, waiting at most wait for a proof.
func (r *rig) purse(wait time.Duration) *purse.Purse {
	return &purse.Purse{Embedded: r.payee, Settler: &publish.RPCSettler{RPC: r.rpc}, Asset: r.asset, Headers: r.chain,
		Wait: wait, Poll: 5 * time.Millisecond}
}

// pay is a payment of sats to the payee for a question of class, as its
// host records it, and the coin it spends. With fee set it spends that coin.
func (r *rig) pay(t *testing.T, to *bwallet.Embedded, sats uint64, class string, fee *mint.Input) (Payment, mint.Input) {
	t.Helper()
	ctx := context.Background()
	r.n++
	prefix := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "prefix-%d", r.n))
	suffix := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "suffix-%d", r.n))
	dest, err := r.payer.Signer().PaymentDestination(ctx, to.Signer().IdentityHex(), prefix, suffix)
	if err != nil {
		t.Fatal(err)
	}
	var in mint.Input
	if fee != nil {
		in = *fee
	} else {
		pp := &producer.Payer{Pool: r.payer.Pool, Tip: r.chain.Height(), Keys: map[string]*bwallet.Signer{r.payer.Signer().IdentityHex(): r.payer.Signer()},
			Asset: r.asset, Fees: mint.LegacyFees}
		if in, err = pp.TakeAtLeast(ctx, 10_000); err != nil {
			t.Fatal(err)
		}
	}
	change, _ := r.payer.Signer().FundScript()
	tx, err := mint.Payment(ctx, dest, sats, in, change, mint.LegacyFees)
	if err != nil {
		t.Fatal(err)
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	p := Payment{Txid: tx.TxID().String(), Beef: base64.StdEncoding.EncodeToString(beef), Satoshis: sats, DerivationPrefix: prefix,
		DerivationSuffix: suffix, SenderIdentityKey: r.payer.Signer().IdentityHex(), Class: class, At: 1}
	if p.Inputs, err = p.Spends(); err != nil {
		t.Fatal(err)
	}
	return p, in
}

// ledger writes ps as a host's ledger, a line cut short at its end.
func ledger(t *testing.T, ps ...Payment) string {
	t.Helper()
	var b bytes.Buffer
	for _, p := range ps {
		line, err := p.Line()
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
	}
	b.WriteString(`{"txid":"ab`)
	path := filepath.Join(t.TempDir(), LedgerFile)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// home is the payee's record, saved to a file as an application saves its
// state.
type home struct {
	Book
	path  string
	fail  bool
	saves int
}

func (h *home) save() error {
	if h.fail {
		return errors.New("disk full")
	}
	h.saves++
	raw, err := jsonIndent(h.Book)
	if err != nil {
		return err
	}
	return os.WriteFile(h.path, raw, 0o600)
}

type run struct {
	rep        Report
	err        error
	out, warns string
}

func settle(t *testing.T, r *rig, h *home, p *purse.Purse, paths ...string) run {
	t.Helper()
	var out, warn bytes.Buffer
	ps, err := ReadLedgers(&warn, paths...)
	if err != nil {
		t.Fatal(err)
	}
	s := &Settler{App: "sample", Payer: p, Record: Saved(&h.Book, h.save), Pool: r.payee.Pool, Out: &out, Warn: &warn}
	rep, err := s.Settle(context.Background(), ps)
	return run{rep, err, out.String(), warn.String()}
}

// The application's own words for a payment settled and for the run, which
// a run must print byte for byte.
func settledLine(p Payment) string {
	return fmt.Sprintf("settled %s: %d sat for %s from %s\n", p.Txid, p.Satoshis, p.Class, termsafe.Abbrev(p.SenderIdentityKey))
}

func reportLine(settled int, sats uint64, already, failed, refusedNow, unsettleable, count int, balance uint64) string {
	return fmt.Sprintf("%d payment(s) settled, %d sat; %d settled before; %d not settled; %d refused (%d before); pool %d output(s), %d sat\n",
		settled, sats, already, failed, refusedNow, unsettleable, count, balance)
}

// Settle takes every payment of two ledgers into the pool once, prints what
// the application prints, records each, and settles nothing twice.
func TestSettleTakesEveryPaymentOnce(t *testing.T) {
	r := newRig(t)
	p1, _ := r.pay(t, r.payee, 5, "history", nil)
	p2, _ := r.pay(t, r.payee, 20, "audit", nil)
	p3, _ := r.pay(t, r.payee, 7, "history", nil)
	a, b := ledger(t, p1, p2), ledger(t, p2, p3)
	h := &home{path: filepath.Join(r.home, "state.json")}
	got := settle(t, r, h, r.purse(5*time.Second), a, b)
	if got.err != nil || got.rep.Problem() != "" {
		t.Fatalf("settle: %v %q\n%s", got.err, got.rep.Problem(), got.warns)
	}
	for _, p := range []Payment{p1, p2, p3} {
		if !strings.Contains(got.out, settledLine(p)) || !h.IsSettled(p.Txid) || !r.chain.Mined(p.Txid) {
			t.Errorf("%s not settled:\n%s", p.Txid, got.out)
		}
	}
	pool := r.payee.Pool
	if !strings.HasSuffix(got.out, reportLine(3, 32, 0, 0, 0, 0, pool.Count(), pool.Balance())) || pool.Balance() != 32 || pool.Count() != 3 {
		t.Fatalf("report:\n%s", got.out)
	}
	wantWarn := a + " line 3: not a payment; skipped\n" + b + " line 3: not a payment; skipped\n"
	if got.warns != wantWarn {
		t.Fatalf("warned %q, want %q", got.warns, wantWarn)
	}
	if h.saves != 3 {
		t.Fatalf("saved %d times, want once a payment", h.saves)
	}
	// Again: nothing to do, and the pool holds each payment once.
	again := settle(t, r, h, r.purse(5*time.Second), a, b)
	if again.err != nil || again.out != reportLine(0, 0, 3, 0, 0, 0, 3, 32) || pool.Balance() != 32 {
		t.Fatalf("again: %v\n%s", again.err, again.out)
	}
}

// silent is a settlement leg that takes everything and says nothing, as a
// leg that accepted a transaction whose input was already spent may.
type silent struct{}

func (silent) Submit(context.Context, *transaction.Transaction) error { return nil }
func (silent) Name() string                                           { return "silent" }

// A payment whose payer spent its coin elsewhere is refused once, recorded,
// and passed over by later runs; the others settle. The leg's refusal says
// so, and where the leg says nothing the node's view of the inputs does.
func TestSettleRecordsAPaymentSpentElsewhere(t *testing.T) {
	for _, leg := range []string{"rpc", "silent"} {
		t.Run(leg, func(t *testing.T) {
			r := newRig(t)
			good, _ := r.pay(t, r.payee, 5, "history", nil)
			gone, coin := r.pay(t, r.payee, 9, "audit", nil)
			src, vout, by := coin.Tx.TxID().String(), coin.Vout, strings.Repeat("ee", 32)
			r.chain.SpendElsewhere(src, vout, by)
			h := &home{path: filepath.Join(r.home, "state.json")}
			path := ledger(t, good, gone)
			p := r.purse(5 * time.Second)
			why := fmt.Sprintf("input 0 (%s.%d) is spent by %s", src, vout, by)
			if leg == "silent" {
				// The good payment reaches the chain on its own.
				if err := r.chain.Send(mustTx(t, good)); err != nil {
					t.Fatal(err)
				}
				p.Settler = silent{}
			} else {
				why = fmt.Sprintf("publish: sendrawtransaction: sendrawtransaction: rpc error -26: missing or spent input 0: %s.%d", src, vout)
			}
			got := settle(t, r, h, p, path)
			if got.err != nil || got.rep.Refused != 1 || got.rep.Settled != 1 || !h.IsUnsettleable(gone.Txid) || !h.IsSettled(good.Txid) {
				t.Fatalf("settle: %v %+v\n%s\n%s", got.err, got.rep, got.out, got.warns)
			}
			want := fmt.Sprintf("payment %s (9 sat, audit): REFUSED, NEVER SETTLES: %s; the payer took the coins back after the question was answered\n", gone.Txid, why)
			if !strings.Contains(got.warns, want) || h.Unsettleable[0].Why != why {
				t.Fatalf("warned:\n%s\nwant:\n%s", got.warns, want)
			}
			if got.rep.Problem() != "1 payment(s) refused by the network: their payers spent the coins elsewhere, and they will never settle" {
				t.Fatalf("problem: %q", got.rep.Problem())
			}
			again := settle(t, r, h, p, path)
			if again.err != nil || again.rep.Problem() != "" || !strings.HasSuffix(again.out, reportLine(0, 0, 1, 0, 0, 1, 1, 5)) || strings.Contains(again.warns, "REFUSED") {
				t.Fatalf("again: %v\n%s\n%s", again.err, again.out, again.warns)
			}
		})
	}
}

func mustTx(t *testing.T, p Payment) *transaction.Transaction {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(p.Beef)
	if err != nil {
		t.Fatal(err)
	}
	_, tx, _, err := parseBEEF(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// Two payments of one coin (two hosts each accepted one): one settles, the
// other is refused, whichever the network took first.
func TestSettleOneOfTwoPaymentsOfOneCoin(t *testing.T) {
	r := newRig(t)
	first, coin := r.pay(t, r.payee, 5, "history", nil)
	second, _ := r.pay(t, r.payee, 6, "history", &coin)
	if (&Claims{}).Claim(first.Txid, first.Inputs) != Accepted || ClaimsOf([]Payment{first}).Refusal(second.Txid, second.Inputs) != Conflict {
		t.Fatal("a host refuses the second")
	}
	h := &home{path: filepath.Join(r.home, "state.json")}
	got := settle(t, r, h, r.purse(5*time.Second), ledger(t, first), ledger(t, second))
	if got.err != nil || got.rep.Settled != 1 || got.rep.Refused != 1 || len(h.Settled) != 1 || len(h.Unsettleable) != 1 {
		t.Fatalf("%v %+v\n%s\n%s", got.err, got.rep, got.out, got.warns)
	}
}

// A payment that has not mined in time is not settled, said in the
// application's words, and the next run takes it.
func TestSettleLeavesAnUnminedPaymentForTheNextRun(t *testing.T) {
	r := newRig(t)
	p, _ := r.pay(t, r.payee, 5, "history", nil)
	r.chain.SetHold(true)
	h := &home{path: filepath.Join(r.home, "state.json")}
	path := ledger(t, p)
	got := settle(t, r, h, r.purse(50*time.Millisecond), path)
	if got.err != nil || got.rep.NotSettled != 1 || h.IsSettled(p.Txid) || h.IsUnsettleable(p.Txid) {
		t.Fatalf("%v %+v", got.err, got.rep)
	}
	if !strings.Contains(got.warns, "payment "+p.Txid+" (5 sat, history): NOT SETTLED: purse: the payment is broadcast and not yet mined: payment "+p.Txid) ||
		!strings.Contains(got.warns, "; run the command again to take it into the pool once it is\n") {
		t.Fatalf("warned:\n%s", got.warns)
	}
	if got.rep.Problem() != "1 payment(s) not settled: until one is, its payer can spend the coins elsewhere" {
		t.Fatalf("problem: %q", got.rep.Problem())
	}
	r.chain.SetHold(false)
	r.chain.Mine()
	again := settle(t, r, h, r.purse(5*time.Second), path)
	if again.err != nil || again.rep.Settled != 1 || !h.IsSettled(p.Txid) {
		t.Fatalf("again: %v %+v\n%s", again.err, again.rep, again.warns)
	}
}

// A payment that fails a check is not settled, and nothing is broadcast.
func TestSettleChecksEachPayment(t *testing.T) {
	r := newRig(t)
	other, err := bwallet.Create(t.TempDir(), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, _ := r.pay(t, other, 5, "history", nil)
	named, _ := r.pay(t, r.payee, 5, "history", nil)
	named.Txid = strings.Repeat("ab", 32)
	torn := Payment{Txid: strings.Repeat("cd", 32), Beef: "%%", Class: "history"}
	later, _ := r.pay(t, r.payee, 5, "history", nil)
	later.V = 3
	h := &home{path: filepath.Join(r.home, "state.json")}
	sent := r.chain.Sent
	got := settle(t, r, h, r.purse(time.Second), ledger(t, elsewhere, named, torn, later))
	if got.err != nil || got.rep.NotSettled != 4 || r.chain.Sent != sent {
		t.Fatalf("%v %+v (%d sent)\n%s", got.err, got.rep, r.chain.Sent-sent, got.warns)
	}
	for _, want := range []string{
		"payment " + elsewhere.Txid + " (5 sat, history): NOT SETTLED: purse: output 0 does not pay the key this identity derives for the remittance and the sender\n",
		"payment " + named.Txid + " (5 sat, history): NOT SETTLED: the ledger names " + named.Txid + " and its BEEF holds ",
		"payment " + torn.Txid + " (0 sat, history): NOT SETTLED: the ledger's BEEF is not base64: ",
		"payment " + later.Txid + " (5 sat, history): NOT SETTLED: payee: the line is of a later ledger version than this reader: version 3, this reader reads up to 2\n",
	} {
		if !strings.Contains(got.warns, want) {
			t.Errorf("warned:\n%s\nwant:\n%s", got.warns, want)
		}
	}
}

// A record that does not persist ends the run, and the payments still out
// are let go.
func TestSettleStopsWhenTheRecordFails(t *testing.T) {
	r := newRig(t)
	var ps []Payment
	for range 3 {
		p, _ := r.pay(t, r.payee, 5, "history", nil)
		ps = append(ps, p)
	}
	h := &home{path: filepath.Join(r.home, "state.json"), fail: true}
	s := &Settler{App: "sample", Payer: r.purse(5 * time.Second), Record: Saved(&h.Book, h.save), InFlight: 1}
	if _, err := s.Settle(context.Background(), ps); err == nil || err.Error() != "disk full" {
		t.Fatalf("settle: %v", err)
	}
}

// fake is a Payer that takes every payment and records what it was asked.
type fake struct {
	mu       sync.Mutex
	args     []wallet.InternalizeActionArgs
	inFlight int
	most     int
}

func (f *fake) Check(_ context.Context, a wallet.InternalizeActionArgs) (*purse.Incoming, error) {
	f.args = append(f.args, a)
	_, tx, _, err := parseBEEF(a.Tx)
	if err != nil {
		return nil, err
	}
	return &purse.Incoming{Tx: tx, Txid: tx.TxID().String()}, nil
}

func (f *fake) Broadcast(context.Context, *purse.Incoming) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight++
	f.most = max(f.most, f.inFlight)
	return nil
}

func (f *fake) Await(context.Context, *purse.Incoming) error {
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	return nil
}

func (f *fake) Take(*purse.Incoming) error { return nil }

// Settle asks the purse for exactly what the application asked of it, for
// every payment of the fixture ledgers, and holds InFlight.
func TestSettleAsksThePurseAsTheApplicationDid(t *testing.T) {
	ps, err := ReadLedgers(nil, filepath.Join(fixtures, "ledger-v2.jsonl"), filepath.Join(fixtures, "ledger-mixed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("%d payments in two ledgers that share them", len(ps))
	}
	f := &fake{}
	var b Book
	var out bytes.Buffer
	s := &Settler{App: "sample", Payer: f, Record: Saved(&b, func() error { return nil }), Out: &out, InFlight: 1}
	rep, err := s.Settle(context.Background(), ps)
	if err != nil || rep.Settled != 3 || rep.Sats != 32 || f.most != 1 {
		t.Fatalf("%v %+v most %d", err, rep, f.most)
	}
	for i, p := range ps {
		a := f.args[i]
		rm, err := purse.Remittance(p.DerivationPrefix, p.DerivationSuffix, p.SenderIdentityKey)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(p.Beef)
		o := a.Outputs[0]
		if !bytes.Equal(a.Tx, raw) || a.Description != "sample priced question "+p.Class || strings.Join(a.Labels, ",") != "sample,payee" || len(a.Outputs) != 1 ||
			o.OutputIndex != 0 || o.Protocol != wallet.InternalizeProtocolWalletPayment ||
			!bytes.Equal(o.PaymentRemittance.DerivationPrefix, rm.DerivationPrefix) || !bytes.Equal(o.PaymentRemittance.DerivationSuffix, rm.DerivationSuffix) ||
			!o.PaymentRemittance.SenderIdentityKey.IsEqual(rm.SenderIdentityKey) {
			t.Errorf("payment %d: asked %+v", i, a)
		}
	}
	if !strings.HasSuffix(out.String(), reportLine(3, 32, 0, 0, 0, 0, 0, 0)) {
		t.Fatalf("report:\n%s", out.String())
	}
	// The fixture state: two settled before, one refused before.
	var st struct{ Book }
	readJSON(t, "state.json", &st)
	f = &fake{}
	s.Payer, s.Record = f, Saved(&st.Book, func() error { return nil })
	if rep, err := s.Settle(context.Background(), ps); err != nil || rep.Before != 2 || rep.Settled != 1 || len(f.args) != 1 {
		t.Fatalf("over the application's state: %v %+v", err, rep)
	}
}

// A Settler that is not set up runs nothing.
func TestSettlerSetUp(t *testing.T) {
	var b Book
	rec := Saved(&b, func() error { return nil })
	for name, s := range map[string]*Settler{
		"no app":       {Payer: &fake{}, Record: rec},
		"no payer":     {App: "a", Record: rec},
		"no record":    {App: "a", Payer: &fake{}},
		"in-flight 65": {App: "a", Payer: &fake{}, Record: rec, InFlight: MaxInFlight + 1},
		"in-flight -1": {App: "a", Payer: &fake{}, Record: rec, InFlight: -1},
	} {
		if _, err := s.Settle(context.Background(), nil); err == nil {
			t.Errorf("%s: ran", name)
		}
	}
}

// The words an application puts on the purse's refusals, and a field held
// to its line.
func TestWordsAndField(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{purse.ErrNoNode, purse.ErrNoNode.Error() + " (config key asset)"},
		{purse.ErrNoSettler, purse.ErrNoSettler.Error() + " (config key settle)"},
		{purse.ErrNotMined, purse.ErrNotMined.Error() + "; run the command again to take it into the pool once it is"},
		{errors.New("other"), "other"},
	} {
		if got := Words(c.err); got.Error() != c.want || !errors.Is(got, c.err) {
			t.Errorf("%v: %q", c.err, got)
		}
	}
	if got := Field("a\nb\r\x1b[31mc d\te"); got != "a b c d e" && got != "a b [31mc d e" {
		t.Errorf("Field: %q", got)
	}
	if strings.ContainsAny(Field("x\ny z\u0085"), "\n\r  \u0085") {
		t.Error("Field let a line break through")
	}
}

func jsonIndent(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	return append(raw, '\n'), err
}

func parseBEEF(b []byte) (*transaction.Beef, *transaction.Transaction, *chainhash.Hash, error) {
	return guard.ParseBEEF(b, guard.DefaultBound)
}
