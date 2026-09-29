package commit

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The vector is the RFC 6962 root of the first n of seventeen fixed leaves
// for every n from 1 to 17, and the audit path of every leaf in each, from a
// second implementation written from the RFC's definitions and checked
// against RFC 9162's bit-by-bit verification (tools/vectors). Every split
// shape up to one leaf past a full tree of sixteen is here, where the
// literal roots in TestRootVector stop at five leaves and no path was pinned
// by value.
func TestIndependentVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "rfc6962-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		LeavesHex []string `json:"leavesHex"`
		Trees     []struct {
			Size    int    `json:"size"`
			RootHex string `json:"rootHex"`
			Paths   []struct {
				Index int `json:"index"`
				Steps []struct {
					HashHex string `json:"hashHex"`
					Left    bool   `json:"left"`
				} `json:"steps"`
			} `json:"paths"`
		} `json:"trees"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	hash := func(s string) [32]byte {
		t.Helper()
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 32 {
			t.Fatalf("hash %q: %v", s, err)
		}
		return [32]byte(b)
	}
	leaves := make([][32]byte, len(v.LeavesHex))
	for i, s := range v.LeavesHex {
		leaves[i] = hash(s)
	}
	if len(v.Trees) != 17 || len(leaves) != 17 {
		t.Fatalf("%d trees over %d leaves, want 17 over 17", len(v.Trees), len(leaves))
	}
	for i, tr := range v.Trees {
		n := tr.Size
		if n != i+1 {
			t.Fatalf("tree %d has size %d, want %d", i, n, i+1)
		}
		root := hash(tr.RootHex)
		if got := Root(leaves[:n]); got != root {
			t.Errorf("n=%d: root %x, want %x", n, got, root)
		}
		if len(tr.Paths) != n {
			t.Fatalf("n=%d: %d paths, want one per leaf", n, len(tr.Paths))
		}
		for _, want := range tr.Paths {
			got, err := Prove(leaves[:n], want.Index)
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, want.Index, err)
			}
			if len(got) != len(want.Steps) {
				t.Errorf("n=%d i=%d: %d steps, want %d", n, want.Index, len(got), len(want.Steps))
				continue
			}
			for k, s := range want.Steps {
				if got[k].Hash != hash(s.HashHex) || got[k].Left != s.Left {
					t.Errorf("n=%d i=%d step %d: got {%x left=%v}, want {%s left=%v}",
						n, want.Index, k, got[k].Hash, got[k].Left, s.HashHex, s.Left)
				}
			}
			if !Verify(leaves[want.Index], got, root) {
				t.Errorf("n=%d i=%d: path does not verify against the vector's root", n, want.Index)
			}
		}
	}
}
