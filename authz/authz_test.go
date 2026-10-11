package authz

import (
	"bytes"
	"context"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

func key(b byte) *ec.PrivateKey {
	k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{b}, 32))
	return k
}

func pw(t *testing.T, k *ec.PrivateKey) wallet.Interface {
	t.Helper()
	w, err := wallet.NewCompletedProtoWallet(k)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func receipt() *Receipt {
	return &Receipt{
		By:         key(0x41).PubKey().Compressed(),
		Org:        key(0x51).PubKey().Compressed(),
		Hall:       "example_hall_vkqzmtrwxp",
		Subject:    key(0x52).PubKey().Compressed(),
		Permission: "bstore/get/photos/a.jpg",
		Allow:      true,
		Seq:        7,
		Token:      [32]byte{1, 2, 3},
		Grants:     [][32]byte{{1}, {2}},
		Policies:   [][32]byte{{3}},
		Time:       1800000000,
	}
}

// TestSignVerify signs a receipt through a wallet and checks it with nothing
// but the signer's identity key, which is what a checker has.
func TestSignVerify(t *testing.T) {
	ctx := context.Background()
	service := key(0x41)
	s, err := Sign(ctx, pw(t, service), "example.com", receipt())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify()
	if err != nil {
		t.Fatalf("a receipt this wallet signed does not verify: %v", err)
	}
	if got.Permission != "bstore/get/photos/a.jpg" || !got.Allow || got.Seq != 7 {
		t.Fatalf("round trip changed the receipt: %+v", got)
	}
	// Another service's key does not verify it, however well formed.
	other := receipt()
	other.By = key(0x42).PubKey().Compressed()
	b, err := other.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Signed{Record: b, Signature: s.Signature}).Verify(); err != ErrSignature {
		t.Fatalf("a receipt claiming another signer verified: %v", err)
	}
	// One flipped byte in the record breaks it.
	bad := append([]byte(nil), s.Record...)
	bad[len(bad)-1] ^= 0x01
	if _, err := (&Signed{Record: bad, Signature: s.Signature}).Verify(); err == nil {
		t.Fatal("a tampered receipt verified")
	}
	// A signature that is not strict low-S DER is refused by its form.
	if _, err := (&Signed{Record: s.Record, Signature: []byte{0x30, 0x00}}).Verify(); err != ErrSignature {
		t.Fatalf("a malformed signature: %v", err)
	}
}

// TestReceiptRules walks the rules of the record, each case breaking one.
func TestReceiptRules(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*Receipt)
	}{
		{"signer is not a key", func(r *Receipt) { r.By = []byte{1, 2, 3} }},
		{"organization is not a key", func(r *Receipt) { r.Org = nil }},
		{"subject is not a key", func(r *Receipt) { r.Subject = bytes.Repeat([]byte{9}, 33) }},
		{"no hall", func(r *Receipt) { r.Hall = "" }},
		{"no permission", func(r *Receipt) { r.Permission = "" }},
		{"permission over the bound", func(r *Receipt) { r.Permission = strings.Repeat("p", MaxPermission+1) }},
		{"grants not ascending", func(r *Receipt) { r.Grants = [][32]byte{{2}, {1}} }},
		{"policies over the bound", func(r *Receipt) { r.Policies = make([][32]byte, MaxUsed+1) }},
		{"time 0", func(r *Receipt) { r.Time = 0 }},
		{"an allow with a reason", func(r *Receipt) { r.Reason = "no-match" }},
	} {
		r := receipt()
		c.mutate(r)
		if _, err := r.Encode(); err == nil {
			t.Fatalf("%s: encoded", c.name)
		}
	}
	// A denial carries its reason.
	r := receipt()
	r.Allow, r.Reason = false, "not-member"
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil || got.Reason != "not-member" || got.Allow {
		t.Fatalf("a denial does not round trip: %v %+v", err, got)
	}
}

// TestUnknownKeyPreserved pins the rule every record of this family follows:
// a key above the last this version defines is kept verbatim.
func TestUnknownKeyPreserved(t *testing.T) {
	r := receipt()
	r.Extra = cbor.Map{{Key: uint64(rkLast + 1), Val: "later"}}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extra) != 1 || got.Extra[0].Val != "later" {
		t.Fatalf("unknown key not preserved: %+v", got.Extra)
	}
	again, err := got.Encode()
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("re-encode changed the bytes: %v", err)
	}
}

// TestDecodeRefusals covers what an encoder cannot produce.
func TestDecodeRefusals(t *testing.T) {
	b, err := receipt().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(append(b, 0)); !bytes.Contains([]byte(err.Error()), []byte("cbor")) {
		t.Fatalf("trailing byte: %v", err)
	}
	wrong := bytes.Replace(b, MagicReceipt, []byte{'a', 'z', 'r', 0x02}, 1)
	if _, err := Decode(wrong); err != record.ErrMagic {
		t.Fatalf("a later version's magic: %v", err)
	}
	m, err := cbor.DecodeValue(b)
	if err != nil {
		t.Fatal(err)
	}
	var without cbor.Map
	for _, p := range m.(cbor.Map) {
		if p.Key == uint64(RKPermission) {
			continue
		}
		without = append(without, p)
	}
	enc, err := cbor.Encode(without)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(enc); err == nil {
		t.Fatal("a receipt with no permission decoded")
	}
}
