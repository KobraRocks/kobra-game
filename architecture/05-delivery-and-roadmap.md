# 05 — Delivery, toolchain, and roadmap

> **Partly superseded by AD-38 and AD-42.** The repository layout, the build
> pipeline, the packaging and launcher configuration, and the browser-era gates of
> §05.1–§05.4 describe the retired web toolchain, which the pivot's first change
> removed from the tree. The native pipeline replaces them and is not written yet.
> §05.5's milestones stand as *history* — M0 to the M4 slice were built — but read
> them as the history of the web implementation.

How Worldspiracy is built, packaged, gated, and sequenced. Every platform rule cited
here is verified in the [README](README.md#what-the-platform-gives-us-and-what-it-forbids);
the packaging rules come from `docs/authoring-a-game.md`.

## 05.1 Repository layout

`games/<name>/` is deliberately untracked by the parent repository (AGENTS.md), so
Worldspiracy is its own repository that happens to be checked out here so the
launcher, packager, and updater can be driven against it. It sits **one level deeper**
than `testgame/`, which changes three paths — the `REPO` line in the Makefile, and
`pkg.toml`'s `binary_dir` and `deny_list`.

```
games/worldspiracy/
├── architecture/            # this document set
├── specs/                   # the 4C System sources of truth (given)
├── src/
│   ├── core/                # Rust crate → game.wasm            (AD-1, AD-2)
│   │   ├── abi.rs           # the C ABI surface (§01.5)
│   │   ├── l0/              # ladder, Master Table, dice, colours (02:02.2)
│   │   ├── l1/              # traits, characters, powers, items
│   │   ├── l2/              # panels, attacks, damage, conditions
│   │   ├── l3/              # sectors, world, AI, schedules
│   │   ├── l4/              # quests, dialogue, factions, progression
│   │   ├── dsl/             # effect-DSL compiler                     (02:02.4)
│   │   ├── validate/        # the one validator                     (AD-15)
│   │   ├── tables/          # GENERATED: ladder + Master Table       (AD-14)
│   │   └── save/            # envelope + delta serialisation          (02:02.9)
│   └── web/                 # TypeScript → ES modules
│       ├── shell/           # boot, session, saves, settings, DOM UI
│       ├── worker/          # loop, ABI glue, script host
│       ├── renderer/        # webgpu/ + webgl2/ backends, vfs, audio  (01:01.3)
│       ├── editor/          # the shipped editor                       (04)
│       └── shared/          # types, protocol, i18n, accessibility
├── content/                 # AUTHORED content sources (JSON/CSV)     (03:03.3)
│   ├── packs/  locales/  shaders/{wgsl,glsl}/  textures/  models/  audio/  ui/
│   └── rules/               # ladders, origins, dice vocab; structural (AD-14)
├── tools/
│   ├── build-core.sh        # cargo + wasm-opt + hash
│   ├── build-web.mjs        # esbuild, deterministic
│   ├── gen-tables/          # source → crates/kobra-core/tables (AD-14); Python, see below
│   ├── gen-mod-api.mjs      # sdk/ from the live engine constants (05:05.2)
│   ├── validate.mjs         # CLI over wsp_load_content + wsp_check_ui (CI gate)
│   ├── pack.mjs             # a mod → the .tar.zst the launcher installs (§27.5)
│   └── package.mjs          # assemble game/ and run a packaging preflight
├── sdk/                     # GENERATED mod API: worldspiracy.d.ts, mod-api.json
├── game/                    # THE SHIPPED PACKAGE; generated in full — see the rule below
│   ├── index.html  shell.js
│   ├── engine/    { engine.manifest.json, game.wasm, *.js }
│   ├── assets/    (built from content/)
│   ├── locales/   (shell-only strings; AD-22)
│   └── editor/    (built from src/web/editor)
├── launcher/                { launcher.config.json, port-deny-list.json }
├── tests/                   { rust/, web/, replay/, fixtures/, perf/, mods/, smoke/ }
├── pkg.toml  Makefile  README.md  CHANGELOG.md
└── dist/                    # build output (git-ignored; games/ is ignored anyway)
```

Rules: **nothing under `game/` is hand-edited.** `make` reproduces it from `src/`
and `content/`, which is why every authored input lives in one of those two
directories: documents under `src/web/*/index.html`, the engine manifest template
at `src/engine.manifest.json`, shell strings at `src/web/shell/locales/`, and
everything the game loads under `content/` (copied to `game/assets/` with its
subtree intact). A preflight in `tools/package.mjs` asserts the packaging
invariants before `kobra-pack` ever runs: the `game/` root holds exactly
`index.html` and `shell.js`, every path segment matches `[A-Za-z0-9._-]+`, every
path named by `engine.manifest.json` exists, `assets/` is not empty, and no build
placeholder survives into the package.

Two smaller deviations from the sketch above, both to keep that rule true rather
than to add anything: `tools/gen-tables` is Python because the authoritative
property test lives in `specs/verify_master_tables.py` and the generator imports
it rather than reimplementing V1–V7 in a second language (AD-15's argument,
applied to the table), and `src/engine.manifest.json` is a template whose
`wasm_hash`/`glue_hash` placeholders are filled by `tools/package.mjs` — the
manifest carries hashes of build outputs, so it cannot be an input *and* an
artifact.

## 05.2 Build pipeline

| Step | Tool | Output | Notes |
|---|---|---|---|
| 1 | `tools/gen-tables` | `crates/kobra-core/tables/*.rs` | Generated from the authoritative Master Table source and the ladder definitions (AD-14). Fails if the source does not satisfy the property tests. |
| 2 | `cargo build --target wasm32-unknown-unknown --release` | `game.wasm` (unoptimised) | `crate-type = ["cdylib"]`, `-C target-feature=+simd128,+bulk-memory`, `panic=abort`, `opt-level=z` |
| 3 | `wasm-opt -Oz --enable-simd` | `game/engine/game.wasm` | **Binaryen 132, pinned** — the optimiser rewrites the module, so a different version is a different artifact. `tools/build-core.sh` reports a mismatch loudly and builds anyway (a developer with a distro `wasm-opt` can still play); only a release needs the pin. The upstream release asset is `binaryen-version_132-<platform>.tar.gz`. Size is a CI regression gate |
| 4 | `wasm-tools` / `sha256sum` | `wasm_hash` for the manifest | The manifest carries `wasm_hash` and `glue_hash` |
| 5 | `tools/build-web.mjs` (esbuild) | `game/engine/*.js` (two workers), `shell.js`, `game/editor/*` | ES modules, no bundler runtime, deterministic chunk names; the play and editor graphs are separate (`AD-12`) |
| 6 | content copy + JSON minify | `game/assets/**` | Authored JSON stays readable in `content/`; shipped copies are minified but never renamed |
| 6b | `blender -b -P tools/blender/exporter.py` + KTX-Software | normalised `.glb`, derived LODs, KTX2 textures, thumbnails, contract report | **Blender 5.2 LTS and KTX-Software are pinned tools** (`AD-32`, `08:08.11`). The step *repairs* a delivery as much as it judges one, and its report is the same validator the runtime uses |
| 7 | `tools/package.mjs` | `game/` complete | Runs the preflight invariants |
| 8 | `kobra-pack check/build/verify` | `dist/*.zip`, `*.tar.zst` | The packager is the authority on packaging, not us |

**Reproducibility is a requirement, not a goal**: the packaging checklist says two
builds must match byte for byte when compared by `SHA256SUMS`. That constrains the
build: pin **every** toolchain that touches an artifact — Rust 1.98.0
(`rust-toolchain.toml`), **Binaryen 132** (`tools/build-core.sh`), esbuild and
TypeScript (`package-lock.json`), **Blender 5.2 LTS** and **KTX-Software**; pass
`--remap-path-prefix` so no absolute path
lands in the wasm; embed no timestamps (including glTF generator strings and Blender scene
metadata, which is a real source of drift in art exports); sort inputs; and keep the
bundler's module order deterministic. A build that embeds `Date.now()` or a Blender build
hash anywhere is a defect.

## 05.3 The package

`pkg.toml` (paths adjusted for the one-level-deeper location):

```toml
[package]
slug        = "worldspiracy"
game_id     = "com.kobragames.worldspiracy"
game_name   = "Worldspiracy"
pkgroot     = "."

[release]
release          = "2026.10.1"
game_version     = "0.1.0"
engine_version   = "0.2.0"
save_version     = 2
launcher_min     = "0.1.0"
channel          = "stable"
scope            = "full"
previous_release = ""

[launcher]
config         = "launcher/launcher.config.json"
deny_list      = "../../launcher/port-deny-list.json"
binary_dir     = "../../launcher/dist"
version_source = "binary"

[platforms.linux-x64]
goos = "linux"; goarch = "amd64"; binary = "launcher"; archive_platform = "linux-x64"

[platforms.win64]
goos = "windows"; goarch = "amd64"; binary = "launcher.exe"; archive_platform = "win64"

[publish]
base    = "https://updates.example.com/worldspiracy"
gpg_key = ""
```

`launcher/launcher.config.json`:

```json
{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.kobragames.worldspiracy",
  "game_name": "Worldspiracy",
  "release": "2026.10.1",
  "port": { "base": 18950, "span": 50, "require_confirmation": false },
  "server": {
    "idle_timeout_seconds": 90,
    "serve_mods": true,
    "cross_origin_embedder_policy": "require-corp",
    "entry_path": "/index.html"
  },
  "browser_preference": ["chromium", "chrome", "brave", "msedge", "opera"],
  "min_browser_version": { "chrome": 113, "edge": 113, "opera": 99, "brave": "1.52" },
  "mime_types": {
    ".wgsl": "text/plain",
    ".gltf": "model/gltf+json",
    ".glb": "model/gltf-binary",
    ".ktx2": "image/ktx2",
    ".csv": "text/csv"
  },
  "locales": ["en"],
  "update": { "channel": "manual" }
}
```

Decisions encoded there, each with a reason:

- **`base: 18950`** — a distinct window from `testgame`'s `18900`, so both can be
  installed and run without colliding. Ports are machine-local state, persisted
  outside the game folder (FR-PORT).
- **`serve_mods: true` is load-bearing and must not be turned off.** With it false
  there is no route at all for mod content, because the launcher implements no
  `/api/data/mods/*` route (AD-10).
- **`cross_origin_embedder_policy: "require-corp"` is the load-bearing performance
  setting.** With COOP `same-origin` (always sent) it makes the origin cross-origin
  isolated, which is what `SharedArrayBuffer`, wasm threads and the shared frame ring
  require (FR-SRV-16, `07:07.2`). Omit it and the engine refuses to start rather than
  silently running single-threaded (FR-SRV-16a).
- **`entry_path: "/index.html"`** is what the launcher opens and what `--print-url`
  prints. A modder's build sets it to `"/editor"` to boot straight into the editor
  (FR-LNCH-9, AD-23).
- **`browser_preference` lists the Chromium family only** — five entries for the same
  engine, so a player with any of them gets the game. `firefox` is deliberately absent
  because a WebGPU-only build cannot run on it (AD-17); the launcher's default list
  omits `chromium`, which Linux players need.
- **`mime_types` is additive** (the config merges over the built-in defaults), so
  `.wasm` stays `application/wasm` and must never be re-declared with anything else
  (`instantiateStreaming` rejects a mismatch — §01.8).
- **`update.channel: "manual"` until a publish URL exists.** `manual` makes
  `/api/update/check` answer "No update server is configured", and `/api/update/apply`
  refuse with `manual_channel`. Flip to `"patch"` with `patch_base_url` when there is
  a server, and the updater takes over from there (the page never does networking —
  AD-19).

`game/engine/engine.manifest.json`:

```json
{
  "schema": "kobra.engine-manifest/1",
  "release": "2026.10.1",
  "engine_version": "0.2.0",
  "wasm": "engine/game.wasm",
  "glue": "engine/worker.js",
  "wasm_hash": "sha256:…",
  "glue_hash": "sha256:…",
  "save_version": 2,
  "required_features": ["wasm", "simd", "bulk-memory", "threads", "shared-array-buffer", "webgpu"],
  "min_browser": { "chrome": 113, "edge": 113, "opera": 99 },
  "memory": { "initial_bytes": 268435456, "maximum_bytes": 1073741824 },
  "notes": "WebGPU and cross-origin isolation are required (AD-3, AD-5, AD-17)."
}
```

`engine_version` and `save_version` must equal `pkg.toml`'s, and `launcher_min` must
not exceed the packaged launcher's version (packaging rules 6). Version fields are
bare `X.Y.Z` — no prerelease suffixes (`VERSIONING.md`, AGENTS.md trap 10).

## 05.4 Testing and gates

The gates are the architecture's enforcement. Each row says what it proves, not just
what it runs.

| Gate | Proves | Where |
|---|---|---|
| **Master Table property tests** | The transcribed table is monotone, covers `00..99` exactly once per row, and matches the golden snapshot. Includes the source-verified property that rank 1000 still fails on `00-04` (the irreducible 5% band). | `tests/rust/tables` |
| **Roll conformance** | Every table in the 4C spec resolves to the documented outcome, generated from the spec's own tables — `00` is 0, not 100. | `tests/rust/rules` |
| **DSL coverage** | All 51 canonical powers are expressible as DSL templates and behave identically to their Rust kernels. This is the proof that the modding surface is as powerful as the shipped rules. **Landed at M2**: the templates ship in `core-powers.pack.json`; the oracle compares every hook over a probe matrix. | `crates/kobra-core/tests/dsl.rs` |
| **Determinism / replay** | Seed + command log → identical state hash after every tick, across runs and platforms. The oracle is `wsp_hash_state` read from the render packet header. | `tests/replay/` |
| **Save round-trip and migration** | Save → load → save is byte-stable; each `save_version` step has a fixture; `content_hash` mismatch is detected and reported, never silently loaded. | `tests/replay/saves` |
| **Content validation parity** | The editor's publish check and the runtime's load check return the same verdict for every fixture, including failures. | `tests/rust/validate` |
| **Mod SDK currency** | `sdk/mod-api.json` and `sdk/worldspiracy.d.ts` equal what `tools/gen-mod-api.mjs` produces from the live constants, the generated types **compile**, and the record-type list covers everything the shipped content uses. A vocabulary rename in the engine is a *stale* SDK rather than a wrong one (`AD-15`). | `make sdk`, `tests/web/mod-api.test.mjs` |
| **Mod overlay** | A fixture mod is discovered by its index, overrides exactly what it declares, adds records, and is disabled cleanly when it does not validate. `tools/validate.mjs` runs the same pass the game and the editor run, and enumerates the fixture's `assets/` tree — which a browser cannot do — so `undeclared_override` and `missing_asset` fail the gate instead of being silent (`03:03.9` rule 7). `tools/pack.mjs` then writes the archive §27.5 fixes and `make mods` reads it back through the launcher's own `extractTarZstd`, so the writer and the reader agree by test (`04:04.7`). Since the M4 slice the same reader is handed the **editor's** archive (`tests/web/editor-archive.mjs`), and `tests/web/editor.test.mjs` proves the editor's bytes equal the CLI's, so browser publishing is gated on the same contract. | `make mods`, `tools/validate.mjs`, `tools/pack.mjs`, `tests/web/editor.test.mjs`, `tests/mods/` |
| **Request volume** | Boot root-request count is *measured and reported*, and does not grow with mod count beyond `1 + N + M + S` (the boot document, one index probe per enabled mod, the content packs, and one fetch per Tier-2 script, which is what the hash `AD-8` records is computed from). The ≤ 150 target is **not gated** — boot time is deferred (`AD-25`), but the counter ships from day one so the number is never a surprise. | `tests/perf/boot-requests` |
| **Package gates** | `kobra-pack check` on every platform; `kobra-pack verify dist` on the built output; two builds compare equal by `SHA256SUMS`. | `make package verify` |
| **Served contract** | The packaged launcher serves everything the shell depends on: the isolation headers, every route, the MIME types (`.wasm` above all), the session handshake, and `/editor` as a second document. Headless by design — rendering needs a WebGPU browser and is M0's exit check, not a CI gate. | `tests/smoke/run.sh` |
| **Action availability** | The engine, not the shell, decides which contextual actions exist and whether each is legal: Wait is withheld in a fight, Fight mid-conversation, Talk with nobody in reach, Spend Fortune with nothing to spend it on — and a conversation can invite a fight the plain action bar cannot. The same answers are checked on the host library and through the packaged wasm, so the interface cannot drift from the engine (`AD-15`). | `crates/kobra-core/tests/replay.rs`, `tests/web/actions.test.mjs` |
| **Dev-mode gate** | A development build reports itself in diagnostics and a release build cannot: the payload carries `dev` only from a `kobra_dev` binary, and the shell offers its development sidebar from that field alone. | `launcher/internal/server/dev_diagnostics_test.go`, `tests/web/shell-flow.test.mjs` |
| **Moddable UI** | A mod's entry mounts from its declared layout and renderer; the layout carries no expressions and no English; a renderer receives only its subscribed projections and can send only its declared commands; a broken pack disables only itself; and a UI pack is save-neutral. | `tests/web/ui-*.test.mjs`, `crates/kobra-core/tests/replay.rs` |
| **Asset contract** | Every shipped model, rig, clip and texture satisfies `08-asset-pipeline.md`: scale, bone names, anchors, clip set, texture rules, tier budgets. The headless Blender compiler normalises then reports; a contract error fails the build, so a contractor's mistake cannot reach a release. | `make assets` |
| **End-to-end** | package → install → run → save → update, driven the way `testgame` drives it. | `make e2e` |
| **Performance budgets** | The FS §17.1/17.2/17.3 numbers, measured on the reference environment, especially WASM compile time, engine memory, and autosave frame impact. | `tests/perf` |
| **Accessibility** | The game and editor UIs pass the same automated audit; keyboard-only operation of both. | `make a11y` |

**Where the gates actually live.** The table names the intent; the files are the
unit tests under `crates/kobra-core/src/**`, the integration suites —
`crates/kobra-core/tests/tables.rs` (the generated ladder), `tests/packet.rs` (the wire
format), `tests/content.rs` (the shipped packs must pass their own validator),
`tests/dsl.rs` (the DSL-coverage oracle), `tests/scripts.rs` (the Tier-2 host) and
`tests/replay.rs` (both golden replays, host side) — and `tests/web/*.test.mjs`
(the ABI, the ring across two threads, the VFS overlay, worker recovery, the script
dispatcher, the available-action set, and **both** golden replays through the packaged
`game.wasm`).
`tests/mods/noir-overhaul/` is the M1 data mod; `tests/perf/` is the request-volume
and budget harness.

**The M3 gates are the two fixtures and the round trip.** `tests/replay.rs` carries
`the_campaign_loop_matches_the_committed_golden` (one hash per command),
`the_campaign_loop_reaches_every_m3_system` (the loop actually reached exploration,
dialogue, the journal, standing and a fight — a hash comparison alone would pass on
a deterministic no-op) and `a_campaign_save_taken_mid_loop_reloads_to_the_same_hash`
(a generated world, its per-sector delta, the clock, the standing and the journal
all round-trip). `tests/web/replay.test.mjs` asserts the same campaign golden
through the packaged wasm. The **mod overlay** row now has its validator gate —
`make mods` drives `tools/validate.mjs` over every fixture and `tests/web/mods.test.mjs`
proves the `undeclared_override` error fires — and one record type can now be
**authored** without a terminal: the M4 walking skeleton's editor publishes an `item`
mod through the shared archive writer (§05.5). Authoring a *second* mod end to end,
and every other record type, is still M4/M6, and `tests/mods/` holds one data mod.

**The M3.5 gate is the action set, on both sides of the ABI.** `crates/kobra-core/tests/replay.rs`
carries a test named for each rule — `talk_is_not_offered_with_nobody_to_talk_to`,
`wait_is_not_offered_in_an_encounter`, `a_conversation_withholds_the_exploration_actions`,
`a_conversation_is_modal`, `a_conversation_can_invite_a_fight`,
`a_merchant_counter_is_offered_beside_its_keeper` and
`a_world_with_a_self_dialogue_offers_talk_when_alone` — and `tests/web/actions.test.mjs`
asserts the same answers through the packaged wasm, because the projection crossing the
ABI is what the page actually reads. The campaign golden was **regenerated** for M3.5: the
merchant is a fourth station, so `hash_state` legitimately moved while the command list did
not, and the M1 golden is untouched. A test named for a rule is the point — a projection
that is merely plausible is the failure mode `02:02.11` warns about.

**The M3.6 gates are the four `ui-*` suites plus the signal and save-neutrality tests.**
`tests/web/ui-layout.test.mjs` renders every vocabulary key and disables a bad node with a
reason; `tests/web/ui-subscription.test.mjs` proves only changed projections are sent and
only their entries repaint; `tests/web/ui-scope.test.mjs` refuses a read-only entry's
`post`; `tests/web/ui-failsoft.test.mjs` disables a broken pack alone, including the worked
example in `tests/mods/noir-overhaul/`; `tests/web/shell-flow.test.mjs` mounts the shipped
panels through the same registry and capability object a mod uses, which is what keeps the
public contract exercised by every build. `crates/kobra-core/tests/replay.rs` carries the signal
tests and `a_save_taken_with_a_ui_pack_loads_without_it`.

Commands, mirroring the parent repo's conventions:

```sh
make -C src/core test            # Rust unit + property + replay tests
make dev                         # serve the source tree via kobra-pack dev
make check                       # preflight + kobra-pack check (no artifacts)
make package                     # build + package + install into dist/
make verify                      # kobra-pack verify dist + reproducibility compare
make smoke                       # drive the served contract headlessly
make e2e                         # full package → install → run → update drive
make perf                        # budgets, on the reference machine
make mods                        # mod fixtures + overlay round-trip
```

A rules bug fix adds a **replay fixture** (`AD-21`), not just a unit test: the
fixture is a seed, a command log, and the expected state hash, so it fails on the old
build and protects every future refactor.

## 05.5 Roadmap

The order is chosen so that the riskiest assumptions are tested earliest, and so that
there is a playable build before there is a big one.

**The demo-first strategy reorders this** (`AD-31`). The studio intends to ship a demo
early to fund 3D artists from pre-sales, which puts two things ahead of schedule that were
previously late: the **asset pipeline contract** (`08-asset-pipeline.md`) and its
validator, because contractors cannot start without them; and the **systems and content**
rather than the art, because the demo's visuals are placeholder by design and swap in as a
presentation pack (`AD-29`). Concretely, the demo is M0 + M1 + M2 + a content slice, with
`08` finished before the first hire — while M3's campaign breadth and M5's content volume
wait for the funded artists.

### M0 — Walking skeleton

Repo skeleton; generated tables for one ladder (validated by
`specs/verify_master_tables.py`); empty wasm that exports `wsp_abi_version`; the shell
boots, exchanges a session, reads `/api/state`, verifies `crossOriginIsolated`, starts
both workers, and draws a solid rectangle from a shared-memory packet; `/editor`
serves a stub page; `make check`, `make package`, `make verify` green in CI.

*Exit:* `kobra-pack dev` serves a folder that renders in a WebGPU-capable Chromium,
opens `/editor` as a second document, and reports the thread count, the backend and
the isolation state in diagnostics. *De-risks:* the isolation configuration, the
two-worker topology, the shared ring, the new launcher routes, and the packaging loop
— all on day one, with trivial content.

### M1 — Vertical slice

Transcribe the Master Table from the authoritative source with the property tests
(R1); implement L0 and the resolution primitive; one character from each creation
mode; one small map (a handful of 1 m tiles, `AD-33`); one encounter with
melee/ranged/dodge/damage/armour/conditions;
panel-based turn order; save and load; the deterministic replay harness; **one data
mod** that adds an item and overrides a texture, loaded through the VFS.

*Exit:* a scripted encounter resolves identically on two machines, and a mod changes
it. *This is the milestone that proves the architecture*, because it exercises every
layer at once — WASM determinism, the packet, the VFS, the overlay map, the
validator, and the save envelope.

**Built.** The slice landed as described. The exit check is
`tests/replay/alley-encounter.json`, a committed golden that pins the state hash
after every command; it has **two independent consumers** —
`crates/kobra-core/tests/replay.rs` (the Rust library on the host) and
`tests/web/replay.test.mjs` (the packaged wasm) — and a third run with the M1 mod
merged must differ, which is the modding half of the criterion as an assertion
rather than a claim. Deviations from the plan, each carried in `README.md`'s
known gaps: only the Basic ladder is compiled; origins, powers, the effect DSL and
vehicles are M2; AI is M3; the world is one authored map, so a save's `world` half
is the entity placements and the encounter clock until the generator lands.

### M2 — Engine completeness

Powers as kernels plus the DSL; the DSL-coverage test over all 51 powers; vehicles;
the full condition set; Tier-2 script host with the `worldspiracy.mod/v1` API and its
determinism contract; worker crash recovery; the request-budget and performance
harnesses.

*Exit:* every power in the spec is implemented and testable, and a script mod can add
one the DSL cannot express.

### M3 — The CRPG frame

Real-time exploration with the panel clock; party; inventory and economy; dialogue;
quests; journal; factions and standing; progression; time of day and schedules; the
procedural-baseline-plus-delta world with its generator version.

*Exit:* a player can create a character, explore, talk, fight, level, and save —
a complete loop with placeholder content.

**Built.** The frame landed as described, and the exit criterion is a committed
golden rather than a claim: `tests/replay/campaign-loop.commands.json` is one
command stream that generates a world, rolls a character, walks, talks through a
skill check, advances the clock, picks up a quest item, spends Fortune on a trait,
buys and wears armour, and fights — with the state hash pinned after every command.
It has the same two independent consumers as M1's fixture
(`crates/kobra-core/tests/replay.rs` and `tests/web/replay.test.mjs`), and three further
tests make the *loop* rather than the hashes the assertion: one walks the state after
the stream and asserts every system was reached, and one saves mid-loop and requires
the reload to hash identically.

| M3 system | Where it lives, and what it is made of |
|---|---|
| **The panel clock** | `l3::clock`. One unit, two drivers (`02:02.5`): the exploration clock and the combat panel are the same quantity, so a three-panel buff decays while the player walks. A day is `rules.time.panels_per_day` (1440 by default), and `DaySlot` is the quarter of it a schedule or a condition names. |
| **Schedules and stations** | `l3::clock::Station` plus `l3::ai`. A station holds a post, walks to the post its schedule names for this hour, and behaves from a `BehaviourRecord`. `advance_stations` returns **decisions**, never mutations: the AI proposes a fight and the command stream disposes, which is what keeps one resolution path (`02:02.6`). |
| **The procedural baseline plus delta** | `l3::gen`. `Baseline::generate` is a pure function of `(seed, width, height, densities)` over integer value noise, so "same seed, same world" holds on every machine; an authored `sector_map` is its own baseline, and a world record's generator has its authored overlays baked in. The save stores only `Baseline::delta` — the sectors that differ — and `GENERATOR_VERSION` is recorded so a mismatch is a **refusal with an explanation** rather than a mis-placed world. |
| **Party** | `sim.party`, and the `add_party`/`remove_party` commands. A station marked `party` joins at world generation. |
| **Inventory and economy** | `l4::economy` and `l1::items`. Items gained `slot`, `price` and `kind` (`AD-29` rule 3); prices scale by the buyer's Lifestyle band (`4c:1255-1272`), a shop sells only its stock and buys only the kinds it names, and a character who cannot afford something is refused rather than given a partial purchase. |
| **Dialogue** | `l4::dialogue`. A node graph with conditions, options, skill checks and effects. A check resolves through the **ordinary resolution primitive**, so a conversation cannot disagree with an attack about what a colour means; an option gated by `when` is withheld, and one gated by a check is shown with the colour it needs (`02:02.11` rule 2). |
| **Quests and the journal** | `l4::quests` and `l4::journal`. `Journal::plan` evaluates the graph against a **frozen** journal and `apply_plan` writes it — a two-phase split the borrow checker enforces, so a condition cannot read this pass's writes. A step the author wants the player to close (`auto_advance: false`) becomes a **pause**, which is the only manual step in the runtime. |
| **Factions and standing** | `l4::factions`. Faction standing is L4 state and Repute is the L1 trait, exactly as `02:02.6` requires; the public-reaction table is reused with a per-faction `reaction_polarity` flag for `4c:1292`'s criminal reversal, and the roll resolves at the Repute score treated as a Rank Value (`D4`). |
| **Progression** | `l4::progression`. `+1 RV costs the current value`, a new power 1000, a new skill 250 (`4c:1381-1391`), all content; plus the Fortune/Repute impact scale of `4c:1229-1251` and `D20`. Advancement preserves the *unspent* Fortune across the recompute it triggers. |

Deviations from the plan, each carried in `README.md`'s known gaps: the generator
is deliberately simple and legible placeholder terrain rather than a biome system;
AI is a station/schedule/behaviour model rather than a full utility scorer; and
quests are content-authored graphs rather than editor-authored ones, which is M4.

### M3.5 — Interface and dev mode

The play page becomes an interface rather than a control panel, *before* M4 builds an
editor on top of it. The engine gains the available-action set (`02:02.13`): which
contextual actions exist, and whether each is legal, is a rule, so it is answered by
`wsp_actions` (ABI 5) and rendered — never re-derived in TypeScript. The shell gains a
full-tab canvas (not fullscreen), bottom-left Journal/Character/Shop sheets, a
bottom-centre contextual action bar, a keyboard model, and — in development builds only —
a diagnostics sidebar.

*Exit:* `make play` opens a canvas that fills the tab; **Talk**, **Wait an hour**,
**Fight** and **Spend Fortune** are offered or withheld by context (Wait in a fight,
Fight mid-conversation, Talk with nobody in reach, Spend Fortune with nothing to spend it
on); the panels open from their icons and pause the table; and the Dev sidebar is present
in a `make play` run and **absent** from a packaged one.

**Built.** The interface landed as described. Three things make the exit criterion a
property rather than a claim:

- **The engine owns the options.** `wsp_actions` reports every action with an `enabled`
  flag and a `reason_key`, and the shell renders that answer. A disabled action stays
  visible with its reason, so a rule the player cannot use is a rule they can *read*
  (`02:02.11` rule 2).
- **The demo content reaches every option.** The world gained a merchant who keeps
  `wsp.shop.surplus`, a counter conversation with two options that invite a fight, and an
  authored self-dialogue, so Talk, Shop, Spend Fortune and the invited fight are all
  reachable in a `make play` run. A counter is a place: `wsp_shops` reports the merchants
  in reach, not every shop in content.
- **Dev mode is the launcher's answer.** `make play` and `make package` serve the same
  `game/` tree, so the page cannot decide for itself; a `kobra_dev` launcher reports
  `dev: true` in `GET /__kobra/diagnostics` (`Launcher-spec.md` §21.4) and a release
  binary has no code path that can. The Dev icon is offered from that field alone.

Deviations from the plan, each carried in `README.md`'s known gaps: the sidebar is
diagnostics and the log, not a cheat console (there are no dev-only commands); the
generated terrain still has no pathfinder, so a wild fight can start across cover the
party cannot cross; and the presentation is CSS and the existing quad renderer — portraits,
item art and animation are M5's, behind the asset contract of `08`.

### M3.6 — Moddable UI and UI packs

The interface becomes a public surface (`09`). Projections gain **subscription** and
**revisions**; the engine gains **signals** (named conditions it evaluates, so a layout
never evaluates one itself); the shell gains a **registry**, a **layout binder**, a
**renderer contract** with a capability object, and a **command scope** per entry. A mod
ships a layout (data), a renderer (an ES module), or a theme (CSS custom properties), and
the shipped panels are rebuilt as the first UI pack so the public contract is exercised by
every build (`AD-36`, `AD-37`).

*Exit:* a mod in `data/mods/` replaces the character sheet and adds a HUD region without
touching a shipped file; its layout carries no English and no expression; its renderer
receives only its subscribed projections and can send only the commands it declared; a
save taken with it loads without it; and a broken pack disables only itself.

**Built.** The normative design is `09` and the decision records `AD-36`/`AD-37`, and the
gates are named in §05.4 and `09:09.11`. Every row of that table is green:
`tests/web/ui-layout.test.mjs` (the binder), `tests/web/ui-subscription.test.mjs`
(subscription and revisions), `tests/web/ui-scope.test.mjs` (the capability object and the
command scope), `tests/web/ui-failsoft.test.mjs` (a broken pack disables only itself), the
signal tests in `crates/kobra-core/tests/replay.rs` and `tests/web/actions.test.mjs`, the
save-neutrality test in `crates/kobra-core/tests/replay.rs`, and the dogfood test in
`tests/web/shell-flow.test.mjs` that mounts the shipped panels through the same registry a
mod uses. `tests/mods/noir-overhaul/` is the worked example: it replaces the character
sheet, adds a right-hand journal feed, ships a theme and declares a signal, and
`docs/creating-a-ui.md` is the author's guide for both a person and an AI agent.

Three decisions differ from the sketch, each for a stated reason:

- **The base panels are renderer entries, not layouts.** The action bar, the sheets and the
  counter need parameterised commands and the key hints, which the layout vocabulary cannot
  express (`09:09.5`), so they register as bundled renderers — through the same registry and
  the same `UiContext`, which is what the dogfood claim requires. A layout can name the
  action set (`from: "actions.talk"`) and gate on a signal, and a renderer is the documented
  answer for anything more.
- **`text-key` may be a `{path}` template.** A projection carries **string ids** (`AD-22`),
  so a list of records that each carry a `text_key` cannot be rendered by a static key. A
  `text-key` of `"{c.name_key}"` resolves first and looks the result up; a bare
  `journal.rumour` is still a string id, not a path.
- **An entry may read more than its one projection.** A layout that binds an action or gates
  on a signal additionally subscribes to `actions` and `signals`; the data paths (`text`,
  `each`) must still stay within the declared projection.

The risks this milestone accepts, stated rather than discovered: a renderer runs
same-origin JavaScript **on the page**, so it can read the DOM and call the launcher's data
API with the session cookie — the user-at-their-own-risk surface `03:03.10` already accepts
for worker scripts, now with DOM access. The mitigations are the capability object, a
command scope that defaults to read-only, per-entry isolation, and a mod manager that says
what a pack declares. The theming contract (custom properties plus z-index bands) and the
keyboard arbitration (`ctx.onKey`, conflict-reported) are part of the exit criterion,
because both are cheap now and unfixable once packs exist.

### M4 — Editor v1

Projects as save slots; the content library; powers/effects, items, characters,
sectors, encounters; **the per-sector heightmap and blocking-height layer with a
line-of-sight overlay** (`02:02.12`), because elevation is mechanical and level design
cannot be done blind to it; the visual descriptor and attachment-slot editor with the
project's pack `kind` (`AD-29`); validation surfaced; scratch-sim playtest with named
modifiers; publish (tar + zstd + install + verify); mod manager that distinguishes
presentation packs from content packs.

*Exit:* the M1 test mod is authored from scratch in the editor by someone who did not
write the engine, and a designer can author a two-level encounter where high ground
matters without editing JSON by hand.

### M4 slice — the editor walking skeleton

The full milestone above is the largest remaining pre-content milestone, and most of
its cost serves our own content authors rather than the modder on-ramp M4 exists to
create. So it is entered through **one vertical slice that closes the loop** —
`open a project → edit one record type → validate → publish → install → play` —
because the loop is the thing that produces the demand signal, and one record type is
enough to exercise the whole spine: the project store, the pack builder, the archive
writer, the validator, the installer, the verifier, and the mod manager's entry.

*Exit:* an item authored in the editor, by someone who did not write the engine,
validates with the same verdict the runtime reaches, publishes to an archive the
launcher's own extractor installs, and is playable from the editor's Play button —
with no terminal involved.

**Built.** The slice landed as described. What makes the criterion a property rather
than a claim:

- **The editor calls the validator, and the parity is a gate.**
  `src/web/editor/validation.ts` builds the same `worldspiracy.content-load/1`
  document the loader builds and hands it to `wsp_validate_content` through the same
  `Core` glue the sim worker uses. `tests/web/editor.test.mjs` asserts the in-process
  verdict and `tools/validate.mjs`'s verdict are equal for both a passing and a
  failing project (the failing one is a closed-vocabulary violation the engine must
  refuse), which is `AD-15` enforced rather than restated.
- **One archive writer.** The `ustar` headers, the entry order, the name rules and
  the zstd frame moved into `src/web/shared/tar.ts` and `src/web/shared/zstd.ts`
  *before* the editor was built, and `tools/pack.mjs` now imports them too. The
  byte-identity test packs one tree through both paths and requires equal bytes, and
  `make mods` hands the **editor's** archive to
  `TestExtractModArchiveFromTheGameCLI`, so the browser's bytes are read by the
  launcher's real `extractTarZstd`. The browser zstd path is settled in `AD-13`'s
  amendment: a shared, dependency-free frame writer, not a third-party wasm.
- **Publish refuses what cannot ship, and proves what did.**
  `src/web/editor/publish.ts` validates first, applies `04:04.7`'s size guard
  (warn 32 MiB, refuse ~45 MiB), installs, and then re-reads the index through
  `/mods/<id>/assets/…` — so a `serve_mods: false` configuration is reported by name
  instead of looking like a mod that did nothing.
- **The page loop is a gate, not a claim.** `tests/web/editor-flow.test.mjs` boots
  `src/web/editor/index.ts` against a fake document, a stubbed launcher surface and
  the real `game.wasm`, then clicks add → edit → save → validate → publish and
  requires the launcher-visible effects. That is the exit criterion's "someone who did
  not write the engine" made mechanical — the document-level tests all pass with a
  renamed button.
- **The project is the pack.** `src/web/editor/project.ts` owns the
  `worldspiracy.editor-project/1` document and emits `mod.manifest.json`, the mod
  index and the pack; record ids are generated through the namespace rule the
  validator enforces (`03:03.5`). Projects are `edit.` save slots and inherit the
  launcher's revision conflict handling (`04:04.5`).

Deviations from the plan, each carried in `README.md`'s known gaps and `04`'s "as
built" notes: one record type (`item`) and one pack per project, with `modifiers`
still authored as JSON and no content library, scratch sim, 3D viewport or heightmap
layer; the `hashes` on a publish summary are the archive's SHA-256, not per-script
hashes, because the slice publishes no scripts; and the accessibility audit for the
editor's workspaces is owed, not done.

### M5 — Content

The campaign: sectors, NPCs, quests, dialogue, items, art, audio, balance. The
longest phase, and the one that benefits from M4 existing.

### M6 — Modding and editor v1.1

Quest and dialogue editors; localization; balance views; templates and archetypes;
the modding guide plus a worked example mod of each tier; upstream requests U1–U3
revisited now that the author loop is exercised.

*Exit:* an external modder ships a content mod using only the shipped editor and the
shipped docs.

## 05.6 Upstream launcher requests

Summarised from `03:03.12`; each is an improvement, none is a dependency:

| # | Request | Would replace |
|---|---|---|
| U1 | `GET /api/data/mods/{id}/manifest`, session-authed (FR-AST-11 promises `/api/data/mods/*`; no such route exists) | the conventional `assets/data/mod.json` index (AD-9) |
| U2 | Publish the mod archive layout schema (FS §27.5 defers it) | guessing `extractTarZstd`'s expectations |
| U3 | Accept a `.zip` mod archive, or a loose-file install | shipping a zstd encoder in the editor (AD-13). **Partly answered by the M4 slice**: the editor publishes through a shared, dependency-free zstd frame writer rather than a third-party wasm, so nothing is blocked — the request stays open for the real-encoder swap, and the number that would justify it is content scale (AD-13's amendment, `03:03.12`) |
| U4 | **Landed.** `server.cross_origin_embedder_policy` is implemented (FR-SRV-16) | the single-threaded decision — threads and the shared frame ring are now available (AD-5) |
| U5 | Implement FR-AST-10 bundling | the game-side content pak and its `Range` reader (`07:07.7`) |
| U7 | **Landed.** The `/editor` route and `server.entry_path` (FR-AST-16, FR-LNCH-9) | a separate editor origin or an editor that is a mode of the game page only (AD-23) |
| U8 | A way to pass browser flags to the browser the launcher opens (a `browser_args` list), so a game that needs Vulkan-based WebGPU can ask for it | the player editing their browser's own flag file, which is what makes the packaged game refuse on a distro Chromium that ships with Vulkan disabled |

## 05.7 Risks

| # | Risk | Impact | Mitigation |
|---|---|---|---|
| R1 | Master Table transcription | Wrong rules everywhere, invisibly | **Source found**: the Libre Edition *Master Tables* document encodes colours twice (cell text and cell shading), so it self-validates. Transcribe from it, cross-check against the CSV (which is expected to disagree in 22 cells), property-test, golden-snapshot. M1 gate. |
| R2 | Mod scripts vs. data-only | Either a capped modding surface or a support burden | Ship both tiers, version the script API, record script hashes in the save (AD-8). Decide finally at M2. |
| R3 | Browser-side tar+zstd for publishing | Publishing blocked | Fallback that always works: export the pack and publish by hand. Decide at M4. |
| R4 | ~~WASM threads wanted later~~ | **Resolved.** COEP is implemented and the shipped config sets `require-corp`; the core is multithreaded (AD-5, §07.4). The residual risk is a publisher misconfiguring it, which the boot check catches (FR-SRV-16a). |
| R5 | **WebGPU-only excludes Firefox and Safari** | Lost audience if the requirement is not stated up front | State it in E1, the launcher config's `min_browser_version`, and the store page; keep the backend behind the `RenderPacket` contract so widening support later is a config change plus a backend (AD-17). The same statement has to carry the *Linux* caveat: WebGPU is Dawn over Vulkan, distro Chromium builds disable Vulkan by default, and the launcher cannot pass the flag that turns it on (U8) — so the requirement is "a WebGPU-capable Chromium **with a GPU adapter it can actually reach**, which on Linux may mean enabling Vulkan". |
| R6 | Save size vs. 64 MiB cap and 8-revision retention | Save failures, disk bloat | 512 KiB target / 4 MiB ceiling with a test; world stored as a delta; `core` kept small (AD-11, §02.9). |
| R7 | Mod overlay unproven end-to-end on this platform | The central moddability promise fails late | M1 includes a real mod through the real launcher; `tests/mods/` runs it in CI. |
| R8 | Reproducible-build requirement | `verify` fails; releases are not auditable | Pin toolchains, remap paths, no timestamps, deterministic bundling; two-build compare in CI (§05.2). |
| R9 | Content validation too slow to run interactively | The editor's core loop degrades | Validation is incremental over changed packs; measure in `tests/perf`; a slow validator is a defect (AD-15). |
| R10 | Scope: the CRPG frame (M3) and the campaign (M5) dwarf the engine | Slipped dates | The engine is deliberately finished early (M1–M2) so content production is never blocked on it; the editor lands before the bulk of the content (M4 before M5). |
| R11 | Boot and load time are deferred by choice, so the first content-heavy build may start slower than we would like | It looks like a performance problem and is really a load-order problem | Counters ship from day one and the pak design is ready in `07:07.7`; promoting the deferred rows in `07:07.1` to gated is a harness-config change (`AD-25`). |
| R12 | **The asset contract is violated by the first contractors** — per-artist rigs, scales or animation conventions — making the cast unshareable and the animation library worthless | The whole art budget, discovered after delivery | The contract, the validator and the editor acceptance step are written **before the first hire** (`08`, `AD-31`); the demo build is the deadline for them. |
| R13 | **Environment art cost** — a multi-profile camera (down to ground level) means the world must be built and lit to be seen from every angle, which multiplies level art and lighting work relative to a fixed camera | Schedule, mostly in M5 | Budget profiles explicitly with per-profile scene budgets (`07:07.6`) and decide early which profiles ship in the demo; the cast is angle-independent, the world is not (`AD-30`). |
| R14 | **The demo's placeholder art sets expectations** — pre-sales buyers judge a 3D demo on its visuals, and the final cast ships later | Commercial risk, not technical | Say plainly that the demo is a systems demo with placeholder art; show the pipeline and the contract (`08`) as evidence the final art is planned, not hoped for. |
| R15 | **Two tools, two sources of truth** — level heights, spawns or anchors edited in Blender *and* in the editor, with the two disagreeing | Level bugs that are hard to attribute, because the rules read the editor's layers and the eye reads the mesh | The ownership table in `AD-32` is the rule: Blender owns geometry and layout, the editor owns the gameplay layers, the build only *proposes* derivations. A designer confirms every proposal. |
| R16 | **Blender version drift** across the studio and contractors produces different glTF, and pinned toolchains plus a pinned Blender are an extra build dependency | Non-reproducible art builds and unreviewable diffs | Pin Blender 5.2 LTS and KTX-Software in the repo, record the producing version in `extras`, and let the validator warn on a mismatch (`08:08.11`). |
| R17 | **The UI API is public and permanent once UI packs ship** (`09`, `AD-36`): a projection shape, a layout key or a `UiContext` member cannot change without a major version and a compatibility story | A breaking change lands on every installed pack at once, and modders stop trusting the surface | The base panels ship as the **first UI pack** through the same registry, so `make check` exercises the public contract on every build; the API is versioned (`worldspiracy.ui/v1`, `worldspiracy.projection/<name>/1`) and a pack that declares an unsupported major is disabled with a reason rather than half-mounted |

## 05.8 Definition of done

A Worldspiracy release is done when:

1. `make check`, `package`, `verify`, `e2e`, `mods`, `perf`, `a11y` pass.
2. Two builds compare byte-for-byte (`SHA256SUMS`).
3. Runtime budgets are met on the reference machine (frame time, memory, sim and render phase costs). Boot and load counters are reported, not gated (`AD-25`).
4. A deterministic replay fixture exists for every rules change since the last
   release.
5. Every save from the previous release migrates, with a fixture proving it.
6. The editor authors, validates, publishes, installs and playtests a mod, with no
   terminal involved.
7. `engine_version`/`save_version` agree across `pkg.toml`, `engine.manifest.json`,
   and the release; the release is calver and immutable.
8. The browser support statement in `§01.10` is true, and the diagnostics payload
   reports which backend, which mods, and which content hash are live.

## 05.9 Toolchain changes landed for this architecture

Two launcher capabilities were prerequisites rather than nice-to-haves, and both are
implemented in this repository rather than requested. Each landed with its schema, its
normative text, and tests.

| Change | Files | Spec | Tests |
|---|---|---|---|
| **Conditional COEP** — `server.cross_origin_embedder_policy` (`""` \| `require-corp` \| `credentialless`), emitted by the security-header middleware when non-empty, so the origin can be cross-origin isolated and `SharedArrayBuffer` / wasm threads become available | `launcher/internal/config/config.go`, `launcher/internal/server/server.go`, `architecture/schemas/launcher.config.schema.json` (+ the four synced copies) | FR-SRV-16, **FR-SRV-16a** (new), Launcher spec §13.7 and Appendix D.2 | `config/entry_coep_test.go`, `server/coep_test.go` |
| **`/editor` route** — serves `game/editor/index.html` at `/editor` and `game/editor/*` for the subtree, with the same confinement and symlink re-checking as the other roots, and a clean 404 when no editor ships | `launcher/internal/static/static.go`, `packaging/internal/pack/collect.go` | **FR-AST-16** (new), Launcher spec §12.3 and §13.1 | `static/editor_test.go`, `pack/.../servable_editor_test.go` |
| **Configurable entry document** — `server.entry_path` (`/` \| `/index.html` \| `/editor`) is what the launcher opens and what `--print-url` prints; an unknown value is rejected at config load | `launcher/internal/config/config.go`, `launcher/cmd/kobra-launcher/main.go` | **FR-LNCH-9** (new), Launcher spec Appendix D.2 | `config/entry_coep_test.go` |

Deliberate non-changes, so the boundary is explicit:

- **No new endpoint.** The editor needs no bootstrap-token machinery: it is
  same-origin, so it inherits the `kobra_session` cookie and reads the non-HttpOnly
  `kobra_csrf` cookie that the double-submit check compares against. `POST /__kobra/open`
  was considered and rejected as unnecessary surface (AD-23).
- **The root-file rule is unchanged.** `server.entry_path` may point at `/editor`, but
  `game/editor.html` is still rejected: the editor lives at `game/editor/`, so the game
  root still serves only `index.html` and `shell.js`.
- **No default behaviour changed.** With `cross_origin_embedder_policy` empty and
  `entry_path` unset, the launcher behaves exactly as before — no COEP header, opens
  `/index.html`. Existing games are unaffected; the new keys are opt-in.

Gates run for these changes, all green:

```sh
make check-schemas            # all five schema copies match architecture/schemas
make -C launcher fmt vet test # includes the new config, static and server tests
make -C launcher faultinject  # request-path middleware changed: PASS=20 FAIL=0
make -C packaging check       # fmt, vet, test
make -C testgame verify       # packager changed: two builds byte-identical
```

