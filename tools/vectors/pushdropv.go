package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
)

type pushdropCase struct {
	// Kind is the lock the case departs from: record (a carrier's record
	// output, payload and signature), funding (a funding output, the tag
	// alone) or state (a state token, tag, commitment and signature).
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Accept is whether a reader takes the encoding: exactly the script a
	// lock-before PushDrop of the decoded key and fields is written as,
	// every push minimal, the key the canonical compressed encoding, and
	// the drops as the template writes them.
	Accept bool `json:"accept"`
	// SDKDecodes is whether go-sdk's pushdrop.Decode, at the pin, reads a
	// key and the canonical lock's fields: a case refused here that the SDK
	// reads is a second encoding of the same fields, and with the key
	// written another way, of the same output.
	SDKDecodes bool   `json:"sdkDecodes"`
	LockHex    string `json:"lockHex"`
}

type pushdropVector struct {
	FundingTagHex string         `json:"fundingTagHex"`
	StateTagHex   string         `json:"stateTagHex"`
	Cases         []pushdropCase `json:"cases"`
}

// minimalPush is data behind the push a minimal encoding uses, written out
// here from the script rules rather than by the template under test.
func minimalPush(d []byte) []byte {
	switch {
	case len(d) == 0 || (len(d) == 1 && d[0] == 0):
		return []byte{script.Op0}
	case len(d) == 1 && d[0] >= 1 && d[0] <= 16:
		return []byte{0x50 + d[0]}
	case len(d) == 1 && d[0] == 0x81:
		return []byte{script.Op1NEGATE}
	case len(d) <= 75:
		return cat([]byte{byte(len(d))}, d)
	case len(d) <= 0xff:
		return cat([]byte{script.OpPUSHDATA1, byte(len(d))}, d)
	default:
		return cat([]byte{script.OpPUSHDATA2, byte(len(d)), byte(len(d) >> 8)}, d)
	}
}

func pushdata1(d []byte) []byte { return cat([]byte{script.OpPUSHDATA1, byte(len(d))}, d) }
func pushdata2(d []byte) []byte {
	return cat([]byte{script.OpPUSHDATA2, byte(len(d)), byte(len(d) >> 8)}, d)
}

// drops is the tail the template writes for n fields: OP_2DROP per pair,
// then OP_DROP for one left over.
func drops(n int) []byte {
	var out []byte
	for ; n > 1; n -= 2 {
		out = append(out, script.Op2DROP)
	}
	if n == 1 {
		out = append(out, script.OpDROP)
	}
	return out
}

// layout is a lock-before PushDrop as its parts: the key push, the field
// pushes and the tail, so a case can change one of them.
type layout struct {
	key    []byte
	fields [][]byte
	tail   []byte
}

func (l layout) bytes() []byte {
	out := cat(l.key, []byte{script.OpCHECKSIG})
	for _, f := range l.fields {
		out = append(out, f...)
	}
	return append(out, l.tail...)
}

func pushdrops(v *txVector) (*pushdropVector, error) {
	out := &pushdropVector{FundingTagHex: v.FundingTagHex, StateTagHex: v.StateTagHex}
	bases := []struct{ kind, lockHex string }{
		{"record", v.Carriers[0].LockHex},
		{"funding", v.FundingLockHex},
		{"state", v.Create.LockHex},
	}
	one := make([]byte, 32)
	one[31] = 1
	alias := new(big.Int).Add(fieldPrime, big.NewInt(1)).FillBytes(make([]byte, 32))
	for _, base := range bases {
		raw, err := hex.DecodeString(base.lockHex)
		if err != nil {
			return nil, err
		}
		s := script.Script(raw)
		d := pushdrop.Decode(&s)
		if d == nil || d.LockingPublicKey == nil {
			return nil, fmt.Errorf("%s: the lock does not decode", base.kind)
		}
		key := d.LockingPublicKey.Compressed()
		canon := layout{key: minimalPush(key), tail: drops(len(d.Fields))}
		for _, f := range d.Fields {
			canon.fields = append(canon.fields, minimalPush(f))
		}
		if !bytes.Equal(canon.bytes(), raw) {
			return nil, fmt.Errorf("%s: the lock is not the canonical layout", base.kind)
		}
		n := len(d.Fields)
		with := func(change func(l *layout)) []byte {
			l := canon
			l.fields = append([][]byte(nil), canon.fields...)
			change(&l)
			return l.bytes()
		}
		k, err := ec.PublicKeyFromBytes(key)
		if err != nil {
			return nil, err
		}
		cases := []struct {
			name   string
			accept bool
			b      []byte
		}{
			{"canonical", true, raw},
			{"the key pushed with OP_PUSHDATA1", false, with(func(l *layout) { l.key = pushdata1(key) })},
			{"the first field pushed with OP_PUSHDATA1", false, with(func(l *layout) { l.fields[0] = pushdata1(d.Fields[0]) })},
			{"the last field pushed with OP_PUSHDATA2", false, with(func(l *layout) { l.fields[n-1] = pushdata2(d.Fields[n-1]) })},
			{"OP_NOP after the drops", false, with(func(l *layout) { l.tail = append(bytes.Clone(l.tail), script.OpNOP) })},
			{"no drops", false, with(func(l *layout) { l.tail = nil })},
			{"the key uncompressed", false, with(func(l *layout) { l.key = minimalPush(k.Uncompressed()) })},
			{"the key replaced by x = 1", true, with(func(l *layout) { l.key = minimalPush(cat([]byte{2}, one)) })},
			{"the key replaced by 02 || p+1, an alias of x = 1", false, with(func(l *layout) { l.key = minimalPush(cat([]byte{2}, alias)) })},
		}
		if n > 1 {
			cases = append(cases, struct {
				name   string
				accept bool
				b      []byte
			}{"every field dropped with OP_DROP", false, with(func(l *layout) { l.tail = bytes.Repeat([]byte{script.OpDROP}, n) })})
		}
		for _, c := range cases {
			cs := script.Script(c.b)
			got := pushdrop.Decode(&cs)
			same := got != nil && got.LockingPublicKey != nil && len(got.Fields) == n
			for i := 0; same && i < n; i++ {
				same = bytes.Equal(got.Fields[i], d.Fields[i])
			}
			out.Cases = append(out.Cases, pushdropCase{Kind: base.kind, Name: c.name, Accept: c.accept,
				SDKDecodes: same, LockHex: hex.EncodeToString(c.b)})
		}
	}
	return out, nil
}
