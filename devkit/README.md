# bcommon devkit

Development and test tooling for applications built on bcommon, a separate
module (`github.com/lightwebinc/bcommon/devkit`, tagged `devkit/vX.Y.Z`).
An application imports it only from its tests and its development commands.
See the [bcommon README](../README.md#devkit-development-and-test-tooling).

## devchain

An application's local stand-in chain (`cmd/devchain` calls
`devchain.Main`): a node's JSON-RPC at `/rpc`, its asset API under
`/api/v1/`, and a header source at `/v1/root/<height>` and `/v1/tip`. No
proof of work and no peers: nothing it mines is money. For a laptop, never
for a network.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `:8080` | address to serve on |
| `-height` | `700` | the height of the tip before anything is mined |
| `-journal` | none | append every accepted RPC call to this file and replay it at start, so a restarted chain is the same chain |
| `-version` | | print the version and exit |

With `Options.Certs` set, `devchain certs` writes a throwaway CA and server
certificates:

| Flag | Meaning |
| --- | --- |
| `-dir` | the directory to write `ca.pem`, `<name>.pem` and `<name>-key.pem` into (required) |
| `-names` | the server names, comma separated (required) |

Exit status: 0, 1 when the chain cannot start or stops with an error, 2 for
a usage error.

## vectors

An application's golden-vector command (`cmd/vectors` calls
`vectors.Main`): writes the files the application's codec generates, or
compares them.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-dir` | `testdata/vectors` (or `Options.Dir`) | vector directory |
| `-check` | false | compare with the files on disk and write nothing; with `Options.Strict`, a `*.json` file nothing generates is refused too |

Exit status: 0, 1 when a file differs, is missing or could not be written,
or the generator failed, 2 for a usage error.

## paidhost

A stand-in for an overlay host's priced terms route, for an application's
tests: the terms document, and lookups over BRC-104 in which a priced class
is answered 402 (BRC-105) until a payment pays the price and verifies. A
library, no flags.

## bcommon's own vector generator

`tools/vectors` (in the main module's tree, run by `make vectors`) takes
`-dir` (default `../../testdata/vectors`), `-check` (compare instead of
write) and `-repo` (default `../..`, the repository root the Unicode data is
read from and the renderer filter's table is written to).
