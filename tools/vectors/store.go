package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// member is one manifest member as the vector records it.
type member struct {
	CHex string `json:"cHex"`
	Name string `json:"name"`
	Size uint64 `json:"size"`
	Type string `json:"type"`
}

// manifestVector is a manifest body: the one-key map whose "members" value
// lists each member as a four-key map (c, name, size, type), every member
// the same shape. RootHex is the RFC 6962 root over the members'
// commitments in list order, which is the root a store of more than one
// member commits to.
type manifestVector struct {
	Members []member `json:"members"`
	BodyHex string   `json:"bodyHex"`
	RootHex string   `json:"rootHex"`
}

// unknownMember is a refs entry member the entry format does not define,
// with its value's canonical encoding.
type unknownMember struct {
	Key      string `json:"key"`
	ValueHex string `json:"valueHex"`
}

// refEntry is one refs entry as the vector records it.
type refEntry struct {
	Name    string          `json:"name"`
	RootHex string          `json:"rootHex"`
	Count   uint64          `json:"count"`
	HeadHex string          `json:"headHex,omitempty"`
	Unknown []unknownMember `json:"unknown,omitempty"`
}

// refsVector is a refs array: the array value a record carries, one map per
// store with the members name, root and count, head when the store is
// linked, and whatever members a later format adds.
type refsVector struct {
	Entries     []refEntry `json:"entries"`
	EncodingHex string     `json:"encodingHex"`
}

// labelled is the 32-byte SHA-256 of a label, a stand-in commitment that is
// fixed and not a repeated byte.
func labelled(label string) []byte {
	h := sha256.Sum256([]byte(label))
	return h[:]
}

func repeat(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func manifest() (manifestVector, error) {
	type m struct {
		c          []byte
		name, kind string
		size       uint64
	}
	// An ordinary part; a member with nothing but its commitment, which is
	// what a chunk identified by position looks like; a name at the 64-byte
	// bound with a size past 32 bits; and a name outside ASCII.
	members := []m{
		{labelled("member 0"), "part-1", "text/plain", 1200},
		{labelled("member 1"), "", "", 0},
		{labelled("member 2"), strings.Repeat("n", 64), "application/octet-stream", 1 << 32},
		{labelled("member 3"), "résumé", "text/uri-list", 65536},
	}
	var v manifestVector
	list := make([]any, 0, len(members))
	leaves := make([][]byte, 0, len(members))
	for _, mem := range members {
		v.Members = append(v.Members, member{CHex: hex.EncodeToString(mem.c), Name: mem.name, Size: mem.size, Type: mem.kind})
		list = append(list, map[string]any{"c": mem.c, "name": mem.name, "size": mem.size, "type": mem.kind})
		leaves = append(leaves, mem.c)
	}
	em, err := encoder()
	if err != nil {
		return v, err
	}
	body, err := em.Marshal(map[string]any{"members": list})
	if err != nil {
		return v, err
	}
	root := mth(leaves)
	v.BodyHex = hex.EncodeToString(body)
	v.RootHex = hex.EncodeToString(root[:])
	return v, nil
}

func refs() (refsVector, error) {
	em, err := encoder()
	if err != nil {
		return refsVector{}, err
	}
	type unknown struct {
		key string
		val node
	}
	type entry struct {
		name    string
		root    []byte
		count   uint64
		head    []byte
		unknown []unknown
	}
	// A store committed to but not linked (no head); a one-member store
	// whose root is the leaf hash of its head, as a one-member store's root
	// must be; and an entry carrying two members the format does not define,
	// one whose key sorts before every defined key and one that sorts among
	// them (after root, before count), which a reader keeps and does not
	// drop.
	mediaHead := repeat(0x55)
	mediaRoot := leafHash(mediaHead)
	entries := []entry{
		{name: "notes", root: repeat(0x44), count: 3},
		{name: "media", root: mediaRoot[:], count: 1, head: mediaHead},
		{name: "archive", root: repeat(0x66), count: 2, head: repeat(0x77), unknown: []unknown{
			{"salt", bytesN(fill(0x88, 16))},
			{"v", uintN(2)},
		}},
	}
	var v refsVector
	arr := make([]any, 0, len(entries))
	for _, e := range entries {
		re := refEntry{Name: e.name, RootHex: hex.EncodeToString(e.root), Count: e.count}
		m := map[string]any{"name": e.name, "root": e.root, "count": e.count}
		if e.head != nil {
			re.HeadHex = hex.EncodeToString(e.head)
			m["head"] = e.head
		}
		for _, u := range e.unknown {
			val, err := u.val.native()
			if err != nil {
				return v, err
			}
			enc, err := em.Marshal(val)
			if err != nil {
				return v, err
			}
			if _, clash := m[u.key]; clash {
				return v, fmt.Errorf("refs: unknown member %q is a defined one", u.key)
			}
			m[u.key] = val
			re.Unknown = append(re.Unknown, unknownMember{Key: u.key, ValueHex: hex.EncodeToString(enc)})
		}
		v.Entries = append(v.Entries, re)
		arr = append(arr, m)
	}
	enc, err := em.Marshal(arr)
	if err != nil {
		return v, err
	}
	v.EncodingHex = hex.EncodeToString(enc)
	return v, nil
}

func storeFamilies() ([]family, error) {
	m, err := manifest()
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	r, err := refs()
	if err != nil {
		return nil, fmt.Errorf("refs: %w", err)
	}
	return []family{{"manifest-v1.json", m}, {"refs-v1.json", r}}, nil
}
