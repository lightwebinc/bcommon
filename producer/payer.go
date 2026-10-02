package producer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// DefaultTimeout is how long a Payer waits for a proof when Timeout is zero,
// and DefaultPoll how often it asks when Poll is zero.
const (
	DefaultTimeout = 10 * time.Minute
	DefaultPoll    = 5 * time.Second
)

// Payer pays for a producer's mined transactions from a coin pool and gets
// them mined.
//
// Take reserves a coin and returns it as a fee input, signed by the key its
// output is locked to; GiveBack returns every coin taken when the
// transaction is not sent after all. Change takes a transaction's outputs
// that pay one of the Payer's keys back into the pool. Settle, SettleAndWait
// and Await put a transaction on the settlement leg and collect its proof.
type Payer struct {
	// Pool is the coin every fee comes from.
	Pool *bwallet.Pool
	// Tip is the chain height coinbase maturity is judged at.
	Tip uint32
	// Keys is every key the producer signs with, by identity key hex: a coin
	// locked to one of their fund keys is spent under that key, and change
	// paid to any of them is taken back into Pool. A producer that has
	// rotated its identity holds its predecessors here too.
	Keys map[string]*bwallet.Signer
	// KeyFor returns the key of the identity that owns a received payment,
	// by identity key hex, which re-derives that payment's key. Nil looks
	// the identity up in Keys.
	KeyFor func(identityHex string) (*bwallet.Signer, error)
	// Kept is the producer's kept transactions. A coin that is change from
	// one of them is spent against that same object.
	Kept *Kept
	// Allow returns the kept transactions whose unproven change Take may
	// spend when no proven coin is left: those the transaction being built
	// already carries in full (see bwallet.Pool.TakeAllowing). Nil allows
	// none.
	Allow func() []string
	// Settler is the settlement leg.
	Settler publish.Settler
	// Asset is the node proofs are waited for from and, with Async, where a
	// coin's parent comes from when the pool does not hold it.
	Asset *nodeapi.Asset
	// Async settles without waiting for a block: Settle returns once the
	// leg has accepted a transaction, and the proof is collected later
	// (Collector). A transaction spending a coin before it mines then
	// carries the coin's parent, with its proof, fetched from Asset.
	Async bool
	// Fees is the fee policy of what the Payer mints itself (a funding tree
	// through Trees). mint.DefaultFees is the usual one; the zero value
	// pays no fee, which a network refuses.
	Fees mint.Fees
	// Timeout bounds a wait for a proof; zero is DefaultTimeout. Poll is how
	// often the wait asks; zero is DefaultPoll.
	Timeout time.Duration
	Poll    time.Duration
	// Note receives each line the Payer reports: what it settled and how,
	// and a key whose change it could not recognise. Nil discards them.
	Note func(format string, args ...any)

	spent []bwallet.Output
}

func (p *Payer) note(format string, args ...any) {
	if p.Note != nil {
		p.Note(format, args...)
	}
}

// NoCoinError is Take finding no coin it may spend. Err is the pool's
// answer, usually bwallet.ErrNoSpendable. Held counts the transactions whose
// change the pool holds back until their proofs arrive: coin that becomes
// spendable once those are collected, so an application can tell "fund the
// pool" from "wait for a block". Minting counts the funding trees minted
// ahead (Trees.Ahead) that took a coin of this pool and had not settled
// when Take stopped waiting for them: coin that returns as change once one
// settles, so the same take can succeed later in the run. It is zero unless
// the wait ended first, on the Payer's Timeout or the context; see Take.
type NoCoinError struct {
	Err     error
	Held    int
	Minting int
}

func (e *NoCoinError) Error() string {
	msg := "fee input: " + e.Err.Error()
	if e.Held > 0 {
		msg = fmt.Sprintf("fee input: %v: the other coins are change from %d transaction(s) whose proofs have not arrived; they become spendable once mined and collected", e.Err, e.Held)
	}
	if e.Minting > 0 {
		msg += fmt.Sprintf("; %d funding tree(s) minted ahead hold a coin and have not settled, and the change returns once one does", e.Minting)
	}
	return msg
}

func (e *NoCoinError) Unwrap() error { return e.Err }

// NoKeyError is a pool coin locked to a fund key that none of the Payer's
// keys derives. Outpoint names it, txid.vout.
type NoKeyError struct {
	Outpoint string
}

func (e *NoKeyError) Error() string {
	return "wallet output " + e.Outpoint + " is locked to a key none of the payer's keys holds"
}

// Take reserves a pool coin as a fee input. Everything taken is returned to
// the pool by GiveBack if the transaction is not sent. Take is
// TakeAtLeast(ctx, p.Fees.Floor): a coin below the fee floor cannot pay for
// any transaction, so it is left in the pool.
//
// A received payment carries its derivation, and its owner's key re-derives
// the spending key. Any other coin is spent by whichever key's fund script
// its locking script is; after a rotation the pool holds more than one.
//
// A failure to find a coin is a *NoCoinError and a coin no key holds is a
// *NoKeyError, each returned as it is, so the application can put its own
// words to them. Every other error is returned with the coin's outpoint.
//
// A pool that has a coin for the take answers at once. When it has none,
// and a funding tree minted ahead (Trees.Ahead) took a coin of this same
// Pool and has not been collected, the coin the take needs may be that
// tree's change, which reaches the pool only when the mint is collected. A
// wallet with one coin is in that state from the moment a tree is minted
// ahead. Take then waits for the mint and collects it, exactly as
// Trees.Wait does (its notes go to the Note of that Trees' Payer, its
// change into the pool, or its unspent coin back), and takes again. The
// wait ends with the mint, with ctx, or after this Payer's Timeout
// (DefaultTimeout when zero), whichever is first; with several such mints,
// by several Trees over one pool, they are collected one at a time under
// that one bound, and the take is tried again after each. The mint itself
// runs under the context of the Spend that started it and, without Async,
// ends within the Timeout of that Trees' Payer.
//
// What the collected mint leaves decides the second take. A tree that
// settled with its proof leaves proven change, which is taken. With Async
// on the Trees' Payer the change is unproven until its block, so the take
// answers a *NoCoinError whose Held counts it, unless Allow lets it
// through: the same "wait for a block" an application already handles. A
// mint that failed before the settlement leg returns its coin, which is
// taken; one that failed after it leaves nothing, and the take answers a
// *NoCoinError. When the wait ends first, the mint is still in flight and
// the *NoCoinError counts it in Minting.
//
// Nothing else waits: not a take the pool can cover, not a mint ahead
// looking for its own coin, which is skipped with a note when there is
// none, and not a tree paid through Trees.Fund, which takes no pool coin.
// Take collects the mint on the calling goroutine, so it belongs to the
// goroutine that owns the Trees, as every use of a Payer and a Trees over
// one pool does (see the package documentation).
func (p *Payer) Take(ctx context.Context) (mint.Input, error) {
	return p.TakeAtLeast(ctx, p.Fees.Floor)
}

// TakeAtLeast is Take for a caller that knows what the coin must pay, the
// outputs it funds and the fee: only a coin of at least sats satoshis is
// taken (bwallet.Pool.TakeAtLeast), and a pool whose spendable coins are all
// smaller answers a *NoCoinError. A coin that is taken but cannot be signed
// for is back in the pool before the error is returned. It waits for a
// funding tree minted ahead as Take does.
func (p *Payer) TakeAtLeast(ctx context.Context, sats uint64) (mint.Input, error) {
	in, _, err := p.take(ctx, sats)
	return in, err
}

// take is TakeAtLeast, also answering the coin, which Trees needs to give
// back or release on its own.
func (p *Payer) take(ctx context.Context, sats uint64) (mint.Input, bwallet.Output, error) {
	var allow []string
	if p.Allow != nil {
		allow = p.Allow()
	}
	o, err := p.Pool.TakeAtLeast(p.Tip, sats, allow)
	minting := 0
	if errors.Is(err, bwallet.ErrNoSpendable) {
		o, minting, err = p.takeAfterMints(ctx, sats, allow, err)
	}
	if err != nil {
		return mint.Input{}, bwallet.Output{}, &NoCoinError{Err: err, Held: len(p.Pool.UnprovenTxids()), Minting: minting}
	}
	p.spent = append(p.spent, o)
	in, err := p.input(ctx, o)
	if err != nil {
		p.giveBackOne(o)
		return mint.Input{}, bwallet.Output{}, err
	}
	return in, o, nil
}

// input signs for a coin already taken from the pool: the half of Take after
// the reservation, which Trees also uses for a tree it mints ahead.
func (p *Payer) input(ctx context.Context, o bwallet.Output) (mint.Input, error) {
	if o.Derivation != nil {
		owner, err := p.keyFor(o.Derivation.OwnerHex)
		if err != nil {
			return mint.Input{}, err
		}
		tx, err := p.Parent(ctx, o)
		if err != nil {
			return mint.Input{}, err
		}
		return mint.Input{Tx: tx, Vout: o.Vout, Unlocker: owner.DerivedUnlocker(o.Derivation)}, nil
	}
	var payer *bwallet.Signer
	for _, w := range p.Keys {
		fs, err := w.FundScript()
		if err != nil {
			return mint.Input{}, err
		}
		if fs.String() == o.LockingScript {
			payer = w
			break
		}
	}
	if payer == nil {
		return mint.Input{}, &NoKeyError{Outpoint: o.Outpoint()}
	}
	tx, err := p.Parent(ctx, o)
	if err != nil {
		return mint.Input{}, err
	}
	return mint.Input{Tx: tx, Vout: o.Vout, Unlocker: payer.FundUnlocker()}, nil
}

func (p *Payer) keyFor(id string) (*bwallet.Signer, error) {
	if p.KeyFor != nil {
		return p.KeyFor(id)
	}
	if w, ok := p.Keys[id]; ok {
		return w, nil
	}
	return nil, fmt.Errorf("producer: no key held for identity %q", id)
}

// giveBackOne returns one reserved coin to the pool and forgets it.
func (p *Payer) giveBackOne(o bwallet.Output) {
	_ = p.Pool.Return(o)
	p.release(o)
}

// release forgets a reserved coin without returning it: the transaction it
// paid for reached the settlement leg, so the coin is spent, and a later
// GiveBack must not put it back in the pool.
func (p *Payer) release(o bwallet.Output) {
	p.spent = slices.DeleteFunc(p.spent, func(s bwallet.Output) bool { return s.Outpoint() == o.Outpoint() })
}

// spentBy names the transaction, other than tx, that spent coin, which tx
// spends too: the one the refusal err names for that outpoint (a
// *nodeapi.SpentError), or the one Asset, when set, shows as its spender.
// It is "" when neither says so: the coin is unspent, or spent by tx
// itself, or the node cannot answer for it. Like
// nodeapi.Asset.SpentElsewhere, it speaks only on a positive word.
func (p *Payer) spentBy(ctx context.Context, coin bwallet.Output, tx *transaction.Transaction, err error) string {
	var se *nodeapi.SpentError
	if errors.As(err, &se) && se.By != "" && strings.EqualFold(se.Outpoint, coin.Outpoint()) {
		return se.By
	}
	if p.Asset == nil {
		return ""
	}
	by, aerr := p.Asset.Spender(ctx, coin.TxID, coin.Vout)
	if aerr != nil || by == "" || (tx != nil && strings.EqualFold(by, tx.TxID().String())) {
		return ""
	}
	return by
}

// drop forgets a reserved coin that the transaction by spent, and takes it
// out of the pool should it be there: a spent coin handed out again only
// fails the next build. what and txid name the transaction it was to pay
// for.
func (p *Payer) drop(coin bwallet.Output, what, txid, by string) {
	p.release(coin)
	if _, err := p.Pool.Remove(coin); err != nil {
		p.note("%s %s: its fee coin %s is spent by %s and could not be taken out of the pool: %v", what, txid, coin.Outpoint(), by, err)
		return
	}
	p.note("%s %s: its fee coin %s is spent by %s, so it is out of the pool and is not returned", what, txid, coin.Outpoint(), by)
}

// giveBackUnspent settles a reserved coin whose transaction tx the
// settlement leg did not take, for the reason err: a coin another
// transaction spent is dropped (see spentBy), and any other goes back to
// the pool.
func (p *Payer) giveBackUnspent(ctx context.Context, coin bwallet.Output, what string, tx *transaction.Transaction, err error) {
	if by := p.spentBy(ctx, coin, tx, err); by != "" {
		p.drop(coin, what, tx.TxID().String(), by)
		return
	}
	p.giveBackOne(coin)
}

// dropSpent is what Settle, SettleAndWait and Await do about a refusal of
// tx: each coin Take reserved that tx spends, and that another transaction
// spent (see spentBy), is dropped, so a GiveBack after the refusal does not
// put a spent coin back in the pool.
func (p *Payer) dropSpent(ctx context.Context, what string, tx *transaction.Transaction, err error) {
	if tx == nil {
		return
	}
	for _, o := range slices.Clone(p.spent) {
		for _, in := range tx.Inputs {
			if in.SourceTXID == nil || in.SourceTxOutIndex != o.Vout || !strings.EqualFold(in.SourceTXID.String(), o.TxID) {
				continue
			}
			if by := p.spentBy(ctx, o, tx, err); by != "" {
				p.drop(o, what, tx.TxID().String(), by)
			}
			break
		}
	}
}

// GiveBack returns every coin Take reserved to the pool, except one that
// paid for a funding tree Trees put on the settlement leg, which is spent,
// and one that a refusal at Settle, SettleAndWait or Await showed spent by
// another transaction, which is taken out of the pool instead (see Settle).
func (p *Payer) GiveBack() {
	for _, o := range p.spent {
		_ = p.Pool.Return(o)
	}
	p.spent = nil
}

// Parent rebuilds enough of a pool coin's parent transaction to sign
// against it, and for a spender kept or published before it mines, enough
// to carry in its BEEF.
//
// Unproven change is only ever taken when its parent is kept (Allow), and
// then the parent is the kept object itself, ancestry and all. A coin whose
// parent the pool holds with its proof is rebuilt from both. With Async, a
// parent the pool holds without a proof is fetched from Asset with its
// proof, because the spender is published before it mines and its BEEF must
// carry a parent that verifies on its own; without Async the pool's copy is
// enough, since the spender is mined before it is published.
//
// A parent the pool does not hold at all (a coinbase) is fetched from Asset
// with its proof, Async or not: an application may keep the spender as BEEF
// before it mines (funding.KeepBEEF), and that BEEF must carry the real
// parent. Only with no Asset is it a placeholder carrying just the coin's
// output under the coin's txid. A placeholder is enough to sign against but
// is not a transaction, and funding.BEEF refuses to write one
// (funding.ErrPlaceholder), so its spender must mine before its BEEF is
// built.
func (p *Payer) Parent(ctx context.Context, o bwallet.Output) (*transaction.Transaction, error) {
	if o.Unproven {
		return p.Kept.Tx(o.TxID)
	}
	if o.Raw != "" {
		tx, err := rawTx(o.Raw)
		if err != nil {
			return nil, fmt.Errorf("wallet output %s: %w", o.Outpoint(), err)
		}
		if o.Bump != "" {
			bump, err := hex.DecodeString(o.Bump)
			if err == nil {
				tx.MerklePath, err = guard.ParseBUMP(bump, guard.DefaultBound)
			}
			if err != nil {
				return nil, fmt.Errorf("wallet output %s proof: %w", o.Outpoint(), err)
			}
			return tx, nil
		}
		if !p.Async || p.Asset == nil {
			return tx, nil
		}
	}
	if p.Asset != nil {
		return p.fetchParent(ctx, o)
	}
	return placeholder(o)
}

// fetchParent is a coin's parent as the node serves it, held to the coin's
// txid, with its proof.
func (p *Payer) fetchParent(ctx context.Context, o bwallet.Output) (*transaction.Transaction, error) {
	raw, err := p.Asset.TxRaw(ctx, o.TxID)
	if err != nil {
		return nil, fmt.Errorf("fee input %s: fetching its parent, which a spender kept or published before it mines must carry: %w", o.Outpoint(), err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		return nil, fmt.Errorf("fee input %s: parent does not parse: %w", o.Outpoint(), err)
	}
	if tx.TxID().String() != o.TxID {
		return nil, fmt.Errorf("fee input %s: the node answered transaction %s", o.Outpoint(), tx.TxID())
	}
	mp, _, err := p.Asset.Proof(ctx, o.TxID)
	if err != nil {
		return nil, fmt.Errorf("fee input %s: its parent's proof, which a spender kept or published before it mines must carry: %w", o.Outpoint(), err)
	}
	tx.MerklePath = mp
	return tx, nil
}

// placeholder stands in for a parent whose bytes are not held and cannot be
// fetched: the coin's output at its index, under the coin's txid, and no
// inputs, which is how funding.BEEF knows it and refuses to write it.
func placeholder(o bwallet.Output) (*transaction.Transaction, error) {
	ls, err := script.NewFromHex(o.LockingScript)
	if err != nil {
		return nil, err
	}
	h, err := hashFromHex(o.TxID)
	if err != nil {
		return nil, err
	}
	tx := transaction.NewTransaction()
	for i := uint32(0); i < o.Vout; i++ {
		tx.AddOutput(&transaction.TransactionOutput{})
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: o.Satoshis, LockingScript: ls})
	tx.SetTxHash(h)
	return tx, nil
}

// rawTx parses a coin's parent as the pool holds it, through the guard.
func rawTx(s string) (*transaction.Transaction, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return guard.ParseTransaction(b, guard.DefaultBound)
}

// hashFromHex reads a txid in display order: the hex is byte-reversed
// relative to the hash, and must be exactly 32 bytes.
func hashFromHex(s string) (*chainhash.Hash, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return chainhash.NewHash(b)
}

// Change adds tx's outputs that pay one of the Payer's keys to the pool.
// The parent is kept whole, with its proof when it has one, so the coin can
// be a fee input for a transaction published before it mines. Change from a
// transaction that has not mined (mp nil) is held back from spending until
// its proof is collected; see bwallet.Output.Unproven.
//
// A key with no usable wallet profile has no fund script to recognise its
// change by, and change left out of the pool reads exactly like spent coin,
// so that key is named in a note rather than passed over in silence.
func (p *Payer) Change(tx *transaction.Transaction, height uint32, mp *transaction.MerklePath) {
	p.change(tx, height, mp, nil)
}

// change is Change, leaving out an output keep refuses. A nil keep takes
// every output that pays one of the keys.
func (p *Payer) change(tx *transaction.Transaction, height uint32, mp *transaction.MerklePath, keep func(vout uint32) bool) {
	var scripts []string
	for id, w := range p.Keys {
		fs, err := w.FundScript()
		if errors.Is(err, bwallet.ErrProfile) {
			p.note("WARNING: wallet %s has no usable fund profile (%v); change paid to it is not added to the pool and will read as spent", id, err)
			continue
		}
		if err == nil {
			scripts = append(scripts, fs.String())
		}
	}
	for i, out := range tx.Outputs {
		if slices.Contains(scripts, out.LockingScript.String()) && (keep == nil || keep(uint32(i))) { //nolint:gosec // output index
			_, _ = p.Pool.Add(bwallet.Output{TxID: tx.TxID().String(), Vout: uint32(i), Satoshis: out.Satoshis, //nolint:gosec // output index
				LockingScript: out.LockingScript.String(), Height: height,
				Raw: tx.Hex(), Bump: funding.BumpHex(mp), Unproven: mp == nil})
		}
	}
}

// Settle hands tx to the settlement leg. It waits for the proof, as
// SettleAndWait does, unless Async is set; then it returns once the leg has
// accepted tx, with no proof, and the proof is collected later. what names
// tx in the notes and errors ("funding tree").
//
// With Asset set, a transaction one of whose inputs the node shows spent by
// another transaction is refused, whatever the leg answered: the error wraps
// ErrRefused and a *nodeapi.SpentError (errors.Is nodeapi.ErrDoubleSpent).
//
// A transaction that is not settled leaves its fee coin reserved, for the
// caller's GiveBack to return. A coin another transaction spent must not
// go back, since the pool would hand it out again to fail the next build.
// So when the leg refuses tx at Submit, or the node's view refuses it
// afterwards, each coin this Payer's Take reserved that tx spends is asked
// about: one the refusal names another spender for (a *nodeapi.SpentError
// for its outpoint), or that Asset, when set, shows spent by a transaction
// other than tx, is forgotten and taken out of the pool
// (bwallet.Pool.Remove), with a note, and the GiveBack that follows
// returns only the rest. A coin the node shows unspent, or spent by tx
// itself, or cannot answer for, stays reserved and is returned as before.
// SettleAndWait and Await do the same, Await when its wait ends on such a
// refusal. Trees does it for a funding tree's coin: see Trees.Spend.
func (p *Payer) Settle(ctx context.Context, what string, tx *transaction.Transaction) (*transaction.MerklePath, uint32, error) {
	if !p.Async {
		return p.SettleAndWait(ctx, what, tx)
	}
	if p.Settler == nil {
		return nil, 0, fmt.Errorf("%s: settle: no settlement leg", what)
	}
	p.note("%s %s: broadcasting via %s (%d bytes)", what, tx.TxID(), p.Settler.Name(), tx.Size())
	if err := p.Settler.Submit(ctx, tx); err != nil {
		p.dropSpent(ctx, what, tx, err)
		return nil, 0, fmt.Errorf("%s: settle: %w", what, err)
	}
	// The leg's acceptance is not the node's view: arcade has accepted a
	// transaction whose input was already spent and mined, and the tcp
	// ingress answers nothing.
	if err := p.Asset.SpentElsewhere(ctx, tx); err != nil {
		p.dropSpent(ctx, what, tx, err)
		return nil, 0, fmt.Errorf("%s: settle: %w: %w", what, ErrRefused, err)
	}
	p.note("%s %s: accepted; its proof is collected later", what, tx.TxID())
	return nil, 0, nil
}

// SettleAndWait hands tx to the settlement leg and waits for its proof,
// whatever Async says: for a transaction that must be mined before anything
// else happens, such as a kill switch's sweep.
func (p *Payer) SettleAndWait(ctx context.Context, what string, tx *transaction.Transaction) (*transaction.MerklePath, uint32, error) {
	if p.Settler == nil {
		return nil, 0, fmt.Errorf("%s: settle: no settlement leg", what)
	}
	p.note("%s %s: settling via %s (%d bytes)", what, tx.TxID(), p.Settler.Name(), tx.Size())
	if err := p.Settler.Submit(ctx, tx); err != nil {
		p.dropSpent(ctx, what, tx, err)
		return nil, 0, fmt.Errorf("%s: settle: %w", what, err)
	}
	return p.Await(ctx, what, tx)
}

// Await waits, up to Timeout, for tx to mine, asking Asset every Poll, and
// gives tx its proof. It is the waiting half of SettleAndWait, for a
// transaction that reached the network some other way, such as a wallet's
// own broadcast.
//
// Each poll also asks Asset whether one of tx's inputs is spent by another
// transaction (nodeapi.WaitSettled). Such a transaction never mines, so
// Await returns at once with an error wrapping ErrRefused and a
// *nodeapi.SpentError rather than waiting out Timeout.
func (p *Payer) Await(ctx context.Context, what string, tx *transaction.Transaction) (*transaction.MerklePath, uint32, error) {
	if p.Asset == nil {
		return nil, 0, fmt.Errorf("%s: waiting for a proof: no node to ask", what)
	}
	timeout, poll := p.Timeout, p.Poll
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mp, height, err := nodeapi.WaitSettled(wctx, p.Asset, tx, poll)
	if errors.Is(err, nodeapi.ErrDoubleSpent) {
		p.dropSpent(ctx, what, tx, err)
		return nil, 0, fmt.Errorf("%s: %w: %w", what, ErrRefused, err)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("%s: waiting for a proof: %w", what, err)
	}
	p.note("%s %s: mined at height %d", what, tx.TxID(), height)
	tx.MerklePath = mp
	return mp, height, nil
}
