package carrier

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/util"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// Once is a carrier minted without ever holding its payload in one buffer of its own: the payload is
// given in parts (a record's head, its large body, its tail), both signatures are taken over hashes
// streamed across the parts, and AppendTx writes the carrier's bytes into the caller's buffer with the
// body copied once, to the place it is sent from.
//
// Mint copies a 4 MiB piece three times before a byte is sent: the caller encodes the record (one copy),
// LockOne copies it into the locking script (two), and serializing the transaction copies the script
// (three), each into a zeroed allocation of the payload's size. MintOnce is byte for byte the carrier Mint
// builds for payload = head||body||tail (the signatures are RFC 6979, so deterministic): the field
// signature is the wallet's over SHA-256 of the payload (CreateSignature hashes Data so; here the hash is
// given), and the input's is signCarrier's over the BIP143 digest, hashOutputs streamed over the parts.
type Once struct {
	funding *transaction.Transaction
	vout    uint32
	sats    uint64
	key     []byte // the derivation's compressed public key
	parts   [3][]byte
	n       int    // len(payload)
	prefix  []byte // the payload's push opcode and length
	tail    []byte // the field signature's minimal push and OP_2DROP
	unlock  []byte // the input's unlocking script
}

// pushPrefix is script.PushDataPrefix for a push of n bytes, from the length alone.
func pushPrefix(n int) ([]byte, error) {
	switch {
	case n <= 75:
		return []byte{byte(n)}, nil
	case n <= 0xff:
		return []byte{script.OpPUSHDATA1, byte(n)}, nil
	case n <= 0xffff:
		return []byte{script.OpPUSHDATA2, byte(n), byte(n >> 8)}, nil
	case uint64(n) <= math.MaxUint32:
		return []byte{script.OpPUSHDATA4, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, nil
	}
	return nil, errors.New("carrier: payload too large to push")
}

// MintOnce signs a carrier for payload head||body||tail spending output vout of funding, as Mint does.
// The parts are read, not kept beyond AppendTx: the caller keeps them unchanged until then.
func MintOnce(ctx context.Context, w wallet.Interface, originator string, p Params, head, body, tail []byte,
	funding *transaction.Transaction, vout uint32) (*Once, error) {
	if w == nil {
		return nil, errors.New("carrier: nil wallet")
	}
	if funding == nil || uint64(vout) >= uint64(len(funding.Outputs)) {
		return nil, errors.New("carrier: funding output out of range")
	}
	n := len(head) + len(body) + len(tail)
	// LockOne locks a payload of one byte or none through Lock (an opcode, not a push): keep that path.
	if n <= 1 {
		return nil, errors.New("carrier: MintOnce needs a payload of two bytes or more (use Mint)")
	}
	d := p.Derivation
	args := wallet.EncryptionArgs{ProtocolID: d.Protocol, KeyID: d.KeyID, Counterparty: pushdrop.Anyone()}
	pub, err := w.GetPublicKey(ctx, wallet.GetPublicKeyArgs{EncryptionArgs: args, ForSelf: util.BoolPtr(true)}, originator)
	if err != nil {
		return nil, err
	}
	// The field signature: SHA-256 of the payload, streamed over its parts.
	fh := sha256.New()
	fh.Write(head)
	fh.Write(body)
	fh.Write(tail)
	fsig, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{EncryptionArgs: args, HashToDirectlySign: fh.Sum(nil)}, originator)
	if err != nil {
		return nil, err
	}
	o := &Once{funding: funding, vout: vout, sats: funding.Outputs[vout].Satoshis, key: pub.PublicKey.Compressed(),
		parts: [3][]byte{head, body, tail}, n: n}
	if o.prefix, err = pushPrefix(n); err != nil {
		return nil, err
	}
	t, err := script.NewScriptFromScriptOps([]*script.ScriptChunk{
		sdkpushdrop.CreateMinimallyEncodedScriptChunk(fsig.Signature.Serialize()),
		{Op: script.Op2DROP},
	})
	if err != nil {
		return nil, err
	}
	o.tail = *t

	// The input signature, as signCarrier takes it: hashOutputs streamed over the output's parts.
	flag := sighash.All | sighash.ForkID
	h := sha256.New()
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], o.sats)
	h.Write(b8[:])
	h.Write(util.VarInt(uint64(o.scriptLen())).Bytes())
	o.writeScript(h)
	first := h.Sum(nil)
	hashOutputs := sha256.Sum256(first)

	// The rest of the preimage as signCarrier builds it, from a transaction of the carrier's shape
	// (no output: SourceOutHash and SequenceHash read only the inputs).
	shape := transaction.NewTransaction()
	shape.LockTime = LockTime
	shape.AddInputFromTx(funding, vout, d.Unlocker(ctx, w, originator))
	shape.Inputs[0].SequenceNumber = Sequence
	in := shape.Inputs[0]
	code := *in.SourceTxScript()
	pre := make([]byte, 0, 156+9+len(code))
	pre = binary.LittleEndian.AppendUint32(pre, shape.Version)
	pre = append(pre, shape.SourceOutHash()[:]...)
	pre = append(pre, shape.SequenceHash()...)
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
	pre = binary.LittleEndian.AppendUint32(pre, shape.LockTime)
	pre = binary.LittleEndian.AppendUint32(pre, uint32(flag))
	digest := sha256.Sum256(pre)
	digest = sha256.Sum256(digest[:])
	isig, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{EncryptionArgs: args, HashToDirectlySign: digest[:]}, originator)
	if err != nil {
		return nil, err
	}
	u := &script.Script{}
	if err := u.AppendPushData(append(isig.Signature.Serialize(), byte(flag))); err != nil {
		return nil, err
	}
	o.unlock = *u
	return o, nil
}

// scriptLen is the locking script's length: `<key> OP_CHECKSIG <payload> <sig> OP_2DROP`.
func (o *Once) scriptLen() int { return 1 + len(o.key) + 1 + len(o.prefix) + o.n + len(o.tail) }

type writer interface{ Write([]byte) (int, error) }

func (o *Once) writeScript(w writer) {
	w.Write([]byte{byte(len(o.key))})
	w.Write(o.key)
	w.Write([]byte{script.OpCHECKSIG})
	w.Write(o.prefix)
	for _, p := range o.parts {
		w.Write(p)
	}
	w.Write(o.tail)
}

// Size is the carrier's serialized length.
func (o *Once) Size() int {
	s := o.scriptLen()
	return 4 + 1 + 32 + 4 + util.VarInt(uint64(len(o.unlock))).Length() + len(o.unlock) + 4 +
		1 + 8 + util.VarInt(uint64(s)).Length() + s + 4
}

// AppendTx appends the carrier's bytes to dst, the payload's body copied once, and returns them with the
// carrier as a transaction whose locking script aliases those bytes (it is not copied: dst must outlive
// the transaction and stay unchanged). The transaction is byte for byte Mint's.
func (o *Once) AppendTx(dst []byte) ([]byte, *transaction.Transaction) {
	in := o.funding.TxID()
	dst = binary.LittleEndian.AppendUint32(dst, 1) // version, as transaction.NewTransaction
	dst = append(dst, 1)
	dst = append(dst, in[:]...)
	dst = binary.LittleEndian.AppendUint32(dst, o.vout)
	dst = append(dst, util.VarInt(uint64(len(o.unlock))).Bytes()...)
	dst = append(dst, o.unlock...)
	dst = binary.LittleEndian.AppendUint32(dst, Sequence)
	dst = append(dst, 1)
	dst = binary.LittleEndian.AppendUint64(dst, o.sats)
	s := o.scriptLen()
	dst = append(dst, util.VarInt(uint64(s)).Bytes()...)
	at := len(dst)
	dst = append(dst, byte(len(o.key)))
	dst = append(dst, o.key...)
	dst = append(dst, script.OpCHECKSIG)
	dst = append(dst, o.prefix...)
	for _, p := range o.parts {
		dst = append(dst, p...)
	}
	dst = append(dst, o.tail...)
	lock := script.Script(dst[at : at+s : at+s])
	dst = binary.LittleEndian.AppendUint32(dst, LockTime)

	tx := transaction.NewTransaction()
	tx.LockTime = LockTime
	tx.AddInputFromTx(o.funding, o.vout, nil)
	tx.Inputs[0].SequenceNumber = Sequence
	unlock := script.Script(o.unlock)
	tx.Inputs[0].UnlockingScript = &unlock
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: o.sats, LockingScript: &lock})
	return dst, tx
}

// TxIDAt sets and returns the txid of a carrier whose bytes are b (as AppendTx wrote them), hashed in place.
func TxIDAt(tx *transaction.Transaction, b []byte) chainhash.Hash {
	first := sha256.Sum256(b)
	id := chainhash.Hash(sha256.Sum256(first[:]))
	tx.SetTxHash(&id)
	return id
}
