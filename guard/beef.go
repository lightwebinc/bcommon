package guard

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// ErrBEEF is every refusal of CheckBEEF and ParseBEEF that the walk makes;
// the SDK's own refusals of a BEEF the walk passed are returned as the SDK
// words them.
var ErrBEEF = errors.New("guard: BEEF refused")

// ErrTransaction is every refusal of ParseTransaction that the walk makes.
var ErrTransaction = errors.New("guard: transaction refused")

// DefaultBound is the size bound this module's own callers pass for a BEEF
// or a transaction: 64 MiB, the bound hostset puts on a whole lookup answer,
// so a BEEF inside an answer cannot have been larger. An application
// calling the guard directly passes the bound it was prepared to read.
const DefaultBound = 64 << 20

// The version words, as the first four bytes read little-endian.
const (
	beefV1     uint32 = 0xEFBE0001
	beefV2     uint32 = 0xEFBE0002
	beefAtomic uint32 = 0x01010101
)

// The smallest encodings, which make every count bound a floor: an input is
// an outpoint, an empty-script length and a sequence; an output a value and
// an empty-script length; a transaction a version, one input, an output
// count and a locktime; a BUMP inside a BEEF a height, a tree height and one
// level count.
const (
	minInputBytes  = 32 + 4 + 1 + 4
	minOutputBytes = 8 + 1
	minTxBytes     = 4 + 1 + minInputBytes + 1 + 4
	minBEEFBump    = 3
	// A V1 entry is a transaction and its has-BUMP byte. A V2 entry is at
	// least a txid-only entry: a format byte and a txid.
	minV1Entry = minTxBytes + 1
	minV2Entry = 1 + 32
)

// V2 data formats (BRC-96).
const (
	formatRawTx     = 0
	formatRawTxBump = 1
	formatTxidOnly  = 2
)

// CheckBEEF walks a BEEF of at most bound bytes and refuses, as ErrBEEF,
// anything whose declared counts or lengths overrun the bytes present or
// that leaves bytes over. It admits BEEF V1 (BRC-62), BEEF V2 (BRC-96) with
// its txid-only entries, and Atomic BEEF (BRC-95) around either, and
// allocates nothing, unless a transaction with no proof has more than one
// input: only such a BEEF can exceed Limits.Ancestry, and it is walked a
// second time into a table of its transactions. After it returns nil no count in the BEEF can ask the SDK
// for more than the bytes hold, and no transaction's ancestry for more
// steps than Limits.Ancestry.
//
// It is a structural check, not a second parser: whether the transactions
// are valid, the proofs prove them and an Atomic BEEF holds its subject are
// the SDK's to decide, and SPV's after it. It is stricter than the SDK in
// three places, each a shape no BEEF this module writes has: trailing bytes,
// a BEEF of no transactions, and a transaction of no inputs (which is also
// the extended-format marker's shape) are refused.
//
// It also holds the BEEF to DefaultLimits; CheckBEEFWithin takes others.
func CheckBEEF(b []byte, bound int) error {
	return checkBEEF(b, bound, DefaultLimits())
}

func checkBEEF(b []byte, bound int, l Limits) error {
	if len(b) > bound {
		return fmt.Errorf("%w: %d bytes, max %d", ErrBEEF, len(b), bound)
	}
	c := &cursor{b: b, lim: l}
	if len(b) >= 4 && binary.LittleEndian.Uint32(b) == beefAtomic {
		if err := c.skip(4 + 32); err != nil { // marker and subject txid
			return fmt.Errorf("%w: atomic header %v", ErrBEEF, err)
		}
	}
	if err := c.beef(); err != nil {
		return fmt.Errorf("%w: %v", ErrBEEF, err)
	}
	if c.remaining() != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrBEEF, c.remaining())
	}
	return nil
}

// ParseBEEF is CheckBEEF, then the SDK's transaction.ParseBeef under a
// recover, so a malformed BEEF is an error and never a crash. Its results
// are ParseBeef's: the BEEF, its subject transaction (which may be nil) and
// the subject's txid.
func ParseBEEF(b []byte, bound int) (beef *transaction.Beef, tx *transaction.Transaction, txid *chainhash.Hash, err error) {
	return parseBEEF(b, bound, DefaultLimits())
}

func parseBEEF(b []byte, bound int, l Limits) (beef *transaction.Beef, tx *transaction.Transaction, txid *chainhash.Hash, err error) {
	if err := checkBEEF(b, bound, l); err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			beef, tx, txid, err = nil, nil, nil, fmt.Errorf("beef parse panicked: %v", r)
		}
	}()
	return transaction.ParseBeef(b)
}

// ParseTransaction walks one raw transaction of at most bound bytes as
// CheckBEEF walks each of a BEEF's, refusing as ErrTransaction, then parses
// it with the SDK under a recover. An Extended Format transaction is refused
// as one of no inputs; RawTransaction turns it into the raw form first.
func ParseTransaction(b []byte, bound int) (tx *transaction.Transaction, err error) {
	return parseTransaction(b, bound, DefaultLimits())
}

func parseTransaction(b []byte, bound int, l Limits) (tx *transaction.Transaction, err error) {
	if len(b) > bound {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrTransaction, len(b), bound)
	}
	c := &cursor{b: b, lim: l}
	if err := c.tx(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransaction, err)
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrTransaction, c.remaining())
	}
	defer func() {
		if r := recover(); r != nil {
			tx, err = nil, fmt.Errorf("transaction parse panicked: %v", r)
		}
	}()
	return transaction.NewTransactionFromBytes(b)
}

// beef walks a V1 or V2 BEEF, not another Atomic wrapper. The ancestry is
// counted only when it can exceed the number of transactions: while every
// transaction without a proof has one input, each has one line of
// ancestors and a walk visits no transaction twice.
func (c *cursor) beef() error {
	start := c.pos
	branches, err := c.beefWalk(false)
	if err != nil || c.lim.Ancestry <= 0 {
		return err
	}
	// Without a branch no ancestry is longer than the transactions there
	// are, so the count is needed only when those alone could exceed it.
	if !branches && c.nTx <= uint64(c.lim.Ancestry) {
		return nil
	}
	c.pos = start
	_, err = c.beefWalk(true)
	return err
}

// beefWalk is one walk of a BEEF. It reports whether a transaction without
// a proof has more than one input, and with count set it counts the
// ancestry.
func (c *cursor) beefWalk(count bool) (bool, error) {
	branches := false
	err := c.beefBody(count, &branches)
	return branches, err
}

func (c *cursor) beefBody(count bool, branches *bool) error {
	v, err := c.uint32LE()
	if err != nil {
		return err
	}
	if v != beefV1 && v != beefV2 {
		return fmt.Errorf("version %#x is not BEEF V1 or V2", v)
	}
	nBumps, err := c.varInt()
	if err != nil {
		return err
	}
	if !c.fits(nBumps, minBEEFBump) {
		return fmt.Errorf("declares %d BUMPs, %d bytes remain", nBumps, c.remaining())
	}
	if over(nBumps, c.lim.BUMPs) {
		return limitErr("BUMPs", nBumps, c.lim.BUMPs)
	}
	for i := uint64(0); i < nBumps; i++ {
		h, err := c.bump()
		if err != nil {
			return fmt.Errorf("BUMP %d: %w", i, err)
		}
		if h == 0 {
			return fmt.Errorf("BUMP %d has no levels", i)
		}
	}
	minEntry := minV2Entry
	if v == beefV1 {
		minEntry = minV1Entry
	}
	nTx, err := c.varInt()
	if err != nil {
		return err
	}
	if !c.fits(nTx, minEntry) {
		return fmt.Errorf("declares %d transactions, %d bytes remain", nTx, c.remaining())
	}
	if nTx == 0 {
		return errors.New("no transactions")
	}
	c.nTx = nTx
	if over(nTx, c.lim.Transactions) {
		return limitErr("transactions", nTx, c.lim.Transactions)
	}
	// For the count, each transaction's id, whether it has a proof, and
	// where its inputs' previous ids lie are kept. The table is made only
	// then, so a walk that does not count allocates nothing.
	var anc *ancTable
	if count {
		anc = &ancTable{}
		c.prevs = &anc.prevs
		defer func() { c.prevs = nil }()
	}
	bumpIndex := func(i uint64) error {
		idx, err := c.varInt()
		if err != nil {
			return err
		}
		if idx >= nBumps {
			return fmt.Errorf("transaction %d names BUMP %d of %d", i, idx, nBumps)
		}
		return nil
	}
	for i := uint64(0); i < nTx; i++ {
		if v == beefV1 {
			start, from := c.pos, anc.mark()
			if err := c.tx(); err != nil {
				return fmt.Errorf("transaction %d: %w", i, err)
			}
			end, nIn := c.pos, c.nIn
			has, err := c.byteAt()
			if err != nil {
				return err
			}
			switch has {
			case 0:
			case 1:
				if err := bumpIndex(i); err != nil {
					return err
				}
			default:
				return fmt.Errorf("transaction %d has-BUMP byte %d", i, has)
			}
			if has == 0 && nIn > 1 {
				*branches = true
			}
			anc.add(c.b[start:end], has == 1, from)
			continue
		}
		format, err := c.byteAt()
		if err != nil {
			return err
		}
		switch format {
		case formatRawTx:
		case formatRawTxBump:
			if err := bumpIndex(i); err != nil {
				return err
			}
		case formatTxidOnly:
			if err := c.skip(32); err != nil {
				return err
			}
			// Known to the reader, so a walk stops at it as at a proof.
			anc.known(c.b[c.pos-32 : c.pos])
			continue
		default:
			return fmt.Errorf("transaction %d data format %d", i, format)
		}
		start, from := c.pos, anc.mark()
		if err := c.tx(); err != nil {
			return fmt.Errorf("transaction %d: %w", i, err)
		}
		if format == formatRawTx && c.nIn > 1 {
			*branches = true
		}
		anc.add(c.b[start:c.pos], format == formatRawTxBump, from)
	}
	if anc != nil {
		return c.ancestry(anc.entries, anc.prevs)
	}
	return nil
}

// ancTable is what the ancestry count keeps of a BEEF. Its methods do
// nothing on a nil table, which is a walk that does not count.
type ancTable struct {
	entries []ancEntry
	prevs   []int
}

// mark is where the next transaction's inputs begin in prevs.
func (a *ancTable) mark() int {
	if a == nil {
		return 0
	}
	return len(a.prevs)
}

// add records a transaction from its raw bytes.
func (a *ancTable) add(raw []byte, proven bool, from int) {
	if a == nil {
		return
	}
	first := sha256.Sum256(raw)
	a.entries = append(a.entries, ancEntry{txid: sha256.Sum256(first[:]), proven: proven, from: from, to: len(a.prevs)})
}

// known records a txid-only entry.
func (a *ancTable) known(txid []byte) {
	if a == nil {
		return
	}
	e := ancEntry{proven: true, from: len(a.prevs), to: len(a.prevs)}
	copy(e.txid[:], txid)
	a.entries = append(a.entries, e)
}

// ancEntry is one transaction of a BEEF for the ancestry count: its id,
// whether a walk stops at it, and its inputs' previous ids as offsets
// prevs[from:to] into the BEEF.
type ancEntry struct {
	txid     [32]byte
	proven   bool
	from, to int
}

// ancestry refuses a BEEF holding a transaction whose ancestry takes more
// than Limits.Ancestry steps to walk (see Limits.Ancestry). The count does
// not depend on the order the transactions are written in.
func (c *cursor) ancestry(entries []ancEntry, prevs []int) error {
	limit := uint64(c.lim.Ancestry) //nolint:gosec // positive, checked by the caller
	index := make(map[[32]byte]int, len(entries))
	for i := range entries {
		// A transaction written twice would be counted as one of its
		// copies and walked as the other, so it is refused: no writer has
		// a reason to repeat one.
		if _, twice := index[entries[i].txid]; twice {
			return fmt.Errorf("transaction %d is in the BEEF twice", i)
		}
		index[entries[i].txid] = i
	}
	const unknown, open = 0, ^uint64(0)
	steps := make([]uint64, len(entries))
	var walk func(i int) uint64
	walk = func(i int) uint64 {
		switch steps[i] {
		case unknown:
		case open:
			// A transaction among its own ancestors: no hash allows it, and
			// counting it once more ends the walk.
			return 1
		default:
			return steps[i]
		}
		e := &entries[i]
		if e.proven {
			steps[i] = 1
			return 1
		}
		steps[i] = open
		n := uint64(1)
		for _, at := range prevs[e.from:e.to] {
			var prev [32]byte
			copy(prev[:], c.b[at:at+32])
			if j, ok := index[prev]; ok {
				n += walk(j)
			}
			if n > limit {
				// Saturated: more than the limit is all a caller asks.
				n = limit + 1
				break
			}
		}
		steps[i] = n
		return n
	}
	for i := range entries {
		if walk(i) > limit {
			return fmt.Errorf("transaction %d has an ancestry of more than %d steps to walk", i, limit)
		}
	}
	return nil
}

// tx walks one raw transaction.
func (c *cursor) tx() error {
	if c.remaining() < minTxBytes {
		return errTruncated
	}
	c.pos += 4 // version
	nIn, err := c.compact()
	if err != nil {
		return err
	}
	if nIn == 0 {
		return errors.New("no inputs")
	}
	c.nIn = nIn
	if !c.fits(nIn, minInputBytes) {
		return fmt.Errorf("declares %d inputs, %d bytes remain", nIn, c.remaining())
	}
	if over(nIn, c.lim.Inputs) {
		return limitErr("inputs", nIn, c.lim.Inputs)
	}
	for i := uint64(0); i < nIn; i++ {
		if err := c.skip(32 + 4); err != nil { // outpoint
			return err
		}
		if c.prevs != nil {
			*c.prevs = append(*c.prevs, c.pos-36)
		}
		l, err := c.script()
		if err != nil {
			return fmt.Errorf("input %d script %w", i, err)
		}
		if err := c.skipN(l); err != nil {
			return fmt.Errorf("input %d script %w", i, err)
		}
		if err := c.skip(4); err != nil { // sequence
			return err
		}
	}
	nOut, err := c.compact()
	if err != nil {
		return err
	}
	if !c.fits(nOut, minOutputBytes) {
		return fmt.Errorf("declares %d outputs, %d bytes remain", nOut, c.remaining())
	}
	if over(nOut, c.lim.Outputs) {
		return limitErr("outputs", nOut, c.lim.Outputs)
	}
	for i := uint64(0); i < nOut; i++ {
		if err := c.skip(8); err != nil { // value
			return err
		}
		l, err := c.script()
		if err != nil {
			return fmt.Errorf("output %d script %w", i, err)
		}
		if err := c.skipN(l); err != nil {
			return fmt.Errorf("output %d script %w", i, err)
		}
	}
	return c.skip(4) // locktime
}

// script reads a script's length and holds it to the Script limit; the
// caller still bounds it by the bytes present.
func (c *cursor) script() (uint64, error) {
	l, err := c.compact()
	if err != nil {
		return 0, err
	}
	if over(l, c.lim.Script) {
		return 0, fmt.Errorf("declares %d bytes, the limit is %d", l, c.lim.Script)
	}
	return l, nil
}
