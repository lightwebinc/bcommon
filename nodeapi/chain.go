package nodeapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
)

// ErrChainSpec is a chain view specification that cannot be used.
var ErrChainSpec = errors.New("nodeapi: bad chain specification")

// ChainOptions are the settings a chain specification does not carry:
// secrets and transport.
type ChainOptions struct {
	// WoCKey is the WhatsOnChain API key, if any, and WoCRate its
	// requests a second (zero is WoCFreeRate).
	WoCKey  string
	WoCRate float64
	// Client, when set, is every backend's HTTP client.
	Client *http.Client
	// Headers is required: every proof a backend answers is checked
	// against it (Checked) before the Sources return it.
	Headers chaintracker.ChainTracker
}

// ParseChain reads a chain view specification, in the style of
// headers.Parse: comma-separated backends, each
//
//	woc:main | woc:test     the public WhatsOnChain API (*WoC)
//	asset:http://node:8090  a Teranode asset API (*Asset)
//
// with every proof checked against opt.Headers (Checked), optionally
// qualified by the one method it serves: tx=, proof=, spend= or
// known=. An unqualified backend serves every method. Transactions and
// proofs are asked of every backend that serves them, in order, since each
// answer is checked. "Spent", "unspent" and "known" come from one backend
// only: the first that serves spend (or known), so that an absence answer
// is never a fall-through. Arcade, which proves only what it was sent, is
// not named here; put a publish.Arcade first in Proofs, as
// producer.Proofs does.
//
//	woc:main                               no node, mainnet
//	asset:http://node:8090                 a node for everything
//	asset:http://node:8090,woc:main        a node, WhatsOnChain for what it lacks
//	woc:test,spend=asset:http://node:8090  WhatsOnChain, a node's spend view
func ParseChain(spec string, opt ChainOptions) (*Sources, error) {
	if opt.Headers == nil {
		return nil, fmt.Errorf("%w: no headers: every proof is checked against the caller's headers", ErrChainSpec)
	}
	s := &Sources{}
	found := false
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		method := ""
		if m, rest, ok := strings.Cut(item, "="); ok && !strings.Contains(m, ":") {
			method, item = m, rest
		}
		b, err := backend(item, opt)
		if err != nil {
			return nil, err
		}
		found = true
		switch method {
		case "":
			s.Tx = append(s.Tx, b)
			s.Proofs = append(s.Proofs, Checked{Source: b, Headers: opt.Headers})
			if s.Spends == nil {
				s.Spends = b
			}
			if s.Knows == nil {
				s.Knows = b
			}
		case "tx":
			s.Tx = append(s.Tx, b)
		case "proof":
			s.Proofs = append(s.Proofs, Checked{Source: b, Headers: opt.Headers})
		case "spend":
			s.Spends = b
		case "known":
			s.Knows = b
		default:
			return nil, fmt.Errorf("%w: %q: method %q is tx, proof, spend or known", ErrChainSpec, spec, method)
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: empty", ErrChainSpec)
	}
	return s, nil
}

// chainBackend is what every backend ParseChain builds answers.
type chainBackend interface {
	Chain
	KnownSource
}

func backend(item string, opt ChainOptions) (chainBackend, error) {
	kind, rest, _ := strings.Cut(item, ":")
	switch kind {
	case "woc":
		w, err := NewWoC(rest, opt.WoCKey)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: woc:main or woc:test", ErrChainSpec, item)
		}
		w.Rate, w.Client = opt.WoCRate, opt.Client
		return w, nil
	case "asset":
		u, err := url.Parse(rest)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%w: %q: asset:<http or https URL>", ErrChainSpec, item)
		}
		return &Asset{Base: strings.TrimRight(rest, "/"), Client: opt.Client}, nil
	}
	return nil, fmt.Errorf("%w: %q: use woc:main, woc:test or asset:URL", ErrChainSpec, item)
}
