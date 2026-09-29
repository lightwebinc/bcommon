package publish

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// STEAK bodies in the two forms an overlay server answers with: the
// Go overlay server's wrapper and the TypeScript host's bare map. Shaped
// from that description, not captured from a host.
const (
	steakWrapped   = `{"STEAK":{"tm_example":{"outputsToAdmit":[0],"coinsToRetain":[],"coinsRemoved":[],"ancillaryTxids":[]}}}`
	steakBare      = `{"tm_example":{"outputsToAdmit":[0,2],"coinsToRetain":[1]}}`
	steakDuplicate = `{"STEAK":{"tm_example":{"outputsToAdmit":[],"coinsToRetain":[],"coinsRemoved":[],"ancillaryTxids":[]}}}`
	steakOtherOnly = `{"STEAK":{"tm_other":{"outputsToAdmit":[0],"coinsToRetain":[]}}}`
)

type capture struct {
	mu      sync.Mutex
	count   int
	topics  []string
	ctype   string
	body    []byte
	headers http.Header
}

func facadeServer(t *testing.T, status int, reply string) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.URL.Path != "/submit" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		c.count++
		c.topics = r.Header.Values("x-topics")
		c.ctype = r.Header.Get("Content-Type")
		c.headers = r.Header.Clone()
		c.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func atomicBEEF(t *testing.T) []byte {
	t.Helper()
	beef, err := signedTx(t).AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	if !IsBEEF(beef) || binary.LittleEndian.Uint32(beef[:4]) != transaction.ATOMIC_BEEF {
		t.Fatal("test BEEF does not start with the atomic marker")
	}
	return beef
}

func TestFacadeSendsOneTopicAndDecodesTheWrappedSTEAK(t *testing.T) {
	srv, c := facadeServer(t, http.StatusOK, steakWrapped)
	beef := atomicBEEF(t)
	res, err := (&Facade{Base: srv.URL + "/"}).Submit(context.Background(), "tm_example", beef)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.topics) != 1 || c.topics[0] != "tm_example" {
		t.Fatalf("x-topics %q, want exactly one header valued tm_example (no JSON array, no spaces)", c.topics)
	}
	if c.ctype != "application/octet-stream" {
		t.Fatalf("content-type %q", c.ctype)
	}
	if !bytes.Equal(c.body, beef) {
		t.Fatal("body is not the raw BEEF bytes")
	}
	if len(res.Admitted) != 1 || res.Admitted[0] != 0 || res.Duplicate || len(res.Retained) != 0 {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(res.Raw, []byte(steakWrapped)) {
		t.Fatal("Raw is not the response body")
	}
}

func TestFacadeDecodesTheBareSTEAK(t *testing.T) {
	srv, _ := facadeServer(t, http.StatusOK, steakBare)
	res, err := (&Facade{Base: srv.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Admitted) != 2 || res.Admitted[1] != 2 || len(res.Retained) != 1 || res.Retained[0] != 1 || res.Duplicate {
		t.Fatalf("result %+v", res)
	}
}

func TestFacadeDuplicateIsAResultNotAnError(t *testing.T) {
	srv, _ := facadeServer(t, http.StatusOK, steakDuplicate)
	res, err := (&Facade{Base: srv.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
	if err != nil {
		t.Fatalf("a 200 with nothing admitted is the engine's duplicate answer, got err %v", err)
	}
	if !res.Duplicate || len(res.Admitted) != 0 {
		t.Fatalf("result %+v, want Duplicate", res)
	}
}

func TestFacadeRefusesABodyThatIsNotBEEF(t *testing.T) {
	srv, c := facadeServer(t, http.StatusOK, steakWrapped)
	f := &Facade{Base: srv.URL}
	ef, err := signedTx(t).EF()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"ef bytes":     ef,
		"raw tx":       signedTx(t).Bytes(),
		"framed":       {0xE3, 0xE1, 0xF3, 0xE8, 0, 0},
		"brc149 magic": {0xBE, 0xEF, 0, 0},
		"short":        {0x01, 0x01},
		"empty":        nil,
	} {
		if _, err := f.Submit(context.Background(), "tm_example", body); !errors.Is(err, ErrNotBEEF) {
			t.Errorf("%s: err %v, want ErrNotBEEF", name, err)
		}
	}
	if c.count != 0 {
		t.Fatalf("%d requests reached the facade for non-BEEF bodies", c.count)
	}
	// Every SDK marker is accepted at the gate.
	for _, m := range []uint32{transaction.BEEF_V1, transaction.BEEF_V2, transaction.ATOMIC_BEEF} {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b, m)
		if !IsBEEF(b) {
			t.Errorf("marker %08x not recognised", m)
		}
	}
}

func TestFacadeRefusesABadTopicBeforeSending(t *testing.T) {
	srv, c := facadeServer(t, http.StatusOK, steakWrapped)
	f := &Facade{Base: srv.URL}
	beef := atomicBEEF(t)
	for _, topic := range []string{"", "tm_a, tm_b", "tm_a,tm_b", "tm example", "tm_example\n"} {
		if _, err := f.Submit(context.Background(), topic, beef); err == nil {
			t.Errorf("topic %q accepted", topic)
		}
	}
	if c.count != 0 {
		t.Fatalf("%d requests sent with a bad topic", c.count)
	}
}

func TestFacadeStatusesAreNamedErrors(t *testing.T) {
	for _, code := range []int{401, 403, 404, 500, 502, 503} {
		srv, _ := facadeServer(t, code, `{"error":"no"}`)
		_, err := (&Facade{Base: srv.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
		if err == nil || !strings.Contains(err.Error(), http.StatusText(code)) {
			t.Errorf("status %d: err %v, want it named", code, err)
		}
	}
}

func TestFacadeSTEAKWithoutOurTopicIsAnError(t *testing.T) {
	srv, _ := facadeServer(t, http.StatusOK, steakOtherOnly)
	_, err := (&Facade{Base: srv.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
	if err == nil || !strings.Contains(err.Error(), "tm_example") {
		t.Fatalf("err %v, want a missing-topic error", err)
	}
	srv2, _ := facadeServer(t, http.StatusOK, `not json`)
	if _, err := (&Facade{Base: srv2.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t)); err == nil {
		t.Fatal("a 200 that is not a STEAK must be an error")
	}
}

// A body one byte over the bound is refused rather than truncated. A clipped
// STEAK is the dangerous shape: the prefix carries the topic and an admission
// list, so a truncated answer can report an admission the host never made.
func TestFacadeBodyOverTheBoundIsRefusedNotTruncated(t *testing.T) {
	steak := func(size int) string {
		head, tail := `{"STEAK":{"tm_example":{"outputsToAdmit":[0],"coinsToRetain":[],"pad":"`, `"}}}`
		pad := size - len(head) - len(tail)
		if pad < 0 {
			t.Fatalf("a STEAK cannot be shaped to %d bytes", size)
		}
		return head + strings.Repeat("a", pad) + tail
	}

	atBound, _ := facadeServer(t, http.StatusOK, steak(maxSteak))
	res, err := (&Facade{Base: atBound.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
	if err != nil {
		t.Fatalf("a body of exactly %d bytes was refused: %v", maxSteak, err)
	}
	if len(res.Admitted) != 1 || res.Admitted[0] != 0 {
		t.Fatalf("result %+v", res)
	}

	over, _ := facadeServer(t, http.StatusOK, steak(maxSteak+1))
	_, err = (&Facade{Base: over.URL}).Submit(context.Background(), "tm_example", atomicBEEF(t))
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("one byte over the bound: %v, want ErrBodyTooLarge", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxSteak)) {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}
}

// The default client takes no proxy. The object leg posts the publisher's
// BEEF and believes the answer about what was admitted; a host named only by
// HTTP_PROXY is in no configuration and in no verbose trace. A caller may
// still supply its own client.
func TestFacadeDefaultClientTakesNoProxyAndTheCallersClientWins(t *testing.T) {
	tr, ok := (&Facade{Base: "https://facade.example"}).httpClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the default client has no transport of its own, so it follows HTTP_PROXY from the environment")
	}
	if tr.Proxy != nil {
		t.Fatal("the default transport asks a proxy function where to send a submission")
	}
	mine := &http.Client{Timeout: time.Second}
	if got := (&Facade{Base: "https://facade.example", HTTP: mine}).httpClient(); got != mine {
		t.Fatal("a caller-supplied client was replaced")
	}
}
