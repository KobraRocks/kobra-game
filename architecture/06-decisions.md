# 06 — Decision log

Each entry is one architectural decision: the context that forces it, the decision,
what follows from it, and what was rejected. Numbers are stable and referenced from
the other documents as `AD-n`.

Format is deliberately terse. If a decision is wrong, the way to show it is to
attack the "Rejected" line.

---

## Supersession index

`AD-38`–`AD-42` record the pivot to a native runtime, an owned distribution, and Lua as the
glue and modding language. This log is append-only, so the older records are not rewritten:
each is marked **reversed** (its decision no longer holds) or **in part** (its conclusion
survives, its mechanism does not). Read an affected record together with the entry below,
and with the record that supersedes it.

| Superseded | By | What changes |
|---|---|---|
| AD-1 | AD-38 (in part) | The deterministic core is the same core, shipped as a native crate rather than a wasm module; "and nothing else" stands |
| AD-2 | AD-38 (in part) | The host links the `rlib`; the hand-written C ABI survives as the plugin boundary of AD-40 |
| AD-3 | AD-38 (reversed) | The renderer is native and owned, not TypeScript in a worker |
| AD-4 | AD-38 (reversed) | No two-worker topology and no shared frame ring; the packet is a frame description, not a transport |
| AD-5 | AD-38 (reversed) | No `SharedArrayBuffer` and no `require-corp`; threads are the host's business now |
| AD-8 | AD-39, AD-40 (reversed) | Lua replaces ES modules, and the tier list grows a native plugin tier |
| AD-12 | AD-42 (reversed) | There is no second document at `/editor`; authoring is headless-first |
| AD-13 | AD-42 (in part) | The `tar.zst` mod archive survives; "built in the browser" becomes `kobra-pack` |
| AD-17 | AD-38 (reversed) | There is no browser floor because there is no browser; U8 is withdrawn |
| AD-19 | AD-41 (in part) | "The page never talks to the network" becomes "the binary never talks to the network"; custody moves in-process (AD-42) |
| AD-23 | AD-42 (reversed) | The `/editor` route and `server.entry_path` retire with the launcher |
| AD-24 | AD-38 (in part) | The 15%-of-native comparison is retired because native *is* the product; budgets, counters and "a miss is a defect" survive |
| AD-35 | AD-42 (reversed) | The development surface is a build flag, not a launcher diagnostics field |
| AD-36 | AD-42 (in part) | Versioned projections, signals and save-neutral UI survive; the DOM renderer and its capability object are re-derived |
| AD-37 | AD-39 (in part) | "No expression language" stands; the renderer tier becomes Lua |

---

## AD-1 — WASM is the deterministic simulation core and nothing else

> **Superseded in part by AD-38.** The core is still the deterministic simulation and
> nothing else — that rule is what makes the pivot cheap. What changed is the artifact: it
> ships as a native crate linked into the host, not as a wasm module, because the shipped
> runtime is native.

**Context.** The launcher's architecture already names the engine as "WASM core
(Rust/C++), in a Worker" (FS §3.1). That leaves open *how much* goes in.

**Decision.** `game.wasm` contains everything whose behaviour a save, a replay, or a
mod must be able to depend on: 4C rules resolution, trait/character/vehicle state,
the combat turn machine, the sector world and its AI, the seeded RNG, save-state
serialisation, and content validation. It contains **no** DOM access, no `fetch`,
no `navigator.gpu`, no `Date.now`, no `Math.random`, and no floats on any path that
can alter state.

**Consequences.** One artifact is the single source of truth for game behaviour. The
editor can run the game's own rules and validator (`AD-15`). A save can be reasoned
about as a pure function. WASM stays small (no browser bindings), and the ABI is
narrow enough to document in a page.

**Rejected.** *Put rules in TypeScript and WASM only for hot loops* — the rules are
not hot; they are **authoritative**. Reimplementing them in the editor or in a mod
script would fork the rules. *Put the renderer in WASM too* — see `AD-3`.

---

## AD-2 — Rust, exported over a hand-written C ABI. No `wasm-bindgen`

> **Superseded in part by AD-38 and AD-40.** The host links the core as an `rlib`, so the
> wasm ABI is no longer the simulation boundary. The discipline survives where it still
> earns its keep: the native plugin ABI of AD-40 is C, for the same reason this decision
> gives — no stable Rust ABI, and a documented contract rather than a generated one.

**Context.** Rust is the pragmatic choice for a rules core (no GC, `#[repr(C)]`
control, mature tooling, `serde` for content validation). The question is the
boundary to JS.

**Decision.** The crate compiles to `wasm32-unknown-unknown` with
`crate-type = ["cdylib"]` and exports a flat, versioned C ABI under a
`wsp_` prefix: `wsp_abi_version()`, `wsp_alloc`, `wsp_free`, `wsp_init`,
`wsp_command`, `wsp_tick`, `wsp_render_packet`, `wsp_save`, `wsp_load`,
`wsp_validate_content`. Glue is hand-written plain JS over `WebAssembly.instantiate`.
Error reporting is an integer status plus a length-prefixed UTF-8 message read from
linear memory, never a JS exception crossing the boundary.

**Consequences.** The JS side has no build-time dependency on the WASM build, so
the worker, renderer, editor and any tooling stay plain ES modules. The ABI is
ours to version: `wsp_abi_version()` is checked at boot and mismatches fail into a
clean error, not a `TypeError` deep in a frame. Modders never need a Rust toolchain.

**Rejected.** *`wasm-bindgen`* — convenient, but it couples the glue to the exact
wasm-bindgen version, inflates the binary, and makes the boundary a generated
artifact rather than a documented contract. *`wasm-pack` + npm packaging* — adds a
Node toolchain to a project whose shipping format is "a folder of files".

---

## AD-3 — WebGPU is the only renderer, and it is TypeScript in a worker

> **Superseded by AD-38.** The renderer is native and owned. The load-bearing rule — the
> simulation never sees a GPU object, and the renderer is disposable — survives; the
> mechanism does not, because there is no worker, no page and no browser.

**Context.** The user-facing question "where do I put the WebGPU code?" has two
honest answers: JS/TS calling `navigator.gpu`, or Rust `wgpu` compiled into the same
wasm as the sim.

**Decision.** **JS/TS + WebGPU, one backend, no fallback.** The renderer is a module
of its own worker. The sim never touches GPU objects; each tick it writes a
`RenderPacket` (draw items, transforms, animation state, string ids for text, camera)
into a region of **shared** linear memory, and the renderer reads it in place and
issues the draw calls. Because compute shaders and storage buffers are available on
every supported browser, no feature is fenced off for a weaker backend, and there is
exactly **one** shader source language: WGSL.

**Consequences.** Small wasm; the renderer is hot-swappable; shaders are plain
`.wgsl` assets owned by the art pipeline and overridable by mods; the browser's
WebGPU debugger shows real draw calls with real names; the sim is testable headlessly
with no GPU. One artifact and one shader set to maintain instead of two, and the
performance plan in `07` can use compute, storage buffers and `timestamp-query`
without a compatibility carve-out. Cost: the audience is Chromium-only (`AD-17`).

**Rejected.** *`wgpu` in wasm* — one language and no packet read at all, but it puts a
measured ~2.4 MB of renderer (egui's wasm demo grew from 5.9 MB to 8.3 MB switching
glow→wgpu) into the artifact the *simulation* ships in, makes shader/mod
extensibility harder, and couples engine upgrades to `wgpu`'s WebGPU backend
maturity. Kept as a documented swap path: the `RenderPacket` is the contract either
way, so a future `wgpu` backend replaces one consumer of it.
*WebGL2 fallback* — **removed by studio directive.** It would have bought
Firefox/Linux and pre-Tahoe Safari, at the cost of a second backend, a second shader
set, and a "no compute, no storage buffers" constraint on the primary path. The
studio's position is that requiring a WebGPU-capable Chromium is an acceptable price
for one fast path; see `AD-17`.

---

## AD-4 — Two workers, one shared frame ring; the main thread is UI only

**Context.** Rendering on the main thread and simulating in a worker means crossing
the thread boundary every frame. With `SharedArrayBuffer` now available (`AD-5`) that
crossing no longer has to be a copy.

**Decision.** Three threads with one job each:

- **main** — DOM UI, input decoding, the data API. Never in the frame loop.
- **sim worker** — `game.wasm` plus a pthread pool, owning the packet memory.
- **render worker** — the WebGPU device and render passes, owning an `OffscreenCanvas`
  transferred from the main thread.

The frame packet lives in a **triple-buffered `SharedArrayBuffer` ring**. The sim
publishes a settled slot and signals with `Atomics`; the renderer acquires the newest
settled slot and reads it in place. The sim never blocks on the renderer, the renderer
never reads a torn packet, and **no per-frame copy crosses a thread boundary**.

**Consequences.** The hot loop touches no boundary at all. UI jank cannot stall the
simulation and simulation stalls cannot freeze the UI. Backpressure is explicit: when
the renderer is more than two slots behind, the oldest unsent slot is dropped, so the
system degrades by skipping a *render* frame rather than by accumulating latency.

**Rejected.** *One worker hosting sim and renderer* — the first revision's design,
correct while `SharedArrayBuffer` was unavailable, and superseded: sharing the packet
is strictly better than serialising sim work behind render work in one thread.
*Main-thread rendering* — couples UI responsiveness to render cost, which a CRPG's
dense UI cannot afford. *Unbounded frame queueing* — latency grows without limit and
the game feels heavy exactly when the machine is loaded.

---

## AD-5 — Multithreaded WASM over `SharedArrayBuffer`; `require-corp` is a shipped setting

**Context.** `SharedArrayBuffer` requires a secure context **and** cross-origin
isolation, which requires `Cross-Origin-Embedder-Policy`. The launcher sends COOP
`same-origin` already, but did not send COEP, and FR-SRV-16 made it conditional with
the decision deferred to the engine team. Performance is the master goal (`AD-24`),
and a single core is the first thing that would cap it.

**Decision.** **`require-corp`, and a multithreaded core.** The launcher now
implements `server.cross_origin_embedder_policy` (empty | `require-corp` |
`credentialless`), the shipped game config sets `require-corp`, and `engine.manifest.json`
declares `threads` and `shared-array-buffer` in `required_features`. The shell verifies
`crossOriginIsolated` at boot and fails with a named cause (FR-SRV-16a, error matrix E2)
if the publisher did not configure it.

Parallelism is governed by the determinism rules in `07:07.4`: parallel systems are
maps over a fixed partition, reductions are ordered, the RNG is splittable and
pre-partitioned, and the golden corpus is replayed at 1, 2, 4 and 8 threads.

**Consequences.** The sim gets the cores, and the frame packet becomes zero-copy.
Determinism survives because it is enforced mechanically rather than assumed. The cost
is a real one: cross-origin subresources must opt in with CORP or CORS, which is why
`AD-19` (no network, everything same-origin) stops being merely principled and becomes
load-bearing.

**Rejected.** *Stay single-threaded* — would leave the master goal to a single core on
a machine with eight, for a launcher change that is a config key and a header.
*`credentialless` instead of `require-corp`* — weaker, and unnecessary here because
nothing is fetched cross-origin.

---

## AD-6 — Integer-only simulation math and one seeded RNG

**Context.** 4C is percentile-integer by construction (`d%` is two d10s), but a CRPG
adds continuous quantities: positions, ranges, durations, animation-independent
speeds. Floating point in wasm is IEEE-754 for the basic operations but does not
guarantee identical NaN payloads or contraction, and `Math.random` cannot be saved.

**Decision.** All state-visible math is integer or fixed-point (`Q16.16` in `i32`,
`Q32.32` in `i64` where range demands), with explicit rounding rules. All randomness
comes from one seeded, splittable integer PRNG (PCG64 or xoshiro256++), stepped by
the sim only. The RNG state is part of the save. `d%` is generated as
`rng.next_u32() % 100` — mapped so that `00` is **0**, per the spec's explicit rule
(`4c_system.md:41`) — never as `1..100`.

**Consequences.** Replays are exact across machines and browsers. Save/load, undo in
the editor, and "playtest this encounter from here" all become cheap. Debugging is
easier because a bug report carries a seed and a command log.

**Rejected.** *Floats with a tolerance-based comparison* — turns every determinism
bug into a physics-tuning argument. *`Math.random` in JS for cosmetic randomness* —
tempting, but it leaks into gameplay the first time someone uses it for a damage
roll; cosmetic randomness uses a *separate* non-saved PRNG stream so it can never
affect state.

---

## AD-7 — Content is data; JSON is the shipping and authoring format

**Context.** We need a content pipeline that a modder can edit with a text editor,
that diffs in git, that the packager will accept, and that a shipped editor can
round-trip. The packager classifies `.json`/`.csv` as `data` assets, the data API
stores arbitrary JSON, and the launcher's design goal #2 is *"the user retains full
visibility and control over assets … because they are ordinary files."*

**Decision.** All content ships as JSON (with CSV accepted as a source format that
the build compiles into JSON). No bespoke binary content format in v1. JSON is
validated by one shared validator that is the *same* code the editor calls and the
*same* code the runtime calls.

**Consequences.** Hot-reloadable, diffable, mod-overridable, and inspectable by the
player. Parse cost is paid at load and at content-reload, never per frame. Large
tables (the Master Table, the rank-band ladder) are compiled into wasm rather than
parsed, because they are rules, not content.

**Rejected.** *A packed binary content format* — faster to load, but it makes every
mod require our build tool and destroys the human-editable property the platform is
built around. *Embedding content in the wasm* (e.g. `include_str!`) — makes mods
impossible, which is the opposite of the goal.

---

## AD-8 — Two modding tiers: data (always) and scripts (versioned, opt-in)

> **Superseded by AD-39 and AD-40.** There are four tiers now, and the script tier is Lua
> rather than ES modules. The load-bearing rules survive verbatim: one rules implementation
> (AD-15), scripts are pure functions of integer state plus the seeded RNG, the save records
> the script set and its hashes, and a mod that throws is disabled rather than fatal. The
> native and mod-wasm doors this decision closed for platform reasons are reopened in AD-40.

**Context.** A data-only mod system cannot express a genuinely new 4C power, a new
quest trigger, or new AI — and 4C powers are the heart of the game. A
code-only system makes trivial content edits require a toolchain. The launcher's
threat model already classifies mods as *trusted user data, not a security
boundary* (FS §1.3, §14), and CSP `script-src 'self'` means a `.js` file under an
enabled mod's `assets/` subtree is loadable.

**Decision.** Two tiers, both first-class in the editor:

- **Tier 1 — Data mods.** Declarative content and declarative effects (the effect
  DSL of `§02.4`). No executable code. This must cover the overwhelming majority of
  real mods.
- **Tier 2 — Script mods.** ES modules under `assets/scripts/` that the engine
  dynamically imports and registers against a **versioned** API
  (`worldspiracy.mod/v1`). Hooks are pure functions of integer state plus the
  seeded RNG. No DOM, no `fetch`, no timers, no floats on state paths.

**Consequences.** A mod can always do more than data allows, and the cost of that is
a documented, versioned contract instead of an implicit one. A Tier-2 mod can break
determinism — so the engine records the script set and its hashes in the save, and a
script that throws is disabled with a diagnostic rather than taking down the boot.
`connect-src 'self'` means a script mod still cannot exfiltrate anything.

**Rejected.** *Data-only* — caps the game's lifetime at what the DSL can say, which
is the one thing the brief explicitly asks us not to do. *Ship a real sandbox
(QuickJS/Wasmer in wasm)* — a large amount of machinery to protect the player from
software they chose to install, when the platform's own spec declines to. Revisit
only if a mod-distribution channel ever appears.

---

## AD-9 — Mod discovery is by a conventional in-`assets/` index, not by listing

**Context.** `mod.manifest.json` lives at `data/mods/<id>/mod.manifest.json`; the
only mod route is `/mods/<id>/assets/*`, which requires the literal `assets/`
segment. The data API exposes `GET /api/data/config/mods` (id, name, version,
priority, enabled, `missing_dependency`) and **no** route that serves a mod's
manifest. So the browser can learn *which* mods are enabled but not *what files*
they contain, and the base `asset.manifest.json` only describes base assets.

**Decision.** Every content mod ships a **conventional index** at
`assets/data/mod.json`, declaring the mod's content entries, its declared hooks, and
its content-pack id. The engine probes `/mods/<id>/assets/data/mod.json` for each
enabled mod id, then loads exactly what the index names. Overrides need no index —
they work by path shadowing in the VFS.

**Consequences.** No launcher change is required, and discovery is explicit and
cheap. It also makes a mod's footprint auditable by the editor and by the mod
manager. Cost: a mod author must include one small file, which the editor writes for
them.

**Rejected.** *Probe a list of conventional filenames per mod* — brittle and
ambiguous. *Request an upstream `GET /api/data/mods/{id}/manifest`* — the right
long-term answer (recorded as an upstream request in `§05.6`), but it must not be a
dependency for v1.

---

## AD-10 — `serve_mods` stays `true`; the base game never assumes its own paths

**Context.** `serve_mods: true` is the default and is what makes `/mods/*` readable.
With it false there is no route at all for mod content, because `/api/data/mods/*`
does not exist in the launcher.

**Decision.** Ship `serve_mods: true`, treat it as load-bearing, and document it as
such in `launcher/launcher.config.json` comments and in the editor. Every asset is
requested through the VFS, which tries mod overlay then base — never a hardcoded
`/assets/...` URL.

**Consequences.** Mods work. The residual risk is covered by the CSP: a mod cannot
reach the network, cannot write to `data/` (no endpoint), and cannot shadow
`engine/`. A player who wants no mods disables them in `data/config/mods.json`
(FR-AST-13), which is a file they can edit without a working game.

**Rejected.** *`serve_mods: false` and gate mods behind the session* — no such route
exists today; would be an upstream change with no benefit here.

---

## AD-11 — A save is a JSON envelope with a readable core and a compact delta

**Context.** Saves are arbitrary JSON values written atomically with revisions, a
64 MiB request ceiling, and 8 retained revisions. A full world snapshot as pretty
JSON is both slow and large; a pure replay-from-seed is small but brittle across
engine versions. Meanwhile the platform's design goal is *user control over
transparency* — a save the player can read.

**Decision.** The payload is a JSON object with three parts:

1. `meta` — `save_version`, `engine_version`, `content_hash`, the enabled mod ids
   and their content hashes, seed, playtime, and a human-readable timestamp.
2. `core` — the **readable, migration-critical** state: character sheets, fortune,
   repute, lifestyle, inventory, journal, quest flags, faction standing, known
   map. Small, stable, and the part we promise to migrate.
3. `world` — a **base64 compact-binary** delta of the simulation world (per-sector
   deltas from the procedural baseline, not the whole world).

**Consequences.** The player and the support process can read and fix the important
half. Migration logic touches only `core`. The world delta keeps normal saves in the
tens-to-hundreds of KiB. The `content_hash` binding means loading a save without its
mods is *detected* and reported, never silently mis-simulated.

**Rejected.** *Full JSON world dump* — inflates by 4–10× and makes the 8-revision
retention a disk problem. *Pure command-log replay* — smallest, and used as the test
oracle (`§05.4`), but a 200-hour campaign would replay for minutes on load, and any
engine change invalidates every save. *Store the world in `settings.json`* — the
settings key whitelist forbids it, and settings are not versioned per slot.

---

## AD-12 — The editor is a second document at `/editor`, not a second app

**Context.** The launcher opens one page and hands it a **single-use** bootstrap token
(`POST /__kobra/session`, single use), so a second document cannot mint its own
session — and packaging keeps the `game/` root to `index.html` + `shell.js`. The first
revision therefore concluded that the editor had to be a *mode* of the game document.

**Decision.** **The editor is its own document at `/editor`, served by the launcher
from `game/editor/`, and it needs no new session machinery.** It is same-origin, so it
inherits the `kobra_session` cookie of whatever established a session, reads the
non-HttpOnly `kobra_csrf` cookie, and satisfies the double-submit check on its own.
The launcher change that makes this possible is `AD-23` (the route plus
`server.entry_path`).

The game shell may still offer an "Editor" link, but it opens `/editor` in a new
document rather than importing the editor into the play page.

**Consequences.** Three wins over the mode design. The **play bundle no longer contains
the editor** — the module graphs are separate, so a player who never opens the editor
never downloads it and the play-mode critical path is shorter, which matters because
performance is the master goal. The editor can be **the entry document** of a
modder's build (`entry_path: "/editor"`), so it is reachable without booting the game
first. And shared code (the wasm core, the data-API client, the widget library) stays
shared, because both documents are in the same origin and the same package. The one
thing that must not regress: neither document may navigate the other, and neither may
depend on a token the other holds.

**Rejected.** *Editor as a mode of `index.html` only* — the first revision's design,
correct before the route existed; it forces the editor into the game's bundle and makes
the editor unreachable to a player who has not booted the game. *A second bootstrap
token via a new `POST /__kobra/open` endpoint* — real added surface for something the
existing cookies already provide (`AD-23`). *A separate origin or a native editor app*
— throws away "playtest the real game in one click" and doubles the platform surface.

---

## AD-13 — Projects are save slots; mods are published as tar.zst built in-browser

**Context.** The data API can write save slots (arbitrary JSON), merge whitelisted
settings, and install a mod from base64 `archive_bytes` that must be a `.tar.zst`.
There is no endpoint that writes loose project files to disk. Export exists as a
download of a save slot.

**Decision.** An editor project is a **save slot** (`edit.<project>`, arbitrary JSON
payload), saved with the same revision/conflict discipline as a game save, and
exportable with `GET /api/export/{slot}`. **Publish** assembles the project into the
mod layout (index, content JSONs, assets), writes a tar stream in JS, compresses it
with a small zstd **wasm** encoder shipped under `game/editor/lib/`, and POSTs
`/api/mod {action:"install", archive_bytes:<base64>}`. This lands the pack directly
in `data/mods/<id>/` where the game will load it on the next boot.

**Consequences.** The whole author→playtest→publish loop works without leaving the
browser and without any launcher change. Two limits are designed around rather than
discovered later: `max_request_bytes` 64 MiB caps a published pack at roughly 45 MiB
before base64 inflation, and the editor must warn before that. The alternative —
export a folder and have the author place it by hand — ships as a fallback path so
publishing never *depends* on the zstd encoder.

**Rejected.** *Publish by writing into a save slot and telling the user to unpack
it* — a terrible loop, and it breaks the "modders expand the game" goal. *Ask for a
new launcher endpoint that writes loose mod files* — the better long-term answer
(upstream request, `§05.6`); v1 must not require it. *Publish as `.zip`* — the
launcher only unpacks tar.zst (FS §27.5 is explicit that the mod archive layout is
deferred).

**As built (M4 walking skeleton) — the tar and the frame are shared code, and the
encoder is ours.** `04:04.7` requires the editor's publish path to produce *the same
archive* as `tools/pack.mjs`, and the stronger property the slice set was
**byte-identical by construction**, not merely the same format. That requirement is
what moved the decision, because it is a constraint the original wording did not
carry:

- **One writer.** The `ustar` headers, the entry order, the name rules and the zstd
  frame live in `src/web/shared/tar.ts` and `src/web/shared/zstd.ts`, imported by
  `tools/pack.mjs` and by the editor. Only the file list differs, because only Node
  can enumerate a directory. The byte-identity test (`tests/web/editor.test.mjs`)
  packs the same tree both ways and requires equal bytes, so a second writer is a
  failing gate rather than a review question.
- **No third-party wasm.** Node's native `zstdCompressSync` and any wasm encoder
  produce different bytes for the same input, and the native encoder's bytes move
  with the Node version — so routing the CLI through a `zstd.wasm` to make the two
  agree would have meant adding a binary to pin, license and reproduce, and making
  the *packager* depend on the browser encoder, in exchange for compression on packs
  that are kilobytes today. The frame writer is instead ~100 lines of shared code
  (comments included) and
  emits valid zstd frames made of `Raw_Block`s, which the launcher's real reader
  accepts (`TestExtractModArchiveFromTheGameCLI` is run against the editor's own
  archive by `make mods`).
- **What that costs, plainly.** The archive is not compressed: a published pack is
  as large as its tar, so the 32 MiB warning and ~45 MiB refusal of `04:04.7` bind
  sooner than they would with a real encoder. That is a size-guard change, not a
  blocked publish, and it is the trade this milestone chose deliberately.
- **Revisit with U3.** Replacing `zstdFrame` with a real shared encoder is a
  contained change — one function, both callers move together — and belongs with
  upstream request U3 (`03:03.12`, `05:05.6`), which stays open until content scale
  gives a number for what compression is worth.

---

## AD-14 — The Master Table is a generated, tested artifact, not hand-typed data

**Context.** `specs/4c_system_master_tables.csv` is the only machine-readable copy
of the table we have, and it does not survive inspection: the Basic colour block's
header labels are in a different order from its data columns, and the Advanced block
has a row that appears shifted. Its provenance is unknown (see R1).

**Decision.** Treat the table as **generated**. A build step reads a single
authoritative source, emits Rust tables, and a test suite asserts the table's
*properties* — monotonic non-decreasing success probability as Rank Value rises,
complete and non-overlapping `d%` bucket coverage, valid row-step clamping at both
ends, and a golden-file snapshot reviewed by a human. The CSV is kept as a
cross-check input, and every disagreement between the two is a hard build failure
until a human resolves it.

**Consequences.** The table cannot silently rot, and the row-step arithmetic that
every roll in the game depends on is covered by property tests rather than by hope.
Cost: the authoritative source must be obtained before the rules core is written.
This is the project's first blocking task (R1).

**Rejected.** *Hand-transcribe the CSV into Rust and move on* — the anomalies in the
CSV would become permanent, invisible rules. *Load the table at runtime from
content* — makes the foundational rules moddable and therefore non-deterministic
across players, and a balance mod must not be able to rewrite the resolution table
out from under a save.

---

## AD-15 — One rules implementation, two consumers, one validator

**Context.** The editor must validate authored content and playtest it. The runtime
must validate loaded content. If those are two implementations, they disagree.

**Decision.** The wasm core exposes `wsp_validate_content(bytes) -> report` and the
editor calls it through the same ABI the game uses. Authored content is validated
before it can be saved or published; runtime content is validated on load and a
failing unit is disabled and reported, never partially applied.

**Consequences.** The editor cannot author something the game rejects, by
construction. Validation messages are data (path + code + args), so the editor
localises them and the game can log them. Cost: the validator must be fast enough to
run interactively on a whole project, which is a design constraint on its
algorithms, not an afterthought.

**Rejected.** *A JSON Schema layer in JS for the editor* — schema validity is not
semantic validity (a power referencing a missing damage type is schema-valid), and
it re-forks the rules. *Validate only at load* — the author finds out at playtest,
which is exactly the loop the editor exists to shorten.

---

## AD-16 — Powers are kernels plus a declarative effect DSL

**Context.** 4C defines ~25 basic and ~53 advanced powers, each with bespoke rules
(`4c_system.md:417-845`). Hardcoding them in Rust blocks new powers; a DSL alone
cannot express all of them.

**Decision.** A power is a **kernel** — a Rust type implementing a small trait
(`on_acquire`, `modify_trait`, `modify_attack`, `modify_damage`, `on_hit`,
`on_own_turn`, `on_resist`, `movement`, `action_economy`, `modify_armor`, …) —
registered in a table keyed by power id. Canonical 4C powers are kernels. New powers
defined by content are `Template` kernels driven by the declarative effect DSL
(`§02.4`), which composes the same hooks from data. Tier-2 scripts (`AD-8`) can
register the same hooks for what neither covers.

**Consequences.** The three tiers degrade gracefully: a data mod can build a new
power from effects; a script mod can express anything; the canonical powers stay
fast and exactly as written. Cost: the hook set is a public API from day one and
must be versioned and documented like one.

**Rejected.** *All powers as data* — cannot express Force Field's short-out rule or
Mind Control's break-free check without inventing a general programming language in
JSON. *All powers as Rust* — no new powers from mods, contradicting the brief.

---

## AD-17 — WebGPU only: the floor is a WebGPU-capable Chromium

> **Superseded by AD-38.** There is no browser floor because there is no browser: the WebGPU
> requirement, the `min_browser` field and upstream request U8 retire with the web runtime.
> The reasoning about *why* one rendering backend beats two is still sound, and still why
> the native runtime has exactly one.

**Context.** The product-wide floor is Chrome/Edge 105 (FS §15.1). WebGPU is a
Chromium 113 feature: it is absent from Firefox on Linux and macOS Intel entirely,
requires Safari 26 on macOS Tahoe, and only reached Chrome desktop Linux in 144. The
first revision therefore carried a WebGL2 fallback.

**Decision.** **No fallback.** The engine requires WebGPU, the manifest raises the
floor to a WebGPU-capable Chromium, and the browser preference list is narrowed to the
Chromium family. Firefox and Safari are out of scope until their WebGPU support is
both universal and stable.

```json
"required_features": ["wasm", "simd", "bulk-memory", "threads", "shared-array-buffer", "webgpu"],
"min_browser": { "chrome": 113, "edge": 113, "opera": 99 },
"notes": "WebGPU and cross-origin isolation are required; see architecture/07-performance.md"
```

**Consequences.** One renderer, one shader language, compute and storage buffers
available, and no compatibility carve-outs in the performance plan. The launcher
already refuses to launch a browser below the configured minimum (FR-LNCH-6) and E1
explains the requirement in plain language, so an unsupported browser fails before the
player sees a blank canvas. The cost is explicit and accepted: a Firefox-only or
pre-Tahoe-Safari player cannot run the game. The launcher keeps a browser-preference
list rather than a hard-coded browser, so widening support later is a config change
plus a backend, not a redesign.

**Rejected.** *WebGL2 fallback* — removed by studio directive; the audience is
Chromium-capable, and a second backend would tax every future rendering feature to
serve browsers the studio is not targeting. *Requiring Chrome specifically rather than
"a WebGPU-capable Chromium"* — needlessly excludes Edge, Brave, Opera and Chromium
itself, which are the same engine.

---

## AD-18 — The engine owns an asset VFS with an overlay **map**, plus a bundle seam

**Context.** `/assets/*` always serves the base game; mods are only reachable at
`/mods/<id>/assets/*`; the manifest names every base asset and marks them all
`mutable`; the schema reserves `bundles` that the packager does not yet emit. The
obvious implementation — try each enabled mod's path, then the base — costs one
request per mod per asset, and FS §17.1 budgets **≤ 150 root requests at boot** (400
as a failure threshold) with FR-AST-10 bundling explicitly unimplemented.

**Decision.** All content access goes through one JS module, `vfs`, which builds an
**overlay map** before the first asset request:

1. fetch `assets/asset.manifest.json` (the base path set);
2. for each enabled mod (descending `priority`, ties by id ascending), fetch its
   `/mods/<id>/assets/data/mod.json` index;
3. merge its declared `overrides` and its packs' asset references into
   `map: path → winning mod id`;
4. resolve any path with **exactly one** request: the winning mod's URL if mapped,
   otherwise `/assets/<path>`.

Zero probing, no expected 404s, and a request count of `1 + N_mods + M_assets`.
`vfs.manifest` wraps the asset manifest, whose hashes are used for change detection
and reporting, never for refusal (FR-AST-4). The VFS API is bundle-oblivious: if the
packager starts emitting bundles, only `vfs` gains a bundle reader.

**Consequences.** Overlay resolution is one code path, testable, identical in game
and editor, and it respects the platform's request budget. The cost is that a mod
**must** declare the paths it provides in its index — which the editor writes
automatically, and which the validator cross-checks against the mod's `assets/` tree
so a forgotten declaration is caught at publish time rather than silently failing to
override. `AD-9`'s conventional index therefore becomes load-bearing for
performance, not just for discovery.

**Rejected.** *Probe every mod for every asset* — simple, but `N_mods × M_assets`
requests; with three mods and 150 assets that is 450 requests against a budget of
150, and it degrades as players install more mods, which is precisely backwards.
*Fetch the whole mod manifest to plan ahead* — unreachable (`AD-9`). *Ask the
launcher to do overlay resolution* — `/assets/*` is a base route; changing that is an
upstream change, and FR-AST-12 assigns resolution to the consumer.

---

## AD-19 — The page never talks to the network; the updater is the launcher's

**Context.** CSP `connect-src 'self'` makes any outbound request from the page
impossible, and the launcher's `internal/update` package is *"the only package
allowed to make an outbound request"* (`docs/launcher-architecture.md` §6).

**Decision.** Design as if offline. No analytics, no patch fetches from the page, no
font/CDN, nothing. "Check for updates" in the UI calls the launcher's
`/api/update/check` and reflects the answer; applying an update records intent and
happens on the next launch. All external content (fonts, icons, audio) is vendored
into `assets/`.

**Consequences.** No CSP loosening is ever needed; the game works from a USB stick
on an air-gapped machine; the mod threat model stays bounded. Cost: any future
online feature is a launcher-level product change, not a game feature — which is the
correct place for that decision to be made.

**Rejected.** *Widen CSP for a CDN or a patch service* — contradicts the platform's
whole reason for existing, and FR-SRV-17 is not ours to relax.

---

## AD-20 — Source layout separates the engine, the web app, and the package

**Context.** The launcher serves `game/`; the packager reads `pkg.toml` and copies
`game/`; build outputs (wasm, bundled JS) must land under `game/engine/`. A game in
`games/<name>/` sits one level deeper than `testgame/`, so toolchain paths shift
(AGENTS.md).

**Decision.** Sources live outside the served tree and are built into it:
`crates/kobra-core/` (Rust), `src/web/` (TS: `shell/`, `worker/`, `renderer/`, `editor/`,
`shared/`), `content/` (authored JSON/CSV sources, including `rules/`), `game/` (the
shipped package, generated into, checked in for the parts that are authored),
`tools/` (build/validate/pack helpers), `tests/`, `architecture/`, `specs/`.

**Consequences.** Nothing generated is edited by hand; `make` reproduces `game/`
from source; a clean checkout + build yields a packageable folder. The rule
"`game/` root is exactly `index.html` + `shell.js`" is enforced by a build-time
check before the packager ever sees it.

**Rejected.** *Author directly in `game/`* — blurs generated and authored files and
makes the wasm/JS build a hand-copy step. *A single `src/` tree with no separation* —
loses the invariant that the served tree is a build output.

---

## AD-21 — Replay is a test oracle; the save is a snapshot

**Context.** Determinism (`AD-6`) makes replay possible. It is tempting to make it
the save format.

**Decision.** Saves are snapshots (`AD-11`). Replay from `seed + ordered command
log` is a **verification** mechanism: golden replays, save round-trip tests,
regression tests for every rule fix, and the editor's deterministic playtest.

**Consequences.** Loading never depends on replaying history, so load time is
independent of campaign length, and an engine upgrade does not invalidate saves.
Determinism is still enforced, because the replay harness compares state hashes on
every CI run.

**Rejected.** *Command-log saves* — see AD-11's rejected line.

---

## AD-22 — Game strings live under `assets/` so mods can translate

**Context.** The launcher serves `/locales/*` from `game/locales/`, but the mod
overlay only maps `assets/`, so a translation shipped as a mod **cannot** shadow
`game/locales/**`. `launcher.config.json`'s `locales` list is launcher-level.

**Decision.** The shell's own minimal strings stay in `game/locales/`, which mods
cannot touch. All *game content* strings live in `game/assets/locales/<lang>.json`
(or per-pack string tables under `assets/`), so a translation mod is an ordinary
asset override. String ids are stable, namespaced, and referenced by content — never
by their English text.

**Consequences.** Translation mods work with the standard overlay, need no code, and
can be partial (fallback chain: mod overlay → base → English). Cost: two locale
systems exist, and the split must be documented in the content style guide so
authors put strings in the right one.

**Rejected.** *Put everything in `/locales/`* — makes community translation
impossible, which is one of the cheapest long-term life extensions available.
*Load English text as the key* — untranslatable and unreviewable.

---

## AD-23 — `/editor` is a launcher route, and the entry document is configurable

**Context.** Two launcher behaviours blocked the editor from being a first-class
document rather than a mode of the game page: the static handler served only
`/`, `/index.html`, `/shell.js`, `/engine/*`, `/assets/*` and `/locales/*`, so an
editor at `/editor` was unreachable; and the launcher always opened `/index.html`, so
the editor could only ever be entered *through* the game. The packaging allowlist had
the same gap (`servablePrefixes` omitted `editor/`).

**Decision.** Both fixed in the launcher, minimally:

- The static handler serves **`/editor` and `/editor/*`** from `game/editor/`, with
  `/editor` resolving to `editor/index.html`, the same confinement and symlink
  re-checking as the other roots, and a clean **404 when the game ships no editor**
  (FR-AST-16). `editor/` is added to the packaging allowlist so a shipped editor is
  indexed rather than packaged-and-404'd.
- **`server.entry_path`** (`/`, `/index.html`, `/editor`) names the document the
  launcher opens and `--print-url` prints (FR-LNCH-9). An unknown value is rejected at
  config load, so a typo cannot silently launch the wrong thing.

**Consequences.** The editor becomes a real second entry point in the same origin: it
inherits the session cookie of whatever established a session, reads the
non-HttpOnly `kobra_csrf` cookie, and needs no new token machinery and no new API
surface. A publisher can ship a modder's build whose launcher opens straight into the
editor. The route costs nothing to games that ship no editor.

**Rejected.** *Keep the editor as a mode of `index.html` only* — the first revision's
design, forced by the route list; it works, but it makes the editor unreachable to a
player who has not booted the game, and it forces the whole game module graph to be
the editor's entry. *A second bootstrap token minted through a new `POST /__kobra/open`
endpoint* — real added surface (a cross-package call into the browser launcher) for
something the existing cookies already provide. Recorded as a possible later addition,
not built.

---

## AD-24 — Performance is the master goal, with a published bar and a native comparison

**Context.** The studio thesis is that a local-server + local-browser game is a
plausible alternative to other ways of shipping games, *including on performance*, and
that the web stack additionally makes modding far easier. An unmeasured performance
goal is an opinion.

**Decision.** Performance gets a document (`07`), a budget table (frame time, sim and
render phase costs, memory, save latency), a committed benchmark scene, a frame-stat
counter set, CI regression gates, and — the claim that actually proves the thesis —
**a native build of the same benchmark scene, measured on the same machine, with the
browser build required to land within 15 % of native frame time.** Boot and asset-load
time are part of the table but **explicitly deferred** (`AD-25`); the claim is about
sustained rendering and simulation cost, not about loading.

Three rules make it enforceable rather than aspirational:

1. Every budget has a target and a fail threshold, and a miss is a defect.
2. No optimization claim is accepted without naming the counter it improves.
3. Machine-dependent budgets run on the reference environment (FS §17.4) and are
   re-measured each release, with the native ratio recorded so drift is visible.

**Consequences.** The engine's structure follows the bar rather than the reverse:
threads, shared memory, SoA layouts, one GPU backend, prebuilt pipelines, a content pak
to respect the request budget, and a determinism harness that survives all of it. It
also means the expensive architectural questions — threads, the second worker, the pak
format — are settled early and for performance reasons, instead of being discovered as
an optimization project after the content exists.

**Rejected.** *"We'll optimize later"* — for a game whose entire pitch is that the web
is fast enough, performance is a product feature, not a phase. *A synthetic
micro-benchmark as the gate* — it would measure the engine instead of the game; the
benchmark scene is real content with real mods loaded.

---

## AD-25 — Boot and load time are deferred; frame time and memory are not

**Context.** `AD-24` made performance the master goal and gave it a bar. That bar
included boot-to-interactive, WASM compile time, root request count and asset-load
throughput — all of which are really *content-volume and packaging* problems, and all
of which would have shaped v1's structure if taken as first-class constraints. In
particular, the FS §17.1 request budget (≤ 150 root requests) was pushing the content
model toward a custom binary pak before a single line of content existed.

**Decision.** **Boot and load time are the lowest priority and are not gated.** The
runtime bar — frame rate, frame-time percentiles, sim and render phase cost, memory,
save latency — is gated and treated as contract. The boot/load keystones remain
documented targets, are measured by counters that ship from day one, and are revisited
if and when boot time becomes a problem.

**Consequences.** v1 ships **loose, human-readable content files** with no bundling and
no in-house container format, which is also the arrangement that makes modding trivial
(`AD-7`). The pak design is specified and ready in `07:07.7` but not built; it touches
one module (`vfs`) when it is needed, because the VFS API is deliberately
bundle-oblivious. The risk this accepts is explicit (P8): the first content-heavy build
may be slower to start than we would like, and we will find out from a counter rather
than from a player. Two things are deliberately *not* deferred even so, because they
cost nothing and are structural rather than optimisations: `instantiateStreaming` for
the wasm, and image decode inside the worker rather than on the main thread.

**Rejected.** *Keep boot in the bar and design for it now* — it would put a
packaging-format decision ahead of the content it packages, for a metric the studio has
said it does not care about yet. *Drop the boot numbers entirely* — deferring is not
forgetting: the targets stay in `07:07.1` and the counters stay live, so promoting them
back to gated is a harness-config change.

---

## AD-26 — The 2D/3D presentation decision is **deferred**, but its seam is fixed now

> **Superseded in part by AD-27 and AD-28.** The deferral this record describes ended: the
> presentation is HD-2D (`AD-27`) and height became rule-bearing (`AD-28`). Point 2 below —
> "the simulation stores height and no rule reads it" — was the temporary state that made
> the later decision cheap; `AD-28` is the rule that now reads it, which is exactly the
> outcome this record was designed to allow. The rest of the seam (descriptors, billboards,
> camera as a preset) stands unchanged.

**Context.** Presentation could be 2D sprites, 3D meshes with a locked isometric camera,
or a full 3D game. The studio has not decided, and the decision is described as one
there is "no coming back" from. What is actually irreversible is not the renderer: it is
(a) the art produced, (b) the **content contract** that references that art, and (c) the
mods shipped against both. The camera itself is nearly free to change. Meanwhile 2D
isometric is the most frame-hungry 2D option (an 8-facing sprite sheet is 300–500 frames
per character), so 2D is not automatically the cheap answer for a large cast or for
visible equipment swaps — the classic paper-doll problem that made Baldur's Gate and
Diablo II pre-render 3D models into sprites. And AI leverage is lopsided: text, code and
2D art are well served today, while rigged, style-consistent 3D characters are not —
a 2026 survey of the field still measures *"a persistent gap … from the production-ready
standard expected by interactive applications"*, naming topology, UVs, PBR materials,
skeletal rigging and scene assembly as the unmet engine-level constraints
(arXiv:2604.23629, *From Visual Synthesis to Interactive Worlds*).

**Decision.** Do **not** choose the art representation now. Do choose the seam, because
the seam is what makes the later choice cheap, and it is nearly free today:

1. **Visuals are descriptors, never asset types.** A content record declares
   `visual: {kind, asset, frames, facings, height, anchors}`; no content schema, editor
   form or save field names a "sprite sheet" or a "mesh". The renderer resolves the
   descriptor. Reskinning the whole game is then a data change.
2. **The simulation stores height and no rule reads it.** Every world entity carries
   `z` and a height extent from day one, used only by the presentation projection.
   Making the third dimension mechanical later becomes a rules *addition* (effect kinds,
   a line-of-sight system) rather than a data migration across every positioned record
   and every existing save.
3. **Billboards are permanent.** The `RenderPacket` keeps `quad` (billboard) and `mesh`
   as equally first-class draw kinds at any `z`, and the camera carries `yaw`/`pitch`/
   `zoom` as data. A 3D build therefore renders 2D assets as camera-facing quads, so
   **every 2D art mod keeps working in a 3D build** — degraded in look, not broken.
   Re-shipping a mod for 3D becomes an optional upgrade rather than a migration.
4. **The camera is a preset, not a hardcode.** Isometric is a value of
   `(yaw, pitch, projection)`, so "go 3D isometric" or "let the camera orbit" is a
   content/config change plus renderer work, not a rewrite of the projection code.
5. **The editor previews whatever the descriptor says.** Its asset inspector is built on
   a visual-provider abstraction, so adding a 3D preview is an incremental editor
   feature rather than a new editor.

**Consequences.** v1 ships 2.5D — a fixed isometric camera over 2D art — which is the
fast path for a solo developer and the one with the widest modder pool (a sprite is a
paint program; a rig is Blender plus an export pipeline) and the best AI leverage. The
door to 3D stays open at a cost of one coordinate, one descriptor indirection and a
packet field. The decision rule, recorded so it is not re-litigated from vibes: **if the
cast is small and equipment is visually abstracted, ship 2D; if the cast is large, or
equipment visibly changes, or 8 facings are wanted, go straight to 3D meshes behind the
locked isometric camera**, because 2D iso's frame count does not scale there. The
recommended long-term move is to ship a future 3D look **as a mod pack** that re-skins
the visual descriptors — which proves the modding thesis with the studio's own content.

**Rejected.** *Decide the art now* — it would put the most expensive, least
AI-assistable work on the critical path before the content exists, and the evidence says
the 3D tier is the one AI cannot yet carry. *2D only, with no seam* — cheap now, and it
converts a future 3D build into exactly the rewrite the studio fears, including forcing
every art mod to be re-shipped. *3D now, because the engine delta is small* — true about
the engine and false about the assets; it trades a solo developer's scarcest resource
(rigging and animation labour) for a rendering feature the rules do not yet use.

---

## AD-27 — Presentation decided: **HD-2D** — a 3D world with 2D sprite characters

> **Superseded by AD-30.** The studio chose full 3D instead. The reasoning recorded here
> about the *technique* (camera-facing billboards avoid a frame-count explosion) is still
> true and still why billboards remain a first-class draw kind, and the DLC mechanism
> described here survives as the placeholder-to-final art swap of AD-29 and AD-31. Read
> AD-30 for the presentation that was actually chosen.

**Context.** `AD-26` deferred the art representation and fixed the seam so the choice
stayed cheap. The studio has now chosen, with *Pillars of Eternity II*, *Wasteland 3* and
*Encased* as feel references and Octopath Traveler's technique as the model: **3D world
geometry rendered through a fixed isometric camera, with 2D sprite characters as
camera-facing billboards.** Two properties of that choice matter more than the look.

1. **The camera-facing billboard removes the frame-count explosion.** The 8-direction
   isometric sprite sheet that made 2D the expensive option for a large cast (300–500
   frames per character, and combinatorial with visible equipment) is not needed: a
   billboard that always faces the camera needs roughly **one facing** plus a flip. Cast
   size stops being the deciding cost.
2. **The world is already 3D**, so it is where the mechanical third dimension
   (`AD-28`) and dynamic lighting live, and a future 3D character pack (`AD-29`) does not
   rebuild a single environment.

**Decision.** HD-2D, with three rules that protect it:

- **The camera does not rotate.** Fixed isometric yaw and pitch, zoom allowed. Rotation
  would multiply sprite production by the number of facings, which is the one thing this
  presentation exists to avoid. This is a design rule, not a limitation: it is what the
  reference games do, and modders asking for an orbit camera should be answered with the
  art budget, not with the feature.
- **Sprites write depth with alpha-test**, so 3D geometry occludes them correctly (a
  character behind a pillar is behind the pillar). Sprites stay billboards; they are not
  flattened into the scene.
- **Sprite lighting in v1 is ambient plus a directional tint plus a blob shadow.** No
  normal-mapped sprites and no per-sprite dynamic shadow: both are real art cost per
  character for a subtle gain, and the same effort is better spent on the world, which
  the 3D pack will reuse.

**The authoring fork, stated honestly so the DLC plan is not built on a hope.** Sprites
may be **baked from 3D models** or **drawn/AI-generated**. Baking is what makes a later 3D
character pack a *swap* rather than a *production*, because the models, rigs and clips
already exist. Drawing is cheaper now and leaves the 3D pack as a character-only
production — still far smaller than a 3D game, because the world is done, but not a swap.
Either way, **animation is the part that does not carry over from drawn sprites**, and it
is the part AI cannot yet produce. The recommendation is to decide this by answering one
question: *is the 3D character pack a product we intend to sell?* If yes, bake from 3D
models from the start and eat the modelling cost early. If no, draw, and treat any future
3D pack as new production.

**Consequences.** v1 is visually distinctive, cheap per character, and keeps the widest
modder pool: a character is a sprite sheet, which is a paint program, while the world is
authored 3D that modders extend with ordinary models. Equipment visibility in the 2D
presentation stays limited by choice (the studio has lowered that expectation for the
first game) and is expressed as a small number of body/outfit variants rather than
per-item layering — but the *slots* exist in the data from day one (`AD-29`) so the 3D
pack can use them.

**Rejected.** *Full 3D characters now* — it puts rigging and animation labour on the
critical path before the content exists, for a look the world already delivers.
*8-direction isometric sprites* — the frame count is the trap, and the billboard is free.
*Rotatable camera* — an 8× art multiplier for a feature none of the reference games ship.

---

## AD-28 — Height is mechanical: elevation as Row Steps, plus cover and falling

> **Distances re-based by AD-33.** This record predates the 1 m tile. Where it says
> "adjacent sector" or "per open sector entered", read a legacy-sector distance — 3 tiles —
> resolved through `rules.knockback_tiles` and `rules.per_distance.rush_bonus` (`02:02.6`).

**Context.** Gameplay will lever on high and low ground, so the third dimension stops
being presentation. `AD-26` already stores `z_q16` and a height extent on every entity
with no rule reading them, so the *data* exists. What is missing is the rules. The 4C
System has no height rule at all: movement, ranges and area effects are expressed in
sectors (`4c:897-914`, `4c:981-991`), and the only sized vertical concept is the climbing
table (`4c:907-912`).

**Decision.** Build elevation on the mechanism 4C already uses for situations — the
**Row Step**. The spec's own circumstance modifiers are row steps (*hiding in shadows −5
RS*, *jostling train −2 RS*, *raining −2 RS*, `4c:1203-1209`), so elevation needs no new
resolution machinery:

| Rule | Model |
|---|---|
| **Elevation modifier** | Per level above the target: `+1` row step; below: `−1`. Clamped to ±2. Content table `rules.elevation_steps`. |
| **Where it applies** | Ranged, power and perception rolls by default; melee only on a charge or rush. Content list `rules.elevation_applies_to`, so a campaign can make melee care. |
| **Line of sight** | Integer raycast over a sector heightfield. Blocked when intervening terrain rises above the lower participant's eye level; partial obstruction applies the cover penalty instead. |
| **Cover** | `rules.cover_steps` (−2 RS default), stacking with elevation only up to a content cap, so a target cannot become unhittable. |
| **Falling** | `rules.fall_damage_per_level` (content, default `Brawn`-independent flat value), applied when a move, a Pound, or a push takes an entity down a level. |
| **Movement cost** | Climbing/descending one level costs extra movement (content, `rules.climb_cost_per_level`), reusing the existing climbing table rather than replacing it (`4c:907-912`). |
| **Area effects** | Each effect declares `vertical_reach` in levels (default 1), so a frag grenade hits its floor and the one below, and a fire effect does not. |
| **Sight range** | Height extends perception range by a content amount per level, folded into the existing Detection/Supersense model (`4c:476-486`). |

Two emergent interactions are worth naming because they reward the choice rather than
taxing it: **Pound Black already knocks the defender into an adjacent sector**
(`4c:1175`), so knocking someone off a ledge becomes a real tactic with no new rule, and
**rushing gains `+1` row step per open sector entered** (`4c:1004`), so a downhill charge
is naturally strong.

**Constraints it must respect.** All of it is integer arithmetic on a heightfield, so it
runs inside the parallel-systems rules (`07:07.4`) — visibility and cover are map-only
passes over a frozen tick state, which makes them ideal parallel work and a determinism
risk only if a reduction is unordered. Elevation must be evaluated from *state*, never
from the renderer, and the editor's `wsp_preview` must fold it into the odds it shows, or
the editor will disagree with the game (`AD-15`).

**Consequences.** The world model gains a heightfield: authored per sector, part of the
world content pack, versioned with it. Because `z` was already stored, there is **no
structural save migration** — only a rules-version bump, which is exactly what `AD-26`'s
seam bought. Level-of-detail in the art now has a mechanical meaning, so terrain that
*gives* high ground must *read* as high ground; silhouette legibility becomes a rule
requirement, not a stylistic preference.

**Rejected.** *Keep height cosmetic* — the studio has said gameplay will lever on it.
*3D distance metrics for range* — 4C measures range in sectors, and redefining it to
euclidean 3D distances would change every table in the book; elevation as row steps keeps
the rules recognizable. *A separate "elevation bonus" stat* — a parallel modifier system
next to row steps is how a tabletop-derived engine drifts into two contradictory rule
sets.

---

## AD-29 — Cosmetic packs are save-neutral; gear slots exist from day one

**Context.** The intended 3D character upgrade ships as a content pack (`AD-27`). Two
things in the current design would make that painful. A save binds `content_hash` over the
whole merged content set (`03:03.5`), so enabling a purely visual pack would look like a
content mismatch and could be reported as one. And equipment has no representation in the
visual descriptor, so "attach a sword mesh to a hand" has nowhere to attach.

**Decision.**

1. **Split the content identity.** `content_hash` becomes **`rules_hash`** (structural
   tables plus every rules-bearing record) and **`presentation_hash`** (visual
   descriptors, string tables, and their assets). A save binds `rules_hash`; a change in
   `presentation_hash` is recorded in the load report and the save header but **never**
   invalidates a save or blocks a load.
2. **Packs declare their kind.** A pack is `content` or `presentation`. A presentation
   pack may define visuals, strings and assets, and **may not** define a rules record;
   the validator rejects a pack that tries (`03:03.9`).
3. **The visual descriptor carries attachment slots.** `visual.slots` names the bones or
   anchor points a rig can attach to — `mainhand`, `offhand`, `head`, `body`, `back` —
   even in v1, where the 2D presentation expresses gear as a small number of outfit
   variants and leaves most slots unused. Item records declare which slot they occupy
   (`item.slot`), which the equipment model needs anyway.

**Consequences.** A 3D character pack is a presentation pack: it can be enabled to try,
disabled to compare, and **applied partially** — the party but not the townsfolk, humans
but not creatures — because the descriptor is per record. It cannot alter balance by
accident, which also means a cosmetic pack is *safe to install* in a way a rules mod is
not, and that is a genuinely useful distinction to surface in the mod manager: **a
presentation pack never invalidates a save**, so it does not need the same warnings. The
cost is one more axis in the content model and a validator rule, both of which the editor
handles by writing packs of the right kind from the project's own noun.

**Rejected.** *One hash, and treat cosmetic packs as content* — every visual experiment
would look like a broken save, and modders would learn to ignore the warning that exists
to protect their saves. *Add gear slots when the 3D pack lands* — it would mean
retrofitting every character and item record after content and mods existed, which is the
migration this decision exists to avoid.

---

## AD-30 — Full 3D, with a multi-profile camera

**Context.** `AD-26` deferred the presentation and `AD-27` chose HD-2D. The studio has
chosen otherwise: **full 3D**, on the strategy of shipping a demo early to fund 3D
artists through pre-sales. The reasoning is sound and one part of it is exactly right:
with real 3D meshes, camera angle no longer multiplies art, so the camera can be
top-down, isometric or ground-level **per context** rather than fixed. The mechanics are
not tied to the representation (`AD-28` evaluates elevation from simulation state), so the
game can be fully implemented before the final art exists.

**Decision.** Full 3D meshes and rigs. Five rules make the flexibility real rather than
nominal — each one exists because the obvious implementation breaks something:

1. **The simulation owns camera *state*; the renderer owns the camera *matrix*.**
   Profiles (`top_down`, `isometric`, `ground_level`, `dialogue`, `overview`), their
   targets, transitions, shake and constraints live in **content** and are evaluated in
   the sim as fixed-point state, so a cinematic is deterministic and replayable. The
   renderer converts that state to a float view matrix and does fine-grained frustum
   culling. Coarse culling and LOD selection live in the sim, which keeps the packet
   bounded instead of shipping every entity every frame.
2. **Perception is computed from the character, never from the camera.** A ground-level
   camera sees over walls the character cannot see past; a top-down camera sees things
   behind the character's back. Sight, cover and cover-based targeting come from the
   character's eye level and the heightfield (`02:02.12`), so what the player *knows* and
   what the UI *offers* never diverge because of a camera move. This is the rule that
   makes a free camera safe in a game with line of sight.
3. **The rules' heightfield is a quantised layer, not the art geometry.** Art may have
   slopes, stairs, rubble and overhangs; the rules read integer levels and blocking
   heights. Coupling rules to art geometry would make balance depend on a modeller's
   mesh, and would put floats in the simulation.
4. **Picking is resolved to discrete targets before it reaches the sim.** A pointer event
   travels main → render worker (which unprojects it using the camera and the actual
   geometry) → `wsp_pick(ray)` → and the sim returns an **entity id or sector**, never a
   float coordinate. Floats decide *what you clicked*; they never enter a rule.
5. **Billboards remain a first-class draw kind.** Not for characters any more, but for
   foliage, particles, decals, impostors and LOD proxies — and because a mod that ships
   2D art should still work (`AD-29`).

**Consequences.** The camera becomes a system with an authoring surface: profiles,
transitions, a per-context director, and cinematics — which is what makes a 3D CRPG feel
cinematic, and is also new scope that the HD-2D plan did not have. Two costs are real and
should be planned for rather than discovered:

- **Environment art is harder, not easier.** Characters are now angle-independent, but
  every environment must be built and lit to be seen from *every* angle a camera profile
  can reach. With a rotatable camera you cannot hide the back of anything, so level art
  and lighting work multiplies relative to a fixed camera. Multi-profile cameras are free
  for the cast and expensive for the world.
- **Animation is the hiring bottleneck, not modelling.** Clips retarget and are shared
  across the cast; a single character with a bespoke rig and bespoke clips cannot be
  extended by a second artist. That is what `AD-31` exists to prevent.

**Rejected.** *Renderer-owned camera* — simpler, but the packet then carries everything
every frame, and cinematics become non-replayable because the camera lives outside the
deterministic state. *Camera-based perception* — the standard 3D shortcut, and it breaks
the contract between what the player sees and what the character can do; in a game with
cover and elevated firing positions it produces constant, inexplicable targeting
failures. *Rules reading the art heightfield* — puts meshes and floats inside the
simulation, and makes a balance value out of a modelling choice.

---

## AD-31 — The asset pipeline is a contract, written before the first contractor

**Context.** The plan is to fund 3D artists from demo pre-sales. The classic way that plan
fails is not budget: it is that the first two contractors each deliver a beautiful
character with **their own rig, their own scale, their own bone names and their own
animation conventions**. Neither can use the other's clips, neither fits the world, and
the "cast" has to be re-rigged before it can be animated — which costs more than the
art did. The compatibility surface for a 3D game is the **shared skeleton**, and it has to
exist before anyone is hired.

**Decision.** Treat the 3D asset pipeline as a normative contract, the same way the content
format is (`08-asset-pipeline.md`), and enforce it mechanically:

1. **A written contract** covering scale and units, the shared humanoid rig and its bone
   names, the export format and axis convention, texture and material rules, animation
   clip naming and conventions, LOD tiers, collision and gameplay proxies, and naming.
2. **An import validator** that checks every rule at load and reports failures the same
   way content validation does (`AD-15`): structured `{path, code, args}`, an error for a
   broken contract and a warning for a suspicious value. A rig whose bone names do not
   match the standard is an **error**, not a warning.
3. **An editor preview as the acceptance step.** A contractor's delivery is accepted when
   it loads in the editor, plays its clips on the shared skeleton, and reports no
   contract errors — not when it "looks right in Blender".
4. **Gameplay anchors are part of the rig**: eye level (which `AD-28` elevation reads),
   hit zones for the melee/ranged distinction, cover anchors, and the equipment
   attachment slots (`AD-29`). They are authored once per rig, inherited by every
   character built on it.
5. **Placeholder art is a presentation pack like any other.** The demo ships with
   placeholder or asset-store models as a `presentation` pack; the final cast is the same
   kind of pack with the same descriptors (`AD-29`), so replacing it touches no rules, no
   content and no save.

**Consequences.** Contractors become parallelisable and their work becomes *reviewable
against a checklist* instead of against taste, which is the difference between art
direction and art arbitration. It also means the demo's art can be deliberately cheap:
because the final cast swaps in as a presentation pack, the demo does not need to be
thrown away — its systems, content and levels are all reused, and only the visuals are
replaced. The cost is front-loaded: writing the contract, the rig and the validator before
the first hire, which is a few days of work that protects the entire art budget.

**Rejected.** *Standardise later* — "later" means after the cast exists, and the retrofit
is re-rigging every character and re-authoring every clip. *Leave the rig to each artist*
— guarantees the animation library cannot be shared, which is the whole economy of a
3D cast. *Validate by eye* — a scale error or a mirrored axis is invisible until it is
systemic, and by then it is in the store page's screenshots.

---

## AD-32 — Blender is the DCC, the level-geometry editor and the asset compiler — not the game editor

**Context.** The studio develops on Linux and asks whether Blender can be the game's 3D
asset editor, to reduce the amount of tooling it has to build. The question has three
different answers depending on which job "asset editor" means, and conflating them is how
the tooling ends up with two sources of truth.

**Decision.**

1. **Blender is the DCC.** Models, UVs, materials, rigs, skins, LODs and animation are
   authored in Blender, with `.blend` sources accompanying every delivery
   (`08:08.7`). The version is **pinned** (Blender 5.2 LTS at the time of writing) because
   its glTF output and Python API both drift between releases.
2. **Blender is the level-geometry editor.** 3D spatial layout — where buildings, rooms,
   props and terrain sit — is authored in Blender through a project addon. Spatial work is
   what Blender is good at, and the edit loop needs no new machinery: save in Blender, the
   dev build's asset watcher reloads it (`FR-AST-14`, dev only), playtest in the game.
   Blender is the modelling surface; the game is the playtest surface.
3. **Blender is the headless asset compiler.** `blender -b -P tools/blender/exporter.py`
   runs in the build and in CI to **normalise** a delivery (scale, axis, naming, rest pose)
   rather than merely judge it, export glTF, generate LODs, convert textures to KTX2
   (via KTX-Software, which Blender does not do), render editor thumbnails, and emit the
   contract report that the runtime's own validator consumes. A contract error fails the
   build.
4. **Blender is not the game editor.** The shipped web editor (`AD-12`, `AD-23`) remains
   the surface for rules, content records, encounters, quests, dialogue, camera profiles
   and cinematics. It is a product feature and the modding funnel: a player must be able to
   change a dialogue line without installing a 3D application.

**The interface between them is `extras`, not a bespoke format.** Blender custom properties
export into glTF `extras`, so anchor names, equipment slots, LOD grouping and per-object
metadata cross the boundary inside the standard file. Two things glTF has no concept of and
therefore need small exporter work, not wishful thinking: **LOD grouping** (a naming
convention plus `extras`) and **animation clip markers** (Blender timeline markers are not
exported by default).

**Ownership is explicit, because overlap is the failure mode:**

| Blender owns | The web editor owns | The build owns |
|---|---|---|
| Geometry, UVs, materials, rigs, skins, LODs, animation, 3D spatial layout | Rules, content, encounters, quests, dialogue, camera profiles, cinematics, and the **authoritative gameplay layers** — sector heightfield, blocking heights, cover, spawn points | **Derivation**: proposing the heightfield and blocking heights from tagged collision meshes for a human to confirm in the editor |

**Consequences.** Most of the 3D tooling that would otherwise be built is not built: no mesh
editor, no rig editor, no animation editor, no terrain sculpting, no viewport navigation.
The editor's 3D viewport exists to *see the game's own rendering* and to inspect anchors and
gameplay layers, not to author geometry. The costs are real and bounded: a pinned Blender
version, an addon that must be GPL-compatible (Blender's position for scripts using its API
— which permits selling it, and does not affect the game or its assets, since tool output is
not a derivative work), KTX-Software as an additional pinned tool, and lightmap baking kept
on a workstation because EEVEE needs a GL context while Cycles CPU bakes headless.

**Rejected.** *Blender as the game editor* — it cannot ship to players or data modders, has
no notion of the content schema, validation, playtest, quests or camera profiles, and would
mean rebuilding the editor as addon panels in a UI worse at data entry than a form.
*Rules data derived from meshes at runtime* — couples balance to modelling and puts floats
in the simulation (`AD-30`). *A bespoke Blender file format* — glTF plus `extras` already
carries anchors, slots and metadata, and every artist and modder already has an exporter for
it. *Let each artist pick their Blender version* — different versions produce different
glTF, and "it looked right in Blender" is not an acceptance criterion (`08:08.10`).

---

## AD-33 — The sector is a 1 m tile, and distance is content in integer tiles

**Context.** 4C states that "one sector is approximately 10 feet" (`4c:914`), a battlemat
abstraction for a game played on a table. The studio models and lays out levels in Blender,
where one unit is one metre (`08:08.1`, `AD-32`), and wants the grid an author works on to be
the grid the rules read. The 3.05 m sector of the first revision snaps to neither the metre
nor an integer, and it is false precision on a source that says *approximately*. The sharper
reason is mechanical rather than artistic: at a 3 m tile the character *is* the tile, so
there is no way to express that a body has a footprint and an opening has a width. A 3 m
character and a 0.8 m door are each "a sector", and footprints, crowds and doorways all
collapse into the same cell.

**Decision.**

1. **The sector is a 1 m × 1 m tile.** One Blender grid square is one sector. The tile is
   **structural and fixed**: not a content parameter, and no mod may change it, for the same
   reason `AD-14` locks the ladder — a save's meaning depends on it.
2. **The 4C tables stay authored in the source's unit and convert once.**
   `1 legacy sector = 3 tiles` (10 ft is 3.048 m, rounded to 3 so every derived distance
   stays an integer). Linear counts multiply by 3; **per-distance modifiers keep their
   step**, stored
   as `{ steps, per_tiles: 3 }` (`4c:880`, `4c:995`, `4c:1004`), because a naive
   "sector → tile" rewrite would silently triple them. The loader asserts
   `per_tiles == legacy_sector_tiles`.
3. **Distance parameters are content, in integer tiles, in the rules pack** (`02:02.6`). No
   floats and no metres on the simulation path (`AD-6`, `02:02.9`), and no weapon id in Rust:
   a weapon's reach is a range record in the `02:02.4` vocabulary, so a mod can retune or add
   one (`02:02.8`).
4. **Granularity is locked; balance is moddable.** Ranges, movement and the per-distance
   steps are moddable records. `tile_mm` and `legacy_sector_tiles` are not, because changing
   them changes the world's geometry — a generator-class change (`02:02.6`), not a balance
   tweak — and would invalidate every saved world and every other mod's coordinates.
5. **Occupancy is one entity per tile, and entering an occupied tile is a contest**, resolved
   through the existing Brawn tables (`4c:1013-1042`) with knock-back (`4c:1175`) rather than
   a new resolution path. Passage requires both a height clearance and a width clearance
   (`02:02.6`).

**Consequences.** The mechanical payoff is that footprint and height both gate movement, so a
3 m character cannot use a door authored for a smaller one, two characters can share what
used to be a single sector, and a waist-high wall is a tile a character can duck behind
rather than a whole-sector property. The cost is spatial: a district holds roughly **9× the
cells**, the heightfield and blocking-height layer grow with it, and a sight-line supercover
takes about **3× the steps** for the same physical distance (`07:07.4`). Content volume, not
correctness, is the budget that moves. Melee adjacency becomes a realistic 1 m, which also
means 4C's rationale for excluding melee from elevation deserves a second look (`02:02.12`).

**Rejected.** *Keep ≈10 ft / 3.05 m* — no footprint resolution at all, and a grid that is
neither metric nor integral. *1 m tiles without rescaling the tables* — the rifle's 8 sectors
become 8 m, which silently rebalances the game instead of resizing its grid; the rescale is
part of the decision, not an implementation detail. *Sector size as a content parameter* — a
granularity knob changes world geometry, so exposing it invites saves and mods that cannot
interoperate; it is structural (`AD-14`). *Metres as the config unit* — puts floats on the
simulation path (`AD-6`, `02:02.9`). *Rewriting every 4C count to tiles in place* — the four
per-distance modifiers would then have to be redefined as "per 3 tiles" at every site, and
the next person to add a "−1 per sector" rule will not know the unit moved; one conversion
constant is cheaper to get right and cheaper to explain.


## AD-34 — The engine owns the available-action set, and the interface renders it

**Context.** A CRPG interface has to answer "what can I do here?" — talk, wait, fight,
spend Fortune — and the answer is a *rule*: Wait is legal in a fight or it is not, Talk has
someone to talk to or it does not. The cheap implementation is for the page to look at the
world, the roster and the open conversation and decide for itself which buttons to show.
It is also a second rules engine, in a second language, that will disagree with the first
the moment either changes (`AD-15`).

**Decision.** The core answers, through `wsp_actions` (`02:02.13`): every contextual action
with `enabled`, a `reason_key`, and whatever target the action needs. The shell renders
that answer and posts the command the action names. A disabled action is shown disabled
with the engine's reason rather than removed (`02:02.11` rule 2). Two capabilities the
answer needs are engine-side for the same reason: a dialogue option may carry an *action*
that invites a fight — the only way a fight begins during a conversation — and a station
may keep a `shop`, so a counter is a place rather than a menu.

**Consequences.** Adding an action, or changing when one is legal, touches the projection
and the spec and nothing else: the interface gains a button and a string id. The offer and
the refusal agree by construction, because they read the same state. The action set is
covered on both sides of the ABI — the host library and the packaged wasm — because the
projection crossing the boundary is what the page actually reads.

**Rejected.** *Derive availability in TypeScript from the existing projections* — no ABI
change and quicker to write, but it duplicates rules (the advancement cost curve, adjacency,
the modal-conversation rule) in a language with no test that can compare it to the engine;
the first divergence is a button that lies. *A UI-only convention, with the engine refusing
afterwards* — the refusal is still needed, but offering an action the engine will reject is
exactly the interface the player learns to distrust. *A separate "legal actions" command
the shell polls* — a projection is enough; a command implies state.

## AD-35 — The development surface is the launcher's answer, and a release has none

**Context.** The play page needs a development mode: diagnostics, the log, the topology and
feature probes. The obvious gate is a build-time flag in the shell. It does not work here:
`make play` and `make package` assemble the **same** `game/` tree, and `make play` merely
serves it with a `kobra_dev` launcher, so the folder and its shell are byte-identical in a
development run and in a packaged one. A flag baked into the shell would either mark both or
neither. Worse, a URL parameter or a key chord the page checks for itself is a gate the
player can open.

**Decision.** The launcher reports it. A `kobra_dev` build sets `dev: true` in
`GET /__kobra/diagnostics` (`Launcher-spec.md` §21.4); the field is omitted when false, and
a release binary has no code path that can set it. The shell offers its Dev sidebar from that
field alone. The sidebar holds diagnostics and the log — no dev-only commands — so the
surface a mis-gate would expose is telemetry rather than the ability to rewrite state.

**Consequences.** The signal is fail-closed in the same way a version gate is
(`VERSIONING.md`): absent means not-development, and only the binary that is a development
build can say otherwise. The page never decides its own privileges. The cost is a launcher
field and its spec text — small, and in the repository that already owns the build tag.

**Rejected.** *A build-time `__WSP_BUILD__` marker* — `assemble` stamps a content hash, not
`dev`, and both paths share the artifact, so it cannot distinguish them. *A `?dev` query or
a key chord checked in the page* — forgeable by anyone, and therefore not a gate. *Ship the
sidebar in every build behind a toggle* — a release surface that exists only to be disabled
is a release surface that will eventually be reached. *Fold dev mode into the launcher's own
UI* — the launcher has no game DOM, and the diagnostics are about the running engine, which
only the page can see.

## AD-36 — The UI is a moddable surface, built on versioned projections

**Context.** A CRPG lives or dies on its interface, and every durable game in the genre has
a moddable one. Ours was a closed shell: panels written in TypeScript, mounted by hand, with
no way for a mod to add a HUD, replace the character sheet or re-skin anything but a texture
and a string. The engine, meanwhile, was already built for this — it owns state, exposes
read models, and answers "what can I do here" (`AD-34`). The interface was the only closed
layer.

Two alternatives were on the table. Keep the shell closed and let modders swap textures and
strings (safe, and leaves the platform's biggest modding surface on the floor). Or open it
and accept that a mod's code runs on the page, with DOM access. The second is what the
industry ships, and `03:03.10` already accepts a comparable residual risk for Tier-2
scripts in the worker.

**Decision.** The interface is a moddable surface with five contracts and no privileged
path (`09`). A mod ships a **layout** (presentation data, no code), a **renderer** (an ES
module with a capability object), or a **theme** (CSS custom properties and a stylesheet).
The contracts are: projections (engine → UI, versioned, subscribed, revised), the action set
(`02:02.13`), the command vocabulary, the string tables (`AD-22`), and UI state (launcher
config, never the save).

Three properties make it safe enough to ship:

1. **The shipped panels are the first UI pack.** They register through the same registry and
   receive the same `UiContext` a mod does, so every build exercises the public contract.
2. **Every mutation goes through one choke point.** The page holds no wasm handle (`AD-4`),
   so `ctx.post(command)` is the only path, and it enforces the command scope each entry
   declared. The default scope is read-only.
3. **The command stream stays the replay log.** UI-only state — an open panel, a tab, a HUD
   position — never enters it, so a replay does not depend on the interface, and UI packs are
   presentation (`AD-29`): `presentation_hash`, never `rules_hash`.

**Consequences.** The projection shapes become a **public API** the moment packs ship, so
they are versioned (`worldspiracy.projection/<name>/1`, `worldspiracy.ui/v1`) and covered by
tests on both sides of the ABI. Adding a projection is a read-model change with a schema and
a test, never a rule change. The engine must not grow UI-shaped commands ("sort inventory",
"mark read") or UI state, because both would put presentation into the replay log. The mod
manager must state what a pack declares before it is enabled. And the base UI is no longer
special: a panel that cannot be expressed through the public contract is a signal that the
contract is wrong, not that the panel needs an exception.

**Rejected.** *A closed shell with texture and string overrides only* — smallest surface,
but it forfeits the platform's most-requested modding category and guarantees forks. *A
private extension API for the editor only* — two UIs, one contract's worth of maintenance,
and the divergence `AD-15` warns about. *Special-casing the base panels in TypeScript* — the
public contract would then be exercised by nobody, and would be discovered broken by the
first modder rather than by CI. *Sandboxed iframes from day one* — real containment, but it
needs a `frame-src` CSP change, a message bridge and a styling story, and it buys protection
against a threat the project has explicitly accepted; the contract is written so the
transport can move into a frame later without a rewrite.

## AD-37 — Layout binds paths and engine signals; there is no UI expression language

**Context.** A declarative UI needs two things a markup document cannot otherwise express:
*display this value*, and *show this only under this condition*. The tempting answer is an
expression language: `text="{fortune - cost}"`, `when="standing('watch') >= 10"`. It is also
the most expensive answer available, for two independent reasons.

First, the platform forbids it. The launcher's CSP is `script-src 'self'
'wasm-unsafe-eval'` (`FR-SRV-17`): no `'unsafe-inline'`, no `'unsafe-eval'`, and
`'wasm-unsafe-eval'` permits WebAssembly compilation only. Any declaration whose expressions
are JavaScript is blocked — which is why HTMX's `hx-on:`, Alpine's default build and Vue's
runtime compiler cannot run here. Alpine's *CSP build* is the proof that interpretation works
and the measure of its cost: a hand-written parser, a restricted grammar, and a permanent
list of surprising limitations.

Second, even interpreted, a condition in the UI would be a second implementation of
`02:02.7`'s condition vocabulary, in TypeScript, drifting from the engine — exactly what
`AD-15` exists to prevent.

**Decision.** Layout has **no expressions**. Two primitives, and nothing else:

- **A path** for display: `text="{roster.leader.fortune}"` resolves a dotted path against
  the subscribed projections and writes the result as text. No operators, no calls, no
  assignments.
- **A signal** for gating: content declares named conditions in the engine's own vocabulary,
  the engine evaluates them, and layout tests a boolean (`when-signal`).

Interaction is a **verb** bound to the action set: an engine verb carries `enabled`,
`label_key` and `reason_key` from `wsp_actions`; a navigation verb (`open`, `toggle`) decides
nothing and needs no engine answer. Anything beyond paths, signals, verbs and iteration is a
**renderer** (`09.6`) — real JavaScript with a capability object, where it can be typed,
tested and reviewed.

**Consequences.** Subscription and dependency tracking fall out of the design rather than
being maintained: a path's first segment *is* its projection, so a layout's subscription set
is a scan and each bound node knows what it depends on. A new condition is a `signal` record
— presentation, validated by the one validator, save-neutral — not a language extension. The
primitive set is deliberately closed, so if three mods need the same operation it is added
as a named, tested binder operation in this document, not as an escape hatch. And mod
authors still have full power: the ceiling is on markup, not on the platform.

**Rejected.** *Interpret an expression language we write* — a grammar, a type model, a
security boundary against `__proto__` and `constructor`, resource limits and per-node error
reporting, owned forever, in exchange for letting game logic into markup where it cannot be
tested like a renderer. *Evaluate conditions in the shell* — a second evaluator of `02:02.7`,
drifting from the quest and dialogue systems it must agree with. *Compile templates at build
time* — the right answer for the **base** UI's ergonomics, but it requires a toolchain, and
the casual modding case is a folder dropped into `data/mods/`; it stays available as an
authoring aid that emits the same registry entries, never as a runtime dependency.
*Borrow HTMX's attributes wholesale* — its surviving attributes are HTTP-and-fragment
oriented and its evaluated ones are CSP-blocked, so what fits is the philosophy (locality of
behaviour, one dispatcher), which this decision adopts without the dependency.

---

## AD-38 — The shipped runtime is native; the web build is retired

**Context.** The web stack was chosen to buy one artifact for every operating system
(AD-1…AD-3). It delivered that, and it charged for it in five documented ways: the player's
browser becomes the product's floor and its support matrix (AD-17); on Linux the browser's
WebGPU needs a Vulkan feature that distro builds disable, and the launcher cannot pass the
flag (upstream request U8, `03:03.12`); a launcher process exists only to hold an HTTP
origin, and brings COEP, sessions, CSRF, port allocation and browser detection with it; the
browser version is the one input the build cannot pin, so "reproducible, own-it-forever" has
a hole in it; and the frame budget carries a tax the project must *defend against* native
rather than spend (AD-24). AD-41 makes the first and the fourth unacceptable: an owned
artifact cannot have a third party's release schedule inside its runtime. The simulation
core was never web code — `crates/kobra-core` is a zero-dependency Rust crate with a hand-written C
ABI and no `wasm-bindgen` (AD-2), integer-only (AD-6), compiled as both `cdylib` and
`rlib` — and its Rust tests and the replay oracle already run **natively**, under
`cargo test`, without a browser.

**Decision.** The shipped runtime is a native Rust binary. `wsp-core` is linked as a crate
(`rlib`), not called through the wasm ABI, so the sim boundary is a Rust call with no
marshalling and no linear memory. The platform layer is `wgpu` (Vulkan on Linux; DX12 and
Metal stay reachable if Windows or macOS are built later) and `winit` (Wayland and X11).
The RenderPacket stops being a *transport*: there is no second worker, no
`SharedArrayBuffer` and no ring, so a frame is a plain description produced by a call and
consumed in the same process. It keeps its integer-only encoding and its `state_hash`,
because those are determinism tools rather than transport. The web build — `src/web`, the
DOM interface, the browser host, the wasm toolchain and the launcher's serving path — is
**retired, and the pivot's first change removes it from the tree** (git history holds it, and
version control remembers better than a directory nobody dares delete). It was the working
prototype of the interface, the modding UX and the content pipeline; what carries forward is
the design intent recorded in `01`, `04`, `09` and this log, not the code.

Three rules keep the pivot from becoming a rewrite: the simulation is untouched; the seam is
the packet; and the content model — packs, the overlay map, the validator, the generated
Master Table — is engine-neutral and moves as it stands.

**Consequences.** AD-3, AD-4 and AD-5 (the TypeScript renderer, the two-worker topology, the
shared frame ring and the `require-corp` isolation configured to enable them) are
superseded. AD-17's browser floor and the whole `min_browser` machinery become moot, and U8
is withdrawn rather than answered. AD-6 (integer-only math) **stays**: it is no longer a
workaround for a narrow ABI, it is the determinism contract, and the native renderer's
floats must never feed back into the sim. AD-24's 15%-of-native comparison is retired — the
native build *is* the product — but its discipline survives intact: budgets, counters, a
committed benchmark scene, and a miss being a defect. The replay oracle keeps its authority
and gains a native host, so a divergence in the native build is still a failed gate.
Reproducibility gets simpler, because every input is now pinned by the build. The cost is
stated rather than hidden: one artifact per operating system again. That is accepted,
because `wgpu` and `winit` keep Windows and macOS reachable from the same code without a
browser, and because AD-41's primary artifact is a folder a player owns rather than a
download that must run everywhere on day one. The removal itself lands as a green state: the
gates that survive — the Master Table property test, `cargo fmt`, and `cargo test` with the
replay oracle — run on the host with no wasm, no node and no toolchain checkout, which is
what makes it safe to demolish before the replacement exists.

**Rejected.** *Keep the web build as the primary runtime* — it cannot satisfy an
owned-forever artifact, and it keeps a browser's release schedule inside the product.
*Maintain both and ship as configured* — two presentation implementations to keep in step,
with the divergence landing in the save format where it is expensive. *A Tauri/WebView shell
around the existing wasm* — it keeps exactly the dependency this decision removes, and adds
a host process to the bargain. *A third-party engine as the host framework* — it would own
the schedule, the scene format and the ECS, and its pre-1.0 breaking releases would become
the project's maintenance cost; the host this project needs is thin and owned (`07` owns the
budget, `09` the interface). *Transliterate the DOM interface into a native toolkit* — the
interface contract is re-derived through AD-42's headless-first authoring step, not ported
widget for widget. *Keep the web build in-tree as a frozen reference* — a second,
unmaintained implementation of the same interface is exactly the false affordance that costs
an agent context and invites it to build against a runtime that no longer exists; git
history is the reference, and it is one `git show` away.

---

## AD-39 — Lua 5.4 is the glue and modding language; Rust remains the rules engine

**Context.** The simulation is Rust and must stay the only implementation of a rule
(AD-15). What remains is content-shaped logic — quest graphs, dialogue conditions, AI
behaviour, encounter scripting, interface flow — plus the modding surface, and both need a
language that a modder can learn from a folder of examples, that an AI agent can author and
review as text, and that is deterministic enough for the replay oracle to keep meaning
something. AD-8 chose ES modules because the platform already had a JavaScript engine.
Natively that reason is gone, and embedding a JavaScript engine would be choosing a
language for the platform's convenience rather than the modder's.

**Decision.** **Lua 5.4, embedded through `mlua`, vendored and pinned.** Lua is the
CRPG/strategy modding lingua franca — it is what modders of this genre already know — it is
small enough to read end to end, and it has the decisive property: a true 64-bit **integer
subtype with two's-complement wraparound**, which is exact parity with the integer-only
simulation (AD-6). Luau is rejected on that ground and not on taste: its numbers are IEEE
doubles, so a mod's arithmetic would silently disagree with the engine's.

The boundary is fixed, not negotiable per feature:

- **Rust owns the rules.** Lua asks the engine to resolve; it never re-derives a roll
  (AD-15).
- **Rust owns the save.** A script changes state only through engine APIs that the
  simulation records (AD-8's rule survives verbatim).
- **Lua owns content-shaped logic**: quests, dialogue, AI decisions, encounter scripts,
  interface flow.
- **No Lua in the per-frame hot path.** Scripts are called at named hooks under a
  documented budget; a Lua frame in a profile is a missing Rust primitive, not a reason for
  more Lua.

Three rules make Lua deterministic enough to sit in the hook path, and each is enforced
rather than documented: sim-path scripts may not use floats, the wall clock, the locale or
unseeded randomness; hash-table iteration order is forbidden on determinism paths (arrays
and explicitly ordered keys only), because Lua 5.4 seeds its string hash per state and
`pairs` order therefore differs between runs; and that seed is pinned at build time
(`LUAI_MAKESEED`) as a second line of defence. The validator gains a Lua lint, so a
violation is a content error before it becomes a replay divergence, and a script's hash is
recorded in the save exactly as AD-8 records it today. The implementation point already
exists: `crates/kobra-core/src/script.rs` documents its host as "absent unless one is `install`ed"
and calls that a production seam, so this replaces one host with another rather than moving
a boundary.

**Consequences.** AD-8's Tier-2 ES modules become Lua, and the interface tier (`ui/*.js`)
becomes Lua plus the layout contract AD-42 re-derives. Tier 1 is untouched: declarative
JSON with no executable code, which is where the overwhelming majority of real mods live.
The core crate stays dependency-free; `mlua` lives in the host crate the core already
expects. Replay fixtures gain a "script set" dimension, so a replay that differs because a
script changed says so by name. Documentation for modders becomes Lua-first, which is a
smaller teaching job than ES modules and a larger ecosystem to borrow examples from.

**Rejected.** *Luau* — doubles, so script arithmetic cannot match the engine's integer
math. *Embedded JavaScript (QuickJS or `deno_core`)* — it would preserve the existing Tier-2
API verbatim, but it keeps a JavaScript toolchain in a native project and asks modders to
write the language the platform used to force on them. *WASM/Wasmtime for the script tier* —
the strongest sandbox and language-agnostic, and the right answer if a curated mod channel
appears (AD-40 keeps that door open), but a much higher authoring barrier for the modder the
tier exists to serve. *Lua for the rules* — a second rules implementation, which is the bug
AD-15 exists to prevent. *A binding generator instead of a hand-written API* — a build
dependency and a generated contract, where the thing modders need to read is a documented,
versioned surface.

---

## AD-40 — Four modding tiers: data, Lua, native plugin, engine

**Context.** AD-8 fixed two tiers and explicitly closed the native and mod-wasm doors, and
it gave its reason: the platform's CSP made them expensive, the threat model already
classified mods as trusted user data rather than a security boundary, and there was no
mod-distribution channel to justify the machinery — "revisit only if a mod-distribution
channel ever appears". A native runtime changes all three conditions: there is no CSP, a
distribution channel is exactly what AD-41's owned artifact needs, and a Linux modder
extends an engine with a shared library. The studio has decided that a modder may ship a
native library that increases engine capability.

**Decision.** Four tiers, and the tier table is normative:

| Tier | Ships | Trust | Determinism |
|---|---|---|---|
| 1 — Data | packs, locales, assets | data, always | hash recorded in the save |
| 2 — Lua | `scripts/*.lua` | sandboxed by `mlua` with a restricted stdlib (AD-39) | script set and hashes recorded in the save |
| 3 — Native | a shared library implementing the plugin ABI | **trusted user code, in-process** | plugin set and hashes recorded in the save |
| 4 — Engine | the binary | us | — |

The native tier's contract is deliberately small:

1. **One exported symbol.** `wsp_plugin_query(u32 abi_version) -> *const WspPluginV1`,
   returning a size-versioned, append-only struct of function pointers — the loader
   pattern, so a plugin compiled against version *n* runs on *n+1* when the struct only
   grew. The boundary is **C**: Rust has no stable ABI, so no Rust type crosses it. The
   repository ships the header, a sample plugin and a build stub, so a plugin can be written
   in an afternoon.
2. **Plugins register primitives, not access.** A plugin adds effect kernels, hooks or Lua
   functions to the sandbox (AD-39); it never walks simulation memory. That keeps
   determinism auditable and the versioning story containable.
3. **Loading is explicit and revocable.** A plugin loads only because the player enabled it
   by name, and "start without mods" is always available. Loading uses local symbol
   visibility (`dlopen` with `RTLD_LOCAL`; the equivalent on other platforms).
4. **The save records the plugin set.** A save taken with a plugin loads without it, with
   the loss named — the same rule AD-8 applies to scripts and AD-36 applies to interface
   packs.
5. **A crash names the plugin.** In-process native code can take the game down; that is the
   accepted price of the tier, consistent with the existing "mods are trusted user data"
   stance. What is owed is a crash report identifying the last plugin entered, and a
   documented recovery path.

**Simple first (studio decision).** Plugins load in-process, from an allowlist, at boot. The
out-of-process plugin host — a real containment boundary that marshals the plugin ABI over
IPC — is the later complexification, and because the ABI is versioned it can move there
without a plugin rewrite.

**Consequences.** The tier list in `03:03.7` is superseded, and its rewrite is owed — the
document carries a pointer at its head until it lands. "There is no mod-wasm path" stops
being a statement about the platform and becomes a statement about the authoring barrier:
WASM remains a candidate for a future script tier rather than a rejected idea. The
determinism gates extend to native plugins, but the validator can check only a plugin's ABI
version and its declared capabilities, never its behaviour — so the documentation must be
explicit that tier 3 is code, not content. Per-platform artifacts become a modder-facing
fact: a plugin is `.so`, `.dll` or `.dylib`, and the authoring guide has to say so.

**Rejected.** *Keep the two-tier limit* — it contradicts the studio decision and leaves
"increase engine capability" unsupported. *A Rust plugin ABI (`cdylib` plus `extern "Rust"`)*
— there is no stable Rust ABI, so every toolchain bump would break every plugin. *An
out-of-process host from the start* — real containment, but it buys protection against a
threat the project has explicitly accepted, at the cost of marshalling the whole ABI over
IPC, and the studio has chosen to stay simple first. *WASM for everything, with no native
tier* — safest, but it declines the studio decision and makes engine extension impossible
for the modder who already has a compiler.

---

## AD-41 — Ownership is the distribution model; Steam is secondary

**Context.** The product promise is "pay once, own forever". Steam does not deliver that:
it delivers "pay once, play forever as long as you run Steam" — the client is the licence,
the update path and the launch path. The studio has therefore decided that Steam is a
**secondary** channel and that the primary artifact is one the player owns outright. This is
a distribution decision with architectural consequences, because ownership is a property of
the artifact and not of the store page: an owned copy must run with nothing installed, no
account, no network, and no third party's release schedule.

**Decision.** The primary artifact is a **self-contained, offline folder** — a native
binary, its assets, and the player's `data/` — distributed as a checksummed archive from our
own site, with Flatpak/AppImage as conveniences rather than as the only form. The rules that
make ownership real:

- **No runtime network.** The game never talks to the network in order to run. AD-19's "the
  page never talks to the network" becomes "the binary never talks to the network"; the only
  exception is an update check the player explicitly asks for.
- **Saves are documented, portable files**, never only in a store's cloud. An optional
  cloud-sync feature may exist; the local file is the source of truth.
- **Releases are signed** (minisign or sigstore), so an update fetched from our own site is
  verifiable offline. A hash-only release is sufficient for a store that verifies on the
  player's behalf and insufficient for a copy the player owns and updates themselves.
- **Store integration is optional and dynamically loaded.** With a store client present,
  achievements and cloud sync may light up; absent, nothing changes and nothing is required.
  No store DRM wrapper, no store API in the boot path.
- **Updates are ours to ship**, which is why the launcher's staging, rollback and retention
  design is kept as a crate (AD-42) rather than deleted. On Linux, self-update is simpler
  than it was on Windows: a running executable can be replaced by `rename()` because the
  running inode survives, so the move-aside dance is unnecessary.
- **One ABI floor.** Build against the oldest glibc supported — or the Steam Linux Runtime,
  which the Deck build may target — and dynamically load only the Vulkan loader. That is the
  whole portability story.

**Consequences.** Steam remains a discovery and payment channel and a secondary build, and
the Steam build is the same binary plus dynamically loaded integration, so the two cannot
drift. Because the primary channel is ours, release engineering becomes an ongoing
first-class cost: signing, checksums, an update manifest and a rollback story are product
features now, not a store's free service. The Deck and Omarchy overlap is a bonus rather
than the plan: the runtime target is what covers the Deck, and Omarchy ships Steam, so both
channels are reachable from the same artifact.

**Rejected.** *Steam-first* — it is the promise this product is defined against. *Steam plus
a DRM-free tarball as a courtesy* — the courtesy is the product; it cannot be the
afterthought. *Mandatory online activation, or a periodic check* — it converts ownership
into a lease and buys nothing a single-player offline CRPG needs. *The store cloud as the
save location* — it makes the save unreadable without the store, and it fails the ownership
test: the folder must be complete on its own.

---

## AD-42 — The launcher is retired; its custody becomes crates, and authoring is headless-first

**Context.** The launcher exists for two reasons with two different fates. Its **transport**
half — an HTTP origin, static serving of a web game, a deterministic loopback port, a
session and CSRF gate, COOP/COEP for `SharedArrayBuffer`, browser detection and a minimum
browser version — exists because the game was a page in the player's browser. AD-38 removes
the page, and the transport half becomes dead machinery. Its **custody** half — the
data-directory contract, atomic saves with revisions and conflict handling (AD-11), mod
installation with the archive refusals, update staging/rollback/retention, the instance
lock, diagnostics — is about the player's files rather than about the browser, and AD-41
makes it *more* important, because with Steam secondary there is no store to do it.

**Decision.** **No launcher process.** The custody code moves into crates the game links,
behind an in-game "Updates & Mods" surface; the Go implementation it replaces was removed
with the launcher. The publisher toolchain returns as `kobra-pack` when there is a native
artifact to package: its web-era code — a Go tool that bundled the launcher into a browser
game folder — went with the launcher it depended on, and its native input is designed with
the native host. The *decision* to have a packager stands; the code does not. The web-only
machinery is deleted rather than kept: the port allocator and deny list, the session and
CSRF gates, the COEP/COOP configuration, browser detection and `min_browser_version`, the
`/editor` route, and the dev-only `--game-dir` escape with the diagnostics `dev` field that
exists to gate it (AD-35) — natively, a development affordance is a flag on a development
build, with no byte-identical page to disambiguate.

**Authoring is headless-first.** The interface is project files, a schema-driven validate
step, a Lua lint, and a packer, so the content pipeline is a command line that an agent and
CI can drive. The graphical editor becomes a **modder-facing tool built later**, and the M4
slice's loop — open a project, edit a record, validate, publish, install, play — survives as
that pipeline rather than as a DOM page.

**Consequences.** The launcher's retirement deletes the project's largest piece of machinery
and its largest support surface, and it removes the last reason for a second process between
the player and their files. AD-12, AD-23 and AD-35 are superseded; AD-13's archive format
survives (it is the mod format) with "built in the browser" replaced by "built by
`kobra-pack`". The editor's TypeScript host is dropped; its *ideas* — projects as versioned
documents, one validator, a publish that re-reads what it wrote — are the parts worth
re-implementing, in Rust, headless. The multi-game library the launcher could have become is
deferred: if Kobra ever becomes a platform, that shell is a mod manager and an updater, not
an HTTP host, and it will be built when there is a second game to justify it.

**Rejected.** *Keep the launcher for updates and mods* — a second process whose remaining
job is a screen the game can show itself. *Keep the launcher as the future multi-game
platform* — building the platform before the second game is the classic way to ship neither;
the toolchain and the folder contract are what a platform would need, and both survive.
*A WebView-based editor as the only authoring path* — it puts a browser back into the
toolchain the moment the runtime stopped needing one, and it makes the pipeline un-drivable
by CI. *Keep the editor as a second document at `/editor`* — there is no origin left to
serve it from.
