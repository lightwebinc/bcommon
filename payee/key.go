package payee

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// IdentityFile is the file in a home that holds its root key, {"wif": ...},
// as package bwallet writes it. The payee key is that key: the home is then
// the payee's wallet, and settled payments go to its pool.
const IdentityFile = "identity.json"

// PrintedKeyNote is what an application says, on its warning stream, after
// it prints KeyLine to its output.
const PrintedKeyNote = "the line above is a private key: keep it out of logs and shells' history"

// KeyEnv is the environment variable a host of app takes its payee key in:
// for "app", APP_PAYEE_KEY.
func KeyEnv(app string) string {
	return strings.ToUpper(app) + "_PAYEE_KEY"
}

// HomeKey is the payee key of the home at dir, read from its IdentityFile.
// app names the command that makes a home, in the error for a home that
// has none.
func HomeKey(dir, app string) (*ec.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IdentityFile)) //nolint:gosec // the operator's own home
	if err != nil {
		return nil, fmt.Errorf("open the home %s: %w (run `%s init`)", dir, err, app)
	}
	var id struct {
		WIF string `json:"wif"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("%s: %w", IdentityFile, err)
	}
	k, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", IdentityFile, err)
	}
	return k, nil
}

// KeyLine is the environment line that hands a host its payee key,
// env=<the key, 64 lowercase hex characters>, newline included.
func KeyLine(env string, k *ec.PrivateKey) string {
	return fmt.Sprintf("%s=%s\n", env, hex.EncodeToString(k.Serialize()))
}

// ErrKeyFileExists is a key file that is already there: a key is never
// written over a file.
var ErrKeyFileExists = errors.New("payee: the key file exists")

// CreateKeyFile writes KeyLine to a new file at path, mode 0600. A file
// already at path is left as it is, and the error is ErrKeyFileExists (and
// os.ErrExist). It returns what an application says on its output once it
// has: "wrote <env> to <path> for payee <compressed public key>".
func CreateKeyFile(path, env string, k *ec.PrivateKey) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's own path
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: %w", ErrKeyFileExists, err)
		}
		return "", err
	}
	if _, err := f.WriteString(KeyLine(env, k)); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s to %s for payee %s", env, path, hex.EncodeToString(k.PubKey().Compressed())), nil
}
