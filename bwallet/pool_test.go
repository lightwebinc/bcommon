package bwallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testLock = "76a914" + "00000000000000000000000000000000000000ff" + "88ac"

func out(id byte, vout uint32, sats uint64, height uint32, coinbase bool) Output {
	return Output{
		TxID:          strings.Repeat(fmt.Sprintf("%02x", id), 32),
		Vout:          vout,
		Satoshis:      sats,
		LockingScript: testLock,
		Height:        height,
		Coinbase:      coinbase,
	}
}

func newPool(t *testing.T) *Pool {
	t.Helper()
	p, err := LoadPool(filepath.Join(t.TempDir(), "outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTakeIsOldestSpendableFirst(t *testing.T) {
	p := newPool(t)
	a := out(0xaa, 0, 10, 200, true)  // coinbase, mature at tip 300
	b := out(0xbb, 0, 20, 100, true)  // coinbase, mature at tip 200
	c := out(0xcc, 1, 30, 150, false) // change, spendable on arrival
	if n, err := p.Add(a, b, c); err != nil || n != 3 {
		t.Fatalf("Add: n=%d err=%v", n, err)
	}
	got, err := p.Take(250)
	if err != nil || got.TxID != b.TxID {
		t.Fatalf("first Take at 250 = %s err %v, want b (oldest mature coinbase)", got.TxID, err)
	}
	got, err = p.Take(250)
	if err != nil || got.TxID != c.TxID {
		t.Fatalf("second Take at 250 = %s err %v, want c", got.TxID, err)
	}
	if _, err := p.Take(250); !errors.Is(err, ErrNoSpendable) {
		t.Fatalf("third Take at 250: err %v, want ErrNoSpendable (a is immature)", err)
	}
	if im := p.Immature(250); len(im) != 1 || im[0].TxID != a.TxID {
		t.Fatalf("Immature(250) = %v, want [a]", im)
	}
	if _, err := p.Take(299); !errors.Is(err, ErrNoSpendable) {
		t.Fatal("a coinbase at 200 needs tip >= 300, not 299")
	}
	got, err = p.Take(300)
	if err != nil || got.TxID != a.TxID {
		t.Fatalf("Take at 300 = %s err %v, want a", got.TxID, err)
	}
	if p.Count() != 0 {
		t.Fatalf("pool should be empty, holds %d", p.Count())
	}
}

func TestTakeAndReturnPersistImmediately(t *testing.T) {
	p := newPool(t)
	if _, err := p.Add(out(1, 0, 5, 1, false), out(2, 0, 6, 2, false)); err != nil {
		t.Fatal(err)
	}
	taken, err := p.Take(0)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := LoadPool(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if disk.Count() != 1 {
		t.Fatalf("after Take the file holds %d outputs, want 1: a reservation only in memory is a double spend after a crash", disk.Count())
	}
	if err := p.Return(taken); err != nil {
		t.Fatal(err)
	}
	if err := p.Return(taken); err != nil {
		t.Fatal(err)
	}
	disk, err = LoadPool(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if disk.Count() != 2 {
		t.Fatalf("after Return the file holds %d outputs, want 2 (and no duplicate)", disk.Count())
	}
	if disk.Balance() != 11 {
		t.Fatalf("balance %d, want 11", disk.Balance())
	}
}

func TestAddDedupesByOutpoint(t *testing.T) {
	p := newPool(t)
	o := out(7, 3, 1, 1, true)
	if n, _ := p.Add(o); n != 1 {
		t.Fatalf("first Add n=%d", n)
	}
	if n, _ := p.Add(o); n != 0 {
		t.Fatalf("second Add n=%d, want 0", n)
	}
	other := o
	other.Vout = 4
	if n, _ := p.Add(other); n != 1 {
		t.Fatal("a different vout of the same txid is a different output")
	}
	if p.Count() != 2 {
		t.Fatalf("count %d", p.Count())
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outputs.json")
	// A stale file with unrelated content and a loose mode: Save must
	// replace it whole and tighten the mode, not append to or truncate it.
	if err := os.WriteFile(path, []byte("garbage that is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPool(path); err == nil {
		t.Fatal("a malformed pool file must be an error, not an empty pool")
	}
	p := &Pool{path: path}
	if _, err := p.Add(out(1, 0, 1, 1, false)); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("pool mode %o, want 0600", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st poolJSON
	if err := json.Unmarshal(raw, &st); err != nil || len(st.Outputs) != 1 {
		t.Fatalf("saved file is not the pool: %v %s", err, raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only outputs.json (no temp file left)", len(entries))
	}
}

func TestLoadPoolMissingIsEmpty(t *testing.T) {
	p, err := LoadPool(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Count() != 0 || p.Balance() != 0 {
		t.Fatal("a missing pool is an empty pool")
	}
	if _, err := p.Take(1000); !errors.Is(err, ErrNoSpendable) {
		t.Fatal("Take on an empty pool must be ErrNoSpendable")
	}
}

// Change from a transaction published before it mined is held back until its
// proof arrives, which keeps every fee input one hop from a proven parent.
func TestUnprovenChangeIsHeldUntilProved(t *testing.T) {
	p := newPool(t)
	if _, err := p.Add(Output{TxID: "aa", Vout: 1, Satoshis: 500, LockingScript: "51", Raw: "00", Unproven: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Take(1000); err == nil {
		t.Fatal("an unproven coin was taken")
	}
	if got := p.UnprovenTxids(); len(got) != 1 || got[0] != "aa" {
		t.Fatalf("unproven parents: %v", got)
	}
	if n, err := p.Prove("aa", "fe", 900); err != nil || n != 1 {
		t.Fatalf("prove: %d %v", n, err)
	}
	o, err := p.Take(1000)
	if err != nil {
		t.Fatalf("a proven coin was not taken: %v", err)
	}
	if o.Bump != "fe" || o.Height != 900 || o.Unproven {
		t.Fatalf("the proof was not recorded on the coin: %+v", o)
	}
	if len(p.UnprovenTxids()) != 0 {
		t.Fatal("a proven parent is still listed as pending")
	}
}

// A wallet with one coin must be able to publish twice inside a block. The
// second transition spends the first token as its previous-token input, so
// that token's change adds nothing to its BEEF and may be taken. Change from
// any other unproven parent is still held.
func TestTakeAllowingReleasesOnlyAParentTheSpenderCarries(t *testing.T) {
	p := newPool(t)
	if _, err := p.Add(
		Output{TxID: "other", Vout: 1, Satoshis: 500, LockingScript: "51", Raw: "00", Unproven: true},
		Output{TxID: "token", Vout: 1, Satoshis: 700, LockingScript: "51", Raw: "00", Unproven: true},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TakeAllowing(1000, nil); err == nil {
		t.Fatal("unproven change was taken with nothing allowed")
	}
	o, err := p.TakeAllowing(1000, []string{"token"})
	if err != nil || o.TxID != "token" {
		t.Fatalf("the carried parent's change was not released: %+v %v", o, err)
	}
	if _, err := p.TakeAllowing(1000, []string{"token"}); err == nil {
		t.Fatal("change from a parent the spender does not carry was taken")
	}
	// A proven coin is always preferred, so the relaxation never displaces
	// ordinary spending.
	if _, err := p.Add(Output{TxID: "mined", Vout: 0, Satoshis: 900, LockingScript: "51"},
		Output{TxID: "token2", Vout: 1, Satoshis: 700, LockingScript: "51", Raw: "00", Unproven: true}); err != nil {
		t.Fatal(err)
	}
	if o, err := p.TakeAllowing(1000, []string{"token2"}); err != nil || o.TxID != "mined" {
		t.Fatalf("a proven coin was not preferred: %+v %v", o, err)
	}
}

// TakeAtLeast leaves a coin too small for the caller in the pool and takes
// the oldest one that is large enough; Take and TakeAllowing never hand out
// a coin of nothing.
func TestTakeAtLeastSkipsCoinsTooSmall(t *testing.T) {
	p := newPool(t)
	zero, small, big := out(1, 0, 0, 1, false), out(2, 0, 100, 2, false), out(3, 0, 5000, 3, false)
	if _, err := p.Add(zero, small, big); err != nil {
		t.Fatal(err)
	}
	o, err := p.TakeAtLeast(0, 300, nil)
	if err != nil || o.Outpoint() != big.Outpoint() {
		t.Fatalf("took %+v %v, want the 5000 sat coin", o, err)
	}
	_, err = p.TakeAtLeast(0, 300, nil)
	if !errors.Is(err, ErrNoSpendable) || err.Error() != "bwallet: no spendable output in the wallet of at least 300 sat" {
		t.Fatalf("only small coins left: %v", err)
	}
	if p.Count() != 2 {
		t.Fatalf("a refused take changed the pool: %d held", p.Count())
	}
	o, err = p.Take(0)
	if err != nil || o.Outpoint() != small.Outpoint() {
		t.Fatalf("Take took %+v %v, want the 100 sat coin past the zero one", o, err)
	}
	if _, err := p.TakeAllowing(0, []string{zero.TxID}); !errors.Is(err, ErrNoSpendable) {
		t.Fatalf("a zero-value coin was taken: %v", err)
	}
	if _, err := p.TakeAtLeast(0, 0, nil); !errors.Is(err, ErrNoSpendable) {
		t.Fatalf("a minimum of zero took a coin of nothing: %v", err)
	}
	if p.Count() != 1 {
		t.Fatalf("pool holds %d, want the zero-value coin alone", p.Count())
	}
}

// The minimum holds on the fallback to allowed unproven change too.
func TestTakeAtLeastAppliesToAllowedChange(t *testing.T) {
	p := newPool(t)
	small, big := out(1, 0, 100, 0, false), out(2, 0, 5000, 0, false)
	small.Unproven, big.Unproven = true, true
	if _, err := p.Add(small, big); err != nil {
		t.Fatal(err)
	}
	allow := []string{small.TxID, big.TxID}
	if _, err := p.TakeAtLeast(0, 300, nil); !errors.Is(err, ErrNoSpendable) {
		t.Fatalf("unproven change taken without an allow: %v", err)
	}
	o, err := p.TakeAtLeast(0, 300, allow)
	if err != nil || o.Outpoint() != big.Outpoint() {
		t.Fatalf("took %+v %v, want the allowed 5000 sat coin", o, err)
	}
	if _, err := p.TakeAtLeast(0, 300, allow); err == nil || !strings.Contains(err.Error(), "of at least 300 sat") {
		t.Fatalf("only a small allowed coin left: %v", err)
	}
}

// Remove forgets a coin the chain shows spent: the file is saved without
// it, a coin the pool does not hold changes nothing, and only the outpoint
// decides which coin is meant.
func TestRemovePersistsImmediately(t *testing.T) {
	p := newPool(t)
	a, b, c := out(1, 0, 5, 1, false), out(1, 1, 6, 1, false), out(2, 0, 7, 2, false)
	if _, err := p.Add(a, b, c); err != nil {
		t.Fatal(err)
	}
	// The outpoint alone names the coin: the rest of the Output may differ.
	if ok, err := p.Remove(Output{TxID: b.TxID, Vout: b.Vout}); err != nil || !ok {
		t.Fatalf("removing a held coin: %v, %v", ok, err)
	}
	disk, err := LoadPool(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := disk.Outputs(); len(got) != 2 || got[0] != a || got[1] != c || p.Balance() != 12 {
		t.Fatalf("after Remove the file holds %+v", got)
	}
	if ok, err := p.Remove(b); err != nil || ok {
		t.Fatalf("removing a coin twice: %v, %v", ok, err)
	}
	if ok, err := p.Remove(out(9, 0, 1, 1, false)); err != nil || ok || p.Count() != 2 {
		t.Fatalf("removing a coin never held: %v, %v, %d held", ok, err, p.Count())
	}
	// A removed coin may be returned: Return adds what is not held.
	if err := p.Return(b); err != nil || p.Count() != 3 {
		t.Fatalf("returning a removed coin: %v, %d held", err, p.Count())
	}
}

// A Remove that cannot be saved leaves the coin held, in its place, so the
// pool in memory is the pool on disk.
func TestRemoveThatCannotSaveKeepsTheCoin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wallet")
	p, err := LoadPool(filepath.Join(dir, "outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := out(1, 0, 5, 1, false), out(2, 0, 6, 2, false)
	if _, err := p.Add(a, b); err != nil {
		t.Fatal(err)
	}
	// The directory is replaced by a file, so no temp file can be created.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := p.Remove(a)
	if err == nil || ok {
		t.Fatalf("a Remove that could not save: %v, %v", ok, err)
	}
	if got := p.Outputs(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("the pool after a failed Remove: %+v", got)
	}
}

// Remove is safe beside the pool's other methods.
func TestRemoveIsConcurrencySafe(t *testing.T) {
	p := newPool(t)
	var coins []Output
	for i := 0; i < 16; i++ {
		coins = append(coins, out(byte(i+1), 0, 5, uint32(i), false))
	}
	if _, err := p.Add(coins...); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	removed := make([]bool, len(coins))
	for i, c := range coins {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ok, err := p.Remove(c)
			if err != nil {
				t.Error(err)
			}
			removed[i] = ok
		}()
		go func() {
			defer wg.Done()
			_ = p.Balance()
			_ = p.Outputs()
		}()
	}
	wg.Wait()
	for i, ok := range removed {
		if !ok {
			t.Fatalf("coin %d was not removed", i)
		}
	}
	disk, err := LoadPool(p.Path())
	if err != nil || disk.Count() != 0 || p.Count() != 0 {
		t.Fatalf("after every Remove: %d on disk, %d held, %v", disk.Count(), p.Count(), err)
	}
}
