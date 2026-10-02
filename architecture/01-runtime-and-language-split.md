# 01 — Runtime and language split

> **Superseded by AD-38.** The shipped runtime is native, and the pivot's first change
> **removed this document's implementation from the tree**: the WASM module, the JavaScript
> host, the WebGPU renderer and the two workers are gone, recoverable from git history. Read
> it for the design intent — the sim owns the rules and the save; the renderer is disposable —
> not for the runtime that ships.

This document answers, concretely: **what goes into the WASM file, what stays
JavaScript, where the WebGPU code lives, and how the pieces talk.** It implements
`AD-1`…`AD-6` and `AD-17`, and it is the contract the other documents build on.

## 01.1 The three artifacts

There are exactly three code artifacts, plus two data surfaces. Everything else is
content.

| Artifact | Built from | Loaded by | Lifetime |
|---|---|---|---|
| `engine/game.wasm` | Rust (`crates/kobra-core`), `wasm32-unknown-unknown`, integer-only | `WebAssembly.instantiateStreaming` in the worker | One instance, many `Sim` handles |
| `engine/*.js` | TypeScript (`src/web/worker`, `src/web/renderer`, `src/web/shared`) → plain ES modules | `new Worker(..., {type:'module'})` | Two workers: sim and render (`AD-4`) |
| `shell.js` + `assets/ui/**` | TypeScript (`src/web/shell`, `src/web/shared`) | `<script type="module" src="/shell.js">` | Main thread, one document |
| `editor/index.html` + `editor/*.js` | TypeScript (`src/web/editor`) | the `/editor` route (FR-AST-16) | Main thread, a **second** document (`AD-12`, `AD-23`) |
| `assets/**` + `locales/**` | JSON, CSV, WGSL, PNG/WebP, glTF, KTX2, OGG | via the VFS / static routes | Loaded on demand |
| `engine/engine.manifest.json` | Hand-authored (ours) | The shell, at boot | Static |

There is no fourth artifact and no plugin ABI (`AD-8`, `AD-19`). A mod is content,
optionally plus a same-origin ES module under `assets/scripts/` (`03:03.7`).

## 01.2 The split, decision by decision

The rule is not "put the fast code in WASM". It is: **the save depends only on
WASM, and the renderer is disposable.**

| Concern | Where | Why not the other side |
|---|---|---|
| 4C resolution (ladder, Master Table, row steps, colours) | WASM | Authoritative: a mod or the editor must not be able to reimplement it and disagree (`AD-15`) |
| Traits, powers, items, characters, vehicles | WASM | They are rules, and powers are bespoke algorithms |
| Combat panel machine, initiative, damage, conditions | WASM | It is the game, and it must replay identically |
| World, sectors, movement, AI, schedules | WASM | Feeds the save; needs the seeded RNG |
| Randomness | WASM, one seeded PRNG (`AD-6`) | `Math.random` cannot be saved or replayed |
| Save-state serialisation | WASM | The save is the sim's own format; JS only transports bytes |
| Content validation and the DSL compiler | WASM | The editor and the runtime must share one validator (`AD-15`) |
| Content parsing and merge (JSON → records) | JS, then a compact struct hand-off | Reading files is a browser job; the *semantics* of the merge are checked in WASM. JSON parsing in Rust would double the binary for no gain |
| WebGPU device, pipelines, passes | JS/TS, in the render worker | See §01.3; WebGPU only (`AD-3`) |
| Asset VFS, fetch, decode (`createImageBitmap`, `decodeAudioData`) | JS | `fetch` does not exist in wasm without a binding; browser decoders are the right tool |
| Animation sampling, particle updates, camera smoothing | JS | Presentation only; must not be able to change state |
| Text layout, glyph atlas, i18n substitution | JS | Fonts and shaping are a rendering concern; the sim emits string ids, not pixels |
| Audio mixing and playback | JS | Presentation only |
| DOM UI, dialogue, journal, inventory screens | Main thread JS | DOM is main-thread-only; the sim never renders UI |
| Editor UI, projects, undo/redo | Main thread JS | See `04` |
| Save transport, revisions, conflicts, heartbeat | Main thread JS | HTTP + CSRF is a shell responsibility (FR-API-1) |
| Tier-2 mod scripts | Worker JS, called synchronously by WASM | They must answer a resolution query immediately (`03:03.7`) |

Two columns of that table are the ones people get wrong, so they are called out:

- **Content parsing in JS is not a determinism risk**, because the *merge* result is
  hashed and the hash is recorded in the save (`03:03.5`), and the validation that
  matters runs in WASM. If JS parsing ever produced two different record sets for
  the same bytes, the hash would catch it.
  **As built at M1**, the hand-off is a JSON document
  (`worldspiracy.content-load/1`, typed in `src/web/shared/content.ts`): JS reads
  files and calls `JSON.parse`, the core does the merge, the structural checks and
  the validation. The "compact struct" the row calls for is therefore realised as a
  *merged* document rather than a binary one, because the core already parses JSON
  for commands, previews and saves (`01:01.5`) and a binary encoder would be a
  second format to keep in step. A packed encoding remains a later optimisation
  behind the same seam — the interface is "one document in, one registry and one
  report out", not "JSON".
- **Animation and particles are not in WASM on purpose.** They are presentation. If
  a particle ever needs to affect the game, it is a game rule and it moves to WASM.

## 01.3 The renderer: WebGPU only

**WebGPU is required, with no fallback** (`AD-3`, `AD-17`). That is a studio
decision with a clear consequence: the supported browser is a **WebGPU-capable
Chromium**, and Firefox and Safari are out of scope until their WebGPU support is
universal and stable.

| Platform | WebGPU | Supported |
|---|---|---|
| Chrome / Edge / Brave / Opera / Chromium desktop, Windows · macOS · ChromeOS | 113+ | **Yes** |
| Chrome desktop Linux | 144+, Intel Gen12+ | Yes on 144+ only |
| Chrome for Android | 121+ | Not targeted (desktop launcher) |
| Firefox Windows | 141+ | No (out of scope by directive) |
| Firefox macOS Apple silicon / Linux / Intel | 145+ / never / never | No |
| Safari macOS Tahoe / iOS 26+ | 26+ | No (out of scope by directive) |

The engine manifest states the requirement, so the shell refuses cleanly instead of
failing at first draw:

```json
"required_features": ["wasm", "simd", "bulk-memory", "threads", "shared-array-buffer", "webgpu"],
"min_browser": { "chrome": 113, "edge": 113, "opera": 99 }
```

`threads` and `shared-array-buffer` are required because the core is multithreaded
(`AD-5`), and `simd` because the spatial work uses `+simd128`; both are safe at this
floor (SIMD: Chrome 91; threads: Chrome 74 *given* cross-origin isolation, which is
now configured — §01.7). Unsupported browsers fail into error matrix E1/E2 with a
plain-language cause.

Because there is exactly one backend, there is exactly one shader language: **WGSL**,
in `assets/shaders/*.wgsl`, fetched through the VFS and therefore moddable. The
earlier "two shader source sets and no compute" constraint is gone, and nothing in the
renderer is fenced off for a weaker path. The performance consequences are in `07`.

### The RenderPacket

The sim never sees a GPU object. Every tick it writes a packet into a region of
**shared** linear memory, and the renderer reads it in place (`AD-4`) through a
`DataView` on the shared buffer. The packet is **integer-only** (`AD-6`); all floats
live in the renderer.

```
RenderPacketHeader  (fixed size, version-stamped)
  u32 packet_version, frame_id, tick, flags
  u32 camera_asset_hint
  i32 camera_x_q16, camera_y_q16, camera_zoom_q16, camera_rot_q16, camera_pitch_q16
  u32 viewport_w, viewport_h
  u32 item_count, text_count, vfx_count
  u32 offset_items, offset_text, offset_vfx
  u32 rng_cosmetic_state            // presentation-only PRNG stream, never saved
  u32 state_hash_lo, state_hash_hi  // free determinism check for the harness

DrawItem
  u32 asset_id          // interned content id, NOT a GPU handle
  u32 atlas_slot        // frame index within the atlas/animation set
  u16 layer             // z-order
  u8  kind              // quad | tile | mesh | nine-slice | trail
  u8  blend             // normal | add | multiply | cutout
  i32 x_q16, y_q16, z_q16, sx_q16, sy_q16, rot_q16
  u32 tint_rgba8
  u32 flags             // flip_x, flip_y, ignore_light, ui_space, …
  // kind == mesh only (AD-30): the renderer samples; the sim never sees a pose.
  u32 clip_id, clip_time_q16      // current clip and its time
  u32 clip_blend_id, clip_blend_q16  // cross-fade partner and weight
  u32 lod, skin_id                // LOD tier; skin/palette handle for GPU skinning

TextItem
  u32 string_id         // interned string id; the renderer resolves it, i18n included
  u32 arg_pack_id       // up to 4 integer/string args
  i32 x_q16, y_q16
  u16 layer
  u8  style             // body | heading | tooltip | damage-number | …
  u8  flags

VfxItem
  u32 effect_id, seed, layer
  i32 x_q16, y_q16, scale_q16, progress_q16
```

**Camera and animation are state; the matrix is presentation (`AD-30`).** The packet's
camera block is produced by the sim's camera *state* — profile, target, transition, shake —
and the renderer turns it into a float view matrix. Cinematics are therefore deterministic
and replayable. A `mesh` draw item carries its clip, clip time and blend weights, so the
renderer samples the animation and **no pose ever enters the simulation**. Picking runs the
other way: the render worker unprojects a pointer event and calls `wsp_pick`, which returns
an **entity id or sector** — the float ray decides *what was clicked* and never becomes a
rule input.

Billboards remain a first-class kind alongside meshes, for foliage, particles, decals,
impostors, LOD proxies and any mod that ships 2D art (`AD-29`).

Four further properties fall out of this design and each one is load-bearing:

1. **The packet carries `asset_id`, never a GPU handle.** That is what makes the
   renderer disposable: a lost device, a resolution change, or a context restore
   rebuilds GPU resources from ids alone, with the sim unaware.
2. **The packet carries `string_id`, never text.** The renderer owns font, shaping,
   layout, and locale substitution (`AD-22`). The sim cannot leak English into a
   save, and a translation mod changes what is drawn without touching game state.
3. **Fixed-point in, float out.** `q16` values are exact; the renderer converts and
   interpolates between the previous and current packet using a presentation clock.
   Render rate and sim rate are independent, and no float ever flows back.
4. **`state_hash` rides along for free.** The harness reads it from the packet header
   each frame, so determinism checking costs one comparison (`07:07.8`).

### GPU resource management

The renderer owns one `ResourceTable`: `asset_id → {texture | mesh | pipeline}`, with
the following rules:

- **Assets are loaded lazily and cached by asset id + content hash**, using
  `asset.manifest.json`'s hashes as the cache key (FR-AST-4: advisory only, so a
  mismatch warns and the file the user has wins).
- **Textures** are decoded with `createImageBitmap` inside the worker from
  same-origin bytes and uploaded with `copyExternalImageToTexture`. The packet's
  `atlas_slot` selects a frame within an atlas, so a character animation is one
  texture and many slots. Mip chains ship prebuilt (KTX2) rather than being generated
  at runtime.
- **Batching.** The sim emits draw items already grouped by `(layer, asset_id, blend)`,
  so the renderer does not sort per frame: it walks the packet and issues one instanced
  draw per batch.
- **Billboards write depth with alpha-test**, so 3D world geometry occludes a sprite
  correctly — a character behind a pillar is behind the pillar (`AD-27`). The alternative
  (manual sort layers, no depth write) is cheaper but breaks the moment a pillar is in
  front, which is precisely the case an isometric CRPG is full of.
- **Sprite lighting is ambient plus a directional tint plus a blob shadow.** No
  normal-mapped sprites and no per-sprite dynamic shadow in v1: both cost art work per
  character for a subtle gain, and the same effort is better spent on the world that a
  future 3D character pack reuses (`AD-27`).
- **Pipelines and bind groups are built once, during the loading screen**, cached by
  `(shader, layout, target format)`. Nothing in the frame loop creates a GPU object.
- **Limits are queried, never assumed.** Defaults are modest — `maxBufferSize` 256 MiB,
  `maxStorageBufferBindingSize` 128 MiB per binding, `maxUniformBufferBindingSize`
  64 KiB, `minUniformBufferOffsetAlignment` 256 bytes — so the renderer reads
  `adapter.limits` and requests what it needs via `requiredLimits`, splitting pools
  when the adapter cannot grant it.
- **No readback on the render path.** Screenshots and editor thumbnails are the only
  readbacks, and they are user-initiated.
- **`timestamp-query`** is used when the adapter exposes it, feeding the same
  frame-stat ring as the CPU phase timers (`07:07.8`).

## 01.4 Worker topology

Three threads, one job each (`AD-4`). The main thread is DOM only.

```
shell.js (main)                     sim worker                  render worker
──────────────                      ──────────                  ─────────────
token → session → CSRF              game.wasm + pthread pool     WebGPU device
GET /api/state, /api/data/*         tick, rules, world, AI       pipelines (prebuilt)
project & save transport            writes RenderPacket         reads packet in place
DOM UI, editor UI                   into a shared ring slot      OffscreenCanvas
   │                                     ▲                            ▲
   │ canvas.transferControlToOffscreen()─┼────────────────────────────┘
   │                                     │
   └── postMessage (UI frequency) ───────┴── SharedArrayBuffer frame ring (3 slots)
                                              publish / acquire via Atomics
```

Capability ladder, resolved once at boot:

| Order | Condition | Topology |
|---|---|---|
| A | `crossOriginIsolated`, `navigator.gpu` in a worker, `OffscreenCanvas` + worker `requestAnimationFrame`, wasm threads | the full topology above |
| B | everything except worker WebGPU | sim in a worker, renderer on the main thread with the window's `requestAnimationFrame`; visible performance warning |
| C | no cross-origin isolation, or no WebGPU | refuse — error matrix E1/E2, naming the missing condition |

Tier A is the shipping configuration and the only one that is performance-supported
(`AD-24`). Tier B exists so a browser quirk degrades instead of failing outright, and a
session in tier B is marked in diagnostics so a performance report is interpretable.

The worker is created with `{type:'module'}` so `import` works, and the canvas is
transferred exactly once (`transferControlToOffscreen` cannot be undone). A worker
crash (WASM trap, script throw, GPU device loss) tears down both workers, replays from
the last save or the command log since the last autosave, and restarts.

**The ring handoff may arrive before the renderer can take it.** The sim worker
posts it as soon as the campaign exists, and the renderer's device may still be
coming up. A `MessagePort` queues a message until `onmessage` is assigned — but the
handler then runs *immediately*, so a handler that reaches for a session that is
still being constructed drops the handoff, and the renderer loops forever with no
ring: a black canvas, no error. The renderer therefore latches the handoff
(`renderer/ring-latch.ts`) and attaches it when the session exists. Any future
consumer of a cross-thread handoff must do the same, and the ordering is unit-tested
rather than left to a race.

Worker `requestAnimationFrame` requires an owner window, which a page-created worker
has; the canonical pattern is exactly the one used here (transfer an `OffscreenCanvas`
in, call `self.requestAnimationFrame`).

## 01.5 The ABI

The WASM/JS boundary is a hand-written C ABI (`AD-2`), version-stamped, and small
enough to hold in your head. Everything crosses as integers plus a length-prefixed
`memcpy` of UTF-8 JSON in linear memory.

### Exports (wasm → JS)

```c
u32  wsp_abi_version(void);                 // compared against the glue's expectation
u32  wsp_alloc(u32 len);                    // JS writes into this before a call
void wsp_free(u32 ptr, u32 len);
u32  wsp_init(u32 seed, u32 cfg_ptr, u32 cfg_len);      // returns 0 or a status
u32  wsp_new_campaign(u32 cfg_ptr, u32 cfg_len);        // returns a Sim handle
u32  wsp_load(u32 bytes_ptr, u32 bytes_len);            // returns a Sim handle
void wsp_free_sim(u32 handle);
u32  wsp_command(u32 handle, u32 ptr, u32 len);         // status
u32  wsp_tick(u32 handle, u32 elapsed_ticks);           // status
u32  wsp_resize(u32 handle, u32 width, u32 height);     // status — the viewport is in the packet
u64  wsp_ring(void);                        // (addr << 32) | len — the shared frame ring
u64  wsp_outbox(u32 handle);                // (ptr << 32) | len  — drained events, JSON
u64  wsp_render_packet(u32 handle);         // (ptr << 32) | len
u64  wsp_preview(u32 handle, u32 ptr, u32 len);
u32  wsp_pick(u32 handle, u32 ray_ptr, u32 ray_len);   // float ray in, discrete target out (AD-30)
u64  wsp_save(u32 handle);
u64  wsp_validate_content(u32 ptr, u32 len);
u64  wsp_status(u32 handle);                // diagnostics document
u64  wsp_roster(u32 handle);                // the characters
u64  wsp_journal(u32 handle);               // quests, lines, clock, standing
u64  wsp_dialogue(u32 handle);              // the open conversation, or none
u64  wsp_world(u32 handle);                 // the world, its stations, the party
u64  wsp_shops(u32 handle);                 // the counters in reach, and the pack
u64  wsp_actions(u32 handle);               // the available-action set (02:02.13)
u64  wsp_signals(u32 handle);               // named conditions, for UI gating (09:09.4)
u64  wsp_check_ui(u32 ptr, u32 len);        // validate layout documents (09:09.5)
u64  wsp_narrate(u32 handle, u32 ptr, u32 len);  // event kinds → string ids (AD-22)
u32  wsp_register_asset(u32 handle, u32 path_hash);     // intern → asset_id
u32  wsp_intern_string(u32 handle, u32 id_hash);        // intern → string_id
u64  wsp_hash_state(u32 handle);            // determinism oracle
u64  wsp_last_error(void);                  // (ptr << 32) | len
```

### Imports (JS → wasm)

```c
u64  env.wsp_script_hook(u32 hook, u32 ctx_ptr, u32 ctx_len);  // AD-8 Tier-2 scripts
void env.wsp_log(u32 level, u32 ptr, u32 len);                 // diagnostics, dev only
```

That is the whole surface: **two imports**. A small boundary is what makes it
possible to keep the two sides honest, and it is why `AD-2` rejected
`wasm-bindgen`'s generated surface.

Two of those exports are newer than the rest and worth naming, because each one
is a mechanism rather than a convenience:

- **`wsp_ring` is how the renderer finds the packet at all.** The ring lives at a
  fixed address in the sim's shared linear memory and its geometry is in its own
  header, so the worker reports `(address, length)` once and the render worker
  reads slots in place from then on. Without it the only way to hand the renderer
  a packet would be a message per frame, which is the copy `AD-4` exists to
  avoid.
- **`wsp_resize` exists because the viewport is in the packet** (see the header
  above). It is presentation *input*, not game state: pixels are integers, and
  the sim needs them to build the header.

The rest of the list is the target surface, not the current one, and this section
is kept current as the surface lands. **ABI 2 (M1)** exports the version check,
the allocator, `wsp_init`, content load and validation, the content hash, a
campaign handle, `wsp_load`, the command stream and its outbox, `wsp_save`,
`wsp_preview`, `wsp_status`, `wsp_roster`, tick, resize, the ring, the packet, the
state hash and the error buffer. The loop that matters is
`wsp_load_content → wsp_new_campaign → wsp_command* → wsp_outbox → wsp_save`.

Still absent, and deliberately so: `wsp_pick` (no mesh pipeline to pick against
until the asset work), `wsp_register_asset` / `wsp_intern_string` (the asset
pipeline is M2/M3, and M1 uses the built-in quad and string ids straight from
content), `wsp_tick`'s pthread pool, and `wsp_script_hook`, which arrives with
Tier-2 scripts in M2. An export that always fails is worse than a missing one,
because `wsp_abi_version` would claim otherwise.

**ABI 3 and 4 were M2 and M3**: the script host, then the campaign projections
(`wsp_journal`, `wsp_dialogue`, `wsp_world`, `wsp_shops`, `wsp_narrate`).
**ABI 5 (M3.5)** adds `wsp_actions` — the available-action set the contextual bar
renders (`02:02.13`) — alongside two content shapes the same milestone needs: a
dialogue option may invite a fight, and a station may keep a shop.

**ABI 6 — the moddable UI (M3.6, `09`)** adds two exports and changes *delivery* rather
than direction. `wsp_signals` reports the truth value of every named condition content
declares, so a layout gates on a boolean the engine computed and never evaluates a
predicate itself (`09:09.4`, `AD-37`); `wsp_check_ui` validates the layout documents a
caller supplies where the file list is known — the editor and the dev loader — exactly as
`check_overrides` takes the `assets` array (`09:09.5`). Delivery becomes **subscribed**
(the page declares what it mounts, the worker sends only that union) and **revised** (each
projection carries a counter, so a panel whose document did not change is not re-sent and
not re-rendered). That is what lets a third-party HUD cost nothing to a player who does
not have it.

The rest of the list is the target surface, not the current one, and this section
is kept current as the surface lands. **ABI 2 (M1)** exports the version check,
the allocator, `wsp_init`, content load and validation, the content hash, a
campaign handle, `wsp_load`, the command stream and its outbox, `wsp_save`,
`wsp_preview`, `wsp_status`, `wsp_roster`, tick, resize, the ring, the packet, the
state hash and the error buffer. The loop that matters is
`wsp_load_content → wsp_new_campaign → wsp_command* → wsp_outbox → wsp_save`.

### Rules that make it safe to call

1. **Handles, not globals.** `wsp_new_campaign` returns an opaque handle, so the
   editor holds a scratch `Sim` next to the live one (`04:04.6`) and the game can
   hold a preview `Sim`. Multiple sims coexist with no shared mutable state.
2. **No allocation per call for the caller.** JS allocates once at startup for
   command and result buffers and reuses them; `wsp_alloc`/`wsp_free` exist for
   growing buffers (content load, save), not for the hot path.
3. **Status codes, never exceptions across the boundary.** Every `u32` return is a
   status; `0` is success. On failure, `wsp_last_error()` returns a JSON
   `{code, message, detail}` in memory. A Rust panic is **not** converted into a
   status: the core is built with `panic = "abort"` (`05:05.2`), so a panic is a
   WebAssembly trap and is handled by rule 4 below. That is a deliberate
   trade — unwinding costs code size on the hot path for a case that only a bug
   can reach — and it is why rule 4 is written the way it is.
4. **A trap is fatal to that sim handle only.** A WebAssembly trap leaves the
   instance's state suspect, so the worker discards the handle, rebuilds from the
   last save, and reports it. It never keeps running on a suspect `Sim`.
5. **ABI version is checked first.** `wsp_abi_version()` must equal what the glue
   was built against; a mismatch is a packaging error (the wasm and the glue are
   shipped together) and fails cleanly with E23 rather than misbehaving.

### Everything crosses as JSON, deliberately

Commands, events, previews, validation reports, and saves cross as UTF-8 JSON with a
length prefix. For the *hot* path this would be wrong — but there is no hot path
across the boundary: commands are player actions (a few per second at most) and
events are narrative (tens per panel). The per-frame data crosses the other way, as
the integer `RenderPacket`, with no serialisation at all. This keeps the ABI
debuggable (you can log the traffic) and language-agnostic (no schema compiler),
while paying nothing where it would matter.

## 01.6 Memory model and the frame boundary

```
wasm linear memory
├── static: ladders, tables, interned content (immutable after init)
├── Sim arena: entities, world, conditions, AI state          ← the saveable state
├── scratch: event outbox, validation reports, string interning
└── render ring: 3 slots × (packet header + draw-item array)
```

The ring's own header carries its geometry — version, slot count, slot stride,
the item offset and stride, the draw-item capacity, a cumulative dropped-item
counter, and one publication counter per slot — so a reader needs one address
and nothing else. Each slot is preceded by nothing and contains, in order:

```
u32 packet_version, frame_id, tick, flags, camera_asset_hint,
i32 camera_x_q16, camera_y_q16, camera_zoom_q16, camera_rot_q16, camera_pitch_q16,
u32 viewport_w, viewport_h, item_count, text_count, vfx_count,
u32 offset_items, offset_text, offset_vfx, rng_cosmetic_state,
u32 state_hash_lo, state_hash_hi                     // 84 bytes, padded to 96
DrawItem[capacity]                                   // 68 bytes each
```

The publication counter is a **seqlock**: the sim sets it odd, writes the slot,
then sets it even with release ordering, and the renderer reads it before and
after reading the body. An odd or changed counter means the slot was mid-write,
and the renderer skips the frame rather than drawing a torn one. That is what
lets one writer and one reader share the memory with no lock and no copy.

- Memory is **created once with a declared ceiling** in `engine.manifest.json`
  (`memory.initial_bytes`, `memory.maximum_bytes`), and grown never on the hot path.
- Memory is declared `shared: true` and sized once against the spec's 800 MiB target
  / 1200 MiB budget, with the 2 000 ms interactive target (4 000 ms cap) as the boot
  budget (FS §17.1); it is never grown on the hot path (`07:07.5`).
- **The packet ring is a `SharedArrayBuffer`** (`AD-5`): the sim writes into a slot,
  publishes it with `Atomics`, and the render worker reads it in place. Nothing is
  copied between threads, and the renderer's only per-frame copy is instance data into
  a reused GPU buffer. The ring is triple-buffered and fixed-capacity, and a slot the
  renderer did not consume is dropped rather than queued (`07:07.3`).
- **The packet regions are sized for the worst case** and the sim *drops* draw items
  beyond capacity, setting a `packet_overflow` flag that is reported in diagnostics.
  Dropping a sprite is always preferable to a frame hitch or a crash.
- The renderer never writes into `Sim` memory. Reads only. That is the enforcement
  point for "the renderer is disposable": a write would be a bug the type system
  cannot catch in JS, so the renderer holds a read-only `DataView` built on a
  snapshot region and is reviewed against the invariant.

## 01.7 Threading and isolation: resolved

`SharedArrayBuffer` requires a secure context **and** cross-origin isolation:
`Cross-Origin-Opener-Policy: same-origin` together with `Cross-Origin-Embedder-Policy`.
Plain `http://127.0.0.1:PORT` **is** a secure context (loopback is a
potentially-trustworthy origin), so no TLS is needed; COOP `same-origin` was already
sent; and the launcher now implements the COEP half as
`server.cross_origin_embedder_policy` (FR-SRV-16), which the shipped game config sets
to `require-corp`.

**Decision (AD-5): multithreaded WASM over `SharedArrayBuffer`.** `engine.manifest.json`
declares `threads` and `shared-array-buffer` in `required_features`, and the shell
verifies `crossOriginIsolated === true` at boot, failing into error matrix E2 with a
named cause if the publisher did not configure it (FR-SRV-16a). There is no silent
single-threaded mode: a configuration that cannot deliver the performance bar is a
configuration error, not a degraded experience (`AD-24`).

Parallelism is constrained by the determinism rules in `07:07.4` — parallel systems
are maps over a fixed id-derived partition, reductions are ordered, the RNG is
splittable and pre-partitioned, and the golden replay corpus is run at 1, 2, 4 and 8
threads. Threads are an implementation detail of the core; the ABI in §01.5 is
unchanged.

The one cost to keep in mind: with COEP set, any cross-origin subresource the page
uses must opt in with CORP or CORS. There are none, by `AD-19` — everything the page
loads is same-origin — so this is a constraint to preserve rather than a problem to
solve.

## 01.8 Boot sequence

Numbered because the order is the contract, and each step has a failure path.

```
 1. shell.js reads `t` from location.hash, erases it (history.replaceState)
 2. POST /__kobra/session {token} → {session_id, csrf_token, api_version}
 3. GET /api/state → verify api_version major; record save_version, data_writable
 4. feature-detect: WebAssembly, instantiateStreaming, a SUCCESSFUL SIMD probe,
    navigator.gpu (and in a worker), OffscreenCanvas + worker rAF, wasm threads,
    and **crossOriginIsolated === true** (FR-SRV-16a)
      → choose topology A/B/C (§01.4); fail into E1/E2, naming the missing condition,
        if C. Isolation is checked here rather than assumed, because it is publisher
        configuration rather than engine capability.
 5. show splash; GET /engine/engine.manifest.json → check engine_version,
    required_features, min_browser against the detection result (E1/E2)
 6. create the worker; transfer the OffscreenCanvas; begin heartbeat at
    /api/state.heartbeat_interval_seconds (×6 while hidden)
 7. worker: instantiateStreaming('/engine/game.wasm') (E23 on failure;
    the shell checks the MIME is application/wasm first, because
    instantiateStreaming rejects any other type)
 8. worker: wsp_abi_version() check; wsp_init
 9. content load (03:03.8): config, mods, manifests, packs, validate, register scripts
10. wsp_load(save) if a save is selected, else wsp_new_campaign(seed + content id)
11. load shaders and the first N assets; first frame; splash out
12. main loop: sim tick at SIM_TICK_MS, render at display rate, heartbeat, autosave
```

Feature detection is a *positive* probe, not a version sniff: a SIMD check uses
`WebAssembly.validate` on a tiny SIMD module, WebGPU uses `navigator.gpu` +
`requestAdapter` inside the worker, wasm threads probe with a tiny shared-memory
module, and worker
rAF is confirmed by the absence of an exception when scheduling. A browser that
claims support but cannot deliver is caught at step 4/5 rather than at first draw.

The MIME check at step 7 is not paranoia: `instantiateStreaming` rejects with a
`TypeError` when the `Content-Type` is not exactly `application/wasm`
(parameters such as `application/wasm;` also fail). The launcher's default map
already sends `application/wasm` for `.wasm`, and the game must not remove that key.

## 01.9 Lifecycle

- **Pause.** `document.visibilitychange` on the main thread posts a `pause` to the
  worker; the worker stops advancing the sim clock, keeps rendering at a low rate or
  stops entirely, and the shell multiplies the heartbeat interval by six (per the
  launcher's requirement that a hidden tab not keep the process alive). On resume,
  the sim advances by **at most one panel**, never by the wall-clock gap — a game
  that fast-forwards three hours of starvation because the player alt-tabbed is a
  bug.
- **Shutdown.** The shell posts a goodbye (`POST /__kobra/goodbye` via
  `fetch(..., {keepalive:true})` with the CSRF header — `sendBeacon` cannot set a
  header and would 403). Autosave first; a shutdown that loses progress is a defect.
- **Update.** The UI calls `GET /api/update/check` and `POST /api/update/apply`,
  which only *records intent*; the swap happens on the next launch (FS §12). The
  shell reports the previous run's outcome from `GET /api/update/progress` at the
  next boot, because the apply happens between sessions.
- **In-memory session.** If `/api/state.data_writable` is `false` (read-only media —
  USB mounted `noexec`/read-only, or a network share), the game still runs: saves
  live in memory, the player is told plainly, and export is offered (error matrix
  E4, FS §11.8). Performance must not degrade — the interface to the save layer is
  the same, only the store differs.
- **Worker crash recovery.** Tear down, rebuild, reload the last save or replay the
  command log since the last autosave, restore the UI state, and report the crash
  through `/__kobra/diagnostics`. The main thread holds the command log precisely so
  this is possible. **As built:** the checkpoint is the last panel boundary rather
  than the last autosave (a commit is when the state is consistent), the shared
  memory and the render worker's ring survive the rebuild, the bound is three
  attempts, and a trap before the first checkpoint stops the session rather than
  silently restarting the campaign (`src/web/worker/recovery.ts`).

## 01.10 Browser support statement

What the game tells the launcher and the player:

| Tier | Requirement | Experience |
|---|---|---|
| **Supported** | A WebGPU-capable Chromium: Chrome, Edge, Brave, Opera or Chromium **113+** on Windows/macOS/ChromeOS; **144+** on Linux, Intel Gen12+ | Full quality, multithreaded sim, shared-memory renderer |
| **Degraded** | The same browsers without worker WebGPU | Identical content and simulation; renderer on the main thread; flagged in diagnostics |
| **Refused** | No WebGPU, or no cross-origin isolation, or no wasm threads/SIMD | E1/E2 with a plain-language cause and no blank canvas |

`browser_preference` in `launcher.config.json` lists the Chromium family —
`chromium`, `chrome`, `brave`, `msedge`, `opera` — because all five are the same
engine and any of them can run the game. `min_browser_version` raises the floor to
113/113/99 so the launcher refuses an older browser before opening it (FR-LNCH-6)
rather than letting the player watch it fail.

Firefox and Safari are deliberately absent: neither can run a WebGPU-only build
reliably across their supported platforms today (`AD-17`). Widening support later is a
config change plus a backend, not a redesign — which is the point of keeping the list
in configuration rather than in code.

## 01.11 Budgets

The complete budget table, with targets, fail thresholds and measurement methods, is
`07:07.1`. The engine-side numbers that shape the code rather than merely measure it:

| Metric | Target | Budget | Consequence for the design |
|---|---|---|---|
| Engine memory (baseline play) | < 800 MiB | 1 200 MiB | Shared memory sized once from the manifest; no growth, no per-tick allocation |
| Sim tick, 2 000 actors | ≤ 2.0 ms | 4.0 ms | Fixed id-derived partition; SoA layout; `+simd128`; ordered reductions |
| Frame time p99 | ≤ 8.3 ms | 16.6 ms | Triple-buffered shared ring; dropping render frames instead of queueing |
| Autosave frame impact | ≤ 1 ms | 2 ms | Saves are serialised on the sim's budget and written by the Go server off-thread |
| Splash → interactive, WASM compile, root requests, asset load | keystones from FS §17.1 | — | **Deferred, measured but not gated (`AD-25`).** v1 ships loose content files; the VFS keeps a bundle seam so a pak can be added in one module if boot time becomes a problem (`07:07.7`). `instantiateStreaming` and in-worker image decode are kept regardless, because they are free |

## 01.12 Invariants

If a change violates one of these, the architecture has been broken, not extended:

1. Nothing outside `game.wasm` can change game state.
2. Nothing inside `game.wasm` touches a browser API, the wall clock, or the platform.
3. The renderer reads a packet and writes pixels. It never writes state, and a lost
   device is recoverable from `asset_id`s alone.
4. All randomness comes from the seeded PRNG; `Math.random` appears nowhere in the
   engine, and only in the cosmetic stream inside the renderer.
5. Floats exist only in the renderer, only after the packet.
6. Parallel systems satisfy `07:07.4` — map-only over a fixed partition, ordered
   reductions, no shared float accumulation, no work stealing. If a system cannot,
   it runs single-threaded.
7. Nothing allocates on the sim tick path, and the packet ring is fixed-capacity with
   an overflow counter instead of growth.
8. The ABI has two imports and version-stamped exports; it changes with a version bump
   and a manifest update, never silently.
9. Every content failure is per-mod and fail-soft; every engine failure is loud and
   explains itself in plain language (FS §16).
10. The page never reaches the network; every asset is same-origin (`AD-19`), which is
    also what keeps `require-corp` cost-free.

## 01.13 The shell interface, and dev mode

The shell is the only DOM in the game (`AD-4`). Until M3.5 it was a diagnostic control
panel — a button per command and a stack of read-only panels. M3.5 makes it an interface:
the canvas is the page, the panels are overlays, and every control is a view of the
engine's own available-action set (`02:02.13`).

### The canvas is the page, and that is not fullscreen

`#stage` is `position: fixed; inset: 0`; the canvas fills it, and every other element —
the clock, the action bar, the menu icons, the panels, the banner — is a HUD layer
positioned over it. Nothing is in document flow below the canvas, so the game uses the
whole tab with no page scroll. This is deliberately **not** the Fullscreen API: the
browser's own chrome stays, the user keeps their tabs and their escape hatch, and the
game is not asking for a mode it would then have to manage.

The renderer is told the real viewport (`wsp_resize`), and the packet carries it
(`§01.3`), so tiles stay square at any window shape. A non-16:9 tab shows *more or less
world*, never a stretched one, and never a letterbox: the camera fits what is there.

### The HUD

| Region | Contents | Behaviour |
|---|---|---|
| Bottom left | **Journal**, **Character**, **Shop** icon buttons | Each opens a modal sheet; `J`, `C`, `I` do the same |
| Bottom centre | The **action bar**, rendered from `wsp_actions` | Every action the engine named, with its key, and the engine's reason when it is disabled |
| Bottom right | The **Dev** icon | Development builds only; opens the diagnostics sidebar |
| Top left | The clock: day and `DaySlot` | The same quantity the combat panel advances (`02:02.5`) |
| Centre | The start screen, until a campaign exists | New campaign, or the scripted encounter |

A sheet is **modal**: while one is open the shell posts no movement and no actions, and
the engine refuses `move`/`begin_encounter` with `in_dialogue` while a conversation is
open (`02:02.13`). Pausing the table is the point — a panel clock that ran behind an open
inventory would make reading the journal a decision with a cost the player never chose.

### The keyboard is the primary control

| Key | Action |
|---|---|
| `W` `A` `S` `D`, arrows | Move one tile (one panel, `02:02.6`) |
| `E` | Interact: talk to whoever is here, else open the counter |
| `Space` | Wait an hour (60 panels) |
| `F` | Fight |
| `1`–`9` | The nth action while exploring; the nth option in a conversation |
| `J` `C` `I` | Journal, Character, Shop |
| `Esc` | Close the topmost sheet |
| `` ` `` | Toggle the development sidebar (dev builds only) |

The action bar prints exactly these hints, so the bar and the keyboard are one interface
rather than two. Every control is a button as well, which is what keeps the interface
operable by keyboard alone and legible to a screen reader — the audit in `05:05.4` covers
the new chrome.

### Dev mode is the launcher's answer, not the page's

`make play` and `make package` serve the **same** `game/` tree: the folder and the shell
are byte-identical, so a page cannot tell a development run from a packaged one by
inspecting itself, and a build-time flag would mark both or neither. The signal is
therefore the launcher's: a `kobra_dev` build reports `dev: true` in
`GET /__kobra/diagnostics` (`Launcher-spec.md` §21.4), and a release binary has no code
path that can set it. The shell offers the Dev icon from that field alone, and a missing
or false field means no dev surface at all — fail-closed, like every other version gate
(`VERSIONING.md`).

The sidebar itself is *diagnostics and the log*, which is what a developer running the
game needs: topology, backend, adapter, isolation, thread and feature probes, the content
report, the state hash, the launcher's own diagnostics, and the engine's narration lines.
It is not a cheat console: there are no dev-only commands, so a defect in the gate would
expose telemetry rather than the ability to rewrite state.

### What the shell still does not do

It draws no game UI of its own beyond text and layout: an icon is inline SVG, the shader
and the tiles are the engine's, and portraits, item art and animations arrive with the
asset pipeline (`08`). The shell resolves every string through the two tables of `AD-22`
— the shell's own from `/locales/`, the game's from `/assets/locales/` — and a key with no
entry renders as itself, so a missing string is a visible gap rather than a blank line.

**The panels are not privileged.** Every panel in this section — the journal, the character
sheet, the counter, the action bar — is the *first UI pack*: it registers through the same
registry and receives the same capability object a mod's panel does (`09`, `AD-36`). The
base panels are bundled into `shell.js` to protect the boot-request budget, but the contract
is identical, which is what keeps the public surface exercised by every build instead of
discovered broken by the first modder.

**Built at M3.6.** The registry, the layout binder, the capability object and the command
scope are `src/web/shell/ui-host.ts`, `ui-layout.ts`, `ui-context.ts` and `ui-mods.ts`; the
base pack is `panels.ts`; the keyboard model claims its keys through the same arbitration a
renderer's `ctx.onKey` uses; and the stacking order is four named bands (`--z-hud`,
`--z-panel`, `--z-modal`, `--z-dev`) a theme overrides rather than four numbers it guesses.
The dogfood property is a test (`tests/web/shell-flow.test.mjs`), not a claim.
