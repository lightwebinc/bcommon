package knownkeys

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Save writes the caller's header and one line per record, and nothing else:
// the sample's comment and blank lines are not records, so they do not come
// back, and every record line comes back byte for byte.
func TestSaveWritesTheHeaderThenOneLinePerRecord(t *testing.T) {
	raw, err := os.ReadFile("testdata/known_keys.sample")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	path := filepath.Join(t.TempDir(), "known_keys")
	save := func(h string, recs []Record) string {
		t.Helper()
		if err := Save(path, h, recs); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}
	body := strings.Join(lines, "\n") + "\n"
	if got, want := save(header, recs), header+"\n"+body; got != want {
		t.Fatalf("Save wrote\n%s\nwant\n%s", got, want)
	}
	// The header is the caller's, not a default: another one changes the
	// first line and nothing else.
	if got, want := save("# another known_keys v2", recs), "# another known_keys v2\n"+body; got != want {
		t.Fatalf("under another header Save wrote\n%s\nwant\n%s", got, want)
	}
	// An empty store is the header alone, not an empty file.
	if got := save(header, nil); got != header+"\n" {
		t.Fatalf("empty store saved as %q", got)
	}
}

// A header Save cannot write as one comment line is refused, and refused
// before the directory is made: a store with no header line no longer says
// what it is, and a header that is not a comment, or that breaks a line,
// reads back as a record.
func TestSaveRefusesAHeaderThatIsNotOneCommentLine(t *testing.T) {
	recs, err := Pin(nil, "alice@example.com", k1, 1, "SHA256:abc", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]string{
		"empty":            "",
		"not a comment":    "example known_keys v1",
		"a line break":     header + "\nmallory@example.com secp256k1 " + k2,
		"a carriage break": header + "\r",
	} {
		dir := filepath.Join(t.TempDir(), "sub")
		err := Save(filepath.Join(dir, "known_keys"), h, recs)
		if err == nil {
			t.Errorf("%s: header %q was accepted", name, h)
			continue
		}
		if !strings.Contains(err.Error(), "header") {
			t.Errorf("%s: the refusal should say it is the header: %v", name, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: a refused save touched the disk: %v", name, err)
		}
	}
	// Control: the same save under a header that is one comment line lands.
	path := filepath.Join(t.TempDir(), "sub", "known_keys")
	for _, h := range []string{header, "#"} {
		if err := Save(path, h, recs); err != nil {
			t.Fatalf("header %q was refused: %v", h, err)
		}
	}
	if got, err := Load(path); err != nil || len(got) != 1 {
		t.Fatalf("the control store does not load back: %v %+v", err, got)
	}
}
