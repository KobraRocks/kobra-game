# Working in this repository (agents)

`CONTRIBUTING.md` is the source of truth for how to work here. This file records
what is easy to get wrong in *this* tree when editing with tools rather than by
hand.

## What this is

The engine for a single-player, offline, deterministic CRPG on the 4C System —
**not a general-purpose engine, and not a launcher.**

The launcher, the packager, the fixture game and their specifications were removed
(`AD-38`–`AD-42`). A browser-served web game is no longer part of this product. If
you find yourself looking for a launcher, a wasm build, a node toolchain, a port
allocator, a `pkg.toml` or a `game/` web package, it is not here and it is not
coming back in that form.

## Where the code is

```
crates/kobra-core/    the engine: L0–L4 rules, the content model, save, validator,
                      the generated Master Table, the ABI
specs/                the 4C text, the Master Tables, the property test
tools/gen-tables/     the table generator (AD-14)
games/worldspiracy/   the first game — its own repository, ignored by this one
```

`games/` is a development checkout of a *consumer*. Nothing under it belongs to this
repository, and `git clean -xfd` will delete it. The engine's own gates do not need
it.

## The gates

```sh
make check     # tables, formatting, the engine's own tests
make clippy    # lints, warnings denied
```

A game's conformance suite — its content, its golden replays — runs in that game's
repository, against the engine. If you changed the engine, run that suite too and
say so: the engine's own tests cannot see a game's data, so a change that looks
green here can still break the game.

## Traps

1. **The engine core has zero dependencies, on purpose.** A dependency there needs a
   stated reason: it is a reproducibility liability and a permanent context cost.
2. **The simulation is integer-only, and that is not a style choice.** No floats on
   state paths, no wall clock, no locale, no unseeded randomness. The golden replays
   are what prove it, and a divergence is a defect, not a flake.
3. **One rules implementation.** The engine resolves; content and Lua ask. A second
   implementation of a rule is the bug (`AD-15`).
4. **No game-specific identifier in an engine crate.** A game's name belongs in its
   content and its own repository, never in the engine's wire contracts. The
   `kobra.*` schemas are the engine's: `kobra.save/1`, `kobra.content-load/1`,
   `kobra.content-pack/1`, `kobra.load-report/1`, `kobra.script-report/1`,
   `kobra.replay/1`, `kobra.ui-layout/1`.
   **Known exception, owed:** *content record ids* still carry the first game's short
   prefix (`wsp.power.*`, `wsp.item.*`, …), because the engine's canonical power
   kernels match core content by id. Splitting core content (the engine's) from
   campaign content (the game's) is a content-namespace change; it lands with the
   core-content move and it regenerates the golden replays.
5. **Cite the spec where the rule lives** (`02:02.7`, `AD-21`, `R6`). If a change
   alters behaviour a specification describes, the specification changes in the same
   commit; a code/spec disagreement is the bug. The engine's normative documents —
   `02-rules-engine.md`, the decision log, `07`, `08` — still live in the game's
   `architecture/` set; moving them here is owed.
6. **Delete dead machinery rather than documenting it.** A config key, an event or an
   exported symbol with no consumer is removed, not annotated.
7. **Version fields are bare `X.Y.Z`, and an unparseable version fails its gate
   closed** — never silently ordered or coerced to zero.

## Definition of done

- The gates for what you touched pass, and you can name which you ran.
- A fixed bug has a regression test named after the failure, and it fails on the old
  code.
- Described behaviour that changed has its specification updated in the same change.
- `cargo fmt` is clean and `cargo clippy` is warning-free.
