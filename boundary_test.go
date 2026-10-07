package bcommon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
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

// processImports and processCalls are what a command owns and a library
// must not: flags, logging to the process's own streams, other processes
// and signals, the environment and the user's directories a configuration
// is found in, the standard streams, and the process's exit. A library that
// reached for any of them would decide, for every application that imports
// it, where its settings come from or what reaches its terminal. termsafe
// is the one package about terminals, and it too takes the environment as
// a parameter rather than reading it.
var (
	processImports = map[string]bool{"flag": true, "log": true, "log/slog": true, "os/exec": true, "os/signal": true}
	processCalls   = map[string]bool{
		"Args": true, "Exit": true,
		"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true,
		"Setenv": true, "Unsetenv": true, "Clearenv": true,
		"UserHomeDir": true, "UserConfigDir": true, "UserCacheDir": true,
		"Stdin": true, "Stdout": true, "Stderr": true,
	}
)

// processConcerns reads every non-test Go file under root, as walkFiles
// does, and returns each use of a process concern, and how many files it
// read. The vector generator is a command and is not held to it.
func processConcerns(root string) (bad []string, files int, err error) {
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
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if within(name, vectorsDir) {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			return nil
		}
		osName := ""
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if processImports[imp] {
				bad = append(bad, fmt.Sprintf("%s imports %s", name, imp))
			}
			if imp == "os" {
				osName = "os"
				if spec.Name != nil {
					osName = spec.Name.Name
				}
			}
		}
		if osName == "" || osName == "_" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == osName && processCalls[sel.Sel.Name] {
				bad = append(bad, fmt.Sprintf("%s uses os.%s", name, sel.Sel.Name))
			}
			return true
		})
		return nil
	})
	return bad, files, err
}

// TestNoProcessConcerns holds every library file, termsafe's included, to
// the rule above.
func TestNoProcessConcerns(t *testing.T) {
	bad, files, err := processConcerns(".")
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if files == 0 {
		t.Fatal("no Go file in the module was read; nothing was checked")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("a library takes its settings and its output as parameters; these belong in the application:\n\t%s", strings.Join(bad, "\n\t"))
	}
}

// The rule on a tree built for it: the flagged files use a concern through
// an import, a call, an aliased os and a stream; a test file, the vector
// generator and a file that only opens files are read or skipped as the
// rule says.
func TestNoProcessConcernsWalk(t *testing.T) {
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
	write("files.go", "package lib\n\nimport \"os\"\n\nfunc f() { _, _ = os.ReadFile(\"x\") }\n")
	write("flags.go", "package lib\n\nimport \"flag\"\n\nvar _ = flag.Bool\n")
	write("env.go", "package lib\n\nimport \"os\"\n\nvar _ = os.Getenv(\"HOME\")\n")
	write("alias.go", "package lib\n\nimport sys \"os\"\n\nvar _ = sys.Stderr\n")
	write("home.go", "package lib\n\nimport \"os\"\n\nfunc g() { _, _ = os.UserHomeDir() }\n")
	write("lib_test.go", "package lib\n\nimport \"os\"\n\nvar _ = os.Getenv(\"HOME\")\n")
	write(vectorsDir+"/main.go", "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(0) }\n")
	write("testdata/x.go", "package x\n\nimport \"log\"\n")

	bad, files, err := processConcerns(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	want := []string{"alias.go uses os.Stderr", "env.go uses os.Getenv", "flags.go imports flag", "home.go uses os.UserHomeDir"}
	if strings.Join(bad, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n\t%s\nwant:\n\t%s", strings.Join(bad, "\n\t"), strings.Join(want, "\n\t"))
	}
	if files != 5 {
		t.Errorf("read %d files, want 5 (every non-test file outside testdata and the generator)", files)
	}
}

// termsafe imports only the standard library, tests included. It is what
// every command that prints someone else's text needs, and nothing about
// filtering a string for a terminal needs go-sdk.
func TestTermsafeImportsOnlyTheStandardLibrary(t *testing.T) {
	gomod := strings.TrimSpace(string(goCmd(t, ".", "env", "GOMOD")))
	out := goCmd(t, filepath.Dir(gomod), "list", "-json=ImportPath,Imports,TestImports,XTestImports", modulePath+"/termsafe")
	var p listed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if p.ImportPath != modulePath+"/termsafe" {
		t.Fatalf("go list answered %q", p.ImportPath)
	}
	var bad []string
	for _, imports := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
		for _, imp := range imports {
			first, _, _ := strings.Cut(imp, "/")
			if strings.Contains(first, ".") && imp != p.ImportPath {
				bad = append(bad, imp)
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("termsafe imports more than the standard library: %v", bad)
	}
}

// layers is what each package added for shared application code may import
// from this module: module in its production code, and tests beside that in
// its tests. A package that is not listed is held only to the module's
// rule. These are listed because their place in the graph is the point of
// them: record reads bytes and needs no SDK; keyed is keys and one cipher
// form over the SDK alone; chaintoken reads a BEEF through the guard and a
// script through pushdrop, and decides nothing an application's record
// enters; testchain serves the wire formats the clients read without
// importing a client, and its tests drive it through those clients;
// sanitize is character rules over a table it embeds, and its tests compare
// it with termsafe; payee is the payee's side of a payment over the purse,
// and settles nothing the purse does not, and its tests run it on the local
// chain; feepolicy does the policy fetch so that mint stays pure, and
// reaches nothing but mint.
var layers = map[string]struct {
	module []string
	tests  []string
	sdk    bool
}{
	"record":     {module: []string{"cbor"}},
	"keyed":      {tests: []string{"goldentest"}, sdk: true},
	"chaintoken": {module: []string{"guard", "pushdrop"}, tests: []string{"goldentest", "mint"}, sdk: true},
	"chainview":  {module: []string{"nodeapi"}, sdk: true},
	"acceptance": {module: []string{"chainview", "nodeapi", "publish"}, tests: []string{"goldentest", "testchain"}, sdk: true},
	"testchain":  {tests: []string{"goldentest", "headers", "nodeapi", "publish"}, sdk: true},
	"sanitize":   {tests: []string{"termsafe"}},
	"chirp":      {sdk: true},
	"commit":     {},
	"feepolicy":  {module: []string{"mint"}},
	"payee":      {module: []string{"guard", "purse", "termsafe"}, tests: []string{"bwallet", "mint", "nodeapi", "producer", "publish", "testchain"}, sdk: true},
}

// testOnly are the packages that exist for tests and local trials. No
// production file of this module imports one: a library that linked its
// stand-in chain or its fixed test key into an application's binary would
// put test code on the path of real value.
var testOnly = []string{"goldentest", "testchain"}

func listPackage(t *testing.T, pkg string) listed {
	t.Helper()
	gomod := strings.TrimSpace(string(goCmd(t, ".", "env", "GOMOD")))
	out := goCmd(t, filepath.Dir(gomod), "list", "-json=ImportPath,Imports,TestImports,XTestImports", modulePath+"/"+pkg)
	var p listed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if p.ImportPath != modulePath+"/"+pkg {
		t.Fatalf("go list answered %q for %s", p.ImportPath, pkg)
	}
	return p
}

// TestLayers holds each listed package to its own imports from this module
// and, where it needs none of it, to no go-sdk.
func TestLayers(t *testing.T) {
	for pkg, rule := range layers {
		p := listPackage(t, pkg)
		production := map[string]bool{pkg: true}
		for _, m := range rule.module {
			production[m] = true
		}
		inTests := map[string]bool{}
		for m := range production {
			inTests[m] = true
		}
		for _, m := range rule.tests {
			inTests[m] = true
		}
		check := func(how string, imports []string, allowedIn map[string]bool) {
			for _, imp := range imports {
				switch {
				case within(imp, sdkPath):
					if !rule.sdk {
						t.Errorf("%s %s %s: it needs nothing of go-sdk", pkg, how, imp)
					}
				case within(imp, modulePath):
					name, _, _ := strings.Cut(strings.TrimPrefix(imp, modulePath+"/"), "/")
					if !allowedIn[name] {
						t.Errorf("%s %s %s, which is not on its layer", pkg, how, imp)
					}
				}
			}
		}
		check("imports", p.Imports, production)
		check("test imports", p.TestImports, inTests)
		check("external test imports", p.XTestImports, inTests)
	}
}

// TestHelpersStayInTests fails on a production import of a test helper
// anywhere in the module.
func TestHelpersStayInTests(t *testing.T) {
	gomod := strings.TrimSpace(string(goCmd(t, ".", "env", "GOMOD")))
	out := goCmd(t, filepath.Dir(gomod), "list", "-json=ImportPath,Imports", modulePath+"/...")
	seen := 0
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
		seen++
		for _, imp := range p.Imports {
			for _, h := range testOnly {
				if imp == modulePath+"/"+h {
					t.Errorf("%s imports %s outside a test", p.ImportPath, imp)
				}
			}
		}
	}
	if seen < 20 {
		t.Fatalf("go list reported %d packages; nothing was checked", seen)
	}
}
