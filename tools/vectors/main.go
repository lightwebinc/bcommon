// Command vectors writes the test vectors the library's packages check their
// own output against, and with -check proves that the committed vectors are
// exactly what it writes.
//
// It is an independent oracle, so it shares no code with the library. Its
// CBOR comes from fxamacker/cbor under the core deterministic encoding
// options, its RFC 6962 trees from its own implementation of the RFC's
// definitions, and its transactions from go-sdk's primitives and templates
// called directly. A vector the library's own code produced would prove only
// that the library agrees with itself.
//
// It is a module of its own so that the library keeps its single direct
// dependency: the encoder this brings in is required here and not there,
// and nothing the library ships imports this module. TestBoundaryFiles, in
// the library root, holds the separation in both directions: it refuses any
// import here outside the standard library, go-sdk and fxamacker/cbor, and
// any import of the library.
//
// Every vector is deterministic. The inputs are fixed, the key is the fixed
// test key, and go-sdk signs with RFC 6979 nonces, so a regeneration that
// changes a byte is a change in this generator or in go-sdk, never noise.
//
// From the repository root, make vectors runs the check and make
// vectors-update writes the files. Run by hand, it runs from this directory
// with the workspace off, so that go-sdk is the version this module pins and
// not whatever a workspace holds:
//
//	GOWORK=off go run .          # write ../../testdata/vectors and the filter's table
//	GOWORK=off go run . -check   # compare byte for byte; exit 1 on any difference
//
// It also writes the renderer filter's property table, generated from the
// Unicode emoji data under third_party/unicode, to the two places the
// library reads it from: sanitize/unicode-15.1.json, which the Go package
// embeds, and ts/src/sanitize-table.ts, which holds the same bytes for the
// TypeScript package. The corpus in sanitize-v1.json comes from a second
// implementation of the filter here, which reads the Unicode files and not
// the table.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// family is one vector file: the file name and the value written to it.
type family struct {
	name string
	v    any
}

func main() {
	dir := flag.String("dir", filepath.Join("..", "..", "testdata", "vectors"), "the directory the vectors are written to, or checked against with -check")
	check := flag.Bool("check", false, "compare the generated vectors with the files in -dir instead of writing them")
	repo := flag.String("repo", filepath.Join("..", ".."), "the repository root: the Unicode data is read from it, and the renderer filter's table is written to it, or checked against it with -check")
	flag.Parse()

	files, err := generate(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vectors:", err)
		os.Exit(1)
	}
	table, _, err := tableFiles(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vectors:", err)
		os.Exit(1)
	}
	if *check {
		bad := compare(*dir, files)
		for _, name := range sortedNames(table) {
			got, err := os.ReadFile(filepath.Join(*repo, name))
			switch {
			case err != nil:
				bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			case !bytes.Equal(got, table[name]):
				bad = append(bad, fmt.Sprintf("%s differs from the generator's output at byte %d", name, firstDifference(got, table[name])))
			}
		}
		if len(bad) > 0 {
			for _, b := range bad {
				fmt.Fprintln(os.Stderr, "vectors:", b)
			}
			fmt.Fprintln(os.Stderr, "vectors: regenerate with `make vectors-update` and review the diff")
			os.Exit(1)
		}
		fmt.Printf("vectors: %d files and the %d table files match\n", len(files), len(table))
		return
	}
	for _, name := range sortedNames(table) {
		if err := os.WriteFile(filepath.Join(*repo, name), table[name], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "vectors:", err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "vectors:", err)
		os.Exit(1)
	}
	for _, name := range sortedNames(files) {
		if err := os.WriteFile(filepath.Join(*dir, name), files[name], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "vectors:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("vectors: wrote %d files to %s\n", len(files), *dir)
}

// generate builds every family and renders each as indented JSON with a
// trailing newline. encoding/json writes struct fields in declaration order
// and map keys sorted, so the same inputs always render the same bytes.
func generate(repo string) (map[string][]byte, error) {
	var families []family
	sanitize := func() ([]family, error) { return sanitizeFamilies(repo) }
	for _, build := range []func() ([]family, error){cborFamilies, storeFamilies, rfc6962Families, txFamilies, recordFamilies, keyedFamilies, segmentFamilies, sanitize} {
		fs, err := build()
		if err != nil {
			return nil, err
		}
		families = append(families, fs...)
	}
	out := make(map[string][]byte, len(families))
	for _, f := range families {
		if _, dup := out[f.name]; dup {
			return nil, fmt.Errorf("two families write %s", f.name)
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(f.v); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		out[f.name] = b.Bytes()
	}
	return out, nil
}

// compare reports every generated file that is missing from dir or differs
// from it, and every file in dir that nothing generates: a stale vector left
// behind would otherwise go on being read by a test that nothing regenerates.
func compare(dir string, files map[string][]byte) []string {
	var bad []string
	for _, name := range sortedNames(files) {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if want := files[name]; !bytes.Equal(got, want) {
			bad = append(bad, fmt.Sprintf("%s differs from the generator's output at byte %d", name, firstDifference(got, want)))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(bad, err.Error())
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok {
			bad = append(bad, fmt.Sprintf("%s is in %s but nothing generates it", e.Name(), dir))
		}
	}
	return bad
}

func firstDifference(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
