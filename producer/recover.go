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
	// unspent, so the tree never reached the chain. The coin is back in the
	// pool, and the application drops the record.
	CoinReturned RecoveryOutcome = iota + 1
	// CoinSpent: the node knows no such tree and shows the fee coin spent
	// by another transaction, named in Recovery.By. Nothing is left to
	// recover, and the application drops the record.
	CoinSpent
	// TreeAdopted: the tree is on the chain. Its unspent change is in the
	// pool, and it was adopted into the state and published, as Spend
	// adopts a new tree. Adopt has dropped the record.
	TreeAdopted
	// TreeHeld: the tree is on the chain and its unspent change is in the
	// pool, but the current tree still has outputs, so the tree is held as
	// the tree minted ahead (Prepared answers it) and adopted when a spend
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
	// Set for TreeAdopted and TreeHeld.
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
//     on the chain. Its proof is taken as Settle would take it (waited for,
//     or with Async asked for once), the change it pays the Payer's keys
//     goes into the pool unless the node shows that change already spent,
//     and the tree is adopted and published (TreeAdopted). When the current
//     tree is locked to Identity and still has outputs, adopting would
//     strand them, so the tree is held as the tree minted ahead instead
//     (TreeHeld) and adopted at the switch; at most one tree is held, and a
//     second is adopted. A held tree locked to another identity than
//     Identity is never switched to (see Spend), so its record is one the
//     application moves to its own history.
//   - The node does not know the tree and shows the coin unspent: the tree
//     never reached the chain. The coin goes back to the pool
//     (CoinReturned).
//   - The node does not know the tree and shows the coin spent by another
//     transaction (CoinSpent).
//
// The application's half is its record. Its Adopt drops the record of the
// tree it is given, in the same save, so TreeAdopted needs nothing more and
// a TreeHeld record lasts until the switch. After CoinReturned and
// CoinSpent it drops the record itself. An error means the node could not
// answer, or the tree has not mined within the Payer's Timeout: nothing is
// decided, the record stays, and the next start asks again. Recover may be
// run again for the same record: a coin the pool holds is not added twice,
// and a tree that is already the current one is answered TreeAdopted with
// nothing done.
//
// A record also outlives a mint that failed after Prepare, since Spend
// puts the coin back and leaves the record alone; Recover then answers
// CoinReturned, or CoinSpent once the coin has paid for something else. It
// should not be dropped on Spend's error, which may come after the tree
// reached the leg.
//
// The node's view is a moment's view. A tree handed to the settlement leg
// an instant before the run stopped may not have reached the node when
// Recover asks, and is then answered CoinReturned; if it lands afterwards
// the returned coin is spent, the transaction that next takes it is
// refused (ErrRefused), and the tree is lost as it would be with no
// Prepare. An application that restarts at once can wait a moment before
// it recovers.
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
			t.Payer.note("funding tree %s never reached the chain: its fee coin %s is spent by %s", tree.Txid, coin.Outpoint(), by)
			return Recovery{Outcome: CoinSpent, By: by}, nil
		}
	}

	// The tree is on the chain. It is rebuilt over its fee coin's parent, so
	// that the BEEF it is kept and published as carries its ancestry.
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

	if cur != nil && cur.IdentityKeyHex == t.Identity && cur.Remaining() > 0 && t.prepared == nil && t.ahead == nil {
		t.prepared = &preparedTree{tx: tx, rec: rec}
		t.Payer.note("funding tree %s is recovered and waits for the switch", rec.Txid)
		return Recovery{Outcome: TreeHeld, Tree: rec}, nil
	}
	t.Payer.note("funding tree %s is recovered", rec.Txid)
	if err := t.adoptAndPublish(ctx, tx, rec); err != nil {
		return Recovery{}, err
	}
	return Recovery{Outcome: TreeAdopted, Tree: rec}, nil
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
