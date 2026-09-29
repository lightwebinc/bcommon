package producer

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/publish"
)

// TreeState is where an application keeps its funding trees: the current
// tree, which carriers spend from, and every tree it minted, which a kill
// switch sweeps. The application's own state file holds both, in its own
// format.
type TreeState interface {
	// Current is the tree carriers spend from now, or nil before the first.
	// Trees reads it and never writes through it; the application advances
	// its Next as it spends outputs, then calls Index.
	Current() *funding.Tree
	// Adopt makes t the current tree, adds it to the trees kept for a
	// sweep, and saves the state. Trees calls it once the tree is minted
	// and settled, before the tree is published.
	Adopt(t funding.Tree) error
}

// Trees is the funding-tree lifecycle. Spend answers the tree the next
// carriers spend from: the current one while it has the outputs, or a new
// one, minted, settled, recorded and published.
type Trees struct {
	// Payer pays for a tree minted from the pool, settles it and takes its
	// change. Its Kept holds the current tree.
	Payer *Payer
	// State is the application's record of its trees.
	State TreeState
	// Identity is the identity key hex whose derived key locks the tree's
	// outputs. A current tree locked to another identity is replaced,
	// because a successor cannot spend a predecessor's tree.
	Identity string
	// Count is how many outputs a new tree has, at least; Spend mints more
	// when one spend needs more. Sats is the value of each.
	Count int
	Sats  uint64
	// Funder is recorded in the new tree's funding.Tree.Funder: how the
	// tree was paid for, in the application's words.
	Funder string
	// Lock returns the funding lock, normally carrier.FundingLock under the
	// application's carrier.Params. It is asked only when the pool pays for
	// a tree, since deriving it asks the wallet.
	Lock func(ctx context.Context) (*script.Script, error)
	// Change returns the script the fee input's change is paid to, normally
	// the signer's bwallet.Signer.FundScript. It is asked only when the pool
	// pays for a tree.
	Change func() (*script.Script, error)
	// Fund, when set, mints and settles a tree some other way, such as a
	// BRC-100 wallet that funds and broadcasts it itself, and returns it
	// with its proof and height, or with neither when it is collected later.
	// The pool, Lock and Change are then not used, and DryRun does not
	// apply.
	Fund func(ctx context.Context, count int) (*transaction.Transaction, *transaction.MerklePath, uint32, error)
	// DryRun builds a tree the pool pays for and returns it without
	// settling, recording or publishing it. The fee input stays reserved
	// until the Payer's GiveBack.
	DryRun bool
	// Facade and Topic are the object leg a new tree is published on: hosts
	// admit its outputs so that a later sweep of them, the kill switch, is a
	// spend they see.
	Facade *publish.Facade
	Topic  string
}

// Spend returns the tree the next need carriers spend from, and the index
// of the first output they spend.
//
// The current tree is used while it has need outputs left and is locked to
// Identity. Otherwise a new tree is minted, never smaller than need, since
// need is known before anything is minted: a producer that spent a tree
// halfway through one publish and then stopped would leave carriers on the
// plane that nothing commits to. The outputs a replaced tree has left are
// stranded, and a sweep takes them with the rest.
//
// A new tree is paid for by Fund when it is set, and otherwise from the pool
// through the Payer, and then settled (Payer.Settle) and its change taken.
// Its funding.Tree, with the BEEF it is kept as while it is unmined, is
// adopted into the state before the tree is published, so a crash after the
// publish never leaves a tree on the plane the state does not know.
func (t *Trees) Spend(ctx context.Context, need uint32) (*transaction.Transaction, uint32, error) {
	if t.Payer == nil || t.State == nil {
		return nil, 0, errors.New("producer: Trees needs a Payer and a State")
	}
	if cur := t.State.Current(); cur != nil && cur.Remaining() >= need && cur.IdentityKeyHex == t.Identity {
		tx, err := t.Payer.Kept.Tx(cur.Txid)
		if err != nil {
			return nil, 0, fmt.Errorf("funding tree: %w", err)
		}
		return tx, cur.Next, nil
	}
	// A tree at least as large as the spend needs, and never smaller than
	// Count asks for.
	count := t.Count
	if uint32(count) < need { //nolint:gosec // a small configured count
		count = int(need)
		t.Payer.note("this transition spends %d outputs, so the tree is minted with %d rather than %d", need, count, t.Count)
	}
	var tree *transaction.Transaction
	var mp *transaction.MerklePath
	var height uint32
	var err error
	if t.Fund != nil {
		if tree, mp, height, err = t.Fund(ctx, count); err != nil {
			return nil, 0, err
		}
	} else {
		fee, err := t.Payer.Take(ctx)
		if err != nil {
			return nil, 0, err
		}
		if t.Change == nil {
			return nil, 0, errors.New("producer: Trees needs a Change script")
		}
		changeTo, err := t.Change()
		if err != nil {
			return nil, 0, err
		}
		if tree, err = t.mint(ctx, count, fee, changeTo); err != nil {
			return nil, 0, err
		}
		t.Payer.note("funding tree %s: %d output(s) of %d sat", tree.TxID(), count, t.Sats)
		if t.DryRun {
			return tree, 0, nil
		}
		if mp, height, err = t.Payer.Settle(ctx, "funding tree", tree); err != nil {
			return nil, 0, err
		}
		t.Payer.Change(tree, height, mp)
	}
	// An unmined tree is kept as its BEEF too, because every carrier that
	// spends it has to carry the tree's ancestry until the proof arrives.
	treeBeef, err := funding.KeepBEEF(tree, mp)
	if err != nil {
		return nil, 0, fmt.Errorf("funding tree BEEF: %w", err)
	}
	if err := t.State.Adopt(funding.Tree{IdentityKeyHex: t.Identity, Txid: tree.TxID().String(), RawHex: tree.Hex(),
		BumpHex: funding.BumpHex(mp), Height: height, BeefHex: treeBeef, Sats: t.Sats,
		Count: uint32(count), Next: 0, Funder: t.Funder}); err != nil { //nolint:gosec // a small configured count
		return nil, 0, err
	}
	tb, err := tree.AtomicBEEF(false)
	if err != nil {
		return nil, 0, err
	}
	if t.Facade == nil {
		return nil, 0, errors.New("publish funding tree: no object leg")
	}
	res, err := t.Facade.Submit(ctx, t.Topic, tb)
	if err != nil {
		return nil, 0, fmt.Errorf("publish funding tree: %w", err)
	}
	if !res.Duplicate && len(res.Admitted) == 0 {
		return nil, 0, fmt.Errorf("publish funding tree: the topic manager admitted nothing: %s", string(res.Raw))
	}
	t.Payer.note("funding tree published: admitted %d output(s)", len(res.Admitted))
	return tree, 0, nil
}

// mint builds and signs a tree the pool pays for. The count and value are
// checked before Lock is asked, so a bad one is reported ahead of a wallet
// that cannot answer, in the words mint.FundingTree uses for it.
func (t *Trees) mint(ctx context.Context, count int, fee mint.Input, change *script.Script) (*transaction.Transaction, error) {
	if count < 1 || t.Sats < 1 {
		return nil, errors.New("mint: a funding tree needs at least one output of at least one satoshi")
	}
	if t.Lock == nil {
		return nil, errors.New("producer: Trees needs a Lock")
	}
	lock, err := t.Lock(ctx)
	if err != nil {
		return nil, err
	}
	return mint.FundingTree(lock, count, t.Sats, fee, change, t.Payer.Fees)
}

// Index keeps the history of trees in step with the current one, so a sweep
// sees the same Next the spends advanced: the entry with cur's txid becomes
// a copy of cur, or cur is appended when no entry has it. It returns the
// history, which the caller stores back. A nil cur changes nothing.
func Index(all []funding.Tree, cur *funding.Tree) []funding.Tree {
	if cur == nil {
		return all
	}
	for i := range all {
		if all[i].Txid == cur.Txid {
			all[i] = *cur
			return all
		}
	}
	return append(all, *cur)
}
