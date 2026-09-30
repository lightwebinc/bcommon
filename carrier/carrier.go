// Package carrier is an object's transport: one transaction that is never
// mined, whose record output holds the object's payload as a signed PushDrop
// [payload] under the producer's derivation, and whose txid is the commitment
// a mined token can carry.
//
// A carrier is kept off the chain by BRC-60's device turned the other way
// round: a far-future nLockTime with a non-final input makes it unmineable in
// practice, while SPV still verifies it through its funding parent. Nothing in
// the SDK's verification path reads finality (verified at v1.5.2), so a host
// admits it like any other object; the topic manager is what refuses one that
// could reach the chain.
//
// The carrier is funded from a funding tree whose outputs are PushDrop [tag]
// with no signature under the same derivation, so the derivation's one
// unlocker spends a funding output whether a carrier or the kill switch
// (Sweep) is the spender.
//
// The application supplies what is its own through Params: the derivation,
// the funding tag, the payload's rules and the sentinel an identity-parse
// failure wraps. Decode takes the application's Classify, which decides
// which PushDrop outputs hold its payload.
package carrier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	hash "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// LockTime is 2100-01-01T00:00:00Z. With an input whose sequence is below the
// maximum, a transaction with this nLockTime is not mineable before then.
// Frozen at the first mint: a reader refuses anything lower.
const LockTime uint32 = 4102444800

// Sequence is the carrier's input sequence. Any value below 0xFFFFFFFF keeps
// the locktime in force; zero is the conventional non-final value.
const Sequence uint32 = 0

// SigHashType is the one sighash type a carrier's input is signed with,
// SIGHASH_ALL|FORKID, as Mint signs it through the derivation's unlocker.
const SigHashType byte = 0x41

// The refusals. Their texts are what readers print, so they do not change.
var (
	ErrNotCarrier = errors.New("carrier: not a carrier")
	ErrShape      = errors.New("carrier: record output has the wrong shape")
	ErrMineable   = errors.New("carrier: mineable; the record could reach the chain")
	ErrLock       = errors.New("carrier: locking key is not the identity's record key")
	ErrSignature  = errors.New("carrier: field signature does not verify")
	// ErrUnlocking is a carrier whose input is not spent by exactly the one
	// canonical signature push CheckUnlocking describes. Anyone who sees a
	// carrier can rewrite a looser unlocking script without the key, and the
	// rewrite is a different txid spending the same funding output.
	ErrUnlocking = errors.New("carrier: unlocking script is not one canonical signature push")
	// ErrIdentity is what an identity key that does not parse, or is not
	// the one canonical compressed encoding (guard.ParsePubKey), wraps when the
	// application names no sentinel of its own in Params.ErrIdentity.
	ErrIdentity = errors.New("carrier: identity key does not parse")
)

// Params are what an application supplies to make the carrier its own.
// Derivation and FundingTag are frozen at the application's first mint: the
// derivation is hashed into every locking key and the funding tag is what
// hosts admit funding outputs by. ValidatePayload and ErrIdentity decide
// what a reader accepts and how it reports a refusal; nothing minted holds
// them.
type Params struct {
	// Derivation locks the record output and the funding outputs.
	Derivation pushdrop.Derivation
	// FundingTag is the one field of a funding output.
	FundingTag []byte
	// ValidatePayload runs first inside Validate, before the finality checks,
	// and its error is returned as is, so a payload that breaks its own rules
	// is refused for them even when the carrier could also be mined. Validate
	// refuses a Params without one rather than accept every payload.
	ValidatePayload func(payload []byte) error
	// ErrIdentity is the sentinel an identity-parse failure wraps; nil means
	// ErrIdentity.
	ErrIdentity error
}

// Classify reports whether a PushDrop's first field is the application's
// payload. (false, _) skips the output, (true, nil) takes it, and (true, err)
// refuses the whole transaction: the output claims to be a payload and is
// not a well-formed one.
type Classify func(payload []byte) (ours bool, err error)

// Carrier is a decoded carrier transaction.
type Carrier struct {
	Tx          *transaction.Transaction
	OutputIndex uint32
	// Payload is the exact bytes pushed, which the field signature covers.
	Payload    []byte
	LockingKey *ec.PublicKey
	Signature  []byte
}

// Commitment is the carrier's txid in hash byte order: SHA-256d over the
// transaction bytes, exactly as a token carries it and as an overlay keys
// the object. It is NOT the display hex reversed.
func Commitment(tx *transaction.Transaction) [32]byte {
	return *tx.TxID()
}

// Decode finds the one record output: a signed PushDrop of one field that
// classify takes. A transaction with no record output is not a carrier; one
// with two is refused as well, because the commitment would then name two
// records at once. A record output the classifier takes that is not the one
// encoding the template writes (pushdrop.CheckCanonical) is refused as
// ErrShape.
func Decode(tx *transaction.Transaction, classify Classify) (*Carrier, error) {
	if classify == nil {
		return nil, errors.New("carrier: nil classifier")
	}
	if tx == nil {
		return nil, fmt.Errorf("%w: nil transaction", ErrNotCarrier)
	}
	var found *Carrier
	for i, out := range tx.Outputs {
		if out == nil || out.LockingScript == nil {
			continue
		}
		d := sdkpushdrop.Decode(out.LockingScript)
		if d == nil || d.LockingPublicKey == nil || len(d.Fields) != 2 {
			continue
		}
		ours, err := classify(d.Fields[0])
		if !ours {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: output %d: %v", ErrShape, i, err)
		}
		if err := pushdrop.CheckCanonical(out.LockingScript); err != nil {
			return nil, fmt.Errorf("%w: output %d: %v", ErrShape, i, err)
		}
		if found != nil {
			return nil, fmt.Errorf("%w: more than one record output", ErrNotCarrier)
		}
		found = &Carrier{
			Tx:          tx,
			OutputIndex: uint32(i), //nolint:gosec // bounded by the output count
			Payload:     d.Fields[0],
			LockingKey:  d.LockingPublicKey,
			Signature:   d.Fields[1],
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: no record output", ErrNotCarrier)
	}
	return found, nil
}

// Validate applies what a carrier must satisfy on its own: the payload's own
// rules, unmineability, the canonical unlocking script (CheckUnlocking), the
// lock derivation from identityKey, and the field signature under that lock. The chain rules that need the previous state
// belong to the verifier and the lookup service, not here.
//
// The order is part of the contract, because a carrier that breaks two rules
// is refused for the first: the payload's rules, then the finality checks,
// then the unlocking script, and only then the identity key. So an invalid payload is refused as such
// even when the carrier could be mined, and a mineable carrier as mineable
// even when its identity key does not parse.
func (c *Carrier) Validate(p Params, identityKey []byte) error {
	if p.ValidatePayload == nil {
		return errors.New("carrier: no payload validator")
	}
	if err := p.ValidatePayload(c.Payload); err != nil {
		return err
	}
	if c.Tx.LockTime < LockTime {
		return fmt.Errorf("%w: nLockTime %d", ErrMineable, c.Tx.LockTime)
	}
	if len(c.Tx.Inputs) == 0 {
		return fmt.Errorf("%w: no inputs", ErrNotCarrier)
	}
	// Every input must be non-final. One final input would be enough for the
	// locktime to still apply under consensus, but a reader that accepted a
	// mix would be relying on the exact rule rather than the safe shape.
	for i, in := range c.Tx.Inputs {
		if in.SequenceNumber == transaction.MaxTxInSequenceNum {
			return fmt.Errorf("%w: input %d is final", ErrMineable, i)
		}
	}
	if err := CheckUnlocking(c.Tx); err != nil {
		return err
	}
	identity, err := guard.ParsePubKey(identityKey)
	if err != nil {
		// An application's own sentinel says only that a field is wrong, so
		// the detail names the field; this package's own already does.
		if p.ErrIdentity != nil {
			return fmt.Errorf("%w: identity key: %v", p.ErrIdentity, err)
		}
		return fmt.Errorf("%w: %v", ErrIdentity, err)
	}
	want, err := p.Derivation.ExpectedLockingKey(identity)
	if err != nil {
		return err
	}
	if !want.IsEqual(c.LockingKey) {
		return ErrLock
	}
	sig, err := ec.ParseDERSignature(c.Signature)
	if err != nil || !sig.Verify(hash.Sha256(c.Payload), c.LockingKey) {
		return ErrSignature
	}
	return nil
}

// CheckUnlocking refuses, as ErrUnlocking, a carrier that does not have
// exactly one input, or whose input's unlocking script is not exactly one
// minimally encoded data push holding a strict DER ECDSA signature (BIP 66)
// with R in [1, n-1] and S in [1, n/2], followed by the one sighash byte
// SigHashType.
//
// The carrier's txid is its commitment, and a host reads a funding output
// spent by another txid as the record retracted. The script interpreter
// accepts a high-S signature, a non-minimal push and an extra push or no-op,
// so without this check anyone who sees a carrier could spend its funding
// output under a new txid carrying the same record and retract it. With it,
// one signature from the key is the only spend a reader takes. The check is
// structural: whether the signature satisfies the funding output is the
// SPV check's (verify.Check).
func CheckUnlocking(tx *transaction.Transaction) error {
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrUnlocking)
	}
	if len(tx.Inputs) != 1 || tx.Inputs[0] == nil {
		return fmt.Errorf("%w: %d inputs, want 1", ErrUnlocking, len(tx.Inputs))
	}
	var s []byte
	if u := tx.Inputs[0].UnlockingScript; u != nil {
		s = *u
	}
	if len(s) == 0 {
		return fmt.Errorf("%w: empty", ErrUnlocking)
	}
	// A signature and its sighash byte are 9 to 73 bytes, which a minimal
	// encoding always pushes with the one-byte opcode that is its length.
	op := s[0]
	switch {
	case op >= script.OpPUSHDATA1 && op <= script.OpPUSHDATA4:
		return fmt.Errorf("%w: not a minimal push", ErrUnlocking)
	case op < 1 || op > 75:
		return fmt.Errorf("%w: opcode 0x%02x is not a signature push", ErrUnlocking, op)
	case len(s) < 1+int(op):
		return fmt.Errorf("%w: truncated push", ErrUnlocking)
	case len(s) > 1+int(op):
		return fmt.Errorf("%w: more than one push or opcode", ErrUnlocking)
	}
	sig := s[1:]
	if h := sig[len(sig)-1]; h != SigHashType {
		return fmt.Errorf("%w: sighash type 0x%02x, want 0x%02x", ErrUnlocking, h, SigHashType)
	}
	if why := strictSignature(sig[:len(sig)-1]); why != "" {
		return fmt.Errorf("%w: %s", ErrUnlocking, why)
	}
	return nil
}

// strictSignature says why der is not a strict DER ECDSA signature under
// BIP 66's rules with R in [1, n-1] and S in [1, n/2], or "" when it is.
func strictSignature(der []byte) string {
	// 0x30 len 0x02 lenR R 0x02 lenS S, each integer at least one byte.
	if len(der) < 8 || len(der) > 72 {
		return "signature length out of range"
	}
	if der[0] != 0x30 || int(der[1]) != len(der)-2 {
		return "signature is not one DER sequence"
	}
	lenR := int(der[3])
	if der[2] != 0x02 || lenR == 0 || 6+lenR > len(der) {
		return "R is not a DER integer"
	}
	lenS := int(der[5+lenR])
	if der[4+lenR] != 0x02 || lenS == 0 || 6+lenR+lenS != len(der) {
		return "S is not a DER integer"
	}
	r, s := der[4:4+lenR], der[6+lenR:]
	for _, v := range []struct {
		name string
		b    []byte
	}{{"R", r}, {"S", s}} {
		if v.b[0]&0x80 != 0 {
			return v.name + " is negative"
		}
		if len(v.b) > 1 && v.b[0] == 0 && v.b[1]&0x80 == 0 {
			return v.name + " is padded"
		}
	}
	n := ec.S256().N
	if R := new(big.Int).SetBytes(r); R.Sign() == 0 || R.Cmp(n) >= 0 {
		return "R out of range"
	}
	S := new(big.Int).SetBytes(s)
	if S.Sign() == 0 {
		return "S is zero"
	}
	if S.Cmp(new(big.Int).Rsh(n, 1)) > 0 {
		return "S is high"
	}
	return ""
}

// Mint builds and signs a carrier for payload, spending output vout of
// funding through the wallet. Output 0 carries the whole input value back
// under the derivation, so the fee is zero and SPV's outputs<=inputs holds.
//
// Mint does not run p.ValidatePayload. The caller built the payload and
// applies its own rules before encoding it; the validator is the reader's
// check on bytes someone else produced.
//
// Only wallet.Interface touches key material: the field signature comes from
// Lock and the input signature from the derivation's unlocker, so a hardware
// or remote wallet mints exactly as an embedded one does.
func Mint(ctx context.Context, w wallet.Interface, originator string, p Params, payload []byte,
	funding *transaction.Transaction, vout uint32) (*transaction.Transaction, error) {
	if w == nil {
		return nil, errors.New("carrier: nil wallet")
	}
	// Compared as uint64, not as int: on a 32-bit target int(vout) is
	// negative from 1<<31 up and would pass the check, and the index below
	// would then panic.
	if funding == nil || uint64(vout) >= uint64(len(funding.Outputs)) {
		return nil, errors.New("carrier: funding output out of range")
	}
	lock, err := p.Derivation.Lock(ctx, w, originator, [][]byte{payload}, true)
	if err != nil {
		return nil, err
	}
	tx := transaction.NewTransaction()
	tx.LockTime = LockTime
	tx.AddInputFromTx(funding, vout, p.Derivation.Unlocker(ctx, w, originator))
	tx.Inputs[0].SequenceNumber = Sequence
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: funding.Outputs[vout].Satoshis, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		return nil, err
	}
	return tx, nil
}

// FundingLock is the script a funding-tree output is locked with: the
// wallet's key under the derivation with the funding tag as its one field
// and no signature, `<key> OP_CHECKSIG <tag> OP_DROP`. The derivation's
// unlocker spends it with a single signature exactly as it would a bare
// pay-to-public-key; the tag is for the host, which admits funding outputs
// by it so that a later spend of one (the kill switch) is something it sees.
func FundingLock(ctx context.Context, w wallet.Interface, originator string, p Params) (*script.Script, error) {
	// An unset tag is refused rather than written: the SDK writes an empty
	// field as OP_0 and reads it back as 0x00, so the output would carry a
	// tag the application never named.
	if len(p.FundingTag) == 0 {
		return nil, errors.New("carrier: empty funding tag")
	}
	return p.Derivation.Lock(ctx, w, originator, [][]byte{p.FundingTag}, false)
}

// DecodeFunding reports whether s is a funding output under tag and returns
// its locking key. Only the one encoding FundingLock writes is a funding
// output (pushdrop.CheckCanonical).
func DecodeFunding(s *script.Script, tag []byte) (*ec.PublicKey, bool) {
	if s == nil {
		return nil, false
	}
	d := sdkpushdrop.Decode(s)
	if d == nil || d.LockingPublicKey == nil || len(d.Fields) != 1 || !bytes.Equal(d.Fields[0], tag) {
		return nil, false
	}
	if pushdrop.CheckCanonical(s) != nil {
		return nil, false
	}
	return d.LockingPublicKey, true
}

// Sweep builds and signs the kill switch: one transaction spending the
// given outputs of a funding tree, whether or not a carrier already spent
// them. Once mined, every carrier that spent one of those outputs is a
// double spend of a consumed output and no host or index will stand behind
// it again.
//
// Output 0 is a TOMBSTONE: a funding-shaped output (FundingLock) carrying
// the swept tree value. It is not there for the satoshis. A host records a
// spend of an admitted output by the spender's ADMITTED outputs, so a sweep
// that admits nothing is seen live but not on restart; a sweep that admits
// its tombstone leaves the kill in storage. With a fee input, the fee
// input's remainder goes to change as output 1.
//
// Sweep runs its own copy of the fee loop because the fee comes out of the
// tombstone when no fee input is given, which no builder in package mint
// does; the rule the loop settles by is the same. Both of its modes, with
// and without a fee input, are pinned by testdata/vectors/transactions-v1.json.
func Sweep(ctx context.Context, w wallet.Interface, originator string, p Params, tree *transaction.Transaction, vouts []uint32,
	fee *transaction.Transaction, feeVout uint32, feeUnlocker transaction.UnlockingScriptTemplate, change *script.Script, feeRate, floor uint64) (*transaction.Transaction, error) {
	if tree == nil || len(vouts) == 0 || change == nil {
		return nil, errors.New("carrier: a sweep needs a tree, outputs and a change script")
	}
	// Checked before the fee loop, which would index a fee output out of
	// range, or sign without a template and leave the fee input unsigned.
	// The indexes are compared as uint64 for the reason Mint gives.
	if fee != nil && (uint64(feeVout) >= uint64(len(fee.Outputs)) || feeUnlocker == nil) {
		return nil, errors.New("carrier: fee input out of range or unsigned")
	}
	tombstone, err := FundingLock(ctx, w, originator, p)
	if err != nil {
		return nil, err
	}
	build := func(feeSats uint64) (*transaction.Transaction, error) {
		tx := transaction.NewTransaction()
		var swept, feeIn uint64
		for _, v := range vouts {
			if uint64(v) >= uint64(len(tree.Outputs)) {
				return nil, fmt.Errorf("carrier: sweep output %d out of range", v)
			}
			tx.AddInputFromTx(tree, v, p.Derivation.Unlocker(ctx, w, originator))
			swept += tree.Outputs[v].Satoshis
		}
		if fee != nil {
			tx.AddInputFromTx(fee, feeVout, feeUnlocker)
			feeIn = fee.Outputs[feeVout].Satoshis
		}
		switch {
		case fee == nil:
			// The tree pays for its own sweep; the tombstone carries the rest.
			if swept <= feeSats {
				return nil, fmt.Errorf("carrier: sweep inputs %d cannot pay fee %d", swept, feeSats)
			}
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: swept - feeSats, LockingScript: tombstone})
		default:
			if feeIn <= feeSats {
				return nil, fmt.Errorf("carrier: fee input %d cannot pay fee %d", feeIn, feeSats)
			}
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: swept, LockingScript: tombstone})
			if rest := feeIn - feeSats; rest >= floor {
				tx.AddOutput(&transaction.TransactionOutput{Satoshis: rest, LockingScript: change})
			}
		}
		return tx, tx.Sign()
	}
	feeSats := floor
	for pass := 0; pass < 6; pass++ {
		tx, err := build(feeSats)
		if err != nil {
			return nil, err
		}
		need := uint64(tx.Size()) * feeRate //nolint:gosec // serialised length
		if need < floor {
			need = floor
		}
		if need <= feeSats {
			return tx, nil
		}
		feeSats = (uint64(tx.Size()) + 2*uint64(len(tx.Inputs))) * feeRate //nolint:gosec // serialised length
		if feeSats < floor {
			feeSats = floor
		}
	}
	return nil, errors.New("carrier: sweep fee did not converge")
}
