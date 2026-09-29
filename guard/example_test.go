package guard_test

import (
	"crypto/sha256"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
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
