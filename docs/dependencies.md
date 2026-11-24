# Dependencies

## One direct dependency

bcommon imports the Go standard library, its own packages and
github.com/bsv-blockchain/go-sdk, and nothing else. The rule covers tests as
well as production code, and packages a build constraint leaves out.

Every application that pins bcommon inherits its requirements. A second
direct dependency would be a cost to every one of them at once: another
module to audit, another license to carry, another version for minimal
version selection to reconcile with the application's own. The rule is
enforced rather than intended:

- `make deps-check` fails unless the only direct requirement in `go.mod` is
  go-sdk, at exactly the pinned version.
- `TestBoundary` and `TestBoundaryFiles` (in the root package) fail on any
  import outside the rule, test imports and constrained files included.

The vector generator in `tools/vectors` is a separate module, so it sits
outside the rule. Nothing in the library imports it. Its own requirements are
go-sdk, at the same pin, and the independent CBOR encoder it exists to bring
in. `make vectors` checks both, and `TestBoundaryFiles` holds its imports to
them (see [vectors.md](vectors.md)).

The other modules in `go.mod` are go-sdk's own requirements. They are listed
in [NOTICE](../NOTICE), with their licenses, and their full texts are in
`LICENSE-THIRD-PARTY`, which `make licences-update` regenerates from what the
packages actually link and `make licences` checks is current.

## Why go-sdk is pinned at exactly v1.7.8

The pin is exact rather than a minimum because minimal version selection
takes the higher of two requirements. A raised pin here would silently change
the go-sdk that every application pinning bcommon builds with, including the
derivation, script and transaction encodings its records depend on.

What the pin has to clear, and what the library does itself whatever the
pin:

| Concern | In go-sdk | In this library |
| --- | --- | --- |
| A merkle path's declared counts sized an allocation | bounded from v1.5.2 | `guard` walks every BUMP, BEEF and raw transaction before the SDK sees it |
| A BEEF's ancestry was walked once per path, so a few kilobytes held a reader for hours | walked once per transaction from v1.7.3 | `guard.Limits.Ancestry` is there for a caller whose own walker is of the old kind; `DefaultLimits` sets none |
| The BRC-103 handshake sent Go's field names, which TypeScript peers on `@bsv/sdk` 2.8 and later refuse | the wire shape from v1.7.1, which still reads the old names | |
| A compressed public key whose x is at or above the field prime was accepted | refused from v1.7.7 | `guard.ParsePubKey` is in front of every key from the wire, whatever the SDK does |

## What moving from v1.7.1 to v1.7.5 changed

Three things changed for a reader of this library's objects. No derivation,
signature or encoding changed: every transaction vector is byte for byte
what v1.7.1 produced.

| Change in go-sdk | From | Effect here |
| --- | --- | --- |
| `spv.Verify` refuses an unmined transaction that is not final, a coinbase spent before 100 blocks, and several shapes a node does not mine | v1.7.2 | A carrier is not final by design. `verify.Check` therefore runs SPV from the SDK's parts with no finality rule (every proof against the tracker, then `spv.VerifyScripts`). Payments still go through `spv.Verify` and gain the stricter rules |
| The script interpreter refuses a high-S signature and an unlocking script that is not push-only, in a version 1 transaction | after v1.7.1 | Three cases of `unlocking-v1.json` are now `scriptValid: false`. `carrier.CheckUnlocking` refused them before and still does |
| The license is Apache-2.0, with a NOTICE and a PATENTS file | v1.7.5 | [NOTICE](../NOTICE) carries go-sdk's attribution; the Open BSV License's terms no longer apply to it |

A test that builds a parent by hand, with no inputs, marks it as mined with
`goldentest.Proven`: the SDK now refuses an unmined transaction without
inputs.

## What moving from v1.7.5 to v1.7.8 changed

Nothing for a reader of this library's objects. No derivation, signature or
encoding changed: every vector is byte for byte what v1.7.5 produced. The
SDK's `spv`, `transaction` and `wallet` packages are unchanged between the
two versions.

| Change in go-sdk | From | Effect here |
| --- | --- | --- |
| `ParsePubKey` refuses a compressed key whose x is not below the field prime | v1.7.7 | None: `guard.ParsePubKey` already refused it, and still does |
| `DecodeScript` treats an `OP_RETURN` that follows an unmatched `OP_ENDIF` as an ordinary opcode, where it folded the rest of the script into one chunk | v1.7.6 | None in the vectors. `pushdrop`'s canonical check reads chunks, and no record this library writes has an unmatched `OP_ENDIF` |
| The SDK's own certificate field decryption refuses a value that is not UTF-8 | v1.7.6 | None: `resolve` decrypts revealed fields itself and does not call it |
| BIP39 normalizes a mnemonic and passphrase (NFKD), a BIP32 derived private key stays 32 bytes, `auth.Peer.ToPeerOnSession` is added, and the storage URL helpers return sentinel errors | v1.7.6 to v1.7.8 | None: the library calls none of them |

The move was made because go-overlay-services v1.3.10 requires go-sdk
v1.7.8, so an overlay host that mounts an application's module already builds
that application on v1.7.8. Pinning it here has the applications test on the
SDK they run on.

## Raising the pin

Raising go-sdk is a deliberate change, never an automatic one. It requires
re-verifying every vector the library's tests check against, because a
change in the SDK's derivation, signing or encoding would change bytes that
applications have already committed to the chain. Raise the version in
`go.mod`, in `tools/vectors/go.mod` and as `SDK_VERSION` in the Makefile
together, run `make verify`, and confirm every vector still matches byte for
byte before tagging.

## The TypeScript package

The package under `ts/` has no runtime dependency. Its one peer, `@bsv/sdk`,
is pinned exactly, at 4.1.2, and the development dependency is the same
version; the package's boundary test fails if the two differ or if a runtime
dependency appears. The exact peer holds the TypeScript twins to the SDK
their bytes were checked against, as the go-sdk pin does for Go. Raising it
means changing both entries together and running the TypeScript tests,
which compare the twins' output with the shared vectors, before tagging.

