package pushdrop_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// The derivation and tag the library's test vectors use. They belong to no
// application, are reserved for tests (docs/registry.md), and an application
// supplies its own.
var (
	exampleDerivation = pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
		KeyID:    "object",
	}
	exampleTag = []byte{'v', 'x', 0x01}
)

// A producer locks a signed, tagged PushDrop through its wallet; a reader
// holding only the producer's identity key recomputes the locking key,
// decodes the output and checks the field signature. The locking key is the
// independent vector's objectLockingKeyHex in
// testdata/vectors/transactions-v1.json.
func ExampleDerivation_Lock() {
	ctx := context.Background()
	// The fixed TEST key (32 bytes of 0x42). The SDK's proto wallet stands
	// in for the producer's BRC-100 wallet: Lock needs only its key calls.
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		fmt.Println(err)
		return
	}
	lock, err := exampleDerivation.Lock(ctx, w, "example.com", [][]byte{exampleTag, []byte("hello")}, true)
	if err != nil {
		fmt.Println(err)
		return
	}

	// The reader's side.
	identity := goldentest.FixedKey().PubKey()
	want, err := exampleDerivation.ExpectedLockingKey(identity)
	if err != nil {
		fmt.Println(err)
		return
	}
	out, err := pushdrop.DecodeTagged(lock, exampleTag, 2)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("locking key:", hex.EncodeToString(out.LockingKey.Compressed()))
	fmt.Println("derived by the reader:", want.IsEqual(out.LockingKey))
	fmt.Printf("fields: %q\n", out.Fields)
	fmt.Println("signature verifies:", out.VerifySignature())

	// Another tag is not this application's output.
	_, err = pushdrop.DecodeTagged(lock, []byte{'v', 'x', 0x02}, 2)
	fmt.Println("another tag refused:", errors.Is(err, pushdrop.ErrNotTagged))
	// Output:
	// locking key: 0324d6d6ee75173da1ca8d964d3889792c8911d66c3290a76c367413f5a68e5446
	// derived by the reader: true
	// fields: ["vx\x01" "hello"]
	// signature verifies: true
	// another tag refused: true
}

// Validate applies the SDK's BRC-43 rules when a derivation is built, rather
// than at the first mint.
func ExampleDerivation_Validate() {
	for _, d := range []pushdrop.Derivation{
		exampleDerivation,
		{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "abcd"}, KeyID: "object"},
		{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector_sample"}, KeyID: "object"},
		{Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"}},
	} {
		fmt.Println(d.Validate())
	}
	// Output:
	// <nil>
	// pushdrop: invalid derivation: protocol name "abcd" is under 5 characters
	// pushdrop: invalid derivation: protocol name "vector_sample" has '_'; only letters, digits and spaces
	// pushdrop: invalid derivation: empty key id
}
