package carrier

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

func onceFixture(t testing.TB) (context.Context, wallet.Interface, Params, *transaction.Transaction) {
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	p := Params{Derivation: pushdrop.Derivation{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "carrier once"}, KeyID: "1"}, FundingTag: []byte("fund")}
	lock, err := FundingLock(ctx, w, "test", p)
	if err != nil {
		t.Fatal(err)
	}
	funding := transaction.NewTransaction()
	fake := chainhash.Hash(goldentest.Fill(0x11))
	funding.AddInput(&transaction.TransactionInput{SourceTXID: &fake, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for _, sats := range []uint64{1000, 1, 5000} {
		funding.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
	}
	return ctx, w, p, funding
}

// MintOnce, written by AppendTx, is byte for byte Mint's carrier and the SDK reference path's, for the
// payload split into head, body and tail at any point, at every push-length boundary, on every funding
// output and for a full 4 MiB piece record; its txid hashed in place is the SDK's, and the aliased
// transaction serializes to the same bytes.
func TestMintOnceMatchesMint(t *testing.T) {
	ctx, w, p, funding := onceFixture(t)
	r := rand.New(rand.NewSource(3))
	for _, n := range []int{2, 16, 17, 75, 76, 255, 256, 65535, 65536, 4<<20 + 300} {
		payload := make([]byte, n)
		r.Read(payload)
		for vout := uint32(0); vout < 3; vout++ {
			want, err := Mint(ctx, w, "test", p, payload, funding, vout)
			if err != nil {
				t.Fatalf("%d bytes: %v", n, err)
			}
			ref, err := mintReference(ctx, w, "test", p, payload, funding, vout)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want.Bytes(), ref.Bytes()) {
				t.Fatalf("%d bytes: Mint differs from the SDK path", n)
			}
			for _, cut := range [][2]int{{0, n}, {n / 3, n - n/5}, {1, n - 1}, {n, n}, {r.Intn(n), n}} {
				a, b := cut[0], max(cut[0], cut[1])
				o, err := MintOnce(ctx, w, "test", p, payload[:a], payload[a:b], payload[b:], funding, vout)
				if err != nil {
					t.Fatalf("%d bytes, cut %v: %v", n, cut, err)
				}
				prefix := []byte("BEEF header bytes before the carrier")
				got, tx := o.AppendTx(append([]byte{}, prefix...))
				raw := got[len(prefix):]
				if len(raw) != o.Size() {
					t.Fatalf("%d bytes: Size %d, wrote %d", n, o.Size(), len(raw))
				}
				if !bytes.Equal(raw, want.Bytes()) {
					t.Fatalf("%d bytes, output %d, cut %v: MintOnce differs from Mint", n, vout, cut)
				}
				if !bytes.Equal(tx.Bytes(), want.Bytes()) {
					t.Fatalf("%d bytes: the aliased transaction serializes differently", n)
				}
				if id := TxIDAt(tx, raw); id != *want.TxID() {
					t.Fatalf("%d bytes: txid in place %s, Mint's %s", n, id, want.TxID())
				}
			}
		}
	}
}

func TestMintOnceRefusesTinyPayloads(t *testing.T) {
	ctx, w, p, funding := onceFixture(t)
	if _, err := MintOnce(ctx, w, "test", p, nil, []byte{7}, nil, funding, 0); err == nil {
		t.Fatal("a one-byte payload took the push path")
	}
	if _, err := MintOnce(ctx, w, "test", p, nil, []byte{1, 2}, nil, funding, 3); err == nil {
		t.Fatal("an output out of range was taken")
	}
}

// One 4 MiB piece carrier: the copy-per-step path (record encoded, Mint, AppendBytes) against MintOnce
// writing straight into the send buffer.
func BenchmarkCarrier4MiB(b *testing.B) {
	ctx, w, p, funding := onceFixture(b)
	body := make([]byte, 4<<20)
	head := make([]byte, 120)
	b.Run("mint-copy", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rec := append(append(make([]byte, 0, len(head)+len(body)), head...), body...)
			tx, err := Mint(ctx, w, "test", p, rec, funding, 0)
			if err != nil {
				b.Fatal(err)
			}
			_ = tx.AppendBytes(make([]byte, 0, tx.Size()))
		}
	})
	b.Run("mint-once", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		buf := make([]byte, 0, 5<<20)
		for i := 0; i < b.N; i++ {
			o, err := MintOnce(ctx, w, "test", p, head, body, nil, funding, 0)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = o.AppendTx(buf[:0])
		}
	})
}
