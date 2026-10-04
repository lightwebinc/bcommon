# @lightwebinc/bcommon

The TypeScript twins of the [bcommon](https://github.com/lightwebinc/bcommon)
Go library that an overlay topic manager or lookup service needs to read what
a producer wrote, tested against the same vectors as the Go packages.

| Entry point | What it provides |
|---|---|
| `@lightwebinc/bcommon` | `encode`/`decodeValue` (deterministic CBOR), `encodeRefs`/`decodeRefs` (store refs entries), `readerLockingKey` (the reader's BRC-42 derivation), `strictPublicKey`/`strictPublicKeyHex` (a key only in its canonical encoding), `decodeStrictPushDrop` (a PushDrop only as the template writes it), `firstPush`/`pushFields`/`pushDropScript` (a lock read leniently and rebuilt canonically from raw script bytes), `recordReader` and `claimsRecord` (the bounded, ordered reading of an application record), `verifyFieldSignature`, `strictSignature`, `decodeFunding`, `decodeCarrier` and its rules, `checkBEEF` and `readWire` with `tokenShape`, `carrierShape`, `aloneShape`, `tokenBEEF` and the token output readers (the BEEF a host admits a mined token, a carrier and a sweep in, read as declared), `filterText`/`filterBytes` (the renderer filter a web page runs over text someone else wrote, over the same Unicode 15.1 table as the Go `sanitize` package; the table's data is Unicode's, under the licence NOTICE carries), and the engine interfaces a module satisfies (`Module`, `ModuleHost`, `TopicManager`, `LookupService`, ...) |
| `@lightwebinc/bcommon/testing` | `countingHost`, `row`, `fakeStorage`, `spends`, `wireParent`, `beefOf`, `atomicOf`, `minter`, hex helpers, and `engineOrder`, which calls a lookup service's callbacks in the order and with the payloads the engine uses |

**Runtime rules.** Every file behind the runtime entry point imports nothing
but `@bsv/sdk` and nothing from `node:`, and compiles without Node's types,
so a browser can load it as well as a host. The testing entry point is for
Node. The package declares no dependency of its own; `@bsv/sdk` is a peer
at exactly 2.7.1. npm installs the peer beside the package when it is
missing, so a module shares the host's copy only when its bundle keeps
`@bsv/sdk` external.

**Versioning.** The package's version equals the repository tag it ships
in; see [docs/versioning.md](https://github.com/lightwebinc/bcommon/blob/main/docs/versioning.md).

**Build and test** (Node 24):

```
npm ci
npm run check    # type-check, and the runtime files without Node's types
npm test         # builds dist/ and runs the tests on it
```

**Licence:** Apache-2.0. See LICENSE and NOTICE, which `npm pack` copies in
from the repository root.
