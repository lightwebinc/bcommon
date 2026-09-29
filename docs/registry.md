# Tag and protocol registry

Applications built on bcommon put identifiers of their own on chain, into
wallets and onto overlay hosts: a derivation protocol and its key ids, the
tag a PushDrop output or an `OP_RETURN` payload starts with, a record's magic
bytes, overlay topic and lookup service names, and wallet basket names. The
library takes all of them as parameters and never chooses one. This file is
where they are chosen, so that no two applications collide.

A collision is not cosmetic:

- Two applications deriving under the same protocol name and key id derive
  the same keys from the same identity. A wallet signing for one signs for
  the other, and each reads the other's outputs as its own.
- Two applications starting outputs with the same tag are indistinguishable
  to a topic manager. A host admits the other application's outputs, or
  refuses its own as malformed, and a kill switch that watches for a spent
  funding output fires on the wrong spend.
- Two applications naming the same topic or lookup service on one host mount
  over each other.
- A shared basket name mixes two applications' coin in every wallet that
  holds both.

Every identifier below is frozen once an application has committed to it on
chain: records and outputs already written are verified against it for as
long as anyone reads them. Changing one after that is a new identifier, never
an edit.

## Rules

- **Protocol names** are BRC-43 protocol ids: at least five characters of
  lowercase letters, digits and single spaces. go-sdk refuses a shorter name
  at derivation time, so a four-letter application name needs a longer
  protocol name. The security level is part of the id: `[1, "name"]` and
  `[2, "name"]` are different protocols.
- **Key ids** are the application's own within its protocol name. Record them
  here when they are fixed strings, and describe their shape when they are
  derived per object.
- **Tags** are a two-byte ASCII prefix owned by one application, followed by
  one type byte the application assigns (`bf` `0x01`, `bf` `0x02`, ...). A
  prefix is registered once and every type under it belongs to that
  application. `OP_RETURN` payload tags follow the same rule at their own
  length.
- **Record magic** is the application's tag prefix, a letter and a version
  byte, so it cannot be mistaken for another application's record.
- **Topics and lookup services** are `tm_<name>` and `ls_<name>`, with a name
  derived from the application's. Lab or test topics add a suffix
  (`tm_<name>_lab`) and are never used in production.
- **Baskets** start with the application's name.

An application registers its identifiers here before it freezes its contract,
in the same change that adds them to its code, and a reviewer checks the new
rows against every existing one.

## Registered

### bfinger

A finger directory: an identity's record, committed on chain by a mined state
token and served by overlay hosts. **Frozen** (committed on chain).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bfinger"]` | Every bfinger derivation |
| Key id | `profile` | The state token's lock |
| Key id | `record` | The record carrier's lock and field signature |
| Key id | `fund` | The embedded wallet's funding key |
| Tag | `bf` `0x01` | State token (`[tag, commitment]`) |
| Tag | `bf` `0x02` | Funding-tree output |
| Record magic | `bfr` `0x01` | Committed record, version 1 |
| Topic | `tm_finger` | Topic manager for tokens, carriers and funding outputs |
| Lookup service | `ls_finger` | Lookup by identity key and by carrier |
| Baskets | `bfinger fund`, `bfinger record funding`, `bfinger state`, `bfinger kill tombstone` | Wallet baskets |

### bgateway

A publishing gateway: funding trees and carriers for forwarded objects.
**Provisional** (not yet committed on chain; assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bgateway"]` | Tree outputs and carrier inputs |
| Key id | `flow` | Tree outputs and carrier inputs |
| Tag | `gw` `0x01` | Funding-tree output |
| Tag | `gw` `0x04` | Registration output of a funding tree |
| Topic | `tm_bgw_lab` | Lab topic only |

The prefix `gw` is registered to bgateway. The design's earlier `bg` prefix
is not used, because it collided with another planned application's.

### bflow

Pay-per-flow payment channels that settle metered usage.
**Provisional** (not yet committed on chain; assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[2, "pay per flow"]` | Every channel derivation |
| Key ids | `<channel id> ...` | Derived per channel and leg (settlement, funding, change, refund, lane) |
| `OP_RETURN` tag | `PPF`, version `0x02` | Settlement commitment payload |

## Not registered

bcommon itself registers nothing: every package that derives, tags or names
takes the value from its caller. Its test vectors use `[1, "vector sample"]`
and the prefix `vx` (`vx` `0x01`, `vx` `0x02`); both are reserved for tests
and no application may use them.
