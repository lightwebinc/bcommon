package commit

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func leaf(b byte) [32]byte {
	var l [32]byte
	for i := range l {
		l[i] = b
	}
	return l
}

// The empty root and the single-leaf root are the two cases RFC 6962 pins
// by formula, so they are checked against the formula, not against this
// package's own output.
func TestRootFormulas(t *testing.T) {
	if Root(nil) != sha256.Sum256(nil) {
		t.Fatal("empty root is not SHA-256 of the empty string")
	}
	one := leaf(0x11)
	want := sha256.Sum256(append([]byte{0x00}, one[:]...))
	if Root([][32]byte{one}) != want {
		t.Fatal("single-leaf root is not SHA-256(0x00 || leaf)")
	}
	// RFC 6962 §2.1 example vector: the root of the empty tree.
	if hex.EncodeToString(func() []byte { r := Root(nil); return r[:] }()) !=
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("empty root vector")
	}
}

func TestPathsVerifyForEveryLength(t *testing.T) {
	for n := 1; n <= 9; n++ {
		leaves := make([][32]byte, n)
		for i := range leaves {
			leaves[i] = leaf(byte(i + 1))
		}
		root := Root(leaves)
		for i := range leaves {
			p, err := Prove(leaves, i)
			if err != nil {
				t.Fatal(err)
			}
			if !Verify(leaves[i], p, root) {
				t.Fatalf("n=%d i=%d: path does not verify", n, i)
			}
			// A different leaf on the same path must not verify: the
			// negative control for the positive above.
			if Verify(leaf(0xEE), p, root) {
				t.Fatalf("n=%d i=%d: a foreign leaf verified", n, i)
			}
		}
	}
	if _, err := Prove(nil, 0); err == nil {
		t.Fatal("proving an empty set must fail")
	}
}

// Leaves and nodes must never collide: the same 64 bytes hashed as a leaf
// and as a node differ, which is the whole reason for the prefixes.
func TestDomainSeparation(t *testing.T) {
	a, b := leaf(0x01), leaf(0x02)
	var joined [32]byte
	copy(joined[:], append(a[:16:16], b[:16]...))
	if LeafHash(joined) == NodeHash(a, b) {
		t.Fatal("leaf and node hashes collide")
	}
}

// rfc6962 is the Merkle Tree Hash of RFC 6962 §2.1 written out a second
// time, straight from the definition with crypto/sha256 and nothing of this
// package, so the literal roots below answer to two routes that share no
// code.
func rfc6962(d [][]byte) []byte {
	switch len(d) {
	case 0:
		h := sha256.Sum256(nil)
		return h[:]
	case 1:
		h := sha256.Sum256(append([]byte{0x00}, d[0]...))
		return h[:]
	}
	k := 1
	for k*2 < len(d) {
		k *= 2
	}
	msg := append([]byte{0x01}, rfc6962(d[:k])...)
	msg = append(msg, rfc6962(d[k:])...)
	h := sha256.Sum256(msg)
	return h[:]
}

// The roots over leaves of 32 bytes of 0x01, 0x02, ... are pinned by
// literal. TestRootFormulas checks the empty and one-leaf roots against
// crypto/sha256 directly, but a root of two or more leaves, the only kind
// the node prefix reaches, is otherwise checked against the package itself,
// and a changed node prefix moves Root, Prove and Verify together, so only a
// root computed elsewhere notices; these were computed outside this package
// and the second implementation recomputes them every run. Two and four
// leaves are balanced trees; three and five split unevenly, leaving a lone
// leaf on the right.
func TestRootVector(t *testing.T) {
	want := []string{
		"dcffe786ded16d283c663846ad0c4ff26558fccde36ca9d30b2ea19eade9fc0e",
		"3a066e0f40c6a1981ebfa60d2411625d0517ae22c2fc8c7c1784ff8a75c78565",
		"df896896c799531f1fd1e556cea26a6989ab06853bcbfdd3e4f5097a611f658f",
		"3b3c0ce45d11517a54300a196b61497c4165150d72b7782a4548e3984da771b2",
		"c51042bb8b9d81dfc115ef99d0e2cecf1954cfc078d70032d187b46615f01b90",
	}
	var leaves [][32]byte
	var raw [][]byte
	for n := 1; n <= len(want); n++ {
		l := leaf(byte(n))
		leaves = append(leaves, l)
		raw = append(raw, l[:])
		if second := hex.EncodeToString(rfc6962(raw)); second != want[n-1] {
			t.Errorf("n=%d: the second implementation gives %s, not the literal %s", n, second, want[n-1])
		}
		if got := Root(leaves); hex.EncodeToString(got[:]) != want[n-1] {
			t.Errorf("n=%d: root %x, want %s", n, got, want[n-1])
		}
	}
}

// A caller that matches the refusal by text sees this exact text, so it is
// pinned by literal, and Prove returns the sentinel itself on either side of
// the leaf set.
func TestErrIndexText(t *testing.T) {
	if got := ErrIndex.Error(); got != "commit: index out of range" {
		t.Fatalf("ErrIndex %q, frozen as %q", got, "commit: index out of range")
	}
	leaves := [][32]byte{leaf(0x01), leaf(0x02)}
	for _, i := range []int{-1, 2} {
		if _, err := Prove(leaves, i); err != ErrIndex {
			t.Errorf("index %d: %v, want ErrIndex itself", i, err)
		}
	}
}
