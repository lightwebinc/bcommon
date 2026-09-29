package bwallet

import (
	"path/filepath"
	"testing"
)

func TestSuccessorSharesThePool(t *testing.T) {
	dir := t.TempDir()
	primary, err := Create(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	succPath := filepath.Join(dir, "successor.json")
	pub, err := NewIdentityFile(succPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdentityFile(succPath); err == nil {
		t.Fatal("overwrote an identity file")
	}
	succ, err := OpenIdentity(succPath, primary.Pool, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !succ.IdentityKey().IsEqual(pub) || succ.IdentityKey().IsEqual(primary.IdentityKey()) {
		t.Fatal("successor identity is not the one written, or equals the primary")
	}
	if succ.Pool != primary.Pool {
		t.Fatal("successor must share the primary's pool")
	}
	// Different roots derive different fund scripts, which is what decides
	// who may spend a given pool output.
	a, _ := primary.FundScript()
	b, _ := succ.FundScript()
	if a.Equals(b) {
		t.Fatal("two identities derived one fund script")
	}
}
