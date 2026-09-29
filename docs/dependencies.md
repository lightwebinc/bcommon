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

## Why go-sdk is pinned at exactly v1.5.2

go-sdk releases below v1.5.0 size a slice from a count the input declares
while parsing a merkle path, so a few hostile bytes can ask for an allocation
that ends the process with an out-of-memory no `recover` sees. v1.5.2 bounds
those counts against the bytes present. The `guard` package checks a BUMP
before the SDK sees it as a second line of defence, but the floor is the fix.

The pin is exact rather than a minimum because minimal version selection
takes the higher of two requirements. A raised pin here would silently change
the go-sdk that every application pinning bcommon builds with, including the
derivation, script and transaction encodings its records depend on.

## Raising the pin

Raising go-sdk is a deliberate change, never an automatic one. It requires
re-verifying every vector the library's tests check against, because a
change in the SDK's derivation, signing or encoding would change bytes that
applications have already committed to the chain. Raise the version in
`go.mod`, in `tools/vectors/go.mod` and as `SDK_VERSION` in the Makefile
together, run `make verify`, and confirm every vector still matches byte for
byte before tagging.
