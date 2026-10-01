package guard

import (
	"errors"
	"fmt"
)

// efMarker follows the version in an Extended Format transaction (BRC-30).
// In a raw transaction those bytes would be an input count of zero, which
// ParseTransaction refuses, so the two forms cannot be confused.
var efMarker = [6]byte{0, 0, 0, 0, 0, 0xEF}

// minEFInputBytes is the smallest Extended Format input: a raw input, then
// the previous output's satoshis and an empty locking-script length.
const minEFInputBytes = minInputBytes + 8 + 1

// IsEF reports whether b carries the Extended Format marker after its
// version. It says nothing of whether the rest of b is well formed.
func IsEF(b []byte) bool {
	return len(b) >= 10 && [6]byte(b[4:10]) == efMarker
}

// RawTransaction returns the raw transaction b is, given either the raw
// form or the Extended Format (BRC-30) that extends it, of at most bound
// bytes. A raw b is walked as ParseTransaction walks it and returned as it
// came; an Extended Format b is walked the same way, its marker and each
// input's previous satoshis and locking script included, and its raw form
// returned in a new slice no longer than b. The txid is the raw form's.
//
// The previous outputs an Extended Format transaction carries are dropped:
// they are the sender's claim about other transactions, which nothing here
// can check, and whoever signs against an input reads the output it spends
// from that output's own transaction.
//
// Every refusal is ErrTransaction, and the walk bounds every count and
// length by the bytes present before anything is allocated.
func RawTransaction(b []byte, bound int) ([]byte, error) {
	if len(b) > bound {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrTransaction, len(b), bound)
	}
	if !IsEF(b) {
		c := &cursor{b: b}
		if err := c.tx(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTransaction, err)
		}
		if c.remaining() != 0 {
			return nil, fmt.Errorf("%w: %d trailing bytes", ErrTransaction, c.remaining())
		}
		return b, nil
	}
	n, err := efWalk(b, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: extended format: %v", ErrTransaction, err)
	}
	out := make([]byte, 0, n)
	if _, err := efWalk(b, &out); err != nil {
		return nil, fmt.Errorf("%w: extended format: %v", ErrTransaction, err)
	}
	return out, nil
}

// efWalk walks one Extended Format transaction that must end at b's last
// byte and returns the length of its raw form. With out it also appends that
// raw form to *out: the version, then each input without its previous
// output, then the outputs and lock time as they stand.
func efWalk(b []byte, out *[]byte) (int, error) {
	n := 0
	emit := func(from, to int) {
		n += to - from
		if out != nil {
			*out = append(*out, b[from:to]...)
		}
	}
	c := &cursor{b: b}
	if c.remaining() < 4+len(efMarker)+1+minEFInputBytes+1+4 {
		return 0, errTruncated
	}
	emit(0, 4)
	c.pos = 4 + len(efMarker)
	s := c.pos
	nIn, err := c.varInt()
	if err != nil {
		return 0, err
	}
	if nIn == 0 {
		return 0, errors.New("no inputs")
	}
	if !c.fits(nIn, minEFInputBytes) {
		return 0, fmt.Errorf("declares %d inputs, %d bytes remain", nIn, c.remaining())
	}
	emit(s, c.pos)
	for i := uint64(0); i < nIn; i++ {
		s = c.pos
		if err := c.skip(32 + 4); err != nil { // outpoint
			return 0, err
		}
		l, err := c.varInt()
		if err != nil {
			return 0, err
		}
		if err := c.skipN(l); err != nil {
			return 0, fmt.Errorf("input %d script %w", i, err)
		}
		if err := c.skip(4); err != nil { // sequence
			return 0, err
		}
		emit(s, c.pos)
		if err := c.skip(8); err != nil { // previous satoshis
			return 0, err
		}
		if l, err = c.varInt(); err != nil {
			return 0, err
		}
		if err := c.skipN(l); err != nil {
			return 0, fmt.Errorf("input %d previous locking script %w", i, err)
		}
	}
	s = c.pos
	nOut, err := c.varInt()
	if err != nil {
		return 0, err
	}
	if !c.fits(nOut, minOutputBytes) {
		return 0, fmt.Errorf("declares %d outputs, %d bytes remain", nOut, c.remaining())
	}
	for i := uint64(0); i < nOut; i++ {
		if err := c.skip(8); err != nil { // value
			return 0, err
		}
		l, err := c.varInt()
		if err != nil {
			return 0, err
		}
		if err := c.skipN(l); err != nil {
			return 0, fmt.Errorf("output %d script %w", i, err)
		}
	}
	if err := c.skip(4); err != nil { // lock time
		return 0, err
	}
	if c.remaining() != 0 {
		return 0, fmt.Errorf("%d trailing bytes", c.remaining())
	}
	emit(s, c.pos)
	return n, nil
}
