package keyed_test

import (
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/keyed"
)

// A publisher draws a key, publishes its commitment, and wraps the key to
// each holder. A holder that opens a wrap checks the key against the
// commitment before anything uses it: a wrong key then names whoever
// wrapped it, not whoever encrypted under it.
func ExampleCheckOpened() {
	// A fixed scalar so the example prints the same thing every time; a
	// publisher calls keyed.SampleKey(nil).
	k := goldentest.Fill(0x07)
	if err := keyed.CheckScalar(k); err != nil {
		fmt.Println(err)
		return
	}
	commitment := keyed.Commitment(k)
	symmetric := keyed.SymmetricKey(k)
	fmt.Println("the commitment says nothing about the key that encrypts:", commitment != symmetric)

	// The wrap: BRC-2's symmetric form under a key the two ends share. A
	// wallet's Encrypt writes the same form with an IV it draws itself.
	shared := goldentest.Fill(0x21)
	wrap, err := keyed.SymmetricSeal(shared[:], goldentest.Fill(0x80), k[:])
	fmt.Println("wrap:", len(wrap), "bytes", err)

	opened, err := keyed.SymmetricOpen(shared[:], wrap)
	if err != nil {
		fmt.Println(err)
		return
	}
	got, err := keyed.CheckOpened(opened, commitment)
	fmt.Println("the committed key:", got == k, err)

	// The same wrap against another commitment, and another key's wrap.
	other := goldentest.Fill(0x08)
	_, err = keyed.CheckOpened(opened, keyed.Commitment(other))
	fmt.Println("another commitment:", errors.Is(err, keyed.ErrCommitment))
	wrong := goldentest.Fill(0x22)
	_, err = keyed.SymmetricOpen(wrong[:], wrap)
	fmt.Println("another shared key opens it:", err == nil)
	// Output:
	// the commitment says nothing about the key that encrypts: true
	// wrap: 80 bytes <nil>
	// the committed key: true <nil>
	// another commitment: true
	// another shared key opens it: false
}
