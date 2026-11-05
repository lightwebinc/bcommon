package pushdrop

import (
	"context"
	"errors"

	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/util"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// LockOne is Lock for one field, signed: `<key> OP_CHECKSIG <field> <sig>
// OP_2DROP`, byte for byte the script Lock builds, with the field copied once,
// into a script allocated at its final size.
//
// Lock copies the fields it is given, then the SDK copies each into a chunk
// and every chunk into the script, each copy a fresh buffer the size of the
// field: for a 4 MiB piece record, three copies and three zeroed allocations
// before a byte is signed, most of a gateway's minting cost after the hashes.
// The field's signature is the SDK's, over the field's bytes as given.
//
// A field of one byte or none, which the SDK writes as an opcode rather than a
// push, is locked by Lock.
func (d Derivation) LockOne(ctx context.Context, w wallet.Interface, originator string, field []byte) (*script.Script, error) {
	if w == nil {
		return nil, errors.New("pushdrop: nil wallet")
	}
	if len(field) <= 1 {
		return d.Lock(ctx, w, originator, [][]byte{field}, true)
	}
	args := wallet.EncryptionArgs{ProtocolID: d.Protocol, KeyID: d.KeyID, Counterparty: Anyone()}
	pub, err := w.GetPublicKey(ctx, wallet.GetPublicKeyArgs{EncryptionArgs: args, ForSelf: util.BoolPtr(true)}, originator)
	if err != nil {
		return nil, err
	}
	sig, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{EncryptionArgs: args, Data: field}, originator)
	if err != nil {
		return nil, err
	}
	prefix, err := script.PushDataPrefix(field)
	if err != nil {
		return nil, err
	}
	tail, err := script.NewScriptFromScriptOps([]*script.ScriptChunk{
		sdkpushdrop.CreateMinimallyEncodedScriptChunk(sig.Signature.Serialize()),
		{Op: script.Op2DROP},
	})
	if err != nil {
		return nil, err
	}
	key := pub.PublicKey.Compressed()
	s := make(script.Script, 0, 1+len(key)+1+len(prefix)+len(field)+len(*tail))
	s = append(s, byte(len(key)))
	s = append(s, key...)
	s = append(s, script.OpCHECKSIG)
	s = append(s, prefix...)
	s = append(s, field...)
	s = append(s, *tail...)
	return &s, nil
}
