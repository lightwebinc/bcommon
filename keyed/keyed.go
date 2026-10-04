// Package keyed holds the part of BRC-369 keyed content that every
// application keyed under a random scalar shares: the content key, its
// symmetric key and its commitment (BRC-369 sections 2.1 and 2.2), the
// check a holder runs on a key it has just unwrapped, and BRC-2's symmetric
// form, in which a key is wrapped and a certificate field is encrypted.
//
// A content key is a random scalar, never derived from an identity or from
// anything else. The commitment is published; the key is released only to
// those meant to hold it. A holder that unwraps a key checks it against the
// commitment before anything uses it (CheckOpened), so that a wrong key
// names whoever wrapped it and not whoever encrypted under it.
//
// SealSegment and OpenSegment encrypt a value under a content key as one
// BRC-369 section 2.3 segment, and EpochWrapKey, WrapToEpoch and
// UnwrapFromEpoch wrap a content key for the members of one group epoch,
// under a key derived from the epoch's symmetric key, the content id and
// the application's own domain string.
//
// What a key encrypts, who it is released to and how a release is framed
// are the application's. This package writes no record and names no
// protocol.
package keyed

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"

	aesgcm "github.com/bsv-blockchain/go-sdk/primitives/aesgcm"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// BRC-369 section 2's domain strings.
const (
	DomainSymmetric  = "metanet keyed content symmetric v1"
	DomainCommitment = "metanet keyed content commitment v1"
)

// The sizes of BRC-2's symmetric form as go-sdk writes it: a 32-byte IV, the
// ciphertext, and a 16-byte tag.
const (
	IVLen = 32
	// TagLen is the length of the authentication tag.
	TagLen = 16
	// Overhead is what the form adds to a plaintext.
	Overhead = IVLen + TagLen
	// WrappedKeyLen is the length of a wrapped 32-byte key.
	WrappedKeyLen = 32 + Overhead
)

// A holder's refusals (BRC-369 section 5.3), never a host's: a host cannot
// see a key.
var (
	// ErrScalar is a key that is not a scalar in [1, n-1].
	ErrScalar = errors.New("keyed: key is not a scalar in [1, n-1]")
	// ErrCommitment is a key that is not the committed one.
	ErrCommitment = errors.New("keyed: key does not match the commitment")
	// ErrUnwrap is a wrap that does not open, or opens to something that
	// is not 32 bytes.
	ErrUnwrap = errors.New("keyed: wrap does not open")
)

// SampleKey draws a key: a scalar uniform in [1, n-1] from r
// (crypto/rand.Reader when r is nil), never derived from anything
// (BRC-369 section 2.1 rule 1).
func SampleKey(r io.Reader) ([32]byte, error) {
	if r == nil {
		r = rand.Reader
	}
	for {
		var k [32]byte
		if _, err := io.ReadFull(r, k[:]); err != nil {
			return k, err
		}
		if CheckScalar(k) == nil {
			return k, nil
		}
	}
}

// CheckScalar refuses, as ErrScalar, a 32-byte big-endian value outside
// [1, n-1].
func CheckScalar(k [32]byte) error {
	v := new(big.Int).SetBytes(k[:])
	if v.Sign() == 0 || v.Cmp(ec.S256().N) >= 0 {
		return ErrScalar
	}
	return nil
}

// SymmetricKey is BRC-369 section 2.1 rule 3: SHA-256 of the domain string
// and the scalar. It is never derived from the commitment.
func SymmetricKey(k [32]byte) [32]byte {
	return sha256.Sum256(append([]byte(DomainSymmetric), k[:]...))
}

// Commitment is BRC-369 section 2.2's digest form: SHA-256 of the domain
// string and the scalar.
func Commitment(k [32]byte) [32]byte {
	return sha256.Sum256(append([]byte(DomainCommitment), k[:]...))
}

// CheckOpened is the tail of every unwrap, in its order: the plaintext is
// exactly 32 bytes (ErrUnwrap), a scalar (ErrScalar), and the committed one
// (ErrCommitment). The key it returns with an error must not be used.
func CheckOpened(plaintext []byte, commitment [32]byte) ([32]byte, error) {
	var k [32]byte
	if len(plaintext) != 32 {
		return k, fmt.Errorf("%w: plaintext of %d bytes", ErrUnwrap, len(plaintext))
	}
	copy(k[:], plaintext)
	if err := CheckScalar(k); err != nil {
		return k, err
	}
	if Commitment(k) != commitment {
		return k, ErrCommitment
	}
	return k, nil
}

// SymmetricSeal is BRC-2's symmetric form as go-sdk writes it: AES-256-GCM
// under key with a 32-byte IV and no associated data, written
// IV || ciphertext || tag. It is what a wallet's Encrypt returns, with the
// IV chosen by the caller in place of a random one: an IV must never be
// used twice under one key, so only a caller that draws it from a CSPRNG,
// or a vector that fixes it to be reproducible, calls this.
func SymmetricSeal(key []byte, iv [IVLen]byte, plaintext []byte) ([]byte, error) {
	ct, tag, err := aesgcm.AESGCMEncrypt(plaintext, key, iv[:], []byte{})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, IVLen+len(ct)+TagLen)
	out = append(out, iv[:]...)
	out = append(out, ct...)
	return append(out, tag...), nil
}

// SymmetricOpen reverses SymmetricSeal through go-sdk's own symmetric key,
// so it opens exactly what a wallet's Decrypt opens under the same key.
func SymmetricOpen(key, sealed []byte) ([]byte, error) {
	if len(sealed) < Overhead {
		return nil, errors.New("keyed: ciphertext too short")
	}
	return ec.NewSymmetricKey(key).Decrypt(sealed)
}
