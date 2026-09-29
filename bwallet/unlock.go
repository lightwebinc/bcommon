package bwallet

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// fundUnlocker signs a P2PKH input locked to the funding key.
//
// It is the shape of go-sdk's p2pkh.P2PKH with the private key replaced by a
// wallet call: the sighash is computed by the transaction, signed by
// CreateSignature with HashToDirectlySign under the Signer's fund derivation,
// and the script is <sig||hashtype> <pubkey>. The key never leaves the key
// deriver.
type fundUnlocker struct {
	w *Signer
}

var _ transaction.UnlockingScriptTemplate = (*fundUnlocker)(nil)

func (u *fundUnlocker) Sign(tx *transaction.Transaction, inputIndex uint32) (*script.Script, error) {
	// Compared as uint64, not as int: on a 32-bit target int(inputIndex) is
	// negative from 1<<31 up and would pass the check.
	if uint64(inputIndex) >= uint64(len(tx.Inputs)) {
		return nil, fmt.Errorf("bwallet: input %d out of range", inputIndex)
	}
	if tx.Inputs[inputIndex].SourceTxOutput() == nil {
		// The BIP143 sighash commits to the source satoshis and script; a
		// signature over an input with neither is unverifiable, and the SDK
		// would silently sign over zero.
		return nil, transaction.ErrEmptyPreviousTx
	}
	sh, err := tx.CalcInputSignatureHash(inputIndex, sighash.AllForkID)
	if err != nil {
		return nil, err
	}
	if err := u.w.Profile.Validate(); err != nil {
		return nil, err
	}
	res, err := u.w.CreateSignature(context.Background(), wallet.CreateSignatureArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   u.w.Profile.FundProtocol,
			KeyID:        u.w.Profile.FundKeyID,
			Counterparty: fundCounterparty,
		},
		HashToDirectlySign: sh,
	}, u.w.Originator)
	if err != nil {
		return nil, fmt.Errorf("bwallet: sign fund input: %w", err)
	}
	pub, err := u.w.FundKey()
	if err != nil {
		return nil, err
	}
	sig := res.Signature.Serialize()
	sigWithType := make([]byte, 0, len(sig)+1)
	sigWithType = append(sigWithType, sig...)
	sigWithType = append(sigWithType, byte(sighash.AllForkID))

	s := &script.Script{}
	if err := s.AppendPushData(sigWithType); err != nil {
		return nil, err
	}
	if err := s.AppendPushData(pub.Compressed()); err != nil {
		return nil, err
	}
	return s, nil
}

// EstimateLength is the P2PKH figure: a 72-byte DER signature plus hashtype
// and a 33-byte compressed key, each with its push opcode.
func (u *fundUnlocker) EstimateLength(_ *transaction.Transaction, _ uint32) uint32 {
	return 106
}
