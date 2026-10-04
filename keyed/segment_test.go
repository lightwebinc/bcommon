package keyed_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightwebinc/bcommon/keyed"
)

// segmentVector is testdata/vectors/keyed-segment-v1.json. tools/vectors
// computed it with the standard library alone: SHA-256, and AES-256-GCM
// with a 12-byte nonce for a segment and a 32-byte one for a wrap.
type segmentVector struct {
	Domains  []string `json:"epochWrapDomains"`
	Segments []struct {
		Name         string `json:"name"`
		KeyHex       string `json:"keyHex"`
		SaltHex      string `json:"saltHex"`
		IVHex        string `json:"ivHex"`
		PlaintextHex string `json:"plaintextHex"`
		SealedHex    string `json:"sealedHex"`
	} `json:"segments"`
	Opens []struct {
		Name         string `json:"name"`
		KeyHex       string `json:"keyHex"`
		SaltHex      string `json:"saltHex"`
		SealedHex    string `json:"sealedHex"`
		Refusal      string `json:"refusal"`
		PlaintextHex string `json:"plaintextHex"`
	} `json:"opens"`
	Wraps []struct {
		Name              string `json:"name"`
		Domain            string `json:"domain"`
		EpochKeyHex       string `json:"epochKeyHex"`
		EpochSymmetricHex string `json:"epochSymmetricHex"`
		ContentIDHex      string `json:"contentIdHex"`
		KeyHex            string `json:"keyHex"`
		CommitmentHex     string `json:"commitmentHex"`
		IVHex             string `json:"ivHex"`
		WrapKeyHex        string `json:"wrapKeyHex"`
		WrapHex           string `json:"wrapHex"`
	} `json:"wraps"`
	Unwraps []struct {
		Name              string `json:"name"`
		Domain            string `json:"domain"`
		EpochSymmetricHex string `json:"epochSymmetricHex"`
		ContentIDHex      string `json:"contentIdHex"`
		WrapHex           string `json:"wrapHex"`
		CommitmentHex     string `json:"commitmentHex"`
		Refusal           string `json:"refusal"`
		KeyHex            string `json:"keyHex"`
	} `json:"unwraps"`
}

func loadSegment(t *testing.T) *segmentVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "keyed-segment-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v segmentVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func salt4(t *testing.T, s string) [keyed.SaltLen]byte {
	t.Helper()
	b := unhex(t, s)
	if len(b) != keyed.SaltLen {
		t.Fatalf("salt %s is %d bytes", s, len(b))
	}
	return [keyed.SaltLen]byte(b)
}

func TestVectorSegments(t *testing.T) {
	v := loadSegment(t)
	if len(v.Segments) < 7 {
		t.Fatalf("%d segments", len(v.Segments))
	}
	for _, c := range v.Segments {
		k, salt, pt := key32(t, c.KeyHex), salt4(t, c.SaltHex), unhex(t, c.PlaintextHex)
		if iv := keyed.SegmentIV(salt, 0); hex.EncodeToString(iv[:]) != c.IVHex {
			t.Errorf("%s: IV %x", c.Name, iv)
		}
		got, err := keyed.SealSegment(k, salt, pt)
		if err != nil || hex.EncodeToString(got) != c.SealedHex {
			t.Errorf("%s: sealed %d bytes (%v)", c.Name, len(got), err)
			continue
		}
		if len(got) != len(pt)+keyed.TagLen {
			t.Errorf("%s: %d bytes for a plaintext of %d", c.Name, len(got), len(pt))
		}
		back, err := keyed.OpenSegment(k, salt, got)
		if err != nil || !bytes.Equal(back, pt) {
			t.Errorf("%s: opened (%v)", c.Name, err)
		}
	}
}

func TestVectorSegmentOpens(t *testing.T) {
	v := loadSegment(t)
	want := map[string]error{"segment": keyed.ErrSegment, "scalar": keyed.ErrScalar, "tag": keyed.ErrTag}
	seen := map[string]bool{}
	for _, c := range v.Opens {
		seen[c.Refusal] = true
		pt, err := keyed.OpenSegment(key32(t, c.KeyHex), salt4(t, c.SaltHex), unhex(t, c.SealedHex))
		if c.Refusal == "" {
			if err != nil || hex.EncodeToString(pt) != c.PlaintextHex {
				t.Errorf("%s: %v", c.Name, err)
			}
			continue
		}
		if !errors.Is(err, want[c.Refusal]) || pt != nil {
			t.Errorf("%s: %v, want %s", c.Name, err, c.Refusal)
		}
	}
	for _, r := range []string{"", "segment", "scalar", "tag"} {
		if !seen[r] {
			t.Errorf("no case for %q", r)
		}
	}
}

func TestVectorEpochWraps(t *testing.T) {
	v := loadSegment(t)
	if len(v.Domains) != 2 || len(v.Wraps) < 4 {
		t.Fatalf("%d domains, %d wraps", len(v.Domains), len(v.Wraps))
	}
	domains := map[string]bool{}
	for _, c := range v.Wraps {
		domains[c.Domain] = true
		ek, sym, cid, k := key32(t, c.EpochKeyHex), key32(t, c.EpochSymmetricHex), key32(t, c.ContentIDHex), key32(t, c.KeyHex)
		if s := keyed.SymmetricKey(ek); s != sym {
			t.Errorf("%s: epoch symmetric key %x", c.Name, s)
		}
		if wk := keyed.EpochWrapKey(c.Domain, sym, cid); hex.EncodeToString(wk[:]) != c.WrapKeyHex {
			t.Errorf("%s: wrap key %x", c.Name, wk)
		}
		w, err := keyed.WrapToEpochWithIV(c.Domain, sym, cid, k, key32(t, c.IVHex))
		if err != nil || hex.EncodeToString(w) != c.WrapHex {
			t.Errorf("%s: wrap %x (%v)", c.Name, w, err)
		}
		w, err = keyed.WrapToEpoch(c.Domain, sym, cid, k, bytes.NewReader(unhex(t, c.IVHex)))
		if err != nil || hex.EncodeToString(w) != c.WrapHex {
			t.Errorf("%s: wrap from a reader %x (%v)", c.Name, w, err)
		}
		got, err := keyed.UnwrapFromEpoch(c.Domain, sym, cid, w, key32(t, c.CommitmentHex))
		if err != nil || got != k {
			t.Errorf("%s: unwrap (%v)", c.Name, err)
		}
	}
	for _, d := range v.Domains {
		if !domains[d] {
			t.Errorf("no wrap under %q", d)
		}
	}
}

func TestVectorEpochUnwraps(t *testing.T) {
	v := loadSegment(t)
	want := map[string]error{"unwrap": keyed.ErrUnwrap, "commitment": keyed.ErrCommitment}
	seen := map[string]bool{}
	for _, c := range v.Unwraps {
		seen[c.Refusal] = true
		k, err := keyed.UnwrapFromEpoch(c.Domain, key32(t, c.EpochSymmetricHex), key32(t, c.ContentIDHex), unhex(t, c.WrapHex), key32(t, c.CommitmentHex))
		if c.Refusal == "" {
			if err != nil || hex.EncodeToString(k[:]) != c.KeyHex {
				t.Errorf("%s: %v", c.Name, err)
			}
			continue
		}
		if !errors.Is(err, want[c.Refusal]) {
			t.Errorf("%s: %v, want %s", c.Name, err, c.Refusal)
		}
	}
	for _, r := range []string{"", "unwrap", "commitment"} {
		if !seen[r] {
			t.Errorf("no case for %q", r)
		}
	}
}

// The bounds the vectors do not carry: a plaintext of MaxSegment bytes is
// one segment and one more is refused, as is an empty one; an empty domain
// string is refused on both sides; a key that is not a scalar wraps
// nothing; and a reader that runs dry is the reader's error.
func TestSegmentBounds(t *testing.T) {
	k, err := keyed.SampleKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	salt, err := keyed.SampleSalt(nil)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, keyed.MaxSegment)
	ct, err := keyed.SealSegment(k, salt, big)
	if err != nil || len(ct) != keyed.MaxSegment+keyed.TagLen {
		t.Fatalf("MaxSegment bytes: %d (%v)", len(ct), err)
	}
	if pt, err := keyed.OpenSegment(k, salt, ct); err != nil || len(pt) != keyed.MaxSegment {
		t.Fatalf("open MaxSegment bytes: %v", err)
	}
	if _, err := keyed.SealSegment(k, salt, append(big, 0)); !errors.Is(err, keyed.ErrSegment) {
		t.Errorf("MaxSegment + 1 bytes: %v", err)
	}
	if _, err := keyed.OpenSegment(k, salt, append(ct, 0)); !errors.Is(err, keyed.ErrSegment) {
		t.Errorf("open MaxSegment + TagLen + 1 bytes: %v", err)
	}
	if _, err := keyed.SealSegment(k, salt, nil); !errors.Is(err, keyed.ErrSegment) {
		t.Errorf("empty plaintext: %v", err)
	}
	if _, err := keyed.SealSegment([32]byte{}, salt, []byte{1}); !errors.Is(err, keyed.ErrScalar) {
		t.Errorf("zero key: %v", err)
	}
	var sym, cid [32]byte
	if _, err := keyed.WrapToEpoch("", sym, cid, k, nil); !errors.Is(err, keyed.ErrDomain) {
		t.Errorf("wrap under an empty domain: %v", err)
	}
	if _, err := keyed.UnwrapFromEpoch("", sym, cid, make([]byte, keyed.WrappedKeyLen), keyed.Commitment(k)); !errors.Is(err, keyed.ErrDomain) {
		t.Errorf("unwrap under an empty domain: %v", err)
	}
	if _, err := keyed.WrapToEpoch("x epoch wrap v1", sym, cid, [32]byte{}, nil); !errors.Is(err, keyed.ErrScalar) {
		t.Errorf("wrap of zero: %v", err)
	}
	if _, err := keyed.WrapToEpoch("x epoch wrap v1", sym, cid, k, bytes.NewReader(make([]byte, 31))); err == nil {
		t.Error("a 31-byte reader gave an IV")
	}
	if _, err := keyed.SampleSalt(bytes.NewReader(make([]byte, 3))); err == nil {
		t.Error("a 3-byte reader gave a salt")
	}
	w, err := keyed.WrapToEpoch("x epoch wrap v1", sym, cid, k, nil)
	if err != nil || len(w) != keyed.WrappedKeyLen {
		t.Fatalf("wrap: %d bytes (%v)", len(w), err)
	}
	if got, err := keyed.UnwrapFromEpoch("x epoch wrap v1", sym, cid, w, keyed.Commitment(k)); err != nil || got != k {
		t.Errorf("unwrap a random wrap: %v", err)
	}
}
