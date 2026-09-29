package nodeapi_test

import (
	"crypto/sha256"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// ProofFor is where every proof a service supplies enters: the guard walks
// it before the SDK parses it, and a proof that does not name the
// transaction it was asked for is refused, because it would verify
// perfectly and prove nothing.
func ExampleProofFor() {
	txid := chainhash.Hash(sha256.Sum256([]byte("a transaction")))
	sibling := chainhash.Hash(sha256.Sum256([]byte("its neighbour")))
	isTxid := true
	raw := transaction.NewMerklePath(90, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: &txid, Txid: &isTxid},
	}}).Bytes()

	mp, err := nodeapi.ProofFor(raw, txid.String())
	fmt.Println("asked for:", mp != nil, err)

	unrelated := chainhash.Hash(sha256.Sum256([]byte("some other transaction")))
	_, err = nodeapi.ProofFor(raw, unrelated.String())
	fmt.Println("asked for another:", err)

	_, err = nodeapi.ProofFor(raw[:len(raw)-1], txid.String())
	fmt.Println("truncated:", err)
	// Output:
	// asked for: true <nil>
	// asked for another: proof does not contain the txid
	// truncated: bump ends mid-structure
}
