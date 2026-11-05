package carrier

import (
	"bytes"
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// mintReference is Mint as it was before LockOne and signCarrier: the SDK's
// lock and the unlocker's tx.Sign. The fast path must match it byte for byte.
func mintReference(ctx context.Context, w wallet.Interface, originator string, p Params, payload []byte,
	funding *transaction.Transaction, vout uint32) (*transaction.Transaction, error) {
	lock, err := p.Derivation.Lock(ctx, w, originator, [][]byte{payload}, true)
	if err != nil {
		return nil, err
	}
	tx := transaction.NewTransaction()
	tx.LockTime = LockTime
	tx.AddInputFromTx(funding, vout, p.Derivation.Unlocker(ctx, w, originator))
	tx.Inputs[0].SequenceNumber = Sequence
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: funding.Outputs[vout].Satoshis, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		return nil, err
	}
	return tx, nil
}

// Mint builds exactly the transaction the SDK's lock and signing build, at
// every push length boundary and for a full 4 MiB piece record, on each
// funding output. The wallet's signatures are deterministic (RFC 6979), so
// equal bytes are equal signatures, field and input both.
func TestMintMatchesTheSDKPath(t *testing.T) {
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
	for _, n := range []int{0, 1, 2, 16, 17, 75, 76, 255, 256, 65535, 65536, 4<<20 + 300} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i*7 + n)
		}
		if n == 1 {
			payload[0] = 9 // an OP_9 field: Lock's path
		}
		for vout := uint32(0); vout < 3; vout++ {
			got, err := Mint(ctx, w, "test", p, payload, funding, vout)
			if err != nil {
				t.Fatalf("%d bytes: %v", n, err)
			}
			want, err := mintReference(ctx, w, "test", p, payload, funding, vout)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("%d bytes, output %d: the transaction differs from the SDK path's", n, vout)
			}
		}
	}
}

// LockOne's script is Lock's for any field, and does not alias the field.
func TestLockOneMatchesLock(t *testing.T) {
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	d := pushdrop.Derivation{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "carrier once"}, KeyID: "7"}
	for _, f := range [][]byte{{}, {0}, {5}, {0x81}, {0x42}, {1, 2}, bytes.Repeat([]byte{3}, 300), bytes.Repeat([]byte{4}, 70000)} {
		got, err := d.LockOne(ctx, w, "test", f)
		if err != nil {
			t.Fatal(err)
		}
		want, err := d.Lock(ctx, w, "test", [][]byte{f}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(*got, *want) {
			t.Fatalf("a field of %d bytes: %x, Lock %x", len(f), (*got)[:min(40, len(*got))], (*want)[:min(40, len(*want))])
		}
		if len(f) > 1 {
			f[0] ^= 0xff
			if bytes.Contains(*got, f) {
				t.Fatal("the script aliases the field")
			}
		}
	}
}

func benchMint(b *testing.B, mint func(context.Context, wallet.Interface, string, Params, []byte, *transaction.Transaction, uint32) (*transaction.Transaction, error)) {
	ctx := context.Background()
	w, _ := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	p := Params{Derivation: pushdrop.Derivation{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "carrier once"}, KeyID: "1"}, FundingTag: []byte("fund")}
	lock, _ := FundingLock(ctx, w, "test", p)
	funding := transaction.NewTransaction()
	fake := chainhash.Hash(goldentest.Fill(0x11))
	funding.AddInput(&transaction.TransactionInput{SourceTXID: &fake, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	funding.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
	payload := bytes.Repeat([]byte("piece "), 4<<20/6)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := mint(ctx, w, "test", p, payload, funding, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMint4MiB(b *testing.B)          { benchMint(b, Mint) }
func BenchmarkMint4MiBReference(b *testing.B) { benchMint(b, mintReference) }
