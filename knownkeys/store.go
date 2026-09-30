package knownkeys

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"
)

// ErrPermissions reports a store other users could write to. It is refused,
// ssh-style, because a pin somebody else can edit is not a pin.
var ErrPermissions = errors.New("known_keys: refusing a file that is group or world writable")

// Load reads the store at path. A missing file is an empty store, not an
// error: first contact for everything is the correct state of a fresh
// install.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is mode %04o", ErrPermissions, path, st.Mode().Perm())
	}
	return Parse(f)
}

// syncFile is a seam so a test can prove the fsync happens, and happens before
// the rename; production always calls (*os.File).Sync.
var syncFile = (*os.File).Sync

// syncDir makes a rename durable.
//
// Syncing the temp file guarantees its BYTES survive a power loss; it says
// nothing about the directory entry that points at them. Until the parent
// directory is synced the rename can be lost, which restores the previous
// file, or half-applied, which leaves neither. The cost is one fsync on a
// directory and it is paid on a path that runs once per pin change.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFile(d)
}

// Save writes the store atomically: the directory is created 0700 if
// missing, the bytes go to a temporary file in the same directory at 0600,
// fsync puts them on the disk, and a rename makes them visible. A reader never
// sees a half-written pin.
//
// The file is header, then one Line per record, and nothing else. The header
// is the application's first line, a comment naming the file and its version
// such as "# example known_keys v1", passed without its line break. It is
// required: an empty header is refused rather than written as a file with no
// header line, because a caller that left it out would otherwise write stores
// that no longer say what they are. A header that is not a comment, or that
// holds a line break, is refused as well, because Parse would read it, or
// what follows the break, as a record line: one it refuses fails every later
// Load, and one it accepts is a pin nobody pinned. Each refusal comes before
// anything touches the disk.
//
// The fsync is not belt and braces here. A rename is atomic in VISIBILITY, not
// in durability, so a crash can leave the rename on disk and the bytes not,
// and this is the one file whose loss fails OPEN: an empty or truncated store
// reads as first contact for every address it used to pin, and the
// application that would have refused an impostor greets it instead.
func Save(path, header string, recs []Record) error {
	if !strings.HasPrefix(header, "#") || strings.ContainsAny(header, "\r\n") {
		return fmt.Errorf("known_keys: header %q must be one comment line starting with #", header)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')
	for _, r := range recs {
		b.WriteString(r.Line())
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, ".known_keys-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := syncFile(tmp); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// canonicalKey refuses a key Pin or Rotate would write that Parse would not
// read back, or that is not the one lower-case spelling of its point: pins
// are compared as strings.
func canonicalKey(keyHex string) error {
	if _, err := guard.ParsePubKeyHex(keyHex); err != nil {
		return fmt.Errorf("known_keys: %w", err)
	}
	if keyHex != strings.ToLower(keyHex) {
		return fmt.Errorf("known_keys: key %q is not lower-case hex", keyHex)
	}
	return nil
}

// Pin records a first contact or an advance of an existing pin: the active
// record for address becomes keyHex at seq, with first/last stamped. Any
// previous active record for the address with the SAME key is replaced in
// place; one with a DIFFERENT key is an error, because replacing it silently
// is precisely what this file exists to prevent (use Rotate or Forget). A
// key that is not the one canonical compressed encoding of its point, in
// lower-case hex, is refused.
func Pin(recs []Record, address, keyHex string, seq uint64, fp string, now time.Time) ([]Record, error) {
	if err := canonicalKey(keyHex); err != nil {
		return nil, err
	}
	now = now.UTC()
	for i, r := range recs {
		if r.Address != address || r.Kind != Active {
			continue
		}
		if r.KeyHex != keyHex {
			return nil, fmt.Errorf("known_keys: %s is pinned to a different key; refusing to replace it", address)
		}
		if seq < r.Seq {
			return nil, fmt.Errorf("known_keys: %s sequence %d is below the pinned %d", address, seq, r.Seq)
		}
		recs[i].Seq, recs[i].Last, recs[i].Fingerprint = seq, now, fp
		return recs, nil
	}
	return append(recs, Record{Kind: Active, Address: address, Algo: "secp256k1", KeyHex: keyHex,
		Seq: seq, First: now, Last: now, Fingerprint: fp}), nil
}

// Rotate replaces the active key for address with newKeyHex after a verified
// rotation: the old record becomes @rotated-from history with until_seq set
// to the last sequence it covered, and a fresh active record is appended.
// The new key is held to Pin's rule.
func Rotate(recs []Record, address, newKeyHex string, seq uint64, fp string, now time.Time) ([]Record, error) {
	if err := canonicalKey(newKeyHex); err != nil {
		return nil, err
	}
	now = now.UTC()
	out := make([]Record, 0, len(recs)+1)
	var rotated bool
	for _, r := range recs {
		if r.Address == address && r.Kind == Active {
			out = append(out, Record{Kind: RotatedFrom, Address: address, Algo: r.Algo, KeyHex: r.KeyHex,
				UntilSeq: seq - 1, At: now})
			rotated = true
			continue
		}
		out = append(out, r)
	}
	if !rotated {
		return nil, fmt.Errorf("known_keys: %s has no active pin to rotate", address)
	}
	return append(out, Record{Kind: Active, Address: address, Algo: "secp256k1", KeyHex: newKeyHex,
		Seq: seq, First: now, Last: now, Fingerprint: fp}), nil
}

// Retire marks address retired: the active record becomes @retired, which
// refuses every later answer for it.
func Retire(recs []Record, address string, now time.Time) []Record {
	now = now.UTC()
	for i, r := range recs {
		if r.Address == address && r.Kind == Active {
			recs[i].Kind, recs[i].At, recs[i].Seq, recs[i].First, recs[i].Last = Retired, now, 0, time.Time{}, time.Time{}
		}
	}
	return recs
}

// Forget removes the pin in force for address, active or retired; with all,
// its @rotated-from history too. History is kept by default because it is
// what explains a later first contact to whoever reads the file.
func Forget(recs []Record, address string, all bool) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if r.Address == address && (all || r.Kind != RotatedFrom) {
			continue
		}
		out = append(out, r)
	}
	return out
}
