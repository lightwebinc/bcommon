package publish

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Journal keeps one record per transition of a publish's two legs, so what
// happened to a transition outlives the process that sent it.
//
// Each entry is one file, <seq>-<txid>.json, mode 0600, written atomically,
// so an entry's update never rewrites another entry. The key is (seq, txid)
// rather than seq alone because a sequence retried after a failure leaves
// one entry per attempt. List refuses a .json file that is not a readable
// <seq>-<txid> entry rather than skip it, and Update may not change an
// entry's key.
type Journal struct {
	// Dir holds the entries; Write creates it when it is missing.
	Dir string
}

// Entry is one transition's record. The journal reads only Seq and TxID,
// which key the entry; every other field holds what the caller writes.
type Entry struct {
	// Acct is the publisher's account, such as a BRC-169 handle@domain, as
	// the application writes it.
	Acct string `json:"acct"`
	// IdentityKey is the publisher's identity key, in the application's
	// encoding.
	IdentityKey string `json:"identityKey"`
	// Seq is the transition's sequence number, the first half of the key.
	Seq uint64 `json:"seq"`
	// Kind is the application's label for the transition.
	Kind string `json:"kind"`
	// TxID is the id of the transaction the settlement leg sends, 64 hex
	// characters, the second half of the key.
	TxID string `json:"txid"`
	// CarrierTxID is the id of the carrier the transition commits to, when
	// it has one.
	CarrierTxID string `json:"carrierTxid,omitempty"`
	// EFSentAt is when the settlement leg took the transaction, and EFError
	// why it did not.
	EFSentAt *time.Time `json:"efSentAt,omitempty"`
	EFError  string     `json:"efError,omitempty"`
	// BEEFSentAt is when the object leg's host answered, BEEFSteak that
	// answer as received, and BEEFError why the leg failed.
	BEEFSentAt *time.Time      `json:"beefSentAt,omitempty"`
	BEEFSteak  json.RawMessage `json:"beefSteak,omitempty"`
	BEEFError  string          `json:"beefError,omitempty"`
	// MinedAt is when the transaction was seen mined, and Height the block
	// height it was mined at.
	MinedAt *time.Time `json:"minedAt,omitempty"`
	Height  uint32     `json:"height,omitempty"`
}

// ErrNoEntry is returned by Read and Update for a sequence that has no file.
var ErrNoEntry = errors.New("publish: no journal entry")

func validTxID(txid string) bool {
	if len(txid) != 64 {
		return false
	}
	_, err := hex.DecodeString(txid)
	return err == nil
}

func (j Journal) path(seq uint64, txid string) string {
	return filepath.Join(j.Dir, strconv.FormatUint(seq, 10)+"-"+txid+".json")
}

// Write creates or replaces the entry for (e.Seq, e.TxID).
func (j Journal) Write(e Entry) error {
	if !validTxID(e.TxID) {
		// The txid is a path component. A value that is not 64 hex
		// characters is refused rather than sanitised, because a sanitised
		// name no longer says which transaction it is.
		return fmt.Errorf("publish: journal: txid %q is not 64 hex characters", e.TxID)
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(j.path(e.Seq, e.TxID), raw, 0o600)
}

// Read returns one entry.
func (j Journal) Read(seq uint64, txid string) (Entry, error) {
	if !validTxID(txid) {
		return Entry{}, fmt.Errorf("publish: journal: txid %q is not 64 hex characters", txid)
	}
	return j.readFile(j.path(seq, txid))
}

func (j Journal) readFile(path string) (Entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("%w: %s", ErrNoEntry, filepath.Base(path))
	}
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, fmt.Errorf("publish: journal %s: %w", filepath.Base(path), err)
	}
	return e, nil
}

// Update reads the entry, applies mutate and writes it back.
func (j Journal) Update(seq uint64, txid string, mutate func(*Entry)) error {
	e, err := j.Read(seq, txid)
	if err != nil {
		return err
	}
	mutate(&e)
	if e.Seq != seq || e.TxID != txid {
		return fmt.Errorf("publish: journal: update may not change seq or txid")
	}
	return j.Write(e)
}

// List returns every entry sorted by sequence, then txid. Directories,
// dotfiles (a write in progress) and names without a .json suffix are not
// entries and are passed over. A .json file that is not a readable
// <seq>-<txid> entry is an ERROR, not a skip: a stranded sequence is exactly
// the one an operator needs shown, and a diagnostic that quietly skipped the
// file it could not parse would report a clean history over a broken one.
func (j Journal) List() ([]Entry, error) {
	ents, err := os.ReadDir(j.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range ents {
		name := d.Name()
		if d.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		seqStr, rest, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !ok || !validTxID(rest) {
			return nil, fmt.Errorf("publish: journal: %s is not <seq>-<txid>.json", name)
		}
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("publish: journal: %s: %w", name, err)
		}
		e, err := j.readFile(filepath.Join(j.Dir, name))
		if err != nil {
			return nil, err
		}
		if e.Seq != seq || e.TxID != rest {
			return nil, fmt.Errorf("publish: journal: %s names seq %d txid %s but holds seq %d txid %s", name, seq, rest, e.Seq, e.TxID)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seq != out[b].Seq {
			return out[a].Seq < out[b].Seq
		}
		return out[a].TxID < out[b].TxID
	})
	return out, nil
}

// writeFileAtomic is the same discipline bwallet uses for its files: a temp
// file in the target's directory, synced, renamed over the target. The
// helper is repeated here rather than shared because this package must not
// import the wallet to write a log line, and a shared internal package for
// twenty lines is a coupling nobody asked for.
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
