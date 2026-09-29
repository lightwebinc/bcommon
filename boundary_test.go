package bcommon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/lightwebinc/bcommon"
	sdkPath    = "github.com/bsv-blockchain/go-sdk"

	// vectorsDir holds the one nested module that is not part of the
	// library: the generator of the vectors the packages' tests compare
	// their output against. cborOracle is the independent encoder it
	// brings in to check the codec against.
	vectorsDir = "tools/vectors"
	cborOracle = "github.com/fxamacker/cbor/v2"
	// vectorsNote marks a refusal under the generator's rule rather than
	// the library's.
	vectorsNote = " (the vector generator may import only the standard library, " + sdkPath + "/... and " + cborOracle + ", never this module)"
)

// listed is the part of `go list -json` the boundary reads.
type listed struct {
	ImportPath     string
	Dir            string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	IgnoredGoFiles []string
	Error          *struct{ Err string }
}

// allowed reports whether a package of this module may import path. The
// standard library is told apart the way the go command tells it apart, by a
// first path element with no dot.
func allowed(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return true
	}
	return within(path, sdkPath) || within(path, modulePath)
}

// vectorsAllowed is the generator's rule. It may import the independent
// encoder it exists to bring in, and go-sdk, whose primitives and templates
// build its transactions. It may not import this module: a generator that
// ran the library's code would check the library against itself.
func vectorsAllowed(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return true
	}
	return within(path, sdkPath) || within(path, cborOracle)
}

// within matches whole path elements, so a module that merely shares a
// prefix (go-sdk-extra, bcommonx) is not mistaken for the one allowed.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// goCmd runs the go command with the workspace off, for the Makefile's
// reason: a workspace resolves siblings from disk, and the boundary is about
// what this module builds on its own.
func goCmd(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out
}

// fileImports reads a file's imports without building it, for files the
// current build constraints leave out of go list's import lists.
func fileImports(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, spec := range f.Imports {
		imp, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		paths = append(paths, imp)
	}
	return paths, nil
}

// TestBoundary fails on any import outside the rule in doc.go. Test imports
// count as much as production ones: a test dependency is still a requirement
// in go.mod, and so in the module graph of every application that pins this
// module. Files left out by build constraints count too, since another
// platform or tag builds them.
func TestBoundary(t *testing.T) {
	gomod := strings.TrimSpace(string(goCmd(t, ".", "env", "GOMOD")))
	if gomod == "" || gomod == os.DevNull {
		t.Fatalf("not inside a module (GOMOD=%q)", gomod)
	}
	out := goCmd(t, filepath.Dir(gomod), "list", "-e",
		"-json=ImportPath,Dir,Imports,TestImports,XTestImports,IgnoredGoFiles,Error",
		modulePath+"/...")

	var bad []string
	seen := false
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		if p.ImportPath == modulePath {
			seen = true
		}
		// A package go list could not load may hold imports it never read,
		// so an error fails the check rather than passing it unexamined.
		if p.Error != nil {
			bad = append(bad, fmt.Sprintf("%s: go list: %s", p.ImportPath, p.Error.Err))
		}
		check := func(how string, imports []string) {
			for _, imp := range imports {
				if !allowed(imp) {
					bad = append(bad, fmt.Sprintf("%s %s %s", p.ImportPath, how, imp))
				}
			}
		}
		check("imports", p.Imports)
		check("test imports", p.TestImports)
		check("external test imports", p.XTestImports)
		for _, name := range p.IgnoredGoFiles {
			imports, err := fileImports(filepath.Join(p.Dir, name))
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: %s: %v", p.ImportPath, name, err))
				continue
			}
			check(fmt.Sprintf("(%s, excluded by build constraints) imports", name), imports)
		}
	}
	// An empty listing would pass every rule, so the root package must show.
	if !seen {
		t.Fatalf("go list did not report %s; nothing was checked", modulePath)
	}
	reject(t, bad)
}

// reject fails the test with every import the rule refuses.
func reject(t *testing.T, bad []string) {
	t.Helper()
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("packages may import only the standard library, %s/... and %s/...; a test that needs anything else belongs with the code that has it:\n\t%s",
			sdkPath, modulePath, strings.Join(bad, "\n\t"))
	}
}

// skipped reports whether the go command ignores a file or directory by its
// name: testdata, and anything starting with an underscore or a dot. Nothing
// the go command ignores is built, so none of it is a dependency.
func skipped(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// walkFiles applies the rule to every Go file under root, read straight from
// disk, and returns each import it refuses and how many files it read. Only
// what the go command ignores by name is skipped (see skipped): a build
// constraint does not skip a file, and a nested go.mod does not stop the
// walk. Files under vectorsDir are held to the generator's rule
// (vectorsAllowed) instead of the library's. Paths are reported relative to
// root.
func walkFiles(root string) (bad []string, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && skipped(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		imports, err := fileImports(path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			return nil
		}
		rule, note := allowed, ""
		if within(name, vectorsDir) {
			rule, note = vectorsAllowed, vectorsNote
		}
		for _, imp := range imports {
			if !rule(imp) {
				bad = append(bad, fmt.Sprintf("%s imports %s%s", name, imp, note))
			}
		}
		return nil
	})
	return bad, files, err
}

// TestBoundaryFiles applies the same rule to every Go file in the repository,
// read straight from disk, because go list does not report every file that
// ships. A package whose every file a build constraint excludes is dropped
// from a ... pattern without an error, and a directory with a go.mod of its
// own is another module, where the pattern stops. Both are still code this
// repository ships, so the walk goes through them. The vector generator is
// such a module, and the walk holds it to its own rule: its one extra
// import is the independent encoder, and it may never import the library it
// checks.
func TestBoundaryFiles(t *testing.T) {
	bad, files, err := walkFiles(".")
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	// This file is in the module, so a walk that read nothing looked in the
	// wrong place.
	if files == 0 {
		t.Fatal("no Go file in the module was read; nothing was checked")
	}
	reject(t, bad)
}

// The walk's traversal rules on a tree built for them, since the module
// itself holds none of these cases: every file here but ok.go and the
// generator's first two imports a package outside the rule that applies to
// it, and only what the go command would build somewhere is flagged.
// A file behind a build tag and a nested module are read; testdata and the
// _ and . directories are not. A file with allowed imports is read and not
// flagged. The generator may import the independent encoder and go-sdk but
// not this module, and its exemption covers its own directory only: not a
// sibling, and not a directory whose name merely starts with its name. An
// import path is allowed by whole path elements, so a module whose path
// merely starts with an allowed one is flagged.
func TestBoundaryFilesWalk(t *testing.T) {
	const foreign = "example.com/app/record"
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := func(header, pkg, imp string) string {
		return header + "package " + pkg + "\n\nimport _ \"" + imp + "\"\n"
	}
	write("ok.go", src("", "lib", sdkPath+"/script"))
	write("prefixmod.go", src("", "lib", modulePath+"x/foo"))
	write("prefixsdk.go", src("", "lib", sdkPath+"-extra/x"))
	write("tagged.go", src("//go:build integration\n\n", "lib", foreign))
	write("nested/go.mod", "module example.com/nested\n\ngo 1.24\n")
	write("nested/nested.go", src("", "nested", foreign))
	write("testdata/fixture.go", src("", "fixture", foreign))
	write("_x/x.go", src("", "x", foreign))
	write(".x/x.go", src("", "x", foreign))
	write(vectorsDir+"/go.mod", "module vectors\n\ngo 1.24\n")
	write(vectorsDir+"/encode.go", src("", "main", cborOracle))
	write(vectorsDir+"/sign.go", src("", "main", sdkPath+"/script"))
	write(vectorsDir+"/self.go", src("", "main", modulePath+"/cbor"))
	write("tools/other/other.go", src("", "other", cborOracle))
	write(vectorsDir+"x/x.go", src("", "x", cborOracle))

	bad, files, err := walkFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	want := []string{
		"nested/nested.go imports " + foreign,
		"prefixmod.go imports " + modulePath + "x/foo",
		"prefixsdk.go imports " + sdkPath + "-extra/x",
		"tagged.go imports " + foreign,
		"tools/other/other.go imports " + cborOracle,
		vectorsDir + "/self.go imports " + modulePath + "/cbor" + vectorsNote,
		vectorsDir + "x/x.go imports " + cborOracle,
	}
	sort.Strings(want)
	if strings.Join(bad, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n\t%s\nwant:\n\t%s", strings.Join(bad, "\n\t"), strings.Join(want, "\n\t"))
	}
	if files != 10 {
		t.Errorf("read %d files, want 10 (ok.go, prefixmod.go, prefixsdk.go, tagged.go, nested/nested.go, three under %s, one beside it and one under tools/other)", files, vectorsDir)
	}
}
