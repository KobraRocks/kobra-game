# Roadmap

**The golden source for "what do I work on next".** Read it after `AGENTS.md`.

It is a **living document**: the change that lands a slice updates this file in the same
commit. A stale roadmap is worse than none, because it sends the next session at work that
is already done.

## How work is ordered

A feature's priority is **how many human steps it removes from the loop** (`AD-43`) — not how
visible it is. The Product Owner is the human; the engine and its tooling exist so that an
agent can close a loop alone, and only *presentation* needs a person.

Every slice states a goal, an **exit criterion** that is a checkable property rather than an
aspiration, its deliverables, and what proves it. A slice is done when its exit criterion
holds, `make check` passes, and the first game's conformance suite still passes.

## Where we are

**The engine exists and is green; nothing else does.**

| | State |
|---|---|
| `crates/kobra-core` | the 4C rules, the simulation, the content model, the save, the validator, the ABI — zero dependencies, 190 tests, plus a sample game that runs end to end |
| `specs/`, `tools/gen-tables/` | the 4C sources of truth and the compiled-table generator (`AD-14`) |
| `architecture/` | the normative set and the decision log, `AD-1`…`AD-44` |
| The web runtime, launcher, packager, fixture game, their specifications | **removed** (`AD-38`–`AD-42`); git history holds them |
| The first game | its own repository beside this one, 44 files, 37 conformance tests |
| Not built | a headless driver, a scenario format, the authoring pipeline, the native host, the editor, the game-folder contract, Lua, plugins |

## Slice 1 — an agent can test a mechanic without a window  *(agreed)*

**Goal.** Close this loop with no human step and no compilation (`AD-43`, `AD-44`):

    author a scenario → run it → read a structured failure → fix → re-run

| Step | Deliverable | Exit criterion |
|---|---|---|
| 1.1 | The frame description reachable from the library, not only across the C ABI | a test reads a draw item and its layer from Rust, without `kobra_alloc` |
| 1.2 | `kobra-run`: a scenario in, structured JSON out, non-zero exit on failure | a failing scenario exits non-zero and names the assertion and its path |
| 1.3 | The scenario schema: seed, setup, commands, assertions, expected hashes | the sample game runs as a scenario file rather than as a Rust test |
| 1.4 | Assertions over projections, the action set, signals, previews and the frame description | a scenario asserts "the journal lists these three entries, in this order" |

**Exit criterion for the slice:** from a clean checkout, an agent authors a scenario, runs it
headless, reads a structured failure, fixes the content, and re-runs — no human step.

**Not in this slice:** the host, the editor, packaging, rendering.

## Slice 2 — the authoring pipeline an agent can drive

**Goal.** Produce something a player could install, through the one path the editor will later
be a face on (`AD-43` property 4).

Deliverables: `validate`, `lint` (the Lua lint of `AD-39` when it exists), `pack`, `install`,
and `play` as commands over the same library — never a second implementation of any step.

**Exit criterion:** an agent edits content, validates it, packs it, installs it into a game
folder, and runs a scenario against the installed copy — one command each, no terminal
archaeology.

## Slice 3 — the native host

**Goal.** The game runs in a window and draws the frame description (`AD-38`).

Deliverables: a `wgpu` + `winit` host crate linking the engine as a library; input, audio and a
game-folder contract (where a game sits, how it is selected, where saves and mods live). This
slice is where the mod-manifest schema belongs, and it is the first slice that needs a human:
nothing here can be verified by an agent past "the frame description is right".

**Exit criterion:** `make check` plus a scenario run that opens a window and exits cleanly on
a headless CI runner — and a human confirms it renders on a real machine.

## Slice 4 — Lua, then plugins

**Goal.** Content-shaped logic without an engine release (`AD-39`), and native capability
extension (`AD-40`).

Deliverables: the `mlua` host behind the existing `install` seam; the integer determinism rules
and the lint; then the tier-3 C ABI with its allowlist, hashes and crash naming.

**Exit criterion:** a game-shipped Lua hook changes a resolution and the golden replays say so
by name; a sample plugin registers one primitive and its hash lands in the save.

## Slice 5 — the editor

**Goal.** A human authors content without touching JSON, over slice 2's pipeline.

Deliverables: a modder-facing GUI — project files, schema-driven forms, validate, publish,
install, play. Headless-first stays true: the GUI calls the same commands an agent does.

**Exit criterion:** someone who did not write the engine authors a mod through the editor and
plays it, with no terminal involved.

## The first game's roadmap is the game's

Content milestones — the campaign, the art, the balance — belong to the game's repository, in
its own `CHANGELOG.md` and its docs. This file plans the **engine and its tooling**. When the
two disagree about what to do next, this file wins for engine work and the game's repository
wins for content.

## Deferred, with what would unblock each

| Item | Blocked on | Notes |
|---|---|---|
| Content-namespace split (`wsp.power.*` ids → engine vs game namespaces) | a decision to spend a golden regeneration | the engine's canonical kernels match core content by id, so core content and campaign content must be separated in the same change (`AGENTS.md` trap 4) |
| Moving the core content into the engine | the namespace split above | core rules/powers/skills/gear are the engine's; a game's campaign is the game's |
| The game-folder contract | slice 3 | where a game sits, how it is selected, saves, mods, entry point — and the mod-manifest schema that has no file today |
| Golden-image capture | slice 3 | it verifies what a human also sees, so it is not where the marginal agent capability is |
| The launcher, as a product | a second game | if it returns it is a mod manager and an updater, not a server (`AD-42`) |

## Open questions

The engine's risks are in `architecture/README.md` under "What is still open" (R1, R6, R12–R15).
This list is the roadmap's own:

- **Does the sample game grow into a reference game?** It currently proves the engine runs *a*
  game. Making it exercise every mechanic would let engine changes be tested without the first
  game's content — at the cost of maintaining a second content set.
- **How much of the interface becomes assertable data?** Slice 1 covers draw items, layers and
  text ids. Whether that is enough for the questions that actually come up is something the
  first few scenarios will answer.
- **When does the first game move from placeholder content to real content?** That is a Product
  Owner call, and it is the point at which the editor (slice 5) stops being optional.
