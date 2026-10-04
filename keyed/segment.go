package keyed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The sizes of one BRC-369 section 2.3 segment.
const (
	// SaltLen is the length of the per-content salt.
	SaltLen = 4
	// SegmentIVLen is the length of a segment's IV: the salt, then the
	// segment index as eight bytes big-endian.
	SegmentIVLen = 12
	// MaxSegment is the most plaintext one segment holds here: 1 MiB.
	// BRC-369 leaves the bound open; this is the one sealing in a single
	// segment is held to, so that one value never needs a second.
	MaxSegment = 1 << 20
)

// Refusals of one segment. ErrTag is a holder's (BRC-369 section 5.3's
// third check): a key that opens the commitment and not the ciphertext names
// whoever encrypted under it.
var (
	// ErrSegment is a plaintext or a ciphertext outside a segment's
	// bounds: a plaintext of 1 to MaxSegment bytes, a ciphertext of 17 to
	// MaxSegment + TagLen.
	ErrSegment = errors.New("keyed: not the length of one segment")
	// ErrTag is a segment whose tag does not verify under the key.
	ErrTag = errors.New("keyed: segment tag does not verify under the key")
	// ErrDomain is an empty epoch-wrap domain string.
	ErrDomain = errors.New("keyed: empty domain string")
)

// SampleSalt draws a salt from r (crypto/rand.Reader when r is nil). A salt
// is drawn fresh for every content key (BRC-369 section 2.3 rule 3).
func SampleSalt(r io.Reader) ([SaltLen]byte, error) {
	if r == nil {
		r = rand.Reader
	}
	var s [SaltLen]byte
	_, err := io.ReadFull(r, s[:])
	return s, err
}

// SegmentIV is BRC-369 section 2.3 rule 2: the salt followed by the segment
// index as eight bytes big-endian. The one segment SealSegment writes is
// index 0, so its IV is the salt and eight zero bytes.
func SegmentIV(salt [SaltLen]byte, i uint64) [SegmentIVLen]byte {
	var iv [SegmentIVLen]byte
	copy(iv[:], salt[:])
	binary.BigEndian.PutUint64(iv[SaltLen:], i)
	return iv
}

func segmentAEAD(k [32]byte) (cipher.AEAD, error) {
	if err := CheckScalar(k); err != nil {
		return nil, err
	}
	sym := SymmetricKey(k)
	blk, err := aes.NewCipher(sym[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// SealSegment encrypts plaintext under the content key k as one BRC-369
// section 2.3 segment: AES-256-GCM under SymmetricKey(k), the IV
// SegmentIV(salt, 0), no associated data, the 16-byte tag after the
// ciphertext. The segment size is the plaintext's length, so the result is
// that length plus TagLen. It is the bytes a segmented sealer writes for a
// plaintext no longer than its segment size.
//
// k must be a scalar (ErrScalar) and the plaintext 1 to MaxSegment bytes
// (ErrSegment). A key and salt used twice break both plaintexts without
// any key: k is fresh per piece of content, and the salt is fresh per key.
func SealSegment(k [32]byte, salt [SaltLen]byte, plaintext []byte) ([]byte, error) {
	if len(plaintext) < 1 || len(plaintext) > MaxSegment {
		return nil, fmt.Errorf("%w: plaintext of %d bytes", ErrSegment, len(plaintext))
	}
	g, err := segmentAEAD(k)
	if err != nil {
		return nil, err
	}
	iv := SegmentIV(salt, 0)
	return g.Seal(make([]byte, 0, len(plaintext)+TagLen), iv[:], plaintext, nil), nil
}

// OpenSegment reverses SealSegment. A ciphertext that cannot be one segment
// (TagLen bytes or fewer, or more than MaxSegment + TagLen) is ErrSegment, a
// key that is not a scalar ErrScalar, and a tag that does not verify ErrTag;
// no plaintext is returned unless the tag verifies (BRC-369 section 2.3
// rule 5). A caller checks the key against its commitment (CheckOpened)
// first, so that a failure here names the encrypter and not the wrapper.
func OpenSegment(k [32]byte, salt [SaltLen]byte, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) <= TagLen || len(ciphertext) > MaxSegment+TagLen {
		return nil, fmt.Errorf("%w: ciphertext of %d bytes", ErrSegment, len(ciphertext))
	}
	g, err := segmentAEAD(k)
	if err != nil {
		return nil, err
	}
	iv := SegmentIV(salt, 0)
	pt, err := g.Open(nil, iv[:], ciphertext, nil)
	if err != nil {
		return nil, ErrTag
	}
	return pt, nil
}

// EpochWrapKey is the key a content key is wrapped under for the members of
// one group epoch: SHA-256 of domain, the epoch's BRC-369 symmetric key
// (SymmetricKey of the epoch key) and the content id of what k encrypts.
// The epoch's symmetric key is an input to the hash, never a cipher key
// itself, so one wrapping key wraps one content key, and an application's
// own domain string keeps its wraps apart from every other application's
// use of the same epoch key. domain is the application's registered
// string, such as "<application> epoch wrap v1".
func EpochWrapKey(domain string, epochSymmetric, contentID [32]byte) [32]byte {
	b := make([]byte, 0, len(domain)+64)
	b = append(b, domain...)
	b = append(b, epochSymmetric[:]...)
	return sha256.Sum256(append(b, contentID[:]...))
}

// WrapToEpoch wraps the content key k under EpochWrapKey in BRC-2's
// symmetric form: a 32-byte IV from r (crypto/rand.Reader when r is nil),
// the 32-byte ciphertext and the tag, WrappedKeyLen bytes. An empty domain
// is ErrDomain and a key that is not a scalar ErrScalar.
func WrapToEpoch(domain string, epochSymmetric, contentID, k [32]byte, r io.Reader) ([]byte, error) {
	if r == nil {
		r = rand.Reader
	}
	var iv [IVLen]byte
	if _, err := io.ReadFull(r, iv[:]); err != nil {
		return nil, err
	}
	return WrapToEpochWithIV(domain, epochSymmetric, contentID, k, iv)
}

// WrapToEpochWithIV is WrapToEpoch with the caller's IV. An IV must never be
// used twice under one wrapping key; outside a vector that fixes it to be
// reproducible, call WrapToEpoch.
func WrapToEpochWithIV(domain string, epochSymmetric, contentID, k [32]byte, iv [IVLen]byte) ([]byte, error) {
	if domain == "" {
		return nil, ErrDomain
	}
	if err := CheckScalar(k); err != nil {
		return nil, err
	}
	w := EpochWrapKey(domain, epochSymmetric, contentID)
	return SymmetricSeal(w[:], iv, k[:])
}

// UnwrapFromEpoch is a member's side: it opens wrap with the key derived
// from the symmetric key of the epoch the record names, and with no other,
// then runs CheckOpened against commitment. A wrap that is not
// WrappedKeyLen bytes or does not open is ErrUnwrap: a holder of another
// epoch's key derives another wrapping key, and the wrap does not open. An
// empty domain is ErrDomain.
func UnwrapFromEpoch(domain string, epochSymmetric, contentID [32]byte, wrap []byte, commitment [32]byte) ([32]byte, error) {
	if domain == "" {
		return [32]byte{}, ErrDomain
	}
	if len(wrap) != WrappedKeyLen {
		return [32]byte{}, fmt.Errorf("%w: wrap of %d bytes", ErrUnwrap, len(wrap))
	}
	w := EpochWrapKey(domain, epochSymmetric, contentID)
	pt, err := SymmetricOpen(w[:], wrap)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %v", ErrUnwrap, err)
	}
	return CheckOpened(pt, commitment)
}
