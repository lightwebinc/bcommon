package pushdrop

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"

	"github.com/lightwebinc/bcommon/guard"
)

// ErrNonCanonical is a lock-before PushDrop written other than the one way
// Lock writes it.
var ErrNonCanonical = errors.New("pushdrop: not the canonical encoding")

// CheckCanonical refuses, as ErrNonCanonical, a script that is not exactly
// the lock-before PushDrop Lock writes for the key and fields go-sdk decodes
// from it: the key pushed directly in its one canonical compressed encoding
// (guard.ParsePubKey), OP_CHECKSIG, each field in its minimal push (OP_0,
// OP_1 to OP_16 and OP_1NEGATE for the values they stand for, a direct push
// up to 75 bytes, then OP_PUSHDATA1, 2 and 4), then OP_2DROP per pair of
// fields and OP_DROP for one left over, and nothing else.
//
// go-sdk's decoder reads looser forms too: a wider push, a trailing opcode,
// drops written one at a time, an uncompressed key or a compressed key whose
// x is at or above the field prime. Each is another script for the same
// fields, so a reader that keys on script bytes, or on key bytes, would see
// two outputs where there is one.
func CheckCanonical(s *script.Script) error {
	if s == nil {
		return fmt.Errorf("%w: nil script", ErrNonCanonical)
	}
	chunks, err := s.Chunks()
	if err != nil || len(chunks) < 2 {
		return fmt.Errorf("%w: not a lock-before PushDrop", ErrNonCanonical)
	}
	if _, err := guard.ParsePubKey(chunks[0].Data); err != nil {
		return fmt.Errorf("%w: locking key: %v", ErrNonCanonical, err)
	}
	d := sdkpushdrop.Decode(s)
	if d == nil || d.LockingPublicKey == nil {
		return fmt.Errorf("%w: not a lock-before PushDrop", ErrNonCanonical)
	}
	if !bytes.Equal(canonicalLock(chunks[0].Data, d.Fields), *s) {
		return fmt.Errorf("%w: not the minimal encoding of its key and fields", ErrNonCanonical)
	}
	return nil
}

// canonicalLock is the lock-before PushDrop of key and fields as Lock
// writes it.
func canonicalLock(key []byte, fields [][]byte) []byte {
	out := append(minimalPush(key), script.OpCHECKSIG)
	for _, f := range fields {
		out = append(out, minimalPush(f)...)
	}
	n := len(fields)
	for ; n > 1; n -= 2 {
		out = append(out, script.Op2DROP)
	}
	if n == 1 {
		out = append(out, script.OpDROP)
	}
	return out
}

// minimalPush is d in the push a minimal encoding uses. The template writes
// an empty field as OP_0 too, which reads back as the one byte 0x00, so an
// empty field and a zero byte share an encoding.
func minimalPush(d []byte) []byte {
	n := len(d)
	switch {
	case n == 0 || (n == 1 && d[0] == 0):
		return []byte{script.Op0}
	case n == 1 && d[0] >= 1 && d[0] <= 16:
		return []byte{script.Op1 - 1 + d[0]}
	case n == 1 && d[0] == 0x81:
		return []byte{script.Op1NEGATE}
	case n <= 75:
		return append([]byte{byte(n)}, d...)
	case n <= 0xff:
		return append([]byte{script.OpPUSHDATA1, byte(n)}, d...)
	case n <= 0xffff:
		return append([]byte{script.OpPUSHDATA2, byte(n), byte(n >> 8)}, d...)
	default:
		return append([]byte{script.OpPUSHDATA4, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, d...)
	}
}
