// Package authz carries the record of one authorization decision: what a
// service decided about one subject, over which state, and its signature.
//
// The decision itself is the application's. borg's contract defines what a
// role and a binding mean and folds them into a subject's effective
// permissions; a service establishes the subject's membership, fetches the
// records and evaluates them. This package writes down what came of that, so
// that a decision can be checked afterwards by someone who was not there:
// the receipt names the organization token the decision rested on, the
// grants and policy records it used, and the answer, and it is signed by the
// service that gave the answer.
//
// A receipt is evidence, never authority. Holding one grants nothing: it
// says only that a service decided something at a time, against a state
// anyone can fetch and check for themselves.
package authz

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/record"
)

// Registry values, frozen once a receipt is published.
const (
	// ProtocolName is the BRC-43 protocol name of a decision signature, at
	// security level 1: the signature is one anyone verifies from the
	// signer's published identity key.
	ProtocolName = "authz decision"
	// KeyID is the key id that protocol derives under.
	KeyID = "receipt"
)

// MagicReceipt is key 0 of a receipt: "az", 'r', version 1.
var MagicReceipt = []byte{'a', 'z', 'r', 0x01}

// Derivation is the derivation a receipt's signature is made and checked
// under: counterparty anyone, so a verifier derives the key from the
// signer's identity key alone.
var Derivation = pushdrop.Derivation{
	Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: ProtocolName},
	KeyID:    KeyID,
}

// Receipt keys.
const (
	RKMagic      = 0
	RKBy         = 1
	RKOrg        = 2
	RKHall       = 3
	RKSubject    = 4
	RKPermission = 5
	RKAllow      = 6
	RKSeq        = 7
	RKToken      = 8
	RKGrants     = 9
	RKPolicies   = 10
	RKTime       = 11
	RKReason     = 12
	rkLast       = RKReason
)

// Bounds.
const (
	// MaxReceipt bounds an encoded receipt.
	MaxReceipt = 4096
	// MaxUsed bounds the commitments one receipt lists per kind.
	MaxUsed = 64
	// MaxPermission bounds the asked permission, borg's bound.
	MaxPermission = 256
	// MaxHall bounds the rendezvous name a receipt carries.
	MaxHall = 64
	// MaxReason bounds the label a denial carries.
	MaxReason = 64
	// MaxTime is the last second RFC 3339 can write.
	MaxTime = 253402300799
)

// The refusals.
var (
	ErrTooLarge  = record.ErrTooLarge
	ErrCBOR      = record.ErrCBOR
	ErrKeyType   = record.ErrKeyType
	ErrMagic     = record.ErrMagic
	ErrMissing   = record.ErrMissing
	ErrType      = record.ErrType
	ErrRange     = record.ErrRange
	ErrList      = record.ErrList
	ErrIdentity  = errors.New("authz: key is not a canonical compressed key")
	ErrSignature = errors.New("authz: signature does not verify under the signer's key")
)

// Receipt is one decision, as it is written down and signed.
type Receipt struct {
	// By is the identity key of the service that decided.
	By []byte // key 1
	// Org is the organization whose state the decision rested on.
	Org []byte // key 2
	// Hall is that organization's rendezvous.
	Hall string // key 3
	// Subject is the key the decision was about.
	Subject []byte // key 4
	// Permission is what was asked, in full and with no wildcard.
	Permission string // key 5
	// Allow is the answer.
	Allow bool // key 6
	// Seq and Token name the organization token the decision rested on: the
	// state a checker fetches to reach the same answer.
	Seq   uint64   // key 7
	Token [32]byte // key 8, txid in hash byte order
	// Grants and Policies are the commitments of the records the decision
	// used, ascending.
	Grants   [][32]byte // key 9
	Policies [][32]byte // key 10
	// Time is when the service decided, Unix seconds on its own clock.
	Time uint64 // key 11
	// Reason is a short label for a denial, empty for an allow.
	Reason string // key 12
	// Extra holds integer keys above the last this version defines.
	Extra cbor.Map
}

func checkKey(k []byte) error {
	if _, err := guard.ParsePubKey(k); err != nil {
		return ErrIdentity
	}
	return nil
}

// Validate applies every rule that needs only the receipt itself.
func (r *Receipt) Validate() error {
	if err := record.CheckExtra(r.Extra, rkLast, rkLast+1); err != nil {
		return err
	}
	for _, k := range [][]byte{r.By, r.Org, r.Subject} {
		if err := checkKey(k); err != nil {
			return err
		}
	}
	if len(r.Hall) < 1 || len(r.Hall) > MaxHall {
		return fmt.Errorf("%w: hall", ErrRange)
	}
	if len(r.Permission) < 1 || len(r.Permission) > MaxPermission {
		return fmt.Errorf("%w: permission", ErrRange)
	}
	if err := record.InRange(r.Seq, 0, 1<<53-1, "seq"); err != nil {
		return err
	}
	if len(r.Grants) > MaxUsed || record.Ascending32(r.Grants) != nil {
		return fmt.Errorf("%w: grants", ErrList)
	}
	if len(r.Policies) > MaxUsed || record.Ascending32(r.Policies) != nil {
		return fmt.Errorf("%w: policies", ErrList)
	}
	if err := record.InRange(r.Time, 1, MaxTime, "time"); err != nil {
		return err
	}
	if len(r.Reason) > MaxReason {
		return fmt.Errorf("%w: reason", ErrRange)
	}
	if r.Allow && r.Reason != "" {
		return fmt.Errorf("%w: an allow carries no reason", ErrRange)
	}
	return nil
}

// Encode writes the receipt in canonical CBOR, the bytes a signature covers.
func (r *Receipt) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(RKMagic), Val: MagicReceipt},
		{Key: uint64(RKBy), Val: r.By},
		{Key: uint64(RKOrg), Val: r.Org},
		{Key: uint64(RKHall), Val: r.Hall},
		{Key: uint64(RKSubject), Val: r.Subject},
		{Key: uint64(RKPermission), Val: r.Permission},
		{Key: uint64(RKAllow), Val: r.Allow},
		{Key: uint64(RKSeq), Val: r.Seq},
		{Key: uint64(RKToken), Val: append([]byte(nil), r.Token[:]...)},
		{Key: uint64(RKGrants), Val: record.Values32(r.Grants)},
		{Key: uint64(RKPolicies), Val: record.Values32(r.Policies)},
		{Key: uint64(RKTime), Val: r.Time},
		{Key: uint64(RKReason), Val: r.Reason},
	}
	return record.Encode(m, r.Extra, MaxReceipt)
}

// Decode parses a receipt in the refusal order.
func Decode(b []byte) (*Receipt, error) {
	f, err := record.Decode(b, MaxReceipt, rkLast, MagicReceipt)
	if err != nil {
		return nil, err
	}
	r := &Receipt{Extra: f.Extra}
	for _, p := range []struct {
		k   uint64
		dst *[]byte
	}{{RKBy, &r.By}, {RKOrg, &r.Org}, {RKSubject, &r.Subject}} {
		if *p.dst, err = f.BytesN(p.k, -1); err != nil {
			return nil, err
		}
	}
	if r.Hall, err = f.Text(RKHall); err != nil {
		return nil, err
	}
	if r.Permission, err = f.Text(RKPermission); err != nil {
		return nil, err
	}
	if r.Allow, err = f.Bool(RKAllow); err != nil {
		return nil, err
	}
	if r.Seq, err = f.Uint(RKSeq, 0, 1<<53-1); err != nil {
		return nil, err
	}
	t, err := f.BytesN(RKToken, 32)
	if err != nil {
		return nil, err
	}
	copy(r.Token[:], t)
	if r.Grants, err = f.List32(RKGrants, MaxUsed); err != nil {
		return nil, err
	}
	if r.Policies, err = f.List32(RKPolicies, MaxUsed); err != nil {
		return nil, err
	}
	if r.Time, err = f.Uint(RKTime, 1, MaxTime); err != nil {
		return nil, err
	}
	if r.Reason, err = f.Text(RKReason); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Signed is a receipt and the signature over it, the pair a service hands
// out and a checker reads.
type Signed struct {
	// Record is the receipt exactly as it was signed.
	Record []byte
	// Signature is strict low-S DER over SHA-256 of Record.
	Signature []byte
}

// Sign writes the receipt and signs it through the service's wallet, under
// the receipt derivation. The wallet's identity key MUST be the receipt's
// By, or nobody can check the signature.
func Sign(ctx context.Context, w wallet.Interface, originator string, r *Receipt) (*Signed, error) {
	if w == nil {
		return nil, errors.New("authz: nil wallet")
	}
	b, err := r.Encode()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(b)
	res, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   Derivation.Protocol,
			KeyID:        Derivation.KeyID,
			Counterparty: pushdrop.Anyone(),
		},
		HashToDirectlySign: digest[:],
	}, originator)
	if err != nil {
		return nil, fmt.Errorf("authz: sign receipt: %w", err)
	}
	return &Signed{Record: b, Signature: res.Signature.Serialize()}, nil
}

// Verify reads a signed receipt and checks its signature under the key the
// receipt names as its signer. It returns the decoded receipt only when the
// signature verifies, so a caller cannot read a receipt it has not checked.
func (s *Signed) Verify() (*Receipt, error) {
	r, err := Decode(s.Record)
	if err != nil {
		return nil, err
	}
	by, err := ec.ParsePubKey(r.By)
	if err != nil {
		return nil, ErrIdentity
	}
	key, err := Derivation.ExpectedLockingKey(by)
	if err != nil {
		return nil, err
	}
	if !chaintoken.VerifyField(key, s.Record, s.Signature) {
		return nil, ErrSignature
	}
	return r, nil
}
