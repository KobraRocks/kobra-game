# Working in this repository (agents)

`CONTRIBUTING.md` is the source of truth for how to work here. This file records what is easy to
get wrong in *this* tree when editing with tools rather than by hand.

[`ROADMAP.md`](ROADMAP.md) is the **golden source for what to work on next**: read it after this
file, work the open slice, and update it in the same commit that lands one. A stale roadmap sends
the next session at finished work.

## What this is

The engine for one kind of game: a **single-player, offline, deterministic CRPG on the 4C
System**. It is a Rust crate with no window, no GPU and no server in it — a host links it (as a
library, or across the hand-written C ABI of `AD-2`), content configures it, and a game is a
universe expressed as content.

It is not a general-purpose engine. Everything here serves that genre and that rules system,
which is what lets a game be *a universe plus content* rather than a new engine.

## Where the code is

```
crates/kobra-core/    the engine: L0–L4 rules, the content model, save, validator,
                      the generated Master Table, the C ABI
architecture/         the normative documents and the decision log (AD-1…AD-44)
specs/                the 4C text, the Master Tables, the property test
tools/gen-tables/     the table generator (AD-14)
```

A **game is its own repository**, checked out beside this one — the first game is at
`../worldspiracy` — and nothing in it belongs to this repository. The engine's gates do not need
it. The one game that lives *here* is the **reference game** (`ROADMAP.md`): the engine's own
demonstration and benchmark, never a third-party game.

## The engine is AI-first (`AD-43`)

An agent must be able to test behaviour without a window. Four properties; breaking one is a
defect.

1. **Every capability is reachable headless** — through the library, with structured output and a
   non-zero exit on failure. A capability that exists only in a window is not done.
2. **No rule is reachable only through the interface.** If a control does something a command
   cannot, the command is missing.
3. **The human owns presentation.** State, projections, the action set, signals, string ids and
   the frame description are assertable data; pixels and feel are not claimed as tested.
4. **One pipeline.** Validate → lint → pack → install → play is one code path, shared by the
   editor, the tools and an agent's script. A second implementation of any step is the bug
   `AD-15` forbids for rules, applied to tooling.

The artifact is a **scenario** (`AD-44`): seed, setup, commands, assertions and expected hashes in
one file, run by `kobra-run`, failing with a structured diff.

## The gates

```sh
make check     # tables, formatting, the engine's own tests
make clippy    # lints, warnings denied
```

A game's conformance suite — its content, its golden replays — runs in that game's repository
against the engine, and the first game's is also a CI job here from a pinned checkout of it. **If
you changed the engine, that job is the end-to-end gate**; the engine's own tests cannot replace
it, because they cannot see a game's data.

## Traps

1. **Zero dependencies in the engine core, on purpose.** A dependency there needs a stated
   reason: it is a reproducibility liability and a permanent context cost.
2. **The simulation is integer-only.** No floats on state paths, no wall clock, no locale, no
   unseeded randomness. The golden replays prove it, and a divergence is a defect, not a flake.
3. **One rules implementation.** The engine resolves; content and scripts ask. A second
   implementation of a rule is the bug (`AD-15`).
4. **No game-specific identifier in an engine crate.** A game's name belongs in its content and
   its own repository, never in the engine's wire contracts — the engine's schemas are `kobra.*`:
   `kobra.save/1`, `kobra.content-load/1`, `kobra.content-pack/1`, `kobra.load-report/1`,
   `kobra.script-report/1`, `kobra.replay/1`, `kobra.ui-layout/1`.
   **Owed:** content *record* ids still carry the first game's prefix (`wsp.power.*`,
   `wsp.item.*`), because the engine's canonical power kernels match core content by id.
   Splitting core content (the engine's) from campaign content (a game's) is a content-namespace
   change, and it regenerates the golden replays.
5. **Cite the spec where the rule lives** (`02:02.7`, `AD-21`, `R6`). The specs are in this
   repository, under `architecture/`, and a change that alters described behaviour updates the
   spec in the same commit — a code/spec disagreement is the bug. `01`, `04` and `05` are stubs
   that keep the section anchors code cites.
6. **Delete dead machinery rather than documenting it.** A config key, an event or an exported
   symbol with no consumer is removed, not annotated.
7. **Version fields are bare `X.Y.Z`**, and an unparseable version fails its gate closed — never
   silently ordered or coerced to zero.

## Definition of done

- The gates for what you touched pass, and you can name which you ran.
- A fixed bug has a regression test named after the failure, and it fails on the old code.
- Described behaviour that changed has its specification updated in the same change.
- `cargo fmt` is clean and `cargo clippy` is warning-free.
