package pushdrop

import (
	"crypto/rand"
	"math/big"
	mrand "math/rand/v2"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// sdkKey is the derivation through go-sdk's key deriver alone.
func sdkKey(d Derivation, identity *ec.PublicKey) (*ec.PublicKey, error) {
	return wallet.NewKeyDeriver(nil).DerivePublicKey(d.Protocol, d.KeyID,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: identity}, false)
}

func randomIdentity(t testing.TB) *ec.PublicKey {
	t.Helper()
	p, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return p.PubKey()
}

func smallIdentity(d int64) *ec.PublicKey {
	_, pub := ec.PrivateKeyFromBytes(big.NewInt(d).Bytes())
	return pub
}

// TestExpectedLockingKeyAgreesWithSDK: the fast path gives go-sdk's key for
// random identities under every shape of derivation the SDK takes (security
// levels, trimmed and upper-case names, linkage names at their bound, key ids
// up to 800 bytes, non-ASCII key ids), and the small-scalar identities.
func TestExpectedLockingKeyAgreesWithSDK(t *testing.T) {
	linkage := "specific linkage revelation "
	ds := []Derivation{
		{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: "bstore records"}, KeyID: "1"},
		{Protocol: wallet.Protocol{SecurityLevel: 0, Protocol: "  Mixed Case Name  "}, KeyID: "entry"},
		{Protocol: wallet.Protocol{SecurityLevel: 1, Protocol: "abcde"}, KeyID: strings.Repeat("k", 800)},
		{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: strings.Repeat("n", 400)}, KeyID: "x"},
		{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: linkage + strings.Repeat("n", 430-len(linkage))}, KeyID: "x"},
		{Protocol: wallet.Protocol{SecurityLevel: 1, Protocol: "protocol sample"}, KeyID: "ünïcödé 🙂 key"},
	}
	n := 3000
	if testing.Short() {
		n = 300
	}
	for i := 0; i < n; i++ {
		id := randomIdentity(t)
		if i < 8 {
			id = smallIdentity(int64(i + 1))
		}
		d := ds[i%len(ds)]
		if i%7 == 0 {
			b := make([]byte, 1+mrand.IntN(64))
			_, _ = rand.Read(b)
			d.KeyID = string(b) // arbitrary bytes, as the SDK takes them
		}
		want, err := sdkKey(d, id)
		if err != nil {
			t.Fatalf("sdk: %v", err)
		}
		got, ok := anyoneChild(id, d.invoice())
		if !ok {
			t.Fatalf("fast path refused a canonical identity %x", id.Compressed())
		}
		if !got.IsEqual(want) {
			t.Fatalf("%q %q: fast %x, sdk %x", d.Protocol.Protocol, d.KeyID, got.Compressed(), want.Compressed())
		}
		viaAPI, err := d.ExpectedLockingKey(id)
		if err != nil || !viaAPI.IsEqual(want) {
			t.Fatalf("ExpectedLockingKey: %v", err)
		}
	}
}

// Inputs outside the fast path go to the SDK, which answers as before:
// a derivation Validate refuses errors as the SDK errors, and identity keys
// that are not canonical points get the SDK's own answer.
func TestExpectedLockingKeyFallsBackToSDK(t *testing.T) {
	id := randomIdentity(t)
	bad := Derivation{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: "abc"}, KeyID: "1"}
	if _, err := bad.ExpectedLockingKey(id); err == nil {
		t.Fatal("a name under five characters derived")
	}
	d := Derivation{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: "bstore records"}, KeyID: "1"}
	p := ec.S256().P
	for i, k := range []*ec.PublicKey{
		{Curve: ec.S256(), X: new(big.Int).Add(id.X, p), Y: id.Y},
		{Curve: ec.S256(), X: id.X, Y: new(big.Int).Add(id.Y, big.NewInt(1))},
		{Curve: ec.S256(), X: new(big.Int).Neg(id.X), Y: id.Y},
	} {
		if _, ok := anyoneChild(k, d.invoice()); ok {
			t.Fatalf("case %d: the fast path took a non-canonical key", i)
		}
		want, werr := sdkKey(d, k)
		got, gerr := d.ExpectedLockingKey(k)
		if (werr == nil) != (gerr == nil) || (werr == nil && !got.IsEqual(want)) {
			t.Fatalf("case %d: sdk %v / %v, ExpectedLockingKey %v / %v", i, want, werr, got, gerr)
		}
	}
}

func BenchmarkExpectedLockingKey(b *testing.B) {
	d := Derivation{Protocol: wallet.Protocol{SecurityLevel: 2, Protocol: "bstore records"}, KeyID: "1"}
	ids := make([]*ec.PublicKey, 64)
	for i := range ids {
		ids[i] = randomIdentity(b)
	}
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := d.ExpectedLockingKey(ids[i%len(ids)]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sdk", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := sdkKey(d, ids[i%len(ids)]); err != nil {
				b.Fatal(err)
			}
		}
	})
}
