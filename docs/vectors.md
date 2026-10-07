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
  RFC's definitions, over leaves of any length. Every path is checked with the bit-by-bit verification
  of RFC 9162 §2.1.3.2 before it is written.
- Transactions are built with go-sdk's primitives and its P2PKH and PushDrop
  templates, called directly. Before it is written, every transaction in
  `transactions-v1.json` but the stand-in coin passes go-sdk's SPV check:
  its scripts run, and its ancestry proves against the two stand-in blocks
  the generator builds. The unlocking cases record go-sdk's SPV verdict
  instead of requiring a pass, since most of them are meant to fail it.
- The BEEF, public-key, PushDrop and chain-token cases are built from the
  transaction family's bytes, by hand where a case breaks one thing. The
  chain-token family's blocks are merkle trees the generator computes
  itself; every path is cut from one and checked to give its root before it
  is written, and the merged path of two transactions in one block is
  checked against what go-sdk's own merge gives.
- The CHIRP family is written by the generator's own node writer, profile 1
  grouping and Base58Check (over `math/big`), from BRC-167's tables. Before
  it writes, it reproduces every golden value the BRC prints: the three
  roots with their hashes and identifiers, the identifier of the `hello`
  blob, and the tree shapes over 256, 257 and 65,537 blobs. Each refusal
  case is labeled by its construction: it breaks one rule of a valid node
  or closure.
- The keyed family uses the standard library alone: SHA-256, `math/big` and
  AES-256-GCM with a 32-byte nonce. It recomputes the symmetric key and the
  commitment BRC-369 prints for its content key and fails if either stops
  matching. Each verdict is
  the case's construction, and the key verdicts come from the generator's
  own range and curve check with `math/big`. Each case also records what
  go-sdk makes of it, so a case the library refuses and the SDK takes is
  visible as such.

- The segment family uses the standard library alone too: AES-256-GCM
  with a 12-byte nonce, the salt and eight zero bytes, under the symmetric
  key it computes with SHA-256; and the epoch wrap as SHA-256 of the domain
  string, the epoch's symmetric key and the content id, sealed with a
  32-byte nonce. It wraps under both registered epoch-wrap domain strings
  and fails if they give one wrap.
- The renderer filter's corpus comes from a second implementation of the
  filter's four rules in the generator, which reads the vendored Unicode
  files under `third_party/unicode/15.1` directly, never the generated
  table, and scans escape sequences with its own functions. Most cases also
  carry the output the rules alone fix, and the generator fails before it
  writes anything when its filter does not reach it. The generator also
  writes the table itself, to `sanitize/unicode-15.1.json` and, as the same
  bytes in one string literal, `ts/src/sanitize-table.ts`; `make vectors`
  checks both.

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
| `bytetree-v1.json` | Twelve leaves of 0 to 4,096 bytes, the root of the first n for every n from 0 to 12 (the empty root included) and the audit path of every leaf; then content of 0, 1, 4,095, 4,096, 4,097, 12,293, 4,194,304 and 4,194,305 bytes cut into 4,096-byte segments, the last unpadded, with its segment count, root, and the compact path (hashes without sides, with its length) of the first, middle, second-last and last segment. The content is SHA-256("bcommon vector content" \|\| uint32be(j)) for j = 0, 1, ... concatenated. | `commit`; `commit.ts` |
| `chirp-v1.json` | BRC-167's three golden roots (empty, `hello`, `hello` with `text/plain`) with their hashes, identifiers and URLs, reproduced from the BRC by the generator before it writes; profile 1 closures of content of 0, 1, 4 MiB - 1, 4 MiB and 4 MiB + 1 bytes and of one blob twice and a tail, with every blob hash, branch and the distinct object count; BRC-167's tree shapes over 256, 257 and 65,537 synthetic blobs, the widths and root children the BRC prints; 36 nodes, admitted or refused for the first rule broken (size, truncation, magic, version, kind, profile 0, a non-minimal CompactSize, 257 children and none, child kind, lengths, the empty-content rule, a trailing byte, and extensions out of order, repeated, of type 0, critical, `mediaType` on a branch or malformed, and over 16,384 value bytes); 18 closures checked object by object, passing or refused (missing, a host's wrong bytes, a refused node, too large, over the reference bound, sixteen nodes deep and seventeen, a blob or a branch of the wrong length, the content hash, and two non-canonical groupings), with a profile 2 closure verified but not called canonical; and identifiers and URLs, admitted and refused. The generator's CHIRP writer and its Base58Check are its own. | `chirp`; `chirp.ts` |
| `transactions-v1.json` | A mined coin, a funding tree of four outputs spending it, two carriers spending the tree, a state token created and then updated, a payment, and two sweeps of the tree: one the tree pays for, and one with a fee input and change. Also the derived keys, the locks, the tree's kept BEEF, and both proofs. | `pushdrop`, `carrier`, `mint`, `funding`; `derive.ts`, `fieldsig.ts`, `funding.ts`, `carrier.ts` |
| `beef-v1.json` | The first carrier as Atomic BEEF V2, BEEF V1 and BEEF V2, V2 with its parent as a bare txid, the kept funding tree, and a V1 and a V2 built by hand; then 13 bytes declaring 2^63 BUMPs, a BUMP declaring 2^32-1 leaves at one level, 2^32-1 transactions, a transaction declaring 2^32-1 inputs, input and output scripts longer than the bytes, a transaction of no inputs, no transactions, BUMP tree heights 65 and 0, a has-BUMP byte of 2, BUMP indexes past the BUMPs in V1 and V2, V2 data format 3, an unknown version, Atomic around Atomic, one byte short, one trailing byte, and nothing. Each records whether the guard admits it and whether go-sdk parses it. | `guard`; `wire.ts` (the structural walk) |
| `pubkeys-v1.json` | The test identity key, its other parity, x = 1 under both prefixes, then 02 and 03 \|\| p+1 (aliases of x = 1), 02 \|\| p, 02 \|\| 2^256-1, x = 5 (off the curve), the identity key uncompressed and hybrid, prefixes 04 and 00 on 33 bytes, 32 and 34 bytes, and nothing. Each records the strict verdict and whether go-sdk parses it. | `guard`; `pubkey.ts` |
| `pushdrop-v1.json` | The first carrier's record lock, the funding lock and the created state token's lock, each canonical and rewritten: the key with `OP_PUSHDATA1`, the first field with `OP_PUSHDATA1`, the last with `OP_PUSHDATA2`, `OP_NOP` after the drops, no drops, every field dropped with `OP_DROP`, the key uncompressed, the key replaced by x = 1 (canonical, so taken) and by 02 \|\| p+1. Each records the verdict and whether go-sdk's decoder reads the canonical fields from it. | `pushdrop`, `carrier`; `pushdrop.ts`, `funding.ts`, `carrier.ts` |
| `unlocking-v1.json` | The first carrier of `transactions-v1.json` with its one unlocking script rewritten: the canonical script, which alone is accepted, and a high S, `OP_PUSHDATA1` and `OP_PUSHDATA2` for a short push, `OP_0` or a byte pushed before the signature, the signature pushed twice, `OP_NOP` after or before it, R or S padded with a zero, a negative R, a sequence length one too long, a byte after the sequence, a zero S, R equal to the group order, the sighash bytes 0x01 and 0xc1, no sighash byte, an empty script, and a carrier of two inputs. Each case records its txid and whether go-sdk's interpreter still accepts the spend: the ones it accepts are spends anyone could make without the key. | `carrier`; `carrier.ts` |
| `record-v1.json` | One sample record under the test magic `vxr` `0x01`, item by item, with the plan a reader follows over its keys, and the ways a record is refused, each for the first rule it breaks: over its bound, not canonical CBOR, not a map, more than 64 entries, a key that is not an unsigned integer, the wrong magic, a key missing, of the wrong type, out of range, or a list over its bound, out of order or with a wrong element; cases that break two rules, to pin the order; records with preserved keys and at each bound; and what claims a record from its head alone. The accepted records come from the independent encoder; the malformed ones are written by hand. | `record`; `record.ts` |
| `keyed-v1.json` | The content key BRC-369 prints, with the symmetric key and commitment the document prints for it, recomputed with SHA-256 alone; keys at and past each end of the scalar range; plaintexts a holder checks against a commitment, each refused for the first check it fails; and BRC-2's symmetric form, sealed with the standard library's AES-256-GCM under a 32-byte nonce, with the ways a sealed value fails to open. | `keyed`; no TypeScript twin |
| `keyed-segment-v1.json` | One BRC-369 segment sealed under three keys and six salts, plaintexts of 1, 15, 16, 17, 64, 300 and 16384 bytes, each with its IV; the ways a segment fails to open (another key or salt, a flipped bit, a cut or added byte, a tag alone, nothing, a key that is not a scalar), each with its refusal; the wrap of a content key under two epoch keys and both registered epoch-wrap domain strings, with the epoch's symmetric key and the wrapping key; and unwraps under the other domain string, another epoch, another content id, another commitment, a short wrap, and wraps of another scalar and of 31 bytes. | `keyed`; no TypeScript twin |
| `sanitize-v1.json` | The renderer filter's corpus, pinned to the table by its SHA-256: 83 inputs, as bytes, and the bytes the filter returns for each. Tabs, U+2028 and U+2029; C0, DEL and C1 controls; color, title, hyperlink, DCS, charset and reset sequences, terminated and not, and C1 CSI; every bidirectional control and every listed zero-width and invisible character; supplementary variation selectors; tag characters: the flags of England, Scotland and Wales kept whole, and runs the table does not list, cut short, too long, after a letter or alone removed; heart on fire, the rainbow and transgender flags, a family, skin tones before and across a joiner, and joiners that join nothing; selector runs, U+FE00 to U+FE0D, keycaps, and selectors after an unlisted base; invalid UTF-8 of each kind; and private use and noncharacters, which stay. | `sanitize`; `sanitize.ts` |
| `chaintoken-v1.json` | The state token of the transaction family, created and updated, in stand-in blocks of two, four and eight transactions, and the BEEF each shape needs, built by hand: a token and its parent as V1 and V2, in two blocks or one (one path, the union of the two minimal paths, with and without the siblings a reader computes, and with the two each other's sibling); a carrier and its funding tree as go-sdk writes them and by hand; a sweep alone. Then each broken one way: an Atomic BEEF of a token, a token alone, an unrelated transaction riding along or in the parent's place, a parent or a token unproven, a bare txid, a path with a leaf it does not need, one leaf twice, two paths of one block, a third txid flag, a sibling missing, a BUMP nothing names, and bytes that are no BEEF with its subject. Also the BEEF a replayer assembles from two stored tokens, a token output read and held to its key (wrong value, wrong field count, a wider push, a trailing opcode, a high-S signature, a signature over other bytes, another key, another tag), and signature encodings, strict or not. | `chaintoken`, `pushdrop`; `wire.ts`, `script.ts` |

The transaction family is built under a derivation and tags that belong to
no application: protocol `vector sample` at security level 1, key ids
`object` and `state`, funding tag `vx` 0x02 and state tag `vx` 0x01. The key
is 32 bytes of 0x42, `goldentest.FixedKey`. It is a test key and must never
be used for anything real. Fees are one satoshi per byte with a 250 satoshi
floor (`mint.LegacyFees`), named explicitly so that a change of
`mint.DefaultFees` never moves a pinned byte.

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
signatures decode and verify as the Go side built them, every
unlocking case is accepted or refused as the Go side decides it, every key
case is taken or refused by `strictPublicKey` as by `guard.ParsePubKey`, and
every PushDrop case by `decodeStrictPushDrop` (with `decodeFunding` and
`inspectScript` on the funding and record cases) as by
`pushdrop.CheckCanonical`. The TypeScript SDK takes the key aliases too, and
writes them back reduced. Every BEEF case is admitted or refused by
`checkBEEF` as by `guard.CheckBEEF`. Every record case is accepted or
refused by `recordReader` for the reason the Go `record` gives, read whole
and one step at a time, and an accepted one re-encodes to its bytes. Every
chain-token shape is accepted or refused as the Go `chaintoken` decides it,
with the same subject, parent, spends and proofs; a case the vector refuses
may be refused at the read in one language and at the shape in the other,
because the TypeScript SDK refuses some paths (one offset listed twice) that
go-sdk reads, and either way it is refused as a BEEF. `tokenBEEF` assembles
each replay byte for byte, and every token output and signature case reads
as it does in Go. Every renderer-filter case gives the Go bytes through
`filterBytes`, and through `filterText` where the input is UTF-8, and
the table module holds the bytes the Go package embeds. The SDK's own
PushDrop is also checked to write the vector's locks byte for byte. The
kept BEEF is read rather than compared, because go-sdk writes Atomic BEEF
V2 and the TypeScript SDK writes V1. The transaction builders have no
TypeScript twin: their transactions are parsed and re-serialized against
each txid, not rebuilt. RFC 6962 roots are not checked on the TypeScript
side.

## Running it

```
make vectors          # regenerate and compare byte for byte; part of make verify
make vectors-update   # regenerate and write testdata/vectors and the filter's table
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
