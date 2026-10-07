package guard

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// ErrPubKey is every refusal of ParsePubKey and ParsePubKeyHex.
var ErrPubKey = errors.New("guard: public key refused")

// fieldPrime is secp256k1's field prime p, written out rather than taken
// from the SDK so that the range check does not rest on the code it guards.
var fieldPrime, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)

// ParsePubKey accepts exactly one encoding of a point: 33 bytes, the prefix
// 0x02 or 0x03, and an x coordinate below the field prime that names a point
// on the curve. Only then is the key handed to the SDK.
//
// go-sdk v1.5.2 to v1.7.1 accept a compressed key whose x is at or above
// the prime (02 || p+1 is an alias of the point with x = 1), keep x
// unreduced and write the alias bytes back, so one point has two encodings and IsEqual
// calls them two keys. No private key is known for such a key, so it cannot
// sign, but anything keyed by key bytes before a signature is checked (a
// pin, an index, a de-duplication) would treat the alias as a second key.
// An uncompressed or hybrid encoding is refused too: a key from the wire in
// this module is always compressed, and a second encoding of the same point
// is the same problem.
func ParsePubKey(b []byte) (*ec.PublicKey, error) {
	if len(b) != 33 {
		return nil, fmt.Errorf("%w: %d bytes, want 33 (compressed)", ErrPubKey, len(b))
	}
	if b[0] != 0x02 && b[0] != 0x03 {
		return nil, fmt.Errorf("%w: prefix 0x%02x, want 0x02 or 0x03", ErrPubKey, b[0])
	}
	if new(big.Int).SetBytes(b[1:]).Cmp(fieldPrime) >= 0 {
		return nil, fmt.Errorf("%w: x is not below the field prime", ErrPubKey)
	}
	k, err := ec.PublicKeyFromBytes(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPubKey, err)
	}
	// Belt and braces: the point is on the curve, and it writes back as the
	// bytes it was read from.
	if !k.Validate() || !bytes.Equal(k.Compressed(), b) {
		return nil, fmt.Errorf("%w: not a canonical point", ErrPubKey)
	}
	return k, nil
}

// ParsePubKeyHex is ParsePubKey over a hex string, which must be exactly the
// key's 66 hex digits.
func ParsePubKeyHex(s string) (*ec.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPubKey, err)
	}
	return ParsePubKey(b)
}
