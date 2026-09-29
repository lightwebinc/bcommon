package resolve_test

import (
	"fmt"

	"github.com/lightwebinc/bcommon/resolve"
)

// ParseAcct accepts the three written forms of a BRC-169 address, lowercases
// it, and keeps a +tag apart from the handle it routes to.
func ExampleParseAcct() {
	for _, s := range []string{"Alice@Example.COM", "@alice+news@example.com", "acct:alice@example.com", "alice@example"} {
		a, err := resolve.ParseAcct(s)
		if err != nil {
			fmt.Println(s, "=>", err)
			continue
		}
		fmt.Printf("%s => %s (tag %q)\n", s, a, a.Tag)
	}
	// Output:
	// Alice@Example.COM => alice@example.com (tag "")
	// @alice+news@example.com => alice@example.com (tag "news")
	// acct:alice@example.com => alice@example.com (tag "")
	// alice@example => resolve: "alice@example": ecosystem "example" has no dot, so it is an alias (BRC-169 section 2.1 rule 5); aliases are not supported, name the domain in full
}

// A manifest names one base URL per service. An absent entry means the
// domain does not offer the service, and a reader must not go looking.
func ExampleManifest_Overlay() {
	m, err := resolve.ParseManifest([]byte(`{"metanet":{"overlays":{"ls_example":"https://overlay.example.com"}}}`))
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(m.Overlay("ls_example"))
	fmt.Println(m.Overlay("ls_other"))
	// Output:
	// https://overlay.example.com true
	//  false
}
