# The host module

What every application's overlay host module runs from
`@lightwebinc/bcommon/host` (see [architecture](architecture.md)): the
terms route, its budgets, payment acceptance, and the state directory that
holds the payee ledger. A host module written in Go runs the same from the
Go package `termsroute`, which answers with the same status codes, bodies,
metric names and ledger lines, so everything below holds for either. Each application's `docs/host.md` links here and
keeps only what is its own: its environment variable prefix (written
`<APP>_` below, for example `BBOX_`), its journal of outpoint rows, its
service name (`ls_<app>`), its defaults, and its metrics prefix
(`<app>_`).

## The terms route

The reference host lets a module add no route of its own, so with
`<APP>_LISTEN` set the module serves its own, beside the host:

| Route | Answer |
| --- | --- |
| `GET /ls_<app>/terms` | the application's terms document; 404 when the host prices nothing |
| `POST /.well-known/auth` | the BRC-104 handshake |
| `POST /lookup` | the same BRC-24 questions as the host's `/lookup` |

Its base URL is what the host publishes as `ls_<app>`'s base (BRC-180
`metanet.overlays`), an origin with no path: a BRC-104 client shakes hands
at the origin's `/.well-known/auth`. A reverse proxy in front of it gives
it an origin of its own (a name, or a port) and passes the path unchanged,
because a BRC-104 signature covers it.

A free question is answered with or without BRC-104. A question of a class
the host prices must be asked over BRC-104: without an `x-bsv-payment`
header it is answered 402 with `x-bsv-payment-version` (`1.0`),
`x-bsv-payment-satoshis-required` and `x-bsv-payment-derivation-prefix`,
signed by the payee's identity key. With one, it is answered once output 0
of the payment pays at least the price, P2PKH, to the key BRC-29 derives
for the prefix, the suffix and the asker, the prefix is one this route
issued, the payment verifies against the host's headers, its txid was never
used before, it spends no coin an accepted payment spent, and it passes
[payment acceptance](#payment-acceptance). Two transactions spending one
coin can both verify and at most one can ever be mined: the second is
refused 409 `ERR_PAYMENT_CONFLICT`, as a used txid is refused 409
`ERR_PAYMENT_REPLAYED`. The coins of every accepted payment are kept in
`payments.jsonl` and refused again after a restart. One payment buys one
question. The same class asked on the host's own `/lookup` is refused with
an error that names this route.

### The session bound

A BRC-104 handshake is unauthenticated, and the SDK's own session store
keeps every session for the life of the process, so the route keeps its
own, bounded: at most `<APP>_SESSIONS` sessions, each forgotten once idle
for `<APP>_SESSION_TTL` seconds, the least recently used first past the
cap. A client whose session was forgotten is refused and shakes hands
again; the SDK's client does that by itself. `<app>_sessions` is the
store's size and `<app>_sessions_evicted_total{why="cap"|"idle"}` what it
forgot.

### The handshake budget

The session bound holds memory, not work: each handshake costs a signature
on the host's own process. So the route answers handshakes from two token
buckets checked before the request body is read and before any signature
work:

- the route's: `<APP>_HANDSHAKES_PER_SEC` a second, up to
  `<APP>_HANDSHAKE_BURST` at once;
- each remote address's: `<APP>_HANDSHAKES_PER_ADDR_PER_SEC` a second, up
  to `<APP>_HANDSHAKE_ADDR_BURST` at once. IPv6 addresses share a bucket
  per /64; an IPv4-mapped address is its IPv4 address.

The address's bucket is checked first, so one address that floods spends
its own budget, not the route's. A handshake over either is answered 429
with `Retry-After` (seconds) and `ERR_RATE_LIMITED`, and takes nothing from
either bucket. The address is the connection's: behind a reverse proxy
every client shares the proxy's address, so raise the per-address limit to
the route's, or limit at the proxy instead.
`<app>_handshakes_total{result}` counts every handshake: `accepted`,
`failed` (malformed or not verified), `limited_address`, `limited_global`.

### The response budget

Every authenticated request is answered with a signature, so one handshake
must not buy signatures without end. Each session has a token bucket,
`<APP>_RESPONSES_PER_SEC` a second up to `<APP>_RESPONSE_BURST` at once. A
request over it is answered 429 with `Retry-After` and `ERR_RATE_LIMITED`,
and nothing is signed. A BRC-104 client reports an unsigned answer as a
failed authentication; the application's CLI waits as the header says (1 to
30 seconds) and asks once more.

An authenticated request sent again byte for byte is refused 401
`ERR_AUTH_REPLAYED` before any signature work. The route remembers the last
65536 requests it verified; the session's budget bounds what an older
replay can cost. `<app>_requests_total{result}` counts both refusals:
`replayed`, `limited`.

## Payment acceptance

A paid question is answered only after the payment is held to bcommon's
acceptance rule (package `acceptance`, the same rule for every layer; see
[configuration](configuration.md#payment-acceptance-acceptance)):

1. the route's own checks (prefix, output 0 pays the derived key, SPV
   against the host's headers, a txid and coins never used before);
2. finality (lock time 0, or every input final) and conservation (every
   input carries its source, and the outputs do not exceed the inputs);
3. the value: at or below `<APP>_ACCEPT_THRESHOLD_SATS`, and within the
   payer's and the total limits, it is fast; otherwise it is held;
4. the host broadcasts it through `<APP>_ARCADE_URL`, unmined ancestors
   first, held or fast: this is how the payee collects;
5. on the fast path the host answers only once arcade reports the network
   took it (`SEEN_ON_NETWORK`, `ACCEPTED_BY_NETWORK`, ...), with no
   `DOUBLE_SPEND_ATTEMPTED` and no competing transaction, and the node
   shows every input unspent or spent by this payment.

A refusal (arcade `REJECTED`, a definitive HTTP refusal, an input spent by
another transaction, a payment that is not final or spends more than it
holds) is answered 400 `ERR_PAYMENT_REFUSED`. Anything else that is not
fast (above the threshold, a limit reached, a conflict reported, no verdict
within 10 s, a spend view that cannot say) is answered 202
`ERR_PAYMENT_HELD` with the txid and the reason, so a client does not pay
again (hosts before bcommon v0.23.0 answered 402 without BRC-105 headers;
the Go package `heldpay` reads both). The held payment is recorded in
`payments.jsonl` with `"decision":"hold"`, keeps its coins, and the same
payment sent again is answered once the node serves its proof and that
proof verifies against the host's headers. A host with no arcade or no node
holds every payment (with arcade alone it still broadcasts, and with the
node alone it answers a payment the node shows mined). At most 100,000 fast
payments are watched at once; past that a payment is held (`watch-limit`).

Every fast payment is watched on the host's background tick (every 30 s,
the 500 least recently checked each time) until it mines. One whose input
another transaction spent, that arcade refuses, or that has not mined
within a day, is written to `unsettleable.jsonl` in the state directory,
logged, and counted; its payer is then held for confirmation on every later
payment. Flags outlive a restart: the host reads `unsettleable.jsonl` when
it starts, so an operator unflags a payer by removing its lines and
restarting. Nothing is clawed back: the answer was given. A restart watches
the fast lines of the last day again, and counts those of the last window
against the limits.

The ledger line carries `decision` (`fast`, `hold` or `mined`) and
`reason`; the application's `payee settle` reads it.

## The state directory

`<APP>_STATE_DIR` holds, beside the engine's storage:

- the application's journal of outpoint rows (its `docs/host.md` names the
  file and its rows): every carrier admitted with the funding output it
  spent, every drop, and every outpoint an admitted sweep spent. It keeps a
  funding output from paying for a second answered carrier, so it is kept
  for the host's life, backed up with the database, and never edited. Each
  line is flushed to disk before the admission it records completes. The
  module holds its carrier lines in memory for the host's life, and nothing
  bounds them (roughly 1 KB each): give Node the heap for the carriers the
  host expects (`--max-old-space-size`) and watch the application's
  `<app>_carrier_lines` gauge;
- `payments.jsonl`: every payment the terms route accepted (the Atomic
  BEEF, the derivation prefix and suffix, the sender's identity key, the
  satoshis, the class, the outpoints it spends, and the decision). The
  route answers once the line is on disk. The host broadcasts the payment;
  the payee still settles it;
- `unsettleable.jsonl`: each fast payment the watch found lost
  (double-spent, refused, or never mined), one JSON line each.

### Settling

A payment is money only once it is mined; until then the payer can race a
conflicting spend to a miner. Settle each line promptly with the
application's `payee settle`, or the payee wallet's BRC-100
`internalizeAction` (protocol `wallet payment`, output 0, the remittance as
written), which broadcasts it.

### Backing it up

The files are append-only, so a copy taken while the host runs is safe: at
worst its last line is cut short, and every reader skips such a line. The
journal of outpoint rows is the file that matters: a host that loses it can
answer a second carrier on a funding output whose first it already
answered, and nothing else holds it. Copy the whole directory once a day
beside the database backup, and keep several days:

```sh
#!/bin/sh
# a dated copy of the state directory, the last 7 kept
set -eu
src=/var/lib/app
dst=/var/backups/app-state
install -d -m 0700 "$dst"
day=$(date -u +%Y%m%d)
tar -C "$(dirname "$src")" -czf "$dst/state-$day.tar.gz.tmp" "$(basename "$src")"
mv "$dst/state-$day.tar.gz.tmp" "$dst/state-$day.tar.gz"
ls -1 "$dst"/state-*.tar.gz | sort | head -n -7 | xargs -r rm -f
```

Run it daily, from a systemd timer (`OnCalendar=daily`, `Persistent=true`)
or cron, as a user that can read the directory (it is mode 0700). The
copies hold `payments.jsonl`, which is money until it is settled: keep them
as private as the directory.

To restore, stop the host, unpack the newest copy over `<APP>_STATE_DIR`,
and start it. Rows written after the copy are lost: a carrier dropped after
the copy is no longer in storage, so its row is gone and its funding output
could answer a second carrier. Restore only when the directory is lost or
damaged, never to roll the host back.
