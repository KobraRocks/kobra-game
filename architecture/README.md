# The Kobra engine — Architecture

**Status: the engine is this repository, and the web runtime is gone.** The shipped
runtime is a native Rust host (`AD-38`); Lua is the glue and modding language
(`AD-39`); modding has four tiers including native plugins (`AD-40`); the primary
artifact is one the player owns, with Steam secondary (`AD-41`); and the launcher is
retired (`AD-42`). The browser runtime these documents were originally written
against — the wasm build, the DOM interface, the editor document, the launcher — was
removed from the tree in the pivot's first change. Git history holds it.

Read the decision log first: `06-decisions.md` opens with an **index of what the
pivot supersedes**, and each affected document carries a pointer at its head. A
document marked *superseded* is kept for its design intent, not as a description of
what runs.

**Paths in these documents are the engine's**, relative to this repository's root
(`crates/kobra-core/`, `specs/`, `tools/`, `architecture/`). A *game's* files — its
content, its golden replays, its mod fixtures — belong to that game's repository,
checked out beside this one.

`01`, `04` and `05` are **stubs**: they described the browser runtime and the web
toolchain that `AD-38`–`AD-42` removed, and they keep only the section anchors that
code and specifications cite, each mapped to the decision or file that replaced it.

**Scope:** the engine — the 4C rules core, the content model, the modding surface,
the runtime architecture, and the delivery path.
**Audience:** whoever builds this. It is written to be argued with.

## The documents

| Doc | Answers | State |
|---|---|---|
| [01-runtime-and-language-split.md](01-runtime-and-language-split.md) | *Stub.* It described the browser runtime's artifact split. | Retired by `AD-38`; keeps its section anchors, each mapped to what replaced it |
| [02-rules-engine.md](02-rules-engine.md) | How the 4C System becomes a deterministic simulation: the Master Table, Rank Values and Row Steps, traits, powers, combat, time, the content data model. | **Normative** |
| [03-content-and-mods.md](03-content-and-mods.md) | The content pipeline, the asset overlay, how mods override and extend content, the modding tiers, what a mod may not do. | §03.7's tiers superseded by `AD-39`/`AD-40` |
| [04-editor.md](04-editor.md) | *Stub.* It described the editor as a document served by the launcher. | Retired by `AD-42`; authoring is headless-first |
| [05-delivery-and-roadmap.md](05-delivery-and-roadmap.md) | *Stub.* It described the web toolchain's build, packaging and gates, and the first game's milestones. | Retired by `AD-38`/`AD-42`; the milestones are that game's `CHANGELOG.md` |
| [06-decisions.md](06-decisions.md) | The decision log (AD-1…AD-42), with the supersession index. | **Normative** |
| [07-performance.md](07-performance.md) | The runtime acceptance bar, the memory and GPU rules, and the parallelism rules that preserve determinism. | **Normative**; its browser sections are marked retired |
| [08-asset-pipeline.md](08-asset-pipeline.md) | **The art contract:** units and scale, the shared rig, gameplay anchors, mesh/material/texture/animation budgets, the import validator, the pinned Blender pipeline. | **Normative** |
| [09-moddable-ui.md](09-moddable-ui.md) | The interface contract: projections with subscription and revisions, engine-computed signals, the layout vocabulary, the renderer capability object. | Contract stands; the DOM implementation is re-derived (`AD-42`) |

## The invariants

Three rules hold the design together, and they are stated because every other
decision follows from them:

1. **The save depends only on the engine core.** Anything that can change the outcome
   of the game lives in the core or in content data. If it lives in a script, a save
   is not reproducible and a mod cannot reliably change it.
2. **The renderer is disposable.** Killing the renderer must never change game state.
   It consumes a frame description; it never produces one.
3. **Content is data, never code, by default.** A mod that only adds items, powers,
   sectors or quests ships zero executable code (`AD-7`, `AD-8`). A *layout* and a
   *theme* are data; a *renderer* is a script, scoped and removable without touching
   a save (`09`, `AD-36`, `AD-39`).

## Non-goals

Stated so nobody designs for them by accident:

- **No network, no multiplayer, no cloud, no telemetry.** The binary never talks to
  the network to run; the only exception is an update check the player asks for
  (`AD-41`). Updates are ours to ship, not a store's.
- **No game-specific identifier in an engine crate.** A game's name belongs in its
  content and its own repository. This is what makes a second game cheap.
- **Full 3D, one cast, many cameras.** A **multi-profile camera** (`AD-30`):
  top-down, isometric or ground-level per context. The cast is angle-independent; the
  world is not, and it must be built and lit for every profile that ships.
- **Perception is never camera-based.** Sight, cover and targeting come from the
  character's eye level and the heightfield, never from where the camera happens to be
  (`AD-28`, `AD-30`).
- **The demo's art is placeholder, and that is a plan.** It ships as a **presentation
  pack**; the final cast is a pack against the same descriptors and swaps in without
  touching rules, content or saves (`AD-29`, `AD-31`).
- **No mod sandbox in the security sense.** Mods are trusted user data (T14). What is
  owed is *crash isolation* (a bad mod disables itself, a crashing plugin is named) and
  *stability* (a versioned API), not a sandbox (`AD-8`, `AD-40`).
- **A native plugin ABI exists, and it is small.** Tier 3 is a shared library behind
  one versioned C entry point, loaded from an allowlist, whose hashes are recorded in
  the save (`AD-40`). It is not a general plugin framework.

## What is still open

| # | Open question | Where |
|---|---|---|
| R1 | **Resolved to a task, not a blocker.** The CSV's colour block is provably corrupt (22 of 286 cells disagree with the primary source, its header is permuted, and it carries no colours for the Advanced table). The authoritative source is the Libre Edition *Master Tables* document, whose colours appear **both** as cell text and as cell shading — two independent encodings that cross-validate. Transcribe from it; treat the CSV as a cross-check that is expected to disagree. | §02.2 |
| R6 | World-sim snapshot size against the save budget and the revision-retention cost. | §02.9, `AD-11` |
| R12 | The asset contract is violated by the first contractors: per-artist rigs, scales or animation conventions, making the cast unshareable. | `08`, `AD-31` |
| R13 | Environment art is built at placeholder fidelity, then has to carry a fully 3D cast and a ground-level camera. | `AD-30` |
| R14 | **The first game's M3 content is placeholder, deliberately.** The generated district is integer value noise, and its campaign is one small loop written to exercise the frame. | §05.5 |
| R15 | **The UI API is public and permanent once UI packs ship.** A projection shape or a layout key cannot change without a major version and a compatibility story, and the shipped panels must keep exercising the contract. | `09`, `AD-36`, `AD-37` |
