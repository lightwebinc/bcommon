# Examples

How to do the common things with bcommon. The first section is what an
application does on mainnet or testnet before anything else: get coin from
its user into the wallet. After it, every Go example on this
page is an `Example` function in a package's `example_test.go`, with an
`// Output:` block, so `go test` compiles and runs it and fails if its output
changes. The excerpts below are taken from those files; follow the link
under each heading for the complete, runnable version.

Those examples touch no network. Where one needs a chain, it builds a
**test chain** in the process: a stand-in mined coin with a stand-in proof,
and a chain tracker (`goldentest.Tracker`) that knows that one block's merkle
root and nothing else. SPV then runs exactly as it would against a reader's
own header source.

The examples use the fixed test key (`goldentest.FixedKey`, 32 bytes of
0x42) and the derivation and tags reserved for tests (`[1, "vector
sample"]`, prefix `vx`). Never use either for anything real; an application
brings its own key and registers its own identifiers in
[registry.md](registry.md).

## Running them

```bash
GOWORK=off go test -run Example -v ./...
```

pkg.go.dev shows each example beside the function it documents.

## Fund a wallet on mainnet or testnet

Everything an application publishes is paid for from coin its user owns.
Users have no coinbase outputs, so on mainnet and testnet the coin arrives
one of two ways:

1. **A payment to the wallet's fund address.** The application prints the
   address, the user sends a small amount to it from their own wallet, and
   the application imports that payment by its txid once it is mined. This
   is the path for an application with an embedded wallet (`bwallet`).
2. **A BRC-100 wallet the user already runs.** Reach it with
   `wirewallet.Dial` (loopback only; see
   [configuration.md](configuration.md#wallet-wire-wirewallet)), sign
   through it as a `bwallet.Signer`, and let it pay for each funding tree
   through `producer.Trees.Fund` (a function the application writes that
   has the wallet build, fund and broadcast the tree), so trees are paid
   from the user's wallet rather than from the pool.

`bwallet.FundFromCoinbase` and `bwallet.Rescan` are neither. They mine
coinbase to the fund address, which works only on a regtest chain you run
(development and tests); see
[configuration.md](configuration.md#coinbase-funding-regtest-only).

### Which networks

| Network | Header source (`headers.New`) | Address prefix | Broadcast (`publish.Arcade.Base`) |
|---|---|---|---|
| mainnet | `woc:main` | `bwallet.Embedded.Mainnet = true` | an ARC service, for example `https://arc.gorillapool.io/v1` |
| testnet | `woc:test` | `Mainnet = false` | an ARC testnet service, for example `https://arc-test.taal.com/v1` (many public services require a `Key`) |
| a regtest chain you run | your own bridge or chaintracks | `Mainnet = false` | your node (`publish.RPCSettler`) or its ARC |

A header source is the root of trust for every proof checked against it.
WhatsOnChain headers are not taken at their word: each one is hashed and
must carry the proof of work its bits claim, and on mainnet at least
`headers.MainnetMinDifficulty`. Running your own chaintracks or node is
stronger still.

### Step 1: create the wallet and show the fund address

```go
// The application's own profile; register its names in registry.md.
// FundProtocol and FundKeyID are frozen once coin is paid to the key.
var profile = bwallet.Profile{
	FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "example app fund"},
	FundKeyID:      "1",
	FundBasket:     "example app",
	Version:        "example-app-0.1.0",
	LegacyPoolFile: "example-app-pool.json",
}

w, err := bwallet.Create(dir, profile) // bwallet.Open(dir, profile) on every later run
w.Mainnet = true                        // false for testnet
addr, err := w.FundAddress(w.Mainnet)
fmt.Println("send a small amount to", addr, "from your own wallet, then import its txid")
```

`dir` holds `identity.json`, the root key, at mode 0600. Back it up: the
coin in the pool is spendable only with that key.

### Step 2: import the user's payment

Once the payment has a block, fetch it with its proof, check the proof
against your header source, and add the outputs that pay the fund address
to the pool. The answer is not trusted: the guard bounds it before go-sdk
parses it, and the txid and the proof are checked here.

```go
func importPayment(ctx context.Context, w *bwallet.Embedded, network, txid string) (int, error) {
	tracker := headers.New("woc:" + network) // "main" or "test"

	// WhatsOnChain serves a mined transaction with its proof as BEEF.
	url := "https://api.whatsonchain.com/v1/bsv/" + network + "/tx/" + txid + "/beef"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: status %d; import it once it is mined", txid, resp.StatusCode)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return 0, err
	}
	_, tx, _, err := guard.ParseBEEF(b, guard.DefaultBound)
	if err != nil {
		return 0, err
	}
	if tx == nil || tx.TxID().String() != txid || tx.MerklePath == nil {
		return 0, fmt.Errorf("%s: not the mined transaction asked for", txid)
	}
	if ok, err := tx.MerklePath.Verify(ctx, tx.TxID(), tracker); err != nil || !ok {
		return 0, fmt.Errorf("%s: proof does not hold against the header source: %v", txid, err)
	}

	fund, err := w.FundScript()
	if err != nil {
		return 0, err
	}
	var outs []bwallet.Output
	for i, o := range tx.Outputs {
		if o.LockingScript == nil || !bytes.Equal(*o.LockingScript, *fund) {
			continue
		}
		outs = append(outs, bwallet.Output{
			TxID: txid, Vout: uint32(i), Satoshis: o.Satoshis,
			LockingScript: o.LockingScript.String(), Height: tx.MerklePath.BlockHeight,
			Raw: tx.Hex(), Bump: funding.BumpHex(tx.MerklePath),
		})
	}
	if len(outs) == 0 {
		return 0, fmt.Errorf("%s pays nothing to the fund address", txid)
	}
	return w.Pool.Add(outs...) // an output already held is not added twice
}
```

An application that runs a Teranode node can read the payment from it
instead: `nodeapi.Asset.TxRaw`, `guard.ParseTransaction` and
`nodeapi.Asset.Proof`, then the same proof check and `Pool.Add`.

Run against the public services with a fresh wallet, and given a mined
mainnet transaction that pays someone else, step 1 and step 2 print:

```text
send a small amount to 12nPSaLaEDhmeCrjJ8g9TrrkY7kpxg2Bi8 from your own wallet, then import its txid
1c80205fca5da7894d410d0173951807984ab22beba4e0c27f28ca4778739e10 pays nothing to the fund address
```

The proof was fetched and held against the WhatsOnChain headers before the
outputs were read; the user's own payment to the printed address adds its
outputs instead. On testnet (`Mainnet = false`, `woc:test`) the address
starts with `m` or `n`. Each new wallet has a new random key, so the
address differs every time.

### Step 3: spend from the pool

The pool now holds a mined, proven coin that is spendable at once (only
coinbase waits for maturity). A `producer.Payer` takes fee inputs from it
and `producer.Trees` mints funding trees from it, settling through
`publish.Arcade`:

```go
settle := &publish.Arcade{Base: "https://arc.gorillapool.io/v1"} // plus Key where the service needs one
if err := settle.Ping(ctx); err != nil {                        // reads /policy; spends nothing
	return err
}
```

The sections below ([Pay a fee from the pool](#pay-a-fee-from-the-pool-as-a-producer),
[Spend from a funding tree](#spend-from-a-funding-tree-minting-the-next))
show the Payer and Trees with every field.

### What it costs

`mint.DefaultFees` pays 1 satoshi per byte with a 250 satoshi floor, which
is above the 100 satoshis per 1000 bytes that BSV mainnet miners publish
(an ARC service answers its own policy at `/v1/policy`). Measured on the
examples below: a funding tree of four outputs is 388 bytes and pays 390
satoshis, a simple payment pays the 250 satoshi floor, and a carrier is
never mined and pays no fee; it spends one funding output (1000 satoshis
in the examples, `producer.Trees.Sats` in an application). A deposit of a
few tens of thousands of satoshis covers many operations. Send only what
the application needs: the key is a hot key on the machine it runs on.

## Derive a PushDrop lock

[`pushdrop/example_test.go`](../pushdrop/example_test.go),
`ExampleDerivation_Lock`

A producer locks a signed, tagged PushDrop through its wallet. A reader that
holds only the producer's identity key recomputes the locking key, decodes
the output and checks the field signature, without asking the producer
anything.

```go
exampleDerivation = pushdrop.Derivation{
	Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
	KeyID:    "object",
}
exampleTag = []byte{'v', 'x', 0x01}

// Producer: lock [tag, "hello"] and append the wallet's signature.
w, err := wallet.NewCompletedProtoWallet(goldentest.FixedKey())
lock, err := exampleDerivation.Lock(ctx, w, "example.com", [][]byte{exampleTag, []byte("hello")}, true)

// Reader: from the identity key alone.
want, err := exampleDerivation.ExpectedLockingKey(identity)
out, err := pushdrop.DecodeTagged(lock, exampleTag, 2)
want.IsEqual(out.LockingKey) // true
out.VerifySignature()        // true
```

The locking key it prints,
`0324d6d6ee75173da1ca8d964d3889792c8911d66c3290a76c367413f5a68e5446`, is the
`objectLockingKeyHex` the independent generator wrote to
`testdata/vectors/transactions-v1.json` for the same key and derivation.

`ExampleDerivation_Validate` shows the BRC-43 rules refusing a four-letter
protocol name, an underscore and an empty key id when the derivation is
built rather than at the first mint.

In an application the wallet is a real BRC-100 wallet: `bwallet.Embedded`,
or one reached with `wirewallet.Dial`. The SDK's `CompletedProtoWallet`
stands in here because `Lock` needs only its key calls.

## Build a funding tree on a test chain

[`mint/example_test.go`](../mint/example_test.go), `ExampleFundingTree`

A funding tree is a mined transaction of equal-valued outputs set aside for
carriers, each locked with the application's funding lock. The fee input is
the test coin; the fee loop signs, measures and rebuilds until the fee covers
the size.

```go
lock, err := carrier.FundingLock(ctx, w, "example.com", params)
fee := mint.Input{Tx: coin, Vout: 0, Unlocker: payer}
tree, err := mint.FundingTree(lock, 4, 1000, fee, change, mint.DefaultFees)

verdict, err := verify.Check(ctx, tree, tracker) // verify.Passed, through the proven coin

beef, err := funding.KeepBEEF(tree, nil) // kept until the tree mines
state := funding.Tree{Txid: tree.TxID().String(), RawHex: tree.Hex(), BeefHex: beef, Sats: 1000, Count: 4}
state.Remaining() // 4
```

Output:

```text
output 0: 1000 sats, funding=true
output 1: 1000 sats, funding=true
output 2: 1000 sats, funding=true
output 3: 1000 sats, funding=true
output 4: 45750 sats, funding=false
fee: 250 sats for 388 bytes
proves through its parent: true <nil>
carriers it can fund: 4
```

At `mint.DefaultFees`, 100 satoshis per 1000 bytes, a 388-byte tree
needs 39 satoshis and pays the 250 satoshi floor. The fee loop targets the
signed size plus two bytes per input, because a DER signature can grow by a
byte when the transaction is re-signed. On mainnet or testnet the coin is the user's own payment,
imported as in [Fund a wallet on mainnet or testnet](#fund-a-wallet-on-mainnet-or-testnet).

## Mint a carrier

[`carrier/example_test.go`](../carrier/example_test.go), `ExampleMint`

A carrier spends one funding output and carries the payload in one signed
PushDrop output. Its far-future locktime and non-final input keep it off the
chain, and its txid, in hash byte order, is the commitment a mined token
carries.

```go
p := carrier.Params{
	Derivation: pushdrop.Derivation{
		Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "vector sample"},
		KeyID:    "object",
	},
	FundingTag: []byte{'v', 'x', 0x02},
	ValidatePayload: func(p []byte) error { ... }, // the payload's own rules
}
tx, err := carrier.Mint(ctx, w, "example.com", p, payload, tree, 0)

c, err := carrier.Decode(tx, exampleClassify)
err = c.Validate(p, identity)          // nil
commitment := carrier.Commitment(tx)   // the txid, hash byte order
verdict, err := verify.Check(ctx, tx, tracker)
```

Output:

```text
locktime: 4102444800 sequence: 0
output 0: 1000 sats, fee: 0
payload "vxr\x01an object" validates: <nil>
commitment is the txid: true
proves through its funding tree: true <nil>
another identity: true
```

The last line is `Validate` against somebody else's identity key, refused as
`carrier.ErrLock`.

## Verify a carrier from a BEEF, with the parse guard

[`verify/example_test.go`](../verify/example_test.go),
`ExampleVerifyCarrier` and `ExampleCheck`

A reader asked a host for the carrier a token commits to, and the host
answered one output as a BEEF. `VerifyCarrier` parses the BEEF, checks the
commitment before anything the carrier says about itself, validates the
carrier, applies the caller's expectations, and only then proves the funding
parent in the reader's own header source.

```go
beef, err := tx.BEEF()
answer := []verify.Item{{Beef: beef, OutputIndex: 0}}
c, code, reason, steps := verify.VerifyCarrier(ctx, answer, want, "record", spec, tracker)
```

The example then hands it the answers a hostile or broken host could give:

| Answer | Code |
|---|---|
| The carrier asked for | `VERIFIED` |
| A different carrier than the one asked for | `REFUSED-COMMIT` |
| The BEEF cut in half | `REFUSED-DECODE` |
| 13 bytes that declare 2^63 proofs | `REFUSED-DECODE` |
| A header source without the funding parent's block | `REFUSED-BUMP` |
| No output | `NO-TOKEN` |
| Two outputs where one is allowed | `REFUSED-FORK` |

The hostile BEEF is refused by the guard before go-sdk sees it, `guard:
BEEF refused: declares 9223372036854775808 BUMPs, 0 bytes remain`:
`guard.ParseBEEF` walks every BEEF the library parses and bounds every count
it declares by the bytes present, and no allocation is attempted. `ERROR` is kept apart from every refusal: it means
the reader could not decide, typically because the header source did not
answer, so an outage is never read as a forgery. `ExampleCheck` shows
`verify.Check` refusing a nil tracker rather than dialing a default.

## Guard a proof before the SDK parses it

[`guard/example_test.go`](../guard/example_test.go), `ExampleParseBUMP`, `ExampleParseBEEF`, `ExampleParsePubKey`;
[`nodeapi/example_test.go`](../nodeapi/example_test.go), `ExampleProofFor`

A BRC-74 BUMP from a service is walked by `guard.ParseBUMP`, allocating
nothing, before the SDK is allowed to size a slice for it.

```go
mp, err := guard.ParseBUMP(good, 1<<20)

hostile := []byte{0x5a, 0x01, 0xfe, 0xff, 0xff, 0xff, 0xff}
_, err = guard.ParseBUMP(hostile, 1<<20)
```

Output:

```text
good: 71 bytes, height 90 <nil>
hostile: bump level 0 declares 4294967295 leaves, 0 bytes remain
trailing: bump has 1 trailing bytes
over the bound: bump is 71 bytes, max 16
```

`guard.ParseBEEF` and `guard.ParsePubKey` have examples beside it: a
13-byte BEEF declaring 2^63 BUMPs and a transaction declaring four billion
inputs are refused, and so is `02 || p+1`, a second encoding of the key with
x = 1 that go-sdk reads.

`nodeapi.ProofFor` runs the guard and then refuses a proof that does not
name the transaction it was asked for (`proof does not contain the txid`),
because a valid proof of some other transaction verifies perfectly and
proves nothing.

## Compute RFC 6962 roots and an inclusion proof

[`commit/example_test.go`](../commit/example_test.go), `ExampleRoot` and
`ExampleProve`

```go
// Leaf i is SHA-256 of the single byte i: the first leaves of
// testdata/vectors/rfc6962-v1.json.
root := commit.Root(leaves(5))
path, err := commit.Prove(leaves(5), 2)
commit.Verify(leaves(5)[2], path, root) // true
commit.Verify(leaves(5)[3], path, root) // false: a path proves only its own leaf
```

Output:

```text
6b313b611b40676b9e1dfd70c4503f2379f88f0f1c2740fb7e1cacc32c113465
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

The first root is the one the independent generator wrote for the same five
leaves; the second is the empty tree's root, SHA-256 of the empty string.
`ExampleProve` prints the path (three siblings, the second on the left) and
`commit: index out of range` for an index past the last leaf.

## Commit a record to a store

[`store/example_test.go`](../store/example_test.go), `ExampleRoot` and
`ExampleRoot_oneMember`

A store of several members has a manifest; its root is over the members'
commitments in manifest order, and its head, the commitment a reader asks a
host for, is the manifest's. A store of one member is its own head, and its
root is that member's leaf hash.

```go
body, err := m.Body(64 << 10)            // the application's body bound
ref := store.Ref{Name: "docs", Count: uint64(len(m.Members)), Head: &head}
ref.Root = store.Root(ref, m.Leaves())
refs, err := store.EncodeRefs([]store.Ref{ref}) // the array value a record carries
back, err := store.DecodeRefs(refs)
h, err := store.Head(back[0])
```

## Encode and decode canonical CBOR

[`cbor/example_test.go`](../cbor/example_test.go), `ExampleEncode` and
`ExampleDecodeValue_refusals`

`cbor.Encode` sorts map keys into canonical order whatever order the `Map`
lists them in, and writes every head in its shortest form.

```go
record := cbor.Map{
	{Key: "name", Val: "example"},
	{Key: uint64(2), Val: []byte{0xca, 0xfe}},
	{Key: uint64(1), Val: int64(-1)},
	{Key: "tags", Val: []cbor.Value{"a", true, nil}},
}
b, err := cbor.Encode(record)
// a401200242cafe646e616d65676578616d706c656474616773836161f5f6
```

The decoder refuses everything the encoder would not have written, so a
value has exactly one encoding a reader accepts:

```text
23 in two bytes: cbor: not canonical
map keys out of order: cbor: map keys out of order
duplicate map key: cbor: duplicate map key
indefinite-length array: cbor: unsupported item
a float: cbor: unsupported item
trailing bytes: cbor: trailing bytes
truncated string: cbor: truncated
```

## Read an application record

[`record/example_test.go`](../record/example_test.go), `ExampleDecode` and
`ExampleReason`

An application's record is a canonical CBOR map with unsigned-integer keys,
key 0 its magic. `record.Decode` holds the bytes to the steps every record
shares, and the application then reads its own keys in its own order. A key
above the last one this version defines is preserved, so a reader of this
version writes back what a later version added.

```go
f, err := record.Decode(b, sampleMax, sampleLast, sampleMagic)
name, err := f.Text(1)
count, err := f.Uint(2, 1, 100)
again, err := record.Encode(cbor.Map{
	{Key: uint64(0), Val: sampleMagic},
	{Key: uint64(1), Val: name},
	{Key: uint64(2), Val: count},
}, f.Extra, sampleMax) // the same bytes
```

A record is refused for the first rule it breaks, and `record.Reason` gives
each refusal the fixed label a host counts it by:

```text
over the bound, whatever it is: too-large
not CBOR: cbor
an array: cbor
a text key: key-type
another magic: magic
key 1 missing: missing
key 1 a number: type
key 2 out of range: range
```

## Admit a mined token from its BEEF

[`chaintoken/example_test.go`](../chaintoken/example_test.go),
`ExampleWire_Token`

A chain of two tokens on a test chain, and what a host does with the
second. A replayer, or the publisher, assembles the one BEEF a token is
admitted in from the token and its parent. The host reads the BEEF as
declared, holds it to the token's shape, finds the parent in it, and reads
the predecessor from the parent, never from what it holds.

```go
beef, err := chaintoken.TokenBEEF(second, first)

wire, err := chaintoken.ReadWire(beef, chaintoken.DefaultMaxBEEF)
tx := wire.SubjectTx()
parent, err := wire.Token(tx) // exactly the token and its parent, each proven minimally
mined := chaintoken.Mined(ctx, tx, tracker) && chaintoken.Mined(ctx, parent.Tx, tracker)

out, ok := chaintoken.ReadOutput(tx.Outputs[0], 0, 2) // two fields, a signature, 1 satoshi
locked := out.LockedTo(*tx.Outputs[0].LockingScript, lockingKey)
signed := out.SignedBy(lockingKey)

for _, s := range chaintoken.Spends(tx, parent) {
	pred, isToken := chaintoken.ReadOutput(s.Output, s.Vout, 2)
	// ...
}
```

```text
a token and its parent: true parent is the first token: true
both mined: true
two fields and a signature of 1 satoshi: true
record "state 1", canonical lock true, signed true
input 0 spends output 0: the predecessor, record "state 0"
input 1 spends output 1: not a token
alone: true
stored: true <nil>
```

The last two lines are the second token alone, as a host stores it: it is
not admissible without its parent (`ErrBEEF`), and a reader reads it with
`chaintoken.Stored`. The record, the tag, the derivation and the rules a
token's record must keep against its predecessor are the application's.

## Commit to a key and wrap it

[`keyed/example_test.go`](../keyed/example_test.go), `ExampleCheckOpened`

A publisher draws a key (`keyed.SampleKey`), publishes its commitment, and
wraps the key to each holder in BRC-2's symmetric form. A holder that opens
a wrap checks the key against the commitment before anything uses it, so a
wrong key names whoever wrapped it and not whoever encrypted under it.

```go
commitment := keyed.Commitment(k)
wrap, err := keyed.SymmetricSeal(shared[:], iv, k[:]) // 80 bytes
opened, err := keyed.SymmetricOpen(shared[:], wrap)
got, err := keyed.CheckOpened(opened, commitment) // 32 bytes, a scalar, the committed one
```

A wallet's `Encrypt` writes the same form with an IV it draws itself, and
its `Decrypt` opens what `SymmetricSeal` wrote under the key it derives.

## Seal a value for the members of a group epoch

[`keyed/example_test.go`](../keyed/example_test.go), `ExampleWrapToEpoch`

A fresh content key seals the value as one BRC-369 segment, and the key is
wrapped under a key derived from the epoch's symmetric key, the content id
and the application's registered domain string. A member opens the wrap
with the key of the epoch the record names, checks the key against the
commitment, and only then opens the segment. Another epoch's key, or
another application's domain string, derives another wrapping key, and the
wrap does not open.

```go
ct, err := keyed.SealSegment(k, salt, plaintext)                         // len(plaintext) + 16 bytes
wrap, err := keyed.WrapToEpoch(domain, epochSym, contentID, k, nil)      // 80 bytes
got, err := keyed.UnwrapFromEpoch(domain, epochSym, contentID, wrap, keyed.Commitment(k))
pt, err := keyed.OpenSegment(got, salt, ct)                               // ErrTag if the encrypter lied
```

## Run a chain inside a test

[`testchain/example_test.go`](../testchain/example_test.go), `ExampleChain`

`testchain.Chain` mines what it is sent, each transaction in a block of its
own, and refuses what a node refuses. It is a chain tracker, and an
`http.Handler` that serves a node's RPC and asset API, a broadcaster and a
header source, so the clients are tested against something they share no
code with.

```go
c := testchain.New(700)
c.Generate(testchain.Maturity+1, addr.AddressString)
err := c.Send(tx)                          // mined in a block of its own
mp, height, mined := c.Proof(tx.TxID().String())
ok, err := mp.Verify(ctx, tx.TxID(), c)    // the chain as a tracker
srv := httptest.NewServer(c)               // and as a node for the clients
```

```text
tip: 801
young coinbase: input 0 spends immature coinbase
mature coinbase: <nil>
mined: true at 802 proof verifies: true <nil>
the same output spent by another transaction: true
```

It is for tests and local trials only; there is no proof of work and no
real block.

## Pay a fee from the pool, as a producer

[`producer/example_test.go`](../producer/example_test.go), `ExamplePayer_Take`

A `producer.Payer` takes a fee input from the application's coin pool,
signed by whichever of its keys the coin is locked to, and takes change back
into the pool. Change from a transaction published before it mined is held
back until its proof arrives, so a second fee finds no coin and says why, as
a `*producer.NoCoinError` the application can word for its own users.
Allowing that parent, because the next transaction carries it anyway, spends
the change against the one copy `producer.Kept` hands out.

```go
payer := &producer.Payer{
	Pool: pool, Tip: 100, Keys: map[string]*bwallet.Signer{signer.IdentityHex(): signer},
	Kept: &producer.Kept{}, Fees: mint.DefaultFees, Note: note,
}
fee, err := payer.Take(ctx)
tx, err := mint.Payment(ctx, dest, 1000, fee, change, mint.DefaultFees)
payer.Change(tx, 0, nil) // published, not yet mined: held back

_, err = payer.Take(ctx) // *producer.NoCoinError, Held 1

payer.Kept.Load = func(txid string) (*transaction.Transaction, error) { return tx, nil }
payer.Allow = func() []string { return []string{tx.TxID().String()} }
next, err := payer.Take(ctx) // next.Tx == tx
```

In an application the Payer also has a `Settler` and an `Asset`, and
`Settle` puts each mined transaction on the settlement leg: it waits for the
proof, or with `Async` returns once the leg has accepted it.

## Spend from a funding tree, minting the next

[`producer/example_test.go`](../producer/example_test.go), `ExampleTrees_Spend`

`producer.Trees.Spend(ctx, need)` answers the tree the next `need` carriers
spend from: the current one while it has the outputs and is locked to the
producer's identity, or a new one. A new tree is at least `need` outputs,
paid for from the pool, settled, adopted into the application's state
through its `TreeState`, and published so hosts see a later sweep of it.
A dry run builds it and does none of the rest.

```go
trees := &producer.Trees{
	Payer: payer, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
	Lock: func(ctx context.Context) (*script.Script, error) {
		return carrier.FundingLock(ctx, signer, signer.Originator, params)
	},
	Change: signer.FundScript,
	DryRun: true,
}
tree, first, err := trees.Spend(ctx, 6)
```

Output:

```text
this transition spends 6 outputs, so the tree is minted with 6 rather than 4
funding tree <tree>: 6 output(s) of 1000 sat
funding outputs: 6 first: 0 recorded: false
coins in the pool after GiveBack: 1
```

Once the tree and the carriers spending it are published before the tree
mines, a `producer.Collector` collects the proof later: each `Pending` item
names a kept transaction and how the application records its proof, and
`Collect` records it, publishes the proven BEEF again so every host upgrades
its copy, stamps the journal and releases held change.
`ExampleCollector_Collect` shows the item's shape with nothing yet mined.

### Mint the next tree ahead

[`producer/example_test.go`](../producer/example_test.go), `ExampleTrees_Wait`

With `Ahead` set, the spend that leaves the current tree with `Ahead`
outputs or fewer mints and settles the next tree in the background, and the
spend the current tree cannot cover switches to it with no wait for a block.
`Wait` collects the mint, and `Prepared` answers the record the tree will be
adopted as; it is adopted and published only at the switch. The example
settles and publishes on a test chain served from the process itself, one
in-process server standing in for arcade, the node and an overlay host,
which mines what it is given.

```go
trees := &producer.Trees{
	Payer: payer, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
	Lock: lock, Change: signer.FundScript, Facade: facade, Topic: topic,
	Ahead: 2,
}
spend(2)        // a tree is minted; 2 outputs are left, so the next is minted ahead
trees.Wait(ctx) // collect it
spend(2)        // the last two
spend(1)        // the switch
```

Output, with the settlement lines left out:

```text
spend 2: <tree 1> from output 0
minted ahead: <tree 2> trees adopted: 1
spend 2: <tree 1> from output 2
spend 1: <tree 2> from output 0
trees adopted: 2
funding tree <tree 1>: 4 output(s) of 1000 sat
funding tree <tree 1>: mined at height 701
funding tree published: admitted 1 output(s)
funding tree <tree 1> has 2 output(s) left, so the next is minted ahead
funding tree <tree 2>: 4 output(s) of 1000 sat
funding tree <tree 2>: mined at height 702
funding tree <tree 2> is minted ahead and waits for the switch
switching to funding tree <tree 2>, minted ahead
funding tree published: admitted 1 output(s)
```

### Recover a tree a stopped run left behind

[`producer/example_test.go`](../producer/example_test.go), `ExampleTrees_Recover`

A tree's fee coin leaves the pool when the tree is signed, and the tree is
adopted only once it has settled, so a run that stops in between leaves a
tree on the chain that the state does not record. With `Prepare` set the
application is handed the tree's record and the coin before the tree
reaches the settlement leg, and saves both; its `Adopt` drops the record.
On the next start it calls `Recover` for each record left. In the example
the run stops while its tree waits for a block, the block arrives, and the
next start adopts and publishes the tree and takes its change.

```go
func (s *state) Prepare(tree funding.Tree, coin bwallet.Output) error {
	s.prepared[tree.Txid] = prepared{tree, coin}
	return s.save()
}

func (s *state) Adopt(t funding.Tree) error {
	delete(s.prepared, t.Txid)
	s.cur, s.all = &t, append(s.all, t)
	return s.save()
}

trees := &producer.Trees{
	Payer: payer, State: state, Identity: signer.IdentityHex(), Count: 4, Sats: 1000, Funder: "pool",
	Lock: lock, Change: signer.FundScript, Facade: facade, Topic: topic,
	Prepare: state.Prepare,
}

// On every start, before the first Spend:
for txid, rec := range state.prepared {
	got, err := trees.Recover(ctx, rec.Tree, rec.Coin)
	if errors.Is(err, producer.ErrPublish) {
		// The tree is adopted and its record dropped: only the publish
		// is repeated, now or, with got.Tree noted, on a later start.
		err = trees.Publish(ctx, got.Tree)
	}
	if err != nil {
		continue // undecided: the record stays for the next start
	}
	if got.Outcome == producer.CoinSpent {
		delete(state.prepared, txid) // no tree: drop the record
	}
	// A CoinReturned record is kept for the next start: the tree may
	// still land.
}
```

Output, with the settlement line left out:

```text
the run stopped: true
trees adopted: 0 records kept: 1 coins in the pool: 0
recovered: tree adopted
trees adopted: 1 records kept: 0 coins in the pool: 1
funding tree <tree>: 4 output(s) of 1000 sat
funding tree <tree>: mined at height 701
funding tree <tree> is recovered
funding tree published: admitted 1 output(s)
```

The four outcomes, why a `CoinReturned` record is kept, what a publish
that fails after `Adopt` returns, and why a tree paid through `Fund` cannot
be covered, are in [configuration.md](configuration.md#prepare-and-recover).

## Read a host's payment ledger and settle it

[`payee/example_test.go`](../payee/example_test.go), `ExampleReadLedger`,
`ExampleClaims` and `ExampleSettler`

A host appends each payment it accepted to its ledger. Every version of a
line is read, a line cut short by a crash is skipped and named, and a line
is written back byte for byte as the host wrote it. The host's own rule
takes a txid once and a coin once.

```go
ps, err := payee.ReadLedger(strings.NewReader(ledger), "payments.jsonl", &warn)
// aa 5 version 1
// bb 7 version 2
// payments.jsonl line 3: not a payment; skipped
var c payee.Claims
c.Claim("aa", []string{"cc.0"})         // accepted
c.Claim("aa", []string{"cc.0"})         // replayed
c.Claim("bb", []string{"cc.0", "dd.1"}) // conflict
```

A payee's command reads the ledgers, then settles them into its home's pool
through its purse, recording each outcome in its own state:

```go
ps, err := payee.ReadLedgers(stderr, paths...)
s := &payee.Settler{App: "sample", Payer: p, Record: payee.Saved(&st.Book, st.Save),
	Pool: home.Pool, Out: stdout, Warn: stderr}
rep, err := s.Settle(ctx, ps)
if problem := rep.Problem(); problem != "" {
	// end the run with the application's refusal status
}
// 0 payment(s) settled, 0 sat; 1 settled before; 0 not settled; 0 refused (1 before); pool 0 output(s), 0 sat
```

## Filter text for a terminal

[`termsafe/example_test.go`](../termsafe/example_test.go), `ExampleSanitize`
and `ExampleValidateBounded`

A value from a record someone else published is filtered before it is
printed: a window-title sequence, a screen clear and a bidirectional
override are dropped, and color survives only when the reader asked for
it. On the publishing side, `ValidateBounded` refuses what a reader would
have to strip, naming the first offense, and an application puts its own
words in front.

```go
termsafe.Text(hostile)                                  // "status green evil"
termsafe.Sanitize(hostile, termsafe.Options{ANSI: true}) // colour kept, then reset
err := termsafe.ValidateBounded("plan", "ring\x07 the bell")
// plan line 1 has a control character (U+0007); errors.Is(err, termsafe.ErrUnsafe)
```

## Filter text for any renderer

[`sanitize/example_test.go`](../sanitize/example_test.go), `ExampleFilter`

Before a terminal or a web page shows text someone else wrote, `Filter`
applies one ordered set of character rules over a pinned Unicode table:
escape sequences, controls, bidirectional controls, zero-width characters
and hidden tag characters go, a tab becomes a space, and emoji keep the
selectors and joiners that change how they draw, the flags of England,
Scotland and Wales their tags. The TypeScript `filterText` and
`filterBytes` give the same bytes for the same value.

```go
sanitize.Filter("ship it" + "\x1b]0;pwned\x07" + "\u202e!")   // "ship it!"
sanitize.Filter("a\u200Db\uFE0F")                              // "ab"
termsafe.Text(sanitize.Filter(value))                           // a terminal also bounds it
```

```ts
import { filterText } from '@lightwebinc/bcommon'
node.textContent = filterText(value) // a text node, never markup
```

## Read a carrier in TypeScript

A topic manager or lookup service reads the same carrier with the TypeScript
twins. This function decodes and validates a carrier the Go example above
minted, with the payload rules and derivation of the same example
application:

```ts
import { Transaction } from '@bsv/sdk'
import { decodeCarrier, readerLockingKey, type PayloadCodec } from '@lightwebinc/bcommon'

// The example application's payload: "vxr", a version byte of 1, then the
// content. The format, like the derivation below, belongs to no application.
const codec: PayloadCodec<Uint8Array> = {
  inspect: (b) => {
    if (b.length < 3 || b[0] !== 0x76 || b[1] !== 0x78 || b[2] !== 0x72) return { kind: 'not-payload' }
    if (b.length < 4) return { kind: 'bad-payload', detail: 'too short' }
    return { kind: 'payload', payload: b }
  },
  validate: (p) => {
    if (p[3] !== 1) throw new Error('not a version 1 payload')
  },
}

export function readCarrier(carrierHex: string, identityHex: string): string {
  const lockingKeyFor = () => readerLockingKey([1, 'vector sample'], 'object', identityHex)
  const c = decodeCarrier(Transaction.fromHex(carrierHex), codec, lockingKeyFor)
  if (typeof c === 'string') return `refused: ${c}`
  return `output ${c.outputIndex}: ${new TextDecoder().decode(c.payload.subarray(4))}`
}
```

Given the carrier `ExampleMint` builds and the fixed key's identity, it
returns `output 0: an object`; given the same carrier with its locktime set
to 0, `refused: mineable`; given another identity, `refused: bad-lock`. The
package's own tests (`make ts-test`) check the twins against the shared
vectors; see [vectors.md](vectors.md).

## Read a record and a token's BEEF in TypeScript

A topic manager reads a record with the reader made for its own refusal
type, and a mined token from the BEEF it was submitted in:

```ts
import { BeefRefusal, readTokenOutput, readWire, recordReader, subjectTx, tokenShape, tokenSpends } from '@lightwebinc/bcommon'

class Refusal extends Error {
  constructor(readonly reason: string, detail?: string) {
    super(detail === undefined ? reason : `${reason}: ${detail}`)
  }
}
const rec = recordReader((reason, detail) => new Refusal(reason, detail))
const magic = Uint8Array.of(0x76, 0x78, 0x72, 0x01) // "vxr", version 1

export function readName(record: Uint8Array): string {
  const f = rec.decode(record, 256, 2, magic)
  return rec.text(f, 1)
}

export function admitToken(beef: Uint8Array): string {
  try {
    const w = readWire(beef) // the structural walk, then the read
    const tx = subjectTx(w)
    const parent = tokenShape(w, tx)
    const lock = Uint8Array.from(tx.outputs[0]!.lockingScript.toBinary())
    const out = readTokenOutput(lock, tx.outputs[0]!.satoshis, 0, 2)
    if (out === undefined) return 'refused: shape'
    return `${readName(out.fields[1]!)} spends ${tokenSpends(tx, parent).length} output(s) of ${parent.txid}`
  } catch (e) {
    if (e instanceof BeefRefusal) return `refused: beef: ${e.message}`
    if (e instanceof Refusal) return `refused: ${e.reason}`
    throw e
  }
}
```

The lock and the field signature are then held to the key the record names
(`tokenLockedTo`, `tokenSignedBy`), both proofs to the host's headers
(`mined`), and the record to the application's own rules against its
predecessor.

## Other offline examples

| Example | Shows |
|---|---|
| [`bwallet`](../bwallet/example_test.go) `ExampleProfile_Validate` | a wallet profile has no default; `Validate` names the missing or unusable field |
| [`knownkeys`](../knownkeys/example_test.go) `ExamplePin` | a second key for a pinned address is refused; a verified rotation keeps the old key as `@rotated-from` history |
| [`resolve`](../resolve/example_test.go) `ExampleParseAcct`, `ExampleManifest_Overlay` | the three written forms of a BRC-169 address, the alias refusal, and a BRC-180 manifest entry that is absent |
| [`hostset`](../hostset/example_test.go) `ExampleStatic_Hosts` | the hosts a static list and an IP literal yield |
| [`publish`](../publish/example_test.go) `ExampleFacade_Submit` | the object leg refusing a body that is not a BEEF, and a topic list, before it sends anything |
| [`producer`](../producer/example_test.go) `ExampleCollector_Collect` | a pending item's shape, reported pending while nothing has mined |
| [`termsafe`](../termsafe/example_test.go) `ExampleUTF8Locale` | the locale read through the application's lookup, and a key abbreviated for a message |
| [`chainview`](../chainview/example_test.go) `ExampleRefusedAnswer` | a settlement leg's error read as the network's definitive refusal or as transient |
| [`purse`](../purse/example_test.go) `ExampleRemittance` | a payment's remittance from what a host's ledger recorded, and the forms it refuses |

## Not covered offline

These need a live service and have no `Example` test: coinbase funding
(`bwallet.FundFromCoinbase`, `bwallet.Rescan`; coinbase: only on a regtest
chain you run, development and tests), node reads
(`nodeapi.WaitMined`, `nodeapi.Asset`), the settlement and object legs
(`publish.TCPIngress`, `RPCSettler`, `Arcade`, `Facade`), a producer's
settlement, proof collection and published trees (`producer.Payer.Settle`,
`SettleAndWait`, `Await`, `producer.Proofs`, `producer.Collector` with a
proof source), overlay lookups
(`lookup.Query`), domain discovery (`resolve.FetchManifest`,
`resolve.ResolveHandle`), the header service (`headers.Client`), a wallet
over the wire (`wirewallet.Dial`), the payment actions (`purse.Purse`,
which pays from a funded pool and settles through a leg) and a settle run
that reaches the chain (`payee.Settler` over a `purse.Purse`). Their parameters are in
[configuration.md](configuration.md); the packages' own tests exercise them
against local stand-ins.
