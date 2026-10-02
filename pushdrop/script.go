package pushdrop

import (
	"encoding/binary"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
)

// FirstPush reads the push that follows a lock-before PushDrop's key and
// OP_CHECKSIG, and nothing else of the script: the script starts with 0x21,
// 33 bytes and 0xac, then one push, an opcode 0x01 to 0x4b or OP_PUSHDATA1,
// 2 or 4 with its little-endian length, every declared byte present. It
// allocates nothing and returns a slice of s.
//
// It is the first look a classifier takes at an output: which record or tag
// the first field claims. A script that claims something is then held to
// the exact script rebuilt from its fields (Script).
func FirstPush(s []byte) ([]byte, bool) {
	if len(s) < 36 || s[0] != 0x21 || s[34] != script.OpCHECKSIG {
		return nil, false
	}
	p := s[35:]
	op := p[0]
	var n, off int
	switch {
	case op >= 0x01 && op <= 0x4b:
		n, off = int(op), 1
	case op == script.OpPUSHDATA1 && len(p) >= 2:
		n, off = int(p[1]), 2
	case op == script.OpPUSHDATA2 && len(p) >= 3:
		n, off = int(binary.LittleEndian.Uint16(p[1:3])), 3
	case op == script.OpPUSHDATA4 && len(p) >= 5:
		v := binary.LittleEndian.Uint32(p[1:5])
		if uint64(v) > uint64(len(p)) {
			return nil, false
		}
		n, off = int(v), 5
	default:
		return nil, false
	}
	if len(p) < off+n {
		return nil, false
	}
	return p[off : off+n], true
}

// op is one parsed script element: an opcode, and for a data push the bytes
// pushed.
type op struct {
	code byte
	data []byte
	push bool
}

// parseScript splits s into opcodes. A push whose declared length runs past
// the end refuses the whole script. The reader is this package's own rather
// than the SDK's, so that the Go and TypeScript halves read every byte
// string the same way.
func parseScript(s []byte) ([]op, bool) {
	var out []op
	for i := 0; i < len(s); {
		c := s[i]
		i++
		var n int
		switch {
		case c == script.Op0:
			out = append(out, op{code: c, data: []byte{}, push: true})
			continue
		case c >= 1 && c <= 75:
			n = int(c)
		case c == script.OpPUSHDATA1:
			if i+1 > len(s) {
				return nil, false
			}
			n = int(s[i])
			i++
		case c == script.OpPUSHDATA2:
			if i+2 > len(s) {
				return nil, false
			}
			n = int(binary.LittleEndian.Uint16(s[i:]))
			i += 2
		case c == script.OpPUSHDATA4:
			if i+4 > len(s) {
				return nil, false
			}
			m := uint64(binary.LittleEndian.Uint32(s[i:]))
			i += 4
			if m > uint64(len(s)-i) {
				return nil, false
			}
			n = int(m)
		default:
			out = append(out, op{code: c})
			continue
		}
		if n > len(s)-i {
			return nil, false
		}
		out = append(out, op{code: c, data: s[i : i+n], push: true})
		i += n
	}
	return out, true
}

// Fields reads s as a lock-before PushDrop, leniently: a 33-byte push,
// OP_CHECKSIG, then every data push up to the first opcode that is not one,
// the field signature included when the lock carries one. Every field is a
// slice of s.
//
// It decides nothing about the script: whether s is the canonical one is
// decided by rebuilding it from these fields (Script) and comparing bytes,
// which is how a host holds a script to one encoding without trusting a
// decoder to refuse every other.
//
// A data push here is OP_0, a direct push or OP_PUSHDATA1, 2 or 4. OP_1 to
// OP_16 and OP_1NEGATE are not, although the canonical lock writes a
// one-byte field of 1 to 16 or 0x81 with them: such a field ends the
// reading, the rebuild then differs, and a host that compares refuses the
// script. A tag or a record is never one of those bytes alone. OP_0 reads
// as an empty field, and rebuilds as OP_0.
func Fields(s []byte) ([][]byte, bool) {
	ops, ok := parseScript(s)
	if !ok || len(ops) < 3 || !ops[0].push || len(ops[0].data) != 33 || ops[1].code != script.OpCHECKSIG {
		return nil, false
	}
	var fields [][]byte
	for _, o := range ops[2:] {
		if !o.push {
			break
		}
		fields = append(fields, o.data)
	}
	return fields, len(fields) > 0
}

// Script is the one canonical lock-before PushDrop for key, fields and an
// optional field signature, exactly as Lock writes it: the 33-byte
// compressed key, OP_CHECKSIG, each field and then the signature as one
// minimal push, and the fewest OP_2DROP then OP_DROP that clear them. A nil
// sig writes an unsigned lock.
//
// A host compares a script it checks with this, byte for byte.
func Script(key *ec.PublicKey, fields [][]byte, sig []byte) []byte {
	ops := []*script.ScriptChunk{{Op: 33, Data: key.Compressed()}, {Op: script.OpCHECKSIG}}
	all := append([][]byte{}, fields...)
	if sig != nil {
		all = append(all, sig)
	}
	for _, f := range all {
		ops = append(ops, sdkpushdrop.CreateMinimallyEncodedScriptChunk(f))
	}
	for n := len(all); n > 0; n -= 2 {
		if n == 1 {
			ops = append(ops, &script.ScriptChunk{Op: script.OpDROP})
		} else {
			ops = append(ops, &script.ScriptChunk{Op: script.Op2DROP})
		}
	}
	s, err := script.NewScriptFromScriptOps(ops)
	if err != nil {
		panic(err) // every chunk above is well formed
	}
	return *s
}
