package publish

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// The public arcade installations GorillaPool runs, the default
// broadcaster: no key, ARC-compatible routes at the root (POST /tx,
// GET /tx/{txid}, GET /policy).
const (
	ArcadeMainnet = "https://arcade.gorillapool.io"
	ArcadeTestnet = "https://testnet.arcade.gorillapool.io"
)

// DefaultSettle is the settlement specification for a network when an
// application configures none: arcade, "arcade:main" or "arcade:test". A
// regtest chain has no public broadcaster, so it has no default.
func DefaultSettle(network string) (string, error) {
	switch network {
	case "main", "test":
		return "arcade:" + network, nil
	}
	return "", fmt.Errorf("%w: no default broadcaster for network %q; configure one", ErrSettleSpec, network)
}

// ErrSettleSpec is a settlement specification that cannot be used.
var ErrSettleSpec = errors.New("publish: bad settlement specification")

// SettleOptions are what a settlement specification does not carry.
type SettleOptions struct {
	// Key is the bearer token for arcade or ARC (TAAL's ARC requires one).
	Key string
	// RPCUser, RPCPass and RPCID authenticate an rpc: leg.
	RPCUser, RPCPass, RPCID string
	// Client, when set, is the HTTP client of an arcade, arc or rpc leg.
	Client *http.Client
	// Spends, when set, is the spend view arcade's verdict is held to
	// (Arcade.Spends); Asset, a node's (Arcade.Asset).
	Spends nodeapi.SpendSource
	Asset  *nodeapi.Asset
	// Note receives arcade's notes (Arcade.Note).
	Note func(format string, args ...any)
}

// ParseSettler reads a settlement specification:
//
//	arcade:main | arcade:test   GorillaPool's public arcade (the default)
//	arcade:https://host         an arcade installation, yours or another
//	arc:https://host[/v1]       an ARC installation (opt-in), such as
//	                            https://arc.gorillapool.io or, with a Key,
//	                            https://arc.taal.com; /v1 is added to a
//	                            bare host
//	rpc:http://node:port        a node's sendrawtransaction
//	tcp:host:port               a fabric ingress, bare EF, no answer
//
// and returns the leg, and the *Arcade when it is one (arcade and ARC
// answer the same API), for proofs and status.
func ParseSettler(spec string, opt SettleOptions) (Settler, *Arcade, error) {
	kind, addr, _ := strings.Cut(strings.TrimSpace(spec), ":")
	switch kind {
	case "arcade":
		switch addr {
		case "main":
			addr = ArcadeMainnet
		case "test":
			addr = ArcadeTestnet
		}
		if err := httpURL(addr); err != nil {
			return nil, nil, fmt.Errorf("%w: %q: %v", ErrSettleSpec, spec, err)
		}
		a := opt.arcade(strings.TrimRight(addr, "/"))
		return a, a, nil
	case "arc":
		if err := httpURL(addr); err != nil {
			return nil, nil, fmt.Errorf("%w: %q: %v", ErrSettleSpec, spec, err)
		}
		u, _ := url.Parse(addr)
		if strings.Trim(u.Path, "/") == "" {
			u.Path = "/v1"
		}
		a := opt.arcade(strings.TrimRight(u.String(), "/"))
		return a, a, nil
	case "rpc":
		if err := httpURL(addr); err != nil {
			return nil, nil, fmt.Errorf("%w: %q: %v", ErrSettleSpec, spec, err)
		}
		rpc := &nodeapi.RPC{URL: addr, User: opt.RPCUser, Pass: opt.RPCPass, ID: opt.RPCID, Client: opt.Client}
		return &RPCSettler{RPC: rpc}, nil, nil
	case "tcp":
		if _, port, ok := strings.Cut(addr, ":"); addr == "" || !ok || port == "" {
			return nil, nil, fmt.Errorf("%w: %q: tcp:<host:port>", ErrSettleSpec, spec)
		}
		return &TCPIngress{Addr: addr}, nil, nil
	}
	return nil, nil, fmt.Errorf("%w: %q: use arcade:main, arcade:test, arcade:URL, arc:URL, rpc:URL or tcp:host:port", ErrSettleSpec, spec)
}

func (o SettleOptions) arcade(base string) *Arcade {
	return &Arcade{Base: base, Key: o.Key, Client: o.Client, Spends: o.Spends, Asset: o.Asset, Note: o.Note}
}

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("not an http or https URL")
	}
	return nil
}
