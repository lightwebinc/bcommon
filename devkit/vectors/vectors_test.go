package vectors

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteThenCheck(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"b-v1.json": []byte("{\"b\":1}\n"), "a-v1.json": []byte("{\"a\":1}\n")}
	var errb bytes.Buffer
	if code := Main([]string{"-dir", dir}, files, nil, Options{}, &errb); code != 0 {
		t.Fatalf("write: %d %s", code, errb.String())
	}
	if code := Main([]string{"-dir", dir, "-check"}, files, nil, Options{}, &errb); code != 0 || errb.Len() != 0 {
		t.Fatalf("check: %d %s", code, errb.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "a-v1.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-v1.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"-dir", dir, "-check"}, files, nil, Options{}, &errb); code != 1 || errb.String() != "differs: "+filepath.Join(dir, "a-v1.json")+"\n" {
		t.Fatalf("a changed file: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := Main([]string{"-dir", dir, "-check"}, files, nil, Options{Strict: true}, &errb); code != 1 ||
		!strings.Contains(errb.String(), "not generated: "+filepath.Join(dir, "old-v1.json")) {
		t.Fatalf("strict: %d %q", code, errb.String())
	}
	if code := Main(nil, nil, errors.New("codec"), Options{}, &errb); code != 1 {
		t.Fatalf("a generator failure: %d", code)
	}
	if code := Main([]string{"-x"}, files, nil, Options{}, &errb); code != 2 {
		t.Fatalf("a bad flag: %d", code)
	}
}
