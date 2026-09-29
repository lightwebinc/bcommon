package carrier_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// txVector is the part of testdata/vectors/transactions-v1.json this
// package builds: the funding lock, the carriers and the two sweeps.
// tools/vectors built them with go-sdk's primitives and templates called
// directly, under a derivation and tags that belong to no application.
type txVector struct {
	PrivateKeyHex       string `json:"privateKeyHex"`
	IdentityKeyHex      string `json:"identityKeyHex"`
	Originator          string `json:"originator"`
	SecurityLevel       int    `json:"securityLevel"`
	Protocol            string `json:"protocol"`
	ObjectKeyID         string `json:"objectKeyId"`
	ObjectLockingKeyHex string `json:"objectLockingKeyHex"`
	FundingTagHex       string `json:"fundingTagHex"`
	SatPerByte          uint64 `json:"satPerByte"`
	Floor               uint64 `json:"floor"`
	ChangeScriptHex     string `json:"changeScriptHex"`
	Coin                struct {
		TxHex   string `json:"txHex"`
		Height  uint32 `json:"height"`
		BumpHex string `json:"bumpHex"`
		RootHex string `json:"rootHex"`
	} `json:"coin"`
	FundingLockHex string `json:"fundingLockHex"`
	FundingTree    struct {
		Count   int    `json:"count"`
		TxHex   string `json:"txHex"`
		Height  uint32 `json:"height"`
		BumpHex string `json:"bumpHex"`
		RootHex string `json:"rootHex"`
	} `json:"fundingTree"`
	Carriers []struct {
		Vout          uint32 `json:"vout"`
		PayloadHex    string `json:"payloadHex"`
		CommitmentHex string `json:"commitmentHex"`
		Txid          string `json:"txid"`
		TxHex         string `json:"txHex"`
	} `json:"carriers"`
	Sweep        vectorSweep `json:"sweep"`
	SweepWithFee vectorSweep `json:"sweepWithFee"`
}

type vectorSweep struct {
	Vouts   []uint32 `json:"vouts"`
	FeeVout *uint32  `json:"feeVout"`
	TxHex   string   `json:"txHex"`
}

// vectorChain is the vector read and made ready to build from: the wallet
// over the fixed key, the vector's Params, and its coin and funding tree
// with their proofs, both known to a tracker.
type vectorChain struct {
	v       *txVector
	ctx     context.Context
	w       wallet.Interface
	p       carrier.Params
	coin    *transaction.Transaction
	tree    *transaction.Transaction
	tracker *goldentest.Tracker
}

// errNotVectorPayload is the vector's own payload rule refusing something
// else, so a validator that ran on the wrong bytes would show.
var errNotVectorPayload = errors.New("sample: not one of the vector's payloads")

func readVectorChain(t *testing.T) *vectorChain {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "transactions-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v txVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	seed := goldentest.Fill(0x42)
	if v.PrivateKeyHex != hex.EncodeToString(seed[:]) ||
		v.IdentityKeyHex != hex.EncodeToString(goldentest.FixedKey().PubKey().Compressed()) {
		t.Fatal("the vector was not built with goldentest.FixedKey")
	}
	// A family that is empty, or under a key these tests do not read, would
	// otherwise pass every loop over it by running none of it.
	if len(v.Carriers) == 0 || v.FundingTree.Count == 0 {
		t.Fatalf("the vector holds %d carriers and a funding tree of %d outputs; a family is missing or renamed",
			len(v.Carriers), v.FundingTree.Count)
	}
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		t.Fatal(err)
	}
	payloads := make([][]byte, 0, len(v.Carriers))
	for _, c := range v.Carriers {
		payloads = append(payloads, goldentest.Hex(t, c.PayloadHex))
	}
	c := &vectorChain{
		v: &v, ctx: context.Background(), w: w,
		p: carrier.Params{
			Derivation: pushdrop.Derivation{
				Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevel(v.SecurityLevel), Protocol: v.Protocol},
				KeyID:    v.ObjectKeyID,
			},
			FundingTag: goldentest.Hex(t, v.FundingTagHex),
			ValidatePayload: func(p []byte) error {
				for _, want := range payloads {
					if bytes.Equal(p, want) {
						return nil
					}
				}
				return errNotVectorPayload
			},
		},
		coin: goldentest.Tx(t, v.Coin.TxHex),
		tree: goldentest.Tx(t, v.FundingTree.TxHex),
	}
	if c.coin.MerklePath, err = transaction.NewMerklePathFromHex(v.Coin.BumpHex); err != nil {
		t.Fatal(err)
	}
	if c.tree.MerklePath, err = transaction.NewMerklePathFromHex(v.FundingTree.BumpHex); err != nil {
		t.Fatal(err)
	}
	if v.FundingTree.Count > len(c.tree.Outputs) {
		t.Fatalf("the funding tree has %d outputs, fewer than its count %d", len(c.tree.Outputs), v.FundingTree.Count)
	}
	c.tracker = &goldentest.Tracker{Roots: map[uint32]string{
		v.Coin.Height:        v.Coin.RootHex,
		v.FundingTree.Height: v.FundingTree.RootHex,
	}, Tip: v.FundingTree.Height + 10}
	return c
}

func (c *vectorChain) script(t *testing.T, s string) *script.Script {
	t.Helper()
	out, err := script.NewFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// verifies runs SPV on tx through the vector's proven ancestry.
func (c *vectorChain) verifies(t *testing.T, name string, tx *transaction.Transaction) {
	t.Helper()
	ok, err := spv.Verify(c.ctx, tx, c.tracker, nil)
	if err != nil || !ok {
		t.Errorf("%s does not verify: ok=%v err=%v", name, ok, err)
	}
}

// FundingLock and DecodeFunding must agree with the independent vector's
// funding lock, and every output of its funding tree must decode under the
// vector's tag to the key a reader derives.
func TestFundingIndependentVector(t *testing.T) {
	c := readVectorChain(t)
	lock, err := carrier.FundingLock(c.ctx, c.w, c.v.Originator, c.p)
	if err != nil {
		t.Fatal(err)
	}
	if lock.String() != c.v.FundingLockHex {
		t.Fatalf("funding lock:\n got %s\nwant %s", lock, c.v.FundingLockHex)
	}
	for i := 0; i < c.v.FundingTree.Count; i++ {
		key, ok := carrier.DecodeFunding(c.tree.Outputs[i].LockingScript, c.p.FundingTag)
		if !ok || hex.EncodeToString(key.Compressed()) != c.v.ObjectLockingKeyHex {
			t.Errorf("tree output %d does not decode as funding to the reader's key (ok=%v)", i, ok)
		}
	}
}

// Mint must build each carrier of the independent vector byte for byte from
// its payload and its funding output, and what it builds must decode,
// validate, commit to the vector's commitment and verify through the tree.
func TestMintIndependentVector(t *testing.T) {
	c := readVectorChain(t)
	identity := goldentest.Hex(t, c.v.IdentityKeyHex)
	for i, want := range c.v.Carriers {
		payload := goldentest.Hex(t, want.PayloadHex)
		tx, err := carrier.Mint(c.ctx, c.w, c.v.Originator, c.p, payload, c.tree, want.Vout)
		if err != nil {
			t.Fatal(err)
		}
		if tx.Hex() != want.TxHex {
			t.Fatalf("carrier %d differs from the independent vector:\n got %s\nwant %s", i, tx.Hex(), want.TxHex)
		}
		commitment := carrier.Commitment(tx)
		if hex.EncodeToString(commitment[:]) != want.CommitmentHex {
			t.Errorf("carrier %d: commitment %x, want %s", i, commitment, want.CommitmentHex)
		}
		if chainhash.Hash(commitment).String() != want.Txid {
			t.Errorf("carrier %d: commitment is not the txid %s in hash byte order", i, want.Txid)
		}
		dec, err := carrier.Decode(goldentest.Tx(t, want.TxHex), func(p []byte) (bool, error) {
			return bytes.Equal(p, payload), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if dec.OutputIndex != 0 || !bytes.Equal(dec.Payload, payload) {
			t.Errorf("carrier %d decodes to output %d payload %x", i, dec.OutputIndex, dec.Payload)
		}
		if err := dec.Validate(c.p, identity); err != nil {
			t.Errorf("carrier %d: %v", i, err)
		}
		c.verifies(t, "carrier", tx)
	}
}

// Sweep must build both of the independent vector's sweeps byte for byte:
// one the tree pays for itself, its fee taken from the tombstone, and one
// with a fee input whose remainder goes to change.
func TestSweepIndependentVector(t *testing.T) {
	c := readVectorChain(t)
	change := c.script(t, c.v.ChangeScriptHex)
	payer, err := p2pkh.Unlock(goldentest.FixedKey(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]vectorSweep{"sweep": c.v.Sweep, "sweepWithFee": c.v.SweepWithFee} {
		var fee *transaction.Transaction
		var feeVout uint32
		var feeUnlocker transaction.UnlockingScriptTemplate
		if want.FeeVout != nil {
			fee, feeVout, feeUnlocker = c.coin, *want.FeeVout, payer
		}
		tx, err := carrier.Sweep(c.ctx, c.w, c.v.Originator, c.p, c.tree, want.Vouts,
			fee, feeVout, feeUnlocker, change, c.v.SatPerByte, c.v.Floor)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tx.Hex() != want.TxHex {
			t.Errorf("%s differs from the independent vector:\n got %s\nwant %s", name, tx.Hex(), want.TxHex)
		}
		c.verifies(t, name, tx)
	}
}
