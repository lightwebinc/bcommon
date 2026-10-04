package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"math/big"
)

// The epoch-wrap domain strings the registry records, each an application's
// own. The family wraps under both, so that a wrapper whose domain string is
// a parameter is held to every registered one.
var epochWrapDomains = []string{"bsecret epoch wrap v1", "bchat epoch wrap v1"}

type segmentCase struct {
	Name         string `json:"name"`
	KeyHex       string `json:"keyHex"`
	SaltHex      string `json:"saltHex"`
	IVHex        string `json:"ivHex"`
	PlaintextHex string `json:"plaintextHex"`
	// SealedHex is the ciphertext followed by its 16-byte tag.
	SealedHex string `json:"sealedHex"`
}

type segmentOpenCase struct {
	Name      string `json:"name"`
	KeyHex    string `json:"keyHex"`
	SaltHex   string `json:"saltHex"`
	SealedHex string `json:"sealedHex"`
	// Refusal is empty when it opens to PlaintextHex, else segment (not
	// the length of one segment), scalar (the key is not one) or tag.
	Refusal      string `json:"refusal"`
	PlaintextHex string `json:"plaintextHex"`
}

type wrapCase struct {
	Name              string `json:"name"`
	Domain            string `json:"domain"`
	EpochKeyHex       string `json:"epochKeyHex"`
	EpochSymmetricHex string `json:"epochSymmetricHex"`
	ContentIDHex      string `json:"contentIdHex"`
	KeyHex            string `json:"keyHex"`
	CommitmentHex     string `json:"commitmentHex"`
	IVHex             string `json:"ivHex"`
	// WrapKeyHex is SHA-256(domain || epoch symmetric key || content id).
	WrapKeyHex string `json:"wrapKeyHex"`
	// WrapHex is BRC-2's symmetric form of the key under the wrap key.
	WrapHex string `json:"wrapHex"`
}

type unwrapCase struct {
	Name              string `json:"name"`
	Domain            string `json:"domain"`
	EpochSymmetricHex string `json:"epochSymmetricHex"`
	ContentIDHex      string `json:"contentIdHex"`
	WrapHex           string `json:"wrapHex"`
	CommitmentHex     string `json:"commitmentHex"`
	// Refusal is empty when the wrap opens to KeyHex, else the first check
	// it fails: unwrap or commitment.
	Refusal string `json:"refusal"`
	KeyHex  string `json:"keyHex"`
}

// segmentVector is one BRC-369 section 2.3 segment, and the epoch wrap of
// a content key, each computed here with the standard library: SHA-256,
// and AES-256-GCM with a 12-byte nonce for a segment and a 32-byte one for
// a wrap.
type segmentVector struct {
	Domains  []string          `json:"epochWrapDomains"`
	Segments []segmentCase     `json:"segments"`
	Opens    []segmentOpenCase `json:"opens"`
	Wraps    []wrapCase        `json:"wraps"`
	Unwraps  []unwrapCase      `json:"unwraps"`
}

// gcm12 is AES-256-GCM under SHA-256(domainSymmetric || k) with the IV
// salt || i as eight bytes big-endian, i = 0, no associated data, written
// ciphertext || tag.
func gcm12(k, salt, plaintext []byte) ([]byte, []byte, error) {
	blk, err := aes.NewCipher(digest(domainSymmetric, k))
	if err != nil {
		return nil, nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, err
	}
	iv := append(append([]byte{}, salt...), make([]byte, 8)...)
	return iv, g.Seal(nil, iv, plaintext, nil), nil
}

func wrapKey(domain string, epochSym, contentID []byte) []byte {
	return digest(domain, append(append([]byte{}, epochSym...), contentID...))
}

func segmentFamilies() ([]family, error) {
	v := segmentVector{Domains: epochWrapDomains}
	printed, err := hex.DecodeString(brc369Key)
	if err != nil {
		return nil, err
	}
	one := big.NewInt(1)
	nMinus1 := x32(new(big.Int).Sub(order, one))
	for _, c := range []struct {
		name    string
		k, salt []byte
		pt      []byte
	}{
		{"one byte, under the key BRC-369 prints", printed, []byte{0, 0, 0, 0}, []byte{0x2a}},
		{"15 bytes, short of one cipher block", printed, []byte{1, 2, 3, 4}, []byte("a short message")},
		{"16 bytes, one cipher block", printed, []byte{1, 2, 3, 4}, fill(0x10, 16)},
		{"17 bytes, past one cipher block", printed, []byte{0xff, 0xff, 0xff, 0xff}, fill(0x20, 17)},
		{"64 bytes under n - 1", nMinus1, []byte{0xde, 0xad, 0xbe, 0xef}, fill(0x30, 64)},
		{"300 bytes under one", x32(one), []byte{0x80, 0, 0, 1}, fill(0, 300)},
		{"16384 bytes, a payload at a large bound", printed, []byte{9, 8, 7, 6}, fill(0x41, 16384)},
	} {
		iv, sealed, err := gcm12(c.k, c.salt, c.pt)
		if err != nil {
			return nil, err
		}
		v.Segments = append(v.Segments, segmentCase{Name: c.name, KeyHex: hex.EncodeToString(c.k), SaltHex: hex.EncodeToString(c.salt),
			IVHex: hex.EncodeToString(iv), PlaintextHex: hex.EncodeToString(c.pt), SealedHex: hex.EncodeToString(sealed)})
	}

	salt := []byte{1, 2, 3, 4}
	pt := []byte("a short message")
	_, good, err := gcm12(printed, salt, pt)
	if err != nil {
		return nil, err
	}
	flip := func(i int) []byte {
		b := append([]byte{}, good...)
		b[i] ^= 1
		return b
	}
	_, other, err := gcm12(nMinus1, salt, pt)
	if err != nil {
		return nil, err
	}
	for _, c := range []struct {
		name, refusal string
		k, salt, b    []byte
	}{
		{"as sealed", "", printed, salt, good},
		{"under another key", "tag", nMinus1, salt, good},
		{"sealed under another key, opened under this one", "tag", printed, salt, other},
		{"under another salt", "tag", printed, []byte{1, 2, 3, 5}, good},
		{"a bit of the ciphertext flipped", "tag", printed, salt, flip(0)},
		{"a bit of the tag flipped", "tag", printed, salt, flip(len(good) - 1)},
		{"the last byte cut", "tag", printed, salt, good[:len(good)-1]},
		{"a byte appended", "tag", printed, salt, append(append([]byte{}, good...), 0)},
		{"17 bytes, the shortest a segment is", "tag", printed, salt, good[:17]},
		{"16 bytes, a tag and no ciphertext", "segment", printed, salt, good[:16]},
		{"nothing", "segment", printed, salt, nil},
		{"under zero, which is not a scalar", "scalar", make([]byte, 32), salt, good},
		{"under n, which is not a scalar", "scalar", x32(order), salt, good},
	} {
		oc := segmentOpenCase{Name: c.name, KeyHex: hex.EncodeToString(c.k), SaltHex: hex.EncodeToString(c.salt),
			SealedHex: hex.EncodeToString(c.b), Refusal: c.refusal}
		if c.refusal == "" {
			oc.PlaintextHex = hex.EncodeToString(pt)
		}
		v.Opens = append(v.Opens, oc)
	}

	epochKeys := [][]byte{fill(0x61, 32), fill(0x62, 32)}
	contentIDs := [][]byte{fill(0xc0, 32), fill(0xc1, 32)}
	iv := fill(0x90, 32)
	commitment := digest(domainCommitment, printed)
	wraps := map[string][]byte{}
	for _, d := range epochWrapDomains {
		for ei, ek := range epochKeys {
			sym := digest(domainSymmetric, ek)
			wk := wrapKey(d, sym, contentIDs[0])
			w, err := gcm32(wk, iv, printed)
			if err != nil {
				return nil, err
			}
			wraps[d+string(rune('0'+ei))] = w
			v.Wraps = append(v.Wraps, wrapCase{Name: "the key BRC-369 prints, under epoch key " + string(rune('A'+ei)) + " and " + d,
				Domain: d, EpochKeyHex: hex.EncodeToString(ek), EpochSymmetricHex: hex.EncodeToString(sym),
				ContentIDHex: hex.EncodeToString(contentIDs[0]), KeyHex: brc369Key, CommitmentHex: hex.EncodeToString(commitment),
				IVHex: hex.EncodeToString(iv), WrapKeyHex: hex.EncodeToString(wk), WrapHex: hex.EncodeToString(w)})
		}
	}
	if bytes.Equal(wraps[epochWrapDomains[0]+"0"], wraps[epochWrapDomains[1]+"0"]) {
		return nil, errors.New("two domain strings gave one wrap")
	}

	symA, symB := digest(domainSymmetric, epochKeys[0]), digest(domainSymmetric, epochKeys[1])
	// A wrap of a 32-byte value that is not the committed key, and of one
	// that is not 32 bytes, each under the right wrapping key.
	d0 := epochWrapDomains[0]
	wrongKey, err := gcm32(wrapKey(d0, symA, contentIDs[0]), iv, nMinus1)
	if err != nil {
		return nil, err
	}
	shortKey, err := gcm32(wrapKey(d0, symA, contentIDs[0]), iv, printed[:31])
	if err != nil {
		return nil, err
	}
	for _, d := range epochWrapDomains {
		otherD := epochWrapDomains[0]
		if d == otherD {
			otherD = epochWrapDomains[1]
		}
		w := wraps[d+"0"]
		for _, c := range []struct {
			name, domain, refusal  string
			sym, cid, wrap, commit []byte
		}{
			{"opened as wrapped", d, "", symA, contentIDs[0], w, commitment},
			{"opened under the other domain string", otherD, "unwrap", symA, contentIDs[0], w, commitment},
			{"opened with another epoch's key", d, "unwrap", symB, contentIDs[0], w, commitment},
			{"opened for another content id", d, "unwrap", symA, contentIDs[1], w, commitment},
			{"checked against another commitment", d, "commitment", symA, contentIDs[0], w, digest(domainCommitment, nMinus1)},
			{"79 bytes", d, "unwrap", symA, contentIDs[0], w[:79], commitment},
		} {
			uc := unwrapCase{Name: d + ": " + c.name, Domain: c.domain, EpochSymmetricHex: hex.EncodeToString(c.sym),
				ContentIDHex: hex.EncodeToString(c.cid), WrapHex: hex.EncodeToString(c.wrap), CommitmentHex: hex.EncodeToString(c.commit), Refusal: c.refusal}
			if c.refusal == "" {
				uc.KeyHex = brc369Key
			}
			v.Unwraps = append(v.Unwraps, uc)
		}
	}
	v.Unwraps = append(v.Unwraps,
		unwrapCase{Name: d0 + ": a wrap of another scalar", Domain: d0, EpochSymmetricHex: hex.EncodeToString(symA),
			ContentIDHex: hex.EncodeToString(contentIDs[0]), WrapHex: hex.EncodeToString(wrongKey), CommitmentHex: hex.EncodeToString(commitment), Refusal: "commitment"},
		unwrapCase{Name: d0 + ": a wrap of 31 bytes, 79 in all", Domain: d0, EpochSymmetricHex: hex.EncodeToString(symA),
			ContentIDHex: hex.EncodeToString(contentIDs[0]), WrapHex: hex.EncodeToString(shortKey), CommitmentHex: hex.EncodeToString(commitment), Refusal: "unwrap"})
	return []family{{"keyed-segment-v1.json", v}}, nil
}
