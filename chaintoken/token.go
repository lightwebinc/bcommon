package chaintoken

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/util"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// ErrSignature is a signature that is not the one strict, low-S DER
// encoding of its (r, s).
var ErrSignature = errors.New("chaintoken: signature is not strict low-S DER")

// TokenBEEF assembles the BEEF a mined token is admitted in (Wire.Token): a
// BEEF V1 of exactly the parent then the token, each with its own proof, or
// one merged proof, flagging both txids, when both are in one block. A replayer builds it from
// the token as a host stores it, alone with its proof, and the parent: the
// token before it, or for the first token of a chain the transaction whose
// output it spends.
//
// Neither transaction is changed: the paths are copied before they are
// merged.
func TokenBEEF(token, parent *transaction.Transaction) ([]byte, error) {
	if token == nil || parent == nil || token.MerklePath == nil || parent.MerklePath == nil {
		return nil, errors.New("chaintoken: a token's BEEF needs the token and its parent, both proven")
	}
	tp, pp := clonePath(token.MerklePath), clonePath(parent.MerklePath)
	bumps := []*transaction.MerklePath{pp, tp}
	idx := []int{0, 1}
	if tp.BlockHeight == pp.BlockHeight {
		rt, err1 := tp.ComputeRoot(token.TxID())
		rp, err2 := pp.ComputeRoot(parent.TxID())
		if err1 == nil && err2 == nil && rt.IsEqual(rp) {
			if err := pp.Combine(tp); err != nil {
				return nil, err
			}
			flagBoth(pp, *token.TxID(), *parent.TxID())
			bumps, idx = []*transaction.MerklePath{pp}, []int{0, 0}
		}
	}
	return writeV1(bumps, []*transaction.Transaction{parent, token}, idx), nil
}

// flagBoth marks a and b as txids at the lowest level of a merged path.
// When the two are each other's sibling, each path lists the other's txid
// as a plain hash, and merging keeps one element for each offset, so one
// of the two flags would be lost and the path would no longer be the union
// of the two minimal paths (MergedPath). In every other case both are
// flagged already and nothing changes.
func flagBoth(mp *transaction.MerklePath, a, b chainhash.Hash) {
	if len(mp.Path) == 0 {
		return
	}
	for _, e := range mp.Path[0] {
		if e.Hash != nil && (*e.Hash == a || *e.Hash == b) {
			yes := true
			e.Txid = &yes
		}
	}
}

func clonePath(mp *transaction.MerklePath) *transaction.MerklePath {
	cp, err := transaction.NewMerklePathFromBinary(mp.Bytes())
	if err != nil {
		panic(err) // a path that serialized parses
	}
	return cp
}

// writeV1 writes a BEEF V1 (BRC-62): the BUMPs, then each transaction with
// its BUMP index (-1 for none), in the order given; the last is the subject.
func writeV1(bumps []*transaction.MerklePath, txs []*transaction.Transaction, bumpOf []int) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, beefV1)
	b.Write(util.VarInt(uint64(len(bumps))).Bytes())
	for _, m := range bumps {
		b.Write(m.Bytes())
	}
	b.Write(util.VarInt(uint64(len(txs))).Bytes())
	for i, tx := range txs {
		b.Write(tx.Bytes())
		if bumpOf[i] < 0 {
			b.WriteByte(0)
			continue
		}
		b.WriteByte(1)
		b.Write(util.VarInt(uint64(bumpOf[i])).Bytes())
	}
	return b.Bytes()
}

// Stored reads a token as a host stores it: the token's transaction alone,
// with its proof, as an overlay engine keeps a proven transaction (an
// Atomic BEEF, or any BEEF whose subject is the token). It returns the
// subject transaction, without a parent. An index and a reader read a
// stored token this way and never admit it again. bound is the BEEF bound,
// DefaultMaxBEEF when zero.
func Stored(b []byte, bound int) (*transaction.Transaction, error) {
	w, err := ReadWire(b, bound)
	if err != nil {
		return nil, err
	}
	return w.SubjectTx(), nil
}

// Mined reports whether tx carries a proof that verifies against the
// headers. No transaction, no proof or no headers means nothing is mined.
func Mined(ctx context.Context, tx *transaction.Transaction, headers chaintracker.ChainTracker) bool {
	if tx == nil || tx.MerklePath == nil || headers == nil {
		return false
	}
	txid := chainhash.Hash(*tx.TxID())
	ok, err := tx.MerklePath.Verify(ctx, &txid, headers)
	return err == nil && ok
}

// CheckDER refuses, as ErrSignature, a signature that is not the one
// strict, low-S DER encoding of its (r, s): 8 to 72 bytes, and
// re-serializing the parsed value gives back the same bytes. A signature
// with another encoding verifies as well as the strict one, so a host that
// took it would take two byte strings for one signature.
func CheckDER(sig []byte) error {
	if len(sig) < 8 || len(sig) > 72 {
		return ErrSignature
	}
	s, err := ec.ParseDERSignature(sig)
	if err != nil || !bytes.Equal(s.Serialize(), sig) {
		return ErrSignature
	}
	return nil
}

// VerifyField checks a PushDrop field signature: strict low-S DER
// (CheckDER), over SHA-256 of signed, the fields concatenated, under key.
func VerifyField(key *ec.PublicKey, signed, der []byte) bool {
	if CheckDER(der) != nil {
		return false
	}
	s, err := ec.ParseDERSignature(der)
	if err != nil {
		return false
	}
	d := sha256.Sum256(signed)
	return s.Verify(d[:], key)
}

// Output is a token output read leniently: the fields its lock pushes
// before the signature, and the signature.
type Output struct {
	// Fields are the pushes before the signature: by convention the tag,
	// then the record.
	Fields [][]byte
	// Signature is the last push.
	Signature []byte
	// Vout is the output's index.
	Vout uint32
}

// ReadOutput reads o as a token output of nfields fields and a signature
// holding exactly 1 satoshi, and reports false for any other shape. It
// reads the script leniently (pushdrop.Fields) and decides nothing about
// its encoding or its key: LockedTo and SignedBy do, once the caller has
// decoded the record and derived the key it names.
func ReadOutput(o *transaction.TransactionOutput, vout uint32, nfields int) (*Output, bool) {
	if o == nil || o.LockingScript == nil || o.Satoshis != 1 {
		return nil, false
	}
	fields, _ := pushdrop.Fields(*o.LockingScript)
	if len(fields) != nfields+1 {
		return nil, false
	}
	return &Output{Fields: fields[:nfields], Signature: fields[nfields], Vout: vout}, true
}

// LockedTo reports whether script is, byte for byte, the canonical
// lock-before PushDrop of the output's fields and signature under key
// (pushdrop.Script).
func (t *Output) LockedTo(script []byte, key *ec.PublicKey) bool {
	return bytes.Equal(script, pushdrop.Script(key, t.Fields, t.Signature))
}

// Signed returns the bytes the field signature covers: the fields
// concatenated.
func (t *Output) Signed() []byte {
	var out []byte
	for _, f := range t.Fields {
		out = append(out, f...)
	}
	return out
}

// SignedBy reports whether the field signature is strict and verifies
// under key (VerifyField).
func (t *Output) SignedBy(key *ec.PublicKey) bool {
	return VerifyField(key, t.Signed(), t.Signature)
}

// Spend is one input of a token that spends an output of its parent.
type Spend struct {
	// Input is the index of the token's input.
	Input int
	// Vout is the index of the parent's output it spends.
	Vout uint32
	// Output is that output.
	Output *transaction.TransactionOutput
}

// Spends lists the inputs of tx that spend an output of parent, in input
// order. An input naming an output the parent does not have, or one with no
// locking script, is left out, as is every input that spends another
// transaction: those pay the token's fee and are not read.
//
// A caller classifies each: the predecessor token it must spend exactly
// once, or for the first token of a chain the output it is created from.
func Spends(tx *transaction.Transaction, parent *Entry) []Spend {
	var out []Spend
	for i, in := range tx.Inputs {
		if *in.SourceTXID != parent.Txid || uint64(in.SourceTxOutIndex) >= uint64(len(parent.Tx.Outputs)) {
			continue
		}
		po := parent.Tx.Outputs[in.SourceTxOutIndex]
		if po == nil || po.LockingScript == nil {
			continue
		}
		out = append(out, Spend{Input: i, Vout: in.SourceTxOutIndex, Output: po})
	}
	return out
}
