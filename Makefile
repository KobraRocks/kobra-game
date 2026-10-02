# The Kobra engine build.
#
#   make check     every gate: tables, formatting, tests
#   make test      cargo test — the rules, the wire format and the tables
#   make tables    regenerate crates/kobra-core/src/tables from the authoritative CSV
#   make fmt       rustfmt over the workspace
#   make clippy    the Rust lints, warnings denied
#   make clean
#
# Running `make` with no goal prints this list rather than building anything.
#
# Toolchains are pinned, not assumed: rust-toolchain.toml pins Rust 1.98.0, and
# the Master Table verifier needs python3.

HERE := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: help tables tables-check test check fmt fmt-check clippy clean

help:
	@echo 'The Kobra engine — a deterministic 4C System CRPG core.'
	@echo
	@echo 'Check it:'
	@echo '  make check     tables, formatting, tests'
	@echo '  make test      cargo test — the rules, the wire format and the tables'
	@echo
	@echo 'Work on it:'
	@echo '  make tables    regenerate crates/kobra-core/src/tables (AD-14)'
	@echo '  make fmt       rustfmt over the workspace'
	@echo '  make clippy    the Rust lints, warnings denied'
	@echo '  make clean     remove build output'

## tables: regenerate the Rust Master Table from the authoritative CSV (AD-14)
tables:
	python3 tools/gen-tables/gen.py

## tables-check: fail when the generated table is stale, and run the property test
tables-check:
	python3 specs/verify_master_tables.py
	python3 tools/gen-tables/gen.py --check

## test: the engine's own suite — the rules, the wire format and the tables
##
## It needs no game: a game's content and its golden replays are tested by that
## game's own conformance suite, against this crate (README.md).
test:
	cargo test

## check: every gate that runs in this tree (what CI runs)
check: tables-check fmt-check test

## fmt: rustfmt over the workspace
fmt:
	cargo fmt --all

## fmt-check: fail when the workspace is not rustfmt-clean (what CI runs)
fmt-check:
	cargo fmt --all -- --check

## clippy: the Rust lints, warnings denied
##
## Not part of `check`: the pinned toolchain installs a minimal profile, so the
## component may be absent on a machine that can still build and test. CI runs
## it, because a hosted runner can install it.
clippy:
	cargo clippy --all-targets -- -D warnings

## clean: remove build output
clean:
	rm -rf $(HERE)target
