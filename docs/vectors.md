# Vectors

The packages' tests compare what the library produces with vectors written
by a separate generator, byte for byte. A vector the library wrote itself
would prove only that the library agrees with itself; these come from code
that shares nothing with it.

## The generator

`tools/vectors` is a Go module of its own. It never imports this library,
and nothing in the library imports it:

- CBOR is encoded by [fxamacker/cbor](https://github.com/fxamacker/cbor)
  under its core deterministic options (RFC 8949 §4.2.1).
- RFC 6962 roots and audit paths come from its own implementation of the
  RFC's definitions. Every path is checked with the bit-by-bit verification
  of RFC 9162 §2.1.3.2 before it is written.
- Transactions are built with go-sdk's primitives and its P2PKH and PushDrop
  templates, called directly. Before it is written, every transaction in
  `transactions-v1.json` but the stand-in coin passes go-sdk's SPV check:
  its scripts run, and its ancestry proves against the two stand-in blocks
  the generator builds. The unlocking cases record go-sdk's SPV verdict
  instead of requiring a pass, since most of them are meant to fail it.

Being a separate module keeps the library at one direct dependency. The
generator's own requirements are checked by `make vectors`: exactly go-sdk,
at the library's pin, and fxamacker/cbor. `TestBoundaryFiles` refuses any
other import in the generator, and any import of this library. Keep the
generator out of any `go.work`, and run it with `GOWORK=off`, so that go-sdk
resolves to the pinned version and not to whatever a workspace holds.

Every vector is deterministic. The inputs are fixed, the key is the fixed
test key, and go-sdk signs with RFC 6979 nonces.

## The families

Each family is one JSON file in `testdata/vectors`. Each records every input
needed to rebuild it, as well as the bytes.

| File | What it holds | Checked by (Go; TypeScript) |
|---|---|---|
| `cbor-v1.json` | A nested value, described item by item with its major type, and its canonical encoding. Its maps have integer and text keys listed out of canonical order. It has integers at every head boundary in both signs out to the widest a uint64 and an int64 hold, byte and text strings at every head width up to two bytes, the simple values, empty containers, and containers nested eight deep. | `cbor`; `cbor.ts` |
| `manifest-v1.json` | A manifest body: the one-key map whose `members` array lists four members as `c`, `name`, `size`, `type` maps. One member is ordinary, one has an empty name and type and a zero size, one has a 64-byte name and a size past 32 bits, and one has a name outside ASCII. Also the RFC 6962 root over the members' commitments. | `store`; `cbor.ts`, the body only |
| `refs-v1.json` | A refs array of three entries: one without a head, a one-member store whose root is the leaf hash of its head, and one with two members the format does not define. | `store`; `store.ts` |
| `rfc6962-v1.json` | Seventeen fixed leaves, the root of the first n for every n from 1 to 17, and the audit path of every leaf in each tree. | `commit` |
| `transactions-v1.json` | A mined coin, a funding tree of four outputs spending it, two carriers spending the tree, a state token created and then updated, a payment, and two sweeps of the tree: one the tree pays for, and one with a fee input and change. Also the derived keys, the locks, the tree's kept BEEF, and both proofs. | `pushdrop`, `carrier`, `mint`, `funding`; `derive.ts`, `fieldsig.ts`, `funding.ts`, `carrier.ts` |
| `unlocking-v1.json` | The first carrier of `transactions-v1.json` with its one unlocking script rewritten: the canonical script, which alone is accepted, and a high S, `OP_PUSHDATA1` and `OP_PUSHDATA2` for a short push, `OP_0` or a byte pushed before the signature, the signature pushed twice, `OP_NOP` after or before it, R or S padded with a zero, a negative R, a sequence length one too long, a byte after the sequence, a zero S, R equal to the group order, the sighash bytes 0x01 and 0xc1, no sighash byte, an empty script, and a carrier of two inputs. Each case records its txid and whether go-sdk's interpreter still accepts the spend: the ones it accepts are spends anyone could make without the key. | `carrier`; `carrier.ts` |

The transaction family is built under a derivation and tags that belong to
no application: protocol `vector sample` at security level 1, key ids
`object` and `state`, funding tag `vx` 0x02 and state tag `vx` 0x01. The key
is 32 bytes of 0x42, `goldentest.FixedKey`. It is a test key and must never
be used for anything real. Fees are one satoshi per byte with a 250 satoshi
floor.

The JSON records only what an application chooses. The rest of the
mechanism is fixed in the library, so it is not a field, and a rebuild in
another language needs it too:

- Every derivation is BRC-42 under the recorded protocol and key id with
  counterparty Anyone. The producer's wallet locks with forSelf true; a
  reader derives the same key from the Anyone key with the identity key as
  counterparty and forSelf false.
- Every PushDrop is lock-before: `<key> OP_CHECKSIG`, then the fields, each
  pushed minimally, then enough `OP_2DROP` and `OP_DROP` to clear them. A
  signed lock's last field is the wallet's DER signature, under the derived
  key, over SHA-256 of the other fields concatenated. The carrier's record
  output and the state token are signed; a funding output carries the
  funding tag alone, unsigned.
- A PushDrop output is spent by a signature from the derived key with
  SIGHASH_ALL|FORKID (0x41). The coin's P2PKH outputs are spent with the
  same sighash type.
- A carrier's nLockTime is 4102444800 (2100-01-01T00:00:00Z) and its one
  input's sequence is 0. Its one output carries the whole value of the tree
  output it spends, so it pays no fee.
- Every transaction that pays a fee settles it the same way. The fee
  starts at the floor. After each signing the target is the signed size
  times the rate, or the floor if that is more, and the transaction is
  accepted once its fee meets the target. Otherwise it is rebuilt with the
  fee set to the signed size plus two bytes per input, times the rate, or
  the floor if that is more. Change under the floor goes to the fee. A
  sweep without a fee input takes its fee from the tombstone.

## The TypeScript package

The TypeScript package's tests (`ts/src/vectors.test.ts`) read the same
files from `testdata/vectors`, never a copy, and hold its twins to the same
bytes: the CBOR value and the manifest body encode to the vector and decode
back to it, the refs entries do the same through the refs codec, and the
transaction family's derived keys, funding outputs, carriers and state-token
signatures decode and verify as the Go side built them, and every
unlocking case is accepted or refused as the Go side decides it. The SDK's own
PushDrop is also checked to write the vector's locks byte for byte. The
kept BEEF is read rather than compared, because go-sdk writes Atomic BEEF
V2 and the TypeScript SDK writes V1. The transaction builders have no
TypeScript twin: their transactions are parsed and re-serialised against
each txid, not rebuilt. RFC 6962 roots are not checked on the TypeScript
side.

## Running it

```
make vectors          # regenerate and compare byte for byte; part of make verify
make vectors-update   # regenerate and write testdata/vectors
make ts-test          # the TypeScript package's tests, which read them too
```

`make vectors` fails on any difference, and on a file in `testdata/vectors`
that nothing generates.

## When a vector changes

A vector changes only when the generator changes or go-sdk does. Both are
deliberate, and the diff needs review before it is committed.

- A change to the generator that moves a vector must be matched by the
  library's output moving the same way. Output bytes that an application has
  already committed to the chain must not move (see
  [versioning.md](versioning.md)).
- The TypeScript tests read the same files, so a vector that moves is
  re-checked there with `make ts-test`, and CI runs both.
- Raising the go-sdk pin re-runs both sides on the new version. A vector
  that moves means the raise changes bytes applications depend on (see
  [dependencies.md](dependencies.md)).
