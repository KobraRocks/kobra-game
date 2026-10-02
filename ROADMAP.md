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

## Slice 2 — the authoring pipeline an agent can drive  *(confirmed)*

**Goal.** Produce something a player could install, through the one path the editor will later
be a face on (`AD-43` property 4).

Deliverables: `validate`, `lint` (the Lua lint of `AD-39` when it exists), `pack`, `install`,
and `play` as commands over the same library — never a second implementation of any step.

**Exit criterion:** an agent edits content, validates it, packs it, installs it into a game
folder, and runs a scenario against the installed copy — one command each, no terminal
archaeology.

## Slice 3 — the native host  *(confirmed)*

**Goal.** The game runs in a window and draws the frame description (`AD-38`).

Deliverables: a `wgpu` + `winit` host crate linking the engine as a library; input, audio and a
game-folder contract (where a game sits, how it is selected, where saves and mods live). This
slice is where the mod-manifest schema belongs, and it is the first slice that needs a human:
nothing here can be verified by an agent past "the frame description is right".

**Exit criterion:** `make check` plus a scenario run that opens a window and exits cleanly on
a headless CI runner — and a human confirms it renders on a real machine.

## Slice 4 — Lua, then plugins  *(confirmed)*

**Goal.** Content-shaped logic without an engine release (`AD-39`), and native capability
extension (`AD-40`).

Deliverables: the `mlua` host behind the existing `install` seam; the integer determinism rules
and the lint; then the tier-3 C ABI with its allowlist, hashes and crash naming.

**Exit criterion:** a game-shipped Lua hook changes a resolution and the golden replays say so
by name; a sample plugin registers one primitive and its hash lands in the save.

## Slice 5 — the editor  *(confirmed)*

**Goal.** A human authors content without touching JSON, over slice 2's pipeline.

Deliverables: a modder-facing GUI — project files, schema-driven forms, validate, publish,
install, play. Headless-first stays true: the GUI calls the same commands an agent does.

**Exit criterion:** someone who did not write the engine authors a mod through the editor and
plays it, with no terminal involved.

## The reference game

The sample game (`crates/kobra-core/tests/fixtures/`) grows into the **reference game**: the
engine's own game, and the one game allowed in this tree. It is not a fixture and not a
third-party game — it is the engine's demonstration and its benchmark, and it is what proves a
full game can be built on the engine *before* the first real one tries.

**Its universe is `Kobraverse`**: small but real, so a human can be shown what the engine does
rather than being told. Two consequences worth stating, because they are the point:

- It is **not the first game's universe**, and it borrows nothing from it. That is the standing
  test that this is a CRPG engine and not a Worldspiracy-shaped one (`AGENTS.md` trap 4).
- Its content ids carry **its own prefix** (`kobraverse.*`) while the engine's wire schemas stay
  `kobra.*`. It is where the content-namespace rule is demonstrated first; the owed split of the
  *core* content (rules, powers, gear) is a separate change, because that one regenerates the
  golden replays.

Three jobs:

1. **Coverage.** Every mechanic the specifications define has a scenario in it (`AD-44`). A
   mechanic with no scenario is not demonstrated, and therefore not ready.
2. **Scale.** It carries the committed benchmark scene (`07`, `AD-24`): a stress scenario that
   drives the frame-time and memory budgets with *generated* actors, not hand-authored ones.
   Breadth of mechanics, depth of scale, and no second content set to maintain by hand.
3. **Generality.** It is a different universe from the first game, with its own namespaces and
   no borrowings from it. That is the standing test that this is a CRPG engine and not a
   Worldspiracy-shaped one (`AGENTS.md` trap 4).

The rule that keeps it affordable: **the reference game is broad and shallow; a real game is
narrow and deep.** It grows one mechanic at a time, alongside the slice that makes that mechanic
testable, and it stays small enough that maintaining it never competes with building the engine.

**Everything the engine ships is demonstrated here first.** A feature that cannot be shown in
the reference game is either not finished or not needed.

## Readiness: when the first game starts for real

The first game moves from placeholder to real content when the reference game passes this gate.
Three clauses, all of which must hold. The first two an agent checks; the third is assembled as
evidence and **confirmed by the Product Owner**, because judgement is the one thing that cannot
be automated and pretending otherwise is how a gate becomes a formality.

1. **Coverage.** Every mechanic on the coverage list has reference content and at least one
   scenario. The list is derived by an agent from `02-rules-engine.md` and the milestone record,
   **approved once by the Product Owner**, and kept with the reference game. A mechanic missing
   from it is an *explicit exclusion with a reason*, never an omission.
2. **The full loop runs twice.** Character creation → explore → talk → fight → level → save →
   load → mod → play again, end to end, driven by scenarios. The second pass is different
   content on a different path, not a re-run of the first command stream.
3. **The engine goes quiet.** The work that produced the second pass required **no new engine
   capability** — no new command, projection, signal, content primitive or rules change. Bug
   fixes are not feature work and are listed separately. The evidence is the commit range for
   the second pass: the agent reports it, the Product Owner confirms it.

Clause 3 is the one that predicts a smooth real-game build. Coverage says the mechanics exist;
the loop says they compose; only a quiet engine says that *a game's worth of content* no longer
generates engine work.

**Next step: derive the coverage list and bring it back for approval.** Until that list exists
this gate cannot be applied, and the reference game's content has no definition of done.

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
| A library shell, as a product | a second game | if it returns it is a mod manager and an updater, not something that owns the runtime (`AD-42`) |

## Open questions

The engine's risks are in `architecture/README.md` under "What is still open" (R1, R6, R12–R15).
These are the roadmap's own, with what this session settled:

- **The coverage list does not exist yet.** The readiness gate's first clause needs it, so it is
  the next piece of work rather than an open question.
- **Is the assertable interface enough?** *Settled as: discover it by doing.* Slice 1 exposes
  draw items, layers and text ids; if a question repeatedly cannot be asked headlessly, that is
  the signal to extend the surface, not to reach for a window.
- **When does the first game move off placeholder content?** Gated on the readiness question
  above; not urgent, because the engine has several slices to land first.
