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

## What a v0 minor may change

Before v1.0.0:

- A **minor** version (v0.1.x to v0.2.0) may change or remove exported API:
  rename a package, identifier or field, change a signature, or change what a
  function refuses and how.
- A **patch** version (v0.1.0 to v0.1.1) fixes defects and adds tests or
  documentation without changing exported API.

Output bytes are a different matter from API. A change to the bytes a
function produces for inputs an application has already committed to the
chain (encodings, derived keys, roots) breaks that application's existing
records whatever the version number says, because its readers verify those
bytes long after they were written. The tests' vectors exist to catch such a
change, and the go-sdk pin exists partly for the same reason (see
[dependencies.md](dependencies.md)).
