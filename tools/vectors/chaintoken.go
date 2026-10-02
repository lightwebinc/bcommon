package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// The stand-in blocks of the chain-token family, beside the transaction
// family's two. The token chain is the state token of the transaction
// family: the created token, and the updated one that spends it.
const (
	createHeight  uint32 = 110 // the created token, in a block of two
	updateHeight  uint32 = 120 // the updated token, in a block of two
	sharedHeight  uint32 = 130 // both, at offsets 1 and 2 of a block of four
	siblingHeight uint32 = 131 // both, at offsets 2 and 3 of a block of eight
	sweepHeight   uint32 = 140 // the sweep, in a block of two
	miscHeight    uint32 = 141 // an unrelated payment, in a block of two
	absentHeight  uint32 = 999 // a height the headers do not hold
)

type tokenBlock struct {
	Height uint32 `json:"height"`
	// RootHex is the block's merkle root in display order.
	RootHex string `json:"rootHex"`
}

type tokenSpend struct {
	Input int    `json:"input"`
	Vout  uint32 `json:"vout"`
}

type shapeCase struct {
	// Name says what the case is, or what it breaks.
	Name string `json:"name"`
	// Shape is the shape the BEEF is held to: token (a mined token and its
	// parent), carrier (an unmined carrier and its funding tree) or alone
	// (one mined transaction).
	Shape string `json:"shape"`
	// Reads is whether the bytes are one BEEF with its subject transaction
	// carried whole; a case that does not read is refused before any shape.
	Reads bool `json:"reads"`
	// Accept is whether the BEEF is exactly what the shape needs.
	Accept bool `json:"accept"`
	// SubjectTxid is the subject of a BEEF that reads, in display order.
	SubjectTxid string `json:"subjectTxid,omitempty"`
	// ParentTxid is, for an accepted token or carrier, the other
	// transaction's txid in display order.
	ParentTxid string `json:"parentTxid,omitempty"`
	// Spends lists, for an accepted token, each input that spends an
	// output of the parent.
	Spends []tokenSpend `json:"spends,omitempty"`
	// Mined is, for an accepted token, whether both proofs verify against
	// the family's blocks.
	Mined   *bool  `json:"mined,omitempty"`
	BeefHex string `json:"beefHex"`
}

type replayCase struct {
	Name string `json:"name"`
	// TokenStoredHex and ParentStoredHex are each transaction alone with
	// its own minimal proof, as Atomic BEEF: how a host stores a proven
	// transaction.
	TokenStoredHex  string `json:"tokenStoredHex"`
	ParentStoredHex string `json:"parentStoredHex"`
	TokenTxid       string `json:"tokenTxid"`
	ParentTxid      string `json:"parentTxid"`
	// BeefHex is the BEEF V1 a replayer assembles from the two: the
	// parent then the token, with two paths, or one path merged as go-sdk
	// merges two paths of one block.
	BeefHex string `json:"beefHex"`
}

type outputCase struct {
	Name     string `json:"name"`
	Satoshis uint64 `json:"satoshis"`
	// Fields is how many fields the reader expects before the signature.
	Fields    int    `json:"fields"`
	ScriptHex string `json:"scriptHex"`
	// Reads is whether the output is that many fields and a signature
	// holding 1 satoshi.
	Reads bool `json:"reads"`
	// FieldsHex and SignatureHex are what a reading returns.
	FieldsHex    []string `json:"fieldsHex,omitempty"`
	SignatureHex string   `json:"signatureHex,omitempty"`
	// Locked is whether the script is byte for byte the canonical lock of
	// those fields and that signature under the family's state key.
	Locked bool `json:"locked"`
	// Signed is whether the signature is strict low-S DER and verifies
	// over the fields under the state key.
	Signed bool `json:"signed"`
}

type derCase struct {
	Name   string `json:"name"`
	Strict bool   `json:"strict"`
	Hex    string `json:"hex"`
}

// chaintokenVector is the BEEF shapes a host admits a mined token, an
// unmined carrier and a mined sweep in, each as go-sdk or a hand-built
// writer lays it out and each broken one way; the BEEF a replayer
// assembles; and a token output read and held to its key.
type chaintokenVector struct {
	// StateLockingKeyHex is the key the token outputs are locked to.
	StateLockingKeyHex string       `json:"stateLockingKeyHex"`
	Blocks             []tokenBlock `json:"blocks"`
	Shapes             []shapeCase  `json:"shapes"`
	Replays            []replayCase `json:"replays"`
	Outputs            []outputCase `json:"outputs"`
	Signatures         []derCase    `json:"signatures"`
}

// standBlock is a stand-in block of a power-of-two number of leaves: every
// level of its merkle tree, computed here from the leaf ids.
type standBlock struct {
	height uint32
	levels [][]chainhash.Hash
}

func newStandBlock(height uint32, leaves []chainhash.Hash) (*standBlock, error) {
	if n := len(leaves); n < 2 || n&(n-1) != 0 {
		return nil, fmt.Errorf("a stand-in block of %d leaves", n)
	}
	b := &standBlock{height: height, levels: [][]chainhash.Hash{leaves}}
	for cur := leaves; len(cur) > 1; {
		next := make([]chainhash.Hash, len(cur)/2)
		for i := range next {
			next[i] = chainhash.Hash(sha256d(append(append([]byte{}, cur[2*i][:]...), cur[2*i+1][:]...)))
		}
		b.levels = append(b.levels, next)
		cur = next
	}
	return b, nil
}

func (b *standBlock) root() chainhash.Hash { return b.levels[len(b.levels)-1][0] }

// leaf is one element of a path under construction.
type leaf struct {
	offset uint64
	txid   bool
}

// path is the BUMP holding exactly the leaves named at each level, taken
// from the block's own tree.
func (b *standBlock) path(levels [][]leaf) *transaction.MerklePath {
	out := make([][]*transaction.PathElement, len(levels))
	for h, ls := range levels {
		for _, l := range ls {
			hash := b.levels[h][l.offset]
			e := &transaction.PathElement{Offset: l.offset, Hash: &hash}
			if l.txid {
				yes := true
				e.Txid = &yes
			}
			out[h] = append(out[h], e)
		}
	}
	return transaction.NewMerklePath(b.height, out)
}

// union is the leaves of the minimal paths of the offsets together, in
// ascending offset order at each level: at the lowest level each offset,
// flagged, and its sibling; above that each ancestor's sibling. With trim
// set, a sibling that is itself an ancestor of another offset is left out
// above the lowest level, since a reader computes it.
func (b *standBlock) union(trim bool, offsets ...uint64) [][]leaf {
	out := make([][]leaf, len(b.levels)-1)
	for h := range out {
		want := map[uint64]bool{}
		ancestors := map[uint64]bool{}
		for _, o := range offsets {
			ancestors[o>>uint(h)] = true
		}
		for a := range ancestors {
			if h == 0 {
				want[a] = true
			}
			if s := a ^ 1; h == 0 || !trim || !ancestors[s] {
				want[s] = true
			}
		}
		for o := uint64(0); o < uint64(len(b.levels[h])); o++ {
			if want[o] {
				out[h] = append(out[h], leaf{offset: o, txid: h == 0 && ancestors[o]})
			}
		}
	}
	return out
}

func (b *standBlock) minimal(offset uint64) *transaction.MerklePath {
	return b.path(b.union(false, offset))
}

// beefEntryV is one transaction of a hand-built BEEF: its raw bytes and the
// index of its BUMP (-1 for none), or a bare txid.
type beefEntryV struct {
	raw      []byte
	bump     int
	txidOnly *chainhash.Hash
}

func handV1(bumps []*transaction.MerklePath, entries ...beefEntryV) []byte {
	out := cat(versionV1, varint(uint64(len(bumps))))
	for _, b := range bumps {
		out = append(out, b.Bytes()...)
	}
	out = append(out, varint(uint64(len(entries)))...)
	for _, e := range entries {
		out = append(out, e.raw...)
		if e.bump < 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, 1)
		out = append(out, varint(uint64(e.bump))...)
	}
	return out
}

func handV2(bumps []*transaction.MerklePath, entries ...beefEntryV) []byte {
	out := cat(versionV2, varint(uint64(len(bumps))))
	for _, b := range bumps {
		out = append(out, b.Bytes()...)
	}
	out = append(out, varint(uint64(len(entries)))...)
	for _, e := range entries {
		switch {
		case e.txidOnly != nil:
			out = append(out, 2)
			out = append(out, e.txidOnly[:]...)
		case e.bump < 0:
			out = append(out, 0)
			out = append(out, e.raw...)
		default:
			out = append(out, 1)
			out = append(out, varint(uint64(e.bump))...)
			out = append(out, e.raw...)
		}
	}
	return out
}

func handAtomic(subject *chainhash.Hash, inner []byte) []byte {
	return cat(versionAtomic, subject[:], inner)
}

func stand(b byte) chainhash.Hash { return chainhash.Hash(bytes.Repeat([]byte{b}, 32)) }

func chaintokens(parts *txParts) (*chaintokenVector, error) {
	create, update := parts.create, parts.update
	tree, carrier, sweep, misc := parts.tree, parts.carriers[0], parts.sweep, parts.payment
	cid, uid := *create.TxID(), *update.TxID()
	createRaw, updateRaw := create.Bytes(), update.Bytes()

	block := func(height uint32, leaves ...chainhash.Hash) (*standBlock, error) {
		return newStandBlock(height, leaves)
	}
	bCreate, err := block(createHeight, stand(0x44), cid)
	if err != nil {
		return nil, err
	}
	bUpdate, err := block(updateHeight, uid, stand(0x55))
	if err != nil {
		return nil, err
	}
	bShared, err := block(sharedHeight, stand(0x61), cid, uid, stand(0x62))
	if err != nil {
		return nil, err
	}
	bSibling, err := block(siblingHeight, stand(0x71), stand(0x72), cid, uid, stand(0x73), stand(0x74), stand(0x75), stand(0x76))
	if err != nil {
		return nil, err
	}
	bSweep, err := block(sweepHeight, stand(0x81), *sweep.TxID())
	if err != nil {
		return nil, err
	}
	bMisc, err := block(miscHeight, *misc.TxID(), stand(0x82))
	if err != nil {
		return nil, err
	}
	tracker := roots{}
	for h, r := range parts.tracker {
		tracker[h] = r
	}
	v := &chaintokenVector{StateLockingKeyHex: hex.EncodeToString(parts.stateKey.Compressed())}
	for _, b := range []*standBlock{bCreate, bUpdate, bShared, bSibling, bSweep, bMisc} {
		tracker[b.height] = b.root()
		v.Blocks = append(v.Blocks, tokenBlock{Height: b.height, RootHex: b.root().String()})
	}
	for _, h := range []uint32{coinHeight, treeHeight} {
		v.Blocks = append(v.Blocks, tokenBlock{Height: h, RootHex: parts.tracker[h].String()})
	}

	// Every path the cases use is checked against the block it is cut
	// from before it is written.
	proves := func(mp *transaction.MerklePath, b *standBlock, ids ...chainhash.Hash) error {
		for _, id := range ids {
			got, err := mp.ComputeRoot(&id)
			if err != nil {
				return err
			}
			if want := b.root(); !got.IsEqual(&want) {
				return fmt.Errorf("a path at height %d computes root %s for %s, want %s", b.height, got, id, want)
			}
		}
		return nil
	}
	pCreate, pUpdate := bCreate.minimal(1), bUpdate.minimal(0)
	pSweep, pMisc := bSweep.minimal(1), bMisc.minimal(0)
	sharedCreate, sharedUpdate := bShared.minimal(1), bShared.minimal(2)
	sharedMerged := bShared.path(bShared.union(false, 1, 2))
	sharedTrimmed := bShared.path(bShared.union(true, 1, 2))
	siblingMerged := bSibling.path(bSibling.union(false, 2, 3))
	for _, c := range []struct {
		mp  *transaction.MerklePath
		b   *standBlock
		ids []chainhash.Hash
	}{
		{pCreate, bCreate, []chainhash.Hash{cid}}, {pUpdate, bUpdate, []chainhash.Hash{uid}},
		{pSweep, bSweep, []chainhash.Hash{*sweep.TxID()}}, {pMisc, bMisc, []chainhash.Hash{*misc.TxID()}},
		{sharedCreate, bShared, []chainhash.Hash{cid}}, {sharedUpdate, bShared, []chainhash.Hash{uid}},
		{sharedMerged, bShared, []chainhash.Hash{cid, uid}}, {sharedTrimmed, bShared, []chainhash.Hash{cid, uid}},
		{siblingMerged, bSibling, []chainhash.Hash{cid, uid}},
	} {
		if err := proves(c.mp, c.b, c.ids...); err != nil {
			return nil, err
		}
	}
	if bytes.Equal(sharedMerged.Bytes(), sharedTrimmed.Bytes()) {
		return nil, fmt.Errorf("the trimmed union is the whole union: the block does not exercise the difference")
	}

	// What go-sdk makes of two paths of one block: one of the two unions.
	sdkMerge := func(parent, token *transaction.MerklePath) (*transaction.MerklePath, error) {
		m, err := transaction.NewMerklePathFromBinary(parent.Bytes())
		if err != nil {
			return nil, err
		}
		t, err := transaction.NewMerklePathFromBinary(token.Bytes())
		if err != nil {
			return nil, err
		}
		return m, m.Combine(t)
	}
	sdkShared, err := sdkMerge(sharedCreate, sharedUpdate)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sdkShared.Bytes(), sharedMerged.Bytes()) && !bytes.Equal(sdkShared.Bytes(), sharedTrimmed.Bytes()) {
		return nil, fmt.Errorf("go-sdk merges two paths of one block into neither union: %x", sdkShared.Bytes())
	}
	// Two txids that are each other's sibling: go-sdk keeps one element
	// for each offset, the token path's, which lists the parent as a plain
	// hash, so the merged path flags the token alone. A replayer flags
	// both, which is the union.
	sdkSibling, err := sdkMerge(bSibling.minimal(2), bSibling.minimal(3))
	if err != nil {
		return nil, err
	}
	if bytes.Equal(sdkSibling.Bytes(), siblingMerged.Bytes()) {
		return nil, fmt.Errorf("go-sdk now flags both siblings when it merges: the case no longer exercises the difference")
	}
	for _, e := range sdkSibling.Path[0] {
		if *e.Hash == cid || *e.Hash == uid {
			flagged := true
			e.Txid = &flagged
		}
	}
	if !bytes.Equal(sdkSibling.Bytes(), siblingMerged.Bytes()) {
		return nil, fmt.Errorf("go-sdk's merge of two sibling paths, with both flagged, is not their union: %x", sdkSibling.Bytes())
	}

	// Wide paths: each proves its transaction and holds one leaf more.
	wideCreate := bShared.path([][]leaf{{{0, false}, {1, true}, {3, false}}, {{1, false}}})
	if err := proves(wideCreate, bShared, cid); err != nil {
		return nil, err
	}
	wideMerged := bSibling.path([][]leaf{{{0, false}, {2, true}, {3, true}}, {{0, false}}, {{1, false}}})
	if err := proves(wideMerged, bSibling, cid, uid); err != nil {
		return nil, err
	}
	thirdFlag := bShared.path([][]leaf{{{0, true}, {1, true}, {2, true}, {3, false}}, {}})
	missingSibling := bShared.path([][]leaf{{{1, true}, {2, true}, {3, false}}, {}})
	onlyCreate := bShared.path([][]leaf{{{0, false}, {1, true}}, {{1, false}}})
	twice := func() *transaction.MerklePath {
		p := bCreate.minimal(1)
		p.Path[0] = append(p.Path[0], p.Path[0][0])
		return p
	}()
	wideTree := func() *transaction.MerklePath {
		// The funding tree's own path with a third leaf at its one level.
		p, _ := transaction.NewMerklePathFromBinary(tree.MerklePath.Bytes())
		extra := stand(0x99)
		p.Path[0] = append(p.Path[0], &transaction.PathElement{Offset: 2, Hash: &extra})
		return p
	}()
	tallTree := func() *transaction.MerklePath {
		// The same path with a level more: one leaf a level, so minimal,
		// and a claim about another block, since its root is another.
		p, _ := transaction.NewMerklePathFromBinary(tree.MerklePath.Bytes())
		extra := stand(0x99)
		p.Path = append(p.Path, []*transaction.PathElement{{Offset: 1, Hash: &extra}})
		return p
	}()
	absent := func(mp *transaction.MerklePath) *transaction.MerklePath {
		p, _ := transaction.NewMerklePathFromBinary(mp.Bytes())
		p.BlockHeight = absentHeight
		return p
	}

	mp := func(ps ...*transaction.MerklePath) []*transaction.MerklePath { return ps }
	tx := func(raw []byte, bump int) beefEntryV { return beefEntryV{raw: raw, bump: bump} }
	bare := func(id chainhash.Hash) beefEntryV { return beefEntryV{txidOnly: &id} }
	treeRaw, carrierRaw, sweepRaw, miscRaw, coinRaw := tree.Bytes(), carrier.Bytes(), sweep.Bytes(), misc.Bytes(), parts.coin.Bytes()

	sdkAtomic, err := carrier.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	sdkV1, err := carrier.BEEF()
	if err != nil {
		return nil, err
	}

	yes, no := true, false
	type sc struct {
		name, shape string
		accept      bool
		parent      *chainhash.Hash
		spends      []tokenSpend
		mined       *bool
		beef        []byte
	}
	tid := tree.TxID()
	updateSpends := []tokenSpend{{Input: 0, Vout: 0}}
	shapes := []sc{
		// A mined token and its parent.
		{"BEEF V1, the parent then the token, a path each", "token", true, &cid, updateSpends, &yes,
			handV1(mp(pCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"BEEF V2 of the same", "token", true, &cid, updateSpends, &yes,
			handV2(mp(pCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"BEEF V1 with the paths in the other order", "token", true, &cid, updateSpends, &yes,
			handV1(mp(pUpdate, pCreate), tx(createRaw, 1), tx(updateRaw, 0))},
		{"both in one block: one path, the union of the two minimal paths", "token", true, &cid, updateSpends, &yes,
			handV1(mp(sharedMerged), tx(createRaw, 0), tx(updateRaw, 0))},
		{"both in one block: the union less the siblings a reader computes", "token", true, &cid, updateSpends, &yes,
			handV1(mp(sharedTrimmed), tx(createRaw, 0), tx(updateRaw, 0))},
		{"both in one block, each the other's sibling", "token", true, &cid, updateSpends, &yes,
			handV1(mp(siblingMerged), tx(createRaw, 0), tx(updateRaw, 0))},
		{"both in one block, BEEF V2", "token", true, &cid, updateSpends, &yes,
			handV2(mp(sharedMerged), tx(createRaw, 0), tx(updateRaw, 0))},
		{"the first token of a chain: its parent is the coin it spends", "token", true, parts.coin.TxID(), []tokenSpend{{Input: 0, Vout: 1}}, &yes,
			handV1(mp(parts.coin.MerklePath, pCreate), tx(coinRaw, 0), tx(createRaw, 1))},
		{"the token proven at a height the headers do not hold", "token", true, &cid, updateSpends, &no,
			handV1(mp(pCreate, absent(pUpdate)), tx(createRaw, 0), tx(updateRaw, 1))},
		{"the parent proven at a height the headers do not hold", "token", true, &cid, updateSpends, &no,
			handV1(mp(absent(pCreate), pUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"the token then the parent: the subject is the parent, which spends nothing of the other", "token", false, nil, nil, nil,
			handV1(mp(pCreate, pUpdate), tx(updateRaw, 1), tx(createRaw, 0))},
		{"an Atomic BEEF of the token and its parent", "token", false, nil, nil, nil,
			handAtomic(&uid, handV1(mp(pCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1)))},
		{"the token alone with its proof, as a host stores it", "token", false, nil, nil, nil,
			handAtomic(&uid, handV1(mp(pUpdate), tx(updateRaw, 0)))},
		{"an unrelated mined transaction riding along", "token", false, nil, nil, nil,
			handV1(mp(pMisc, pCreate, pUpdate), tx(miscRaw, 0), tx(createRaw, 1), tx(updateRaw, 2))},
		{"an unrelated mined transaction in the parent's place", "token", false, nil, nil, nil,
			handV1(mp(pMisc, pUpdate), tx(miscRaw, 0), tx(updateRaw, 1))},
		{"the parent unproven", "token", false, nil, nil, nil,
			handV1(mp(pUpdate), tx(createRaw, -1), tx(updateRaw, 0))},
		{"the token unproven", "token", false, nil, nil, nil,
			handV1(mp(pCreate), tx(createRaw, 0), tx(updateRaw, -1))},
		{"the parent as a bare txid", "token", false, nil, nil, nil,
			handV2(mp(pUpdate), bare(cid), tx(updateRaw, 0))},
		{"the parent's path holding a leaf the proof does not need", "token", false, nil, nil, nil,
			handV1(mp(wideCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"the parent's path listing one leaf twice", "token", false, nil, nil, nil,
			handV1(mp(twice, pUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"both in one block with two separate paths", "token", false, nil, nil, nil,
			handV1(mp(sharedCreate, sharedUpdate), tx(createRaw, 0), tx(updateRaw, 1))},
		{"both named by one path that proves only the parent", "token", false, nil, nil, nil,
			handV1(mp(onlyCreate), tx(createRaw, 0), tx(updateRaw, 0))},
		{"a merged path holding a leaf neither proof needs", "token", false, nil, nil, nil,
			handV1(mp(wideMerged), tx(createRaw, 0), tx(updateRaw, 0))},
		{"a merged path flagging a third leaf as a txid", "token", false, nil, nil, nil,
			handV1(mp(thirdFlag), tx(createRaw, 0), tx(updateRaw, 0))},
		{"a merged path missing a sibling a reader cannot compute", "token", false, nil, nil, nil,
			handV1(mp(missingSibling), tx(createRaw, 0), tx(updateRaw, 0))},
		{"three BUMPs for two transactions", "token", false, nil, nil, nil,
			handV1(mp(pCreate, pUpdate, pMisc), tx(createRaw, 0), tx(updateRaw, 1))},

		// An unmined carrier and its funding tree.
		{"a carrier as go-sdk writes its Atomic BEEF", "carrier", true, tid, nil, nil, sdkAtomic},
		{"a carrier as go-sdk writes its BEEF V1", "carrier", true, tid, nil, nil, sdkV1},
		{"a carrier, BEEF V2 by hand", "carrier", true, tid, nil, nil,
			handV2(mp(tree.MerklePath), tx(treeRaw, 0), tx(carrierRaw, -1))},
		{"a carrier without its funding tree", "carrier", false, nil, nil, nil,
			handV1(nil, tx(carrierRaw, -1))},
		{"a carrier whose funding tree is unproven, with the tree's parent", "carrier", false, nil, nil, nil,
			handV1(mp(parts.coin.MerklePath), tx(coinRaw, 0), tx(treeRaw, -1), tx(carrierRaw, -1))},
		{"a carrier with an unrelated mined transaction riding along", "carrier", false, nil, nil, nil,
			handV1(mp(pMisc, tree.MerklePath), tx(miscRaw, 0), tx(treeRaw, 1), tx(carrierRaw, -1))},
		{"a carrier whose funding tree is a bare txid", "carrier", false, nil, nil, nil,
			handV2(mp(tree.MerklePath), bare(*tid), tx(carrierRaw, -1))},
		{"a carrier whose tree's path holds a leaf the proof does not need", "carrier", false, nil, nil, nil,
			handV1(mp(wideTree), tx(treeRaw, 0), tx(carrierRaw, -1))},
		{"a carrier whose tree's path has a level more: minimal, and a proof of another root, which the headers refuse", "carrier", true, tid, nil, nil,
			handV1(mp(tallTree), tx(treeRaw, 0), tx(carrierRaw, -1))},
		{"a carrier with a second BUMP nothing names", "carrier", false, nil, nil, nil,
			handV1(mp(tree.MerklePath, pMisc), tx(treeRaw, 0), tx(carrierRaw, -1))},
		{"a carrier named as proven by its tree's path", "carrier", false, nil, nil, nil,
			handV1(mp(tree.MerklePath), tx(treeRaw, 0), tx(carrierRaw, 0))},

		// One mined transaction alone.
		{"a sweep alone with its minimal path, BEEF V1", "alone", true, nil, nil, nil,
			handV1(mp(pSweep), tx(sweepRaw, 0))},
		{"a sweep alone, Atomic BEEF", "alone", true, nil, nil, nil,
			handAtomic(sweep.TxID(), handV2(mp(pSweep), tx(sweepRaw, 0)))},
		{"a sweep with an unrelated mined transaction riding along", "alone", false, nil, nil, nil,
			handV1(mp(pMisc, pSweep), tx(miscRaw, 0), tx(sweepRaw, 1))},
		{"a sweep unproven, with the tree it spends", "alone", false, nil, nil, nil,
			handV1(mp(tree.MerklePath), tx(treeRaw, 0), tx(sweepRaw, -1))},
		{"a sweep alone beside a path it does not name", "alone", false, nil, nil, nil,
			handV1(mp(pSweep), tx(sweepRaw, -1))},
		{"a sweep proven by another transaction's path", "alone", false, nil, nil, nil,
			handV1(mp(pMisc), tx(sweepRaw, 0))},
	}
	// The subject of each case: the transaction its shape is about, except
	// where the case puts another one last.
	subjectOf := map[string]*transaction.Transaction{"token": update, "carrier": carrier, "alone": sweep}
	otherSubject := map[string]*transaction.Transaction{
		"the first token of a chain: its parent is the coin it spends":                            create,
		"the token then the parent: the subject is the parent, which spends nothing of the other": create,
	}
	for _, c := range shapes {
		subject := subjectOf[c.shape]
		if t, ok := otherSubject[c.name]; ok {
			subject = t
		}
		row := shapeCase{Name: c.name, Shape: c.shape, Reads: true, Accept: c.accept, SubjectTxid: subject.TxID().String(),
			Spends: c.spends, Mined: c.mined, BeefHex: hex.EncodeToString(c.beef)}
		if c.parent != nil {
			row.ParentTxid = c.parent.String()
		}
		v.Shapes = append(v.Shapes, row)
	}
	// Bytes that are no BEEF with its subject carried whole: refused
	// before any shape.
	good := handV1(mp(pCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1))
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"nothing", nil},
		{"one trailing byte", cat(good, []byte{0})},
		{"one byte short", good[:len(good)-1]},
		{"an Atomic BEEF whose subject is not in it", handAtomic(misc.TxID(), good)},
		{"a BEEF V2 whose last entry, its subject, is a bare txid", handV2(mp(pCreate), tx(createRaw, 0), bare(uid))},
		{"an unknown version", cat(le32(0xEFBE0003), good[4:])},
	} {
		v.Shapes = append(v.Shapes, shapeCase{Name: c.name, Shape: "token", BeefHex: hex.EncodeToString(c.b)})
	}

	// What a replayer assembles from two stored tokens.
	stored := func(t *transaction.Transaction, p *transaction.MerklePath) string {
		return hex.EncodeToString(handAtomic(t.TxID(), handV2(mp(p), tx(t.Bytes(), 0))))
	}
	v.Replays = []replayCase{
		{Name: "two blocks: a path each", TokenStoredHex: stored(update, pUpdate), ParentStoredHex: stored(create, pCreate),
			TokenTxid: uid.String(), ParentTxid: cid.String(),
			BeefHex: hex.EncodeToString(handV1(mp(pCreate, pUpdate), tx(createRaw, 0), tx(updateRaw, 1)))},
		{Name: "one block: the two paths merged as go-sdk merges them", TokenStoredHex: stored(update, sharedUpdate), ParentStoredHex: stored(create, sharedCreate),
			TokenTxid: uid.String(), ParentTxid: cid.String(),
			BeefHex: hex.EncodeToString(handV1(mp(sdkShared), tx(createRaw, 0), tx(updateRaw, 0)))},
		{Name: "one block, each the other's sibling: merged as go-sdk merges them, with both txids flagged", TokenStoredHex: stored(update, bSibling.minimal(3)), ParentStoredHex: stored(create, bSibling.minimal(2)),
			TokenTxid: uid.String(), ParentTxid: cid.String(),
			BeefHex: hex.EncodeToString(handV1(mp(sdkSibling), tx(createRaw, 0), tx(updateRaw, 0)))},
		{Name: "the first token of a chain and the coin it spends", TokenStoredHex: stored(create, pCreate), ParentStoredHex: stored(parts.coin, parts.coin.MerklePath),
			TokenTxid: cid.String(), ParentTxid: parts.coin.TxID().String(),
			BeefHex: hex.EncodeToString(handV1(mp(parts.coin.MerklePath, pCreate), tx(coinRaw, 0), tx(createRaw, 1)))},
	}

	// A token output read and held to its key. The canonical lock is the
	// updated token's; each other case rewrites one thing in it.
	lock := []byte(*update.Outputs[0].LockingScript)
	key := parts.stateKey.Compressed()
	if len(lock) < 35 || lock[0] != 33 || !bytes.Equal(lock[1:34], key) || lock[34] != script.OpCHECKSIG {
		return nil, fmt.Errorf("the token's lock does not start with the state key")
	}
	tag := stateTag
	commitment := parts.carriers[1].TxID()[:]
	rest := lock[35:]
	wantFields := cat(minimalPush(tag), minimalPush(commitment))
	if !bytes.HasPrefix(rest, wantFields) {
		return nil, fmt.Errorf("the token's lock does not push its tag and commitment minimally")
	}
	sigPush := rest[len(wantFields) : len(rest)-2]
	if int(sigPush[0]) != len(sigPush)-1 || !bytes.Equal(rest[len(rest)-2:], []byte{script.Op2DROP, script.OpDROP}) {
		return nil, fmt.Errorf("the token's lock does not end with its signature and the drops")
	}
	sig := sigPush[1:]
	d, err := parseDER(sig)
	if err != nil {
		return nil, err
	}
	head := lock[:35]
	layoutOf := func(sig []byte, fields ...[]byte) []byte {
		out := append([]byte{}, head...)
		for _, f := range fields {
			out = append(out, minimalPush(f)...)
		}
		out = append(out, minimalPush(sig)...)
		return append(out, drops(len(fields)+1)...)
	}
	if !bytes.Equal(layoutOf(sig, tag, commitment), lock) {
		return nil, fmt.Errorf("the hand-built lock is not the token's")
	}
	highS := der{r: d.r, s: minimalInt(new(big.Int).Sub(order, new(big.Int).SetBytes(d.s)))}.encode()
	// The updated token's key signs the record alone: a strict signature
	// over other bytes than the fields.
	other, err := parts.b.lock(stateKeyID, true, commitment)
	if err != nil {
		return nil, err
	}
	otherRest := []byte(*other)[35+len(minimalPush(commitment)):]
	recordOnlySig := otherRest[1 : len(otherRest)-1]
	if int(otherRest[0]) != len(recordOnlySig) || otherRest[len(otherRest)-1] != script.Op2DROP {
		return nil, fmt.Errorf("the one-field lock does not end with its signature and a drop")
	}
	objectLock, err := parts.b.lock(objectKeyID, true, tag, commitment)
	if err != nil {
		return nil, err
	}
	objectFields, objectSig, err := tailOf([]byte(*objectLock), 2)
	if err != nil {
		return nil, err
	}
	funding := []byte(*tree.Outputs[0].LockingScript)
	three, err := parts.b.lock(stateKeyID, true, tag, commitment, []byte("third"))
	if err != nil {
		return nil, err
	}
	threeFields, threeSig, err := tailOf([]byte(*three), 3)
	if err != nil {
		return nil, err
	}
	hx := func(bs ...[]byte) []string {
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = hex.EncodeToString(b)
		}
		return out
	}
	base := hx(tag, commitment)
	v.Outputs = []outputCase{
		{Name: "the updated token's output", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(lock),
			Reads: true, FieldsHex: base, SignatureHex: hex.EncodeToString(sig), Locked: true, Signed: true},
		{Name: "the same holding 2 satoshis", Satoshis: 2, Fields: 2, ScriptHex: hex.EncodeToString(lock)},
		{Name: "the same holding nothing", Satoshis: 0, Fields: 2, ScriptHex: hex.EncodeToString(lock)},
		{Name: "the same read as one field and a signature", Satoshis: 1, Fields: 1, ScriptHex: hex.EncodeToString(lock)},
		{Name: "the same read as three fields and a signature", Satoshis: 1, Fields: 3, ScriptHex: hex.EncodeToString(lock)},
		{Name: "a three-field signed lock under the same key", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString([]byte(*three))},
		{Name: "the three-field lock read as three fields", Satoshis: 1, Fields: 3, ScriptHex: hex.EncodeToString([]byte(*three)),
			Reads: true, FieldsHex: hx(threeFields...), SignatureHex: hex.EncodeToString(threeSig), Locked: true, Signed: true},
		{Name: "a funding output: one field and no signature", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(funding)},
		{Name: "the commitment pushed with OP_PUSHDATA1", Satoshis: 1, Fields: 2,
			ScriptHex: hex.EncodeToString(cat(head, minimalPush(tag), pushdata1(commitment), minimalPush(sig), drops(3))),
			Reads:     true, FieldsHex: base, SignatureHex: hex.EncodeToString(sig), Locked: false, Signed: true},
		{Name: "OP_NOP after the drops", Satoshis: 1, Fields: 2,
			ScriptHex: hex.EncodeToString(cat(lock, []byte{script.OpNOP})),
			Reads:     true, FieldsHex: base, SignatureHex: hex.EncodeToString(sig), Locked: false, Signed: true},
		{Name: "the drops written one at a time", Satoshis: 1, Fields: 2,
			ScriptHex: hex.EncodeToString(cat(head, minimalPush(tag), minimalPush(commitment), minimalPush(sig), []byte{script.OpDROP, script.OpDROP, script.OpDROP})),
			Reads:     true, FieldsHex: base, SignatureHex: hex.EncodeToString(sig), Locked: false, Signed: true},
		{Name: "the signature's high-S twin", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(layoutOf(highS, tag, commitment)),
			Reads: true, FieldsHex: base, SignatureHex: hex.EncodeToString(highS), Locked: true, Signed: false},
		{Name: "a strict signature by the same key over the second field alone", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(layoutOf(recordOnlySig, tag, commitment)),
			Reads: true, FieldsHex: base, SignatureHex: hex.EncodeToString(recordOnlySig), Locked: true, Signed: false},
		{Name: "the same fields locked and signed under another key", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString([]byte(*objectLock)),
			Reads: true, FieldsHex: hx(objectFields...), SignatureHex: hex.EncodeToString(objectSig), Locked: false, Signed: false},
		{Name: "another tag, the signature left as it was", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(layoutOf(sig, fundingTag, commitment)),
			Reads: true, FieldsHex: hx(fundingTag, commitment), SignatureHex: hex.EncodeToString(sig), Locked: true, Signed: false},
		{Name: "a push running past the end of the script", Satoshis: 1, Fields: 2,
			ScriptHex: hex.EncodeToString(cat(head, minimalPush(tag), []byte{0x20}, commitment[:10]))},
		{Name: "no key before OP_CHECKSIG", Satoshis: 1, Fields: 2, ScriptHex: hex.EncodeToString(lock[34:])},
		{Name: "nothing", Satoshis: 1, Fields: 2, ScriptHex: ""},
	}

	// A signature's encoding alone.
	pad := func(b []byte) []byte { return append([]byte{0}, b...) }
	for _, c := range []struct {
		name   string
		strict bool
		b      []byte
	}{
		{"the token's field signature", true, sig},
		{"its high-S twin", false, highS},
		{"R padded with a zero byte", false, der{r: pad(d.r), s: d.s}.encode()},
		{"S padded with a zero byte", false, der{r: d.r, s: pad(d.s)}.encode()},
		{"a sequence length one too long", false, func() []byte { b := bytes.Clone(sig); b[1]++; return b }()},
		{"a byte after the sequence", false, append(bytes.Clone(sig), 0)},
		{"a zero S", false, der{r: d.r, s: []byte{0}}.encode()},
		{"R equal to the group order", false, der{r: minimalInt(order), s: d.s}.encode()},
		{"seven bytes", false, []byte{0x30, 0x05, 0x02, 0x01, 0x01, 0x02, 0x00}},
		{"nothing", false, nil},
	} {
		v.Signatures = append(v.Signatures, derCase{Name: c.name, Strict: c.strict, Hex: hex.EncodeToString(c.b)})
	}
	return v, nil
}

// tailOf splits a canonical signed lock of n short fields into the fields
// and the signature, each behind a direct push.
func tailOf(lock []byte, n int) ([][]byte, []byte, error) {
	p := lock[35:]
	var out [][]byte
	for i := 0; i <= n; i++ {
		if len(p) < 1 || p[0] < 1 || p[0] > 75 || len(p) < 1+int(p[0]) {
			return nil, nil, fmt.Errorf("field %d is not a direct push", i)
		}
		out = append(out, p[1:1+int(p[0])])
		p = p[1+int(p[0]):]
	}
	return out[:n], out[n], nil
}
