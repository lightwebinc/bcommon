package carrier_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
)

// flipS rewrites a canonical unlocking script's signature to the other S
// that verifies, n - S, strictly encoded: the malleation anyone can make
// without the key.
func flipS(t *testing.T, unlocking []byte) []byte {
	t.Helper()
	if len(unlocking) < 2 || int(unlocking[0]) != len(unlocking)-1 {
		t.Fatalf("not one direct push: %x", unlocking)
	}
	sig := unlocking[1:]
	der, hashType := sig[:len(sig)-1], sig[len(sig)-1]
	lenR := int(der[3])
	r, s := der[4:4+lenR], new(big.Int).SetBytes(der[6+lenR:])
	flipped := new(big.Int).Sub(ec.S256().N, s).Bytes()
	if flipped[0]&0x80 != 0 {
		flipped = append([]byte{0}, flipped...)
	}
	body := append([]byte{0x02, byte(len(r))}, r...)
	body = append(append(body, 0x02, byte(len(flipped))), flipped...)
	out := append([]byte{0x30, byte(len(body))}, body...)
	out = append(out, hashType)
	return append([]byte{byte(len(out))}, out...)
}

type unlockingVector struct {
	Carrier     int  `json:"carrier"`
	SigHashType byte `json:"sigHashType"`
	Cases       []struct {
		Name         string `json:"name"`
		Accept       bool   `json:"accept"`
		ScriptValid  bool   `json:"scriptValid"`
		UnlockingHex string `json:"unlockingHex"`
		Txid         string `json:"txid"`
		TxHex        string `json:"txHex"`
	} `json:"cases"`
}

// Every case of the shared unlocking vector: carrier 0 of the transaction
// family with its unlocking script rewritten. Only the canonical one is
// taken, by CheckUnlocking and by Decode with Validate alike, and each
// refusal is ErrUnlocking. The interpreter's verdict is re-checked too,
// because the cases it accepts are the reason for the check: each is a
// spend of the same funding output under another txid with the same record.
func TestUnlockingIndependentVector(t *testing.T) {
	c := readVectorChain(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "unlocking-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v unlockingVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.SigHashType != carrier.SigHashType {
		t.Fatalf("the vector's sighash type 0x%02x is not SigHashType 0x%02x", v.SigHashType, carrier.SigHashType)
	}
	if v.Carrier < 0 || v.Carrier >= len(c.v.Carriers) {
		t.Fatalf("the vector departs from carrier %d of %d", v.Carrier, len(c.v.Carriers))
	}
	base := c.v.Carriers[v.Carrier]
	payload := goldentest.Hex(t, base.PayloadHex)
	identity := goldentest.Hex(t, c.v.IdentityKeyHex)
	var accepted, malleable int
	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			tx := goldentest.Tx(t, tc.TxHex)
			if tx.TxID().String() != tc.Txid {
				t.Fatalf("txid %s, want %s", tx.TxID(), tc.Txid)
			}
			if got := fmt.Sprintf("%x", []byte(*tx.Inputs[0].UnlockingScript)); got != tc.UnlockingHex {
				t.Fatalf("unlocking %s, want %s", got, tc.UnlockingHex)
			}
			if tc.Accept != (tc.TxHex == base.TxHex) {
				t.Fatal("only carrier 0's own bytes are accepted")
			}
			err := carrier.CheckUnlocking(tx)
			if tc.Accept != (err == nil) || (err != nil && !errors.Is(err, carrier.ErrUnlocking)) {
				t.Fatalf("CheckUnlocking: %v, accept %v", err, tc.Accept)
			}
			dec, err := carrier.Decode(tx, func(p []byte) (bool, error) { return bytes.Equal(p, payload), nil })
			if err != nil {
				t.Fatal(err)
			}
			err = dec.Validate(c.p, identity)
			if tc.Accept != (err == nil) || (err != nil && !errors.Is(err, carrier.ErrUnlocking)) {
				t.Fatalf("Validate: %v, accept %v", err, tc.Accept)
			}
			for _, in := range tx.Inputs {
				in.SourceTransaction = c.tree
			}
			ok, err := spv.Verify(c.ctx, tx, c.tracker, nil)
			if valid := ok && err == nil; valid != tc.ScriptValid {
				t.Fatalf("the interpreter says %v (%v), the vector %v", valid, err, tc.ScriptValid)
			}
		})
		if tc.Accept {
			accepted++
		} else if tc.ScriptValid {
			malleable++
		}
	}
	// A vector that lost its cases would pass every loop above by running
	// none of them.
	if len(v.Cases) < 21 || accepted != 1 || malleable == 0 {
		t.Fatalf("%d cases, %d accepted, %d malleations the interpreter takes", len(v.Cases), accepted, malleable)
	}
}

// go-sdk writes a low-S strict DER signature every time, so every carrier
// Mint builds passes. Many mints, each over another payload and so another
// nonce, would meet a high S about half the time if it did not.
func TestHonestCarriersAreCanonical(t *testing.T) {
	f := newFixture(t)
	const mints = 400
	for i := 0; i < mints; i++ {
		tx := f.mint([]byte(fmt.Sprintf("smp\x01object %d", i)), uint32(i%3)) //nolint:gosec // i%3 is below 3
		if err := carrier.CheckUnlocking(tx); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if err := f.decode(tx).Validate(params(), f.identity); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
}

// Each refusal CheckUnlocking gives, by its text, from one minted carrier.
func TestCheckUnlockingRefusals(t *testing.T) {
	f := newFixture(t)
	good := f.mint([]byte("smp\x01an object"), 0)
	u := []byte(*good.Inputs[0].UnlockingScript)
	sig := u[1:]
	der := sig[:len(sig)-1]
	push := func(b []byte) []byte { return append([]byte{byte(len(b))}, b...) }
	withDER := func(d []byte) []byte { return push(append(bytes.Clone(d), carrier.SigHashType)) }
	// intDER is a DER signature of the two given integer contents.
	intDER := func(r, s []byte) []byte {
		body := append([]byte{0x02, byte(len(r))}, r...)
		body = append(append(body, 0x02, byte(len(s))), s...)
		return append([]byte{0x30, byte(len(body))}, body...)
	}
	lenR := int(der[3])
	r, s := der[4:4+lenR], der[6+lenR:]
	order := ec.S256().N.Bytes()

	rows := []struct {
		name      string
		unlocking []byte
		why       string
	}{
		{"empty", nil, "empty"},
		{"OP_PUSHDATA1", append([]byte{0x4c, byte(len(sig))}, sig...), "not a minimal push"},
		{"OP_PUSHDATA4", append([]byte{0x4e, byte(len(sig)), 0, 0, 0}, sig...), "not a minimal push"},
		{"OP_0 first", append([]byte{0}, u...), "opcode 0x00 is not a signature push"},
		{"OP_NOP first", append([]byte{0x61}, u...), "opcode 0x61 is not a signature push"},
		{"OP_NOP after", append(bytes.Clone(u), 0x61), "more than one push or opcode"},
		{"two pushes", append(bytes.Clone(u), u...), "more than one push or opcode"},
		{"truncated", u[:len(u)-1], "truncated push"},
		{"sighash 0x01", push(append(bytes.Clone(der), 0x01)), "sighash type 0x01, want 0x41"},
		{"no sighash byte", push(der), "sighash type 0x" + fmt.Sprintf("%02x", der[len(der)-1]) + ", want 0x41"},
		{"short", withDER([]byte{0x30, 0x05, 0x02, 0x01, 0x01, 0x02, 0x01}), "signature length out of range"},
		{"not a sequence", withDER(append([]byte{0x31}, der[1:]...)), "signature is not one DER sequence"},
		{"sequence length", withDER(append([]byte{0x30, der[1] + 1}, der[2:]...)), "signature is not one DER sequence"},
		{"R tag", withDER(append([]byte{0x30, der[1], 0x03}, der[3:]...)), "R is not a DER integer"},
		{"R empty", withDER(intDER(nil, s)), "R is not a DER integer"},
		{"S empty", withDER(intDER(r, nil)), "S is not a DER integer"},
		{"a byte after S", withDER(append(bytes.Clone(der), 0)), "signature is not one DER sequence"},
		{"R negative", withDER(intDER([]byte{0x80, 1}, s)), "R is negative"},
		{"S negative", withDER(intDER(r, []byte{0x80, 1})), "S is negative"},
		{"R padded", withDER(intDER([]byte{0, 1}, s)), "R is padded"},
		{"S padded", withDER(intDER(r, []byte{0, 1})), "S is padded"},
		{"R zero", withDER(intDER([]byte{0}, s)), "R out of range"},
		{"R the order", withDER(intDER(append([]byte{0}, order...), s)), "R out of range"},
		{"S zero", withDER(intDER(r, []byte{0})), "S is zero"},
		{"S high", flipS(t, u), "S is high"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			tx := good.ShallowClone()
			tx.Inputs = []*transaction.TransactionInput{{SourceTXID: good.Inputs[0].SourceTXID,
				SourceTxOutIndex: good.Inputs[0].SourceTxOutIndex, SequenceNumber: carrier.Sequence}}
			if row.unlocking != nil {
				s := script.Script(row.unlocking)
				tx.Inputs[0].UnlockingScript = &s
			}
			err := carrier.CheckUnlocking(tx)
			want := carrier.ErrUnlocking.Error() + ": " + row.why
			if !errors.Is(err, carrier.ErrUnlocking) || err.Error() != want {
				t.Fatalf("%v, want %q", err, want)
			}
		})
	}

	if err := carrier.CheckUnlocking(good); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, tx := range map[string]*transaction.Transaction{
		"nil":        nil,
		"no inputs":  {},
		"two inputs": {Inputs: []*transaction.TransactionInput{good.Inputs[0], good.Inputs[0]}},
		"nil input":  {Inputs: []*transaction.TransactionInput{nil}},
	} {
		err := carrier.CheckUnlocking(tx)
		if !errors.Is(err, carrier.ErrUnlocking) || !strings.HasPrefix(err.Error(), carrier.ErrUnlocking.Error()+": ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
