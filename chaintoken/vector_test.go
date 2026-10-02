package chaintoken_test

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
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/goldentest"
)

// tokenVector is testdata/vectors/chaintoken-v1.json. tools/vectors built
// its transactions with go-sdk's primitives and templates, its blocks and
// paths from its own merkle trees, and its BEEFs by hand, one field at a
// time; each verdict is the case's construction.
type tokenVector struct {
	StateLockingKeyHex string `json:"stateLockingKeyHex"`
	Blocks             []struct {
		Height  uint32 `json:"height"`
		RootHex string `json:"rootHex"`
	} `json:"blocks"`
	Shapes []struct {
		Name        string `json:"name"`
		Shape       string `json:"shape"`
		Reads       bool   `json:"reads"`
		Accept      bool   `json:"accept"`
		SubjectTxid string `json:"subjectTxid"`
		ParentTxid  string `json:"parentTxid"`
		Spends      []struct {
			Input int    `json:"input"`
			Vout  uint32 `json:"vout"`
		} `json:"spends"`
		Mined   *bool  `json:"mined"`
		BeefHex string `json:"beefHex"`
	} `json:"shapes"`
	Replays []struct {
		Name            string `json:"name"`
		TokenStoredHex  string `json:"tokenStoredHex"`
		ParentStoredHex string `json:"parentStoredHex"`
		TokenTxid       string `json:"tokenTxid"`
		ParentTxid      string `json:"parentTxid"`
		BeefHex         string `json:"beefHex"`
	} `json:"replays"`
	Outputs []struct {
		Name         string   `json:"name"`
		Satoshis     uint64   `json:"satoshis"`
		Fields       int      `json:"fields"`
		ScriptHex    string   `json:"scriptHex"`
		Reads        bool     `json:"reads"`
		FieldsHex    []string `json:"fieldsHex"`
		SignatureHex string   `json:"signatureHex"`
		Locked       bool     `json:"locked"`
		Signed       bool     `json:"signed"`
	} `json:"outputs"`
	Signatures []struct {
		Name   string `json:"name"`
		Strict bool   `json:"strict"`
		Hex    string `json:"hex"`
	} `json:"signatures"`
}

func loadVector(t testing.TB) (*tokenVector, *goldentest.Tracker) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "chaintoken-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v tokenVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	tr := &goldentest.Tracker{Roots: map[uint32]string{}}
	for _, b := range v.Blocks {
		tr.Roots[b.Height] = b.RootHex
		if b.Height > tr.Tip {
			tr.Tip = b.Height
		}
	}
	return &v, tr
}

// Every BEEF reads or not as the vector says, names the vector's subject,
// and is exactly what its shape needs or not. An accepted token's parent,
// the inputs that spend it and whether both proofs verify are the vector's.
func TestVectorShapes(t *testing.T) {
	v, tracker := loadVector(t)
	ctx := context.Background()
	if len(v.Shapes) < 45 {
		t.Fatalf("%d shape cases; the vector is not the one this test was written for", len(v.Shapes))
	}
	accepted := map[string]int{}
	for _, c := range v.Shapes {
		w, err := chaintoken.ReadWire(goldentest.Hex(t, c.BeefHex), 0)
		if (err == nil) != c.Reads {
			t.Errorf("%s: reads %v (%v), want %v", c.Name, err == nil, err, c.Reads)
			continue
		}
		if err != nil {
			if !errors.Is(err, chaintoken.ErrBEEF) {
				t.Errorf("%s: refused with %v, want ErrBEEF", c.Name, err)
			}
			continue
		}
		tx := w.SubjectTx()
		if got := tx.TxID().String(); got != c.SubjectTxid {
			t.Errorf("%s: subject %s, want %s", c.Name, got, c.SubjectTxid)
			continue
		}
		var parent *chaintoken.Entry
		switch c.Shape {
		case "token":
			parent, err = w.Token(tx)
		case "carrier":
			parent, err = w.Carrier(tx)
		case "alone":
			err = w.Alone(tx)
		default:
			t.Fatalf("%s: unknown shape %q", c.Name, c.Shape)
		}
		if (err == nil) != c.Accept {
			t.Errorf("%s: accepted %v (%v), want %v", c.Name, err == nil, err, c.Accept)
			continue
		}
		if err != nil {
			if !errors.Is(err, chaintoken.ErrBEEF) {
				t.Errorf("%s: refused with %v, want ErrBEEF", c.Name, err)
			}
			continue
		}
		accepted[c.Shape]++
		if c.Shape == "alone" {
			continue
		}
		if got := parent.Txid.String(); got != c.ParentTxid || parent.Tx == nil {
			t.Errorf("%s: parent %s, want %s", c.Name, got, c.ParentTxid)
			continue
		}
		if c.Shape != "token" {
			continue
		}
		spends := chaintoken.Spends(tx, parent)
		if len(spends) != len(c.Spends) {
			t.Errorf("%s: %d inputs spend the parent, want %d", c.Name, len(spends), len(c.Spends))
		}
		for i, s := range spends {
			if i < len(c.Spends) && (s.Input != c.Spends[i].Input || s.Vout != c.Spends[i].Vout || s.Output != parent.Tx.Outputs[s.Vout]) {
				t.Errorf("%s: spend %d is input %d of output %d", c.Name, i, s.Input, s.Vout)
			}
		}
		if c.Mined == nil {
			t.Errorf("%s: the vector does not say whether it is mined", c.Name)
			continue
		}
		if got := chaintoken.Mined(ctx, tx, tracker) && chaintoken.Mined(ctx, parent.Tx, tracker); got != *c.Mined {
			t.Errorf("%s: mined %v, want %v", c.Name, got, *c.Mined)
		}
	}
	for shape, min := range map[string]int{"token": 8, "carrier": 3, "alone": 2} {
		if accepted[shape] < min {
			t.Errorf("%d accepted %s cases, want at least %d", accepted[shape], shape, min)
		}
	}
}

// A replayer's BEEF is the vector's, byte for byte, from each transaction
// as a host stores it; it reads back as a token and its parent; and the
// stored transactions are left as they were.
func TestVectorReplays(t *testing.T) {
	v, tracker := loadVector(t)
	ctx := context.Background()
	if len(v.Replays) < 4 {
		t.Fatalf("%d replay cases", len(v.Replays))
	}
	for _, c := range v.Replays {
		token, err := chaintoken.Stored(goldentest.Hex(t, c.TokenStoredHex), 0)
		if err != nil || token.TxID().String() != c.TokenTxid || token.MerklePath == nil {
			t.Errorf("%s: the stored token: %v", c.Name, err)
			continue
		}
		parent, err := chaintoken.Stored(goldentest.Hex(t, c.ParentStoredHex), 0)
		if err != nil || parent.TxID().String() != c.ParentTxid || parent.MerklePath == nil {
			t.Errorf("%s: the stored parent: %v", c.Name, err)
			continue
		}
		before := [2]string{token.MerklePath.Hex(), parent.MerklePath.Hex()}
		beef, err := chaintoken.TokenBEEF(token, parent)
		if err != nil || hex.EncodeToString(beef) != c.BeefHex {
			t.Errorf("%s: assembled %x (%v)", c.Name, beef, err)
			continue
		}
		if token.MerklePath.Hex() != before[0] || parent.MerklePath.Hex() != before[1] {
			t.Errorf("%s: TokenBEEF changed a stored transaction's path", c.Name)
		}
		w, err := chaintoken.ReadWire(beef, 0)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		p, err := w.Token(w.SubjectTx())
		if err != nil || p.Txid.String() != c.ParentTxid {
			t.Errorf("%s: the assembled BEEF is not a token and its parent: %v", c.Name, err)
			continue
		}
		if !chaintoken.Mined(ctx, w.SubjectTx(), tracker) || !chaintoken.Mined(ctx, p.Tx, tracker) {
			t.Errorf("%s: a proof in the assembled BEEF does not verify", c.Name)
		}
	}
	if _, err := chaintoken.TokenBEEF(nil, nil); err == nil {
		t.Error("TokenBEEF of nothing")
	}
}

func TestVectorOutputs(t *testing.T) {
	v, _ := loadVector(t)
	key, err := ec.PublicKeyFromBytes(goldentest.Hex(t, v.StateLockingKeyHex))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Outputs) < 18 {
		t.Fatalf("%d output cases", len(v.Outputs))
	}
	for _, c := range v.Outputs {
		s := goldentest.Hex(t, c.ScriptHex)
		ls := scriptOf(s)
		out, ok := chaintoken.ReadOutput(&transaction.TransactionOutput{Satoshis: c.Satoshis, LockingScript: ls}, 3, c.Fields)
		if ok != c.Reads {
			t.Errorf("%s: reads %v, want %v", c.Name, ok, c.Reads)
			continue
		}
		if !ok {
			continue
		}
		if out.Vout != 3 || len(out.Fields) != len(c.FieldsHex) || hex.EncodeToString(out.Signature) != c.SignatureHex {
			t.Errorf("%s: read %d fields and signature %x", c.Name, len(out.Fields), out.Signature)
			continue
		}
		var signed []byte
		for i, f := range out.Fields {
			if hex.EncodeToString(f) != c.FieldsHex[i] {
				t.Errorf("%s: field %d is %x", c.Name, i, f)
			}
			signed = append(signed, f...)
		}
		if !bytes.Equal(out.Signed(), signed) {
			t.Errorf("%s: Signed is not the fields concatenated", c.Name)
		}
		if got := out.LockedTo(s, key); got != c.Locked {
			t.Errorf("%s: locked %v, want %v", c.Name, got, c.Locked)
		}
		if got := out.SignedBy(key); got != c.Signed {
			t.Errorf("%s: signed %v, want %v", c.Name, got, c.Signed)
		}
	}
	if _, ok := chaintoken.ReadOutput(nil, 0, 2); ok {
		t.Error("a nil output reads")
	}
	if _, ok := chaintoken.ReadOutput(&transaction.TransactionOutput{Satoshis: 1}, 0, 2); ok {
		t.Error("an output with no script reads")
	}
}

func TestVectorSignatures(t *testing.T) {
	v, _ := loadVector(t)
	if len(v.Signatures) < 10 {
		t.Fatalf("%d signature cases", len(v.Signatures))
	}
	for _, c := range v.Signatures {
		err := chaintoken.CheckDER(goldentest.Hex(t, c.Hex))
		if (err == nil) != c.Strict || (err != nil && !errors.Is(err, chaintoken.ErrSignature)) {
			t.Errorf("%s: %v, want strict %v", c.Name, err, c.Strict)
		}
	}
}

// Mined answers false, never an error, for everything that is not a proven
// transaction and a header source.
func TestMinedNeedsEverything(t *testing.T) {
	v, tracker := loadVector(t)
	ctx := context.Background()
	tx, err := chaintoken.Stored(goldentest.Hex(t, v.Replays[0].TokenStoredHex), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !chaintoken.Mined(ctx, tx, tracker) {
		t.Fatal("the stored token is not mined")
	}
	if chaintoken.Mined(ctx, nil, tracker) || chaintoken.Mined(ctx, tx, nil) {
		t.Error("mined with no transaction or no headers")
	}
	bare := *tx
	bare.MerklePath = nil
	if chaintoken.Mined(ctx, &bare, tracker) {
		t.Error("mined with no proof")
	}
	if chaintoken.Mined(ctx, tx, &goldentest.Tracker{Roots: map[uint32]string{tx.MerklePath.BlockHeight: chainhash.Hash{}.String()}}) {
		t.Error("mined against another root")
	}
}

// The bound is the caller's: a BEEF one byte over it is refused before it
// is read, and zero means the default.
func TestBound(t *testing.T) {
	v, _ := loadVector(t)
	b := goldentest.Hex(t, v.Shapes[0].BeefHex)
	if _, err := chaintoken.ReadWire(b, len(b)); err != nil {
		t.Errorf("at the bound: %v", err)
	}
	if _, err := chaintoken.ReadWire(b, len(b)-1); !errors.Is(err, chaintoken.ErrBEEF) {
		t.Errorf("one over the bound: %v", err)
	}
	if _, err := chaintoken.Stored(b, len(b)-1); !errors.Is(err, chaintoken.ErrBEEF) {
		t.Errorf("Stored one over the bound: %v", err)
	}
	if _, err := chaintoken.ReadWire(make([]byte, chaintoken.DefaultMaxBEEF+1), 0); !errors.Is(err, chaintoken.ErrBEEF) {
		t.Errorf("over the default bound: %v", err)
	}
}

func scriptOf(b []byte) *script.Script {
	s := script.Script(b)
	return &s
}
