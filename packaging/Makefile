# kobra-pack — the publisher's toolchain.
#
#   make build     the kobra-pack binary
#   make test      unit and pipeline conformance tests
#   make check     gofmt, vet, test   (what CI runs)
#   make release   a stripped binary for a studio's CI image
#
# The module is separate from launcher/ on purpose: the launcher ships to users
# and keeps a three-dependency footprint (Launcher spec §3.3), while this tool
# runs on a publisher's machine and carries a schema validator and a TOML
# parser. Nothing here is ever placed in a package.

MODULE      := kobragames.local/packaging
BIN         := kobra-pack
CMD         := ./cmd/kobra-pack

VERSION     ?= 0.1.0

GOFLAGS     := -trimpath -buildvcs=false
LDFLAGS     := -s -w -X main.version=$(VERSION)

export CGO_ENABLED := 0

# This checkout is built in a sandbox where the system Go caches are read-only,
# so the caches are redirected into the workspace. Override GOCACHE/GOMODCACHE
# on the command line to use the system defaults.
REPO_ROOT   := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))..
GOCACHE     ?= $(REPO_ROOT)/.gocache/build
GOMODCACHE  ?= $(REPO_ROOT)/.gocache/mod
GOSUMDB     := off
export GOCACHE GOMODCACHE GOSUMDB

.PHONY: all build test check vet fmt race release clean

all: build

## build: kobra-pack at packaging/kobra-pack
build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(CMD)

## test: unit and pipeline conformance tests
test:
	go test ./...

## race: the same suite under the race detector. The rest of the module builds
## with CGO_ENABLED=0; -race needs cgo, so this one recipe re-enables it and
## says so plainly when no C toolchain exists instead of failing with a
## confusing `-race requires cgo`.
race:
	@command -v gcc >/dev/null 2>&1 || command -v cc >/dev/null 2>&1 || { \
	  echo "race: no C toolchain found (need gcc or cc); go test -race requires cgo" >&2; \
	  exit 1; \
	}
	CGO_ENABLED=1 go test -race ./...

## vet: static checks
vet:
	go vet ./...

## fmt: report files that are not gofmt-clean. `set -e` makes a failed tee (for
## example a closed stderr) fail the recipe rather than being swallowed by a
## later command in the pipeline.
fmt:
	@set -e; \
	files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
	  printf '%s\n' "$$files" | tee /dev/stderr >/dev/null; \
	  echo "fmt: gofmt reported unformatted files" >&2; \
	  exit 1; \
	fi; \
	echo "fmt: all files are gofmt-clean"

## check: what CI runs
check: fmt vet test

## release: a stripped binary with a stamped version
release:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS) -X main.version=$(VERSION)' -o $(BIN) $(CMD)
	@echo "built $(BIN) $(VERSION)"

clean:
	rm -f $(BIN)
