# Test-game fixture build.
#
# The fixture is the conformance suite for `kobra-pack` and the launcher: it is
# a complete, valid pkgroot that the real packager must be able to build and the
# real launcher must be able to run. See README.md.
#
#   make check     every gate, writes nothing
#   make package   both releases into dist/, plus dist/install/
#   make verify    prove two builds are byte-identical
#   make dev       edit-and-run loop against the source tree
#   make e2e       the full drive (run.sh) — what CI runs
#   make clean

HERE      := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
REPO      := $(HERE)..
LAUNCHER  := $(REPO)/launcher
PACKAGING := $(REPO)/packaging
PACK      := $(PACKAGING)/kobra-pack

export GOCACHE    := $(REPO)/.gocache/build
export GOMODCACHE := $(REPO)/.gocache/mod
export GOSUMDB    := off

PORT ?= 18915

.PHONY: all check package verify dev e2e test tools clean

all: package

## tools: build kobra-pack and the linux launcher this fixture packages
tools:
	$(MAKE) -C $(PACKAGING) build
	$(MAKE) -C $(LAUNCHER) dist-linux

## check: every gate, no archives written (what a pre-commit hook runs)
check: tools
	$(PACK) check --config $(HERE)pkg.toml --platform linux-x64

## package: both releases into dist/, then extract release 1 for the launcher
package: tools
	$(PACK) build --config $(HERE)pkg.toml --platform linux-x64 \
	  --out $(HERE)dist --install

## verify: package twice and require byte-identical archives (§5.2)
verify: tools
	@rm -rf $(HERE)dist
	@$(PACK) build --config $(HERE)pkg.toml --platform linux-x64 --out $(HERE)dist >/dev/null 2>&1
	@sha256sum $(HERE)dist/*.zip $(HERE)dist/*.tar.zst > /tmp/testgame-verify-1
	@$(PACK) build --config $(HERE)pkg.toml --platform linux-x64 --out $(HERE)dist >/dev/null 2>&1
	@sha256sum $(HERE)dist/*.zip $(HERE)dist/*.tar.zst > /tmp/testgame-verify-2
	@diff -u /tmp/testgame-verify-1 /tmp/testgame-verify-2 \
	  && echo "verify: two builds are byte-identical"
	@rm -f /tmp/testgame-verify-1 /tmp/testgame-verify-2

## dev: serve the source tree through a shim game folder
dev: tools
	$(PACK) dev --config $(HERE)pkg.toml --platform linux-x64 --port $(PORT)

## e2e: the full drive in run.sh
e2e:
	bash run.sh

## test: alias for e2e, so `make test` works from here too
test: e2e

## clean: remove build output and the fixture's machine-local state
clean:
	rm -rf $(HERE)dist $(HERE).state $(HERE).kobra-dev
