package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// What the transactions are built under. None of it belongs to any
// application: the protocol, key ids, tags, payloads and originator exist so
// that the vector pins the mechanism and nothing else.
var (
	protocol   = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"}
	fundingTag = []byte("vx\x02")
	stateTag   = []byte("vx\x01")
	payloads   = [][]byte{[]byte("sample object one"), []byte("sample object two")}
)

const (
	originator  = "vectors.example.com"
	objectKeyID = "object"
	stateKeyID  = "state"

	// A carrier's nLockTime is 2100-01-01T00:00:00Z and its one input's
	// sequence is below the maximum, which together keep it out of any
	// block before then.
	carrierLockTime uint32 = 4102444800
	carrierSequence uint32 = 0

	// The fee policy: one satoshi per byte with a 250 satoshi floor.
	satPerByte uint64 = 1
	floor      uint64 = 250

	coinOutputs        = 5
	coinSats    uint64 = 50000
	treeCount          = 4
	treeSats    uint64 = 1000
	tokenSats   uint64 = 1
	paySats     uint64 = 1000
)

// The heights of the two stand-in blocks: the coin's, and the funding
// tree's.
const (
	coinHeight uint32 = 90
	treeHeight uint32 = 100
)

type coinTx struct {
	// Sats is the value of each of the coin's outputs, all locked to the
	// test key's P2PKH script (payScriptHex).
	Sats    uint64 `json:"sats"`
	Txid    string `json:"txid"`
	TxHex   string `json:"txHex"`
	Height  uint32 `json:"height"`
	BumpHex string `json:"bumpHex"`
	// RootHex is the block's merkle root in display order, the form a
	// header source reports it in.
	RootHex string `json:"rootHex"`
}

type fundingTreeTx struct {
	FeeVout uint32 `json:"feeVout"`
	Count   int    `json:"count"`
	Sats    uint64 `json:"sats"`
	FeeSats uint64 `json:"feeSats"`
	Txid    string `json:"txid"`
	TxHex   string `json:"txHex"`
	// KeptBeefHex is the tree's Atomic BEEF while it was unmined: the tree
	// and its proven parent, the coin.
	KeptBeefHex string `json:"keptBeefHex"`
	Height      uint32 `json:"height"`
	BumpHex     string `json:"bumpHex"`
	RootHex     string `json:"rootHex"`
}

type carrierTx struct {
	// Vout is the funding tree output the carrier spends.
	Vout       uint32 `json:"vout"`
	PayloadHex string `json:"payloadHex"`
	LockHex    string `json:"lockHex"`
	// CommitmentHex is the carrier's txid in hash byte order, SHA-256d of
	// its bytes, the form a token field carries.
	CommitmentHex string `json:"commitmentHex"`
	Txid          string `json:"txid"`
	TxHex         string `json:"txHex"`
}

type transitionTx struct {
	// Carrier is the index in carriers of the carrier whose commitment the
	// token's second field holds.
	Carrier int `json:"carrier"`
	// PrevVout, when present, is the output of the create transition this
	// one spends.
	PrevVout *uint32 `json:"prevVout,omitempty"`
	FeeVout  uint32  `json:"feeVout"`
	Sats     uint64  `json:"sats"`
	LockHex  string  `json:"lockHex"`
	FeeSats  uint64  `json:"feeSats"`
	Txid     string  `json:"txid"`
	TxHex    string  `json:"txHex"`
}

type paymentTx struct {
	FeeVout       uint32 `json:"feeVout"`
	Sats          uint64 `json:"sats"`
	DestScriptHex string `json:"destScriptHex"`
	FeeSats       uint64 `json:"feeSats"`
	Txid          string `json:"txid"`
	TxHex         string `json:"txHex"`
}

type sweepTx struct {
	// Vouts are the funding tree outputs swept, in input order.
	Vouts []uint32 `json:"vouts"`
	// FeeVout, when present, is the coin output that pays the fee; without
	// one the tree pays for its own sweep.
	FeeVout *uint32 `json:"feeVout,omitempty"`
	FeeSats uint64  `json:"feeSats"`
	Txid    string  `json:"txid"`
	TxHex   string  `json:"txHex"`
}

// txVector is a funding tree, two carriers spending it, a state token
// created committing to the first carrier and updated to commit to the
// second, a payment, and two sweeps of the tree, with every input needed to
// rebuild each one. The coin that pays the fees is a stand-in mined parent
// whose one input spends nothing real.
type txVector struct {
	PrivateKeyHex       string        `json:"privateKeyHex"`
	IdentityKeyHex      string        `json:"identityKeyHex"`
	Originator          string        `json:"originator"`
	SecurityLevel       int           `json:"securityLevel"`
	Protocol            string        `json:"protocol"`
	ObjectKeyID         string        `json:"objectKeyId"`
	StateKeyID          string        `json:"stateKeyId"`
	ObjectLockingKeyHex string        `json:"objectLockingKeyHex"`
	StateLockingKeyHex  string        `json:"stateLockingKeyHex"`
	FundingTagHex       string        `json:"fundingTagHex"`
	StateTagHex         string        `json:"stateTagHex"`
	SatPerByte          uint64        `json:"satPerByte"`
	Floor               uint64        `json:"floor"`
	PayScriptHex        string        `json:"payScriptHex"`
	ChangeScriptHex     string        `json:"changeScriptHex"`
	Coin                coinTx        `json:"coin"`
	FundingLockHex      string        `json:"fundingLockHex"`
	FundingTree         fundingTreeTx `json:"fundingTree"`
	Carriers            []carrierTx   `json:"carriers"`
	Create              transitionTx  `json:"create"`
	Update              transitionTx  `json:"update"`
	Payment             paymentTx     `json:"payment"`
	Sweep               sweepTx       `json:"sweep"`
	SweepWithFee        sweepTx       `json:"sweepWithFee"`
}

// unlockTemplate adapts go-sdk's pushdrop.Unlocker, whose Sign takes an int
// and whose EstimateLength takes nothing, to the template a transaction
// input carries.
type unlockTemplate struct{ u *pushdrop.Unlocker }

func (t unlockTemplate) Sign(tx *transaction.Transaction, i uint32) (*script.Script, error) {
	return t.u.Sign(tx, int(i))
}

func (t unlockTemplate) EstimateLength(*transaction.Transaction, uint32) uint32 {
	return t.u.EstimateLength()
}

// roots is a chain tracker that knows the two blocks built here.
type roots map[uint32]chainhash.Hash

var _ chaintracker.ChainTracker = roots(nil)

func (r roots) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	want, ok := r[height]
	return ok && root != nil && want.IsEqual(root), nil
}

func (r roots) CurrentHeight(context.Context) (uint32, error) { return treeHeight + 10, nil }

// builder holds the key and the wallet every transaction is built through.
type builder struct {
	ctx      context.Context
	pd       *pushdrop.PushDrop
	payer    transaction.UnlockingScriptTemplate
	change   *script.Script
	identity *ec.PublicKey
}

func anyone() wallet.Counterparty {
	return wallet.Counterparty{Type: wallet.CounterpartyTypeAnyone}
}

// lock is a lock-before PushDrop of fields to the key's derived key under
// keyID, counterparty Anyone, forSelf true, with the wallet's signature
// over the fields when sign is set. The fields slice is built afresh for
// every call because the template appends the signature to it.
func (b *builder) lock(keyID string, sign bool, fields ...[]byte) (*script.Script, error) {
	fresh := make([][]byte, 0, len(fields))
	for _, f := range fields {
		fresh = append(fresh, bytes.Clone(f))
	}
	return b.pd.Lock(b.ctx, fresh, protocol, keyID, anyone(), true, sign, pushdrop.LockBefore)
}

func (b *builder) unlocker(keyID string) transaction.UnlockingScriptTemplate {
	return unlockTemplate{b.pd.Unlock(b.ctx, protocol, keyID, anyone(), wallet.SignOutputsAll, false)}
}

// readerKey is the reader's side of a derivation: from the anyone root, the
// identity's key under keyID.
func (b *builder) readerKey(keyID string) (*ec.PublicKey, error) {
	return wallet.NewKeyDeriver(nil).DerivePublicKey(protocol, keyID,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: b.identity}, false)
}

// checkLock proves a lock is what a reader expects before it is written:
// locked to the key a reader derives, carrying fields, and, when signed,
// with a signature over the fields concatenated that verifies under it.
func (b *builder) checkLock(s *script.Script, keyID string, signed bool, fields ...[]byte) error {
	want, err := b.readerKey(keyID)
	if err != nil {
		return err
	}
	d := pushdrop.Decode(s)
	if d == nil || d.LockingPublicKey == nil || !d.LockingPublicKey.IsEqual(want) {
		return fmt.Errorf("lock under %q is not to the reader-derived key", keyID)
	}
	n := len(fields)
	if signed {
		n++
	}
	if len(d.Fields) != n {
		return fmt.Errorf("lock under %q has %d fields, want %d", keyID, len(d.Fields), n)
	}
	var all []byte
	for i, f := range fields {
		if !bytes.Equal(d.Fields[i], f) {
			return fmt.Errorf("lock under %q: field %d differs", keyID, i)
		}
		all = append(all, f...)
	}
	if signed {
		sig, err := ec.ParseDERSignature(d.Fields[len(fields)])
		if err != nil {
			return err
		}
		h := sha256.Sum256(all)
		if !sig.Verify(h[:], want) {
			return fmt.Errorf("lock under %q: field signature does not verify", keyID)
		}
	}
	return nil
}

// settle is the fee rule every fee-paying transaction here is built under.
// Start at the floor and sign; accept as soon as the fee covers the signed
// size at the rate, never less than the floor; otherwise rebuild at the
// rate over the signed size plus two bytes per input, since every input's
// signature is made again over a new preimage and its DER length can move.
func settle(build func(fee uint64) (*transaction.Transaction, error)) (*transaction.Transaction, error) {
	atRate := func(size int) uint64 {
		if f := uint64(size) * satPerByte; f > floor {
			return f
		}
		return floor
	}
	fee := floor
	for pass := 0; pass < 6; pass++ {
		tx, err := build(fee)
		if err != nil {
			return nil, err
		}
		if err := tx.Sign(); err != nil {
			return nil, err
		}
		if atRate(tx.Size()) <= fee {
			return tx, nil
		}
		fee = atRate(tx.Size() + 2*len(tx.Inputs))
	}
	return nil, errors.New("fee did not converge")
}

// addChange pays what the inputs leave after the outputs and the fee to the
// change script, unless it is under the floor, when it goes to the fee
// rather than making dust.
func (b *builder) addChange(tx *transaction.Transaction, in, fee uint64) error {
	out := tx.TotalOutputSatoshis()
	if in < out+fee {
		return fmt.Errorf("inputs %d cannot pay outputs %d and fee %d", in, out, fee)
	}
	if rest := in - out - fee; rest >= floor {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: rest, LockingScript: b.change})
	}
	return nil
}

// feePaid is what tx leaves to the fee: its inputs' values less its outputs'.
func feePaid(tx *transaction.Transaction) uint64 {
	var in uint64
	for _, i := range tx.Inputs {
		in += i.SourceTransaction.Outputs[i.SourceTxOutIndex].Satoshis
	}
	return in - tx.TotalOutputSatoshis()
}

// sha256d is SHA-256 twice, how a txid and a merkle node are taken.
func sha256d(b []byte) [32]byte {
	first := sha256.Sum256(b)
	return sha256.Sum256(first[:])
}

// mine puts tx in a two-transaction block at height, beside a stand-in
// transaction whose id is 32 repeated bytes, at offset 0 when first is set
// and 1 otherwise. It returns the block's merkle root, computed here from
// the two ids, and refuses a proof whose root go-sdk computes differently.
func mine(tx *transaction.Transaction, height uint32, other byte, first bool) (chainhash.Hash, error) {
	id := *tx.TxID()
	stand := chainhash.Hash(bytes.Repeat([]byte{other}, 32))
	yes := true
	own := &transaction.PathElement{Hash: &id, Txid: &yes}
	neighbour := &transaction.PathElement{Hash: &stand}
	left, right := own, neighbour
	if !first {
		left, right = neighbour, own
	}
	left.Offset, right.Offset = 0, 1
	tx.MerklePath = transaction.NewMerklePath(height, [][]*transaction.PathElement{{left, right}})
	root := chainhash.Hash(sha256d(append(append([]byte{}, left.Hash[:]...), right.Hash[:]...)))
	computed, err := tx.MerklePath.ComputeRoot(&id)
	if err != nil {
		return root, err
	}
	if !computed.IsEqual(&root) {
		return root, fmt.Errorf("go-sdk computes root %s, want %s", computed, root)
	}
	return root, nil
}

func p2pkhLiteral(b byte) (*script.Script, error) {
	return script.NewFromHex("76a914" + hex.EncodeToString(bytes.Repeat([]byte{b}, 20)) + "88ac")
}

func txFamilies() ([]family, error) {
	v, parts, err := transactions()
	if err != nil {
		return nil, fmt.Errorf("transactions: %w", err)
	}
	u, err := unlockings(v, parts)
	if err != nil {
		return nil, fmt.Errorf("unlocking: %w", err)
	}
	bf, err := beefs(v, parts)
	if err != nil {
		return nil, fmt.Errorf("beef: %w", err)
	}
	k, err := pubkeys(v)
	if err != nil {
		return nil, fmt.Errorf("pubkeys: %w", err)
	}
	pd, err := pushdrops(v)
	if err != nil {
		return nil, fmt.Errorf("pushdrop: %w", err)
	}
	return []family{{"transactions-v1.json", v}, {"unlocking-v1.json", u}, {"beef-v1.json", bf},
		{"pubkeys-v1.json", k}, {"pushdrop-v1.json", pd}}, nil
}

// txParts is what the unlocking family is built from: the transactions
// themselves rather than their hex, and the builder and tracker that made
// and proved them.
type txParts struct {
	b        *builder
	tree     *transaction.Transaction
	carriers []*transaction.Transaction
	tracker  roots
}

func transactions() (*txVector, *txParts, error) {
	ctx := context.Background()
	seed := bytes.Repeat([]byte{0x42}, 32)
	key, _ := ec.PrivateKeyFromBytes(seed)
	w, err := wallet.NewCompletedProtoWallet(key)
	if err != nil {
		return nil, nil, err
	}
	payer, err := p2pkh.Unlock(key, nil)
	if err != nil {
		return nil, nil, err
	}
	// The change and the payment go to literal P2PKH hashes rather than to
	// derived keys, so these bytes move only when the builders do.
	change, err := p2pkhLiteral(0x66)
	if err != nil {
		return nil, nil, err
	}
	dest, err := p2pkhLiteral(0x77)
	if err != nil {
		return nil, nil, err
	}
	b := &builder{ctx: ctx, pd: &pushdrop.PushDrop{Wallet: w, Originator: originator},
		payer: payer, change: change, identity: key.PubKey()}

	objectKey, err := b.readerKey(objectKeyID)
	if err != nil {
		return nil, nil, err
	}
	stateKey, err := b.readerKey(stateKeyID)
	if err != nil {
		return nil, nil, err
	}
	v := &txVector{
		PrivateKeyHex:       hex.EncodeToString(seed),
		IdentityKeyHex:      hex.EncodeToString(key.PubKey().Compressed()),
		Originator:          originator,
		SecurityLevel:       int(protocol.SecurityLevel),
		Protocol:            protocol.Protocol,
		ObjectKeyID:         objectKeyID,
		StateKeyID:          stateKeyID,
		ObjectLockingKeyHex: hex.EncodeToString(objectKey.Compressed()),
		StateLockingKeyHex:  hex.EncodeToString(stateKey.Compressed()),
		FundingTagHex:       hex.EncodeToString(fundingTag),
		StateTagHex:         hex.EncodeToString(stateTag),
		SatPerByte:          satPerByte,
		Floor:               floor,
		ChangeScriptHex:     change.String(),
	}

	// The coin: a mined parent with one input nobody can spend and five
	// outputs to the test key, one for each fee-paying transaction.
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), true)
	if err != nil {
		return nil, nil, err
	}
	pay, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, nil, err
	}
	v.PayScriptHex = pay.String()
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(bytes.Repeat([]byte{0x11}, 32))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, SourceTxOutIndex: 0,
		UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for i := 0; i < coinOutputs; i++ {
		coin.AddOutput(&transaction.TransactionOutput{Satoshis: coinSats, LockingScript: pay})
	}
	coinRoot, err := mine(coin, coinHeight, 0x22, true)
	if err != nil {
		return nil, nil, err
	}
	v.Coin = coinTx{Sats: coinSats, Txid: coin.TxID().String(), TxHex: coin.Hex(),
		Height: coinHeight, BumpHex: coin.MerklePath.Hex(), RootHex: coinRoot.String()}
	tracker := roots{coinHeight: coinRoot}

	// The funding tree: count outputs of the funding lock, the funding tag
	// alone under the object derivation with no signature.
	fundingLock, err := b.lock(objectKeyID, false, fundingTag)
	if err != nil {
		return nil, nil, err
	}
	if err := b.checkLock(fundingLock, objectKeyID, false, fundingTag); err != nil {
		return nil, nil, err
	}
	v.FundingLockHex = fundingLock.String()
	const treeFeeVout = 0
	tree, err := settle(func(fee uint64) (*transaction.Transaction, error) {
		tx := transaction.NewTransaction()
		tx.AddInputFromTx(coin, treeFeeVout, b.payer)
		for i := 0; i < treeCount; i++ {
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: treeSats, LockingScript: fundingLock})
		}
		return tx, b.addChange(tx, coin.Outputs[treeFeeVout].Satoshis, fee)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("funding tree: %w", err)
	}
	kept, err := tree.AtomicBEEF(false)
	if err != nil {
		return nil, nil, err
	}
	treeRoot, err := mine(tree, treeHeight, 0x33, false)
	if err != nil {
		return nil, nil, err
	}
	tracker[treeHeight] = treeRoot
	v.FundingTree = fundingTreeTx{FeeVout: treeFeeVout, Count: treeCount, Sats: treeSats, FeeSats: feePaid(tree),
		Txid: tree.TxID().String(), TxHex: tree.Hex(), KeptBeefHex: hex.EncodeToString(kept),
		Height: treeHeight, BumpHex: tree.MerklePath.Hex(), RootHex: treeRoot.String()}

	// The carriers: each spends one tree output, carries its whole value to
	// the record output, a signed PushDrop of the payload under the object
	// derivation, and is unmineable by its locktime and its input's sequence.
	var carriers []*transaction.Transaction
	for i, payload := range payloads {
		vout := uint32(i)
		record, err := b.lock(objectKeyID, true, payload)
		if err != nil {
			return nil, nil, err
		}
		if err := b.checkLock(record, objectKeyID, true, payload); err != nil {
			return nil, nil, err
		}
		tx := transaction.NewTransaction()
		tx.LockTime = carrierLockTime
		tx.AddInputFromTx(tree, vout, b.unlocker(objectKeyID))
		tx.Inputs[0].SequenceNumber = carrierSequence
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: tree.Outputs[vout].Satoshis, LockingScript: record})
		if err := tx.Sign(); err != nil {
			return nil, nil, err
		}
		c := sha256d(tx.Bytes())
		if c != *tx.TxID() {
			return nil, nil, fmt.Errorf("carrier %d: SHA-256d of the bytes is not go-sdk's txid", i)
		}
		carriers = append(carriers, tx)
		v.Carriers = append(v.Carriers, carrierTx{Vout: vout, PayloadHex: hex.EncodeToString(payload),
			LockHex: record.String(), CommitmentHex: hex.EncodeToString(c[:]), Txid: tx.TxID().String(), TxHex: tx.Hex()})
	}

	// The state token: created committing to carrier 0, then updated to
	// commit to carrier 1 by spending the created token, each paid for by
	// one coin output. The previous token comes first among the inputs and
	// the token is output 0.
	transition := func(carrier int, prev *transaction.Transaction, feeVout uint32) (*transaction.Transaction, *script.Script, error) {
		c := carriers[carrier].TxID()
		lock, err := b.lock(stateKeyID, true, stateTag, c[:])
		if err != nil {
			return nil, nil, err
		}
		if err := b.checkLock(lock, stateKeyID, true, stateTag, c[:]); err != nil {
			return nil, nil, err
		}
		tx, err := settle(func(fee uint64) (*transaction.Transaction, error) {
			tx := transaction.NewTransaction()
			var in uint64
			if prev != nil {
				tx.AddInputFromTx(prev, 0, b.unlocker(stateKeyID))
				in += prev.Outputs[0].Satoshis
			}
			tx.AddInputFromTx(coin, feeVout, b.payer)
			in += coin.Outputs[feeVout].Satoshis
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: tokenSats, LockingScript: lock})
			return tx, b.addChange(tx, in, fee)
		})
		return tx, lock, err
	}
	create, createLock, err := transition(0, nil, 1)
	if err != nil {
		return nil, nil, fmt.Errorf("create: %w", err)
	}
	v.Create = transitionTx{Carrier: 0, FeeVout: 1, Sats: tokenSats, LockHex: createLock.String(),
		FeeSats: feePaid(create), Txid: create.TxID().String(), TxHex: create.Hex()}
	update, updateLock, err := transition(1, create, 2)
	if err != nil {
		return nil, nil, fmt.Errorf("update: %w", err)
	}
	prevVout := uint32(0)
	v.Update = transitionTx{Carrier: 1, PrevVout: &prevVout, FeeVout: 2, Sats: tokenSats, LockHex: updateLock.String(),
		FeeSats: feePaid(update), Txid: update.TxID().String(), TxHex: update.Hex()}

	// A payment: one output to the destination, then change.
	const payFeeVout = 3
	payment, err := settle(func(fee uint64) (*transaction.Transaction, error) {
		tx := transaction.NewTransaction()
		tx.AddInputFromTx(coin, payFeeVout, b.payer)
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: paySats, LockingScript: dest})
		return tx, b.addChange(tx, coin.Outputs[payFeeVout].Satoshis, fee)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("payment: %w", err)
	}
	v.Payment = paymentTx{FeeVout: payFeeVout, Sats: paySats, DestScriptHex: dest.String(),
		FeeSats: feePaid(payment), Txid: payment.TxID().String(), TxHex: payment.Hex()}

	// The sweeps: tree outputs spent with the object derivation's unlocker
	// whether or not a carrier already spent them, and output 0 a tombstone,
	// a funding-lock output holding the swept value. Without a fee input
	// the tombstone carries the swept value less the fee; with one it
	// carries all of it, and the fee input's remainder goes to change.
	sweep := func(vouts []uint32, feeVout *uint32) (*transaction.Transaction, error) {
		return settle(func(fee uint64) (*transaction.Transaction, error) {
			tx := transaction.NewTransaction()
			var swept uint64
			for _, vo := range vouts {
				tx.AddInputFromTx(tree, vo, b.unlocker(objectKeyID))
				swept += tree.Outputs[vo].Satoshis
			}
			if feeVout == nil {
				if swept <= fee {
					return nil, fmt.Errorf("swept %d cannot pay fee %d", swept, fee)
				}
				tx.AddOutput(&transaction.TransactionOutput{Satoshis: swept - fee, LockingScript: fundingLock})
				return tx, nil
			}
			tx.AddInputFromTx(coin, *feeVout, b.payer)
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: swept, LockingScript: fundingLock})
			return tx, b.addChange(tx, swept+coin.Outputs[*feeVout].Satoshis, fee)
		})
	}
	selfPaid, err := sweep([]uint32{2, 3}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sweep: %w", err)
	}
	v.Sweep = sweepTx{Vouts: []uint32{2, 3}, FeeSats: feePaid(selfPaid), Txid: selfPaid.TxID().String(), TxHex: selfPaid.Hex()}
	sweepFeeVout := uint32(4)
	withFee, err := sweep([]uint32{0, 3}, &sweepFeeVout)
	if err != nil {
		return nil, nil, fmt.Errorf("sweep with a fee input: %w", err)
	}
	v.SweepWithFee = sweepTx{Vouts: []uint32{0, 3}, FeeVout: &sweepFeeVout, FeeSats: feePaid(withFee),
		Txid: withFee.TxID().String(), TxHex: withFee.Hex()}

	// Every transaction must be spendable, not only stable: its scripts run
	// and its ancestry proves against the two blocks.
	all := append([]*transaction.Transaction{tree}, carriers...)
	all = append(all, create, update, payment, selfPaid, withFee)
	for _, tx := range all {
		ok, err := spv.Verify(ctx, tx, tracker, nil)
		if err != nil || !ok {
			return nil, nil, fmt.Errorf("%s does not verify: ok=%v err=%v", tx.TxID(), ok, err)
		}
	}
	return v, &txParts{b: b, tree: tree, carriers: carriers, tracker: tracker}, nil
}
