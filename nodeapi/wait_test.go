package nodeapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// A node that places the transaction on the third poll and serves its proof
// on the second ask after that, which is the observed node sequence: the
// txmeta lands first, the merkle_proof route answers 500 until the subtree
// is sealed.
func TestWaitMinedPollsUntilPlacedAndProven(t *testing.T) {
	txid, err := chainhash.NewHashFromHex(txidHex)
	if err != nil {
		t.Fatal(err)
	}
	proof := testBump(t, txid, 1200).Bytes()
	var metaHits, proofHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/txmeta/"):
			if atomic.AddInt32(&metaHits, 1) < 3 {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(fixtureTxMeta))
		case strings.HasPrefix(r.URL.Path, "/api/v1/merkle_proof/"):
			if atomic.AddInt32(&proofHits, 1) < 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(proof)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	mp, height, err := WaitMined(context.Background(), &Asset{Base: srv.URL}, txidHex, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if height != 1200 || mp == nil || mp.BlockHeight != 1200 {
		t.Fatalf("height %d proof %+v", height, mp)
	}
	if metaHits < 3 || proofHits < 2 {
		t.Fatalf("meta %d proof %d: the waits did not happen", metaHits, proofHits)
	}
}

func TestWaitMinedStopsWithTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, _, err := WaitMined(ctx, &Asset{Base: srv.URL}, txidHex, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the context's deadline", err)
	}
}

// A proof whose block disagrees with the placement is a node disagreeing
// with itself, and WaitMined refuses rather than picking one.
func TestWaitMinedRefusesHeightDisagreement(t *testing.T) {
	txid, err := chainhash.NewHashFromHex(txidHex)
	if err != nil {
		t.Fatal(err)
	}
	proof := testBump(t, txid, 1199).Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/txmeta/"):
			_, _ = w.Write([]byte(fixtureTxMeta))
		case strings.HasPrefix(r.URL.Path, "/api/v1/merkle_proof/"):
			_, _ = w.Write(proof)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, _, err = WaitMined(context.Background(), &Asset{Base: srv.URL}, txidHex, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "placement height") {
		t.Fatalf("err %v, want a height disagreement", err)
	}
}
