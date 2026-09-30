package carrier_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
)

// The record and funding cases of the shared PushDrop vector. Decode takes a
// carrier whose record output is the canonical lock and refuses one written
// any other way as ErrShape; DecodeFunding reads a funding output only in
// its canonical form. go-sdk's decoder reads most of the refused ones.
func TestCanonicalLocksIndependentVector(t *testing.T) {
	c := readVectorChain(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "pushdrop-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		FundingTagHex string `json:"fundingTagHex"`
		Cases         []struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Accept  bool   `json:"accept"`
			LockHex string `json:"lockHex"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	tag := goldentest.Hex(t, v.FundingTagHex)
	payload := goldentest.Hex(t, c.v.Carriers[0].PayloadHex)
	ran := map[string]int{}
	for _, tc := range v.Cases {
		t.Run(tc.Kind+"/"+tc.Name, func(t *testing.T) {
			lock := script.Script(goldentest.Hex(t, tc.LockHex))
			switch tc.Kind {
			case "funding":
				if _, ok := carrier.DecodeFunding(&lock, tag); ok != tc.Accept {
					t.Fatalf("DecodeFunding: %v, accept %v", ok, tc.Accept)
				}
			case "record":
				tx := goldentest.Tx(t, c.v.Carriers[0].TxHex)
				tx.Outputs[0].LockingScript = &lock
				_, err := carrier.Decode(tx, func(p []byte) (bool, error) { return bytes.Equal(p, payload), nil })
				if tc.Accept != (err == nil) || (err != nil && !errors.Is(err, carrier.ErrShape)) {
					t.Fatalf("Decode: %v, accept %v", err, tc.Accept)
				}
			default:
				return
			}
			ran[tc.Kind]++
		})
	}
	if ran["funding"] == 0 || ran["record"] == 0 {
		t.Fatalf("cases run: %v", ran)
	}
}
