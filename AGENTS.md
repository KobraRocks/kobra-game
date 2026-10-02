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

The engine is a Rust crate inside the first game's own repository, checked out here
because this working directory must be writable:

```
games/worldspiracy/src/core/      the engine crate — zero dependencies
games/worldspiracy/specs/         the 4C text, the Master Tables, the property test
games/worldspiracy/tests/replay/  the golden replays — the determinism gate
games/worldspiracy/content/       the first game's content (campaign packs)
```

Nothing under `games/` belongs to this repository — `git clean -xfd` will delete
it. The engine moves here next, with fixtures of its own; until then the gates run
from that checkout.

## The gates

```sh
cd games/worldspiracy && make check     # tables, formatting, the whole test suite
cd games/worldspiracy && make clippy    # lints, warnings denied
```

`cargo test` runs on the host: no wasm, no browser, no node.

## Traps

1. **The engine core has zero dependencies, on purpose.** A dependency there needs a
   stated reason: it is a reproducibility liability and a permanent context cost.
2. **The simulation is integer-only, and that is not a style choice.** No floats on
   state paths, no wall clock, no locale, no unseeded randomness. The replay oracle
   is what proves it, and a divergence is a defect, not a flake.
3. **One rules implementation.** The engine resolves; content and Lua ask. A second
   implementation of a rule is the bug (`AD-15`).
4. **No game-specific identifier in an engine crate.** A game's name belongs in its
   content and packaging, never in the engine's wire contracts. This is what makes a
   second game cheap, and it is *not yet true* of every schema string in
   `games/worldspiracy/src/core` — fixing that is part of the extraction.
5. **Cite the spec where the rule lives** (`02:02.7`, `AD-21`, `R6`). If a change
   alters behaviour a specification describes, the specification changes in the same
   commit; a code/spec disagreement is the bug.
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
