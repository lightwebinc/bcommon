package producer

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// Pending is one transaction the producer published before it mined and
// keeps until its proof is collected: a funding tree, a state token, an
// anchor.
type Pending struct {
	// What names the transaction in the lines Collect reports ("funding
	// tree").
	What string
	// Txid, RawHex and BeefHex are the kept copy: its id, its raw bytes, and
	// the BEEF kept while it is unmined (funding.KeepBEEF), which the proven
	// transaction is rebuilt from to be published again.
	Txid    string
	RawHex  string
	BeefHex string
	// Proven records the proof in the application's state: the proof's
	// hex (funding.BumpHex), the height, and the kept BEEF cleared, which a
	// proven transaction no longer needs. Collect saves the state after it.
	Proven func(mp *transaction.MerklePath, height uint32)
	// Stamp marks the journal entry (Seq, Txid) mined once the proof is
	// saved.
	Stamp bool
	Seq   uint64
	// Refused reports that the network refused the transaction, which
	// means hosts hold something built on it that will never mine. The
	// error wraps ErrRefused. Nil reports it in a generic warning line; an
	// application usually says what it means for its users.
	Refused func(err error)
	// Unbuilt reports a transaction that mined but whose kept copy does not
	// rebuild; its proof is then not recorded. Nil reports it in a generic
	// note.
	Unbuilt func(err error)
}

// Collector collects the proofs of what a producer published before it
// mined, and publishes each proven transaction again.
//
// The republish is the point. A host admitted the unproven copy and will go
// on serving it until it sees the proof; the proven BEEF is different bytes,
// so the plane delivers it to every host like any other object, and each
// host verifies the path against its own headers and upgrades what it
// holds. One submission reaches all of them, and no host asks anyone.
//
// Collect never fails the run it is part of: a proof that has not arrived is
// the ordinary case, and a check that could not be made is a note. The one
// loud outcome is a refusal.
type Collector struct {
	// Proofs answers whether each transaction mined.
	Proofs Proofs
	// Kept, when set, gives the copy of a proven transaction already handed
	// out in this run its proof too.
	Kept *Kept
	// Pool, when set, has the change it holds back until a proof arrives
	// released as each proof is collected.
	Pool *bwallet.Pool
	// Journal, when set, has its entries still waiting on a proof stamped
	// mined, and the entries of the Pending items that ask for it.
	Journal *publish.Journal
	// Facade and Topic are where a proven transaction is published again.
	// With no Facade nothing is republished.
	Facade *publish.Facade
	Topic  string
	// Retry, when set, ends the note about a proof that could not be
	// published with how the application sends it again.
	Retry string
	// Save saves the application's state after a proof is recorded; a
	// failure is a note.
	Save func() error
	// Note receives each line Collect reports; nil discards them.
	Note func(format string, args ...any)
}

func (c *Collector) note(format string, args ...any) {
	if c.Note != nil {
		c.Note(format, args...)
	}
}

// Collect asks for the proof of each pending item in order, then of each
// journal entry that was published, has not mined, and is not one of skip,
// then of each parent whose change the pool holds back.
//
// A journal entry for a transaction superseded before its own proof was
// collected is no longer anything the application tracks, yet it mined all
// the same, and left alone it would read as pending for ever; skip names the
// transactions the application does track, whose entries the items handle.
func (c *Collector) Collect(ctx context.Context, items []Pending, skip ...string) {
	for _, it := range items {
		c.collect(ctx, it)
	}
	if c.Journal != nil {
		if entries, err := c.Journal.List(); err == nil {
			for _, en := range entries {
				if en.MinedAt != nil || en.BEEFSentAt == nil || en.EFError != "" || slices.Contains(skip, en.TxID) {
					continue
				}
				if _, height, err := c.Proofs.Of(ctx, en.TxID); err == nil {
					c.stamp(en.Seq, en.TxID, height)
				}
			}
		}
	}
	if c.Pool != nil {
		for _, txid := range c.Pool.UnprovenTxids() {
			mp, height, err := c.Proofs.Of(ctx, txid)
			if err != nil {
				continue
			}
			if n, perr := c.Pool.Prove(txid, mp.Hex(), height); perr != nil {
				c.note("note: could not record the proof for held change from %s: %v", txid, perr)
			} else if n > 0 {
				c.note("change from %s: mined at height %d, %d coin(s) spendable again", txid, height, n)
			}
		}
	}
}

func (c *Collector) collect(ctx context.Context, it Pending) {
	mp, height, err := c.Proofs.Of(ctx, it.Txid)
	switch {
	case err == nil:
		tx, terr := funding.Rebuild(it.RawHex, "", it.BeefHex)
		if terr != nil {
			if it.Unbuilt != nil {
				it.Unbuilt(terr)
			} else {
				c.note("note: %s %s mined, but the kept copy does not rebuild: %v", it.What, it.Txid, terr)
			}
			return
		}
		tx.MerklePath = mp
		// A transaction being built may already hold this one, as an input
		// or a fee input's parent; give that same object the proof, so its
		// BEEF stops at the proof rather than carrying the ancestry.
		c.Kept.Prove(it.Txid, mp)
		if it.Proven != nil {
			it.Proven(mp, height)
		}
		if c.Save != nil {
			if err := c.Save(); err != nil {
				c.note("note: could not save collected proofs: %v", err)
			}
		}
		c.note("%s %s: mined at height %d", it.What, it.Txid, height)
		if it.Stamp {
			c.stamp(it.Seq, it.Txid, height)
		}
		c.republish(ctx, it.What, tx)
	case errors.Is(err, nodeapi.ErrNotMined):
		c.note("%s %s: accepted, proof pending", it.What, it.Txid)
	case errors.Is(err, ErrRefused):
		if it.Refused != nil {
			it.Refused(err)
		} else {
			c.note("WARNING: %s %s was %v", it.What, it.Txid, err)
		}
	default:
		c.note("note: could not check %s %s: %v", it.What, it.Txid, err)
	}
}

// republish submits a transaction that has just proven, so every host
// upgrades the unproven copy it holds. A failure is a note: the next
// collection sends it again.
func (c *Collector) republish(ctx context.Context, what string, tx *transaction.Transaction) {
	if c.Facade == nil {
		return
	}
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		c.note("note: %s %s proven, but its BEEF does not build: %v", what, tx.TxID(), err)
		return
	}
	if _, err := c.Facade.Submit(ctx, c.Topic, b); err != nil {
		if c.Retry != "" {
			c.note("note: %s %s proven, but publishing the proof failed: %v; %s", what, tx.TxID(), err, c.Retry)
		} else {
			c.note("note: %s %s proven, but publishing the proof failed: %v", what, tx.TxID(), err)
		}
		return
	}
	c.note("%s %s: proof published to the hosts", what, tx.TxID())
}

// stamp records in the journal that a transaction mined.
func (c *Collector) stamp(seq uint64, txid string, height uint32) {
	if c.Journal == nil {
		return
	}
	now := time.Now().UTC()
	_ = c.Journal.Update(seq, txid, func(e *publish.Entry) { e.MinedAt, e.Height = &now, height })
}
