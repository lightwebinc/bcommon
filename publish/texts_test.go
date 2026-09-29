package publish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// cause stands, in a frozen text, for the error a format wraps with %w when
// that error is not this package's: the standard library's or the SDK's
// wording is theirs to change, the words around it are ours.
const cause = "<cause>"

// frozen is one text a caller sees, and the literal it is held to.
//
// The journal's refusals and every error the settlement and object legs
// return reach an operator as text, through a caller's stderr or a journal
// entry's efError and beefError. Scripts and operators match on it, so the
// words are pinned by literal: a test that built the expected text from the
// same format would pass through any rewording.
type frozen struct {
	name string
	err  error
	want string
}

func (f frozen) check(t *testing.T) {
	t.Helper()
	if f.err == nil {
		t.Errorf("%s: no error, want %q", f.name, f.want)
		return
	}
	want := f.want
	if strings.Contains(want, cause) {
		// The cause is taken from the wrap chain, so a %w turned into %v
		// fails here even though the words would read the same.
		inner := errors.Unwrap(f.err)
		if inner == nil {
			t.Errorf("%s: %q does not wrap its cause", f.name, f.err)
			return
		}
		want = strings.Replace(want, cause, inner.Error(), 1)
	}
	if f.err.Error() != want {
		t.Errorf("%s: text %q, frozen as %q", f.name, f.err.Error(), want)
	}
}

// closedURL is the address of a server that is no longer there.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// unencodable is a transaction whose one input has no source output, so it
// has no EF encoding.
func unencodable() *transaction.Transaction {
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{})
	return tx
}

// Every refusal Write, Read, Update and List return.
func TestJournalTextsAreFrozen(t *testing.T) {
	var rows []frozen
	add := func(name string, err error, want string) { rows = append(rows, frozen{name, err, want}) }

	j := Journal{Dir: t.TempDir()}
	add("write, bad txid", j.Write(Entry{Seq: 1, TxID: "abc"}), `publish: journal: txid "abc" is not 64 hex characters`)
	_, err := j.Read(1, "abc")
	add("read, bad txid", err, `publish: journal: txid "abc" is not 64 hex characters`)
	_, err = j.Read(4, txA)
	add("read, no entry", err, "publish: no journal entry: 4-"+txA+".json")
	if err := os.WriteFile(filepath.Join(j.Dir, "5-"+txA+".json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = j.Read(5, txA)
	add("read, not JSON", err, "publish: journal 5-"+txA+".json: "+cause)
	if err := j.Write(Entry{Seq: 3, TxID: txB}); err != nil {
		t.Fatal(err)
	}
	add("update renames", j.Update(3, txB, func(e *Entry) { e.TxID = txC }), "publish: journal: update may not change seq or txid")

	list := func(name, body string) error {
		j := Journal{Dir: t.TempDir()}
		if err := os.WriteFile(filepath.Join(j.Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := j.List()
		return err
	}
	add("list, stray file", list("notes.json", "{}"), "publish: journal: notes.json is not <seq>-<txid>.json")
	add("list, bad seq", list("x-"+txA+".json", "{}"), "publish: journal: x-"+txA+".json: "+cause)
	add("list, name and content disagree", list("5-"+txB+".json", `{"seq":6,"txid":"`+txB+`"}`),
		"publish: journal: 5-"+txB+".json names seq 5 txid "+txB+" but holds seq 6 txid "+txB)

	for _, r := range rows {
		r.check(t)
	}
}

// Every error Submit, Status and Ping return, the note a verdict that did
// not arrive leaves, and what reason quotes from a failure body.
func TestArcadeTextsAreFrozen(t *testing.T) {
	ctx := context.Background()
	var rows []frozen
	add := func(name string, err error, want string) { rows = append(rows, frozen{name, err, want}) }
	answer := func(status int, body string) func(http.ResponseWriter, string) {
		return func(w http.ResponseWriter, _ string) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	other := strings.Repeat("ab", 32)
	spentBy := strings.Repeat("de", 32)
	spentByToo := strings.Repeat("ef", 32)

	add("nil transaction", (&Arcade{Base: "http://arcade.invalid"}).Submit(ctx, nil), "publish: nil transaction")
	add("no EF encoding", (&Arcade{Base: "http://arcade.invalid"}).Submit(ctx, unencodable()), "publish: ef: "+cause)
	add("transport", (&Arcade{Base: closedURL(t)}).Submit(ctx, efTx(t)), "publish: arcade: "+cause)

	for _, row := range []struct {
		name string
		post func(http.ResponseWriter, string)
		// want is completed with the submitted txid wherever %[1]s appears.
		want string
	}{
		{"acknowledged another txid", answer(http.StatusAccepted, `{"txid":"`+other+`","txStatus":"SEEN_ON_NETWORK"}`),
			"publish: arcade acknowledged " + other + ", we sent %[1]s"},
		{"refused with reason and competitors", func(w http.ResponseWriter, txid string) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"txid":%q,"txStatus":"REJECTED","extraInfo":"input already spent","competingTxs":[%q,%q]}`, txid, spentBy, spentByToo)
		}, "publish: arcade refused %[1]s: REJECTED: input already spent (competing " + spentBy + ", " + spentByToo + ")"},
		{"refused bare", accepted("DOUBLE_SPEND_ATTEMPTED"), "publish: arcade refused %[1]s: DOUBLE_SPEND_ATTEMPTED"},
		{"answer does not parse", answer(http.StatusAccepted, "not json"), "publish: arcade answered 202 with a body that does not parse: " + cause},
		{"policy refusal", answer(http.StatusBadRequest, `{"error":"validation","reason":"non-final transaction"}`),
			"publish: arcade answered 400: non-final transaction"},
		{"failure, not retried", answer(http.StatusInternalServerError, `{"error":"failed to submit"}`),
			"publish: arcade answered 500: failed to submit"},
	} {
		tx := efTx(t)
		a, _ := arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){row.post}, statuses: []string{"SEEN_ON_NETWORK"}}, tx)
		want := row.want
		if strings.Contains(want, "%[1]s") {
			want = fmt.Sprintf(want, tx.TxID().String())
		}
		add(row.name, a.Submit(ctx, tx), want)
	}

	// The network's verdict, read after arcade took the transaction.
	tx := efTx(t)
	txid := tx.TxID().String()
	a, _ := arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: []string{"DOUBLE_SPEND_ATTEMPTED"}, extra: "competing spend"}, tx)
	add("the network refused", a.Submit(ctx, tx), "publish: the network refused "+txid+": DOUBLE_SPEND_ATTEMPTED: competing spend")

	// No verdict inside the bound is a note, not an error.
	tx = efTx(t)
	txid = tx.TxID().String()
	a, notes := arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){accepted("RECEIVED")}, statuses: []string{"SENT_TO_NETWORK"}}, tx)
	if err := a.Submit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if want := "arcade holds " + txid + " as SENT_TO_NETWORK; the network's verdict had not arrived after 300ms"; len(*notes) != 1 || (*notes)[0] != want {
		t.Errorf("verdict note %q, frozen as %q", *notes, want)
	}

	// A status read that never succeeds leaves the note naming RECEIVED,
	// which is all a 202 says, whatever txStatus the POST answer carried.
	for _, posted := range []string{"RECEIVED", "QUEUED"} {
		tx := efTx(t)
		txid := tx.TxID().String()
		var gets atomic.Int32
		blind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/tx" {
				accepted(posted)(w, txid)
				return
			}
			gets.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"status unavailable"}`))
		}))
		t.Cleanup(blind.Close)
		var held []string
		a := &Arcade{Base: blind.URL, Verdict: 300 * time.Millisecond, Poll: 20 * time.Millisecond,
			Note: func(format string, args ...any) { held = append(held, fmt.Sprintf(format, args...)) }}
		if err := a.Submit(ctx, tx); err != nil {
			t.Errorf("posted %s, no status read: %v, want nil", posted, err)
		}
		if gets.Load() == 0 {
			t.Errorf("posted %s: the status endpoint was never asked", posted)
		}
		if want := "arcade holds " + txid + " as RECEIVED; the network's verdict had not arrived after 300ms"; len(held) != 1 || held[0] != want {
			t.Errorf("posted %s: verdict note %q, frozen as %q", posted, held, want)
		}
	}

	// Status and Ping, against a server whose answers are set per path.
	unknown := strings.Repeat("cd", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tx/" + unknown:
			w.WriteHeader(http.StatusNotFound)
		case "/tx/" + txA:
			_, _ = w.Write([]byte("not json"))
		case "/tx/" + txB:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"detail":"upstream down"}`))
		case "/policy":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing bearer token"}`))
		}
	}))
	t.Cleanup(srv.Close)
	s := &Arcade{Base: srv.URL}
	_, err := s.Status(ctx, unknown)
	add("status, unknown", err, "arcade: transaction not known: "+unknown)
	_, err = s.Status(ctx, txA)
	add("status does not parse", err, "arcade: status for "+txA+" does not parse: "+cause)
	_, err = s.Status(ctx, txB)
	add("status failure", err, "arcade: status for "+txB+" answered 502: upstream down")
	_, err = (&Arcade{Base: closedURL(t)}).Status(ctx, txA)
	add("status transport", err, "arcade: "+cause)
	add("policy", s.Ping(ctx), "policy answered 401: missing bearer token")

	for _, r := range rows {
		r.check(t)
	}

	// What reason quotes from a failure body: arcade's reason, then detail,
	// then error, else the body itself, trimmed and cut at 200 bytes.
	for body, want := range map[string]string{
		`{"error":"e","reason":"r","detail":"d"}`: "r",
		`{"error":"e","detail":"d"}`:              "d",
		`{"error":"e"}`:                           "e",
		`{}`:                                      "{}",
		"  service unavailable \n":                "service unavailable",
		strings.Repeat("x", 200):                  strings.Repeat("x", 200),
		strings.Repeat("x", 201):                  strings.Repeat("x", 200) + "…",
	} {
		if got := reason([]byte(body)); got != want {
			t.Errorf("reason(%.20q) = %q, frozen as %q", body, got, want)
		}
	}
}

// Every error the object leg returns, in the order Submit can reach them.
func TestFacadeTextsAreFrozen(t *testing.T) {
	ctx := context.Background()
	const topic = "tm_example"
	beef := atomicBEEF(t)
	var rows []frozen
	add := func(name string, err error, want string) { rows = append(rows, frozen{name, err, want}) }
	submit := func(status int, body string) error {
		srv, _ := facadeServer(t, status, body)
		_, err := (&Facade{Base: srv.URL}).Submit(ctx, topic, beef)
		return err
	}
	long := strings.Repeat("y", 301)

	_, err := (&Facade{Base: "http://facade.invalid"}).Submit(ctx, topic, []byte("not a beef"))
	add("not a BEEF", err, "publish: body is not a BEEF")
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a, tm_b", beef)
	add("two topics", err, `publish: topic "tm_a, tm_b" must be one name with no separators`)
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "", beef)
	add("an empty topic", err, `publish: topic "" must be one name with no separators`)
	// One row per refused character, so dropping any one of them from the
	// set fails here.
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a,tm_b", beef)
	add("a comma", err, `publish: topic "tm_a,tm_b" must be one name with no separators`)
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a tm_b", beef)
	add("a space", err, `publish: topic "tm_a tm_b" must be one name with no separators`)
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a\ttm_b", beef)
	add("a tab", err, `publish: topic "tm_a\ttm_b" must be one name with no separators`)
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a\rtm_b", beef)
	add("a CR", err, `publish: topic "tm_a\rtm_b" must be one name with no separators`)
	_, err = (&Facade{Base: "http://facade.invalid"}).Submit(ctx, "tm_a\ntm_b", beef)
	add("an LF", err, `publish: topic "tm_a\ntm_b" must be one name with no separators`)
	_, err = (&Facade{Base: closedURL(t)}).Submit(ctx, topic, beef)
	add("transport", err, "publish: facade: "+cause)

	// A body cut off before its declared length fails the read.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nabc")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(cut.Close)
	_, err = (&Facade{Base: cut.URL}).Submit(ctx, topic, beef)
	add("read", err, "publish: facade: read: "+cause)

	add("401", submit(http.StatusUnauthorized, `{"error":"no"}`), `publish: facade refused with 401 Unauthorized: {"error":"no"}`)
	add("403", submit(http.StatusForbidden, `{"error":"no"}`), `publish: facade refused with 403 Forbidden: {"error":"no"}`)
	add("502", submit(http.StatusBadGateway, `{"error":"no"}`), `publish: facade failed with 502 Bad Gateway: {"error":"no"}`)
	add("404", submit(http.StatusNotFound, `{"error":"no"}`), `publish: facade answered 404 Not Found: {"error":"no"}`)
	add("404, body of 300 bytes whole", submit(http.StatusNotFound, long[:300]), "publish: facade answered 404 Not Found: "+long[:300])
	add("404, body cut at 300 bytes", submit(http.StatusNotFound, long), "publish: facade answered 404 Not Found: "+long[:300]+"...")
	add("200 over the bound", submit(http.StatusOK, strings.Repeat("z", maxSteak+1)), "publish: facade: publish: response body exceeds the bound (1048576 bytes)")
	add("wrapped STEAK without the topic", submit(http.StatusOK, `{"STEAK":{"tm_other":{}}}`),
		`publish: STEAK has no entry for topic "tm_example": {"STEAK":{"tm_other":{}}}`)
	add("bare STEAK without the topic", submit(http.StatusOK, `{"tm_other":{}}`),
		`publish: STEAK has no entry for topic "tm_example": {"tm_other":{}}`)
	add("not a STEAK", submit(http.StatusOK, "not json"), "publish: facade answered 200 with a body that is not a STEAK: "+cause+": not json")
	add("STEAK entry of the wrong shape", submit(http.StatusOK, `{"tm_example":5}`), `publish: STEAK entry for "tm_example": `+cause)

	for _, r := range rows {
		r.check(t)
	}
}

// shortConn reports a write of fewer bytes than it was given, with no error.
type shortConn struct{ net.Conn }

func (c shortConn) Write([]byte) (int, error) { return 3, nil }

// Every error the TCP ingress and the RPC settler return, and each leg's
// name.
func TestSettlerTextsAreFrozen(t *testing.T) {
	ctx := context.Background()
	const addr = "192.0.2.1:8725"
	tx := signedTx(t)
	txid := tx.TxID().String()
	ef, err := tx.EF()
	if err != nil {
		t.Fatal(err)
	}
	var rows []frozen
	add := func(name string, err error, want string) { rows = append(rows, frozen{name, err, want}) }
	ingress := func(conn func() net.Conn, dialErr error) *TCPIngress {
		return &TCPIngress{Addr: addr, dial: func(context.Context, string) (net.Conn, error) {
			if dialErr != nil {
				return nil, dialErr
			}
			return conn(), nil
		}}
	}
	pipe := func() net.Conn {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		return a
	}

	add("tcp, nil transaction", ingress(pipe, nil).Submit(ctx, nil), "publish: nil transaction")
	add("tcp, no EF encoding", ingress(pipe, nil).Submit(ctx, unencodable()), "publish: ef: "+cause)
	add("tcp, dial", ingress(nil, errors.New("connection refused")).Submit(ctx, tx), "publish: dial "+addr+": connection refused")
	add("tcp, write", ingress(func() net.Conn { return &failingConn{Conn: pipe()} }, nil).Submit(ctx, tx),
		fmt.Sprintf("publish: write 3 of %d EF bytes to %s: peer went away", len(ef), addr))
	add("tcp, short write", ingress(func() net.Conn { return shortConn{pipe()} }, nil).Submit(ctx, tx),
		fmt.Sprintf("publish: short write to %s: 3 of %d EF bytes", addr, len(ef)))

	msg := "TX rejected: bad-txns-inputs-missingorspent"
	refusing, _ := rpcStub(t, func(string, []any) (any, *string) { return nil, &msg })
	other := strings.Repeat("00", 32)
	acking, _ := rpcStub(t, func(string, []any) (any, *string) { return other, nil })
	rpc := func(url string) *RPCSettler { return &RPCSettler{RPC: &nodeapi.RPC{URL: url, ID: "app-under-test"}} }
	add("rpc, nil transaction", rpc(refusing.URL).Submit(ctx, nil), "publish: nil transaction")
	add("rpc, the node's refusal", rpc(refusing.URL).Submit(ctx, tx), "publish: sendrawtransaction: sendrawtransaction: rpc error -25: "+msg)
	add("rpc, acknowledged another txid", rpc(acking.URL).Submit(ctx, tx), "publish: sendrawtransaction acknowledged "+other+", we sent "+txid)
	noID := (&RPCSettler{RPC: &nodeapi.RPC{URL: refusing.URL}}).Submit(ctx, tx)
	add("rpc, no request id", noID, "publish: sendrawtransaction: "+cause)

	for _, r := range rows {
		r.check(t)
	}

	// The node client's error stays reachable through the settler's words:
	// a caller tells a missing id or an HTTP status apart with errors.Is and
	// errors.As, not by reading the text.
	if !errors.Is(noID, nodeapi.ErrNoID) {
		t.Errorf("rpc with no id: %v does not wrap nodeapi.ErrNoID", noID)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)
	if err := rpc(down.URL).Submit(ctx, tx); !nodeapi.IsHTTP(err, http.StatusBadGateway) {
		t.Errorf("rpc answering 502: %v does not wrap a *nodeapi.HTTPError with status 502", err)
	}

	// Each leg's name is how a journal line says which leg it was.
	for got, want := range map[string]string{
		(&Arcade{Base: "https://arcade.example"}).Name():                         "arcade:https://arcade.example",
		(&TCPIngress{Addr: addr}).Name():                                         "tcp:" + addr,
		(&RPCSettler{RPC: &nodeapi.RPC{URL: "http://node.example:8332"}}).Name(): "rpc:http://node.example:8332",
	} {
		if got != want {
			t.Errorf("leg name %q, frozen as %q", got, want)
		}
	}
}

// Every transaction status the code names, and statuses it does not, against
// the three decisions Submit and the proof path take. An ARC status added to
// or dropped from a list moves a transaction between "wait", "go on" and
// "stop", and none of the flow tests covers every status.
func TestArcadeDecisionsAreFrozen(t *testing.T) {
	for _, row := range []struct {
		status                   string
		refused, accepted, mined bool
	}{
		{"REJECTED", true, false, false},
		{"DOUBLE_SPEND_ATTEMPTED", true, false, false},
		{"ACCEPTED_BY_NETWORK", false, true, false},
		{"SEEN_ON_NETWORK", false, true, false},
		{"SEEN_MULTIPLE_NODES", false, true, false},
		{"STUMP_PROCESSING", false, true, false},
		{"MINED", false, true, true},
		{"IMMUTABLE", false, true, true},
		// Not named: arcade holds these, and the network has not spoken.
		{"RECEIVED", false, false, false},
		{"SENT_TO_NETWORK", false, false, false},
		{"QUEUED", false, false, false},
		{"", false, false, false},
		{"mined", false, false, false},
	} {
		st := &ArcadeStatus{TxStatus: row.status, MerklePath: "fe"}
		if st.Refused() != row.refused || st.Accepted() != row.accepted || st.Mined() != row.mined {
			t.Errorf("%q: refused=%v accepted=%v mined=%v, frozen as %v %v %v",
				row.status, st.Refused(), st.Accepted(), st.Mined(), row.refused, row.accepted, row.mined)
		}
		// Mined needs the path as well: there is nothing to prove from
		// without one.
		st.MerklePath = ""
		if st.Mined() {
			t.Errorf("%q with no merkle path reported as mined", row.status)
		}
	}
}

// Backpressure is retried three times and no more: four POSTs, a note
// before each retry, then the fourth 503 is the error.
func TestArcadeGivesUpAfterThreeRetries(t *testing.T) {
	tx := efTx(t)
	busy := func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service overloaded, retry shortly"}`))
	}
	f := &fakeArcade{post: []func(http.ResponseWriter, string){busy}, statuses: []string{"SEEN_ON_NETWORK"}}
	a, notes := arcadeFor(t, f, tx)
	err := a.Submit(context.Background(), tx)
	if want := "publish: arcade answered 503: service overloaded, retry shortly"; err == nil || err.Error() != want {
		t.Fatalf("err %v, want %q", err, want)
	}
	if len(f.bodies) != 4 {
		t.Fatalf("%d POSTs under backpressure, want 4", len(f.bodies))
	}
	want := strings.Repeat("arcade is under backpressure; retrying in 1s|", 3)
	if got := strings.Join(*notes, "|") + "|"; got != want {
		t.Fatalf("notes %q, want three backoff notes", *notes)
	}
}

// The wait a 503 asks for is honoured from one to ten seconds; anything
// else, or no Retry-After at all, waits one second. The note says which, and
// each row cancels from inside the note so that no row sleeps.
func TestArcadeBackoffIsFrozen(t *testing.T) {
	for _, row := range []struct{ header, wait string }{
		{"", "1s"},
		{"0", "1s"},
		{"1", "1s"},
		{"2", "2s"},
		{"10", "10s"},
		{"11", "1s"},
		{"-3", "1s"},
		{"soon", "1s"},
	} {
		tx := efTx(t)
		busy := func(w http.ResponseWriter, _ string) {
			if row.header != "" {
				w.Header().Set("Retry-After", row.header)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		a, notes := arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){busy}, statuses: []string{"SEEN_ON_NETWORK"}}, tx)
		ctx, cancel := context.WithCancel(context.Background())
		record := a.Note
		a.Note = func(format string, args ...any) {
			record(format, args...)
			cancel()
		}
		err := a.Submit(ctx, tx)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Retry-After %q: err %v, want the wait cancelled", row.header, err)
		}
		if want := "arcade is under backpressure; retrying in " + row.wait; len(*notes) != 1 || (*notes)[0] != want {
			t.Errorf("Retry-After %q: notes %q, frozen as %q", row.header, *notes, want)
		}
	}
}

// Arcade's answer to a POST is read up to 64 KiB. An answer of exactly the
// bound is read whole; one byte over is cut, and a cut JSON body does not
// parse.
func TestArcadeAnswerBoundIsFrozen(t *testing.T) {
	answer := func(size int) func(http.ResponseWriter, string) {
		head, tail := `{"txStatus":"SEEN_ON_NETWORK","extraInfo":"`, `"}`
		body := head + strings.Repeat("p", size-len(head)-len(tail)) + tail
		return func(w http.ResponseWriter, _ string) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(body))
		}
	}
	tx := efTx(t)
	a, _ := arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){answer(64 << 10)}, statuses: []string{"SEEN_ON_NETWORK"}}, tx)
	if err := a.Submit(context.Background(), tx); err != nil {
		t.Errorf("an answer of exactly 64 KiB: %v, want it read whole", err)
	}
	tx = efTx(t)
	a, _ = arcadeFor(t, &fakeArcade{post: []func(http.ResponseWriter, string){answer(64<<10 + 1)}, statuses: []string{"SEEN_ON_NETWORK"}}, tx)
	frozen{"an answer one byte over 64 KiB", a.Submit(context.Background(), tx),
		"publish: arcade answered 202 with a body that does not parse: " + cause}.check(t)
}

// A caller that sets no bound gets these. DefaultVerdict is exported, so a
// caller may have sized its own timeouts around it.
func TestPublishDefaultsAreFrozen(t *testing.T) {
	if DefaultVerdict != 15*time.Second {
		t.Errorf("DefaultVerdict %s, frozen as 15s", DefaultVerdict)
	}
}

// deadlineConn accepts every write whole and keeps the write deadline it
// was given.
type deadlineConn struct {
	net.Conn
	at *time.Time
}

func (c deadlineConn) SetWriteDeadline(t time.Time) error { *c.at = t; return nil }

func (c deadlineConn) Write(b []byte) (int, error) { return len(b), nil }

// The TCP ingress writes under WriteTimeout, ten seconds when unset, and
// under the caller's deadline when that comes first.
func TestIngressWriteDeadlineIsFrozen(t *testing.T) {
	tx := signedTx(t)
	for _, row := range []struct {
		name    string
		timeout time.Duration
		ctx     time.Duration // 0: no deadline on the context
		want    time.Duration
	}{
		{"unset", 0, 0, 10 * time.Second},
		{"set", 3 * time.Second, 0, 3 * time.Second},
		{"context first", 0, 2 * time.Second, 2 * time.Second},
		{"context later", 3 * time.Second, time.Minute, 3 * time.Second},
	} {
		var at time.Time
		in := &TCPIngress{Addr: "192.0.2.1:8725", WriteTimeout: row.timeout, dial: func(context.Context, string) (net.Conn, error) {
			a, b := net.Pipe()
			t.Cleanup(func() { _ = b.Close() })
			return deadlineConn{Conn: a, at: &at}, nil
		}}
		before := time.Now()
		ctx, cancel := context.Background(), context.CancelFunc(func() {})
		if row.ctx > 0 {
			ctx, cancel = context.WithTimeout(ctx, row.ctx)
		}
		err := in.Submit(ctx, tx)
		after := time.Now()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if at.Before(before.Add(row.want)) || at.After(after.Add(row.want)) {
			t.Errorf("%s: write deadline %s after the call, frozen as %s", row.name, at.Sub(before), row.want)
		}
	}
}
