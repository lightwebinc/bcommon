# bcommon

bcommon is a Go library of building blocks for BSV overlay applications that
publish and verify committed records: the codec a record is written in, the
roots and store references it commits to, the derivation and PushDrop outputs
that lock it, the carrier and mined transactions that put it on chain, the
clients a producer and a reader talk to, and the SPV check a reader runs. An
application supplies its own schema, derivation, tags and wallet profile as
parameters; nothing here names one.

**Status:** pre-1.0. Every application pins an exact tag, and the API may
change between v0 minor versions. One tag versions both languages. See
[docs/versioning.md](docs/versioning.md).

| Package | What it provides |
|---|---|
| `cbor` | Deterministic CBOR, an RFC 8949 subset; the decoder refuses anything non-canonical |
| `commit` | RFC 6962 Merkle roots, inclusion paths and their verification |
| `store` | Store reference entries, manifests, and the rule that computes a store's root from its entry and its members' commitments |
| `pushdrop` | BRC-42/43 derivation under counterparty Anyone, and tagged PushDrop locks, unlockers and decoding |
| `carrier` | An unmineable carrier transaction that commits a payload, and the funding lock, decode and sweep it spends from |
| `mint` | Builders for a state transition, a funding tree and a payment, with a fee loop that signs to measure the size and rebuilds at the rate until the fee covers it |
| `funding` | Funding-tree state kept between runs, and the BEEF kept for transactions spent before they mine |
| `guard` | A bounds check on a BRC-74 BUMP before the SDK allocates for it |
| `nodeapi` | A Teranode JSON-RPC and asset API client with bounded responses and a txid-in-proof check |
| `bwallet` | An embedded BRC-100 wallet backend and coin pool, a Signer, and BRC-29 derivations, keyed by an application profile |
| `wirewallet` | A BRC-100 wallet over the wallet wire, loopback only, and a handler that serves one |
| `publish` | The settlement leg (EF to an ingress, hex to a node RPC, arcade) and the BEEF object leg to an overlay host, which never share a socket, plus a transition journal |
| `headers` | A chain tracker over the header API of [overlay-bridge](https://github.com/lightwebinc/overlay-bridge) |
| `hostset` | Host sources and quorum fan-out across the addresses behind one overlay host |
| `lookup` | A BRC-24 lookup client for output-list answers |
| `resolve` | BRC-169 handle resolution and BRC-180 overlay discovery, under a strict HTTPS client policy |
| `knownkeys` | The grammar and store of a pinned-key file: pin, rotate, retire, forget |
| `verify` | The refusal vocabulary, SPV verdicts on one transaction, and the carrier check a reader runs |
| `goldentest` | Test helpers: a fixed key, hex and transaction parsing that fail the test, and a stub chain tracker |

The TypeScript package under [ts/](ts/), `@lightwebinc/bcommon`, holds the
twins an overlay topic manager or lookup service needs, tested against the
same vectors as the Go packages. It has two entry points:

| Entry point | What it provides |
|---|---|
| `@lightwebinc/bcommon` | Deterministic CBOR, store refs entries, the reader's BRC-42 derivation, PushDrop field signatures, the funding decode, the carrier check, and the overlay engine interfaces a module satisfies. It imports nothing but its peer `@bsv/sdk` and nothing from `node:`, so a browser can load it as well as a host |
| `@lightwebinc/bcommon/testing` | Test helpers for Node: a counting host, restore rows and storage, BEEF built as the engine builds it, a minter over a test key, and a simulator that calls a lookup service in the engine's order |

**Requirements:** Go 1.26.2 or later, and github.com/bsv-blockchain/go-sdk
pinned at exactly v1.5.2, the library's only direct dependency. See
[docs/dependencies.md](docs/dependencies.md). The TypeScript package needs
Node 24 and `@bsv/sdk` 2.7.1 exactly, as a peer; `make ts-test` builds and
tests it, and nothing in the Go build needs Node.

**Vectors:** the tests compare the library's output byte for byte with
vectors from an independent generator. See [docs/vectors.md](docs/vectors.md).

**Licence:** Apache-2.0 ([LICENSE](LICENSE)). Third-party notices are in
[NOTICE](NOTICE) and [LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
