# Repository-level chores.
#
# This is deliberately not a build entry point. The launcher and the packager are
# separate Go modules with their own Makefiles, and everything you would normally
# want to run — fmt, vet, test, race, cross, faultinject, verify, e2e — lives
# there. What belongs here is only the work the *repository* owns rather than a
# module: today that is the published schema set, which architecture/ owns and
# which both modules vendor copies of.
#
#   make                 this help (never a mutating target)
#   make sync-schemas    copy architecture/schemas into every vendored copy
#   make check-schemas   report drift without writing (non-zero if any)
#
# Gates are not here on purpose. Run them per module:
#
#   make -C launcher fmt vet test race
#   make -C packaging check race
#   make -C testgame verify e2e
#   bash .e2e/run.sh
#
# See CONTRIBUTING.md for which gate a given change needs.

REPO_ROOT := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))

.PHONY: help sync-schemas check-schemas

# help is the first target, so it is the default: running `make` with no goal
# must print something rather than quietly rewrite files.
help:
	@echo 'Repository-level chores. Gates live in the module Makefiles.'
	@echo
	@echo '  make sync-schemas    copy architecture/schemas into every vendored copy'
	@echo '  make check-schemas   report schema drift without writing'
	@echo
	@echo 'Gates (see CONTRIBUTING.md):'
	@echo '  make -C launcher fmt vet test race'
	@echo '  make -C packaging check race'
	@echo '  make -C testgame verify e2e'
	@echo '  bash .e2e/run.sh'

## sync-schemas: re-copy the published schemas from architecture/schemas
##
## architecture/schemas is the published, normative set and the only place a
## schema is edited. //go:embed cannot reach outside its own package and the
## packager ships the set to publishers, so the same bytes have to exist in five
## places — including launcher/port-deny-list.json, which `make package` copies
## into a shipped game folder. scripts/sync-schemas.sh owns that map; this target
## is the way to run it.
##
## This is a convenience, not the guard: the drift tests in both modules fail
## the build if someone edits a schema and forgets to run it.
sync-schemas:
	@bash $(REPO_ROOT)/scripts/sync-schemas.sh

## check-schemas: report schema drift without writing (non-zero if any)
check-schemas:
	@bash $(REPO_ROOT)/scripts/sync-schemas.sh --check
