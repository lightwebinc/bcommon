package hostset_test

import (
	"context"
	"fmt"

	"github.com/lightwebinc/bcommon/hostset"
)

// A Static source is the list a caller was given; each entry is dialed by
// its own name. The DNS source, the default, instead resolves the one base
// URL a manifest names to every address behind it.
func ExampleStatic_Hosts() {
	src := hostset.Static{Bases: []string{"https://a.overlay.example.com", "https://b.overlay.example.com:8443/api"}}
	hosts, err := src.Hosts(context.Background(), "ignored")
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, h := range hosts {
		fmt.Println(h.Base, h.Name, h.Addr)
	}

	ips, err := hostset.DNS{}.Hosts(context.Background(), "https://[2001:db8::10]:8443")
	fmt.Println(ips, err)
	// Output:
	// https://a.overlay.example.com a.overlay.example.com a.overlay.example.com:443
	// https://b.overlay.example.com:8443/api b.overlay.example.com b.overlay.example.com:8443
	// [{https://[2001:db8::10]:8443 2001:db8::10 [2001:db8::10]:8443}] <nil>
}
