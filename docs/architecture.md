# Architecture

bcommon is a library, not a service: twenty-one Go packages under one module,
one TypeScript package, and no process of its own. Each package owns one part of what an
overlay application does when it publishes a committed record and when a
reader checks one, and none of them names an application. This page covers
how the packages are layered, what each one owns, the rules they share, and
how the Go and TypeScript halves are held to the same bytes.

## The shape of an application built on it

A **producer** writes a record in canonical CBOR and puts it in a carrier,
a transaction that is never mined and whose txid is the record's
commitment. A mined transaction, typically a state token the application
moves on each change, carries that commitment in a field. The producer pays
for carriers from a funding tree, a mined transaction of equal-valued outputs
set aside for them, and publishes each mined transaction on two legs: for
mining on the settlement leg, and as a BEEF to an overlay host on the object
leg.

A **reader** finds the producer's overlay host through the domain's
manifest, asks it a BRC-24 lookup question, and checks every answer itself:
the carrier's shape, its locking key against the identity it claims, its
field signature, its commitment against the one asked for, and its funding
parent against block headers the reader holds, never against a service it
would have to trust. It pins the identity key it saw.

```text
  producer                                   reader
  ────────                                   ──────
  cbor ─▶ store/commit (roots)               resolve ─▶ hostset ─▶ lookup
  pushdrop ─▶ carrier (payload lock)                         │
  mint ─▶ funding tree, state token                          ▼
  bwallet / wirewallet (keys, coin)          verify ─▶ carrier, pushdrop
  nodeapi (mine, prove) ─▶ guard             headers (chain tracker)
  publish: settlement leg │ object leg       knownkeys (pins)
  producer: fees, trees, kept, proofs        termsafe (what it prints)
```

## Package layers

The in-module import graph, from `go list` over the module's production
code (tests excluded). A package imports only packages on a lower layer,
beside the standard library and, where noted, go-sdk.

| Layer | Package | Imports from this module | Imports go-sdk |
|---|---|---|---|
| 0 | `cbor` | none | no |
| 0 | `commit` | none | no |
| 0 | `hostset` | none | no |
| 0 | `knownkeys` | none | no |
| 0 | `resolve` | none | no |
| 0 | `guard` | none | yes |
| 0 | `pushdrop` | none | yes |
| 0 | `mint` | none | yes |
| 0 | `funding` | none | yes |
| 0 | `headers` | none | yes |
| 0 | `wirewallet` | none | yes |
| 0 | `goldentest` | none | yes |
| 0 | `termsafe` | none | no |
| 1 | `store` | `cbor`, `commit` | no |
| 1 | `carrier` | `pushdrop` | yes |
| 1 | `nodeapi` | `guard` | yes |
| 1 | `lookup` | `hostset` | no |
| 2 | `verify` | `carrier` | yes |
| 2 | `bwallet` | `nodeapi`, `pushdrop` | yes |
| 2 | `publish` | `nodeapi` | yes |
| 3 | `producer` | `bwallet`, `funding`, `mint`, `nodeapi`, `publish` | yes |

Every edge inside the module:

```text
  producer ──▶ bwallet, funding, mint, nodeapi, publish
  verify   ──▶ carrier ──▶ pushdrop
  bwallet  ──▶ pushdrop
  bwallet  ──▶ nodeapi ──▶ guard
  publish  ──▶ nodeapi
  store    ──▶ cbor, commit
  lookup   ──▶ hostset
```

The graph is shallow on purpose. `mint` takes every lock script and every
unlocker as a parameter, so it needs neither `pushdrop` nor `carrier`, and
`producer` takes the funding lock the same way, so it needs neither either.
`termsafe` imports only the standard library.
`hostset` repeats `resolve`'s same-origin redirect rule rather than importing
it, so that neither package depends on the other. `headers` speaks the
overlay bridge's header API over HTTP and imports nothing from the bridge.
The root package, `github.com/lightwebinc/bcommon`, holds documentation and
the boundary tests, and no code.

To list the edges from the code:

```bash
GOWORK=off go list -f '{{$p := .ImportPath}}{{range .Imports}}{{$p}} {{.}}{{"\n"}}{{end}}' ./... \
  | grep ' github.com/lightwebinc/bcommon/' | sed 's#github.com/lightwebinc/bcommon/##g'
```

## What each package owns

### Record format

- **`cbor`** owns the bytes a record is written in: the RFC 8949 subset a
  record needs, under the core deterministic rules, enforced on both sides.
  The encoder only writes canonical bytes, and the decoder refuses anything
  it would not have written, so one value has exactly one encoding a reader
  accepts. Floats, tags, indefinite lengths and nesting past `MaxDepth` (16)
  are refused.
- **`commit`** owns the RFC 6962 Merkle Tree Hash, inclusion paths and their
  verification. The `0x00` leaf and `0x01` interior prefixes keep a leaf and
  a subtree from ever being passed off as each other.
- **`store`** owns how a record commits to a set of other records: the refs
  entry (name, root, count, optional head), the manifest that lists a
  store's members, and `store.Root`, the one rule that computes a store's
  root. A one-member store is its own head and its root is the leaf hash of
  that member; any other store's root is over its members' commitments in
  manifest order. An entry carrying a member this version does not define is
  kept verbatim and makes that one store unreadable, never the whole record.

### Keys and outputs

- **`pushdrop`** owns the BRC-42/43 derivation every output is locked under
  (counterparty Anyone, forSelf true on the producer's side), the tagged
  PushDrop lock and its unlocker, and the decoding a reader runs. A reader
  holding only the identity key recomputes the locking key with
  `ExpectedLockingKey` and checks the embedded field signature.
- **`carrier`** owns the carrier transaction: one input spending a funding
  output, one signed PushDrop record output carrying the payload, nLockTime
  4102444800 (2100-01-01) with a non-final input so it cannot be mined, and
  zero fee. It also owns the funding-output lock (`<key> OP_CHECKSIG <tag>
  OP_DROP`), its decode, the kill-switch `Sweep`, and `Validate`, whose check
  order is part of the contract: the payload's rules, then unmineability,
  then the lock derivation, then the signature.

### Producer

- **`mint`** owns the mined transactions: a state-token transition, a
  funding tree and a payment, with a fee loop that signs to measure the size
  and rebuilds at the rate until the fee covers it. It holds no key material:
  every input arrives with the template that signs it.
- **`funding`** owns the state an application keeps about its funding tree
  between runs (`Tree`, whose JSON form is part of the application's state
  file) and the BEEF it keeps for a transaction published before it mined.
- **`bwallet`** owns an embedded BRC-100 wallet backend (an identity file
  and a coin pool, both written atomically at mode 0600), a `Signer` that
  signs through any `wallet.Interface`, BRC-29 payment derivations, and
  coinbase funding on a chain the application mines itself. Every method it
  does not implement returns `ErrNotSupported` rather than `nil, nil`.
- **`wirewallet`** owns the wallet wire: a client for a BRC-100 wallet on
  the loopback interface only, and a handler that serves any
  `wallet.Interface` over the wire.
- **`nodeapi`** owns a Teranode JSON-RPC and asset API client: mining,
  direct submission, placement and proofs. Every body is bounded before it
  is parsed, and every proof is checked to name the transaction it was asked
  for.
- **`publish`** owns the two submission legs, which never share a
  connection. The settlement leg (`TCPIngress`, `RPCSettler`, `Arcade`) only
  ever takes a `*transaction.Transaction`; the object leg (`Facade`) only
  ever takes a BEEF, and refuses anything else before sending. No exported
  function takes both, and a test walks the package's exported declarations
  to hold that. It also owns the per-transition `Journal`.
- **`producer`** owns the orchestration around those builders that every
  producer runs the same way. `Payer` takes a fee input from the coin pool,
  signed by the key its coin is locked to, gives it back when a transaction
  is not sent, takes change back into the pool (holding change from an
  unmined transaction back until its proof arrives), and settles: waiting
  for a proof, or with `Async` returning once the leg has accepted the
  transaction. `Kept` makes every use of one kept transaction the same
  object, so no BEEF merges two copies of it. `Trees` is the funding-tree
  lifecycle: spend from the current tree while it has the outputs, otherwise
  mint, settle, record (through the application's `TreeState`) and publish
  the next one, sized to the spend. `Proofs` asks arcade, then the node,
  whether a transaction mined, holding arcade's proof to the node's checks,
  and `Collector` collects every proof still owed, records it, publishes
  the proven transaction again so every host upgrades its copy, stamps the
  journal and releases held change. The application keeps its own state and
  its own words: the library reaches the state through `TreeState`,
  `Kept.Load` and `Pending`'s callbacks, reports progress through a `Note`
  function, and returns the refusals an application may word itself as
  typed errors (`NoCoinError`, `NoKeyError`) or through `Pending`'s hooks.

### Reader

- **`headers`** owns the chain tracker, the root of trust for every proof a
  reader checks. A 404 from the header service is "not held yet", and any
  other failure is an error, never a false answer, so an outage is not read
  as a forged proof. A source that serves header fields has each header's
  proof of work checked against the network's floor.
- **`hostset`** owns which address answers for an overlay host: DNS or a
  static list as the source, a first, random or all policy, and an optional
  quorum of distinct hosts whose bodies the caller compares.
- **`lookup`** owns the BRC-24 output-list client over `hostset`. It does
  not use go-sdk's resolver, which runs discovery against public trackers
  when unconfigured.
- **`resolve`** owns BRC-169 handle resolution and BRC-180 overlay discovery
  under a strict client policy: HTTPS only, system roots only, no proxy,
  redirects only within the origin, bounded bodies. It does not go looking
  for a service the domain's manifest does not declare.
- **`knownkeys`** owns the grammar and store of a pin file: pin, rotate,
  retire, forget. A malformed line is an error, never a skipped pin, and a
  file other users can write is refused.
- **`verify`** owns the refusal vocabulary (`Code`), the SPV verdict on one
  transaction (`Check`) and the carrier check every reader runs
  (`VerifyCarrier`). It refuses a nil chain tracker rather than let the SDK
  dial a public service in its place.
- **`guard`** owns the bounds check on a BRC-74 BUMP before the SDK
  allocates for it.

### Terminal

- **`termsafe`** owns what reaches a terminal from text someone else wrote:
  `Sanitize` passes printable text and newlines, drops every control
  character, every escape sequence but a colour one the reader asked for,
  and the zero-width and bidirectional characters that disguise one string
  as another, and bounds the result at 200 lines of 512 columns.
  `Validate` and `ValidateBounded` are the publishing side of the same
  rules. It is the one package about terminals, and it still reads no
  environment itself: `UTF8Locale` takes the application's lookup.

### Tests

- **`goldentest`** holds the helpers the packages' tests share: the fixed
  test key (32 bytes of 0x42, never for real use), hex and transaction
  parsing that fail the test, and a chain tracker that knows only the roots a
  test hands it. No binary should import it.

## Parse and guard before trusting

Every byte string that arrives from another party is hostile until checked,
and the rule for length-prefixed data is that a declared length is a claim,
checked against the bytes actually present before it sizes any memory. A
count a few bytes can declare can ask for an allocation that ends the
process with an out-of-memory no `recover` sees, so checking after the
allocation is too late.

Where the library applies it:

| Input | Guard | Where |
|---|---|---|
| A BRC-74 BUMP from a service | `guard.ParseBUMP` walks it allocating nothing, refuses any level count the remaining bytes cannot encode and any trailing byte, then parses under a `recover` | `nodeapi.ProofFor`, `nodeapi.Asset.MerkleProof` |
| A BEEF from an overlay host | go-sdk v1.5.2's BEEF parser bounds every count against the bytes remaining, which is one reason the pin is exact ([dependencies.md](dependencies.md)); there is no separate BEEF guard | `verify.VerifyCarrier`, `funding.Rebuild` |
| A CBOR record | the decoder checks every declared length against the bytes present before allocating, and bounds nesting at `MaxDepth` | `cbor.DecodeValue` |
| A record's store references | `MaxRefs` (64 stores), `MaxRefMembers` (8 members per entry), `MaxRefName` (64 bytes), `MaxMembers` (1024 per manifest) bound the work one record can ask of a reader | `store` |
| An HTTP response | every body is read to a bound before it is parsed, and most clients refuse one over the bound rather than parse a truncated answer | `headers`, `nodeapi`, `hostset`, `resolve`, `publish`, `wirewallet` |

After parsing, a reader checks what an answer is before believing what it
says. `VerifyCarrier` checks the commitment against the one asked for before
anything the carrier says about itself, applies the caller's expectations
before any header lookup, and only then proves the funding parent. A proof
from a node must name the transaction it was requested for, because a valid
proof of some other transaction verifies perfectly and proves nothing.

## Applications supply their own constants

Nothing in the library chooses a derivation, a tag, a record magic, a topic,
a basket, an RPC id or a pin-file header. Every one arrives as a parameter:

- `pushdrop.Derivation` and `carrier.Params` carry the derivation protocol,
  key id and funding tag.
- `bwallet.Profile` carries the fund-key derivation, the basket, the version
  string and the legacy coin-file name, and has no default.
- `nodeapi.RPC.ID` is required, and `knownkeys.Save` requires the header.

A default would quietly re-key an application that forgot to pass its own,
or put one application's name on another's requests. Because these values
are hashed into keys or written on chain, they are frozen once an
application first uses them; [configuration.md](configuration.md) lists
which. The values applications have chosen are recorded in
[registry.md](registry.md), so that no two collide. The library's own tests
use a derivation and tag reserved for tests (`[1, "vector sample"]`, prefix
`vx`) that no application may use.

## Go and TypeScript twins

An overlay host that admits an application's outputs runs a topic manager
and a lookup service, which are usually TypeScript. The package under `ts/`,
`@lightwebinc/bcommon`, holds the parts of the Go library such a module needs
to read what a producer wrote:

| Go | TypeScript |
|---|---|
| `cbor.Encode`, `cbor.DecodeValue` | `encode`, `decodeValue` |
| `store.EncodeRefs`, `store.DecodeRefs`, the `MaxRef*` bounds | `encodeRefs`, `decodeRefs`, the same bounds |
| `pushdrop.Derivation.ExpectedLockingKey` | `readerLockingKey` |
| `pushdrop.Tagged.VerifySignature` | `verifyFieldSignature` |
| `carrier.DecodeFunding` | `decodeFunding` |
| `carrier.Decode` with `Carrier.Validate`, `carrier.Commitment`, `carrier.LockTime` | `decodeCarrier` (and its steps `inspectScript`, `payloadRefusal`, `mineableRefusal`, `unlockingRefusal`, `lockRefusal`, `signatureRefusal`), `commitment`, `LockTime` |
| `carrier.CheckUnlocking`, `carrier.SigHashType` | `unlockingRefusal`, `SigHashType` |

The TypeScript `decodeCarrier` applies the same checks in the same order as
the Go `Validate`, and reports a refusal as one of a small fixed set of
strings, because a topic manager counts it as a metric label.

Both refuse a carrier whose input is not spent by exactly one canonical
signature push: one input, and an unlocking script that is exactly one
minimally encoded push of a strict DER signature (BIP 66) with R in
[1, n-1] and S in [1, n/2], then the sighash byte SIGHASH_ALL|FORKID (0x41).
The script interpreter accepts looser forms, a high S, a wider push, an
extra push or a no-op, and each is a different txid spending the same
funding output with the same record, so without the check anyone who saw a
carrier could present a spend of its funding output under another txid, and
a host would read the record as retracted. In Go the refusal is `carrier.ErrUnlocking`, reported
by `verify.VerifyCarrier` as `REFUSED-UNLOCKING`; in TypeScript it is
`non-canonical-unlocking`, which `decodeCarrier` returns after the finality
check and before the lock. A host that must decide on a transaction before
it knows which output is the record calls `unlockingRefusal` on its own.

The package also carries the overlay engine's module interfaces (`TopicManager`,
`LookupService`, `Module` and the rest) and, under the separate
`@lightwebinc/bcommon/testing` entry point, Node test helpers that drive a
lookup service in the engine's own order.

The builders (`mint`, `carrier.Mint`, `carrier.Sweep`), the RFC 6962
functions, the network clients and `verify` have no TypeScript twin: a topic
manager reads outputs, it does not build them.

The runtime entry point imports nothing but its peer `@bsv/sdk`, pinned
exactly at 2.7.1, and nothing from `node:`, so a browser can load it too.

## One set of vectors for both

Both halves are tested against the same files in `testdata/vectors`, which
a separate generator writes with code that shares nothing with the library
(an independent CBOR encoder, its own RFC 6962 implementation, go-sdk's
primitives called directly). The Go tests rebuild each vector byte for byte;
the TypeScript tests read the same files, never a copy, and decode and
verify what the Go side built. A change that moves a vector is a change in
bytes applications may already have committed to the chain. See
[vectors.md](vectors.md) for the families and what each side checks, and
[versioning.md](versioning.md) for why one tag versions both languages.

## Imports

Every package, in production code and in tests, imports only the standard
library, go-sdk and other packages of this module. `TestBoundary` and
`TestBoundaryFiles` enforce it, and `make deps-check` holds `go.mod` to one
direct requirement at the pinned version. See
[dependencies.md](dependencies.md).

No package takes on what belongs to the application's command: flags,
logging, the environment, the user's configuration directories, the
standard streams, other processes or the process's exit.
`TestNoProcessConcerns` reads every library file for them, and
`TestTermsafeImportsOnlyTheStandardLibrary` keeps the terminal filter free
of go-sdk.
