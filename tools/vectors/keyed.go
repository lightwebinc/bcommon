package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

// BRC-369 section 2's domain strings, and the values the document prints
// under "Test Vectors" for its content key. The generator recomputes the
// printed symmetric key and commitment from the printed key with the
// standard library alone and fails when either stops matching.
const (
	domainSymmetric  = "metanet keyed content symmetric v1"
	domainCommitment = "metanet keyed content commitment v1"

	brc369Key        = "0ff730f7b66f7bb6626229ca20c5b4c2c47ecc32248203a14cd247ef7ed298d8"
	brc369Symmetric  = "42dcb945809f42a6f827c8336affc7a04c284dfd6e4b65c6663f6e864240ad0e"
	brc369Commitment = "2013064f87db545aa20f273b3abc8bdb470ddc69349cac55b7b397585af15797"
)

type keyCase struct {
	Name   string `json:"name"`
	KeyHex string `json:"keyHex"`
	// Scalar is whether the 32 bytes are in [1, n-1].
	Scalar bool `json:"scalar"`
	// SymmetricHex and CommitmentHex are SHA-256 of each domain string and
	// the key, whether or not the key is a scalar.
	SymmetricHex  string `json:"symmetricHex"`
	CommitmentHex string `json:"commitmentHex"`
}

type openedCase struct {
	Name          string `json:"name"`
	PlaintextHex  string `json:"plaintextHex"`
	CommitmentHex string `json:"commitmentHex"`
	// Refusal is empty when the plaintext is the committed key, else the
	// first check it fails: unwrap (not 32 bytes), scalar or commitment.
	Refusal string `json:"refusal"`
}

type sealedCase struct {
	Name         string `json:"name"`
	KeyHex       string `json:"keyHex"`
	IVHex        string `json:"ivHex"`
	PlaintextHex string `json:"plaintextHex"`
	// SealedHex is IV || ciphertext || tag.
	SealedHex string `json:"sealedHex"`
}

type openCase struct {
	Name      string `json:"name"`
	KeyHex    string `json:"keyHex"`
	SealedHex string `json:"sealedHex"`
	Opens     bool   `json:"opens"`
}

// keyedVector is the content key of BRC-369 section 2 and BRC-2's symmetric
// form, each computed here with the standard library: SHA-256, math/big
// and AES-256-GCM with a 32-byte nonce.
type keyedVector struct {
	DomainSymmetric  string `json:"domainSymmetric"`
	DomainCommitment string `json:"domainCommitment"`
	// OrderHex is n, the order of secp256k1's group.
	OrderHex string       `json:"orderHex"`
	Keys     []keyCase    `json:"keys"`
	Opened   []openedCase `json:"opened"`
	Sealed   []sealedCase `json:"sealed"`
	Opens    []openCase   `json:"opens"`
}

func digest(domain string, k []byte) []byte {
	h := sha256.Sum256(append([]byte(domain), k...))
	return h[:]
}

// gcm32 is AES-256-GCM with a 32-byte nonce and no associated data, written
// nonce || ciphertext || tag.
func gcm32(key, iv, plaintext []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCMWithNonceSize(blk, len(iv))
	if err != nil {
		return nil, err
	}
	return g.Seal(append([]byte{}, iv...), iv, plaintext, nil), nil
}

func keyedFamilies() ([]family, error) {
	v := keyedVector{DomainSymmetric: domainSymmetric, DomainCommitment: domainCommitment, OrderHex: hex.EncodeToString(x32(order))}
	one := big.NewInt(1)
	printed, err := hex.DecodeString(brc369Key)
	if err != nil {
		return nil, err
	}
	if got := hex.EncodeToString(digest(domainSymmetric, printed)); got != brc369Symmetric {
		return nil, fmt.Errorf("BRC-369's symmetric key no longer reproduces: %s", got)
	}
	if got := hex.EncodeToString(digest(domainCommitment, printed)); got != brc369Commitment {
		return nil, fmt.Errorf("BRC-369's commitment no longer reproduces: %s", got)
	}
	for _, c := range []struct {
		name string
		k    []byte
	}{
		{"the content key BRC-369 prints, with the symmetric key and commitment it prints", printed},
		{"one, the least scalar", x32(one)},
		{"n - 1, the greatest scalar", x32(new(big.Int).Sub(order, one))},
		{"a key whose first byte is zero", append([]byte{0}, fill(0x51, 31)...)},
		{"zero", make([]byte, 32)},
		{"n", x32(order)},
		{"n + 1", x32(new(big.Int).Add(order, one))},
		{"2^256 - 1", bytes.Repeat([]byte{0xff}, 32)},
	} {
		x := new(big.Int).SetBytes(c.k)
		v.Keys = append(v.Keys, keyCase{Name: c.name, KeyHex: hex.EncodeToString(c.k),
			Scalar:       x.Sign() > 0 && x.Cmp(order) < 0,
			SymmetricHex: hex.EncodeToString(digest(domainSymmetric, c.k)), CommitmentHex: hex.EncodeToString(digest(domainCommitment, c.k))})
	}

	commitment := digest(domainCommitment, printed)
	other := fill(0x51, 32)
	for _, c := range []struct {
		name, refusal string
		pt, commit    []byte
	}{
		{"the committed key", "", printed, commitment},
		{"31 bytes", "unwrap", printed[:31], commitment},
		{"33 bytes", "unwrap", append(append([]byte{}, printed...), 0), commitment},
		{"nothing", "unwrap", nil, commitment},
		{"zero, under its own commitment: not a scalar", "scalar", make([]byte, 32), digest(domainCommitment, make([]byte, 32))},
		{"n, under its own commitment: not a scalar", "scalar", x32(order), digest(domainCommitment, x32(order))},
		{"another scalar", "commitment", other, commitment},
		{"the key under the symmetric key in the commitment's place", "commitment", printed, digest(domainSymmetric, printed)},
	} {
		v.Opened = append(v.Opened, openedCase{Name: c.name, PlaintextHex: hex.EncodeToString(c.pt),
			CommitmentHex: hex.EncodeToString(c.commit), Refusal: c.refusal})
	}

	wrapKey, iv := fill(0x01, 32), fill(0x80, 32)
	for _, c := range []struct {
		name string
		pt   []byte
	}{
		{"a 32-byte key, as a wrap holds it", printed},
		{"one byte", []byte{0x2a}},
		{"nothing", nil},
		{"text of 17 bytes, past one cipher block", []byte("a certificate fld")},
		{"300 bytes", fill(0, 300)},
	} {
		sealed, err := gcm32(wrapKey, iv, c.pt)
		if err != nil {
			return nil, err
		}
		v.Sealed = append(v.Sealed, sealedCase{Name: c.name, KeyHex: hex.EncodeToString(wrapKey), IVHex: hex.EncodeToString(iv),
			PlaintextHex: hex.EncodeToString(c.pt), SealedHex: hex.EncodeToString(sealed)})
	}

	good, err := gcm32(wrapKey, iv, printed)
	if err != nil {
		return nil, err
	}
	flip := func(i int) []byte {
		b := append([]byte{}, good...)
		b[i] ^= 1
		return b
	}
	empty, err := gcm32(wrapKey, iv, nil)
	if err != nil {
		return nil, err
	}
	for _, c := range []struct {
		name   string
		key, b []byte
		opens  bool
	}{
		{"as sealed", wrapKey, good, true},
		{"an empty plaintext, the shortest that opens", wrapKey, empty, true},
		{"another key", fill(0x02, 32), good, false},
		{"a bit of the IV flipped", wrapKey, flip(0), false},
		{"a bit of the ciphertext flipped", wrapKey, flip(40), false},
		{"a bit of the tag flipped", wrapKey, flip(len(good) - 1), false},
		{"the last byte cut", wrapKey, good[:len(good)-1], false},
		{"47 bytes, less than an IV and a tag", wrapKey, good[:47], false},
		{"nothing", wrapKey, nil, false},
	} {
		v.Opens = append(v.Opens, openCase{Name: c.name, KeyHex: hex.EncodeToString(c.key), SealedHex: hex.EncodeToString(c.b), Opens: c.opens})
	}
	return []family{{"keyed-v1.json", v}}, nil
}
