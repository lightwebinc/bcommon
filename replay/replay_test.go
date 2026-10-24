package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/module"
)

// A synthetic chain application: a token is output 0 of a transaction,
// OP_FALSE OP_RETURN "tok" <chain> <seq>, spending the token before it (or,
// at seq 0, output 0 of a chain tree). Each transaction mines in a block of
// its own beside fixed siblings, or two in one block.

const service = "ls_test"

var sib = func(b byte) *chainhash.Hash { h := chainhash.Hash(bytes.Repeat([]byte{b}, 32)); return &h }

type mined struct {
	height uint32
}

func (m *mined) next() uint32 { m.height++; return m.height }

// alone proves tx at offset 1 of a block of two.
func (m *mined) alone(tx *transaction.Transaction) {
	yes := true
	tx.MerklePath = transaction.NewMerklePath(m.next(), [][]*transaction.PathElement{{
		{Offset: 0, Hash: sib(0x55)},
		{Offset: 1, Hash: tx.TxID(), Txid: &yes},
	}})
}

// together proves a and b in one block of four (a, s, b, s'), each with
// its own minimal path.
func (m *mined) together(a, b *transaction.Transaction) {
	h := m.next()
	yes := true
	n0 := transaction.MerkleTreeParent(a.TxID(), sib(0x61))
	n1 := transaction.MerkleTreeParent(b.TxID(), sib(0x62))
	a.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{
		{{Offset: 0, Hash: a.TxID(), Txid: &yes}, {Offset: 1, Hash: sib(0x61)}},
		{{Offset: 1, Hash: n1}},
	})
	b.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{
		{{Offset: 2, Hash: b.TxID(), Txid: &yes}, {Offset: 3, Hash: sib(0x62)}},
		{{Offset: 0, Hash: n0}},
	})
}

func tokenScript(chain string, seq uint64) *script.Script {
	b := []byte{script.OpFALSE, script.OpRETURN, 3, 't', 'o', 'k', byte(len(chain))}
	b = append(b, chain...)
	b = append(b, 8)
	b = binary.BigEndian.AppendUint64(b, seq)
	s := script.Script(b)
	return &s
}

func readToken(chain string) func(tx *transaction.Transaction) (*Token, error) {
	return func(tx *transaction.Transaction) (*Token, error) {
		if len(tx.Outputs) == 0 || len(tx.Inputs) == 0 || tx.Outputs[0].LockingScript == nil {
			return nil, errors.New("no token")
		}
		want := []byte(*tokenScript(chain, 0))
		b := []byte(*tx.Outputs[0].LockingScript)
		if len(b) != len(want) || !bytes.Equal(b[:len(b)-8], want[:len(want)-8]) {
			return nil, errors.New("no token of this chain")
		}
		t := &Token{Seq: binary.BigEndian.Uint64(b[len(b)-8:])}
		in := tx.Inputs[0]
		if t.Seq == 0 {
			t.Tree = transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}
		} else {
			t.Prev = *in.SourceTXID
		}
		return t, nil
	}
}

var nonce byte

func newTx(src *transaction.Transaction, out *script.Script) *transaction.Transaction {
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src.TxID(), SourceTxOutIndex: 0, UnlockingScript: &script.Script{}, SequenceNumber: 0xffffffff})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: out})
	nonce++
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: &script.Script{script.OpFALSE, script.OpRETURN, script.OpDATA1, nonce}})
	return tx
}

// chainSim is one chain as its publisher made it: the tree, the tokens by
// seq (more than one at a seq for a fork), each token's parent and the bytes
// a host stores for it.
type chainSim struct {
	name   string
	m      *mined
	tree   *transaction.Transaction
	bySeq  map[uint64][]*transaction.Transaction
	parent map[chainhash.Hash]*transaction.Transaction
	stored map[chainhash.Hash][]byte
}

func newChain(t *testing.T, name string, m *mined) *chainSim {
	t.Helper()
	root := transaction.NewTransaction()
	root.AddInput(&transaction.TransactionInput{SourceTXID: sib(0x09), UnlockingScript: &script.Script{}, SequenceNumber: 0xffffffff})
	root.AddOutput(&transaction.TransactionOutput{Satoshis: 10, LockingScript: &script.Script{script.OpTRUE}})
	tree := newTx(root, &script.Script{script.OpTRUE})
	m.alone(tree)
	return &chainSim{name: name, m: m, tree: tree, bySeq: map[uint64][]*transaction.Transaction{}, parent: map[chainhash.Hash]*transaction.Transaction{},
		stored: map[chainhash.Hash][]byte{}}
}

// add makes the token at seq on parent (the tree at seq 0); mine says
// whether to mine it alone now.
func (c *chainSim) add(t *testing.T, seq uint64, parent *transaction.Transaction, mine bool) *transaction.Transaction {
	t.Helper()
	tx := newTx(parent, tokenScript(c.name, seq))
	if mine {
		c.m.alone(tx)
	}
	c.bySeq[seq] = append(c.bySeq[seq], tx)
	c.parent[*tx.TxID()] = parent
	return tx
}

// publish stores every token as a host stores it: the subject of the BEEF it
// was published in, alone with the path that BEEF gave it.
func (c *chainSim) publish(t *testing.T) {
	t.Helper()
	for _, ts := range c.bySeq {
		for _, tx := range ts {
			b, err := chaintoken.TokenBEEF(tx, c.parent[*tx.TxID()])
			if err != nil {
				t.Fatal(err)
			}
			beef, err := transaction.NewBeefFromBytes(b)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := beef.AtomicBytes(tx.TxID())
			if err != nil {
				t.Fatal(err)
			}
			c.stored[*tx.TxID()] = raw
		}
	}
}

// line makes seqs 0..n-1, one token each, each mined alone.
func (c *chainSim) line(t *testing.T, n int) {
	t.Helper()
	p := c.tree
	for seq := range uint64(n) {
		p = c.add(t, seq, p, true)
	}
}

func (c *chainSim) top() uint64 {
	var top uint64
	for s := range c.bySeq {
		top = max(top, s)
	}
	return top
}

// peerSim answers the application's two questions from the chains it
// holds: {"chain"} the tokens at the highest seq, {"chain", "seq"} those
// at seq, each as stored.
type peerSim struct {
	chains  map[string]*chainSim
	asked   []string
	refuse  map[string]bool
	failing bool
}

func (p *peerSim) ask(_ context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	if p.failing {
		return nil, errors.New("connection refused")
	}
	var m map[string]string
	if err := json.Unmarshal(q.Query, &m); err != nil {
		return nil, err
	}
	p.asked = append(p.asked, string(q.Query))
	if p.refuse[m["chain"]] {
		return nil, fmt.Errorf("%w: ls_test: this host does not carry %s", module.ErrRefused, m["chain"])
	}
	c := p.chains[m["chain"]]
	ans := &lookup.LookupAnswer{Type: lookup.AnswerTypeOutputList, Outputs: []*lookup.OutputListItem{}}
	if c == nil {
		return ans, nil
	}
	seq := c.top()
	if s, ok := m["seq"]; ok {
		n, _ := strconv.ParseUint(s, 10, 64)
		seq = n
	}
	for _, tx := range c.bySeq[seq] {
		ans.Outputs = append(ans.Outputs, &lookup.OutputListItem{Beef: c.stored[*tx.TxID()], OutputIndex: 0})
	}
	return ans, nil
}

// hostSim is the replaying host: the tokens it holds as stored, what it
// applied, and what it refuses. admit holds every answered object to the
// rules a host and this package promise: a token in the BEEF of exactly
// the token and its parent with minimal or merged paths, its parent held or
// admitted earlier in the same answer, its After admitted.
type hostSim struct {
	t       *testing.T
	read    map[string]func(*transaction.Transaction) (*Token, error)
	held    map[chainhash.Hash]*transaction.Transaction
	chainOf map[chainhash.Hash]string
	seqOf   map[chainhash.Hash]uint64
	applied map[chainhash.Hash]bool
	refuse  map[chainhash.Hash]bool
	order   []string
}

func newHost(t *testing.T) *hostSim {
	return &hostSim{t: t, read: map[string]func(*transaction.Transaction) (*Token, error){}, held: map[chainhash.Hash]*transaction.Transaction{},
		chainOf: map[chainhash.Hash]string{}, seqOf: map[chainhash.Hash]uint64{}, applied: map[chainhash.Hash]bool{}, refuse: map[chainhash.Hash]bool{}}
}

// hold takes a token as published, as the plane would have delivered it.
func (h *hostSim) hold(c *chainSim, tx *transaction.Transaction) {
	h.t.Helper()
	tx2, err := transaction.NewTransactionFromBytes(tx.Bytes())
	if err != nil {
		h.t.Fatal(err)
	}
	beef, err := transaction.NewBeefFromBytes(c.stored[*tx.TxID()])
	if err != nil {
		h.t.Fatal(err)
	}
	tx2.MerklePath = beef.FindTransactionByHash(tx.TxID()).MerklePath
	h.keep(c.name, tx2)
}

func (h *hostSim) keep(chain string, tx *transaction.Transaction) {
	tok, err := h.read[chain](tx)
	if err != nil {
		h.t.Fatal(err)
	}
	h.held[*tx.TxID()] = tx
	h.chainOf[*tx.TxID()] = chain
	h.seqOf[*tx.TxID()] = tok.Seq
	h.applied[*tx.TxID()] = true
}

func (h *hostSim) admit(a *module.ReplayAnswer, chain string) (admitted []chainhash.Hash) {
	h.t.Helper()
	now := map[chainhash.Hash]bool{}
	for _, o := range a.Objects {
		for _, dep := range o.After {
			if !now[dep] {
				h.t.Fatalf("%s submitted before %s, its After", o.Txid, dep)
			}
		}
		w, err := chaintoken.ReadWire(o.BEEF, 0)
		if err != nil {
			h.t.Fatalf("%s: %v", o.Txid, err)
		}
		tx := w.SubjectTx()
		if *tx.TxID() != o.Txid || o.Kind != KindToken {
			h.t.Fatalf("object %s is %s, kind %s", o.Txid, tx.TxID(), o.Kind)
		}
		parent, err := w.Token(tx)
		if err != nil {
			h.t.Fatalf("token %s: %v", o.Txid, err)
		}
		tok, err := h.read[chain](tx)
		if err != nil {
			h.t.Fatal(err)
		}
		if tok.Seq > 0 && h.held[parent.Txid] == nil && !now[parent.Txid] {
			h.t.Fatalf("token %s (seq %d) submitted before its parent %s is held", o.Txid, tok.Seq, parent.Txid)
		}
		if h.applied[o.Txid] {
			h.t.Fatalf("%s answered though applied", o.Txid)
		}
		if h.refuse[o.Txid] {
			continue
		}
		stored, err := transaction.NewTransactionFromBytes(tx.Bytes())
		if err != nil {
			h.t.Fatal(err)
		}
		stored.MerklePath = tx.MerklePath
		h.keep(chain, stored)
		now[o.Txid] = true
		h.order = append(h.order, fmt.Sprintf("%d", tok.Seq))
		admitted = append(admitted, o.Txid)
	}
	return admitted
}

func (h *hostSim) chain(c *chainSim, peer *peerSim, latest []*Output) *Chain {
	return &Chain{
		Name:   c.name,
		Latest: latest,
		AtSeq: func(seq uint64) (string, map[string]string) {
			return service, map[string]string{"chain": c.name, "seq": strconv.FormatUint(seq, 10)}
		},
		Held: func(seq uint64) []chainhash.Hash {
			var out []chainhash.Hash
			for txid, s := range h.seqOf {
				if s == seq && h.chainOf[txid] == c.name {
					out = append(out, txid)
				}
			}
			return out
		},
		Local: func(_ context.Context, txid chainhash.Hash) (*transaction.Transaction, error) {
			return h.held[txid], nil
		},
		Read: h.read[c.name],
	}
}

type treeSource struct {
	txs   map[string]*transaction.Transaction
	asked int
}

func (s *treeSource) TxRaw(_ context.Context, txid string) ([]byte, error) {
	s.asked++
	if tx := s.txs[txid]; tx != nil {
		return tx.Bytes(), nil
	}
	return nil, errors.New("not known")
}

func (s *treeSource) Proof(_ context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	if tx := s.txs[txid]; tx != nil {
		return tx.MerklePath, tx.MerklePath.BlockHeight, nil
	}
	return nil, 0, errors.New("not mined")
}

// walk runs whole walks of one chain until one is Done, admitting each
// answer, and returns the answers.
func walkChain(t *testing.T, h *hostSim, c *chainSim, peer *peerSim, trees TreeSource, maxQ, maxO int) []*module.ReplayAnswer {
	t.Helper()
	var cursor any
	var answers []*module.ReplayAnswer
	for calls := 0; ; calls++ {
		if calls > 1000 {
			t.Fatal("the walk does not end")
		}
		req := &module.ReplayRequest{Topic: "tm_test", Peer: "peer", Ask: peer.ask, Cursor: cursor, MaxQuestions: maxQ, MaxObjects: maxO,
			Applied: func(_ context.Context, txid chainhash.Hash) (bool, error) { return h.applied[txid], nil }}
		a, err := Run(context.Background(), req, Phase{Name: "tokens",
			Items: func(context.Context, *Walk) ([]string, error) { return []string{c.name}, nil },
			Do: func(ctx context.Context, w *Walk, item string, page *string) (bool, error) {
				ch := h.chain(c, peer, nil)
				ch.LatestQ = func() (string, map[string]string) { return service, map[string]string{"chain": item} }
				ch.Trees = trees
				return w.Tokens(ctx, ch, page)
			}})
		if err != nil {
			t.Fatal(err)
		}
		if a.Questions > max(maxQ, 0) && maxQ > 0 || (maxO > 0 && len(a.Objects) > maxO) {
			t.Fatalf("a call asked %d questions and answered %d objects, bounds %d and %d", a.Questions, len(a.Objects), maxQ, maxO)
		}
		h.admit(a, c.name)
		answers = append(answers, a)
		if a.Done {
			return answers
		}
		cursor = a.Cursor
	}
}

func objects(as []*module.ReplayAnswer) (n int) {
	for _, a := range as {
		n += len(a.Objects)
	}
	return n
}

func gaps(as []*module.ReplayAnswer) (n int) {
	for _, a := range as {
		n += a.Gaps
	}
	return n
}

func setup(t *testing.T, n int) (*hostSim, *chainSim, *peerSim) {
	t.Helper()
	c := newChain(t, "alpha", &mined{height: 100})
	c.line(t, n)
	c.publish(t)
	h := newHost(t)
	h.read[c.name] = readToken(c.name)
	return h, c, &peerSim{chains: map[string]*chainSim{c.name: c}}
}

func (h *hostSim) holds(c *chainSim) bool {
	for _, ts := range c.bySeq {
		for _, tx := range ts {
			if h.held[*tx.TxID()] == nil {
				return false
			}
		}
	}
	return true
}

// TestTail: the host holds seqs 0 to 2 of 6; one walk answers 3, 4 and 5 in
// order, each after its parent, and a second walk answers nothing and asks
// only for the latest.
func TestTail(t *testing.T) {
	h, c, peer := setup(t, 6)
	for s := range uint64(3) {
		h.hold(c, c.bySeq[s][0])
	}
	as := walkChain(t, h, c, peer, nil, 0, 0)
	if !h.holds(c) || strings.Join(h.order, ",") != "3,4,5" || gaps(as) != 0 {
		t.Fatalf("order %v, holds %v, gaps %d", h.order, h.holds(c), gaps(as))
	}
	if as[0].Objects[1].After == nil || as[0].Objects[1].After[0] != *c.bySeq[3][0].TxID() || as[0].Objects[0].After != nil {
		t.Fatalf("After: %v %v", as[0].Objects[0].After, as[0].Objects[1].After)
	}
	peer.asked = nil
	again := walkChain(t, h, c, peer, nil, 0, 0)
	if objects(again) != 0 || len(peer.asked) != 1 {
		t.Fatalf("a level host: %d objects, asked %v", objects(again), peer.asked)
	}
}

// TestGap: a token missing in the middle is answered on its held parent,
// and nothing else is asked for but its seq.
func TestGap(t *testing.T) {
	h, c, peer := setup(t, 5)
	for _, s := range []uint64{0, 1, 3, 4} {
		h.hold(c, c.bySeq[s][0])
	}
	as := walkChain(t, h, c, peer, nil, 0, 0)
	if !h.holds(c) || strings.Join(h.order, ",") != "2" || len(peer.asked) != 2 {
		t.Fatalf("order %v, asked %v", h.order, peer.asked)
	}
	_ = as
}

// TestFirstToken: a chain the host holds nothing of needs its first token's
// parent, the chain tree, which only a chain source gives. Without one the
// chain is a gap and nothing of it is answered; with one, all of it is, in
// order.
func TestFirstToken(t *testing.T) {
	h, c, peer := setup(t, 3)
	as := walkChain(t, h, c, peer, nil, 0, 0)
	if objects(as) != 0 || gaps(as) != 1 || !strings.Contains(strings.Join(as[0].Notes, " "), "no chain source") {
		t.Fatalf("no chain source: %d objects, %d gaps, %v", objects(as), gaps(as), as[0].Notes)
	}
	trees := &treeSource{txs: map[string]*transaction.Transaction{c.tree.TxID().String(): c.tree}}
	as = walkChain(t, h, c, peer, trees, 0, 0)
	if !h.holds(c) || strings.Join(h.order, ",") != "0,1,2" || gaps(as) != 0 || trees.asked != 1 {
		t.Fatalf("order %v, gaps %d, tree asked %d", h.order, gaps(as), trees.asked)
	}
	wrong := &treeSource{txs: map[string]*transaction.Transaction{c.tree.TxID().String(): c.bySeq[0][0]}}
	h2, _, _ := setup(t, 0)
	h2.read[c.name] = readToken(c.name)
	as = walkChain(t, h2, c, peer, wrong, 0, 0)
	if objects(as) != 0 || gaps(as) != 1 {
		t.Fatalf("a chain source answering another transaction: %d objects, %d gaps", objects(as), gaps(as))
	}
}

// TestBounds: with one question and two objects a call, every call keeps to
// its bounds and the walks together answer the same as one unbounded walk.
func TestBounds(t *testing.T) {
	h, c, peer := setup(t, 12)
	h.hold(c, c.bySeq[0][0])
	as := walkChain(t, h, c, peer, nil, 1, 2)
	if !h.holds(c) || len(as) < 6 || strings.Join(h.order, ",") != "1,2,3,4,5,6,7,8,9,10,11" {
		t.Fatalf("%d calls, order %v", len(as), h.order)
	}
	h, c, peer = setup(t, 12)
	h.hold(c, c.bySeq[0][0])
	as = walkChain(t, h, c, peer, nil, 2, 1)
	if !h.holds(c) || strings.Join(h.order, ",") != "1,2,3,4,5,6,7,8,9,10,11" {
		t.Fatalf("%d calls, order %v", len(as), h.order)
	}
}

// TestRefusedStopsTheChain: a token the host does not admit stops the
// chain for the walk, so nothing built on it is answered; the next walk
// offers it again.
func TestRefusedStopsTheChain(t *testing.T) {
	h, c, peer := setup(t, 6)
	h.hold(c, c.bySeq[0][0])
	h.hold(c, c.bySeq[1][0])
	h.refuse[*c.bySeq[3][0].TxID()] = true
	as := walkChain(t, h, c, peer, nil, 1, 1)
	if strings.Join(h.order, ",") != "2" || gaps(as) == 0 {
		t.Fatalf("order %v, gaps %d", h.order, gaps(as))
	}
	delete(h.refuse, *c.bySeq[3][0].TxID())
	walkChain(t, h, c, peer, nil, 0, 0)
	if !h.holds(c) || strings.Join(h.order, ",") != "2,3,4,5" {
		t.Fatalf("the next walk: order %v", h.order)
	}
}

// TestForkAndMergedPaths: two tokens at seq 2, the peer's latest on the one
// the host does not hold, and a token mined in its parent's block (stored
// with the merged path it was published with). The other token at seq 2 is
// answered before its child, every path minimal or merged as the shape
// requires.
func TestForkAndMergedPaths(t *testing.T) {
	m := &mined{height: 200}
	c := newChain(t, "beta", m)
	t0 := c.add(t, 0, c.tree, true)
	t1 := c.add(t, 1, t0, true)
	a2 := c.add(t, 2, t1, true)
	b2 := c.add(t, 2, t1, false)
	b3 := c.add(t, 3, b2, false)
	m.together(b2, b3)
	b4 := c.add(t, 4, b3, true)
	c.publish(t)
	if beef, _ := transaction.NewBeefFromBytes(c.stored[*b3.TxID()]); chaintoken.MinimalPath(beef.FindTransactionByHash(b3.TxID()).MerklePath, *b3.TxID()) {
		t.Fatal("the stored path of a token mined in its parent's block is merged, not minimal")
	}
	h := newHost(t)
	h.read[c.name] = readToken(c.name)
	for _, tx := range []*transaction.Transaction{t0, t1, a2} {
		h.hold(c, tx)
	}
	peer := &peerSim{chains: map[string]*chainSim{c.name: c}}
	as := walkChain(t, h, c, peer, nil, 0, 0)
	if !h.holds(c) || strings.Join(h.order, ",") != "2,3,4" || gaps(as) != 0 {
		t.Fatalf("order %v, gaps %d", h.order, gaps(as))
	}
	_ = b4
	// From nothing held but the first token, with every path cut where it
	// must be: b3's BEEF (with b2, one block) and b4's (with b3, two).
	h = newHost(t)
	h.read[c.name] = readToken(c.name)
	h.hold(c, t0)
	walkChain(t, h, c, peer, nil, 0, 0)
	if !h.holds(c) {
		t.Fatal("not every token is held")
	}
}

// TestPeerErrors: a peer that refuses a question has nothing for the item
// (counted, noted); one that does not answer ends the call with an error.
func TestPeerErrors(t *testing.T) {
	h, c, peer := setup(t, 3)
	h.hold(c, c.bySeq[0][0])
	peer.refuse = map[string]bool{c.name: true}
	as := walkChain(t, h, c, peer, nil, 0, 0)
	if objects(as) != 0 || as[0].Refused != 1 || !as[0].Done {
		t.Fatalf("refused: %+v", as[0])
	}
	peer.refuse, peer.failing = nil, true
	_, err := Run(context.Background(), &module.ReplayRequest{Ask: peer.ask}, Phase{Name: "tokens",
		Items: func(context.Context, *Walk) ([]string, error) { return []string{"x"}, nil },
		Do: func(ctx context.Context, w *Walk, item string, _ *string) (bool, error) {
			_, err := w.Ask(ctx, service, map[string]string{"chain": item})
			return err == nil, err
		}})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("a peer that does not answer: %v", err)
	}
}

// TestRunPhases: a phase that answered ends the call so the host admits
// before the next phase reads; items resume by key and page; Each answers
// what the walk wants once, keep filtering; a walk always ends.
func TestRunPhases(t *testing.T) {
	applied := map[chainhash.Hash]bool{}
	pages := map[string][][]string{"a": {{"a1", "a2"}, {"a3"}}, "b": {{"b1", "a1"}}}
	tx := func(label string) *transaction.Transaction {
		x := transaction.NewTransaction()
		h := chainhash.Hash(sha256.Sum256([]byte(label)))
		x.AddInput(&transaction.TransactionInput{SourceTXID: &h, UnlockingScript: &script.Script{}, SequenceNumber: 0xffffffff})
		x.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
		mm := &mined{height: 300}
		mm.alone(x)
		return x
	}
	byLabel := map[string]*transaction.Transaction{}
	for _, l := range []string{"a1", "a2", "a3", "b1"} {
		byLabel[l] = tx(l)
	}
	ask := func(_ context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
		var m map[string]string
		_ = json.Unmarshal(q.Query, &m)
		n, _ := strconv.Atoi(m["page"])
		ans := &lookup.LookupAnswer{Type: lookup.AnswerTypeOutputList}
		if n < len(pages[m["item"]]) {
			for _, l := range pages[m["item"]][n] {
				b, _ := byLabel[l].AtomicBEEF(false)
				ans.Outputs = append(ans.Outputs, &lookup.OutputListItem{Beef: b, OutputIndex: 0})
			}
		}
		return ans, nil
	}
	var order []string
	var cursor any
	calls := 0
	firstRan := 0
	for {
		calls++
		req := &module.ReplayRequest{Ask: ask, Cursor: cursor, MaxQuestions: 2, MaxObjects: 5,
			Applied: func(_ context.Context, txid chainhash.Hash) (bool, error) { return applied[txid], nil }}
		a, err := Run(context.Background(), req,
			Phase{Name: "first", Items: func(context.Context, *Walk) ([]string, error) { firstRan++; return nil, nil },
				Do: func(context.Context, *Walk, string, *string) (bool, error) { return true, nil }},
			Phase{Name: "records", Items: func(context.Context, *Walk) ([]string, error) { return []string{"b", "a"}, nil },
				Do: func(ctx context.Context, w *Walk, item string, page *string) (bool, error) {
					for {
						n, _ := strconv.Atoi(*page)
						outs, err := w.Each(ctx, "record", service, map[string]string{"item": item, "page": strconv.Itoa(n)}, func(o *Output) bool {
							return o.Txid != *byLabel["a3"].TxID() || item == "a"
						})
						if err != nil {
							return false, err
						}
						if len(outs) == 0 {
							return true, nil
						}
						*page = strconv.Itoa(n + 1)
					}
				}})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range a.Objects {
			for l, x := range byLabel {
				if *x.TxID() == o.Txid {
					order = append(order, l)
				}
			}
			applied[o.Txid] = true
		}
		if a.Done {
			break
		}
		if calls > 20 {
			t.Fatal("the walk does not end")
		}
		cursor = a.Cursor
	}
	if strings.Join(order, ",") != "a1,a2,a3,b1" {
		t.Fatalf("order %v over %d calls", order, calls)
	}
	if firstRan != 1 {
		t.Fatalf("the first phase's items were listed %d times", firstRan)
	}
}

// TestMinimal: a merged path is cut to each transaction's minimal path,
// which proves the same root; a minimal one is kept as it is.
func TestMinimal(t *testing.T) {
	m := &mined{height: 400}
	c := newChain(t, "gamma", m)
	a := newTx(c.tree, &script.Script{script.OpTRUE})
	b := newTx(a, &script.Script{script.OpTRUE})
	m.together(a, b)
	merged := transaction.NewMerklePath(a.MerklePath.BlockHeight, a.MerklePath.Path)
	if err := merged.Combine(b.MerklePath); err != nil {
		t.Fatal(err)
	}
	for _, x := range []*transaction.Transaction{a, b} {
		cut, err := chaintoken.Minimal(merged, *x.TxID())
		if err != nil || !chaintoken.MinimalPath(cut, *x.TxID()) {
			t.Fatalf("cut %s: %v", x.TxID(), err)
		}
		r1, _ := cut.ComputeRoot(x.TxID())
		r2, _ := merged.ComputeRoot(x.TxID())
		if *r1 != *r2 {
			t.Fatal("the cut path proves another root")
		}
	}
	if _, err := chaintoken.Minimal(merged, chainhash.Hash{1}); err == nil {
		t.Fatal("a path cut for a transaction it does not hold")
	}
	keep := a.ShallowClone()
	keep.MerklePath = a.MerklePath
	if got, err := minimal(keep); err != nil || got != keep {
		t.Fatal("a minimal path is cut")
	}
	var txids []string
	for _, x := range []*transaction.Transaction{a, b} {
		txids = append(txids, x.TxID().String())
	}
	sort.Strings(txids)
}
