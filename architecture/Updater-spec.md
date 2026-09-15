# Technical Specification: The Kobra Updater

**Document status:** Authoritative for update *execution*. Normative for the mechanism by
which an installed game folder advances from one release to the next.
**Consumes:** Functional Specification (FS) §12, §16, §19; Launcher spec §19, §4, §23, §26;
Packaging spec §8, §9.
**Audience:** the implementer of `launcher/internal/update` and the driver of the
release build.

> **Implementation status.** Implemented in the launcher, with the amendment set of
> §20 applied. Three places where the build corrected this document are marked in line:
> the staging trees live in the sidecar rather than the game folder (§5, R9.9), the
> launcher tree is merged in place by a separate pipeline step (§9.2 step 12a, R9.10),
> and R14.4's forward-compatibility promise for unknown config keys is defeated by the
> config schema's `additionalProperties: false` (R14.4's status note, §22 P5). The
> end-to-end proof is the apply phase of `testgame/run.sh`, whose assertions are the
> §18.1 table.

> **Scope note.** Three documents already exist and none of them owns this
> subject. The FS says *what must be true* after an update (FR-UPD-1…9) and
> states the error texts. The Launcher spec describes the five functions
> (`GuardApply`, `DownloadArchive`, `Verify`, `Extract`, `Swap`, `Recover`) and
> their contracts, but never composes them. The Packaging spec says what a
> publisher must produce. This document is the missing fourth: the **execution
> model** that binds them — when an update starts, what writes the intent down,
> what survives a crash, who owns each byte, and how the whole thing is proven
> end to end.
>
> Where this document and another disagree, this document wins for *execution*;
> the other documents win for their own subject matter (schema, package
> contents, user-facing requirements). Every such conflict is listed in
> §16.2 rather than left implicit.

---

## Table of Contents

1. [Overview and Decisions](#1-overview-and-decisions)
2. [The Two Channels, Corrected](#2-the-two-channels-corrected)
3. [Normative Language and Definitions](#3-normative-language-and-definitions)
4. [Component Map](#4-component-map)
5. [Artifact Ownership and On-Disk Layout](#5-artifact-ownership-and-on-disk-layout)
6. [The Trigger State Machine](#6-the-trigger-state-machine)
7. [Startup Integration](#7-startup-integration)
8. [The Downloader](#8-the-downloader)
9. [The Apply Pipeline](#9-the-apply-pipeline)
10. [Disk-Space Preflight](#10-disk-space-preflight)
11. [Progress, Result, and the Query Surface](#11-progress-result-and-the-query-surface)
12. [Cancellation](#12-cancellation)
13. [Trust Model for the Patch Channel](#13-trust-model-for-the-patch-channel)
14. [Configuration Merge Across a Swap](#14-configuration-merge-across-a-swap)
15. [Rollback Retention](#15-rollback-retention)
16. [Error Semantics and Taxonomy](#16-error-semantics-and-taxonomy)
17. [Concurrency and Locking](#17-concurrency-and-locking)
18. [Test Plan](#18-test-plan)
19. [Deferred Work](#19-deferred-work)
20. [Amendment Set](#20-amendment-set)
21. [Traceability](#21-traceability)
22. [Open Items That Need a Product Decision](#22-open-items-that-need-a-product-decision)

---

## 1. Overview and Decisions

### 1.1 The Gap This Document Closes

The launcher contains every primitive an update needs and no update. Concretely,
at the time of writing:

| Primitive | State |
|---|---|
| `FetchLatest`, `FetchManifest`, `fetchDocument` | Implemented, tested, live on `GET /api/update/check` |
| `GuardApply`, `Verify`, `Extract`, `Swap`, `Recover` | Implemented, unit-tested, **zero non-test callers** |
| Archive download | **Absent** |
| Orchestration | **Absent** |
| Pending-update intent | **Absent** — `POST /api/update/apply` answers `202` and does nothing |
| Progress | **Absent** |
| Retention counter (FR-UPD-8) | **Absent** — `game.old/` is kept forever |
| End-to-end proof | **Absent** |

The result is a launcher that advertises an assisted patch path it cannot
perform. This document specifies the smallest complete mechanism that makes the
advertisement true.

### 1.2 The Decisions, Stated Up Front

**D1 — The apply point is the next start, not the current session.**
An explicit user action records intent and exits; the *next* launcher start
performs download → verify → extract → swap **before the HTTP server binds**.

Rejected alternative: *inline apply* — download and swap inside the running
session. It is strictly worse here:

- the server is serving `game/` at the moment the swap replaces it, so inline
  apply needs a quiesce protocol (stop serving, drain in-flight reads, re-bind)
  that the process model does not have;
- a failure mid-download leaves a running game holding a stale manifest and a
  half-written `.part`;
- it needs resolution authority the current architecture deliberately withholds
  (FS §7.6 forbids WebSocket, so progress would have to be polled from the same
  process that is trying to replace the files it is serving);
- it contradicts the contract `POST /api/update/apply` already publishes.

The restart model costs one line of user-visible copy ("the update is installed
the next time the game starts") and buys crash-safety for free, because it runs
the §19.4 state machine at the only moment in the process lifetime when `game/`
is not being served.

**Consequence, accepted deliberately.** Packaging spec §9.4 requires
in-browser byte-and-percent progress while downloading. Under D1 the download
does not happen while a browser is open, so that requirement is **deferred**, not
satisfied (§19.1). What replaces it is a recorded, replayable progress record
(§11) so the requirement can be met without redesign if a future release moves
the download in-session.

**D2 — Signatures are recommended, not required, for the patch channel.**
Launcher spec §19.3 step 2 and Packaging spec §8.6 rule 5 currently make the
`patch` channel require publisher signing *and* an installed verifier. Rule 3 of
the same section makes the launcher fail closed on a declared signature with no
verifier. Since no verifier ships, the only configuration that works today is an
**unsigned** patch — which §8.6 rule 5 declares non-conforming. The two rules
cannot both hold.

Resolution: for V1 the `patch` channel accepts an unsigned manifest, and
signing becomes a recommended hardening step (§13). The integrity claim is
explicitly weakened and stated: `archive.hash` plus the `files[]` index detects
corruption, truncation and mirror tampering; it does **not** detect a
compromised origin. Publishing that residual risk is better than shipping an
OpenPGP parser the project has decided it cannot maintain.

**D3 — `launcher_min`, not the channel, is the only hard refusal.** A release
whose `launcher_min` exceeds the installed launcher is refused (E29) and the
current release keeps working. Nothing else about an update is fatal to startup
(§7.3).

---

## 2. The Two Channels, Corrected

Packaging spec §8.1 defines the channel axis; this section restates the parts an
implementer must get right, and corrects the one contradiction.

| `update.channel` | Launcher behaviour | Cryptography in the launcher |
|---|---|---|
| `manual` (default) | Never downloads. `GET /api/update/check` may still read `latest.json` and report. The user replaces files from the ZIP. | None |
| `patch` | The full mechanism in this document. | None in V1; a verifier may be injected via `update.SetSignatureVerifier` |
| `none` | No outbound request at all; `check` reports that checks are disabled. | None |

**R2.1.** A manifest that declares `signatures.gpg` MUST be refused when no
verifier is installed. This is Launcher spec §19.3 step 2's fail-closed rule and
it is unchanged by D2 — the launcher never silently accepts a signature it
cannot check.

**R2.2.** A manifest that declares **no** signature MUST be accepted by the
`patch` path when the remaining verification in §9.3 passes. This is the
correction to Packaging spec §8.6 rule 5 and to §15.3's "obliges them to sign
manifests".

**R2.3.** Packaging MUST NOT publish `signatures.gpg` on a release intended for
the `patch` channel unless the shipping launcher build contains a verifier.
Doing so makes the release unappliable by the very path that is supposed to
apply it. The build SHOULD enforce this as a gate (Packaging spec §11.2).

**R2.4.** `SHA256SUMS` and `SHA256SUMS.sig` remain mandatory artefacts for human
verification (Packaging spec §7.1). They are part of the manual path and are not
read by the launcher.

**R2.5.** Every release consumed by the `patch` channel MUST declare
`archive.hash` and `archive.size`, and SHOULD declare a complete `files[]`
index. A manifest declaring neither `archive.hash` nor the flat
`archive_sha256` alias is refused rather than downgraded to per-file
verification (existing `verify.go` behaviour, retained).

---

## 3. Normative Language and Definitions

**R3.1.** MUST, MUST NOT, SHOULD, MAY are used in the RFC 2119 sense. "Release"
is always a date-version identifier of the form `YYYY.MM.P` (Packaging spec
§4.2).

| Term | Meaning |
|---|---|
| **Game folder** | The directory containing `game/`, `launcher/`, `data/`, resolved from the executable path (Launcher spec §5.1) |
| **Sidecar directory** | Machine-local, per-game, outside the game folder (Launcher spec §6.1) |
| **Installed release** | The release the *running* folder reports, resolved per §6.2 |
| **Target release** | The release named by the pending marker |
| **Staging root** | `<sidecar>/update/`, holding `game.new/` and `launcher.new/` |
| **Backup** | `<game folder>/game.old/` |
| **Swap marker** | `<game folder>/update.state`, owned by Launcher spec §19.4 |
| **Pending marker** | `<sidecar>/update/pending.json`, owned by this document |
| **Update directory** | `<sidecar>/update/` — machine-local substrate for the download, progress, results, retention and cancel flag |
| **Apply** | One execution of §9 from a validated pending marker to a completed swap |
| **Started successfully** | The launcher reached `SERVE` and the shell sent at least one heartbeat (Launcher spec §19.6) |

**R3.2.** The update directory is created lazily with mode `0700` and contains
only artifacts this document names. An update MUST NOT create, modify or delete
anything inside the game folder except: the backup directory, the swap marker,
and the release-owned files of the swap and launcher merge themselves. The
staging trees are **not** in the game folder; R5.3 explains why, and §9.2 step 10
names their locations.

**R3.3.** No path in an update error message may contain an absolute path, a
user name, or the game folder location (Launcher spec §22.4). Diagnostics and
events are the only channel for those values.

---

## 4. Component Map

| Component | Location | Status | Spec |
|---|---|---|---|
| `UpdateCheck` (proxy) | `internal/server/update.go`, `internal/dataapi/handlers.go` | Exists | Launcher §19.2 |
| `Check`, `FetchLatest`, `FetchManifest` | `internal/update/check.go`, `manifest.go` | Exists | Launcher §19.2 |
| `GuardApply` | `internal/update/check.go` | Exists | Launcher §19.5 |
| `Verify` | `internal/update/verify.go` | Exists | Launcher §19.3 |
| `Extract` | `internal/update/extract.go` | Exists | Launcher §19.3 |
| `Swap` | `internal/update/swap.go` | Exists | Launcher §19.4 |
| `Recover` | `internal/update/swap.go` | Exists | Launcher §19.4 |
| **`DownloadArchive`** | `internal/update/download.go` | **New** | §8 |
| **`Apply`** | `internal/update/apply.go` | **New** | §9 |
| **Pending marker** | `internal/update/pending.go` | **New** | §6 |
| **Startup hook** | `cmd/kobra-launcher/main.go` | **New call site** | §7 |
| **Progress/result record** | `internal/update/progress.go` | **New** | §11 |
| **Retention counter** | `internal/update/retention.go` | **New** | §15 |
| **Config merge** | `internal/update/configmerge.go` | **New** | §14 |
| **`GET /api/update/progress`** | `internal/dataapi/handlers.go` | **New route** | §11 |

**R4.1.** All outbound network access remains confined to
`internal/update` (Launcher spec §3.3). The apply pipeline is a method on the
`update` package, not a new package, and it takes no `context` from an HTTP
request.

**R4.2.** `Apply` MUST be callable with a nil logger; logging goes through the
existing `update.SetLogger` hook.

---

## 5. Artifact Ownership and On-Disk Layout

```
<game folder>/
  game/                      # replaced by the swap
  game.old/                  # backup; retained per §15
  update.state               # swap marker; owned by §19.4
  launcher/                  # merged in place; never swapped away
  data/                      # NEVER touched

<sidecar>/update/
  pending.json               # intent; survives exit
  game.new/                  # staged game tree, awaiting the swap
  launcher.new/              # staged launcher tree, awaiting the merge
  download.part              # the in-flight archive; never mistaken for complete
  download.meta.json         # what download.part is, and how far it got
  progress.json              # polled by §11; overwritten atomically each tick
  result.json                # last apply outcome; read by §11 on the next boot
  cancel                     # flag file; presence requests cancellation
  started.json               # retention counter (§15)
  apply.lock                 # exclusivity (§17)
```

The two staging trees are sidecar state, not game-folder state. `game.new/` is what
`Swap` renames over `game/`, so it holds the game tree's *contents* with the archive's
`game/` prefix already stripped; `launcher.new/` is a staging copy that §9.2 step 12
merges into the live `launcher/` directory. Extracting the published archive flat would
produce `game.new/game/index.html`, which the swap would install as
`game/game/index.html` — a silently nested, unrunnable release — so the routing is
normative (R9.6, R9.9).

**R5.1.** The archive MUST be downloaded to `download.part` and renamed to a
terminal name **only** after `Verify` succeeds. A file whose name does not end in
`.part` is either absent or verified; there is no third state.

**R5.2.** `download.part` SHOULD be removed after `Verify` succeeds. Once the
archive is verified there is no reason to keep it, and removing it before
extraction relaxes the disk-space peak (§10).

**R5.3.** The pending marker, progress record, result record, retention counter
and cancel flag live in the sidecar directory, **not** in the game folder. Three
reasons, in order of weight:

1. a USB folder copied to another machine MUST NOT carry another machine's
   interrupted download;
2. the game folder is the portable artefact (FS §5.1) and machine-local state
   already has a home (Launcher spec §6);
3. `data/` must never be a destination for update state (FR-UPD-7), and keeping
   state out of the game folder entirely makes that impossible by construction.

**R5.4.** Every write to a sidecar update artifact is atomic: write
`<name>.tmp` in the same directory, `fsync` the file, `rename` over the target,
`fsync` the directory (Launcher spec §15.3). A reader never observes a partial
record.

**R5.5.** No artifact in the update directory may be world-readable. The
directory is `0700`; the mode of files is `0600`.

---

## 6. The Trigger State Machine

### 6.1 Recording Intent

**R6.1.** `POST /api/update/apply` is the only way an update is requested. It
MUST:

1. validate session and CSRF (existing pipeline);
2. enforce the one-per-session rate limit (existing);
3. call `UpdateCheck` and refuse when no update is available, with the existing
   `{"applied": false, "detail": "already current"}` body and `200`;
4. **write `pending.json`** atomically (`R6.2`);
5. resolve only if the marker is durable on disk.

**R6.2.** `pending.json` MUST contain at least:

```json
{
  "schema": "kobra.update-pending/1",
  "from_release": "2026.09.1",
  "target_release": "2026.10.1",
  "manifest_url": "https://…/pkgtest-2026.10.1.release.manifest.json",
  "archive_url": "https://…/pkgtest-2026.10.1-linux-x64.tar.zst",
  "archive_size": 3467509,
  "archive_hash": "sha256:7f86…04ec",
  "launcher_version": "0.1.0",
  "requested": "2026-09-15T16:54:12Z"
}
```

The URLs and the hash are recorded **as observed at request time** so the
confirmation the user saw ("34 MB, 2026.10.1") is the thing that will be
fetched. The apply step re-validates them against a freshly fetched manifest
(R9.2); a mismatch is not silent (§16.3 E35).

**R6.3.** Writing the marker MUST NOT depend on the game exiting cleanly. A
`kill -9` immediately after the `202` MUST still leave the update pending.

**R6.4.** The response MUST be `202 Accepted` carrying `target_release` and
`archive_size` so the shell can render "34 MB · installs on next start" without
a second request.

### 6.2 Clearing Intent

**R6.5.** The marker is removed exactly when the apply reaches a terminal
outcome:

| Outcome | Marker |
|---|---|
| Swap completed | Removed |
| E35 manifest drift (R9.2) | Removed, with `result.json` explaining |
| `launcher_min` no longer satisfied (E29) | Removed |
| Archive hash/size mismatch (E28) | Removed, with `result.json` |
| Cancelled by the user (§12) | Removed |
| Non-retryable guard failure | Removed |
| Network unreachable / timeout | **Retained** for one retry |
| Disk space insufficient (E32) | **Retained** — the user may free space |
| Unrecognised or corrupt marker | Removed, with a warning event |

**R6.6.** A retained marker MUST carry a `retry_count`. When `retry_count`
reaches **3**, the marker MUST be removed and `result.json` MUST be written with
a retryable failure, so a folder that can never download does not attempt a
network fetch on every launch forever.

**R6.7.** Deleting the marker is always safe. An update whose intent is lost is
simply an update that was not requested; no on-disk state is corrupted.

### 6.3 State Machine

```
                   ┌──────────┐
   POST apply ────▶│ PENDING  │  pending.json present, swap marker absent
                   └────┬─────┘
        startup, valid │  startup, invalid / retries exhausted / cancelled
                        ▼                                     ▼
                   ┌──────────┐                          ┌────────┐
                   │ APPLYING │                          │ FAILED │  result.json
                   └────┬─────┘                          └────────┘
        ┌───────────────┼───────────────┐
        │               │               │
   verify fail     swap started     swap done
   (E28/E30/E35)   (marker written)      │
        │               │                ▼
        ▼               ▼          ┌────────────┐
   ┌────────┐     ┌──────────┐     │ COMPLETED  │  result.json + retention armed
   │ FAILED │     │ CRASHED  │     └────────────┘
   └────────┘     └────┬─────┘
                       │ next startup, before anything else
                       ▼
                  Recover() → rolled forward or rolled back  (§7.2)
```

**R6.8.** `PENDING` with a swap marker present is reachable (a crash between
`writeState` and the marker deletion). Recovery MUST run first (§7.2), and the
apply step MUST re-evaluate whether the installed release already equals
`target_release` before doing any work; if it does, the update is complete and
the marker is cleared without a download.

---

## 7. Startup Integration

### 7.1 Position in the Sequence

Launcher spec §4 fixes the startup order. This document inserts one step and
amends one.

```
1.   Parse flags.
2.   Resolve executable path.
3.   Resolve game folder (recovery form).        ← may be absent (post-crash)
3a.  update.Recover(folder)                      ← EXISTING, unchanged
3b.  update.Apply(folder, sidecar)               ← NEW (this document)
4.   Resolve game folder (strict form).           ← now safe: 3b left a runnable game/
5.   Load + validate launcher.config.json
6.   Initialise diagnostics log.
7.   Fast paths (--check-port / --diagnostics / --print-url) return.
8.   --repair: roll back, clear intent, rebuild scaffolding, enforce retention
9.   Acquire or detect instance lock.
…    unchanged
```

**R7.1.** `Apply` MUST run **after** `Recover` and **before** the strict layout
check. The ordering rationale is the same as §19.4's: a crash can leave `game/`
absent, which is precisely the state `paths.Resolve` rejects. Recovery restores
runnability; apply may then replace it; only then can the launcher demand a
valid layout.

**R7.2.** `Apply` MUST run **before** the instance lock is acquired.

Rationale: the lock's purpose is single-instance election among *serving*
launchers. An apply is a short, exclusive, folder-level mutation that precedes
serving. Taking the lock first would mean updating only after winning an
election the user has already resolved (FR-UPD-3 refuses to update while an
instance runs, and `POST /api/update/apply` cannot be issued without a running
instance). Exclusivity during apply is provided instead by `apply.lock` (§17).

**R7.3.** `Apply` MUST run **before** the port is bound, the sidecar `port.json`
is written, the browser is opened, or any socket is listening. No client may
ever observe a half-swapped game folder.

**R7.4.** `Apply` MUST NOT block a fast path indefinitely. `--check-port`,
`--diagnostics` and `--print-url` MUST still complete. Under `--print-url` the
apply runs (the server needs the new release), but the download deadline of §8.6
applies. Under `--check-port` and `--diagnostics` the apply MUST be **skipped**;
those paths answer "can I run?" and must not mutate the install to do it.

**R7.5.** `Apply` MUST honour a cancellation request (§12) even on a fast path
that does perform it.

### 7.2 Recovery Precedence

**R7.6.** The sequence is fixed: `Recover` completes or rolls back an interrupted
swap; `Apply` then either finishes the same intent (if the marker survives) or
does nothing. `Apply` MUST NOT attempt to reason about a half-swapped folder; it
trusts `Recover`'s postcondition (a folder with `game/` present and no swap
marker).

**R7.7.** When `Recover` reports `RecoveryRolledBack`, the installed release is
the pre-swap release. If the pending marker's `from_release` matches it, the
marker MUST be retained so the user's request survives a crash-and-rollback
cycle; the retry budget of R6.6 counts it.

**R7.8.** A `Recover` error (a marker with nothing to act on) MUST NOT be
swallowed into the apply path. It is reported once, at the point the diagnostics
logger exists (existing `pendingRecovery` mechanism), and the apply proceeds
only if `game/` is present.

### 7.3 Failure Policy

**R7.9.** An apply failure MUST NOT prevent the launcher from starting. The
launcher logs the outcome, writes `result.json`, and continues with the installed
release. The only apply outcome that changes control flow is a disk-space refusal
on a launcher invoked with no other work to do, which SHOULD be surfaced
prominently but still MUST NOT brick the install (E29's principle: the current
release remains runnable).

**R7.10.** Every apply outcome MUST be recorded as a diagnostics event (§16.4)
regardless of log level, so a user can be asked for a log tail.

**R7.11.** Startup MUST NOT report success for an apply that did not complete.
The launcher's `--diagnostics` payload and `/api/state` MUST report the installed
release that actually exists on disk, never the target release.

---

## 8. The Downloader

### 8.1 Signature

```
DownloadArchive(ctx context.Context, url, dest string, size int64,
                opts DownloadOptions) (DownloadResult, error)
```

**R8.1.** `DownloadArchive` streams to a `.part` file in `dest`'s directory. It
MUST NOT buffer the archive in memory. Peak resident memory for a 4 GiB archive
MUST be bounded by the copy buffer (R8.4), not by the archive size.

**R8.2.** `size` is authoritative and comes from `manifest.archive.size`. The
downloader MUST NOT trust `Content-Length` as the size of the artefact; it uses
it, at most, to detect a truncated *response* early.

**R8.3.** The downloader MUST fail when more than `size` bytes arrive. A server
that ignores `Range` and returns the whole file, or a hostile origin that streams
forever, MUST NOT be able to fill the disk.

### 8.2 The Dedicated Client — the 20-Second Trap

**R8.4.** The downloader MUST use its own `*http.Client`. It MUST NOT use
`httpClient` from `internal/update/http.go`, whose `Timeout: manifestTimeout`
(20 s) applies to the **entire request including body read** and would abort
every realistic archive download after twenty seconds.

The download client's limits:

| Limit | Value | Applies to |
|---|---|---|
| Dial timeout | 10 s | connection establishment |
| TLS handshake timeout | 10 s | handshake |
| Response header timeout | 15 s | time to first header byte |
| **Idle/stall timeout** | 30 s | **maximum gap between successful reads** — enforced by a watchdog on the body reader, not by `Client.Timeout` |
| Total request timeout | **unset** | — |

**R8.5.** The stall watchdog MUST be implemented in the read loop (reset a
deadline on every successful `Read`) so that a slow-but-progressing 4 GiB
download is not killed while a stalled connection is. This is the entire reason
`Client.Timeout` is unusable here.

**R8.6.** The redirect policy MUST be the same bounded policy as `httpClient`
(≤ 3 hops) and MUST refuse a redirect that changes scheme from `https` to
`http`. A downgrade redirect from a mirror is a compromise, not a convenience.

**R8.7.** `Accept` MUST be `application/zstd, application/octet-stream, */*`. The
downloader MUST NOT send `Accept: application/json`.

### 8.3 Resume

**R8.8.** Partial downloads SHOULD be resumable. `download.meta.json` records
`url`, `archive_hash`, `size`, `bytes_written`, `etag` and `last_modified`. On
resume the downloader MUST:

1. discard the partial when `url`, `archive_hash` or `size` differ;
2. issue `Range: bytes=<n>-` **and** `If-Range: <etag>` (or
   `If-Range: <last-modified>` when no ETag was supplied);
3. treat a `200` response as "the server ignored the range" and restart the
   `.part` from zero;
4. treat `206` as a genuine resume and append;
5. re-hash the **whole** file at the end regardless, because a resumed file's
   hash is the only thing that proves the two halves belong together.

**R8.9.** If `download.meta.json` is absent, unreadable, or inconsistent with the
manifest, the `.part` MUST be discarded and the download restarted. A resumable
path that can be tricked into assembling a file from two different archives is
worse than no resume at all.

**R8.10.** `download.part` MUST be `0600` and MUST live inside the update
directory. It MUST NOT be named in a way that `Extract`, the static server, or
`Recover` could mistake for a game artefact.

### 8.4 Integrity During Transfer

**R8.11.** The downloader MUST compute SHA-256 incrementally over the bytes it
writes and MUST return the digest. It MUST NOT re-read the file to hash it
unless a resume occurred (R8.8 step 5).

**R8.12.** The downloader MUST verify the byte count equals `size` before
returning success. A short read is a failure, even when the digest could not
match anyway, because the two conditions produce different user-facing errors
(truncated transfer vs. corrupt archive) and the distinction is worth keeping in
the log.

### 8.5 Progress and Cancellation

**R8.13.** The downloader MUST invoke a progress callback at most once per
`progressInterval` (SHOULD be 250 ms) and MUST always invoke it once on
completion and once on failure. The callback receives `bytes_written`, `size`
and the current phase.

**R8.14.** The callback MUST be invoked from the downloading goroutine and MUST
NOT be called after `DownloadArchive` returns.

**R8.15.** The downloader MUST observe `ctx.Done()` between reads and MUST abort
promptly, leaving `download.part` and accurate `download.meta.json` in place so
R8.8 can resume.

**R8.16.** On abort or failure the downloader MUST NOT delete `download.part`
unless the failure makes it useless (R8.9). Deleting a 3 GiB partial because the
user alt-F4'd the browser is a bad trade.

### 8.6 Bounds and Timeouts

**R8.17.** `DownloadOptions` MUST carry a `MaxArchiveBytes` ceiling, defaulting
to **8 GiB**, and the downloader MUST refuse a manifest declaring more. Without
it, a compromised manifest is an unbounded disk-fill primitive on a folder that
may live on a USB stick.

**R8.18.** The effective total deadline MUST be `max(minDownloadTimeout, size /
floorRate)` where the default floor rate is **64 KiB/s** and
`minDownloadTimeout` is **5 minutes**. A 34 MB fixture therefore gets 5 minutes;
a 4 GiB archive gets ~18 hours, bounded by the stall watchdog in practice.

**R8.19.** `size <= 0` or a missing `archive.size` is a refusal (E36), not an
attempt-anyway. The schema requires the field; a manifest without it is corrupt
or hostile.

---

## 9. The Apply Pipeline

### 9.1 Signature and Contract

```
Apply(ctx context.Context, gameFolder, sidecarDir, launcherVersion string,
      opts ApplyOptions) (ApplyResult, error)
```

**R9.1.** `Apply` is the **only** caller of `GuardApply`, `DownloadArchive`,
`Verify`, `Extract`, `Swap` and `ConfigMerge`. Those functions MUST NOT be called
from anywhere else in the process. This is what makes the pipeline auditable:
one read of one function tells you the entire update.

### 9.2 Ordered Pipeline

The order is normative. Each step's failure disposition is given.

| # | Step | On failure |
|---|---|---|
| 1 | Validate marker (schema, `target_release` > `from_release`) | Clear marker, warn |
| 2 | Read installed release (§6.2) | Clear marker, warn |
| 3 | Short-circuit if installed == target | Clear marker, record "already applied" |
| 4 | `FetchManifest(manifest_url)` | Retry budget (R6.6) |
| 5 | **Reconcile** marker vs. manifest (R9.3) | Clear marker, E35 |
| 6 | `GuardApply(manifest, launcherVersion)` | Clear marker, E29 / E37 |
| 7 | **Disk-space preflight** (§10) | Retain marker, E32 |
| 8 | `DownloadArchive` | Retain marker, E36 / retry |
| 9 | `Verify(archivePath, manifest)` | Clear marker, E28 |
| 10 | `Extract(archivePath, stagingRoot, manifest)` — routes `game/…` → `game.new/…` and `launcher/…` → `launcher.new/…`, prefixes stripped | Clear marker, E28/E30 |
| 11 | **Post-extraction `data/` re-assertion** (R9.5) | Clear marker, E30 |
| 12 | **Config merge into `launcher.new/launcher.config.json`** (§14) | Clear marker, E28 |
| 12a | **`MergeLauncherInto`** — copy `launcher.new/` over the live `launcher/`, per file, preserving modes | Clear marker, E28 |
| 13 | **`Swap(gameFolder, target)`** | `Recover` at next start; E27 if it survived |
| 14 | Record `result.json`, arm retention (§15) | Warn; not fatal |

**R9.2.** `GuardApply` MUST be evaluated against the **current** launcher version
at apply time, not the version recorded in the marker. A launcher downgraded
between request and apply must refuse (FR-UPD-6).

**R9.3.** Reconciliation (step 5) MUST compare the manifest's `release`,
`archive.url` (resolved), `archive.size` and `archive.hash` against the marker.
Any difference MUST abort with E35 — *not* proceed with the new values. The user
confirmed a size; installing a larger one silently violates Packaging spec §9.4
requirement 2.

**R9.4.** Step 4 MUST re-fetch `latest.json` **only** if `manifest_url` is
absent from the marker. The marker normally pins the manifest URL so a release
published between request and apply cannot be substituted.

**R9.5.** After `Extract`, `Apply` MUST independently verify that no path under
`data/` was created or modified. `Extract` already enforces this internally
(FR-UPD-7) and does so by construction; this second check exists because it is
the product's central promise and a redundant assertion on the critical path is
cheap. The check MUST compare a snapshot of `data/`'s entry list and mtimes taken
**before** step 8 against one taken after step 10. A difference is E30, the
staging directory is removed, and the swap does not happen.

**R9.6.** Steps 11–13 MUST be performed with the archive already verified and the
staging tree complete or not at all. There MUST be no code path that reaches
`Swap` with a partially extracted `game.new/`.

**R9.9.** `Extract` MUST route the archive's two subtrees to the two staging
directories and strip the prefix from each (R3.2, §5). A member whose first
segment is neither `game` nor `launcher` MUST be rejected. The routing is
normative rather than an implementation detail because the swap's two operations
depend on it: `game.new/` is renamed *over* `game/`, so it must contain the game
tree's contents, while `launcher/` is merged in place (Packaging spec §9.3).

**R9.10.** The launcher tree MUST be merged into the live `launcher/` directory by
copying files, not by renaming a directory over it: on every supported platform
the running executable is inside that directory, and a directory rename would
replace the image the process is executing from. A file-level replacement is safe
because the running image is already mapped. Files the release does not ship MUST
be left in place; `launcher/` is merged, never swept.

**R9.7.** `Apply` MUST NOT delete `game.old/`. Retirement is §15's job and
depends on a successful start that has not happened yet.

**R9.8.** `Apply` MUST be idempotent with respect to the marker: running it twice
in a row with no other change MUST be a no-op on the second run (step 3 short
circuit).

### 9.3 Verification, Restated as a Contract

`Verify` is retained unchanged, and `Apply` depends on its exact guarantees:

| Guarantee | Requirement |
|---|---|
| Archive SHA-256 equals `manifest.archive.hash` (accepting the `sha256:` prefix and the flat `archive_sha256` alias) | FR-UPD-1, FR-UPD-5 |
| A manifest declaring a signature with no verifier is refused | Launcher §19.3 step 2, R2.1 |
| A manifest declaring neither archive hash nor alias is refused | R2.5 |
| Every archive member is listed in `files[]` when the index is present | Packaging §6.4 |
| Archive is a well-formed zstd tar | FR-UPD-5 |

`Extract`'s guarantees are unchanged and normative (FR-UPD-5, FR-UPD-7): regular
files and directories only; relative names only; no `..`, no absolute or
drive-qualified path, no Windows device name; nothing under `data/`; the running
byte count never exceeds `total_size`; every member matches the index's size and
hash.

---

## 10. Disk-Space Preflight

**R10.1.** Before downloading, `Apply` MUST check free space on the filesystem
that holds the game folder and refuse with E32 when the requirement is not met.

**R10.2.** The requirement is the peak of the staged sequence, which is not
simply `size + total_size`:

| Moment | Live bytes | Note |
|---|---|---|
| Before | `A` = installed `game/` | |
| After download | `A + archive` | |
| After extract, archive removed | `A + total_size` | R5.2 makes this the peak *of these two* |
| After swap | `total_size + total_size` | `game.old/` + new `game/` (same inode count as `A + total_size`) |

The governing requirement is therefore:

```
free ≥ archive.size + total_size + total_size + headroom
```

because the backup copy and the new copy coexist after the swap, and the
post-swap `game.old/` is exactly the pre-swap `game/`.

**R10.3.** `headroom` MUST be the greater of **64 MiB** and **5%** of
`total_size`, to absorb filesystem block slack (many small files each round up
to a block), the `launcher/` component, and the sidecar `download.meta.json`.

**R10.4.** The preflight MUST use a real `statfs`-equivalent on the **game
folder's** filesystem, not the sidecar's, and MUST NOT treat the two as
interchangeable. If they differ (a `--data-dir` on another volume, a sidecar on
`/tmp`), the game folder's volume is the binding constraint for staging and the
swap.

**R10.5.** An unknown or unqueryable free-space value MUST NOT refuse the
update. The check is a guard against a foreseeable failure (a device too small
for a double copy), not a capability probe; failing closed here would break
update on filesystems that cannot report space. It MUST be logged when skipped.

**R10.6.** When the preflight fails, the marker MUST be retained (R6.5) and
`result.json` MUST carry the required and available byte counts so the UI can
say how much room is needed. Byte counts are not paths and are safe to report
(Launcher spec §22.4).

**R10.7.** Packaging spec §3.4 imposes no size limit on a package; this section
is the corresponding launcher-side discipline. A publisher shipping a package
larger than the median user's free space will produce E32 on those machines, and
that is a product consequence of §3.4, not a defect in this mechanism.

---

## 11. Progress, Result, and the Query Surface

### 11.1 The Problem D1 Creates

Under D1 the download happens at startup, before any server exists. There is no
browser to receive progress and no request to hang it on. Two facts follow:

- the *new* session cannot watch the download that installed it;
- the *previous* session initiated the download but is not the one performing it.

The design answer is to make progress a **record**, not a stream: the apply
writes `progress.json` on every tick, and any later session can read the whole
story. If a future release moves the download in-session (§19.1), the same file
is what a polled endpoint serves live, with no protocol change.

### 11.2 The Record

**R11.1.** `progress.json` MUST be written atomically (R5.4) and MUST contain:

```json
{
  "schema": "kobra.update-progress/1",
  "seq": 412,
  "phase": "downloading",
  "from_release": "2026.09.1",
  "target_release": "2026.10.1",
  "bytes": 18432000,
  "total_bytes": 3467509,
  "percent": 53,
  "cancellable": true,
  "updated": "2026-09-15T16:55:01Z"
}
```

`phase` is one of `idle | downloading | verifying | extracting | swapping |
complete | failed | cancelled`. `percent` MUST be an integer `0..100` and MUST be
derived from the bytes of the **declared** `archive.size`, never from
`Content-Length` (R8.2).

**R11.3.** `seq` MUST be monotonically increasing within one apply so a reader
can detect a stale atomic swap. It is not a timestamp.

**R11.4.** `progress.json` MUST be removed when a new pending marker is written,
so a stale `complete` from a previous update is never shown against a new one.

**R11.5.** `result.json` MUST be written at every terminal outcome and MUST
contain `schema`, `release` (outcome), `target_release`, `installed_release`,
`message` (containing the FS §16 text where one applies), `reason` (a stable
machine code), `bytes_written`, `retained_backup` (bool) and `at`.

### 11.3 The Endpoint

**R11.6.** `GET /api/update/progress` MUST be added to the route table
(Launcher spec §14.1) and MUST require the normal session and CSRF pipeline,
including `Sec-Fetch-Site`/`Origin` validation. It is a read of a small local
file and needs no CSRF token itself, but it MUST NOT be reachable without a
session.

**R11.7.** It MUST return the contents of `progress.json` with an added
`result` member carrying `result.json` when the latter is newer than the
progress record. It MUST return `phase: "idle"` rather than an error when no
update record exists.

**R11.8.** It MUST NOT be polled by the shell on a timer. Packaging spec §9.4
requirement 1 forbids background checks and unrequested badges; reading this
endpoint is legitimate only in response to a user action (opening the update
panel, or an explicit check). The shell MAY read it **once** on boot to report
the outcome of the update that installed the running session, which is a
response to the user's earlier action, not a poll.

**R11.9.** The endpoint MUST NOT expose URLs, absolute paths, or the archive
hash. Percent, bytes, phases and release identifiers are the entire public
surface. A locally reachable endpoint that echoes the update URL is a small but
real information leak about a machine-local install.

**R11.10.** `POST /api/update/apply` MUST gain a complementary
`POST /api/update/cancel` (§12.4).

---

## 12. Cancellation

**R12.1.** Cancellation MUST be safe in the Packaging spec §9.4 requirement 4
sense: a cancelled update leaves the install **untouched** and leaves no partial
archive where the launcher could mistake it for complete.

**R12.2.** Because the download happens at startup with no browser attached,
cancellation MUST have a channel that does not require a live session. The
mechanism is a flag file: creating `<sidecar>/update/cancel` requests
cancellation.

**R12.3.** The downloader's progress tick (R8.13) MUST stat the flag file and
cancel by calling the context's cancel function. The stat rate is therefore the
tick rate (250 ms), which is responsive enough and costs nothing.

**R12.4.** `POST /api/update/cancel` MUST create the flag file and MUST also
clear `pending.json` when the apply is not currently running (the common case:
the user cancels a pending update before restarting). It MUST be
session-authenticated and rate-limited like `apply`.

**R12.5.** The flag file MUST be removed at the start of every apply, after the
pending marker is read, so a cancel from a previous cycle does not silently kill
the next one.

**R12.6.** A cancelled download MUST retain `download.part` and
`download.meta.json` unless R8.9 applies, so a re-request resumes rather than
restarts. Cancelling MUST NOT be punished with a full re-download.

**R12.7.** A cancel received after step 8 of §9.2 (archive verified) MUST be
ignored through step 13, which MUST run to completion. There is no safe point
between "verified" and "swapped" to stop, and stopping there would leave a
`game.new/` that a later launch would silently promote — a state change the user
did not confirm. Cancellation is therefore defined as: **before the swap
begins**, immediately; **after it begins**, never.

---

## 13. Trust Model for the Patch Channel

**R13.1.** The following MUST be stated in the Packaging spec's channel
documentation and in the update UI where the channel is described:

> With `update.channel: patch`, the launcher verifies that the downloaded
> archive matches the hash in the release manifest. This detects corruption, a
> truncated download, and a mirror that served a different file. It does **not**
> prove who published the release: whoever controls the update server controls
> both the manifest and the archive. If that server is compromised, so is the
> update.

**R13.2.** Signing is **recommended**. A publisher who signs SHOULD:
1. publish `SHA256SUMS` and `SHA256SUMS.sig` (already mandatory, Packaging §7.1);
2. publish the signing key's fingerprint out of band (Packaging §8.6);
3. install a verifier in the launcher build before publishing
   `signatures.gpg` on a `patch` release (R2.3).

**R13.3.** The launcher MUST NOT be described as providing publisher
authentication in any user-facing text, log line, or error message. A message
that implies an unsigned patch was authenticated is a defect.

**R13.4.** `SetSignatureVerifier` MUST remain the seam. When a verifier is
installed, `Verify`'s outcome for a signed manifest becomes
verify-against-fingerprint; when none is installed, the behaviour is R2.1's
refusal. There MUST NOT be a configuration flag that turns a failed signature
check into a warning.

**R13.5.** A `manual`-channel install is unaffected: the launcher performs no
cryptography, and the user's own `gpg`/`sha256sum` run is the check.

**R13.6.** Transport MUST be HTTPS for a `patch` channel, and a plain `http`
base URL MUST be refused outside development builds. (The fixture harness uses
`http://127.0.0.1` today; loopback MUST be permitted, remote cleartext MUST
not.) This is enforcement of Packaging §7.3's "transport trust is not
integrity" rather than a replacement for it.

---

## 14. Configuration Merge Across a Swap

### 14.1 The Trap

The update archive contains `launcher/launcher.config.json`. It must: Packaging
spec §9.3 requires `launcher/` to be replaced in place, and the manifest index
lists the file. The archive's copy is the **release's** configuration.

But `launcher.config.json` also holds machine-local, user-owned state — most
importantly `update.channel` and `update.patch_base_url`, which the user or the
installer may have set, and which the *previous* release's archive may not
contain at all.

A naive swap therefore reverts local configuration. Concretely, in the existing
fixture: `POST /api/update/apply` is only reachable when the channel is `patch`,
but both fixture releases ship `"channel": "manual"`. Applying the update
replaces the local `patch` setting with the archive's `manual`, and the game
silently stops updating. **The current e2e harness would not catch this**, because
it never applies an update.

### 14.2 The Rule

**R14.1.** After extraction and before the swap, `Apply` MUST merge the release's
copy of the config (`launcher.new/launcher.config.json` in the staging root, its
prefix stripped by R9.9) with the **installed**
`launcher/launcher.config.json` (the local copy) and write the result back into
the staged tree, so that step 12a installs the merged document.

**R14.2.** The merge is **local-wins** for machine-local keys and
**release-wins** for release-owned keys:

| Key | Owner | Rule |
|---|---|---|
| `update.channel` | local | Local value wins; release value is used only when local is absent |
| `update.patch_base_url` | local | Local wins; never removed by an update |
| `port.base`, `port.span`, `port.require_confirmation` | local | Local wins (the port is a per-machine fact, FS §6) |
| `server.idle_timeout_seconds` | local | Local wins |
| `browser_preference` | local | Local wins |
| `data_dir` (if present) | local | **MUST win.** It is a path on this machine |
| `release` | release | **Release wins.** This is how the new release identity is recorded (§14.3) |
| `game_id`, `game_name`, `mime_types`, `locales`, `schema` | release | Release wins on conflict; local-only keys are preserved |
| Any key present in local but not in the release copy | local | Preserved as-is |

**R14.3.** `release` MUST be taken from the release's copy. This is not
cosmetic: the archive deliberately omits `game/release.manifest.json`
(Packaging spec §9.1), so after a swap the installed release is reported from
`launcher.config.json` (`update.InstalledRelease`'s documented fallback). The
merge is therefore what makes the post-swap folder report the new release
truthfully, and §15's retention logic depends on it.

**R14.4.** The merge step MUST preserve every key it does not explicitly own. A
key the launcher does not know about MUST NOT be dropped by the merge, and such a
key MUST be named in the merge event so the decision is visible.

**Status of this requirement.** It is satisfied by the merge and then defeated by
validation, and the defeat is deliberate: `launcher.config.schema.json` sets
`additionalProperties: false`, so an unrecognised key cannot be part of a valid
config, and R14.5 refuses to install one. The observable behaviour is therefore
that an unknown key in the *local* config aborts the update with E28 rather than
being carried forward. That is the safe reading — installing a config the next
launch will reject is worse than refusing the update — but it is not the
forward-compatibility promise this requirement's wording makes. Forward
compatibility for unknown config keys requires relaxing the schema first
(§22 P5); until then R14.5 governs. The merge is implemented so that relaxing the
schema is the only change needed.

**R14.5.** The merged file MUST be re-validated against
`launcher.config.schema.json` **before** the swap. A merge that produces an
invalid config MUST abort the apply (E28) rather than install a folder the next
launch will refuse.

**R14.6.** If the release's copy is absent from the archive, no merge occurs and
the local copy is preserved: it is not in `launcher.new/`, and `launcher/` is
merged rather than swapped away. This is the correct outcome and MUST NOT be
treated as an error.

**R14.7.** If the local copy is absent or unreadable at apply time, the
release's copy MUST be installed as-is and the apply MUST continue. The update
does not fail because the machine lost its local config.

**R14.8.** The merge MUST be logged as an event naming which keys were taken
from local and which from the release (key names only; no values, per §3.3 and
Launcher spec §22.4).

### 14.3 Amendment Consequence

**R14.9.** Packaging spec §9.3's swap-protocol description MUST be amended to
name the config merge as part of the protocol, because a package author's
assumptions about `launcher/` replacement now have an exception. See §20.

---

## 15. Rollback Retention

### 15.1 What FR-UPD-8 Actually Requires

> `game.old` is retained until the new release has started successfully twice,
> then removed.

`Recover` today preserves `game.old/` unconditionally, with an accurate comment
explaining that it cannot know the count. Nothing computes the count, so the
retention window never closes: `game.old/` occupies a full copy of the game
forever, and the only way to reclaim it is `--repair`, which also rolls back —
the opposite of what a user who wants the space wants.

### 15.2 The Counter

**R15.1.** The launcher MUST maintain `<sidecar>/update/started.json`:

```json
{
  "schema": "kobra.update-started/1",
  "release": "2026.10.1",
  "count": 1,
  "updated": "2026-09-15T17:02:44Z"
}
```

**R15.2.** The counter MUST be incremented only when **both** conditions hold:

1. a `game.old/` directory exists beside a `game/` directory, **and**
2. the launcher reached `SERVE` and received at least one shell heartbeat
   (Launcher spec §19.6's definition of "started successfully").

**R15.3.** Incrementing MUST happen at the heartbeat handler, not at startup. A
release that starts and then dies before the shell connects MUST **not** count —
that is precisely the failure mode the two-start rule exists to catch.

**R15.4.** The counter MUST be keyed by the installed release. When the
recorded `release` differs from the currently installed release, the record MUST
be reset to `count: 1` rather than incremented, so a counter from an older
update cannot retire a newer backup.

**R15.5.** When `count` reaches **2**, the launcher MUST remove `game.old/` and
delete the counter record, logging `update.backup.retired`. This is the only
automatic deletion of `game.old/`.

**R15.6.** The removal MUST be attempted once, at the heartbeat that reaches the
threshold. Failure MUST log a warning and leave the folder; it MUST NOT be
retried in a loop and MUST NOT fail the session.

**R15.7.** The counter MUST NOT be incremented when `game.old/` is absent. A
folder that was never updated MUST NOT accumulate a meaningless count.

### 15.3 Interaction with Recovery

**R15.8.** `Recover`'s preservation of `game.old/` is **correct and unchanged**.
Its comment ("Recover has no way to know that count, so it is preserved") becomes
true in a stronger sense: the count is not Recover's business; it is §15's.
`Recover` MUST NOT be modified to delete `game.old/`.

**R15.9.** After a rollback (`game.old → game`), the counter MUST be reset or
cleared. A rolled-back install has no backup to retire, and a stale count would
retire the *next* update's backup prematurely.

**R15.10.** After `--repair`, the counter MUST be cleared alongside the rollback,
for the same reason.

### 15.4 The User-Facing Offer

**R15.11.** While a retained `game.old/` exists, the launcher MUST expose the
fact so the shell can offer "Revert to previous version" (Packaging spec §9.4
requirement 8). The offer MUST include the release identifier recorded in
`started.json` or, failing that, in `update.state`.

**R15.12.** The revert action MUST be `--repair`'s rollback path and MUST NOT
touch `data/` (FR-UPD-9). A newer save stays on disk, preserved and unreadable by
the older build.

---

## 16. Error Semantics and Taxonomy

### 16.1 Existing Codes Used Unchanged

| Code | Text (FS §16, verbatim) | Used at |
|---|---|---|
| E27 | "The last update didn't finish." | `update.state` found at boot; an apply that could not leave a runnable release |
| E28 | "The update file is damaged." | `Verify` failure, extraction failure, invalid merged config |
| E29 | "This update needs a newer launcher." | `GuardApply` failure |
| E30 | "The update was rejected because it would modify your saves." | `Extract` refusal, post-extraction `data/` assertion |

### 16.2 New Codes

The FS §16 matrix MUST be extended with the following. They are conditions the
update mechanism can reach that no existing code describes, and E28 is wrong for
all of them because nothing is damaged.

| Code | Condition | Text (exact, user-facing) | Disposition |
|---|---|---|---|
| **E32** | Disk-space preflight fails (§10) | "There isn't enough free space to install this update." | Keep current release; retain marker; report bytes required |
| **E33** | Download refused by policy (§8.6) or archive size missing/invalid | "This update cannot be downloaded." | Keep current release; retain marker once |
| **E34** | Apply cancelled by the user (§12) | "The update was cancelled." | Keep current release; no partial state |
| **E35** | Manifest no longer matches the confirmed update (§9.2 step 5) | "The update changed since you confirmed it. Check for updates again." | Keep current release; clear marker |
| **E36** | Download failed (network, status, truncation) | "The update could not be downloaded. The launcher may be offline." | Keep current release; retain marker with retry budget |
| **E37** | Release manifest unreadable: fetch, decode, or field validation failed, including an unreadable `launcher_min` | "The update manifest was not readable." | Keep the current release runnable; clear marker; report to the publisher |

**R16.1.** E32–E37 MUST be added to the FS §16 matrix, and E32, E34, E35, E37
MUST be added to Packaging spec §9.4 requirement 5's list of texts used verbatim.

**R16.2.** The words "damaged", "corrupt", and "invalid" MUST NOT be used for
E32–E37. The user's install is fine; the update did not happen.

**R16.3.** E34 is informational and SHOULD be rendered as a neutral message, not
an error.

**R16.5.** E37 MUST NOT be reported as E29. An unreadable `launcher_min` is a
fault in the publisher's document, so the E29 text ("This update needs a newer
launcher.") would send the player looking for a launcher that does not exist and
offer a remedy that cannot work. A manifest whose floor cannot be read is refused
as unreadable (E37); E29 is reserved for a launcher that is older than a floor
that was read. Both leave the current release runnable, so the difference is in
the guidance, not the disposition.

**R16.6.** E37 has no scenario in §18's E2E list yet. It is covered by the
launcher's unit tests — the manifest rejection at fetch time, and the
defence-in-depth refusal at the gate — so the rule is enforced, but a reader
looking for E37 in the E2E matrix will not find it. Adding one is deferred rather
than silently implied.

### 16.3 The `launcher_min`-With-No-Update Case

**R16.4.** A release whose `launcher_min` exceeds the installed launcher is
refused at step 6 with E29, and the marker is cleared. The current release keeps
working and the launcher MUST NOT brick itself (Launcher spec §19.5). Packaging
spec §9.5's requirement that launcher upgrades also travel through the ZIP is
the user-facing mitigation and MUST be repeated in the E29 message's guidance
link. A manifest whose `launcher_min` cannot be read is refused at the same step
with E37 instead of E29, and with the same disposition (R16.5).

### 16.4 Diagnostics Events

The following MUST be emitted through the existing `update.SetLogger` hook
(Launcher spec §21.2, §21.5). Fields are counts, releases, and phases only —
never URLs, paths, or hashes (§11.9, Launcher spec §22.4).

| Event | Level | Fields |
|---|---|---|
| `update.apply.begin` | info | `from`, `target` |
| `update.apply.skipped` | info | `reason` |
| `update.apply.done` | info | `target`, `installed` |
| `update.apply.failed` | warn | `reason` (stable code) |
| `update.manifest.drift` | warn | `field` (name only) |
| `update.download.begin` | info | `bytes` |
| `update.download.progress` | debug | `bytes`, `total` |
| `update.download.resume` | info | `from_bytes` |
| `update.download.restart` | warn | `reason` |
| `update.download.failed` | warn | `reason` |
| `update.disk.insufficient` | warn | `required`, `available` |
| `update.disk.unknown` | debug | `reason` |
| `update.extract.begin` | info | `release` |
| `update.extract.done` | info | `release`, `entries` |
| `update.config.merge` | info | `from_local`, `from_release` (key-name lists) |
| `update.swap.begin` / `.complete` / `.rollback` | existing | existing fields |
| `update.backup.retained` | info | `release`, `count` |
| `update.backup.retired` | info | `release` |
| `update.cancel.requested` / `.honoured` | info / warn | `phase` |
| `update.pending.cleared` | info | `reason` |

**R16.5.** `update.download.progress` at `debug` MUST NOT be emitted at `info`
level; a 4 GiB download at one event per 250 ms is a log-size bug.

---

## 17. Concurrency and Locking

**R17.1.** At most one apply may run per game folder. `Apply` MUST acquire an
exclusive OS-level lock on `<sidecar>/update/apply.lock` (the existing
`sidecar` lock primitives: `flock` on Unix, `LockFileEx` on Windows). Failing to
acquire it MUST abort the apply with `update.apply.skipped` and leave the marker
intact.

**R17.2.** The lock MUST be released when `Apply` returns, including on panic
(deferred). A crashed apply MUST NOT leave the lock held: the OS lock is released
by process death, which is why an OS lock is required rather than a PID file.

**R17.3.** `Apply` MUST NOT hold the lock while the launcher serves. It runs
before the instance lock is acquired (§7.2) and releases on return.

**R17.4.** Two launchers started simultaneously on the same folder MUST not
both apply. The second observes the lock and skips; the first completes the
swap; the second then starts against the new release normally. No user-visible
error is warranted — the outcome is correct.

**R17.5.** `Apply` MUST NOT be invoked from an HTTP handler. `POST
/api/update/apply` records intent only (§6.1). This is what keeps the swap out of
the serving window and eliminates an entire class of lock-ordering bugs.

**R17.6.** The heartbeat-driven retention increment (§15.3) MUST be safe for
concurrent heartbeats: it uses the same atomic read-modify-write discipline as
`R5.4` and MUST tolerate two heartbeats racing (the outcome may be one increment
short or the write may be retried; it MUST NOT corrupt or double-count beyond
the observed heartbeats). A single-writer election already exists for tab-level
writes (Launcher spec §17.2) and SHOULD be reused.

---

## 18. Test Plan

### 18.1 End-to-End, in the Existing Harness

`testgame/run.sh` already builds two releases, serves `dist/` over loopback,
and exercises `GET /api/update/check`. It MUST be extended with an apply phase.
The fixture must gain a `2026.10.1` level file so the swap is observable.

| # | Test | Assertion |
|---|---|---|
| E2E-1 | Apply `2026.09.1 → 2026.10.1` | Server starts, level file from `2026.10.1` is served |
| E2E-2 | `data/` untouched | `data/` entry list and file hashes identical before/after (checksum compare, as `run1.sums` already does) |
| E2E-3 | Backup retained | `game.old/` exists and contains the `2026.09.1` level file |
| E2E-4 | Installed release reported | `/api/update/check` reports `current: 2026.10.1` after the swap — pins the §14.3 fallback |
| E2E-5 | Local config preserved | `update.channel` is still `patch` after the swap and a second apply is possible — pins §14 |
| E2E-6 | Staging cleaned | `game.new/` and `launcher.new/` absent from the staging root; `update.state` absent from the game folder; `download.part` absent |
| E2E-7 | Retention retires at two | Two heartbeating starts remove `game.old/`; one start does not |
| E2E-8 | Mid-swap kill recovers | `SIGKILL` between the two renames (fault injection at `writeState`/rename boundaries), next start completes or rolls back per §19.4 |
| E2E-9 | Corrupt archive | Fixture archive byte flipped → E28, install unchanged and runnable |
| E2E-10 | Truncated archive | Server closes early → E36, marker retained, `.part` present for resume |
| E2E-11 | Oversized archive | Server serves more than `archive.size` → refused at the declared bound (R8.3) |
| E2E-12 | Manifest drift | `latest.json`/manifest rewritten between request and apply → E35, no swap |
| E2E-13 | `launcher_min` too high | Patch the manifest's `launcher_min` → E29, current release still runs |
| E2E-14 | Cancel before restart | `POST /api/update/cancel` then restart → no download, no swap |
| E2E-15 | Resume | Kill mid-download, restart → `Range` request, completed archive hash matches |
| E2E-16 | Disk space | Fault-inject the free-space query → E32, marker retained, `required`/`available` reported |
| E2E-17 | No update | Restart with no marker → no outbound request at all (assert server saw zero requests) |
| E2E-18 | Progress record | After an apply, `progress.json` ends at `phase: complete`, `percent: 100` |
| E2E-19 | Concurrent launch | Start two launchers on one folder with a pending marker → exactly one apply |

**R18.1.** E2E-1 through E2E-5 are **release-blocking**. They are the four
claims the product makes about updates (it installs, saves are safe, rollback is
possible, the right version is reported) plus the config trap that would silently
disable the feature.

**R18.2.** E2E-8 and E2E-9 are release-blocking as well: they are the two
failure modes that destroy a user's install if wrong.

**R18.3.** E2E-4 MUST assert the release reported *after* the swap, not merely
that the swap changed files. This is the pin on the M1 fallback risk in §14.3.

### 18.2 Unit Coverage the Pipeline Adds

**R18.4.** `DownloadArchive` MUST have unit tests for: exact-size success; one
byte over; one byte short; a stalled reader triggering the stall watchdog; a
context cancellation mid-stream; resume with `206`; resume where the server
answers `200`; resume where the meta file's hash differs (discard); and a
redirect chain exceeding the cap.

**R18.5.** The disk preflight MUST be unit-tested against a table of
`(installed, archive, total, headroom, free)` tuples, including the
equal-to-required boundary (must pass) and one byte under (must fail).

**R18.6.** The config merge MUST be unit-tested for: local-only key preserved;
release-only key added; conflicting key resolved per R14.2; `data_dir` local win;
`release` release win; an unknown key preserved; an absent release copy (no
merge); an absent local copy (release copy installed); a merge producing an
invalid config (abort).

**R18.7.** The trigger state machine (§6.3) MUST be tested for every transition,
including the `PENDING` + swap-marker state and the retry-budget exhaustion
(R6.6).

**R18.8.** The retention counter MUST be unit-tested for: first start
(`count: 1`); second start retires; a start with no heartbeat does not count; a
release change resets the record; rollback clears it; `game.old/` absent does
not increment.

### 18.3 The Fault-Injection Requirement

**R18.9.** `Swap` MUST expose the crash points it already has (before
`writeState`, after `writeState`, after `game → game.old`, after `game.new →
game`, and between the launcher merge and the swap) to the test harness without a
build tag. Launcher spec §19.4 claims the
state machine is "exhaustively tested with fault injection at each step
(§26.6)"; this requirement makes that claim testable at the level the claim is
made, rather than only inside `swap.go`'s own unit tests.

---

## 19. Deferred Work

Each item is deferred deliberately, with the cost of deferring stated.

### 19.1 In-Browser Progress and Confirmation UI

**Deferred.** Packaging spec §9.4 requirements 2, 3, 6, 7 and 8 (confirm with
size, live byte/percent progress, release notes before applying, plain result,
rollback offer) are **not satisfied** by V1's restart model. What V1 provides is
the substrate: the size is in the `202` response (R6.4), progress is recorded
(§11.2), the result and the rollback offer are recorded (§11.2, §15.4). The
browser UI (`testgame/game/shell.js`) has none of it.

**Cost of deferring:** a user cannot see progress during the download and learns
the outcome on the next launch. This is a real regression against §9.4 and MUST
be recorded as such in the Packaging spec, not quietly dropped.

**Exit criterion for the deferral:** a shell update panel that reads
`/api/update/check`, shows size and notes, POSTs `apply`, and reads
`/api/update/progress` once on the following boot to report the result. No new
launcher mechanism is required to build it, which is the point of §11.

### 19.2 `game-only` Scope Correctness

**Deferred.** Under `scope: full` the archive carries
`launcher/launcher.config.json`, so the post-swap release is reported correctly
(§14.3). Under `scope: game-only` the archive carries no `launcher/`, so the
config's stale `release` would be reported and §15's counter would key on the
wrong release.

**Cost of deferring:** `game-only` releases cannot be applied correctly.

**Options for the eventual fix** (§22, decision P3): require
`game/release.manifest.json` in the archive for the assisted path; or introduce
`<sidecar>/update/installed.json` as the authoritative installed-release record
written at swap time.

### 19.3 A Launcher-Side Verifier

**Deferred** per D2. The `SetSignatureVerifier` seam is retained (R13.4) and the
fail-closed rule is retained (R2.1).

### 19.4 Patch-From-Any-Previous-Release

**Deferred.** Packaging spec §9.2 requires the publisher to ship a patch for
every supported source release or a `full` archive alongside. The launcher
assumes the manifest it fetches is applicable to the installed release. If the
installed release is not the manifest's `previous_release` and the scope is
`patch`, the launcher MUST refuse with E35 rather than extract a delta onto the
wrong base. This check is specified here but marked SHOULD for V1 because the
fixture only produces `full` archives.

### 19.5 Multi-Platform Archive Selection

**Deferred.** The manifest names one archive per release per platform (Packaging
spec §3.6), and `latest.json` names one manifest. A launcher on a platform whose
archive is not the one named MUST refuse with E35. V1 has one platform in the
fixture.

---

## 20. Amendment Set

This document implies a bounded set of edits to the three existing specs. They
are listed together so they can be applied as one change and reviewed as one.

### 20.1 Functional Specification

| Section | Change |
|---|---|
| §16 error matrix | Add E32–E36 with the exact texts in §16.2. **Applied** — note that E31 was already taken by "two tabs, save conflict", which is why the new codes start at E32 |
| §12.2 | State that the assisted patch path records intent and applies at the next start (D1), and that in-browser progress is deferred (§19.1) **Applied.** |
| §19.2 test matrix | Add the update apply row set from §18.1 **Applied.** |
| §21 decisions table | Add the D1 and D2 rows **Applied.** |

### 20.2 Launcher Specification

| Section | Change |
|---|---|
| §4 startup sequence | Insert step 3b `update.Apply` between recovery and the strict layout check; note the fast-path exemptions (R7.4) |
| §14.1 route table | Add `GET /api/update/progress` and `POST /api/update/cancel` |
| §19.2 | Specify the `202` body (R6.4) and that the marker, not the request, is the durable intent |
| §19.3 | Reference §9.2's ordered pipeline as the caller contract, and §9.3's dependence table |
| §19.6 | Point retention at `started.json` (§15), and state that `Recover`'s preservation is correct |
| New §19.7 | In-browser progress is deferred; the record in §11 is the substrate |
| §22 error taxonomy | Add the E32–E36 mapping and exit-code dispositions |
| §21.2 event catalogue | Add the §16.4 events |
| §23 concurrency | Add `apply.lock` (§17) |

### 20.3 Packaging Specification

| Section | Change |
|---|---|
| §8.6 rule 5 | **Amend**: the `patch` channel does not oblige signing in V1 (D2, R2.2) |
| §8.6 | Add R13.1's residual-risk statement verbatim |
| §9.3 | Add the config merge to the swap-protocol description (§14) |
| §9.4 requirement 5 | Add E32, E34, E35 texts |
| §9.4 | Mark requirements 2, 3, 6, 7, 8 as deferred to the shell work (§19.1) rather than "already supported" |
| §15.3 | **Amend**: signing is recommended, not obligatory, for `patch` |
| §11.2 build gates | Add R2.3: refuse to publish `signatures.gpg` on a `patch` release when the shipped launcher has no verifier |

**R20.1.** Amendment of Packaging §15.3 is the explicit product decision D2. It
MUST be applied together with §8.6 rule 5, or the two will contradict again.

---

## 21. Traceability

### 21.1 Requirements to Mechanism

| Requirement | Mechanism |
|---|---|
| FR-UPD-1 (manifest present, indexed) | Existing; §9.3 consumes |
| FR-UPD-2 (user-initiated, silent when offline) | §6.1; R11.8; R13.6 |
| FR-UPD-3 (refuse while running) | Existing `POST /api/update/apply` reachability; §17.5 |
| FR-UPD-4 (crash-safe swap) | Existing `Swap`/`Recover`; §7.2; R18.9 |
| FR-UPD-5 (zip-slip/bomb, declared size) | Existing `Verify`/`Extract`; R8.3, R8.17, R9.5 |
| FR-UPD-6 (`launcher_min`) | R9.2, E29, E37 |
| FR-UPD-7 (`data/` untouched) | R9.5, R5.3, E30 |
| FR-UPD-8 (retain `game.old` for two starts) | §15 |
| FR-UPD-9 (rollback does not roll back saves) | R15.12 |
| Packaging §9.4 update experience | §11, §19.1 (partial, explicitly deferred) |
| Launcher §19.2 check | Existing, unchanged |
| Launcher §19.3 verification | §9.2, §9.3 |
| Launcher §19.4 crash safety | §7.2, §18.3 |
| Launcher §19.5 `launcher_min` | R9.2, R16.4 |
| Launcher §19.6 rollback | §15 |
| Packaging §9.1 archive contents | §14.1 |
| Packaging §9.3 swap tolerance | §14 |

### 21.2 Mechanisms to Tests

| Mechanism | Test |
|---|---|
| Pipeline happy path | E2E-1…E2E-6 |
| Config merge (§14) | E2E-4, E2E-5, R18.6 |
| Retention (§15) | E2E-7, R18.8 |
| Crash safety (§7.2) | E2E-8, R18.9 |
| Damage handling (E28) | E2E-9, R18.4 |
| Network failure (E36) | E2E-10, E2E-15, R18.4 |
| Policy bounds (E33) | E2E-11, R8.17 |
| Drift (E35) | E2E-12 |
| `launcher_min` (E29) | E2E-13 |
| Cancellation (§12) | E2E-14, E2E-15 |
| Disk guard (E32) | E2E-16, R18.5 |
| No-op path (R9.8, R11.8) | E2E-17 |
| Progress record (§11) | E2E-18, R18.7 |
| Exclusivity (§17) | E2E-19 |

---

## 22. Open Items That Need a Product Decision

These are recorded, not resolved, because each changes user-visible behaviour
and belongs to the product owner rather than the implementer.

**P1 — Is `patch` the default channel?** Packaging §15.3's answer is still
`manual`, and D2 does not change that. The question is whether a publisher who
has shipped a working patch path should leave the default manual, which means
most users never update. Recommendation: keep `manual` as the build default and
let each game opt in, because D1's restart model is the first release of this
mechanism and a silent auto-update is not a thing to discover a bug in.

**P2 — Should the retry budget be 3?** R6.6 avoids a permanent per-launch
network attempt. Three is a guess. The cost of too few is a user who must
re-request an update after a transient outage; the cost of too many is a
repeated delay on every launch.

**P3 — How is the installed release recorded?** R14.3 works today because the
archive carries `launcher.config.json`. It is fragile: a `game-only` release
breaks it (§19.2), and the config file is a user-owned document doing double duty
as a release record. The alternatives are to require
`game/release.manifest.json` in the archive (simple, and consistent with
Packaging §9.1's omission only being justified by "the manifest describes the
archive, so it cannot be in it" — which is about the *released* manifest, not the
*installed* one) or to add `<sidecar>/update/installed.json`. Recommendation:
require the installed manifest in the archive, because it also restores
`update.InstalledRelease`'s primary path and removes the fallback from the
critical path entirely.

**P4 — Does the update notify on completion?** Under D1 the user relaunches and
is already on the new release, possibly without having wanted the restart to be
the trigger. R11.8 permits one read on boot; whether the shell *shows* anything
is a product decision, and Packaging §9.4 requirement 7 ("the result is stated
plainly") suggests it should.

**P5 — Should `launcher.config.json` keep `additionalProperties: false`?**
R14.4's forward-compatibility promise for unknown config keys is defeated by the
schema's strictness (see R14.4's status note): an unrecognised key in a local
config makes the merge produce a document the launcher itself refuses, so the
update aborts with E28. The alternatives are to relax the schema for unknown keys
(which weakens the "config is validated" guarantee that keeps a typo from
silently taking effect) or to accept the abort and reword R14.4. This is the
first thing a third-party config key will hit.

**P6 — Should the startup apply be bounded by wall-clock time?** A 4 GiB archive
on a slow link makes the launcher appear hung before the browser opens, with
nothing on screen. The stall watchdog and the size-derived deadline (§8.6) bound
it in practice, but there is no user-visible sign of progress during startup.
Options: a splash/dialog, a bounded "defer this update to the next launch"
timeout, or relying on the shell work of §19.1. The current behaviour is
specified (R8.18) but was not chosen against an alternative.

---

## Appendix A. The Apply Pipeline, Condensed

```
Apply(folder, sidecar, launcherVersion):
  lock apply.lock                                   # §17, else skip
  marker ← read pending.json                        # §6
  if marker invalid: clear, warn, return
  installed ← read installed release                # §14.3 order
  if installed == target: clear, record done, return
  manifest ← FetchManifest(marker.manifest_url)      # retry budget §6.2
  reconcile(marker, manifest)                        # E35 on drift
  GuardApply(manifest, launcherVersion)              # E29 / E37
  preflight(free space)                              # E32
  snapshot data/                                     # R9.5
  DownloadArchive(url, download.part, size)          # §8, resumable, cancellable
  Verify(download.part, manifest)                    # E28
  remove download.part                               # R5.2
  Extract(archive, stagingRoot, manifest)            # E28 / E30, prefix-routed
  assert data/ unchanged                             # E30
  ConfigMerge(launcher.new, launcher)                # §14, validate
  MergeLauncherInto(launcher.new, launcher)          # §19.3, in place
  Swap(folder, stagingRoot, target)                  # §19.4, swap marker
  record result.json; arm retention                  # §11.2, §15
  clear pending.json
  unlock
```

## Appendix B. Files This Document Requires

| Path | New/Modified | Purpose |
|---|---|---|
| `launcher/internal/update/download.go` | New | §8 |
| `launcher/internal/update/apply.go` | New | §9 |
| `launcher/internal/update/pending.go` | New | §6 |
| `launcher/internal/update/progress.go` | New | §11 |
| `launcher/internal/update/retention.go` | New | §15 |
| `launcher/internal/update/configmerge.go` | New | §14 |
| `launcher/internal/update/intent.go` | New | §6.1 |
| `launcher/internal/update/startup.go` | New | §7 |
| `launcher/internal/update/runtime.go` | New | §5, §10 |
| `launcher/internal/update/progress.go` | New | §11 |
| `launcher/internal/update/stall.go` | New | §8.2 |
| `launcher/internal/update/space_unix.go`, `space_windows.go` | New | §10 |
| `launcher/internal/update/applylock_unix.go`, `applylock_windows.go` | New | §17 |
| `launcher/internal/update/download_test.go` | New | R18.4 |
| `launcher/internal/update/apply_test.go` | New | R18.5–R18.7 |
| `launcher/internal/update/configmerge_test.go` | New | R18.6 |
| `launcher/internal/update/retention_test.go` | New | R18.8 |
| `launcher/internal/dataapi/handlers.go` | Modified | `apply` writes the marker; add `progress`, `cancel` |
| `launcher/internal/server/update.go` | Modified | Reconcile at request time; expose result |
| `launcher/internal/apitypes/apitypes.go` | Modified | Progress/result/cancel wire types |
| `launcher/cmd/kobra-launcher/main.go` | Modified | Step 3b; heartbeat retention hook |
| `testgame/game/assets/data/levels.json` | Modified | Observable `2026.10.1` change |
| `testgame/run.sh` | Modified | §18.1 phase |
