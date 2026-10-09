package payee

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BookFile is a Book kept in a JSON file of its own, for a home whose state
// file has no place to embed one. The file holds the Book's two keys and
// nothing else; Save replaces it whole, through a temporary file beside it,
// at mode 0600.
type BookFile struct {
	Book
	path string
}

// OpenBookFile reads the book at path. A missing file is an empty book,
// written on the first Save.
func OpenBookFile(path string) (*BookFile, error) {
	f := &BookFile{path: path}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's own home
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &f.Book); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return f, nil
}

// Save writes the book to its file.
func (f *BookFile) Save() error {
	raw, err := json.MarshalIndent(&f.Book, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), filepath.Base(f.path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}

// Record is the book as a Record that saves the file after each change.
func (f *BookFile) Record() Record { return Saved(&f.Book, f.Save) }
