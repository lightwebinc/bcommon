package chirp

import (
	"bytes"
	"crypto/sha256"
	"strings"

	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"
)

// uhrpPrefix is BRC-26's two-byte prefix of an object identifier.
var uhrpPrefix = []byte{0xce, 0x00}

// Identifier is the UHRP object identifier of hash: Base58Check of the
// prefix ce00 and the 32 hash bytes, in natural order.
func Identifier(hash [32]byte) string {
	b := append(append([]byte(nil), uhrpPrefix...), hash[:]...)
	sum := sha256.Sum256(b)
	sum = sha256.Sum256(sum[:])
	return base58.Encode(append(b, sum[:4]...))
}

// ParseIdentifier returns the hash an object identifier names, refusing a
// bad checksum, another prefix or another length.
func ParseIdentifier(s string) ([32]byte, error) {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 2+32+4 || !bytes.Equal(b[:2], uhrpPrefix) {
		return [32]byte{}, ErrIdentifier
	}
	sum := sha256.Sum256(b[:34])
	sum = sha256.Sum256(sum[:])
	if !bytes.Equal(sum[:4], b[34:]) {
		return [32]byte{}, ErrIdentifier
	}
	h := [32]byte(b[2:34])
	// Base58 has one encoding per byte string, but a decoder may accept a
	// string another one would not write; the canonical string is the one
	// this hash encodes to.
	if Identifier(h) != s {
		return [32]byte{}, ErrIdentifier
	}
	return h, nil
}

// URL is the canonical CHIRP URI of a root: chirp://<identifier>.
func URL(root [32]byte) string { return "chirp://" + Identifier(root) }

// ParseURL returns the root a CHIRP URI names. It takes the canonical
// chirp://<identifier> and the compact chirp:<identifier>, the scheme in any
// case, and nothing else: no user information, port, path, query or
// fragment, which a valid identifier cannot contain.
func ParseURL(s string) ([32]byte, error) {
	if len(s) < 6 || !strings.EqualFold(s[:6], "chirp:") {
		return [32]byte{}, ErrIdentifier
	}
	return ParseIdentifier(strings.TrimPrefix(s[6:], "//"))
}
