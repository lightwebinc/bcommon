package keyed

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

func key(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return k
}

func TestSampleKey(t *testing.T) {
	seen := map[[32]byte]bool{}
	for range 64 {
		k, err := SampleKey(nil)
		if err != nil || CheckScalar(k) != nil || seen[k] {
			t.Fatalf("sample: %v", err)
		}
		seen[k] = true
	}
	// A reader that first yields a value at or above the order, then zero,
	// then a scalar: the first two are drawn again, never reduced.
	n := ec.S256().N.FillBytes(make([]byte, 32))
	want := key(7)
	r := io.MultiReader(bytes.NewReader(n), bytes.NewReader(make([]byte, 32)), bytes.NewReader(want[:]))
	k, err := SampleKey(r)
	if err != nil || k != want {
		t.Fatalf("rejection sampling: %x, %v", k, err)
	}
	if _, err := SampleKey(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("a short reader gave a key")
	}
}

func TestScalarBounds(t *testing.T) {
	n := ec.S256().N
	for name, c := range map[string]struct {
		v  *big.Int
		ok bool
	}{
		"zero":  {big.NewInt(0), false},
		"one":   {big.NewInt(1), true},
		"n - 1": {new(big.Int).Sub(n, big.NewInt(1)), true},
		"n":     {n, false},
		"n + 1": {new(big.Int).Add(n, big.NewInt(1)), false},
	} {
		var k [32]byte
		c.v.FillBytes(k[:])
		if err := CheckScalar(k); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The symmetric key and the commitment are different digests of one key: a
// published commitment says nothing about the key that encrypts.
func TestSymmetricKeyIsNotTheCommitment(t *testing.T) {
	k := key(3)
	if SymmetricKey(k) == Commitment(k) {
		t.Fatal("the symmetric key equals the commitment")
	}
	if SymmetricKey(k) == SymmetricKey(key(4)) || Commitment(k) == Commitment(key(4)) {
		t.Fatal("two keys share a digest")
	}
}

// CheckOpened refuses in its order: the length, then the scalar, then the
// commitment.
func TestCheckOpenedOrder(t *testing.T) {
	k := key(5)
	if got, err := CheckOpened(k[:], Commitment(k)); err != nil || got != k {
		t.Fatalf("the committed key: %v", err)
	}
	if _, err := CheckOpened(k[:31], Commitment(k)); !errors.Is(err, ErrUnwrap) {
		t.Errorf("31 bytes: %v", err)
	}
	zero := make([]byte, 32)
	if _, err := CheckOpened(zero, Commitment(k)); !errors.Is(err, ErrScalar) {
		t.Errorf("zero against another commitment: %v, want the scalar refusal first", err)
	}
	if _, err := CheckOpened(k[:], Commitment(key(6))); !errors.Is(err, ErrCommitment) {
		t.Errorf("another commitment: %v", err)
	}
}

// SymmetricSeal writes what a wallet's Encrypt writes, and SymmetricOpen
// opens what a wallet's Encrypt wrote: the form is BRC-2's as go-sdk
// implements it, under the key the wallet derives.
func TestSymmetricFormIsTheWallets(t *testing.T) {
	ctx := context.Background()
	a, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	aw, err := wallet.NewCompletedProtoWallet(a)
	if err != nil {
		t.Fatal(err)
	}
	bw, err := wallet.NewCompletedProtoWallet(b)
	if err != nil {
		t.Fatal(err)
	}
	proto := wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "vector sample"}
	toB := wallet.EncryptionArgs{ProtocolID: proto, KeyID: "wrap 1", Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: b.PubKey()}}
	toA := wallet.EncryptionArgs{ProtocolID: proto, KeyID: "wrap 1", Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: a.PubKey()}}
	sk, err := wallet.NewKeyDeriver(a).DeriveSymmetricKey(proto, "wrap 1", toB.Counterparty)
	if err != nil {
		t.Fatal(err)
	}
	k := key(9)
	var iv [IVLen]byte
	if _, err := rand.Read(iv[:]); err != nil {
		t.Fatal(err)
	}
	sealed, err := SymmetricSeal(sk.ToBytes(), iv, k[:])
	if err != nil || len(sealed) != WrappedKeyLen {
		t.Fatalf("seal: %d bytes, %v", len(sealed), err)
	}
	res, err := bw.Decrypt(ctx, wallet.DecryptArgs{EncryptionArgs: toA, Ciphertext: sealed}, "")
	if err != nil || !bytes.Equal(res.Plaintext, k[:]) {
		t.Fatalf("the recipient's wallet does not open it: %v", err)
	}
	enc, err := aw.Encrypt(ctx, wallet.EncryptArgs{EncryptionArgs: toB, Plaintext: k[:]}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(enc.Ciphertext) != WrappedKeyLen {
		t.Fatalf("a wallet's wrap is %d bytes, want %d", len(enc.Ciphertext), WrappedKeyLen)
	}
	pt, err := SymmetricOpen(sk.ToBytes(), enc.Ciphertext)
	if err != nil || !bytes.Equal(pt, k[:]) {
		t.Fatalf("SymmetricOpen does not open the wallet's: %v", err)
	}
	if got, err := CheckOpened(pt, Commitment(k)); err != nil || got != k {
		t.Fatalf("the opened key: %v", err)
	}
}

// A key with a leading zero byte survives the wrap whole: nothing trims it.
func TestLeadingZeroKey(t *testing.T) {
	k := key(0x11)
	k[0], k[1] = 0, 0
	wrap := key(0x22)
	sealed, err := SymmetricSeal(wrap[:], key(0x33), k[:])
	if err != nil {
		t.Fatal(err)
	}
	pt, err := SymmetricOpen(wrap[:], sealed)
	if err != nil || len(pt) != 32 {
		t.Fatalf("opened %d bytes, %v", len(pt), err)
	}
	if got, err := CheckOpened(pt, Commitment(k)); err != nil || got != k {
		t.Fatal(err)
	}
}
