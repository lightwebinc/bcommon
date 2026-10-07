// Package mint builds the mined transactions an application's state lives
// on: a transition of its state token, a funding tree, and a plain payment,
// each with a fee input and a change output supplied by the caller.
//
// Every lock script arrives as a parameter and every input arrives with the
// template that signs it, so the package knows no application's derivation
// or tags and holds no key material. What it owns is the fee logic: a loop
// that signs to measure the size and rebuilds at the rate until the fee
// covers it.
//
// A carrier is not built here: it is never mined and pays no fee, so package
// carrier builds it.
package mint

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Input is one input the caller funds a transaction with: its source
// transaction, the output index, and the template that signs it. The
// template is what keeps key material out of this package: an embedded
// wallet's funding unlocker signs through wallet.CreateSignature.
type Input struct {
	Tx       *transaction.Transaction
	Vout     uint32
	Unlocker transaction.UnlockingScriptTemplate
}

// Fees is the fee policy: a rate on the signed transaction's size, a floor
// under the fee, the change below which change is dropped, and the bounds
// that keep a bad rate from draining a wallet.
//
// The rate is Rate, satoshis per bytes exactly as miners publish it (ARC's
// miningFee), computed in integer arithmetic and rounded up. SatPerByte is
// the older whole-number form and is read only when Rate is zero, as
// Rate{SatPerByte, 1}; a Fees written before Rate existed means what it
// meant. The fee for a size is ceil(size*Sats/Bytes), at least Floor, with
// the rate first lowered to MaxRate when MaxRate is set; above Max it is
// refused with ErrFeeTooHigh rather than paid.
type Fees struct {
	// SatPerByte is a whole number of satoshis per byte, read only when
	// Rate is zero.
	SatPerByte uint64
	// Floor is the least fee a transaction pays, in satoshis.
	Floor uint64
	// Rate is the fee rate. Bytes must be at least 1 when Sats is set.
	Rate Rate
	// Dust is the least change kept as an output; change below it is
	// added to the fee. Zero means Floor, the rule before Dust existed.
	Dust uint64
	// MaxRate, when set, caps Rate: a rate above it is lowered to it. It
	// bounds what a fee rate from someone else's policy endpoint can cost.
	MaxRate Rate
	// Max, when set, is the most one transaction may pay in fee. A fee
	// above it is refused with ErrFeeTooHigh: overpaying is lost money, so
	// it fails closed.
	Max uint64
}

// DefaultFees is the network's rate, 100 satoshis per 1000 bytes (the
// miningFee GorillaPool's and TAAL's ARC and GorillaPool's arcade publish),
// with a 250 satoshi floor and change under 250 satoshis dropped, as
// before the rate moved.
//
// TODO(D3): the floor and the change-drop threshold stay at 250 until the
// owner rules; NetworkFees is the proposal (floor 1, dust 1).
var DefaultFees = Fees{Rate: Rate{Sats: 100, Bytes: 1000}, Floor: 250, Dust: 250}

// NetworkFees is the network's rate with no padding: 100 satoshis per 1000
// bytes, a floor of one satoshi, and every change of a satoshi or more
// kept. A 225-byte payment pays 23 satoshis.
var NetworkFees = Fees{Rate: Rate{Sats: 100, Bytes: 1000}, Floor: 1, Dust: 1}

// LegacyFees is one satoshi per byte with a 250 satoshi floor: DefaultFees
// until the network rate became the default. A test vector that pins its fee amounts names it, so a
// change of default never moves a pinned byte.
var LegacyFees = Fees{SatPerByte: 1, Floor: 250}

// ErrFeeTooHigh is a fee above Fees.Max.
var ErrFeeTooHigh = errors.New("mint: fee above the per-transaction maximum")

// ErrRate is a rate that cannot be computed: satoshis over zero bytes, or a
// fee that overflows 64 bits.
var ErrRate = errors.New("mint: bad fee rate")

// rate is the rate in force: Rate, or SatPerByte per byte when Rate is
// zero, lowered to MaxRate when that is set.
func (f Fees) rate() (Rate, error) {
	r := f.Rate
	if r.IsZero() {
		r = Rate{Sats: f.SatPerByte, Bytes: 1}
	}
	if r.Bytes == 0 {
		return Rate{}, fmt.Errorf("%w: %d satoshis over zero bytes", ErrRate, r.Sats)
	}
	if !f.MaxRate.IsZero() {
		if f.MaxRate.Bytes == 0 {
			return Rate{}, fmt.Errorf("%w: maximum of %d satoshis over zero bytes", ErrRate, f.MaxRate.Sats)
		}
		if r.Cmp(f.MaxRate) > 0 {
			r = f.MaxRate
		}
	}
	return r, nil
}

// dust is the change threshold in force.
func (f Fees) dust() uint64 {
	if f.Dust == 0 {
		return f.Floor
	}
	return f.Dust
}

// For is the fee a signed transaction of size bytes pays: the rate rounded
// up, at least Floor, and an error above Max.
func (f Fees) For(size int) (uint64, error) {
	if size < 0 {
		return 0, fmt.Errorf("%w: negative size %d", ErrRate, size)
	}
	r, err := f.rate()
	if err != nil {
		return 0, err
	}
	fee, err := r.Fee(uint64(size))
	if err != nil {
		return 0, err
	}
	fee = max(fee, f.Floor)
	if f.Max > 0 && fee > f.Max {
		return 0, fmt.Errorf("%w: %d satoshis for %d bytes, maximum %d", ErrFeeTooHigh, fee, size, f.Max)
	}
	return fee, nil
}

var (
	ErrInsufficient = errors.New("mint: fee input cannot cover the outputs and the fee")
	ErrNoChange     = errors.New("mint: nil change script")
	// ErrTreeTooLarge is a funding tree of more than MaxFundingOutputs
	// funding outputs.
	ErrTreeTooLarge = errors.New("mint: a funding tree holds at most 1023 funding outputs")
)

// MaxFundingOutputs is the most funding outputs one funding tree holds,
// its change outputs aside. A sweep of every funding output of a tree with
// one fee input then spends at most 1024 inputs, the least a host that
// admits a sweep must accept, so the kill switch for a whole tree is one
// transaction every such host takes. Every carrier's BEEF carries its whole
// tree as well, so an application usually wants a tree far smaller.
const MaxFundingOutputs = 1023

// errNoLock refuses a nil lock before the fee loop. The SDK dereferences
// every output's script when it computes a signature hash, so a nil one
// would panic in the first pass rather than return an error.
var errNoLock = errors.New("mint: nil lock script")

// outOfRange reports whether vout names no output of tx. It compares as
// uint64 rather than converting vout to int: on a 32-bit target int(vout) is
// negative from 1<<31 up, passes a >= len check, and the index that follows
// panics.
func outOfRange(vout uint32, tx *transaction.Transaction) bool {
	return uint64(vout) >= uint64(len(tx.Outputs))
}

// Transition builds and signs a state token transaction: one output of sats
// locked with lock.
//
// Inputs: the previous token output when prev is not nil (an update spends
// its predecessor; a create spends none), then the fee input. Outputs: the
// token at index 0, then change. Change below the dust threshold
// (Fees.Dust, the floor when unset) is dropped rather than left as dust.
//
// prev.Unlocker is required. The key that signs the previous token is not
// always the one the new lock names (after a rotation the predecessor signs
// and the successor is locked to), so which one it is stays the caller's
// decision rather than a default here.
func Transition(lock *script.Script, sats uint64, prev *Input, fee Input, change *script.Script, fees Fees) (*transaction.Transaction, error) {
	if lock == nil {
		return nil, errNoLock
	}
	return build(func() (*transaction.Transaction, uint64, error) {
		tx := transaction.NewTransaction()
		var in uint64
		if prev != nil {
			if prev.Tx == nil || outOfRange(prev.Vout, prev.Tx) {
				return nil, 0, errors.New("mint: previous token output out of range")
			}
			if prev.Unlocker == nil {
				return nil, 0, errors.New("mint: previous token output unsigned")
			}
			tx.AddInputFromTx(prev.Tx, prev.Vout, prev.Unlocker)
			in += prev.Tx.Outputs[prev.Vout].Satoshis
		}
		if fee.Tx == nil || outOfRange(fee.Vout, fee.Tx) || fee.Unlocker == nil {
			return nil, 0, errors.New("mint: fee input out of range or unsigned")
		}
		tx.AddInputFromTx(fee.Tx, fee.Vout, fee.Unlocker)
		in += fee.Tx.Outputs[fee.Vout].Satoshis
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
		return tx, in, nil
	}, change, fees)
}

// FundingTree builds and signs a funding tree: count outputs of sats each,
// locked with lock, plus change. Each carrier spends one; a mined spend of
// any of them by the owner (a kill switch) is a transaction the caller
// builds with the unlocker for lock. count is 1 to MaxFundingOutputs; a
// larger one is refused with an error wrapping ErrTreeTooLarge.
func FundingTree(lock *script.Script, count int, sats uint64, fee Input, change *script.Script, fees Fees) (*transaction.Transaction, error) {
	if count < 1 || sats < 1 {
		return nil, errors.New("mint: a funding tree needs at least one output of at least one satoshi")
	}
	if count > MaxFundingOutputs {
		return nil, fmt.Errorf("%w: %d asked", ErrTreeTooLarge, count)
	}
	if lock == nil {
		return nil, errNoLock
	}
	return build(func() (*transaction.Transaction, uint64, error) {
		tx := transaction.NewTransaction()
		if fee.Tx == nil || outOfRange(fee.Vout, fee.Tx) || fee.Unlocker == nil {
			return nil, 0, errors.New("mint: fee input out of range or unsigned")
		}
		tx.AddInputFromTx(fee.Tx, fee.Vout, fee.Unlocker)
		for i := 0; i < count; i++ {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
		}
		return tx, fee.Tx.Outputs[fee.Vout].Satoshis, nil
	}, change, fees)
}

// build runs the fee loop over a skeleton the caller assembles: inputs and
// non-change outputs, returning the input total.
//
// A DER signature is 70 to 72 bytes and every pass re-signs a different
// preimage (the change moved), so the size can grow by a byte per input
// between passes. The fee therefore targets the measured size plus two
// bytes per input, which is the most any pass can add, and the loop accepts
// as soon as the fee covers the size it measured. Six passes is far more
// than it ever needs; the bound exists so a bug cannot spin.
func build(skeleton func() (*transaction.Transaction, uint64, error), change *script.Script, fees Fees) (*transaction.Transaction, error) {
	if change == nil {
		return nil, ErrNoChange
	}
	fee, err := fees.For(0)
	if err != nil {
		return nil, err
	}
	dust := fees.dust()
	for pass := 0; pass < 6; pass++ {
		tx, in, err := skeleton()
		if err != nil {
			return nil, err
		}
		out := tx.TotalOutputSatoshis()
		if in < out+fee {
			return nil, fmt.Errorf("%w: inputs %d, outputs %d, fee %d", ErrInsufficient, in, out, fee)
		}
		if rest := in - out - fee; rest >= dust {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: rest, LockingScript: change})
		}
		if err := tx.Sign(); err != nil {
			return nil, err
		}
		need, err := fees.For(tx.Size())
		if err != nil {
			return nil, err
		}
		if need <= fee {
			return tx, nil
		}
		if fee, err = fees.For(tx.Size() + 2*len(tx.Inputs)); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("mint: fee did not converge")
}

// Payment builds and signs a simple payment: one output of sats to dest,
// change back to the payer. It is the BRC-29 leg: the destination is a key
// derived from the recipient's identity, and the transaction itself is an
// ordinary P2PKH spend.
//
// ctx is unused: nothing here reaches the network, and a template that
// signs through a wallet carries its own context. It is kept so the
// signature stays stable for callers if a later version needs it.
func Payment(ctx context.Context, dest *script.Script, sats uint64, fee Input, change *script.Script, fees Fees) (*transaction.Transaction, error) {
	if dest == nil || sats == 0 {
		return nil, errors.New("mint: a payment needs a destination and an amount")
	}
	_ = ctx
	return build(func() (*transaction.Transaction, uint64, error) {
		tx := transaction.NewTransaction()
		if fee.Tx == nil || outOfRange(fee.Vout, fee.Tx) || fee.Unlocker == nil {
			return nil, 0, errors.New("mint: fee input out of range or unsigned")
		}
		tx.AddInputFromTx(fee.Tx, fee.Vout, fee.Unlocker)
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: dest})
		return tx, fee.Tx.Outputs[fee.Vout].Satoshis, nil
	}, change, fees)
}
