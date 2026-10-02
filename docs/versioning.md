# Versioning

bcommon is versioned with semantic version tags of the form `vMAJOR.MINOR.PATCH`
on the main branch. The Go module proxy caches a tag the first time anyone
fetches it, so a published tag is permanent: it is never moved or deleted, and
a mistake is fixed by a new tag.

## Pin an exact tag

The library is pre-1.0. Every application requires an exact tag in its
`go.mod`, for example:

```
require github.com/lightwebinc/bcommon v0.1.0
```

and moves to a newer one deliberately, running its own tests and checking its
own vectors against the new tag before it ships. Build a release binary with
`GOWORK=off` and confirm the version it links with `go version -m`, since a
workspace resolves the library from disk and hides which tag is in use.

## One tag, both languages

The TypeScript package under `ts/` has no version line of its own. The
commit tagged `vX.Y.Z` carries `ts/package.json` at version `X.Y.Z`: tag
`v0.1.0` ships `@lightwebinc/bcommon` 0.1.0. A change to either
language is a new tag for both, even when the other did not change, so one
version names one tree and one set of vectors, and the two languages are
held to the same bytes at every version.

The version is set in `ts/package.json` in the commit that is tagged, never
after it: a tag is permanent, and one whose `package.json` names another
version breaks the rule for good.

An application that uses both languages pins the same tag in each: the Go
module in `go.mod`, and the package packed from that tag (`npm pack` in
`ts/`), for example vendored as a tarball and named as a `file:`
dependency, so that its lockfile pins the bytes. The package declares
`@bsv/sdk` as a peer at one exact version, so the application supplies
that version itself.

## What a v0 minor may change

Before v1.0.0:

- A **minor** version (v0.1.x to v0.2.0) may change or remove exported API:
  rename a package, identifier or field, change a signature, or change what a
  function refuses and how.
- A **patch** version (v0.1.0 to v0.1.1) fixes defects and adds tests or
  documentation without changing exported API.

The same holds for the TypeScript package's exports, from both of its
entry points.

Output bytes are a different matter from API. A change to the bytes a
function produces for inputs an application has already committed to the
chain (encodings, derived keys, roots) breaks that application's existing
records whatever the version number says, because its readers verify those
bytes long after they were written. The tests' vectors exist to catch such a
change, and the go-sdk pin, like the TypeScript package's exact `@bsv/sdk`
peer, exists partly for the same reason (see
[dependencies.md](dependencies.md)).

## Release history

| Tag | What changed |
|---|---|
| v0.6.0 | Minor, additive only: every v0.5.5 identifier is unchanged, so an application moves by changing its pin. Six packages added, each for code more than one application was carrying its own copy of. `record`: the bounded, ordered reading of an application record (`Decode`, `DecodeMap`, `Split`, `Fields` and its readers, `CheckExtra`, `CheckExtraKeys`, `Encode`, `Claims`, `Reason`, `MaxKeys` and the eight refusals). `keyed`: the content key of BRC-369 section 2, its symmetric key and commitment, the check on an unwrapped key and BRC-2's symmetric form (`SampleKey`, `CheckScalar`, `SymmetricKey`, `Commitment`, `CheckOpened`, `SymmetricSeal`, `SymmetricOpen`). `chaintoken`: the BEEF a host admits a mined token, an unmined carrier and a mined sweep in, read as declared on the wire, and a token output held to its key (`ReadWire`, `Wire.Token`, `Wire.Carrier`, `Wire.Alone`, `MinimalPath`, `MergedPath`, `TokenBEEF`, `Stored`, `Mined`, `ReadOutput`, `Output.LockedTo`, `Output.SignedBy`, `Spends`, `CheckDER`, `VerifyField`). `chainview`: whether a transaction can still mine, from a leg's error and the node's view (`RefusedAnswer`, `SpentElsewhere`). `purse`: the client and payee legs of a BRC-105 payment over the embedded wallet (`Purse` with `CreateAction`, `Settle`, `Refund`, `InternalizeAction` and its halves, `Remittance`, `Unmined`, `RefusedError`). `testchain`: a local stand-in chain for tests (`Chain`). New in `pushdrop`: `FirstPush`, `Fields`, `Script`. `TokenBEEF` flags both txids when a token and its parent are each other's sibling in one block, where merging the two paths with go-sdk alone leaves one unflagged and the BEEF inadmissible. In TypeScript: `recordReader`, `claimsRecord`, `ascending`, `MaxKeys`; `firstPush`, `pushFields`, `pushDropScript`, `minimalPushBytes`; `checkBEEF`, `readWire`, `subjectTx`, `anyTxidOnly`, `tokenShape`, `carrierShape`, `aloneShape`, `minimalPath`, `mergedPath`, `tokenBEEF`, `storedToken`, `mined`, `readTokenOutput`, `tokenLockedTo`, `tokenSigned`, `tokenSignedBy`, `tokenSpends`, `verifyField`, `BeefRefusal`, `DefaultMaxBEEF`; and `strictSignature`, which was internal. The TypeScript package now reads a BEEF, behind the same structural walk as the Go guard. New vector families `record-v1.json`, `keyed-v1.json` and `chaintoken-v1.json`; existing vectors are unchanged, and `beef-v1.json` is now read by the TypeScript walk too. The registry records bsecret (protocols `[1, "bsecret"]` and `[2, "bsecret self"]`, prefix `se`, record magics `sev`, `ses` and `sep` `0x01`, topics `tm_bsecret_<name>_<suffix>`, lookup service `ls_bsecret`, its terms route, baskets and domain strings), provisional until its first publish, adds borg's key id `signer`, and adds a rule for domain strings. |
| v0.5.5 | Documentation only: the registry records borg (protocol `[1, "borg organisation"]`, key ids `org`, `group`, `record`, `grant`, `chain` and `fund`, prefix `bo` with tags `0x01` to `0x03`, record magics `boo`, `bog`, `bom` and `bow` `0x01`, topics `tm_borg_<name>_<suffix>`, lookup service `ls_borg`, its terms route, baskets, the membership grant's certificate type, and its use of BRC-369's release protocol), provisional until its first publish, and adds rules for certificate types and borrowed protocols. The TypeScript package changes only its version. |
| v0.5.4 | Fix: arcade can answer ACCEPTED_BY_NETWORK for a transaction one of whose inputs another transaction already spent, which never mines, so `producer.Payer.Settle`, `SettleAndWait` and `Await` waited out their timeout and `producer.Proofs` reported it pending for ever. The node's view of the inputs now decides: `Payer.Settle` with `Async`, `Payer.Await` (so `SettleAndWait` and `Settle` without `Async`), `Proofs.OfTx`, `Proofs.Of` with the new `Proofs.Tx`, and `Collector` for its `Pending` items return at once an error wrapping `producer.ErrRefused` and a `*nodeapi.SpentError` when the node shows an input spent by another transaction; `publish.Arcade` does the same at `Submit` when its new `Asset` field is set. An input the node cannot answer for is passed over. New: `nodeapi.ErrDoubleSpent`, `nodeapi.SpentError`, `nodeapi.Asset.Spender`, `nodeapi.Asset.SpentElsewhere`, `nodeapi.WaitSettled`, `publish.Arcade.Asset`, `producer.Proofs.Tx`, `producer.Proofs.OfTx`. The TypeScript package changes only its version. |
| v0.5.3 | Fix: a node that answers `/api/v1/tx/<txid>` in Extended Format (BRC-30), as a Teranode asset API does, coinbases included, was refused by `guard.ParseTransaction` as a transaction of no inputs, so since v0.5.1 `producer.Payer.Parent` could not fetch a coinbase coin's parent and a producer paying from coinbase failed. `nodeapi.Asset.TxRaw` now reads either form and returns the raw transaction, and walks the answer before returning it, so a malformed or trailing-byte answer is refused there as `guard.ErrTransaction`. New: `guard.RawTransaction`, which walks a raw or Extended Format transaction with the same bounds as the rest of the guard and returns the raw form, dropping the previous outputs Extended Format carries, and `guard.IsEF`. `guard.ParseTransaction` is unchanged and still refuses Extended Format. The TypeScript package changes only its version. |
| v0.5.2 | Documentation only: the registry records bbox (protocol `[1, "bbox message"]`, key ids `envelope`, `signature` and `fund`, prefix `bb`, record magics `bbe` `0x01` and `bbr` `0x01`, topics `tm_bbox_<name>_<suffix>`, lookup service `ls_bbox`, its terms route and baskets), provisional until its first publish. The TypeScript package changes only its version. |
| v0.5.1 | Fix: a transaction paid from a coinbase coin whose parent the pool holds no bytes of, kept as BEEF before it mined (`funding.KeepBEEF`), carried a placeholder parent of no inputs that the v0.5.0 guard refused on reading back (`funding.Rebuild`, `producer.Kept`); its bytes were never the coin's transaction, so the SDK read without the guard left the fee input unlinked too. `producer.Payer.Parent` now fetches such a parent with its proof from `Asset` whether or not `Async` is set, and builds a placeholder only with no `Asset`. New: `funding.BEEF`, which writes an Atomic BEEF after refusing any placeholder in the ancestry, and `funding.ErrPlaceholder`; `funding.KeepBEEF`, and the funding tree and proof republish in `producer`, go through it, so a placeholder is refused when the BEEF is built, never written. The guard is unchanged and still refuses a transaction of no inputs in any BEEF it reads. The TypeScript package changes only its version. |
| v0.5.0 | Minor: new API, and parsing is stricter, so this version refuses some inputs earlier versions accepted. A BEEF or raw transaction is walked by the guard before go-sdk parses it, bounding every declared count and length by the bytes present, which no longer rests on the go-sdk pin. A public key from the wire is accepted only as its one canonical compressed encoding, and a PushDrop lock only as the one script the template writes. A zero-value coinbase is no longer pooled, the pool never hands out a coin too small to pay, and a producer's failed funding tree returns its coin. New: `guard.CheckBEEF`, `guard.ParseBEEF`, `guard.ParseTransaction`, `guard.ParsePubKey`, `guard.ParsePubKeyHex`, `guard.ErrBEEF`, `guard.ErrTransaction`, `guard.ErrPubKey`, `guard.DefaultBound`, `pushdrop.CheckCanonical`, `pushdrop.ErrNonCanonical`, `bwallet.Pool.TakeAtLeast`, `producer.Payer.TakeAtLeast`; in TypeScript `strictPublicKey`, `strictPublicKeyHex`, `decodeStrictPushDrop`. Newly refused: a BEEF with trailing bytes, with no transactions, with a transaction of no inputs, or over the size bound (64 MiB where the library parses, `guard.DefaultBound`), each of which go-sdk parses, and in `funding.Rebuild` and `producer.Payer.Parent` a raw transaction of no inputs or a proof with trailing bytes; a public key that is uncompressed, hybrid, or compressed with x at or above the field prime, as an identity in `carrier.Validate`, a counterparty in `bwallet.Counterparty`, a pin in `knownkeys` (and `Pin` and `Rotate` refuse upper-case hex), or a locking key; a PushDrop lock with any non-minimal push, extra or missing drop opcodes, or trailing opcodes, which `carrier.Decode` refuses as `ErrShape`, `carrier.DecodeFunding` no longer reads as funding and `pushdrop.DecodeTagged` refuses as `ErrNonCanonical`, and which TypeScript `decodeCarrier` refuses as `bad-record` and `decodeFunding` no longer reads; an identity that is not a canonical key in TypeScript `readerLockingKey`, which throws. Behaviour: `Pool.Take` and `TakeAllowing` skip zero-value coins; `Payer.Take` takes a coin of at least the fee floor, and a coin it cannot sign for is back in the pool when it returns; `Trees.Spend` takes a coin of at least the tree's value and the fee floor, returns it on every failure before the tree reaches the settlement leg, and releases it once the tree is on the leg so a later `GiveBack` cannot return a spent coin. New vector families `beef-v1.json`, `pubkeys-v1.json`, `pushdrop-v1.json`; existing vectors are unchanged. |
| v0.4.0 | `producer.Trees` mints ahead: with `Ahead` set, a spend that leaves the current funding tree with that many outputs or fewer mints and settles the next tree in the background, and the spend the current tree cannot cover switches to it with no wait for a block. The tree minted ahead is adopted and published only at the switch. New: `Trees.Ahead`, `Trees.Wait`, `Trees.Prepared`. With `Ahead` zero nothing changes. The registry adds blogs and the rule that a topic more than one independent user can create carries a random suffix. The TypeScript package changes only its version. |
| v0.3.4 | Security fix: a carrier is refused unless it has exactly one input whose unlocking script is exactly one minimally encoded push of a strict DER signature (BIP 66) with a low S and the sighash byte SIGHASH_ALL\|FORKID (0x41). Before this, anyone who saw a carrier could rewrite its unlocking script without the key (flip S, widen the push, add a push or a no-op) and spend its funding output under another txid carrying the same record, which a host reads as the record retracted. Carriers `carrier.Mint` builds already have this form and still pass. New: `carrier.CheckUnlocking`, `carrier.ErrUnlocking`, `carrier.SigHashType`, `verify.RefusedUnlocking` (`REFUSED-UNLOCKING`); in TypeScript `unlockingRefusal`, `SigHashType` and the refusal `non-canonical-unlocking`, which `decodeCarrier` now applies. New vector family `unlocking-v1.json`. |
| v0.3.3 | `headers` retries a 429 answer with back-off (honouring `Retry-After`) and reuses a root proven by its header's work for ten minutes, so a rate-limited public source is not read as a failed proof. The TypeScript package changes only its version. |
| v0.3.2 | Documentation only: the registry records bgateway's record magic (`gwr` `0x01`), funding key id and basket, and drops topics that are not for publication; examples and tests say "test chain". The TypeScript package changes only its version. |
| v0.3.1 | `termsafe`: a coloured value cut at the line bound is reset before `[truncated]`, so its colour no longer runs on into what is printed next. The TypeScript package changes only its version. |
| v0.3.0 | `headers` reads WhatsOnChain (`woc:main`, `woc:test`) and chaintracks v2 (`chaintracks:URL`) as well as a bridge, and checks the proof of work of every header those serve, with a mainnet floor (`MainnetMinDifficulty`). New: `Parse`, `NewSource`, `Kind`, `Client.Kind`, `Client.Network`, `Client.MinDifficulty`, `ErrSource`, `ErrProofOfWork`. `New` now takes a source specification; a bridge URL means what it did. The TypeScript package changes only its version. |
| v0.2.0 | Two packages added: `producer`, a producer's fees, settlement, funding-tree lifecycle, kept transactions and proof collection, and `termsafe`, text filtered for a terminal. Additive only: every v0.1.0 identifier is unchanged, so an application moves by changing its pin. The TypeScript package changes only its version. |
| v0.1.0 | The first release. |
