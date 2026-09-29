# Configuration

bcommon is a library. It has no configuration file, reads no environment
variables and keeps no state of its own; everything it needs arrives as a
parameter from the application that imports it. This page lists every
parameter and option struct a caller supplies, the default each one takes
when left at its zero value, and which values are frozen once an application
has used them on chain.

Two rules run through all of it:

- **What names an application has no default.** The library never fills in
  a derivation, a tag, a wallet profile, an RPC id or a pin-file header, and
  the calls that take a profile, an RPC id, a header or a payload validator
  refuse one left out rather than fall back. A default would quietly re-key
  one application as another, or put one application's name on another's
  requests.
- **Network clients take no proxy by default.** Every default HTTP client in
  the library ignores `HTTP_PROXY`, so a run reaches exactly the hosts in its
  configuration. (The wallet wire dials loopback only, which Go never
  proxies.) A deployment that needs a proxy supplies its own
  `*http.Client`, where the choice is visible.

## Frozen once used

These values are hashed into keys, written into transactions or recomputed
by every reader. Changing one after an application has published under it
orphans what was already published: it is a new value, never an edit.

| Value | Where it is set | Why it cannot change |
|---|---|---|
| Derivation protocol (security level and name) and key id | `pushdrop.Derivation`, `carrier.Params.Derivation` | Hashed into every derived locking key (BRC-43) |
| Output tags (funding tag, state tag, any other type byte) | `carrier.Params.FundingTag`, the fields passed to `Derivation.Lock` | What a topic manager admits outputs by |
| Record magic, topic and lookup service names | the application's own code | What hosts and readers key on; see [registry.md](registry.md) |
| Carrier nLockTime 4102444800 and input sequence 0 | `carrier.LockTime`, `carrier.Sequence` (constants) | A reader refuses a carrier with a lower locktime |
| RFC 6962 leaf and node prefixes, the store root rule | `commit`, `store.Root` (fixed) | Every reader recomputes roots with them |
| Canonical CBOR encoding | `cbor` (fixed) | A record's commitment covers its exact bytes |
| Fund-key derivation of a wallet | `bwallet.Profile.FundProtocol`, `FundKeyID` | Changing either strands every coin already paid to the key |
| Funding-tree state file shape | `funding.Tree` JSON tags | Part of the application's state file |

The library's own tests use `[1, "vector sample"]` with key ids `object` and
`state`, and the tag prefix `vx`. These are reserved for tests and no
application may use them.

## Derivation: `pushdrop.Derivation`

| Field | Type | Default | |
|---|---|---|---|
| `Protocol.SecurityLevel` | `wallet.SecurityLevel` | none | 0, 1 or 2. Part of the protocol id: `[1, "name"]` and `[2, "name"]` are different protocols |
| `Protocol.Protocol` | `string` | none | the protocol name |
| `KeyID` | `string` | none | the key id within the protocol, 1 to 800 bytes |

The invoice number is `<level>-<name>-<key id>`. Every derivation is
counterparty Anyone; the producer's wallet locks with forSelf true, and a
reader recomputes the key from the identity key alone with
`ExpectedLockingKey`.

`Derivation.Validate` applies go-sdk v1.5.2's BRC-43 rules when the value is
built, rather than at the first mint: a name of 5 to 400 characters (430 for
a `specific linkage revelation ` name) of lowercase letters, digits and
single spaces, measured after trimming and lower-casing, and not ending in
` protocol`. The SDK stays the authority; nothing in the library calls
`Validate` for you.

`Derivation.Lock(ctx, w, originator, fields, sign)` takes the wallet, the
BRC-100 originator presented on every key call, the fields (tag first by
convention), and whether to append the wallet's signature over them.
`pushdrop.DecodeTagged(script, tag, nfields)` takes the tag and the field
count the application expects, tag included.

## Carrier: `carrier.Params` and `carrier.Classify`

| Field | Default | |
|---|---|---|
| `Derivation` | none | locks the record output and the funding outputs. **Frozen** |
| `FundingTag` | none | the one field of a funding output, `<key> OP_CHECKSIG <tag> OP_DROP`. **Frozen** |
| `ValidatePayload` | none; `Validate` refuses a `Params` without one | the payload's own rules, run first inside `Validate`; its error is returned as is |
| `ErrIdentity` | `carrier.ErrIdentity` | the sentinel an identity key that does not parse is wrapped in, so the application words its own refusal |

`carrier.Decode(tx, classify)` takes a `Classify func(payload []byte) (ours
bool, err error)`: `(false, _)` skips an output, `(true, nil)` takes it, and
`(true, err)` refuses the whole transaction. Exactly one output may be taken.

`carrier.Mint(ctx, w, originator, p, payload, funding, vout)` spends output
`vout` of the funding tree; output 0 carries the whole input value, so the
fee is zero. `carrier.Sweep(ctx, w, originator, p, tree, vouts, fee,
feeVout, feeUnlocker, change, feeRate, floor)` is the kill switch: with a nil
`fee` the fee comes out of the tombstone, otherwise from `fee` with the
remainder to `change`.

Constants, fixed in the library: `LockTime` 4102444800 (2100-01-01T00:00:00Z)
and `Sequence` 0.

## Transaction builders: `mint`

| Parameter | Default | |
|---|---|---|
| `Fees.SatPerByte` | none; `mint.DefaultFees` is 1 | rate per byte of the signed transaction |
| `Fees.Floor` | none; `mint.DefaultFees` is 250 | minimum fee in satoshis; change below it goes to the fee |
| `Input{Tx, Vout, Unlocker}` | none | an input's source transaction, output index and the template that signs it. The template keeps key material out of `mint` |
| `change` | none; a nil script is `ErrNoChange` | where the fee input's remainder goes |

`FundingTree(lock, count, sats, fee, change, fees)` builds `count` outputs of
`sats` each under `lock` (normally `carrier.FundingLock`).
`Transition(lock, sats, prev, fee, change, fees)` takes a nil `prev` for a
create and the previous token (with its unlocker, which is required) for an
update. `Payment(ctx, dest, sats, fee, change, fees)` pays `dest`, normally a
BRC-29 destination from `bwallet`.

## Funding-tree state: `funding.Tree`

The application stores this struct in its own state file. Its JSON field
names (`identityKey`, `txid`, `rawHex`, `bumpHex`, `beefHex`, `height`,
`sats`, `count`, `next`, `funder`) are part of that file's format. `BeefHex`
is kept while the tree is unmined and cleared once `BumpHex` is set; `Funder`
is the application's own note and nothing in the library reads it.

## Stores and records: `store`, `cbor`

| Bound | Value | |
|---|---|---|
| `cbor.MaxDepth` | 16 | nesting depth the decoder accepts |
| `store.MaxRefs` | 64 | stores one record may commit to |
| `store.MaxRefMembers` | 8 | members in one refs entry (four are defined) |
| `store.MaxRefName` | 64 bytes | a store name, and a manifest member's name and type |
| `store.MaxMembers` | 1024 | members in one manifest |
| `Manifest.Body(bound)` | caller's | the body bound of the record that will carry the manifest; the application's, because it follows what that application's records may carry |

Where the refs value sits in a record, and which record kinds may carry one,
belong to the application: the codec works on the array value alone.
`store.MemberOverhead` is the measured encoded cost of one member apart from
its name and type, for sizing a manifest before minting its members; it is
read-only.

## Wallet: `bwallet`

### `bwallet.Profile`

Required, with no default. `Profile.Validate` names the first field missing
or unusable, and `Create`, `Open`, `OpenIdentity` and `OpenPool` call it
before touching the disk.

| Field | |
|---|---|
| `FundProtocol` | the fund key's protocol, derived with counterparty self. The security level is taken as given (0 is valid). **Frozen** once coin is paid to the key |
| `FundKeyID` | the fund key's key id. **Frozen** once coin is paid to the key |
| `FundBasket` | the one basket `ListOutputs` answers for; lowercase letters, digits and spaces |
| `Version` | what `GetVersion` answers |
| `LegacyPoolFile` | a bare file name the coin file had before `wallet.json`, adopted in place when `wallet.json` is absent. Required even with no earlier name: then use a name no program writes, such as `<application>-pool.json`. It may not be `wallet.json` or `identity.json` |

### Directory layout

`Create(dir, profile)` writes a new identity and refuses a directory that
already holds one. `Open(dir, profile)` reads one and treats a missing pool as
empty. The directory is mode 0700 and holds `identity.json` (the root key as
WIF) and `wallet.json` (the spendable outputs), both 0600 and both written
atomically. `OpenIdentity(path, pool, profile)` opens a second identity
file that shares the primary's pool, for a key rotation; pass the primary's
profile. `OpenPool(dir, profile)` opens the coin without an identity, for a
directory whose key lives in a wire wallet.

### `bwallet.Embedded` and `bwallet.Signer`

| Field | Default | |
|---|---|---|
| `Embedded.Chain` | nil | the tip height source (`headers.Client` satisfies it). Without one, `GetHeight` answers `ErrNoChain` and coinbase outputs are reported not spendable |
| `Embedded.Mainnet` | false | address prefix and the `GetNetwork` answer; a private chain answers testnet |
| `Embedded.Originator` | empty | the BRC-100 originator on every key call the wallet makes for itself |
| `Signer.Interface` | none | the wallet that signs: an `Embedded`, or one reached through `wirewallet.Dial` |
| `Signer.Identity` | none | the root public key |
| `Signer.Originator` | empty | originator on every call |
| `Signer.Mainnet` | false | address prefix where one is rendered |
| `Signer.Profile` | none | the fund methods refuse an incomplete profile; calls that never touch the fund key work without one |

### Funding on a chain you mine

`FundFromCoinbase(ctx, signer, pool, rpc, asset, blocks, batch)` mines
`blocks` blocks paying the fund address and adds the coinbase outputs to the
pool. `batch` is how many blocks one `generatetoaddress` call asks for; 0
means `DefaultFundBatch` (30). A coinbase output is spendable once
`CoinbaseMaturity` (100) blocks bury it. `Rescan(ctx, signer, pool, asset,
from, to)` recovers outputs a stopped run did not record. Both need a node;
none of the examples run them.

`PaymentProtocol` is BRC-29's fixed protocol (`[2, "3241645161d8"]`). It is
read-only.

## Wallet wire: `wirewallet`

`Dial(originator, base, timeout)` connects to a BRC-100 wallet over the
wallet wire. `base` must be an `http` URL on a loopback address or
`localhost` (`CheckURL`); anything else is `ErrNotLoopback`, because the wire
carries no authentication. A remote wallet is reached through a tunnel.
`timeout` bounds each call; 0 means none. `Serve(w)` returns the handler for
the other end.

## Node API: `nodeapi`

### `nodeapi.RPC`

| Field | Default | |
|---|---|---|
| `URL` | none | the JSON-RPC endpoint, for example `http://node.example.com:21292/` |
| `User`, `Pass` | empty | basic authentication |
| `ID` | none; `Call` refuses an empty id with `ErrNoID` before sending | the JSON-RPC request id, sent verbatim; the application's to choose |
| `Client` | 60 s timeout, no proxy, TLS 1.2 minimum | long because `generatetoaddress` mines inline; the lever for a slow node is the batch size |

### `nodeapi.Asset`

| Field | Default | |
|---|---|---|
| `Base` | none | the asset API root with no path suffix, for example `http://node.example.com:20090`; requests go to `<Base>/api/v1/...` |
| `Client` | 30 s timeout, no proxy, TLS 1.2 minimum | |

`WaitMined(ctx, asset, txid, poll)` polls every `poll`; 0 means 2 s. Every
response body is bounded at 1 MiB, and a 429 from either API is retried after
200 ms, 1 s and 3 s. `ProofFor(raw, txid)` holds a supplied proof to the same
bound.

## Publishing: `publish`

| Type and field | Default | |
|---|---|---|
| `TCPIngress.Addr` | none | `host:port` of a fabric ingress that takes a bare EF transaction stream |
| `TCPIngress.DialTimeout` | 5 s | |
| `TCPIngress.WriteTimeout` | 10 s | a context deadline overrides it |
| `RPCSettler.RPC` | none | a `*nodeapi.RPC`; the transaction goes as hex through `sendrawtransaction` |
| `Arcade.Base` | none | the ARC-compatible API base; requests go to `<Base>/tx`, `<Base>/tx/{txid}` and `<Base>/policy` |
| `Arcade.Key` | empty | sent as a bearer token when set |
| `Arcade.Client` | 30 s timeout, no proxy | |
| `Arcade.Verdict` | `DefaultVerdict` (15 s) | how long `Submit` waits for the network's verdict; no verdict inside it is not an error |
| `Arcade.Poll` | 1 s | interval between status checks while waiting |
| `Arcade.Note` | nil | receives lines a caller may show: a verdict that did not arrive, a backoff |
| `Facade.Base` | none | the overlay host's root; the BEEF goes to `<Base>/submit` |
| `Facade.HTTP` | 30 s timeout, no proxy | |
| `Journal.Dir` | none | one `<seq>-<txid>.json` file per transition attempt, mode 0600, created on first write |

`Facade.Submit(ctx, topic, beef)` takes exactly one topic name per call. It
refuses a body without a BEEF marker (`ErrNotBEEF`) and a topic that is empty
or contains a comma, space, tab, CR or LF, both before sending anything.
The facade's answer is refused over 1 MiB; arcade's answers are read to at
most 64 KiB (submit) and 1 MiB (status) before they are parsed.

## Producer: `producer`

### `producer.Payer`

| Field | Default | |
|---|---|---|
| `Pool` | none | the `*bwallet.Pool` every fee comes from and change returns to |
| `Tip` | 0 | the chain height coinbase maturity is judged at |
| `Keys` | none | every key the producer signs with, by identity key hex; a coin is spent by the key whose fund script locks it, and change to any of them is taken back |
| `KeyFor` | a lookup in `Keys` | the key of the identity that owns a received payment, for an application that words its own refusal |
| `Kept` | nil | the producer's `*producer.Kept`; needed once unproven change can be spent |
| `Allow` | none allowed | the kept transactions whose unproven change `Take` may spend when no proven coin is left |
| `Settler` | none | the settlement leg; `Settle` refuses without one |
| `Asset` | none | the node a proof is waited for from and, with `Async`, where a coin's parent is fetched |
| `Async` | false | settle on the leg's acceptance and collect the proof later |
| `Fees` | the zero policy | fees for what the Payer mints itself (a funding tree); `mint.DefaultFees` is the usual one, and zero pays no fee |
| `Timeout` | `DefaultTimeout` (10 min) | how long a wait for a proof lasts |
| `Poll` | `DefaultPoll` (5 s) | how often the wait asks |
| `Note` | discard | receives each progress line |

`Take` returns `*producer.NoCoinError` when the pool has no coin it may
spend (`Held` counts the transactions whose change is waiting for a proof)
and `*producer.NoKeyError` for a coin none of the keys holds, so the
application can say what its users should do.

### `producer.Trees`

| Field | Default | |
|---|---|---|
| `Payer`, `State` | none | the Payer that pays for a tree, and the application's `TreeState` (`Current`, `Adopt`) |
| `Identity` | none | identity key hex whose key locks the tree; a current tree of another identity is replaced |
| `Count`, `Sats` | none | outputs in a new tree (at least; a larger spend gets a larger tree) and the value of each |
| `Funder` | empty | recorded in `funding.Tree.Funder` |
| `Lock`, `Change` | none | the funding lock and the change script, asked only when the pool pays |
| `Fund` | nil | mints and settles a tree some other way, such as a funding wallet |
| `DryRun` | false | build a pool-paid tree and record, settle and publish nothing |
| `Facade`, `Topic` | none | the object leg a new tree is published on |

### `producer.Collector` and `producer.Pending`

| Field | Default | |
|---|---|---|
| `Proofs` | nothing mined | `producer.Proofs{Arcade, Asset}`: arcade first, when it is the settlement leg, then the node |
| `Kept`, `Pool`, `Journal` | nil | the copy handed out gets the proof; held change is released; journal entries are stamped |
| `Facade`, `Topic` | nil | where a proven transaction is published again; nil publishes nothing |
| `Retry` | empty | ends the note about a proof that could not be published with how to send it again |
| `Save` | nil | saves the application's state after a proof is recorded |
| `Note` | discard | receives each line |
| `Pending.What`, `Txid`, `RawHex`, `BeefHex` | none | the kept transaction and how the lines name it |
| `Pending.Proven` | nil | records the proof in the application's state |
| `Pending.Stamp`, `Seq` | false | stamp the journal entry (Seq, Txid) mined |
| `Pending.Refused`, `Unbuilt` | a generic line | report a refusal, or a kept copy that does not rebuild, in the application's words |

## Chain tracker: `headers.Client`

| Field | Default | |
|---|---|---|
| `Base` | none | the header read API of an [overlay-bridge](https://github.com/lightwebinc/overlay-bridge), with no path suffix; the client reads `/v1/tip` and `/v1/root/{height}` |
| `HTTP` | a client with no proxy | |
| `Timeout` | 10 s | used only when `HTTP` is nil |

`headers.New(base)` trims a trailing slash. Answers are bounded at 1 MiB.
Which header source a reader trusts is the security decision behind every
proof it checks, so choose one that received its headers itself.

## Hosts: `hostset.Client`

| Field | Default | |
|---|---|---|
| `Source` | `hostset.DNS{}` | where the hosts behind a base URL come from |
| `Policy` | `PolicyFirst` | `PolicyFirst` stops at the first success, `PolicyRandom` shuffles first, `PolicyAll` asks every host |
| `Quorum` | 1 (0 and 1 both mean one) | successful answers needed from distinct hosts, in policy order; more than the name resolves to is an error |
| `HTTP` | none | supplies TLS settings, the overall timeout and the redirect policy; its transport is cloned, never used directly |
| `Timeout` | 15 s | per attempt |
| `MaxBody` | `DefaultMaxBody` (64 MiB) | per response body |

Sources: `DNS{Resolver}` resolves the base URL's name to every A and AAAA
record (`Resolver` defaults to `net.DefaultResolver`, and an IP literal
yields itself); `Static{Bases}` is an explicit list, each dialed by its own
name. The client always disables keep-alives, takes no proxy, and, unless
`HTTP` sets its own, follows redirects only within the origin, at most five.

A failure under `PolicyFirst` is a connect error, a timeout, a body over the
bound or a 5xx; a 4xx is an answer.

## Lookup: `lookup.Query`

`Query(ctx, hs, base, q)` POSTs `q` to `<base>/lookup` on the hosts `hs`
selects. A nil `hs` is `&hostset.Client{}`: the DNS source and first
success. `Question.Service` names the lookup service and `Question.Query` is
marshalled as given. Only an `output-list` answer is accepted, and every host
asked must answer one.

## Discovery: `resolve`

`NewHTTPClient(timeout)` is the discovery policy in one client: system TLS
roots only, TLS 1.2 minimum, no proxy, redirects only within the origin (at
most five). A `timeout` of 0 or less means 15 s. `FetchManifest(ctx, c,
domain)` and `ResolveHandle(ctx, c, m, acct)` use `NewHTTPClient(0)` when `c`
is nil. Every answer is bounded at 256 KiB.

`FetchManifest` reads `https://<domain>/manifest.json` and nothing else.
`ResolveHandle` uses the resolve URL the manifest's `metanet.handles`
declares, or the well-known path `/.well-known/metanet-handles/resolve` at
the domain, and refuses a domain whose manifest has no `metanet.handles`
(`ErrNoHandles`) rather than probe. `ParseAcct` accepts `user@domain`,
`@user@domain` and `acct:user@domain`, and refuses a dotless ecosystem as an
alias.

## Pin file: `knownkeys`

| Parameter | |
|---|---|
| `path` | the application's; the library has no default location. `Load` of a missing file is an empty store |
| `header` | required by `Save`: one comment line naming the file and its version, such as `# example known_keys v1`, with no line break |

`Save` creates the directory at 0700 and writes the file at 0600 through a
synced temporary file and a rename. `Load` refuses a file that is group or
world writable (`ErrPermissions`) and a malformed line. The grammar, one
record per line in three forms (active, `@rotated-from`, `@retired`), is in
`knownkeys/testdata/known_keys.sample`.

## Verification: `verify`

`VerifyCarrier(ctx, items, want, what, spec, tracker)` needs every field of
`CarrierSpec`; a missing hook is `ERROR`, not a verdict.

| Field | |
|---|---|
| `Params` | the application's `carrier.Params`, with `ValidatePayload` set |
| `Classify` | the application's `carrier.Classify` |
| `IdentityOf` | returns the identity key bytes the payload names |
| `Expect` | the caller's own rules for the payload at the position asked for; a non-empty `Code` refuses before any header lookup |

`want` is the commitment asked for, in hash byte order, and `what` labels the
carrier in every reason. `Check(ctx, tx, tracker)` and `VerifyCarrier` both
refuse a nil `tracker` with `ErrNoTracker`.

## Proof guard: `guard.ParseBUMP`

`ParseBUMP(b, bound)` takes the caller's bound on the proof's size, in bytes;
a bound of zero or less admits nothing. `nodeapi` uses its 1 MiB body bound.

## Terminal text: `termsafe`

`Sanitize(s, Options{ANSI, ASCII})` bounds its output at `MaxLines` (200)
lines of `MaxCols` (512) columns. `ANSI` keeps colour and weight sequences
and nothing else, and ends coloured output with a reset; `ASCII` prints
every rune above 0x7E as `?`. `Validate(field, s)` and
`ValidateBounded(field, s)` refuse what `Sanitize` would strip, the second
within the same two bounds; every refusal matches `ErrUnsafe`.
`UTF8Locale(getenv)` reads `LC_ALL`, `LC_CTYPE` and `LANG` through the
lookup the application passes, so the package itself reads no environment.
