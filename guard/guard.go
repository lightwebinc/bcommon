// Package guard checks a BRC-74 BUMP someone else supplied before the SDK is
// allowed to allocate for it.
//
// A BUMP reader that sizes a slice from a wire count can be asked, by a
// handful of bytes, for an allocation that ends the process with an
// out-of-memory no recover() sees. go-sdk v1.5.2 bounds its leaf count
// itself, which is why that version is the floor, but whoever serves a proof
// is one more party choosing the bytes, and the rule for length-prefixed data
// from any other party is that a declared length is a claim, checked against
// the bytes present before it sizes memory.
//
// There is no BEEF counterpart: go-sdk v1.5.2's BEEF parser bounds every
// count against the bytes remaining.
package guard

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// ParseBUMP guards and parses one BRC-74 BUMP of at most bound bytes. The
// bound is the caller's, since only the caller knows how large an answer it
// was prepared to read; a bound of zero or less admits nothing.
//
// The parse runs under a recover, so a malformed proof is an error and never
// a crash.
func ParseBUMP(b []byte, bound int) (mp *transaction.MerklePath, err error) {
	if err := guardBUMP(b, bound); err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			mp, err = nil, fmt.Errorf("bump parse panicked: %v", r)
		}
	}()
	return transaction.NewMerklePathFromBinary(b)
}

// maxTreeHeight bounds a path's level count. The wire field is one byte and a
// tree over the whole 64-bit offset space is 64 levels, so this only rejects
// the absurd.
const maxTreeHeight = 64

// minLeafBytes is the smallest encoding of one path element: a 1-byte offset
// varint plus the 1-byte flags. A non-duplicate leaf also carries a 32-byte
// hash, so this is a floor, which is what makes the bound safe.
const minLeafBytes = 2

var errTruncated = errors.New("bump ends mid-structure")

type cursor struct {
	b   []byte
	pos int
}

func (c *cursor) remaining() int { return len(c.b) - c.pos }

func (c *cursor) skip(n int) error {
	if n < 0 || c.remaining() < n {
		return errTruncated
	}
	c.pos += n
	return nil
}

func (c *cursor) byteAt() (byte, error) {
	if c.remaining() < 1 {
		return 0, errTruncated
	}
	v := c.b[c.pos]
	c.pos++
	return v, nil
}

// varInt reads a Bitcoin VarInt. The wide forms are the point of the guard:
// they are how a few bytes name a number no allocation could satisfy.
func (c *cursor) varInt() (uint64, error) {
	p, err := c.byteAt()
	if err != nil {
		return 0, err
	}
	switch p {
	case 0xFD:
		if c.remaining() < 2 {
			return 0, errTruncated
		}
		v := uint64(binary.LittleEndian.Uint16(c.b[c.pos:]))
		c.pos += 2
		return v, nil
	case 0xFE:
		if c.remaining() < 4 {
			return 0, errTruncated
		}
		v := uint64(binary.LittleEndian.Uint32(c.b[c.pos:]))
		c.pos += 4
		return v, nil
	case 0xFF:
		if c.remaining() < 8 {
			return 0, errTruncated
		}
		v := binary.LittleEndian.Uint64(c.b[c.pos:])
		c.pos += 8
		return v, nil
	default:
		return uint64(p), nil
	}
}

// fits reports whether n items of at least each bytes could be encoded in
// what remains.
func (c *cursor) fits(n uint64, each int) bool {
	return n <= uint64(c.remaining()/each)
}

// guardBUMP walks one BRC-74 BUMP allocating nothing and rejects any declared
// count the remaining bytes could not encode. After it returns nil the SDK
// parser cannot be asked for more than the body holds.
func guardBUMP(raw []byte, bound int) error {
	if len(raw) > bound {
		return fmt.Errorf("bump is %d bytes, max %d", len(raw), bound)
	}
	c := &cursor{b: raw}
	if _, err := c.varInt(); err != nil { // block height
		return err
	}
	treeHeight, err := c.byteAt()
	if err != nil {
		return err
	}
	if treeHeight > maxTreeHeight {
		return fmt.Errorf("bump declares tree height %d, max %d", treeHeight, maxTreeHeight)
	}
	for lv := 0; lv < int(treeHeight); lv++ {
		nLeaves, err := c.varInt()
		if err != nil {
			return err
		}
		if !c.fits(nLeaves, minLeafBytes) {
			return fmt.Errorf("bump level %d declares %d leaves, %d bytes remain", lv, nLeaves, c.remaining())
		}
		for lf := uint64(0); lf < nLeaves; lf++ {
			if _, err := c.varInt(); err != nil { // offset
				return err
			}
			flags, err := c.byteAt()
			if err != nil {
				return err
			}
			if flags&1 == 0 { // not a duplicate: a 32-byte hash follows
				if err := c.skip(32); err != nil {
					return err
				}
			}
		}
	}
	if c.remaining() != 0 {
		// Trailing bytes are not "a bigger proof"; they are a body that is
		// not a BUMP, and the SDK would ignore them silently.
		return fmt.Errorf("bump has %d trailing bytes", c.remaining())
	}
	return nil
}
