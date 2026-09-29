package store_test

import (
	"crypto/sha256"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/commit"
	"github.com/lightwebinc/bcommon/store"
)

// A store of several members: the manifest lists the members' commitments,
// the root is over those commitments in manifest order, and the head a
// reader asks a host for is the manifest's own commitment. The refs entry
// is what the parent record carries.
func ExampleRoot() {
	m := &store.Manifest{Members: []store.Member{
		{C: sha256.Sum256([]byte("part one")), Name: "1", Size: 8, Type: "text/plain"},
		{C: sha256.Sum256([]byte("part two")), Name: "2", Size: 8, Type: "text/plain"},
		{C: sha256.Sum256([]byte("part three")), Name: "3", Size: 10, Type: "text/plain"},
	}}
	// The bound is the application's: the body bound of the record that
	// will carry the manifest.
	body, err := m.Body(64 << 10)
	if err != nil {
		fmt.Println(err)
		return
	}
	enc, err := cbor.Encode(body)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("manifest body bytes:", len(enc))

	// In an application the head is the commitment (carrier txid) of the
	// record that carries the manifest body; a stand-in is used here.
	head := sha256.Sum256(enc)
	ref := store.Ref{Name: "docs", Count: uint64(len(m.Members)), Head: &head}
	ref.Root = store.Root(ref, m.Leaves())
	fmt.Println("root is over the members:", ref.Root == commit.Root(m.Leaves()))

	refs, err := store.EncodeRefs([]store.Ref{ref})
	if err != nil {
		fmt.Println(err)
		return
	}
	back, err := store.DecodeRefs(refs)
	if err != nil {
		fmt.Println(err)
		return
	}
	h, err := store.Head(back[0])
	fmt.Println("head round-trips:", h == head, err)
	// Output:
	// manifest body bytes: 208
	// root is over the members: true
	// head round-trips: true <nil>
}

// A store of one member is its own head: its root is the leaf hash of that
// member and no manifest exists.
func ExampleRoot_oneMember() {
	member := sha256.Sum256([]byte("the only part"))
	ref := store.Ref{Name: "note", Count: 1, Head: &member}
	ref.Root = store.Root(ref, nil)
	fmt.Println(ref.Root == commit.LeafHash(member))
	// Output:
	// true
}
