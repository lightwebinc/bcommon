# Examples

How to do the common things with bcommon, offline. Every Go example on this
page is an `Example` function in a package's `example_test.go`, with an
`// Output:` block, so `go test` compiles and runs it and fails if its output
changes. The excerpts below are taken from those files; follow the link
under each heading for the complete, runnable version.

Nothing here touches a network. Where an example needs a chain, it builds a
**lab chain** in the process: a stand-in mined coin with a stand-in proof,
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

## Build a funding tree on a lab chain

[`mint/example_test.go`](../mint/example_test.go), `ExampleFundingTree`

A funding tree is a mined transaction of equal-valued outputs set aside for
carriers, each locked with the application's funding lock. The fee input is
the lab coin; the fee loop signs, measures and rebuilds until the fee covers
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
output 4: 45610 sats, funding=false
fee: 390 sats for 388 bytes
proves through its parent: true <nil>
carriers it can fund: 4
```

The fee is the signed size plus two bytes per input, at one satoshi per
byte, because a DER signature can grow by a byte when the transaction is
re-signed. On a real chain the coin comes from `bwallet.FundFromCoinbase`,
which mines to the wallet's fund address on a node the application controls;
that needs a node and is not run here.

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

The hostile BEEF is refused with go-sdk's own reason, `BEEF BUMPs count
9223372036854775808 exceeds capacity of 0 remaining bytes`: go-sdk v1.5.2
bounds every count a BEEF declares against the bytes present, which is why
the library pins it exactly (see [dependencies.md](dependencies.md)), and no
allocation is attempted. `ERROR` is kept apart from every refusal: it means
the reader could not decide, typically because the header source did not
answer, so an outage is never read as a forgery. `ExampleCheck` shows
`verify.Check` refusing a nil tracker rather than dialing a default.

## Guard a proof before the SDK parses it

[`guard/example_test.go`](../guard/example_test.go), `ExampleParseBUMP`;
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

## Other offline examples

| Example | Shows |
|---|---|
| [`bwallet`](../bwallet/example_test.go) `ExampleProfile_Validate` | a wallet profile has no default; `Validate` names the missing or unusable field |
| [`knownkeys`](../knownkeys/example_test.go) `ExamplePin` | a second key for a pinned address is refused; a verified rotation keeps the old key as `@rotated-from` history |
| [`resolve`](../resolve/example_test.go) `ExampleParseAcct`, `ExampleManifest_Overlay` | the three written forms of a BRC-169 address, the alias refusal, and a BRC-180 manifest entry that is absent |
| [`hostset`](../hostset/example_test.go) `ExampleStatic_Hosts` | the hosts a static list and an IP literal yield |
| [`publish`](../publish/example_test.go) `ExampleFacade_Submit` | the object leg refusing a body that is not a BEEF, and a topic list, before it sends anything |

## Not covered offline

These need a live service and have no example: mining and funding
(`bwallet.FundFromCoinbase`, `bwallet.Rescan`), node reads
(`nodeapi.WaitMined`, `nodeapi.Asset`), the settlement and object legs
(`publish.TCPIngress`, `RPCSettler`, `Arcade`, `Facade`), overlay lookups
(`lookup.Query`), domain discovery (`resolve.FetchManifest`,
`resolve.ResolveHandle`), the header service (`headers.Client`) and a wallet
over the wire (`wirewallet.Dial`). Their parameters are in
[configuration.md](configuration.md); the packages' own tests exercise them
against local stand-ins.
