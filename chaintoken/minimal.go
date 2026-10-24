package chaintoken

import (
	"errors"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Minimal is mp cut to the minimal path of txid (MinimalPath): at the
// lowest level txid, flagged as the txid, and its sibling; above that the
// one sibling each level needs, computed from the leaves below when mp
// holds only those. mp is not changed, and the result proves the same root.
//
// A host stores a mined token with the path it was published with, which
// is a merged path (MergedPath) when the token mined in its parent's block.
// A replayer that assembles the token's BEEF with a child of another block
// cuts that path first, since each path of a token's BEEF must be minimal
// unless the two share a block (Wire.Token).
func Minimal(mp *transaction.MerklePath, txid chainhash.Hash) (*transaction.MerklePath, error) {
	if mp == nil || len(mp.Path) == 0 {
		return nil, errors.New("chaintoken: no proof")
	}
	index := transaction.IndexedPath(make([]map[uint64]*transaction.PathElement, len(mp.Path)))
	var at *transaction.PathElement
	for l, leaves := range mp.Path {
		index[l] = map[uint64]*transaction.PathElement{}
		for _, e := range leaves {
			index[l][e.Offset] = e
			if l == 0 && e.Hash != nil && *e.Hash == txid {
				at = e
			}
		}
	}
	if at == nil {
		return nil, fmt.Errorf("chaintoken: the proof does not hold %s", txid)
	}
	isTxid := true
	leaf := txid
	path := make([][]*transaction.PathElement, len(mp.Path))
	path[0] = []*transaction.PathElement{{Offset: at.Offset, Hash: &leaf, Txid: &isTxid}}
	for l := range mp.Path {
		off := at.Offset>>uint(l) ^ 1
		e := index.GetOffsetLeaf(l, off)
		if e == nil {
			return nil, fmt.Errorf("chaintoken: the proof lacks the sibling at level %d", l)
		}
		sib := &transaction.PathElement{Offset: off}
		if e.Duplicate != nil && *e.Duplicate {
			dup := true
			sib.Duplicate = &dup
		} else {
			h := *e.Hash
			sib.Hash = &h
		}
		path[l] = append(path[l], sib)
	}
	sort.Slice(path[0], func(i, j int) bool { return path[0][i].Offset < path[0][j].Offset })
	out := transaction.NewMerklePath(mp.BlockHeight, path)
	want, err := mp.ComputeRoot(&txid)
	if err != nil {
		return nil, err
	}
	got, err := out.ComputeRoot(&txid)
	if err != nil || *got != *want {
		return nil, fmt.Errorf("chaintoken: the minimal proof of %s does not reach the block's root", txid)
	}
	return out, nil
}
