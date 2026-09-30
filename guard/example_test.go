package guard_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// ParseBUMP walks a BRC-74 BUMP someone else supplied, allocating nothing,
// and hands it to the SDK only once every count it declares fits the bytes
// present. The bound is the caller's: how large an answer it was prepared
// to read.
func ExampleParseBUMP() {
	// A well-formed proof: a transaction at offset 1 of a two-transaction
	// block at height 90.
	txid := chainhash.Hash(sha256.Sum256([]byte("a transaction")))
	sibling := chainhash.Hash(sha256.Sum256([]byte("its neighbour")))
	isTxid := true
	good := transaction.NewMerklePath(90, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: &txid, Txid: &isTxid},
	}}).Bytes()

	mp, err := guard.ParseBUMP(good, 1<<20)
	fmt.Println("good:", len(good), "bytes, height", mp.BlockHeight, err)

	// Seven bytes that declare four billion leaves: the SDK is never asked
	// to size a slice for them.
	hostile := []byte{0x5a, 0x01, 0xfe, 0xff, 0xff, 0xff, 0xff}
	_, err = guard.ParseBUMP(hostile, 1<<20)
	fmt.Println("hostile:", err)

	_, err = guard.ParseBUMP(append(good, 0x00), 1<<20)
	fmt.Println("trailing:", err)

	_, err = guard.ParseBUMP(good, 16)
	fmt.Println("over the bound:", err)
	// Output:
	// good: 71 bytes, height 90 <nil>
	// hostile: bump level 0 declares 4294967295 leaves, 0 bytes remain
	// trailing: bump has 1 trailing bytes
	// over the bound: bump is 71 bytes, max 16
}

// ParseBEEF walks a BEEF before the SDK parses it: every count and length it
// declares must fit the bytes present, the walk must end at the last byte,
// and the whole must be within the caller's bound.
func ExampleParseBEEF() {
	// Thirteen bytes: BEEF V1, then a BUMP count of 2^63.
	hostile := []byte{0x01, 0x00, 0xbe, 0xef, 0xff, 0, 0, 0, 0, 0, 0, 0, 0x80}
	_, _, _, err := guard.ParseBEEF(hostile, guard.DefaultBound)
	fmt.Println(err)

	// A BEEF V1 whose one transaction declares four billion inputs.
	manyInputs := append([]byte{0x01, 0x00, 0xbe, 0xef, 0x00, 0x01, 0x01, 0, 0, 0, 0xfe, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)...)
	_, _, _, err = guard.ParseBEEF(manyInputs, guard.DefaultBound)
	fmt.Println(err)
	// Output:
	// guard: BEEF refused: declares 9223372036854775808 BUMPs, 0 bytes remain
	// guard: BEEF refused: transaction 0: declares 4294967295 inputs, 64 bytes remain
}

// ParsePubKey takes a compressed key only in its one canonical encoding.
// 02 || p+1 names the same point as the key with x = 1, and go-sdk reads it.
func ExampleParsePubKey() {
	x1, _ := hex.DecodeString("020000000000000000000000000000000000000000000000000000000000000001")
	alias, _ := hex.DecodeString("02fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30")

	k, err := guard.ParsePubKey(x1)
	fmt.Println(hex.EncodeToString(k.Compressed()) == hex.EncodeToString(x1), err)

	_, err = guard.ParsePubKey(alias)
	fmt.Println(err)
	sdk, err := ec.PublicKeyFromBytes(alias)
	fmt.Println("go-sdk:", err == nil, sdk.X.Cmp(k.X) != 0)
	// Output:
	// true <nil>
	// guard: public key refused: x is not below the field prime
	// go-sdk: true true
}
