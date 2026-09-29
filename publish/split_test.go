package publish

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// The structural rule as an observation: one publish, both legs, and the two
// peers see exactly what the rule says they see. The TCP peer sees one
// connection whose first bytes are neither a BEEF marker nor the framed magic,
// so its grammar locks to bare transactions; the HTTP peer sees a body that
// begins with a BEEF marker. Nothing in publish can hand either leg the
// other's bytes, and this is where that is checked rather than asserted.
func TestLegsNeverShareASocket(t *testing.T) {
	rec := listen(t)
	srv, c := facadeServer(t, http.StatusOK, steakWrapped)

	tx := signedTx(t)
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}

	ingress := &TCPIngress{Addr: rec.ln.Addr().String()}
	facade := &Facade{Base: srv.URL}

	if err := ingress.Submit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := facade.Submit(context.Background(), "tm_example", beef); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	conns := rec.seen()
	if len(conns) != 1 {
		t.Fatalf("the ingress saw %d connections, want exactly 1", len(conns))
	}
	first := conns[0]
	if len(first) < 4 {
		t.Fatalf("ingress got %d bytes", len(first))
	}
	if IsBEEF(first) {
		t.Fatal("a BEEF marker reached the EF socket")
	}
	if bytes.HasPrefix(first, []byte{0xE3, 0xE1, 0xF3, 0xE8}) {
		t.Fatal("framed magic reached the EF socket")
	}
	if bytes.HasPrefix(first, []byte{0xBE, 0xEF}) {
		t.Fatal("BRC-149 magic reached the EF socket")
	}
	ef, _ := tx.EF()
	if !bytes.Equal(first, ef) {
		t.Fatal("the EF socket did not receive the EF bytes verbatim")
	}

	if c.count != 1 {
		t.Fatalf("the facade saw %d requests, want exactly 1", c.count)
	}
	if !IsBEEF(c.body) || binary.LittleEndian.Uint32(c.body[:4]) != transaction.ATOMIC_BEEF {
		t.Fatal("the facade body does not begin with the atomic BEEF marker")
	}
	if bytes.HasPrefix(c.body, ef[:8]) {
		t.Fatal("EF bytes reached the facade")
	}
}
