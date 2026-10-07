package mint

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// Rate is a fee rate exactly as miners publish it: Sats satoshis per Bytes
// bytes (ARC's policy.miningFee, whose field names the JSON form keeps, so
// a policy answer pastes into a configuration). {1, 1} is one satoshi a
// byte, {100, 1000} is 100 satoshis a kilobyte, {1, 1000000} one satoshi a
// megabyte. Every computation on it is integer arithmetic; nothing here
// converts it to a float.
type Rate struct {
	Sats  uint64 `json:"satoshis"`
	Bytes uint64 `json:"bytes"`
}

// IsZero reports the zero Rate, which a Fees reads as "use SatPerByte".
func (r Rate) IsZero() bool { return r.Sats == 0 && r.Bytes == 0 }

// String is the rate as ParseRate reads it: sats/bytes.
func (r Rate) String() string {
	return strconv.FormatUint(r.Sats, 10) + "/" + strconv.FormatUint(r.Bytes, 10)
}

// Fee is ceil(size * Sats / Bytes), with no floor. Bytes zero, or a fee
// that does not fit in 64 bits, is an error wrapping ErrRate.
func (r Rate) Fee(size uint64) (uint64, error) {
	if r.Bytes == 0 {
		return 0, fmt.Errorf("%w: %d satoshis over zero bytes", ErrRate, r.Sats)
	}
	hi, lo := bits.Mul64(size, r.Sats)
	if hi >= r.Bytes {
		// The quotient would not fit in 64 bits (and bits.Div64 panics).
		return 0, fmt.Errorf("%w: %d bytes at %s overflows", ErrRate, size, r)
	}
	q, rem := bits.Div64(hi, lo, r.Bytes)
	if rem != 0 {
		if q == ^uint64(0) {
			return 0, fmt.Errorf("%w: %d bytes at %s overflows", ErrRate, size, r)
		}
		q++
	}
	return q, nil
}

// Cmp compares two rates by value: -1 when r is the lower, 0 when they are
// equal (100/1000 equals 1/10), +1 when r is the higher. A rate over zero
// bytes compares as the highest there is, so a guard never mistakes it for
// a cheap one.
func (r Rate) Cmp(o Rate) int {
	switch {
	case r.Bytes == 0 && o.Bytes == 0:
		return 0
	case r.Bytes == 0:
		return 1
	case o.Bytes == 0:
		return -1
	}
	// r.Sats/r.Bytes against o.Sats/o.Bytes, cross-multiplied in 128 bits.
	ah, al := bits.Mul64(r.Sats, o.Bytes)
	bh, bl := bits.Mul64(o.Sats, r.Bytes)
	switch {
	case ah < bh || ah == bh && al < bl:
		return -1
	case ah == bh && al == bl:
		return 0
	}
	return 1
}

// Clamp is r held to [lo, hi]: lo when r is below it, hi when r is above
// it. A zero bound is no bound.
func (r Rate) Clamp(lo, hi Rate) Rate {
	if !lo.IsZero() && r.Cmp(lo) < 0 {
		r = lo
	}
	if !hi.IsZero() && r.Cmp(hi) > 0 {
		r = hi
	}
	return r
}

// ParseRate reads a rate written as sats/bytes ("100/1000") or as a whole
// number of satoshis a byte ("1", which is 1/1). Bytes must be at least 1.
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	num, den, slash := strings.Cut(s, "/")
	sats, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: %q is not sats/bytes", ErrRate, s)
	}
	r := Rate{Sats: sats, Bytes: 1}
	if slash {
		if r.Bytes, err = strconv.ParseUint(strings.TrimSpace(den), 10, 64); err != nil || r.Bytes == 0 {
			return Rate{}, fmt.Errorf("%w: %q is not sats/bytes with bytes at least 1", ErrRate, s)
		}
	}
	return r, nil
}
