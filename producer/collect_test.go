package producer_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
)

// published is a producer that minted a tree before it mined, with the
// proofs collected later: the tree is kept as its BEEF, its change is held
// in the pool, and the journal holds entries in several states.
type published struct {
	tr      *producer.Trees
	chain   *testChain
	st      *memTrees
	tree    *transaction.Transaction
	journal *publish.Journal
}

func publishUnproven(t *testing.T) *published {
	t.Helper()
	tr, l, st, _ := treesFor(t)
	tr.Payer.Async = true
	tree, _, err := tr.Spend(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	tr.Payer.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		if st.cur == nil || st.cur.Txid != txid {
			return nil, errors.New("not kept")
		}
		return funding.Rebuild(st.cur.RawHex, st.cur.BumpHex, st.cur.BeefHex)
	}
	return &published{tr: tr, chain: l, st: st, tree: tree, journal: &publish.Journal{Dir: filepath.Join(t.TempDir(), "journal")}}
}

func (p *published) pending(proven *int) producer.Pending {
	return producer.Pending{
		What: "funding tree", Txid: p.st.cur.Txid, RawHex: p.st.cur.RawHex, BeefHex: p.st.cur.BeefHex,
		Proven: func(mp *transaction.MerklePath, height uint32) {
			*proven++
			p.st.cur.BumpHex, p.st.cur.Height, p.st.cur.BeefHex = mp.Hex(), height, ""
		},
	}
}

func (p *published) collector(n *notes, saves *int) *producer.Collector {
	return &producer.Collector{
		Proofs: producer.Proofs{Arcade: p.chain.arcade(), Asset: p.chain.asset()}, Kept: p.tr.Payer.Kept,
		Pool: p.tr.Payer.Pool, Journal: p.journal, Facade: p.chain.facade(), Topic: testTopic,
		Save: func() error { *saves++; return nil }, Note: n.note,
	}
}

func (p *published) entry(t *testing.T, seq uint64, txid string, sent bool, efErr string) {
	t.Helper()
	e := publish.Entry{Acct: "alice@example.com", Seq: seq, Kind: "update", TxID: txid, EFError: efErr}
	if sent {
		now := time.Now().UTC()
		e.BEEFSentAt = &now
	}
	if err := p.journal.Write(e); err != nil {
		t.Fatal(err)
	}
}

// A mined item's proof is recorded, saved, reported, stamped in the
// journal, given to the copy already handed out, and published again; a
// journal entry left behind is stamped, one the caller tracks is not; held
// change is released.
func TestCollectRecordsAndRepublishesAProof(t *testing.T) {
	ctx := context.Background()
	p := publishUnproven(t)
	treeID := p.tree.TxID().String()
	handedOut, err := p.tr.Payer.Kept.Tx(treeID)
	if err != nil {
		t.Fatal(err)
	}
	superseded, tracked, failed, unsent := strings.Repeat("51", 32), strings.Repeat("52", 32), strings.Repeat("53", 32), strings.Repeat("54", 32)
	p.entry(t, 7, treeID, true, "")
	p.entry(t, 1, superseded, true, "")
	p.entry(t, 2, tracked, true, "")
	p.entry(t, 3, failed, true, "settle refused")
	p.entry(t, 4, unsent, false, "")
	p.chain.mine(treeID, superseded, tracked, failed, unsent)
	before := len(p.chain.submissions())

	n, saves, proven := &notes{}, 0, 0
	it := p.pending(&proven)
	it.Stamp, it.Seq = true, 7
	p.collector(n, &saves).Collect(ctx, []producer.Pending{it}, tracked)

	if proven != 1 || saves != 1 || p.st.cur.Height != 701 || p.st.cur.BeefHex != "" {
		t.Fatalf("proven %d saves %d state %+v", proven, saves, p.st.cur)
	}
	if handedOut.MerklePath == nil || handedOut.MerklePath.BlockHeight != 701 {
		t.Fatal("the copy handed out did not get the proof")
	}
	want := []string{
		"funding tree " + treeID + ": mined at height 701",
		"funding tree " + treeID + ": proof published to the hosts",
		"change from " + treeID + ": mined at height 701, 1 coin(s) spendable again",
	}
	if strings.Join(n.all(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", n, strings.Join(want, "\n"))
	}
	subs := p.chain.submissions()
	if len(subs) != before+1 {
		t.Fatalf("%d republished", len(subs)-before)
	}
	if again := subjectOf(t, subs[len(subs)-1].beef); again.TxID().String() != treeID || again.MerklePath == nil {
		t.Fatal("what was republished is not the proven tree")
	}
	stamped := map[string]bool{}
	entries, err := p.journal.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		stamped[e.TxID] = e.MinedAt != nil
	}
	if !stamped[treeID] || !stamped[superseded] || stamped[tracked] || stamped[failed] || stamped[unsent] {
		t.Fatalf("journal stamps: %v", stamped)
	}
	held := p.tr.Payer.Pool.Outputs()
	if len(held) != 1 || held[0].Unproven || held[0].Height != 701 || held[0].Bump == "" {
		t.Fatalf("held change was not released: %+v", held)
	}
}

// The outcomes that are not a proof: still pending, refused, a kept copy
// that does not rebuild, and a check that could not be made, each reported
// in the library's words unless the item words it itself; none records
// anything.
func TestCollectReportsEveryOtherOutcome(t *testing.T) {
	ctx := context.Background()
	p := publishUnproven(t)
	treeID := p.tree.TxID().String()
	n, saves, proven := &notes{}, 0, 0
	c := p.collector(n, &saves)
	c.Pool, c.Journal = nil, nil

	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	if got := n.String(); got != "funding tree "+treeID+": accepted, proof pending" {
		t.Fatalf("pending: %q", got)
	}

	p.chain.mu.Lock()
	p.chain.refused[treeID] = "double spend"
	p.chain.mu.Unlock()
	n.lines = nil
	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	if got := n.String(); got != "WARNING: funding tree "+treeID+" was refused by the network: REJECTED: double spend" {
		t.Fatalf("refused: %q", got)
	}
	var said error
	it := p.pending(&proven)
	it.Refused = func(err error) { said = err }
	n.lines = nil
	c.Collect(ctx, []producer.Pending{it})
	if !errors.Is(said, producer.ErrRefused) || len(n.all()) != 0 {
		t.Fatalf("a Refused hook: %v, notes %q", said, n)
	}

	p.chain.mu.Lock()
	delete(p.chain.refused, treeID)
	p.chain.mu.Unlock()
	p.chain.mine(treeID)
	broken := p.pending(&proven)
	broken.BeefHex = "zz"
	n.lines = nil
	c.Collect(ctx, []producer.Pending{broken})
	if got := n.String(); !strings.HasPrefix(got, "note: funding tree "+treeID+" mined, but the kept copy does not rebuild: ") {
		t.Fatalf("unbuilt: %q", got)
	}
	var unbuilt error
	broken.Unbuilt = func(err error) { unbuilt = err }
	n.lines = nil
	c.Collect(ctx, []producer.Pending{broken})
	if unbuilt == nil || len(n.all()) != 0 {
		t.Fatalf("an Unbuilt hook: %v, notes %q", unbuilt, n)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"reason":"overloaded"}`, http.StatusInternalServerError)
	}))
	defer failing.Close()
	c.Proofs.Arcade = &publish.Arcade{Base: failing.URL}
	n.lines = nil
	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	if got := n.String(); !strings.HasPrefix(got, "note: could not check funding tree "+treeID+": ") || !strings.Contains(got, "overloaded") {
		t.Fatalf("unchecked: %q", got)
	}
	if proven != 0 || saves != 0 {
		t.Fatalf("an outcome that is not a proof recorded one: proven %d saves %d", proven, saves)
	}
}

// A proof that cannot be published is a note that ends with how the
// application sends it again; the proof is still recorded. With no facade
// nothing is republished, and a state that cannot be saved is a note.
func TestCollectRepublishFailures(t *testing.T) {
	ctx := context.Background()
	p := publishUnproven(t)
	treeID := p.tree.TxID().String()
	p.chain.mine(treeID)
	p.chain.facadeDown = true
	n, saves, proven := &notes{}, 0, 0
	c := p.collector(n, &saves)
	c.Pool, c.Journal = nil, nil
	c.Retry = "`example publish -resume` sends it again"
	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	lines := n.all()
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "note: funding tree "+treeID+" proven, but publishing the proof failed: ") ||
		!strings.HasSuffix(lines[1], "; `example publish -resume` sends it again") {
		t.Fatalf("notes:\n%s", n)
	}
	if proven != 1 || saves != 1 {
		t.Fatal("a publish failure lost the proof")
	}

	c.Retry = ""
	n.lines = nil
	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	if lines := n.all(); len(lines) != 2 || strings.Contains(lines[1], ";") {
		t.Fatalf("no retry hint: %q", lines)
	}

	c.Facade = nil
	c.Save = func() error { return errors.New("disk full") }
	n.lines = nil
	c.Collect(ctx, []producer.Pending{p.pending(&proven)})
	want := "note: could not save collected proofs: disk full\nfunding tree " + treeID + ": mined at height 701"
	if n.String() != want {
		t.Fatalf("notes:\n%s\nwant:\n%s", n, want)
	}
}
