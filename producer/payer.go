package producer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
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
// pool" from "wait for a block".
type NoCoinError struct {
	Err  error
	Held int
}

func (e *NoCoinError) Error() string {
	if e.Held > 0 {
		return fmt.Sprintf("fee input: %v: the other coins are change from %d transaction(s) whose proofs have not arrived; they become spendable once mined and collected", e.Err, e.Held)
	}
	return "fee input: " + e.Err.Error()
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
// the pool by GiveBack if the transaction is not sent.
//
// A received payment carries its derivation, and its owner's key re-derives
// the spending key. Any other coin is spent by whichever key's fund script
// its locking script is; after a rotation the pool holds more than one.
//
// A failure to find a coin is a *NoCoinError and a coin no key holds is a
// *NoKeyError, each returned as it is, so the application can put its own
// words to them. Every other error is returned with the coin's outpoint.
func (p *Payer) Take(ctx context.Context) (mint.Input, error) {
	var allow []string
	if p.Allow != nil {
		allow = p.Allow()
	}
	o, err := p.Pool.TakeAllowing(p.Tip, allow)
	if err != nil {
		return mint.Input{}, &NoCoinError{Err: err, Held: len(p.Pool.UnprovenTxids())}
	}
	p.spent = append(p.spent, o)
	return p.input(ctx, o)
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

// GiveBack returns every coin Take reserved to the pool.
func (p *Payer) GiveBack() {
	for _, o := range p.spent {
		_ = p.Pool.Return(o)
	}
	p.spent = nil
}

// Parent rebuilds enough of a pool coin's parent transaction to sign
// against it, and for a spender published before it mines, enough to carry
// in its BEEF.
//
// Unproven change is only ever taken when its parent is kept (Allow), and
// then the parent is the kept object itself, ancestry and all. A coin whose
// parent the pool holds with its proof is rebuilt from both. With Async, a
// parent the pool holds without a proof, or does not hold at all, is fetched
// from the node with its proof, because the spender is published before it
// mines and its BEEF must carry a parent that verifies on its own. Otherwise
// a parent the pool does not hold (a coinbase) is a stub carrying just the
// coin's output, which is enough to sign against when the spender is mined
// before it is published, since a mined transaction's BEEF carries no
// ancestry.
func (p *Payer) Parent(ctx context.Context, o bwallet.Output) (*transaction.Transaction, error) {
	if o.Unproven {
		return p.Kept.Tx(o.TxID)
	}
	if o.Raw != "" {
		tx, err := transaction.NewTransactionFromHex(o.Raw)
		if err != nil {
			return nil, fmt.Errorf("wallet output %s: %w", o.Outpoint(), err)
		}
		if o.Bump != "" {
			if tx.MerklePath, err = transaction.NewMerklePathFromHex(o.Bump); err != nil {
				return nil, fmt.Errorf("wallet output %s proof: %w", o.Outpoint(), err)
			}
			return tx, nil
		}
		if !p.Async {
			return tx, nil
		}
	}
	if p.Async && p.Asset != nil {
		raw, err := p.Asset.TxRaw(ctx, o.TxID)
		if err != nil {
			return nil, fmt.Errorf("fee input %s: fetching its parent, which an unmined spender must carry: %w", o.Outpoint(), err)
		}
		tx, err := transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("fee input %s: parent does not parse: %w", o.Outpoint(), err)
		}
		if tx.TxID().String() != o.TxID {
			return nil, fmt.Errorf("fee input %s: the node answered transaction %s", o.Outpoint(), tx.TxID())
		}
		mp, _, err := p.Asset.Proof(ctx, o.TxID)
		if err != nil {
			return nil, fmt.Errorf("fee input %s: its parent's proof, which an unmined spender must carry: %w", o.Outpoint(), err)
		}
		tx.MerklePath = mp
		return tx, nil
	}
	tx, err := transaction.NewTransactionFromHex(o.Raw)
	if err != nil || o.Raw == "" {
		tx = transaction.NewTransaction()
		for i := uint32(0); i <= o.Vout; i++ {
			tx.AddOutput(&transaction.TransactionOutput{})
		}
		ls, err := script.NewFromHex(o.LockingScript)
		if err != nil {
			return nil, err
		}
		tx.Outputs[o.Vout] = &transaction.TransactionOutput{Satoshis: o.Satoshis, LockingScript: ls}
		h, err := hashFromHex(o.TxID)
		if err != nil {
			return nil, err
		}
		tx.SetTxHash(h)
	}
	return tx, nil
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
		if slices.Contains(scripts, out.LockingScript.String()) {
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
func (p *Payer) Settle(ctx context.Context, what string, tx *transaction.Transaction) (*transaction.MerklePath, uint32, error) {
	if !p.Async {
		return p.SettleAndWait(ctx, what, tx)
	}
	if p.Settler == nil {
		return nil, 0, fmt.Errorf("%s: settle: no settlement leg", what)
	}
	p.note("%s %s: broadcasting via %s (%d bytes)", what, tx.TxID(), p.Settler.Name(), tx.Size())
	if err := p.Settler.Submit(ctx, tx); err != nil {
		return nil, 0, fmt.Errorf("%s: settle: %w", what, err)
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
		return nil, 0, fmt.Errorf("%s: settle: %w", what, err)
	}
	return p.Await(ctx, what, tx)
}

// Await waits, up to Timeout, for tx to mine, asking Asset every Poll, and
// gives tx its proof. It is the waiting half of SettleAndWait, for a
// transaction that reached the network some other way, such as a wallet's
// own broadcast.
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
	mp, height, err := nodeapi.WaitMined(wctx, p.Asset, tx.TxID().String(), poll)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: waiting for a proof: %w", what, err)
	}
	p.note("%s %s: mined at height %d", what, tx.TxID(), height)
	tx.MerklePath = mp
	return mp, height, nil
}
