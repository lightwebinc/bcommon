package payee

import (
	"os"
	"path/filepath"
	"testing"
)

// A missing book file is an empty book; what is recorded survives a reopen,
// at mode 0600, and a file that does not decode is an error.
func TestBookFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payee.json")
	f, err := OpenBookFile(path)
	if err != nil || len(f.Settled) != 0 || len(f.Unsettleable) != 0 {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("opening wrote the file: %v", err)
	}
	r := f.Record()
	if err := r.RecordSettled("aa"); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordUnsettleable("bb", "spent elsewhere"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", st, err)
	}
	g, err := OpenBookFile(path)
	if err != nil || !g.IsSettled("aa") || !g.IsUnsettleable("bb") || g.IsSettled("bb") {
		t.Fatalf("reopened: %+v %v", g, err)
	}
	left, _ := filepath.Glob(path + ".*.tmp")
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBookFile(path); err == nil {
		t.Fatal("a file that does not decode opened")
	}
}
