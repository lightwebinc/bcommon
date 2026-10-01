package guard

import (
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
// allocates nothing. After it returns nil no count in the BEEF can ask the
// SDK for more than the bytes hold.
//
// It is a structural check, not a second parser: whether the transactions
// are valid, the proofs prove them and an Atomic BEEF holds its subject are
// the SDK's to decide, and SPV's after it. It is stricter than the SDK in
// three places, each a shape no BEEF this module writes has: trailing bytes,
// a BEEF of no transactions, and a transaction of no inputs (which is also
// the extended-format marker's shape) are refused.
func CheckBEEF(b []byte, bound int) error {
	if len(b) > bound {
		return fmt.Errorf("%w: %d bytes, max %d", ErrBEEF, len(b), bound)
	}
	c := &cursor{b: b}
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
	if err := CheckBEEF(b, bound); err != nil {
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
	if len(b) > bound {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrTransaction, len(b), bound)
	}
	c := &cursor{b: b}
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

// beef walks a V1 or V2 BEEF, not another Atomic wrapper.
func (c *cursor) beef() error {
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
			if err := c.tx(); err != nil {
				return fmt.Errorf("transaction %d: %w", i, err)
			}
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
			continue
		default:
			return fmt.Errorf("transaction %d data format %d", i, format)
		}
		if err := c.tx(); err != nil {
			return fmt.Errorf("transaction %d: %w", i, err)
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
	nIn, err := c.varInt()
	if err != nil {
		return err
	}
	if nIn == 0 {
		return errors.New("no inputs")
	}
	if !c.fits(nIn, minInputBytes) {
		return fmt.Errorf("declares %d inputs, %d bytes remain", nIn, c.remaining())
	}
	for i := uint64(0); i < nIn; i++ {
		if err := c.skip(32 + 4); err != nil { // outpoint
			return err
		}
		l, err := c.varInt()
		if err != nil {
			return err
		}
		if err := c.skipN(l); err != nil {
			return fmt.Errorf("input %d script %w", i, err)
		}
		if err := c.skip(4); err != nil { // sequence
			return err
		}
	}
	nOut, err := c.varInt()
	if err != nil {
		return err
	}
	if !c.fits(nOut, minOutputBytes) {
		return fmt.Errorf("declares %d outputs, %d bytes remain", nOut, c.remaining())
	}
	for i := uint64(0); i < nOut; i++ {
		if err := c.skip(8); err != nil { // value
			return err
		}
		l, err := c.varInt()
		if err != nil {
			return err
		}
		if err := c.skipN(l); err != nil {
			return fmt.Errorf("output %d script %w", i, err)
		}
	}
	return c.skip(4) // locktime
}
