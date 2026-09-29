package publish

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
)

// A facade that answers 401 with an error page larger than the bound must
// still be reported as a 401. The named status branches exist so an operator
// reads which failure it was; bounding the body first would replace every one
// of them with a size error.
func TestPaddedAuthFailureKeepsItsStatus(t *testing.T) {
	big := strings.Repeat("x", int(maxSteak)+64)
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "401"},
		{http.StatusForbidden, "403"},
		{http.StatusBadGateway, "502"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(big))
		}))
		f := &Facade{Base: srv.URL, HTTP: srv.Client()}
		_, err := f.Submit(context.Background(), "tm_example", atomicBEEFMarker())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: want an error naming %s, got %v", tc.status, tc.want, err)
		}
	}
}

// An oversized 200 IS refused: there the body is the answer.
func TestOversized200IsRefused(t *testing.T) {
	big := strings.Repeat("x", int(maxSteak)+64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()
	f := &Facade{Base: srv.URL, HTTP: srv.Client()}
	if _, err := f.Submit(context.Background(), "tm_example", atomicBEEFMarker()); err == nil {
		t.Fatal("an oversized 200 must be refused, not truncated")
	}
}

// atomicBEEFMarker is the smallest body that passes the marker gate, so these
// tests exercise the response path rather than the request check.
func atomicBEEFMarker() []byte {
	return []byte{0x01, 0x01, 0x01, 0x01}
}

// Every http.Client this package builds must carry an explicit Transport.
//
// The seam test asserts that the constructor returns a client with no proxy;
// it cannot see a bare &http.Client{} written inline at a call site, and such
// a client would use HTTP_PROXY. This reads the source instead, the same way
// this package guards its two-socket rule.
func TestNoBareHTTPClientLiteral(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
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
		}
	}
}
