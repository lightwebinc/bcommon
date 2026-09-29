package store

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// A manifest is the body of a store's head record: the ordered list of the
// store's members. It is what turns a set of carriers back into one thing,
// and it is the store's head rather than one of its members, so the store's
// root is over what it lists and not over it.
//
// The list is complete, so a reader recomputes the store's root from it and
// compares that to the root in the record. No inclusion path is carried:
// with the whole list in hand a path proves less than the recomputation
// does, and a path per member would cost a store of a thousand members
// about forty kilobytes to say what the list already says. The path
// arithmetic stays in package commit for the case it is actually for, a
// member handed to someone out of band without the list.

// MemberKey is the body key a manifest's member list lives under. A
// manifest's body is the whole of its record, so this cannot collide with a
// publisher's own field the way a reserved name in a profile would.
const MemberKey = "members"

var (
	// ErrNotManifest is a body that is not a member list.
	ErrNotManifest = errors.New("store: not a manifest body")
	// ErrMemberCount is a manifest with no members or more than MaxMembers.
	ErrMemberCount = errors.New("store: manifest member count out of range")
)

// Member is one entry of a manifest.
type Member struct {
	// C is the member sub-record's commitment, in hash byte order, the
	// order every other commitment in a record is carried in.
	C [32]byte
	// Name labels the part. Empty is allowed and ordinary: a chunked
	// document's parts are identified by their position.
	Name string
	// Size is the member's content length in bytes, so a reader can size a
	// buffer and refuse a store larger than it wants before fetching it.
	Size uint64
	// Type is the member's media type: "text/plain" for a document part, a
	// locator type for a member that names content held elsewhere.
	Type string
}

// MemberOverhead is the encoded cost of one member entry excluding its
// name and type, measured rather than assumed: the fixed-width map, its
// four keys, the 32-byte commitment and a size. It is what lets a caller
// work out how many members a manifest can hold before it mints any of
// them, since MaxMembers alone is reachable only for short names.
//
// It is a variable because it is computed from the encoder when the package
// loads, so it cannot disagree with what Body encodes. Callers read it and
// must not assign to it. Every package in the process reads the same value,
// and a changed one would have each of them size manifests by a cost the
// encoder does not have: more members than Body accepts, or fewer than fit.
var MemberOverhead = func() int {
	one, err := (&Manifest{Members: []Member{{}}}).list()
	if err != nil {
		panic(err)
	}
	empty, err := cbor.Encode(cbor.Map{{Key: MemberKey, Val: []cbor.Value{}}})
	if err != nil {
		panic(err)
	}
	b, err := cbor.Encode(one)
	if err != nil {
		panic(err)
	}
	return len(b) - len(empty)
}()

// Manifest is a store's member list.
type Manifest struct {
	Members []Member
}

// Leaves is the member commitments in order, which is what the store's root
// is computed over (see Root).
func (m *Manifest) Leaves() [][32]byte {
	out := make([][32]byte, len(m.Members))
	for i, mem := range m.Members {
		out[i] = mem.C
	}
	return out
}

// Body encodes the manifest as a record body, refusing one whose encoding
// is over bound, the body bound of the record that will carry it. The
// bound is the application's, because it follows what that application's
// records may carry. The count bound alone is not enough: a member costs more
// than bound/MaxMembers once its name and type are counted, so a caller
// that checked only the count would mint every member and then fail on the
// record that was supposed to name them.
func (m *Manifest) Body(bound int) (cbor.Map, error) {
	body, err := m.list()
	if err != nil {
		return nil, err
	}
	enc, err := cbor.Encode(body)
	if err != nil {
		return nil, err
	}
	if len(enc) > bound {
		return nil, fmt.Errorf("%w: %d members encode to %d bytes, over the %d bound", ErrMemberCount, len(m.Members), len(enc), bound)
	}
	return body, nil
}

// list is the body without the size bound, which is what MemberOverhead
// measures.
func (m *Manifest) list() (cbor.Map, error) {
	if len(m.Members) == 0 || len(m.Members) > MaxMembers {
		return nil, fmt.Errorf("%w: %d", ErrMemberCount, len(m.Members))
	}
	arr := make([]cbor.Value, 0, len(m.Members))
	for i, mem := range m.Members {
		if len(mem.Name) > MaxRefName {
			return nil, fmt.Errorf("%w: member %d name", ErrField, i)
		}
		if len(mem.Type) > MaxRefName {
			return nil, fmt.Errorf("%w: member %d type", ErrField, i)
		}
		// Fixed width, every member the same shape. An optional key would
		// make two encodings of one member, and a member's bytes are what
		// the store's root is taken over.
		arr = append(arr, cbor.Map{
			{Key: "c", Val: mem.C[:]},
			{Key: "name", Val: mem.Name},
			{Key: "size", Val: mem.Size},
			{Key: "type", Val: mem.Type},
		})
	}
	return cbor.Map{{Key: MemberKey, Val: arr}}, nil
}

// ParseManifest reads a manifest out of a record body. It is deliberately
// strict: a body with anything else in it is not a manifest, because a
// manifest's body is the whole of what that record is for.
func ParseManifest(body cbor.Map) (*Manifest, error) {
	if len(body) != 1 {
		return nil, fmt.Errorf("%w: body holds %d field(s)", ErrNotManifest, len(body))
	}
	v, ok := body.Get(MemberKey)
	if !ok {
		return nil, fmt.Errorf("%w: no %q", ErrNotManifest, MemberKey)
	}
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not an array", ErrNotManifest, MemberKey)
	}
	if len(arr) == 0 || len(arr) > MaxMembers {
		return nil, fmt.Errorf("%w: %d", ErrMemberCount, len(arr))
	}
	m := &Manifest{Members: make([]Member, 0, len(arr))}
	for i, e := range arr {
		em, ok := e.(cbor.Map)
		if !ok || len(em) != 4 {
			return nil, fmt.Errorf("%w: member %d", ErrField, i)
		}
		var mem Member
		c, _ := em.Get("c")
		if err := fixed(c, mem.C[:], "member c"); err != nil {
			return nil, err
		}
		name, _ := em.Get("name")
		if mem.Name, ok = name.(string); !ok || len(mem.Name) > MaxRefName {
			return nil, fmt.Errorf("%w: member %d name", ErrField, i)
		}
		size, _ := em.Get("size")
		var err error
		if mem.Size, err = unsigned(size, "member size"); err != nil {
			return nil, err
		}
		typ, _ := em.Get("type")
		if mem.Type, ok = typ.(string); !ok || len(mem.Type) > MaxRefName {
			return nil, fmt.Errorf("%w: member %d type", ErrField, i)
		}
		m.Members = append(m.Members, mem)
	}
	return m, nil
}
