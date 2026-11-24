package pushdrop

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	hash "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
)

// ErrNotTagged reports a script that is not the tagged PushDrop DecodeTagged
// was asked for.
var ErrNotTagged = errors.New("pushdrop: not a tagged PushDrop")

// Tagged is a decoded signed PushDrop whose first field is a tag.
type Tagged struct {
	// Fields are the fields Lock was given, tag first. The signature is not
	// one of them.
	Fields [][]byte
	// LockingKey is the key the output is locked to.
	LockingKey *ec.PublicKey
	// Signature is Lock's DER signature over sha256 of Fields concatenated.
	Signature []byte
}

// DecodeTagged parses what Lock writes with sign set: nfields fields, tag
// included, the first equal to tag, then the signature. Only the lock-before
// layout decodes, which is the layout Lock writes; a lock-after script from
// some other producer is refused rather than guessed at. What the fields
// after the tag must hold is the caller's to check.
//
// A script of the caller's tag that is not the one encoding Lock writes
// (CheckCanonical) is refused with ErrNonCanonical, not ErrNotTagged: it is
// the caller's output written another way, and a reader skipping it as
// somebody else's would miss that.
func DecodeTagged(s *script.Script, tag []byte, nfields int) (*Tagged, error) {
	if nfields < 1 {
		return nil, fmt.Errorf("pushdrop: %d fields asked for, but the tag is one", nfields)
	}
	if s == nil {
		return nil, fmt.Errorf("%w: nil script", ErrNotTagged)
	}
	d := Loose(s)
	if d == nil || d.LockingPublicKey == nil {
		return nil, fmt.Errorf("%w: not a lock-before PushDrop", ErrNotTagged)
	}
	// Lock appends the signature as one more field than it was given. The
	// count is checked before the tag because a script the SDK decodes with
	// no fields at all (a P2PK is one) has no tag to compare.
	if len(d.Fields) != nfields+1 {
		return nil, fmt.Errorf("%w: %d fields, want %d", ErrNotTagged, len(d.Fields), nfields+1)
	}
	if !bytes.Equal(d.Fields[0], tag) {
		return nil, fmt.Errorf("%w: tag %x", ErrNotTagged, d.Fields[0])
	}
	// Only once the script is known to be the caller's: a foreign script is
	// not tagged, while one of the caller's written another way is refused.
	if err := CheckCanonical(s); err != nil {
		return nil, err
	}
	return &Tagged{Fields: d.Fields[:nfields], LockingKey: d.LockingPublicKey, Signature: d.Fields[nfields]}, nil
}

// Loose is what go-sdk's decoder reads of s, for a reader that must learn
// whose a script is before CheckCanonical decides whether it is well written:
// a script of the reader's own written another way is to be refused, not
// skipped as somebody else's. It is never by itself a reason to take a
// script; follow it with CheckCanonical on s as written.
//
// go-sdk from v1.7.7 does not parse a compressed key whose x is at or above
// the field prime, so its decoder reads nothing of a script written with one.
// Loose then reads the same script with that key reduced to the canonical
// encoding of its point, as go-sdk through v1.7.6 did, so the answer does not
// depend on the SDK an application builds with. The key it reports in that
// case is the reduced one; CheckCanonical refuses the script.
func Loose(s *script.Script) *sdkpushdrop.PushDropData {
	if s == nil {
		return nil
	}
	d := sdkpushdrop.Decode(s)
	if d == nil || d.LockingPublicKey == nil {
		if alt := withAliasReduced(s); alt != nil {
			d = sdkpushdrop.Decode(alt)
		}
	}
	return d
}

// withAliasReduced is s with its leading key push rewritten to the canonical
// encoding of the same point, when that push is a compressed key whose x is
// at or above the field prime (02 || p+1 for x = 1), and nil otherwise.
func withAliasReduced(s *script.Script) *script.Script {
	b := []byte(*s)
	var off int
	switch {
	case len(b) > 0 && b[0] == 33:
		off = 1
	case len(b) > 1 && b[0] == script.OpPUSHDATA1 && b[1] == 33:
		off = 2
	case len(b) > 2 && b[0] == script.OpPUSHDATA2 && b[1] == 33 && b[2] == 0:
		off = 3
	case len(b) > 4 && b[0] == script.OpPUSHDATA4 && b[1] == 33 && b[2] == 0 && b[3] == 0 && b[4] == 0:
		off = 5
	default:
		return nil
	}
	if len(b) < off+33 || (b[off] != 0x02 && b[off] != 0x03) {
		return nil
	}
	p := ec.S256().Params().P
	x := new(big.Int).SetBytes(b[off+1 : off+33])
	if x.Cmp(p) < 0 {
		return nil
	}
	out := bytes.Clone(b)
	x.Sub(x, p).FillBytes(out[off+1 : off+33])
	alt := script.Script(out)
	return &alt
}

// Signed returns the bytes Lock signed: the fields concatenated.
func (t *Tagged) Signed() []byte {
	n := 0
	for _, f := range t.Fields {
		n += len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range t.Fields {
		out = append(out, f...)
	}
	return out
}

// VerifySignature checks the embedded signature under the locking key. The
// wallet signs sha256 of the concatenated fields, so that is what is verified.
func (t *Tagged) VerifySignature() bool {
	sig, err := ec.ParseDERSignature(t.Signature)
	if err != nil {
		return false
	}
	return sig.Verify(hash.Sha256(t.Signed()), t.LockingKey)
}
