package pushdrop

import (
	"bytes"
	"errors"
	"fmt"

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
func DecodeTagged(s *script.Script, tag []byte, nfields int) (*Tagged, error) {
	if nfields < 1 {
		return nil, fmt.Errorf("pushdrop: %d fields asked for, but the tag is one", nfields)
	}
	if s == nil {
		return nil, fmt.Errorf("%w: nil script", ErrNotTagged)
	}
	d := sdkpushdrop.Decode(s)
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
	return &Tagged{Fields: d.Fields[:nfields], LockingKey: d.LockingPublicKey, Signature: d.Fields[nfields]}, nil
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
