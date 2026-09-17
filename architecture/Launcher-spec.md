# Technical Specification: The Kobra Launcher

> **Companion documents.** This specification states how the launcher process is
> built. The product requirements are in `functional-specification.md`; what a
> publisher ships is in `Packaging-spec.md`; and the update execution model that §19
> depends on is specified in `Updater-spec.md`.

**Document Version:** 1.0
**Companion to:** Functional Specification v2.1 (September 2026)
**Date:** September 2026
**Status:** Implementation-Ready
**Classification:** Technical Architecture Specification
**Audience:** Launcher implementers, release engineers, security reviewers

> **Scope note.** The Functional Specification (FS) defines *what* the system must do and *why*. This document defines *how* the launcher — the single native Go binary at the centre of the architecture — is built. It is subordinate to the FS: where the two conflict, the FS wins and this document is wrong. Every requirement here is written to satisfy one or more `FR-*` identifiers, and each section names them.
>
> **What is not in scope.** The engine (WASM core), the game shell (`shell.js`), the rendering backend, and the game's gameplay logic. This document covers only the process that: resolves the game folder, allocates a port, runs the loopback HTTP server, enforces the data API, performs atomic writes, applies updates, and manages its own lifecycle. Assets and manifests are *consumed* by the launcher but authored elsewhere.

---

## Table of Contents

1. [Overview and Design Principles](#1-overview-and-design-principles)
2. [Process Model and Lifecycle](#2-process-model-and-lifecycle)
3. [Repository and Package Layout](#3-repository-and-package-layout)
4. [Startup Sequence](#4-startup-sequence)
5. [Game Folder and Path Resolution](#5-game-folder-and-path-resolution)
6. [Sidecar State](#6-sidecar-state)
7. [Configuration Loading](#7-configuration-loading)
8. [Port Allocation Subsystem](#8-port-allocation-subsystem)
9. [Instance Locking and Single-Instance Election](#9-instance-locking-and-single-instance-election)
10. [HTTP Server](#10-http-server)
11. [Session and CSRF Subsystem](#11-session-and-csrf-subsystem)
12. [Request Validation Pipeline](#12-request-validation-pipeline)
13. [Static Serving](#13-static-serving)
14. [Data API Implementation](#14-data-api-implementation)
15. [Storage Engine](#15-storage-engine)
16. [Path Confinement and Identifier Validation](#16-path-confinement-and-identifier-validation)
17. [Write Lock and Quota Enforcement](#17-write-lock-and-quota-enforcement)
18. [Revision Tracking and Conflict Detection](#18-revision-tracking-and-conflict-detection)
19. [Update Application](#19-update-application)
20. [Browser Detection and Launch](#20-browser-detection-and-launch)
21. [Diagnostics and Logging](#21-diagnostics-and-logging)
22. [Error Taxonomy and Propagation](#22-error-taxonomy-and-propagation)
23. [Concurrency Model](#23-concurrency-model)
24. [Memory and Performance Budgets](#24-memory-and-performance-budgets)
25. [Build, Signing, and Distribution](#25-build-signing-and-distribution)
26. [Testing Strategy](#26-testing-strategy)
27. [Open Questions and Deferred Decisions](#27-open-questions-and-deferred-decisions)

**Appendices**

- [A. Go Interface Sketches](#appendix-a-go-interface-sketches)
- [B. Configuration Reference](#appendix-b-configuration-reference)
- [C. Log Event Catalogue](#appendix-c-log-event-catalogue)
- [D. Requirement Coverage Map](#appendix-d-requirement-coverage-map)

---

## 1. Overview and Design Principles

### 1.1 What the Launcher Is

The launcher is a single self-contained native binary that runs with the user's own file permissions. It is the only component in the architecture that touches the filesystem. Its responsibilities, in dependency order:

1. **Resolve** the game folder from its own executable path (FS §13.1, FR-LNCH-1).
2. **Load and validate** `launcher/launcher.config.json` against the schema (FS Appendix B).
3. **Acquire** the per-game instance lock, or hand off to the running instance (FS §6.7).
4. **Allocate** a stable, user-confirmed port (FS §6, FR-PORT-1…4).
5. **Probe** the data directory for write access and record the result (FR-SAVE-16).
6. **Start** the loopback HTTP server bound to `127.0.0.1` (FR-SRV-1…3).
7. **Launch** a supported browser at the origin with a bootstrap token (FS §13.4).
8. **Serve** `game/` read-only and `data/` through the authenticated data API (FS §7, §8).
9. **Persist** saves and config with atomic writes and server-owned revisions (FS §11).
10. **Drain and exit** when the session goes idle or a signal arrives (FR-SRV-19…21).

Nothing else. The launcher does no rendering, holds no game state, and never contacts the network outbound except when the user initiates an update check (FR-UPD-2).

### 1.2 Design Principles

These principles resolve the majority of "which way should this go" questions during implementation.

1. **The filesystem is the only source of truth.** No state is authoritative in the browser, in memory beyond a session, or in the sidecar. The sidecar holds machine-local preferences (port, lock, logs) whose loss is recoverable; the game folder holds everything the user cares about.
2. **Fail explicitly, never silently.** A failed write probe, a blocked port, an unwritable sidecar, a stale lock — each is a discrete, logged event with a defined degradation path (FS §16). The launcher never falls back to a mode the user has not been told about.
3. **Validate before touching disk.** Every identifier is validated by regex before it becomes a path component. Every request is validated for Host, Origin, session, and CSRF before it reaches a handler. The order is fixed and is not negotiable per-endpoint (FS §12).
4. **The browser is untrusted, the launcher is trusted.** The page can ask; the launcher decides. There is no capability the page can hold that the server does not grant per request.
5. **Atomicity is the launcher's job, not the filesystem's.** `fsync` then `rename` is the durability primitive. `O_EXCL`, file locks, and mtimes are not relied on, because none of them is portable across the media this architecture targets (FS C7, C8; §15.3).
6. **One binary, no runtime.** Go's standard library covers HTTP, JSON, crypto, process control, and filesystem primitives. No cgo, no external service, no shared library (FR-LNCH-2, FR-LNCH-4).
7. **Prefer a small surface that is fully testable over a large surface with untested branches.** Every handler has a contract test; every error code is enumerated (Appendix C).

### 1.3 Relationship to the Functional Specification

| FS area | This document's sections |
|---------|--------------------------|
| §5 Folder structure | §5, §6, §7 |
| §6 Port allocation | §8, §9 |
| §7 Server security and lifecycle | §10, §11, §12, §13, §23 |
| §8 Data API | §14, §16, §17, §18 |
| §11 Save system | §15, §18 |
| §12 Updates | §19 |
| §13 Launcher | §2, §4, §20, §25 |
| §14 Threat model | §12, §16, §17 |
| §16 Error matrix | §22, Appendix C |

---

## 2. Process Model and Lifecycle

### 2.1 Process Overview

The launcher is a single OS process with a bounded lifetime. It owns:

- one listening TCP socket on `127.0.0.1:<port>`,
- one sidecar lock file,
- a bounded set of in-memory structures (sessions, write lock, revision cache, quota counters),
- an append-only log file in the sidecar directory.

It spawns exactly one child process: the browser (FS §13.4). It does not spawn a shell, does not fork a worker, and does not daemonize.

### 2.2 State Machine

```
       ┌──────────┐
       │  START   │
       └────┬─────┘
            │ resolve exe → game folder → config
            ▼
       ┌──────────┐  no lock / stale lock
       │  ACQUIRE │───────────────────────┐
       └────┬─────┘                       │
            │ live lock, same game        │
            │                             ▼
            │                    ┌────────────────┐
            │                    │  HANDOFF/EXIT  │  (open existing origin, exit 0)
            │                    └────────────────┘
            ▼
       ┌──────────┐
       │ ALLOCATE │  probe → propose → confirm → bind
       └────┬─────┘
            │
            ▼
       ┌──────────┐  probe data/ → data_writable=true|false
       │  PROBE   │
       └────┬─────┘
            │
            ▼
       ┌──────────┐  start heartbeat watcher, open browser
       │  SERVE   │◄──────────────────┐
       └────┬─────┘                   │
            │                         │ heartbeat received (last_seen updated)
            │ idle_timeout exceeded   │
            │ or SIGINT/SIGTERM       │
            │ or console close        │
            ▼                         │
       ┌──────────┐                   │
       │  DRAIN   │  stop accept, wait in-flight writes ──┘
       └────┬─────┘
            │
            ▼
       ┌──────────┐  remove lock, flush log, close socket
       │  EXIT    │
       └──────────┘
```

**Guards against every transition being re-entered:**

- `ACQUIRE → ALLOCATE` is single-pass; a failed acquire does not retry, it either hands off or exits.
- `ALLOCATE → PROBE` proceeds with whatever port was bound, including a fallback after `EADDRINUSE` (FR-SRV-23).
- `SERVE → DRAIN` is triggered exactly once; a second signal during drain is ignored (the drain is bounded by `drain_timeout_seconds`, FR-SRV-20).

### 2.3 Lifecycle Timings

| Event | Default | Source |
|-------|---------|--------|
| Heartbeat cadence, visible page | 5 s | FR-SRV-18 |
| Heartbeat cadence, hidden page | 30 s | FR-SRV-18 |
| Idle timeout | 90 s | FR-SRV-19 |
| Drain timeout | 15 s | FR-SRV-20 |
| Bootstrap token TTL | 120 s | FR-SRV-7 |
| Session TTL | 8 h | FR-SRV-7 |
| Session inactivity timeout | 30 min | FR-SRV-7 |
| `--max-lifetime` (testing only) | off | FR-SRV-22 |

### 2.4 Signal Handling

| Signal / event | Platform | Behaviour |
|----------------|----------|-----------|
| `SIGINT` | POSIX | Start drain, then exit 0. |
| `SIGTERM` | POSIX | Start drain, then exit 0. |
| `SIGQUIT` | POSIX | Dump goroutine stacks to the log, then treat as `SIGTERM`. Debug builds only; in release it is treated as `SIGTERM`. |
| `CTRL_CLOSE_EVENT` | Windows | Start drain, then exit 0. Windows grants a short window; the drain is bounded and any incomplete temp file is recorded, not deleted (FR-SRV-20). |
| `CTRL_C_EVENT` | Windows console | Same as `SIGINT`. |
| `SIGHUP` | POSIX | Reload the log file handle (rotation). No server state is reloaded. |

A second `SIGINT`/`SIGTERM` during drain is logged and ignored; the process continues draining. If the drain exceeds its timeout, in-flight writes are abandoned with a recovery note and the process exits 0.

### 2.5 Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Normal exit: idle shutdown, signal drain, or handoff to an existing instance. |
| 1 | Startup failure with a user-facing message (no compatible browser, config invalid, sidecar and fallback both unwritable). |
| 2 | Reserved for future use; currently unused. |
| 3 | Update application failed and the game was left in a runnable state (the previous release is intact). |
| 4 | Panic recovered by the top-level handler (FR-SRV-24). The log path is included in the OS dialog. |

Exit code is the launcher's only machine-readable channel to a calling script; the OS dialog and log are for humans. `--print-url`, `--check-port`, and `--diagnostics` follow a separate contract (Appendix B).

Exit code 3 is produced when an update was the launcher's *only* work and failed: `--repair` (and, in a future `apply` subcommand, any invocation that exists to update). A failure of the startup apply of §4 step 3b does **not** produce 3 by itself, because the launcher then proceeds to serve the installed release; the failure is recorded in `result.json` and the `update.apply.failed` event, and the session exits normally. This is the reading of Updater spec R7.9 that keeps the exit contract honest: the code describes how the process ended, not what it would have done.

---

## 3. Repository and Package Layout

### 3.1 Top-level Layout

```
launcher/
├── cmd/
│   └── kobra-launcher/
│       └── main.go               # flag parsing, orchestration, exit codes
├── internal/
│   ├── config/                   # launcher.config.json load + schema validation
│   ├── paths/                    # game folder resolution, sidecar location
│   ├── sidecar/                  # port.json, instance.lock, log file management
│   ├── port/                     # deterministic default, probe, deny list, confirmation
│   ├── server/                   # HTTP server, routing, middleware
│   ├── session/                  # bootstrap token, session store, CSRF
│   ├── storage/                  # atomic writes, revisions, write lock, quotas
│   ├── dataapi/                  # /api/* handlers, request validation, error envelopes
│   ├── static/                   # game/ and assets/ serving, MIME, ranges, caching
│   ├── update/                   # manifest check, archive download, verification,
│   │                             #   extraction, directory swap, trigger state
│   ├── browser/                  # detection, version check, launch
│   └── diagnostics/              # structured log, ring buffer, diagnostics endpoint
├── schemas/                      # vendored JSON Schemas (Appendix B of the FS)
├── port-deny-list.json           # shared with the shell (FS §6.4)
├── go.mod
├── go.sum
└── Makefile
```

### 3.2 Package Boundaries

Each `internal/` package has a narrow responsibility and a documented interface (Appendix A). The dependency graph is acyclic and flows one way:

```
main → config, paths, sidecar, port, server, update, browser, diagnostics
server → session, storage, dataapi, static, diagnostics
dataapi → storage, paths, diagnostics
storage → paths, diagnostics
static → paths, diagnostics
update → storage, paths, diagnostics
port → sidecar, diagnostics
paths → diagnostics
```

No package imports `main`. No package imports `net/http` except `server`, `dataapi`, and `static`. `storage` is the only package that calls `os.WriteFile`, `os.Rename`, `os.OpenFile`, or `os.Remove` on game data.

### 3.3 Build Constraints

- **No cgo.** `CGO_ENABLED=0` for every target triple. This keeps the binary self-contained and reproducible.
- **No `unsafe` outside `storage` and only where provably needed** (currently: none).
- **No network imports outside `update`** (`net/http` as a client) and `browser` (never; browser launch uses `os/exec`). The CSP forbids the *page* from network access; the launcher's own outbound client exists only for the user-initiated update check (FR-UPD-2).
- **Vendored dependencies only.** `go mod vendor` is committed; the build does not touch the network.
- **Reproducible builds.** `-trimpath`, `-buildvcs=false` (version injected via `-ldflags`), and a pinned Go toolchain in `go.mod`.

### 3.4 Vendored Dependencies

The intended dependency set is deliberately tiny:

| Dependency | Purpose | Justification |
|------------|---------|---------------|
| `github.com/santhosh-tekuri/jsonschema/v5` | JSON Schema 2020-12 validation | Schema validation is required by FR-SCH-3; hand-rolling a validator is more code than vendoring one. |
| `github.com/klauspost/compress/zstd` | zstd for patch archives | `archive/tar` + this codec covers patch extraction without shelling out. |
| `golang.org/x/sys` | `unix` and `windows` syscall wrappers | Signal handling and PID-liveness checks that the stdlib does not expose portably. |

Everything else — HTTP, JSON, crypto/rand, hashing, exec, file I/O — is standard library.

---

## 4. Startup Sequence

`main.go` runs a fixed, ordered sequence. Any step that fails produces a specific message and exit code (§22). Steps are ordered so that cheap, user-visible failures happen first.

```
1.  Parse flags.                          → exit 1 on unknown flag
2.  Resolve executable path.              → exit 1 if os.Executable fails
3.  Resolve game folder.                  → exit 1 if layout invalid (FR-LNCH-1)
3a. Finish/undo an interrupted swap.      → §19.4 recovery, before the strict check
3b. Apply a pending update.               → Updater spec §7; downloads, verifies,
                                             extracts and swaps before any socket
4.  Load + validate launcher.config.json. → exit 1 on schema failure (FR-SCH-3)
5.  Initialise diagnostics log.           → fall back to stderr on sidecar failure
6.  --check-port / --diagnostics / --print-url fast paths return here.
7.  Acquire or detect instance lock.      → handoff and exit 0 if live (FR-SRV-25)
8.  Allocate port:                        → user prompt unless --yes/--port
       probe → propose → validate → bind  → EADDRINUSE → re-enter once (FR-SRV-23)
9.  Probe data/ write access.             → set data_writable; log reason on failure
10. Start HTTP server on the bound socket.
11. Write sidecar port.json.
12. Detect a compatible browser.          → exit 1 with install guidance if none (FR-LNCH-7)
13. Generate bootstrap token.
14. Open browser at origin#t=token.
15. Start heartbeat watcher (idle shutdown).
16. Block until drain trigger.
17. Drain: stop accept, wait in-flight writes, flush log, remove lock.
18. Exit 0.
```

Steps 3a and 3b run before the strict layout check because §19.4's own scenario can
leave `game/` absent — exactly the state `paths.Resolve` rejects. Recovery must
therefore be able to restore a runnable release before the launcher insists on finding
one, and the apply of Updater spec §7 may then replace it outright. Both run before the
instance lock, the port, `port.json`, the browser and every socket, so no client can
observe a half-swapped game folder.

### 4.1 Fast Paths

Three flags short-circuit the sequence before any user interaction:

- `--check-port [n]` — run steps 1–4, then probe the candidate (or the deterministic default) and print a one-line JSON result. No lock, no server, no browser.
- `--print-url` — run steps 1–11, print the origin (including the bootstrap token fragment) to stdout, and block on the server. Intended for embedding and for CI.
- `--diagnostics` — run steps 1–6, print the diagnostics payload (the same one `GET /__kobra/diagnostics` returns; §21.4) and exit.

`--print-url` implies `--no-open`; it is otherwise incompatible with `--check-port` and `--diagnostics`.

`--check-port` and `--diagnostics` answer "can I run?" and MUST NOT mutate the install
to do it, so they run step 3a (recovery) but skip step 3b (apply). `--print-url` runs
both, because the server it starts must serve the release the user asked for.

### 4.2 Ordering Rationale

- **Lock before port.** A second instance must not transiently claim the port; the lock is the authoritative "another launcher for this game is alive" signal.
- **Port before browser.** The browser cannot be opened at a URL whose port is not yet bound.
- **Write probe before server start.** `data_writable` is part of `/api/state`, which the shell reads on boot; the probe must complete before the first request can be served.
- **Token after server start.** The token is generated once the server is listening, so a token cannot exist for a socket that is not yet accepting.

---

## 5. Game Folder and Path Resolution

### 5.1 The Executable Path Rule

**FR-LNCH-1 is load-bearing.** The game folder is derived exclusively from the launcher's own executable path, never from the working directory, the `$PWD`, or an environment variable.

```
1. exePath = os.Executable()                        # resolved, symlink-evaluated
2. exeDir  = filepath.Dir(exePath)                   # e.g. <root>/launcher
3. root    = filepath.Dir(exeDir)                    # e.g. <root>
4. verify  root/launcher/launcher.config.json exists
5. verify  root/game/index.html exists
6. verify  root/data/ exists or can be created
```

If step 4 or 5 fails, the launcher searches upward by at most one directory for a folder that contains a valid `launcher/launcher.config.json` and a `game/` subdirectory. This accommodates the common case of a user extracting a nested `GameName/GameName/` layout. It does not search downward, sideways, or by name; an unbounded search is a path-confusion risk.

### 5.2 Failure Modes

| Condition | Behaviour |
|-----------|-----------|
| `os.Executable()` fails | Exit 1. Message: *"Couldn't determine where the launcher is running from. Try moving the game folder to a local drive."* |
| `launcher.config.json` missing at root and one level up | Exit 1. Message names the expected path. |
| `game/index.html` missing | Exit 1. Message: *"The game files are missing. Re-extract the game archive, or apply the latest update."* |
| `data/` missing and cannot be created | Proceed; the write probe (step 9) records `data_writable: false` and the launcher starts in degraded mode (FR-SAVE-17). |
| Symlinked launcher executable | The symlink is resolved to its target before deriving `exeDir`. A symlinked launcher therefore resolves to the target's game folder, not the symlink's. Documented as a known behaviour, not a bug. |

### 5.3 Path Canonicalisation

Every path the launcher stores or serves is canonicalised once, at resolution time, and held as a `paths.Root` value:

```go
type Root struct {
    GameFolder string   // absolute, canonical
    GameDir    string   // <GameFolder>/game
    DataDir    string   // <GameFolder>/data, or the --data-dir override
    LauncherDir string  // <GameFolder>/launcher
    SidecarDir string   // OS-specific; §6
    LogDir     string   // <SidecarDir>/logs
}
```

`paths.Resolve` calls `filepath.Abs` then `filepath.Clean` and, on platforms where it matters, `filepath.EvalSymlinks` on the *root only* (never on served paths; §16.4). The `Root` value is immutable after construction and is passed by value or by read-only interface; no handler derives a path from `os.Getwd()`.

---

## 6. Sidecar State

### 6.1 Location

The sidecar directory lives outside the game folder (FS §5.3), because port selection, the instance lock, and logs are machine-local, not folder-local.

| OS | Path |
|----|------|
| Windows | `%LOCALAPPDATA%\KobraGames\<game-id>\` |
| macOS | `~/Library/Application Support/KobraGames/<game-id>/` |
| Linux | `${XDG_STATE_HOME:-~/.local/state}/kobra-games/<game-id>/` |

`game-id` is taken from `launcher.config.json` and is validated by the schema's `^[a-z0-9]+(\.[a-z0-9-]+)+$` pattern before it is used as a directory name.

### 6.2 Contents

| File | Purpose | Recovery if missing |
|------|---------|---------------------|
| `port.json` | Chosen port, origin, `origin_history` (last 5). | The deterministic default is used (FS §6.2). |
| `instance.lock` | PID + process start time + launcher version. | Treated as absent. |
| `logs/launcher.log` | Structured append-only log, rotated at 1 MB × 5 files. | Recreated. |
| `logs/crash-<ts>.txt` | A panic stack dump (FR-SRV-24). | Not recreated; a support artefact. |

### 6.3 Fallback

If the sidecar directory cannot be created — a locked-down machine, a roaming profile with no writable state directory, a container with no `$HOME` — the launcher falls back to `<GameFolder>/.kobra/` and records the fallback as a `sidecar.fallback` event in the log and in the diagnostics payload (E26).

**The fallback has a documented consequence:** the sidecar state (including the chosen port) now travels with the game folder, which can conflict when the folder is used from two machines with different port availability. This is stated in the diagnostics pane, not hidden.

### 6.4 `port.json` Schema

```json
{
  "schema": "kobra.port-state/1",
  "game_id": "com.kobra.stardrifter",
  "port": 8771,
  "origin": "http://127.0.0.1:8771",
  "origin_history": [
    { "origin": "http://127.0.0.1:8770", "last_seen": "2026-09-13T14:30:00Z" },
    { "origin": "http://127.0.0.1:8771", "last_seen": "2026-09-15T09:12:44Z" }
  ],
  "updated": "2026-09-15T09:12:44Z"
}
```

`origin_history` is capped at 5 entries, oldest-first eviction. It exists solely to power the "I changed the port and lost my browser state" support path (FS §6.6); it holds no authoritative data.

Writes to `port.json` follow the same atomic protocol as data files (`storage.WriteAtomic`; §15.3), because a torn `port.json` is a startup failure.

---

## 7. Configuration Loading

### 7.1 Source

`launcher/launcher.config.json` is read once at startup and validated against `kobra.launcher-config/1` (FS Appendix B). Validation is not optional: a config that fails schema validation is a startup failure (exit 1), not a warning, because every subsequent subsystem reads its policy from this file.

### 7.2 Merge Order

| Layer | Source | Overrides |
|-------|--------|-----------|
| 1. Schema defaults | The JSON Schema's `default` keywords | — |
| 2. Shipped config | `launcher/launcher.config.json` | Layer 1 |
| 3. CLI flags | `--port`, `--data-dir`, `--no-open`, `--log-level`, etc. | Layer 2 |

Environment variables are **not** consulted. A launcher's behaviour must not change based on ambient environment state, both for reproducibility and because environment variables are a favourite of exploitation chains.

### 7.3 Derived Values

Several runtime values are derived, not configured:

| Derived value | From | Rule |
|---------------|------|------|
| `defaultPort` | `game_id`, `port.base`, `port.span` | `base + fnv1a32(game_id) % span` (FS §6.2) |
| `denySet` | `port.deny_list_file` | Parsed once at startup; §8.4 |
| `csp` | `server.csp` if set, else the built-in default | The default is the strict policy in FR-SRV-17. |
| `bootstrapTTL` | `server.bootstrap_token_ttl_seconds` | Clamped to [10, 600]. |
| `idleTimeout` | `server.idle_timeout_seconds` | Clamped to [30, 3600]. A value below 30 is rejected, not clamped silently. |

`fnv1a32` is the standard 32-bit FNV-1a hash, implemented locally, over the UTF-8 bytes of `game_id`. It is stable across launches and OSes; the test suite pins known inputs to known outputs (a golden test; §26.3).

---

## 8. Port Allocation Subsystem

**Satisfies:** FR-PORT-1…4, FR-SRV-23, and FS §6 in full.

### 8.1 Candidate Selection

```
func SelectCandidate(sidecar, config) (candidate uint16, source Source):
    if sidecar.port.json exists and parses:
        return sidecar.port, SourceSaved
    return DefaultPort(config), SourceDefault

func DefaultPort(config) uint16:
    return config.Port.Base + fnv1a32(config.GameID) % config.Port.Span
```

Saved port wins over the deterministic default. This is what makes the origin stable across launches (FR-PORT-1) on the common path.

### 8.2 Probing

A port's availability is determined by *connecting and asking*, not by attempting a bare TCP connect (FS §6.3). The probe:

```
GET http://127.0.0.1:<candidate>/__kobra/probe   (timeout 500 ms)
```

| Response | Interpretation |
|----------|----------------|
| Connection refused | Free. |
| `200` with `{"app":"kobra-launcher","game_id":"<our id>"}` | Occupied by us — a second instance is running. |
| `200` with `{"app":"kobra-launcher","game_id":"<other>"}` | Occupied by another Kobra game. |
| Any other status or body | Occupied by another application. |

The probe is deliberately the same endpoint the shell and the diagnostic tooling use (FR-SRV-9); there is exactly one well-known unauthenticated path, and it is opaque.

### 8.3 Bind and the TOCTOU Window

Probing is advisory; **binding is authoritative.** The launcher always:

1. binds `127.0.0.1:<candidate>` with `net.Listen`,
2. on success, treats the port as taken,
3. on `EADDRINUSE`, re-enters the allocation flow once (FR-SRV-23).

The re-entry scans `base..base+span-1` (excluding the deny set) for the first port whose probe returns "free" or "ours". If the scan exhausts the window, the launcher widens to `1024..49151` excluding the deny set and the reserved ephemeral range. If that also fails, it exits 1 with *"No usable port could be found. Close other applications or choose a port manually with --port."*

### 8.4 Deny-List Validation

`port-deny-list.json` (FS Appendix B) is loaded once and reduced to a set of denied ports and a list of denied ranges. Validation runs on every candidate, whether it came from the sidecar, the deterministic default, a scan, or the user:

```go
func ValidatePort(p uint16, deny *port.DenyList) error {
    switch {
    case p < 1024:
        return ErrPrivilegedPort
    case deny.Contains(p):
        return deny.Reason(p)   // carries a user-facing message
    case p >= 49152:
        return ErrEphemeralPort
    default:
        return nil
    }
}
```

The deny list is shared with the shell. If the two disagree — the shell's copy is stale — the launcher's decision is authoritative, and a `port.deny.mismatch` event is logged (§21.2).

### 8.5 Confirmation Prompt

**FR-PORT-2** requires the user to confirm or edit the port. The prompt is a native OS dialog on Windows and macOS, and a terminal prompt on Linux, unless `--yes` or `--port` is supplied (FR-A11Y-5).

The prompt displays:

```
The game will be served at:

    http://127.0.0.1:8771

[ Change ]  [ OK ]
```

On **Change**, the user may type a port. The port is re-validated (deny list, privileged range, ephemeral range, availability). If it is taken, the dialog names the owner:

> *Port 8771 is used by another program. Use 8772?*

The prompt times out to the proposed default after 60 seconds, so an unattended launcher does not hang (FR-A11Y-5). The timeout is recorded in the log.

### 8.6 Persistence

After a successful bind, the launcher writes `port.json` (§6.4), appending the new origin to `origin_history` and truncating to 5. It does **not** delete the old entry, because the user may want to restore the old port to recover browser-stored state (FS §6.6).

### 8.7 `--port` and `--yes` Behaviour

| Flags | Behaviour |
|-------|-----------|
| `--port N` | Use `N` without prompting. Validated; on failure, exit 1 (the flag is explicit intent, and silently substituting a different port would violate it). |
| `--yes` | Accept the proposed candidate without prompting. On conflict, fall through to the scan and take the first free port, logging the substitution. |
| `--port N --yes` | `--port` wins; `--yes` is ignored. |
| `--check-port [N]` | Print the probe result and exit, regardless of `--yes` or `--port`. |

---

## 9. Instance Locking and Single-Instance Election

**Satisfies:** FR-SRV-25, FS §6.7, §6.8.

### 9.1 The Lock File

`instance.lock` is a JSON document:

```json
{
  "schema": "kobra.instance-lock/1",
  "pid": 41217,
  "start_time": "2026-09-15T09:12:44Z",
  "launcher_version": "1.4.2",
  "game_id": "com.kobra.stardrifter",
  "port": 8771
}
```

`start_time` is the wall-clock time the launcher recorded at lock acquisition, not the OS process start time, because the latter is not portable across the target platforms without a syscall wrapper. PID reuse is detected by comparing the recorded `start_time` against the *live* process's start time read from the OS where available, and by the health check below where not.

### 9.2 Acquisition

```
1. Try to create instance.lock with O_CREATE|O_EXCL.
   ├─ success → write our identity, hold the file open (advisory), proceed.
   └─ exists  → read it.
        ├─ parse fails → treat as stale; reclaim.
        └─ parse ok    → liveness check (§9.3).
```

### 9.3 Liveness Check

The lock is considered **stale** if any of the following holds:

1. The recorded PID does not exist.
2. The recorded PID exists but its start time does not match `start_time` (PID reuse).
3. `GET http://127.0.0.1:<recorded port>/__kobra/health` returns anything other than a matching `{"app":"kobra-launcher","game_id":"<ours>","instance":"<recorded>"}` within 500 ms.
4. The lock file's modification time is older than the process's own boot time on the same machine (a heuristic that catches a lock left behind by a VM snapshot).

The health check is the primary signal; the others are backstops for the case where the port was reused by a different process.

### 9.4 Handoff

If the lock is live and the `game_id` matches, the launcher **does not start a second server**. It:

1. logs `instance.handoff {existing_port: <n>}`,
2. opens a browser at the existing origin (without a token — the existing instance's session model applies),
3. exits 0.

**FR-SRV-25**'s "second tab is served normally" applies to the same origin; the handoff case is for a second *launcher process* against the same game folder.

### 9.5 Two Folders, Same Game

If the lock belongs to the same `game_id` but a different game folder, the launcher must not hijack it. Per FS §6.7, it prompts:

> *This game is already running from another folder. [ Open that one ] [ Run this copy on a different port ]*

If the user chooses "Open that one", the launcher opens the existing origin and exits 0. If "Run this copy", it proceeds to allocation with the deny set expanded to exclude the existing instance's port, and writes its own `port.json` under a *folder-scoped* sidecar key (`game_id + folder hash`) to avoid clobbering the first instance's `port.json`.

### 9.6 Reclamation and Stale Locks

On reclaiming a stale lock, the launcher:

1. logs `instance.lock.reclaimed {stale_pid, reason}`,
2. removes the lock file,
3. retries acquisition once.

If the lock cannot be reclaimed (another process won the race), the launcher falls into the liveness check again and either hands off or exits 1 with *"Another launcher is starting. Try again in a moment."*

### 9.7 Release

The lock is released in the `DRAIN → EXIT` transition (§2.2), after the socket is closed and the log is flushed. If the process is killed before release, the lock is stale on the next startup and is reclaimed by §9.3.

---

## 10. HTTP Server

**Satisfies:** FR-SRV-1…25.

### 10.1 Listener

```go
ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
```

`tcp4` is explicit: it binds the IPv4 loopback only. Binding `tcp` would accept the IPv6 form, which **FR-SRV-3** forbids. The listener is created with `SO_REUSEADDR` on POSIX but **not** `SO_REUSEPORT`, so a second bind fails rather than silently sharing the socket.

### 10.2 `http.Server` Configuration

| Field | Value | Rationale |
|-------|-------|-----------|
| `Addr` | (empty; the listener is passed to `Serve`) | The listener is bound before `Serve`, so `EADDRINUSE` is caught at bind time (§8.3). |
| `ReadHeaderTimeout` | 5 s | Bounds a slow-header attack from a local process. |
| `ReadTimeout` | 30 s | A request body is bounded by `max_request_bytes` (§17.4); this is the wall-clock backstop. |
| `WriteTimeout` | 0 (disabled) | Static file serving can exceed any fixed timeout on a slow network share. Handlers enforce their own bounds. |
| `IdleTimeout` | 60 s | Keep-alive idle ceiling. |
| `MaxHeaderBytes` | 32 KiB | Far above any legitimate request. |
| `ErrorLog` | A `log.Logger` writing to the diagnostics ring buffer | Nothing goes to stderr in release. |

`WriteTimeout` being disabled is a deliberate exception; it is safe because every handler writes to a bounded buffer or streams a file with a known length.

### 10.3 Middleware Order

Middleware is applied in a fixed, documented order. The order matters: a request must pass a cheaper, earlier check before it reaches a more expensive one.

```
1. Panic recovery            → FR-SRV-24
2. Request ID + logging      → §21
3. Host validation           → FR-SRV-4
4. Origin validation         → FR-SRV-5
5. Security headers          → FR-SRV-16, FR-SRV-17
6. Static / probe routing    → §13
7. Session gate              → FR-SRV-8
8. CSRF gate (writes only)   → FR-SRV-6a
9. Route handler             → §14
```

Steps 3 and 4 are cheap header comparisons and run before anything touches the session store or the filesystem. Step 5 runs before any response so that even an error response carries the security headers. Steps 7 and 8 are skipped for `/__kobra/probe`, `/__kobra/health`, and the static `game/` namespace (FR-SRV-8).

### 10.4 `Server` Struct

```go
type Server struct {
    root     paths.Root
    cfg      config.Config
    listener net.Listener
    sessions *session.Store
    storage  *storage.Engine
    log      *diagnostics.Logger

    lastSeen atomic.Int64      // unix nanos, updated by heartbeat
    started  time.Time
    instance string            // opaque per-process id
    writer   atomic.Pointer[string] // current writer session id
}
```

`lastSeen` and `writer` are the only mutable atomics the server exposes; everything else is either immutable or guarded by the storage engine's locks.

### 10.5 Heartbeat and Idle Shutdown

A single goroutine wakes every second and checks:

```go
if time.Since(lastSeen) > cfg.Server.IdleTimeout {
    s.initiateShutdown(ReasonIdle)
}
```

The heartbeat endpoint (`POST /__kobra/heartbeat`) updates `lastSeen` and nothing else; it does not allocate, does not log at info level, and does not require CSRF (it is authenticated by session, which is enough for a state update that only affects shutdown timing).

The `pagehide` goodbye (§10.6) is a `navigator.sendBeacon()` POST to `/__kobra/goodbye`, which sets `lastSeen` to `now - idle_timeout` so that the next tick triggers shutdown immediately.

### 10.6 Drain

`initiateShutdown` sets a `draining` flag (atomic), which makes the middleware reject new requests with `503 Service Unavailable` and `Connection: close`. In-flight requests are allowed to complete; the storage engine's write lock guarantees that any in-flight save finishes or aborts cleanly (§15.6). After `drain_timeout_seconds` (default 15 s), the listener is closed, the log is flushed, the lock is removed, and `main` returns.

A request that starts after the drain flag is set is rejected with `503`; the shell's retry logic treats `503` as a signal that the session is ending.

---

## 11. Session and CSRF Subsystem

**Satisfies:** FR-SRV-6a, FR-SRV-7, FR-SRV-8; FS Appendix C.1.

### 11.1 Bootstrap Token

The token is generated at startup with `crypto/rand`:

```go
raw := make([]byte, 32)               // 256 bits
rand.Read(raw)
token := base64.RawURLEncoding.EncodeToString(raw)   // 43 chars
```

It is stored in memory with a `created` timestamp and a `used` flag. The token is passed to the browser in the URL fragment (`#t=<token>`), which is never sent in the request line, never logged by the server, and erased by the shell with `history.replaceState()`.

### 11.2 Session Establishment

```
POST /__kobra/session
Content-Type: application/json

{ "token": "<43-char base64>" }
```

On success:

- `Set-Cookie: kobra_session=<opaque>; HttpOnly; SameSite=Strict; Path=/`
- `Set-Cookie: kobra_csrf=<opaque>; SameSite=Strict; Path=/` (not `HttpOnly`)
- Body: `{"session_id":"<id>","csrf_token":"<opaque>","api_version":1}`

The `kobra_session` cookie value is a 256-bit random string; the `kobra_csrf` value is an independent 256-bit random string. The session store maps `kobra_session` value → `Session{ id, csrf, created, last_active, writer bool }`.

On failure (unknown token, expired token, already-used token), the response is `401 no_session` with a body that does **not** distinguish "unknown" from "expired" from "already used" (FR-API-13). The failure is logged with the requester's `Origin`.

### 11.3 Session Store

```go
type Store struct {
    mu       sync.Mutex
    sessions map[string]*Session   // keyed by kobra_session cookie value
    tokens   map[string]*bootstrap // pending bootstrap tokens
}

type Session struct {
    ID          string
    CSRF        string
    Created     time.Time
    LastActive  time.Time
    Writer      bool
}
```

Sessions are evicted on:
- expiry (`created + session_ttl`),
- inactivity (`last_active + 30 min`),
- shutdown.

The store is bounded: at most 16 concurrent sessions per game. A 17th session-establishment request is rejected with `429 rate_limited`. This bounds the memory a malicious local page could cause the launcher to hold.

### 11.4 CSRF Validation

The double-submit pattern is enforced for every write:

```go
func (s *Server) checkCSRF(r *http.Request, sess *Session) error {
    cookie, err := r.Cookie("kobra_csrf")
    if err != nil {
        return ErrBadCSRF
    }
    header := r.Header.Get("X-Kobra-CSRF")
    if header == "" {
        return ErrBadCSRF
    }
    if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
        return ErrBadCSRF
    }
    if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(sess.CSRF)) != 1 {
        return ErrBadCSRF
    }
    return nil
}
```

Comparison is constant-time to avoid leaking the token one byte at a time through timing. A failure returns `403 bad_csrf` and is logged at **warn** level with the request's `Origin`, because a CSRF failure against a loopback server is a security event (E16).

### 11.5 Session Claims

A session may claim the writer role via `POST /api/save` with `"claim": true`. The claim is atomic:

```go
func (s *Server) claimWriter(sess *Session) error {
    if !s.writer.CompareAndSwap(nil, &sess.ID) {
        if *s.writer.Load() == sess.ID {
            return nil   // idempotent
        }
        return ErrWriterHeld
    }
    sess.Writer = true
    return nil
}
```

`ErrWriterHeld` becomes `409 conflict` with `{"error":"conflict","detail":{"current_writer":"…"}}`. The shell interprets this as "another tab is the writer" (FR-SHELL-5).

---

## 12. Request Validation Pipeline

**Satisfies:** FR-SRV-4, FR-SRV-5, FR-SRV-6, FR-SRV-10, FR-SRV-12; FS §14.2 threats T1, T2, T3.

### 12.1 Host Validation

```go
host := r.Host
if host != "127.0.0.1:"+port {
    // 421 Misdirected Request, closed without body.
}
```

The comparison is exact. It does **not** accept `localhost:<port>`, `[::1]:<port>`, or any hostname. This is what defeats DNS rebinding (T1): a malicious page that resolves `evil.example` to `127.0.0.1` will still send `Host: evil.example`, and the request is rejected before any handler runs.

### 12.2 Origin Validation

If the request carries an `Origin` header, it must be exactly `http://127.0.0.1:<port>`. If it is present and different, the request is rejected with `403 bad_origin`. If it is absent (same-origin `GET`, `HEAD`, or a non-browser client), the check is skipped and the session/CSRF gates apply as usual.

This check applies to **every** request, not just writes (FR-SRV-5). A cross-origin `GET /api/data/saves` is a data-exfiltration attempt and is rejected.

### 12.3 Method Validation

| Route class | Allowed methods |
|-------------|----------------|
| `/`, `/index.html`, `/shell.js`, `/assets/*`, `/engine/*`, `/locales/*` | `GET`, `HEAD` |
| `/editor`, `/editor/*` | `GET`, `HEAD` |
| `/mods/*` (when `serve_mods`) | `GET`, `HEAD` |
| `/api/*` reads | `GET` |
| `/api/save`, `/api/config`, `/api/mod` | `POST` |
| `/api/save/{slot}`, `/api/mod/{id}` | `DELETE` |
| `/__kobra/session`, `/__kobra/heartbeat`, `/__kobra/goodbye` | `POST` |
| `/__kobra/probe`, `/__kobra/health` | `GET` |
| `/__kobra/shutdown`, `/__kobra/log` | `POST` |

A method mismatch is `405 Method Not Allowed` with an `Allow` header listing the permitted methods for that route.

### 12.4 Content-Type Validation

Write endpoints require `Content-Type: application/json` (or `application/octet-stream` for binary payloads, e.g. a mod archive). Anything else is `415 bad_content_type`.

The check is done on the *parsed* media type, not the raw header, so `application/json; charset=utf-8` is accepted and `application/json-something` is not.

### 12.5 Body Bounding

Before reading the body:

```go
if r.ContentLength > cfg.DataAPI.MaxRequestBytes {
    return errTooLarge   // 413
}
```

`Content-Length` is required for writes; a chunked request without it is `411 Length Required`. This is stricter than the HTTP spec allows but is appropriate for a loopback server whose only legitimate client is a shell that knows the size of what it sends.

After the length check, the body is read with an `io.LimitReader` capped at `MaxRequestBytes+1`, so a lying `Content-Length` still cannot cause an unbounded read.

### 12.6 The Validation Order, Restated

The order is fixed and is a security property:

1. **Host** — cheap, defeats rebinding.
2. **Origin** — cheap, defeats CSRF-via-cross-origin-read.
3. **Method** — cheap.
4. **Content-Type** — cheap.
5. **Content-Length** — cheap, bounds the body.
6. **Session** — hits the session store.
7. **CSRF** — hits the CSRF cookie and store.
8. **Identifier** — regex over the path parameters.
9. **Handler** — touches the filesystem.

A request that would fail step 8 must fail before step 9. A request that would fail step 6 must fail before step 7, so that an unauthenticated request never causes a CSRF comparison.

---

## 13. Static Serving

**Satisfies:** FR-SRV-10…17; FS §9.

### 13.1 Roots

| URL prefix | Filesystem root | Notes |
|------------|-----------------|-------|
| `/` and `/index.html` | `<GameFolder>/game/index.html` | `no-cache` |
| `/shell.js`, `/engine/*` | `<GameFolder>/game/` | `immutable` for engine, `no-cache` for shell |
| `/assets/*` | `<GameFolder>/game/assets/` | Per-manifest cache policy |
| `/mods/<id>/assets/*` | `<GameFolder>/data/mods/<id>/assets/` | Only when `serve_mods` and the mod is enabled |
| `/locales/*` | `<GameFolder>/game/locales/` | `no-cache` |
| `/editor`, `/editor/*` | `<GameFolder>/game/editor/` (`/editor` → `editor/index.html`) | `no-cache` for the document, asset policy for the subtree; `404` when the game ships no editor |

Static serving never reaches `data/saves/`, `data/config/`, or the launcher binary itself. A request for `/data/saves/slot1.json` does not match any static prefix and is not a data API route either; it is a `404`.

### 13.2 Path Resolution

```go
func (h *StaticHandler) resolve(root, urlPath string) (string, error) {
    // 1. urlPath is already URL-decoded by net/http; it must not contain NUL.
    if strings.ContainsRune(urlPath, 0) {
        return "", ErrBadPath
    }
    // 2. Reject Windows device names (CON, NUL, COM1..9, LPT1..9, AUX, PRN).
    if isWindowsDeviceName(urlPath) {
        return "", ErrBadPath
    }
    // 3. Clean and join.
    cleaned := path.Clean("/" + urlPath)          // guarantees a leading slash
    full := filepath.Join(root, filepath.FromSlash(cleaned))
    // 4. Confine: full must be within root.
    rel, err := filepath.Rel(root, full)
    if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
        return "", ErrBadPath
    }
    return full, nil
}
```

Every path parameter of a data API route is validated separately (§16); static serving never accepts a client-supplied filename other than through `resolve`.

### 13.3 Symlink Policy

Symlinks are followed, but the resolved target is re-checked against the root:

```go
realRoot, _ := filepath.EvalSymlinks(root)
realFull, err := filepath.EvalSymlinks(full)
if err != nil { return "", err }   // broken symlink → 404
rel, err := filepath.Rel(realRoot, realFull)
if err != nil || strings.HasPrefix(rel, "..") { return "", ErrBadPath }
```

The default build does **not** disable symlink support, because some users organize assets via symlinks. The re-check makes a symlink to `/etc/passwd` a `404`, not a disclosure.

### 13.4 MIME Types

MIME is set explicitly from `launcher.config.json`'s `mime_types` map, falling back to `application/octet-stream` with `Content-Disposition: attachment` for anything unknown (FR-SRV-13). The built-in defaults are:

```go
".wasm": "application/wasm",
".js":   "text/javascript",
".mjs":  "text/javascript",
".json": "application/json",
".html": "text/html",
".css":  "text/css",
".ogg":  "audio/ogg",
".mp3":  "audio/mpeg",
".png":  "image/png",
".webp": "image/webp",
".svg":  "image/svg+xml",
".woff2":"font/woff2",
```

`.wasm` → `application/wasm` is critical: `WebAssembly.instantiateStreaming` refuses any other type (FR-AST-2). A wrong `.wasm` MIME type is logged as a build error at startup, not discovered by the browser.

### 13.5 Range Requests

Single-range requests are supported for audio and video (FR-SRV-14):

- `Range: bytes=N-M` → `206 Partial Content` with `Content-Range`.
- `Range: bytes=N-` → `206` to EOF.
- `Range: bytes=-N` → `206` last N bytes.
- Multiple ranges → `416` (the server does not implement `multipart/byteranges`).
- Unsatisfiable → `416` with `Content-Range: bytes */<size>`.

`Accept-Ranges: bytes` is advertised on all asset responses.

### 13.6 Caching

| Path | `Cache-Control` |
|------|-----------------|
| `index.html`, `shell.js`, `locales/*`, `*.manifest.json` | `no-cache` |
| `engine/*` | `public, max-age=31536000, immutable` |
| `/assets/<content-hash>/…` (when `content_addressed`) | `public, max-age=31536000, immutable` |
| `/assets/…` (otherwise) | `public, max-age=3600` |

`ETag` is a SHA-256 prefix of the file contents, computed once per file and cached in memory for the session. `Last-Modified` is the file mtime. `If-None-Match` and `If-Modified-Since` are honoured (FR-SRV-15).

### 13.7 Security Headers

Every response, including errors, carries:

```
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Embedder-Policy: <server.cross_origin_embedder_policy>   # omitted when empty
Content-Security-Policy: <configured>
```

The CSP default is the strict policy in FR-SRV-17. `Cross-Origin-Embedder-Policy` is set from `server.cross_origin_embedder_policy` (empty | `require-corp` | `credentialless`) and omitted entirely when empty, because it breaks embedding of any cross-origin subresource that does not opt in with CORP or CORS. With COOP `same-origin` already present, either non-empty value makes the origin cross-origin isolated, which is what `SharedArrayBuffer` and WebAssembly threads require (FR-SRV-16a).

---

## 14. Data API Implementation

**Satisfies:** FR-API-1…13; FS Appendix C.

### 14.1 Route Table

| Route | Handler | Auth | CSRF |
|-------|---------|------|------|
| `GET /api/state` | `handleState` | session | — |
| `GET /api/data/saves` | `handleSavesList` | session | — |
| `GET /api/data/saves/{slot}` | `handleSaveRead` | session | — |
| `GET /api/data/config` | `handleConfigRead` | session | — |
| `GET /api/data/config/mods` | `handleModsRead` | session | — |
| `GET /api/export/{slot}` | `handleExport` | session | — |
| `POST /api/save` | `handleSaveWrite` | session | ✔ |
| `DELETE /api/save/{slot}` | `handleSaveDelete` | session | ✔ |
| `POST /api/config` | `handleConfigWrite` | session | ✔ |
| `POST /api/mod` | `handleModAction` | session | ✔ |
| `DELETE /api/mod/{id}` | `handleModDelete` | session | ✔ |
| `GET /api/update/check` | `handleUpdateCheck` | session | — |
| `POST /api/update/apply` | `handleUpdateApply` | session | ✔ |
| `POST /api/update/cancel` | `handleUpdateCancel` | session | ✔ |
| `GET /api/update/progress` | `handleUpdateProgress` | session | — |

Routes are registered on a `*http.ServeMux` with explicit method prefixes; no route matches more than one method.

`POST /api/update/apply` accepts no request body but still passes the §12.4
Content-Type gate, which applies to every write-method route regardless of body. It
records intent only; see §19.2 and Updater spec §6.1.

### 14.2 Request and Response Types

Types mirror `data-api.schema.json` (FS Appendix B). The Go structs are generated by hand and validated against the schema in tests; code generation was rejected because it obscures the small number of deliberate null-vs-absent distinctions.

```go
type SaveWriteRequest struct {
    Slot        string          `json:"slot"`
    IfRevision  *int64          `json:"if_revision,omitempty"`
    Claim       bool            `json:"claim,omitempty"`
    Compression string          `json:"compression,omitempty"`
    Payload     json.RawMessage `json:"payload"`
    Meta        *SaveMeta       `json:"meta,omitempty"`
}

type SaveWriteResponse struct {
    Slot     string `json:"slot"`
    Revision int64  `json:"revision"`
    Modified string `json:"modified"`
    Bytes    int64  `json:"bytes"`
}
```

`IfRevision` is a pointer so that "absent" (unconditional overwrite) is distinguishable from `0` (which would be a valid revision for a new file and is therefore rejected). This is the kind of distinction that a naive generated struct would lose.

### 14.3 Error Envelope

Every error response is:

```json
{
  "error": "<enum>",
  "message": "<user-presentable, no paths>",
  "detail": { ... },
  "retry_after_seconds": <int, optional>
}
```

The `message` is written by the handler, not derived from the underlying error's `Error()` method, because a Go error string can leak a path (`open /home/user/...`). A test asserts that no `message` contains a `/` followed by a non-whitelisted path segment, and that no `message` contains the OS username (§26.5).

### 14.4 Handler Contract

Every handler follows the same shape:

```go
func (s *Server) handleSaveWrite(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    sess := sessionFromContext(ctx)
    if err := checkCSRF(r, sess); err != nil { writeError(w, err); return }

    var req SaveWriteRequest
    if err := decodeJSON(r, &req, s.cfg.DataAPI.MaxRequestBytes); err != nil {
        writeError(w, err); return
    }
    if err := validateIdentifier(req.Slot); err != nil {
        writeError(w, err); return
    }

    res, err := s.storage.WriteSave(ctx, storage.SaveWrite{
        Slot:       req.Slot,
        IfRevision: req.IfRevision,
        Payload:    req.Payload,
        Meta:       req.Meta,
        SessionID:  sess.ID,
        Claim:      req.Claim,
    })
    if err != nil { writeError(w, err); return }

    writeJSON(w, http.StatusOK, SaveWriteResponse{
        Slot:     res.Slot,
        Revision: res.Revision,
        Modified: res.Modified.UTC().Format(time.RFC3339),
        Bytes:    res.Bytes,
    })
}
```

Handlers do not touch `os` directly. All filesystem work is delegated to `storage.Engine`, which is the single point of path confinement and atomicity.

### 14.5 `GET /api/state`

Returns the payload defined in `data-api.schema.json`. It is the shell's boot handshake:

```json
{
  "api_version": 1,
  "game_id": "com.kobra.stardrifter",
  "release": "2026.09.1",
  "engine_version": "2.4.0",
  "save_version": 3,
  "data_writable": true,
  "data_dir_kind": "game",
  "writer": null,
  "uptime_seconds": 42
}
```

`data_writable` comes from the startup probe (§15.7). `writer` is read from the atomic writer pointer (§11.5). The shell refuses to run against a major `api_version` it does not understand (FR-API-12).

### 14.6 `GET /api/data/saves`

The slot index is **advisory** (FS §11.3). The handler:

1. reads `data/saves/index.json` if present and fresh,
2. enumerates `data/saves/*.json` and reads each file's header,
3. merges, preferring the file's header over a stale index entry,
4. sets `rebuilt_index: true` if the index was missing or rebuilt,
5. returns the list sorted by `modified` descending.

A missing `index.json` is **not** an error. A corrupt `index.json` is logged and the handler falls through to enumeration. This is the "a lost index must never mean lost saves" rule (FS §11.3).

### 14.7 Deletion Semantics

`DELETE /api/save/{slot}` and `DELETE /api/mod/{id}` do **not** unlink. They move the target into `data/.trash-<timestamp>/`:

```
data/.trash-2026-09-15T09-12-44Z/
    saves/slot1.json
    saves/slot1.json.bak
```

The move uses `os.Rename` when the trash directory is on the same filesystem, falling back to copy-then-remove with the `EXDEV` handling of §15.4. Retention is governed by `trash_retention_days`; cleanup runs at startup, not on the delete path, so a delete never blocks on a slow filesystem.

---

## 15. Storage Engine

**Satisfies:** FR-SAVE-1…20, FR-API-4…7, FR-SRV-25; FS §11.

### 15.1 Role

`storage.Engine` is the only package that writes game data. It owns:

- the atomic write protocol (§15.3),
- the per-slot write lock (§17.1),
- the revision store (§18),
- the quota counters (§17.3),
- the write probe result (§15.7),
- the trash and backup rotation (§15.5).

Its public interface is intentionally small:

```go
type Engine struct { /* ... */ }

func (e *Engine) WriteSave(ctx context.Context, w SaveWrite) (SaveWriteResult, error)
func (e *Engine) ReadSave(ctx context.Context, slot string) ([]byte, error)
func (e *Engine) ListSaves(ctx context.Context) ([]SaveSummary, error)
func (e *Engine) DeleteSave(ctx context.Context, slot string) error
func (e *Engine) ReadConfig(ctx context.Context) ([]byte, error)
func (e *Engine) MergeConfig(ctx context.Context, m ConfigMerge) (RevisionResult, error)
func (e *Engine) ReadMods(ctx context.Context) (ModsView, error)
func (e *Engine) ModAction(ctx context.Context, a ModAction) error
func (e *Engine) Writer() string
func (e *Engine) DataWritable() bool
```

There is no `WriteFile(path, data)` general method. Every write names a *kind* of data (save, config, mods manifest), and the engine maps the kind to a validated directory.

### 15.2 The Two Writable Roots

| Root | Contents | Writable by |
|------|----------|-------------|
| `DataDir/saves/` | `<slot>.json`, `.bak`, `.tmp-*`, revision sidecars | Engine only |
| `DataDir/config/` | `settings.json`, `mods.json` | Engine only |
| `DataDir/mods/<id>/` | `mod.manifest.json`, `assets/*` | Engine only (install) |
| `DataDir/.trash-<ts>/` | Moved deletions | Engine only |

`DataDir` itself is either `<GameFolder>/data` or the `--data-dir` override (FR-SAVE-19). The engine records which, as `data_dir_kind` in `/api/state`.

### 15.3 Atomic Write Protocol

**FR-SAVE-5** is implemented as a single function used by every writer:

```go
func (e *Engine) writeAtomic(ctx context.Context, target string, data []byte, mode os.FileMode) error {
    dir := filepath.Dir(target)

    // 1. Temp file with a unique name.
    tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d-%d",
        filepath.Base(target), os.Getpid(), e.nextCounter()))
    f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
    if err != nil { return err }

    // 2. Write + fsync + close.
    if _, err := f.Write(data); err != nil {
        f.Close(); os.Remove(tmp); return err
    }
    if err := f.Sync(); err != nil {
        f.Close(); os.Remove(tmp); return err
    }
    if err := f.Close(); err != nil {
        os.Remove(tmp); return err
    }

    // 3. Verify the temp file's length.
    info, err := os.Stat(tmp)
    if err != nil || info.Size() != int64(len(data)) {
        os.Remove(tmp); return ErrVerifyFailed
    }

    // 4. Rotate the existing file to .bak (remove old .bak first).
    if _, err := os.Stat(target); err == nil {
        bak := target + ".bak"
        os.Remove(bak)
        if err := os.Rename(target, bak); err != nil {
            os.Remove(tmp); return err
        }
    }

    // 5. Atomic rename into place.
    if err := os.Rename(tmp, target); err != nil {
        // EXDEV fallback (§15.4)
        return e.renameCrossDevice(tmp, target)
    }

    // 6. fsync the directory to durably record the rename.
    if d, err := os.Open(dir); err == nil {
        d.Sync()
        d.Close()
    }
    return nil
}
```

**Step 6 is not optional.** On ext4 with `data=ordered` (the default), the rename itself is journaled, but on some filesystems (notably XFS and certain network mounts) a rename can be lost on power failure unless the directory is fsynced. The target hardware includes USB sticks, and a user pulling a stick during a save is a normal event; the directory fsync is the difference between "the old save survives" and "neither save survives".

### 15.4 Cross-Device Fallback

`os.Rename` fails with `EXDEV` when `tmp` and `target` are on different filesystems. This happens when `--data-dir` points at a different mount, or when `saves/` is itself a symlink. The fallback:

```go
func (e *Engine) renameCrossDevice(src, dst string) error {
    in, err := os.Open(src)
    if err != nil { return err }
    defer in.Close()
    out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
    if err != nil { return err }
    if _, err := io.Copy(out, in); err != nil {
        out.Close(); os.Remove(dst); return err
    }
    if err := out.Sync(); err != nil {
        out.Close(); os.Remove(dst); return err
    }
    out.Close()
    return os.Remove(src)
}
```

Atomicity is **degraded** in this path (a crash mid-copy leaves a partial file), so the engine logs `storage.atomicity.degraded {src, dst, reason:"EXDEV"}` at warn level (E17) and the diagnostics payload reflects it. The `.bak` file is the safety net: a partial write still leaves the previous good copy intact.

### 15.5 Backup Rotation and Trash

- `.bak` is **one** generation, not a history. This is deliberate: the purpose is "recover from a torn write", and a history is a filesystem-shape liability across FAT32 and network shares.
- `.trash-<ts>/` retains deletions for `trash_retention_days` (default 30). Cleanup runs at startup and deletes trash directories older than the retention window. `trash_retention_days: 0` disables cleanup entirely, which is the correct setting for a user who wants to inspect every deletion.
- Revision sidecars (`<slot>.json.<rev>`) are capped at `keep_revisions` (default 8) per slot, oldest evicted first.

### 15.6 Write Lock

A single `sync.RWMutex` guards the whole `DataDir`. Writes take the write lock; reads take the read lock. The lock is held for the duration of an atomic write, not for the whole request. Two writes to *different* slots do **not** need to serialise against each other, but the current implementation serialises them anyway, because:

- the target media include FAT32 and network shares where concurrent renames are not reliably ordered,
- the measured cost of serialising is a few milliseconds on local SSD and well within the save latency budget (FS §17.3),
- a single-writer rule is simpler to reason about and to test.

**FR-SAVE-6** permits per-slot concurrency. The implementation chooses global serialisation as a conservative superset. If a future profiling run shows the global lock is a bottleneck, it can be relaxed to per-slot mutexes without changing the API.

### 15.7 Startup Write Probe

**FR-SAVE-16** is implemented as a five-step sequence against a probe file in `DataDir`:

```
1. MkdirAll(DataDir, 0755)
2. OpenFile(DataDir/.write-probe-<pid>, O_WRONLY|O_CREATE|O_EXCL, 0644)
3. Write "ok"; Sync; Close
4. Rename to .write-probe-<pid>.renamed
5. Remove both
```

Any failure records a specific `data_writable: false` reason:

| Failure | Reason recorded |
|---------|-----------------|
| `MkdirAll` fails with `EACCES` | `permission_denied` |
| `OpenFile` fails with `EROFS` | `read_only_filesystem` |
| `OpenFile` fails with `ENOENT` and the parent is a file | `not_a_directory` |
| `Rename` fails with `EXDEV` | `cross_device` (probe passes; the engine will use the fallback) |
| Any other | `unknown` with the errno name |

The reason is surfaced in `/api/state` via `data_writable: false` and in the diagnostics payload, but **not** in the shell's user-facing banner, which is plain-language (FR-SAVE-18).

---

## 16. Path Confinement and Identifier Validation

**Satisfies:** FR-API-4, FR-SRV-10; FS threat T3.

### 16.1 The Single Rule

> **Paths are built from validated identifiers, never from client-supplied strings.**

A client supplies a `slot`, a config key, or a mod `id`. The server validates it against a regex, then constructs the path itself:

```go
var identifierRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validateIdentifier(s string) error {
    if !identifierRe.MatchString(s) {
        return ErrBadIdentifier
    }
    // Belt and braces: reject even though the regex cannot match these.
    if strings.ContainsAny(s, `/\`) || s == "." || s == ".." {
        return ErrBadIdentifier
    }
    return nil
}

func savePath(dataDir, slot string) (string, error) {
    if err := validateIdentifier(slot); err != nil { return "", err }
    return filepath.Join(dataDir, "saves", slot+".json"), nil
}
```

The regex enforces lowercase ASCII, a leading alphanumeric, and a length cap. It cannot match a path separator, a drive letter, a UNC prefix, a dot segment, or a NUL. The explicit `ContainsAny` and dot checks are redundant against the regex but are kept as a defence against a future regex change.

### 16.2 The Forbidden Set

The following are rejected with `400 bad_identifier` before any filesystem call:

- `/`, `\`
- `..`, `.`
- a drive letter (`C:`)
- a UNC prefix (`\\`)
- an absolute path (leading `/` or `\`)
- a NUL byte
- a Windows device name (`CON`, `NUL`, `COM1`..`COM9`, `LPT1`..`LPT9`, `AUX`, `PRN`), case-insensitively
- a trailing dot or space (Windows strips these, causing a mismatch between the validated name and the created file)

The Windows device-name check is done on the *identifier*, not just on the path, because `COM1` as a slot name would, on Windows, refer to a device rather than a file even when joined under `saves/`.

### 16.3 Config Keys

Config keys are not free-form. The merge-write endpoint (`POST /api/config`) accepts only the top-level keys declared in the config schema, and rejects unknown keys with `400 malformed_body`. This prevents a compromised page from injecting arbitrary structure into `settings.json`. Nested merges are performed key-by-key against the schema's declared properties.

### 16.4 Path Canonicalisation Inside the Engine

Even after identifier validation, the engine re-checks the constructed path:

```go
func (e *Engine) confine(full string) error {
    rel, err := filepath.Rel(e.dataDir, full)
    if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
        return ErrOutOfRoot
    }
    return nil
}
```

This is defence in depth: if a future code path forgets to validate an identifier, the confinement check still catches the escape. `ErrOutOfRoot` is a `500 io_error` to the client (it indicates a server bug, not a client error) and is logged at error level with the offending identifier.

---

## 17. Write Lock and Quota Enforcement

**Satisfies:** FR-API-7, FR-API-9, FR-SRV-25.

### 17.1 Write Lock

The `sync.RWMutex` of §15.6 is the write lock. It is acquired *after* request validation and *before* the first filesystem call, and released when the handler returns. A request that is rejected by validation never acquires the lock, so a flood of malformed requests cannot block legitimate writes.

### 17.2 Single-Writer Election Across Tabs

The server holds one `*string` writer identity (`s.writer`), independent of the storage lock. The first session to call `/api/save` with `claim: true` becomes the writer; subsequent sessions that omit `claim` still succeed (they are served by the same server), but the shell is expected to use `claim` and to treat non-writers as read-only (FR-SHELL-5…8).

The server does **not** refuse a save from a non-writer session. The shell's single-writer election is a UX affordance, not a security boundary; the storage lock serialises writes regardless of which session issues them.

### 17.3 Quotas

Per-session counters, reset on a rolling one-minute window:

| Counter | Default | Enforcement |
|---------|---------|-------------|
| Writes | 100/min | `429 rate_limited` with `Retry-After` |
| Bytes written | 64 MiB/min | `429 rate_limited` with `Retry-After` |

Counters are per-session, not per-server, so a second tab does not consume the first tab's budget. A session that exceeds its budget is not killed; it is throttled. The window is a simple sliding count, not a token bucket, because the goal is to bound disk damage, not to shape traffic.

A `429` response includes `Retry-After: <seconds>` and the shell backs off (E14). The counters are reset on session establishment, so a new tab has a fresh budget — a deliberate choice, because the alternative (per-IP or per-server counters) would let a broken tab starve a working one.

### 17.4 Request Size Ceiling

`max_request_bytes` (default 64 MiB) is enforced at two points: the `Content-Length` check (§12.5) and an `io.LimitReader` on the body. A request that omits `Content-Length` is `411`; a request that lies is truncated at `max_request_bytes+1` and rejected with `413`.

### 17.5 Disk-Space Guard

Before a write, the engine checks free space on the target filesystem where the OS exposes it (`statfs`/`GetDiskFreeSpaceEx`). If free space is below `max_request_bytes × 2` (space for the temp file and the rotation), the write is rejected with `500 io_error` and `detail.reason: "insufficient_space"`, and the shell surfaces E5 ("the disk is full"). This turns a partial write into a clean, pre-flight failure.

On filesystems where free space is not queryable (some network mounts), the check is skipped and the OS error (`ENOSPC`) is surfaced instead (E5).

---

## 18. Revision Tracking and Conflict Detection

**Satisfies:** FR-SAVE-9, FR-SAVE-10; FS §11.5.

### 18.1 The Revision Store

Each data file has a monotonic revision, held in a sidecar (`<slot>.json.<rev>`) and in memory:

```go
type revisionStore struct {
    mu sync.Mutex
    m  map[string]int64   // key: relative path under DataDir
}
```

On startup, the store is rebuilt by scanning `saves/*.json.<rev>` sidecars and taking the maximum per slot. If a sidecar is missing, the revision is treated as `1` and the next write sets it to `2`; this is recorded as `storage.revision.rebuilt` so a support engineer can see that a conflict check may have been weakened for that slot.

### 18.2 Conflict Detection

```go
func (e *Engine) checkRevision(key string, ifRev *int64) error {
    if ifRev == nil {
        return nil   // unconditional overwrite; only after explicit user choice
    }
    cur := e.revisions.get(key)
    if *ifRev != cur {
        return &ConflictError{Current: cur, Modified: e.modified(key)}
    }
    return nil
}
```

A stale `if_revision` returns `409 conflict` with the current revision and modified timestamp (FS Appendix C.3). The shell presents overwrite / save-as-new-slot / cancel, defaulting to a new slot (FR-SAVE-10).

### 18.3 Revision Update

The revision is bumped **after** the atomic write succeeds, and the sidecar is written with the same atomic protocol. If the sidecar write fails, the save is still durable (the `.json` was renamed), and the in-memory revision is bumped anyway; the sidecar is retried on the next write. A missing sidecar therefore causes a false conflict on the next write, which the user resolves by choosing overwrite — an inconvenience, not a data loss.

### 18.4 Why Not Timestamps

FAT32 stores mtimes with 2-second granularity, and network shares frequently have clock skew. A timestamp comparison can miss a conflict that a monotonic counter always catches (FS §11.5). The revision is server-owned and never derived from the filesystem.

---

## 19. Update Application

**Satisfies:** FR-UPD-1…9; FS §12.

### 19.1 Update Paths

| Path | Trigger | Mechanism |
|------|---------|-----------|
| Manual replacement | User extracts a new ZIP over the folder | No launcher involvement. `game/` is replaced; `data/` is never in the archive. |
| Launcher-assisted patch | User clicks "Check for updates" (FR-UPD-2) | Launcher downloads, verifies, extracts to `game.new/`, swaps. |
| In-game check | User clicks "Check for updates" in the shell | Shell calls `GET /api/update/check`; launcher performs the outbound request. |

The assisted patch path is **restart-to-apply**: `POST /api/update/apply` records the
request and the *next* launcher start performs download, verification, extraction and
swap before the server binds (step 3b of §4). Nothing is downloaded or replaced while a
session is being served, so the swap needs no quiesce protocol and no client can observe
a half-swapped folder. The full execution model is Updater spec §7.

### 19.2 `GET /api/update/check`

This endpoint is session-authenticated and does nothing unless the user has explicitly initiated a check. It:

1. reads `game/release.manifest.json`,
2. fetches the configured `patch_base_url` + `/latest.json` (a small document naming the latest release and its manifest URL),
3. compares releases,
4. returns `{"current":"2026.09.1","latest":"2026.10.1","available":true,"notes_url":"…"}`.

It does **not** download the patch. A separate, explicit user action (`POST /api/update/apply`) is required, and that action is rate-limited to one per session.

`POST /api/update/apply` does not perform the update inline. It resolves the target
release, writes a durable pending marker into the machine-local update directory, and
answers `202 Accepted`:

```json
{ "applied": false, "pending": true,
  "target_release": "2026.10.1", "archive_size": 3467509,
  "detail": "The update will be applied the next time the launcher starts." }
```

The marker is the intent. A `kill -9` immediately after the response still leaves the
update pending. `POST /api/update/cancel` clears a request that has not started and
writes the flag a running apply polls. `GET /api/update/progress` reports the recorded
phase, byte counts and outcome; it is not a stream, and the shell MUST NOT poll it on a
timer (Packaging spec §9.4 requirement 1). The record and endpoint shapes are Updater
spec §6 and §11.

### 19.3 Verification

Before extraction:

1. Fetch `release.manifest.json` for the target release.
2. Verify the manifest's own signature if `signatures.gpg` is present (FR-LNCH-5).
3. Verify the downloaded archive's SHA-256 against the manifest's declared hash.
4. Verify the declared `total_size` against the archive's actual extracted size (zip-bomb defence, FR-UPD-5).

Before swapping, into the machine-local staging root (`<sidecar>/update/`), which is
what keeps an interrupted download out of a portable game folder:

1. Extract the archive's `game/…` members to `<staging>/game.new/…` and its
   `launcher/…` members to `<staging>/launcher.new/…`, with those prefixes stripped. The
   published archive mirrors the game folder (Packaging spec §9.1); routing its two
   subtrees to different staging directories is what makes steps 5 and 6 below
   consistent.
2. Walk the staged trees and reject any path that is not under one of them, any symlink, any hardlink, and any Windows device name.
3. Verify that no extracted path is under `data/` (FR-UPD-7). If one is, abort with E30.
4. Merge `launcher.new/launcher.config.json` over the installed `launcher/launcher.config.json`, local values winning, and validate the result (Updater spec §14).
5. Write `update.state` describing the swap.
6. `game → game.old`, `game.new → game`.
7. Replace `launcher/` in place from `launcher.new/`, so the running binary survives its own release.
8. On success, remove `update.state`.

Verification of the archive itself (step 3 above is one of its checks) is §19.3's first
half and is unchanged; the composition of the whole pipeline is Updater spec §9.2.

### 19.4 Crash Safety

`update.state` is written **before** the first directory move and removed after the swap completes. On startup, the launcher checks for it:

- If `game.new/` exists and `game/` does not, complete the swap.
- If `game.old/` exists and `game/` does not, roll back.
- If both `game/` and `game.old/` exist and `update.state` is present, the swap was interrupted after the first move; roll forward if `game.new/` is intact, otherwise roll back.
- If `update.state` is absent, the swap did not start or completed; remove any stray `game.new/`/`game.old/`.

The state machine is exhaustively tested with fault injection at each step (§26.6).

### 19.5 `launcher_min`

If the installed launcher is older than `launcher_min`, the release refuses to run and the launcher surfaces E29: *"This update needs a newer launcher."* with a download link. The current release remains runnable; the launcher never bricks itself.

A `launcher_min` that cannot be read is a different failure and MUST NOT be reported as E29: the launcher is not old, the publisher's manifest is unreadable, and a download link cannot fix it. That case is refused as E37, *"The update manifest was not readable."*, and leaves the current release runnable (Updater spec §16.2, R16.5).

### 19.6 Rollback

`game.old/` is retained until the new release has started successfully twice (FR-UPD-8). "Started successfully" means the launcher reached the `SERVE` state and the shell sent at least one heartbeat. Two successes guards against a release that starts but crashes on the first save.

The count is kept in `<sidecar>/update/started.json` and advanced at the first heartbeat
of a process, never at startup: a release that starts and then dies before the shell
connects must not count. `Recover` deliberately does not delete `game.old/` — it cannot
know the count, which is the counter's job — so the retention window closes on the second
heartbeat rather than never. Updater spec §15 specifies the record and the retire rule.

Rollback is a manual action (`--repair` or the launcher's "Revert to previous version" menu) that swaps `game.old → game` and leaves `data/` untouched. A newer save opened by an older build is refused per the compatibility table (FS §12.3), not migrated backward.

### 19.7 In-Browser Update Progress

Packaging spec §9.4 requires byte-and-percent progress while downloading, confirmation
with the download size, release notes before applying, and a plain statement of the
result. Under restart-to-apply the download happens at startup, so there is no browser
open to receive any of it.

This is **deferred, not satisfied**. What ships instead is the substrate the shell work
needs and nothing more: the size is in the `202` response (§19.2), the phase and byte
counts are recorded in `<sidecar>/update/progress.json`, the outcome and the retained
backup are in `result.json`, and `GET /api/update/progress` exposes them. A shell update
panel can be built against those without a new launcher mechanism. Updater spec §19.1
records the deferral and its cost.

---

## 20. Browser Detection and Launch

**Satisfies:** FR-LNCH-6…8; FS §13.4.

### 20.1 Detection

Candidate browsers are probed in `browser_preference` order. On Windows, detection consults both the canonical install locations and the `App Paths` registry keys (via `golang.org/x/sys/windows/registry`). On macOS, it consults `/Applications` and `~/Applications`. On Linux, it consults `$PATH` and the XDG desktop entries.

Version is read from the executable's metadata on Windows and macOS, and from `<exe> --version` on Linux. A browser whose version cannot be determined is treated as unsupported and is not launched.

### 20.2 Version Check

A browser is a candidate only if its major version is ≥ the minimum in `min_browser_version` (default Chrome/Edge 105, Opera 91, Brave 1.45). Below the minimum, the launcher does **not** launch it (FR-COMPAT-1); it surfaces E1.

### 20.3 Launch

```go
cmd := exec.Command(browserPath, url)
cmd.Stdout = nil
cmd.Stderr = nil
cmd.Start()   // not Run; the browser outlives the launcher
```

The URL is `http://127.0.0.1:<port>/index.html#t=<token>`. The `#t=` fragment is never sent to the server and is erased by the shell on boot.

On Windows, `cmd.SysProcAttr` sets `HideWindow: true` so no console flashes. On macOS, the launcher is built with `LSUIElement` for tray-only builds; a windowed build is also supported.

### 20.4 No-Candidate Behaviour

If no candidate is found:

- Windows: offer to open the Microsoft Store page for Edge.
- macOS: open the Chrome download page in the default browser.
- Linux: print the install command for the detected distro family (`apt install chromium`, `dnf install chromium`, etc.) and offer to open the download page.

The launcher exits 1 after showing the guidance. It never launches an unsupported browser "just to see".

### 20.5 Connection Watchdog

After launching the browser, the launcher arms a 20-second watchdog. If no `/__kobra/heartbeat` arrives within that window, it surfaces E22: *"The game opened but couldn't reach the launcher."* with the log path and a copy-diagnostics action. The watchdog fires once; it does not retry, because a proxy or `--user-data-dir` policy that blocks loopback will not be fixed by retrying.

---

## 21. Diagnostics and Logging

**Satisfies:** FR-SRV-9, FR-BETA-1, FR-BETA-2, FR-LNCH-8; FS §13.5.

### 21.1 Log Format

Structured JSON lines, one object per line, written to `logs/launcher.log`:

```json
{"t":"2026-09-15T09:12:44.123Z","lvl":"info","evt":"server.start","port":8771,"origin":"http://127.0.0.1:8771","release":"2026.09.1"}
{"t":"2026-09-15T09:12:45.004Z","lvl":"info","evt":"session.established","origin":"http://127.0.0.1:8771","sid":"a1b2"}
{"t":"2026-09-15T09:13:02.881Z","lvl":"info","evt":"save.write","slot":"slot1","rev":42,"bytes":91234,"ms":37}
{"t":"2026-09-15T09:14:11.220Z","lvl":"warn","evt":"csrf.reject","origin":"http://evil.example"}
```

Levels: `error`, `warn`, `info`, `debug`. The default is `info`; `--log-level debug` enables `debug`. A `debug` build logs full request lines with the session id but never the CSRF token or the bootstrap token.

### 21.2 Event Catalogue

See Appendix C for the full list. Events are stable identifiers (`server.start`, `save.write`, `csrf.reject`, …) and are relied on by the test suite; renaming one is a breaking change.

### 21.3 Ring Buffer

The last 500 log lines are held in memory in a ring buffer and served by `GET /__kobra/diagnostics`. The buffer is bounded so that a long session does not grow memory without bound.

### 21.4 `GET /__kobra/diagnostics`

Session-authenticated. Returns:

```json
{
  "launcher_version": "1.4.2",
  "release": "2026.09.1",
  "engine_version": "2.4.0",
  "port": 8771,
  "origin": "http://127.0.0.1:8771",
  "origin_history": [ "http://127.0.0.1:8770" ],
  "data_dir_kind": "game",
  "data_writable": true,
  "atomicity_degraded": false,
  "browser": { "name": "chrome", "version": "117.0.5938.132" },
  "mods": [ { "id": "hd-textures", "enabled": true } ],
  "saves": [ { "slot": "slot1", "rev": 42, "modified": "…" } ],
  "log_tail": [ "…", "…" ]
}
```

The payload **must not** contain filesystem paths or the OS username (FR-SRV-9, FR-BETA-2). A test asserts this by injecting a sentinel path into the environment and confirming it does not appear (§26.5).

### 21.5 Privacy

- No telemetry is sent anywhere, ever (FR-BETA-1).
- The diagnostics payload is shown to the user in full before it is copied.
- The `--diagnostics` CLI flag prints the same payload to stdout, so a user can inspect it without a browser.

### 21.6 Log Rotation

`logs/launcher.log` is rotated when it exceeds `log_max_bytes` (default 1 MiB). Five generations are kept (`launcher.log.1` … `launcher.log.5`). Rotation is in-process (the file is closed, renamed, and reopened) and is triggered on write when the size threshold is crossed. On POSIX, `SIGHUP` forces rotation.

---

## 22. Error Taxonomy and Propagation

**Satisfies:** FS §16 in full.

### 22.1 Internal Error Types

```go
type KobraError struct {
    Code    string          // the API error enum: "conflict", "bad_identifier", …
    HTTP    int             // the HTTP status
    Msg     string          // user-presentable, no paths
    Detail  map[string]any  // structured, no paths
    Cause   error           // internal only; never serialised
}
```

Handlers return `*KobraError` (or a wrapped error that `errors.As` can extract one from). `writeError` serialises the `Code`, `HTTP`, `Msg`, and `Detail`, and logs the `Cause` at the appropriate level.

### 22.2 Mapping Table

| Internal error | `Code` | HTTP | Log level |
|----------------|--------|------|-----------|
| `ErrBadIdentifier` | `bad_identifier` | 400 | info |
| `ErrMalformedBody` | `malformed_body` | 400 | info |
| `ErrNoSession` | `no_session` | 401 | info |
| `ErrBadOrigin` | `bad_origin` | 403 | **warn** |
| `ErrBadCSRF` | `bad_csrf` | 403 | **warn** |
| `ErrNotFound` | `not_found` | 404 | info |
| `ErrConflict` | `conflict` | 409 | info |
| `ErrTooLarge` | `too_large` | 413 | info |
| `ErrBadContentType` | `bad_content_type` | 415 | info |
| `ErrRateLimited` | `rate_limited` | 429 | warn |
| `ErrIO` | `io_error` | 500 | error |
| `ErrReadOnly` | `read_only` | 503 | warn |

`bad_origin` and `bad_csrf` are logged at **warn** because they are security-relevant: a local page attempting a cross-origin write is exactly the event the threat model (T1, T2) is designed to catch, and a support engineer should be able to see it.

The update subsystem's failures are **not** `Code` values. They are conditions of a local
operation, not of an HTTP request, and the FS §16 E-codes are the vocabulary for them.
The mapping the update endpoint uses when it must answer a request is:

| Condition | FS §16 code | Endpoint result |
|-----------|-------------|-----------------|
| No update available | — | `200`, `{"applied":false,"detail":"already current"}` |
| Channel is not `patch`, or no update server configured | — | `409 conflict`, `detail.reason` = `manual_channel` / `no_update_server` |
| Update server unreachable | — | `500 io_error`, `detail.reason` = `unreachable`; the check itself fails silently per FR-UPD-2 |
| Disk space, download policy, cancellation, drift, download failure | E32–E36 | Recorded in `result.json` and `update.apply.failed`; reported by `GET /api/update/progress`, not by the `apply` response |

Applying an update is a startup operation (Updater spec §7), so most failures never reach
an HTTP response at all. The endpoint that *does* answer, `GET /api/update/progress`,
carries the E-code text in its `message` member and the stable reason in `reason`.

### 22.3 Panic Recovery

A `recover()` in the outermost middleware catches any panic, logs it with a full stack trace to a `crash-<ts>.txt` file in the sidecar log directory, and responds `500 io_error`. In a release build, the launcher then initiates a drain and exits 4 (FR-SRV-24). In a debug build, it re-panics so the developer sees the failure.

### 22.4 The "No Paths in Errors" Rule

No `*KobraError.Msg` or `*KobraError.Detail` may contain:

- an absolute filesystem path,
- the OS username,
- a stack trace,
- the bootstrap token or CSRF token.

This is enforced by a test that walks every error constructor and asserts the message matches a whitelist of patterns (§26.5). It is the single most important error-handling rule in the codebase, because a leaked path in an error response is exactly the kind of information disclosure the loopback threat model is written to prevent.

---

## 23. Concurrency Model

### 23.1 Goroutines

The launcher runs a fixed, small set of goroutines:

| Goroutine | Count | Lifetime |
|-----------|-------|----------|
| `main` | 1 | Process |
| `http.Server.Serve` | 1 | SERVE state |
| Heartbeat watcher | 1 | SERVE state |
| Session reaper | 1 | SERVE state |
| Per-request handler | ≤ `MaxConcurrentRequests` (default 32) | Per request |
| Browser launcher | 1 | Startup |

No goroutine is spawned per connection. `net/http`'s built-in connection handling is bounded by `MaxConcurrentRequests` (a counting semaphore in a middleware). A request that exceeds the bound is rejected with `503` rather than queueing, because a local client that issues more than 32 concurrent requests is misbehaving.

### 23.2 Shared State

| State | Guard |
|-------|-------|
| `lastSeen` | `atomic.Int64` |
| `firstHeartbeat` | `atomic.Bool` (guards the retention hook) |
| `writer` | `atomic.Pointer[string]` |
| `draining` | `atomic.Bool` |
| Session store | `sync.Mutex` |
| Revision store | `sync.Mutex` |
| Write lock | `sync.RWMutex` |
| Quota counters | `sync.Mutex` per session |
| Diagnostics ring | `sync.Mutex` |

No shared state is guarded by a channel; all are mutexes or atomics. This is deliberate: the launcher's concurrency is low-contention and mutexes are easier to reason about under the failure modes the test suite injects.

### 23.2.1 The Apply Lock

The startup apply of §4 step 3b takes an **exclusive OS lock** on
`<sidecar>/update/apply.lock` (`flock` on Unix, `LockFileEx` on Windows) before it reads
the pending marker. It is an OS lock rather than a PID file so that a crashed apply
releases it when the process dies; a second launcher started against the same folder
observes the lock and skips the apply without an error, because the first one is already
doing the work. The lock is released when the apply returns, and the apply runs before
`instance.lock` is acquired, so the two locks are never held at once and cannot be
ordered against each other. Updater spec §17 specifies the behaviour.

### 23.3 Context Propagation

Every request carries a `context.Context` derived from the server's base context. On drain, the base context is cancelled, which propagates to:

- in-flight reads (they abort),
- in-flight writes (they **do not** abort mid-`fsync`; the storage engine checks the context between steps and completes the current atomic write, per FR-SRV-20),
- the update download (it aborts).

The storage engine's `writeAtomic` deliberately does **not** check the context between the `fsync` and the `rename`; cancelling there would leave a temp file with no clear owner. It checks the context before step 1 and after step 6, and if cancelled after step 6, it returns success (the write completed).

### 23.4 Shutdown Ordering

```
1. Set draining = true.
2. Cancel the base context.
3. Wait for in-flight handlers to return, up to drain_timeout.
4. Close the listener.
5. Stop the heartbeat watcher and session reaper.
6. Flush and close the log.
7. Remove instance.lock.
8. Exit 0.
```

Steps 1–3 are the drain (FR-SRV-20). Step 4 happens *after* the drain so that a client that is mid-response is not cut off. Step 7 happens *after* the log is flushed so that the lock's removal is the last durable action.

---

## 24. Memory and Performance Budgets

**Satisfies:** FS §17.

### 24.1 Measured Budgets

| Metric | Target | Budget |
|--------|--------|--------|
| Launcher RSS at idle | < 20 MiB | 40 MiB |
| Launcher RSS during a 64 MiB save | < 90 MiB | 160 MiB |
| Startup → listening | < 150 ms | 400 ms |
| Save write, 1 MiB, local SSD (server-side) | < 40 ms | 90 ms |
| Save write, 1 MiB, USB/SD (server-side) | < 250 ms | 500 ms |
| `GET /api/state` | < 2 ms | 10 ms |
| `GET /api/data/saves` (50 slots) | < 60 ms | 150 ms |
| Static asset throughput, local SSD | ≥ 300 MB/s | 120 MB/s |

The server-side save budgets are tighter than the round-trip budgets in the FS because the launcher cannot control browser scheduling or loopback latency.

### 24.2 Allocation Discipline

- The 64 MiB request ceiling means a single request can allocate 64 MiB for the body plus 64 MiB for the JSON-unmarshalled payload plus 64 MiB for the re-marshalled file. The handler reuses buffers where possible and streams the config read; the save payload is held twice (parsed and serialised) by design, because the checksum is over the canonicalised form.
- Static serving uses `io.Copy` from the file to the response; it does not read files into memory. `Range` responses use `io.CopyN`.
- The diagnostics ring buffer is a fixed 500-entry slice with a write index; no allocations after construction.

### 24.3 Profiling Hooks

A debug build exposes `net/http/pprof` on a **separate** loopback-only listener on an ephemeral port, printed to the log at startup. The pprof listener is never enabled in a release build; the build tag `kobra_debug` is required, and the release Makefile does not set it.

---

## 25. Build, Signing, and Distribution

**Satisfies:** FR-LNCH-2…5; FS §13.3.

### 25.1 Build Matrix

| OS | Arch | Binary | Signing |
|----|------|--------|---------|
| Windows | amd64 | `launcher.exe` | Authenticode, EV certificate |
| Windows | arm64 | `launcher.exe` | Authenticode, EV certificate |
| macOS | amd64 + arm64 | `launcher` (universal) | Developer ID + notarisation + staple |
| Linux | amd64 | `launcher` | None; GPG-detached signature on the tarball |
| Linux | arm64 | `launcher` | None |

The macOS build is a universal binary produced by `lipo` of the two `GOARCH` builds. The Windows builds are cross-compiled from Linux in CI; the macOS builds require a macOS runner for `codesign` and `notarytool`.

### 25.2 Reproducibility

`Makefile`:

```
GOFLAGS = -trimpath -buildvcs=false
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)
```

`VERSION`, `COMMIT`, and `BUILD_TIME` are the only inputs that vary between builds; a reproducible build with the same inputs produces a byte-identical binary. CI verifies this by building twice and comparing hashes.

### 25.3 Signing

**Windows.** Sign `launcher.exe` with `signtool` using an EV certificate. The EV certificate grants immediate SmartScreen reputation (FS §13.3). The README documents the *More info → Run anyway* path for the case where MOTW from the ZIP triggers the dialog even on a signed binary.

**macOS.** Sign with a Developer ID Application certificate, enable the hardened runtime, notarise with `notarytool`, and staple the ticket. Entitlements: `com.apple.security.network.client` (for the loopback listener) and `com.apple.security.network.server` (for accepting loopback connections). No sandbox entitlement; the launcher must read and write the game folder.

**Linux.** No signing authority. Ship a tarball with the executable bit set and a `.desktop` file. Document `chmod +x launcher` for the case where the bit is lost in transit.

### 25.4 Checksums and Signatures

Every release publishes, alongside the binaries:

- `SHA256SUMS` — per-file SHA-256,
- `SHA256SUMS.sig` — a detached GPG signature over `SHA256SUMS`.

The download page links both. The README explains the verification command for each OS. This is the only outbound network action the launcher's documentation encourages, and it is done by the user's tools, not by the launcher.

### 25.5 Distribution Packaging

The launcher ships **inside the game ZIP**, not as a separate download. The ZIP layout is the one in FS §5.1:

```
GameName/
├── launcher/launcher.exe
├── game/…
├── data/.keep
└── README.txt
```

The launcher is not distributed via a package manager, an installer, or an app store. This is what "copy-and-run" means in practice (FR-LNCH-2).

---

## 26. Testing Strategy

**Satisfies:** FS §19.2 in full.

### 26.1 Unit Tests

Every package has unit tests covering its public functions. The heavy-coverage packages are:

- `storage` — atomic write, EXDEV fallback, backup rotation, revision store, quota counters.
- `port` — deny-list parsing, deterministic default (golden hashes), probe interpretation, scan logic.
- `session` — token generation and single-use, session expiry, CSRF constant-time comparison.
- `dataapi` — request decoding, identifier validation, error envelope construction, no-paths-in-errors.
- `update` — manifest verification, extraction safety, swap crash recovery.

### 26.2 Contract Tests

The data API is tested against `data-api.schema.json`: a table-driven suite sends valid and invalid requests and asserts the response validates against the schema. This is run on every commit (FR-SCH-3).

### 26.3 Golden Tests

The deterministic port hash is pinned: `fnv1a32("com.kobra.stardrifter") % 100` is a fixed value, and the test fails if a Go upgrade or a code change alters it. This guards against an implementation change silently moving every existing install to a new origin.

### 26.4 Fault Injection

A `kobra_faultinject` build tag enables injection points:

- **Crash after `fsync`, before `rename`** — asserts the `.bak` path is loadable and the temp file is quarantined.
- **Crash after `rename`, before `fsync(dir)`** — asserts the save is durable on a simulated power loss.
- **`EXDEV` on rename** — asserts the fallback runs and logs degradation.
- **Disk-full on write** — asserts `413`/`500` mapping and that the shell's dirty state is preserved.
- **Panic in a handler** — asserts recovery, log, and exit 4.
- **Signal during drain** — asserts the drain completes and the lock is removed.

Fault injection is disabled in release builds by the build tag.

### 26.5 The "No Paths" Test

A test walks every `*KobraError` constructor, sets a sentinel path and username in the environment, and asserts that neither appears in `Msg` or `Detail`. This is run on every commit. It is the only test that asserts a *negative* property about error content, and it is the one most likely to catch a well-intentioned change that adds a path for debugging.

### 26.6 End-to-End Tests

A test harness drives the full flow:

1. Build the launcher.
2. Create a synthetic game folder with a minimal `index.html`, `shell.js`, and a trivial WASM module.
3. Run the launcher with `--yes --no-open`.
4. Drive the API with an HTTP client that mimics the shell: session exchange, save write, save read, config write, conflict, multi-tab.
5. Assert the origin is stable across a restart.
6. Kill the launcher mid-write and assert recovery.

The E2E harness runs on Linux in CI and on Windows and macOS in a nightly job.

### 26.7 Manual Test Matrix

The FS §19.2 matrix is executed manually before each release. The launcher-relevant cells are: port conflicts on each OS, read-only and `noexec` media, network shares, SMB/NFS latency, and the browser-connection watchdog on a proxy-configured machine.

---

## 27. Open Questions and Deferred Decisions

These are known, deliberate gaps. Each has an owner-visible note in the issue tracker.

### 27.1 Per-Slot vs Global Write Lock

The current implementation serialises all writes behind one `sync.RWMutex` (§15.6). Per-slot mutexes are permitted by FR-SAVE-6 and would improve concurrency for a game that writes multiple slots in parallel. **Deferred** until a profiling run shows the global lock is a bottleneck, because the current cost is within budget and the single-lock model is simpler to test.

### 27.2 Windows Console-Close Drain Window

Windows grants a short, undocumented window between `CTRL_CLOSE_EVENT` and forced termination. The current implementation attempts the drain and, if the OS kills the process first, relies on the `.bak` file for recovery. **Deferred** is a Windows service or a helper process that would extend the window; neither is justified by the current failure rate.

### 27.3 macOS Tray-Only vs Windowed

`LSUIElement` (tray-only) is cleaner for a launcher with no UI, but it removes the menu bar item that some users expect for "Quit". A menu-bar presence with a "Quit" item is the likely resolution. **Deferred** pending user testing in the portability beta.

### 27.4 Sidecar Fallback Conflict Detection

When the sidecar falls back to `<GameFolder>/.kobra/` (§6.3), two machines running the same folder on the same network share will write conflicting `port.json` files. The current mitigation is a warning in the diagnostics pane. **Deferred** is a per-machine subdirectory under `.koba/` keyed by hostname, which trades a clean fallback for a more complex one.

### 27.5 Mod Archive Extraction

`POST /api/mod` with `archive_bytes` is specified but the archive format is not fixed. The likely choice is the same `.tar.zst` used by updates. **Deferred** until the modder beta clarifies what format authors actually produce.

### 27.6 CSP for `SharedArrayBuffer`

`Cross-Origin-Embedder-Policy` is publisher configuration (FR-SRV-16) and is implemented as `server.cross_origin_embedder_policy`. **Resolved:** an engine that needs `SharedArrayBuffer` sets `require-corp`; the shell verifies `crossOriginIsolated` at boot and fails into E2 with a plain-language cause if the publisher did not (FR-SRV-16a).

### 27.7 Update Channel Configuration

`update.channel` exists in the schema but only `manual` is implemented. `patch` is specified in §19 but not yet exercised end-to-end. **Deferred** to Phase 5 of the roadmap (FS §20).

---

## Appendix A. Go Interface Sketches

These are sketches, not final signatures. They exist to fix the shape of each package's API so that the packages can be developed in parallel.

### A.1 `paths`

```go
package paths

type Root struct {
    GameFolder  string
    GameDir     string
    DataDir     string
    LauncherDir string
    SidecarDir  string
    LogDir      string
}

func Resolve(execPath string, dataDirOverride string) (Root, error)
```

### A.2 `sidecar`

```go
package sidecar

type State struct {
    Schema        string        `json:"schema"`
    GameID        string        `json:"game_id"`
    Port          uint16        `json:"port"`
    Origin        string        `json:"origin"`
    OriginHistory []OriginEntry `json:"origin_history"`
    Updated       time.Time     `json:"updated"`
}

func Load(dir string) (*State, error)
func (s *State) Save(dir string) error
func (s *State) RememberOrigin(origin string)   // append + truncate to 5

type Lock struct { /* ... */ }

func AcquireLock(dir, gameID, version string, port uint16) (*Lock, error)
func (l *Lock) Release() error
func (l *Lock) CheckLiveness() (live bool, instance string, err error)
```

### A.3 `port`

```go
package port

type DenyList struct { /* ... */ }

func LoadDenyList(path string) (*DenyList, error)
func (d *DenyList) Contains(p uint16) bool
func (d *DenyList) Reason(p uint16) string

func Default(gameID string, base, span uint16) uint16
func Probe(host string, p uint16) (ProbeResult, error)
func Validate(p uint16, d *DenyList) error
func Scan(base, span uint16, d *DenyList, exclude map[uint16]bool) (uint16, error)
```

### A.4 `session`

```go
package session

type Store struct { /* ... */ }
type Session struct { /* ... */ }

func NewStore() *Store
func (s *Store) NewBootstrapToken() (string, error)
func (s *Store) Exchange(token string) (*Session, error)
func (s *Store) Get(cookieValue string) (*Session, bool)
func (s *Store) Reap()   // called by the session reaper goroutine
```

### A.5 `storage`

```go
package storage

type Engine struct { /* ... */ }

func New(dataDir string, cfg config.DataAPIConfig, log *diagnostics.Logger) (*Engine, error)

type SaveWrite struct {
    Slot       string
    IfRevision *int64
    Payload    json.RawMessage
    Meta       *SaveMeta
    SessionID  string
    Claim      bool
}
type SaveWriteResult struct {
    Slot     string
    Revision int64
    Modified time.Time
    Bytes    int64
}

func (e *Engine) WriteSave(ctx context.Context, w SaveWrite) (SaveWriteResult, error)
func (e *Engine) ReadSave(ctx context.Context, slot string) ([]byte, error)
func (e *Engine) ListSaves(ctx context.Context) ([]SaveSummary, error)
func (e *Engine) DeleteSave(ctx context.Context, slot string) error
func (e *Engine) ReadConfig(ctx context.Context) ([]byte, error)
func (e *Engine) MergeConfig(ctx context.Context, m ConfigMerge) (RevisionResult, error)
func (e *Engine) ProbeWrite() error
func (e *Engine) DataWritable() bool
func (e *Engine) DataDirKind() string   // "game" or "override"
```

### A.6 `dataapi`

```go
package dataapi

func Register(mux *http.ServeMux, s *server.Server)

type KobraError struct { /* ... */ }

func writeError(w http.ResponseWriter, err error)
func writeJSON(w http.ResponseWriter, status int, v any)
func decodeJSON(r *http.Request, v any, max int64) error
func validateIdentifier(s string) error
```

### A.7 `update`

```go
package update

type Manifest struct { /* ... */ }

func FetchLatest(baseURL string) (*Manifest, error)
func Verify(archivePath string, m *Manifest) error
func Extract(archivePath, destRoot string, m *Manifest) error
func Swap(gameDir string) error
func Recover(gameDir string) (RecoveryAction, error)   // called at startup
```

### A.8 `browser`

```go
package browser

type Candidate struct {
    Name    string
    Path    string
    Version string
}

func Detect(pref []string, minVersions map[string]string) ([]Candidate, error)
func Launch(path, url string) error
```

---

## Appendix B. Configuration Reference

This appendix duplicates the operational parts of `launcher.config.schema.json` (FS Appendix B) in prose, for implementers who read this document without the schema open.

### B.1 CLI Flags

| Flag | Type | Default | Effect |
|------|------|---------|--------|
| *(none)* | — | — | Interactive: propose port, confirm, start, open browser. |
| `--port <n>` | int | — | Use `n` without prompting. Validated. |
| `--check-port [n]` | int? | — | Report whether `n` (or the default) is free; exit. |
| `--yes` | bool | false | Accept the proposed port without prompting. |
| `--data-dir <path>` | string | — | Place `data/` outside the game folder. Warns once. |
| `--browser <path>` | string | — | Use a specific browser executable. |
| `--no-open` | bool | false | Start the server without opening a browser. |
| `--print-url` | bool | false | Print the origin (with token) to stdout; block on the server. |
| `--diagnostics` | bool | false | Print the diagnostics payload; exit. |
| `--reset-origin-state` | bool | false | Clear the remembered port. Does not touch browser storage. |
| `--repair` | bool | false | Roll back an interrupted update; rebuild `data/` scaffolding. |
| `--log-level <lvl>` | string | `info` | `error\|warn\|info\|debug`. |
| `--max-lifetime <dur>` | duration | off | Testing only. |

### B.2 `launcher.config.json` Fields

The schema is normative; this is a reading guide.

- `game_id` — reverse-DNS, lowercase. Keys the sidecar directory and the deterministic port hash. **Changing it changes the port and the sidecar location.**
- `port.base` / `port.span` — the candidate window. Default `8765`/`100`.
- `port.require_confirmation` — if `false`, the launcher starts without prompting. Only for automation.
- `port.allow_out_of_range` — if `true`, a user-typed port outside the window is accepted (subject to deny-list and privileged checks).
- `port.deny_list_file` — path to the shared deny list.
- `server.bind` — fixed `127.0.0.1`. The schema rejects anything else.
- `server.idle_timeout_seconds` — clamped to [30, 3600].
- `server.heartbeat_interval_seconds` — clamped to [1, 60].
- `server.session_ttl_seconds` — default 8 h.
- `server.bootstrap_token_ttl_seconds` — clamped to [10, 600].
- `server.probe_path` — must match `^/__[a-z]+/(probe|health)$`.
- `server.csrf_required` — fixed `true`.
- `server.drain_timeout_seconds` — default 15.
- `server.serve_mods` — serve enabled mods at `/mods/*` read-only.
- `server.cross_origin_embedder_policy` — `""` (default, no header) | `require-corp` | `credentialless`. Any other value is rejected at load. Non-empty makes the origin cross-origin isolated (FR-SRV-16a).
- `server.entry_path` — the document the launcher opens and `--print-url` prints: `/`, `/index.html`, or `/editor` (FR-LNCH-9). Any other value is rejected at load.
- `server.csp` — override the default strict policy. **Changing this weakens a security control** and requires a spec revision.
- `data_api.max_request_bytes` — default 64 MiB.
- `data_api.writes_per_minute` / `bytes_per_minute` — per-session quotas.
- `data_api.keep_revisions` — default 8.
- `data_api.trash_retention_days` — default 30; `0` keeps indefinitely.
- `browser_preference` — probe order. Firefox is deliberately absent.
- `min_browser_version` — floor per browser.
- `mime_types` — explicit extension → type map.
- `update.channel` — `manual` (implemented) | `patch` (specified, not yet exercised) | `none`.
- `update.patch_base_url` — used only by the launcher-assisted path.
- `update.protect_data_dir` — fixed `true`.
- `diagnostics.expose_filesystem_paths` — fixed `false`.

---

## Appendix C. Log Event Catalogue

Events are stable identifiers. Each line is `{"t":<iso>,"lvl":<level>,"evt":<name>,…fields}`.

### C.1 Startup and Lifecycle

| Event | Level | Fields |
|-------|-------|--------|
| `launcher.start` | info | `version`, `commit`, `os`, `arch` |
| `launcher.folder.resolved` | info | `game_folder_kind` (canonical/nested) |
| `config.loaded` | info | `game_id`, `release` |
| `config.invalid` | error | `reason` |
| `sidecar.dir` | info | `kind` (native/fallback) |
| `sidecar.fallback` | warn | `reason` |
| `instance.lock.acquired` | info | `pid` |
| `instance.lock.reclaimed` | warn | `stale_pid`, `reason` |
| `instance.handoff` | info | `existing_port` |
| `server.start` | info | `port`, `origin`, `release` |
| `server.stop` | info | `reason` (idle/signal/drain) |
| `server.drain.begin` | info | `in_flight` |
| `server.drain.complete` | info | `ms` |
| `server.panic` | error | `stack_file` |

### C.2 Port

| Event | Level | Fields |
|-------|-------|--------|
| `port.candidate` | info | `port`, `source` (saved/default/scan/user) |
| `port.probe` | debug | `port`, `result` (free/ours/other) |
| `port.confirmed` | info | `port`, `user_edited` |
| `port.bind.failed` | warn | `port`, `errno` |
| `port.scan` | info | `from`, `to`, `found` |
| `port.deny.mismatch` | warn | `port` |
| `port.change` | info | `old`, `new` |

### C.3 Session and Auth

| Event | Level | Fields |
|-------|-------|--------|
| `session.bootstrap.issued` | info | — |
| `session.established` | info | `sid`, `origin` |
| `session.expired` | info | `sid`, `reason` |
| `session.bootstrap.rejected` | warn | `origin`, `reason` |
| `csrf.reject` | warn | `origin` |
| `origin.reject` | warn | `origin` |
| `host.reject` | warn | `host` |
| `quota.exceeded` | warn | `sid`, `kind` (writes/bytes) |

### C.4 Storage

| Event | Level | Fields |
|-------|-------|--------|
| `storage.write.probe` | info | `writable`, `reason` |
| `save.write` | info | `slot`, `rev`, `bytes`, `ms` |
| `save.write.failed` | error | `slot`, `errno` |
| `save.recover` | warn | `slot`, `from` (bak/quarantine) |
| `save.corrupt` | error | `slot`, `reason` |
| `storage.atomicity.degraded` | warn | `reason` (EXDEV) |
| `storage.revision.rebuilt` | warn | `slot` |
| `storage.tmp.recovered` | info | `path` |
| `storage.trash.cleanup` | info | `removed` |
| `config.write` | info | `rev`, `keys` |
| `mod.action` | info | `id`, `action` |

### C.5 Update

| Event | Level | Fields |
|-------|-------|--------|
| `update.check` | info | `current`, `latest`, `available` |
| `update.verify.ok` | info | `release` |
| `update.verify.fail` | error | `release`, `reason` |
| `update.extract` | info | `release`, `files` |
| `update.swap.begin` | info | `release` |
| `update.swap.complete` | info | `release` |
| `update.swap.rollback` | warn | `release`, `reason` |
| `update.data.rejected` | error | `release` |
| `update.pending.recorded` | info | `from`, `target`, `bytes` |
| `update.pending.cleared` | info | `reason` |
| `update.apply.begin` | info | `from`, `target` |
| `update.apply.done` | info | `target`, `installed` |
| `update.apply.skipped` | info | `reason` |
| `update.apply.failed` | warn | `reason`, `retryable` |
| `update.manifest.drift` | warn | `field` |
| `update.download.begin` | info | `bytes` |
| `update.download.done` | info | `bytes` |
| `update.download.progress` | debug | `bytes`, `total` |
| `update.download.resume` | info | `from_bytes` |
| `update.download.restart` | warn | `reason` |
| `update.download.failed` | warn | `reason` |
| `update.disk.insufficient` | warn | `required`, `available` |
| `update.disk.unknown` | debug | `reason` |
| `update.extract.begin` | info | `release` |
| `update.extract.done` | info | `release` |
| `update.config.merge` | info | `result`, `from_local`, `from_release` |
| `update.backup.retained` | info | `release`, `count` |
| `update.backup.retired` | info | `release` |
| `update.cancel.requested` | info | `pending` |
| `update.cancel.honoured` | info | `phase` |
| `update.result.write_failed` | warn | `reason` |
| `update.recovery` | info | `action` |

`update.download.progress` is emitted at `debug` only. A multi-gigabyte download at one
event per tick would be a log-size defect, not observability (Updater spec R16.5). No
event carries a URL, an absolute path, or an archive hash (§22.4, Updater spec R11.9).

The updater's own event table, with the full field semantics, is Updater spec §16.4.

### C.6 Diagnostics

| Event | Level | Fields |
|-------|-------|--------|
| `diag.export` | info | `bytes` |
| `asset.hash.mismatch` | warn | `path`, `expected`, `actual` |
| `browser.detected` | info | `name`, `version` |
| `browser.launch` | info | `name` |
| `browser.watchdog.timeout` | warn | — |

---

## Appendix D. Requirement Coverage Map

Every `FR-*` family in the Functional Specification v2.1, mapped to the section(s) of this document that implement it.

| Family | Sections |
|--------|----------|
| FR-PORT-1…4 | §8, §9 |
| FR-SRV-1…3 | §10.1 |
| FR-SRV-4…6a | §12.1, §12.2, §11.4 |
| FR-SRV-7…9 | §11, §21.4 |
| FR-SRV-10…17 | §13 |
| FR-SRV-18…20 | §10.5, §10.6, §23.4 |
| FR-SRV-21…24 | §2.4, §2.2, §22.3 |
| FR-SRV-25 | §9, §17.2 |
| FR-API-1…13 | §14, §16, §17, §18 |
| FR-AST-1…15 | §13 (serving), §20 (browser), §21 (diagnostics) |
| FR-SHELL-1…8 | §11, §14.5, §17.2 |
| FR-SAVE-1…20 | §15, §16, §17, §18 |
| FR-UPD-1…9 | §19 |
| FR-LNCH-1…8 | §2, §4, §5, §20, §25 |
| FR-COMPAT-1…5 | §20.2, §12.5 |
| FR-SCH-1…6 | §7.1, §26.2 |
| FR-A11Y-1…6 | §8.5, Appendix B |
| FR-I18N-1…7 | §16, §21.1 |
| FR-BETA-1…2 | §21.5, §21.4 |

---

*End of Launcher Technical Specification.*