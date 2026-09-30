package guard

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

type pubkeyVector struct {
	Cases []struct {
		Name      string `json:"name"`
		Accept    bool   `json:"accept"`
		SDKParses bool   `json:"sdkParses"`
		KeyHex    string `json:"keyHex"`
	} `json:"cases"`
}

func readPubkeyVector(t testing.TB) pubkeyVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "pubkeys-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v pubkeyVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every case of the shared key vector. A key is accepted exactly when the
// independent rule accepts it, and an accepted key writes back as the bytes
// it was read from. The SDK's verdict is re-checked, since the cases it
// takes and the rule refuses are why ParsePubKey exists.
func TestParsePubKeyIndependentVector(t *testing.T) {
	var aliases int
	for _, c := range readPubkeyVector(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			b := unhex(t, c.KeyHex)
			k, err := ParsePubKey(b)
			if c.Accept != (err == nil) || (err != nil && !errors.Is(err, ErrPubKey)) {
				t.Fatalf("ParsePubKey: %v, accept %v", err, c.Accept)
			}
			if c.Accept && !bytes.Equal(k.Compressed(), b) {
				t.Fatal("an accepted key does not write back as itself")
			}
			if _, err := ParsePubKeyHex(c.KeyHex); c.Accept != (err == nil) {
				t.Fatalf("ParsePubKeyHex: %v, accept %v", err, c.Accept)
			}
			_, sdkErr := ec.PublicKeyFromBytes(b)
			if c.SDKParses != (sdkErr == nil) {
				t.Fatalf("go-sdk parses: %v, the vector says %v", sdkErr == nil, c.SDKParses)
			}
			if !c.Accept && c.SDKParses && len(b) == 33 {
				aliases++
			}
		})
	}
	if aliases == 0 {
		t.Fatal("no case go-sdk takes and the rule refuses: the vector no longer shows the alias")
	}
}

func TestParsePubKeyHexRefusesWhatIsNotHex(t *testing.T) {
	for _, s := range []string{"", "zz", "02" + strings.Repeat("0", 63)} {
		if _, err := ParsePubKeyHex(s); !errors.Is(err, ErrPubKey) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

// FuzzParsePubKey: whatever ParsePubKey accepts is 33 bytes, is a point on
// the curve, the SDK parses it, and it writes back as itself.
func FuzzParsePubKey(f *testing.F) {
	for _, c := range readPubkeyVector(f).Cases {
		f.Add(unhex(f, c.KeyHex))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		k, err := ParsePubKey(b)
		if err != nil {
			if !errors.Is(err, ErrPubKey) {
				t.Fatalf("a refusal that is not ErrPubKey: %v", err)
			}
			return
		}
		if len(b) != 33 || !k.Validate() || !bytes.Equal(k.Compressed(), b) {
			t.Fatalf("accepted %s", hex.EncodeToString(b))
		}
		if _, err := ec.PublicKeyFromBytes(b); err != nil {
			t.Fatalf("accepted what the SDK refuses: %v", err)
		}
	})
}
