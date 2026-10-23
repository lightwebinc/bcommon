// Package termsroute is a host's terms route: one small HTTP handler beside
// an overlay host, whose base URL is what the host publishes for its lookup
// service (BRC-180 metanet.overlays). It is the Go twin of the TypeScript
// package's host entry point (ts/src/host: LookupFront, its budgets, its
// sessions, the payee ledger and payment acceptance), for an overlay host
// written in Go. It serves
//
//	GET  <base>/<service>/terms   the host terms document (404 when the host prices nothing)
//	POST <base>/.well-known/auth  the BRC-104 handshake
//	POST <base>/lookup            BRC-24 questions to the service, free and priced
//
// A free question is answered with or without BRC-104 authentication. A
// question of a class the host prices must be asked over BRC-104; without a
// payment it is answered 402 with BRC-105's headers (the version, the price,
// the derivation prefix; the payee is the server's identity key in the
// authentication headers), and with one that pays the price to the key
// BRC-29 derives for that prefix, suffix and asker, and that verifies
// against the host's headers, it is held to acceptance (Gate): the host
// broadcasts it, and answers at once when it is small and the network took
// it, or 202 ERR_PAYMENT_HELD until it mines (the same payment, sent again
// once mined, buys the answer). One payment buys one question.
//
// Every authenticated request is answered with a signed response, so a
// session is given a budget of them (KeyedBudget), and a request sent twice
// is refused (SeenRequests); handshakes are held to a budget
// (HandshakeBudget) before any signature work, and the sessions they buy
// are bounded (Sessions). Status codes, bodies, metric names (under the
// application's prefix) and ledger lines are the TypeScript route's, so a
// client sees one route whichever language the host is written in.
package termsroute

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PaymentVersion is BRC-105's payment version.
const PaymentVersion = "1.0"

// MaxSafe is 2^53 - 1: the largest price, as the TypeScript route bounds it.
const MaxSafe = 1<<53 - 1

// PricedClass is a class of question, as a price list names it.
type PricedClass struct {
	Name string
	// Free is a class that never has a price.
	Free bool
}

// TermsPath is the terms route of a lookup service, below the base URL.
func TermsPath(service string) string { return "/" + service + "/terms" }

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// ParsePrices parses a host's prices, `history=5,history-after=5`: a comma
// list of <class>=<satoshis>, each a priceable class of classes once,
// priced 0 to 2^53 - 1. A free class is refused: it never has a price. A
// class and its -after form, where classes defines one, are priced
// together or not at all. variable names the setting in errors, for
// example APP_PRICES. An empty raw is no price.
func ParsePrices(raw string, classes []PricedClass, variable string) (map[string]uint64, error) {
	out := map[string]uint64{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	find := func(name string) *PricedClass {
		for i := range classes {
			if classes[i].Name == name {
				return &classes[i]
			}
		}
		return nil
	}
	for _, entry := range strings.Split(raw, ",") {
		e := strings.TrimSpace(entry)
		eq := strings.IndexByte(e, '=')
		name := e
		if eq >= 0 {
			name = e[:eq]
		}
		cls := find(name)
		if eq < 0 || cls == nil {
			return nil, fmt.Errorf("%s: %q is not <class>=<satoshis>", variable, e)
		}
		if cls.Free {
			return nil, fmt.Errorf("%s: %s is a free class and never has a price", variable, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s names %s twice", variable, name)
		}
		v := e[eq+1:]
		n, err := strconv.ParseUint(v, 10, 64)
		if !decimal.MatchString(v) || err != nil || n > MaxSafe {
			return nil, fmt.Errorf("%s: %s: %q is not an integer from 0 to 2^53 - 1", variable, name, v)
		}
		out[name] = n
	}
	// A class and its -after form sell one walk: priced apart, the free one
	// answers every page but the first for nothing.
	for _, c := range classes {
		after := c.Name + "-after"
		if find(after) == nil {
			continue
		}
		if (out[c.Name] > 0) != (out[after] > 0) {
			return nil, fmt.Errorf("%s: %s and %s are priced together or not at all", variable, c.Name, after)
		}
	}
	return out, nil
}

// TermsClass is one line of the terms document.
type TermsClass struct {
	Class    string `json:"class"`
	Satoshis uint64 `json:"satoshis"`
}

// Terms is the host terms document.
type Terms struct {
	Service string       `json:"service"`
	Terms   int          `json:"terms"`
	Classes []TermsClass `json:"classes"`
}

// TermsDocument is the host terms document: the priced classes in the
// order of classes.
func TermsDocument(service string, classes []PricedClass, prices map[string]uint64) Terms {
	t := Terms{Service: service, Terms: 1, Classes: []TermsClass{}}
	for _, c := range classes {
		if p, ok := prices[c.Name]; ok {
			t.Classes = append(t.Classes, TermsClass{Class: c.Name, Satoshis: p})
		}
	}
	return t
}
