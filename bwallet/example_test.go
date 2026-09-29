package bwallet_test

import (
	"fmt"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
)

// A Profile has no default. Validate names the first field that is missing
// or unusable, before any wallet is opened or any key is derived.
func ExampleProfile_Validate() {
	p := bwallet.Profile{
		FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "example app"},
		FundKeyID:      "coin",
		FundBasket:     "example coin",
		Version:        "example-embedded-1",
		LegacyPoolFile: "example-pool.json",
	}
	fmt.Println(p.Validate())

	p.FundBasket = ""
	fmt.Println(p.Validate())

	p.FundBasket = "example coin"
	p.LegacyPoolFile = "identity.json"
	fmt.Println(p.Validate())
	// Output:
	// <nil>
	// bwallet: wallet profile incomplete: FundBasket is empty
	// bwallet: wallet profile incomplete: LegacyPoolFile "identity.json" is one of the wallet's own files
}
