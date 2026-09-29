package store

import (
	"errors"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/commit"
)

// The two halves of the rule, each against the hashes it is defined by
// rather than against Root itself.
func TestRootRule(t *testing.T) {
	a, b, c := fill(0x0a), fill(0x0b), fill(0x0c)

	// One member is its own head: the root is its leaf hash and no member
	// list is read, so one passed anyway changes nothing.
	one := Ref{Name: "notes", Count: 1, Head: &a}
	if Root(one, nil) != commit.LeafHash(a) {
		t.Error("a one-member store's root is not the leaf hash of its head")
	}
	if Root(one, [][32]byte{b, c}) != commit.LeafHash(a) {
		t.Error("a one-member store's root read the member list")
	}

	// More than one: the root is over the members in manifest order and
	// never over the head, which is the manifest.
	two := Ref{Name: "doc", Count: 2, Head: &c}
	want := commit.NodeHash(commit.LeafHash(a), commit.LeafHash(b))
	if Root(two, [][32]byte{a, b}) != want {
		t.Error("a two-member store's root is not over its members")
	}
	if Root(two, [][32]byte{b, a}) == want {
		t.Error("the root does not depend on member order")
	}
	m := &Manifest{Members: []Member{{C: a}, {C: b}}}
	if Root(two, m.Leaves()) != want {
		t.Error("a manifest's leaves do not build its store's root")
	}
}

func TestHead(t *testing.T) {
	h := fill(0xcc)
	for _, c := range []struct {
		name string
		ref  Ref
		is   error
		want string
	}{
		{"extended", Ref{Name: "notes", Count: 1, Head: &h, Unknown: cbor.Map{{Key: "salt", Val: "x"}, {Key: "algo", Val: "y"}}},
			ErrUnsupported, `unsupported store: store "notes" uses [salt algo], which this reader does not implement`},
		{"no head", Ref{Name: "notes", Count: 1}, ErrNoHead, `no head: store "notes" is committed to but not linked`},
		// A ref inconsistent with itself is neither sentinel: it is not a
		// newer store and not an unlinked one.
		{"no members", Ref{Name: "notes", Head: &h}, nil, `store "notes" commits to no members`},
	} {
		_, err := Head(c.ref)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
			continue
		}
		if errors.Unwrap(err) != c.is {
			t.Errorf("%s: wraps %v, want %v", c.name, errors.Unwrap(err), c.is)
		}
	}
	got, err := Head(Ref{Name: "doc", Count: 3, Head: &h})
	if err != nil || got != h {
		t.Fatalf("head %x, %v", got, err)
	}
}
