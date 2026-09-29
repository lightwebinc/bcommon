// Package store is how a record commits to a set of other records and how a
// reader opens that set: the refs entry that names a store and commits to its
// root, the manifest that lists a store's members, and the rule that computes
// the root from the entry and the members' commitments.
//
// The entry and the manifest are the same for every application that stores
// records this way, so they live here. Where an entry sits in a record (which
// key, which kinds may carry one) and how large a manifest body may be belong
// to the application: the entry codec works on the array value alone, and
// Body takes the bound.
//
// A refusal that wraps a sentinel starts with that sentinel's text, so an
// application that words its own refusals can swap the prefix for its own
// and keep the rest.
package store

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// MaxRefMembers bounds one refs entry, so an entry cannot carry unbounded
// material under names this version does not define. Four are defined; the
// rest is room for the format to grow without orphaning a reader.
const MaxRefMembers = 8

// refMembers are the entry members this version defines.
var refMembers = map[string]struct{}{"name": {}, "root": {}, "count": {}, "head": {}}

// MaxRefs bounds how many stores one record may commit to. Without it a
// record is an instruction to a reader to make an unbounded number of
// requests, because a reader reads every store a record links. Generous
// for a directory entry and small enough that the work a record can ask
// for is bounded by the record.
const MaxRefs = 64

// MaxMembers bounds a manifest's member list, so the work one manifest can
// ask of a reader, a fetch and a check per member, is bounded by the
// manifest. A store that needs more members than this should hold locators
// rather than content.
const MaxMembers = 1024

// MaxRefName bounds a store name, and a manifest member's name and type.
const MaxRefName = 64

var (
	// ErrField is an entry or a member of the wrong type, width or length.
	ErrField = errors.New("store: field has the wrong shape")
	// ErrDupRef is two entries naming one store.
	ErrDupRef = errors.New("store: two refs entries name the same store")
)

// Ref names a sub-store and commits to it: an RFC 6962 root over the
// commitments of that store's sub-records, and how many there are.
//
// Head, when present, is the commitment of the store's head member, in hash
// byte order like every other commitment in the record. It is what lets a
// reader find the store at all: a root cannot be inverted and the host holds
// no root-to-member index. A one-member store's root is exactly LeafHash of
// its member, so with Count 1 the reader proves membership by hashing Head
// once. Absent, the store is committed to but not publicly linked: whoever
// is meant to read it is handed a member some other way.
type Ref struct {
	Name  string
	Root  [32]byte
	Count uint64
	Head  *[32]byte
	// Unknown carries every member of this entry that this version does not
	// define, verbatim, so a re-encode is faithful and an older reader is
	// not orphaned by a newer store.
	//
	// It is preserved and NOT ignored. A member a reader does not know may
	// change how the store's membership is computed, so a reader that
	// silently skipped it could accept a membership proof that is wrong.
	// The rule is therefore: an entry carrying an unknown member makes THAT
	// STORE unreadable to this reader, and changes nothing else about the
	// record. Refusing the whole record instead, which is what a strict
	// entry width does, means one new store field costs every existing
	// reader the rest of the record as well.
	Unknown cbor.Map
}

// Extended reports whether this entry carries a member this version does not
// define, which makes the store unreadable here.
func (r *Ref) Extended() bool { return len(r.Unknown) > 0 }

// EncodeRefs writes a record's refs entries as the array value the record
// carries, checking every entry's shape.
func EncodeRefs(in []Ref) ([]cbor.Value, error) {
	if len(in) > MaxRefs {
		return nil, fmt.Errorf("%w: %d refs", ErrField, len(in))
	}
	refs := make([]cbor.Value, 0, len(in))
	seenRef := make(map[string]struct{}, len(in))
	for _, ref := range in {
		if ref.Name == "" || len(ref.Name) > MaxRefName {
			return nil, fmt.Errorf("%w: ref name", ErrField)
		}
		// A store name has to identify one root. The CBOR map rules refuse a
		// duplicate KEY, but refs is an array, so nothing upstream catches a
		// second entry with the same name, and two roots for one name means
		// "fetch that store" has two answers and a reader takes whichever it
		// happens to iterate first.
		if _, dup := seenRef[ref.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDupRef, ref.Name)
		}
		seenRef[ref.Name] = struct{}{}
		entry := cbor.Map{
			{Key: "name", Val: ref.Name},
			{Key: "root", Val: ref.Root[:]},
			{Key: "count", Val: ref.Count},
		}
		if ref.Head != nil {
			entry = append(entry, cbor.Pair{Key: "head", Val: ref.Head[:]})
		}
		for _, p := range ref.Unknown {
			k, ok := p.Key.(string)
			if !ok {
				return nil, fmt.Errorf("%w: ref member key %T", ErrField, p.Key)
			}
			if _, known := refMembers[k]; known {
				return nil, fmt.Errorf("%w: %q is a defined ref member", ErrField, k)
			}
			entry = append(entry, p)
		}
		if len(entry) > MaxRefMembers {
			return nil, fmt.Errorf("%w: ref %q has %d members", ErrField, ref.Name, len(entry))
		}
		refs = append(refs, entry)
	}
	return refs, nil
}

// DecodeRefs reads a record's refs value, checking every entry's shape and
// preserving the members this version does not define.
func DecodeRefs(v cbor.Value) ([]Ref, error) {
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("%w: refs", ErrField)
	}
	if len(arr) > MaxRefs {
		return nil, fmt.Errorf("%w: %d refs", ErrField, len(arr))
	}
	refs := make([]Ref, 0, len(arr))
	refNames := make(map[string]struct{}, len(arr))
	for _, e := range arr {
		em, ok := e.(cbor.Map)
		if !ok || len(em) < 3 || len(em) > MaxRefMembers {
			return nil, fmt.Errorf("%w: ref", ErrField)
		}
		var ref Ref
		name, _ := em.Get("name")
		root, _ := em.Get("root")
		count, _ := em.Get("count")
		if head, has := em.Get("head"); has {
			var h [32]byte
			if err := fixed(head, h[:], "ref head"); err != nil {
				return nil, err
			}
			ref.Head = &h
		}
		// Members this version does not define are kept, not refused:
		// refusing would make one new store field cost every existing
		// reader the whole record. They are not ignored either; see
		// Ref.Unknown.
		for _, p := range em {
			k, ok := p.Key.(string)
			if !ok {
				return nil, fmt.Errorf("%w: ref member key %T", ErrField, p.Key)
			}
			if _, known := refMembers[k]; !known {
				ref.Unknown = append(ref.Unknown, p)
			}
		}
		if ref.Name, ok = name.(string); !ok || ref.Name == "" || len(ref.Name) > MaxRefName {
			return nil, fmt.Errorf("%w: ref name", ErrField)
		}
		if err := fixed(root, ref.Root[:], "ref root"); err != nil {
			return nil, err
		}
		var err error
		if ref.Count, err = unsigned(count, "ref count"); err != nil {
			return nil, err
		}
		if _, dup := refNames[ref.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDupRef, ref.Name)
		}
		refNames[ref.Name] = struct{}{}
		refs = append(refs, ref)
	}
	return refs, nil
}

func fixed(v cbor.Value, dst []byte, name string) error {
	b, ok := v.([]byte)
	if !ok || len(b) != len(dst) {
		return fmt.Errorf("%w: %s wants %d bytes", ErrField, name, len(dst))
	}
	copy(dst, b)
	return nil
}

func unsigned(v cbor.Value, name string) (uint64, error) {
	n, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("%w: %s wants an unsigned integer", ErrField, name)
	}
	return n, nil
}
