package knownkeys_test

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightwebinc/bcommon/knownkeys"
)

// The key an address was first seen with is pinned; a different key for the
// same address is refused unless the application verified a rotation.
func ExamplePin() {
	first := "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	next := "0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c"
	fp := func(keyHex string) string {
		b, _ := hex.DecodeString(keyHex)
		return knownkeys.Fingerprint(b)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	recs, err := knownkeys.Pin(nil, "alice@example.com", first, 1, fp(first), now)
	if err != nil {
		fmt.Println(err)
		return
	}
	_, err = knownkeys.Pin(recs, "alice@example.com", next, 2, fp(next), now)
	fmt.Println("another key:", err != nil)

	recs, err = knownkeys.Rotate(recs, "alice@example.com", next, 2, fp(next), now.Add(time.Hour))
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, r := range recs {
		fmt.Println(r.Line())
	}
	// Output:
	// another key: true
	// @rotated-from alice@example.com secp256k1 0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 until_seq=1 at=2026-01-02T04:04:05Z
	// alice@example.com secp256k1 0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c seq=2 first=2026-01-02T04:04:05Z last=2026-01-02T04:04:05Z fp=SHA256:p80oF5TbGEhPJA2b38CSIauJ63wV48p6tnwwY89tfjc
}
