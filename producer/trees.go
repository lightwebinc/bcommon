package producer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"weak"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
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
	// when one spend needs more, up to mint.MaxFundingOutputs. A tree of
	// more is refused with an error wrapping mint.ErrTreeTooLarge before a
	// coin is taken or Fund is called. Sats is the value of each.
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
	// Ahead, when above zero, mints the next tree before the current one
	// runs out: once a spend leaves Ahead outputs or fewer on the current
	// tree, the next tree is minted and settled in the background, and Spend
	// switches to it when the current tree cannot cover a spend, with no
	// wait for a block. Zero mints a tree only when one is needed. See
	// Spend for the rules, and Wait and Prepared for the tree minted ahead.
	// A pool-paid mint holds its coin until it is collected, and a take the
	// pool cannot cover meanwhile waits for it: see Payer.Take.
	Ahead uint32
	// Prepare, when set, is called with each tree the pool pays for, once it
	// is signed and before it reaches the settlement leg: on the goroutine
	// that called Spend, and for a tree minted ahead before its background
	// half starts. tree is the record the tree is later adopted as, without
	// a proof, a height or a kept BEEF, which it does not have yet, and coin
	// is the fee coin it spends, already out of the pool. An application
	// saves both, so a run that stops before Adopt leaves a record of a
	// tree that may be on the chain and of the coin it took; Recover
	// settles that record on the next start. An error aborts the mint and
	// puts the coin back in the pool. Prepare is not called for a DryRun,
	// nor for a tree Fund pays for: see Spend.
	Prepare func(tree funding.Tree, coin bwallet.Output) error

	// ahead delivers the result of the mint in flight, nil when none is;
	// held is every tree that is settled and not yet used, in the order
	// Spend switches to them: the tree minted ahead, and each tree Recover
	// found on the chain while the current tree had outputs left; aheadFor
	// is the current tree the last mint ahead was started for, so a failure
	// is not retried on every spend from the same tree.
	//
	// mintPool is the pool the mint in flight took its coin from, set while
	// that mint is listed for a Payer's take to wait for (see minting).
	ahead    chan aheadResult
	held     []*preparedTree
	aheadFor string
	mintPool weak.Pointer[bwallet.Pool]
}

// ErrPublish is what errors.Is matches a *PublishError by: a funding tree
// that is adopted into the state and was not published.
var ErrPublish = errors.New("producer: funding tree adopted but not published")

// PublishError is the error of Spend and Recover when TreeState.Adopt took
// a tree and the publish that follows it failed. The tree is recorded, it
// is the current tree, and its Prepare record is dropped: nothing is left
// to mint, settle or recover, and only the publish has to be repeated,
// which Trees.Publish does with Tree. Until it is, the hosts have not
// admitted the tree's outputs. errors.Is(err, ErrPublish) matches it,
// errors.As gives the tree, and Unwrap gives the cause.
type PublishError struct {
	// Tree is the record the tree was adopted as.
	Tree funding.Tree
	// Err is why the publish failed.
	Err error
}

func (e *PublishError) Error() string { return e.Err.Error() }

func (e *PublishError) Unwrap() error { return e.Err }

// Is matches ErrPublish.
func (e *PublishError) Is(target error) bool { return target == ErrPublish }

// preparedTree is a held tree: settled, its change taken, and its record
// built, but not adopted and not published.
type preparedTree struct {
	tx  *transaction.Transaction
	rec funding.Tree
}

// aheadResult is what the background half of a mint ahead hands back to the
// goroutine that owns the Trees.
type aheadResult struct {
	tx     *transaction.Transaction
	mp     *transaction.MerklePath
	height uint32
	count  int
	id     string
	funder string
	// change is set when the pool paid, so the tree's change is taken; coin
	// is the fee coin to put back when the tree never reached the leg, and
	// by the transaction that spent that coin, when one did: the coin is
	// then dropped rather than put back.
	change bool
	coin   *bwallet.Output
	by     string
	notes  []string
	err    error
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
// The pool pays with a coin of at least the tree's value and the fee floor.
// A failure before the tree reaches the settlement leg puts that coin back in
// the pool before Spend returns; once the tree has reached the leg the coin
// is spent and no later GiveBack returns it.
//
// One failure before the leg does not put the coin back: a tree the leg
// refuses because its fee coin is already spent. When the refusal names
// another transaction as the coin's spender (a *nodeapi.SpentError for its
// outpoint, as publish.Arcade held to a node answers), or the Payer's
// Asset, when set, shows the coin spent by a transaction other than the
// tree, the coin is taken out of the pool (bwallet.Pool.Remove) and is not
// returned, with a note naming the spender: put back, it would be handed
// out again and fail the next build, until a later start's Recover took it
// out. A refusal for any other reason, a coin the node shows unspent or
// spent by the tree itself, and a coin the node cannot answer for all put
// the coin back as before. A tree minted ahead is treated the same when it
// is collected. Its Prepare record is left alone either way, and Recover
// answers CoinSpent for it. Payer.Settle does the same for the fee coin of
// any other transaction.
//
// The pool-paid mint here takes its coin as Payer.Take does, so with no
// coin in the pool it first waits for a tree another Trees over the same
// pool is minting ahead, whose change may pay for this one.
// Its funding.Tree, with the BEEF it is kept as while it is unmined, is
// adopted into the state before the tree is published, so a crash after the
// publish never leaves a tree on the plane the state does not know.
//
// That order means a publish can fail with the tree already adopted. Spend
// then returns a *PublishError (errors.Is ErrPublish) and no tree: the tree
// is the current one, and a later Spend answers it without publishing it,
// so the application repeats the publish with Publish, in this run or a
// later one, and then calls Spend again. Any other error leaves no tree
// adopted.
//
// With Ahead set, and not DryRun, a spend that leaves Ahead outputs or
// fewer on the tree it answers starts minting the next one, of Count
// outputs, once per tree. A pool-paid tree takes its fee coin and is signed
// here, on the caller's goroutine, so the reservation never races another
// Take; only a proven coin is taken, of at least the tree's value and the
// fee floor, never change Allow would let through,
// because the tree is settled on its own. The settlement (Payer.Settle), or
// Fund when it is set, then runs in the background under ctx, so ctx should
// be the producer's run, not one that ends with this call. Its notes are
// held and reported through Note by the Spend or Wait that collects it,
// which also takes the tree's change into the pool. At most one mint is in
// flight, and none is started while a tree minted ahead waits unused.
//
// Between the moment a pool-paid mint ahead takes its coin and the moment
// it is collected, the coin is out of the pool and its change is not yet in
// it. A wallet with one coin has none in that time. A mint ahead that finds
// no coin for itself never waits: it is skipped with a note, and the tree
// is minted when it is needed. A take through a Payer over the same pool
// (Payer.Take, Payer.TakeAtLeast, and the mint a Spend makes on demand)
// that the pool cannot cover does wait: it collects the mint, as Wait
// does, and takes again, bounded by its context and its Payer's Timeout.
// Payer.Take has the rules.
//
// When the current tree cannot cover need, Spend waits for a mint still in
// flight, then switches to the tree minted ahead if it is locked to Identity
// and has need outputs: it is adopted and published exactly as a new tree
// is, and nothing is minted. A mint ahead that failed is reported through
// Note, the fee coin it had not spent goes back to the pool, and the tree is
// minted here as it is with Ahead zero. A tree minted ahead that is too small
// waits for a later switch; one locked to an earlier identity is dropped.
//
// The tree minted ahead is adopted and published only when it is used, never
// when it is minted: adopting it earlier would make it current and strand
// the outputs left on the tree carriers still spend, and publishing it
// before it is adopted would break the rule above. It lives in memory until
// then, so a crash before the switch leaves it on the chain but in neither
// the state nor the plane; its change is already in the pool, and only its
// funding outputs are stranded. An application that wants a sweep to take
// those too records Prepared in its own history.
//
// A run can also stop between the moment a tree's fee coin leaves the pool
// and the moment the tree is adopted: while the tree waits for its block,
// or, minted ahead, for the switch. The pool is saved without the coin, and
// the state does not know the tree, so the coin, the tree's change and its
// outputs are all lost to the application, though the chain holds them. An
// application closes that gap with Prepare, which hands it the tree's
// record and the coin before the tree reaches the settlement leg, and with
// Recover, which it runs for each such record on the next start.
//
// Neither covers a tree Fund pays for. The wallet behind Fund chooses the
// coins, signs and broadcasts inside one call, so there is no moment
// between the signing and the settlement leg for Prepare to be called in,
// and no pool coin to return: a run that stops after Fund has broadcast and
// before Adopt leaves a tree only that wallet knows.
func (t *Trees) Spend(ctx context.Context, need uint32) (*transaction.Transaction, uint32, error) {
	if t.Payer == nil || t.State == nil {
		return nil, 0, errors.New("producer: Trees needs a Payer and a State")
	}
	_ = t.collect(ctx, false)
	if cur := t.State.Current(); cur != nil && cur.Remaining() >= need && cur.IdentityKeyHex == t.Identity {
		tx, err := t.Payer.Kept.Tx(cur.Txid)
		if err != nil {
			return nil, 0, fmt.Errorf("funding tree: %w", err)
		}
		t.mintAhead(ctx, cur.Txid, cur.Remaining()-need)
		return tx, cur.Next, nil
	}
	if t.ahead != nil {
		if err := t.collect(ctx, true); err != nil && ctx.Err() != nil {
			return nil, 0, err
		}
	}
	// The held trees are tried in order. One too small for this spend keeps
	// its place for a later switch.
	for i := 0; i < len(t.held); {
		pt := t.held[i]
		switch {
		case pt.rec.IdentityKeyHex != t.Identity:
			t.held = slices.Delete(t.held, i, i+1)
			t.Payer.note("funding tree %s minted ahead is locked to another identity and is not used", pt.rec.Txid)
		case pt.rec.Count >= need:
			t.held = slices.Delete(t.held, i, i+1)
			t.Payer.note("switching to funding tree %s, minted ahead", pt.rec.Txid)
			return t.adopt(ctx, pt.tx, pt.rec, need)
		default:
			i++
		}
	}
	// A tree at least as large as the spend needs, and never smaller than
	// Count asks for, and never larger than a tree holds.
	count := t.Count
	if uint32(count) < need { //nolint:gosec // a small configured count
		if need > mint.MaxFundingOutputs {
			return nil, 0, fmt.Errorf("producer: this transition spends %d outputs: %w", need, mint.ErrTreeTooLarge)
		}
		count = int(need)
		t.Payer.note("this transition spends %d outputs, so the tree is minted with %d rather than %d", need, count, t.Count)
	}
	if count > mint.MaxFundingOutputs {
		return nil, 0, fmt.Errorf("producer: Count %d: %w", count, mint.ErrTreeTooLarge)
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
		fee, coin, err := t.Payer.take(ctx, t.treeNeed(count))
		if err != nil {
			return nil, 0, err
		}
		// Every failure before the tree reaches the leg puts the coin back
		// here, so a caller that returns on the error without a GiveBack
		// loses no coin.
		if t.Change == nil {
			t.Payer.giveBackOne(coin)
			return nil, 0, errors.New("producer: Trees needs a Change script")
		}
		changeTo, err := t.Change()
		if err != nil {
			t.Payer.giveBackOne(coin)
			return nil, 0, err
		}
		if tree, err = t.mint(ctx, count, fee, changeTo); err != nil {
			t.Payer.giveBackOne(coin)
			return nil, 0, err
		}
		t.Payer.note("funding tree %s: %d output(s) of %d sat", tree.TxID(), count, t.Sats)
		if t.DryRun {
			return tree, 0, nil
		}
		if err := t.prepare(tree, count, t.Identity, t.Funder, coin); err != nil {
			t.Payer.giveBackOne(coin)
			return nil, 0, err
		}
		// The copy holds no reserved coin: what becomes of this tree's coin
		// is decided here, not by the copy's Settle.
		settler := *t.Payer
		settler.spent = nil
		var leg *recordingSettler
		if settler.Settler != nil {
			leg = &recordingSettler{Settler: settler.Settler}
			settler.Settler = leg
		}
		mp, height, err = settler.Settle(ctx, "funding tree", tree)
		if err != nil {
			if leg == nil || !leg.submitted {
				t.Payer.giveBackUnspent(ctx, coin, "funding tree", tree, err)
			} else {
				t.Payer.release(coin)
			}
			return nil, 0, err
		}
		// The coin is spent by a tree on the leg: a GiveBack after a later
		// failure must not put it back.
		t.Payer.release(coin)
		t.Payer.Change(tree, height, mp)
	}
	rec, err := t.record(tree, mp, height, count, t.Identity, t.Funder)
	if err != nil {
		return nil, 0, err
	}
	return t.adopt(ctx, tree, rec, need)
}

// record is the funding.Tree a new tree is adopted as. An unmined tree is
// kept as its BEEF too, because every carrier that spends it has to carry
// the tree's ancestry until the proof arrives.
func (t *Trees) record(tree *transaction.Transaction, mp *transaction.MerklePath, height uint32, count int, id, funder string) (funding.Tree, error) {
	treeBeef, err := funding.KeepBEEF(tree, mp)
	if err != nil {
		return funding.Tree{}, fmt.Errorf("funding tree BEEF: %w", err)
	}
	return funding.Tree{IdentityKeyHex: id, Txid: tree.TxID().String(), RawHex: tree.Hex(),
		BumpHex: funding.BumpHex(mp), Height: height, BeefHex: treeBeef, Sats: t.Sats,
		Count: uint32(count), Next: 0, Funder: funder}, nil //nolint:gosec // a small configured count
}

// prepare hands a signed tree's record and its fee coin to Prepare, when it
// is set, before the tree reaches the settlement leg.
func (t *Trees) prepare(tree *transaction.Transaction, count int, id, funder string, coin bwallet.Output) error {
	if t.Prepare == nil {
		return nil
	}
	rec := funding.Tree{IdentityKeyHex: id, Txid: tree.TxID().String(), RawHex: tree.Hex(), Sats: t.Sats,
		Count: uint32(count), Funder: funder} //nolint:gosec // a small configured count
	if err := t.Prepare(rec, coin); err != nil {
		return fmt.Errorf("funding tree: prepare: %w", err)
	}
	return nil
}

// adopt records a new tree in the state, then publishes it, and starts the
// next mint ahead when this spend leaves the new tree low.
func (t *Trees) adopt(ctx context.Context, tree *transaction.Transaction, rec funding.Tree, need uint32) (*transaction.Transaction, uint32, error) {
	if err := t.adoptAndPublish(ctx, tree, rec); err != nil {
		return nil, 0, err
	}
	if rec.Count >= need {
		t.mintAhead(ctx, rec.Txid, rec.Count-need)
	}
	return tree, 0, nil
}

// adoptAndPublish records a tree in the state and then publishes it, in
// that order. A failure once the state has the tree is a *PublishError.
func (t *Trees) adoptAndPublish(ctx context.Context, tree *transaction.Transaction, rec funding.Tree) error {
	if err := t.State.Adopt(rec); err != nil {
		return err
	}
	if err := t.publish(ctx, tree); err != nil {
		return &PublishError{Tree: rec, Err: err}
	}
	return nil
}

// Publish publishes a tree the state already holds, on Facade and Topic,
// as Spend publishes a new one. It is how an application repeats the
// publish after a *PublishError, with the error's Tree, and it may be
// called for any adopted tree: the hosts answer a tree they already hold
// as a duplicate, which is not an error. The tree is rebuilt from its
// record (funding.Rebuild), so an unmined tree needs the BEEF it is kept
// as. A failure is again a *PublishError.
func (t *Trees) Publish(ctx context.Context, tree funding.Tree) error {
	if t.Payer == nil {
		return errors.New("producer: Trees needs a Payer")
	}
	tx, err := funding.Rebuild(tree.RawHex, tree.BumpHex, tree.BeefHex)
	if err == nil && tx.TxID().String() != tree.Txid {
		err = fmt.Errorf("the record's bytes are transaction %s", tx.TxID())
	}
	if err != nil {
		return &PublishError{Tree: tree, Err: fmt.Errorf("publish funding tree %s: %w", tree.Txid, err)}
	}
	if err := t.publish(ctx, tx); err != nil {
		return &PublishError{Tree: tree, Err: err}
	}
	return nil
}

// publish submits a tree to the object leg.
func (t *Trees) publish(ctx context.Context, tree *transaction.Transaction) error {
	tb, err := funding.BEEF(tree)
	if err != nil {
		return err
	}
	if t.Facade == nil {
		return errors.New("publish funding tree: no object leg")
	}
	res, err := t.Facade.Submit(ctx, t.Topic, tb)
	if err != nil {
		return fmt.Errorf("publish funding tree: %w", err)
	}
	if !res.Duplicate && len(res.Admitted) == 0 {
		return fmt.Errorf("publish funding tree: the topic manager admitted nothing: %s", string(res.Raw))
	}
	t.Payer.note("funding tree published: admitted %d output(s)", len(res.Admitted))
	return nil
}

// mintAhead starts minting the next tree when left, the outputs a spend
// leaves on the tree curTxid, is at or below Ahead, and no mint is in
// flight, waiting unused, or already tried for that tree.
func (t *Trees) mintAhead(ctx context.Context, curTxid string, left uint32) {
	if t.Ahead == 0 || t.DryRun || left > t.Ahead || t.ahead != nil || len(t.held) > 0 || t.aheadFor == curTxid {
		return
	}
	t.aheadFor = curTxid
	count, id, funder := t.Count, t.Identity, t.Funder
	t.Payer.note("funding tree %s has %d output(s) left, so the next is minted ahead", curTxid, left)
	fail := func(err error) {
		t.Payer.note("the next funding tree was not minted ahead (%v); it is minted when it is needed", err)
	}
	if count > mint.MaxFundingOutputs {
		fail(fmt.Errorf("producer: Count %d: %w", count, mint.ErrTreeTooLarge))
		return
	}
	done := make(chan aheadResult, 1)
	if t.Fund != nil {
		fund := t.Fund
		t.ahead = done
		go func() {
			tree, mp, height, err := fund(ctx, count)
			done <- aheadResult{tx: tree, mp: mp, height: height, count: count, id: id, funder: funder, err: err}
		}()
		return
	}
	// The reservation and the signing happen here, on the goroutine that
	// owns the Payer. Only a proven coin is taken: the tree is settled on its
	// own and carries no kept transaction.
	o, err := t.Payer.Pool.TakeAtLeast(t.Payer.Tip, t.treeNeed(count), nil)
	if err != nil {
		fail(&NoCoinError{Err: err, Held: len(t.Payer.Pool.UnprovenTxids())})
		return
	}
	tree, err := t.mintFrom(ctx, count, o)
	if err != nil {
		_ = t.Payer.Pool.Return(o)
		fail(err)
		return
	}
	t.Payer.note("funding tree %s: %d output(s) of %d sat", tree.TxID(), count, t.Sats)
	if err := t.prepare(tree, count, id, funder, o); err != nil {
		_ = t.Payer.Pool.Return(o)
		fail(err)
		return
	}
	// The background half settles through a copy of the Payer whose notes
	// are held for the collecting goroutine, and whose leg records whether
	// the tree reached it.
	var held []string
	bg := *t.Payer
	bg.spent = nil
	bg.Note = func(format string, args ...any) { held = append(held, fmt.Sprintf(format, args...)) }
	var leg *recordingSettler
	if bg.Settler != nil {
		leg = &recordingSettler{Settler: bg.Settler}
		bg.Settler = leg
	}
	t.ahead = done
	// From here until the mint is collected the coin is out of the pool and
	// its change not yet in it: a take the pool cannot cover waits for it.
	t.minting(t.Payer.Pool)
	go func() {
		mp, height, err := bg.Settle(ctx, "funding tree", tree)
		r := aheadResult{tx: tree, mp: mp, height: height, count: count, id: id, funder: funder, change: true, notes: held, err: err}
		if err != nil && (leg == nil || !leg.submitted) {
			r.coin = &o
			r.by = bg.spentBy(ctx, o, tree, err)
		}
		done <- r
	}()
}

// treeNeed is the least a coin must hold to pay for a tree of count
// outputs: their value and the fee floor. The fee is at least the floor, so
// a smaller coin cannot pay; a coin at or above it may still fall short once
// the tree is signed and measured, and the mint's failure puts it back.
func (t *Trees) treeNeed(count int) uint64 {
	outs := uint64(max(count, 0)) //nolint:gosec // a small configured count
	if t.Sats != 0 && outs > (math.MaxUint64-t.Payer.Fees.Floor)/t.Sats {
		return math.MaxUint64
	}
	return outs*t.Sats + t.Payer.Fees.Floor
}

// mintFrom signs for the coin o and mints a tree of count outputs with it.
func (t *Trees) mintFrom(ctx context.Context, count int, o bwallet.Output) (*transaction.Transaction, error) {
	fee, err := t.Payer.input(ctx, o)
	if err != nil {
		return nil, err
	}
	if t.Change == nil {
		return nil, errors.New("producer: Trees needs a Change script")
	}
	changeTo, err := t.Change()
	if err != nil {
		return nil, err
	}
	return t.mint(ctx, count, fee, changeTo)
}

// recordingSettler records whether a transaction reached the leg, so a fee
// coin is put back only when it did not.
type recordingSettler struct {
	publish.Settler
	submitted bool
}

func (s *recordingSettler) Submit(ctx context.Context, tx *transaction.Transaction) error {
	err := s.Settler.Submit(ctx, tx)
	s.submitted = err == nil
	return err
}

// collect takes the result of the mint in flight, waiting for it when wait
// is set: its held notes are reported, its change is taken into the pool, a
// coin it never spent is put back, or dropped when another transaction
// spent it, and a tree it minted becomes the tree minted ahead. It returns the mint's error, already reported, or ctx's
// while waiting; the mint is still in flight after the latter.
func (t *Trees) collect(ctx context.Context, wait bool) error {
	if t.ahead == nil {
		return nil
	}
	var r aheadResult
	if wait {
		select {
		case r = <-t.ahead:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		select {
		case r = <-t.ahead:
		default:
			return nil
		}
	}
	t.ahead = nil
	if r.change {
		t.minted()
	}
	for _, l := range r.notes {
		t.Payer.note("%s", l)
	}
	switch {
	case r.coin != nil && r.by != "":
		t.Payer.drop(*r.coin, "funding tree", r.tx.TxID().String(), r.by)
	case r.coin != nil:
		_ = t.Payer.Pool.Return(*r.coin)
	}
	if r.err == nil && r.tx == nil {
		r.err = errors.New("the funder returned no tree")
	}
	if r.err == nil {
		if r.change {
			t.Payer.Change(r.tx, r.height, r.mp)
		}
		var rec funding.Tree
		if rec, r.err = t.record(r.tx, r.mp, r.height, r.count, r.id, r.funder); r.err == nil {
			t.hold(r.tx, rec)
			t.Payer.note("funding tree %s is minted ahead and waits for the switch", rec.Txid)
			return nil
		}
	}
	t.Payer.note("the next funding tree was not minted ahead (%v); it is minted when it is needed", r.err)
	return r.err
}

// Wait waits for the mint ahead in flight, if there is one, and collects it
// as Spend does: its notes are reported, its change is taken into the pool,
// and the tree becomes the one Prepared answers. It returns the mint's
// error, which Note has already reported and which Spend recovers from by
// minting when a tree is needed, or ctx's error, which leaves the mint in
// flight. With nothing in flight it returns nil at once.
//
// An application need not call it before it takes a fee: a take the pool
// cannot cover while the mint holds a coin collects the mint itself (see
// Payer.Take). Wait remains how a run that is ending collects the mint, so
// that the change reaches the pool and Prepared answers the tree.
func (t *Trees) Wait(ctx context.Context) error {
	return t.collect(ctx, true)
}

// Prepared returns the record the tree minted ahead will be adopted as, or
// nil when there is none: none was started, it is still in flight (see
// Wait), it failed, or Spend has switched to it. The tree is in no state
// until the switch; an application that wants a sweep to take its outputs
// after a crash keeps this record in its own history. When more than one
// tree is held, which Recover can bring about, it is the first of them,
// and Held answers them all.
func (t *Trees) Prepared() *funding.Tree {
	if len(t.held) == 0 {
		return nil
	}
	rec := t.held[0].rec
	return &rec
}

// Held returns the record of every tree that is settled and waits for a
// switch, in the order they were held: the tree minted ahead, and each
// tree Recover answered TreeHeld for. It is empty when there is none. When
// the current tree cannot cover a spend, Spend switches to the first of
// them that is locked to Identity and large enough, and adopts and
// publishes it then; one too small for that spend keeps its place. No tree
// is minted ahead while one is held.
func (t *Trees) Held() []funding.Tree {
	out := make([]funding.Tree, 0, len(t.held))
	for _, pt := range t.held {
		out = append(out, pt.rec)
	}
	return out
}

// hold puts a settled tree behind the trees already held, once.
func (t *Trees) hold(tx *transaction.Transaction, rec funding.Tree) {
	for _, pt := range t.held {
		if pt.rec.Txid == rec.Txid {
			return
		}
	}
	t.held = append(t.held, &preparedTree{tx: tx, rec: rec})
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
