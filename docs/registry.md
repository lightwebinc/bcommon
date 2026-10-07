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
  derived from the application's. An application topic that more than one
  independent user can create MUST carry entropy: a random suffix of
  letters, chosen once when the topic is created, so that unrelated users
  cannot collide on a shared object plane. Names follow BRC-87: lowercase
  letters and underscores only, no leading, trailing or doubled underscore,
  and at most 50 characters, suffix included. A reader never identifies such
  a topic by its readable part alone; it holds the whole name, suffix and
  all.
- **Baskets** start with the application's name.
- **Certificate types** are BRC-52 type ids, made by BRC-169 section 4.5's
  rule: the standard base64 of SHA-256 of a derivation string. The string
  starts with the application's name, names the certificate and ends with a
  version (`"<application> <certificate> v1"`), so two applications cannot
  derive one type, and a change to a certificate's field names or meanings is
  a new string and so a new type (BRC-52 gives one type one set of field
  names and meanings). Record the string and the type it derives.
- **Domain strings** are fixed strings an application hashes into an
  identifier or a key, so that a hash made for one purpose is never one made
  for another. A domain string starts with the application's name, names
  what it derives and ends with a version (`"<application> <purpose> v1"`).
  Record each one: two applications hashing the same string over the same
  inputs derive the same identifier.
- **Borrowed protocols** are protocols another specification defines and an
  application uses as written, such as a BRC's own derivation. They are not
  the application's and are not registered to it; a note under the
  application records the use, so that a reviewer can see two applications
  sharing one on purpose.

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
| Key id | `fund` | The publisher's funding key |
| Tag | `gw` `0x01` | Funding-tree output |
| Tag | `gw` `0x04` | Registration output of a funding tree |
| Record magic | `gwr` `0x01` | Batch record, version 1 |
| Basket | `bgateway fund` | Wallet basket |

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

### blogs

A distributed logging application.
**Provisional** until its first publish (not yet committed on chain;
assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "blogs"]` | Every blogs derivation |
| Key id | `batch` | The batch carrier's output, its field signature, and funding outputs |
| Key id | `anchor` | The anchor token's lock |
| Key id | `fund` | The embedded wallet's funding key |
| Tag | `bl` `0x01` | Anchor token |
| Tag | `bl` `0x02` | Funding-tree output |
| Record magic | `blb` `0x01` | Batch record, version 1 |
| Record magic | `bla` `0x01` | Anchor record, version 1 |
| Topics | `tm_log_<name>_<suffix>` | One topic per log stream; `<suffix>` is 10 random lowercase letters chosen when the stream is created |
| Lookup service | `ls_log` | Lookup service for log streams |
| Baskets | `blogs fund`, `blogs batch funding`, `blogs anchor`, `blogs kill tombstone` | Wallet baskets |

blogs owns the `tm_log_` topic namespace: no other application names a
topic that starts with it. A stream's topic carries entropy under the rule
above, because anyone can create a stream; with `tm_log_`, the separating
underscore and the suffix taking 18 characters, `<name>` has at most 32.

### bbox

A message box replicated by overlay hosts: encrypted envelopes and signed
receipts, each on an unmined carrier.
**Provisional** until its first publish (not yet committed on chain;
assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bbox message"]` | Every bbox derivation (`[1, "bbox"]` is under five characters and cannot derive) |
| Key id | `envelope` | The record output of every carrier (envelopes and receipts), its field signature, and funding outputs |
| Key id | `signature` | The BRC-169 envelope signature carried in an envelope's content |
| Key id | `fund` | The embedded wallet's funding key |
| Tag | `bb` `0x02` | Funding-tree output |
| Record magic | `bbe` `0x01` | Envelope record, version 1 |
| Record magic | `bbr` `0x01` | Receipt record, version 1 |
| Topics | `tm_bbox_<name>_<suffix>` | One topic per office; `<suffix>` is 10 random lowercase letters chosen when the office is created |
| Lookup service | `ls_bbox` | Lookup service for every office on a host |
| Host route | `<base>/ls_bbox/terms` | The host terms document for priced questions |
| Baskets | `bbox fund`, `bbox envelope funding`, `bbox kill tombstone` | Wallet baskets |

The prefix `bb` is registered to bbox. `0x65` and `0x72` are never assigned
as tag type bytes under it, because `bb` `0x65` and `bb` `0x72` are the
first three bytes of the record magics `bbe` and `bbr`.

bbox owns the `tm_bbox_` topic namespace: no other application names a topic
that starts with it. An office's topic carries entropy under the rule above,
because anyone can create an office; with `tm_bbox_`, the separating
underscore and the suffix taking 19 characters, `<name>` has at most 31.

### borg

Organizations and groups replicated by overlay hosts: a membership roll and
groups as chains of mined tokens, membership grants (BRC-52 certificates) and
epoch-key wraps on unmined carriers.
**Provisional** until its first publish (not yet committed on chain;
assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "borg organisation"]` | Every borg derivation (`[1, "borg"]` is under five characters and cannot derive) |
| Key id | `org` | An organization token's lock and field signature |
| Key id | `group` | A group token's lock and field signature |
| Key id | `record` | A wrap carrier's record output and field signature, and the outputs of funding trees that fund wraps |
| Key id | `grant` | A grant carrier's record output and field signature, and the outputs of funding trees that fund grants |
| Key id | `chain` | The outputs of funding trees that pay for a chain's first token; never a carrier's |
| Key id | `fund` | The embedded wallet's funding key |
| Key id | `signer` | The organization's signer key: a member key every instance of the organization's tooling derives from the organization's identity key, to which an epoch key is wrapped like any member's, so that a second device catches up on a key the first one sampled. No host or reader rule reads it |
| Tag | `bo` `0x01` | Organization token, first field |
| Tag | `bo` `0x02` | Funding-tree output |
| Tag | `bo` `0x03` | Group token, first field |
| Record magic | `boo` `0x01` | Organization record, version 1 |
| Record magic | `bog` `0x01` | Group record, version 1 |
| Record magic | `bom` `0x01` | Membership grant record, version 1 |
| Record magic | `bow` `0x01` | Epoch-key wrap record, version 1 |
| Topics | `tm_borg_<name>_<suffix>` | One topic per hall, shared by many organizations; `<suffix>` is 10 random lowercase letters chosen when the hall is created |
| Lookup service | `ls_borg` | Lookup service for every hall on a host |
| Host route | `<base>/ls_borg/terms` | The host terms document for priced questions |
| Baskets | `borg fund`, `borg grant funding`, `borg record funding`, `borg chain funding`, `borg chain`, `borg kill tombstone` | Wallet baskets |
| Certificate type | `poEEbYGwpJbTzKTeSRBJzV31s8PNknNycAbyjDV3ALc=` | A membership grant: base64 of SHA-256 of `"borg membership grant v1"` |

The prefix `bo` is registered to borg. `0x67`, `0x6d`, `0x6f` and `0x77` are
never assigned as tag type bytes under it, because `bo` `0x67`, `bo` `0x6d`,
`bo` `0x6f` and `bo` `0x77` are the first three bytes of the record magics
`bog`, `bom`, `boo` and `bow`. `0x00` is not assigned.

borg owns the `tm_borg_` topic namespace: no other application names a topic
that starts with it. A hall's topic carries entropy under the rule above,
because anyone can create a hall; with `tm_borg_`, the separating underscore
and the suffix taking 19 characters, `<name>` has at most 31.

Borrowed, not registered: borg releases epoch keys under BRC-369's
`[2, "keyed content release"]` with key id the standard base64 of
`SHA-256(epoch id || epoch)`, as BRC-369 section 5.2 defines it. Every
BRC-369 implementation shares that protocol; the key id separates each
group's each epoch.

### bsecret

Secrets replicated by overlay hosts: a vault as a chain of mined tokens, and
each secret version as a keyed object on an unmined carrier, its content key
wrapped to the owner's wallet and under a key derived from a borg group
epoch's key.
**Signed by its owner; provisional** until its first publish (not yet
committed on chain; assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bsecret"]` | Every public bsecret derivation (counterparty anyone) and the funding key |
| Key id | `vault` | A vault token's lock and field signature |
| Key id | `secret` | A secret carrier's record output and field signature, and the outputs of funding trees that fund secrets |
| Key id | `chain` | The outputs of funding trees that pay for a vault's first token; never a carrier's |
| Key id | `fund` | The embedded wallet's funding key |
| Protocol | `[2, "bsecret self"]` | The self wrap: a content key encrypted to the owner's own wallet, counterparty self |
| Key ids | the standard padded base64 of a content id | One self wrap; derived per secret version, so a wrap is bound to what it carries |
| Tag | `se` `0x01` | Vault token, first field |
| Tag | `se` `0x02` | Funding-tree output |
| Record magic | `sev` `0x01` | Vault record, version 1 |
| Record magic | `ses` `0x01` | Secret record, version 1 |
| Record magic | `sep` `0x01` | Payload (the plaintext a secret's ciphertext holds), version 1 |
| Topics | `tm_bsecret_<name>_<suffix>` | One topic per crypt, shared by many vaults; `<suffix>` is 10 random lowercase letters chosen when the crypt is created |
| Lookup service | `ls_bsecret` | Lookup service for every crypt on a host |
| Host route | `<base>/ls_bsecret/terms` | The host terms document for priced questions |
| Baskets | `bsecret fund`, `bsecret secret funding`, `bsecret chain funding`, `bsecret chain`, `bsecret kill tombstone` | Wallet baskets |
| Domain string | `bsecret vault id v1` | Hashed into a vault identifier |
| Domain string | `bsecret secret id v1` | Hashed into a secret identifier |
| Domain string | `bsecret epoch wrap v1` | Hashed into the key that wraps a content key under a group epoch |

The prefix `se` is registered to bsecret. `0x70`, `0x73` and `0x76` are
never assigned as tag type bytes under it, because `se` `0x70`, `se` `0x73`
and `se` `0x76` are the first three bytes of the record magics `sep`, `ses`
and `sev`. `0x00` is not assigned.

The prefix is `se` and not `bs`, because more than one planned application's
name begins `bs` and the prefix would name none of them. `bs` stays
unassigned: no application takes it. A later storage application should take
`st`, and must not take `se`.

bsecret owns the `tm_bsecret_` topic namespace: no other application names a
topic that starts with it. A crypt's topic carries entropy under the rule
above, because anyone can create a crypt; with `tm_bsecret_`, the separating
underscore and the suffix taking 22 characters, `<name>` has at most 28.

bsecret registers no certificate type: a reader of a vault is a member of a
borg group, and the membership grant is borg's.

Borrowed, not registered: bsecret hashes a content key into its symmetric
key and its commitment under BRC-369 section 2's domain strings,
`metanet keyed content symmetric v1` and
`metanet keyed content commitment v1`, as written; every BRC-369
implementation shares them. It derives nothing under BRC-369's
`[2, "keyed content release"]`: the epoch keys it wraps under were released
under that protocol by borg.

### bchat

Team chat replicated by overlay hosts: a workspace as a chain of mined
tokens that carries its channel directory, its moderation and the anchors
of its message intervals, and each message as a signed record on an unmined
carrier, keyed under a borg group epoch in a private channel.
**Signed by its owner; provisional** until its first publish (not yet
committed on chain; assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bchat"]` | Every public bchat derivation (counterparty anyone) and the funding key |
| Key id | `message` | A message carrier's record output and field signature, and the outputs of funding trees that fund messages |
| Key id | `workspace` | A workspace token's lock and field signature |
| Key id | `chain` | The outputs of funding trees that pay for a workspace's first token; never a carrier's |
| Key id | `fund` | The embedded wallet's funding key |
| Tag | `bc` `0x01` | Workspace token, first field |
| Tag | `bc` `0x02` | Funding-tree output |
| Record magic | `bcm` `0x01` | Message record, version 1 |
| Record magic | `bcw` `0x01` | Workspace record, version 1 |
| Record magic | `bcp` `0x01` | Payload (the text, mentions and attachments a message carries), version 1 |
| Topics | `tm_bchat_<name>_<suffix>` | One topic per workspace; `<name>` is at most 30 characters and `<suffix>` is 10 random lowercase letters chosen when the workspace is created |
| Lookup service | `ls_bchat` | Lookup service for every workspace on a host |
| Host routes | `<base>/ls_bchat/terms`, `<base>/ls_bchat/watch` | The host terms document for priced questions; the optional notification stream |
| Baskets | `bchat fund`, `bchat message funding`, `bchat chain funding`, `bchat chain`, `bchat kill tombstone` | Wallet baskets |
| Domain string | `bchat workspace id v1` | Hashed into a workspace identifier |
| Domain string | `bchat epoch wrap v1` | Hashed into the key that wraps a message's content key under a group epoch |
| Domain string | `bchat dm v1` | Hashed into a conversation id |
| bbox plaintext member | `bchat` | bchat's member of a bbox plaintext, which bbox preserves |
| bbox box | `bchat` | Where a direct message goes; a payment goes to `payment_inbox`, as BRC-33 uses it |

The prefix `bc` is registered to bchat. `0x6d`, `0x70` and `0x77` are never
assigned as tag type bytes under it, because `bc` `0x6d`, `bc` `0x70` and
`bc` `0x77` are the first three bytes of the record magics `bcm`, `bcp` and
`bcw`. `0x00` is not assigned.

bchat owns the `tm_bchat_` topic namespace: no other application names a
topic that starts with it. A workspace's topic carries entropy under the
rule above, because anyone can create a workspace; with `tm_bchat_`, the
separating underscore and the suffix taking 20 characters, `<name>` has at
most 30.

bchat registers no certificate type: a writer of a gated or private channel
is a member of a borg group, and the membership grant is borg's.

Borrowed, not registered: bchat hashes a content key into its symmetric key
and its commitment under BRC-369 section 2's domain strings,
`metanet keyed content symmetric v1` and
`metanet keyed content commitment v1`, as written. It sends direct messages
and payments as bbox envelopes, so it uses `[2, "message encryption"]`
through bbox and BRC-29's `[2, "3241645161d8"]` for payments, each as its
specification defines it. It derives nothing under BRC-369's
`[2, "keyed content release"]`: the epoch keys it wraps under were released
under that protocol by borg.

### bstore

Object storage as a market on overlay hosts: a bucket as a chain of mined
tokens committing to its key map, object entries, outcome records and front
statements on unmined carriers for hosts, the object's CHIRP (BRC-167)
pieces on a second topic for storage providers, and hold outputs that pay
providers who hold an object.
**Signed by its owner; provisional** until its first publish (not yet
committed on chain; assigned here).

| Kind | Value | Use |
|---|---|---|
| Protocol | `[1, "bstore"]` | Every bstore derivation (counterparty anyone) and the funding key |
| Key id | `bucket` | A bucket token's lock and field signature |
| Key id | `chain` | The outputs of funding trees that pay for a bucket's first token; never a carrier's |
| Key id | `entry` | An entry carrier's record output and field signature, and the outputs of funding trees that fund entries |
| Key id | `piece` | A piece carrier's record output and field signature, and the outputs of funding trees that fund pieces |
| Key id | `outcome` | An outcome carrier's record output and field signature, and the outputs of funding trees that fund outcomes |
| Key id | `front` | A front statement carrier's record output and field signature, and the outputs of funding trees that fund them |
| Key id | `hold` | A hold output's lock |
| Key id | `challenge` | The signature of a challenge response |
| Key id | `fund` | The embedded wallet's funding key |
| Tag | `st` `0x01` | Bucket token, first field |
| Tag | `st` `0x02` | Funding-tree output |
| Tag | `st` `0x03` | Hold output, its one field |
| Tag | `st` `0x04` | Reserved; not assigned in this version |
| Record magic | `stb` `0x01` | Bucket record, version 1 |
| Record magic | `ste` `0x01` | Object entry, version 1 |
| Record magic | `stp` `0x01` | Piece, version 1 |
| Record magic | `sto` `0x01` | Outcome record, version 1 |
| Record magic | `stf` `0x01` | Front statement, version 1 |
| Record magic | `stq` `0x01` | Challenge request, version 1 |
| Record magic | `str` `0x01` | Challenge response body, version 1 |
| Topics | `tm_bstore_<name>_<suffix>` | A bucket's records topic, which overlay hosts carry; `<name>` is 1 to 25 characters and `<suffix>` is 10 random lowercase letters chosen when the bucket is created |
| Topics | `tm_bstoreblob_<name>_<suffix>` | The same bucket's blob topic, which storage providers subscribe to and no host admits; the same `<name>` and `<suffix>` |
| Lookup service | `ls_bstore` | Lookup service for every bucket on a host |
| Host route | `<base>/ls_bstore/terms` | The host terms document for priced questions |
| Provider routes | `<front>/challenge`, `<front>/renew`, `<front>/settle`, `<front>/terms` | A storage provider's routes; a derived front ends `/bstore/v1` |
| Baskets | `bstore fund`, `bstore entry funding`, `bstore piece funding`, `bstore outcome funding`, `bstore front funding`, `bstore chain funding`, `bstore chain`, `bstore hold`, `bstore kill tombstone` | Wallet baskets |
| Domain string | `bstore bucket id v1` | Hashed into a bucket id |

The prefix `st` is registered to bstore. `0x62`, `0x65`, `0x66`, `0x6f`,
`0x70`, `0x71` and `0x72` are never assigned as tag type bytes under it,
because `st` followed by one of them is the first three bytes of the record
magics `stb`, `ste`, `stf`, `sto`, `stp`, `stq` and `str`. `0x00` is not
assigned. `0x04` is reserved and not assigned.

bstore owns both the `tm_bstore_` and the `tm_bstoreblob_` topic
namespaces: no other application names a topic that starts with either.
Neither namespace is a prefix of another registered one, and a bucket's
topics carry entropy under the rule above, because anyone can create a
bucket. With `tm_bstoreblob_`, the separating underscore and the suffix
taking 25 characters, `<name>` has at most 25, and the records topic takes
the same bound so that one name serves both.

bstore registers no certificate type: which providers a writer treats as
independent operators is the writer's own policy.

Borrowed, not registered: bstore reads and writes BRC-26 advertisements
under `[2, "uhrp advertisement"]` with key id `1`, as the SDK has it, and
pays providers by BRC-29 under `[2, "3241645161d8"]`, each as its
specification defines it. Its pieces are CHIRP (BRC-167) profile 1 objects,
whose roots carry no extension.

## Not registered

bcommon itself registers nothing: every package that derives, tags or names
takes the value from its caller. Its test vectors use `[1, "vector sample"]`
and the prefix `vx` (`vx` `0x01`, `vx` `0x02`); both are reserved for tests
and no application may use them.
