package chainview

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

func TestRefusedAnswer(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"publish: arcade refused abc: REJECTED: bad-txns-inputs-missingorspent", true},
		{"publish: the network refused abc: DOUBLE_SPEND_ATTEMPTED", true},
		{"publish: arcade answered 465: fee too low", true},
		{"publish: arcade answered 422: unprocessable", true},
		{"publish: arcade answered 460: not extended format", true},
		{"publish: arcade answered 503: busy", false},
		{"publish: arcade answered 401: unauthorized", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: mandatory-script-verify-flag-failed", true},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -25: missing inputs", true},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: mempool full", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: too-long-mempool-chain", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -28: loading", false},
		{"publish: dial 192.0.2.1:8725: connection refused", false},
		{"context deadline exceeded", false},
	} {
		if _, got := RefusedAnswer(errors.New(c.err)); got != c.want {
			t.Errorf("%q: refused %v, want %v", c.err, got, c.want)
		}
	}
	if _, got := RefusedAnswer(nil); got {
		t.Error("nil is a refusal")
	}
}

// Only the node's positive word is worded: an input it answers for with a
// status that is no evidence, even one naming a spender, is passed over.
func TestSpentElsewhereWordsOnlyAPositiveWord(t *testing.T) {
	parent, err := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: parent, SourceTxOutIndex: 0, UnlockingScript: &script.Script{}})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	other := strings.Repeat("33", 32)
	for status, want := range map[string]string{
		"SPENT":       "input 0 (" + parent.String() + ".0) is spent by " + other,
		"NOT_FOUND":   "",
		"CONFLICTING": "",
		"LOCKED":      "",
		"FROZEN":      "",
	} {
		body := `[{"vout":0,"status":"` + status + `","spendingData":{"txId":"` + other + `","vin":0}}]`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		got := SpentElsewhere(context.Background(), &nodeapi.Asset{Base: srv.URL}, tx)
		srv.Close()
		if got != want {
			t.Fatalf("%s: %q", status, got)
		}
	}
}

type spendsBy string

func (s spendsBy) Spender(context.Context, string, uint32) (string, error) { return string(s), nil }

func TestSpentElsewhereInAnySpendView(t *testing.T) {
	src, _ := chainhash.NewHashFromHex(strings.Repeat("ef", 32))
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src, SourceTxOutIndex: 3, SequenceNumber: 0xffffffff})
	by := strings.Repeat("cd", 32)
	want := "input 0 (" + strings.Repeat("ef", 32) + ".3) is spent by " + by
	if got := SpentElsewhereIn(context.Background(), spendsBy(by), tx); got != want {
		t.Fatalf("%q", got)
	}
	if got := SpentElsewhereIn(context.Background(), spendsBy(""), tx); got != "" {
		t.Fatalf("unspent: %q", got)
	}
	if got := SpentElsewhereIn(context.Background(), nil, tx); got != "" {
		t.Fatalf("no view: %q", got)
	}
	if got := SpentElsewhere(context.Background(), nil, tx); got != "" {
		t.Fatalf("no node: %q", got)
	}
}
