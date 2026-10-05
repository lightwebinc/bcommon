package nodeapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// spendingTx spends output 1 of txidHex and output 0 of a parent the node
// does not know.
func spendingTx(t *testing.T) *transaction.Transaction {
	t.Helper()
	known, err := chainhash.NewHashFromHex(txidHex)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := chainhash.NewHashFromHex(strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: unknown, SourceTxOutIndex: 0, UnlockingScript: &script.Script{}})
	tx.AddInput(&transaction.TransactionInput{SourceTXID: known, SourceTxOutIndex: 1, UnlockingScript: &script.Script{}})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	return tx
}

// utxoNode answers the UTXO view of txidHex with body, and 404 for
// everything else.
func utxoNode(t *testing.T, body string) *Asset {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/utxos/"+txidHex+"/json" {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Asset{Base: srv.URL}
}

const otherHex = "3333333333333333333333333333333333333333333333333333333333333333"

// utxosFixture is a node's answer for txidHex with one output in each
// status the node answers, in the node's own shape.
func utxosFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../testdata/fixtures/nodeapi/utxos.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Only OK and SPENT with a spender are evidence. Every other answer is an
// error wrapping ErrSpendUnknown, never "", whatever else it carries: a
// FROZEN output names a placeholder spender and a CONFLICTING one may name
// a real one, and neither is SPENT.
func TestSpenderReadsTheNodesUTXOView(t *testing.T) {
	ctx := context.Background()
	a := utxoNode(t, utxosFixture(t))
	if by, err := a.Spender(ctx, txidHex, 0); err != nil || by != "" {
		t.Fatalf("unspent: %q %v", by, err)
	}
	if by, err := a.Spender(ctx, txidHex, 1); err != nil || by != otherHex {
		t.Fatalf("spent: %q %v", by, err)
	}
	for vout, want := range map[uint32]string{
		2: `status "NOT_FOUND"`,
		3: `status "IMMATURE"`,
		4: `status "FROZEN"`,
		5: `status "CONFLICTING"`,
		6: `status "LOCKED"`,
		7: `status "CONFLICTING"`,
		8: "names no spender",
		9: "no output 9",
	} {
		by, err := a.Spender(ctx, txidHex, vout)
		if by != "" || !errors.Is(err, ErrSpendUnknown) || !strings.Contains(err.Error(), want) {
			t.Fatalf("output %d: %q %v", vout, by, err)
		}
	}
}

// A transaction the node does not serve, which it also answers for one it
// has pruned, is no evidence either.
func TestSpenderOfATransactionTheNodeDoesNotServe(t *testing.T) {
	body, err := os.ReadFile("../testdata/fixtures/nodeapi/utxos_unknown.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	by, err := (&Asset{Base: srv.URL}).Spender(context.Background(), txidHex, 0)
	if by != "" || !errors.Is(err, ErrSpendUnknown) || !IsHTTP(err, http.StatusNotFound) {
		t.Fatalf("%q %v", by, err)
	}
}

// An answer that is not the node's shape is no evidence, even where it
// would read as unspent.
func TestSpenderRefusesAMalformedAnswer(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, body string }{
		{"not JSON", `{`},
		{"not a list", `{"vout":0,"status":"OK"}`},
		{"a null entry", `[null]`},
		{"no index", `[{"status":"OK"}]`},
		{"no status", `[{"vout":0}]`},
		{"an empty status", `[{"vout":0,"status":""}]`},
		{"another status", `[{"vout":0,"status":"UNSPENT"}]`},
		{"the output twice", `[{"vout":0,"status":"OK"},{"vout":0,"status":"OK"}]`},
		{"another transaction", `[{"txid":"` + otherHex + `","vout":0,"status":"OK"}]`},
		{"a spender that is not a txid", `[{"vout":0,"status":"SPENT","spendingData":{"txId":"zz","vin":0}}]`},
		{"an empty answer", `[]`},
	} {
		by, err := utxoNode(t, c.body).Spender(ctx, txidHex, 0)
		if by != "" || !errors.Is(err, ErrSpendUnknown) {
			t.Fatalf("%s: %q %v", c.name, by, err)
		}
	}
	// A failed read is the same.
	_, err := (&Asset{Base: "http://127.0.0.1:1"}).Spender(ctx, txidHex, 0)
	if !errors.Is(err, ErrSpendUnknown) {
		t.Fatalf("no node: %v", err)
	}
}

// The node's positive word refuses; an input it cannot answer for, or one
// spent by the transaction itself, does not.
func TestSpentElsewhere(t *testing.T) {
	ctx := context.Background()
	tx := spendingTx(t)
	spent := `[{"vout":1,"status":"SPENT","spendingData":{"txId":"%s","vin":1}}]`
	a := utxoNode(t, strings.Replace(spent, "%s", otherHex, 1))
	err := a.SpentElsewhere(ctx, tx)
	var se *SpentError
	if !errors.Is(err, ErrDoubleSpent) || !errors.As(err, &se) || se.Input != 1 || se.By != otherHex || se.Outpoint != txidHex+".1" {
		t.Fatalf("spent elsewhere: %v", err)
	}
	if err := utxoNode(t, strings.Replace(spent, "%s", tx.TxID().String(), 1)).SpentElsewhere(ctx, tx); err != nil {
		t.Fatalf("own spend: %v", err)
	}
	if err := utxoNode(t, `[{"vout":1,"status":"OK"}]`).SpentElsewhere(ctx, tx); err != nil {
		t.Fatalf("unspent: %v", err)
	}
	for _, status := range []string{"NOT_FOUND", "FROZEN", "CONFLICTING", "LOCKED", "IMMATURE", ""} {
		body := `[{"vout":1,"status":"` + status + `","spendingData":{"txId":"` + otherHex + `","vin":0}}]`
		if err := utxoNode(t, body).SpentElsewhere(ctx, tx); err != nil {
			t.Fatalf("status %q: %v", status, err)
		}
	}
	var none *Asset
	if err := none.SpentElsewhere(ctx, tx); err != nil {
		t.Fatalf("no node: %v", err)
	}
}

// WaitSettled returns the refusal at once instead of waiting for ctx.
func TestWaitSettledStopsOnADoubleSpend(t *testing.T) {
	a := utxoNode(t, `[{"vout":1,"status":"SPENT","spendingData":{"txId":"`+otherHex+`","vin":0}}]`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := WaitSettled(ctx, a, spendingTx(t), 5*time.Millisecond)
	if !errors.Is(err, ErrDoubleSpent) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

// A status that is no evidence settles nothing: the wait goes on until the
// transaction mines or ctx ends, and never reads as settled.
func TestWaitSettledWaitsThroughAnUnknownStatus(t *testing.T) {
	a := utxoNode(t, `[{"vout":1,"status":"NOT_FOUND"}]`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	mp, _, err := WaitSettled(ctx, a, spendingTx(t), 5*time.Millisecond)
	if mp != nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDoubleSpent) {
		t.Fatalf("%v %v", mp, err)
	}
}
