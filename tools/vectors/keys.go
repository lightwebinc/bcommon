package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// fieldPrime is secp256k1's p, written out so that the vectors' verdicts do
// not rest on the code they test.
var fieldPrime, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)

type pubkeyCase struct {
	Name string `json:"name"`
	// Accept is the strict rule: 33 bytes, prefix 0x02 or 0x03, x below
	// the field prime, and x on the curve (x^3 + 7 a square mod p).
	Accept bool `json:"accept"`
	// SDKParses is whether go-sdk's PublicKeyFromBytes, at the pin, takes
	// the bytes. A case refused here that go-sdk parses is a second
	// encoding of a point, or an encoding a wire key never has.
	SDKParses bool   `json:"sdkParses"`
	KeyHex    string `json:"keyHex"`
}

type pubkeyVector struct {
	Cases []pubkeyCase `json:"cases"`
}

// onCurve is whether x names a point: x^3 + 7 has a square root mod p.
func onCurve(x *big.Int) bool {
	rhs := new(big.Int).Exp(x, big.NewInt(3), fieldPrime)
	rhs.Add(rhs, big.NewInt(7)).Mod(rhs, fieldPrime)
	return new(big.Int).ModSqrt(rhs, fieldPrime) != nil
}

// strict is the rule, computed here with math/big alone.
func strict(b []byte) bool {
	if len(b) != 33 || (b[0] != 2 && b[0] != 3) {
		return false
	}
	x := new(big.Int).SetBytes(b[1:])
	return x.Cmp(fieldPrime) < 0 && onCurve(x)
}

func x32(x *big.Int) []byte { return x.FillBytes(make([]byte, 32)) }

func pubkeys(v *txVector) (*pubkeyVector, error) {
	id, err := hex.DecodeString(v.IdentityKeyHex)
	if err != nil {
		return nil, err
	}
	key, err := ec.PublicKeyFromBytes(id)
	if err != nil {
		return nil, err
	}
	other := bytes.Clone(id)
	other[0] ^= 1
	one := big.NewInt(1)
	five := big.NewInt(5)
	if !onCurve(one) || onCurve(five) {
		return nil, fmt.Errorf("x = 1 must be on the curve and x = 5 off it")
	}
	p1 := new(big.Int).Add(fieldPrime, one)
	allOnes := new(big.Int).Sub(new(big.Int).Lsh(one, 256), one)
	uncompressed := key.Uncompressed()
	hybrid := bytes.Clone(uncompressed)
	hybrid[0] = 0x06 | (id[0] & 1)

	cases := []struct {
		name string
		b    []byte
	}{
		{"the test identity key", id},
		{"the test identity key with the other parity", other},
		{"x = 1, prefix 02", cat([]byte{2}, x32(one))},
		{"x = 1, prefix 03", cat([]byte{3}, x32(one))},
		{"02 || p+1, an alias of x = 1", cat([]byte{2}, x32(p1))},
		{"03 || p+1, an alias of x = 1", cat([]byte{3}, x32(p1))},
		{"02 || p", cat([]byte{2}, x32(fieldPrime))},
		{"02 || 2^256-1", cat([]byte{2}, x32(allOnes))},
		{"x = 5, not on the curve", cat([]byte{2}, x32(five))},
		{"the test identity key uncompressed", uncompressed},
		{"the test identity key hybrid", hybrid},
		{"prefix 04 on 33 bytes", cat([]byte{4}, id[1:])},
		{"prefix 00", cat([]byte{0}, id[1:])},
		{"32 bytes", id[:32]},
		{"34 bytes", cat(id, []byte{0})},
		{"empty", nil},
	}
	out := &pubkeyVector{}
	for _, c := range cases {
		_, err := ec.PublicKeyFromBytes(c.b)
		out.Cases = append(out.Cases, pubkeyCase{Name: c.name, Accept: strict(c.b), SDKParses: err == nil, KeyHex: hex.EncodeToString(c.b)})
	}
	for _, c := range out.Cases[:4] {
		if !c.Accept {
			return nil, fmt.Errorf("%s: a canonical key is refused", c.Name)
		}
	}
	return out, nil
}
