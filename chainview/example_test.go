package chainview_test

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/chainview"
)

// A settlement leg's error is either the network's definitive refusal of
// these bytes, which no retry changes, or something transient.
func ExampleRefusedAnswer() {
	for _, e := range []string{
		"publish: arcade refused 5f2c: REJECTED: bad-txns-inputs-missingorspent",
		"publish: sendrawtransaction: rpc error -26: mandatory-script-verify-flag-failed",
		"publish: sendrawtransaction: rpc error -26: mempool full",
		"publish: arcade answered 503: busy",
		"context deadline exceeded",
	} {
		_, refused := chainview.RefusedAnswer(errors.New(e))
		fmt.Printf("%t  %s\n", refused, e)
	}
	// Output:
	// true  publish: arcade refused 5f2c: REJECTED: bad-txns-inputs-missingorspent
	// true  publish: sendrawtransaction: rpc error -26: mandatory-script-verify-flag-failed
	// false  publish: sendrawtransaction: rpc error -26: mempool full
	// false  publish: arcade answered 503: busy
	// false  context deadline exceeded
}
