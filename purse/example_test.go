package purse_test

import (
	"encoding/base64"
	"fmt"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/purse"
)

// A payee takes a payment from what a host's ledger recorded about it: the
// derivation prefix and suffix, in base64, and the sender's identity key.
// Remittance turns those into what InternalizeAction checks the payment's
// output against, and refuses anything that is not that.
func ExampleRemittance() {
	prefix := base64.StdEncoding.EncodeToString([]byte("prefix-1"))
	suffix := base64.StdEncoding.EncodeToString([]byte("suffix-1"))
	sender := fmt.Sprintf("%x", goldentest.FixedKey().PubKey().Compressed())

	r, err := purse.Remittance(prefix, suffix, sender)
	fmt.Println(string(r.DerivationPrefix), string(r.DerivationSuffix), err)

	_, err = purse.Remittance("not base64!", suffix, sender)
	fmt.Println(err)
	_, err = purse.Remittance(prefix, suffix, "02"+sender[2:10])
	fmt.Println(err != nil)
	// Output:
	// prefix-1 suffix-1 <nil>
	// purse: derivation prefix "not base64!" is not base64
	// true
}
