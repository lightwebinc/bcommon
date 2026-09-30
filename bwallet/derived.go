package bwallet

import (
	"context"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/guard"
)

// PaymentProtocol is BRC-29's protocol: security level 2, the fixed name.
// The key id is "<derivationPrefix> <derivationSuffix>", so the invoice
// number is "2-3241645161d8-<prefix> <suffix>".
//
// It is a variable only because Go has no constant of a struct type. Callers
// read it and must not assign to it: PaymentDestination derives every payment
// destination in the process under it, so an assignment would send every
// payment to a key the recipient, deriving under BRC-29's protocol, would not
// find. DerivedKey, DerivedScript and DerivedUnlocker take the protocol from
// the Derivation instead, so they would not follow the change.
var PaymentProtocol = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "3241645161d8"}

// PaymentKeyID is BRC-29's key id for one output.
func PaymentKeyID(prefix, suffix string) string { return prefix + " " + suffix }

// Counterparty parses a compressed identity key into a counterparty. The key
// is often a sender's, taken from a payment, so only its one canonical
// encoding is accepted (guard.ParsePubKeyHex).
func Counterparty(idHex string) (wallet.Counterparty, error) {
	pub, err := guard.ParsePubKeyHex(idHex)
	if err != nil {
		return wallet.Counterparty{}, fmt.Errorf("bwallet: counterparty %q: %w", idHex, err)
	}
	return wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: pub}, nil
}

func (d *Derivation) args() (wallet.EncryptionArgs, error) {
	cp, err := Counterparty(d.CounterpartyHex)
	if err != nil {
		return wallet.EncryptionArgs{}, err
	}
	return wallet.EncryptionArgs{
		ProtocolID:   wallet.Protocol{SecurityLevel: wallet.SecurityLevel(d.SecurityLevel), Protocol: d.Protocol},
		KeyID:        d.KeyID,
		Counterparty: cp,
	}, nil
}

type derivedUnlocker struct {
	w *Signer
	d *Derivation
}

var _ transaction.UnlockingScriptTemplate = (*derivedUnlocker)(nil)

func (u *derivedUnlocker) Sign(tx *transaction.Transaction, inputIndex uint32) (*script.Script, error) {
	// Compared as uint64 for the reason fundUnlocker.Sign gives.
	if uint64(inputIndex) >= uint64(len(tx.Inputs)) {
		return nil, fmt.Errorf("bwallet: input %d out of range", inputIndex)
	}
	if tx.Inputs[inputIndex].SourceTxOutput() == nil {
		return nil, transaction.ErrEmptyPreviousTx
	}
	sh, err := tx.CalcInputSignatureHash(inputIndex, sighash.AllForkID)
	if err != nil {
		return nil, err
	}
	args, err := u.d.args()
	if err != nil {
		return nil, err
	}
	res, err := u.w.CreateSignature(context.Background(), wallet.CreateSignatureArgs{EncryptionArgs: args, HashToDirectlySign: sh}, u.w.Originator)
	if err != nil {
		return nil, fmt.Errorf("bwallet: sign derived input: %w", err)
	}
	pub, err := u.w.DerivedKey(context.Background(), u.d)
	if err != nil {
		return nil, err
	}
	sig := res.Signature.Serialize()
	sigWithType := append(append(make([]byte, 0, len(sig)+1), sig...), byte(sighash.AllForkID))
	s := &script.Script{}
	if err := s.AppendPushData(sigWithType); err != nil {
		return nil, err
	}
	if err := s.AppendPushData(pub.Compressed()); err != nil {
		return nil, err
	}
	return s, nil
}

func (u *derivedUnlocker) EstimateLength(_ *transaction.Transaction, _ uint32) uint32 { return 106 }

// PaymentDestination, DerivedKey, DerivedScript and DerivedUnlocker on the
// embedded wallet are the Signer's; see signer.go.
func (e *Embedded) PaymentDestination(ctx context.Context, recipientHex, prefix, suffix string) (*script.Script, error) {
	return e.Signer().PaymentDestination(ctx, recipientHex, prefix, suffix)
}

func (e *Embedded) DerivedKey(ctx context.Context, d *Derivation) (*ec.PublicKey, error) {
	return e.Signer().DerivedKey(ctx, d)
}

func (e *Embedded) DerivedScript(ctx context.Context, d *Derivation) (*script.Script, error) {
	return e.Signer().DerivedScript(ctx, d)
}

func (e *Embedded) DerivedUnlocker(d *Derivation) transaction.UnlockingScriptTemplate {
	return e.Signer().DerivedUnlocker(d)
}
