# 04 — The shipped editor

> **Superseded by AD-42.** There is no `/editor` document and no browser host: the launcher
> is retired. Authoring is headless-first — project files, validate, Lua lint, pack — and the
> graphical editor becomes a modder-facing tool built later. The ideas here (projects as
> versioned documents, one validator, a publish that re-reads what it wrote) survive; the
> DOM implementation does not.

The editor is a first-class product deliverable, not a debug tool. It ships inside
`game/`, it runs in the player's browser from the same loopback origin as the game,
and it exists so that a player can become a modder without installing a toolchain.
This document specifies what it is, how it is structured, and how it uses the
architecture of `01`–`03`.

## 04.1 What it is for

Three audiences, in priority order, because the priority decides what gets built
first:

1. **The player-modder.** Wants to change a power, add items, reskin the UI, write a
   small quest, translate the game, and see the result immediately. Needs: no
   install, no build, immediate playtest, one-click publish.
2. **The content author (us).** Wants to build the campaign's sectors, NPCs, quests
   and dialogue in the same tool the modders use — which is the only way to
   guarantee the tool is good enough for modders.
3. **The systems designer.** Wants to inspect the rules, simulate an encounter, and
   see distributions. This is the audience that benefits most from `wsp_preview`.

Non-audiences: 3D modellers, audio engineers, programmers editing engine Rust. The
editor links out to the filesystem workflow for those (`§04.9`).

## 04.2 How the editor is reached

The editor is **its own document**, served by the launcher at `/editor` from
`game/editor/` (FR-AST-16), not a mode of the game page (`AD-12`). That route is a
launcher capability this architecture required and got (`AD-23`).

```
GET /editor            → game/editor/index.html
GET /editor/modes/*.js → game/editor/modes/*.js        (confined, same rules as /engine/*)
```

Four consequences decide the rest of the design:

- **It is same-origin, so it needs no session machinery.** The editor inherits the
  `kobra_session` cookie of whatever established a session on this origin and reads the
  non-HttpOnly `kobra_csrf` cookie, which is exactly what the double-submit check
  compares. It therefore saves, installs mods, and reads the player's data with no new
  token flow and no new API surface. If no session exists yet, every API call answers
  `401` and the editor says so instead of pretending to work (`§04.8`).
- **The play document offers it, in a new tab.** A URL is not an on-ramp for a player
  (`§04.1`), so the start screen carries an editor entry. It is an
  `<a href="/editor" target="_blank">` rather than a control the shell handles, for two
  reasons: the route stays reachable when the shell's own boot fails — the way out of a
  broken build should not depend on the broken build — and the play page sends
  `POST /__kobra/goodbye` on `pagehide` (`01:01.9`), which back-dates the launcher's idle
  clock by two timeouts, so navigating *in place* would stop the server the editor is
  still loading from. A new tab leaves the play page alive and its heartbeat running.
  This is the one place the two documents are coupled, and it is one anchor: no module,
  no protocol, no shared state.
- **The play and editor module graphs are separate.** The game page never imports
  editor code, so the editor costs the play path nothing — which matters because
  performance is the master goal (`07`). Both documents share the wasm core, the
  data-API client and the widget library, because they are the same origin and the same
  package.
- **`server.entry_path` can point at it.** A modder's build sets `entry_path: "/editor"`
  so the launcher opens the editor directly (FR-LNCH-9), which is the whole reason the
  key exists.

The editor shares the **heartbeat** with the game and must keep it running even when
idle, or the launcher's 90 s idle timer drains the server out from under it
(`docs/authoring-a-game.md` §Heartbeat). Undo/redo is the editor's own concern (project
documents); the sim has no undo, and the editor replays a project revision into a
scratch sim instead (`§04.6`).

## 04.3 Structure

```
game/editor/                 # served at /editor (FR-AST-16)
├── index.html             # the editor document
├── index.js               # entry: builds the workspace shell
├── shell-ui.js           # editor chrome: menus, panels, docking, command palette
├── project.js            # the project document, undo/redo, dirty state, revisions
├── store.js              # persistence adapter over the data API (slots, conflicts)
├── publish.js            # pack builder → tar → zstd → POST /api/mod
├── lib/zstd.wasm         # browser-side zstd encoder (AD-13)
├── models/               # pure editors over one record type, all using shared forms
│   ├── powers.js  items.js  origins.js  npcs.js  characters.js
│   ├── quests.js  dialogue.js  sectors.js  factions.js  locales.js
│   ├── assets.js         # model/rig/clip import, contract report, LOD + budget view
│   ├── camera.js         # profiles, transitions, cinematic timeline
│   └── balance.js
├── dsl/                  # the effect-DSL editor (form view + raw JSON view)
│   ├── editor.js  palette.js  graph.js
├── validate.js           # thin wrapper over wsp_validate_content + report renderer
├── preview.js            # scratch sim, deterministic playtest, scenario bookmarks
└── shared/               # forms, schema-driven widgets, integer/dice inputs, i18n
```

`models/*` are thin by design: a model describes a record type's fields, its
references, and its grouping, and the shared form engine renders it. A new record
type is data plus a small descriptor, which is what keeps the editor able to track
the content model as it grows.

**As built (the M4 walking skeleton — `05:05.5`).** The slice that landed is the one
vertical that closes the loop rather than the whole of `04`, and the tree above is
the target it is a step toward. What ships:

```
src/web/editor/
├── index.html      editor.css      # the document and its stylesheet
├── index.ts                        # the workspace wiring: the boot sequence, the
│                                   #   heartbeat, the buttons, and nothing else
├── project.ts                      # the project document, the emitted files, undo
├── store.ts                        # save slots, revisions, 409 conflicts
├── publish.ts                      # tar → zstd → install → verify
├── validation.ts                   # base content + Core → wsp_validate_content
├── forms.ts                        # the shared form engine (field descriptors)
└── models/items.ts                 # the one record type: item
```

Two modules import from outside the editor and both are deliberate: `validation.ts`
uses the sim worker's `Core` glue, because the ABI has one implementation and a
second one would be a second ABI, and `publish.ts`/`project.ts` use
`src/web/shared/tar.ts` and `zstd.ts`, because the archive has one writer
(`§04.7`). The directories the sketch names but this slice does not need —
`models/{powers,origins,npcs,…}.js`, `dsl/` and `preview.js` — are **absent rather
than empty**: `05:05.5` and rule 3 of `CONTRIBUTING.md` both say a placeholder is
not progress.

## 04.4 The editing surface

| Workspace | Edits | Built on | Ships when |
|---|---|---|---|
| **Content library** | browse/search every merged record, see its source pack, jump to the editor for its type; the conflict/override report | `ContentRegistry` from `03:03.5` | v1 |
| **Powers & effects** | the DSL: trigger/condition/effect graphs, parameter declarations, live validation, "simulate this power" | `wsp_validate_content`, `wsp_preview` | v1 |
| **Items & gear** | weapons, armour, grenades, potions, the lesser/greater variant transform (`4c:1565-1569`) | DSL | v1 |
| **Rules & distance** | the rules pack's distance parameters (`02:02.6`): the verbatim 4C tables, the per-distance steps, and every override — with the derived tile counts shown **read-only** and the granularity block locked | `wsp_validate_content`, `wsp_preview` | v1 |
| **Characters** | the 4C sheet (traits, secondary traits, skills, powers), rolled/budgeted/authored modes (`02:02.3`), NPC parties | `wsp_character_create` | v1 |
| **Sectors & map** | the **1 m tile grid** (`AD-33`) and its graph, terrain, material value, named places, occupancy, **per-tile footprints and opening clearance heights**, generator parameters, **the per-tile heightmap and blocking-height layer**, and a line-of-sight overlay that shows which shots are clear, covered or blocked from a selected position | world model, `02:02.12` | v1 |
| **3D viewport** | free-fly or profile-locked preview of the level as the game renders it, with the physics/anchor helpers (`eye_level`, capsule, cover anchors, attachment slots) drawn on demand | renderer, `AD-30` | v1 |
| **Models, rigs & animation** | import a `.glb`, preview it **on the shared skeleton**, play and scrub its clips, see the validator's contract report, and estimate draw calls and texture memory before accepting a delivery | `08-asset-pipeline.md`, import validator | v1 |
| **Visuals & slots** | the visual descriptor for any record: mesh and rig, LODs, clip set, height, anchors, and the equipment attachment slots (`mainhand`, `offhand`, `head`, `body`, `back`) — with the pack `kind` (`content` \| `presentation`) chosen by the project | descriptor model, `AD-29` | v1 |
| **Camera profiles & cinematics** | the profile set (`top_down`, `isometric`, `ground_level`, `dialogue`, `overview`), their targets, transitions and per-profile budgets, and a timeline for the camera director that content steps can drive | `AD-30`, camera state model | v1.1 |
| **Encounters** | an encounter is a map region (a set of tiles) + combatants + initial state; "playtest this" launches a scratch sim at that state, and the odds panel names its modifiers — *"+2 RS, high ground"*, *"−2 RS, cover"* | `02:02.5`, `02:02.12`, `wsp_preview` | v1 |
| **Quests & dialogue** | node graphs with conditions, effects, skill checks; the string ids are authored inline | DSL + campaign predicates | v1.1 |
| **Balance** | distributions: damage by class, success probability by effective rank, fortune economy, encounter difficulty | `wsp_preview` and the L0 primitive | v1.1 |
| **Localization** | string tables, side-by-side against English, partial-translation warnings | `assets/locales/*` | v1.1 |
| **Mod packs** | project → pack, dependencies, overrides, publish, install/uninstall | `§04.6` | v1 |

Everything is keyboard-reachable and every field is a labelled control with an
associated error message; this is a data tool whose users are editing thousands of
records, and the accessibility work here is also the productivity work.

## 04.5 Projects

A project is the **authoring source** for one or more content packs, and it is
persisted as a save slot (`AD-13`), because that is the only arbitrary-JSON store
the platform offers.

```json
// slot: edit.noir-overhaul          (slot pattern ^[a-z0-9][a-z0-9._-]{0,63}$)
{
  "schema": "worldspiracy.editor-project/1",
  "id": "noir-overhaul",
  "modified": "2026-09-17T10:00:00Z",
  "engine_min": "0.1.0",
  "packs":   [ { "pack": "noir.gear", "records": [ … ] } ],
  "scripts": [ { "path": "scripts/hooks.js", "source": "…" } ],
  "assets":  [ { "path": "textures/ui/noir-frame.png", "source": "base64:…" } ],
  "notes": "…"
}
```

Rules and consequences:

- **The project is the pack**, so publishing is a copy, not a translation step.
  There is no separate intermediate format to drift.
- **Slot namespace.** Projects use the `edit.` prefix. Play saves never do. The
  game's save/load UI filters `edit.*` out of its list; the editor's project browser
  filters everything else out. Both read the same `GET /api/data/saves`.
- **Revision conflicts are real** and are handled like a game save: pass
  `if_revision`, and on `409 conflict` show the metadata and let the author choose
  overwrite or reload (`docs/authoring-a-game.md` §Saves). Two editor tabs are
  representative of two modders on one USB stick, not an exotic case.
- **Autosave** respects the launcher's `writes_per_minute: 100` and
  `bytes_per_minute: 64 MiB` budgets: debounce to at most one write per 20 s and
  force a write on destructive operations. A project over ~40 MiB is warned about,
  because `max_request_bytes` is 64 MiB and base64-encoded assets inflate by 4/3.
- **Import** uses a plain `<input type="file">` and `FileReader`. This is *not* the
  File System Access API and does not request storage permission, so it does not
  violate FR-SHELL-1; the authoritative copy still lands in `data/` through the data
  API. **Export** is `GET /api/export/{slot}`, which serves the save file as a
  download.
- **Large binary assets do not belong in the project.** The project stores small
  assets inline (icons, UI frames) and *references* large ones by path; the
  filesystem workflow (`§04.9`) is the way to bring in real art. This keeps projects
  in the save-slot size budget where they belong.

## 04.6 Playtest

The editor's reason to exist is the loop from edit to consequence. Three mechanisms,
in increasing cost:

1. **`wsp_preview`** — resolve a hypothetical action against the current state and
   return the colour distribution and effect list, with no commit
   (`02:02.10`). Powers the "what does this do" panel and the balance views. Instant.
2. **Scratch sim** — `wsp_new_campaign` returns an opaque handle, so the editor
   holds a **second sim** alongside the live one and replays a project revision into
   it with a fixed seed. "Playtest this encounter" starts the encounter in the
   scratch sim, rendered by the real renderer into the real canvas, with a banner
   that says it is scratch. Fully deterministic, so a report is `(project revision,
   seed, command log)` and reproduces exactly.
3. **Install and play** — publish (`§04.7`) and start a normal session. The only
   way to test save/load interaction, mod ordering, and real pacing.

Revision-based playtest is what makes the scratch sim cheap: the project document
is an immutable revision list, so "replay revision 41" is exact, and undo is just
selecting revision 40. The editor never mutates a live sim to undo a design change.

## 04.7 Publishing

```
project (editable doc)
  → build: pack files, string tables, scripts, assets         (in memory)
  → validate: wsp_validate_content on the full merged load list
  → emit: mod.manifest.json + assets/data/mod.json + assets/**
  → tar (JS, POSIX ustar, regular files only, no traversal)
  → zstd (game/editor/lib/zstd.wasm)
  → POST /api/mod { "action":"install", "archive_bytes": "<base64>" }
  → verify: GET /mods/<id>/assets/data/mod.json  (must be reachable and match)
  → report: enable it, offer "play with this mod now"
```

Hard requirements that come straight from the platform:

- **The archive must be `.tar.zst`.** `installModLocked` calls `extractTarZstd`, and
  `Launcher-spec.md` §27.5 now fixes the layout: the mod's own file tree at the
  archive root, **no wrapper directory**, because extraction lands in
  `data/mods/<id>/`. The writer must still match the reader *exactly*: POSIX tar,
  regular files only, paths relative and confined, no symlinks, no absolute paths,
  no `..`. The publish path gets a test that runs our writer's output through the
  launcher's own reader. `tools/pack.mjs` is that writer for the text workflow —
  deterministic bytes, refusing what the extractor rejects, read back by
  `TestExtractModArchiveFromTheGameCLI` in `make mods` — so the editor's publish path
  should produce the same archive rather than a second format.
- **Validate before packing** (`AD-15`). A published mod that fails validation is a
  bug in the editor, because the editor had the chance to refuse.
- **Size guard.** Warn at 32 MiB and refuse above ~45 MiB of archive, because the
  base64 transport inflates by 4/3 against a 64 MiB request ceiling.
- **Verify after install.** A silent install failure is worse than a loud one; the
  probe confirms the files are actually servable, and catches `serve_mods: false`.
- **`mod.manifest.json` is ours to write**, matching
  `architecture/schemas/mod.manifest.schema.json`: id equal to the directory name,
  version as bare `X.Y.Z`, `assets[]` entries with `type` from the schema's enum,
  `base_release` set to the running release so a mismatch produces the spec's
  warning rather than silence (FR-AST-8).
- **A pack may be published with no code**, and that is the common case. Tier-2
  scripts are opt-in per project and are shown in the publish summary with their
  hashes, so the author knows what they are shipping.

**As built.** The sequence above is implemented end to end for one record type, and
the parts that were easy to get wrong are the parts with a gate:

- **One writer, and byte-identity as the assertion.** The `ustar` headers, the entry
  order, the name rules and the zstd frame are `src/web/shared/tar.ts` and
  `src/web/shared/zstd.ts`; `tools/pack.mjs` and the editor both import them, and
  only the *file list* is read locally, because only Node can enumerate a directory.
  `tests/web/editor.test.mjs` packs the same tree through both paths and requires
  equal bytes, so "the editor should produce the same archive rather than a second
  format" is enforced rather than reviewed.
- **The zstd encoder is ours, and uncompressed** — `AD-13`'s amendment records why:
  byte-identity rules out two encoders, and the frame writer emits valid `Raw_Block`
  frames, which the launcher's real reader accepts. The size guard in the bullet
  above is what bounds the cost.
- **Validate before packing, with the engine's own verdict.** The Validate button
  calls `wsp_validate_content` through the same `Core` glue the sim worker uses, on
  the same `worldspiracy.content-load/1` document `tools/validate.mjs` builds, and
  the report's paths are bound back to the form fields they name. Publish re-runs
  it and refuses on any error, so a published mod that fails validation is
  impossible rather than merely unlikely.
- **Verify after install.** The probe re-reads `assets/data/mod.json` through
  `/mods/<id>/assets/…` and reports a `serve_mods: false` configuration by name
  rather than leaving the author to guess why playtest changed nothing.
- **What the slice does not do**, stated so it is not discovered: there is no content
  library, no scratch sim, no 3D viewport and no heightmap layer; the `item` model
  covers the fields `l1::items` reads except `modifiers`; and the project carries one
  pack. Those are the remaining `04:04.4` workspaces, and `05:05.5` sequences them.

## 04.8 Failure modes and their handling

| Failure | Handling |
|---|---|
| `serve_mods: false` | Publish verifies, fails loudly, and explains that mods cannot be loaded in this configuration (no route exists) |
| `413 too_large` | The pre-flight size guard should have caught it; on 413, report the exact limit and the archive size |
| `429 rate_limited` | Autosave debounce keeps us under 100 writes/min; on 429, back off exponentially and keep the document dirty |
| `409 conflict` | Show a revision diff summary and let the author choose overwrite/reload; never silently clobber |
| `read_only` data dir (degraded mode, `/api/state.data_writable === false`) | Enter **read-only editor** mode: everything works, saving and publishing are disabled with an explanation, export is offered instead |
| No session yet (editor opened before the game ever booted, or the session expired) | Say so plainly and offer two actions: open the game, or relaunch from the launcher. Every data-API call answers `401` (`E15`) until a session exists; the editor must never look like it saved |
| `missing_dependency` mod | Surfaced in the load report before playtest; the launcher already reports it as disabled |
| A script throws during playtest | Disabled for the session with a diagnostic (`03:03.7`); the editor shows the stack in the console panel, not as a modal |
| The worker crashes | The shell restarts the worker and restores the last document revision from `store.js`; the editor keeps the project in the main thread precisely so a worker crash is not data loss |

The last row is the reason `project.js` and `store.js` live on the **main thread**:
the editor's authoritative document must outlive the engine worker it is testing.

## 04.9 The filesystem workflow, and what the editor deliberately omits

The editor is not a replacement for a real toolchain. It links out to it:

- **Art.** The editor imports and references textures, audio and models; it does **not**
  author them, because **Blender does** (`AD-32`). The editor's 3D viewport shows the
  game's own rendering and the gameplay anchors; geometry, rigs and animation are Blender's.
  A delivery is normalised and exported by the headless compiler (`08:08.11`) and accepted
  here, against the contract report rather than by eye.
- **Level geometry.** Spatial layout is authored in Blender and exported; the editor owns
  the **gameplay layers** the rules read — the sector heightfield, blocking heights, cover
  and spawn points — seeded from the build's proposals and confirmed by a designer
  (`08:08.11`). The `vfs` overlay means an artist can also drop a file into
  `data/mods/<id>/assets/` and reload.
- **Bulk content.** Large packs are edited in a text editor and validated by the
  same CLI the editor calls (`tools/validate.mjs`), which is also the CI gate. The
  editor's raw-JSON view exists so the two workflows are the same document.
- **Code.** Tier-2 scripts are edited in the editor's text pane or externally; the
  editor does not attempt to be an IDE. It does run the script through the same
  load path so a syntax error is visible immediately.
- **Version control.** Projects export as JSON so they can live in git. The editor
  does not talk to git.

Explicit non-goals, each with a real tool behind it instead: a 3D mesh editor, a rig
editor and an animation editor (**Blender**, `AD-32`); an audio editor; a shader graph
editor (WGSL is text, and hot-reloads); and a scripting language of our own.

### The daily loop, end to end

This is the sequence the studio and its contractors actually live in, and the order matters
because two steps are easy to skip (`AD-18`, `AD-32`).

```
author in Blender                     models, rigs, animation, level geometry, anchors
        │
        ├─ blender -b -P tools/blender/exporter.py        normalise, export, LODs, KTX2,
        │                                                 thumbnail, contract report (08:08.11)
        ▼
presentation pack  data/mods/<pack>/assets/models/…      NOT game/ — that is a build output
        │
        ├─ declared in  data/mods/<pack>/assets/data/mod.json    overrides + pack asset refs
        ├─ pack enabled in data/config/mods.json                 (or via the mod manager)
        ▼
reload the editor   (dev build: the asset watcher reloads it for you — FR-AST-14)
        │
        ├─ confirm the derived sector heightfield and blocking heights   proposals → authored
        ├─ place spawn points, wire triggers, set the encounter and camera profile
        ├─ inspect the anchors the game will read: eye level, cover, slots, capsule
        ▼
playtest from the same document     scratch sim at the encounter's state (04.6)
```

Four things about that loop that are not obvious:

- **`game/` is not where art goes during development.** `game/` is a build output
  (`§05.1`, `AD-20`); the VFS overlays a presentation pack in `data/mods/` *before*
  `game/assets/` (`AD-18`), and the build compiles the pack into `game/assets/` for a
  release. Working through the pack is not a workaround — it exercises the same path a
  modder uses, so the mod pipeline is proven by your own content every day.
- **A dropped file that is not declared is a file the engine never looks for.** The VFS
  resolves overrides from a map built at boot from each mod's index, deliberately, instead
  of probing (`AD-18`). The editor writes the declaration; the validator raises
  `undeclared_override` when it is missing (`03:03.9`). This is the single most likely
  "I exported it and nothing happened" cause.
- **Live reload is a development-build feature.** `GET /__kobra/watch` exists only in a dev
  build (`FR-AST-14`, `FR-AST-15`); a release build needs a page reload. The editor also
  needs a session, so launch with `server.entry_path: "/editor"` or open it from the running
  game (`AD-23`).
- **The editor's value here is reconciliation, not visualisation.** The rules never read the
  mesh (`AD-30`): the build *derives* heightfield and blocking-height proposals from tagged
  geometry and a designer confirms them. The overlay that draws the gameplay layers on top of
  the art is what catches "the art shows two storeys and the rules see one" — which is the
  failure this whole split exists to make visible.

## 04.10 Verification

The editor is tested like the game:

- **Round-trip**: project → publish → install → load → export produces an
  equivalent project. Every record type has a round-trip fixture.
- **The archive contract**: our tar+zstd writer's bytes are fed to the launcher's
  own extractor in a test (`§04.7`).
- **Validator parity**: the editor's "is this publishable" answer must equal the
  runtime's `wsp_validate_content` answer for every fixture, including the failure
  cases (`AD-15`).
- **Determinism**: a playtest from a fixed project revision and seed produces a
  committed state hash (`§05.4`).
- **Accessibility**: the editor's workspaces pass the same automated audit as the
  game UI, because they are the same widget library.

**As built, and what is still owed.** `tests/web/editor.test.mjs` is the exit
criterion as a gate: it asserts byte-identity against `tools/pack.mjs`, the root-level
archive layout, validator parity against `tools/validate.mjs` for both a passing and
a failing project, and the project's own round-trip. `tests/web/editor-flow.test.mjs`
drives the **page** — it boots the real entry against the real `game.wasm` and clicks
add → edit → save → validate → publish, requiring a save payload, a validator
verdict and an installed archive a stubbed launcher reads back — because a renamed
element or a handler-less button passes every document-level test and is not an
editor a player can use. `tests/web/editor-archive.mjs` builds a project in memory
and `make mods` hands that archive to the launcher's
`TestExtractModArchiveFromTheGameCLI`, so the browser's bytes are extracted by the
real `extractTarZstd`. Two rows of the list above are therefore **not yet** fully
earned and are named rather than implied: the round-trip fixture exists for `item`
only (the other record types are not authored in the editor yet), and the
accessibility audit grows with the widget library — the form engine labels every
control and associates its error paragraph, but there is no `make a11y` target for
the editor yet.
