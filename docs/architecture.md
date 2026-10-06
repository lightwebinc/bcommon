# Architecture

bcommon is a library, not a service: twenty-eight Go packages under one module,
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
| 0 | `chirp` | none | yes |
| 0 | `hostset` | none | no |
| 0 | `resolve` | none | no |
| 0 | `guard` | none | yes |
| 0 | `mint` | none | yes |
| 0 | `headers` | none | yes |
| 0 | `wirewallet` | none | yes |
| 0 | `goldentest` | none | yes |
| 0 | `termsafe` | none | no |
| 0 | `sanitize` | none | no |
| 0 | `keyed` | none | yes |
| 0 | `testchain` | none | yes |
| 1 | `store` | `cbor`, `commit` | no |
| 1 | `record` | `cbor` | no |
| 1 | `knownkeys` | `guard` | no |
| 1 | `pushdrop` | `guard` | yes |
| 1 | `funding` | `guard` | yes |
| 1 | `nodeapi` | `guard` | yes |
| 1 | `lookup` | `hostset` | no |
| 2 | `carrier` | `guard`, `pushdrop` | yes |
| 2 | `chaintoken` | `guard`, `pushdrop` | yes |
| 2 | `chainview` | `nodeapi` | yes |
| 2 | `bwallet` | `guard`, `nodeapi`, `pushdrop` | yes |
| 2 | `publish` | `nodeapi` | yes |
| 3 | `verify` | `carrier`, `guard` | yes |
| 3 | `acceptance` | `chainview`, `nodeapi`, `publish` | yes |
| 3 | `producer` | `bwallet`, `funding`, `guard`, `mint`, `nodeapi`, `publish` | yes |
| 4 | `purse` | `bwallet`, `chainview`, `funding`, `guard`, `mint`, `nodeapi`, `producer`, `publish`, `termsafe` | yes |
| 5 | `payee` | `guard`, `purse`, `termsafe` | yes |

Every edge inside the module:

```text
  payee    ──▶ purse, guard, termsafe
  purse    ──▶ producer, bwallet, chainview, funding, guard, mint, nodeapi,
               publish, termsafe
  producer ──▶ bwallet, funding, guard, mint, nodeapi, publish
  verify   ──▶ carrier ──▶ pushdrop ──▶ guard
  chaintoken ▶ pushdrop, guard
  acceptance ▶ chainview, nodeapi, publish
  chainview ─▶ nodeapi
  verify   ──▶ guard
  carrier  ──▶ guard
  bwallet  ──▶ pushdrop
  bwallet  ──▶ nodeapi ──▶ guard
  bwallet  ──▶ guard
  funding  ──▶ guard
  knownkeys ─▶ guard
  publish  ──▶ nodeapi
  store    ──▶ cbor, commit
  record   ──▶ cbor
  lookup   ──▶ hostset
```

The graph is shallow on purpose. `mint` takes every lock script and every
unlocker as a parameter, so it needs neither `pushdrop` nor `carrier`, and
`producer` takes the funding lock the same way, so it needs neither either.
`termsafe` and `sanitize` import only the standard library. `record` reads bytes and
needs no SDK. `keyed` and `testchain` stand on go-sdk alone: the stand-in
chain serves the wire formats the clients read without importing a client,
so a test of a client is a test against something it shares no code with.
`TestLayers` holds `record`, `keyed`, `chaintoken`, `chainview`, `acceptance`,
`testchain`, `sanitize`, `payee`, `commit` and `chirp` to these edges, tests included.
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
  a subtree from ever being passed off as each other. Leaves are 32-byte
  commitments (`Root`, `Prove`, `Verify`) or byte strings of any length
  (`HashLeaf`, `RootOfBytes`, `ProveBytes`, `VerifyBytes`); `Builder` and
  `SegmentWriter` compute a root over leaves or content that arrive as a
  stream; `RootOfSubtrees` and `ProveSubtrees` compose per-block subtree
  roots into the whole tree when every block but the last holds the same
  power of two of leaves; `PathLen` and `VerifyAt` check a compact path,
  its hashes alone, whose sides follow from the index and the leaf count
  (RFC 9162 section 2.1.3.2). Indices there are 64-bit, so a tree of more
  than 2^31 leaves is described on 32-bit platforms too.
- **`chirp`** owns CHIRP (BRC-167) version 1 and chunking profile 1: the
  root and branch node encodings (the decoder refuses everything the BRC
  says a consumer rejects, and the encoder refuses what the decoder would),
  profile 1's canonical construction (`Build`, `BuildTree`, the streaming
  `Chunker`), `Verify`, which fetches a closure object by object, hashing
  each before reading it, under a reference bound, and checks content hash
  and canonical construction, and the UHRP object identifier and CHIRP URL
  of a hash. It takes go-sdk only for Base58. Minor versions above 0 are
  refused rather than read as 1.0.
- **`store`** owns how a record commits to a set of other records: the refs
  entry (name, root, count, optional head), the manifest that lists a
  store's members, and `store.Root`, the one rule that computes a store's
  root. A one-member store is its own head and its root is the leaf hash of
  that member; any other store's root is over its members' commitments in
  manifest order. An entry carrying a member this version does not define is
  kept verbatim and makes that one store unreadable, never the whole record.

- **`record`** owns the bounded, ordered reading of one application
  record: a canonical CBOR map with unsigned-integer keys, whose key 0 is
  the record's magic and whose keys above the last one a version defines are
  preserved and ignored. A record is refused for the first rule it breaks,
  in one order: the bound, before anything is decoded; one canonical CBOR
  map; at most `MaxKeys` (64) entries; unsigned-integer keys; the magic;
  then each defined key as the application reads it (missing, type, range
  or list). `Reason` gives each refusal the fixed label a host counts it
  by. `Claims` is the first look a classifier takes: a map head, key 0 and
  the magic, and nothing else. The application supplies the bound, the last
  defined key and the magic, and reads its own fields.

### Keys and outputs

- **`pushdrop`** owns the BRC-42/43 derivation every output is locked under
  (counterparty Anyone, forSelf true on the producer's side), the tagged
  PushDrop lock and its unlocker, and the decoding a reader runs. A reader
  holding only the identity key recomputes the locking key with
  `ExpectedLockingKey` and checks the embedded field signature.
  `CheckCanonical` takes a lock only as the one script the template writes
  for its key and fields, every push minimal and the key in its canonical
  encoding, and `DecodeTagged` applies it. A host that must decide on raw
  script bytes has the same rule in three steps: `FirstPush` reads the one
  push a classifier needs, `Fields` reads the pushes leniently, and
  `Script` rebuilds the one canonical script from them, to be compared byte
  for byte with what was read.
- **`carrier`** owns the carrier transaction: one input spending a funding
  output, one signed PushDrop record output carrying the payload, nLockTime
  4102444800 (2100-01-01) with a non-final input so it cannot be mined, and
  zero fee. It also owns the funding-output lock (`<key> OP_CHECKSIG <tag>
  OP_DROP`), its decode, the kill-switch `Sweep`, and `Validate`, whose check
  order is part of the contract: the payload's rules, then unmineability,
  then the lock derivation, then the signature. `Decode` and
  `DecodeFunding` take a lock only in its canonical encoding, and `Validate`
  parses the identity key with `guard.ParsePubKey`.

- **`chaintoken`** owns the BEEF a host admits a mined chain token in, and
  the token's own output. A chain token is a mined PushDrop output that the
  next token spends; a host reads the predecessor from the parent the
  submission's BEEF carries, never from what the topic holds, so a verdict
  depends on the submission's bytes and the host's headers alone. `ReadWire`
  reads a BEEF exactly as declared on the wire, before any parser merges two
  proofs of one block or collapses a transaction listed twice, so a rule
  counts what was sent. `MinimalPath` and `MergedPath` hold a Merkle path to
  exactly the leaves a proof needs. `Wire.Token`, `Wire.Carrier` and
  `Wire.Alone` are the three shapes: a token with its parent, a carrier
  with its funding tree, and a mined transaction alone. `TokenBEEF`
  assembles the first from a stored token and its parent, which is how a
  replayer brings a chain to another host. `ReadOutput`, `Output.LockedTo`
  and `Output.SignedBy` read a token output and hold it to the canonical
  script and a strict field signature (`CheckDER`, `VerifyField`), and
  `Spends` lists what a token spends of its parent. The application
  supplies its tags, its record codec, its derivation and its transition
  rules; the package decides no admission.
- **`keyed`** owns the part of BRC-369 keyed content that every
  application keyed under a random scalar shares: the content key
  (`SampleKey`, `CheckScalar`), its symmetric key and its commitment
  (BRC-369 sections 2.1 and 2.2), the check a holder runs on a key it has
  just unwrapped (`CheckOpened`: 32 bytes, a scalar, the committed one, in
  that order), and BRC-2's symmetric form (`SymmetricSeal`,
  `SymmetricOpen`), in which a key is wrapped and a certificate field is
  encrypted. It also owns one BRC-369 section 2.3 segment (`SealSegment`,
  `OpenSegment`: AES-256-GCM under the symmetric key with the IV
  `salt || 0^8`, the tag after), and the wrap of a content key for the
  members of one group epoch (`EpochWrapKey`, `WrapToEpoch`,
  `UnwrapFromEpoch`): BRC-2's symmetric form under
  `SHA-256(domain || epoch symmetric key || content id)`, the domain string
  the application's own, so that two applications using one epoch key never
  share a wrapping key. What a key encrypts, who it is released to and how
  a release is framed are the application's.

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
  signed by the key its coin is locked to and large enough to pay (never
  under the fee floor, and `TakeAtLeast` for a known amount), gives it back
  when a transaction is not sent, takes change back into the pool (holding change from an
  unmined transaction back until its proof arrives), and settles: waiting
  for a proof, or with `Async` returning once the leg has accepted the
  transaction. `Kept` makes every use of one kept transaction the same
  object, so no BEEF merges two copies of it. `Trees` is the funding-tree
  lifecycle: spend from the current tree while it has the outputs, otherwise
  mint, settle, record (through the application's `TreeState`) and publish
  the next one, sized to the spend. A tree's fee coin is back in the pool
  before any failure short of the settlement leg returns, and is released,
  never returned, once the tree is on the leg. With `Ahead`, it mints and settles the
  next tree in the background once a spend leaves the current one low, so
  a producer whose trees must mine before they are published does not wait
  for a block when the current tree runs out; the tree minted ahead is
  adopted and published only when a spend switches to it. Between the
  moment a tree's fee coin leaves the pool and the moment the tree is
  adopted, a run that stops leaves a tree that may be on the chain and that
  no state records, and a pool saved without the coin. `Prepare`, when set,
  closes that gap: it is called on the caller's goroutine with the tree's
  record and its fee coin once the tree is signed and before it reaches the
  settlement leg (for a tree minted ahead, before its background half), the
  application saves both, and its error aborts the mint with the coin back
  in the pool. On the next start `Recover` asks the node what became of each
  record: a tree that has a proof, or whose coin the node shows spent by it,
  is adopted and published, or held, behind
  any tree already held, while the current tree still has outputs, with its
  spent fee coin taken out of the pool and whatever of its change is unspent
  taken in; a tree the node does not know, whose coin is unspent, has not
  reached the chain, and the coin goes back to the pool, with the record
  kept for the next start in case the tree lands late; a tree with no proof
  whose coin another transaction spent is adopted nowhere, served by the
  node or not, and its coin leaves the pool. A publish that fails
  once `Adopt` has the tree is a `*PublishError` (`ErrPublish`), from
  `Spend` and `Recover` alike, and `Publish` repeats the publish alone. A
  tree paid through `Fund` cannot be covered: the wallet
  behind it chooses the coins, signs and broadcasts in one call, so there is
  no moment before the broadcast to record it in and no pool coin to
  return. `Proofs` asks arcade, then the node,
  whether a transaction mined, holding arcade's proof to the node's checks
  and arcade's acceptance to the node's view of the inputs (an input spent
  by another transaction is a refusal, since arcade can accept a double
  spend),
  and `Collector` collects every proof still owed, records it, publishes
  the proven transaction again so every host upgrades its copy, stamps the
  journal and releases held change. The application keeps its own state and
  its own words: the library reaches the state through `TreeState`,
  `Kept.Load` and `Pending`'s callbacks, reports progress through a `Note`
  function, and returns the refusals an application may word itself as
  typed errors (`NoCoinError`, `NoKeyError`) or through `Pending`'s hooks.
- **`chainview`** owns the answer to whether a transaction can still mine,
  from the network's own words and the node's view of outputs:
  `RefusedAnswer` reads a settlement leg's error as the network's
  definitive refusal or as transient, and `SpentElsewhere` names the first
  input another transaction spent. A leg that accepted a transaction, or
  that answers nothing, cannot be taken at its word, so a sender's sweep
  and a payee's payment both ask.
- **`purse`** owns the two payment actions the embedded wallet does not
  implement, for a priced question a host answers with 402 (BRC-105). On
  the client leg `CreateAction` pays exactly one P2PKH output of at most
  `MaxPay` from the pool, unbroadcast, as Atomic BEEF, which is what the
  SDK's AuthFetch asks a wallet for; `Settle` keeps the payment the host
  accepted and `Refund` returns every coin when none was. On the payee leg
  `InternalizeAction` (or its halves `Check`, `Broadcast`, `Await`, `Take`)
  takes a BRC-29 payment to this identity: each output must pay the key
  the identity derives for the remittance and the sender, and the
  transaction must verify against the headers, before it is broadcast and
  pooled with its derivation. A payment the network will never mine is a
  `RefusedError`.
- **`payee`** owns the payee's side of those payments, over the purse: the
  payee key (`HomeKey`, the home's root key in `identity.json`, and
  `KeyLine`, `CreateKeyFile` for the host's environment), the ledger a host
  appends each accepted payment to (`Payment`, versioned `LedgerV1` and
  `LedgerV2`, read by `ReadLedger` and written byte for byte as a host
  writes it by `Payment.Line`), the host's own rule over it (`Claims`: a
  txid once, a coin once, else 409), and `Settler`, which settles every
  payment the payee's record (`Record`, whose JSON is `Book`) holds neither
  as settled nor as refused. It checks each through the purse, broadcasts
  them all before it waits for any, at most `InFlight` at once, records
  each outcome as it comes, and counts the run in a `Report`. Whether a
  payment's coin was spent elsewhere is the purse's answer, from the leg's
  refusal and the node's view of the inputs (`chainview`): `payee` decides
  nothing about the chain itself.
- **`acceptance`** owns how much evidence a payment needs before the
  receiver acts on it. `Policy.Decide` is the value discriminator: a
  payment at or below the threshold is `Fast`, one above it is `Hold`, and
  the threshold is static satoshis or US cents through a `PriceSource`, zero
  (everything held) when the price is unknown or stale. `Exposure` bounds
  what the fast path has taken and not seen mined, per payer and in total,
  within a window, and holds a flagged payer. `Verifier.Accept` checks a
  payment (well formed, final, pays each `Output`, no more out than in, SPV
  of its ancestry to mined proofs against the receiver's headers),
  broadcasts it on the receiver's own leg, and for a fast decision asks the
  broadcasters' status and the node's spend view, through an optional
  watch window. `Verifier.Confirm` waits for a held payment's proof and
  checks it against the headers; `Monitor` watches fast payments until
  they mine, and reports and flags one that is lost. The zero `Policy`
  holds every payment.

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
- **`guard`** owns the checks on bytes someone else supplied before the SDK
  allocates for them or trusts them: a structural walk of a BRC-74 BUMP, a
  BEEF and a raw transaction, each bounding every declared count and length
  by the bytes present, and the one canonical encoding of a compressed
  public key.

### Terminal

- **`termsafe`** owns what reaches a terminal from text someone else wrote:
  `Sanitize` passes printable text and newlines, drops every control
  character, every escape sequence but a colour one the reader asked for,
  and the zero-width and bidirectional characters that disguise one string
  as another, and bounds the result at 200 lines of 512 columns.
  `Validate` and `ValidateBounded` are the publishing side of the same
  rules. It is the one package about terminals, and it still reads no
  environment itself: `UTF8Locale` takes the application's lookup.
- **`sanitize`** owns the character rules a renderer of any kind applies
  before it shows text someone else wrote, so that a terminal and a web
  page show the same characters: `Filter` maps a tab to a space and the
  line and paragraph separators to LF; removes controls but LF, escape
  sequences, the bidirectional controls, the zero-width and invisible
  characters, the supplementary variation selectors and tag characters
  outside a listed emoji tag sequence; keeps U+FE0E and U+FE0F only in a
  listed emoji variation sequence; and keeps U+200D only between two
  Extended_Pictographic characters. The property table it reads is
  generated from Unicode 15.1's emoji data and embedded, and the TypeScript
  twin reads the same bytes; neither uses its runtime's Unicode tables.
  `termsafe` is unchanged: a terminal renderer runs `Filter` and then
  `termsafe`, which also bounds the value and drops what it does not
  print.

### Tests

- **`goldentest`** holds the helpers the packages' tests share: the fixed
  test key (32 bytes of 0x42, never for real use), hex and transaction
  parsing that fail the test, and a chain tracker that knows only the roots a
  test hands it. No binary should import it.
- **`testchain`** holds a local stand-in chain for tests and local trials:
  it mines what it is sent, each transaction in a block of its own, and
  serves a node's JSON-RPC and asset API, an ARC-compatible broadcaster, a
  fabric ingress and a header source, and is itself a chain tracker. It
  refuses what a node refuses in the ways that matter to a test: a missing
  or spent input, immature coinbase, a script that fails, a non-final
  transaction. A test sets `Hold`, `Refuse` and `Busy` to stage what a
  network does. There is no proof of work and no real block; nothing that
  moves value should import it.

`TestHelpersStayInTests` fails on a production import of either anywhere in
the module.

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
| A BEEF from an overlay host, or kept by the application | `guard.ParseBEEF` walks V1, V2 (with its txid-only entries) and Atomic BEEF allocating nothing, bounds the BUMP count and each BUMP, the transaction count, every input, output and script length by the bytes remaining, refuses trailing bytes, a BEEF of no transactions and a transaction of no inputs, applies the caller's total-size bound, then parses under a `recover`. go-sdk v1.5.2 bounds its own counts too, but the protection does not rest on the pin | `verify.VerifyCarrier`, `funding.Rebuild` |
| A raw transaction from a node, a wallet or the application's state | `guard.ParseTransaction`, the same walk for one transaction | `funding.Rebuild`, `producer.Payer.Parent` |
| A node's transaction answer, raw or Extended Format (BRC-30) | `guard.RawTransaction` walks either form the same way, the Extended Format's previous outputs included, and returns the raw form, dropping those outputs | `nodeapi.Asset.TxRaw` |
| A public key from the wire | `guard.ParsePubKey` takes 33 bytes, prefix 0x02 or 0x03, x below the field prime and on the curve, and nothing else. go-sdk takes a compressed key whose x is at or above the prime and writes it back unreduced, a second encoding of one point | `carrier.Validate`, `bwallet.Counterparty`, `knownkeys`, and every PushDrop lock through `pushdrop.CheckCanonical` |
| A PushDrop lock | `pushdrop.CheckCanonical`: the lock is exactly the script the template writes for its key and fields; go-sdk's decoder reads wider pushes, trailing opcodes and other key encodings as the same fields | `pushdrop.DecodeTagged`, `carrier.Decode`, `carrier.DecodeFunding` |
| A CBOR record | the decoder checks every declared length against the bytes present before allocating, and bounds nesting at `MaxDepth` | `cbor.DecodeValue` |
| An application record | `record.Decode` checks the record's own bound before the decoder runs, then the entry count against `MaxKeys`; `Fields.Array` and `Fields.List32` check a list's length before the caller allocates for its elements | `record` |
| A BEEF a host is asked to admit | `chaintoken.ReadWire` applies the caller's bound and `guard.CheckBEEF` before it reads anything, so every count it then follows fits the bytes present; the TypeScript `readWire` runs the same walk (`checkBEEF`) before the SDK's readers | `chaintoken`, `wire.ts` |
| A locking script | `pushdrop.FirstPush` and `pushdrop.Fields` hold every declared push length to the bytes present and return slices of the script, allocating nothing for the data | `pushdrop`, `chaintoken.ReadOutput` |
| A payment handed to a wallet | `guard.ParseBEEF` under `guard.DefaultBound` before anything is derived or broadcast | `purse.Check` |
| A host's payment ledger | a line is read to `MaxLine` (16 MiB); a version 1 line's coins are read through `guard.ParseBEEF` under `guard.DefaultBound` | `payee.ReadLedger`, `payee.Payment.Spends` |
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
| `guard.ParsePubKey`, `guard.ParsePubKeyHex` | `strictPublicKey`, `strictPublicKeyHex` (and `readerLockingKey` applies it to the identity) |
| `pushdrop.CheckCanonical` | `decodeStrictPushDrop` (and `decodeFunding` and `inspectScript` apply it) |
| `pushdrop.FirstPush`, `pushdrop.Fields`, `pushdrop.Script` | `firstPush`, `pushFields`, `pushDropScript` (and `minimalPushBytes`) |
| `record.Decode` and its steps, the `Fields` readers, `record.CheckExtra`, `record.Encode`, `record.Claims`, `record.MaxKeys` | `recordReader` (`decode`, `decodeMap`, `split`, `checkMagic`, the readers, `checkExtra`, `encode`), `claimsRecord`, `MaxKeys` |
| `guard.CheckBEEF` | `checkBEEF` |
| `chaintoken.ReadWire`, `Wire.Token`, `Wire.Carrier`, `Wire.Alone`, `MinimalPath`, `MergedPath`, `TokenBEEF`, `Stored`, `Mined` | `readWire`, `tokenShape`, `carrierShape`, `aloneShape`, `minimalPath`, `mergedPath`, `tokenBEEF`, `storedToken`, `mined` |
| `chaintoken.ReadOutput`, `Output.LockedTo`, `Output.Signed`, `Output.SignedBy`, `Spends`, `VerifyField`, `CheckDER` | `readTokenOutput`, `tokenLockedTo`, `tokenSigned`, `tokenSignedBy`, `tokenSpends`, `verifyField`, `strictSignature` |
| `sanitize.Filter`, `sanitize.UnicodeVersion` | `filterText` (a string) and `filterBytes` (UTF-8 bytes), `UnicodeVersion` |
| `commit.HashLeaf`, `NodeHash`, `RootOfBytes`, `RootOfLeafHashes`, `RootOfSubtrees`, `ProveBytes`, `ProveLeafHashes`, `ProveSubtrees`, `VerifyBytes`, `PathLen`, `VerifyAt`, `Builder`, `SegmentRoot`, `SegmentWriter` | `hashLeaf`, `hashNode`, `rootOfBytes`, `rootOfLeafHashes`, `rootOfSubtrees`, `proveBytes`, `proveLeafHashes`, `proveSubtrees`, `verifyBytes`, `pathLength`, `verifyAt`, `RootBuilder`, `segmentRoot`, `SegmentWriter` |
| `acceptance.DefaultPolicy`, `Policy.Threshold`, `Policy.Decide`, `Exposure`, the fast path's reading of an `ArcadeStatus` | `defaultAcceptancePolicy`, `paymentThreshold`, `decidePayment`, `PaymentExposure`, `arcadeVerdict` |
| `chirp.EncodeRoot`, `EncodeBranch`, `DecodeRoot`, `DecodeBranch`, `Kind`, `BuildTree`, `Build`, `Verify`, `Identifier`, `ParseIdentifier`, `URL`, `ParseURL`, `ValidMediaType`, `Reason` | `encodeRoot`, `encodeBranch`, `decodeRoot`, `decodeBranch`, `nodeKind`, `buildChirpTree`, `buildChirp`, `verifyClosure` (async over the fetch), `chirpIdentifier`, `parseChirpIdentifier`, `chirpURL`, `parseChirpURL`, `validMediaType`, `ChirpError.code` |

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

The Go record reader returns sentinel errors, which an application counts
by `record.Reason`. The TypeScript `recordReader` takes the error to throw
instead, a function from a label and a detail to the application's own
refusal, so that a record refusal is one of the application's refusals and
not a second kind its callers must tell apart. The steps, their order and
their labels are the same.

A topic manager that admits a mined token must read the BEEF it was
submitted in, as declared, because what the BEEF holds is part of the rule.
`readWire` does that, after the same structural walk the Go guard makes
(`checkBEEF`): every count and length the bytes declare is held to the
bytes present before the SDK's readers follow it. The SDK's own readers
refuse some paths the Go reader reads and a shape then refuses, a path
listing one offset twice for one; either way the BEEF is refused as a BEEF,
and the vectors hold both languages to the same verdict on every case.

The renderer filter is the one twin a host does not need: a web renderer
runs it in the browser, over the same table, held to the same corpus
(`sanitize-v1.json`) byte for byte. Invalid UTF-8 becomes U+FFFD as the
WHATWG decoder (TextDecoder) replaces it, which the Go side does too, so a
value read from bytes filters the same in both.

A host or a provider that checks a stored object needs the byte-leaf roots
and the CHIRP codec, so both have twins, held to the same vectors
(`bytetree-v1.json`, `chirp-v1.json`) and the same refusal codes in the same
order. The streaming `chirp.Chunker` has none: a publisher builds closures,
a host checks them.

The builders (`mint`, `carrier.Mint`, `carrier.Sweep`), the 32-byte-leaf
RFC 6962 functions, the network clients, `verify`, `keyed`, `purse`, `payee`,
`acceptance`'s `Verifier` and `Monitor`,
`chainview` and `testchain` have no TypeScript twin: a topic manager reads
outputs, it does not build them, pay for them, or hold a key. A host's
ledger is written by the host itself, and `payee` reads and writes the same
bytes.

A host that answers a priced question takes the payment in TypeScript, so
it needs the value discriminator there too: the twin is the decision alone
(the threshold, the price's fail-safe, the per-payer and total exposure,
and how a broadcaster's status reads), with the same reason labels. The
evidence (the host's own broadcast, the status read, the node's spend
view) is gathered by the host with its own clients.

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
of go-sdk. `TestLayers` holds the packages of shared application code to
their place in the graph, and `TestHelpersStayInTests` keeps the test
helpers out of every production import.
