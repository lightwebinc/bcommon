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
| v0.3.4 | Security fix: a carrier is refused unless it has exactly one input whose unlocking script is exactly one minimally encoded push of a strict DER signature (BIP 66) with a low S and the sighash byte SIGHASH_ALL\|FORKID (0x41). Before this, anyone who saw a carrier could rewrite its unlocking script without the key (flip S, widen the push, add a push or a no-op) and spend its funding output under another txid carrying the same record, which a host reads as the record retracted. Carriers `carrier.Mint` builds already have this form and still pass. New: `carrier.CheckUnlocking`, `carrier.ErrUnlocking`, `carrier.SigHashType`, `verify.RefusedUnlocking` (`REFUSED-UNLOCKING`); in TypeScript `unlockingRefusal`, `SigHashType` and the refusal `non-canonical-unlocking`, which `decodeCarrier` now applies. New vector family `unlocking-v1.json`. |
| v0.3.3 | `headers` retries a 429 answer with back-off (honouring `Retry-After`) and reuses a root proven by its header's work for ten minutes, so a rate-limited public source is not read as a failed proof. The TypeScript package changes only its version. |
| v0.3.2 | Documentation only: the registry records bgateway's record magic (`gwr` `0x01`), funding key id and basket, and drops topics that are not for publication; examples and tests say "test chain". The TypeScript package changes only its version. |
| v0.3.1 | `termsafe`: a coloured value cut at the line bound is reset before `[truncated]`, so its colour no longer runs on into what is printed next. The TypeScript package changes only its version. |
| v0.3.0 | `headers` reads WhatsOnChain (`woc:main`, `woc:test`) and chaintracks v2 (`chaintracks:URL`) as well as a bridge, and checks the proof of work of every header those serve, with a mainnet floor (`MainnetMinDifficulty`). New: `Parse`, `NewSource`, `Kind`, `Client.Kind`, `Client.Network`, `Client.MinDifficulty`, `ErrSource`, `ErrProofOfWork`. `New` now takes a source specification; a bridge URL means what it did. The TypeScript package changes only its version. |
| v0.2.0 | Two packages added: `producer`, a producer's fees, settlement, funding-tree lifecycle, kept transactions and proof collection, and `termsafe`, text filtered for a terminal. Additive only: every v0.1.0 identifier is unchanged, so an application moves by changing its pin. The TypeScript package changes only its version. |
| v0.1.0 | The first release. |
