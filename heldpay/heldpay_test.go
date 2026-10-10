package heldpay_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/heldpay"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/testchain"
)

func server(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/held":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"error","code":"ERR_PAYMENT_HELD","description":"held for confirmation"}`)
		case "/legacy":
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"status":"error","code":"ERR_PAYMENT_HELD","description":"held, the old way"}`)
		case "/price":
			w.Header().Set("x-bsv-payment-version", "1.0")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"code":"ERR_PAYMENT_REQUIRED"}`)
		case "/staged":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"state":"staged"}`)
		default:
			_, _ = io.WriteString(w, `ok`)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// A held answer is seen in both its forms, and its body still reaches the
// caller; a price and another 202 are not a hold; the payment sent is kept
// across Reset.
func TestTap(t *testing.T) {
	base := server(t)
	tap := &heldpay.Tap{}
	c := &http.Client{Transport: tap}
	get := func(path, payment string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader("{}"))
		if payment != "" {
			req.Header.Set("x-bsv-payment", payment)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, p := range []string{"/price", "/staged", "/plain"} {
		get(p, "")
		if _, held := tap.Held(); held {
			t.Fatalf("%s read as a hold", p)
		}
	}
	if b := get("/held", `{"derivationPrefix":"a"}`); !strings.Contains(b, heldpay.Code) {
		t.Fatalf("the body did not reach the caller: %q", b)
	}
	if why, held := tap.Held(); !held || why != "held for confirmation" || tap.Payment() != `{"derivationPrefix":"a"}` {
		t.Fatalf("held %v %q, payment %q", held, why, tap.Payment())
	}
	tap.Reset()
	if _, held := tap.Held(); held || tap.Payment() == "" {
		t.Fatal("Reset forgot the payment or kept the hold")
	}
	get("/legacy", "")
	if why, held := tap.Held(); !held || why != "held, the old way" {
		t.Fatalf("the legacy 402: %v %q", held, why)
	}
}

// Mined returns at once for a mined transaction and times out for one the
// chain does not hold.
func TestMined(t *testing.T) {
	c := testchain.New(700)
	s := httptest.NewServer(c)
	t.Cleanup(s.Close)
	k, _ := ec.NewPrivateKey()
	addr, _ := script.NewAddressFromPublicKey(k.PubKey(), false)
	if _, err := c.Generate(1, addr.AddressString); err != nil {
		t.Fatal(err)
	}
	txids := c.Txids()
	if len(txids) == 0 {
		t.Fatal("no transaction mined")
	}
	chain := &nodeapi.Asset{Base: s.URL}
	mined := c.Tx(txids[len(txids)-1])
	if err := heldpay.Mined(context.Background(), chain, mined, 10*time.Second, 50*time.Millisecond); err != nil {
		t.Fatalf("a mined transaction: %v", err)
	}
	stray := transaction.NewTransaction()
	if err := heldpay.Mined(context.Background(), chain, stray, 300*time.Millisecond, 50*time.Millisecond); err == nil {
		t.Fatal("a transaction the chain does not hold was taken as mined")
	}
}
