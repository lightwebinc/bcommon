// Package chaintoken reads and writes the BEEF a host admits a mined chain
// token in, and the token's own output.
//
// A chain token is a mined PushDrop output that the next token spends, so
// that a chain of them is an ordered state a fork of which is a double
// spend. A host that admits one reads its predecessor from the parent the
// submission's BEEF carries, never from what the topic holds, so a verdict
// depends on the submission's bytes and the host's headers alone. That only
// works when a BEEF holds exactly what the object needs: nobody can attach
// anything to it, and two hosts given the same bytes decide the same.
//
// The package holds the parts of that rule no application's record enters:
//
//   - ReadWire reads a BEEF exactly as declared on the wire, before any
//     parser merges two proofs of one block or collapses a transaction
//     listed twice, so a rule counts what was sent;
//   - MinimalPath and MergedPath hold a Merkle path to exactly the leaves a
//     proof needs;
//   - Wire.Token, Wire.Carrier and Wire.Alone are the three shapes: a token
//     with its parent, a carrier with its funding tree, and a mined
//     transaction alone;
//   - TokenBEEF assembles the first shape from a stored token and its
//     parent, which is how a replayer brings a chain to another host;
//   - ReadOutput, Output.LockedTo and Output.SignedBy read a token output
//     and hold it to the canonical script and a strict field signature, and
//     Spends lists what a token spends of its parent.
//
// The application supplies its tags, its record codec, its derivation and
// its transition rules. This package decides no admission.
package chaintoken

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// DefaultMaxBEEF is a BEEF bound for a host that names none, and the floor
// for a host's own: 256 KiB.
const DefaultMaxBEEF = 256 << 10

// The BEEF versions on the wire (BRC-62, BRC-96, BRC-95).
const (
	beefV1     = uint32(0xEFBE0001)
	beefV2     = uint32(0xEFBE0002)
	beefAtomic = uint32(0x01010101)
)

// ErrBEEF is a BEEF that is over its bound, does not parse as exactly one
// BEEF with nothing after it, lacks its subject, or holds anything but what
// the object needs.
var ErrBEEF = errors.New("chaintoken: BEEF is not exactly what the object needs")

// Entry is one transaction as the BEEF declares it.
type Entry struct {
	// Tx is the transaction, nil for a txid-only entry. Its MerklePath is
	// the BUMP the entry names, and each input's SourceTransaction is the
	// earlier entry that holds it, when one does.
	Tx *transaction.Transaction
	// Txid is the transaction's txid, hash byte order.
	Txid chainhash.Hash
	// Bump is the index of the entry's BUMP, or -1 when it has none.
	Bump int
	// TxidOnly marks a BRC-96 txid-only entry.
	TxidOnly bool
}

// Wire is a BEEF exactly as declared on the wire: its form, its BUMPs and
// its transactions in order.
type Wire struct {
	// Atomic reports an Atomic BEEF (BRC-95).
	Atomic bool
	// Subject is the Atomic BEEF's subject, else the last entry's txid.
	Subject chainhash.Hash
	// Bumps are the BUMPs in the order declared.
	Bumps []*transaction.MerklePath
	// Entries are the transactions in the order declared.
	Entries []Entry

	byTxid map[chainhash.Hash]*Entry
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: malformed BEEF: %s", ErrBEEF, fmt.Sprintf(format, args...))
}

// ReadWire reads a BEEF V1, V2 or Atomic BEEF of at most bound bytes
// (DefaultMaxBEEF when bound is zero) as declared on the wire. The guard
// walks it first, so every count and length it declares is held to the
// bytes present before anything is allocated for it. It then links each
// transaction's inputs to the earlier entries that hold their source
// transactions, and each proven transaction to its BUMP.
//
// Every refusal wraps ErrBEEF: over the bound, not one well-formed BEEF,
// bytes after it, no transaction, or no subject transaction carried whole.
func ReadWire(b []byte, bound int) (*Wire, error) {
	if bound == 0 {
		bound = DefaultMaxBEEF
	}
	if err := guard.CheckBEEF(b, bound); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBEEF, err)
	}
	w := &Wire{byTxid: map[chainhash.Hash]*Entry{}}
	r := bytes.NewReader(b)
	var v uint32
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return nil, malformed("no version")
	}
	if v == beefAtomic {
		w.Atomic = true
		if _, err := io.ReadFull(r, w.Subject[:]); err != nil {
			return nil, malformed("no subject")
		}
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, malformed("no version")
		}
	}
	if v != beefV1 && v != beefV2 {
		return nil, malformed("version %08x", v)
	}
	n, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		mp, err := transaction.NewMerklePathFromReader(r)
		if err != nil {
			return nil, malformed("%v", err)
		}
		w.Bumps = append(w.Bumps, mp)
	}
	if n, err = readVarInt(r); err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		e := Entry{Bump: -1}
		format := byte(0)
		if v == beefV2 {
			if format, err = r.ReadByte(); err != nil || format > 2 {
				return nil, malformed("data format")
			}
		}
		if format == 2 {
			e.TxidOnly = true
			if _, err := io.ReadFull(r, e.Txid[:]); err != nil {
				return nil, malformed("txid")
			}
		} else {
			if format == 1 { // V2: the BUMP index precedes the transaction
				if e.Bump, err = readBumpIndex(r, len(w.Bumps)); err != nil {
					return nil, err
				}
			}
			tx := &transaction.Transaction{}
			if _, err := tx.ReadFrom(r); err != nil {
				return nil, malformed("%v", err)
			}
			e.Tx, e.Txid = tx, *tx.TxID()
			if v == beefV1 { // V1: a flag and the index follow the transaction
				f, err := r.ReadByte()
				if err != nil || f > 1 {
					return nil, malformed("has-BUMP flag")
				}
				if f == 1 {
					if e.Bump, err = readBumpIndex(r, len(w.Bumps)); err != nil {
						return nil, err
					}
				}
			}
			if e.Bump >= 0 {
				tx.MerklePath = w.Bumps[e.Bump]
			}
		}
		w.Entries = append(w.Entries, e)
	}
	if r.Len() != 0 || len(w.Entries) == 0 {
		return nil, malformed("trailing bytes or no transaction")
	}
	if !w.Atomic {
		w.Subject = w.Entries[len(w.Entries)-1].Txid
	}
	for i := range w.Entries {
		e := &w.Entries[i]
		if e.Tx != nil {
			for _, in := range e.Tx.Inputs {
				if src := w.byTxid[*in.SourceTXID]; src != nil && src.Tx != nil {
					in.SourceTransaction = src.Tx
				}
			}
		}
		w.byTxid[e.Txid] = e
	}
	if s := w.byTxid[w.Subject]; s == nil || s.Tx == nil {
		return nil, malformed("no subject transaction")
	}
	return w, nil
}

// SubjectTx is the subject transaction, which ReadWire has held to be
// present and carried whole.
func (w *Wire) SubjectTx() *transaction.Transaction { return w.byTxid[w.Subject].Tx }

// Entry is the entry for txid, or nil. When a BEEF lists one txid twice it
// is the later one.
func (w *Wire) Entry(txid chainhash.Hash) *Entry { return w.byTxid[txid] }

// AnyTxidOnly reports a BRC-96 txid-only entry. Every shape here carries
// each transaction whole.
func (w *Wire) AnyTxidOnly() bool {
	for _, e := range w.Entries {
		if e.TxidOnly {
			return true
		}
	}
	return false
}

func readBumpIndex(r *bytes.Reader, n int) (int, error) {
	idx, err := readVarInt(r)
	if err != nil || idx >= uint64(n) {
		return -1, malformed("BUMP index")
	}
	return int(idx), nil
}

func readVarInt(r *bytes.Reader) (uint64, error) {
	h, err := r.ReadByte()
	if err != nil {
		return 0, malformed("count")
	}
	var n int
	switch h {
	case 0xfd:
		n = 2
	case 0xfe:
		n = 4
	case 0xff:
		n = 8
	default:
		return uint64(h), nil
	}
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, malformed("count")
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// pathSet is the leaves a path holds at each level: offset to whether the
// leaf is flagged as a txid.
func pathSet(mp *transaction.MerklePath) ([]map[uint64]bool, bool) {
	out := make([]map[uint64]bool, len(mp.Path))
	for h, level := range mp.Path {
		out[h] = map[uint64]bool{}
		for _, e := range level {
			if _, dup := out[h][e.Offset]; dup {
				return nil, false
			}
			out[h][e.Offset] = e.Txid != nil && *e.Txid
		}
	}
	return out, true
}

// txidOffset finds txid at the lowest level, flagged as a txid.
func txidOffset(mp *transaction.MerklePath, txid chainhash.Hash) (uint64, bool) {
	if len(mp.Path) == 0 {
		return 0, false
	}
	for _, e := range mp.Path[0] {
		if e.Hash != nil && *e.Hash == txid && e.Txid != nil && *e.Txid {
			return e.Offset, true
		}
	}
	return 0, false
}

// MinimalPath reports whether mp proves txid with only the leaves that
// needs: at the lowest level the txid, flagged as one, and its sibling (a
// hash or a duplicate), and above that exactly the one sibling each level
// needs. A proof carrying anything else carries bytes a host would store and
// serve for nothing.
//
// It judges the leaves of the levels the path has, not how many levels
// there are or what root they give: whether the proof is true is the
// header check's (Mined).
func MinimalPath(mp *transaction.MerklePath, txid chainhash.Hash) bool {
	return MergedPath(mp, txid, txid)
}

// MergedPath reports whether mp is the union of the minimal paths of a and
// b (a == b for one transaction): at the lowest level a and b, each flagged
// as a txid and no other leaf flagged, and their siblings; above that only
// the siblings of their ancestors. A sibling that is itself an ancestor of
// the other transaction can be computed and MAY be left out, as BEEF
// writers do when they merge two paths of one block; every other sibling
// MUST be there, and nothing else may be.
func MergedPath(mp *transaction.MerklePath, a, b chainhash.Hash) bool {
	if mp == nil {
		return false
	}
	set, ok := pathSet(mp)
	if !ok || len(set) == 0 {
		return false
	}
	oa, okA := txidOffset(mp, a)
	ob, okB := txidOffset(mp, b)
	if !okA || !okB {
		return false
	}
	flagged := 0
	for _, isTxid := range set[0] {
		if isTxid {
			flagged++
		}
	}
	want := 2
	if a == b {
		want = 1
	}
	if flagged != want {
		return false
	}
	for h := range set {
		ancestors := map[uint64]bool{oa >> uint(h): true, ob >> uint(h): true}
		allowed := map[uint64]bool{}
		for x := range ancestors {
			allowed[x^1] = true
			if h == 0 {
				allowed[x] = true
			}
		}
		for off := range set[h] {
			if !allowed[off] {
				return false
			}
		}
		for x := range ancestors {
			s := x ^ 1
			if h == 0 && !set[0][x] {
				return false // the txid itself
			}
			if _, have := set[h][s]; !have && !ancestors[s] {
				return false // a sibling that cannot be computed
			}
		}
	}
	return true
}

// Token is the shape a mined token is admitted in: a BEEF V1 or V2, never
// an Atomic BEEF, of exactly two transactions carried whole, tx and one
// other that an input of tx spends, each proven: two minimal paths of two
// blocks, or one merged path when both are in one block. Two separate paths
// of one block are refused, since BEEF writers merge them. It returns the
// other transaction's entry, the token's parent.
//
// BRC-95 allows in an Atomic BEEF only the subject and the ancestors needed
// to validate its inputs, and a mined token needs none, so its parent would
// be an unrelated transaction there.
func (w *Wire) Token(tx *transaction.Transaction) (*Entry, error) {
	if w.Atomic || w.AnyTxidOnly() || len(w.Entries) != 2 {
		return nil, fmt.Errorf("%w: a token is a BEEF V1 or V2 of exactly the token and its parent", ErrBEEF)
	}
	txid := *tx.TxID()
	self := w.byTxid[txid]
	var parent *Entry
	for i := range w.Entries {
		if w.Entries[i].Txid != txid {
			parent = &w.Entries[i]
		}
	}
	if self == nil || parent == nil || parent.Tx == nil || self.Bump < 0 || parent.Bump < 0 {
		return nil, fmt.Errorf("%w: the token and its parent, each proven", ErrBEEF)
	}
	spent := false
	for _, in := range tx.Inputs {
		if *in.SourceTXID == parent.Txid {
			spent = true
		}
	}
	if !spent {
		return nil, fmt.Errorf("%w: no input spends the other transaction", ErrBEEF)
	}
	switch len(w.Bumps) {
	case 2:
		if w.Bumps[0].BlockHeight == w.Bumps[1].BlockHeight {
			return nil, fmt.Errorf("%w: two paths of one block, which BEEF writers merge", ErrBEEF)
		}
		if self.Bump == parent.Bump || !MinimalPath(w.Bumps[self.Bump], txid) || !MinimalPath(w.Bumps[parent.Bump], parent.Txid) {
			return nil, fmt.Errorf("%w: two minimal paths, one each", ErrBEEF)
		}
	case 1:
		if !MergedPath(w.Bumps[0], txid, parent.Txid) {
			return nil, fmt.Errorf("%w: one merged path flagging exactly both txids", ErrBEEF)
		}
	default:
		return nil, fmt.Errorf("%w: %d BUMPs", ErrBEEF, len(w.Bumps))
	}
	return parent, nil
}

// Carrier is the shape an unmined carrier is admitted in: exactly two
// transactions carried whole and one BUMP, tx unproven and the transaction
// its first input spends proven by that BUMP, minimally. It returns that
// transaction's entry, the carrier's funding tree.
func (w *Wire) Carrier(tx *transaction.Transaction) (*Entry, error) {
	if w.AnyTxidOnly() || len(w.Entries) != 2 || len(w.Bumps) != 1 {
		return nil, fmt.Errorf("%w: %d BUMPs and %d transactions", ErrBEEF, len(w.Bumps), len(w.Entries))
	}
	if len(tx.Inputs) == 0 {
		return nil, fmt.Errorf("%w: a carrier of no inputs", ErrBEEF)
	}
	parent := *tx.Inputs[0].SourceTXID
	self, fund := w.byTxid[*tx.TxID()], w.byTxid[parent]
	if self == nil || fund == nil || self.Bump != -1 || fund.Bump != 0 || !MinimalPath(w.Bumps[0], parent) {
		return nil, fmt.Errorf("%w: not the carrier and its funding tree's minimal path", ErrBEEF)
	}
	return fund, nil
}

// Alone is the shape a mined transaction with no parent to read is admitted
// in, a sweep for one: exactly one transaction, tx, proven by the one BUMP,
// its own minimal path.
func (w *Wire) Alone(tx *transaction.Transaction) error {
	if w.AnyTxidOnly() || len(w.Entries) != 1 || len(w.Bumps) != 1 || w.Entries[0].Bump != 0 || !MinimalPath(w.Bumps[0], *tx.TxID()) {
		return fmt.Errorf("%w: one transaction and its own minimal path", ErrBEEF)
	}
	return nil
}
