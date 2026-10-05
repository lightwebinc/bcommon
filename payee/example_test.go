package payee_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/payee"
)

// A host's ledger is read whatever version wrote each line, and a line cut
// short by a crash is passed over: its question was never answered.
func ExampleReadLedger() {
	ledger := `{"txid":"aa","beef":"","outputIndex":0,"satoshis":5,"derivationPrefix":"cA==","derivationSuffix":"cw==","senderIdentityKey":"02ab","class":"history"}
{"txid":"bb","beef":"","outputIndex":0,"satoshis":7,"derivationPrefix":"cA==","derivationSuffix":"cw==","senderIdentityKey":"02ab","class":"history","inputs":["cc.0"],"at":1}
{"txid":"dd","be`
	var warn bytes.Buffer
	ps, err := payee.ReadLedger(strings.NewReader(ledger), "payments.jsonl", &warn)
	for _, p := range ps {
		fmt.Println(p.Txid, p.Satoshis, "version", p.Version())
	}
	fmt.Print(warn.String(), err, "\n")
	line, _ := ps[1].Line()
	fmt.Print(string(line))
	// Output:
	// aa 5 version 1
	// bb 7 version 2
	// payments.jsonl line 3: not a payment; skipped
	// <nil>
	// {"txid":"bb","beef":"","outputIndex":0,"satoshis":7,"derivationPrefix":"cA==","derivationSuffix":"cw==","senderIdentityKey":"02ab","class":"history","inputs":["cc.0"],"at":1}
}

// A host accepts a txid once, and a coin once: it answers a replay and a
// conflict 409.
func ExampleClaims() {
	var c payee.Claims
	fmt.Println(c.Claim("aa", []string{"cc.0"}))
	fmt.Println(c.Claim("aa", []string{"cc.0"}))
	fmt.Println(c.Claim("bb", []string{"cc.0", "dd.1"}))
	fmt.Println(c.Claim("bb", []string{"dd.1"}))
	// Output:
	// accepted
	// replayed
	// conflict
	// accepted
}

// An application settles from its command: it reads the ledgers before it
// opens its home, hands the Settler its purse and its record, and ends a run
// that did not settle everything with its refusal status. Here the record
// already holds every payment, so the run touches no purse.
func ExampleSettler() {
	ps := []payee.Payment{{Txid: "aa", Satoshis: 5, Class: "history"}, {Txid: "bb", Satoshis: 7, Class: "audit"}}
	var book payee.Book // embedded in the application's state
	book.AddSettled("aa")
	book.AddUnsettleable("bb", "input 0 (cc.0) is spent by dd")
	var out bytes.Buffer
	s := &payee.Settler{
		App:    "sample",
		Payer:  noPurse{}, // the payee's *purse.Purse
		Record: payee.Saved(&book, func() error { return nil }),
		Out:    &out,
	}
	rep, err := s.Settle(context.Background(), ps)
	fmt.Print(out.String())
	fmt.Printf("%q %v\n", rep.Problem(), err)
	// Output:
	// 0 payment(s) settled, 0 sat; 1 settled before; 0 not settled; 0 refused (1 before); pool 0 output(s), 0 sat
	// "" <nil>
}

// noPurse stands in for the purse in the example; it is never asked.
type noPurse struct{ payee.Payer }
