package bwallet_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/nodeapi"
)

// The mined payment is a real one: WhatsOnChain's BEEF of a mainnet
// transaction in block 900000, captured 2026-10-07.
const (
	realTx   = "52854619b1c2e78d6c1e9a91fdb14c4bef1b8d1897de3253a538e152bd055be6"
	realRoot = "62272ce3662923219acd98587fdb5c0b01557597036d8635207bda8a3fa72a7e"
)

func source(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sources", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func realBEEF(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimSpace(string(source(t, "woc-main-tx-beef.txt"))))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func realHeaders() *goldentest.Tracker {
	return &goldentest.Tracker{Roots: map[uint32]string{900000: realRoot}, Tip: 970000}
}

// The script the real payment's output 0 pays, standing in for a fund
// script.
func realFund(t *testing.T) *script.Script {
	t.Helper()
	raw, _ := hex.DecodeString(strings.TrimSpace(string(source(t, "woc-main-tx-hex.txt"))))
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tx.Outputs[0].LockingScript
}

func TestImportBEEFOfAMinedPayment(t *testing.T) {
	ctx := context.Background()
	fund := realFund(t)
	im, err := bwallet.ImportBEEF(ctx, realBEEF(t), fund, realHeaders(), bwallet.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !im.Mined || im.Height != 900000 || im.Txid != realTx || im.BeefHex != "" || len(im.Outputs) == 0 {
		t.Fatalf("%+v", im)
	}
	for _, o := range im.Outputs {
		if o.Unproven || o.Bump == "" || o.Raw == "" || o.Height != 900000 || !o.Spendable(970000) {
			t.Fatalf("%+v", o)
		}
	}
	// RefuseUnmined does not touch a mined payment.
	if _, err := bwallet.ImportBEEF(ctx, realBEEF(t), fund, realHeaders(), bwallet.ImportOptions{RefuseUnmined: true}); err != nil {
		t.Fatal(err)
	}
	wrong := &goldentest.Tracker{Roots: map[uint32]string{900000: strings.Repeat("11", 32)}}
	if _, err := bwallet.ImportBEEF(ctx, realBEEF(t), fund, wrong, bwallet.ImportOptions{}); !errors.Is(err, nodeapi.ErrProofRefused) {
		t.Fatalf("headers that do not hold the root: %v", err)
	}
	if _, err := bwallet.ImportBEEF(ctx, realBEEF(t), &script.Script{script.OpTRUE}, realHeaders(), bwallet.ImportOptions{}); !errors.Is(err, bwallet.ErrPaysNothing) {
		t.Fatalf("pays nothing: %v", err)
	}
	if _, err := bwallet.ImportBEEF(ctx, realBEEF(t)[:40], fund, realHeaders(), bwallet.ImportOptions{}); err == nil {
		t.Fatal("a truncated BEEF")
	}
	if _, err := bwallet.ImportBEEF(ctx, realBEEF(t), fund, nil, bwallet.ImportOptions{}); err == nil {
		t.Fatal("no headers")
	}
}

type key struct {
	lock   *script.Script
	unlock transaction.UnlockingScriptTemplate
}

func keyOf(t *testing.T, seed byte) key {
	t.Helper()
	b := goldentest.Fill(seed)
	k, _ := ec.PrivateKeyFromBytes(b[:])
	addr, err := script.NewAddressFromPublicKey(k.PubKey(), true)
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := p2pkh.Lock(addr)
	unlock, _ := p2pkh.Unlock(k, nil)
	return key{lock, unlock}
}

// minedCoin is a transaction paying sats to k, alone in a block at height,
// so its proof's root is its txid; headers learn that root.
func minedCoin(t *testing.T, k key, sats uint64, height uint32, headers *goldentest.Tracker) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	src, _ := chainhash.NewHashFromHex(strings.Repeat("0f", 32))
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src, SequenceNumber: 0xffffffff, UnlockingScript: &script.Script{}})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: k.lock})
	mp, err := transaction.NewMerklePathFromCoinbaseTxid(tx.TxID(), height)
	if err != nil {
		t.Fatal(err)
	}
	tx.MerklePath = mp
	headers.Roots[height] = tx.TxID().String()
	return tx
}

// payment spends parent's output 0 to fund, with change to the payer.
func payment(t *testing.T, parent *transaction.Transaction, payer key, fund *script.Script) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(parent, 0, payer.unlock)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 3000, LockingScript: fund})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1900, LockingScript: payer.lock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

func beefOf(t *testing.T, tx *transaction.Transaction) []byte {
	t.Helper()
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImportBEEFOfAnUnminedPayment(t *testing.T) {
	ctx := context.Background()
	headers := &goldentest.Tracker{Roots: map[uint32]string{}, Tip: 200}
	payer, app := keyOf(t, 0x31), keyOf(t, 0x32)
	pay := payment(t, minedCoin(t, payer, 5000, 150, headers), payer, app.lock)

	im, err := bwallet.ImportBEEF(ctx, beefOf(t, pay), app.lock, headers, bwallet.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if im.Mined || im.Sats != 3000 || len(im.Outputs) != 1 || !im.Outputs[0].Unproven || im.Outputs[0].Vout != 0 || im.BeefHex == "" {
		t.Fatalf("%+v", im)
	}
	// The kept BEEF rebuilds the payment with its proven parent.
	re, err := funding.Rebuild("", "", im.BeefHex)
	if err != nil || re.TxID().String() != im.Txid || re.Inputs[0].SourceTransaction.MerklePath == nil {
		t.Fatalf("rebuild: %v", err)
	}
	// Into the pool held back, and spendable once its proof is collected.
	pool, err := bwallet.LoadPool(filepath.Join(t.TempDir(), "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Add(im.Outputs...); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Take(200); !errors.Is(err, bwallet.ErrNoSpendable) {
		t.Fatalf("an unmined payment is held back: %v", err)
	}
	if got := pool.UnprovenTxids(); len(got) != 1 || got[0] != im.Txid {
		t.Fatalf("waiting on %v", got)
	}
	mp, _ := transaction.NewMerklePathFromCoinbaseTxid(pay.TxID(), 160)
	if n, err := pool.Prove(im.Txid, mp.Hex(), 160); err != nil || n != 1 {
		t.Fatalf("prove: %d %v", n, err)
	}
	if o, err := pool.Take(200); err != nil || o.TxID != im.Txid {
		t.Fatalf("take: %v", err)
	}

	if _, err := bwallet.ImportBEEF(ctx, beefOf(t, pay), app.lock, headers, bwallet.ImportOptions{RefuseUnmined: true}); !errors.Is(err, bwallet.ErrUnmined) {
		t.Fatalf("refused by option: %v", err)
	}
}

func TestImportBEEFRefusesWhatCannotMine(t *testing.T) {
	ctx := context.Background()
	headers := &goldentest.Tracker{Roots: map[uint32]string{}, Tip: 200}
	payer, app := keyOf(t, 0x41), keyOf(t, 0x42)

	// A parent that is itself unmined: only one unmined hop is taken.
	coin := minedCoin(t, payer, 9000, 150, headers)
	mid := payment(t, coin, payer, payer.lock)
	deep := transaction.NewTransaction()
	deep.AddInputFromTx(mid, 0, payer.unlock)
	deep.AddOutput(&transaction.TransactionOutput{Satoshis: 2000, LockingScript: app.lock})
	if err := deep.Sign(); err != nil {
		t.Fatal(err)
	}
	if _, err := bwallet.ImportBEEF(ctx, beefOf(t, deep), app.lock, headers, bwallet.ImportOptions{}); !errors.Is(err, bwallet.ErrUnprovenParent) {
		t.Fatalf("unproven parent: %v", err)
	}

	// A parent whose proof the headers do not hold.
	stranger := minedCoin(t, payer, 5000, 151, headers)
	delete(headers.Roots, 151)
	if _, err := bwallet.ImportBEEF(ctx, beefOf(t, payment(t, stranger, payer, app.lock)), app.lock, headers, bwallet.ImportOptions{}); !errors.Is(err, nodeapi.ErrProofRefused) {
		t.Fatalf("parent proof: %v", err)
	}

	// A signature that does not verify.
	forged := payment(t, minedCoin(t, payer, 5000, 152, headers), payer, app.lock)
	other := keyOf(t, 0x43)
	forged.Inputs[0].UnlockingScriptTemplate = other.unlock
	if err := forged.Sign(); err != nil {
		t.Fatal(err)
	}
	if _, err := bwallet.ImportBEEF(ctx, beefOf(t, forged), app.lock, headers, bwallet.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("bad signature: %v", err)
	}

	// Not final: a lock time with a non-final input.
	late := transaction.NewTransaction()
	late.AddInputFromTx(minedCoin(t, payer, 5000, 153, headers), 0, payer.unlock)
	late.Inputs[0].SequenceNumber = 0
	late.LockTime = 2_000_000_000
	late.AddOutput(&transaction.TransactionOutput{Satoshis: 3000, LockingScript: app.lock})
	if err := late.Sign(); err != nil {
		t.Fatal(err)
	}
	if _, err := bwallet.ImportBEEF(ctx, beefOf(t, late), app.lock, headers, bwallet.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "not final") {
		t.Fatalf("not final: %v", err)
	}
}

// ImportTxid over WhatsOnChain, served from its captured answers.
func TestImportTxidOverWhatsOnChain(t *testing.T) {
	ctx := context.Background()
	routes := map[string][]byte{
		"/tx/" + realTx + "/hex":  source(t, "woc-main-tx-hex.txt"),
		"/tx/" + realTx + "/beef": source(t, "woc-main-tx-beef.txt"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write(b)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/beef") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/proof/tsc") {
			_, _ = w.Write([]byte("null"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	woc := &nodeapi.WoC{Network: "main", Base: srv.URL, Client: srv.Client(), Rate: 1000}
	im, err := bwallet.ImportTxid(ctx, " "+strings.ToUpper(realTx)+" ", realFund(t), woc, realHeaders())
	if err != nil || !im.Mined || im.Height != 900000 {
		t.Fatalf("%+v %v", im, err)
	}
	if _, err := bwallet.ImportTxid(ctx, realTx, realFund(t), woc, &goldentest.Tracker{Roots: map[uint32]string{}}); !errors.Is(err, nodeapi.ErrProofRefused) {
		t.Fatalf("headers: %v", err)
	}
	// Another transaction's bytes under the txid asked for.
	routes["/tx/"+strings.Repeat("ab", 32)+"/hex"] = source(t, "woc-main-tx-hex.txt")
	if _, err := bwallet.ImportTxid(ctx, strings.Repeat("ab", 32), realFund(t), woc, realHeaders()); err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("another transaction's bytes: %v", err)
	}
	if _, err := bwallet.ImportTxid(ctx, strings.Repeat("cd", 32), realFund(t), woc, realHeaders()); !nodeapi.IsNotFound(err) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := bwallet.ImportTxid(ctx, realTx, realFund(t), nil, realHeaders()); err == nil {
		t.Fatal("no source")
	}
}
