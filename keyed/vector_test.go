package keyed_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/keyed"
)

// keyedVector is testdata/vectors/keyed-v1.json. tools/vectors computed it
// with the standard library alone: SHA-256, math/big and AES-256-GCM with a
// 32-byte nonce. Its first key is the one BRC-369 prints, with the
// symmetric key and commitment the document prints for it.
type keyedVector struct {
	DomainSymmetric  string `json:"domainSymmetric"`
	DomainCommitment string `json:"domainCommitment"`
	OrderHex         string `json:"orderHex"`
	Keys             []struct {
		Name          string `json:"name"`
		KeyHex        string `json:"keyHex"`
		Scalar        bool   `json:"scalar"`
		SymmetricHex  string `json:"symmetricHex"`
		CommitmentHex string `json:"commitmentHex"`
	} `json:"keys"`
	Opened []struct {
		Name          string `json:"name"`
		PlaintextHex  string `json:"plaintextHex"`
		CommitmentHex string `json:"commitmentHex"`
		Refusal       string `json:"refusal"`
	} `json:"opened"`
	Sealed []struct {
		Name         string `json:"name"`
		KeyHex       string `json:"keyHex"`
		IVHex        string `json:"ivHex"`
		PlaintextHex string `json:"plaintextHex"`
		SealedHex    string `json:"sealedHex"`
	} `json:"sealed"`
	Opens []struct {
		Name      string `json:"name"`
		KeyHex    string `json:"keyHex"`
		SealedHex string `json:"sealedHex"`
		Opens     bool   `json:"opens"`
	} `json:"opens"`
}

func loadVector(t *testing.T) *keyedVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "keyed-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v keyedVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func key32(t *testing.T, s string) [32]byte {
	t.Helper()
	b := unhex(t, s)
	if len(b) != 32 {
		t.Fatalf("%s is %d bytes", s, len(b))
	}
	return [32]byte(b)
}

func TestVectorKeys(t *testing.T) {
	v := loadVector(t)
	if v.DomainSymmetric != keyed.DomainSymmetric || v.DomainCommitment != keyed.DomainCommitment {
		t.Fatal("the domain strings differ from the vector's")
	}
	if new(big.Int).SetBytes(unhex(t, v.OrderHex)).Cmp(ec.S256().N) != 0 {
		t.Fatal("the vector's order is not the curve's")
	}
	if len(v.Keys) < 8 {
		t.Fatalf("%d keys", len(v.Keys))
	}
	for _, c := range v.Keys {
		k := key32(t, c.KeyHex)
		if err := keyed.CheckScalar(k); (err == nil) != c.Scalar || (err != nil && !errors.Is(err, keyed.ErrScalar)) {
			t.Errorf("%s: CheckScalar %v, want scalar %v", c.Name, err, c.Scalar)
		}
		if s := keyed.SymmetricKey(k); hex.EncodeToString(s[:]) != c.SymmetricHex {
			t.Errorf("%s: symmetric key %x", c.Name, s)
		}
		if m := keyed.Commitment(k); hex.EncodeToString(m[:]) != c.CommitmentHex {
			t.Errorf("%s: commitment %x", c.Name, m)
		}
	}
}

func TestVectorOpened(t *testing.T) {
	v := loadVector(t)
	want := map[string]error{"unwrap": keyed.ErrUnwrap, "scalar": keyed.ErrScalar, "commitment": keyed.ErrCommitment}
	seen := map[string]bool{}
	for _, c := range v.Opened {
		pt := unhex(t, c.PlaintextHex)
		k, err := keyed.CheckOpened(pt, key32(t, c.CommitmentHex))
		seen[c.Refusal] = true
		if c.Refusal == "" {
			if err != nil || !bytes.Equal(k[:], pt) {
				t.Errorf("%s: %v", c.Name, err)
			}
			continue
		}
		if !errors.Is(err, want[c.Refusal]) {
			t.Errorf("%s: %v, want %s", c.Name, err, c.Refusal)
		}
	}
	for _, r := range []string{"", "unwrap", "scalar", "commitment"} {
		if !seen[r] {
			t.Errorf("no case for %q", r)
		}
	}
}

func TestVectorSymmetricForm(t *testing.T) {
	v := loadVector(t)
	if len(v.Sealed) < 5 || len(v.Opens) < 9 {
		t.Fatalf("%d sealed, %d opens", len(v.Sealed), len(v.Opens))
	}
	for _, c := range v.Sealed {
		key, pt := unhex(t, c.KeyHex), unhex(t, c.PlaintextHex)
		got, err := keyed.SymmetricSeal(key, key32(t, c.IVHex), pt)
		if err != nil || hex.EncodeToString(got) != c.SealedHex {
			t.Errorf("%s: sealed %x (%v)", c.Name, got, err)
			continue
		}
		if len(got) != len(pt)+keyed.Overhead {
			t.Errorf("%s: %d bytes for a plaintext of %d", c.Name, len(got), len(pt))
		}
		back, err := keyed.SymmetricOpen(key, got)
		if err != nil || !bytes.Equal(back, pt) {
			t.Errorf("%s: opened %x (%v)", c.Name, back, err)
		}
	}
	for _, c := range v.Opens {
		_, err := keyed.SymmetricOpen(unhex(t, c.KeyHex), unhex(t, c.SealedHex))
		if (err == nil) != c.Opens {
			t.Errorf("%s: %v, want opens %v", c.Name, err, c.Opens)
		}
	}
}
