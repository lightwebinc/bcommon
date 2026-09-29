package knownkeys

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const k1 = "0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c"
const k2 = "02d8b9f4b27f4b5a6a0d8f29c1a7e6a5d4c3b2a1908f7e6d5c4b3a2918f7e6d5c4"

// header is the first line these tests' stores carry. It is the sample's, and
// deliberately no application's.
const header = "# example known_keys v1"

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "known_keys")
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	recs, err := Pin(nil, "alice@example.com", k1, 1, "SHA256:abc", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, header, recs); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o", st.Mode().Perm())
	}
	if dst, _ := os.Stat(filepath.Dir(path)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %04o", dst.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].KeyHex != k1 || got[0].Seq != 1 || !got[0].First.Equal(now) || got[0].Fingerprint != "SHA256:abc" {
		t.Fatalf("%+v", got)
	}
	// What Save writes, Parse reads: the line grammar is the contract.
	if got[0].Line() != recs[0].Line() {
		t.Fatal("line changed across a round trip")
	}
}

func TestLoadRefusesLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_keys")
	if err := os.WriteFile(path, []byte("alice@example.com secp256k1 "+k1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod, not the WriteFile mode: the mode passed to WriteFile is masked
	// by the process umask, so under the common umask 022 the file would come
	// out 0644 and this test would assert nothing. Group and world are tried
	// apart as well as together: either one alone is a pin somebody else can
	// edit.
	for _, mode := range []os.FileMode{0o666, 0o620, 0o602} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); !errors.Is(err, ErrPermissions) {
			t.Fatalf("a store at mode %04o: %v, want ErrPermissions", mode, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if recs, err := Load(filepath.Join(t.TempDir(), "missing")); err != nil || recs != nil {
		t.Fatal("a missing store must be empty, not an error")
	}
	// Permissions come before parsing: a loose store that also holds a
	// malformed line is refused as loose.
	bad := filepath.Join(t.TempDir(), "known_keys")
	if err := os.WriteFile(bad, []byte("alice@example.com secp256k1 02a1b2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); !errors.Is(err, ErrPermissions) {
		t.Fatalf("a loose store holding a malformed line: %v, want ErrPermissions", err)
	}
}

func TestPinRotateRetireForget(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	recs, _ := Pin(nil, "a@example.com", k1, 1, "fp1", now)
	// Advancing the same key moves seq and last.
	recs, err := Pin(recs, "a@example.com", k1, 3, "fp1", now.Add(time.Hour))
	if err != nil || recs[0].Seq != 3 || !recs[0].Last.Equal(now.Add(time.Hour)) {
		t.Fatalf("advance: %v %+v", err, recs)
	}
	// A rollback and a silent key swap are both refused.
	if _, err := Pin(recs, "a@example.com", k1, 2, "fp1", now); err == nil {
		t.Fatal("rollback accepted")
	}
	if _, err := Pin(recs, "a@example.com", k2, 4, "fp2", now); err == nil {
		t.Fatal("key swap accepted by Pin")
	}
	// A swap that is also a rollback is refused as the swap: a different key
	// is the refusal that matters, whatever sequence it arrives with.
	if _, err := Pin(recs, "a@example.com", k2, 2, "fp2", now); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("a swap at a lower sequence: %v, want the different-key refusal", err)
	}
	// Rotation keeps history and never satisfies a pin again.
	recs, err = Rotate(recs, "a@example.com", k2, 4, "fp2", now)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := ActiveFor(recs, "a@example.com"); !ok || a.KeyHex != k2 || a.Seq != 4 {
		t.Fatalf("active after rotate: %+v", a)
	}
	var hist int
	for _, r := range recs {
		if r.Kind == RotatedFrom && r.KeyHex == k1 && r.UntilSeq == 3 {
			hist++
		}
	}
	if hist != 1 {
		t.Fatalf("history lines: %d", hist)
	}
	// What the transitions leave is what the store will hold, so the lines are
	// compared whole: history carries until_seq and the time it was
	// superseded, and no sequence of its own.
	if got, want := linesOf(recs), "@rotated-from a@example.com secp256k1 "+k1+" until_seq=3 at=2026-09-24T12:00:00Z\n"+
		"a@example.com secp256k1 "+k2+" seq=4 first=2026-09-24T12:00:00Z last=2026-09-24T12:00:00Z fp=fp2"; got != want {
		t.Fatalf("after rotate:\n%s\nwant\n%s", got, want)
	}
	if _, err := Rotate(recs, "nobody@example.com", k2, 1, "", now); err == nil {
		t.Fatal("rotating an unpinned address succeeded")
	}
	// Retire is a pin that refuses; Forget without all keeps history.
	recs = Retire(recs, "a@example.com", now)
	if _, ok := ActiveFor(recs, "a@example.com"); ok {
		t.Fatal("retired address still active")
	}
	if r, _ := ActiveFor(recs, "a@example.com"); r.Kind != Retired {
		t.Fatal("retirement record not returned")
	}
	// A retirement keeps the key and its fingerprint and the time, and drops
	// the sequence and the contact times, which no longer describe a pin in
	// force.
	if got, want := linesOf(recs), "@rotated-from a@example.com secp256k1 "+k1+" until_seq=3 at=2026-09-24T12:00:00Z\n"+
		"@retired a@example.com secp256k1 "+k2+" at=2026-09-24T12:00:00Z fp=fp2"; got != want {
		t.Fatalf("after retire:\n%s\nwant\n%s", got, want)
	}
	recs = Forget(recs, "a@example.com", false)
	if len(recs) != 1 || recs[0].Kind != RotatedFrom {
		t.Fatalf("forget kept the wrong lines: %+v", recs)
	}
	recs = Forget(recs, "a@example.com", true)
	if len(recs) != 0 {
		t.Fatal("forget with all left lines")
	}
}

// Advancing a pin in place moves its sequence, its last-contact time and its
// fingerprint together, and leaves first contact where it was. A repeat at the
// pinned sequence is an advance too, not a rollback.
func TestPinAdvanceMovesTheFingerprint(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	recs, err := Pin(nil, "a@example.com", k1, 1, "fp1", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{3, 3} {
		if recs, err = Pin(recs, "a@example.com", k1, seq, "fp3", now.Add(time.Hour)); err != nil {
			t.Fatalf("advance to %d: %v", seq, err)
		}
	}
	if got, want := linesOf(recs), "a@example.com secp256k1 "+k1+" seq=3 first=2026-09-24T12:00:00Z last=2026-09-24T13:00:00Z fp=fp3"; got != want {
		t.Fatalf("after the advance:\n%s\nwant\n%s", got, want)
	}
}

// A retirement is not the pin in force, so Pin neither advances it nor
// refuses it as a different key: it appends a fresh active pin after it and
// leaves the retirement as it was, whichever key the new pin carries. That is
// the retirement-then-active order Parse is tested to read with the active
// line as the answer.
func TestPinAfterARetirementAppendsAndLeavesIt(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	retired := "@retired a@example.com secp256k1 " + k1 + " at=2026-09-24T11:00:00Z fp=fp1"
	for _, key := range []string{k1, k2} {
		recs, err := Parse(strings.NewReader(retired + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		recs, err = Pin(recs, "a@example.com", key, 5, "fp5", now)
		if err != nil {
			t.Fatalf("key %s: pinning a retired address: %v", key[:8], err)
		}
		want := retired + "\na@example.com secp256k1 " + key + " seq=5 first=2026-09-24T12:00:00Z last=2026-09-24T12:00:00Z fp=fp5"
		if got := linesOf(recs); got != want {
			t.Fatalf("key %s: after the pin:\n%s\nwant\n%s", key[:8], got, want)
		}
		back, err := Parse(strings.NewReader(linesOf(recs) + "\n"))
		if err != nil {
			t.Fatalf("key %s: the store Pin left does not parse: %v", key[:8], err)
		}
		if got, ok := ActiveFor(back, "a@example.com"); !ok || got.KeyHex != key || got.Seq != 5 {
			t.Fatalf("key %s: ActiveFor = %+v, %v; want the new pin", key[:8], got, ok)
		}
	}
}

// Rotate needs an active pin to supersede. An address that holds only a
// retirement, or only history, has none, and is refused in the same words as
// one never pinned: rotating a retirement would re-trust an identity that
// was retired on purpose.
func TestRotateRefusesAnAddressWithNoActivePin(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for name, line := range map[string]string{
		"retired": "@retired a@example.com secp256k1 " + k1 + " at=2026-09-24T11:00:00Z fp=fp1",
		"history": "@rotated-from a@example.com secp256k1 " + k1 + " until_seq=4 at=2026-09-24T11:00:00Z",
	} {
		recs, err := Parse(strings.NewReader(line + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := Rotate(recs, "a@example.com", k2, 5, "fp2", now)
		if want := "known_keys: a@example.com has no active pin to rotate"; err == nil || err.Error() != want {
			t.Fatalf("%s: Rotate = %v, %v; want the refusal %q", name, linesOf(out), err, want)
		}
		if got := linesOf(recs); got != line {
			t.Fatalf("%s: a refused rotation changed the store to\n%s", name, got)
		}
	}
}

func linesOf(recs []Record) string {
	var l []string
	for _, r := range recs {
		l = append(l, r.Line())
	}
	return strings.Join(l, "\n")
}

// A rename gives atomicity of VISIBILITY, not durability: after a crash the
// rename can be on disk while the bytes are not. For this file that fails
// OPEN, because an empty or short store reads as first contact for every
// address, so the pin has to be durable before it is made visible.
func TestSaveSyncsBeforeTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_keys")
	orig := syncFile
	defer func() { syncFile = orig }()
	// Two syncs, in this order: the temp FILE before the rename, so the bytes
	// survive; then the DIRECTORY after it, so the rename itself survives.
	// Syncing only the file leaves a durable set of bytes that nothing points
	// at, which loses the pin just as completely.
	type call struct {
		name         string
		renamedFirst bool
		contents     string
	}
	var calls []call
	syncFile = func(f *os.File) error {
		_, err := os.Stat(path)
		c := call{name: f.Name(), renamedFirst: err == nil}
		if b, err := os.ReadFile(f.Name()); err == nil {
			c.contents = string(b)
		}
		calls = append(calls, c)
		return orig(f)
	}
	recs, err := Pin(nil, "alice@example.com", k1, 1, "SHA256:abc", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, header, recs); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("Save fsynced %d times; want the temp file then the directory", len(calls))
	}
	if calls[0].renamedFirst {
		t.Fatal("the file fsync must happen before the rename, not after it")
	}
	if !strings.Contains(calls[0].contents, k1) {
		t.Fatal("the pin was not written before the fsync")
	}
	if !calls[1].renamedFirst {
		t.Fatal("the directory fsync must happen after the rename, or it makes nothing durable")
	}
	if calls[1].name != filepath.Dir(path) {
		t.Fatalf("second fsync was %q, want the parent directory %q", calls[1].name, filepath.Dir(path))
	}
}

// A store that could not be made durable is not a store. Renaming it into
// place anyway would publish a file that a crash can empty, and an empty pin
// file trusts everything on first contact.
func TestSaveLeavesNothingWhenTheSyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_keys")
	orig := syncFile
	defer func() { syncFile = orig }()
	syncFile = func(*os.File) error { return errors.New("no space left on device") }
	recs, err := Pin(nil, "alice@example.com", k1, 1, "SHA256:abc", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, header, recs); err == nil {
		t.Fatal("a store that was never made durable was reported saved")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed save left a pin file: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("a failed save left %d file(s) behind", len(ents))
	}
}
