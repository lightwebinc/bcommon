package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const spendTx = "1111111111111111111111111111111111111111111111111111111111111111"
const spendBy = "2222222222222222222222222222222222222222222222222222222222222222"

// utxoServer answers a UTXO view of n outputs: output 0 spent, the rest
// unspent, except output 3, whose status is not evidence. It counts reads.
func utxoServer(t *testing.T, n int, reads *atomic.Int32) *Asset {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		var b strings.Builder
		b.WriteByte('[')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			switch i {
			case 0:
				fmt.Fprintf(&b, `{"txid":%q,"vout":0,"status":"SPENT","spendingData":{"txId":%q}}`, spendTx, spendBy)
			case 3:
				fmt.Fprintf(&b, `{"txid":%q,"vout":3,"status":"FROZEN"}`, spendTx)
			default:
				fmt.Fprintf(&b, `{"txid":%q,"vout":%d,"status":"OK"}`, spendTx, i)
			}
		}
		b.WriteByte(']')
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	return &Asset{Base: srv.URL}
}

func TestSpendCacheAnswersAllOutputsFromOneRead(t *testing.T) {
	var reads atomic.Int32
	c := &SpendCache{Source: utxoServer(t, 1024, &reads), TTL: time.Hour}
	ctx := context.Background()
	if by, err := c.Spender(ctx, spendTx, 0); err != nil || by != spendBy {
		t.Fatalf("output 0: %q, %v", by, err)
	}
	for v := uint32(1); v < 1024; v++ {
		by, err := c.Spender(ctx, spendTx, v)
		if v == 3 {
			if !errors.Is(err, ErrSpendUnknown) {
				t.Fatalf("output 3: %v", err)
			}
			continue
		}
		if err != nil || by != "" {
			t.Fatalf("output %d: %q, %v", v, by, err)
		}
	}
	if _, err := c.Spender(ctx, spendTx, 1024); !errors.Is(err, ErrSpendUnknown) {
		t.Fatalf("an output the answer leaves out: %v", err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads", n)
	}
}

func TestSpendCacheReadsAgainAfterTTL(t *testing.T) {
	var reads atomic.Int32
	now := time.Unix(1000, 0)
	c := &SpendCache{Source: utxoServer(t, 8, &reads), TTL: time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()
	for range 3 {
		if _, err := c.Spender(ctx, spendTx, 1); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Second)
	if _, err := c.Spender(ctx, spendTx, 1); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads", n)
	}
}

func TestSpendCacheSharesARead(t *testing.T) {
	var reads atomic.Int32
	c := &SpendCache{Source: utxoServer(t, 64, &reads), TTL: time.Minute}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := uint32(i%16 + 4)
			if by, err := c.Spender(context.Background(), spendTx, v); err != nil || by != "" {
				t.Errorf("output %d: %q, %v", v, by, err)
			}
		}()
	}
	wg.Wait()
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads", n)
	}
}

// A failed read answers its callers with the error and is not remembered.
func TestSpendCacheForgetsAFailedRead(t *testing.T) {
	fail := atomic.Bool{}
	fail.Store(true)
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if fail.Load() {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `[{"txid":%q,"vout":0,"status":"OK"}]`, spendTx)
	}))
	defer srv.Close()
	c := &SpendCache{Source: &Asset{Base: srv.URL}, TTL: time.Hour}
	ctx := context.Background()
	_, err := c.Spender(ctx, spendTx, 0)
	if !errors.Is(err, ErrSpendUnknown) {
		t.Fatalf("a failed read: %v", err)
	}
	if !IsHTTP(err, http.StatusNotFound) {
		t.Fatalf("the status is not kept: %v", err)
	}
	fail.Store(false)
	if by, err := c.Spender(ctx, spendTx, 0); err != nil || by != "" {
		t.Fatalf("the next read: %q, %v", by, err)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads", n)
	}
}

type oneByOne struct{ calls atomic.Int32 }

func (o *oneByOne) Spender(context.Context, string, uint32) (string, error) {
	o.calls.Add(1)
	return "", nil
}

// A source that answers per output is asked per output.
func TestSpendCachePassesThroughPerOutputSource(t *testing.T) {
	src := &oneByOne{}
	c := &SpendCache{Source: src, TTL: time.Hour}
	for range 3 {
		if _, err := c.Spender(context.Background(), spendTx, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := src.calls.Load(); n != 3 {
		t.Fatalf("%d calls", n)
	}
}

func TestCacheSpendsWrapsOnlyAWholeTransactionSource(t *testing.T) {
	var reads atomic.Int32
	s := &Sources{Spends: utxoServer(t, 2, &reads)}
	s.CacheSpends(time.Minute)
	if _, ok := s.Spends.(*SpendCache); !ok {
		t.Fatalf("an asset source stays %T", s.Spends)
	}
	per := &oneByOne{}
	s = &Sources{Spends: per}
	s.CacheSpends(time.Minute)
	if s.Spends != SpendSource(per) {
		t.Fatalf("a per-output source became %T", s.Spends)
	}
	s = &Sources{}
	s.CacheSpends(time.Minute)
	if s.Spends != nil {
		t.Fatalf("no source became %T", s.Spends)
	}
}
