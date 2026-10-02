package producer_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// exampleProfile and exampleParams are the example application's wallet
// profile and carrier parameters, under the protocol and tags reserved for
// tests. A real application registers its own.
var (
	exampleProfile = bwallet.Profile{
		FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
		FundKeyID:      "coin",
		FundBasket:     "vector sample coin",
		Version:        "vector-sample-1",
		LegacyPoolFile: "coins.json",
	}
	exampleParams = carrier.Params{
		Derivation: pushdrop.Derivation{
			Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
			KeyID:    "object",
		},
		FundingTag:      []byte{'v', 'x', 0x02},
		ValidatePayload: func([]byte) error { return nil },
	}
)

// exampleProducer is a producer on a test chain that exists only in this
// process: the fixed test key's signer, and a pool in dir holding one
// 50,000-satoshi coin paying its fund key, with a stand-in proof at height
// 90. Its lines go to lines.
func exampleProducer(dir string, lines *[]string) (*bwallet.Signer, *producer.Payer, error) {
	w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
	if err != nil {
		return nil, nil, err
	}
	signer := &bwallet.Signer{Interface: w, Identity: goldentest.FixedKey().PubKey(), Originator: "example.com", Profile: exampleProfile}
	pool, err := bwallet.LoadPool(filepath.Join(dir, "wallet.json"))
	if err != nil {
		return nil, nil, err
	}
	lock, err := signer.FundScript()
	if err != nil {
		return nil, nil, err
	}
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash(goldentest.Fill(0x11))
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: 50000, LockingScript: lock})
	sibling := chainhash.Hash(goldentest.Fill(0x33))
	isTxid := true
	coin.MerklePath = transaction.NewMerklePath(90, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: coin.TxID(), Txid: &isTxid},
	}})
	if _, err := pool.Add(bwallet.Output{TxID: coin.TxID().String(), Vout: 0, Satoshis: 50000,
		LockingScript: lock.String(), Height: 90, Raw: coin.Hex(), Bump: coin.MerklePath.Hex()}); err != nil {
		return nil, nil, err
	}
	payer := &producer.Payer{
		Pool: pool, Tip: 100, Keys: map[string]*bwallet.Signer{signer.IdentityHex(): signer},
		Kept: &producer.Kept{}, Fees: mint.DefaultFees,
		Note: func(format string, args ...any) { *lines = append(*lines, fmt.Sprintf(format, args...)) },
	}
	return signer, payer, nil
}

// exampleState keeps the example's trees in memory. An application keeps
// them in its own state file.
type exampleState struct {
	cur *funding.Tree
	all []funding.Tree
}

func (s *exampleState) Current() *funding.Tree { return s.cur }

func (s *exampleState) Adopt(t funding.Tree) error {
	s.cur = &t
	s.all = append(s.all, t)
	return nil
}

// A fee input comes from the pool, signed by the key its coin is locked to.
// Change from a transaction published before it mined is held back, so the
// next fee finds no coin, and says why; allowing that parent, which the
// next transaction carries anyway, spends the change against the one kept
// copy of it.
func ExamplePayer_Take() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "producer-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	var lines []string
	signer, payer, err := exampleProducer(dir, &lines)
	if err != nil {
		fmt.Println(err)
		return
	}

	fee, err := payer.Take(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}
	change, err := signer.FundScript()
	if err != nil {
		fmt.Println(err)
		return
	}
	tx, err := mint.Payment(ctx, &script.Script{script.OpTRUE}, 1000, fee, change, mint.DefaultFees)
	if err != nil {
		fmt.Println(err)
		return
	}
	payer.Change(tx, 0, nil) // published, not yet mined
	fmt.Println("held back:", payer.Pool.UnprovenTxids()[0] == tx.TxID().String())

	_, err = payer.Take(ctx)
	var nc *producer.NoCoinError
	fmt.Println(errors.As(err, &nc), nc.Held)
	fmt.Println(err)

	payer.Kept.Load = func(txid string) (*transaction.Transaction, error) { return tx, nil }
	payer.Allow = func() []string { return []string{tx.TxID().String()} }
	next, err := payer.Take(ctx)
	fmt.Println("spent against the kept copy:", next.Tx == tx, err)
	// Output:
	// held back: true
	// true 1
	// fee input: bwallet: no spendable output in the wallet: the other coins are change from 1 transaction(s) whose proofs have not arrived; they become spendable once mined and collected
	// spent against the kept copy: true <nil>
}

// Spend answers the tree the next carriers spend from, minting one when the
// current tree cannot fund them. Six carriers ask more than the four-output
// tree the producer mints by default, so the tree is sized to them. A dry
// run builds it and records, settles and publishes nothing.
func ExampleTrees_Spend() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "producer-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	var lines []string
	signer, payer, err := exampleProducer(dir, &lines)
	if err != nil {
		fmt.Println(err)
		return
	}
	state := &exampleState{}
	trees := &producer.Trees{
		Payer: payer, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
		Lock: func(ctx context.Context) (*script.Script, error) {
			return carrier.FundingLock(ctx, signer, signer.Originator, exampleParams)
		},
		Change: signer.FundScript,
		DryRun: true,
	}

	tree, first, err := trees.Spend(ctx, 6)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, l := range lines {
		fmt.Println(strings.ReplaceAll(l, tree.TxID().String(), "<tree>"))
	}
	funded := 0
	for _, out := range tree.Outputs {
		if _, ok := carrier.DecodeFunding(out.LockingScript, exampleParams.FundingTag); ok {
			funded++
		}
	}
	fmt.Println("funding outputs:", funded, "first:", first, "recorded:", state.Current() != nil)
	payer.GiveBack()
	fmt.Println("coins in the pool after GiveBack:", payer.Pool.Count())
	// Output:
	// this transition spends 6 outputs, so the tree is minted with 6 rather than 4
	// funding tree <tree>: 6 output(s) of 1000 sat
	// funding outputs: 6 first: 0 recorded: false
	// coins in the pool after GiveBack: 1
}

// With Ahead set, a spend that leaves the current tree with Ahead outputs
// or fewer mints the next tree in the background, and the spend the current
// tree cannot cover switches to it with no wait for a block. The tree
// minted ahead is adopted and published only at the switch. The example
// settles on a test chain in this process that mines what it is given.
func ExampleTrees_Wait() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "producer-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	var lines []string
	signer, payer, err := exampleProducer(dir, &lines)
	if err != nil {
		fmt.Println(err)
		return
	}
	chain := &testChain{known: map[string]*transaction.Transaction{}, accepted: map[string]bool{},
		mined: map[string]uint32{}, refused: map[string]string{}, height: 700, mineOnSubmit: true}
	chain.srv = httptest.NewServer(http.HandlerFunc(chain.serve))
	defer chain.srv.Close()
	payer.Settler, payer.Asset, payer.Poll = chain.arcade(), chain.asset(), 10*time.Millisecond
	payer.Kept.Load = func(txid string) (*transaction.Transaction, error) {
		chain.mu.Lock()
		defer chain.mu.Unlock()
		return chain.known[txid], nil
	}
	state := &exampleState{}
	trees := &producer.Trees{
		Payer: payer, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
		Lock: func(ctx context.Context) (*script.Script, error) {
			return carrier.FundingLock(ctx, signer, signer.Originator, exampleParams)
		},
		Change: signer.FundScript,
		Facade: chain.facade(), Topic: "tm_vector_sample",
		Ahead: 2,
	}

	names := map[string]string{}
	name := func(txid string) string {
		if names[txid] == "" {
			names[txid] = fmt.Sprintf("<tree %d>", len(names)+1)
		}
		return names[txid]
	}
	spend := func(need uint32) {
		tree, first, err := trees.Spend(ctx, need)
		if err != nil {
			fmt.Println(err)
			return
		}
		state.cur.Next += need // the application's half: the outputs are spent
		fmt.Printf("spend %d: %s from output %d\n", need, name(tree.TxID().String()), first)
	}
	spend(2) // a tree is minted; 2 outputs are left, so the next is minted ahead
	if err := trees.Wait(ctx); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("minted ahead:", name(trees.Prepared().Txid), "trees adopted:", len(state.all))
	spend(2) // the last two
	spend(1) // the switch
	fmt.Println("trees adopted:", len(state.all))
	hex := regexp.MustCompile(`[0-9a-f]{64}`)
	for _, l := range lines {
		if !strings.Contains(l, "settling via") {
			fmt.Println(hex.ReplaceAllStringFunc(l, name))
		}
	}
	// Output:
	// spend 2: <tree 1> from output 0
	// minted ahead: <tree 2> trees adopted: 1
	// spend 2: <tree 1> from output 2
	// spend 1: <tree 2> from output 0
	// trees adopted: 2
	// funding tree <tree 1>: 4 output(s) of 1000 sat
	// funding tree <tree 1>: mined at height 701
	// funding tree published: admitted 1 output(s)
	// funding tree <tree 1> has 2 output(s) left, so the next is minted ahead
	// funding tree <tree 2>: 4 output(s) of 1000 sat
	// funding tree <tree 2>: mined at height 702
	// funding tree <tree 2> is minted ahead and waits for the switch
	// switching to funding tree <tree 2>, minted ahead
	// funding tree published: admitted 1 output(s)
}

// exampleRecords is exampleState with the records Prepare hands it: each
// tree signed for a coin, kept with that coin until the tree is adopted. An
// application keeps them in its state file, saved before Prepare returns.
type exampleRecords struct {
	exampleState
	prepared map[string]examplePrepared
}

type examplePrepared struct {
	Tree funding.Tree
	Coin bwallet.Output
}

func (s *exampleRecords) Prepare(tree funding.Tree, coin bwallet.Output) error {
	s.prepared[tree.Txid] = examplePrepared{tree, coin}
	return nil
}

// Adopt drops the record of the tree it adopts, in the same save.
func (s *exampleRecords) Adopt(t funding.Tree) error {
	delete(s.prepared, t.Txid)
	return s.exampleState.Adopt(t)
}

// stoppingLeg ends the run once the leg has taken a transaction.
type stoppingLeg struct {
	publish.Settler
	stop func()
}

func (l stoppingLeg) Submit(ctx context.Context, tx *transaction.Transaction) error {
	err := l.Settler.Submit(ctx, tx)
	l.stop()
	return err
}

// Prepare hands the application each tree's record and fee coin before the
// tree reaches the settlement leg, and Recover settles a record a run left
// behind. Here the run stops while its tree waits for a block: the coin is
// spent and the state knows no tree. The block arrives, and the next start
// recovers the tree: it is adopted and published, and its change is in the
// pool.
func ExampleTrees_Recover() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "producer-example-")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	var lines []string
	signer, payer, err := exampleProducer(dir, &lines)
	if err != nil {
		fmt.Println(err)
		return
	}
	chain := &testChain{known: map[string]*transaction.Transaction{}, accepted: map[string]bool{},
		mined: map[string]uint32{}, refused: map[string]string{}, height: 700}
	chain.srv = httptest.NewServer(http.HandlerFunc(chain.serve))
	defer chain.srv.Close()
	run, stop := context.WithCancel(ctx)
	defer stop()
	payer.Settler, payer.Asset, payer.Poll = stoppingLeg{chain.arcade(), stop}, chain.asset(), 10*time.Millisecond
	state := &exampleRecords{prepared: map[string]examplePrepared{}}
	treesOver := func(p *producer.Payer) *producer.Trees {
		return &producer.Trees{
			Payer: p, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
			Lock: func(ctx context.Context) (*script.Script, error) {
				return carrier.FundingLock(ctx, signer, signer.Originator, exampleParams)
			},
			Change: signer.FundScript,
			Facade: chain.facade(), Topic: "tm_vector_sample",
			Prepare: state.Prepare,
		}
	}

	_, _, err = treesOver(payer).Spend(run, 1)
	fmt.Println("the run stopped:", errors.Is(err, context.Canceled))
	fmt.Println("trees adopted:", len(state.all), "records kept:", len(state.prepared), "coins in the pool:", payer.Pool.Count())

	// The block arrives while the application is down.
	for txid := range state.prepared {
		chain.mine(txid)
	}

	// The next start: the pool as it is on disk, and the records the state
	// kept. Every record is recovered before the first Spend.
	pool, err := bwallet.LoadPool(payer.Pool.Path())
	if err != nil {
		fmt.Println(err)
		return
	}
	next := treesOver(&producer.Payer{
		Pool: pool, Tip: 100, Keys: payer.Keys, Kept: &producer.Kept{}, Fees: mint.DefaultFees,
		Settler: chain.arcade(), Asset: chain.asset(), Poll: 10 * time.Millisecond, Note: payer.Note,
	})
	for txid, rec := range state.prepared {
		got, err := next.Recover(ctx, rec.Tree, rec.Coin)
		if errors.Is(err, producer.ErrPublish) {
			// The tree is adopted and its record dropped: only the publish
			// is repeated, now or, with got.Tree noted, on a later start.
			err = next.Publish(ctx, got.Tree)
		}
		if err != nil {
			fmt.Println(err) // undecided: the record stays for the next start
			continue
		}
		fmt.Println("recovered:", got.Outcome)
		if got.Outcome == producer.CoinSpent {
			delete(state.prepared, txid) // no tree: the application drops the record
		}
		// A CoinReturned record is kept for the next start: the tree may
		// still land.
	}
	fmt.Println("trees adopted:", len(state.all), "records kept:", len(state.prepared), "coins in the pool:", pool.Count())
	hex := regexp.MustCompile(`[0-9a-f]{64}`)
	for _, l := range lines {
		if !strings.Contains(l, "settling via") {
			fmt.Println(hex.ReplaceAllString(l, "<tree>"))
		}
	}
	// Output:
	// the run stopped: true
	// trees adopted: 0 records kept: 1 coins in the pool: 0
	// recovered: tree adopted
	// trees adopted: 1 records kept: 0 coins in the pool: 1
	// funding tree <tree>: 4 output(s) of 1000 sat
	// funding tree <tree>: mined at height 701
	// funding tree <tree> is recovered
	// funding tree published: admitted 1 output(s)
}

// Collect asks for the proof of each transaction still waiting for one. With
// no proof source configured nothing has mined, which is the ordinary
// answer between a publish and its block, so the item is reported pending
// and nothing is recorded.
func ExampleCollector_Collect() {
	var lines []string
	c := &producer.Collector{
		Proofs: producer.Proofs{}, // an arcade installation and a node in an application
		Note:   func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	}
	txid := strings.Repeat("ab", 32)
	c.Collect(context.Background(), []producer.Pending{{
		What: "anchor", Txid: txid,
		Proven:  func(mp *transaction.MerklePath, height uint32) { fmt.Println("recorded at", height) },
		Refused: func(err error) { fmt.Println("tell the operator:", err) },
	}})
	for _, l := range lines {
		fmt.Println(strings.ReplaceAll(l, txid, "<anchor>"))
	}
	// Output:
	// anchor <anchor>: accepted, proof pending
}
