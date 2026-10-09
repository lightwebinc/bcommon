// Package vectors is an application's golden-vector command: it writes the
// files the application's codec generates under a directory, or with
// -check compares them byte for byte and writes nothing. An application's
// cmd/vectors is one call:
//
//	func main() {
//		files, err := vectors.Generate() // the application's own generator
//		os.Exit(devvectors.Main(os.Args[1:], files, err, devvectors.Options{}, os.Stderr))
//	}
package vectors

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Options are what differ between applications.
type Options struct {
	// Dir is the default vector directory; empty is testdata/vectors.
	Dir string
	// Strict makes -check also refuse a *.json file in the directory that
	// nothing generates: a vector the codec dropped is stale too.
	Strict bool
}

// Main runs the command on args (without the program name): -dir DIR and
// -check. files is what the generator made, and genErr its failure. It
// returns the exit status: 0, 1 when a file differs, is missing or could
// not be written, or the generator failed, 2 for a usage error.
func Main(args []string, files map[string][]byte, genErr error, o Options, stderr io.Writer) int {
	def := o.Dir
	if def == "" {
		def = "testdata/vectors"
	}
	fs := flag.NewFlagSet("vectors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", def, "vector directory")
	check := fs.Bool("check", false, "compare with the files on disk and write nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if genErr != nil {
		fmt.Fprintln(stderr, "vectors:", genErr)
		return 1
	}
	problems, err := Run(*dir, files, *check, o.Strict)
	for _, p := range problems {
		fmt.Fprintln(stderr, p)
	}
	if err != nil {
		fmt.Fprintln(stderr, "vectors:", err)
		return 1
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// Run writes files into dir, or with check compares them with what is
// there and returns a line per difference ("differs: <path>", and with
// strict "not generated: <path>" for a *.json file nothing generates). An
// error is a file that could not be written or a directory that could not
// be read.
func Run(dir string, files map[string][]byte, check, strict bool) ([]string, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var problems []string
	if check && strict {
		have, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return nil, err
		}
		for _, p := range have {
			if _, ok := files[filepath.Base(p)]; !ok {
				problems = append(problems, "not generated: "+p)
			}
		}
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if check {
			have, err := os.ReadFile(path) //nolint:gosec // the application's own vector directory
			if err != nil || !bytes.Equal(have, files[name]) {
				problems = append(problems, "differs: "+path)
			}
			continue
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil { //nolint:gosec // vectors are published files
			return problems, err
		}
	}
	return problems, nil
}
