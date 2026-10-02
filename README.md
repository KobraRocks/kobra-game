# Kobra — the engine

The engine for **one kind of game**: a single-player, offline, deterministic CRPG
built on the [4C System (Libre Edition)](games/worldspiracy/specs/4c_system.md).

It is not a general-purpose engine. It is shaped by one genre and one rules system,
so that a game built on it is a **universe plus content** rather than a new engine.
The decision is recorded in the first game's decision log, `AD-38`–`AD-42`.

**Status: the old toolchain is gone, and the engine is being extracted here.** What
this repository used to be — a launcher that served a web game to the player's
browser, a packager, a fixture game, and the specifications for all three — is
retired. It was *removed* rather than kept as reference, because a tree that an
agent must re-learn as "not needed" is a tax on every task that follows. Git
history holds all of it (`git log --diff-filter=D -- launcher`).

The engine exists today as a Rust crate inside the first game's checkout:

```
games/worldspiracy/src/core/      the engine: L0–L4 rules, the content model,
                                  save, validator, the generated Master Table, the ABI
games/worldspiracy/specs/         the 4C text, the authoritative Master Tables,
                                  the property test, the table generator
games/worldspiracy/tests/replay/  the golden replays — the determinism gate
```

That checkout is a **separate repository**; this one ignores `games/`. It is where
the engine is developed until the extraction lands. Moving the engine here, with
fixtures of its own so it can be tested without a game, is the next change.

## What this repository is for

- **The engine, as crates, with no game-specific code in them.**
- **The 4C System transcription** — the Master Tables, the property test, the table
  generator — and the deterministic replay oracle.
- **Fixtures of its own**, enough content to test the rules without a game.

A game — Worldspiracy first — lives in its own repository and is built on the
engine's **public surface**: content packs, Lua (`AD-39`), the host's asset and
interface contracts, and optionally a native plugin against the versioned C ABI
(`AD-40`). If a game needs something that surface cannot express, the engine grows
a documented feature; the game does not fork the engine.

## Working here

`AGENTS.md` records what is easy to get wrong in this tree. `CONTRIBUTING.md` has
the setup, the gates, and the rules of the house.
