package lookup

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/hostset"
)

// fixture reads a response an overlay host actually sent, vendored verbatim
// under testdata/fixtures. lookup_output_list is an opaque sample: one
// output-list answer from a host on a test chain, captured with limit 1. The
// tests read its shape and parse its BEEF, and nothing depends on what its
// transaction holds.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheHostsOwnAnswerDecodesAndItsBeefParses(t *testing.T) {
	var a Answer
	if err := json.Unmarshal(fixture(t, "lookup_output_list"), &a); err != nil {
		t.Fatal(err)
	}
	if a.Type != TypeOutputList {
		t.Fatalf("type = %q", a.Type)
	}
	if len(a.Outputs) != 1 {
		t.Fatalf("%d outputs, want exactly 1 (the fixture was captured with limit 1)", len(a.Outputs))
	}
	out := a.Outputs[0]
	if len(out.Beef) == 0 {
		t.Fatal("no beef bytes")
	}
	beef, tx, _, err := transaction.ParseBeef(out.Beef)
	if err != nil {
		t.Fatalf("go-sdk cannot parse the host's beef: %v", err)
	}
	if beef == nil && tx == nil {
		t.Fatal("ParseBeef returned nothing")
	}
	if tx != nil && int(out.OutputIndex) >= len(tx.Outputs) {
		t.Fatalf("outputIndex %d is beyond the %d outputs of the transaction", out.OutputIndex, len(tx.Outputs))
	}
}

// The live host serialises beef as a number array; the SDK would have
// produced base64. Both must decode to the same bytes.
func TestBeefAcceptsANumberArrayAndBase64(t *testing.T) {
	want := []byte{2, 0, 190, 239}
	var fromArray, fromString Output
	if err := json.Unmarshal([]byte(`{"beef":[2,0,190,239],"outputIndex":3,"context":[1,2]}`), &fromArray); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"beef":"AgC+7w==","outputIndex":3,"context":"AQI="}`), &fromString); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Output{"array": fromArray, "base64": fromString} {
		if !bytes.Equal(o.Beef, want) || o.OutputIndex != 3 || !bytes.Equal(o.Context, []byte{1, 2}) {
			t.Errorf("%s: %+v", name, o)
		}
	}
	var noContext Output
	if err := json.Unmarshal([]byte(`{"beef":[],"outputIndex":0}`), &noContext); err != nil || noContext.Context != nil {
		t.Errorf("absent context: %v %+v", err, noContext)
	}
}

// An answer that names no type is refused like any other type: the type is
// what says the outputs are an output-list at all.
func TestQueryRefusesAnAnswerWithNoType(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"outputs":[]}`))
	})
	_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_example", Query: map[string]any{}})
	u, _ := url.Parse(srv.URL)
	if want := fmt.Sprintf(`lookup: answer is not an output-list: %s@%s answered type ""`, u.Hostname(), u.Host); !errors.Is(err, ErrNotOutputList) || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A null beef or context is absent, not malformed.
func TestNullBeefAndContextDecodeToNil(t *testing.T) {
	var o Output
	if err := json.Unmarshal([]byte(`{"beef":null,"outputIndex":0,"context":null}`), &o); err != nil {
		t.Fatal(err)
	}
	if o.Beef != nil || o.Context != nil {
		t.Fatalf("beef %v, context %v, want both nil", o.Beef, o.Context)
	}
}

func TestBeefRefusesNonBytes(t *testing.T) {
	for _, bad := range []string{`[256]`, `[-1]`, `[1.5]`, `["a"]`, `{"x":1}`, `7`, `"not base64!"`} {
		var o Output
		if err := json.Unmarshal([]byte(`{"beef":`+bad+`,"outputIndex":0}`), &o); err == nil {
			t.Errorf("beef %s was accepted as %v", bad, o.Beef)
		}
	}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func static(srv *httptest.Server) *hostset.Client {
	return &hostset.Client{Source: hostset.Static{Bases: []string{srv.URL}}}
}

func TestQueryPostsTheQuestionAndDecodesTheAnswer(t *testing.T) {
	var gotBody atomic.Value
	var gotPath, gotType, gotAccept atomic.Value
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(b)
		gotPath.Store(r.Method + " " + r.URL.Path)
		gotType.Store(r.Header.Get("Content-Type"))
		gotAccept.Store(r.Header.Get("Accept"))
		_, _ = w.Write(fixture(t, "lookup_output_list"))
	})
	q := Question{Service: "ls_example", Query: map[string]any{"topic": "tm_example", "limit": 1}}
	answers, err := Query(context.Background(), static(srv), srv.URL+"/", q)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath.Load() != "POST /lookup" || gotType.Load() != "application/json" || gotAccept.Load() != "application/json" {
		t.Errorf("request was %v, Content-Type %v, Accept %v", gotPath.Load(), gotType.Load(), gotAccept.Load())
	}
	if want := `{"service":"ls_example","query":{"limit":1,"topic":"tm_example"}}`; string(gotBody.Load().([]byte)) != want {
		t.Errorf("body on the wire was %s", gotBody.Load())
	}
	if len(answers) != 1 || len(answers[0].Answer.Outputs) != 1 || answers[0].Host.Base != srv.URL {
		t.Fatalf("answers = %+v", answers)
	}
}

func TestQueryRefusesAnythingButAnOutputList(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"freeform","result":{"anything":true}}`))
	})
	_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_other", Query: map[string]any{"all": true}})
	if !errors.Is(err, ErrNotOutputList) || !strings.Contains(err.Error(), `"freeform"`) {
		t.Fatalf("got %v", err)
	}
}

func TestQueryNamesANon200(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","description":"unknown service"}`))
	})
	_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_nope", Query: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("got %v", err)
	}
}

// Only a 200 is an answer: another 2xx is refused with its status rather
// than parsed as an empty body.
func TestQueryRefusesA2xxThatIsNot200(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_example", Query: map[string]any{}})
	want := "lookup: 127.0.0.1@" + strings.TrimPrefix(srv.URL, "http://") + " answered status 204: "
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A host whose attempt failed is skipped, not reported: hostset has already
// moved on to the next address, and the answer that counts is the one that
// came back. This is the lookup half of failover.
func TestQuerySkipsAHostWhoseAttemptFailed(t *testing.T) {
	up := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fixture(t, "lookup_output_list")) })
	down := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for _, c := range []struct{ name, first string }{{"503", down.URL}, {"closed listener", closedURL}} {
		hs := &hostset.Client{Source: hostset.Static{Bases: []string{c.first, up.URL}}, Quorum: 1}
		answers, err := Query(context.Background(), hs, "", Question{Service: "ls_example", Query: map[string]any{"limit": 1}})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(answers) != 1 || answers[0].Host.Base != up.URL {
			t.Errorf("%s: answers = %+v, want one from %s", c.name, answers, up.URL)
		}
	}
}

// Two replicas under PolicyAll come back as two HostAnswers for the caller
// to compare, which is the material the fork rule needs.
func TestQueryReturnsEveryHostUnderPolicyAll(t *testing.T) {
	one := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fixture(t, "lookup_output_list")) })
	two := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fixture(t, "lookup_output_list")) })
	hs := &hostset.Client{Source: hostset.Static{Bases: []string{one.URL, two.URL}}, Policy: hostset.PolicyAll}
	answers, err := Query(context.Background(), hs, "", Question{Service: "ls_other", Query: map[string]any{"all": true, "limit": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0].Host.Base != one.URL || answers[1].Host.Base != two.URL {
		t.Fatalf("answers = %+v", answers)
	}
}

// A base given with a trailing slash still asks /lookup, not //lookup. The
// Static source keeps the base as given, so the slash reaches Query.
func TestQueryTrimsATrailingSlashFromTheBase(t *testing.T) {
	var gotPath atomic.Value
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		_, _ = w.Write(fixture(t, "lookup_output_list"))
	})
	hs := &hostset.Client{Source: hostset.Static{Bases: []string{srv.URL + "/"}}}
	answers, err := Query(context.Background(), hs, "", Question{Service: "ls_example", Query: map[string]any{"limit": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath.Load() != "/lookup" {
		t.Fatalf("the host was asked %v, want /lookup", gotPath.Load())
	}
	if len(answers) != 1 || answers[0].Host.Base != srv.URL+"/" {
		t.Fatalf("answers = %+v", answers)
	}
}

// A host's error body is shown whole up to 200 bytes and cut, with an
// ellipsis, only past that.
func TestNon200BodyIsCutOnlyPastTwoHundredBytes(t *testing.T) {
	body := strings.Repeat("x", 201)
	for _, n := range []int{200, 201} {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(body[:n]))
		})
		_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_example", Query: map[string]any{}})
		want := "lookup: 127.0.0.1@" + strings.TrimPrefix(srv.URL, "http://") + " answered status 404: " + body[:200]
		if n > 200 {
			want += "..."
		}
		if err == nil || err.Error() != want {
			t.Errorf("%d bytes: got %v, want\n%q", n, err, want)
		}
	}
}

// An output whose beef and context are both malformed is refused for its
// beef: beef is decoded first, and the refusal names the field it stopped
// at.
func TestBeefIsDecodedBeforeContext(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[{"beef":[256],"outputIndex":0,"context":[-1]}]}`))
	})
	_, err := Query(context.Background(), static(srv), srv.URL, Question{Service: "ls_example", Query: map[string]any{}})
	want := "lookup: 127.0.0.1@" + strings.TrimPrefix(srv.URL, "http://") + ": answer is not the expected JSON: beef: element 0 is 256, not a byte"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want\n%q", err, want)
	}
}

// With no host set, Query asks the addresses the base URL's name resolves
// to. An IP literal resolves to itself, so the one host there is asked once
// and its answer comes back.
func TestQueryWithNoHostSetAsksTheBase(t *testing.T) {
	var asked atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write(fixture(t, "lookup_output_list"))
	})
	answers, err := Query(context.Background(), nil, srv.URL, Question{Service: "ls_example", Query: map[string]any{"limit": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Host.Base != srv.URL || answers[0].Host.Addr != srv.Listener.Addr().String() || len(answers[0].Answer.Outputs) != 1 {
		t.Fatalf("answers = %+v", answers)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the host was asked %d time(s), want 1", n)
	}
}

// With no host set, Query takes the first host that answers, however many
// the name resolves to. The name here resolves to two hosts, both the one
// test listener, so asking every host would ask it twice.
func TestQueryWithNoHostSetStopsAtTheFirstAnswer(t *testing.T) {
	resolveTwice(t)
	var asked atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write(fixture(t, "lookup_output_list"))
	})
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	base := "http://replicas.example:" + port
	// Control: the name does resolve to two hosts.
	if hosts, err := (hostset.DNS{}).Hosts(context.Background(), base); err != nil || len(hosts) != 2 {
		t.Fatalf("the test name resolves to %v, %v; want two hosts", hosts, err)
	}
	answers, err := Query(context.Background(), nil, base, Question{Service: "ls_example", Query: map[string]any{"limit": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Host.Name != "replicas.example" {
		t.Fatalf("answers = %+v, want the first host's alone", answers)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the hosts were asked %d time(s), want 1", n)
	}
}

// With no host set, first success follows the order the name resolves in:
// the hosts are not shuffled. The name here resolves to two addresses, each
// with a listener of its own on one port, so the address that answered shows
// the order. A shuffle would lead with the second in about half the runs.
func TestQueryWithNoHostSetTriesTheResolvedOrder(t *testing.T) {
	answer := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fixture(t, "lookup_output_list")) }
	first := serve(t, answer)
	_, port, _ := net.SplitHostPort(first.Listener.Addr().String())
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Skipf("no second loopback address at port %s: %v", port, err)
	}
	second := httptest.NewUnstartedServer(http.HandlerFunc(answer))
	_ = second.Listener.Close()
	second.Listener = l
	second.Start()
	t.Cleanup(second.Close)

	resolveTo(t, [4]byte{127, 0, 0, 1}, [4]byte{127, 0, 0, 2})
	base := "http://replicas.example:" + port
	// Control: the name resolves to both addresses, in that order.
	if hosts, err := (hostset.DNS{}).Hosts(context.Background(), base); err != nil || len(hosts) != 2 ||
		hosts[0].Addr != first.Listener.Addr().String() || hosts[1].Addr != second.Listener.Addr().String() {
		t.Fatalf("the test name resolves to %v, %v; want %s then %s", hosts, err, first.Listener.Addr(), second.Listener.Addr())
	}
	for i := 0; i < 32; i++ {
		answers, err := Query(context.Background(), nil, base, Question{Service: "ls_example", Query: map[string]any{"limit": 1}})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if len(answers) != 1 || answers[0].Host.Addr != first.Listener.Addr().String() {
			t.Fatalf("run %d: answers = %+v, want the first resolved address's alone", i, answers)
		}
	}
}

// resolveTwice answers every A question with 127.0.0.1 twice: two hosts, one
// listener.
func resolveTwice(t *testing.T) {
	t.Helper()
	resolveTo(t, [4]byte{127, 0, 0, 1}, [4]byte{127, 0, 0, 1})
}

// resolveTo replaces the process's default resolver, for the test, with one
// that answers every A question with ips, in the order given, and every other
// question with nothing. A nil host set resolves names with the default
// resolver, so this is how it is handed several hosts without a network. No
// test in this package runs in parallel, so nothing else resolves meanwhile.
func resolveTo(t *testing.T, ips ...[4]byte) {
	t.Helper()
	orig := net.DefaultResolver
	t.Cleanup(func() { net.DefaultResolver = orig })
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go answerWith(server, ips)
		return client, nil
	}}
}

// answerWith serves one DNS exchange in the stream framing a connection that
// is not a packet connection gets: a two-byte length, then the message.
func answerWith(c net.Conn, ips [][4]byte) {
	defer c.Close()
	var n [2]byte
	if _, err := io.ReadFull(c, n[:]); err != nil {
		return
	}
	q := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(c, q); err != nil || len(q) < 12 {
		return
	}
	end := 12
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	end += 5 // the root label, the type and the class
	if end > len(q) {
		return
	}
	var answers []byte
	count := byte(0)
	if binary.BigEndian.Uint16(q[end-4:]) == 1 { // A
		for _, ip := range ips {
			answers = append(answers, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, ip[0], ip[1], ip[2], ip[3])
		}
		count = byte(len(ips))
	}
	// The ID, a recursive answer with no error, the question as asked, and
	// the answers.
	msg := append([]byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, count, 0, 0, 0, 0}, q[12:end]...)
	msg = append(msg, answers...)
	_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...))
}
