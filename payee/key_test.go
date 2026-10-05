package payee

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/bwallet"
)

// The payee key is the home's root key, handed to a host as one line.
func TestHomeKeyAndItsLine(t *testing.T) {
	dir := t.TempDir()
	e, err := bwallet.Create(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	k, err := HomeKey(dir, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(k.PubKey().Compressed()) != e.Signer().IdentityHex() {
		t.Fatal("the payee key is not the home's identity")
	}
	env := KeyEnv("sample")
	line := KeyLine(env, k)
	if env != "SAMPLE_PAYEE_KEY" || line != "SAMPLE_PAYEE_KEY="+hex.EncodeToString(k.Serialize())+"\n" || len(line) != len(env)+1+64+1 {
		t.Fatalf("line %q", line)
	}
	out := filepath.Join(t.TempDir(), "payee.env")
	said, err := CreateKeyFile(out, env, k)
	if err != nil {
		t.Fatal(err)
	}
	if said != "wrote SAMPLE_PAYEE_KEY to "+out+" for payee "+e.Signer().IdentityHex() {
		t.Fatalf("said %q", said)
	}
	raw, err := os.ReadFile(out)
	if err != nil || string(raw) != line {
		t.Fatalf("the file holds %q", raw)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Never over a file.
	if err := os.WriteFile(out, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateKeyFile(out, env, k); !errors.Is(err, ErrKeyFileExists) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("over a file: %v", err)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "kept\n" {
		t.Fatal("the file was written over")
	}
	if _, err := CreateKeyFile(filepath.Join(out, "x"), env, k); err == nil || errors.Is(err, ErrKeyFileExists) {
		t.Fatalf("under a file: %v", err)
	}
}

// A home that is not one says what to run.
func TestHomeKeyOfNoHome(t *testing.T) {
	dir := t.TempDir()
	if _, err := HomeKey(dir, "sample"); err == nil || !errors.Is(err, os.ErrNotExist) ||
		!strings.HasPrefix(err.Error(), "open the home "+dir+": ") || !strings.HasSuffix(err.Error(), " (run `sample init`)") {
		t.Fatalf("%v", err)
	}
	for body, want := range map[string]string{
		"{":                  "identity.json: unexpected end of JSON input",
		`{"wif":"nonsense"}`: "identity.json: ",
	} {
		if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := HomeKey(dir, "sample"); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
}
