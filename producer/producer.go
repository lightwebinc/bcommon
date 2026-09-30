// Package producer is the orchestration a producer of committed records runs
// around the builders in mint and carrier: where the fee for a mined
// transaction comes from and where its change goes, the funding tree a
// carrier spends and when to mint the next one, the one copy of each
// transaction the producer keeps while it is unproven, and the collection of
// proofs for what was published before it mined.
//
// A producer publishes on two legs. The settlement leg takes mined
// transactions (a funding tree, a state token, an anchor) for mining; the
// object leg takes each one, and every carrier, as a BEEF to the overlay
// hosts. With proofs collected later rather than waited for, a transaction
// reaches the hosts before it mines, so its BEEF carries its unproven
// ancestry, and the producer keeps that BEEF until the proof arrives. When
// it does, the proven BEEF is published again, and every host upgrades the
// copy it holds.
//
// The pieces:
//
//   - Payer takes a fee input from a coin pool (bwallet.Pool), gives it back
//     if the transaction is not sent, takes change back into the pool, and
//     settles: hands a transaction to the settlement leg and waits for its
//     proof or, with Async, returns once the leg has accepted it.
//   - Kept is the cache that makes every use of one kept transaction the
//     same object, so a BEEF built from it never merges two copies.
//   - Trees is the funding-tree lifecycle: spend from the current tree, or
//     mint, settle, record and publish the next one when the current tree
//     cannot fund what is asked; with Ahead, the next tree is minted and
//     settled in the background before the current one runs out.
//   - Proofs asks whether a transaction mined; Collector asks it for
//     everything still waiting, records each proof, republishes the proven
//     transaction, and releases change the pool held back.
//
// The application keeps its own state (a state file in its own format) and
// the words for its own commands. The library reaches that state through
// TreeState, Kept.Load and the Pending callbacks, and reports progress
// through a Note function in plain lines that name no application. What a
// user should do about a refusal is the application's to say, so the
// refusals an application may want to word itself come back as typed errors
// (NoCoinError, NoKeyError) or through Pending's Refused and Unbuilt hooks.
//
// Nothing here is safe for concurrent use: one producer run owns its Payer,
// Kept and Trees. The one goroutine the package starts itself is a Trees
// minting ahead (Trees.Ahead), and it touches none of them: the tree's fee
// coin is reserved and signed on the caller's goroutine, and the background
// only settles it, through the Payer's Settler and Asset, or calls Fund.
// Those, and the Settler's own Note, must tolerate being used from that
// goroutine while the application uses them from its own; the ones in
// publish and nodeapi do. The background's notes are held and reported
// through the Payer's Note by the Spend or Wait that collects its result.
package producer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// ErrRefused is a published transaction the network will not mine. It is
// not an ordinary "not yet": hosts hold a state built on it that will never
// be real, and the operator has to know.
var ErrRefused = errors.New("refused by the network")

// Proofs asks whoever can answer whether a transaction has mined.
type Proofs struct {
	// Arcade, when set, is asked first: an arcade installation tracks what
	// it broadcast and reports a refusal, which a node cannot. It does not
	// know a transaction that reached the network some other way, and then
	// Asset answers.
	Arcade *publish.Arcade
	// Asset is the node's asset API. With neither set, nothing has mined.
	Asset *nodeapi.Asset
}

// Of returns txid's proof and block height if it has mined. It returns
// nodeapi.ErrNotMined when it has not yet, and an error wrapping ErrRefused
// when arcade reports that the network refused it.
//
// Arcade's proof is held to the checks a node's is: it must parse through
// the BUMP guard, name the transaction asked about, and agree with the block
// height arcade reported. A proof of some other transaction verifies
// perfectly and proves nothing.
func (p Proofs) Of(ctx context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	if p.Arcade != nil {
		st, err := p.Arcade.Status(ctx, txid)
		switch {
		case err == nil && st.Refused():
			return nil, 0, fmt.Errorf("%w: %s", ErrRefused, st.Why())
		case err == nil && st.Mined():
			raw, herr := hex.DecodeString(st.MerklePath)
			if herr != nil {
				return nil, 0, fmt.Errorf("arcade's proof for %s is not hex: %w", txid, herr)
			}
			mp, perr := nodeapi.ProofFor(raw, txid)
			if perr != nil {
				return nil, 0, fmt.Errorf("arcade's proof for %s: %w", txid, perr)
			}
			if st.BlockHeight != 0 && st.BlockHeight != mp.BlockHeight {
				// The same disagreement a node is refused for.
				return nil, 0, fmt.Errorf("arcade's proof for %s: proof height %d, reported height %d", txid, mp.BlockHeight, st.BlockHeight)
			}
			return mp, mp.BlockHeight, nil
		case err == nil:
			return nil, 0, nodeapi.ErrNotMined
		case !errors.Is(err, publish.ErrArcadeUnknown):
			return nil, 0, err
		}
	}
	if p.Asset == nil {
		return nil, 0, nodeapi.ErrNotMined
	}
	return p.Asset.Proof(ctx, txid)
}

// Kept is the one in-memory copy of each transaction a producer published
// and still keeps, by txid.
//
// A transaction the producer spends before it mines (a state token that is
// the next transition's previous input, a funding tree whose change pays the
// next fee) can be needed in more than one place in one run. Two copies of
// it, one that carries its ancestry and one that does not, would leave a
// BEEF built from both depending on which copy it merged first. Every place
// asks Kept, so every place gets the same object, and a proof collected
// during the run reaches that object too.
//
// The zero value is usable once Load is set.
type Kept struct {
	// Load rebuilds a transaction the application keeps from its own state,
	// usually with funding.Rebuild. Its error is returned as it is, so it is
	// also how the application words "not one of mine".
	Load func(txid string) (*transaction.Transaction, error)

	txs map[string]*transaction.Transaction
}

// Tx returns the kept transaction txid: the copy already handed out, or one
// Load rebuilds, which is then the copy every later call gets. A failed Load
// is not remembered.
func (k *Kept) Tx(txid string) (*transaction.Transaction, error) {
	if k == nil {
		return nil, fmt.Errorf("producer: no kept transactions, so none is %s", txid)
	}
	if tx, ok := k.txs[txid]; ok {
		return tx, nil
	}
	if k.Load == nil {
		return nil, fmt.Errorf("producer: no loader for kept transaction %s", txid)
	}
	tx, err := k.Load(txid)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, fmt.Errorf("producer: the loader returned no transaction for %s", txid)
	}
	if k.txs == nil {
		k.txs = map[string]*transaction.Transaction{}
	}
	k.txs[txid] = tx
	return tx, nil
}

// Prove gives the copy of txid already handed out, if there is one, its
// proof, so a transaction being built that holds it stops its BEEF at the
// proof rather than carrying the ancestry. A nil Kept holds nothing.
func (k *Kept) Prove(txid string, mp *transaction.MerklePath) {
	if k == nil {
		return
	}
	if tx, ok := k.txs[txid]; ok {
		tx.MerklePath = mp
	}
}
