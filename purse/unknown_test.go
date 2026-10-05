package purse

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

	"github.com/lightwebinc/bcommon/nodeapi"
)

// A payment whose input the node answers for with a status that is no
// evidence (NOT_FOUND, or one naming a spender without being SPENT) is
// neither refused nor settled: Await ends in ErrNotMined, which an
// application retries, and never in a *RefusedError.
func TestAwaitDecidesNothingOnAStatusThatIsNoEvidence(t *testing.T) {
	parent, err := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: parent, SourceTxOutIndex: 0, UnlockingScript: &script.Script{}})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	for _, body := range []string{
		`[{"vout":0,"status":"NOT_FOUND"}]`,
		`[{"vout":0,"status":"CONFLICTING","spendingData":{"txId":"` + strings.Repeat("33", 32) + `","vin":0}}]`,
		`[{"vout":0,"status":"FROZEN","spendingData":{"txId":"` + strings.Repeat("ff", 32) + `","vin":0}}]`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/utxos/"+parent.String()+"/json" {
				_, _ = w.Write([]byte(body))
				return
			}
			http.NotFound(w, r)
		}))
		p := &Purse{Asset: &nodeapi.Asset{Base: srv.URL}, Wait: 50 * time.Millisecond, Poll: 5 * time.Millisecond}
		err := p.Await(context.Background(), &Incoming{Tx: tx, Txid: tx.TxID().String()})
		srv.Close()
		var re *RefusedError
		if !errors.Is(err, ErrNotMined) || errors.As(err, &re) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}
