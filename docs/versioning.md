# Versioning

bcommon is versioned with semantic version tags of the form `vMAJOR.MINOR.PATCH`
on the main branch. The Go module proxy caches a tag the first time anyone
fetches it, so a published tag is permanent: it is never moved or deleted, and
a mistake is fixed by a new tag.

## Pin an exact tag

The library is pre-1.0. Every application requires an exact tag in its
`go.mod`, for example:

```
require github.com/lightwebinc/bcommon v0.1.0
```

and moves to a newer one deliberately, running its own tests and checking its
own vectors against the new tag before it ships. Build a release binary with
`GOWORK=off` and confirm the version it links with `go version -m`, since a
workspace resolves the library from disk and hides which tag is in use.

## One tag, both languages

The TypeScript package under `ts/` has no version line of its own. The
commit tagged `vX.Y.Z` carries `ts/package.json` at version `X.Y.Z`: tag
`v0.1.0` ships `@lightwebinc/bcommon` 0.1.0. A change to either
language is a new tag for both, even when the other did not change, so one
version names one tree and one set of vectors, and the two languages are
held to the same bytes at every version.

The version is set in `ts/package.json` in the commit that is tagged, never
after it: a tag is permanent, and one whose `package.json` names another
version breaks the rule for good.

An application that uses both languages pins the same tag in each: the Go
module in `go.mod`, and the package packed from that tag (`npm pack` in
`ts/`), for example vendored as a tarball and named as a `file:`
dependency, so that its lockfile pins the bytes. The package declares
`@bsv/sdk` as a peer at one exact version, so the application supplies
that version itself.

## What a v0 minor may change

Before v1.0.0:

- A **minor** version (v0.1.x to v0.2.0) may change or remove exported API:
  rename a package, identifier or field, change a signature, or change what a
  function refuses and how.
- A **patch** version (v0.1.0 to v0.1.1) fixes defects and adds tests or
  documentation without changing exported API.

The same holds for the TypeScript package's exports, from both of its
entry points.

Output bytes are a different matter from API. A change to the bytes a
function produces for inputs an application has already committed to the
chain (encodings, derived keys, roots) breaks that application's existing
records whatever the version number says, because its readers verify those
bytes long after they were written. The tests' vectors exist to catch such a
change, and the go-sdk pin, like the TypeScript package's exact `@bsv/sdk`
peer, exists partly for the same reason (see
[dependencies.md](dependencies.md)).
