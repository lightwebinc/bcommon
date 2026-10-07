# Dependencies

## One direct dependency

bcommon imports the Go standard library, its own packages and
github.com/bsv-blockchain/go-sdk, and nothing else. The rule covers tests as
well as production code, and packages a build constraint leaves out.

Every application that pins bcommon inherits its requirements. A second
direct dependency would be a cost to every one of them at once: another
module to audit, another licence to carry, another version for minimal
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
in [NOTICE](../NOTICE), with their licences, and their full texts are in
`LICENSE-THIRD-PARTY`, which `make licences-update` regenerates from what the
packages actually link and `make licences` checks is current.

## Why go-sdk is pinned at exactly v1.7.1

go-sdk releases below v1.5.0 size a slice from a count the input declares
while parsing a merkle path, so a few hostile bytes can ask for an allocation
that ends the process with an out-of-memory no `recover` sees. v1.5.2 bounds
those counts against the bytes present, and so does v1.7.1. The `guard` package walks every
BUMP, BEEF and raw transaction the library parses before the SDK sees it,
bounding each count by the bytes present, so the library's own protection
does not rest on the pin; the floor is the SDK's.

The pin is exact rather than a minimum because minimal version selection
takes the higher of two requirements. A raised pin here would silently change
the go-sdk that every application pinning bcommon builds with, including the
derivation, script and transaction encodings its records depend on.

v1.7.1 replaced v1.5.2 in v0.12.0. It sends the BRC-103 handshake's
requested certificate set in the wire shape (`certifiers` and `types`),
which TypeScript peers on `@bsv/sdk` 2.8 and later require; v1.5.2 sent
Go's field names and those peers refused the handshake. v1.7.1 still reads
the old names, so a peer on v1.5.2 keeps working with it in both
directions. Every vector matched byte for byte across the change.

v1.7.1 still accepts a compressed public key whose x is at or above the
field prime. `guard.ParsePubKey` stays in front of every key from the wire
whatever the pin.

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
is pinned exactly, at 2.7.1, and the development dependency is the same
version; the package's boundary test fails if the two differ or if a runtime
dependency appears. The exact peer holds the TypeScript twins to the SDK
their bytes were checked against, as the go-sdk pin does for Go. Raising it
means changing both entries together and running the TypeScript tests,
which compare the twins' output with the shared vectors, before tagging.

