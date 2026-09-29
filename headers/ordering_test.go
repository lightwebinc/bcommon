package headers

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// A height the store does not hold answers 404, and IsValidRootForHeight
// turns that into (false, nil) because the proof cannot be checked yet, which
// is not the same as a forged proof. A service that pads its 404 page must
// not turn that into an error: bounding the body before reading the status
// would do exactly that, and this is the test that keeps the order right.
func TestPadded404IsStillNotKnown(t *testing.T) {
	big := strings.Repeat("x", int(maxBody)+64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.HTTP = srv.Client()
	root := chainhash.Hash{}
	ok, err := c.IsValidRootForHeight(context.Background(), &root, 101)
	if err != nil {
		t.Fatalf("a padded 404 must not be an error: %v", err)
	}
	if ok {
		t.Fatal("a 404 must not validate a root")
	}
}

// An oversized 200 IS refused, because there the body is the answer.
func TestOversized200IsRefused(t *testing.T) {
	big := strings.Repeat("x", int(maxBody)+64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.HTTP = srv.Client()
	if _, err := c.CurrentHeight(context.Background()); err == nil {
		t.Fatal("an oversized 200 must be refused, not truncated")
	}
}

// Every http.Client this package builds must carry an explicit Transport.
//
// The seam tests assert that the constructor returns a client with no proxy;
// they cannot see a bare &http.Client{} written inline at a call site, and
// such a client would use HTTP_PROXY. This reads the source instead, the same
// way publish guards its two-socket rule.
func TestNoBareHTTPClientLiteral(t *testing.T) {
	assertNoBareClient(t, ".")
}

func assertNoBareClient(t *testing.T, dir string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Client" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "http" {
					return true
				}
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Transport" {
							return true
						}
					}
				}
				t.Errorf("%s: http.Client built with no Transport; it would honour HTTP_PROXY", fset.Position(lit.Pos()))
				return true
			})
			_ = name
		}
	}
}
