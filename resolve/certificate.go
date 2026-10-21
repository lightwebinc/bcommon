package resolve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bsv-blockchain/go-sdk/auth/certificates"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// HandleCertificateType is the BRC-52 type of a BRC-169 handle certificate:
// the standard base64 of SHA-256("metanet-handles handle certificate v1")
// (BRC-169 section 4.5).
const HandleCertificateType = "XgCFdUfxEcI+3xtDjsIuSAjMl5EwzCUjsQc45ds1lC8="

// RevocationFreshness is how old a revocation check may be before a
// value-moving action, by default (BRC-169 section 4.2 rule 4). See
// Handle.CertificateFresh.
const RevocationFreshness = 60 * time.Second

// SpendSource names the transaction that spent an output, "" with a nil
// error only when the source shows it unspent, and an error for every other
// answer: nodeapi.SpendSource's contract, which nodeapi.WoC and
// nodeapi.Asset meet, declared here so that this package imports no other.
type SpendSource interface {
	Spender(ctx context.Context, txid string, vout uint32) (string, error)
}

// disabledOutpoint is BRC-52's sentinel for a certificate that cannot be
// revoked.
const disabledOutpoint = "0000000000000000000000000000000000000000000000000000000000000000.0"

var (
	// ErrCertificate is what every refusal of a handle certificate's own
	// content wraps: checks 1 to 4 of BRC-169 section 4.1 (the signature,
	// the certifier, the handle and domain, the subject) and the shape of
	// the certificate. The error says which rule failed.
	ErrCertificate = errors.New("handle certificate refused")
	// ErrCertificateRevoked is a handle certificate whose revocation
	// outpoint a revocation source shows spent (BRC-169 section 4.2): the
	// handle was released, reassigned or revoked. The error wraps
	// ErrRevoked too.
	ErrCertificateRevoked = errors.New("handle certificate revoked: its revocation outpoint is spent")
)

// CheckHandleCertificate verifies h.Certificate, the BRC-52 handle
// certificate a resolution answer carried, under BRC-169 section 4.1,
// against m, the manifest of h's domain:
//
//  1. the certificate's signature verifies under BRC-52 (the certifier's
//     key derived under [2, "certificate signature"], key id
//     "<type> <serialNumber>", counterparty anyone), and is strict low-S
//     DER;
//  2. the certifier is m's metanet.trust.publicKey;
//  3. fields.handle and fields.domain, normalized, are h's handle and
//     domain;
//  4. the subject is h.IdentityKey;
//  5. the revocation outpoint is unspent, by spends (section 4.2).
//
// The type must be HandleCertificateType, and BRC-52's disabled
// revocation outpoint is refused, since a handle certificate is retired by
// spending its outpoint (section 4.3). A failure of any of 1 to 4, or of the
// certificate's shape, is an error wrapping ErrCertificate, checked before
// any source is asked.
//
// BRC-169 does not say how a resolution answer served without
// authentication reveals the two fields to the reader. Two forms are read,
// and nothing else: the certificate object carries a keyring for the
// verifier "anyone" (BRC-52's verifier keyring, so any reader decrypts
// it), and the fields are decrypted through it; or it carries no keyring,
// and each field value is read as the base64 of its plaintext, the form of
// BRC-169's worked example (Appendix A.3). The signature covers the field
// text as served either way, so a production certificate whose fields are
// encrypted, served without such a keyring, fails rule 3.
//
// BRC-52's JSON form writes revocationOutpoint as "txid.vout"; BRC-169's
// example writes it as an object {"txid", "vout"}. Both are read. A
// duplicate member name anywhere in the certificate is refused, as BRC-52
// requires; members this package does not know are ignored, as BRC-169
// section 5.1 requires.
//
// Revocation is detectable only through a source that indexes spends
// (section 4.2), so spends are those sources, each a SpendSource
// (nodeapi.WoC, nodeapi.Asset, or an index of the caller's). They are
// asked in order, and all of them are asked: the certificate is revoked
// when any names a spender (ErrCertificateRevoked, wrapping ErrRevoked),
// and verified when none does and at least one shows the outpoint
// unspent. When no source is given, or none could answer, the error wraps
// ErrCertificateUnverified: checks 1 to 4 passed, and the certificate MUST
// NOT be treated as valid for a value-moving action (section 4.2 rule 6),
// though a caller may render existing content marked unverified.
//
// On success it sets h.CertificateChecked, h.CertificateCheckedAt (the
// time of the revocation check, which section 4.2 rule 3 requires be kept
// with a cached result) and h.RevocationOutpoint, and returns nil. On any
// error it clears h.CertificateChecked.
func CheckHandleCertificate(ctx context.Context, h *Handle, m *Manifest, spends ...SpendSource) error {
	if h == nil {
		return fmt.Errorf("%w: no handle", ErrCertificate)
	}
	h.CertificateChecked = false
	h.CertificateCheckedAt = time.Time{}
	op, err := checkCertificate(ctx, h, m)
	if err != nil {
		return fmt.Errorf("resolve: %s: %w", h.Acct, err)
	}
	h.RevocationOutpoint = op.String()
	answered := false
	var why []string
	for _, s := range spends {
		if s == nil {
			continue
		}
		by, err := s.Spender(ctx, op.Txid.String(), op.Index)
		switch {
		case err != nil:
			why = append(why, err.Error())
		case by != "":
			return fmt.Errorf("resolve: %s: %w (by %s): %w", h.Acct, ErrCertificateRevoked, by, ErrRevoked)
		default:
			answered = true
		}
	}
	if !answered {
		if len(why) == 0 {
			return fmt.Errorf("resolve: %s: %w: no revocation source was given", h.Acct, ErrCertificateUnverified)
		}
		return fmt.Errorf("resolve: %s: %w: no revocation source could answer for %s: %s", h.Acct, ErrCertificateUnverified, op, strings.Join(why, "; "))
	}
	h.CertificateChecked = true
	h.CertificateCheckedAt = time.Now()
	return nil
}

// VerifyHandleCertificate is CheckHandleCertificate with no revocation
// source: checks 1 to 4 of BRC-169 section 4.1. It never sets
// CertificateChecked, since revocation is not checked: a certificate that
// passes returns an error wrapping ErrCertificateUnverified, and one that
// fails returns an error wrapping ErrCertificate.
func VerifyHandleCertificate(h *Handle, m *Manifest) error {
	return CheckHandleCertificate(context.Background(), h, m)
}

// CertificateFresh reports whether h's certificate was verified, revocation
// included, no longer than bound before now: what section 4.2 rule 4 asks
// before a value-moving action, with bound RevocationFreshness by default.
// The bound is capped at the resolution's TTL (rule 4: it must not exceed
// it), and a TTL of 0 allows no cached result at all.
func (h *Handle) CertificateFresh(now time.Time, bound time.Duration) bool {
	if h == nil || !h.CertificateChecked || h.CertificateCheckedAt.IsZero() {
		return false
	}
	if ttl := time.Duration(h.TTL) * time.Second; h.TTL >= 0 && ttl < bound {
		bound = ttl
	}
	age := now.Sub(h.CertificateCheckedAt)
	return age >= 0 && age <= bound
}

// wireCertificate is a handle certificate as a resolution answer carries
// it: BRC-52's core certificate JSON, with an optional verifier keyring.
type wireCertificate struct {
	Type               string            `json:"type"`
	SerialNumber       string            `json:"serialNumber"`
	Subject            string            `json:"subject"`
	Certifier          string            `json:"certifier"`
	RevocationOutpoint json.RawMessage   `json:"revocationOutpoint"`
	Fields             map[string]string `json:"fields"`
	Signature          string            `json:"signature"`
	Keyring            map[string]string `json:"keyring"`
}

// checkCertificate is checks 1 to 4 and the certificate's shape. It
// returns the revocation outpoint.
func checkCertificate(ctx context.Context, h *Handle, m *Manifest) (*transaction.Outpoint, error) {
	raw := bytes.TrimSpace(h.Certificate)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, fmt.Errorf("%w: the answer carries no certificate", ErrCertificate)
	}
	if err := noDuplicates(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCertificate, err)
	}
	var w wireCertificate
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: not the expected JSON: %v", ErrCertificate, err)
	}
	if w.Type != HandleCertificateType {
		return nil, fmt.Errorf("%w: type %q is not the handle certificate type %s", ErrCertificate, w.Type, HandleCertificateType)
	}
	if b, err := base64.StdEncoding.Strict().DecodeString(w.SerialNumber); err != nil || len(b) != 32 {
		return nil, fmt.Errorf("%w: serialNumber %q is not 32 bytes of base64", ErrCertificate, w.SerialNumber)
	}
	subject, err := certKey("subject", w.Subject)
	if err != nil {
		return nil, err
	}
	certifier, err := certKey("certifier", w.Certifier)
	if err != nil {
		return nil, err
	}
	op, err := parseOutpoint(w.RevocationOutpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: revocationOutpoint: %v", ErrCertificate, err)
	}
	if op.String() == disabledOutpoint {
		return nil, fmt.Errorf("%w: revocationOutpoint is BRC-52's disabled sentinel, so the binding could never be revoked", ErrCertificate)
	}
	sig, err := hex.DecodeString(w.Signature)
	if err != nil || !strictDER(sig) {
		return nil, fmt.Errorf("%w: signature is not strict low-S DER, hex", ErrCertificate)
	}
	if len(w.Fields) == 0 {
		return nil, fmt.Errorf("%w: no fields", ErrCertificate)
	}
	fields := make(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64, len(w.Fields))
	for n, v := range w.Fields {
		if n == "" || len(n) > 50 || !utf8.ValidString(n) {
			return nil, fmt.Errorf("%w: field name %q is not 1 to 50 bytes of UTF-8", ErrCertificate, n)
		}
		if _, err := base64.StdEncoding.Strict().DecodeString(v); err != nil {
			return nil, fmt.Errorf("%w: field %q is not base64", ErrCertificate, n)
		}
		fields[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(v)
	}
	cert := certificates.NewCertificate(wallet.StringBase64(w.Type), wallet.StringBase64(w.SerialNumber),
		*subject, *certifier, op, fields, sig)

	// 1. The signature.
	if err := cert.Verify(ctx); err != nil {
		return nil, fmt.Errorf("%w: the signature does not verify under the certifier's key: %v", ErrCertificate, err)
	}
	// 2. The certifier is the domain's.
	if m == nil || m.Trust == nil || m.Trust.PublicKey == "" {
		return nil, fmt.Errorf("%w: the domain's manifest has no metanet.trust.publicKey to check the certifier against", ErrCertificate)
	}
	if !strings.EqualFold(m.Trust.PublicKey, w.Certifier) {
		return nil, fmt.Errorf("%w: certifier %s is not the domain's metanet.trust.publicKey %s", ErrCertificate, w.Certifier, m.Trust.PublicKey)
	}
	// 4. The subject is the key the endpoint answered (before 3, which may
	// decrypt under the subject).
	if !bytes.Equal(subject.Compressed(), h.IdentityKey[:]) {
		return nil, fmt.Errorf("%w: subject %s is not the identityKey the endpoint answered, %x", ErrCertificate, w.Subject, h.IdentityKey)
	}
	// 3. The handle and domain.
	plain, err := revealed(ctx, cert, w.Keyring)
	if err != nil {
		return nil, err
	}
	got, err := ParseAcct(plain["handle"] + "@" + plain["domain"])
	if err != nil || got.Tag != "" {
		return nil, fmt.Errorf("%w: fields.handle %q and fields.domain %q are not a handle and a domain", ErrCertificate, plain["handle"], plain["domain"])
	}
	if got.Handle != h.Handle || got.Domain != h.Domain {
		return nil, fmt.Errorf("%w: the certificate binds %s, not %s", ErrCertificate, got, h.Acct)
	}
	return op, nil
}

// revealed is the plaintext of fields.handle and fields.domain: through a
// keyring for the verifier anyone when there is one, else each value read
// as the base64 of its plaintext.
func revealed(ctx context.Context, cert *certificates.Certificate, keyring map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if len(keyring) == 0 {
		for _, n := range []string{"handle", "domain"} {
			v, ok := cert.Fields[wallet.CertificateFieldNameUnder50Bytes(n)]
			if !ok {
				return nil, fmt.Errorf("%w: no field %q", ErrCertificate, n)
			}
			b, _ := base64.StdEncoding.DecodeString(string(v)) // checked when read
			if !utf8.Valid(b) {
				return nil, fmt.Errorf("%w: field %q is not UTF-8 text and no keyring reveals it", ErrCertificate, n)
			}
			out[n] = string(b)
		}
		return out, nil
	}
	ring := make(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64, 2)
	for _, n := range []string{"handle", "domain"} {
		k, ok := keyring[n]
		if !ok {
			return nil, fmt.Errorf("%w: the keyring does not reveal %q", ErrCertificate, n)
		}
		if _, ok := cert.Fields[wallet.CertificateFieldNameUnder50Bytes(n)]; !ok {
			return nil, fmt.Errorf("%w: no field %q", ErrCertificate, n)
		}
		ring[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(k)
	}
	anyone, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypeAnyone})
	if err != nil {
		return nil, err
	}
	subject := cert.Subject
	for n, k := range ring {
		// BRC-52's verifier keyring: the field's revelation key, encrypted
		// by the subject to the verifier under [2, "certificate field
		// encryption"], key id "<serialNumber> <field>"; then the field,
		// under that key.
		ek, err := base64.StdEncoding.Strict().DecodeString(string(k))
		if err != nil {
			return nil, fmt.Errorf("%w: keyring entry %q is not base64", ErrCertificate, n)
		}
		proto, keyID := certificates.GetCertificateEncryptionDetails(string(n), string(cert.SerialNumber))
		key, err := anyone.Decrypt(ctx, wallet.DecryptArgs{
			EncryptionArgs: wallet.EncryptionArgs{ProtocolID: proto, KeyID: keyID,
				Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: &subject}},
			Ciphertext: ek,
		}, "")
		if err != nil {
			return nil, fmt.Errorf("%w: the keyring does not open %q for the verifier anyone", ErrCertificate, n)
		}
		ct, _ := base64.StdEncoding.DecodeString(string(cert.Fields[n])) // checked when read
		pt, err := ec.NewSymmetricKey(key.Plaintext).Decrypt(ct)
		if err != nil || !utf8.Valid(pt) {
			return nil, fmt.Errorf("%w: the revealed key does not open field %q", ErrCertificate, n)
		}
		out[string(n)] = string(pt)
	}
	return out, nil
}

// strictDER is a DER signature in its one canonical, low-S encoding.
func strictDER(sig []byte) bool {
	if len(sig) < 8 || len(sig) > 72 {
		return false
	}
	s, err := ec.ParseDERSignature(sig)
	return err == nil && bytes.Equal(s.Serialize(), sig)
}

// certKey reads a 33-byte compressed key, hex.
func certKey(what, s string) (*ec.PublicKey, error) {
	k, err := parseIdentityKey(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCertificate, what, err)
	}
	pk, err := ec.PublicKeyFromBytes(k[:])
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a point on the curve", ErrCertificate, what)
	}
	return pk, nil
}

// parseOutpoint reads BRC-52's "txid.vout" or BRC-169's {"txid", "vout"}.
func parseOutpoint(raw json.RawMessage) (*transaction.Outpoint, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		txid, vout, ok := strings.Cut(s, ".")
		if !ok {
			return nil, fmt.Errorf("%q is not txid.vout", s)
		}
		n, err := strconv.ParseUint(vout, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != vout {
			return nil, fmt.Errorf("%q: vout is not a decimal 32-bit index", s)
		}
		return outpoint(txid, uint32(n))
	}
	var o struct {
		Txid *string `json:"txid"`
		Vout *uint32 `json:"vout"`
	}
	if err := json.Unmarshal(raw, &o); err != nil || o.Txid == nil || o.Vout == nil {
		return nil, errors.New(`neither "txid.vout" nor {"txid", "vout"}`)
	}
	return outpoint(*o.Txid, *o.Vout)
}

func outpoint(txid string, vout uint32) (*transaction.Outpoint, error) {
	if len(txid) != 64 {
		return nil, fmt.Errorf("txid %q is not 64 hex characters", txid)
	}
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, fmt.Errorf("txid %q: %v", txid, err)
	}
	return &transaction.Outpoint{Txid: *h, Index: vout}, nil
}

// noDuplicates refuses a JSON document in which any object repeats a member
// name: encoding/json keeps the last of two, and BRC-52 forbids accepting
// duplicate field names.
func noDuplicates(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				name, _ := k.(string)
				if seen[name] {
					return fmt.Errorf("member %q appears twice", name)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = d.Token() // the closing delimiter
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after the certificate")
	}
	return nil
}
