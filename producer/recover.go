package producer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/nodeapi"
)

// RecoveryOutcome is what Trees.Recover found for one record Prepare was
// given.
type RecoveryOutcome int

const (
	// CoinReturned: the node knows no such tree and shows the fee coin
	// unspent, so the tree had not reached the chain when Recover asked.
	// The coin is back in the pool. The application keeps the record and
	// recovers it again on its next start, because a tree handed to the
	// settlement leg just before the run stopped can still land: see
	// Recover.
	CoinReturned RecoveryOutcome = iota + 1
	// CoinSpent: the node knows no such tree and shows the fee coin spent
	// by another transaction, named in Recovery.By. The coin is out of the
	// pool, nothing is left to recover, and the application drops the
	// record.
	CoinSpent
	// TreeAdopted: the tree is on the chain. Its fee coin is out of the
	// pool, its unspent change is in the pool, and it was adopted into the
	// state and published, as Spend adopts a new tree. Adopt has dropped
	// the record. When Recover returns it together with a *PublishError,
	// the tree is adopted and only the publish is to be repeated.
	TreeAdopted
	// TreeHeld: the tree is on the chain, its fee coin is out of the pool
	// and its unspent change is in it, but the current tree still has
	// outputs, so the tree is held, behind any tree already held (Held
	// answers them in order, Prepared the first), and adopted when a spend
	// switches to it. The application keeps the record until Adopt names
	// its txid.
	TreeHeld
)

func (o RecoveryOutcome) String() string {
	switch o {
	case CoinReturned:
		return "coin returned"
	case CoinSpent:
		return "coin spent"
	case TreeAdopted:
		return "tree adopted"
	case TreeHeld:
		return "tree held"
	}
	return "unknown"
}

// Recovery is the result of Trees.Recover.
type Recovery struct {
	Outcome RecoveryOutcome
	// Tree is the tree's record with its proof and height, or the BEEF it
	// is kept as while it is unmined: what it was adopted as, or will be.
	// Set for TreeAdopted and TreeHeld. It is what Publish takes when
	// Recover returns a *PublishError.
	Tree funding.Tree
	// By is the transaction that spent the fee coin. Set for CoinSpent.
	By string
}

// Recover settles one record Prepare was given and Adopt never dropped: a
// tree that was signed for the coin and that a run stopped short of
// adopting. An application runs it on its next start, once for each such
// record, before the first Spend, with tree and coin as Prepare received
// them. It asks the Payer's Asset, which must be set, what became of the
// tree:
//
//   - The node knows the tree, or shows the coin spent by it: the tree is
//     on the chain. Its fee coin is spent, so it is taken out of the pool
//     if an earlier CoinReturned put it there. Its proof is taken as Settle
//     would take it (waited for, or with Async asked for once), the change
//     it pays the Payer's keys goes into the pool unless the node shows
//     that change already spent, and the tree is adopted and published
//     (TreeAdopted). When the current tree is locked to Identity and still
//     has outputs, adopting would strand them, so the tree is held instead
//     (TreeHeld) and adopted at a switch. Any number of trees are held, in
//     the order Recover was asked about them, and Spend switches to the
//     first that is large enough for the spend (see Held), so a second
//     record found on the chain strands nothing. A held tree locked to
//     another identity than Identity is never switched to (see Spend), so
//     its record is one the application moves to its own history.
//   - The node does not know the tree and shows the coin unspent: the tree
//     has not reached the chain. The coin goes back to the pool
//     (CoinReturned).
//   - The node does not know the tree and shows the coin spent by another
//     transaction (CoinSpent). The coin is taken out of the pool if it is
//     there.
//
// The application's half is its record. Its Adopt drops the record of the
// tree it is given, in the same save, so TreeAdopted needs nothing more and
// a TreeHeld record lasts until the switch. After CoinSpent it drops the
// record itself. After CoinReturned it keeps the record and recovers it
// again on its next start, until a later Recover answers CoinSpent,
// TreeAdopted or TreeHeld, or until a transaction of its own that spends
// the coin has mined; the reason is the node's view, below.
//
// An error with no outcome means the node could not answer, or the tree has
// not mined within the Payer's Timeout: the tree is neither adopted nor
// held, the record stays, and the next start asks again. The fee coin of a
// tree the node knows is out of the pool even so, which is right, since the
// node holds it spent. Recover may be run again for the same record: a coin
// the pool holds is not added twice, and a tree that is already the current
// one, or already held, is answered TreeAdopted or TreeHeld with nothing
// done.
//
// One error comes with an outcome. Adopt is called before the tree is
// published (see Spend), so the publish can fail with the tree adopted and
// its record dropped. Recover then returns TreeAdopted with the tree and a
// *PublishError (errors.Is ErrPublish): nothing is left to recover, and the
// application repeats the publish alone, with Publish and Recovery.Tree.
// Running Recover again would not publish it, since the tree is then the
// current one.
//
// A record also outlives a mint that failed after Prepare, since Spend
// puts the coin back and leaves the record alone; Recover then answers
// CoinReturned, or CoinSpent once the coin has paid for something else. It
// should not be dropped on Spend's error, which may come after the tree
// reached the leg.
//
// The node's view is a moment's view. A tree handed to the settlement leg
// an instant before the run stopped may not have reached the node when
// Recover asks, and is then answered CoinReturned, with the coin back in
// the pool; the tree can still land afterwards. That is why a CoinReturned
// record is kept: the next start's Recover finds the tree on the chain,
// takes the coin, which the tree spent, out of the pool, takes the tree's
// change and adopts or holds the tree, so nothing is lost. An application
// that drops the record at CoinReturned is, for such a tree, where it was
// without Prepare: the tree, its change and its outputs are lost to it, and
// the spent coin stays in its pool.
//
// Two things remain uncovered, both inside the same moment:
//
//   - Between the start that answered CoinReturned and the next, the coin
//     is in the pool. If the tree lands in that time and the coin is taken
//     to pay for a transaction, that transaction is refused (ErrRefused)
//     and has to be built again. No coin or tree is lost by it; if the
//     transaction was a funding tree, its own record is answered CoinSpent.
//   - A kept record is asked about on every start, and CoinReturned puts
//     the coin back each time. If the application has since spent the coin
//     in a transaction of its own, and that transaction in turn reached the
//     leg an instant before a stop, the node may show the coin unspent once
//     more and the coin goes back to the pool though it is spent. A
//     transaction that then takes it is refused, or, reaching the node
//     first, displaces the earlier one. The next Recover that answers
//     CoinSpent takes the coin out again.
//
// An application that restarts at once can wait a moment before it
// recovers, which makes both rarer.
//
// A tree Fund paid for has no record, and Recover has nothing to say about
// it: see Spend.
func (t *Trees) Recover(ctx context.Context, tree funding.Tree, coin bwallet.Output) (Recovery, error) {
	if t.Payer == nil || t.State == nil {
		return Recovery{}, errors.New("producer: Trees needs a Payer and a State")
	}
	asset := t.Payer.Asset
	if asset == nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: no node to ask", tree.Txid)
	}
	cur := t.State.Current()
	if cur != nil && cur.Txid == tree.Txid {
		return Recovery{Outcome: TreeAdopted, Tree: *cur}, nil
	}
	for _, pt := range t.held {
		if pt.rec.Txid == tree.Txid {
			return Recovery{Outcome: TreeHeld, Tree: pt.rec}, nil
		}
	}
	tx, err := rawTx(tree.RawHex)
	if err != nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: %w", tree.Txid, err)
	}
	if tx.TxID().String() != tree.Txid {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: the record's bytes are transaction %s", tree.Txid, tx.TxID())
	}
	var fee *transaction.TransactionInput
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == coin.TxID && in.SourceTxOutIndex == coin.Vout {
			fee = in
		}
	}
	if fee == nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: it does not spend the coin %s", tree.Txid, coin.Outpoint())
	}

	known, err := knows(ctx, asset, tree.Txid)
	if err != nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: %w", tree.Txid, err)
	}
	if !known {
		by, err := asset.Spender(ctx, coin.TxID, coin.Vout)
		if err != nil {
			return Recovery{}, fmt.Errorf("funding tree %s: recover: whether its fee coin %s is spent: %w", tree.Txid, coin.Outpoint(), err)
		}
		switch {
		case by == "":
			if err := t.Payer.Pool.Return(coin); err != nil {
				return Recovery{}, fmt.Errorf("funding tree %s: recover: returning its fee coin: %w", tree.Txid, err)
			}
			t.Payer.note("funding tree %s never reached the chain: its fee coin %s is unspent and back in the pool", tree.Txid, coin.Outpoint())
			return Recovery{Outcome: CoinReturned}, nil
		case !strings.EqualFold(by, tree.Txid):
			if err := t.spent(tree.Txid, coin); err != nil {
				return Recovery{}, err
			}
			t.Payer.note("funding tree %s never reached the chain: its fee coin %s is spent by %s", tree.Txid, coin.Outpoint(), by)
			return Recovery{Outcome: CoinSpent, By: by}, nil
		}
	}

	// The tree is on the chain, so its fee coin is spent. An earlier
	// recovery that asked before the tree landed put the coin back in the
	// pool; it comes out before anything else is done, so that it pays for
	// nothing while the proof is waited for.
	if err := t.spent(tree.Txid, coin); err != nil {
		return Recovery{}, err
	}

	// The tree is rebuilt over its fee coin's parent, so that the BEEF it is
	// kept and published as carries its ancestry.
	parent, err := t.Payer.Parent(ctx, coin)
	if err != nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: %w", tree.Txid, err)
	}
	fee.SourceTransaction = parent
	var mp *transaction.MerklePath
	var height uint32
	if t.Payer.Async {
		if mp, height, err = asset.Proof(ctx, tree.Txid); errors.Is(err, nodeapi.ErrNotMined) {
			mp, height, err = nil, 0, nil
		}
		tx.MerklePath = mp
	} else {
		mp, height, err = t.Payer.Await(ctx, "funding tree", tx)
	}
	if err != nil {
		return Recovery{}, fmt.Errorf("funding tree %s: recover: %w", tree.Txid, err)
	}
	// Change the pool was already given may have paid a fee since, so only
	// change the node shows unspent is taken.
	var cerr error
	t.Payer.change(tx, height, mp, func(vout uint32) bool {
		by, err := asset.Spender(ctx, tree.Txid, vout)
		if err != nil && cerr == nil {
			cerr = fmt.Errorf("funding tree %s: recover: whether its change, output %d, is spent: %w", tree.Txid, vout, err)
		}
		return err == nil && by == ""
	})
	if cerr != nil {
		return Recovery{}, cerr
	}
	rec, err := t.record(tx, mp, height, int(tree.Count), tree.IdentityKeyHex, tree.Funder)
	if err != nil {
		return Recovery{}, err
	}
	rec.Sats = tree.Sats

	// Adopting a tree while the current one has outputs would strand them,
	// so the tree waits its turn behind whatever is already held.
	if cur != nil && cur.IdentityKeyHex == t.Identity && cur.Remaining() > 0 {
		t.hold(tx, rec)
		t.Payer.note("funding tree %s is recovered and waits for the switch", rec.Txid)
		return Recovery{Outcome: TreeHeld, Tree: rec}, nil
	}
	t.Payer.note("funding tree %s is recovered", rec.Txid)
	if err := t.adoptAndPublish(ctx, tx, rec); err != nil {
		if errors.Is(err, ErrPublish) {
			// The state has the tree and the record is dropped: only the
			// publish is left, and the caller is told so.
			return Recovery{Outcome: TreeAdopted, Tree: rec}, err
		}
		return Recovery{}, err
	}
	return Recovery{Outcome: TreeAdopted, Tree: rec}, nil
}

// spent takes a fee coin the node shows spent out of the pool, where an
// earlier CoinReturned may have put it, and out of the coins a GiveBack
// would return.
func (t *Trees) spent(tree string, coin bwallet.Output) error {
	t.Payer.release(coin)
	removed, err := t.Payer.Pool.Remove(coin)
	if err != nil {
		return fmt.Errorf("funding tree %s: recover: taking its spent fee coin %s out of the pool: %w", tree, coin.Outpoint(), err)
	}
	if removed {
		t.Payer.note("funding tree %s: its fee coin %s is spent and is taken out of the pool", tree, coin.Outpoint())
	}
	return nil
}

// knows reports whether the node serves the transaction txid, mined or not.
func knows(ctx context.Context, asset *nodeapi.Asset, txid string) (bool, error) {
	raw, err := asset.TxRaw(ctx, txid)
	if nodeapi.IsHTTP(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return false, err
	}
	if tx.TxID().String() != txid {
		return false, fmt.Errorf("the node answered transaction %s", tx.TxID())
	}
	return true, nil
}
