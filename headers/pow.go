package headers

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// ErrProofOfWork is a header that does not carry the work it claims, or
// claims less than its network's floor. It is the header source lying (or
// serving another chain), never a proof that failed: a caller must not read
// it as "not yet".
var ErrProofOfWork = errors.New("headers: header fails its proof of work")

// header is the six fields a block header commits to, plus the hash and
// height the source claims for it.
type header struct {
	Height  uint32
	Version uint32
	Prev    chainhash.Hash
	Root    chainhash.Hash
	Time    uint32
	Bits    uint32
	Nonce   uint32
	Hash    chainhash.Hash
}

// serialize is the 80 bytes that are hashed. Hashes go in internal byte
// order, which is what chainhash holds.
func (h *header) serialize() []byte {
	b := make([]byte, 80)
	binary.LittleEndian.PutUint32(b[0:], h.Version)
	copy(b[4:36], h.Prev[:])
	copy(b[36:68], h.Root[:])
	binary.LittleEndian.PutUint32(b[68:], h.Time)
	binary.LittleEndian.PutUint32(b[72:], h.Bits)
	binary.LittleEndian.PutUint32(b[76:], h.Nonce)
	return b
}

// compactToBig expands the compact target encoding. A negative or zero
// target is invalid.
func compactToBig(bits uint32) *big.Int {
	mantissa := int64(bits & 0x007fffff)
	exp := uint(bits >> 24)
	n := big.NewInt(mantissa)
	if exp <= 3 {
		n.Rsh(n, 8*(3-exp))
	} else {
		n.Lsh(n, 8*(exp-3))
	}
	if bits&0x00800000 != 0 {
		n.Neg(n)
	}
	return n
}

// hashToBig reads a hash as the number it is compared to the target as.
func hashToBig(h chainhash.Hash) *big.Int {
	r := make([]byte, len(h))
	for i := range h {
		r[len(h)-1-i] = h[i]
	}
	return new(big.Int).SetBytes(r)
}

// maxTarget is difficulty 1 (compact 0x1d00ffff).
var maxTarget = compactToBig(0x1d00ffff)

// floorTarget is the largest target a header may claim at minDifficulty.
func floorTarget(minDifficulty float64) *big.Int {
	f := new(big.Float).SetInt(maxTarget)
	f.Quo(f, big.NewFloat(minDifficulty))
	t, _ := f.Int(nil)
	return t
}

// check proves the header is what the source says it is: its 80 bytes hash
// to the claimed hash, the hash is at or under the header's own target, and
// the target is at or under the floor when there is one.
func (h *header) check(want uint32, minDifficulty float64) error {
	if h.Height != want {
		return fmt.Errorf("%w: asked for height %d, answered for %d", ErrProofOfWork, want, h.Height)
	}
	got := chainhash.DoubleHashH(h.serialize())
	if got != h.Hash {
		return fmt.Errorf("%w: height %d: the fields hash to %s, not the claimed %s", ErrProofOfWork, want, got, h.Hash)
	}
	target := compactToBig(h.Bits)
	if target.Sign() <= 0 {
		return fmt.Errorf("%w: height %d: bits %08x is not a target", ErrProofOfWork, want, h.Bits)
	}
	if hashToBig(got).Cmp(target) > 0 {
		return fmt.Errorf("%w: height %d: hash is above its own target", ErrProofOfWork, want)
	}
	if minDifficulty > 0 && target.Cmp(floorTarget(minDifficulty)) > 0 {
		return fmt.Errorf("%w: height %d: bits %08x claim less work than the network floor", ErrProofOfWork, want, h.Bits)
	}
	return nil
}

// minDifficulty is the floor for the client's network.
func (c *Client) minDifficulty() float64 {
	if c.MinDifficulty > 0 {
		return c.MinDifficulty
	}
	if c.Network == Mainnet {
		return MainnetMinDifficulty
	}
	return 0
}

// flexUint reads a number that one source renders as a JSON number and
// another as hex in a string (WhatsOnChain's bits).
type flexUint uint32

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	base := 10
	if len(b) > 0 && b[0] == '"' {
		base = 16
	}
	v, err := strconv.ParseUint(s, base, 32)
	if err != nil {
		return err
	}
	*f = flexUint(v)
	return nil
}

type wireHeader struct {
	Height  uint32   `json:"height"`
	Version uint32   `json:"version"`
	Hash    string   `json:"hash"`
	Root    string   `json:"merkleRoot"`
	WocRoot string   `json:"merkleroot"`
	Prev    string   `json:"previousHash"`
	WocPrev string   `json:"previousblockhash"`
	Time    uint32   `json:"time"`
	Bits    flexUint `json:"bits"`
	Nonce   uint32   `json:"nonce"`
}

func (w *wireHeader) decode() (*header, error) {
	root, prev := w.Root, w.Prev
	if root == "" {
		root = w.WocRoot
	}
	if prev == "" {
		prev = w.WocPrev
	}
	h := &header{Height: w.Height, Version: w.Version, Time: w.Time, Bits: uint32(w.Bits), Nonce: w.Nonce}
	for _, f := range []struct {
		name string
		hex  string
		into *chainhash.Hash
	}{{"hash", w.Hash, &h.Hash}, {"merkle root", root, &h.Root}, {"previous hash", prev, &h.Prev}} {
		if f.hex == "" {
			// Genesis has no previous hash to report.
			if f.into == &h.Prev && w.Height == 0 {
				continue
			}
			return nil, fmt.Errorf("header service: height %d: no %s", w.Height, f.name)
		}
		p, err := chainhash.NewHashFromHex(f.hex)
		if err != nil {
			return nil, fmt.Errorf("header service: height %d: %s: %w", w.Height, f.name, err)
		}
		*f.into = *p
	}
	return h, nil
}

// fullHeader fetches the header at height from a source that serves header
// fields. A 404 is (nil, nil): the source does not know that height.
func (c *Client) fullHeader(ctx context.Context, height uint32) (*header, error) {
	h := strconv.FormatUint(uint64(height), 10)
	var w wireHeader
	var status int
	var err error
	switch c.Kind {
	case WhatsOnChain:
		status, err = c.get(ctx, "/block/"+h+"/header", &w)
	case Chaintracks:
		var env struct {
			Status string          `json:"status"`
			Value  json.RawMessage `json:"value"`
		}
		status, err = c.get(ctx, "/header/height/"+h, &env)
		if err == nil && status == http.StatusOK {
			// chaintracks answers an unknown height with a 404; a 200 that is
			// not a success is a broken answer, not "not yet".
			if env.Status != "success" || len(env.Value) == 0 || string(env.Value) == "null" {
				return nil, fmt.Errorf("header service: header at height %d: status %q with no header", height, env.Status)
			}
			err = json.Unmarshal(env.Value, &w)
		}
	default:
		return nil, fmt.Errorf("header service: %s sources carry no header fields", c.Kind)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("header service: header at height %d: status %d", height, status)
	}
	return w.decode()
}
