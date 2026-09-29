package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// sigHashAllForkID is the sighash type a carrier's input is signed with.
const sigHashAllForkID byte = 0x41

// order is the order n of secp256k1's group, written out here rather than
// taken from go-sdk so that the vector's high-S value does not rest on the
// code it tests.
var order, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

type unlockingCase struct {
	// Name says what the case changes in carrier 0's unlocking script.
	Name string `json:"name"`
	// Accept is whether a reader takes the carrier: only the canonical
	// unlocking script is taken.
	Accept bool `json:"accept"`
	// ScriptValid is whether go-sdk's script interpreter, through SPV
	// against the vector's two blocks, still accepts the spend. A case that
	// is script valid and refused is a malleation the chain's rules allow
	// and a reader must refuse.
	ScriptValid bool `json:"scriptValid"`
	// UnlockingHex is input 0's unlocking script.
	UnlockingHex string `json:"unlockingHex"`
	Txid         string `json:"txid"`
	TxHex        string `json:"txHex"`
}

// unlockingVector is carrier 0 of the transaction family with its one
// input's unlocking script rewritten in each way a third party could
// without the key, and in the ways a hand-built carrier could get it wrong.
// The record output, the outpoint spent, the locktime and the sequence are
// carrier 0's in every case but the two-input one.
type unlockingVector struct {
	// Carrier is the index in the transaction family's carriers of the
	// carrier every case departs from.
	Carrier     int             `json:"carrier"`
	SigHashType byte            `json:"sigHashType"`
	Cases       []unlockingCase `json:"cases"`
}

// der is a DER signature as its two integers' content bytes, so that a case
// can break one encoding rule and keep every other.
type der struct{ r, s []byte }

// minimalInt is x's big-endian bytes as a strict DER INTEGER holds them: no
// leading zero unless the top bit would otherwise be set.
func minimalInt(x *big.Int) []byte {
	b := x.Bytes()
	if len(b) == 0 {
		return []byte{0}
	}
	if b[0]&0x80 != 0 {
		return append([]byte{0}, b...)
	}
	return b
}

func (d der) encode() []byte {
	body := append([]byte{0x02, byte(len(d.r))}, d.r...)
	body = append(body, 0x02, byte(len(d.s)))
	body = append(body, d.s...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// parseDER reads the strict DER signature go-sdk wrote. It refuses anything
// else, because every case below is built by changing one thing in it.
func parseDER(b []byte) (der, error) {
	if len(b) < 8 || b[0] != 0x30 || int(b[1]) != len(b)-2 || b[2] != 0x02 {
		return der{}, fmt.Errorf("not DER: %x", b)
	}
	lr := int(b[3])
	if 6+lr > len(b) || b[4+lr] != 0x02 || 6+lr+int(b[5+lr]) != len(b) {
		return der{}, fmt.Errorf("not DER: %x", b)
	}
	return der{r: bytes.Clone(b[4 : 4+lr]), s: bytes.Clone(b[6+lr:])}, nil
}

// push is data behind the one-byte push opcode a minimal encoding uses for
// 1 to 75 bytes.
func push(data []byte) []byte {
	if len(data) < 1 || len(data) > 75 {
		panic("push: length out of the direct range")
	}
	return append([]byte{byte(len(data))}, data...)
}

func unlockings(v *txVector, parts *txParts) (*unlockingVector, error) {
	const base = 0
	orig := parts.carriers[base]
	u := orig.Inputs[0].UnlockingScript
	if u == nil || len(*u) < 2 || int((*u)[0]) != len(*u)-1 {
		return nil, fmt.Errorf("carrier %d's unlocking script is not one direct push: %x", base, u)
	}
	sig := (*u)[1:]
	if sig[len(sig)-1] != sigHashAllForkID {
		return nil, fmt.Errorf("carrier %d is signed with sighash 0x%02x", base, sig[len(sig)-1])
	}
	d, err := parseDER(sig[:len(sig)-1])
	if err != nil {
		return nil, err
	}
	s := new(big.Int).SetBytes(d.s)
	if s.Cmp(new(big.Int).Rsh(order, 1)) > 0 {
		return nil, fmt.Errorf("carrier %d's signature has a high S", base)
	}
	withHash := func(b []byte, h byte) []byte { return append(bytes.Clone(b), h) }
	canonical := withHash(d.encode(), sigHashAllForkID)
	if !bytes.Equal(canonical, sig) {
		return nil, fmt.Errorf("carrier %d's signature does not re-encode to itself", base)
	}

	highS := der{r: d.r, s: minimalInt(new(big.Int).Sub(order, s))}
	paddedR := der{r: append([]byte{0}, d.r...), s: d.s}
	paddedS := der{r: d.r, s: append([]byte{0}, d.s...)}
	// A negative R: the top bit of its first content byte set, which a strict
	// encoding would have padded with a zero. The padding is dropped if the
	// value needs it, and the bit set otherwise.
	negR := der{r: bytes.Clone(d.r), s: d.s}
	if negR.r[0] == 0 {
		negR.r = negR.r[1:]
	} else {
		negR.r[0] |= 0x80
	}
	zeroS := der{r: d.r, s: []byte{0}}
	rAtOrder := der{r: minimalInt(order), s: d.s}
	wrongLength := d.encode()
	wrongLength[1]++

	cases := []struct {
		name      string
		accept    bool
		unlocking []byte
	}{
		{"canonical", true, *u},
		{"high S", false, push(withHash(highS.encode(), sigHashAllForkID))},
		{"OP_PUSHDATA1 for a short push", false, append([]byte{0x4c, byte(len(sig))}, sig...)},
		{"OP_PUSHDATA2 for a short push", false, append([]byte{0x4d, byte(len(sig)), 0}, sig...)},
		{"OP_0 pushed before the signature", false, append([]byte{0x00}, push(sig)...)},
		{"a byte pushed before the signature", false, append(push([]byte{0x01}), push(sig)...)},
		{"the signature pushed twice", false, append(push(sig), push(sig)...)},
		{"OP_NOP after the signature", false, append(push(sig), 0x61)},
		{"OP_NOP before the signature", false, append([]byte{0x61}, push(sig)...)},
		{"R padded with a zero", false, push(withHash(paddedR.encode(), sigHashAllForkID))},
		{"S padded with a zero", false, push(withHash(paddedS.encode(), sigHashAllForkID))},
		{"R negative", false, push(withHash(negR.encode(), sigHashAllForkID))},
		{"sequence length one more than the content", false, push(withHash(wrongLength, sigHashAllForkID))},
		{"a byte after the DER sequence", false, push(withHash(append(d.encode(), 0x00), sigHashAllForkID))},
		{"S zero", false, push(withHash(zeroS.encode(), sigHashAllForkID))},
		{"R equal to the order", false, push(withHash(rAtOrder.encode(), sigHashAllForkID))},
		{"sighash ALL without FORKID", false, push(withHash(d.encode(), 0x01))},
		{"sighash ALL|ANYONECANPAY|FORKID", false, push(withHash(d.encode(), 0xc1))},
		{"no sighash byte", false, push(d.encode())},
		{"empty", false, nil},
	}
	out := &unlockingVector{Carrier: base, SigHashType: sigHashAllForkID}
	for _, c := range cases {
		tx, err := transaction.NewTransactionFromHex(orig.Hex())
		if err != nil {
			return nil, err
		}
		s := script.Script(bytes.Clone(c.unlocking))
		tx.Inputs[0].UnlockingScript = &s
		uc, err := unlockingRow(parts, c.name, c.accept, tx)
		if err != nil {
			return nil, err
		}
		if c.accept != (tx.Hex() == v.Carriers[base].TxHex) {
			return nil, fmt.Errorf("%s: only the canonical case is carrier %d's bytes", c.name, base)
		}
		out.Cases = append(out.Cases, uc)
	}

	// Two inputs, each spending a tree output with a canonical signature
	// from the key: a carrier spends exactly one funding output.
	b := parts.b
	two := transaction.NewTransaction()
	two.LockTime = carrierLockTime
	for _, vo := range []uint32{uint32(v.Carriers[base].Vout), 2} {
		two.AddInputFromTx(parts.tree, vo, b.unlocker(objectKeyID))
	}
	for _, in := range two.Inputs {
		in.SequenceNumber = carrierSequence
	}
	two.AddOutput(&transaction.TransactionOutput{Satoshis: orig.Outputs[0].Satoshis, LockingScript: orig.Outputs[0].LockingScript})
	if err := two.Sign(); err != nil {
		return nil, err
	}
	uc, err := unlockingRow(parts, "two inputs", false, two)
	if err != nil {
		return nil, err
	}
	out.Cases = append(out.Cases, uc)
	return out, nil
}

// unlockingRow checks what every case keeps of carrier 0 and records
// whether the interpreter still accepts the spend.
func unlockingRow(parts *txParts, name string, accept bool, tx *transaction.Transaction) (unlockingCase, error) {
	orig := parts.carriers[0]
	if len(tx.Outputs) != 1 || !bytes.Equal(*tx.Outputs[0].LockingScript, *orig.Outputs[0].LockingScript) ||
		!tx.Inputs[0].SourceTXID.IsEqual(orig.Inputs[0].SourceTXID) || tx.Inputs[0].SourceTxOutIndex != orig.Inputs[0].SourceTxOutIndex {
		return unlockingCase{}, fmt.Errorf("%s: the record or the outpoint moved", name)
	}
	if !accept && tx.TxID().IsEqual(orig.TxID()) {
		return unlockingCase{}, fmt.Errorf("%s: the txid did not move", name)
	}
	check, err := transaction.NewTransactionFromHex(tx.Hex())
	if err != nil {
		return unlockingCase{}, err
	}
	for _, in := range check.Inputs {
		in.SourceTransaction = parts.tree
	}
	ok, err := spv.Verify(context.Background(), check, parts.tracker, nil)
	return unlockingCase{Name: name, Accept: accept, ScriptValid: ok && err == nil,
		UnlockingHex: hex.EncodeToString(*tx.Inputs[0].UnlockingScript), Txid: tx.TxID().String(), TxHex: tx.Hex()}, nil
}
