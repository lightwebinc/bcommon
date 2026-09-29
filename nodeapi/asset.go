package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// Asset is a Teranode asset-HTTP-API client.
type Asset struct {
	// Base is the API root with no path suffix, e.g. http://node.example.com:20090.
	Base string
	// Client is optional; the default has a 30s timeout.
	Client *http.Client
}

// get fetches one path with the 429 ladder and the body bound applied. A
// non-200 is an *HTTPError so callers can tell a 404 from a transport error.
func (a *Asset) get(ctx context.Context, path string) ([]byte, error) {
	client := clientOr(a.Client, 30*time.Second)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.Base, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		// limit+1 so the check below sees an over-bound body instead of a
		// truncated one. The check waits until after the status is read, so a
		// node that pads an error page still answers 404 here and a caller
		// still reads that as ErrNotMined.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < len(backoff429) {
			if err := sleep(ctx, backoff429[attempt]); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &HTTPError{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Body: trim(raw)}
		}
		if len(raw) > maxBody {
			return nil, fmt.Errorf("GET %s: %w (%d bytes)", path, ErrBodyTooLarge, maxBody)
		}
		return raw, nil
	}
}

func getJSON[T any](ctx context.Context, a *Asset, path string) (*T, error) {
	raw, err := a.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return &v, nil
}

// Header is the subset of a block header answer a caller needs.
type Header struct {
	Hash       string `json:"hash"`
	Prev       string `json:"previousblockhash"`
	MerkleRoot string `json:"merkleroot"`
	Height     uint32 `json:"height"`
}

// BestHeader reads the tip.
func (a *Asset) BestHeader(ctx context.Context) (*Header, error) {
	return getJSON[Header](ctx, a, "/api/v1/bestblockheader/json")
}

// Header reads one block header by hash.
func (a *Asset) Header(ctx context.Context, hash string) (*Header, error) {
	return getJSON[Header](ctx, a, "/api/v1/header/"+hash+"/json")
}

// TxMeta is the placement subset of /api/v1/txmeta/{txid}/json. The slices
// are parallel, one entry per block the transaction landed in, and
// MainChainIndex selects the main-chain one.
type TxMeta struct {
	BlockHashes    []string `json:"blockHashes"`
	BlockHeights   []uint32 `json:"blockHeights"`
	SubtreeIdxs    []int    `json:"subtreeIdxs"`
	MainChainIndex int      `json:"mainChainIndex"`
	IsCoinbase     bool     `json:"isCoinbase"`
}

// Placement returns the main-chain block hash and height, or ErrNotMined
// when the node knows the transaction but has not placed it.
func (m *TxMeta) Placement() (hash string, height uint32, err error) {
	if len(m.BlockHashes) == 0 || len(m.BlockHashes) != len(m.BlockHeights) {
		return "", 0, ErrNotMined
	}
	i := m.MainChainIndex
	if i < 0 || i >= len(m.BlockHashes) {
		return "", 0, ErrNotMined
	}
	return m.BlockHashes[i], m.BlockHeights[i], nil
}

// TxMeta reads a transaction's placement. A 404 is ErrNotMined: for a
// submission in flight, "never seen" and "not placed yet" are one state.
func (a *Asset) TxMeta(ctx context.Context, txid string) (*TxMeta, error) {
	m, err := getJSON[TxMeta](ctx, a, "/api/v1/txmeta/"+txid+"/json")
	if err != nil {
		if IsHTTP(err, http.StatusNotFound) {
			return nil, ErrNotMined
		}
		return nil, err
	}
	return m, nil
}

// TxRaw returns a transaction's standard serialisation as the node stores it.
func (a *Asset) TxRaw(ctx context.Context, txid string) ([]byte, error) {
	return a.get(ctx, "/api/v1/tx/"+txid)
}

// MerkleProof returns the BRC-74 BUMP for a mined transaction, parsed.
//
// The node answers 404 for a transaction it has not placed in a main-chain
// block and 500 for a proof with an empty path (a single-transaction block);
// both are ErrNotMined to a caller that will ask again after the next block.
//
// The bytes are walked by the guard package before the SDK parses them, and
// the parse runs under a recover, so a malformed proof is an error and never
// a crash.
// The parsed path must also contain txid at its leaf level: a proof the node
// served for the wrong transaction verifies perfectly and proves nothing.
func (a *Asset) MerkleProof(ctx context.Context, txid string) (*transaction.MerklePath, error) {
	raw, err := a.get(ctx, "/api/v1/merkle_proof/"+txid)
	if err != nil {
		if IsHTTP(err, http.StatusNotFound) || IsHTTP(err, http.StatusInternalServerError) {
			return nil, ErrNotMined
		}
		return nil, err
	}
	mp, err := ProofFor(raw, txid)
	if err != nil {
		return nil, fmt.Errorf("merkle_proof %s: %w", txid, err)
	}
	return mp, nil
}

// ProofFor parses a BUMP someone else supplied as the proof for txid, and
// refuses one that does not name txid at its leaf level. Every source of a
// proof goes through here: the bytes come from a service, and a path for some
// other transaction would build a BEEF that no host accepts. The proof is held
// to the same bound as every answer this package reads.
func ProofFor(raw []byte, txid string) (*transaction.MerklePath, error) {
	mp, err := guard.ParseBUMP(raw, maxBody)
	if err != nil {
		return nil, err
	}
	want, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, err
	}
	if len(mp.Path) > 0 {
		for _, leaf := range mp.Path[0] {
			if leaf.Hash != nil && leaf.Hash.Equal(*want) {
				return mp, nil
			}
		}
	}
	return nil, errors.New("proof does not contain the txid")
}

// Block is the subset of /api/v1/block/{hash}/json read here: the coinbase
// outputs are what fund the pool.
type Block struct {
	Hash       string `json:"hash"`
	Height     uint32 `json:"height"`
	CoinbaseTx struct {
		TxID    string `json:"txid"`
		Outputs []struct {
			Satoshis      uint64 `json:"satoshis"`
			LockingScript string `json:"lockingScript"`
		} `json:"outputs"`
	} `json:"coinbase_tx"`
}

// Block reads one block by hash.
func (a *Asset) Block(ctx context.Context, hash string) (*Block, error) {
	return getJSON[Block](ctx, a, "/api/v1/block/"+hash+"/json")
}

// HashAtHeight resolves a main-chain block hash by height through the paged
// block list, where offset counts back from the tip. The tip can move between
// the two reads, so the answer is checked against the height asked for and
// the read is retried when it does.
func (a *Asset) HashAtHeight(ctx context.Context, height uint32) (string, error) {
	type page struct {
		Data []struct {
			Height uint32 `json:"height"`
			Hash   string `json:"hash"`
		} `json:"data"`
	}
	for attempt := 0; attempt < 3; attempt++ {
		tip, err := a.BestHeader(ctx)
		if err != nil {
			return "", err
		}
		if height > tip.Height {
			return "", fmt.Errorf("height %d is above the tip %d", height, tip.Height)
		}
		pg, err := getJSON[page](ctx, a, "/api/v1/blocks?limit=1&offset="+strconv.FormatUint(uint64(tip.Height-height), 10))
		if err != nil {
			return "", err
		}
		if len(pg.Data) == 1 && pg.Data[0].Height == height {
			return pg.Data[0].Hash, nil
		}
	}
	return "", fmt.Errorf("height %d: block list moved under the read", height)
}

// Proof asks once whether txid is mined and returns its proof and height if
// so, or ErrNotMined. It is WaitMined without the waiting, for a caller that
// collects proofs later rather than blocking on them.
func (a *Asset) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	return tryMined(ctx, a, txid)
}

// WaitMined polls until txid is placed in a main-chain block and its proof is
// served, or ctx ends. It returns the proof and the block height.
func WaitMined(ctx context.Context, asset *Asset, txid string, poll time.Duration) (*transaction.MerklePath, uint32, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		mp, height, err := tryMined(ctx, asset, txid)
		if err == nil {
			return mp, height, nil
		}
		if err != ErrNotMined {
			return nil, 0, err
		}
		if err := sleep(ctx, poll); err != nil {
			return nil, 0, fmt.Errorf("waiting for %s to mine: %w", txid, err)
		}
	}
}

func tryMined(ctx context.Context, asset *Asset, txid string) (*transaction.MerklePath, uint32, error) {
	meta, err := asset.TxMeta(ctx, txid)
	if err != nil {
		return nil, 0, err
	}
	_, height, err := meta.Placement()
	if err != nil {
		return nil, 0, err
	}
	mp, err := asset.MerkleProof(ctx, txid)
	if err != nil {
		return nil, 0, err
	}
	if mp.BlockHeight != height {
		// The proof names a block; the placement names a block. A node that
		// disagrees with itself is not one to build a BEEF from.
		return nil, 0, fmt.Errorf("merkle_proof %s: proof height %d, placement height %d", txid, mp.BlockHeight, height)
	}
	return mp, height, nil
}
