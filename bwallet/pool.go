package bwallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
)

// CoinbaseMaturity is how many blocks must bury a coinbase before it may be
// spent. A pool output is spendable when Height+CoinbaseMaturity <= tip.
const CoinbaseMaturity = 100

// ErrNoSpendable is returned by Take when nothing in the pool is spendable at
// the given tip. Immature coinbase is the usual reason; Immature says so.
var ErrNoSpendable = errors.New("bwallet: no spendable output in the wallet")

// Output is one spendable output the wallet holds. Raw and Bump are optional:
// a coinbase output straight from generatetoaddress has neither until its
// block's proof is fetched, and a spend that needs to be a BEEF ancestor asks
// for them then.
//
// Height and Coinbase are fields of their own because maturity is what
// decides whether an output is spendable.
type Output struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Satoshis      uint64 `json:"satoshis"`
	LockingScript string `json:"lockingScript"`
	Height        uint32 `json:"height,omitempty"`
	Coinbase      bool   `json:"coinbase,omitempty"`
	Raw           string `json:"raw,omitempty"`
	Bump          string `json:"bump,omitempty"`
	// Unproven marks change from a transaction that was published before it
	// mined. Such a coin is not taken until its parent's proof arrives: a
	// transaction spending it would otherwise carry an unproven parent in
	// its BEEF, and that parent's own inputs after it, so one quick update
	// after another would publish an ever-deeper chain of unmined ancestry.
	// Holding it back keeps every fee input one hop from a proven parent.
	Unproven bool `json:"unproven,omitempty"`
	// Derivation names the key that spends this output when it is not the
	// fund key: a payment received under BRC-29 is locked to a key derived
	// from the sender's identity, and only the wallet that owns Owner can
	// re-derive it.
	Derivation *Derivation `json:"derivation,omitempty"`
}

// Derivation is a BRC-42/43 derivation an output was locked with, enough to
// re-derive the spending key: the protocol triple, the key id, and the
// counterparty whose shared secret the derivation used.
type Derivation struct {
	SecurityLevel   int    `json:"level"`
	Protocol        string `json:"protocol"`
	KeyID           string `json:"keyID"`
	CounterpartyHex string `json:"counterparty"`
	// OwnerHex is the identity that can derive the private key.
	OwnerHex string `json:"owner"`
}

// Outpoint renders txid.vout, the BRC-100 form.
func (o Output) Outpoint() string { return fmt.Sprintf("%s.%d", o.TxID, o.Vout) }

// Spendable reports whether the output may be spent at tip. Only coinbase
// has a maturity rule; everything else is spendable on arrival.
func (o Output) Spendable(tip uint32) bool {
	if o.Unproven {
		return false
	}
	if !o.Coinbase {
		return true
	}
	return uint64(o.Height)+CoinbaseMaturity <= uint64(tip)
}

type poolJSON struct {
	Outputs []Output `json:"outputs"`
}

// Pool is the on-disk funding pool. Every mutation that changes what may be
// spent (Add, Take, Return, Remove) saves before it returns: a reservation that lives
// only in memory is a double spend after a crash.
type Pool struct {
	mu      sync.Mutex
	path    string
	outputs []Output
}

// LoadPool opens the pool at path, or an empty pool when the file does not
// exist yet.
func LoadPool(path string) (*Pool, error) {
	p := &Pool{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	var st poolJSON
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("bwallet: %s: %w", path, err)
	}
	p.outputs = st.Outputs
	p.sortLocked()
	return p, nil
}

// Path is the file the pool persists to.
func (p *Pool) Path() string { return p.path }

// sortLocked keeps the pool oldest first, so Take is "first spendable in
// order". Stable, so outputs from one block keep their arrival order.
func (p *Pool) sortLocked() {
	sort.SliceStable(p.outputs, func(i, j int) bool { return p.outputs[i].Height < p.outputs[j].Height })
}

// Add appends outputs the pool does not already hold and saves. It returns
// how many were new. Duplicates are keyed by outpoint, so a rescan over
// blocks already funded from is a no-op rather than a double entry.
func (p *Pool) Add(outs ...Output) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]struct{}, len(p.outputs))
	for _, o := range p.outputs {
		seen[o.Outpoint()] = struct{}{}
	}
	added := 0
	for _, o := range outs {
		k := o.Outpoint()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		p.outputs = append(p.outputs, o)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	p.sortLocked()
	return added, p.saveLocked()
}

// Take removes and returns the oldest output spendable at tip, saving the
// pool without it. The output is reserved from that moment; Return undoes it.
// A coin of zero satoshis is never taken: it can pay for nothing. Take is
// TakeAtLeast(tip, 1, nil).
func (p *Pool) Take(tip uint32) (Output, error) {
	return p.TakeAtLeast(tip, 1, nil)
}

// TakeAllowing is Take, falling back to an unproven coin whose parent is one
// of allow when no proven coin is spendable.
//
// A caller allows a parent its transaction already carries in full: a state
// token that the next transition spends as its previous-token input, or a
// funding tree its carrier spends. Taking that parent's change adds nothing
// to the spender's BEEF, so it does not deepen the unmined chain the hold on
// unproven change exists to bound, and it is what lets a wallet with a single
// coin publish twice inside one block. TakeAllowing is TakeAtLeast(tip, 1,
// allow).
func (p *Pool) TakeAllowing(tip uint32, allow []string) (Output, error) {
	return p.TakeAtLeast(tip, 1, allow)
}

// TakeAtLeast is TakeAllowing restricted to coins of at least minSats
// satoshis, for a caller that knows what the coin must pay: a coin below it
// is left in the pool rather than handed out to fail the build. A minSats of
// zero counts as one, since a coin of nothing pays for nothing. The oldest
// proven coin that is large enough is taken first, then the oldest allowed
// unproven one. When coins are held but none is large enough the error
// wraps ErrNoSpendable and says so.
func (p *Pool) TakeAtLeast(tip uint32, minSats uint64, allow []string) (Output, error) {
	minSats = max(minSats, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	small := false
	pick := func(ok func(Output) bool) (Output, bool, error) {
		for i, o := range p.outputs {
			if !ok(o) {
				continue
			}
			if o.Satoshis < minSats {
				small = true
				continue
			}
			p.outputs = append(p.outputs[:i:i], p.outputs[i+1:]...)
			if err := p.saveLocked(); err != nil {
				// Put it back so memory and disk agree on failure.
				p.outputs = append(p.outputs, o)
				p.sortLocked()
				return Output{}, true, err
			}
			return o, true, nil
		}
		return Output{}, false, nil
	}
	if o, found, err := pick(func(o Output) bool { return o.Spendable(tip) }); found {
		return o, err
	}
	if len(allow) > 0 {
		if o, found, err := pick(func(o Output) bool { return o.Unproven && slices.Contains(allow, o.TxID) }); found {
			return o, err
		}
	}
	if small {
		return Output{}, fmt.Errorf("%w of at least %d sat", ErrNoSpendable, minSats)
	}
	return Output{}, ErrNoSpendable
}

// Prove records that txid mined: every output of it the pool holds gets
// its proof and height and becomes spendable. It reports how many changed.
func (p *Pool) Prove(txid, bump string, height uint32) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.outputs {
		if p.outputs[i].TxID != txid || !p.outputs[i].Unproven {
			continue
		}
		p.outputs[i].Unproven, p.outputs[i].Bump, p.outputs[i].Height = false, bump, height
		n++
	}
	if n == 0 {
		return 0, nil
	}
	p.sortLocked()
	return n, p.saveLocked()
}

// UnprovenTxids is every distinct parent the pool is waiting on a proof for.
func (p *Pool) UnprovenTxids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, o := range p.outputs {
		if o.Unproven && !seen[o.TxID] {
			seen[o.TxID] = true
			out = append(out, o.TxID)
		}
	}
	return out
}

// Return puts a taken output back (a build or submit that did not happen).
func (p *Pool) Return(o Output) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, held := range p.outputs {
		if held.Outpoint() == o.Outpoint() {
			return nil
		}
	}
	p.outputs = append(p.outputs, o)
	p.sortLocked()
	return p.saveLocked()
}

// Remove drops the held output with o's outpoint and saves the pool without
// it, reporting whether one was held. It is for a coin the chain shows
// spent while the pool still holds it: Take reserves a coin for a spend the
// caller is about to make, and Remove forgets one a transaction already
// spent. An output the pool does not hold changes nothing and is not an
// error. When the save fails the output stays held, so memory and disk
// agree, and the error is returned.
func (p *Pool) Remove(o Output) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := o.Outpoint()
	for i, held := range p.outputs {
		if held.Outpoint() != k {
			continue
		}
		p.outputs = append(p.outputs[:i:i], p.outputs[i+1:]...)
		if err := p.saveLocked(); err != nil {
			p.outputs = append(p.outputs, held)
			p.sortLocked()
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// Immature lists the outputs that are held but not spendable at tip.
func (p *Pool) Immature(tip uint32) []Output {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Output
	for _, o := range p.outputs {
		if !o.Spendable(tip) {
			out = append(out, o)
		}
	}
	return out
}

// Outputs is a snapshot of everything held, oldest first.
func (p *Pool) Outputs() []Output {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Output, len(p.outputs))
	copy(out, p.outputs)
	return out
}

// Count is how many outputs are held, spendable or not.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.outputs)
}

// Balance is the satoshi total of everything held, spendable or not.
func (p *Pool) Balance() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sum uint64
	for _, o := range p.outputs {
		sum += o.Satoshis
	}
	return sum
}

// Save writes the pool to disk.
func (p *Pool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *Pool) saveLocked() error {
	raw, err := json.MarshalIndent(poolJSON{Outputs: p.outputs}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.path, raw, 0o600)
}

// writeFileAtomic writes data to a temp file in path's directory, syncs it
// and renames it over path. A rename within one directory is atomic on every
// filesystem this runs on, so a reader sees the old file or the new one and
// never a truncated one. The temp file is created 0600 and chmodded to perm,
// so even an interrupted write never leaves key material world-readable.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
