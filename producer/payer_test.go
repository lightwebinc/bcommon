package producer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
)

// A coin is spent under the key its output is locked to, against its
// parent rebuilt with its proof, and the input it becomes signs.
func TestTakeSpendsACoinUnderTheKeyItIsLockedTo(t *testing.T) {
	ctx := context.Background()
	l, n := newLab(t), &notes{}
	pool := poolIn(t)
	own, other := signerOf(t, newKey(t)), signerOf(t, newKey(t))
	coin := fund(t, pool, own, 5000, 0x11)
	p := payerFor(l, pool, own, n)
	p.Keys[other.IdentityHex()] = other

	in, err := p.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.Tx.TxID().String() != coin.TxID().String() || in.Vout != 0 || in.Tx.MerklePath == nil {
		t.Fatalf("the input is not the proven coin: %s:%d proof %v", in.Tx.TxID(), in.Vout, in.Tx.MerklePath != nil)
	}
	change, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	dest := &script.Script{script.OpTRUE}
	tx, err := mint.Payment(ctx, dest, 1000, in, change, mint.DefaultFees)
	if err != nil {
		t.Fatalf("the input does not sign under its key: %v", err)
	}
	if !tx.Inputs[0].SourceTxOutput().LockingScript.IsP2PKH() {
		t.Fatal("the fee input is not the fund key's P2PKH")
	}
	if pool.Count() != 0 {
		t.Fatalf("the coin taken is still in the pool: %d", pool.Count())
	}
	p.GiveBack()
	if pool.Count() != 1 {
		t.Fatalf("GiveBack did not return the coin: pool holds %d", pool.Count())
	}
	p.GiveBack()
	if pool.Count() != 1 {
		t.Fatalf("a second GiveBack returned a coin twice: pool holds %d", pool.Count())
	}
}

// No coin is a *NoCoinError the application can put its own words to, and it
// says whether coin is merely waiting for a proof.
func TestTakeWithNoCoinIsANoCoinError(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	own := signerOf(t, newKey(t))
	p := payerFor(l, poolIn(t), own, &notes{})

	_, err := p.Take(ctx)
	var nc *producer.NoCoinError
	if !errors.As(err, &nc) || nc.Held != 0 || !errors.Is(err, bwallet.ErrNoSpendable) {
		t.Fatalf("an empty pool: %#v", err)
	}
	if err.Error() != "fee input: bwallet: no spendable output in the wallet" {
		t.Fatalf("text: %q", err)
	}

	// Change from a transaction that has not mined is held back.
	lock, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	parent := coinFor(lock, 900, 0x21)
	parent.MerklePath = nil
	p.Change(parent, 0, nil)
	_, err = p.Take(ctx)
	if !errors.As(err, &nc) || nc.Held != 1 {
		t.Fatalf("held change: %#v", err)
	}
	want := "fee input: bwallet: no spendable output in the wallet: the other coins are change from 1 transaction(s) whose proofs have not arrived; they become spendable once mined and collected"
	if err.Error() != want {
		t.Fatalf("text: %q", err)
	}
}

// Unproven change is taken only when its parent is one the transaction
// being built already carries, and then it is spent against that same kept
// object, so the BEEF never holds two copies of it.
func TestTakeAllowsUnprovenChangeOfAKeptParent(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	lock, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	parent := coinFor(lock, 900, 0x22)
	parent.MerklePath = nil
	p := payerFor(l, pool, own, &notes{})
	p.Change(parent, 0, nil)
	loads := 0
	p.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		loads++
		if txid != parent.TxID().String() {
			return nil, errors.New("not kept")
		}
		return parent, nil
	}
	p.Allow = func() []string { return []string{parent.TxID().String()} }

	in, err := p.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := p.Kept.Tx(parent.TxID().String())
	if err != nil {
		t.Fatal(err)
	}
	if in.Tx != kept || kept != parent || loads != 1 {
		t.Fatalf("the fee parent is not the one kept object (loads %d)", loads)
	}
}

func TestTakeRefusesACoinNoKeyHolds(t *testing.T) {
	l := newLab(t)
	pool := poolIn(t)
	own, stranger := signerOf(t, newKey(t)), signerOf(t, newKey(t))
	coin := fund(t, pool, stranger, 5000, 0x12)
	_, err := payerFor(l, pool, own, &notes{}).Take(context.Background())
	var nk *producer.NoKeyError
	if !errors.As(err, &nk) || nk.Outpoint != coin.TxID().String()+".0" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), coin.TxID().String()+".0") {
		t.Fatalf("the refusal must name the coin: %v", err)
	}
}

// A received payment is spent under a key its owner re-derives from the
// sender, found through KeyFor, or through Keys when KeyFor is not set.
func TestTakeSpendsAReceivedPaymentUnderItsOwnersKey(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	pool := poolIn(t)
	owner, sender := signerOf(t, newKey(t)), signerOf(t, newKey(t))
	dest, err := sender.PaymentDestination(ctx, owner.IdentityHex(), "cHJlZml4", "c3VmZml4")
	if err != nil {
		t.Fatal(err)
	}
	coin := coinFor(dest, 3000, 0x13)
	d := &bwallet.Derivation{SecurityLevel: int(bwallet.PaymentProtocol.SecurityLevel), Protocol: bwallet.PaymentProtocol.Protocol,
		KeyID: bwallet.PaymentKeyID("cHJlZml4", "c3VmZml4"), CounterpartyHex: sender.IdentityHex(), OwnerHex: owner.IdentityHex()}
	add := func() {
		if _, err := pool.Add(bwallet.Output{TxID: coin.TxID().String(), Vout: 0, Satoshis: 3000, LockingScript: dest.String(),
			Height: 90, Raw: coin.Hex(), Bump: coin.MerklePath.Hex(), Derivation: d}); err != nil {
			t.Fatal(err)
		}
	}
	add()
	p := payerFor(l, pool, owner, &notes{})
	in, err := p.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	change, _ := owner.FundScript()
	if _, err := mint.Payment(ctx, &script.Script{script.OpTRUE}, 100, in, change, mint.DefaultFees); err != nil {
		t.Fatalf("the payment does not sign under the derived key: %v", err)
	}

	// KeyFor answers for the owner, in the application's words.
	p.GiveBack()
	p.KeyFor = func(id string) (*bwallet.Signer, error) { return nil, errors.New("no key file for " + id[:12]) }
	if _, err := p.Take(ctx); err == nil || err.Error() != "no key file for "+owner.IdentityHex()[:12] {
		t.Fatalf("KeyFor was not asked: %v", err)
	}

	// With neither, the refusal names the identity.
	p.GiveBack()
	p.KeyFor = nil
	delete(p.Keys, owner.IdentityHex())
	if _, err := p.Take(ctx); err == nil || !strings.Contains(err.Error(), owner.IdentityHex()) {
		t.Fatalf("a missing owner: %v", err)
	}
}

// A key's fund script failing is not skipped: Take stops on it.
func TestTakeStopsOnAKeyThatCannotDerive(t *testing.T) {
	l := newLab(t)
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 5000, 0x14)
	bare := &bwallet.Signer{Interface: own.Interface, Identity: own.Identity}
	p := payerFor(l, pool, bare, &notes{})
	if _, err := p.Take(context.Background()); !errors.Is(err, bwallet.ErrProfile) {
		t.Fatalf("got %v, want the profile refusal", err)
	}
}

// change recognises a producer's change by each key's fund script. A key with
// no profile has none, and its change left out of the pool reads as spent
// coin, so change names that key. The keys it can derive for are still
// recorded, and a key whose derivation fails for any other reason is passed
// over.
func TestChangeNamesAWalletWithNoProfile(t *testing.T) {
	primary := signerOf(t, newKey(t))
	bareKey := newKey(t)
	bareWallet, err := wallet.NewCompletedProtoWallet(bareKey)
	if err != nil {
		t.Fatal(err)
	}
	refusing := &bwallet.Signer{Interface: refusingKeys{bareWallet}, Identity: bareKey.PubKey(), Profile: testProfile}
	noProfile := &bwallet.Signer{Interface: bareWallet, Identity: bareKey.PubKey()}
	lock, err := primary.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 700, LockingScript: lock})

	run := func(t *testing.T, w *bwallet.Signer) (said string, held []bwallet.Output) {
		t.Helper()
		pool := poolIn(t)
		n := &notes{}
		p := &producer.Payer{Pool: pool, Keys: map[string]*bwallet.Signer{primary.IdentityHex(): primary, w.IdentityHex(): w}, Note: n.note}
		p.Change(tx, 9, nil)
		return n.String(), pool.Outputs()
	}

	for _, c := range []struct {
		name string
		w    *bwallet.Signer
		want string
	}{
		{"a wallet that refuses its key call", refusing, ""},
		{"a wallet with no profile", noProfile, "WARNING: wallet " + noProfile.IdentityHex() +
			" has no usable fund profile (bwallet: wallet profile incomplete: FundProtocol has no protocol name);" +
			" change paid to it is not added to the pool and will read as spent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			said, held := run(t, c.w)
			if said != c.want {
				t.Errorf("change said %q, want %q", said, c.want)
			}
			if len(held) != 1 || held[0].TxID != tx.TxID().String() || held[0].Vout != 0 || held[0].Satoshis != 700 || !held[0].Unproven || held[0].Height != 9 {
				t.Errorf("the primary's change is not recorded beside it: %+v", held)
			}
		})
	}
}

// refusingKeys is a wallet whose key calls fail for a reason other than its
// profile.
type refusingKeys struct{ wallet.Interface }

func (refusingKeys) GetPublicKey(context.Context, wallet.GetPublicKeyArgs, string) (*wallet.GetPublicKeyResult, error) {
	return nil, errors.New("refused by the test")
}

// Change from a mined transaction carries its proof and is spendable at
// once; change from one that has not mined is held back until its proof is
// collected. Only outputs paying one of the keys are taken.
func TestChangeKeepsTheParentWholeAndHoldsUnprovenChange(t *testing.T) {
	own := signerOf(t, newKey(t))
	lock, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx := coinFor(lock, 800, 0x31)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 5, LockingScript: &script.Script{script.OpTRUE}})
	pool := poolIn(t)
	p := &producer.Payer{Pool: pool, Keys: map[string]*bwallet.Signer{own.IdentityHex(): own}}
	p.Change(tx, 91, tx.MerklePath)
	out := pool.Outputs()
	if len(out) != 1 || out[0].Unproven || out[0].Bump != tx.MerklePath.Hex() || out[0].Raw != tx.Hex() || out[0].Height != 91 {
		t.Fatalf("proven change: %+v", out)
	}
	if !out[0].Spendable(0) {
		t.Fatal("proven change is not spendable")
	}
}

// Parent rebuilds what a fee input signs against: the pool's copy with its
// proof, the pool's copy alone when the spender is mined before it is
// published, the node's copy with its proof when it is not, and a stub for a
// coinbase the pool holds no bytes of.
func TestParentRebuildsWhatTheSpenderCarries(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	own := signerOf(t, newKey(t))
	lock, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	coin := coinFor(lock, 700, 0x41)
	out := bwallet.Output{TxID: coin.TxID().String(), Vout: 0, Satoshis: 700, LockingScript: lock.String(), Height: 90}
	p := payerFor(l, poolIn(t), own, &notes{})

	proven := out
	proven.Raw, proven.Bump = coin.Hex(), coin.MerklePath.Hex()
	tx, err := p.Parent(ctx, proven)
	if err != nil || tx.MerklePath == nil || tx.TxID().String() != coin.TxID().String() {
		t.Fatalf("raw and proof: %v", err)
	}

	bare := out
	bare.Raw = coin.Hex()
	if tx, err := p.Parent(ctx, bare); err != nil || tx.MerklePath != nil {
		t.Fatalf("raw alone, waiting for blocks: %v", err)
	}

	// Async: the node's copy, with the proof it serves.
	p.Async = true
	l.know(coin)
	l.mine(coin.TxID().String())
	tx, err = p.Parent(ctx, bare)
	if err != nil || tx.MerklePath == nil {
		t.Fatalf("async: %v", err)
	}
	// The node answering some other transaction is refused.
	other := coinFor(lock, 701, 0x42)
	l.mu.Lock()
	l.known[coin.TxID().String()] = other
	l.mu.Unlock()
	if _, err := p.Parent(ctx, bare); err == nil || !strings.Contains(err.Error(), "the node answered transaction "+other.TxID().String()) {
		t.Fatalf("a substituted parent: %v", err)
	}
	// And a parent the node has not got is an error naming the coin.
	missing := out
	missing.TxID = strings.Repeat("ab", 32)
	if _, err := p.Parent(ctx, missing); err == nil || !strings.Contains(err.Error(), "fee input "+missing.Outpoint()+": fetching its parent") {
		t.Fatalf("a parent the node lacks: %v", err)
	}

	// A coinbase with no bytes, spent by a transaction mined before it is
	// published: a stub with the one output, under the coin's own txid.
	p.Async = false
	cb := out
	cb.Vout = 2
	tx, err = p.Parent(ctx, cb)
	if err != nil {
		t.Fatal(err)
	}
	if tx.TxID().String() != cb.TxID || len(tx.Outputs) != 3 || tx.Outputs[2].Satoshis != 700 || !tx.Outputs[2].LockingScript.Equals(lock) {
		t.Fatalf("stub: %s with %d outputs", tx.TxID(), len(tx.Outputs))
	}
}

// Async settles on the leg's acceptance and leaves the proof for later.
func TestSettleAsyncReturnsOnAcceptance(t *testing.T) {
	ctx := context.Background()
	l, n := newLab(t), &notes{}
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 5000, 0x51)
	p := payerFor(l, pool, own, n)
	p.Async = true
	tx := paymentFrom(t, p, own)

	mp, height, err := p.Settle(ctx, "payment", tx)
	if err != nil || mp != nil || height != 0 {
		t.Fatalf("async settle: %v %v %d", err, mp, height)
	}
	id := tx.TxID().String()
	want := []string{
		"payment " + id + ": broadcasting via arcade:" + l.srv.URL + "/arc (" + itoa(tx.Size()) + " bytes)",
		"payment " + id + ": accepted; its proof is collected later",
	}
	if strings.Join(n.all(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", n, strings.Join(want, "\n"))
	}
}

// Without Async a settle waits for the proof and gives it to the
// transaction; SettleAndWait waits whatever Async says.
func TestSettleWaitsForTheProof(t *testing.T) {
	ctx := context.Background()
	for _, async := range []bool{false, true} {
		l, n := newLab(t), &notes{}
		l.mineOnSubmit = true
		pool := poolIn(t)
		own := signerOf(t, newKey(t))
		fund(t, pool, own, 5000, 0x52)
		p := payerFor(l, pool, own, n)
		p.Async = async
		tx := paymentFrom(t, p, own)
		var mp *transaction.MerklePath
		var height uint32
		var err error
		if async {
			mp, height, err = p.SettleAndWait(ctx, "sweep", tx)
		} else {
			mp, height, err = p.Settle(ctx, "sweep", tx)
		}
		if err != nil || mp == nil || height != 701 || tx.MerklePath != mp {
			t.Fatalf("async=%v: %v height %d", async, err, height)
		}
		id := tx.TxID().String()
		want := []string{
			"sweep " + id + ": settling via arcade:" + l.srv.URL + "/arc (" + itoa(tx.Size()) + " bytes)",
			"sweep " + id + ": mined at height 701",
		}
		if strings.Join(n.all(), "\n") != strings.Join(want, "\n") {
			t.Fatalf("async=%v notes:\n%s", async, n)
		}
	}
}

func TestSettleReportsTheLegsRefusalAndATimeout(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	pool := poolIn(t)
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 5000, 0x53)
	fund(t, pool, own, 5000, 0x54)
	p := payerFor(l, pool, own, &notes{})
	tx := paymentFrom(t, p, own)
	l.mu.Lock()
	l.refused[tx.TxID().String()] = "fee too low"
	l.mu.Unlock()
	if _, _, err := p.Settle(ctx, "token", tx); err == nil || !strings.HasPrefix(err.Error(), "token: settle: ") || !strings.Contains(err.Error(), "fee too low") {
		t.Fatalf("a refusal: %v", err)
	}

	p.Timeout = 50 * time.Millisecond
	tx2 := paymentFrom(t, p, own)
	if _, _, err := p.Settle(ctx, "token", tx2); err == nil || !strings.HasPrefix(err.Error(), "token: waiting for a proof: ") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no block: %v", err)
	}

	var none producer.Payer
	if _, _, err := none.Settle(ctx, "token", tx2); err == nil {
		t.Fatal("a Payer with no settlement leg settled")
	}
	if _, _, err := none.Await(ctx, "token", tx2); err == nil {
		t.Fatal("a Payer with no node waited")
	}
}

// paymentFrom is a small signed payment funded by p, for the legs to carry.
func paymentFrom(t *testing.T, p *producer.Payer, own *bwallet.Signer) *transaction.Transaction {
	t.Helper()
	in, err := p.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	change, err := own.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := mint.Payment(context.Background(), &script.Script{script.OpTRUE}, 1000, in, change, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func itoa(n int) string { return strconv.Itoa(n) }

// The Payer never writes outside its pool: Take and GiveBack persist the
// reservation, so a crash between them never double-spends a coin.
func TestTakePersistsTheReservation(t *testing.T) {
	l := newLab(t)
	dir := t.TempDir()
	pool, err := bwallet.LoadPool(filepath.Join(dir, "wallet.json"))
	if err != nil {
		t.Fatal(err)
	}
	own := signerOf(t, newKey(t))
	fund(t, pool, own, 5000, 0x61)
	p := payerFor(l, pool, own, &notes{})
	if _, err := p.Take(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := bwallet.LoadPool(filepath.Join(dir, "wallet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Count() != 0 {
		t.Fatalf("the reservation was not on disk: %d coin(s)", again.Count())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("the payer wrote beside the pool: %v", entries)
	}
}
