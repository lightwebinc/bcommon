package pushdrop_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// txVector is the part of testdata/vectors/transactions-v1.json this
// package's output appears in: the derived keys, the unsigned funding lock
// and the signed state-token locks. tools/vectors built them with go-sdk's
// key deriver and PushDrop template called directly, under a derivation and
// tags that belong to no application.
type txVector struct {
	PrivateKeyHex       string `json:"privateKeyHex"`
	IdentityKeyHex      string `json:"identityKeyHex"`
	Originator          string `json:"originator"`
	SecurityLevel       int    `json:"securityLevel"`
	Protocol            string `json:"protocol"`
	ObjectKeyID         string `json:"objectKeyId"`
	StateKeyID          string `json:"stateKeyId"`
	ObjectLockingKeyHex string `json:"objectLockingKeyHex"`
	StateLockingKeyHex  string `json:"stateLockingKeyHex"`
	FundingTagHex       string `json:"fundingTagHex"`
	StateTagHex         string `json:"stateTagHex"`
	FundingLockHex      string `json:"fundingLockHex"`
	Carriers            []struct {
		PayloadHex    string `json:"payloadHex"`
		LockHex       string `json:"lockHex"`
		CommitmentHex string `json:"commitmentHex"`
	} `json:"carriers"`
	Create struct {
		Carrier int    `json:"carrier"`
		LockHex string `json:"lockHex"`
	} `json:"create"`
	Update struct {
		Carrier int    `json:"carrier"`
		LockHex string `json:"lockHex"`
	} `json:"update"`
}

func readTxVector(t *testing.T) *txVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "transactions-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v txVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	// The vector was built with the fixed test key; a vector from any other
	// key would not be the one these tests sign with.
	seed := goldentest.Fill(0x42)
	if v.PrivateKeyHex != hex.EncodeToString(seed[:]) ||
		v.IdentityKeyHex != hex.EncodeToString(goldentest.FixedKey().PubKey().Compressed()) {
		t.Fatal("the vector was not built with goldentest.FixedKey")
	}
	// An empty or renamed carrier family would pass the record-lock loop by
	// running none of it, and leave the token rows no carrier to name.
	if len(v.Carriers) == 0 {
		t.Fatal("the vector holds no carriers; the family is missing or renamed")
	}
	for _, i := range []int{v.Create.Carrier, v.Update.Carrier} {
		if i < 0 || i >= len(v.Carriers) {
			t.Fatalf("a transition names carrier %d of %d", i, len(v.Carriers))
		}
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

// Every key and lock this package derives must be the independent vector's
// byte for byte: the reader's key under each derivation, the unsigned
// funding lock, the signed record locks and the signed state-token locks,
// and DecodeTagged must read a token lock back to its fields and key.
func TestIndependentVector(t *testing.T) {
	v := readTxVector(t)
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	proto := wallet.Protocol{SecurityLevel: wallet.SecurityLevel(v.SecurityLevel), Protocol: v.Protocol}
	object := pushdrop.Derivation{Protocol: proto, KeyID: v.ObjectKeyID}
	state := pushdrop.Derivation{Protocol: proto, KeyID: v.StateKeyID}
	for _, d := range []pushdrop.Derivation{object, state} {
		if err := d.Validate(); err != nil {
			t.Fatalf("the vector's derivation %+v: %v", d, err)
		}
	}
	identity, err := ec.PublicKeyFromBytes(unhex(t, v.IdentityKeyHex))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		d    pushdrop.Derivation
		want string
	}{{object, v.ObjectLockingKeyHex}, {state, v.StateLockingKeyHex}} {
		k, err := c.d.ExpectedLockingKey(identity)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(k.Compressed()); got != c.want {
			t.Errorf("%s: reader key %s, want %s", c.d.KeyID, got, c.want)
		}
	}

	lock := func(d pushdrop.Derivation, sign bool, fields ...[]byte) string {
		t.Helper()
		s, err := d.Lock(ctx, w, v.Originator, fields, sign)
		if err != nil {
			t.Fatal(err)
		}
		return s.String()
	}
	fundingTag := unhex(t, v.FundingTagHex)
	if got := lock(object, false, fundingTag); got != v.FundingLockHex {
		t.Errorf("funding lock:\n got %s\nwant %s", got, v.FundingLockHex)
	}
	for i, c := range v.Carriers {
		if got := lock(object, true, unhex(t, c.PayloadHex)); got != c.LockHex {
			t.Errorf("carrier %d record lock:\n got %s\nwant %s", i, got, c.LockHex)
		}
	}
	stateTag := unhex(t, v.StateTagHex)
	for _, tr := range []struct {
		name    string
		carrier int
		lockHex string
	}{{"create", v.Create.Carrier, v.Create.LockHex}, {"update", v.Update.Carrier, v.Update.LockHex}} {
		c := unhex(t, v.Carriers[tr.carrier].CommitmentHex)
		if got := lock(state, true, stateTag, c); got != tr.lockHex {
			t.Errorf("%s token lock:\n got %s\nwant %s", tr.name, got, tr.lockHex)
		}
		s, err := script.NewFromHex(tr.lockHex)
		if err != nil {
			t.Fatal(err)
		}
		tagged, err := pushdrop.DecodeTagged(s, stateTag, 2)
		if err != nil {
			t.Fatalf("%s: %v", tr.name, err)
		}
		if !bytes.Equal(tagged.Fields[1], c) || hex.EncodeToString(tagged.LockingKey.Compressed()) != v.StateLockingKeyHex {
			t.Errorf("%s: decoded fields %x and key %x", tr.name, tagged.Fields, tagged.LockingKey.Compressed())
		}
		if !tagged.VerifySignature() {
			t.Errorf("%s: the vector's field signature does not verify", tr.name)
		}
	}
}
