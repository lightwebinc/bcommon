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
| A record's bound, its last defined key and the meaning of each key | the arguments to `record.Decode` and the application's own reads | A reader accepts every record within the bound and preserves keys above the last; lowering a bound or re-using a key refuses or misreads records already published |
| The content key's domain strings | `keyed.DomainSymmetric`, `keyed.DomainCommitment` (constants, BRC-369 section 2) | Hashed into every symmetric key and commitment |
| The epoch-wrap domain string | the `domain` argument to `keyed.EpochWrapKey`, `WrapToEpoch` and `UnwrapFromEpoch`; the application's own, recorded in [registry.md](registry.md) | Hashed into every wrapping key; a reader derives another key and no wrap opens |
| The renderer filter's rules and Unicode version | `sanitize` (fixed: Unicode 15.1, `sanitize.UnicodeVersion`) | An application that holds its renderers to a corpus pins it to this table; a later version is a new table and a new corpus |
| Carrier nLockTime 4102444800 and input sequence 0 | `carrier.LockTime`, `carrier.Sequence` (constants) | A reader refuses a carrier with a lower locktime |
| RFC 6962 leaf and node prefixes, the store root rule | `commit`, `store.Root` (fixed) | Every reader recomputes roots with them |
| Canonical CBOR encoding | `cbor` (fixed) | A record's commitment covers its exact bytes |
| Fund-key derivation of a wallet | `bwallet.Profile.FundProtocol`, `FundKeyID` | Changing either strands every coin already paid to the key |
| Funding-tree state file shape | `funding.Tree` JSON tags | Part of the application's state file |
| A payee's record and a host's payment ledger | `payee.Book` and `payee.Payment` JSON tags, `payee.LedgerFile` | Part of the application's state file, and what a host has already written; a later ledger version adds keys and names itself in `"v"` |

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

`Derivation.Validate` applies go-sdk v1.7.1's BRC-43 rules when the value is
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
| `Fees.Rate` | none; `mint.DefaultFees` is `{100, 1000}` | `mint.Rate{Sats, Bytes}`: `Sats` satoshis per `Bytes` bytes of the signed transaction, exactly as miners publish it (ARC's `miningFee`; JSON `{"satoshis", "bytes"}`). The fee is `ceil(size * Sats / Bytes)` in integer arithmetic; a fee that overflows 64 bits, or `Sats` over zero `Bytes`, is `mint.ErrRate` |
| `Fees.SatPerByte` | none | the older whole-number rate, read only when `Rate` is zero, as `Rate{SatPerByte, 1}`. A `Fees` written before `Rate` existed means what it meant |
| `Fees.Floor` | none; `mint.DefaultFees` is 100 | the least fee a transaction pays, in satoshis |
| `Fees.Dust` | zero, which means `Floor` | the least change kept as an output; change below it goes to the fee. `mint.DefaultFees` sets 100 |
| `Fees.MaxRate` | none; `mint.DefaultFees` is `{100, 1000}` | caps `Rate` (and `SatPerByte`): a rate above it is lowered to it |
| `Fees.Max` | none | the most one transaction may pay in fee; a fee above it is refused with `mint.ErrFeeTooHigh` rather than paid |
| `Input{Tx, Vout, Unlocker}` | none | an input's source transaction, output index and the template that signs it. The template keeps key material out of `mint` |
| `change` | none; a nil script is `ErrNoChange` | where the fee input's remainder goes |

Three fee policies are named:

| Name | Rate | Floor | Dust | |
|---|---|---|---|---|
| `mint.DefaultFees` | `{100, 1000}`, capped there | 100 | 100 | the network's rate and never more; the floor is what 1000 bytes pay at that rate |
| `mint.NetworkFees` | `{100, 1000}` | 1 | 1 | the network's rate with no padding: a 225-byte payment pays 23 satoshis |
| `mint.LegacyFees` | `SatPerByte` 1 | 250 | 250 (from `Floor`) | `DefaultFees` until the network rate became the default; test vectors that pin fee amounts name it, so a change of default never moves a pinned byte |

`Fees.For(size)` is the fee for a signed size under the policy, and
`mint.ParseRate("100/1000")` reads a rate written as a flag (a bare whole
number is satoshis a byte). `Rate.Cmp` compares two rates by value and
`Rate.Clamp(lo, hi)` holds one to bounds.

`FundingTree(lock, count, sats, fee, change, fees)` builds `count` outputs of
`sats` each under `lock` (normally `carrier.FundingLock`), plus change.
`count` is 1 to `mint.MaxFundingOutputs` (1023), so that a sweep of every
funding output of a tree with one fee input stays within 1024 inputs; a
larger count is an error wrapping `mint.ErrTreeTooLarge`, returned before
anything is signed.
`Transition(lock, sats, prev, fee, change, fees)` takes a nil `prev` for a
create and the previous token (with its unlocker, which is required) for an
update. `Payment(ctx, dest, sats, fee, change, fees)` pays `dest`, normally a
BRC-29 destination from `bwallet`.

## Fee policy: `feepolicy`

A `feepolicy.Source` answers the `mint.Fees` a build uses now. `mint` itself
reaches no network; the policy fetch lives here.

- `feepolicy.Static(fees)` is a fixed policy, the default.
- `*feepolicy.ARC` is the policy a broadcaster publishes at
  `GET /v1/policy` (ARC, and arcade, which answers the same shape). It is
  opt-in, and its URLs follow the broadcaster a deployment settles through.

| `ARC` field | Default | |
|---|---|---|
| `URLs` | none | broadcaster URLs; a bare host is asked at `/v1/policy`, a URL with a path at that path plus `/policy` (so `https://arc.taal.com/v1` and `https://arcade.gorillapool.io` both work) |
| `Key` | none | a bearer token |
| `Base` | none | the fees returned with the policy's rate in place of their own: floor, dust and `Max`, and the rate used when no policy is at hand |
| `Min`, `Max` | `DefaultMinRate` `{100, 1000}`, `DefaultMaxRate` `{100, 1000}` | the policy's rate is raised to `Min` and lowered to `Max`; `Max` is also set as `Fees.MaxRate` |
| `TTL` | 5 minutes | one fetch per TTL, also after a failure |
| `Stale` | 24 hours | how long the last good answer is used while the endpoints fail; after it, `Base`'s rate |
| `Timeout` | 5 s | the fetch bound; a mint never waits longer on the policy |
| `Client`, `Note`, `Now` | no proxy; none; `time.Now` | |

An answer is held to `policy.miningFee` as whole numbers, satoshis at least
1 and bytes 1 to 1e9 (`ParsePolicy`, refusing as `ErrPolicy`): GorillaPool's
testnet ARC publishes 0 satoshis today, which is refused. Across several
URLs the highest rate is taken (a transaction must be accepted by whichever
is used) and the lowest stated `maxtxsizepolicy` and `maxscriptsizepolicy`
(`ARC.Policy`). `ARC.Fees` never fails on the network; `ARC.Status` says
what it used (`arc`, `cache` or `static`), how old, and the last error, for
a metric or a status line.

`feepolicy.Config` is the fee block of an application's configuration,
keys sorted:

```json
"fee": {
  "dust": 100,
  "floor": 100,
  "max_rate": {"bytes": 1000, "satoshis": 100},
  "max_tx": 0,
  "min_rate": {"bytes": 1000, "satoshis": 100},
  "policy_urls": ["https://arcade.gorillapool.io"],
  "rate": {"bytes": 1000, "satoshis": 100},
  "source": "static"
}
```

Every key is optional; `Config.Fees(defaults)` lays it over `defaults`
(usually `mint.DefaultFees`) and `Config.Build(defaults)` returns the
`Source`: `static` (the default) or `arc` (also `arcade`), which needs
`policy_urls`. `ParseConfig` refuses unknown keys. `MergeLegacy(c,
satPerByte, floor)` folds the older keys `fee_sat_per_byte` and
`fee_floor` in as `rate {fee_sat_per_byte, 1}` and `floor`, and refuses
either beside the new key it aliases.

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

## Application records: `record`

| Argument | Where | |
|---|---|---|
| `max` | `Decode`, `DecodeMap`, `Encode` | the record's bound in bytes, checked before the decoder runs and on what the encoder wrote; the application's, per record kind |
| `last` | `Decode`, `Split`, `CheckExtra`, `CheckExtraKeys` | the last key this version defines; keys above it are preserved in `Fields.Extra` |
| `magic` | `Decode`, `Fields.CheckMagic`, `Claims` | key 0's value, four bytes: the application's tag prefix, a letter and a version byte |
| `defined` | `CheckExtra` | how many defined pairs the record writes, so that they and the preserved ones are at most `MaxKeys` together |

`MaxKeys` (64) bounds the entries of every record map and is fixed. Each
reader takes the key's own bounds: `Uint(k, lo, hi)`, `BytesN(k, n)`,
`BytesRange(k, lo, hi)`, `Array(k, max)`, `List32(k, max)`.

## Chain tokens: `chaintoken`

`ReadWire(b, bound)` and `Stored(b, bound)` take the host's BEEF bound in
bytes; zero means `DefaultMaxBEEF`, 256 KiB, which is also the floor a host
should keep its own bound at or above, so that every host admits what any
conforming publisher sends. `ReadOutput(o, vout, nfields)` takes the number
of fields before the signature the application's token carries. The tag,
the record codec, the derivation (`pushdrop.Derivation`) and the rules a
token keeps against its predecessor are the application's; `Mined` takes
the host's own header source.

## Payments: `purse.Purse`

| Field | Type | Default | |
|---|---|---|---|
| `Embedded` | `*bwallet.Embedded` | none | the wallet: identity key and coin pool |
| `NewPayer` | `func() *producer.Payer` | none | a payer over the wallet's pool, for a payment's fee coin and change |
| `Fees` | `mint.Fees` | none | the payment's fee policy |
| `MaxPay` | `uint64` | 0 | the most one `CreateAction` pays, in satoshis; zero pays nothing |
| `Settler` | `publish.Settler` | none | the leg an internalized payment is broadcast on |
| `Asset` | `*nodeapi.Asset` | none | the node a payment's proof is read from, and its inputs checked against |
| `Chain` | `nodeapi.Chain` | none | the same in `Asset`'s place, from a chain view that needs no node |
| `Headers` | `chaintracker.ChainTracker` | none | what an incoming payment is verified against before anything is broadcast; `Check` refuses without one |
| `Wait` | `time.Duration` | `producer.DefaultTimeout` | how long `Await` waits for a payment to mine |
| `Poll` | `time.Duration` | `producer.DefaultPoll` | how often it asks |

`CreateAction` pays exactly one P2PKH output funded by this wallet and
refuses any other shape (`ErrRefusedAction`). After the question is
answered the caller calls `Settle` when the host accepted a payment, which
keeps the last one made and refunds the rest, or `Refund` when it did not.
The refusals an application words itself are sentinels: `ErrOverMaxPay` (the
application names the setting that raises the cap), `ErrNoNode` and
`ErrNoSettler` (it names the settings that supply them) and `ErrNotMined`
(it says what to run again). A payment the network will never mine is a
`*RefusedError`.

## Payee: `payee`

### `payee.Settler`

| Field | Type | Default | |
|---|---|---|---|
| `App` | `string` | none, refused when empty | the application's name: a payment is internalized with the description `<App> priced question <class>` and the labels `<App>` and `payee` |
| `Payer` | `payee.Payer` | none, refused when nil | the payee's `*purse.Purse`, set up for the payee leg (`Settler`, `Asset`, `Headers`, `Wait`, `Poll`) |
| `Record` | `payee.Record` | none, refused when nil | what is settled and what was refused, kept across runs; `payee.Saved` makes one of a `payee.Book` and the application's save |
| `Pool` | `payee.Pool` | none: the report shows an empty pool | the payee's `*bwallet.Pool`, read for the report |
| `InFlight` | `int` | `DefaultInFlight` (16) | payments broadcast and not yet mined at once, 1 to `MaxInFlight` (64); outside that, `Settle` refuses to run |
| `Words` | `func(error) error` | `payee.Words` | the application's wording of the purse's refusals: which setting names the node or the leg, what to run again |
| `Out`, `Warn` | `io.Writer` | none: nothing is written | a line for each payment settled and the report; a line for each payment not settled, refused, or (from `ReadLedgers`) skipped |

`Settle` returns an error only when the run cannot go on: a `Settler` not set
up, or a `Record` that does not persist. A payment not settled, a context
that ended among them, is counted in the `Report`, named on `Warn`, and left for the next run;
`Report.Problem` is the line an application ends such a run with, under its
refusal status. A run is idempotent: a payment the record holds as settled
or refused is counted and not touched, so the command is safe on a timer.
Two runs over one home at once are the application's to prevent, as for any
command that writes its state.

### The ledger

| Name | Value | |
|---|---|---|
| `LedgerFile` | `payments.jsonl` | the ledger's name in a host's state directory |
| `LedgerV1` | 1 | `txid`, `beef`, `outputIndex`, `satoshis`, `derivationPrefix`, `derivationSuffix`, `senderIdentityKey`, `class` |
| `LedgerV2` | 2 | adds `inputs` (every outpoint the payment spends, `<txid>.<index>`) and `at` (Unix seconds) |
| `LedgerVersion` | `LedgerV2` | the latest this package reads, and the one `Payment.Line` writes |
| `MaxLine` | 16 MiB | the longest line read |

A line names no version up to `LedgerV2` and is told apart by its shape. A
later version writes its number in `"v"`; `Settle` reports such a line as not
settled (`ErrNewerLedger`) and leaves it in the ledger for a reader that
knows it. A line that does not parse, one cut short by a crash, is skipped:
its question was never answered.

### The payee key and the home

The payee's home is a `bwallet` directory: its root key in `identity.json`
(`payee.IdentityFile`) is the payee key, and its pool is where settled
payments go. `HomeKey(dir, app)` reads the key; `app` names the command that
makes a home, in the error for a directory that is not one. `KeyEnv(app)` is
the host's variable, `<APP>_PAYEE_KEY`; `KeyLine` is the line that sets it;
`CreateKeyFile` writes that line to a new file at mode 0600 and never over
one (`ErrKeyFileExists`), and returns what the command says when it has.
The flags that choose a file or standard output are the application's.

## Payment acceptance: `acceptance`

### `acceptance.Policy`

The zero `Policy` holds every payment for its block. `DefaultPolicy()` is
the recommended starting point.

| Field | Type | Default (`DefaultPolicy`) | |
|---|---|---|---|
| `ThresholdSats` | `uint64` | 25,000,000 | the largest payment the fast path takes; inclusive |
| `ThresholdCents` | `uint64` | 0 | the threshold in US cents, converted through `Price`; with `ThresholdSats` set too the smaller applies |
| `Price` | `PriceSource` | none | the price a cents threshold converts through; any error, a zero price or one older than `MaxPriceAge` makes the threshold zero |
| `MaxPriceAge` | `time.Duration` | 1h | how old a dated price may be; an undated price (`StaticPrice`) never goes stale |
| `Window` | `time.Duration` | 1h | how long a fast payment counts against its payer and the total unless it mines first; zero counts it until it mines or is released |
| `PayerLimit` | `uint64` | 25,000,000 | satoshis one payer may have on the fast path and unmined within `Window`; zero allows nothing |
| `TotalLimit` | `uint64` | 250,000,000 | the same across every payer; zero allows nothing |
| `Agree` | `int` | 0 (one) | how many status sources must answer a fast payment accepted |
| `Wait` | `time.Duration` | 10s | how long the fast path waits for that acceptance |
| `Watch` | `time.Duration` | 0 | how long it keeps watching for a conflict before it answers fast |
| `Poll` | `time.Duration` | 500ms | the pace of both, and of `Confirm` |

`DefaultThresholdSats` is 25 US dollars at 100 US dollars a coin: below that
price it is worth less than 25 dollars, which is the safe side. Review it
against the price on a schedule, or set `ThresholdCents` with a price source
the application trusts. The library names no live price service;
`StaticPrice` is a fixed price and `CachedPrice` asks a source at most once
per `TTL`, answering its last good price with that price's own date, so
`MaxPriceAge` still ends it.

### `acceptance.Verifier`

| Field | Type | Default | |
|---|---|---|---|
| `Policy` | `acceptance.Policy` | zero, holds everything | the rule |
| `Exposure` | `*acceptance.Exposure` | none: nothing is fast | what the fast path has taken and not seen mined, and the flagged payers; `NewExposure` |
| `Headers` | `chaintracker.ChainTracker` | none, refused | the receiver's own headers: SPV and every proof |
| `Settler` | `publish.Settler` | none: nothing is fast | the receiver's broadcast leg |
| `Status` | `[]acceptance.StatusSource` | none: nothing is fast | broadcasters asked for the network's verdict (`*publish.Arcade`) |
| `Spends` | `acceptance.SpendView` | none, not asked | the node's spend view (`*nodeapi.Asset`); when set it must answer for every input |
| `Proofs` | `acceptance.ProofSource` | none | where `Confirm` and `Monitor` read a proof (`*nodeapi.Asset`) |

A `Payment` names the transaction (read from its BEEF through `guard`), the
payer it is charged to (an identity key; payers that give none share one
charge), the `Output`s it must hold (the scripts the receiver derived), and
the payer's `Ask`. A payer may ask to be held; asking to be fast changes
nothing.

### `acceptance.Monitor`

| Field | Type | Default | |
|---|---|---|---|
| `Verifier` | `*acceptance.Verifier` | none | the sources and the `Exposure` it releases and flags in |
| `MaxAge` | `time.Duration` | 0, never | how long a fast payment may stay unmined before it is reported `Unmined` |
| `Hook` | `func(acceptance.Event)` | none | each `Confirmed`, `DoubleSpent`, `Refused` or `Unmined` payment |

## Test chain: `testchain.Chain`

`New(start)` is a chain whose tip is at height `start`. `Hold` keeps
accepted transactions unmined until `Mine`; `HoldIf` holds only those it
answers true for; `Refuse` refuses a transaction with the reason it
returns; `Busy` answers 503 to the RPC and the broadcaster. `Maturity` (100)
and `CoinbaseValue` are fixed. It is for tests and local trials only.

`Send` takes a transaction the chain already holds again, with nothing done
and no error. A node's `sendrawtransaction` may instead refuse one it
already has, and `RefuseKnown` (off by default; `SetRefuseKnown` while the
chain serves) models that: `Send`, and so the RPC and the ingress, refuse
such a transaction with `ErrAlreadyKnown`, whose text is
`txn-already-known`. The broadcaster under `/arcade` answers the status of
a transaction it holds either way, as an ARC-compatible broadcaster does.

`SpendElsewhere(txid, vout, by)` marks an output spent by a transaction the
chain took from someone else. A transaction the chain already holds that
spends the same output is displaced: the chain keeps it and goes on serving
it under `/api/v1/tx/`, as a node may, while the UTXO view names `by` as the
spender. That is the case `producer.Trees.Recover` answers `CoinSpent` for.
The chain does not drop a displaced transaction from the ones `Mine` mines,
so a test of that case does not call `Mine` afterwards.

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

### Funding on mainnet and testnet

Users hold no coinbase, so on mainnet and testnet the pool is funded from a
payment the user sends from their own wallet to `FundAddress(mainnet)`, and
the application imports it once, checked:

- `ImportBEEF(ctx, beef, fund, headers, ImportOptions)` takes the payment
  as the BEEF (or Atomic BEEF) the user's wallet hands over, with no
  lookup. A mined payment's proof must verify against `headers`. An unmined
  one is accepted by default (`ImportOptions.RefuseUnmined` refuses it) when
  it is final and every parent carries a proof `headers` hold
  (`ErrUnprovenParent` otherwise), and its scripts verify against those
  parents.
- `ImportTxid(ctx, txid, fund, chain, headers)` fetches a mined payment and
  its proof from a chain view (`nodeapi.ParseChain`, a node, WhatsOnChain)
  and checks the proof; a payment not mined yet is `ErrUnmined`. When the
  view also answers spends (every one `ParseChain` builds does), each output
  paying `fund` is checked for a spend first: one shown spent is left out,
  every one spent is `ErrSpent`, and one the view cannot answer for
  (`nodeapi.ErrSpendUnknown`) fails the import. With no node, "unspent" is
  WhatsOnChain's word and is trusted: no proof of absence exists, while a
  proof and a named spender are checked.

Either returns an `*Import`: the outputs paying `fund` as `[]Output` for
`Pool.Add` (`ErrPaysNothing` when there are none), their total, and whether
the payment is mined and at what height. A mined payment's outputs carry
`Raw`, `Bump` and `Height` and are spendable at once (they are not
`Coinbase`). An unmined payment's outputs are `Unproven`, held back until a
`producer.Collector` with `Pool` set collects the proof by txid (its
`Proofs` needs a source that knows a transaction the wallet broadcast, such
as `Source` set to the chain view), and `Import.BeefHex` is the BEEF to keep
for it meanwhile, from which a `producer.Kept` loader rebuilds it
(`funding.Rebuild`) should `Allow` let it pay a fee before then.
Alternatively a BRC-100 wallet reached through
`wirewallet.Dial` signs as the `Signer.Interface` and pays for each funding
tree through `producer.Trees.Fund`. Both are worked through in
[examples.md](examples.md#fund-a-wallet-on-mainnet-or-testnet).

### Coinbase funding (regtest only)

Coinbase: only on a regtest chain you run (development and tests).

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
| `Client` | 60 s timeout, no proxy, TLS 1.2 minimum | long because `generatetoaddress` (coinbase: only on a regtest chain you run) mines inline; the lever for a slow node is the batch size |

### `nodeapi.Asset`

| Field | Default | |
|---|---|---|
| `Base` | none | the asset API root with no path suffix, for example `http://node.example.com:20090`; requests go to `<Base>/api/v1/...` |
| `Client` | 30 s timeout, no proxy, TLS 1.2 minimum | |

`WaitMined(ctx, asset, txid, poll)` polls every `poll`; 0 means 2 s.
`WaitSettled(ctx, asset, tx, poll)` is the same wait for a transaction the
caller holds, and returns a `*SpentError` (`ErrDoubleSpent`) as soon as the
node shows one of its inputs spent by another transaction (`Spender`,
`SpentElsewhere`, read from `/api/v1/utxos/<txid>/json`). Every
response body is bounded at 1 MiB, and a 429 from either API is retried after
200 ms, 1 s and 3 s. `ProofFor(raw, txid)` holds a supplied proof to the same
bound.

`Spender(ctx, txid, vout)` answers only on evidence. Every other answer is
an error wrapping `ErrSpendUnknown`, never `""`:

| The node's answer for the output | `Spender` |
|---|---|
| status `OK` | `""`, nil: unspent |
| status `SPENT` naming a spender | the spender's txid, nil |
| status `SPENT` naming no spender, or one that is not a txid | `ErrSpendUnknown` |
| status `NOT_FOUND`, which a node answers for an output it has pruned or could not read, as well as one it never stored | `ErrSpendUnknown` |
| status `IMMATURE`, `FROZEN`, `CONFLICTING`, `LOCKED`, empty, or any other, even with a spender named | `ErrSpendUnknown` |
| the output missing from the answer, or named twice | `ErrSpendUnknown` |
| a 404 for the transaction, which a node also answers for a fully spent transaction it has pruned | `ErrSpendUnknown`, and `IsHTTP(err, 404)` |
| an answer that does not decode, or is for another transaction, or a failed read | `ErrSpendUnknown` |

A caller that acts on "unspent", such as putting a coin back in a pool,
acts only on `""` with a nil error, and treats an error as undecided: it
changes nothing and asks again later. `SpentElsewhere` refuses only on a
`SPENT` naming another transaction and passes over every input `Spender`
cannot answer for, so its nil says no input is shown spent elsewhere, not
that every input is unspent.

### Chain views: `nodeapi` interfaces

What an application reads from the chain is four narrow views, each an
interface that `*nodeapi.Asset` (a node), `*nodeapi.WoC` (WhatsOnChain) and
`*nodeapi.Sources` satisfy:

| Interface | Method | Answers |
|---|---|---|
| `TxSource` | `TxRaw(ctx, txid)` | the raw transaction; not held is an error for which `IsNotFound` is true (`ErrTxNotFound`, or a 404) |
| `ProofSource` | `Proof(ctx, txid)` | the proof and height, or `ErrNotMined` (not mined, or not known); `*publish.Arcade` is one too, for what it was sent |
| `SpendSource` | `Spender(ctx, txid, vout)` | the spender, `""` and nil only for unspent, `ErrSpendUnknown` otherwise |
| `KnownSource` | `Known(ctx, txid)` | whether the source holds the transaction at all, mined or not; `*publish.Arcade` too |

`Chain` is `TxSource`, `ProofSource` and `SpendSource` together, the view
`producer.Payer.Chain` and `purse.Purse.Chain` take in place of `Asset`.
`SpentElsewhereIn(ctx, spends, tx)`, `WaitMinedOn(ctx, proofs, txid, poll)`
and `WaitSettledOn(ctx, proofs, spends, tx, poll)` are `SpentElsewhere`,
`WaitMined` and `WaitSettled` over any view. `Checked{Source, Headers}` is
a `ProofSource` whose every proof must name the txid and verify against the
caller's headers (`CheckProof`), or is refused as `ErrProofRefused`.

`ParseChain(spec, ChainOptions)` builds a `*Sources` from a specification,
in the style of `headers.Parse`: comma-separated backends, each optionally
qualified by the one method it serves (`tx=`, `proof=`, `spend=`,
`known=`).

| Specification | |
|---|---|
| `woc:main`, `woc:test` | WhatsOnChain, no node |
| `asset:http://node:8090` | a node for everything |
| `asset:http://node:8090,woc:main` | a node, WhatsOnChain for transactions and proofs the node lacks |
| `woc:test,spend=asset:http://node:8090` | WhatsOnChain, with a node's spend view |

Transactions and proofs are asked of every backend that serves them, in
order, since each answer is checked; "spent", "unspent" and "known" come
from one backend only, the first that serves them, so an absence answer is
never a fall-through. `ChainOptions.Headers` is required, and every proof
is wrapped in `Checked` against it. `ChainOptions.WoCKey` and `WoCRate` are
the WhatsOnChain API key and its rate; `Client` is every backend's HTTP
client.

### `nodeapi.WoC`

| Field | Default | |
|---|---|---|
| `Network` | none | `main` or `test` (`NewWoC` refuses anything else) |
| `Base` | `https://api.whatsonchain.com/v1/bsv/<Network>` | a mirror, or a test server |
| `Key` | none | sent as the `Authorization` header, as WhatsOnChain documents for an API key |
| `Rate` | `WoCFreeRate`, 3 a second | requests a second, shared by every call on the value; a 429 is retried after 200 ms, 1 s and 3 s (or `Retry-After`) |
| `MaxTx` | `guard.DefaultBound` | the largest transaction read |
| `Client` | 30 s timeout, no proxy | |

| Method | Endpoint | |
|---|---|---|
| `TxRaw` | `/tx/<txid>/hex` | must hash to the txid; a 404 is `ErrTxNotFound` |
| `TxBEEF` | `/tx/<txid>/beef` | served but undocumented; a 422 (it declines some unmined transactions) is `ErrNotMined`, a 404 or its 500 "No such mempool or blockchain transaction" is `ErrTxNotFound` |
| `Proof` | `/tx/<txid>/beef`, else `/tx/<txid>/proof/tsc` | the BEEF's proof; when that endpoint fails otherwise, the TSC proof converted to a BUMP (`TSCProof.MerklePath`) at the height `/block/<hash>/header` names |
| `Spender` | `/tx/<txid>/<vout>/spent` | 200 names the spender (mined or not); 404 is unspent, WhatsOnChain's "known but spent details are not found"; 400 is its "UTXO is unknown" (also its answer for an unspendable output) and is `ErrSpendUnknown`, never unspent |
| `Known` | `/tx/hash/<txid>` | 200 is known, 404 is not |

GorillaPool's ordinals `spends` endpoint answers an empty 200 for an
unspent output and for one that does not exist alike, so no backend here
reads it, and nothing reads it as evidence of "unspent".

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
| `Arcade.Asset` | nil | the node arcade's acceptance is held to: an input it shows spent by another transaction is a refusal wrapping `nodeapi.ErrDoubleSpent`, whatever arcade answered |
| `Arcade.Spends` | nil | the same, from any spend view in `Asset`'s place (`nodeapi.WoC`, a `nodeapi.Sources`), so the accepted-but-spent case stays refused with no node |
| `Facade.Base` | none | the overlay host's root; the BEEF goes to `<Base>/submit` |
| `Facade.HTTP` | 30 s timeout, no proxy | |
| `Journal.Dir` | none | one `<seq>-<txid>.json` file per transition attempt, mode 0600, created on first write |

`Facade.Submit(ctx, topic, beef)` takes exactly one topic name per call. It
refuses a body without a BEEF marker (`ErrNotBEEF`) and a topic that is empty
or contains a comma, space, tab, CR or LF, both before sending anything.
The facade's answer is refused over 1 MiB; arcade's answers are read to at
most 64 KiB (submit) and 1 MiB (status) before they are parsed.

`Arcade.Proof(ctx, txid)` is arcade as a `nodeapi.ProofSource` for what it
was sent: the merkle path once mined, held to the BUMP guard, the txid and
the height arcade reports; not mined, or a transaction arcade does not hold
(`ErrArcadeUnknown`), is `nodeapi.ErrNotMined`, so a caller asks another
source; a refusal is `ErrArcadeRefused`. `Arcade.Known(ctx, txid)` is
arcade as a `nodeapi.KnownSource`.

`ParseSettler(spec, SettleOptions)` builds the settlement leg from a
specification, and returns the `*Arcade` when the leg is one:

| Specification | Leg |
|---|---|
| `arcade:main`, `arcade:test` | GorillaPool's public arcade, `ArcadeMainnet` (`https://arcade.gorillapool.io`) or `ArcadeTestnet` (`https://testnet.arcade.gorillapool.io`): the default broadcaster, no key |
| `arcade:https://host` | any arcade installation, your own included |
| `arc:https://host` | an ARC installation, opt-in: `/v1` is added to a URL with no path, so `arc:https://arc.gorillapool.io` and `arc:https://arc.taal.com` (which needs a `Key`) both work |
| `rpc:http://node:port` | a node's `sendrawtransaction` (`RPCSettler`) |
| `tcp:host:port` | a fabric ingress, bare EF, no answer (`TCPIngress`) |

`DefaultSettle(network)` is `arcade:main` or `arcade:test`; a regtest chain
has no default. `SettleOptions` carries what a specification does not:
`Key`, `RPCUser`, `RPCPass` and `RPCID`, `Client`, `Spends` and `Asset`
(the views arcade's verdict is held to), and `Note`.

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
| `Asset` | none | the node a proof is waited for from and where a coin's parent is fetched: with `Async`, any parent not held with its proof; otherwise one the pool holds no bytes of (a coinbase). With none, such a parent is a placeholder that signs but that `funding.BEEF` and `funding.KeepBEEF` refuse to write (`funding.ErrPlaceholder`) |
| `Chain` | none | a `nodeapi.Chain` asked everything `Asset` is asked, in its place: a chain view that needs no node (`nodeapi.ParseChain`, WhatsOnChain with every proof checked against the producer's headers). `Trees.Recover` asks it too |
| `Async` | false | settle on the leg's acceptance and collect the proof later |
| `Fees` | the zero policy | fees for what the Payer mints itself (a funding tree); `mint.DefaultFees` is the usual one, and zero pays no fee. A `feepolicy.Source` answers it per run for a live rate |
| `Timeout` | `DefaultTimeout` (10 min) | how long a wait for a proof lasts |
| `Poll` | `DefaultPoll` (5 s) | how often the wait asks |
| `Note` | discard | receives each progress line |

`Take` returns `*producer.NoCoinError` when the pool has no coin it may
spend (`Held` counts the transactions whose change is waiting for a proof,
`Minting` the trees minted ahead that still hold a coin; see below) and
`*producer.NoKeyError` for a coin none of the keys holds, so the
application can say what its users should do.

#### A fee while a tree is minted ahead

A tree minted ahead (`Trees.Ahead`) takes its coin when the mint starts,
and its change reaches the pool only when the mint is collected. A wallet
with one coin has none in between. A take the pool cannot cover in that
time waits for the mint instead of failing:

| | |
|---|---|
| Which calls wait | `Payer.Take`, `Payer.TakeAtLeast`, and the tree `Trees.Spend` mints on demand, on any `Payer` over the same `*bwallet.Pool` the mint took its coin from |
| When | only when the pool has no coin for the take and a pool-paid mint ahead is uncollected; a take the pool can cover answers at once, as before |
| What they do | wait for the mint, collect it exactly as `Trees.Wait` does (its notes go to the `Note` of the `Trees`' `Payer`, its change into the pool, or its unspent coin back), and take again |
| For how long | until the mint ends, the context ends, or the `Timeout` of the `Payer` the take was called on has passed (`DefaultTimeout`, 10 min, when zero), whichever is first; several mints, by several `Trees` over one pool, are collected one at a time under that one bound, with a take after each |
| What never waits | a mint ahead looking for its own coin, which is skipped with a note and minted when needed; a tree paid through `Fund`, which takes no pool coin |

What the collected mint leaves decides the second take:

- a tree that settled with its proof leaves proven change, and the take is
  paid from it;
- with `Async` on the `Trees`' `Payer` the change is unproven until its
  block, so the take answers `*NoCoinError` with `Held` counting it, unless
  `Allow` lets it through: the wait for a block an application already
  handles;
- a mint that failed before the settlement leg returns its coin, and the
  take is paid from it; one that failed after it leaves nothing, and the
  take answers `*NoCoinError`;
- when the wait ends first, the mint is still in flight and the
  `*NoCoinError` counts it in `Minting`. A later take waits again.

The take collects the mint on the goroutine that called it, so every
`Payer` and `Trees` over one pool belongs to one goroutine, as the package
already requires of a `Payer` and its `Trees`. Collecting calls the `Note`
of the `Trees`' `Payer` and saves the pool, and nothing else of the
application's (no `TreeState`, no `Prepare`, no leg), and takes no coin
itself, so a take inside a `Spend`, or under a lock the application holds
around its own state, cannot wait on itself. An application that called
`Trees.Wait` before each fee to get this no longer needs to; `Wait` is
still how an ending run collects the mint.

#### A refusal for a coin already spent

A transaction whose fee coin another transaction has spent never mines,
and the coin must not go back to the pool, where it would be handed out
again to fail the next build. When the settlement leg refuses a transaction
at `Submit`, or the node's view refuses it afterwards (`producer.ErrRefused`
with `nodeapi.ErrDoubleSpent`), the coin is dropped, not returned, when
either:

- the refusal names another transaction as the coin's spender, a
  `*nodeapi.SpentError` for its outpoint, as `publish.Arcade` with `Asset`
  set answers; or
- `Payer.Asset`, when set, shows the coin spent by a transaction other than
  the one refused.

Dropped means forgotten by the `Payer` and taken out of the pool
(`bwallet.Pool.Remove`), with a note naming the spender. A refusal for any
other reason, a coin the node shows unspent or spent by the refused
transaction itself, and a coin the node cannot answer for
(`nodeapi.ErrSpendUnknown`) keep the coin as before: a coin is dropped only
on a spender named.

| Transaction | Where | The coin when it is not spent elsewhere |
|---|---|---|
| a funding tree `Trees.Spend` mints on demand | when `Spend` returns the refusal | back in the pool before `Spend` returns |
| a funding tree minted ahead | when the mint is collected (`Spend`, `Wait`, or a take that waits) | back in the pool at that collection |
| any transaction paid with `Take` | in `Payer.Settle`, `SettleAndWait` and `Await`, for each coin that `Payer`'s `Take` reserved and the transaction spends | still reserved, and returned by the caller's `GiveBack` |

A carrier pays no fee from the pool, since it spends a funding tree's
output, so it has no coin to settle here. A dropped tree's `Prepare` record
is left alone; `Recover` answers `CoinSpent` for it on the next start.

### `producer.Trees`

| Field | Default | |
|---|---|---|
| `Payer`, `State` | none | the Payer that pays for a tree, and the application's `TreeState` (`Current`, `Adopt`) |
| `Identity` | none | identity key hex whose key locks the tree; a current tree of another identity is replaced |
| `Count`, `Sats` | none | outputs in a new tree (at least; a larger spend gets a larger tree, up to `mint.MaxFundingOutputs`) and the value of each |
| `Funder` | empty | recorded in `funding.Tree.Funder` |
| `Lock`, `Change` | none | the funding lock and the change script, asked only when the pool pays |
| `Fund` | nil | mints and settles a tree some other way, such as a funding wallet |
| `DryRun` | false | build a pool-paid tree and record, settle and publish nothing |
| `Facade`, `Topic` | none | the object leg a new tree is published on |
| `Ahead` | 0, off | outputs left on the current tree at or below which the next tree is minted and settled in the background; see below |
| `Prepare` | nil, off | called with each pool-paid tree's record and fee coin before the tree reaches the settlement leg, so a run that stops before `Adopt` can be recovered; see below |

With `Ahead` above zero, and not `DryRun`, a spend that leaves the current
tree with `Ahead` outputs or fewer starts minting the next one, of `Count`
outputs, once per tree and with at most one mint in flight. A pool-paid tree
takes a proven coin and is signed on the caller's goroutine, so the
reservation never races another `Take`; only the settlement, or `Fund` when
it is set, runs in the background, under the context of the `Spend` that
started it, so pass the producer's run context. `Wait` collects the result,
as every `Spend` also does without waiting: the held notes go to `Note`, the
change goes into the pool, and `Prepared` answers the tree's record. Until
then the coin the mint took is out of the pool, and a fee the pool cannot
pay meanwhile waits for the mint: see
[a fee while a tree is minted ahead](#a-fee-while-a-tree-is-minted-ahead).
When the current tree cannot cover a spend, `Spend` waits for a mint in flight
and switches to the tree minted ahead, which is adopted and then published
exactly as a new tree is; a mint that failed is reported, its unspent coin
goes back to the pool (or is dropped when another transaction spent it; see
[a refusal for a coin already spent](#a-refusal-for-a-coin-already-spent)),
and the tree is minted on demand. The tree minted ahead is in no state until the switch, so
a crash before it strands its funding outputs (its change is already in the
pool); an application that wants a sweep to take them records `Prepared` in
its own history.

#### `Prepare` and `Recover`

A tree's fee coin leaves the pool, which is saved without it, when the tree
is signed, and the tree is adopted only once it has settled; a tree minted
ahead is adopted later still, at the switch. A run that stops in between
leaves a tree that may be on the chain and that the state does not record:
the coin, the tree's change and its outputs are lost to the application.

With `Prepare` set, `Spend` calls it for every tree the pool pays for, once
the tree is signed and before it reaches the settlement leg, on the
goroutine that called `Spend`; for a tree minted ahead that is before the
background half starts. It receives:

- the tree's `funding.Tree` record: `Txid`, `RawHex`, `Count`, `Sats`,
  `IdentityKeyHex` and `Funder`, with no proof, height or kept BEEF yet;
- the fee coin, a `bwallet.Output`, as it was taken from the pool.

The application saves both in its state before it returns. An error aborts
the mint: the coin goes back to the pool and nothing reaches the leg. Its
`Adopt` drops the record of the tree it is given, in the same save. With
`Prepare` nil nothing changes. It is not called for a `DryRun`.

On the next start, before the first `Spend`, the application calls
`Recover(ctx, tree, coin)` for each record still in its state. `Recover`
asks the Payer's `Asset` whether the node serves the tree, then, when it
does, for the tree's proof, and then, for a tree with no proof, which
transaction spent the coin. A proof settles it: a mined tree is on the
chain whatever else the node shows. Without a proof the coin's spender
decides, whether or not the node serves the tree. `Recover` answers one of
four outcomes:

| Outcome | What the node showed | What `Recover` did | The application's record |
|---|---|---|---|
| `TreeAdopted` | the tree has a proof, or the coin is spent by the tree, or the node serves the tree and shows the coin unspent | took the fee coin out of the pool if it was there, took the proof as `Settle` would, took the tree's unspent change into the pool, adopted and published the tree | dropped by `Adopt` |
| `TreeHeld` | the same, while the current tree is locked to `Identity` and has outputs left | took the fee coin out of the pool if it was there, took the proof and the unspent change, and holds the tree behind any tree already held (`Held` answers them in order, `Prepared` the first) until a spend switches to it | kept until `Adopt` names its txid, so a later start recovers it again |
| `CoinReturned` | it does not know the tree, and the coin is unspent | put the coin back in the pool | keep it, and recover it again on the next start; see below |
| `CoinSpent` | the tree has no proof, and the coin is spent by another transaction, `Recovery.By`, whether or not the node serves the tree | took the coin out of the pool if it was there; adopted, held and published nothing | drop it |

An error with no outcome decides nothing about the tree: the node could not
answer, or the tree did not mine within `Timeout`. A status that is no
evidence (`nodeapi.ErrSpendUnknown`, such as `NOT_FOUND` for an output the
node has pruned) is such an error, for the fee coin and for the tree's
change alike: it never reads as unspent. The record stays and the
next start asks again. A node that serves an unproven tree and cannot say
who spent its coin is such an error with `Async`, so that no tree is adopted
without a proof or the node's word on its coin; without `Async` the wait
for the proof decides, and adopts only a tree that mines. The fee coin of a
tree the node knows is out of the
pool even then, since the node holds it spent. `Recover` may be repeated
for the same record: a coin the pool already holds is not added twice,
change the node shows spent is not taken, and a tree that is already the
current one, or already held, is answered `TreeAdopted` or `TreeHeld` with
nothing done.

Any number of trees are held. Each tree `Recover` finds on the chain while
the current tree has outputs left goes behind the ones already held, in the
order `Recover` was asked. When the current tree cannot cover a spend,
`Spend` switches to the first held tree that is large enough for it, which
is adopted and published at that switch; one too small keeps its place. So
a second record found on the chain strands nothing: the current tree is
spent first, then each held tree in turn. `Held` answers the held records
in order, and no tree is minted ahead while one is held.

A record also outlives a mint that failed after `Prepare`, where `Spend`
already put the coin back; `Recover` then answers `CoinReturned`, or
`CoinSpent` once the coin has paid for something else. Do not drop a record
on `Spend`'s error, which may come after the tree reached the leg.

#### A tree that lost a double spend

A node may go on serving a transaction that can no longer mine: one it
took, unmined, whose input another transaction then spent. A tree in that
state has no proof and never gets one, and the node's UTXO view names the
other transaction as the spender of its fee coin. `Recover` answers
`CoinSpent` for it, with that transaction in `Recovery.By`: the coin is
taken out of the pool, in memory and on disk, and nothing is adopted, held
or published. The answer is the same with `Async` and without, when the
coin is spent while `Recover` waits for the tree's proof, and on every
later `Recover`. The application drops the record, as for any `CoinSpent`.

Before v0.6.3 the node serving the tree was taken as the tree being on the
chain. Without `Async`, `Recover` then returned an error wrapping
`producer.ErrRefused` and `nodeapi.ErrDoubleSpent` with no outcome, on
every start, so an application that keeps its record on an error kept it
for ever. With `Async` the tree was adopted with no proof and no question
about its coin. An application that adopted such a tree under v0.6.2 holds
a current tree that never mines. A `producer.Collector` given the tree as a
`Pending` with its `RawHex` reports it through `Refused`.

#### A publish that fails after `Adopt`

`Adopt` is called before the tree is published, so that a crash after the
publish never leaves a tree on the plane that the state does not know. The
publish can therefore fail with the tree already adopted. `Spend` and
`Recover` then return a `*producer.PublishError`, which `errors.Is` matches
with `producer.ErrPublish`, which carries the adopted record in `Tree`, and
which unwraps to the cause. `Recover` returns it together with the
`TreeAdopted` outcome and `Recovery.Tree`; `Spend` returns it with no tree.
Either way the tree is the current tree and its `Prepare` record is
dropped, so nothing is left to mint, settle or recover. Only the publish is
repeated:

```go
got, err := trees.Recover(ctx, rec.Tree, rec.Coin)
if errors.Is(err, producer.ErrPublish) {
	err = trees.Publish(ctx, got.Tree)
}
```

`Publish(ctx, tree)` publishes a tree the state already holds, rebuilt from
its record, on `Facade` and `Topic`. It needs only the record, so an
application that cannot repeat the publish in the same run notes the tree
in its state and calls `Publish` on a later start. A host that already
holds the tree answers a duplicate, which is not an error. Calling `Spend`
or `Recover` again does not publish the tree: it is the current one by
then, and both answer it as it is. Any other error from `Spend` or
`Recover` leaves no tree adopted.

#### Keep a `CoinReturned` record

The node's view is a moment's view. A tree handed to the leg an instant
before the run stopped may not have reached the node when `Recover` asks,
and is answered `CoinReturned`, with the coin back in the pool. The tree can
still land afterwards, and then the pool holds a coin the tree has spent.

So keep a `CoinReturned` record until a later `Recover` answers `CoinSpent`
or `TreeAdopted` for it (a `TreeHeld` record is kept until `Adopt`, as
always), or until the application has itself spent the coin in a
transaction that mined. On the next start `Recover` asks again: if the tree
landed, it takes the spent coin out of the pool, takes the tree's change,
and adopts or holds the tree, and nothing is lost. A record whose coin
simply stays unspent is answered `CoinReturned` on each start and costs one
question to the node.

What remains uncovered:

- Between the start that answered `CoinReturned` and the next, the coin is
  in the pool. If the tree lands in that time and the coin is taken to pay
  for a transaction, that transaction is refused (`producer.ErrRefused`)
  and is built again. The refusal takes the spent coin out of the pool
  when the node shows the tree as its spender (see
  [a refusal for a coin already spent](#a-refusal-for-a-coin-already-spent)).
  No coin or tree is lost; if the refused transaction
  was a funding tree, its own record is answered `CoinSpent`.
- `CoinReturned` puts the coin back on every start the record is kept. If
  the application has since spent the coin in a transaction of its own, and
  that transaction reached the leg an instant before a stop, the node can
  show the coin unspent once more, and the coin goes back to the pool
  though it is spent. A transaction that then takes it is refused, or,
  reaching the node first, displaces the earlier one. That refusal takes
  the coin out again when the node names its spender by then, and
  otherwise the next `Recover` that answers `CoinSpent` does.
- An application that drops the record at `CoinReturned` is, for a tree
  that lands late, where it was without `Prepare`: the tree, its change and
  its outputs are lost to it, and the spent coin stays in its pool.
- A tree paid through `Fund` is not covered. A wallet that funds and
  broadcasts a tree itself does both inside one call, so `Prepare` has no
  moment before the broadcast to be called in, there is no pool coin to
  return, and `Recover` has no record to work from. A run that stops after
  `Fund` has broadcast and before `Adopt` leaves a tree only that wallet
  knows.

An application that restarts at once waits a moment before it recovers,
which makes the first two rarer.

### `producer.Collector` and `producer.Pending`

| Field | Default | |
|---|---|---|
| `Proofs` | nothing mined | `producer.Proofs{Arcade, Asset, Source, Spends, Tx}`: arcade first, when it is the settlement leg, then the node (or `Source` in its place, a chain view such as `nodeapi.ParseChain` builds); an unmined transaction one of whose inputs the node (or `Spends`) shows spent by another is refused (`OfTx`, or `Of` with `Tx`) |
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

`headers.New(spec)` takes a header source specification:

| Specification | Source | Reads |
|---|---|---|
| `woc:main`, `woc:test` | the public WhatsOnChain API | `/block/{h}/header`, `/chain/info` |
| `chaintracks:https://host/v2` | a chaintracks v2 service (status envelope) | `/header/height/{h}`, `/height` |
| `bhs:https://host:8080` | a [block-headers-service](https://github.com/bsv-blockchain/block-headers-service); `/api/v1` is added to a URL with no path | `/chain/header/byHeight?height={h}&count=1`, `/chain/tip/longest` |
| `arcade:https://host` | the chaintracks server an [arcade](https://github.com/bsv-blockchain/arcade) installation embeds, under `/chaintracks/v2` (bare values, no envelope) | `/header/height/{h}`, `/height` |
| `https://host:port` | an [overlay-bridge](https://github.com/lightwebinc/overlay-bridge) header read API | `/v1/root/{h}`, `/v1/tip` |

`headers.NewSource(spec)` returns the parse error instead; `New` returns a
client that answers every call with it.

| Field | Default | |
|---|---|---|
| `Base` | from the specification | the source's base URL, with no path suffix |
| `Kind` | from the specification | `Native`, `WhatsOnChain`, `Chaintracks`, `BlockHeadersService` or `Arcade` |
| `Network` | `main` for WhatsOnChain main, chaintracks, bhs and arcade, `test` for WhatsOnChain test, none for a bridge | sets the proof-of-work floor; set `test` for a testnet chaintracks, bhs or arcade |
| `Token` | none | a bearer token sent on every request: a block-headers-service requires one unless its operator turned authentication off |
| `MinDifficulty` | `MainnetMinDifficulty` (4e9) on `main`, none otherwise | overrides the floor |
| `HTTP` | a client with no proxy | |
| `Timeout` | 10 s | used only when `HTTP` is nil |

Answers are bounded at 1 MiB. Which header source a reader trusts is the
security decision behind every proof it checks. A bridge's `/v1` answers
carry no header fields and are taken as given, so point at one that received
its headers itself. An answer from any other kind is not taken as
given: every header is hashed, must carry the work its bits claim, and on
mainnet must claim at least the floor, so a source that lies has to mine a
block to do it. A header that fails is `ErrProofOfWork`, an error and never a
false answer. For the strongest check run your own block-headers-service, chaintracks or
node. block-headers-service's `merkleroot/verify` is never asked: it would
give up the proof-of-work check.

`Client.HeaderAt(ctx, height)` returns the checked header (hash, time,
merkle root): how an application with no node reads a block's hash or time
by height. A height the source does not hold is `ErrUnknownHeight`; a
bridge source carries no header fields and is an error.

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

## Parse guard: `guard`

`ParseBUMP(b, bound)`, `CheckBEEF(b, bound)`, `ParseBEEF(b, bound)`,
`ParseTransaction(b, bound)` and `RawTransaction(b, bound)` take the caller's
bound on the input's size, in bytes; a bound of zero or less admits nothing.
`nodeapi` uses its 1 MiB body bound for a proof and for a transaction. `verify.VerifyCarrier`, `funding.Rebuild` and
`producer.Payer` pass `DefaultBound`, 64 MiB, the bound `hostset` puts on a
whole lookup answer, and `purse.Check` passes it for a payment.
`chaintoken.ReadWire` passes the host's own BEEF bound. `ParsePubKey` and
`ParsePubKeyHex` take no setting.

## Terminal text: `termsafe`

`Sanitize(s, Options{ANSI, ASCII})` bounds its output at `MaxLines` (200)
lines of `MaxCols` (512) columns. `ANSI` keeps color and weight sequences
and nothing else, and ends coloured output with a reset; `ASCII` prints
every rune above 0x7E as `?`. `Validate(field, s)` and
`ValidateBounded(field, s)` refuse what `Sanitize` would strip, the second
within the same two bounds; every refusal matches `ErrUnsafe`.
`UTF8Locale(getenv)` reads `LC_ALL`, `LC_CTYPE` and `LANG` through the
lookup the application passes, so the package itself reads no environment.

## Renderer filter: `sanitize`

`Filter(s)` takes no setting: its four rules and its table are fixed, so
that every renderer that calls it, in Go or through the TypeScript
`filterText` and `filterBytes`, shows the same characters for the same
value. The table is Unicode 15.1's (`UnicodeVersion`), embedded in the
package and generated by `make vectors-update` from the data files under
`third_party/unicode/15.1`. It does not bound a value's length: a terminal
renderer passes what `Filter` returns through `termsafe.Sanitize`, which
bounds it, and a web renderer bounds what it shows itself and inserts the
text as text.

## Keyed values: `keyed`

`SealSegment(k, salt, plaintext)` takes a plaintext of 1 to
`keyed.MaxSegment` (1 MiB) bytes and returns it sealed as one segment, 16
bytes longer; `OpenSegment` refuses a ciphertext that cannot be one segment
(`ErrSegment`) and a tag that does not verify (`ErrTag`). The salt is
`keyed.SaltLen` (4) bytes, from `SampleSalt` or the caller's own CSPRNG,
fresh for each content key. The epoch wrap takes the application's domain
string as its first argument, never empty (`ErrDomain`), and the epoch's
symmetric key (`SymmetricKey` of the epoch key) rather than the epoch key
itself.
