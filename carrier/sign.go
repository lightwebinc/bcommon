package carrier

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/util"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// signCarrier signs a carrier's one input as the derivation's unlocker does
// (pushdrop.Derivation.Unlocker, SIGHASH_ALL|FORKID: the wallet's signature
// over the BIP143 digest, pushed with its flag), byte for byte, except that
// hashOutputs is taken over the one output's bytes where they lie. The SDK
// serializes every output into a fresh buffer and hashes that: a 4 MiB copy
// for each piece carrier, where the hash alone is what the signature needs.
func signCarrier(ctx context.Context, w wallet.Interface, originator string, d pushdrop.Derivation, tx *transaction.Transaction) error {
	if len(tx.Inputs) != 1 || len(tx.Outputs) != 1 {
		return errors.New("carrier: not one input and one output")
	}
	in, out := tx.Inputs[0], tx.Outputs[0]
	if in.SourceTXID == nil || in.SourceTxOutput() == nil || out.LockingScript == nil {
		return errors.New("carrier: the input's source output is not attached")
	}
	flag := sighash.All | sighash.ForkID

	// hashOutputs: SHA-256d over the output's serialization, streamed.
	h := sha256.New()
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], out.Satoshis)
	h.Write(b8[:])
	h.Write(util.VarInt(uint64(len(*out.LockingScript))).Bytes())
	h.Write(*out.LockingScript)
	first := h.Sum(nil)
	hashOutputs := sha256.Sum256(first)

	// The rest of the preimage as the SDK assembles it (assemblePreimage).
	code := *in.SourceTxScript()
	pre := make([]byte, 0, 156+9+len(code))
	pre = binary.LittleEndian.AppendUint32(pre, tx.Version)
	pre = append(pre, tx.SourceOutHash()[:]...)
	pre = append(pre, tx.SequenceHash()...)
	pre = append(pre, in.SourceTXID[:]...)
	pre = binary.LittleEndian.AppendUint32(pre, in.SourceTxOutIndex)
	pre = append(pre, util.VarInt(uint64(len(code))).Bytes()...)
	pre = append(pre, code...)
	sats := uint64(0)
	if s := in.SourceTxSatoshis(); s != nil {
		sats = *s
	}
	pre = binary.LittleEndian.AppendUint64(pre, sats)
	pre = binary.LittleEndian.AppendUint32(pre, in.SequenceNumber)
	pre = append(pre, hashOutputs[:]...)
	pre = binary.LittleEndian.AppendUint32(pre, tx.LockTime)
	pre = binary.LittleEndian.AppendUint32(pre, uint32(flag))
	digest := sha256.Sum256(pre)
	digest = sha256.Sum256(digest[:])

	sig, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{
		EncryptionArgs:     wallet.EncryptionArgs{ProtocolID: d.Protocol, KeyID: d.KeyID, Counterparty: pushdrop.Anyone()},
		HashToDirectlySign: digest[:],
	}, originator)
	if err != nil {
		return err
	}
	s := &script.Script{}
	if err := s.AppendPushData(append(sig.Signature.Serialize(), byte(flag))); err != nil {
		return err
	}
	in.UnlockingScript = s
	return nil
}
