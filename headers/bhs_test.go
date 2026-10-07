package headers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The block-headers-service and arcade fixtures are block 900000's real
// header (testdata/sources/woc-main-900000.json) written in each service's
// answer shape as its source defines it: block-headers-service v1.3.0
// BlockHeaderResponse (transports/http/endpoints/api/headers/model.go), and
// go-chaintracks' BlockHeader as arcade v0.16.0 serves it
// (services/chaintracks_server/routes.go). Neither has a public instance
// to capture from.

func TestBlockHeadersServiceValidatesAndSendsTheToken(t *testing.T) {
	var auth, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, query = r.Header.Get("Authorization"), r.URL.RawQuery
		switch {
		case r.URL.Path == "/api/v1/chain/header/byHeight" && r.URL.Query().Get("height") == "900000":
			_, _ = w.Write(sourceFixture(t, "bhs-900000"))
		case r.URL.Path == "/api/v1/chain/header/byHeight" && r.URL.Query().Get("height") == "900001":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/api/v1/chain/tip/longest":
			_, _ = w.Write([]byte(`{"header":{},"state":"LONGEST_CHAIN","chainWork":"1","height":970077}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"ErrHeaderNotFound","message":"header not found"}`))
		}
	}))
	defer srv.Close()
	c, err := NewSource("bhs:" + srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.Token, c.HTTP = "secret", srv.Client()
	ctx := context.Background()
	ok, err := c.IsValidRootForHeight(ctx, mustHash(t, root900000), 900000)
	if err != nil || !ok {
		t.Fatalf("real header: %v %v", ok, err)
	}
	if auth != "Bearer secret" || !strings.Contains(query, "height=900000") || !strings.Contains(query, "count=1") {
		t.Fatalf("auth %q query %q", auth, query)
	}
	for _, h := range []uint32{900001, 900002} {
		if ok, err := c.IsValidRootForHeight(ctx, mustHash(t, root900000), h); ok || err != nil {
			t.Fatalf("height %d past the tip: %v %v, want false nil", h, ok, err)
		}
	}
	if h, err := c.CurrentHeight(ctx); err != nil || h != 970077 {
		t.Fatalf("tip: %d %v", h, err)
	}
	hd, err := c.HeaderAt(ctx, 900000)
	if err != nil || hd.MerkleRoot.String() != root900000 || hd.Time != 1749183955 || hd.Height != 900000 {
		t.Fatalf("%+v %v", hd, err)
	}
	if _, err := c.HeaderAt(ctx, 900001); !errors.Is(err, ErrUnknownHeight) {
		t.Fatalf("unknown height: %v", err)
	}
}

func TestBlockHeadersServiceForgedHeaderIsRefused(t *testing.T) {
	forged := strings.Replace(string(sourceFixture(t, "bhs-900000")), `"nonce":2569813228`, `"nonce":2569813229`, 1)
	srv := serveAt(t, "/api/v1/chain/header/byHeight", 200, []byte(forged))
	c, _ := NewSource("bhs:" + srv.URL)
	c.HTTP = srv.Client()
	if _, err := c.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000); !errors.Is(err, ErrProofOfWork) {
		t.Fatalf("%v", err)
	}
	two := serveAt(t, "/api/v1/chain/header/byHeight", 200, []byte(`[{},{}]`))
	c2, _ := NewSource("bhs:" + two.URL)
	c2.HTTP = two.Client()
	if _, err := c2.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000); err == nil {
		t.Fatal("two headers for one height")
	}
	auth := serveAt(t, "/api/v1/chain/header/byHeight", 401, []byte(`{"code":"ErrMissingAuthHeader"}`))
	c3, _ := NewSource("bhs:" + auth.URL)
	c3.HTTP = auth.Client()
	if _, err := c3.IsValidRootForHeight(context.Background(), mustHash(t, root900000), 900000); err == nil {
		t.Fatal("a 401 is an error, not 'not yet'")
	}
}

func TestArcadeHeaderServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chaintracks/v2/header/height/900000":
			_, _ = w.Write(sourceFixture(t, "arcade-chaintracks-900000"))
		case "/chaintracks/v2/height":
			_, _ = w.Write([]byte(`{"height":970077}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Header not found"}`))
		}
	}))
	defer srv.Close()
	c, err := NewSource("arcade:" + srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = srv.Client()
	ctx := context.Background()
	if ok, err := c.IsValidRootForHeight(ctx, mustHash(t, root900000), 900000); err != nil || !ok {
		t.Fatalf("real header: %v %v", ok, err)
	}
	if ok, err := c.IsValidRootForHeight(ctx, mustHash(t, root900000), 900001); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
	if h, err := c.CurrentHeight(ctx); err != nil || h != 970077 {
		t.Fatalf("tip: %d %v", h, err)
	}
	hd, err := c.HeaderAt(ctx, 900000)
	if err != nil || time.Unix(int64(hd.Time), 0).UTC().Year() != 2025 {
		t.Fatalf("%+v %v", hd, err)
	}
}

func TestHeaderAtOtherKinds(t *testing.T) {
	woc := serveAt(t, "/block/900000/header", 200, sourceFixture(t, "woc-main-900000"))
	hd, err := client(woc, WhatsOnChain, Mainnet).HeaderAt(context.Background(), 900000)
	if err != nil || hd.Hash.String() != "000000000000000002feb6a36e1b8bf81409d0252e285449e3d0ef2388c5506a" {
		t.Fatalf("%+v %v", hd, err)
	}
	native := serveAt(t, "/v1/tip", 200, []byte(`{}`))
	if _, err := client(native, Native, "").HeaderAt(context.Background(), 1); err == nil {
		t.Fatal("a native source has no header fields")
	}
	if _, err := New("woc:regtest").HeaderAt(context.Background(), 1); !errors.Is(err, ErrSource) {
		t.Fatalf("bad spec: %v", err)
	}
	tip := serveAt(t, "/height", 200, []byte(`{}`))
	if _, err := client(tip, Arcade, Mainnet).CurrentHeight(context.Background()); err == nil {
		t.Fatal("a tip with no height is an error")
	}
}
