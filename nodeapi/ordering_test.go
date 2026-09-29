package nodeapi

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

// A node that pads its 429 page must still be retried, and one that pads an
// error page must still report its status. The size verdict comes last for
// exactly this reason, and Asset.get keeps the same order.
func TestPaddedStatusesSurviveTheBound(t *testing.T) {
	big := strings.Repeat("x", int(maxBody)+64)

	t.Run("429 still retries", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			if n == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(big))
				return
			}
			_, _ = w.Write([]byte(`{"result":123,"error":null}`))
		}))
		defer srv.Close()
		r := &RPC{URL: srv.URL, ID: testID, Client: srv.Client()}
		var out int
		if err := r.Call(context.Background(), "getinfo", nil, &out); err != nil {
			t.Fatalf("a padded 429 must retry, not fail on size: %v", err)
		}
		if n < 2 {
			t.Fatalf("no retry happened, calls=%d", n)
		}
	})

	t.Run("error page keeps its status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(big))
		}))
		defer srv.Close()
		r := &RPC{URL: srv.URL, ID: testID, Client: srv.Client()}
		err := r.Call(context.Background(), "getinfo", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("want an error naming 500, got %v", err)
		}
	})

	t.Run("oversized 200 is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(big))
		}))
		defer srv.Close()
		r := &RPC{URL: srv.URL, ID: testID, Client: srv.Client()}
		if err := r.Call(context.Background(), "getinfo", nil, nil); err == nil {
			t.Fatal("an oversized 200 must be refused, not truncated")
		}
	})
}

// Every http.Client this package builds must carry an explicit Transport.
// The seam test cannot see a bare &http.Client{} written inline at a call
// site, and such a client would use HTTP_PROXY. This reads the source
// instead.
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
