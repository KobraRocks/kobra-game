# Kobra — the engine

The engine for **one kind of game**: a single-player, offline, deterministic CRPG
built on the [4C System (Libre Edition)](specs/4c_system.md).

It is not a general-purpose engine. It is shaped by one genre and one rules system,
so that a game built on it is a **universe plus content** rather than a new engine.
The decision is recorded in the first game's decision log, `AD-38`–`AD-42`.

## What is here

```
crates/kobra-core/   the engine: the 4C rules, the simulation, the content model,
                     the save format, the ABI. Zero dependencies, on purpose.
specs/               the 4C System text, the authoritative Master Tables, and the
                     property test that cross-validates them
tools/gen-tables/    the generator that turns the CSV into the compiled table (AD-14)
```

`cargo test` at the root runs the engine's own suite: the rules, the wire format and
the generated tables. It needs no game.

```sh
make check     # tables, formatting, tests — the gates CI runs
make test      # cargo test: the engine's own suite
make tables    # regenerate crates/kobra-core/src/tables from the authoritative CSV
make clippy    # lints, warnings denied
```

Requirements: the pinned Rust toolchain (`rust-toolchain.toml`, 1.98.0) and
`python3` for the Master Table property test.

## How a game uses it

A game is its own repository. It ships **content** (packs, locales, art), **Lua**
for content-shaped logic (`AD-39`), and — if it must — a native plugin against the
versioned C ABI (`AD-40`). It consumes the engine as a path dependency in the
development workspace and pins a tag for a release; the engine is not published to
a registry.

The first game, Worldspiracy, is checked out under `games/` — ignored by this
repository, because it *is* its own. Its conformance suite is the half of the
engine's behaviour a unit test cannot reach: the shipped content loads and
validates, the golden replays hash identically, the mod fixture changes the
trajectory, and the string table covers the engine's narration vocabulary.

## Working here

`AGENTS.md` records what is easy to get wrong in this tree. `CONTRIBUTING.md` has
the setup, the gates, and the rules of the house.
