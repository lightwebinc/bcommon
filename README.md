# bcommon

[![CI](https://github.com/lightwebinc/bcommon/actions/workflows/ci.yml/badge.svg)](https://github.com/lightwebinc/bcommon/actions/workflows/ci.yml)
[![CodeQL](https://github.com/lightwebinc/bcommon/actions/workflows/codeql.yml/badge.svg)](https://github.com/lightwebinc/bcommon/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/lightwebinc/bcommon)](https://github.com/lightwebinc/bcommon/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/lightwebinc/bcommon.svg)](https://pkg.go.dev/github.com/lightwebinc/bcommon)
[![Go version](https://img.shields.io/github/go-mod/go-version/lightwebinc/bcommon)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> [!WARNING]
> **Experimental software.** bcommon is part of [bstack](https://github.com/lightwebinc/bstack),
> the applications and patterns built on [BSV Layered Multicast](https://github.com/lightwebinc/bsv-multicast).
> It is published to be built on and improved. Interfaces, formats and behavior may change
> rapidly between releases; pin an exact version.

bcommon is a Go library of building blocks for BSV overlay applications that
publish and verify committed records: the codec a record is written in, the
roots and store references it commits to, the derivation and PushDrop outputs
that lock it, the carrier and mined transactions that put it on chain, the
BEEF a host admits them in, the clients a producer and a reader talk to, and
the SPV check a reader runs. An
application supplies its own schema, derivation, tags and wallet profile as
parameters; nothing here names one.

One tag versions both the Go module and the TypeScript package; what a v0
minor or patch may change is in [docs/versioning.md](docs/versioning.md).

## Packages

| Package | What it provides |
|---|---|
| `cbor` | Deterministic CBOR, an RFC 8949 subset; the decoder refuses anything non-canonical |
| `record` | The bounded, ordered reading of an application record: a canonical CBOR map with integer keys, a magic and preserved unknown keys, refused for the first rule it breaks |
| `commit` | RFC 6962 Merkle roots, inclusion paths and their verification, over 32-byte commitments or leaves of any length: a streaming root, the root of content cut into fixed segments, per-block subtree roots, and the compact path whose sides follow from the index and the leaf count |
| `chirp` | CHIRP (BRC-167) version 1 and its profile 1: root and branch node codecs, the canonical construction from content (in memory or streamed), closure verification object by object, and the UHRP object identifier and CHIRP URL of a hash |
| `store` | Store reference entries, manifests, and the rule that computes a store's root from its entry and its members' commitments |
| `pushdrop` | BRC-42/43 derivation under counterparty Anyone, tagged PushDrop locks, unlockers and decoding, and a lock read leniently and rebuilt canonically from raw script bytes |
| `carrier` | An unmineable carrier transaction that commits a payload, and the funding lock, decode and sweep it spends from |
| `chaintoken` | The BEEF a host admits a mined chain token, an unmined carrier and a mined sweep in, read exactly as declared on the wire and held to exactly what the object needs, and a token output held to its key |
| `keyed` | The content key of BRC-369 keyed content, its symmetric key and commitment, the check on an unwrapped key, BRC-2's symmetric form, one BRC-369 segment, and a content key's wrap under a group epoch with the application's domain string |
| `mint` | Builders for a state transition, a funding tree of at most 1023 funding outputs and a payment, with a fee loop that signs to measure the size and rebuilds at an exact integer rate (satoshis per bytes, as miners publish it) until the fee covers it |
| `feepolicy` | Where the fee rate comes from: a static policy, or a broadcaster's published `/v1/policy`, cached and held to minimum and maximum rates |
| `funding` | Funding-tree state kept between runs, and the BEEF kept for transactions spent before they mine |
| `guard` | A structural walk of a BRC-74 BUMP, a BEEF or a raw transaction before the SDK allocates for it, and a public key taken only in its canonical encoding |
| `nodeapi` | The chain views an application reads (transactions, proofs, spends, known) as narrow interfaces, over a Teranode JSON-RPC and asset API client or WhatsOnChain, picked per method from a specification, with every proof checked against the caller's headers |
| `bwallet` | An embedded BRC-100 wallet backend and coin pool, a Signer, BRC-29 derivations, and the import of a funding payment from the user's wallet BEEF or by txid, keyed by an application profile |
| `wirewallet` | A BRC-100 wallet over the wallet wire, loopback only, and a handler that serves one |
| `publish` | The settlement leg (EF to an ingress, hex to a node RPC, arcade by default, ARC) and the BEEF object leg to an overlay host, which never share a socket, plus a transition journal |
| `producer` | A producer's orchestration: fee inputs and change from a coin pool, settlement, the funding-tree lifecycle, the one kept copy of each unproven transaction, and proof collection that republishes what has mined |
| `chainview` | Whether a transaction can still mine: a settlement leg's error read as a definitive refusal or as transient, and the input another transaction spent |
| `purse` | The client and payee legs of a BRC-105 payment for a priced question, over the embedded wallet: pay one output on a 402, and take a BRC-29 payment into the pool |
| `payee` | The payee's side of those payments: the payee key, a host's versioned ledger of accepted payments read and written byte for byte, the host's replay and conflict rule, and an idempotent settle run into the pool with its counts |
| `acceptance` | The value discriminator for an incoming payment: fast on SPV, the receiver's own broadcast and the network's verdict at or below a threshold, held for a proof above it, bounded per payer and in total, and a monitor that flags a payer whose fast payment is lost |
| `headers` | A chain tracker over WhatsOnChain, chaintracks, block-headers-service, arcade's header server or an [overlay-bridge](https://github.com/lightwebinc/overlay-bridge), checking proof of work |
| `hostset` | Host sources and quorum fan-out across the addresses behind one overlay host |
| `lookup` | A BRC-24 lookup client for output-list answers |
| `resolve` | BRC-169 handle resolution and BRC-180 overlay discovery, under a strict HTTPS client policy |
| `knownkeys` | The grammar and store of a pinned-key file: pin, rotate, retire, forget |
| `verify` | The refusal vocabulary, SPV verdicts on one transaction, and the carrier check a reader runs |
| `termsafe` | Text someone else wrote, filtered before it reaches a terminal, and the same rules checked before a producer publishes text |
| `sanitize` | The renderer filter: four ordered character rules over a pinned Unicode 15.1 emoji table, which a terminal and a web renderer apply alike before they show text someone else wrote |
| `goldentest` | Test helpers: a fixed key, hex and transaction parsing that fail the test, and a stub chain tracker |
| `testchain` | A local stand-in chain for tests: it mines what it is sent and serves a node's RPC and asset API, a broadcaster, an ingress and a header source |

The TypeScript package under [ts/](ts/), `@lightwebinc/bcommon`, holds the
twins an overlay topic manager or lookup service needs, tested against the
same vectors as the Go packages. It has three entry points:

| Entry point | What it provides |
|---|---|
| `@lightwebinc/bcommon` | Deterministic CBOR, the record reader, store refs entries, the reader's BRC-42 derivation, PushDrop reading and field signatures, the funding decode, the carrier check, the BEEF a mined token is admitted in, byte-leaf RFC 6962 roots and compact paths, the CHIRP codec and closure check, the renderer filter, the writer (funding lock, carrier and self-paying sweep through a BRC-100 wallet), and the overlay engine interfaces a module satisfies. It imports nothing but its peer `@bsv/sdk` and nothing from `node:`, so a browser can load it as well as a host |
| `@lightwebinc/bcommon/host` | For Node: what an overlay host module runs beside the engine when it answers questions itself. The terms route (`LookupFront`: BRC-104 server side, BRC-105 priced questions, the terms document and price list), its hardening (handshake budgets checked before any signature work, per-session response budgets, replay refusal, bounded sessions), the payee ledger as a host writes and reads it, the Go `payee` package's lines byte for byte, and payment acceptance over arcade and the node |
| `@lightwebinc/bcommon/testing` | Test helpers for Node: a counting host, restore rows and storage, BEEF built as the engine builds it, a minter over a test key, and a simulator that calls a lookup service in the engine's order |

## Install

Go, pinned to an exact tag, the latest in [docs/versioning.md](docs/versioning.md):

```bash
go get github.com/lightwebinc/bcommon@v0.18.0
```

TypeScript: the package is packed from the same tag and vendored, so the
application's lockfile pins its bytes, and the application supplies the
`@bsv/sdk` peer at the exact version the package names:

```bash
git clone --depth 1 --branch v0.18.0 https://github.com/lightwebinc/bcommon
cd bcommon/ts && npm ci && npm pack    # writes lightwebinc-bcommon-0.18.0.tgz
# in the application, with the tarball copied to vendor/
npm install ./vendor/lightwebinc-bcommon-0.18.0.tgz @bsv/sdk@2.7.1
```

## Usage

A record body in canonical CBOR, its RFC 6962 commitment, and the key a
reader expects the record's output to be locked to, from the producer's
identity key alone:

```go
import (
	"crypto/sha256"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/commit"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// A placeholder: an application registers its own in docs/registry.md.
var record = pushdrop.Derivation{
	Protocol: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "example app"},
	KeyID:    "record",
}

body, err := cbor.Encode(cbor.Map{{Key: "name", Val: "example"}})
root := commit.Root([][32]byte{sha256.Sum256(body)})
key, err := record.ExpectedLockingKey(identity) // identity is an *ec.PublicKey
```

The same derivation on the TypeScript side, in a topic manager:

```ts
import { readerLockingKey } from '@lightwebinc/bcommon'

const key = readerLockingKey([1, 'example app'], 'record', identityHex)
```

### Mainnet and testnet

The library works on BSV mainnet and testnet with no infrastructure of
your own: `headers.New("woc:main")` checks proofs against WhatsOnChain
headers (with proof of work checked), `nodeapi.ParseChain("woc:main", ...)`
reads transactions, proofs and spends from WhatsOnChain, and
`publish.ParseSettler("arcade:main", ...)` broadcasts through GorillaPool's
public arcade (`test` for testnet throughout). A node, block-headers-service
or a self-hosted arcade replaces any of them by configuration. An
application's coin comes from its user: the wallet prints its fund address,
the user pays it from their own wallet, and the application imports the
payment from the BEEF the user's wallet hands over or by txid once it is
mined, or it hands funding to a BRC-100 wallet the user already runs.
[docs/examples.md](docs/examples.md#fund-a-wallet-on-mainnet-or-testnet)
shows both. The coinbase helpers (`bwallet.FundFromCoinbase`,
`bwallet.Rescan`) work only on a regtest chain you run, for development and
tests.

[docs/examples.md](docs/examples.md) also walks through deriving and
decoding a PushDrop lock, building a funding tree and a carrier, verifying a
carrier from a BEEF, guarding a proof, RFC 6962 proofs, stores and CBOR,
reading an application record, admitting a mined token from its BEEF,
committing to a key and wrapping it, paying fees and minting the next
funding tree as a producer, and filtering text for a terminal. Those Go
examples are `Example` tests that `go test` compiles and checks, on an
in-process test chain.

## Documentation

- [Architecture](docs/architecture.md): the package layers and import graph, what each package owns, the parse-and-guard rule, application-supplied constants, and the Go and TypeScript twins
- [Configuration](docs/configuration.md): every caller-supplied parameter and option struct, the defaults, and the values frozen once used on chain
- [Examples](docs/examples.md): funding a wallet on mainnet or testnet, then task-by-task how-to backed by compiled example tests
- [Registry](docs/registry.md): the derivation protocols, tags, record magic, topics and baskets that applications built on bcommon have chosen, so that no two collide
- [Vectors](docs/vectors.md): the tests compare the library's output byte for byte with vectors from an independent generator
- [Dependencies](docs/dependencies.md): the one direct dependency and why its version is exact
- [Versioning](docs/versioning.md): exact tags, one tag for both languages, and what a v0 minor may change

## Requirements

Go 1.27.1 or later, and github.com/bsv-blockchain/go-sdk pinned at exactly
v1.7.1, the library's only direct dependency. See
[docs/dependencies.md](docs/dependencies.md). The TypeScript package needs
Node 24 and `@bsv/sdk` 2.7.1 exactly, as a peer; `make ts-test` builds and
tests it, and nothing in the Go build needs Node.

## Build and test

```bash
make verify     # formatting, vet, the dependency rule, licenses, vectors, build, tests
make ts-test    # the TypeScript package: type-check, build, tests (Node 24)
```

Every Go target runs with `GOWORK=off`, so what is checked is what a tag ships.

## License

Apache-2.0 ([LICENSE](LICENSE)). Third-party notices are in
[NOTICE](NOTICE) and [LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
