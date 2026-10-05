package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// efTx is a transaction that EF-encodes: its one input carries its source.
func efTx(t *testing.T) *transaction.Transaction {
	t.Helper()
	parent := transaction.NewTransaction()
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: &script.Script{script.OpTRUE}})
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(parent, 0, nil)
	tx.Inputs[0].UnlockingScript = &script.Script{}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: &script.Script{script.OpTRUE}})
	return tx
}

// fakeArcade answers POST /tx with post, and GET /tx/{txid} with the next
// status in statuses (the last one repeats). It records what it was sent.
type fakeArcade struct {
	mu       sync.Mutex
	post     []func(w http.ResponseWriter, txid string)
	statuses []string
	extra    string
	gets     int
	bodies   [][]byte
	auth     []string
}

func (f *fakeArcade) handler(txid string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/tx":
			b, _ := io.ReadAll(r.Body)
			f.bodies = append(f.bodies, b)
			next := f.post[0]
			if len(f.post) > 1 {
				f.post = f.post[1:]
			}
			next(w, txid)
		case r.Method == http.MethodGet && r.URL.Path == "/tx/"+txid:
			i := min(f.gets, len(f.statuses)-1)
			f.gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"txid": txid, "txStatus": f.statuses[i], "extraInfo": f.extra})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tx/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"transaction not found"}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func accepted(status string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, txid string) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"txid":%q,"status":202,"txStatus":%q}`, txid, status)
	}
}

func arcadeFor(t *testing.T, f *fakeArcade, tx *transaction.Transaction) (*Arcade, *[]string) {
	t.Helper()
	srv := httptest.NewServer(f.handler(tx.TxID().String()))
	t.Cleanup(srv.Close)
	var notes []string
	return &Arcade{Base: srv.URL, Verdict: 300 * time.Millisecond, Poll: 20 * time.Millisecond,
		Note: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }}, &notes
}

func TestArcadeSubmitSendsEFAndReturnsOnceTheNetworkAccepts(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: []string{"SENT_TO_NETWORK", "SEEN_ON_NETWORK"}}
	a, notes := arcadeFor(t, f, tx)
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	ef, _ := tx.EF()
	if len(f.bodies) != 1 || string(f.bodies[0]) != string(ef) {
		t.Fatal("arcade was not sent the transaction's EF bytes")
	}
	if f.gets < 2 {
		t.Fatalf("returned before the network's verdict: %d status reads", f.gets)
	}
	if len(*notes) != 0 {
		t.Errorf("unexpected note: %v", *notes)
	}
}

func TestArcadeSubmitReturnsAtOnceWhenAlreadyAccepted(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("SEEN_ON_NETWORK")}, statuses: []string{"SEEN_ON_NETWORK"}}
	a, _ := arcadeFor(t, f, tx)
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if f.gets != 0 {
		t.Errorf("polled %d times for a verdict the submit answer already gave", f.gets)
	}
}

// The reason this settler waits at all: a transaction the network refuses must
// not go on to be published to hosts as if it were a state.
func TestArcadeSubmitRefusesWhatTheNetworkRefuses(t *testing.T) {
	for _, status := range []string{"REJECTED", "DOUBLE_SPEND_ATTEMPTED"} {
		tx := efTx(t)
		f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: []string{status}, extra: "competing spend"}
		a, _ := arcadeFor(t, f, tx)
		err := a.Submit(context.Background(), tx)
		if err == nil || !strings.Contains(err.Error(), status) || !strings.Contains(err.Error(), "competing spend") {
			t.Errorf("%s: want a refusal naming the status and arcade's reason, got %v", status, err)
		}
	}
	// And a refusal in the submit answer itself, on an idempotent re-submit.
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("REJECTED")}, statuses: []string{"REJECTED"}}
	a, _ := arcadeFor(t, f, tx)
	if err := a.Submit(context.Background(), tx); err == nil {
		t.Error("a re-submit answered REJECTED was accepted")
	}
}

func TestArcadeSubmitReportsAPolicyRefusalWithItsReason(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"validation","reason":"non-final transaction"}`))
	}}, statuses: []string{"REJECTED"}}
	a, _ := arcadeFor(t, f, tx)
	err := a.Submit(context.Background(), tx)
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "non-final transaction") {
		t.Fatalf("want the 400 and arcade's reason, got %v", err)
	}
}

func TestArcadeSubmitRetriesBackpressureOnly(t *testing.T) {
	tx := efTx(t)
	busy := func(w http.ResponseWriter, _ string) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service overloaded, retry shortly"}`))
	}
	f := &fakeArcade{post: []func(http.ResponseWriter, string){busy, accepted("SEEN_ON_NETWORK")}, statuses: []string{"SEEN_ON_NETWORK"}}
	a, _ := arcadeFor(t, f, tx)
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatalf("a 503 then a 202: %v", err)
	}
	if len(f.bodies) != 2 {
		t.Fatalf("want one retry, got %d posts", len(f.bodies))
	}
	// A 500 is not documented as retryable, so it is not retried.
	tx2 := efTx(t)
	f2 := &fakeArcade{post: []func(http.ResponseWriter, string){func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to submit"}`))
	}, accepted("SEEN_ON_NETWORK")}, statuses: []string{"SEEN_ON_NETWORK"}}
	a2, _ := arcadeFor(t, f2, tx2)
	if err := a2.Submit(context.Background(), tx2); err == nil {
		t.Fatal("a 500 was retried into a success")
	}
	if len(f2.bodies) != 1 {
		t.Fatalf("a 500 was retried: %d posts", len(f2.bodies))
	}
}

// No verdict inside the bound is not a failure: the transaction is in
// arcade's hands. Waiting on would put a publisher back to waiting for a
// block, which is what this settler exists to avoid.
func TestArcadeSubmitDoesNotWaitPastTheBound(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: []string{"SENT_TO_NETWORK"}}
	a, notes := arcadeFor(t, f, tx)
	start := time.Now()
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %s past a 300ms bound", time.Since(start))
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "SENT_TO_NETWORK") {
		t.Fatalf("want one note naming the last status, got %v", *notes)
	}
}

func TestArcadeRefusesAnAcknowledgementForADifferentTransaction(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"txid":"` + strings.Repeat("ab", 32) + `","status":202,"txStatus":"SEEN_ON_NETWORK"}`))
	}}, statuses: []string{"SEEN_ON_NETWORK"}}
	a, _ := arcadeFor(t, f, tx)
	if err := a.Submit(context.Background(), tx); err == nil || !strings.Contains(err.Error(), "we sent") {
		t.Fatalf("want a txid mismatch refusal, got %v", err)
	}
}

func TestArcadeStatusTellsUnknownFromMinedAndSendsTheKey(t *testing.T) {
	tx := efTx(t)
	f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("SEEN_ON_NETWORK")}, statuses: []string{"MINED"}}
	a, _ := arcadeFor(t, f, tx)
	a.Key = "secret"
	if _, err := a.Status(context.Background(), strings.Repeat("cd", 32)); !errors.Is(err, ErrArcadeUnknown) {
		t.Fatalf("an unknown txid: %v", err)
	}
	st, err := a.Status(context.Background(), tx.TxID().String())
	if err != nil {
		t.Fatal(err)
	}
	// MINED without a path is not something to build a BEEF from.
	if st.Mined() {
		t.Error("MINED with no merkle path reported as provable")
	}
	st.MerklePath = "fe"
	if !st.Mined() {
		t.Error("MINED with a path not reported as provable")
	}
	for _, h := range f.auth {
		if h != "Bearer secret" {
			t.Fatalf("a request went without the key: %q", h)
		}
	}
}

// Arcade's acceptance is held to the node's view when Asset is set: an input
// the node shows spent by another transaction is a refusal, while Submit
// still waits for a verdict and after arcade answers ACCEPTED_BY_NETWORK.
func TestArcadeSubmitHeldToTheNodeRefusesADoubleSpend(t *testing.T) {
	other := strings.Repeat("e7", 32)
	for _, statuses := range [][]string{{"RECEIVED"}, {"ACCEPTED_BY_NETWORK"}} {
		tx := efTx(t)
		f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: statuses}
		a, _ := arcadeFor(t, f, tx)
		a.Verdict = 30 * time.Second
		src := tx.Inputs[0].SourceTXID.String()
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/utxos/"+src+"/json" {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `[{"vout":0,"status":"SPENT","spendingData":{"txId":%q,"vin":0}}]`, other)
		}))
		t.Cleanup(node.Close)
		a.Asset = &nodeapi.Asset{Base: node.URL}
		start := time.Now()
		err := a.Submit(context.Background(), tx)
		if !errors.Is(err, nodeapi.ErrDoubleSpent) || !strings.Contains(err.Error(), "the network refused") || !strings.Contains(err.Error(), other) {
			t.Fatalf("%v: %v", statuses, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%v: refused after %s", statuses, d)
		}
	}
}

// A status that is no evidence of a spender, even one naming a spender
// without being SPENT, is not a refusal: arcade's acceptance stands, and the
// proof, collected later, settles the transaction.
func TestArcadeSubmitHeldToTheNodePassesOverNoEvidence(t *testing.T) {
	other := strings.Repeat("e7", 32)
	for _, status := range []string{"NOT_FOUND", "CONFLICTING", "LOCKED", "FROZEN"} {
		tx := efTx(t)
		f := &fakeArcade{post: []func(http.ResponseWriter, string){accepted("ACCEPTED_BY_NETWORK")}}
		a, _ := arcadeFor(t, f, tx)
		src := tx.Inputs[0].SourceTXID.String()
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/utxos/"+src+"/json" {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `[{"vout":0,"status":%q,"spendingData":{"txId":%q,"vin":0}}]`, status, other)
		}))
		t.Cleanup(node.Close)
		a.Asset = &nodeapi.Asset{Base: node.URL}
		if err := a.Submit(context.Background(), tx); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}
}
