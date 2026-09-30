package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

type beefCase struct {
	// Name says what the case is, or what it breaks.
	Name string `json:"name"`
	// Accept is whether a reader's structural guard admits the bytes to the
	// parser: every declared count and length fits the bytes present, the
	// walk ends at the last byte, and the shape is one this library reads.
	Accept bool `json:"accept"`
	// SDKParses is whether go-sdk's ParseBeef, at the pin, returns without
	// an error. A case refused here that go-sdk parses is one a reader
	// holds to a stricter shape than the SDK does.
	SDKParses bool   `json:"sdkParses"`
	BeefHex   string `json:"beefHex"`
}

// beefVector is BEEF in each form a reader admits, and in each way the
// bytes can declare more than they hold or a shape the reader refuses. The
// admitted forms carry the transaction family's first carrier or its kept
// funding tree.
type beefVector struct {
	Cases []beefCase `json:"cases"`
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// varint is a Bitcoin VarInt, in its minimal form.
func varint(v uint64) []byte {
	switch {
	case v < 0xfd:
		return []byte{byte(v)}
	case v <= 0xffff:
		return append([]byte{0xfd}, binary.LittleEndian.AppendUint16(nil, uint16(v))...)
	case v <= 0xffffffff:
		return append([]byte{0xfe}, le32(uint32(v))...)
	default:
		return append([]byte{0xff}, binary.LittleEndian.AppendUint64(nil, v)...)
	}
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

var (
	versionV1     = le32(0xEFBE0001)
	versionV2     = le32(0xEFBE0002)
	versionAtomic = le32(0x01010101)
)

// sdkParses runs go-sdk's parser under a recover: a panic is a refusal.
func sdkParses(b []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_, _, _, err := transaction.ParseBeef(b)
	return err == nil
}

func beefs(v *txVector, parts *txParts) (*beefVector, error) {
	carrier := parts.carriers[0]
	tree := parts.tree
	if tree.MerklePath == nil {
		return nil, fmt.Errorf("the funding tree has no proof")
	}
	atomic, err := carrier.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	v1, err := carrier.BEEF()
	if err != nil {
		return nil, err
	}
	bv2, err := transaction.NewBeefFromTransaction(carrier)
	if err != nil {
		return nil, err
	}
	v2, err := bv2.Bytes()
	if err != nil {
		return nil, err
	}
	bare, err := transaction.NewBeefFromTransaction(carrier)
	if err != nil {
		return nil, err
	}
	if bare.MakeTxidOnly(tree.TxID()) == nil {
		return nil, fmt.Errorf("the tree is not in the carrier's BEEF")
	}
	txidOnly, err := bare.Bytes()
	if err != nil {
		return nil, err
	}
	kept, err := hex.DecodeString(v.FundingTree.KeptBeefHex)
	if err != nil {
		return nil, err
	}

	// Built by hand from the parts, so that each malformed case below
	// changes one thing in a BEEF whose layout is known here: a V1 of the
	// tree with its proof, then the carrier.
	bump := tree.MerklePath.Bytes()
	treeRaw, carrierRaw := tree.Bytes(), carrier.Bytes()
	manual := func(bumps [][]byte, entries ...[]byte) []byte {
		out := cat(versionV1, varint(uint64(len(bumps))))
		for _, b := range bumps {
			out = append(out, b...)
		}
		out = append(out, varint(uint64(len(entries)))...)
		for _, e := range entries {
			out = append(out, e...)
		}
		return out
	}
	proven := cat(treeRaw, []byte{1}, varint(0))
	unproven := cat(carrierRaw, []byte{0})
	handV1 := manual([][]byte{bump}, proven, unproven)
	if !bytes.Equal(handV1, v1) {
		return nil, fmt.Errorf("the hand-built V1 is not go-sdk's")
	}
	// A transaction that declares huge counts, padded past the smallest
	// transaction so that only the count is wrong.
	pad := make([]byte, 64)
	hugeInputs := cat(le32(1), varint(0xffffffff), pad)
	outpoint := make([]byte, 36)
	longInputScript := cat(le32(1), varint(1), outpoint, varint(0x7fffffff), pad)
	longOutputScript := cat(le32(1), varint(1), outpoint, varint(0), le32(0), varint(1), make([]byte, 8), varint(0x7fffffff), pad)
	noInputs := cat(le32(1), varint(0), varint(1), make([]byte, 8), varint(0), le32(0), pad[:40])
	v2Entry := func(format byte, rest ...[]byte) []byte { return cat(append([][]byte{{format}}, rest...)...) }
	v2Of := func(entries ...[]byte) []byte {
		out := cat(versionV2, varint(1), bump, varint(uint64(len(entries))))
		for _, e := range entries {
			out = append(out, e...)
		}
		return out
	}
	// A proof with a tree height outside what a block can have.
	tall := cat(varint(uint64(treeHeight)), []byte{65})
	flat := cat(varint(uint64(treeHeight)), []byte{0})

	cases := []struct {
		name   string
		accept bool
		b      []byte
	}{
		{"carrier, Atomic BEEF V2", true, atomic},
		{"carrier, BEEF V1", true, v1},
		{"carrier, BEEF V2", true, v2},
		{"carrier, BEEF V2 with its parent as a bare txid", true, txidOnly},
		{"unmined funding tree, Atomic BEEF V2 as kept", true, kept},
		{"empty", false, nil},
		{"13 bytes declaring 2^63 BUMPs", false, cat(versionV1, varint(1<<63))},
		{"a BUMP declaring 2^32-1 leaves at level 0", false, cat(versionV1, varint(1), []byte{0x5a, 0x01}, varint(0xffffffff))},
		{"2^32-1 transactions", false, cat(versionV1, varint(0), varint(0xffffffff))},
		{"a transaction declaring 2^32-1 inputs", false, v2Of(v2Entry(0, hugeInputs))},
		{"an input script longer than the bytes", false, v2Of(v2Entry(0, longInputScript))},
		{"an output script longer than the bytes", false, v2Of(v2Entry(0, longOutputScript))},
		{"a transaction of no inputs", false, v2Of(v2Entry(0, noInputs))},
		{"no transactions", false, cat(versionV1, varint(0), varint(0))},
		{"a BUMP of tree height 65", false, manual([][]byte{tall}, unproven)},
		{"a BUMP of no levels", false, manual([][]byte{flat}, unproven)},
		{"V1 has-BUMP byte 2", false, manual([][]byte{bump}, cat(treeRaw, []byte{2}), unproven)},
		{"V1 BUMP index past the BUMPs", false, manual([][]byte{bump}, cat(treeRaw, []byte{1}, varint(1)), unproven)},
		{"V2 BUMP index past the BUMPs", false, v2Of(v2Entry(1, varint(1), treeRaw), v2Entry(0, carrierRaw))},
		{"V2 data format 3", false, v2Of(v2Entry(3, treeRaw))},
		{"version 0xEFBE0003", false, cat(le32(0xEFBE0003), handV1[4:])},
		{"Atomic around Atomic", false, cat(versionAtomic, carrier.TxID()[:], atomic)},
		{"truncated by one byte", false, atomic[:len(atomic)-1]},
		{"one trailing byte", false, cat(atomic, []byte{0})},
		{"V1 hand-built as go-sdk writes it", true, handV1},
		{"V2 hand-built, proven parent then carrier", true, v2Of(v2Entry(1, varint(0), treeRaw), v2Entry(0, carrierRaw))},
	}
	out := &beefVector{}
	for _, c := range cases {
		out.Cases = append(out.Cases, beefCase{Name: c.name, Accept: c.accept, SDKParses: sdkParses(c.b), BeefHex: hex.EncodeToString(c.b)})
	}
	for _, c := range out.Cases {
		if c.Accept && !c.SDKParses {
			return nil, fmt.Errorf("%s: an accepted case go-sdk does not parse", c.Name)
		}
	}
	return out, nil
}
