package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/module"
)

// KindToken is the kind Tokens answers its tokens as.
const KindToken = "token"

// MaxParentDepth bounds how far Tokens goes down a branch to answer a
// token's parent before the token itself, when the parent sits at a seq at
// which the host holds another token.
const MaxParentDepth = 16

// Token is what Tokens needs of a chain token, read from its transaction
// alone, as a host stores it.
type Token struct {
	Seq uint64
	// Prev is the txid of the token it spends, the one at Seq - 1; zero at
	// seq 0.
	Prev chainhash.Hash
	// Tree is, at seq 0, the outpoint of the chain tree it spends: its
	// parent, which no host keeps.
	Tree transaction.Outpoint
}

// TreeSource gives a transaction's bytes and its proof by txid: a chain
// source (nodeapi.Asset, nodeapi.WoC and nodeapi.Sources are one). A chain
// tree comes from one, since no host keeps it and a node drops a
// transaction once every output is spent, which a chain tree is once each
// of its outputs paid for a chain's first token.
type TreeSource interface {
	TxRaw(ctx context.Context, txid string) ([]byte, error)
	Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error)
}

// Chain is one chain of tokens for Tokens to repair.
type Chain struct {
	// Name says which chain, in notes.
	Name string
	// Latest are the peer's latest tokens of the chain, when the caller has
	// them already (a listing page's); otherwise LatestQ is asked.
	Latest []*Output
	// LatestQ is the application's question for the chain's latest tokens.
	LatestQ func() (service string, query map[string]string)
	// AtSeq is the application's question for the chain's tokens at seq.
	AtSeq func(seq uint64) (service string, query map[string]string)
	// Held are the txids of the tokens of the chain the host holds at seq.
	Held func(seq uint64) []chainhash.Hash
	// Local reads a token the host holds, as stored (alone, with its
	// proof); nil, nil when it holds none of that txid.
	Local func(ctx context.Context, txid chainhash.Hash) (*transaction.Transaction, error)
	// Read reads a token of this chain from its transaction, an error when
	// the transaction holds none.
	Read func(tx *transaction.Transaction) (*Token, error)
	// Trees gives a first token's parent; nil when the host has no chain
	// source, and a missing first token is then a gap.
	Trees TreeSource
	// Check holds an assembled token BEEF to the application's admission
	// rules before it is answered, so that a peer's bad answer is not
	// submitted; nil leaves it to the host's admission.
	Check func(ctx context.Context, beef []byte) error
}

type peerToken struct {
	out *Output
	tok *Token
}

// Tokens repairs one chain: every seq from 0 to the peer's latest at which
// the host holds no token, ascending, and the peer's latest tokens. Each
// token there that the walk wants (Wanted) is answered in the BEEF of the
// token and its parent (TokenBEEF), after its parent: the parent is read
// from the host, or from the peer's answer at seq - 1 and answered first,
// or for a first token from the chain source. The chain stops at the first
// token it cannot answer (counted in Gaps), and at a token whose parent the
// walk answered and the host did not admit, so no token is ever answered
// without its parent held or answered before it.
//
// done is false when the call's bound stopped it. *page then keeps the
// peer's latest seq, so the next call goes on without asking for the
// latest again (it asks for the tokens at that seq instead) and finds what
// the host admitted meanwhile; the caller passes the phase's page.
func (w *Walk) Tokens(ctx context.Context, c *Chain, page *string) (done bool, err error) {
	byTxid := map[chainhash.Hash]*peerToken{}
	bySeq := map[uint64][]*peerToken{}
	var top uint64
	found := false
	if n, ok := strings.CutPrefix(*page, "top:"); ok {
		if top, err = strconv.ParseUint(n, 10, 64); err != nil {
			return false, fmt.Errorf("replay: %s: page %q", c.Name, *page)
		}
		found = true
	} else {
		latest := c.Latest
		if latest == nil && c.LatestQ != nil {
			service, query := c.LatestQ()
			if latest, err = w.Ask(ctx, service, query); err != nil {
				return false, budget(err)
			}
		}
		for _, o := range latest {
			t, err := c.Read(o.Tx)
			if err != nil {
				continue
			}
			pt := &peerToken{out: o, tok: t}
			byTxid[o.Txid] = pt
			bySeq[t.Seq] = append(bySeq[t.Seq], pt)
			if !found || t.Seq > top {
				top, found = t.Seq, true
			}
		}
	}
	if !found {
		return true, nil
	}
	stop := func(err error) (bool, error) {
		if errors.Is(err, ErrBudget) {
			*page = "top:" + strconv.FormatUint(top, 10)
			return false, ErrBudget
		}
		return false, err
	}
	asked := map[uint64]bool{}
	// The latest tokens stand for the answer at the top seq when the call
	// has them; a call resumed from a page asks for that seq like any other.
	if len(bySeq[top]) > 0 {
		asked[top] = true
	}
	at := func(seq uint64) ([]*peerToken, error) {
		if asked[seq] {
			return bySeq[seq], nil
		}
		service, query := c.AtSeq(seq)
		outs, err := w.Ask(ctx, service, query)
		if err != nil {
			return nil, err
		}
		asked[seq] = true
		for _, o := range outs {
			if byTxid[o.Txid] != nil {
				continue
			}
			t, err := c.Read(o.Tx)
			if err != nil || t.Seq != seq {
				continue
			}
			pt := &peerToken{out: o, tok: t}
			byTxid[o.Txid] = pt
			bySeq[seq] = append(bySeq[seq], pt)
		}
		return bySeq[seq], nil
	}
	for seq := uint64(0); seq <= top; seq++ {
		if seq < top && len(c.Held(seq)) > 0 {
			continue
		}
		ts, err := at(seq)
		if err != nil {
			return stop(err)
		}
		if len(ts) == 0 {
			if seq < top {
				w.Gap(fmt.Sprintf("%s: the peer answers no token at seq %d, below its latest %d", c.Name, seq, top))
			}
			return true, nil
		}
		ts = append([]*peerToken(nil), ts...)
		sort.Slice(ts, func(i, j int) bool { return ts[i].out.Txid.String() < ts[j].out.Txid.String() })
		some := false
		for _, pt := range ts {
			ok, err := w.token(ctx, c, pt, byTxid, at, 0)
			if err != nil {
				return stop(err)
			}
			some = some || ok
		}
		// Nothing at this seq is held or answered: the chain stops here for
		// this walk, since every later token needs a parent at or above it.
		if !some {
			return true, nil
		}
	}
	return true, nil
}

// budget passes ErrBudget through as the bound it is, anything else as the
// error it is.
func budget(err error) error {
	if errors.Is(err, ErrBudget) {
		return ErrBudget
	}
	return err
}

// token answers one of the peer's tokens if the walk wants it, its parent
// first when the host does not hold it. ok is false when the chain must
// stop here.
func (w *Walk) token(ctx context.Context, c *Chain, pt *peerToken, byTxid map[chainhash.Hash]*peerToken,
	at func(uint64) ([]*peerToken, error), depth int) (ok bool, err error) {
	txid := pt.out.Txid
	if w.gapped[txid] {
		return false, nil
	}
	if w.Sent(txid) {
		if w.SentNow(txid) {
			return true, nil
		}
		// Answered earlier in this walk and still not held: the host did
		// not admit it, so nothing built on it is answered in this walk.
		if held, err := w.held(ctx, c, txid); err != nil || held {
			return held, err
		}
		w.tokenGap(txid, fmt.Sprintf("%s: token %s (seq %d) was offered and is not held; the chain waits for the next walk", c.Name, txid, pt.tok.Seq))
		return false, nil
	}
	want, err := w.Wanted(ctx, txid)
	if err != nil || !want {
		return err == nil, err
	}
	var parent *transaction.Transaction
	var after []chainhash.Hash
	if pt.tok.Seq == 0 {
		if parent, err = w.tree(ctx, c, pt.tok.Tree.Txid); err != nil {
			w.tokenGap(txid, fmt.Sprintf("%s: the first token %s: its parent, the chain tree %s: %v", c.Name, txid, pt.tok.Tree.Txid, err))
			return false, nil
		}
	} else {
		prev := pt.tok.Prev
		local, err := c.Local(ctx, prev)
		if err != nil {
			return false, err
		}
		switch {
		case local != nil:
			parent = local
		case w.SentNow(prev):
			pp := byTxid[prev]
			if pp == nil {
				return false, fmt.Errorf("replay: %s: token %s was answered in this call and is not among the peer's tokens", c.Name, prev)
			}
			parent, after = pp.out.Tx, []chainhash.Hash{prev}
		case w.Sent(prev):
			w.tokenGap(txid, fmt.Sprintf("%s: token %s (seq %d) spends %s, which was offered and is not held; the chain waits for the next walk", c.Name, txid, pt.tok.Seq, prev))
			return false, nil
		default:
			pp := byTxid[prev]
			if pp == nil {
				ts, err := at(pt.tok.Seq - 1)
				if err != nil {
					return false, err
				}
				for _, x := range ts {
					if x.out.Txid == prev {
						pp = x
					}
				}
			}
			if pp == nil {
				w.tokenGap(txid, fmt.Sprintf("%s: token %s (seq %d) spends %s, which the peer does not answer at seq %d", c.Name, txid, pt.tok.Seq, prev, pt.tok.Seq-1))
				return false, nil
			}
			if depth >= MaxParentDepth {
				w.tokenGap(txid, fmt.Sprintf("%s: token %s (seq %d) is %d tokens above what the host holds on its branch; the next walk goes on", c.Name, txid, pt.tok.Seq, depth))
				return false, nil
			}
			// The parent first: answered in this call (then After), or
			// found applied after all.
			if ok, err := w.token(ctx, c, pp, byTxid, at, depth+1); err != nil || !ok {
				return ok, err
			}
			parent = pp.out.Tx
			if w.SentNow(prev) {
				after = []chainhash.Hash{prev}
			}
		}
	}
	beef, err := TokenBEEF(pt.out.Tx, parent)
	if err != nil {
		w.tokenGap(txid, fmt.Sprintf("%s: token %s: its BEEF: %v", c.Name, txid, err))
		return false, nil
	}
	if c.Check != nil {
		if err := c.Check(ctx, beef); err != nil {
			w.tokenGap(txid, fmt.Sprintf("%s: token %s (seq %d) as assembled breaks the application's rules here: %v", c.Name, txid, pt.tok.Seq, err))
			return false, nil
		}
	}
	if err := w.Emit(module.ReplayObject{Txid: txid, BEEF: beef, Kind: KindToken, After: after}); err != nil {
		return false, err
	}
	return true, nil
}

// tokenGap counts a token the walk cannot answer, once a call.
func (w *Walk) tokenGap(txid chainhash.Hash, note string) {
	w.gapped[txid] = true
	w.Gap(note)
}

// held reports whether the host holds a token of the chain by txid.
func (w *Walk) held(ctx context.Context, c *Chain, txid chainhash.Hash) (bool, error) {
	tx, err := c.Local(ctx, txid)
	return tx != nil, err
}

// tree reads a chain tree from the chain source, with its proof, held to
// the txid asked for; once a walk.
func (w *Walk) tree(ctx context.Context, c *Chain, txid chainhash.Hash) (*transaction.Transaction, error) {
	if tx := w.trees[txid]; tx != nil {
		return tx, nil
	}
	if c.Trees == nil {
		return nil, errors.New("no chain source is configured, and no host keeps a chain tree")
	}
	raw, err := c.Trees.TxRaw(ctx, txid.String())
	if err != nil {
		return nil, err
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if *tx.TxID() != txid {
		return nil, fmt.Errorf("the chain source answered transaction %s", tx.TxID())
	}
	mp, _, err := c.Trees.Proof(ctx, txid.String())
	if err != nil {
		return nil, fmt.Errorf("its proof: %w", err)
	}
	if mp == nil {
		return nil, errors.New("the chain source gives no proof")
	}
	tx.MerklePath = mp
	w.trees[txid] = tx
	return tx, nil
}

// TokenBEEF is the BEEF a host admits a token in, assembled from the token
// as a host stores it (alone, with its proof) and its parent
// (chaintoken.TokenBEEF): each path is first cut to its minimal form
// (chaintoken.Minimal) when it is not, since a stored path is the merged
// one a token was published with when it mined in its parent's block.
// Neither transaction is changed.
func TokenBEEF(token, parent *transaction.Transaction) ([]byte, error) {
	if token == nil || parent == nil || token.MerklePath == nil || parent.MerklePath == nil {
		return nil, errors.New("replay: a token's BEEF needs the token and its parent, both proven")
	}
	t, err := minimal(token)
	if err != nil {
		return nil, err
	}
	p, err := minimal(parent)
	if err != nil {
		return nil, err
	}
	return chaintoken.TokenBEEF(t, p)
}

func minimal(tx *transaction.Transaction) (*transaction.Transaction, error) {
	txid := *tx.TxID()
	if chaintoken.MinimalPath(tx.MerklePath, txid) {
		return tx, nil
	}
	mp, err := chaintoken.Minimal(tx.MerklePath, txid)
	if err != nil {
		return nil, err
	}
	c := tx.ShallowClone()
	c.MerklePath = mp
	return c, nil
}
