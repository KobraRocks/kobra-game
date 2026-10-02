# 07 — Performance architecture

Performance is the **master goal**. The engine is native (`AD-38`), so the bar is no
longer "is a browser fast enough" — that thesis is retired with the web runtime — but
"does the game hold its frame budget on the reference machine". A build that misses a
budget is a defect, and an optimisation claim that does not name the counter it
improves is not accepted.

This document states the acceptance bar, the parallelism and memory rules that meet it,
and the measurement discipline that keeps it honest. `AD-6`, `AD-21` and `AD-24` are the
decisions behind it.

## 07.1 The acceptance bar

The reference machine: 4-core x86-64 ≥ 2.5 GHz, 16 GiB, NVMe, clean profile, release
build, no debugger attached.

**Gated — the runtime bar.** These numbers decide whether the game feels native:

| Metric | Target | Fail above | How it is measured |
|---|---|---|---|
| Frame rate, benchmark scene, 1080p | 120 FPS sustained | 60 FPS | In-engine frame stats, p50 |
| Frame time p99 | ≤ 8.3 ms (120 Hz) | 16.6 ms | Frame-stat ring |
| Worst frame after the first second | ≤ 33 ms | 50 ms | Frame-stat ring, max |
| Sim cost, 2 000 actors, multithreaded | ≤ 2.0 ms | 4.0 ms | In-engine phase timers |
| Render CPU (frame description → submit), 5 000 draw items | ≤ 3.0 ms | 5.0 ms | In-engine phase timers |
| GPU frame | ≤ 5.0 ms | 10.0 ms | Timestamp queries when the device exposes them |
| Input → state applied | ≤ 2 frames | 3 frames | Instrumented input event |
| Engine memory, baseline play | ≤ 800 MiB | 1 200 MiB | The host's allocation counters |
| Save write, 1 MiB slot, local | ≤ 80 ms p99 | 200 ms | Harness round trip |
| Frame impact of an autosave | ≤ 1 ms | 2 ms | Frame-stat ring around the write |

**Deferred — boot and load time.** Boot time is explicitly deprioritised (`AD-25`): it is
the least of the current concerns and cheap to revisit, because it is a load-order
problem rather than a runtime one. These targets are measured, not gated, until someone
decides boot time has become a problem:

| Metric | Target | Fail above | Note |
|---|---|---|---|
| Boot: splash → interactive (warm) | ≤ 2 000 ms | 4 000 ms | Depends mostly on content volume |
| Asset load, 200 MB content set, NVMe | ≤ 3 s | 6 s | Scales with content, not with the runtime |

The counters that make these measurable ship from day one (`§07.8`), so promoting a
deferred row to a gated one is a harness-configuration change, not a new instrumentation
project.

## 07.2 Threads and isolation (retired)

**Retired by `AD-38`.** This section described why the browser core could be
multithreaded: `server.cross_origin_embedder_policy` made the game origin cross-origin
isolated, which made `SharedArrayBuffer` and wasm threads available, and the shell
verified `crossOriginIsolated` at boot.

None of it applies: a native engine has threads and shared memory by construction, and
there is no origin, no embedder policy and no isolation check.

## 07.3 Concurrency architecture (retired)

**Retired by `AD-38`.** This section described the two-worker topology — a simulation
worker holding the wasm module and a pthread pool, a render worker holding the GPU
device, and a three-slot shared-memory frame ring published and acquired with `Atomics`.

The native host's concurrency is not written yet. Two rules from that design survive and
bind whatever replaces it:

- **One writer per buffer.** The simulation owns the frame description; the renderer owns
  GPU objects. The renderer never writes into the frame description, and the simulation
  never sees a GPU handle — the "renderer is disposable" invariant in
  [README.md](README.md).
- **Backpressure is explicit.** Dropping a frame is always preferable to growing latency,
  and the simulation never stalls to keep up with the display.

## 07.4 Parallelism without losing determinism

Determinism (`AD-6`, `AD-21`) is what makes saves, replays and the golden-file harness
possible, and threads are its natural enemy. The rules are therefore strict, and they are
enforced by a test that runs the same command log with 1 thread and with N and compares
state hashes after every tick:

1. **Parallel systems are maps, never folds with side effects.** A parallel system reads a
   frozen snapshot of the tick's input state and writes only to its own entity's output
   slot. Entity `i`'s write can never be observed by entity `j` within the same system.
2. **Partition is fixed and derived from entity id**, not from chunk timing or a work
   queue. Partition `w` gets exactly `{i : i mod N == w}`. No work stealing, ever — a
   work-stealing scheduler makes the *order* of independent work nondeterministic, which
   leaks into any reduction that is not perfectly associative.
3. **Reductions are ordered explicitly.** Sums are integer and accumulated in index order
   per partition, then combined in partition order. Min/max use integer comparisons. No
   float atomics, no `+=` on a shared float, no atomic on f32/f64.
4. **The RNG is splittable and pre-partitioned.** Each partition draws from a sub-stream
   seeded by `(tick, system, partition)`, so the draws do not depend on how work is
   scheduled. This is why the core uses PCG64/xoshiro with explicit splitting rather than
   one global stream.
5. **The thread count is part of the state.** A save records `sim_threads`; loading with a
   different count is allowed only because rule 4 makes streams count-independent — and
   the harness proves that by replaying the whole golden corpus at 1, 2, 4 and 8 threads.
6. **Nothing on the simulation path allocates from a shared allocator during a tick.**
   Arenas are sized at load; a parallel system that allocates is a design error.

If a system cannot satisfy rules 1–3, it runs single-threaded. Correctness first; the
profile decides what is worth parallelising.

**Granularity sets the size of that workload.** With the sector fixed at a 1 m tile
(`AD-33`), a district holds roughly **9× the cells** of the old 10-foot abstraction, with
a heightfield and blocking-height layer to match, and a sight-line supercover whose step
count grows about **3×** for the same physical distance (`02:02.12`). Visibility is the
textbook parallel workload precisely because it is a per-entity map with no cross-entity
writes, so rule 1 absorbs the cost; the budgets to watch are memory and load, not
correctness (`§07.5`).

## 07.5 Memory

- **Structure-of-arrays throughout the simulation.** Hot loops iterate `pos_x[]`, then
  `pos_y[]`, not an array of entity structs, so SIMD and the cache both get a contiguous
  run. Spatial queries, visibility and culling are real vector work, not scalar loops with
  extra steps.
- **Arenas are sized at load and never grown during a tick.** Rule 6 of `§07.4` is the
  reason: an allocation on the simulation path is a design error, not a slow path.
- **The frame description is sized at init** and the simulation *drops* draw items past
  capacity with a `packet_overflow` flag rather than growing (`AD-6`). Dropping a draw
  item beats a hitch, and the counter is surfaced so it becomes a content or capacity
  decision rather than a mystery.
- **No per-frame allocation anywhere on the render path.** Scratch buffers are pooled;
  a per-frame allocation is a hitch waiting for the allocator to be unlucky.

## 07.6 GPU

One backend (`AD-38`), one shader language — **WGSL** — and compute and storage buffers
available, so there are no compatibility carve-outs in the plan.

| Concern | Approach |
|---|---|
| Draw-call count | One instanced draw per `(layer, asset, blend)` batch; the simulation emits batches already sorted, so the renderer does not sort per frame |
| Pipeline/bind-group churn | Built once at load, cached by `(shader, layout, target format)`; never created in the frame loop |
| Buffer traffic | One persistent instance buffer per frame slot, written with a single transfer per batch; dynamic offsets respect the device's alignment |
| Limits | Device limits are queried and requested explicitly rather than assumed; a large atlas or mesh pool must either ask for more or be split |
| Texture cost | Atlases with pre-built mip chains shipped as KTX2 (no runtime mip generation) |
| Readback | Never on the render path. Screenshots are the only readback and they are user-initiated |
| GPU timing | Timestamp queries when the device exposes them, feeding the same frame-stat ring as the CPU timers |
| Shader compilation stalls | All pipelines are created during the loading screen, so the first frame of gameplay is not the first time a shader is seen |

The presentation is **full 3D with a multi-profile camera** (`AD-30`), which moves real
cost onto the GPU and changes what the budget is spent on:

| Property | Effect on the frame budget |
|---|---|
| Skinned characters | The dominant new cost. One draw per character per material, plus a skinning pass. Use **GPU skinning** via compute rather than CPU skinning; keep bones ≤ 80 and influences ≤ 4 (`08:08.2`) |
| Materials per character | Each extra material is a draw call multiplied by visible character count. The two-material ceiling (`08:08.4`) is a frame-budget rule disguised as an art rule |
| LODs | Authored LOD0/1/2 with a deterministic selection distance; LOD2 as a billboard cutout is the cheapest crowd win, and billboards are already a first-class kind (`AD-30`) |
| Draw calls | One instanced draw per `(layer, asset, blend)` batch for static geometry; characters batch only within a material. Target a few hundred draw calls in a combat scene, not a few thousand |
| Culling | **Coarse culling and LOD selection in the simulation, fine frustum culling in the renderer** (`AD-30`). That keeps the frame description bounded instead of shipping every entity every frame |
| Shadows | The biggest hidden cost. Static world geometry is **pre-lit / baked**; only characters and a small dynamic-light budget cast real-time shadows. Budget the shadow pass as its own line and cap the cascade count |
| Texture memory | KTX2 with mip chains, atlas per character, ≤ 2048² for characters. BC7/BC5 variants cut memory roughly 4× where the device supports them; keep an uncompressed fallback (`08:08.5`) |
| Camera profiles | Camera *state* is simulation-side and fixed-point; only the matrix is float. A camera move therefore costs nothing in the frame description and nothing in determinism, but a **ground-level profile raises the visible triangle count**, so profiles must have declared budgets |
| The world seen from every angle | A rotatable or low camera cannot hide the back of anything, so level art and lighting work multiplies relative to a fixed camera — a production cost, not a frame cost, but it lands in the same schedule (`AD-30`) |

Elevation is mechanical (`AD-28`), which adds one cost worth watching: **line-of-sight and
cover are evaluated per observer per tick**. They are map-only passes over frozen state,
which is exactly the shape the parallel rules want (`§07.4`), but the result must be
cached per tick rather than recomputed per query, and the frame-stat ring should carry the
visibility phase as its own counter so a regression is attributable. In 3D this cost grows
with the number of *aiming* entities, not with the scene, so it scales with combat size
rather than with level size.

## 07.7 I/O and load time (deferred, and retired in form)

**Boot and asset loading are the lowest priority** (`AD-25`), so content ships as loose,
readable files and nothing is bundled. That much survives.

**Retired by `AD-38`:** the rest of this section described the loopback HTTP server's
connection cap, the boot request budget, `Range` requests, `immutable` caching and a
content-pak design. There is no server, no request counter and no pak. When boot time
becomes a problem, the design starts from a file read rather than a request, and nothing
here constrains it.

## 07.8 Measurement

The counters that make the bar real, and each one is part of the build rather than a
later instrumentation project:

- **Frame-stat ring**, always on and cheap: p50/p95/p99/max frame time, per-phase timings
  (simulation, parallel systems, frame description, render CPU, GPU), dropped-frame count,
  `packet_overflow` count, and the visibility phase as its own line.
- **Determinism harness** (`crates/kobra-core/tests/`): state hashes at 1/2/4/8 threads,
  and the sample game's pinned hashes — the engine's own end-to-end case.
- **A game's conformance suite**: its golden replays, run against the engine.
- **Perf harness**: the benchmark scene on the reference machine, compared against
  committed budgets; a regression fails the build.
- **Memory**: the host's allocation counters at a fixed point in the benchmark.

## 07.9 Launcher-side performance (retired)

**Retired by `AD-42`.** This section described the launcher's save writes, `ETag`/`304`
caching, `Range` support and asset throughput targets, and the upstream requests (U5, U6)
that went with them.

Saves are written in-process now, and there is no server. The one rule worth carrying
forward is the frame-impact budget for an autosave, which `§07.1` gates at ≤ 1 ms.

## 07.10 Risks

| # | Risk | Mitigation |
|---|---|---|
| P1 | A parallel system breaks determinism subtly | The rules in `§07.4` are mechanical, and the 1-vs-N-thread hash test runs on every golden fixture |
| P4 | Kernel/pipeline compile stalls on the first gameplay frame | All pipelines are built during the loading screen, and the frame-stat ring would flag a first-second worst frame |
| P5 | Thread count varies by machine and a save leaks the difference | `sim_threads` is recorded, sub-streams are count-independent, and the harness replays at 1/2/4/8 |
| P8 | Deferring boot time means the first content-heavy build surprises us | The load-time and memory counters ship from day one (`§07.8`), so the number is available before it is a crisis |
