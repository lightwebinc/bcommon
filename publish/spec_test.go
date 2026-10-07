package publish

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/nodeapi"
)

func TestParseSettler(t *testing.T) {
	spends := spendsBy{}
	opt := SettleOptions{Key: "k", RPCUser: "u", RPCPass: "p", RPCID: "app", Spends: spends}
	for spec, want := range map[string]string{
		"arcade:main":                     "arcade:" + ArcadeMainnet,
		"arcade:test":                     "arcade:" + ArcadeTestnet,
		" arcade:https://arcade.example/": "arcade:https://arcade.example",
		"arc:https://arc.gorillapool.io":  "arcade:https://arc.gorillapool.io/v1",
		"arc:https://arc.taal.com/v1/":    "arcade:https://arc.taal.com/v1",
		"rpc:http://node:8332":            "rpc:http://node:8332",
		"tcp:ingress:8725":                "tcp:ingress:8725",
	} {
		s, a, err := ParseSettler(spec, opt)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if s.Name() != want {
			t.Errorf("%q: %s, want %s", spec, s.Name(), want)
		}
		if strings.HasPrefix(want, "arcade:") {
			if a == nil || a.Key != "k" || a.Spends == nil {
				t.Errorf("%q: arcade %+v", spec, a)
			}
		} else if a != nil {
			t.Errorf("%q: not an arcade", spec)
		}
		if r, ok := s.(*RPCSettler); ok && (r.RPC.User != "u" || r.RPC.Pass != "p" || r.RPC.ID != "app") {
			t.Errorf("rpc options: %+v", r.RPC)
		}
	}
	for _, bad := range []string{"", "arcade:", "arcade:regtest", "arc:", "arc:ftp://x", "rpc:node", "tcp:", "tcp:host", "woc:main", "https://arcade.example"} {
		if _, _, err := ParseSettler(bad, opt); !errors.Is(err, ErrSettleSpec) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for net, want := range map[string]string{"main": "arcade:main", "test": "arcade:test"} {
		if got, err := DefaultSettle(net); err != nil || got != want {
			t.Errorf("%s: %s %v", net, got, err)
		}
	}
	if _, err := DefaultSettle("regtest"); !errors.Is(err, ErrSettleSpec) {
		t.Fatal(err)
	}
}

// The mined transaction and its BUMP, from WhatsOnChain's BEEF of a mainnet
// transaction in block 900000, captured 2026-10-07.
func minedFixture(t *testing.T) (string, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sources", "woc-main-tx-beef.txt"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil {
		t.Fatal(err)
	}
	return tx.TxID().String(), tx.MerklePath.Hex()
}

func statusServer(t *testing.T, answers map[string]string) *Arcade {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := answers[strings.TrimPrefix(r.URL.Path, "/tx/")]
		if !ok {
			// arcade.gorillapool.io's answer for a txid it does not hold.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"transaction not found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Arcade{Base: srv.URL, Client: srv.Client()}
}

func TestArcadeProofAndKnown(t *testing.T) {
	ctx := context.Background()
	txid, bump := minedFixture(t)
	other := strings.Repeat("ab", 32)
	a := statusServer(t, map[string]string{
		txid:                     fmt.Sprintf(`{"txid":%q,"txStatus":"MINED","blockHeight":900000,"merklePath":%q}`, txid, bump),
		strings.Repeat("01", 32): `{"txStatus":"SEEN_ON_NETWORK"}`,
		strings.Repeat("02", 32): `{"txStatus":"REJECTED","extraInfo":"bad"}`,
		strings.Repeat("03", 32): fmt.Sprintf(`{"txStatus":"MINED","blockHeight":900001,"merklePath":%q}`, bump),
		other:                    fmt.Sprintf(`{"txStatus":"MINED","merklePath":%q}`, bump),
	})
	mp, h, err := a.Proof(ctx, txid)
	if err != nil || h != 900000 || mp.BlockHeight != 900000 {
		t.Fatalf("%d %v", h, err)
	}
	if _, _, err := a.Proof(ctx, strings.Repeat("01", 32)); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("seen: %v", err)
	}
	if _, _, err := a.Proof(ctx, strings.Repeat("04", 32)); !errors.Is(err, nodeapi.ErrNotMined) || !errors.Is(err, ErrArcadeUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if _, _, err := a.Proof(ctx, strings.Repeat("02", 32)); !errors.Is(err, ErrArcadeRefused) {
		t.Fatalf("refused: %v", err)
	}
	if _, _, err := a.Proof(ctx, other); err == nil || errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("a proof of another transaction: %v", err)
	}
	if ok, err := a.Known(ctx, txid); !ok || err != nil {
		t.Fatalf("known: %v", err)
	}
	if ok, err := a.Known(ctx, strings.Repeat("04", 32)); ok || err != nil {
		t.Fatalf("unknown: %v", err)
	}
	// Behind Sources, arcade's "not mined" passes the question on.
	src := &nodeapi.Sources{Proofs: []nodeapi.ProofSource{a}}
	if _, _, err := src.Proof(ctx, strings.Repeat("04", 32)); !errors.Is(err, nodeapi.ErrNotMined) {
		t.Fatalf("sources: %v", err)
	}
}

// spendsBy names by as the spender of every output.
type spendsBy struct{ by string }

func (s spendsBy) Spender(context.Context, string, uint32) (string, error) { return s.by, nil }

func TestArcadeSubmitHeldToASpendView(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("ACCEPTED_BY_NETWORK")}, statuses: []string{"ACCEPTED_BY_NETWORK"}}
	a, _ := arcadeFor(t, f, tx)
	a.Spends = spendsBy{by: strings.Repeat("cd", 32)}
	err := a.Submit(context.Background(), tx)
	var se *nodeapi.SpentError
	if !errors.Is(err, nodeapi.ErrDoubleSpent) || !errors.As(err, &se) {
		t.Fatalf("%v", err)
	}
	// Spends wins over Asset.
	a.Asset = &nodeapi.Asset{Base: "http://127.0.0.1:1"}
	if err := a.Submit(context.Background(), tx); !errors.Is(err, nodeapi.ErrDoubleSpent) {
		t.Fatalf("%v", err)
	}
	a.Spends = spendsBy{}
	a.Asset = nil
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatalf("unspent: %v", err)
	}
}
