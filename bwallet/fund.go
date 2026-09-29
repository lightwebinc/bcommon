package bwallet

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// DefaultFundBatch is how many blocks one generatetoaddress call asks for.
// It is kept small because a large call can outlast the RPC timeout, most
// of all while another miner contends for the chain.
const DefaultFundBatch = 30

// FundFromCoinbase mines blocks paying the fund address and adds each block's
// coinbase outputs that pay FundScript to the pool, marked Coinbase with
// their height so Take applies maturity. It returns how many outputs were
// added and every block hash mined, including blocks whose coinbase paid
// nothing to us.
//
// This is the funding rule: the pool is funded from fresh coinbase paid to
// the application's own fund address, never by sharing outputs another
// wallet also spends from, so nothing else can spend what the pool counts
// on. Mining is a deliberate step, never an unattended one.
func FundFromCoinbase(ctx context.Context, e *Signer, pool *Pool, rpc *nodeapi.RPC, asset *nodeapi.Asset, blocks, batch int) (added int, hashes []string, err error) {
	if blocks <= 0 {
		return 0, nil, fmt.Errorf("bwallet: fund: blocks must be positive, got %d", blocks)
	}
	if batch <= 0 {
		batch = DefaultFundBatch
	}
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return 0, nil, err
	}
	for mined := 0; mined < blocks; {
		n := min(batch, blocks-mined)
		got, err := rpc.GenerateToAddress(ctx, n, addr)
		if err != nil {
			return added, hashes, fmt.Errorf("bwallet: generatetoaddress %d: %w", n, err)
		}
		if len(got) == 0 {
			return added, hashes, fmt.Errorf("bwallet: generatetoaddress %d returned no block hashes", n)
		}
		// Add per batch, not at the end: a failure in a later batch still
		// leaves the earlier blocks' outputs on disk, and Rescan can pick up
		// whatever this did not.
		n2, err := addCoinbase(ctx, e, pool, asset, got)
		added += n2
		hashes = append(hashes, got...)
		if err != nil {
			return added, hashes, err
		}
		mined += len(got)
	}
	return added, hashes, nil
}

// Rescan walks heights fromHeight..toHeight inclusive and adds any coinbase
// output paying FundScript that the pool does not already hold. It is the
// recovery for a funding run that stopped between mining and adding.
func Rescan(ctx context.Context, e *Signer, pool *Pool, asset *nodeapi.Asset, fromHeight, toHeight uint32) (int, error) {
	if toHeight < fromHeight {
		return 0, fmt.Errorf("bwallet: rescan: toHeight %d below fromHeight %d", toHeight, fromHeight)
	}
	added := 0
	for h := fromHeight; ; h++ {
		hash, err := asset.HashAtHeight(ctx, h)
		if err != nil {
			return added, fmt.Errorf("bwallet: rescan height %d: %w", h, err)
		}
		n, err := addCoinbase(ctx, e, pool, asset, []string{hash})
		added += n
		if err != nil {
			return added, err
		}
		if h == toHeight {
			return added, nil
		}
	}
}

// addCoinbase reads each block and adds its coinbase outputs paying us.
func addCoinbase(ctx context.Context, e *Signer, pool *Pool, asset *nodeapi.Asset, hashes []string) (int, error) {
	lock, err := e.FundScript()
	if err != nil {
		return 0, err
	}
	ours := hex.EncodeToString(*lock)
	added := 0
	for _, h := range hashes {
		blk, err := asset.Block(ctx, h)
		if err != nil {
			return added, fmt.Errorf("bwallet: block %s: %w", h, err)
		}
		var outs []Output
		for i, out := range blk.CoinbaseTx.Outputs {
			if out.LockingScript != ours {
				continue
			}
			outs = append(outs, Output{
				TxID:          blk.CoinbaseTx.TxID,
				Vout:          uint32(i),
				Satoshis:      out.Satoshis,
				LockingScript: out.LockingScript,
				Height:        blk.Height,
				Coinbase:      true,
			})
		}
		n, err := pool.Add(outs...)
		added += n
		if err != nil {
			return added, err
		}
	}
	return added, nil
}
