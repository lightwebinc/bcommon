package store

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/commit"
)

// ErrUnsupported is a store whose entry carries a member this version does
// not define. The record is fine; this one store cannot be read here.
var ErrUnsupported = errors.New("unsupported store")

// ErrNoHead is a ref that commits to a store without naming a member of it.
// Not a fault: the publisher may intend the store to be found some other
// way, and a reader has nothing to ask a host for.
var ErrNoHead = errors.New("no head")

// Root is the root a ref commits to. A store of one member is its own head
// and its root is the leaf hash of it, so a reader proves membership with one
// hash and no manifest exists to list it; members is not read then. Any other
// store's root is over members, the member commitments in manifest order
// (Manifest.Leaves), and never over the manifest, which is the head.
//
// It is one function because the publisher that writes a root and the reader
// that recomputes it must agree on it exactly; two copies of the rule are
// two chances for a store to be written under one and read under the other.
// Whether members holds Count commitments is the caller's to check, since
// the caller is the one that knows what a mismatch means for it.
func Root(ref Ref, members [][32]byte) [32]byte {
	if ref.Count == 1 && ref.Head != nil {
		return commit.LeafHash(*ref.Head)
	}
	return commit.Root(members)
}

// Head is the commitment a reader asks the host for to open a store: the
// one member when the store has one, the manifest when it has more. It
// refuses a ref that names no head, which is a store committed to but not
// linked, and a count of zero, which commits to nothing.
func Head(ref Ref) ([32]byte, error) {
	// A member this reader does not define may change how membership is
	// computed, so reading the store anyway would risk accepting a proof
	// that is wrong. Refusing this store and nothing else is the safe
	// outcome: the rest of the record is unaffected.
	if ref.Extended() {
		names := make([]string, 0, len(ref.Unknown))
		for _, p := range ref.Unknown {
			if k, ok := p.Key.(string); ok {
				names = append(names, k)
			}
		}
		return [32]byte{}, fmt.Errorf("%w: store %q uses %v, which this reader does not implement", ErrUnsupported, ref.Name, names)
	}
	if ref.Head == nil {
		return [32]byte{}, fmt.Errorf("%w: store %q is committed to but not linked", ErrNoHead, ref.Name)
	}
	if ref.Count == 0 {
		return [32]byte{}, fmt.Errorf("store %q commits to no members", ref.Name)
	}
	return *ref.Head, nil
}
