package producer_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
)

// An arcade proof is a service's answer like a node's, and is held to the
// same checks: it parses without panicking, it names the transaction asked
// about, and it agrees with the height arcade reported. A path for some other
// transaction would be stored as this one's and republished to every host.
func TestArcadeProofIsHeldToTheNodesChecks(t *testing.T) {
	txid := strings.Repeat("11", 32)
	path := func(id string, height uint32) string {
		h, err := chainhash.NewHashFromHex(id)
		if err != nil {
			t.Fatal(err)
		}
		mp, err := transaction.NewMerklePathFromCoinbaseTxid(h, height)
		if err != nil {
			t.Fatal(err)
		}
		return mp.Hex()
	}
	good := path(txid, 700)
	cases := []struct {
		name, path string
		height     uint32
		want       string
	}{
		{"valid", good, 700, ""},
		{"height not reported", good, 0, ""},
		{"another transaction's path", path(strings.Repeat("22", 32), 700), 700, "does not contain the txid"},
		{"truncated", good[:12], 700, "arcade's proof"},
		{"not hex", "zz", 700, "not hex"},
		{"height disagrees", good, 701, "reported height 701"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": txid, "txStatus": "MINED", "merklePath": c.path, "blockHeight": c.height})
		}))
		mp, height, err := producer.Proofs{Arcade: &publish.Arcade{Base: srv.URL}}.Of(context.Background(), txid)
		srv.Close()
		if c.want == "" {
			if err != nil || mp == nil || height != 700 {
				t.Errorf("%s: %v height %d", c.name, err, height)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// Arcade answers first when it knows the transaction: its refusal is
// ErrRefused, and accepted-but-unmined is not mined. What arcade does not
// know, the node answers, and with neither there is nothing mined.
func TestProofsAskArcadeThenTheNode(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	p := producer.Proofs{Arcade: l.arcade(), Asset: l.asset()}
	refused, accepted, elsewhere := strings.Repeat("a1", 32), strings.Repeat("a2", 32), strings.Repeat("a3", 32)
	l.mu.Lock()
	l.refused[refused] = "double spend"
	l.accepted[accepted] = true
	l.mu.Unlock()

	if _, _, err := p.Of(ctx, refused); !errors.Is(err, producer.ErrRefused) || !strings.Contains(err.Error(), "REJECTED: double spend") {
		t.Fatalf("refused: %v", err)
	}
	if _, _, err := p.Of(ctx, accepted); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("accepted: %v", err)
	}
	if _, _, err := p.Of(ctx, elsewhere); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("unknown to both: %v", err)
	}
	l.mine(elsewhere)
	if mp, height, err := p.Of(ctx, elsewhere); err != nil || mp == nil || height != 701 {
		t.Fatalf("mined, broadcast elsewhere: %v %d", err, height)
	}
	if _, _, err := (producer.Proofs{}).Of(ctx, elsewhere); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("no source: %v", err)
	}
	// An arcade that fails otherwise is an error, not a fall-through.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"reason":"overloaded"}`, http.StatusInternalServerError)
	}))
	defer failing.Close()
	down := producer.Proofs{Arcade: &publish.Arcade{Base: failing.URL}, Asset: l.asset()}
	if _, _, err := down.Of(ctx, elsewhere); err == nil || errors.Is(err, nodeapi.ErrNotMined) || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("an arcade that fails: %v", err)
	}
}
