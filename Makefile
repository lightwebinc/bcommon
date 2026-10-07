# GOWORK=off everywhere on purpose. In a workspace a member module resolves
# to a sibling's local HEAD, so a go.mod skew stays invisible here and
# surfaces only in an application that pins this module by tag. Verify what
# a tag ships, not what the workspace happens to have on disk.

.PHONY: verify build test fmt-check vet deps-check licences licences-update vectors vectors-update ts ts-test ts-licences ts-licences-update

SDK := github.com/bsv-blockchain/go-sdk
SDK_VERSION := v1.7.1
VECTORS := tools/vectors
CBOR_ORACLE := github.com/fxamacker/cbor/v2

verify: fmt-check vet deps-check licences vectors build test

build:
	GOWORK=off go build ./...

test:
	GOWORK=off go test ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

vet:
	GOWORK=off go vet ./...

# One direct dependency, go-sdk, at exactly the pinned version. Every
# application that imports this library inherits both: a second direct
# dependency is a cost to all of them at once, and MVS takes the higher of
# two go-sdk pins, so a raised pin here silently changes what every
# application ships. docs/dependencies.md says why the version is fixed.
deps-check:
	@direct=$$(GOWORK=off go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all | grep -v '^github.com/lightwebinc/bcommon$$' | grep . || true); \
	if [ "$$direct" != "$(SDK)" ]; then \
		echo "expected exactly one direct dependency, $(SDK), found:"; \
		echo "$$direct"; \
		exit 1; \
	fi; \
	v=$$(GOWORK=off go list -m -f '{{.Version}}' $(SDK)); \
	if [ "$$v" != "$(SDK_VERSION)" ]; then \
		echo "$(SDK) is $$v, want exactly $(SDK_VERSION)"; \
		exit 1; \
	fi

# Generated from what the packages actually link, so a dependency arriving
# by accident brings its licence obligation into the file rather than
# leaving it undischarged.
licences:
	python3 scripts/gen-third-party-licenses.py . --check

licences-update:
	python3 scripts/gen-third-party-licenses.py .

# The vectors the packages' tests compare their output against, regenerated
# by an independent generator and compared byte for byte (docs/vectors.md).
# The generator is a module of its own, outside the one-dependency rule
# above, so its requirements are checked here instead: go-sdk at the
# library's pin, because a vector built on another go-sdk would not be the
# bytes the library produces, plus the independent encoder, and nothing
# else.
vectors:
	@direct=$$(cd $(VECTORS) && GOWORK=off go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all | grep -v '^vectors$$' | sort | tr '\n' ' '); \
	if [ "$$direct" != "$(SDK) $(CBOR_ORACLE) " ]; then \
		echo "$(VECTORS): expected direct dependencies $(SDK) and $(CBOR_ORACLE), found: $$direct"; \
		exit 1; \
	fi; \
	v=$$(cd $(VECTORS) && GOWORK=off go list -m -f '{{.Version}}' $(SDK)); \
	if [ "$$v" != "$(SDK_VERSION)" ]; then \
		echo "$(VECTORS): $(SDK) is $$v, want exactly $(SDK_VERSION)"; \
		exit 1; \
	fi
	cd $(VECTORS) && GOWORK=off go vet ./...
	cd $(VECTORS) && GOWORK=off go run . -check

vectors-update:
	cd $(VECTORS) && GOWORK=off go run .

# The TypeScript package under ts/ has its own toolchain, so it is not part
# of verify: a machine that builds the Go library needs no Node. CI runs it
# in its own job, and these targets are how to run it here. Its tests read
# the same vectors as the Go tests, so run both after regenerating them.
ts:
	cd ts && npm ci && npm run check && npm run build

ts-test: ts
	cd ts && npm test

# The package's production tree, peers left out, which is empty: its one
# runtime import is the peer @bsv/sdk, the installer's own. A runtime
# dependency arriving by accident would be installed beside the package
# with an obligation NOTICE does not state, so it fails here. Needs npm and
# an installed ts/node_modules (make ts).
ts-licences:
	python3 scripts/gen-third-party-licenses.py ts --check

ts-licences-update:
	python3 scripts/gen-third-party-licenses.py ts
