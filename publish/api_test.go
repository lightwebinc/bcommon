package publish

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"
)

// The compile-time half of the package doc's structural rule, checked at test
// time because Go has no way to say "no exported signature mentions both of
// these types": every exported function, method and struct in this package is
// read from source, and any one whose parameters (or fields) name both
// *transaction.Transaction and a []byte fails. A []byte is the only way a
// BEEF travels here, so a signature taking both is a place where one writer
// could be handed both encodings.
//
// The scan asserts it saw every Submit method, so a broken walk cannot pass
// by seeing nothing.
func TestNoExportedAPITakesBoth(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "publish" {
			t.Fatalf("%s is package %s", name, f.Name.Name)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no source files parsed")
	}

	seen := map[string]bool{}
	check := func(name string, fields *ast.FieldList) {
		if fields == nil {
			return
		}
		var kinds []string
		for _, f := range fields.List {
			kinds = append(kinds, types.ExprString(f.Type))
		}
		sig := strings.Join(kinds, ", ")
		hasTx := strings.Contains(sig, "transaction.Transaction")
		hasBytes := strings.Contains(sig, "[]byte")
		if hasTx && hasBytes {
			t.Errorf("%s takes both a transaction and a byte slice: (%s)", name, sig)
		}
		seen[name] = true
	}

	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					recv := types.ExprString(d.Recv.List[0].Type)
					base := strings.TrimPrefix(recv, "*")
					if !ast.IsExported(base) {
						continue
					}
					name = base + "." + name
				}
				if !ast.IsExported(d.Name.Name) {
					continue
				}
				check(name, d.Type.Params)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ast.IsExported(ts.Name.Name) {
						continue
					}
					switch tt := ts.Type.(type) {
					case *ast.StructType:
						check(ts.Name.Name, tt.Fields)
					case *ast.InterfaceType:
						for _, m := range tt.Methods.List {
							if ft, ok := m.Type.(*ast.FuncType); ok && len(m.Names) == 1 {
								check(ts.Name.Name+"."+m.Names[0].Name, ft.Params)
							}
						}
					}
				}
			}
		}
	}
	for _, want := range []string{"TCPIngress.Submit", "RPCSettler.Submit", "Arcade.Submit", "Facade.Submit", "Settler.Submit"} {
		if !seen[want] {
			t.Errorf("scan did not see %s; the walk is broken and proves nothing", want)
		}
	}
}
