// Package hostset chooses which address answers for an overlay host.
//
// A BRC-180 manifest names ONE base URL per service, secured by control of
// the domain. The name behind that URL may resolve to many addresses, and
// every one of them is a replica: every host behind the name is sent the
// same objects, no host learns anything from another, and the reader verifies
// each answer itself. So any address's answer is as good as any
// other's, a wrong one is caught rather than trusted, and the only questions
// left are availability (which address is up) and agreement (do they say the
// same thing). This package answers the first with a policy and hands the
// caller the material for the second.
//
// The fan-out therefore lives at the name, in DNS, where the operator already
// controls it, and not in a client-side host list or a manifest extension. A
// manifest that listed several base URLs would respecify what BRC-180 rule 2
// says the base URL already means, and a client list would be a second source
// of truth for what the domain declares.
package hostset

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

// Host is one address that answers for a base URL.
type Host struct {
	// Base is the base URL exactly as configured. Requests are built against
	// it, so the scheme, port and path the operator gave are preserved.
	Base string
	// Name is Base's hostname. It stays in the URL, the Host header and the
	// TLS handshake whatever address is dialed.
	Name string
	// Addr is the ip:port to dial (name:port for a Static host, which is
	// dialed by name).
	Addr string
}

// Source lists the hosts behind a base URL.
type Source interface {
	Hosts(ctx context.Context, base string) ([]Host, error)
}

// DNS resolves the base URL's hostname to every A and AAAA record. This is
// where a BRC-180 base URL fans out: the operator publishes several records,
// or a routing policy that answers with several, and this source sees them
// all. An IP literal yields itself.
type DNS struct {
	// Resolver defaults to net.DefaultResolver.
	Resolver *net.Resolver
}

// Hosts implements Source.
func (d DNS) Hosts(ctx context.Context, base string) ([]Host, error) {
	name, port, err := splitBase(base)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(name); ip != nil {
		return []Host{{Base: base, Name: name, Addr: net.JoinHostPort(name, port)}}, nil
	}
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupIPAddr(ctx, name)
	if err != nil {
		return nil, err
	}
	hosts := make([]Host, 0, len(addrs))
	for _, a := range addrs {
		hosts = append(hosts, Host{Base: base, Name: name, Addr: net.JoinHostPort(a.IP.String(), port)})
	}
	return hosts, nil
}

// Static is an explicit list of base URLs, for a caller given several, on its
// command line or in its configuration. Each entry is dialed by its own name.
// The base argument to Hosts is ignored, because the list IS the answer.
type Static struct {
	Bases []string
}

// Hosts implements Source.
func (s Static) Hosts(_ context.Context, _ string) ([]Host, error) {
	hosts := make([]Host, 0, len(s.Bases))
	for _, b := range s.Bases {
		name, port, err := splitBase(b)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, Host{Base: b, Name: name, Addr: net.JoinHostPort(name, port)})
	}
	return hosts, nil
}

// splitBase takes the hostname and port out of a base URL, defaulting the
// port from the scheme.
func splitBase(base string) (name, port string, err error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", "", err
	}
	switch u.Scheme {
	case "https":
		port = "443"
	case "http":
		port = "80"
	default:
		return "", "", fmt.Errorf("base %q: scheme %q is not http or https", base, u.Scheme)
	}
	name = u.Hostname()
	if name == "" {
		return "", "", fmt.Errorf("base %q has no host", base)
	}
	if p := u.Port(); p != "" {
		port = p
	}
	return name, port, nil
}
