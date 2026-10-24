package pushdrop

import (
	"fmt"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	crypto "github.com/bsv-blockchain/go-sdk/primitives/hash"
)

// invoice is the BRC-43 invoice number of a derivation that passes
// Validate, exactly as go-sdk's key deriver writes it:
// "<level>-<name trimmed and lower-cased>-<key id>".
func (d Derivation) invoice() string {
	return fmt.Sprintf("%d-%s-%s", d.Protocol.SecurityLevel, strings.ToLower(strings.TrimSpace(d.Protocol.Protocol)), d.KeyID)
}

// anyoneChild is go-sdk's identity.DeriveChild(anyone, invoice), the
// reader's BRC-42 derivation, without its first step. The anyone root key is
// the scalar 1, so the shared secret, identity times 1, is identity itself;
// the SDK computes it with a general scalar multiplication that costs as much
// as one with a random scalar (its endomorphism split of 1 yields two
// 128-bit halves), about two thirds of the whole derivation. The rest is the
// SDK's own: HMAC-SHA256 of the invoice under identity's compressed key, that
// times the generator, plus identity.
//
// ok is false for a key whose coordinates are not below the field prime or
// that is not on the curve: the SDK reduces the first and refuses the second,
// so the caller asks the SDK, which stays the authority on both.
func anyoneChild(identity *ec.PublicKey, invoice string) (_ *ec.PublicKey, ok bool) {
	if identity == nil || identity.X == nil || identity.Y == nil {
		return nil, false
	}
	curve := ec.S256()
	p := curve.Params().P
	if identity.X.Sign() < 0 || identity.Y.Sign() < 0 || identity.X.Cmp(p) >= 0 || identity.Y.Cmp(p) >= 0 ||
		!curve.IsOnCurve(identity.X, identity.Y) {
		return nil, false
	}
	h := crypto.Sha256HMAC([]byte(invoice), identity.Compressed())
	tx, ty := curve.ScalarBaseMult(h)
	x, y := curve.Add(tx, ty, identity.X, identity.Y)
	if x.Sign() == 0 && y.Sign() == 0 {
		return nil, false // the point at infinity: leave it to the SDK
	}
	return &ec.PublicKey{Curve: curve, X: x, Y: y}, true
}
