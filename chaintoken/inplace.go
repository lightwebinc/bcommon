package chaintoken

import (
	"encoding/binary"
	"errors"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// errNotInPlace sends a transaction parseTxInPlace does not take (no inputs:
// the extended format, or an incomplete transaction) to go-sdk's parser.
var errNotInPlace = errors.New("chaintoken: parsed by go-sdk")

// parseTxInPlace parses one transaction in the standard serialization at the
// start of b, as transaction.ReadFrom does, except that every script is a
// slice of b rather than a copy: capped at its own end, so an append to it
// reallocates instead of writing over the bytes after it. It returns the
// transaction and the bytes it took. A transaction with no inputs goes to
// go-sdk (errNotInPlace), which reads the extended format and incomplete
// transactions there; every other input is parsed field for field as
// go-sdk parses it, the same varint forms accepted and every length held to
// the bytes that remain.
func parseTxInPlace(b []byte) (*transaction.Transaction, int, error) {
	p := 0
	short := errors.New("chaintoken: transaction runs past the bytes")
	need := func(n uint64) error {
		if n > uint64(len(b)-p) {
			return short
		}
		return nil
	}
	u32 := func() (uint32, error) {
		if err := need(4); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint32(b[p:])
		p += 4
		return v, nil
	}
	varint := func() (uint64, error) {
		if err := need(1); err != nil {
			return 0, err
		}
		t := b[p]
		p++
		var n uint64
		switch t {
		case 0xff:
			if err := need(8); err != nil {
				return 0, err
			}
			n = binary.LittleEndian.Uint64(b[p:])
			p += 8
		case 0xfe:
			if err := need(4); err != nil {
				return 0, err
			}
			n = uint64(binary.LittleEndian.Uint32(b[p:]))
			p += 4
		case 0xfd:
			if err := need(2); err != nil {
				return 0, err
			}
			n = uint64(binary.LittleEndian.Uint16(b[p:]))
			p += 2
		default:
			n = uint64(t)
		}
		return n, nil
	}
	slice := func(n uint64) (*script.Script, error) {
		if err := need(n); err != nil {
			return nil, err
		}
		s := script.NewFromBytes(b[p : p+int(n) : p+int(n)])
		p += int(n)
		return s, nil
	}

	tx := &transaction.Transaction{}
	var err error
	if tx.Version, err = u32(); err != nil {
		return nil, 0, err
	}
	nin, err := varint()
	if err != nil {
		return nil, 0, err
	}
	if nin == 0 {
		return nil, 0, errNotInPlace
	}
	// An input is at least 41 bytes (txid, index, a one-byte length,
	// sequence), so a count past what remains cannot be right.
	if nin > uint64(len(b)-p)/41 {
		return nil, 0, short
	}
	tx.Inputs = make([]*transaction.TransactionInput, 0, nin)
	for range nin {
		if err := need(32); err != nil {
			return nil, 0, err
		}
		in := &transaction.TransactionInput{}
		if in.SourceTXID, err = chainhash.NewHash(b[p : p+32]); err != nil {
			return nil, 0, err
		}
		p += 32
		if in.SourceTxOutIndex, err = u32(); err != nil {
			return nil, 0, err
		}
		n, err := varint()
		if err != nil {
			return nil, 0, err
		}
		if in.UnlockingScript, err = slice(n); err != nil {
			return nil, 0, err
		}
		if in.SequenceNumber, err = u32(); err != nil {
			return nil, 0, err
		}
		tx.Inputs = append(tx.Inputs, in)
	}
	nout, err := varint()
	if err != nil {
		return nil, 0, err
	}
	// An output is at least 9 bytes (satoshis and a one-byte length).
	if nout > uint64(len(b)-p)/9 {
		return nil, 0, short
	}
	tx.Outputs = make([]*transaction.TransactionOutput, 0, nout)
	for range nout {
		if err := need(8); err != nil {
			return nil, 0, err
		}
		out := &transaction.TransactionOutput{Satoshis: binary.LittleEndian.Uint64(b[p:])}
		p += 8
		n, err := varint()
		if err != nil {
			return nil, 0, err
		}
		if out.LockingScript, err = slice(n); err != nil {
			return nil, 0, err
		}
		tx.Outputs = append(tx.Outputs, out)
	}
	if tx.LockTime, err = u32(); err != nil {
		return nil, 0, err
	}
	return tx, p, nil
}
