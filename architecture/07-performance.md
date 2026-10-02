# 07 — Performance architecture

> **Partly superseded by AD-38.** The budgets, the counters and "a miss is a defect" stand.
> The native comparison is retired because the native build *is* the product, and the rows
> that measure a browser — frame-time tax, request volume, cross-origin isolation — no
> longer describe the runtime.

Performance is the **master goal**. The studio thesis is that a downloadable game,
served from a loopback HTTP server, rendered by the player's own browser, is a
plausible alternative to other ways of shipping games — *including on performance* —
and that the web-technology stack buys easier authoring and far easier modding. If
the game stutters, the thesis fails regardless of how good the modding is.

This document states the acceptance bar, the concurrency and memory architecture that
meets it, the measurement discipline that keeps it honest, and the launcher changes it
forced. `AD-5`, `AD-17`, `AD-23` and `AD-24` are the decisions behind it.

## 07.1 The acceptance bar

Budgets are contract, not aspiration: a build that misses one is a defect
(FS §17). The reference machine is the one in FS §17.4 (4-core x86-64 ≥ 2.5 GHz,
16 GiB, NVMe, clean profile, release build, no devtools).

**Gated now — the runtime bar.** These are the numbers that decide whether the game
feels native, and a miss is a defect:

| Metric | Target | Fail above | How it is measured |
|---|---|---|---|
| Frame rate, benchmark scene, 1080p | 120 FPS sustained | 60 FPS | In-engine frame stats, p50 |
| Frame time p99 | ≤ 8.3 ms (120 Hz) | 16.6 ms | Frame-stat ring |
| Worst frame after the first second | ≤ 33 ms | 50 ms | Frame-stat ring, max |
| Sim cost, 2 000 actors, multithreaded | ≤ 2.0 ms | 4.0 ms | In-engine phase timers |
| Render CPU (packet → submit), 5 000 draw items | ≤ 3.0 ms | 5.0 ms | In-engine phase timers |
| GPU frame | ≤ 5.0 ms | 10.0 ms | `timestamp-query` when available |
| Input → state applied | ≤ 2 frames | 3 frames | Instrumented input event |
| Engine memory, baseline play | ≤ 800 MiB | 1 200 MiB | `measureUserAgentSpecificMemory()` |
| Save write, 1 MiB slot, local | ≤ 80 ms p99 | 200 ms | Harness round trip |
| Frame impact of an autosave | ≤ 1 ms | 2 ms | Frame-stat ring around the write |

**Deferred — boot and load time.** The studio has explicitly deprioritised boot time
(`AD-25`): it is the *least* of the current concerns, and it is cheap to revisit
because it is a load-order and packaging problem rather than a runtime one. These
numbers are keystones from FS §17.1 and are still the targets we intend to hit, but
they are **measured, not gated**, until someone decides boot time has become a
problem:

| Metric | Target | Fail above | Note |
|---|---|---|---|
| Boot: splash → interactive (warm) | ≤ 2 000 ms | 4 000 ms | Depends mostly on content volume and request count |
| WASM compile (`instantiateStreaming`) | ≤ 800 ms | 1 500 ms | Kept small by `wasm-opt -Oz`; watch it, do not gate it |
| Root requests at boot | ≤ 150 | 400 | The FS §17.1 budget; drives §07.7 |
| Asset load, 200 MB content set, NVMe | ≤ 3 s | 6 s | Scales with content, not with the runtime |

The counters that make these measurable ship from day one (`§07.8`), so promoting a
deferred row to a gated one is a config change in the harness, not a new
instrumentation project. That is the whole point of deferring deliberately rather
than ignoring: the number is available whenever we want to look at it.

**The comparison that actually proves the thesis.** Shipping the same benchmark
scene as a native build and measuring both on the reference machine, with the browser
build required to land **within 15 % of native frame time** at 1080p. Note that the
claim is about *frame time* — sustained rendering cost — not about loading: a browser
build that loads a second slower but renders identically is a success by this bar.
Until that benchmark exists and is committed to CI, the thesis is unproven — so it is
milestone M2, not a later nicety.

## 07.2 What changed, and why

The first revision of this architecture accepted a single-threaded core because the
launcher did not emit `Cross-Origin-Embedder-Policy`, so `SharedArrayBuffer` was
unavailable. That is now fixed at the launcher: **`server.cross_origin_embedder_policy`
is implemented (FR-SRV-16)**, COOP `same-origin` was already sent, and with
`require-corp` the game origin is cross-origin isolated.

Three consequences follow, and they are the core of this document:

1. **WebAssembly threads are available**, so the simulation can use a real thread pool
   instead of a single core.
2. **`SharedArrayBuffer` is available**, so the sim and the renderer can share the
   render packet instead of copying it per frame.
3. **Cross-origin isolation is now a hard boot precondition.** The shell verifies
   `crossOriginIsolated` and fails with a plain-language cause (FR-SRV-16a, error
   matrix E2) rather than starting and failing later.

## 07.3 Concurrency architecture

```
┌─ main thread ─────────────────────────────────────────────────────────────┐
│ DOM UI, menus, sheet, dialogue, journal, editor UI, save/config transport │
│ postMessage only — never in the frame loop                                │
└───────────────┬───────────────────────────────────────────────────────────┘
                │ control messages (low frequency)
┌───────────────┴───────────────────────┐   SharedArrayBuffer: frame ring
│ sim worker                            │◄──────────────────────────────┐
│  game.wasm                            │                               │
│  ├─ main wasm thread (tick, ABI)      │                               │
│  └─ pthread pool (N-1 workers)        │                               │
│      parallel systems: movement, AI,   │                               │
│      spatial queries, visibility,      │                               │
│      batching, culling                 │                               │
└───────────────────────────────────────┘                               │
                                                                        │
┌───────────────────────────────────────┐                               │
│ render worker                         │───────────────────────────────┘
│  WebGPU device, pipelines, passes     │  Atomics frame flags: publish /
│  reads the packet in place — no copy  │  acquire / release, backpressure
│  OffscreenCanvas                      │
└───────────────────────────────────────┘
```

Design rules, each of which exists because of a failure mode it prevents:

- **The frame packet lives in shared memory.** The sim publishes frame *n* into ring
  slot `n % 3` and flips a flag with `Atomics.notify`; the renderer acquires the
  newest settled slot. Triple buffering means the sim never blocks on the renderer and
  the renderer never reads a torn packet. This replaces the copy that a
  transferable-`ArrayBuffer` design pays every frame (`AD-3`/`AD-4` in the first
  revision).
- **Exactly one writer per ring slot.** The sim owns the packet memory; the renderer
  owns GPU objects. The renderer never writes into the packet, and the sim never sees
  a GPU handle. That is still the "renderer is disposable" invariant — a lost device
  rebuilds from `asset_id`s in the packet with the sim unaware.
- **The main thread never runs game or render work.** It is DOM, input decoding and
  the data API. If a UI panel ever needs sim data, it asks for a snapshot, and the
  snapshot is a `postMessage` at UI frequency, never per frame.
- **Backpressure is explicit.** If the renderer is more than two frames behind, the
  ring's oldest unsent slot is dropped (the frame is skipped, not queued). Skipping a
  render frame is always preferable to growing latency; the sim never stalls to keep
  up with the display.
- **Single canonical worker count.** `SIM_THREADS = clamp(hardwareConcurrency - 2, 1, 8)`
  — one core left for the render worker, one for the main thread. The number is fixed
  at init and **recorded in the save's `meta`**, because it is part of determinism
  (§07.4).

## 07.4 Parallelism without losing determinism

Determinism (AD-6, AD-21) is what makes saves, replays and the golden-file harness
possible, and threads are its natural enemy. The rules are therefore strict, and they
are enforced by a test that runs the same command log with 1 thread and with N and
compares state hashes after every tick:

1. **Parallel systems are maps, never folds with side effects.** A parallel system
   reads a frozen snapshot of the tick's input state and writes only to its own
   entity's output slot. Entity `i`'s write can never be observed by entity `j`
   within the same system.
2. **Partition is fixed and derived from entity id**, not from chunk timing or a work
   queue. Worker `w` gets exactly `{i : i mod N == w}`. No work stealing, ever — a
   work-stealing scheduler makes the *order* of independent work nondeterministic,
   which leaks into any reduction that is not perfectly associative.
3. **Reductions are ordered explicitly.** Sums are integer and accumulated in index
   order per partition, then combined in partition order. Min/max use integer
   comparisons. No float atomics, no `+=` on a shared float, no `Atomic` on f32/f64.
4. **The RNG is splittable and pre-partitioned.** Each partition draws from a
   sub-stream seeded by `(tick, system, partition)`, so the draws do not depend on
   how work is scheduled. This is why the core uses PCG64/xoshiro with explicit
   splitting rather than one global stream.
5. **The thread count is part of the state.** A save records `sim_threads`; loading
   with a different count is allowed only because rule 4 makes streams
   count-independent — and the harness proves that by replaying the whole golden
   corpus at 1, 2, 4 and 8 threads.
6. **Nothing on the sim path allocates from a shared allocator during a tick.**
   Arenas are sized at load; a parallel system that allocates is a design error.

If a future system cannot satisfy rules 1–3, it runs single-threaded. Correctness
first; the profile decides what is worth parallelising.

**Granularity sets the size of that workload.** With the sector fixed at a 1 m tile
(`AD-33`), a district holds roughly **9× the cells** of the old 10-foot abstraction, with a
heightfield and blocking-height layer to match, and a sight-line supercover whose step count
grows about **3×** for the same physical distance (`02:02.12`). Visibility is the textbook
parallel workload precisely because it is a per-entity map with no cross-entity writes, so
rule 1 absorbs the cost; the budgets to watch are memory and load, not correctness
(`§07.5`, `§07.7`).

## 07.5 Memory

- **Shared memory is created once with a fixed maximum and never grown.**
  `WebAssembly.Memory({shared: true, maximum})` growth relocates nothing for
  already-shared views but forces every worker to re-acquire `memory.buffer`, which is
  a class of race that buys nothing here. Initial size is the campaign's declared
  working set; the ceiling is `engine.manifest.json`'s `memory.maximum_bytes`.
- **Structure-of-arrays throughout the sim.** Hot loops iterate `pos_x[]`, then
  `pos_y[]`, not an array of `Entity` structs, so SIMD and the cache both get a
  contiguous run. With `+simd128` available, spatial queries, visibility and culling
  are real SIMD work, not scalar loops with extra steps.
- **No GC pressure.** JS allocates nothing per frame: packet memory is in the arena,
  scratch buffers are pooled in the worker, and per-frame closures are hoisted. A GC
  pause during a frame is indistinguishable from a hitch, and the frame-stat ring will
  show it as a p99 violation.
- **The packet ring is sized from the manifest** and the sim *drops* draw items past
  capacity with a `packet_overflow` flag rather than growing (AD-6's "no allocation
  during a tick"). Dropping a sprite beats a hitch; the counter is surfaced in the
  perf overlay so it becomes a content or capacity decision, not a mystery.

## 07.6 GPU

WebGPU only (`AD-17`), which removes the WebGL2 constraints the earlier revision
carried: compute shaders and storage buffers are usable, and there is **one** shader
source language (`WGSL`) instead of two.

The rules that matter for a 2.5D scene with many sprites:

| Concern | Approach |
|---|---|
| Draw-call count | One instanced draw per `(layer, asset, blend)` batch; the sim emits batches already sorted, so the renderer does not sort per frame |
| Pipeline/bind-group churn | Built once at load, cached by `(shader, layout, target format)`; never created in the frame loop |
| Buffer traffic | One persistent instance buffer per ring slot, written with a single `writeBuffer` per batch; dynamic uniform offsets are 256-byte aligned (the WebGPU minimum) |
| Limits | `adapter.limits` is queried and `requiredLimits` requested explicitly. Defaults are modest — `maxBufferSize` 256 MiB, `maxStorageBufferBindingSize` 128 MiB per binding, `maxUniformBufferBindingSize` 64 KiB — so a large atlas or mesh pool must either request more or be split |
| Texture cost | Atlases with pre-built mip chains shipped as KTX2 (no runtime mip generation), `copyExternalImageToTexture` for runtime decode |
| Readback | Never on the render path. Screenshots and the editor's thumbnails are the only readbacks and they are user-initiated |
| GPU timing | `timestamp-query` when the adapter exposes it, feeding the same frame-stat ring as the CPU timers |
| Shader compilation stalls | All pipelines are created during the loading screen, so the first frame of gameplay is not the first time a shader is seen |

The presentation is **full 3D with a multi-profile camera** (`AD-30`), which moves real
cost onto the GPU and changes what the budget is spent on:

| Property | Effect on the frame budget |
|---|---|
| Skinned characters | The dominant new cost. One draw per character per material, plus a skinning pass. Use **GPU skinning** via compute (available because the engine is WebGPU-only) rather than CPU skinning; keep bones ≤ 80 and influences ≤ 4 (`08:08.2`) |
| Materials per character | Each extra material is a draw call multiplied by visible character count. The two-material ceiling (`08:08.4`) is a frame-budget rule disguised as an art rule |
| LODs | Authored LOD0/1/2 with a deterministic selection distance; LOD2 as a billboard cutout is the cheapest crowd win, and billboards are already a first-class kind (`AD-30`) |
| Draw calls | One instanced draw per `(layer, asset, blend)` batch for static geometry; characters batch only within a material. Target a few hundred draw calls in a combat scene, not a few thousand |
| Culling | **Coarse culling and LOD selection in the sim, fine frustum culling in the renderer** (`AD-30`). That keeps the packet bounded instead of shipping every entity every frame |
| Shadows | The biggest hidden cost. Static world geometry is **pre-lit / baked**; only characters and a small dynamic-light budget cast real-time shadows. Budget the shadow pass as its own line and cap the cascade count |
| Texture memory | KTX2 with mip chains, atlas per character, ≤ 2048² for characters. The BC7/BC5 variants cut memory roughly 4× where the adapter exposes `texture-compression-bc`; keep the uncompressed fallback (`08:08.5`) |
| Camera profiles | Camera *state* is sim-side and fixed-point; only the matrix is float. A camera move therefore costs nothing in the packet and nothing in determinism, but a **ground-level profile raises the visible triangle count**, so profiles must have declared budgets |
| The world seen from every angle | A rotatable or low camera cannot hide the back of anything, so level art and lighting work multiplies relative to a fixed camera — a production cost, not a frame cost, but it lands in the same schedule (`AD-30`) |

Elevation is mechanical (`AD-28`), which adds one cost worth watching: **line-of-sight and
cover are evaluated per observer per tick**. They are map-only passes over frozen state,
which is exactly the shape the parallel rules want (`§07.4`), but the result must be cached
per tick rather than recomputed per query, and the frame-stat ring should carry the
visibility phase as its own counter so a regression is attributable. In 3D this cost grows
with the number of *aiming* entities, not with the scene, so it scales with combat size
rather than with level size.

## 07.7 I/O and load time (deferred)

**This section is deliberately not driving any current design work.** Boot and asset
loading are the studio's lowest priority (`AD-25`), so v1 ships **loose content files**
under `assets/`, which is also the most transparent and most moddable arrangement
(`AD-7`, `AD-18`). Nothing is bundled, and no in-house container format is invented.

The problem to keep in mind, for the day boot time *does* matter: the launcher serves
HTTP/1.1 over loopback, and Chrome does not implement cleartext HTTP/2 (`h2c`), so
per-origin parallelism is capped at roughly six connections. Hundreds of small content
files therefore cost hundreds of round trips, serialised six at a time, and FS §17.1
budgets **≤ 150 root requests at boot** (FR-AST-10 bundling is the specified answer and
is **not implemented** in the packager). Two multipliers matter: content file count and
**installed mod count**, since each mod adds its own assets.

When it becomes a problem, the fix is already designed and needs no new architecture:

1. **A content pak.** One `assets/<set>.pak` plus a small index, read with **HTTP
   `Range` requests** — which the launcher already supports natively (FR-SRV-14,
   `server.max_range_bytes`, default 8 MiB). A few large sequential ranges cover the
   manifest and hot content; the rest streams on demand. Mods ship their own pak, and
   the overlay map (`03:03.4`) resolves to a pak inside the mod's `assets/` subtree.
2. **Content-addressed pak paths** are served `immutable` (FR-SRV-15), so a returning
   player pays no requests for unchanged content.
3. **The upstream fix, if it lands:** FR-AST-10 bundling, which makes the game-side pak
   redundant and deletable (U5).

The cheap I/O wins that cost nothing and are already in the design stay in v1:
`instantiateStreaming` for the wasm (so compilation overlaps the download), and image
decode via `createImageBitmap` **inside the worker** so a decode never blocks a frame.
The VFS API is bundle-oblivious, so adding a pak reader later touches one module and no
call sites.

## 07.8 Measurement

An optimization claim that does not name the counter it improves is not accepted
(FS §17.4). The counters:

- **Frame-stat ring** in the engine: p50/p95/p99/max frame time, per-phase timings
  (sim, parallel-systems, packet build, render CPU, GPU), dropped-frame count,
  `packet_overflow` count, GC pause detection. Always on, cheap, exposed in the perf
  overlay and in `/__kobra/diagnostics`.
- **Determinism harness** (`tests/replay`): state hashes at 1/2/4/8 threads.
- **Perf harness** (`tests/perf`): the benchmark scene, run headless in CI on the
  reference machine, comparing against committed budgets; a regression fails the
  build.
- **Request counter**: root requests and bytes at boot, asserted against the budget.
- **Memory**: `measureUserAgentSpecificMemory()` at a fixed point in the benchmark.
- **The native comparison**: the same scene built natively, measured on the same
  machine, with the ratio recorded per release so drift is visible.

**What `tests/perf/perf.mjs` measures today.** Before there is a browser drive,
the harness reports the numbers that are honest outside a browser and are
available from the packaged artifact: the built `game.wasm`'s byte size, the host
cost of booting it (compile + init + content load), the sim step and state-hash
cost over the real ABI, and the save envelope size for the committed scripted
encounter. It **gates exactly one**: the save envelope against `AD-11`'s 512 KiB
target / 4 MiB ceiling (`R6`). Everything else is reported, never asserted —
boot and load time are deferred (`AD-25`) — and the browser-only rows above are
not claimed here. `tests/perf/boot-requests` is the companion gate for the
request row: it drives the real loader and overlay with `fetch` stubbed, reports
the `1 (boot document) + N (mods) + M (packs) + S (Tier-2 scripts)` decomposition,
and fails a shape regression that grows with `N × M` (`AD-18`), while leaving the
≤ 150 target ungated. The `S` term is one fetch per declared script, because that
fetch is what the hash `AD-8` records is computed from (`03:03.7`).

## 07.9 Launcher-side performance

The launcher is already the right shape for this: saves are written off the game
thread by the Go server (FS §17.2's ≤ 1 ms frame impact), `ETag`/`304` and immutable
caching keep repeat loads free (FR-SRV-15), `Range` is native (FR-SRV-14), and asset
throughput targets are 300 MB/s on SSD / 60 MB/s on USB (FS §17.1). Nothing in this
document requires the launcher to get faster; two things would help, and both are
already listed as upstream requests:

| # | Request | Performance effect |
|---|---|---|
| U5 | Implement FR-AST-10 bundling | Removes the need for the game-side pak — **only relevant once boot time matters** (`§07.7`) |
| U6 | **Completed.** COEP is implemented (`server.cross_origin_embedder_policy`) | Unlocks wasm threads and `SharedArrayBuffer` — the whole of §07.3 |

## 07.10 Risks

| # | Risk | Mitigation |
|---|---|---|
| P1 | A parallel system breaks determinism subtly | The rules in §07.4 are mechanical, and the 1-vs-N-thread hash test runs on every golden fixture |
| P2 | `crossOriginIsolated` is false because a publisher misconfigured COEP | The shell checks at boot and fails with a named cause (FR-SRV-16a); the shipped config sets `require-corp` |
| P3 | COEP breaks an embedded cross-origin resource | There are none by design (AD-19: no network); `assets/` is entirely same-origin |
| P4 | Kernel/pipeline compile stalls on the first gameplay frame | All pipelines are built during the loading screen, and the frame-stat ring would flag a first-second worst frame |
| P5 | Thread count varies by machine and a save leaks the difference | `sim_threads` is recorded, sub-streams are count-independent, and the harness replays at 1/2/4/8 |
| P6 | ~~Content pak becomes an unmaintainable in-house format~~ | **Not applicable yet** — no pak is built in v1 (`§07.7`). If one is introduced, it stays minimal (index + concatenated blocks + `Range`), is isolated in `vfs`, and is deleted when FR-AST-10 lands |
| P8 | Deferring boot time means the first content-heavy build surprises us | The request, byte and load-time counters ship from day one (`§07.8`), so the number is available before it is a crisis; the pak design is ready and touches one module (`§07.7`) |
| P7 | The native comparison is unflattering on some content | That is the point of measuring it: it converts "is the web fast enough" from an opinion into a number, early, while it is still cheap to change the content mix |
