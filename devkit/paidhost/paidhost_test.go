package paidhost

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/testchain"
)

func route(t *testing.T, prices map[string]uint64) *httptest.Server {
	t.Helper()
	k, _ := ec.PrivateKeyFromBytes([]byte(strings.Repeat("\x07", 32)))
	s := Service{
		Name:    "ls_sample",
		Classes: []string{"recent", "history"},
		Ask: func(q json.RawMessage) (string, func() (any, error), error) {
			var x struct {
				Class string `json:"class"`
			}
			if err := json.Unmarshal(q, &x); err != nil || x.Class == "" {
				return "", nil, errors.New("no class")
			}
			return x.Class, func() (any, error) { return []string{"answer to " + x.Class}, nil }, nil
		},
		Headers: testchain.New(700),
	}
	srv := httptest.NewServer(New(s, k, prices))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func lookup(t *testing.T, url, body string) (int, string) {
	t.Helper()
	res, err := http.Post(url+"/lookup", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The terms document lists the priced classes in the service's order; a
// free class is answered to anyone; a priced one needs BRC-104 first; a
// question for another service, or one Ask refuses, is answered 400.
func TestTermsAndUnpaidQuestions(t *testing.T) {
	srv := route(t, map[string]uint64{"history": 5})
	if code, body := get(t, srv.URL+"/ls_sample/terms"); code != 200 || body != `{"classes":[{"class":"history","satoshis":5}],"service":"ls_sample","terms":1}` {
		t.Fatalf("terms: %d %s", code, body)
	}
	if code, body := lookup(t, srv.URL, `{"service":"ls_sample","query":{"class":"recent"}}`); code != 200 || !strings.Contains(body, `"outputs":["answer to recent"]`) {
		t.Fatalf("free: %d %s", code, body)
	}
	if code, body := lookup(t, srv.URL, `{"service":"ls_sample","query":{"class":"history"}}`); code != 401 || !strings.Contains(body, "ERR_AUTH_REQUIRED") {
		t.Fatalf("priced without auth: %d %s", code, body)
	}
	if code, _ := lookup(t, srv.URL, `{"service":"ls_other","query":{"class":"recent"}}`); code != 400 {
		t.Fatalf("another service: %d", code)
	}
	if code, body := lookup(t, srv.URL, `{"service":"ls_sample","query":{}}`); code != 400 || !strings.Contains(body, "no class") {
		t.Fatalf("a question Ask refuses: %d %s", code, body)
	}
	if code, _ := get(t, route(t, nil).URL+"/ls_sample/terms"); code != 404 {
		t.Fatalf("no prices, no terms: %d", code)
	}
}
