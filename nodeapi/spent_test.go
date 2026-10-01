package nodeapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestSpenderReadsTheNodesUTXOView(t *testing.T) {
	ctx := context.Background()
	a := utxoNode(t, `[{"txid":"`+txidHex+`","vout":0,"status":"OK"},{"txid":"`+txidHex+`","vout":1,"status":"SPENT","spendingData":{"txId":"`+otherHex+`","vin":0}},{"vout":2,"status":"SPENT"}]`)
	if by, err := a.Spender(ctx, txidHex, 0); err != nil || by != "" {
		t.Fatalf("unspent: %q %v", by, err)
	}
	if by, err := a.Spender(ctx, txidHex, 1); err != nil || by != otherHex {
		t.Fatalf("spent: %q %v", by, err)
	}
	if _, err := a.Spender(ctx, txidHex, 2); err == nil || !strings.Contains(err.Error(), "names no spender") {
		t.Fatalf("spent with no spender: %v", err)
	}
	if _, err := a.Spender(ctx, txidHex, 3); err == nil || !strings.Contains(err.Error(), "no output 3") {
		t.Fatalf("no such output: %v", err)
	}
	if _, err := a.Spender(ctx, otherHex, 0); !IsHTTP(err, http.StatusNotFound) {
		t.Fatalf("unknown: %v", err)
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
