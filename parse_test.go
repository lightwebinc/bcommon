package bcommon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sdkParsers are go-sdk's readers of transactions, BEEF and BUMPs from
// bytes: each sizes memory from counts in the bytes it is given. Only guard
// calls them on bytes from another party, after its bounded walk.
var sdkParsers = map[string]bool{
	"ParseBeef":                 true,
	"NewBeefFromBytes":          true,
	"NewBeefFromHex":            true,
	"NewBeefFromAtomicBytes":    true,
	"NewTransactionFromBEEF":    true,
	"NewTransactionFromBEEFHex": true,
	"NewTransactionFromBytes":   true,
	"NewTransactionFromHex":     true,
	"NewTransactionFromStream":  true,
	"NewMerklePathFromBinary":   true,
	"NewMerklePathFromHex":      true,
	"NewMerklePathFromReader":   true,
}

// sdkReadMethods are the same readers as methods, whatever their receiver.
var sdkReadMethods = map[string]bool{"ReadFrom": true, "ReadFromExtended": true, "FromBEEF": true}

// guardedParses are the files outside guard that may call an SDK reader,
// each for the reason given, by call. A new call anywhere else fails
// TestNoUnguardedParse until it goes through guard or is listed here with
// its reason.
var guardedParses = map[string]map[string]string{
	"chaintoken/wire.go": {
		"NewMerklePathFromReader": "ReadWire reads only after guard.CheckBEEF admitted the same bytes",
		"ReadFrom":                "ReadWire reads only after guard.CheckBEEF admitted the same bytes",
	},
	"chaintoken/token.go": {
		"NewMerklePathFromBinary": "clonePath copies a path from its own serialization",
	},
}

// guardedPackages may call any reader: guard is the guard, and testchain
// and goldentest are test helpers that read what a test wrote.
var guardedPackages = []string{"guard", "testchain", "goldentest"}

// unguardedParses reads every non-test Go file under root, as walkFiles
// does, and returns each call of an SDK reader that the rule above does not
// admit, and how many files it read.
func unguardedParses(root string) (bad []string, files int, err error) {
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
		for _, p := range guardedPackages {
			if strings.HasPrefix(name, p+"/") {
				return nil
			}
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			return nil
		}
		txName := ""
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if imp == sdkPath+"/transaction" {
				txName = "transaction"
				if spec.Name != nil {
					txName = spec.Name.Name
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, isIdent := sel.X.(*ast.Ident)
			fn := sel.Sel.Name
			hit := (isIdent && txName != "" && x.Name == txName && sdkParsers[fn]) || (!(isIdent && x.Name == txName) && sdkReadMethods[fn])
			if !hit {
				return true
			}
			if _, ok := guardedParses[name][fn]; ok {
				return true
			}
			bad = append(bad, fmt.Sprintf("%s calls %s", name, fn))
			return true
		})
		return nil
	})
	return bad, files, err
}

// TestNoUnguardedParse: no bcommon path hands bytes to an SDK reader
// except through guard, or at a call listed with its reason.
func TestNoUnguardedParse(t *testing.T) {
	bad, files, err := unguardedParses(".")
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if files == 0 {
		t.Fatal("no Go file in the module was read; nothing was checked")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("bytes from another party reach the SDK through guard (ParseBEEF, ParseTransaction, ParseBUMP, RawTransaction):\n\t%s", strings.Join(bad, "\n\t"))
	}
	// Every listed exception still exists: a stale entry would admit a new
	// call of the same name in its file.
	for file, calls := range guardedParses {
		src, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for fn := range calls {
			if !strings.Contains(string(src), "."+fn+"(") {
				t.Errorf("%s no longer calls %s; drop it from guardedParses", file, fn)
			}
		}
	}
}

// The rule on a tree built for it: a package-level reader, an aliased
// import, a method reader, a test file, guard and a listed exception.
func TestNoUnguardedParseWalk(t *testing.T) {
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
	tx := `import "` + sdkPath + `/transaction"` + "\n"
	write("a/a.go", "package a\n\n"+tx+"func f(b []byte) { transaction.ParseBeef(b) }\n")
	write("b/b.go", "package b\n\nimport t \""+sdkPath+"/transaction\"\n\nfunc f(b []byte) { t.NewTransactionFromBytes(b) }\n")
	write("c/c.go", "package c\n\nfunc f(x interface{ ReadFrom(any) }) { x.ReadFrom(nil) }\n")
	write("d/d.go", "package d\n\n"+tx+"func f(b []byte) { _ = transaction.NewTransaction() }\n")
	write("a/a_test.go", "package a\n\n"+tx+"func g(b []byte) { transaction.ParseBeef(b) }\n")
	write("guard/g.go", "package guard\n\n"+tx+"func f(b []byte) { transaction.ParseBeef(b) }\n")
	write("chaintoken/token.go", "package chaintoken\n\n"+tx+"func f(b []byte) { transaction.NewMerklePathFromBinary(b); transaction.ParseBeef(b) }\n")
	bad, files, err := unguardedParses(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	want := []string{"a/a.go calls ParseBeef", "b/b.go calls NewTransactionFromBytes", "c/c.go calls ReadFrom", "chaintoken/token.go calls ParseBeef"}
	if strings.Join(bad, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n\t%s\nwant:\n\t%s", strings.Join(bad, "\n\t"), strings.Join(want, "\n\t"))
	}
	if files != 5 {
		t.Errorf("read %d files, want 5", files)
	}
}
