package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/auth/certificates"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// BRC-169 Appendix A: the lkup.net certifier, @deggen@lkup.net, and the
// handle certificate of A.3, exactly as the BRC prints it.
const (
	exCertifier = "0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9"
	exSubject   = "0359c5f3bfe249f6c0ca99d0e9cc1517da51a511f3d04f18e47a5d7ae55f04008c"
	exRevTxid   = "2b09f724127b5213ead87842deade00ef6cb1a834c951d1612e162f5891fb3cb"
	exCert      = `{
  "type": "XgCFdUfxEcI+3xtDjsIuSAjMl5EwzCUjsQc45ds1lC8=",
  "serialNumber": "JMNxKTvlkhOO88EJZRgnpTKL78dC1XwxQ9REUysjy08=",
  "subject": "0359c5f3bfe249f6c0ca99d0e9cc1517da51a511f3d04f18e47a5d7ae55f04008c",
  "certifier": "0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9",
  "revocationOutpoint": {
    "txid": "2b09f724127b5213ead87842deade00ef6cb1a834c951d1612e162f5891fb3cb",
    "vout": 0
  },
  "fields": {
    "domain": "bGt1cC5uZXQ=",
    "handle": "ZGVnZ2Vu"
  },
  "signature": "30450221008becb25058954be7cf6f8c46d3a0411a85a9aed55ef90466651fc75374e2bdcf02205a232796a1c2dd0bd7096428a5e6fb766eee404f441a93a261986486ba3b553c"
}`
	// exPreimage is A.3's signature preimage, CertificateBinary without the
	// signature.
	exPreimage = "5e00857547f111c23edf1b438ec22e4808cc979130cc2523b10738e5db35942f" +
		"24c371293be592138ef3c109651827a5328befc742d57c3143d444532b23cb4f" +
		"0359c5f3bfe249f6c0ca99d0e9cc1517da51a511f3d04f18e47a5d7ae55f04008c" +
		"0371f0ec5992a9d38e09fe528e367890969c66eaebdb01b4d35a2fc0d61251b3f9" +
		"2b09f724127b5213ead87842deade00ef6cb1a834c951d1612e162f5891fb3cb" +
		"00" + "02" +
		"06646f6d61696e" + "0c62477431634335755a58513d" +
		"0668616e646c65" + "085a47566e5a325675"
)

func exHandle(t *testing.T, cert string) *Handle {
	t.Helper()
	k, err := parseIdentityKey(exSubject)
	if err != nil {
		t.Fatal(err)
	}
	return &Handle{Acct: Acct{Handle: "deggen", Domain: "lkup.net"}, IdentityKey: k, Certificate: json.RawMessage(cert), TTL: 3600}
}

func exManifest(key string) *Manifest { return &Manifest{Trust: &Trust{Name: "LkUp", PublicKey: key}} }

// spender is a revocation source with one answer.
type spender struct {
	by  string
	err error
}

func (s spender) Spender(context.Context, string, uint32) (string, error) { return s.by, s.err }

// The certificate type is the one section 4.5 derives, and the worked
// example's preimage is what go-sdk's serializer builds from its JSON.
func TestHandleCertificateTypeAndPreimage(t *testing.T) {
	sum := sha256.Sum256([]byte("metanet-handles handle certificate v1"))
	if got := base64.StdEncoding.EncodeToString(sum[:]); got != HandleCertificateType {
		t.Fatalf("type %s", got)
	}
	var w wireCertificate
	if err := json.Unmarshal([]byte(exCert), &w); err != nil {
		t.Fatal(err)
	}
	h := exHandle(t, exCert)
	if _, err := checkCertificate(context.Background(), h, exManifest(exCertifier)); err != nil {
		t.Fatal(err)
	}
	subject, _ := certKey("subject", w.Subject)
	certifier, _ := certKey("certifier", w.Certifier)
	op, _ := parseOutpoint(w.RevocationOutpoint)
	fields := map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64{}
	for n, v := range w.Fields {
		fields[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(v)
	}
	b, err := certificates.NewCertificate(wallet.StringBase64(w.Type), wallet.StringBase64(w.SerialNumber), *subject, *certifier, op, fields, nil).ToBinary(false)
	if err != nil || hex.EncodeToString(b) != exPreimage {
		t.Fatalf("preimage %x, %v", b, err)
	}
}

// BRC-169's worked certificate passes checks 1 to 4. Without a revocation
// source it is unverified; with one it is verified, revoked, or unverified
// when no source can answer.
func TestCheckHandleCertificateWorkedExample(t *testing.T) {
	ctx := context.Background()
	m := exManifest(exCertifier)

	h := exHandle(t, exCert)
	if err := VerifyHandleCertificate(h, m); !errors.Is(err, ErrCertificateUnverified) || errors.Is(err, ErrCertificate) || h.CertificateChecked {
		t.Fatalf("no source: %v, checked %v", err, h.CertificateChecked)
	}
	if h.RevocationOutpoint != exRevTxid+".0" {
		t.Fatalf("revocation outpoint %q", h.RevocationOutpoint)
	}

	before := time.Now()
	if err := CheckHandleCertificate(ctx, h, m, spender{err: errors.New("down")}, spender{}); err != nil || !h.CertificateChecked {
		t.Fatalf("unspent: %v", err)
	}
	if h.CertificateCheckedAt.Before(before) || !h.CertificateFresh(time.Now(), RevocationFreshness) {
		t.Fatalf("checked at %v", h.CertificateCheckedAt)
	}

	err := CheckHandleCertificate(ctx, h, m, spender{}, spender{by: strings.Repeat("ab", 32)})
	if !errors.Is(err, ErrCertificateRevoked) || !errors.Is(err, ErrRevoked) || h.CertificateChecked || h.CertificateFresh(time.Now(), time.Hour) {
		t.Fatalf("spent: %v", err)
	}

	err = CheckHandleCertificate(ctx, h, m, spender{err: errors.New("no index")}, nil)
	if !errors.Is(err, ErrCertificateUnverified) || !strings.Contains(err.Error(), "no index") || h.CertificateChecked {
		t.Fatalf("no answer: %v", err)
	}
}

// Freshness is bounded by the caller's bound and by the TTL.
func TestCertificateFresh(t *testing.T) {
	now := time.Now()
	h := &Handle{CertificateChecked: true, CertificateCheckedAt: now.Add(-30 * time.Second), TTL: 3600}
	if !h.CertificateFresh(now, RevocationFreshness) || h.CertificateFresh(now, 10*time.Second) {
		t.Fatal("bound")
	}
	h.TTL = 20
	if h.CertificateFresh(now, RevocationFreshness) {
		t.Fatal("a check older than the TTL is fresh")
	}
	h.TTL = 0
	if h.CertificateFresh(now, RevocationFreshness) {
		t.Fatal("a TTL of 0 allows a cached result")
	}
	if (&Handle{CertificateCheckedAt: now, TTL: 60}).CertificateFresh(now, time.Minute) {
		t.Fatal("an unchecked certificate is fresh")
	}
}

// Every rule of section 4.1 and every malformed certificate is refused,
// with ErrCertificate, and no source is asked.
func TestCheckHandleCertificateRefusals(t *testing.T) {
	edit := func(old, new string) string {
		if !strings.Contains(exCert, old) {
			t.Fatalf("no %q in the example", old)
		}
		return strings.Replace(exCert, old, new, 1)
	}
	highS := func() string {
		sig, _ := hex.DecodeString("30450221008becb25058954be7cf6f8c46d3a0411a85a9aed55ef90466651fc75374e2bdcf02205a232796a1c2dd0bd7096428a5e6fb766eee404f441a93a261986486ba3b553c")
		s, err := ec.ParseDERSignature(sig)
		if err != nil {
			t.Fatal(err)
		}
		// n - s, encoded by hand: the same signature with high S.
		n := ec.S256().N
		hs := new(big.Int).Sub(n, s.S)
		r := s.R.Bytes()
		if r[0]&0x80 != 0 {
			r = append([]byte{0}, r...)
		}
		sb := hs.Bytes()
		if sb[0]&0x80 != 0 {
			sb = append([]byte{0}, sb...)
		}
		der := append([]byte{0x30, byte(4 + len(r) + len(sb)), 0x02, byte(len(r))}, r...)
		der = append(der, 0x02, byte(len(sb)))
		der = append(der, sb...)
		return edit("30450221008becb25058954be7cf6f8c46d3a0411a85a9aed55ef90466651fc75374e2bdcf02205a232796a1c2dd0bd7096428a5e6fb766eee404f441a93a261986486ba3b553c", hex.EncodeToString(der))
	}
	other := "02" + strings.Repeat("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 1)
	for _, c := range []struct {
		name, cert string
		manifest   *Manifest
		acct       *Acct
		want       string
	}{
		{"no certificate", ``, nil, nil, "no certificate"},
		{"null", `null`, nil, nil, "no certificate"},
		{"not JSON", `{"type":`, nil, nil, ""},
		{"a duplicate member", edit(`"handle": "ZGVnZ2Vu"`, `"handle": "ZGVnZ2Vu", "handle": "ZGVnZ2Vu"`), nil, nil, "appears twice"},
		{"a duplicate top-level member", edit(`"type": `, `"type": "x", "type": `), nil, nil, "appears twice"},
		{"another type", edit("XgCFdUfxEcI+3xtDjsIuSAjMl5EwzCUjsQc45ds1lC8=", "30kchAJIGfLxzCNloCJNLI3AtkgA8UbkxlXU2Cj4PpA="), nil, nil, "not the handle certificate type"},
		{"a short serial", edit("JMNxKTvlkhOO88EJZRgnpTKL78dC1XwxQ9REUysjy08=", "JMNx"), nil, nil, "serialNumber"},
		{"an uncompressed subject", edit(`"subject": "03`, `"subject": "04`), nil, nil, "subject"},
		{"no outpoint", edit(`"vout": 0`, `"index": 0`), nil, nil, "revocationOutpoint"},
		{"the disabled outpoint", edit(exRevTxid, strings.Repeat("0", 64)), nil, nil, "disabled sentinel"},
		{"a high-S signature", highS(), nil, nil, "strict low-S DER"},
		{"a tampered field", edit(`"handle": "ZGVnZ2Vu"`, `"handle": "ZGVnZ2Vv"`), nil, nil, "signature does not verify"},
		{"no fields", edit(`"domain": "bGt1cC5uZXQ=",
    "handle": "ZGVnZ2Vu"`, ``), nil, nil, "no fields"},
		{"another certifier", exCert, exManifest(other), nil, "is not the domain's metanet.trust.publicKey"},
		{"no trust anchor", exCert, &Manifest{}, nil, "no metanet.trust.publicKey"},
		{"another handle", exCert, nil, &Acct{Handle: "alice", Domain: "lkup.net"}, "the certificate binds deggen@lkup.net, not alice@lkup.net"},
		{"another domain", exCert, nil, &Acct{Handle: "deggen", Domain: "lkup.org"}, "not deggen@lkup.org"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := exHandle(t, c.cert)
			if c.acct != nil {
				h.Acct = *c.acct
			}
			m := c.manifest
			if m == nil {
				m = exManifest(exCertifier)
			}
			asked := false
			src := spenderFunc(func() { asked = true })
			err := CheckHandleCertificate(context.Background(), h, m, src)
			if !errors.Is(err, ErrCertificate) || !strings.Contains(err.Error(), c.want) || h.CertificateChecked || asked {
				t.Fatalf("got %v (source asked %v)", err, asked)
			}
		})
	}
	// The subject must be the key the endpoint answered.
	h := exHandle(t, exCert)
	h.IdentityKey[32] ^= 1
	if err := VerifyHandleCertificate(h, exManifest(exCertifier)); !errors.Is(err, ErrCertificate) || !strings.Contains(err.Error(), "is not the identityKey") {
		t.Fatalf("subject: %v", err)
	}
	if err := CheckHandleCertificate(context.Background(), nil, nil); !errors.Is(err, ErrCertificate) {
		t.Fatalf("nil handle: %v", err)
	}
}

type spenderFunc func()

func (f spenderFunc) Spender(context.Context, string, uint32) (string, error) { f(); return "", nil }

// A production certificate encrypts its fields. Served with a keyring for
// the verifier anyone, any reader opens the handle and domain and the
// certificate verifies; served without one, the fields are not the handle
// and it is refused; a keyring that does not open them is refused.
func TestCheckHandleCertificateEncryptedFields(t *testing.T) {
	ctx := context.Background()
	certKeyPriv, _ := ec.PrivateKeyFromBytes(sha("certifier"))
	subjPriv, _ := ec.PrivateKeyFromBytes(sha("subject"))
	certifier, _ := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: certKeyPriv})
	subjectW, _ := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: subjPriv})
	subj := subjPriv.PubKey()
	rev := chainhash.Hash(sha("revocation"))
	mc, err := certificates.IssueCertificateForSubject(ctx, certifier,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: subj},
		map[string]string{"handle": "alice", "domain": "example.com"}, HandleCertificateType,
		func(string) (*transaction.Outpoint, error) { return &transaction.Outpoint{Txid: rev, Index: 1}, nil }, "")
	if err != nil {
		t.Fatal(err)
	}
	certPub := certKeyPriv.PubKey()
	ring, err := certificates.CreateKeyringForVerifier(ctx, subjectW,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: certPub},
		wallet.Counterparty{Type: wallet.CounterpartyTypeAnyone},
		mc.Fields, []wallet.CertificateFieldNameUnder50Bytes{"handle", "domain"}, mc.MasterKeyring, mc.SerialNumber, false, "")
	if err != nil {
		t.Fatal(err)
	}
	wire := func(keyring map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64) string {
		doc := map[string]any{
			"type": mc.Type, "serialNumber": mc.SerialNumber,
			"subject": hex.EncodeToString(subj.Compressed()), "certifier": hex.EncodeToString(certPub.Compressed()),
			"revocationOutpoint": rev.String() + ".1", "fields": mc.Fields, "signature": hex.EncodeToString(mc.Signature),
		}
		if keyring != nil {
			doc["keyring"] = keyring
		}
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var key [33]byte
	copy(key[:], subj.Compressed())
	m := exManifest(hex.EncodeToString(certPub.Compressed()))
	h := &Handle{Acct: Acct{Handle: "alice", Domain: "example.com"}, IdentityKey: key, TTL: 60}

	h.Certificate = json.RawMessage(wire(ring))
	if err := CheckHandleCertificate(ctx, h, m, spender{}); err != nil || !h.CertificateChecked || h.RevocationOutpoint != rev.String()+".1" {
		t.Fatalf("with a keyring for anyone: %v", err)
	}

	h.Certificate = json.RawMessage(wire(nil))
	if err := CheckHandleCertificate(ctx, h, m, spender{}); !errors.Is(err, ErrCertificate) {
		t.Fatalf("encrypted fields and no keyring: %v", err)
	}

	// A keyring for another verifier does not open the fields for anyone.
	otherPriv, _ := ec.PrivateKeyFromBytes(sha("verifier"))
	theirs, err := certificates.CreateKeyringForVerifier(ctx, subjectW,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: certPub},
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: otherPriv.PubKey()},
		mc.Fields, []wallet.CertificateFieldNameUnder50Bytes{"handle", "domain"}, mc.MasterKeyring, mc.SerialNumber, false, "")
	if err != nil {
		t.Fatal(err)
	}
	h.Certificate = json.RawMessage(wire(theirs))
	if err := CheckHandleCertificate(ctx, h, m, spender{}); !errors.Is(err, ErrCertificate) || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("another verifier's keyring: %v", err)
	}

	// A keyring that reveals only one of the two fields.
	h.Certificate = json.RawMessage(wire(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64{"handle": ring["handle"]}))
	if err := CheckHandleCertificate(ctx, h, m, spender{}); !errors.Is(err, ErrCertificate) || !strings.Contains(err.Error(), `does not reveal "domain"`) {
		t.Fatalf("half a keyring: %v", err)
	}
}

func sha(s string) []byte { b := sha256.Sum256([]byte("bcommon resolve test / " + s)); return b[:] }
