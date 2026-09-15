# Kobra Launcher build (Launcher spec §25.2)
#
# Reproducibility inputs are exactly VERSION, COMMIT and BUILD_TIME; the rest is
# derived from the source tree. GOFLAGS carries -trimpath and -buildvcs=false so
# the build does not embed a local path or VCS state.

MODULE      := kobragames.local/launcher
BIN         := launcher
CMD         := ./cmd/kobra-launcher

VERSION     ?= 0.1.0
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOFLAGS     := -trimpath -buildvcs=false
LDFLAGS     := -s -w \
               -X main.version=$(VERSION) \
               -X main.commit=$(COMMIT) \
               -X main.buildTime=$(BUILD_TIME)

# CGO_ENABLED=0 for every target triple (§3.3): no cgo, no shared libraries.
export CGO_ENABLED := 0

# This checkout is built in a sandbox where the system Go caches are read-only,
# so the caches are redirected into the workspace. Override GOCACHE/GOMODCACHE
# on the command line to use the system defaults.
REPO_ROOT   := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))..
GOCACHE     ?= $(REPO_ROOT)/.gocache/build
GOMODCACHE  ?= $(REPO_ROOT)/.gocache/mod
GOSUMDB     := off
export GOCACHE GOMODCACHE GOSUMDB

.PHONY: all build test vet fmt race faultinject dist cross dist-linux clean tidy smoke

all: build

## build: native binary at launcher/launcher
build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(CMD)

## faultinject: the §26.4 crash-safety scenarios. Builds with the
## kobra_faultinject tag, runs the tagged Go tests (EXDEV fallback, disk-full
## mapping, injected panic through the middleware), then drives the scenarios
## that need a real process in test/faultinject.sh: a crash between fsync and
## rename with the temp file quarantined on the next start, a crash between
## rename and the directory fsync, a panicking handler exiting 4, and a signal
## during drain. A release build contains none of this machinery.
faultinject:
	go test -tags kobra_faultinject ./...
	bash test/faultinject.sh

## test: unit, contract and package tests
test:
	go test ./...

## race: the same suite under the race detector. Every other target in this file
## builds with CGO_ENABLED=0 (§3.3), but -race needs cgo, so this one recipe
## re-enables it and says so plainly when no C toolchain exists instead of
## failing with a confusing `-race requires cgo`.
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
	files="$$(gofmt -l . | sed '/^vendor\//d')"; \
	if [ -n "$$files" ]; then \
	  printf '%s\n' "$$files" | tee /dev/stderr >/dev/null; \
	  echo "fmt: gofmt reported unformatted files" >&2; \
	  exit 1; \
	fi

## cross: cross-compile the supported matrix (§25.1) with CGO disabled
cross:
	GOOS=linux   GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/linux-amd64/$(BIN)     $(CMD)
	GOOS=linux   GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/linux-arm64/$(BIN)     $(CMD)
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/windows-amd64/$(BIN).exe $(CMD)
	GOOS=windows GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/windows-arm64/$(BIN).exe $(CMD)
	GOOS=darwin  GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/darwin-amd64/$(BIN)   $(CMD)
	GOOS=darwin  GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/darwin-arm64/$(BIN)   $(CMD)

## dist-linux: just linux/amd64, which is the platform kobra-pack builds by
## default and the one a Linux studio ships. §10 keeps the rest of the matrix
## available through `cross` without making every package build pay for it.
dist-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/linux-amd64/$(BIN) $(CMD)
	@echo "built dist/linux-amd64/$(BIN) $(VERSION)"

## dist: native release binary plus checksums
dist: clean
	$(MAKE) build VERSION=$(VERSION)
	cd .. && sha256sum launcher/$(BIN) > launcher/dist-SHA256SUMS

## smoke: end-to-end launcher drive (§26.6)
smoke:
	bash ../.e2e/run.sh

## tidy: refresh go.mod/go.sum
tidy:
	go mod tidy

## clean: remove build outputs
clean:
	rm -rf $(BIN) $(BIN)-dev dist dist-SHA256SUMS

# --- The launcher is game-agnostic; a per-game launcher is a config file -----
#
# There is no per-game binary and no code change per game. Everything that makes
# a launcher "for this game" is launcher/launcher.config.json inside the game
# folder: game_id (keys the sidecar directory and the deterministic port),
# game_name, release, the port window, browser floor, CSP and data_api policy.
# Copying the binary into another game folder produces that game's launcher.
#
# `make package` performs that copy to a target game folder.
#
#   make package GAME=/path/to/OtherGame

GAME ?=

## package: install this launcher into a game folder's launcher/ directory
package:
	@test -n "$(GAME)" || { echo "usage: make package GAME=/path/to/GameFolder"; exit 2; }
	@test -d "$(GAME)/launcher" || { echo "$(GAME)/launcher does not exist; create the game folder layout first (§5.1)"; exit 2; }
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o "$(GAME)/launcher/$(BIN)" $(CMD)
	@test -f "$(GAME)/launcher/launcher.config.json" || echo "note: $(GAME)/launcher/launcher.config.json is missing; the launcher will refuse to start (§5.1 step 4)"
	@cp -f port-deny-list.json "$(GAME)/launcher/" 2>/dev/null || true
	@echo "installed $(GAME)/launcher/$(BIN)"

# --- Development against a game source tree in another folder ---------------
#
# The launcher derives the game folder from its own executable path and never
# from the working directory (FR-LNCH-1). To develop a game whose source lives
# at another path, run it through a small "shim" game folder whose game/ entry
# is a symlink to the real source tree:
#
#   make run-dev GAME=../my-game/src ARGS="--yes --no-open"
#
# The shim gives the launcher the layout it requires, the symlink keeps the real
# game files where the developer edits them (serving follows symlinks and
# re-checks confinement, §13.3), and nothing in the launcher changes. data/ is a
# real directory under the shim so saves never land in the source tree; pass
# DATA_DIR to place it elsewhere.

DEV_DIR   ?= $(REPO_ROOT)/.dev
DATA_DIR  ?=
ARGS      ?= --yes

## build-dev: development build that also accepts --game-dir (FR-LNCH-1 dev escape)
#
# The kobra_dev tag is what keeps --game-dir out of a release binary: a shipped
# build has no flag to register and no code path that takes a game folder from
# the command line, so FR-LNCH-1 holds for users while developers can point the
# launcher at a tree in another folder.
build-dev:
	go build $(GOFLAGS) -tags kobra_dev -ldflags '$(LDFLAGS)' -o $(BIN)-dev $(CMD)
	@echo "built $(BIN)-dev (development build: --game-dir is available)"

## run-dev: serve a game source tree from another path via a shim game folder
run-dev:
	@test -n "$(GAME)" || { echo "usage: make run-dev GAME=/path/to/game-source-dir [ARGS='--yes --no-open']"; exit 2; }
	@test -f "$(GAME)/index.html" || { echo "$(GAME) does not look like a game source tree ($(GAME)/index.html not found)"; exit 2; }
	@mkdir -p "$(DEV_DIR)/launcher"
	$(MAKE) --no-print-directory build
	@cp -f $(BIN) "$(DEV_DIR)/launcher/$(BIN)"
	@cp -f port-deny-list.json "$(DEV_DIR)/launcher/" 2>/dev/null || true
	@test -f "$(DEV_DIR)/launcher/launcher.config.json" || cp -f dev/launcher.config.dev.json "$(DEV_DIR)/launcher/launcher.config.json"
	@ln -sfn "$(abspath $(GAME))" "$(DEV_DIR)/game"
	@test -n "$(DATA_DIR)" || mkdir -p "$(DEV_DIR)/data"
	@echo "shim: $(DEV_DIR)  ->  game/ -> $(abspath $(GAME))"
	@cd "$(DEV_DIR)" && XDG_STATE_HOME="$(DEV_DIR)/state" ./launcher/$(BIN) $(ARGS) $(if $(DATA_DIR),--data-dir $(abspath $(DATA_DIR)),)

## run-dev-clean: remove the development shim
run-dev-clean:
	rm -rf "$(DEV_DIR)"
