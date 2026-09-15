# Functional Specification: Portable Game Folder Architecture with a Native Launcher

**Document Version:** 2.2
**Supersedes:** 2.1 (September 2026), 2.0 (September 2026), 1.0 (September 2026)
**Date:** September 2026
**Status:** Implementation-Ready
**Classification:** Technical Architecture Specification

> **Companion documents.** This specification states *what* the product must do. How
> the launcher is built is specified in `Launcher-spec.md`; how a game becomes a
> distributable package in `Packaging-spec.md`; and how an installed folder advances
> from one release to the next in `Updater-spec.md`. Where a companion is normative for
> a subject, this document says so rather than duplicating it.

> **Revision note (v2.2).** Two self-contradictions in §7 are resolved, both found by building the `testgame/` fixture against the launcher rather than by review.
>
> 1. **CSRF now has a stated scope.** FR-SRV-6a required a double-submit token of *every* write, while FR-SRV-8 accepted a bearer session and §7.3 recorded that the shell may hold one. The two could not both hold: the check requires a CSRF **cookie**, so a bearer client — which has no cookie jar by construction — could read but never write. FR-SRV-6a now applies the double-submit rule to cookie-authenticated requests only, which is what the rule was always for: it defends an *ambient* credential, and a bearer id is not ambient.
> 2. **FR-SRV-19 no longer specifies `sendBeacon`.** `navigator.sendBeacon()` cannot set a request header, so it could never satisfy FR-SRV-6a's requirement that the token be echoed in `X-Kobra-CSRF`. The goodbye was specified as unreachable code. It is now `fetch(..., {keepalive: true})`, which has the same unload semantics and can set the header.
>
> Neither change weakens the threat model in §14: `SameSite=Strict`, exact `Origin` checking and the §7.2 host check are untouched, and the cross-origin write it defends against (T2) is still impossible.
>
> **Revision note (v2.1).** The File System Access API is **removed from the architecture entirely.** The v2.0 design still asked the browser for a `readwrite` handle on a user-selected folder, which cost a permission prompt, an IndexedDB handle store, an async/sync file-I/O bridge, an OPFS fallback, and a Chromium-only support requirement. None of that bought anything: the native launcher already holds the user's file permissions and already reads the game folder to serve assets. Persistence is now a small, authenticated **HTTP data API on the launcher** (Section 8), with atomic writes and revision tracking owned by the Go server (Section 11). Saves live in `data/` inside the game folder, so portability no longer depends on browser state at all. Consequences: no permission prompts, no folder-reselection flows, no non-portable fallback, and the Chromium-only constraint is gone (Section 15).
>
> **Revision note (v2.0).** Resolved the blocking findings of the v1.0 architecture review: the random-port origin model was replaced by a **user-validated, per-game stable port** (Section 6), asset loading moved **entirely to HTTP** (Section 9), and the local server gained a concrete **security and lifecycle contract** (Section 7). Appendix F maps every review finding to its resolution.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Glossary and Normative Language](#2-glossary-and-normative-language)
3. [Architecture Overview](#3-architecture-overview)
4. [User Journey](#4-user-journey)
5. [Folder Structure](#5-folder-structure)
6. [Port Allocation and Origin Stability](#6-port-allocation-and-origin-stability)
7. [Local HTTP Origin: Security and Lifecycle](#7-local-http-origin-security-and-lifecycle)
8. [Save and Config API](#8-save-and-config-api)
9. [Asset Pipeline (HTTP)](#9-asset-pipeline-http)
10. [Game Shell Flows](#10-game-shell-flows)
11. [Save System](#11-save-system)
12. [Update and Migration Strategy](#12-update-and-migration-strategy)
13. [Autoinstaller / Launcher](#13-autoinstaller--launcher)
14. [Security and Threat Model](#14-security-and-threat-model)
15. [Compatibility](#15-compatibility)
16. [Error Handling Matrix](#16-error-handling-matrix)
17. [Performance Targets](#17-performance-targets)
18. [Accessibility and Internationalization](#18-accessibility-and-internationalization)
19. [Rollout and Testing Plan](#19-rollout-and-testing-plan)
20. [Implementation Roadmap](#20-implementation-roadmap)
21. [Summary of Decisions](#21-summary-of-decisions)

**Appendices**

- [A. Sequence Diagrams](#appendix-a-sequence-diagrams)
- [B. Manifest Schemas](#appendix-b-manifest-schemas)
- [C. Data API Reference](#appendix-c-data-api-reference)
- [D. Launcher CLI and Config](#appendix-d-launcher-cli-and-config)
- [E. Requirements Traceability](#appendix-e-requirements-traceability)
- [F. Review Finding Resolution](#appendix-f-review-finding-resolution)

---

## 1. Overview

### 1.1 Purpose

This specification defines the functional requirements for a browser-based game distribution architecture that enables "pay once, own forever" ownership. The architecture, designated the **Native Launcher / Portable Game Folder Model**, lets users store their game on any local storage medium (USB drive, SD card, external HDD) and run it from any capable machine with a modern browser, without a live service dependency.

Two processes cooperate, and the boundary between them is the whole design:

- **The launcher is a native Go binary.** It resolves the game folder, allocates a stable port (Section 6), and runs a loopback HTTP server (Section 7) that serves the game read-only and persists saves and configuration through a small authenticated write API (Section 8). Being native, it has the user's file permissions and needs no browser-mediated permission to touch the disk.
- **The browser is a rendering and input host.** The game shell, engine, assets, save data, and config are all just HTTP resources on that origin. The page holds no filesystem handles, requests no storage permission, and stores no authoritative state in browser storage.

The consequence is that the ownership promise rests on the filesystem — a folder the user can copy, back up, and move — and on nothing else. Not on a browser permission grant, not on IndexedDB surviving eviction, not on an origin's storage quota.

### 1.2 Design Goals

Objectives in strict order of precedence:

1. **Portability over convenience.** The user must be able to move the game folder to any storage medium and run it on any machine with a compatible browser. Because saves live in the folder and no state lives in the browser, moving the folder is sufficient — there is no handle to re-grant and no origin-bound store to reconstruct.
2. **User control over asset transparency.** The user retains full visibility and control over assets (images, audio, data files, configs, saves) and may modify them with ordinary file tools, because they are ordinary files.
3. **Simplified user journey.** The launcher minimizes friction between extracting a ZIP and playing. The target journey contains no permission prompt and no browser-managed onboarding step.
4. **Origin stability.** Each game has a stable, user-approved HTTP origin so that any browser-side state that does exist (renderer caches, engine caches, service-worker-free caches) survives across sessions, and so that games remain isolated from each other (Section 6).

### 1.3 Non-Goals

Explicitly out of scope:

- **Anti-piracy measures.** WASM provides limited opacity for the engine core; asset protection is not a requirement. See Section 14.4 for the honest limits of that claim.
- **Cloud save synchronisation.** Saves live in the game folder. (Network shares are supported as a storage medium; a sync service is not.)
- **Multiplayer or online features.** Single-player, offline-first. The architecture enforces this at the CSP layer (Section 7.4).
- **Browser-side filesystem access.** The File System Access API, OPFS, and IndexedDB are not used for authoritative data. They are not merely optional — they are excluded, so that no requirement can silently depend on them again.
- **Mobile browser support.** The launcher is a desktop binary and the engine is compiled for desktop targets. Mobile is not attempted.
- **Mod sandboxing.** Mods are trusted, user-installed, native-format content. The threat model (Section 14) treats installed mods as trusted data and documents the residual risk.

---

## 2. Glossary and Normative Language

| Term | Definition |
|------|------------|
| **Game folder** | The directory the user extracts from the distribution ZIP. Contains `game/`, `data/`, `launcher/`, and bootstrap files. |
| **Launcher** | The native Go binary that allocates the port, runs the loopback server, and persists game data. |
| **Loopback server** | The launcher's embedded HTTP server, bound to `127.0.0.1` only. |
| **Data API** | The launcher's authenticated HTTP write interface for saves, config, and mod management (Section 8). |
| **Game ID** | Stable reverse-DNS identifier, e.g. `com.kobra.stardrifter`. Keys origin-scoped state and sidecar files. |
| **Origin** | `http://127.0.0.1:PORT` — scheme, host, and port. Browser storage, caches, and permissions are keyed by origin. |
| **Slot** | A named save container, identified by a validated slug (`slot1`, `autosave`), never by a client-supplied path. |
| **Revision** | A server-owned monotonically increasing counter per data file, used for conflict detection (Section 11.5). |
| **Session** | A short-lived authenticated browser session, established by exchanging a bootstrap token (Section 7.3). |
| **Sidecar state** | Machine-local launcher state (chosen port, instance lock, logs) stored outside the game folder. |
| **Release** | One published version of the game, identified by a date-version string, e.g. `2026.09.1`. |

Normative keywords **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are to be interpreted as in RFC 2119. Requirements carry stable identifiers (`FR-<AREA>-<n>`) for traceability (Appendix E).

**Requirement families in use:** `FR-PORT` (port and origin), `FR-SRV` (server security and lifecycle), `FR-API` (data API), `FR-AST` (assets), `FR-SHELL` (shell flows), `FR-SAVE` (save system), `FR-UPD` (updates), `FR-LNCH` (launcher), `FR-COMPAT` (compatibility), `FR-SCH` (schemas), `FR-A11Y` (accessibility), `FR-I18N` (internationalization), `FR-BETA` (beta instrumentation). The `FR-FSA` family from v2.0 is retired; Appendix E.2 maps each retired requirement to its replacement.

---

## 3. Architecture Overview

### 3.1 Layers

| Layer | Component | Technology | Responsibility |
|-------|-----------|------------|----------------|
| **Delivery** | Launcher | Go single binary (Section 13.2) | Platform/browser detection, port allocation, server start, browser launch |
| **Runtime** | Loopback server | Go `net/http` | Serve `game/` read-only; expose the authenticated data API; own atomicity and revisions |
| **Presentation** | Game shell | HTML5 + JavaScript | UI, input, save/config calls, game loop host |
| **Engine** | WASM core | WebAssembly (Rust/C++), in a Worker | Game logic, physics, rendering |
| **Storage** | HTTP over loopback | `fetch` in the page | Assets read from `/assets/*`; saves and config read/written via `/api/*` |
| **Persistence** | Game folder on disk | The host filesystem | `data/` holds saves, config, and mods as ordinary files |

### 3.2 Critical Platform Constraints

These constraints drive the design. Each is stated with its consequence.

| # | Constraint | Consequence |
|---|-----------|-------------|
| C1 | `file://` origins block WASM/JS loads under the Same-Origin Policy. | A local HTTP origin is mandatory (Section 7). |
| C2 | Browser-managed state (caches, quotas, storage) is **origin-bound**, and `http://127.0.0.1:PORT` with a varying port is a varying origin. | The port MUST be stable per game and user-approved (Section 6). |
| C3 | A native process needs no browser permission to read or write files it owns. | Persistence belongs in the launcher, not in a browser API (Section 8). |
| C4 | A local HTTP server is reachable by any local process and by any web page that can reach loopback. | Every write endpoint MUST be authenticated and origin-checked (Section 7). |
| C5 | Loopback HTTP is a secure context in every supported browser. | No TLS is needed; any other hostname (e.g. a LAN IP) is out of scope. |
| C6 | Some TCP ports are blocked as unsafe by browser implementations. | Port allocation must exclude a deny list (Section 6.4). |
| C7 | Media can be mounted read-only or `noexec`. | The launcher MUST detect a failed write probe at startup and degrade explicitly (Section 11.8). |
| C8 | Concurrent writers to a file cannot be arbitrated by the filesystem portably (no reliable locking on FAT32 or SMB). | A single writer MUST be elected in-process and revisions MUST be server-owned (Sections 7 and 11.5). |

### 3.3 Component Diagram

```
                 ┌──────────────────────────────────────────────┐
                 │  Browser — origin 127.0.0.1:<port>           │
                 │                                              │
                 │  Game shell (main thread)                    │
                 │   • splash / engine host                     │
                 │   • fetch() only — no storage APIs           │
                 │                                              │
                 │  Engine worker                               │
                 │   • wasm (opaque engine core)                │
                 │   • config + save buffers transferred in     │
                 └───────┬──────────────────────┬───────────────┘
                         │ GET /game, /assets   │ GET/POST /api/*
                         ▼                      ▼
      ┌──────────────────────────────────────────────────────────┐
      │  Launcher (native Go process, user's own permissions)     │
      │                                                          │
      │  Loopback HTTP server  127.0.0.1:<port>                  │
      │   ├── GET  /            → game/        (read-only)        │
      │   ├── GET  /assets/*    → game/assets/ (read-only)        │
      │   ├── GET  /api/data/*  → data/        (read, session)    │
      │   ├── POST /api/save    → atomic write (session)  ◄── new │
      │   ├── POST /api/config  → atomic write (session)          │
      │   ├── POST /api/mod     → mod install/remove (session)    │
      │   └── GET  /__kobra/*   → probe/health (unauthenticated)  │
      │                                                          │
      │  Storage engine: temp → verify → rename, revisions,      │
      │  write lock, backup rotation                             │
      └──────────┬──────────────────────────────┬────────────────┘
                 │ reads/writes                 │ port, lock, logs
                 ▼                              ▼
      ┌──────────────────────────┐   ┌──────────────────────────┐
      │ <game folder>/           │   │ Sidecar state (machine)  │
      │  game/   (read-only)     │   │  port.json, instance.lock│
      │  data/   (saves,config,  │   │  logs/                   │
      │           mods)          │   └──────────────────────────┘
      └──────────────────────────┘
```

---

## 4. User Journey

| Stage | Actor | Action | System behaviour |
|-------|-------|--------|------------------|
| 1 — Purchase | User | One-time payment on the developer's site or a storefront. | Download link for the game ZIP is issued. |
| 2 — Download | User | Downloads the ZIP. | Target size under 500 MB (Section 17.1). |
| 3 — Extraction | User | Extracts to any location: local drive, USB stick, SD card, network share. | Standard archive extraction; no installer required. |
| 4 — Launcher run | User | Runs `launcher/GameName.exe` (or the platform binary). | Launcher resolves the game folder, verifies it can write `data/`, allocates a port (Section 6), starts the loopback server, and opens the browser. |
| 5 — Play | User | Plays. | The shell loads the engine and assets over HTTP and calls the data API for config and saves. **There is no permission prompt and no folder selection step.** |
| 6 — Save | User | Reaches a save point. | The shell POSTs to `/api/save`; the launcher writes atomically to `data/saves/`. |
| 7 — Subsequent launch | User | Runs the launcher again. | The launcher reuses the saved port, the server starts on the **same origin**, and the shell loads the newest save. No prompt, no picker, no browser-storage dependency. |
| 8 — Move | User | Copies the game folder to a USB stick and runs it on another machine. | Everything travels: engine, assets, saves, config, mods. The launcher probes for a free port and confirms it (Section 6.3), then the game continues from the same saves. |
| 9 — Update | User | Applies a later release (Section 12). | The launcher verifies the release manifest and atomically swaps `game/`. `data/` is never touched. Saves migrate on next load through the migration chain. |

**Design intent.** Stages 4–8 contain no browser permission step, no re-grant after a move, and no dependence on origin-scoped browser storage. The only user decision in the whole journey is confirming the port, and only when the preferred port is unavailable.

---

## 5. Folder Structure

### 5.1 Distribution Layout

```
GameName/
├── launcher/
│   ├── launcher.exe            # Windows launcher
│   ├── launcher                # macOS/Linux launcher binary
│   ├── launcher.config.json     # Game identity, default port, server policy
│   └── VERSION                  # Launcher version (plain text)
├── game/                        # Read-only at runtime; replaced wholesale by updates
│   ├── index.html               # Game shell entry point (served, never file://)
│   ├── shell.js                 # Shell logic: port UX, save calls, worker host
│   ├── engine/
│   │   ├── game.wasm            # WebAssembly engine core
│   │   ├── game.js              # Emscripten/JS glue
│   │   └── engine.manifest.json # Engine compatibility metadata
│   ├── assets/
│   │   ├── textures/  audio/  models/  data/
│   │   └── asset.manifest.json  # Asset index (advisory checksums)
│   ├── locales/                 # i18n string catalogs (Section 18.2)
│   └── release.manifest.json    # Release id, file index, migrations
├── data/                        # Written ONLY by the launcher; travels with the folder
│   ├── saves/
│   │   ├── slot1.json           # Slot data
│   │   ├── slot1.json.bak       # Previous good copy
│   │   ├── slot1.json.<rev>     # Revision sidecar (conflict detection)
│   │   └── index.json           # Slot metadata (advisory, rebuildable)
│   ├── config/
│   │   └── settings.json
│   ├── mods/
│   │   └── <mod-id>/
│   │       ├── mod.manifest.json
│   │       └── assets/…         # Overlay priority 100 (shadows base assets)
│   └── .keep                    # Ensures the folder exists in the ZIP
├── README.txt
└── LICENSES/
```

### 5.2 Why `game/` and `data/` Are Separate

| Property | `game/` | `data/` |
|----------|---------|---------|
| Served over HTTP | Yes, read-only, at `/` and `/assets/*` | Read at `/api/data/*`; written only through the data API |
| Who may write it | Nobody at runtime | The launcher only (the browser never holds a write path) |
| Overwritten by updates | Yes, atomically | Never |
| User-modifiable | Discouraged (breaks checksums; edits are lost on update) | Expected and supported, with ordinary file tools |
| Backed up by user | Optional | Yes — this is what matters |

The separation is enforced by the **launcher**, not by a browser permission: the server maps write requests onto `data/` only, and rejects any request that would resolve outside it (FR-API-4). This is stronger than the v2.0 arrangement, where the browser held a handle capable of writing wherever the user pointed it.

### 5.3 Machine-Local Sidecar State (Not in the Game Folder)

Selected port, instance lock, and logs MUST NOT live in the game folder; otherwise running the same folder from two machines or two paths produces conflicting state, and running from read-only media fails.

| OS | Location |
|----|----------|
| Windows | `%LOCALAPPDATA%\KobraGames\<game-id>\` |
| macOS | `~/Library/Application Support/KobraGames/<game-id>/` |
| Linux | `${XDG_STATE_HOME:-~/.local/state}/kobra-games/<game-id>/` |

Contents: `port.json` (chosen port and origin), `instance.lock` (PID + start time), `logs/launcher.log` (rotated, 5 × 1 MB).

Where the sidecar directory cannot be created (restricted profile, locked-down machine), the launcher MUST fall back to a `.kobra/` directory inside the game folder and MUST say so in the diagnostics pane, because that fallback makes the state travel with the folder and can therefore conflict across machines.

---

## 6. Port Allocation and Origin Stability

### 6.1 Principles

- **FR-PORT-1.** The port for a given game+installation MUST be stable across launches. Consequence: browser-side caches and any engine-side origin state survive, and the user's chosen address does not change under them.
- **FR-PORT-2.** The port MUST be proposed by the launcher and **confirmed or edited by the user** before the server starts, so the user retains control when a conflict occurs.
- **FR-PORT-3.** Different games MUST default to different ports, so that each game owns an isolated origin with isolated browser storage and caches.
- **FR-PORT-4.** The launcher MUST NOT silently start on a different origin than the one the user confirmed. A change of origin MUST be an explicit, informed user action.

### 6.2 Deterministic Default Port

Each game derives a default port from its stable `game_id`, so installs are reproducible and two games rarely collide:

```
base    = 8765
span    = 100                                   # ports 8765–8864
hash    = fnv1a32(game_id)                      # stable across launches and OSes
default = base + (hash % span)
```

`launcher.config.json` MAY override `port.base` and `port.span` (Appendix D). The publisher maintains a reservation table in the release repository so first-party titles are assigned disjoint ranges; the hash is the fallback for third-party builds, not a coordination protocol.

### 6.3 Allocation Flow

```
launcher start
  │
  ├─ read sidecar port.json
  │    ├─ found ──► candidate = saved port   (origin unchanged — preferred path)
  │    └─ absent ─► candidate = deterministic default (first run, or new machine)
  │
  ├─ probe candidate on 127.0.0.1
  │    ├─ free ─────────────► proposal = candidate
  │    ├─ occupied by us ───► SAME GAME ALREADY RUNNING
  │    │                       → focus/reload it, exit 0 (no second server)
  │    └─ occupied by other ► scan base..base+span for free ports
  │                            → proposal = first free; explain the change
  │
  ├─ validate against unsafe-port deny list (6.4) and reserved range (6.5)
  │
  ├─ present proposal to the user:  "http://127.0.0.1:8771  [Change]"
  │    ├─ keep ────► commit
  │    └─ change ──► user edits port; re-validate; if occupied → show owner,
  │                  offer next free port
  │
  ├─ probe write access to data/  ──► on failure, see Section 11.8
  ├─ start server bound to 127.0.0.1:chosen
  ├─ write sidecar port.json  { port, origin, game_id, origin_history }
  └─ open browser at http://127.0.0.1:<chosen>/index.html#t=<token>
```

**Probing** MUST NOT rely on TCP connect alone: an occupied port may be held by an unrelated service, and connecting to it can be noisy. The launcher probes with `GET /__kobra/probe` and expects `200 {"app":"kobra-launcher","game_id":"…","instance":"…"}`. A refused connection means free. Any other response — or a response with a different `game_id` — means occupied by another application.

**Probing correctness note.** Between probe and bind there is a TOCTOU window. The launcher therefore MUST bind the socket first and treat `EADDRINUSE` as authoritative, re-running the proposal step once per conflict rather than trusting the probe.

### 6.4 Unsafe Ports

Browsers block requests to a set of ports (a superset of the Fetch Standard's "bad port" list). Ports in that list MUST be rejected at validation time with an explanatory message, because the failure otherwise appears as an opaque `net::ERR_UNSAFE_PORT` after the browser opens. The list is maintained as a single shared JSON file, `port-deny-list.json` (Appendix B), vendored into the launcher and versioned with it.

### 6.5 Privileged and Reserved Ports

- Ports **< 1024** MUST be rejected with a "reserved for system services" message.
- Ports **1024–49151** are acceptable but outside the game's range; the launcher warns that another application may claim the port later.
- Windows may require a firewall exception dialog for the bound port; because the server binds to `127.0.0.1` only, the launcher SHOULD instruct users to **Deny** any public-network firewall prompt, and a loopback-only bind does not require an inbound rule on any supported OS (Section 7.1).

### 6.6 Consequences of Changing Port

The launcher MUST display, before committing a port change:

> Changing the port changes the game's browser address. Your saves and settings are unaffected — they live in the game folder. The browser will treat this as a new site, so anything the browser had cached for the old address will need to be rebuilt.

- Existing browser storage under the old origin **is not deleted**; it becomes unreachable until the port is restored. The launcher SHOULD offer "Clear browser state for old origins" as an explicit action.
- The sidecar records `origin_history` (last 5 origins) so a support path exists for "I changed the port and lost my browser-stored state."
- Because no authoritative data is origin-bound (goal 1), a port change is now a **cache-invalidation event, not a data-loss event**. This is the practical payoff of removing the File System Access API.

### 6.7 Multiple Games and Multiple Instances

- **Isolation.** Because each game has its own port, each game's origin is distinct: browser caches and quotas never collide. Two games running concurrently is supported and expected.
- **Same game twice.** `instance.lock` (PID + creation time) prevents a second server for the same game folder. A second launch detects the running instance via `/__kobra/health`, opens the existing origin, and exits. If the lock is stale (PID gone, or the recorded process start time differs from the live PID's start time), the lock is reclaimed.
- **Different folders, same game.** If the user extracts the game twice and runs both, the second folder's launcher sees the port occupied by the identical `game_id`. It MUST NOT hijack the first instance. It MUST ask: *"This game is already running from another folder. Open that one, or run this copy on a different port?"* Choosing a different port creates a distinct origin, which now affects only browser caches — both copies still write their own `data/` directories independently.
- **Concurrent save access across copies.** Two copies of the game have two `data/` directories and cannot corrupt each other. Two tabs against the *same* copy are serialised by the server's single-writer lock (Sections 7.5, 11.5).

### 6.8 Crash and Restart Behaviour

Because saves are server-owned and written atomically (Section 11.4), a browser crash, a tab kill, or a lost network-to-loopback connection cannot leave a half-written save: the Go process completes or abandons the write independently of the page. The launcher's idle timeout (FR-SRV-19) shuts the server down after the page goes away, and any in-flight write completes before shutdown (FR-SRV-20).

---

## 7. Local HTTP Origin: Security and Lifecycle

This section resolves v1.0 critical issue 4, and — because v2.1 adds write endpoints — it is now the **primary security boundary of the architecture**.

### 7.1 Bind and Address Policy

- **FR-SRV-1.** The server MUST bind to `127.0.0.1` only. It MUST NOT bind `0.0.0.0`, `::`, or any LAN address. Binding loopback is the primary defence against LAN exposure, and it is why no firewall rule is required on Windows or macOS.
- **FR-SRV-2.** The canonical URL MUST use `127.0.0.1` rather than `localhost`. `localhost` can resolve to `::1` or be remapped by a hosts file or proxy configuration; a literal loopback IP removes that ambiguity.
- **FR-SRV-3.** IPv6 loopback (`[::1]`) SHOULD NOT be bound. Supporting both addresses doubles the origin surface for no user benefit.

### 7.2 Host Header Validation (DNS Rebinding)

Even on loopback, a malicious web page can resolve an attacker-controlled hostname to `127.0.0.1` and issue requests that carry `Host: evil.example`. Because the server now writes files, this is a privilege-escalation vector, not a nuisance.

- **FR-SRV-4.** Requests whose `Host` header is not exactly `127.0.0.1:<port>` MUST be rejected with `421 Misdirected Request` and closed without a body.
- **FR-SRV-5.** Requests with an `Origin` header that is present and different from `http://127.0.0.1:<port>` MUST be rejected with `403`. This check MUST apply to every request, not only to writes.
- **FR-SRV-6.** The server MUST NOT respond to `OPTIONS` with a permissive CORS policy. `Access-Control-Allow-Origin` MUST NOT be sent for cross-origin requests; no wildcard is ever emitted.
- **FR-SRV-6a.** Write endpoints MUST additionally require a **double-submit CSRF token**: a value placed in a non-`HttpOnly` cookie at session establishment and echoed in the `X-Kobra-CSRF` header. A cross-origin page can neither read the cookie nor set the header under these rules, so a request that satisfies `Host`, `Origin`, and the session cookie but lacks the CSRF token MUST be rejected with `403`.
  - **Scope (added in v2.2).** The double-submit check defends an **ambient** credential — the session cookie, which the browser attaches to every same-origin request without the page asking. It therefore applies **only to cookie-authenticated requests**. A request authenticated by `Authorization: Bearer <session-id>` (FR-SRV-8) carries no ambient credential: the caller attached the id explicitly, and a cross-origin page cannot read it, because the session exchange response is not CORS-readable. Such a request MUST be accepted without a CSRF cookie or header. The launcher distinguishes the two cases explicitly (`internal/dataapi/pipeline.go`); before v2.2 it required the cookie unconditionally, which made the documented bearer path usable for reads and impossible for writes.
  - **Consequence for `sendBeacon`.** Because the check requires *both* carriers, a request that cannot set a header can never satisfy it. `navigator.sendBeacon()` is such a request, which is why FR-SRV-19 no longer specifies it.

### 7.3 Bootstrap Token and Authenticated Sessions

Loopback HTTP is reachable by any process on the machine, including a user's browser visiting an unrelated site. Two mechanisms keep the game's privileged endpoints private:

- **FR-SRV-7 (bootstrap token).** On start, the launcher generates a 256-bit random token and opens `http://127.0.0.1:<port>/index.html#t=<token>`. The shell reads it from `location.hash`, immediately exchanges it at `POST /__kobra/session` for an `HttpOnly; SameSite=Strict` session cookie plus a CSRF cookie, and erases the fragment with `history.replaceState()`. The token is single-use and expires after 120 seconds; the session expires after 8 hours or 30 minutes of inactivity.
- **FR-SRV-8 (unauthenticated surface).** Without a session, only `/__kobra/probe`, `/__kobra/health`, and the static read-only `game/` namespace are served. Every state-changing endpoint (`/api/save`, `/api/config`, `/api/mod`, `/__kobra/shutdown`, `/__kobra/log`) requires a valid session **and** — when that session is carried by the cookie — a valid CSRF token (FR-SRV-6a). Read access to `/api/data/*` also requires a session, because save contents are user data that a random local process has no business enumerating.
- **FR-SRV-9 (opaque probe).** `/__kobra/probe` and `/__kobra/health` MUST NOT disclose the filesystem path, the user's OS account name, or the game folder location. They return only `app`, `game_id`, `instance`, `release`, and `port`.

### 7.4 Static Serving Rules

- **FR-SRV-10.** The server MUST normalise and canonicalise every request path, rejecting anything that escapes the served root (`..`, encoded traversal, absolute paths, NUL bytes, Windows device names such as `CON`, `NUL`, `COM1`). Rejection is `404`, not `403`, to avoid confirming path existence. This applies identically to `game/` and `data/` roots.
- **FR-SRV-11.** Symlinks MUST NOT be followed outside the served root. If symlink support is enabled, the resolved real path is re-checked against the root.
- **FR-SRV-12.** Only `GET` and `HEAD` are allowed for static content; everything else is `405`.
- **FR-SRV-13.** MIME types MUST be explicit rather than guessed where it matters: `.wasm` → `application/wasm`, `.js`/`.mjs` → `text/javascript`, `.json` → `application/json`, `.html` → `text/html`, `.ogg` → `audio/ogg`, `.png` → `image/png`, `.webp` → `image/webp`. An unknown extension is served as `application/octet-stream` with `Content-Disposition: attachment` so that unexpected content cannot execute in the game's origin.
- **FR-SRV-14.** The server MUST support HTTP `Range` requests (`206 Partial Content`, `Accept-Ranges: bytes`, single range) for audio and video seeking (Section 17.2), and MUST return `416` for unsatisfiable ranges.
- **FR-SRV-15.** `ETag` and `Last-Modified` MUST be emitted; `If-None-Match`/`If-Modified-Since` MUST be honoured with `304`. `Cache-Control` policy: `no-cache` for `index.html`, `shell.js`, `/api/data/*`, and all `*.manifest.json`; `public, max-age=31536000, immutable` for `engine/*` and content-addressed asset paths (Section 9.4).
- **FR-SRV-16.** Responses MUST carry `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, and `Cross-Origin-Opener-Policy: same-origin`. `Cross-Origin-Embedder-Policy` is set to `require-corp` only when the build needs `SharedArrayBuffer`.
- **FR-SRV-17.** A strict `Content-Security-Policy` MUST be sent for the game origin: `default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'`. This enforces the offline-first non-goal: the game cannot reach the network, and an installed mod cannot exfiltrate data over the network through the page.

### 7.5 Lifecycle

- **FR-SRV-18 (heartbeat).** The shell sends `POST /__kobra/heartbeat` every 5 seconds while the page is visible, and every 30 seconds while hidden. The server records `last_seen`; the cadence is a client responsibility.
- **FR-SRV-19 (idle shutdown).** The server MUST exit when `now - last_seen > idle_timeout` (default 90 s, configurable, minimum 30 s). On `pagehide` the shell sends a goodbye with `fetch('/__kobra/goodbye', {method: 'POST', keepalive: true})` so that closing the tab shuts the server down in about one heartbeat, and the idle timeout is the backstop for crashes and forced kills. **Corrected in v2.2:** this was specified as `navigator.sendBeacon()`, which cannot set the `X-Kobra-CSRF` header and therefore cannot satisfy FR-SRV-6a on any POST — the goodbye always answered `403` and the idle timeout did all the work. `fetch(..., {keepalive: true})` has the same delivery guarantee for an unload and *can* set the header.
- **FR-SRV-20 (drain before shutdown).** On shutdown the server MUST (1) stop accepting new requests, (2) allow any in-flight atomic write to finish or abort cleanly, and (3) remove `instance.lock`. It MUST NOT leave a partially written data file, and it MUST NOT delete a temp file whose write was still in progress without recording a recovery note in the log.
- **FR-SRV-21 (signal handling).** `SIGINT`/`SIGTERM` on POSIX and console-close/`CTRL_CLOSE_EVENT` on Windows MUST trigger the drain-and-shutdown sequence. The server MUST run without a console window where the platform allows it (Windows: `-H windowsgui` subsystem; macOS: `LSUIElement` for a tray-only build).
- **FR-SRV-22 (bounded lifetime).** A `--max-lifetime` option (default: off) exists for automated testing; the shipping default relies on heartbeat and idle timeout only, because a hard cap would kill long play sessions.
- **FR-SRV-23 (port conflict at bind).** If binding fails after user confirmation, the launcher MUST re-enter the allocation flow (Section 6.3) rather than exiting with a stack trace, and MUST report which port failed.
- **FR-SRV-24 (crash containment).** A panic in the server MUST be caught, logged to the sidecar log, and reported in a minimal OS dialog with the log path — never a silent exit.
- **FR-SRV-25 (single writer).** The server MUST hold an exclusive in-process write lock on `data/`. A second *tab* is served normally (the lock is per server, not per connection), but the server MUST expose the current writer identity through `GET /api/state` so the shell can elect a single saving tab (FR-SHELL-5). The lock is released only at shutdown.

### 7.6 Server Non-Goals

The server MUST NOT gain, without a specification revision: TLS, authentication beyond FR-SRV-7, dynamic plugin loading, directory listing, a WebSocket endpoint, or any outbound network client. It MUST NOT serve anything outside `game/` and `data/`. The v1.0 text called it "static only" while also requiring MIME handling and ranges; Section 7 enumerates the actual surface, including the write endpoints, so the claim is falsifiable.

---

## 8. Save and Config API

This section replaces v2.0's File System Access layer. The question that drove the change is simple: *what does FSA buy here?* The answer was nothing — the native launcher already has the user's permissions and already reads the game folder. FSA added a permission prompt, a handle store, an async/sync file-I/O bridge, an OPFS fallback, and a Chromium-only requirement, while providing no boundary the launcher did not already have.

### 8.1 Role of the API

- **FR-API-1.** All authoritative persistence MUST go through the launcher's authenticated data API. The shell MUST NOT use the File System Access API, OPFS, IndexedDB, or `localStorage` for save data, configuration, or mod installation.
- **FR-API-2.** The shell MUST treat the data API as the only source of truth for configuration and saves at boot: it reads config and the newest save before creating the engine worker, then transfers the buffers into the worker.
- **FR-API-3.** Browser storage MAY be used only for non-authoritative conveniences (e.g. a cached UI preference), and losing it MUST NOT lose anything the user cares about.
- **FR-API-4 (path confinement).** The server MUST build every data path from a **validated identifier**, never from a client-supplied path. Slot names, config keys, and mod IDs MUST match `^[a-z0-9][a-z0-9._-]{0,63}$` before any filesystem operation. Requests carrying `/`, `\`, `..`, a drive letter, a UNC prefix, or an absolute path in an identifier MUST be rejected with `400`. This is the primary defence for the write path and MUST be the only way paths are constructed.

### 8.2 Least Privilege, Honestly Stated

- **FR-API-5.** The browser MUST NOT hold any filesystem capability. It cannot name a path that the server has not validated, and it cannot reach outside `data/`.
- **FR-API-6.** `game/` MUST be served read-only and MUST NOT be writable through any API endpoint. Update is a launcher-side operation (Section 12), not an API operation.
- **FR-API-7.** The server MAY enforce per-session write quotas (default: 100 writes/minute and 64 MiB/minute per session) and MUST return `429` with `Retry-After` when exceeded. This bounds the damage a compromised page can do to the user's disk.

In v2.0 this section had to concede that the File System Access API cannot express read-only-root-with-writable-subdirectory, so least privilege was approximated architecturally. With the launcher owning both, least privilege is now literally enforced by the server's routing table.

### 8.3 Endpoint Surface

The complete contract is normative in Appendix C. Summary:

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/api/state` | GET | session | Server identity, release, `save_version`, writer identity, revisions |
| `/api/data/saves` | GET | session | Slot index (id, `save_version`, `modified`, `playtime_seconds`) |
| `/api/data/saves/{slot}` | GET | session | One save, verbatim JSON |
| `/api/save` | POST | session + CSRF | Write or overwrite a slot atomically |
| `/api/save/{slot}` | DELETE | session + CSRF | Delete a slot (moves to `.trash-<ts>` first) |
| `/api/data/config` | GET | session | Config document |
| `/api/config` | POST | session + CSRF | Merge-write the config document atomically |
| `/api/mod` | POST | session + CSRF | Install/enable/disable a mod by validated id |
| `/api/mod/{id}` | DELETE | session + CSRF | Remove a mod (moves to `.trash-<ts>`) |
| `/api/export/{slot}` | GET | session | Download a save as a file, for backups and support |

- **FR-API-8.** Write requests MUST be `POST`/`DELETE` only, `Content-Type: application/json` (or `application/octet-stream` for binary payloads), and MUST be rejected with `415` for anything else.
- **FR-API-9.** Requests MUST be bounded: `Content-Length` is required, and bodies larger than the configured per-request ceiling (default 64 MiB) MUST be rejected with `413` before being read.
- **FR-API-10.** Responses MUST be `application/json` and MUST use the error envelope in Appendix C.4. Errors MUST NOT include absolute filesystem paths.
- **FR-API-11.** Writes MUST report the post-write `revision` and `modified` timestamp so the shell can update its in-memory view without a re-read.
- **FR-API-12.** The API MUST be versioned (`api_version` in `/api/state`); the shell MUST refuse to run against a major version it does not understand, and MUST surface a clear "update the launcher" message (E22).

### 8.4 Why Not Keep FSA as an Option

An optional FSA path was considered and rejected: it would preserve the permission prompt, the handle store, both code paths, and the Chromium-only requirement, while serving only the case of "saves outside the game folder". That case is better served by `--data-dir` (Section 11.8) and by the export endpoint, both of which are a few lines of Go rather than a parallel browser subsystem.

---

## 9. Asset Pipeline (HTTP)

### 9.1 Asset Sources

- **FR-AST-1.** The engine and all base assets MUST be loaded over HTTP from the loopback origin. Assets are never read from the filesystem by the page.
- **FR-AST-2.** The shell MUST instantiate the engine with `WebAssembly.instantiateStreaming(fetch('/engine/game.wasm'))`, falling back to `instantiate(await (await fetch(url)).arrayBuffer())` only if the MIME type is wrong. A wrong MIME type is a build error and MUST also be logged as a diagnostic.
- **FR-AST-3.** Large assets MUST be fetched as streams and consumed incrementally where the format permits, and MUST support `AbortController` cancellation when a level unloads (Section 17.2).

### 9.2 Manifest-Driven Discovery

`assets/asset.manifest.json` remains the single source of truth for asset discovery. The engine MUST NOT hardcode asset paths. Schema: Appendix B.

```json
{
  "schema": "kobra.asset-manifest/1",
  "release": "2026.09.1",
  "generated": "2026-09-13T14:30:00Z",
  "assets": [
    { "path": "textures/terrain.png", "type": "texture",
      "size": 1048576, "hash": "sha256:abc123…", "mutable": true },
    { "path": "audio/music_theme.ogg", "type": "audio",
      "size": 5242880, "hash": "sha256:def456…", "mutable": true }
  ]
}
```

### 9.3 Checksums: Advisory, Not Enforcement

- **FR-AST-4.** The `hash` field is **advisory**. The engine MUST NOT refuse to start, refuse to load an asset, or degrade gameplay because a checksum mismatches.
- **FR-AST-5.** When a hash mismatches, the engine MUST log a structured warning (`asset.hash_mismatch {path, expected, actual, source}`) and continue with the file present on disk. The diagnostics pane shows the count.
- **FR-AST-6.** Hash **verification is mandatory only for the release payload**, performed by the launcher during installation/update (Section 12.2), where the threat is a corrupted download rather than a deliberate user edit. The launcher MUST distinguish "the release is corrupt" (hard failure, re-download) from "the user changed an asset" (informational).
- **FR-AST-7.** A mismatching asset whose `mutable` flag is `false` MUST be reported as an error and the engine MUST fail safe — but the `mutable` flag MUST be used sparingly and MUST be justified in the release notes, because every `mutable: false` asset is a mod that cannot work.
- **FR-AST-8 (mod override).** A mod manifest MAY declare `expected_base_hash: "any"` for assets it deliberately replaces. The engine MUST honour the mod's assertion over the base manifest's, and MUST warn (not fail) when a mod was authored against a different base release.

### 9.4 Cache and Content Addressing

- **FR-AST-9.** Assets MUST be served with stable, manifest-derived cache validators. Where the release process supports it, asset paths SHOULD be content-addressed (`assets/<sha256-prefix>/terrain.png`) or served under an `/assets/<release>/` prefix, so `immutable` caching is safe and updates invalidate by path rather than by header.
- **FR-AST-10.** Small assets MUST be bundled at build time: any asset under 32 KiB SHOULD be concatenated into a packing format declared in the manifest (`"bundle": "data/small.pak"`) to avoid per-file request overhead on high-latency media such as network shares.

### 9.5 Mod Overlay

- **FR-AST-11.** Mods live in `data/mods/` and are served by the launcher at `/api/data/mods/*` under session authentication, or at `/mods/*` read-only when `serve_mods` is enabled in `launcher.config.json`. Either way the launcher, not the browser, decides what is servable.
- **FR-AST-12.** Resolution order for any asset path is: `data/mods/<id>/assets/` (priority 100, enabled mods only) → `game/assets/` (base). A mod MUST NOT be able to shadow `engine/` or `index.html`, and the server MUST enforce this by refusing to serve a mod path outside its declared `assets/` subtree.
- **FR-AST-13.** Mod enable/disable MUST be a manifest operation (`data/config/mods.json`), applied at boot, so that a broken mod can be disabled without a working game: the user edits the file directly, and the shell reloads it via the API on next launch.

### 9.6 Asset Hot-Reload (Development and Modding)

- **FR-AST-14.** In development builds only, the server MAY implement `GET /__kobra/watch` (Server-Sent Events) and emit `asset-changed` events using OS filesystem notifications, filtered to the served roots. A `lastModified` polling fallback is specified for platforms without notification APIs, at a 1000 ms interval and only while a debug overlay is open.
- **FR-AST-15.** Hot-reload MUST be disabled in release builds; the release flag is compiled into `shell.js` by the build, not read from a runtime-configurable file, so it cannot be enabled by editing an asset.

---

## 10. Game Shell Flows

### 10.1 First Launch

1. **Boot.** Read the bootstrap token from the fragment, exchange it for a session and CSRF cookie at `POST /__kobra/session`, erase the fragment.
2. **Splash.** Show the game logo and a progress indicator. There is no folder-picker step and no permission prompt.
3. **State.** Call `GET /api/state`; verify `api_version` compatibility and record `save_version`.
4. **Data.** Call `GET /api/data/config` and `GET /api/data/saves`, then `GET /api/data/saves/{newest}`.
5. **Start.** Create the engine worker, transfer the config and save buffers, begin the game loop. Start the heartbeat (FR-SRV-18).
6. **First run only.** If no config exists, the shell POSTs the default config so the user has a file to edit, and shows a one-time notice: *"Your saves and settings are stored in the `data` folder next to the game. Back up or copy that folder to move your progress."*

**FR-SHELL-1.** The shell MUST NOT request any browser storage permission and MUST NOT call any File System Access API. A build that references those APIs MUST fail CI (FR-COMPAT-5).

### 10.2 Subsequent Launch and Folder Moves

1. Retrieve `GET /api/state` and the save index.
2. No permission check exists, so there is no resume or re-grant step: the data API is available whenever the launcher is running.
3. If the save index is empty or unreadable, the shell starts a new game and offers import via the file input (FR-SAVE-14).
4. If the user moved the game folder to another machine, the launcher may land on a different port (Section 6.3). Nothing else changes: the saves are in the folder, and the shell reads them from the new origin. The user sees a port confirmation, not a data-recovery flow.

### 10.3 Missing, Damaged, or Unreadable Data

The shell MUST detect each of the following and handle it without deleting user data.

- **FR-SHELL-2.** On `404` for a slot, a `corrupt` error code, or a validation failure, the shell MUST follow the recovery chain in Section 11.6; the server preserves the damaged file and any `.bak`.
- **FR-SHELL-3.** On an unreadable `data/` (permissions, read-only media), the launcher MUST report it at startup and the shell MUST run in **In-Memory Session** mode with a persistent banner and a working export path (Section 11.8).
- **FR-SHELL-4.** No recovery flow may delete user data. Damaged files are quarantined, never removed, and the shell MUST say where they went.

The multi-tab requirements below were FR-SHELL-3…6 in v2.0; inserting the recovery requirements above shifted them to FR-SHELL-5…8.

### 10.4 Multi-Tab and Multi-Instance Behaviour

- **FR-SHELL-5.** On boot the shell calls `GET /api/state` and compares `writer` with its own session id. The first session to claim the writer role (via `POST /api/save` with `claim: true`) becomes the **writer**; other tabs become **read-only sessions** and display a banner: *"Another window is playing. This window won't save."*
- **FR-SHELL-6.** Read-only sessions MUST NOT call any write endpoint.
- **FR-SHELL-7.** When the writer's session expires or the tab closes, a read-only session MAY offer **Take over saving**; it MUST re-read the newest save and its `revision` before offering.
- **FR-SHELL-8.** Two tabs remain fully supported for assets, which are read-only over HTTP.

---

## 11. Save System

### 11.1 Location and Portability Guarantee

Saves live in `<game folder>/data/saves/`. This is the mechanism that makes the ownership promise real: the folder travels, and the saves travel with it.

**FR-SAVE-1.** Saves MUST be written only under `data/`. The game MUST NOT store authoritative game state in browser storage in any mode, and no mode may claim portability while using browser storage.

### 11.2 Format

```json
{
  "schema": "kobra.save/1",
  "save_version": 3,
  "game_version": "1.2.0",
  "release": "2026.09.1",
  "created": "2026-09-13T14:30:00Z",
  "modified": "2026-09-13T15:02:11Z",
  "revision": 42,
  "playtime_seconds": 8412,
  "checksum": "sha256:…",
  "payload": { "…": "engine-defined state" }
}
```

- **FR-SAVE-2.** Saves MUST be UTF-8 JSON with a versioned header for debugging and support. **However**, the save body SHOULD be stored as a compressed or binary sidecar when it exceeds 1 MiB, referenced by `payload_ref`, to keep JSON parsing and rewriting cost bounded (Section 17.1).
- **FR-SAVE-3.** The `checksum` field covers the canonicalised payload only, so that header fields such as `modified` and `revision` do not invalidate a body that did not change.
- **FR-SAVE-4.** The engine MUST preserve unknown top-level and payload fields when rewriting a save, so a newer build's save can be loaded and re-saved by an older build without silent data loss. Migration MUST be additive where possible.

### 11.3 Slot Index

`data/saves/index.json` is an **advisory** index (slot id, `game_version`, `modified`, `revision`, `playtime_seconds`, thumbnail path). If it is missing or stale, the server rebuilds it by enumerating `saves/*.json` and reading headers; the API's `/api/data/saves` MUST return a correct answer even when `index.json` is absent. The index MUST NOT be required for correctness — a lost index must never mean lost saves.

### 11.4 Atomic Write Protocol

- **FR-SAVE-5.** Every save write MUST follow this sequence, **executed by the launcher in Go** (Section 8):
  1. Write `saves/<slot>.json.tmp-<pid>-<counter>`.
  2. `fsync` the temp file, then `close` it. This is the durability boundary.
  3. Re-read the temp file and verify length plus the header checksum.
  4. If `saves/<slot>.json` exists, rotate it to `saves/<slot>.json.bak` (removing the previous `.bak` first).
  5. Atomically rename the temp file into place (`os.Rename` on the same filesystem, which is atomic on POSIX and on NTFS).
  6. Update `index.json` and the revision sidecar **last**, after the slot is durable.
- **FR-SAVE-6.** Writes to different slots MAY proceed concurrently. Two writes to the same slot MUST be serialised by a per-slot mutex in the server.
- **FR-SAVE-7.** Temp file names MUST be unique per attempt and MUST be cleaned up on success, on failure, and on shutdown. A temp file older than the current process start time MUST be reported as recovered debris in the log at startup.
- **FR-SAVE-8.** A failed write MUST return a structured error (Appendix C.4) and MUST NOT clear the shell's dirty state. The shell MUST surface the failure to the user (Section 16) and MUST retry at most twice with backoff before offering export.

**Cross-platform note.** `os.Rename` is atomic within a filesystem on all target platforms. If `data/` spans a filesystem boundary (unusual, e.g. a symlinked saves directory on a network share), the server MUST detect `EXDEV`, fall back to copy-then-rename with verification, and log that atomicity was degraded.

### 11.5 Concurrency and Conflict Detection

- **FR-SAVE-9.** The server MUST maintain a monotonic `revision` per data file and expose it. A write MAY include `if_revision`; if it does not match the current revision, the server MUST reject with `409 Conflict` and return the current revision and `modified` timestamp. (A write MAY omit `if_revision` to force an overwrite, which the shell MUST use only after an explicit user choice.)
- **FR-SAVE-10.** The shell's conflict flow: on `409`, present *"Another window saved more recently"* with **Overwrite**, **Save to a new slot**, and **Cancel**, defaulting to **Save to a new slot**. The shell MUST NOT auto-overwrite.

Replacing v2.0's timestamp comparison with a server-owned counter matters on the media this architecture targets: FAT32 stores timestamps with 2-second granularity, so a timestamp check can miss a conflict that a revision counter always catches.

### 11.6 Corruption Recovery

On load, in order:

1. Parse `saves/<slot>.json`. Validate `schema`, `save_version`, and `checksum`.
2. **Parse failure or checksum mismatch** → report `corrupt`, then try `saves/<slot>.json.bak`.
3. **`.bak` also bad** → list other slots and offer recovery selection.
4. **No usable save** → start a new game, and quarantine the damaged file as `saves/.trash-<timestamp>/<slot>.json` rather than deleting it, so support can inspect it. A user's damaged save is not garbage.
5. **`.tmp` present at load** (crash during write) → remove it after step 1 succeeds, and log the recovery.

**FR-SAVE-11.** The server MUST report which file it recovered from, and the shell MUST display a non-blocking notice whenever it recovers from `.bak` or quarantines a file, and MUST write the event to the diagnostics log.

### 11.7 Save Migration Chain

- **FR-SAVE-12.** Migrations MUST be declared in `game/release.manifest.json` as an ordered chain (`1→2`, `2→3`, …) with a JS or WASM function name per step. Migration MUST run on a **copy** of the parsed body, and the pre-migration file MUST be preserved as `saves/<slot>.json.prev-<save_version>` before the first successful post-migration write.
- **FR-SAVE-13.** If the save's `save_version` exceeds the engine's, the engine MUST refuse to load it, MUST NOT write to it, and MUST explain that the save comes from a newer version. Refusal MUST NOT be silent, and MUST NOT offer "continue anyway" — silently downgrading a newer save destroys data.
- **FR-SAVE-14 (exports and imports).** `GET /api/export/{slot}` MUST provide a downloadable copy of any save for backup and support, and the shell MUST accept a file import that passes the same validation as a load.
- **FR-SAVE-15.** Migration MUST be transactional per slot: a failure mid-chain leaves the original file untouched.

### 11.8 Read-Only Media and In-Memory Saves

- **FR-SAVE-16 (startup probe).** The launcher MUST verify that `data/` exists or can be created and that a test file can be written, `fsync`ed, renamed, and removed. The result MUST be reported in `/api/state` as `data_writable: true|false`.
- **FR-SAVE-17 (degraded mode).** If the probe fails (locked SD card, `noexec`/read-only mount, network share without write permission), the launcher MUST:
  1. start normally and serve the game read-only,
  2. log the specific failure reason,
  3. report `data_writable: false` to the shell.
- **FR-SAVE-18 (in-memory session).** On `data_writable: false`, the shell MUST enter **In-Memory Session** mode: saves are held in server memory for the session, a persistent non-dismissible banner states that **saves will not be kept after you quit**, and the export endpoint MUST remain available so the user can save their progress to a writable location before quitting.
- **FR-SAVE-19 (escape hatch).** The launcher MUST accept `--data-dir <path>` to place `data/` on a writable filesystem. When used, the launcher MUST warn once that saves now live outside the game folder and will not travel with it. When the game folder itself is writable, `--data-dir` MUST default to `<game folder>/data`.
- **FR-SAVE-20.** In In-Memory Session mode the shell MUST NOT claim portability, MUST NOT write to browser storage as a substitute, and MUST NOT silently retry writes against the read-only folder.

---

## 12. Update and Migration Strategy

### 12.1 Release Identification

- **FR-UPD-1.** Every release MUST carry `game/release.manifest.json` (schema: Appendix B) with `release` (date-version, e.g. `2026.09.1`), `launcher_min`, `engine_version`, `save_version`, a file index with hashes and sizes, and the migration chain (FR-SAVE-12).

### 12.2 Update Paths

| Path | Mechanism | Who it suits |
|------|-----------|--------------|
| **A. Manual replacement** (baseline, always supported) | User downloads the new ZIP (or a `game/`-only patch archive), closes the game, and extracts over the existing folder. `data/` is untouched because releases never contain it. | Users on USB/SD and anyone avoiding in-app update logic. This end-to-end path MUST remain functional and MUST be documented; it is the ownership guarantee in its purest form. |
| **B. Launcher-assisted patch** (optional convenience) | The user's request is recorded; the **next launcher start** downloads `patch-<from>-to-<to>.tar.zst`, verifies it against `release.manifest.json`, extracts to a staging tree outside the game folder, then swaps directories: `game → game.old`, `game.new → game`, and `launcher/` in place. `game.old` is retained per FR-UPD-8. Nothing is downloaded or replaced while a session is being served. | Users who want one click. |
| **C. In-game update check** | Shell calls `GET /api/update/check` **only when the user clicks "Check for updates"**; the launcher performs the outbound check (the page's CSP forbids network access) and reports the result. | Never automatic: offline-first forbids background network use. |

- **FR-UPD-2.** The update check MUST be user-initiated and MUST fail silently when offline.
- **FR-UPD-3.** The launcher MUST refuse to update while an instance of the game is running (detected via `instance.lock` and `/__kobra/health`), and MUST say why.
- **FR-UPD-4.** Directory swaps MUST be crash-safe: the launcher writes `update.state` before touching directories and, on next start, either completes or rolls back based on that file. A half-swapped `game/` MUST be detectable, and the launcher MUST restore `game.old` automatically.
- **FR-UPD-5.** Archive extraction MUST reject absolute paths, `..` components, symlinks, hardlinks, Windows device names, and any entry outside the target root (zip-slip). Total extracted size MUST be checked against the manifest's declared total before extraction begins (zip-bomb defence).
- **FR-UPD-6.** `launcher_min` MUST be enforced: if the installed launcher is older than the release requires, the release MUST refuse to run and instruct the user to replace the launcher.
- **FR-UPD-7.** An update MUST NOT modify, move, or delete anything under `data/`. The launcher MUST verify this after extraction and MUST abort the swap if the archive touched `data/`.

### 12.3 Save Compatibility Rules

| Situation | Required behaviour |
|-----------|--------------------|
| Newer engine, older save (save_version < current) | Run the migration chain; preserve `prev-<n>`; log each step. |
| Newer engine, same save_version | Load directly. |
| Older engine, newer save | Refuse to load; explain; never write. |
| Engine version changed but `save_version` did not | Load, but log a warning — this indicates a migration was forgotten. |
| `schema` field unknown | Refuse; explain that the save was written by a different product line. |

### 12.4 Rollback

- **FR-UPD-8.** Replacing `game/` with the previous release MUST be supported: `game.old` is retained until the new release has started successfully twice, then removed.
- **FR-UPD-9.** Rolling back MUST NOT require rolling back saves. A newer save is preserved untouched and unreadable by the older build (per the table above), so the user can roll forward again without loss.

---

## 13. Autoinstaller / Launcher

### 13.1 Responsibilities

1. Resolve the game folder from its own executable path. This is load-bearing: every other path decision — the served root, the update target, the `data/` location — derives from it.
   - **FR-LNCH-1.** The launcher MUST derive the game folder from its own executable path, never from the working directory, which the invoking shell may set arbitrarily.
2. Read and validate `launcher/launcher.config.json` (schema: Appendix B).
3. Detect the OS and enumerate candidate browsers.
4. Allocate and confirm the port (Section 6).
5. Probe `data/` for write access and start in degraded mode if needed (Section 11.8).
6. Start the loopback server (Section 7), owning the heartbeat lifecycle and the storage engine.
7. Open the chosen browser at the origin with the bootstrap token.
8. Handle instance locking, update application (Section 12), and diagnostics logging.

### 13.2 Implementation Choice

**A single self-contained Go binary.** This is no longer a preference but the consequence of the write API: the launcher is now the component that performs atomic file writes, revision tracking, path validation, and authenticated request handling, and Go's standard library covers all of it (`net/http`, `os`, `encoding/json`, `crypto/rand`, `os/exec`, `embed`).

| Option | Binary size | Runtime deps | Verdict |
|--------|-------------|--------------|---------|
| **Go** | 6–12 MB | None | **Chosen.** Cross-compiles to all target triples in one command, no toolkit to keep patched, and `os.Rename` gives real atomicity without a library. |
| **Rust** | 3–8 MB | None | Acceptable alternative with the same properties; marginally smaller, marginally more build complexity. |
| **Tauri** | 3–10 MB + WebView2 runtime | WebView2 (Windows), WebKitGTK (Linux) | **Rejected.** It pulls a versioned web-engine runtime to display no UI and to serve a web page the user's browser will render anyway. |
| **Shell/batch script** | Minimal | Requires interpreter and third-party tools | Acceptable as a **fallback** only: scripts cannot portably implement atomic writes, request authentication, or a storage engine, so a script path MUST delegate to a bundled binary. |

- **FR-LNCH-2.** The launcher MUST be a single file per platform with no installer, so that the game folder remains copy-and-run.
- **FR-LNCH-3.** The launcher MUST NOT require administrator/root privileges, and MUST NOT write outside the game folder and the sidecar state directory (Section 5.3).
- **FR-LNCH-4.** The launcher MUST NOT depend on any runtime that is not present on a stock install of each supported OS.

### 13.3 Code Signing and OS Trust

| Platform | Requirement | Detail |
|----------|-------------|--------|
| **Windows** | Authenticode signing | Sign `launcher.exe` with an **EV certificate** (or an OV certificate plus a submission to Microsoft) to obtain SmartScreen reputation immediately. Unsigned or newly signed binaries trigger "Windows protected your PC"; the README MUST document the **More info → Run anyway** path, and the launcher MUST NOT attempt to disable or bypass SmartScreen. Ship SHA-256 checksums on the download page. Note that Mark-of-the-Web is inherited from the ZIP and can cause an extra "Open File" dialog even when signed. |
| **macOS** | Sign + notarize + staple | Sign with a **Developer ID Application** certificate, enable the **hardened runtime**, notarize with `notarytool`, and **staple** the ticket. Distribute as a `.app` inside a ZIP or a `.dmg`. The launcher needs no sandbox entitlement (it must read and write the game folder) but does need the network-client entitlement for the loopback listener. Gatekeeper blocks ad-hoc-signed or unstapled builds, and on Apple silicon the binary MUST be arm64 or a universal build. Document the right-click → Open path for the case where notarisation fails. |
| **Linux** | No signing authority | Ship a tarball or an **AppImage**. Ensure the executable bit survives distribution: ZIP extraction does not preserve it reliably on all platforms, so the README MUST include `chmod +x launcher`. Ship a `.desktop` file and an icon. Document that some file managers mount USB media `noexec` or read-only — the launcher will detect this and run in degraded mode (Section 11.8), but copying the folder to a local disk is the better experience. |

**FR-LNCH-5.** The download page MUST publish per-file SHA-256 checksums and, where possible, a detached GPG signature, so users who distrust the transport can verify.

### 13.4 Browser Detection

- Probe in order from `browser_preference` (default `["chrome", "msedge", "brave", "opera", "firefox"]`) using each platform's canonical locations and, on Windows, the `App Paths` registry keys.
- Read the major version from `--version` output or the executable's metadata.
- **FR-LNCH-6.** A browser is a candidate only if its major version is ≥ the minimum in Section 15.1.
- **FR-LNCH-7.** If no candidate is found: on Windows, offer to open the Microsoft Store page for Edge; on macOS/Linux, print the install command for the distro and offer to open the download page in the default browser. The launcher MUST NOT launch a browser known to be unsupported only to have it fail.
- **FR-LNCH-8.** If the user's chosen browser is behind a proxy or a `--user-data-dir` policy that breaks loopback, the launcher MUST detect "browser opened but no heartbeat within 20 s" and surface a diagnostic with the log path.

### 13.5 Diagnostics

`GET /__kobra/diagnostics` (session-authenticated) returns launcher version, release, engine version, port, origin, origin history, `data/` location kind (game folder or `--data-dir`), `data_writable`, browser name/version, mod list, save slot summary, and the last 50 log lines. A "Copy diagnostics" button puts that text on the clipboard for support. The diagnostics payload MUST NOT contain filesystem paths or the OS username (FR-SRV-9).

---

## 14. Security and Threat Model

### 14.1 Assets and Adversaries

| Asset | Adversary | Motivation |
|-------|-----------|------------|
| Game folder contents (`data/`) | Malicious web page reaching loopback | Write to the user's disk outside the game's own data |
| Same | Any local process | Read or manipulate saves |
| Game origin | Remote page / local process | Enumerate installed games, fingerprint versions |
| Publisher's payload integrity | Network attacker, mirror compromise | Deliver a trojaned build |
| User's privacy | Port scanner | Enumerate installed games and versions |

The threat profile changed materially in v2.1: the server now writes files, so loopback exposure is a **write** risk rather than a read-only information disclosure. The defences below are therefore weighted toward authenticating and confining writes.

### 14.2 Threats and Mitigations

| # | Threat | Vector | Mitigation | Residual risk |
|---|--------|--------|-----------|---------------|
| T1 | **DNS rebinding** | Attacker domain resolves to `127.0.0.1`; page issues write requests | Loopback-only bind; strict `Host` allow-list (`421`); `Origin` check on every request (`403`) (FR-SRV-4…6) | Very low |
| T2 | **CSRF / cross-origin writes** | Remote page POSTs to `/api/save` | No permissive CORS; `SameSite=Strict` session cookie; double-submit CSRF token required on writes (FR-SRV-6a, FR-SRV-7) | Low |
| T3 | **Path traversal into the wider filesystem** | `slot` or `id` containing `../`, an absolute path, or a drive letter | Paths built only from validated identifiers, never from client input; canonicalisation re-checked against the data root (FR-API-4, FR-SRV-10) | Very low |
| T4 | **Malicious site probing localhost** | Page scans ports to fingerprint games and versions | `/__kobra/*` unauthenticated responses carry no paths and no user data; no CORS headers means the page cannot read the response (FR-SRV-9, FR-SRV-6) | Low. Port scanning remains possible; responses are opaque. |
| T5 | **Disk exhaustion or write amplification from a compromised page** | Repeated large writes to `/api/save` | Per-session write quotas and volume ceilings; `413` before reading oversized bodies (FR-API-7, FR-API-9) | Low |
| T6 | **Token leakage** | URL shared or logged | Token is in the fragment (not sent in the request line, not in `Referer`), single-use, 120 s TTL, erased via `replaceState`; session cookie is `HttpOnly` (FR-SRV-7) | Low |
| T7 | **Zip-slip / zip-bomb on update** | Malicious or corrupt patch archive | Path validation, symlink rejection, declared-size pre-check (FR-UPD-5) | Low |
| T8 | **Trojaned build** | Compromised mirror | Code signing per platform, published hashes and signatures, release hash verification before extraction (FR-LNCH-5, Section 12.2B) | Low, contingent on certificate custody |
| T9 | **Exfiltration through the game page** | Compromised asset leads to script execution | `connect-src 'self'` and no network allow-list (FR-SRV-17); the data API refuses all requests whose `Origin` is not the game origin, so injected script cannot exfiltrate to a third party even if it runs | Low |
| T10 | **Local malware with full filesystem access** | Any malware the user already has | **Out of scope.** The launcher is native code running with the user's permissions; nothing it does can defend the filesystem from a process the user invited in. Stated explicitly so it is not silently assumed to be covered. | Accepted |
| T11 | **Shared-machine save access** | Other OS users | OS file permissions on the game folder. The launcher MUST NOT weaken inherited ACLs; on POSIX, files are created `0644` under the user's umask, and `data/` MAY be created `0700` on request. | Accepted |
| T12 | **Fingerprinting the launcher** | Site scans for the probe endpoint string | Probe path is compile-time configurable and MUST NOT be referenced in any shipped asset or documentation page. | Low |
| T13 | **Tampered saves** | User or malware edits JSON | Out of scope by design. Saves are user property (goal 2), and the checksum is advisory (FR-AST-6). It MUST NOT be treated as DRM. | Accepted |
| T14 | **Mod escaping its overlay** | Mod manifest declaring `engine/` paths or a traversal path | Overlay confined to the mod's `assets/` subtree; schema and server both reject escapes (FR-AST-12) | Low |

### 14.3 Trust Boundaries

- The **launcher process** is fully trusted: it is the user's own executable, running with the user's file permissions.
- The **loopback server** is semi-trusted: reachable by any local process and by the browser, so it authenticates and CSRF-protects every privileged endpoint, confines all paths to `data/`, and serves no content outside its roots.
- The **game page** is trusted only as far as its origin and session: it can read and write the game's own `data/`, and nothing else. CSP prevents it from reaching the network; the `Origin` check prevents it from leaking data anywhere else.
- **Mods** are trusted data (1.3), constrained by the overlay rules (FR-AST-12) and by CSP.

### 14.4 Honest Limits of WASM Opacity

v1.0 claimed WASM "cannot be meaningfully read, modified, or reverse-engineered without significant effort." That is true only in the weak sense: WASM is not human-readable source. Tooling (`wasm2wat`, decompilers, dynamic instrumentation) makes logic recovery feasible for a determined party, and doing so is legal in many jurisdictions for interoperability. **The opacity is a speed bump, not a protection.** No requirement in this document depends on it: update integrity uses signatures, save integrity is explicitly user-owned, and path confinement is enforced by the server.

---

## 15. Compatibility

### 15.1 Browser Matrix

v2.1 removes the File System Access API requirement, which was the sole reason for the v2.0 Chromium-only stance. The remaining requirements are secure-context `fetch`, streaming WASM compilation, `WebAssembly`, and the engine's rendering backend. **The support matrix below is deliberately kept Chromium-focused** because that is what the project will actually test; Firefox and Safari have no API blocker left, but they are not committed targets and MUST NOT be advertised as supported until the engine's rendering backend is verified on them.

| Browser | Minimum version | Basis | Status |
|---------|----------------|-------|--------|
| Chrome / Chromium | **105** | Streaming WASM + `fetch` streams + modern engine APIs | ✅ Supported (primary target) |
| Edge | **105** | Same engine | ✅ Supported |
| Opera | **91** | Chromium 105 equivalent | ✅ Supported |
| Brave | **1.45** (Chromium 105) | Same engine | ✅ Supported |
| Firefox | — | No API blocker since v2.1; rendering backend unverified | ⚠️ Untested, not advertised |
| Safari | — | No API blocker since v2.1; rendering backend unverified | ⚠️ Untested, not advertised |
| Mobile browsers | — | Desktop launcher required | ❌ Not supported |

- **FR-COMPAT-1.** The launcher MUST block below the minimum version rather than launching and failing inside the game.
- **FR-COMPAT-2.** The shell MUST feature-detect the APIs it needs (`WebAssembly.instantiateStreaming`, `ReadableStream`, the required WebGL/WebGPU context) and MUST show a specific, actionable message rather than a blank screen.
- **FR-COMPAT-3.** The release MUST be tested on the current stable and one version behind for each supported browser (Section 19.2).
- **FR-COMPAT-4.** The launcher MUST NOT offer a "serve on my LAN" option. Sharing must go through the game files themselves, not through the origin.
- **FR-COMPAT-5.** CI MUST fail if any shipped shell or engine-glue source references `showDirectoryPicker`, `showSaveFilePicker`, `createWritable`, `navigator.storage.getDirectory`, or `indexedDB` for authoritative data. This keeps the FSA-free decision from eroding by accident.

### 15.2 Secure Context and Origin

- Loopback is a secure context in every supported browser, so no TLS is needed (C5).
- Any other origin — a LAN IP, a hostname mapped by DNS, a `file://` URL, or a reverse proxy on a different port — MUST NOT be used: it breaks the `Host`/`Origin` checks that protect the write API.

### 15.3 Filesystem Compatibility

| Medium | Constraint | Handling |
|--------|-----------|----------|
| FAT32 USB stick | No symlinks; 4 GiB per-file limit; 2 s timestamp granularity; case-insensitive | Saves are small. Atomic rename works. Revision counters, not timestamps, detect conflicts (FR-SAVE-9). |
| exFAT SD card | Case-insensitive; no POSIX permissions | Same as FAT32. |
| NTFS/APFS/ext4 | Full semantics | No special handling. |
| Network share (SMB/NFS) | High per-file latency, unreliable locking, delayed write-through | Small-file bundling (FR-AST-10) matters most here. Save writes assume ≥ 100 ms latency; atomicity depends on rename, not on locking. A cross-filesystem `data/` triggers the `EXDEV` fallback (Section 11.4). Users SHOULD be warned that network shares are the least reliable medium for saves. |
| Read-only or `noexec` media | Cannot write | Startup probe fails; the launcher reports `data_writable: false` and the shell runs In-Memory Session mode with export (Section 11.8). |

---

## 16. Error Handling Matrix

All recoverable errors MUST be surfaced in-product with a plain-language cause and a next action, and MUST be logged to the diagnostics buffer. Raw exception text MUST NOT be the primary message.

| ID | Error / condition | Detection | User-facing message | Recovery |
|----|-------------------|-----------|---------------------|----------|
| E1 | Unsupported browser | Feature detection at shell boot; version check in launcher | "This game needs Chrome, Edge, or Opera 105 or newer." | Show install/upgrade link; do not launch |
| E2 | Required API missing | Feature detection | "Your browser is missing a feature the game needs (WebAssembly streams)." | Name the missing feature; suggest a supported browser |
| E3 | Launcher and game API mismatch | `api_version` major mismatch | "This game needs a newer launcher." | Link to launcher download; keep current release runnable |
| E4 | Data folder not writable | Startup write probe (FR-SAVE-16) | "The game folder can't be written to — it may be read-only or a network share. Saves will not be kept after you quit." | In-Memory Session + export (Section 11.8); offer `--data-dir` |
| E5 | Save write failed — disk full | `ENOSPC` from the write | "Couldn't save: the disk is full." | Keep dirty state; offer export to another location; retry |
| E6 | Save write failed — permission revoked mid-session | `EACCES`/`EPERM` | "Saving stopped because the game folder became read-only." | Switch to In-Memory Session; offer export |
| E7 | Save write failed — generic I/O | Other write errors | "Couldn't save your progress." | Retry twice with backoff, then offer export; keep dirty state |
| E8 | Save conflict | `409 Conflict` with a newer revision | "Another window saved more recently." | Overwrite / new slot / cancel, defaulting to new slot (FR-SAVE-10) |
| E9 | Save corrupt on load | `corrupt` error code or checksum failure | "Your save was damaged. The last good backup was loaded." | Section 11.6 chain; quarantine, never delete |
| E10 | Save from a newer version | `save_version` > engine | "This save was made by a newer version of the game." | Refuse to load; explain; never write (FR-SAVE-13) |
| E11 | Save missing | `404` on slot | "That save could not be found." | Refresh the slot index; offer another slot |
| E12 | Invalid save slot name | `400` from identifier validation | (Not shown — internal) | Shell MUST validate before sending; a `400` indicates a shell bug and MUST be logged |
| E13 | Request body too large | `413` | "That save is too large to store." | Report the configured ceiling; suggest reducing the save size |
| E14 | Write rate limit hit | `429` + `Retry-After` | "Saving too frequently — the game will retry." | Back off and retry; surface only if it persists |
| E15 | Session expired | `401` on any API call | (Not shown — recoverable) | Re-establish a session via the bootstrap flow; if the token is gone, prompt to relaunch from the launcher |
| E16 | CSRF or Origin mismatch | `403` | "The game lost its connection to the launcher." | Relaunch from the launcher; log the detail (this can indicate an attack and MUST be logged at warn level) |
| E17 | Cross-filesystem rename | `EXDEV` during the atomic write | (Not shown) | Copy-then-rename fallback with verification; log degraded atomicity (Section 11.4) |
| E18 | Port occupied by another app | Probe returns a non-Kobra response, or `EADDRINUSE` | "Port 8771 is used by another program. Use 8772?" | Re-propose next free; user may edit (6.3) |
| E19 | Port occupied by the same game | Probe returns matching `game_id` | "This game is already running." | Focus the existing origin; exit cleanly |
| E20 | Unsafe or privileged port | Deny-list / `< 1024` check | "Port 6000 can't be used by browsers. Pick another." | Auto-advance to the next valid candidate |
| E21 | No compatible browser installed | Browser enumeration + version check | "No supported browser was found. Install Chrome, Edge, or Opera." | Open the vendor download page (FR-LNCH-7) |
| E22 | Browser opened but never connects | No `/__kobra/heartbeat` within 20 s | "The game opened but couldn't reach the launcher." | Show log path, copy-diagnostics, retry, alternate browser |
| E23 | WASM load failure | `instantiateStreaming` rejects; MIME check | "The game engine couldn't start. The download may be incomplete." | Verify `engine.manifest.json` hash; point to re-download; never a blank canvas |
| E24 | Asset missing (404) | Fetch rejects / status 404 | "Some game content is missing (12 files)." | Continue with placeholders; list files in diagnostics |
| E25 | Asset hash mismatch | Manifest comparison (FR-AST-5) | (Informational) "3 files were modified." | Continue; list in diagnostics |
| E26 | Sidecar state unwritable | Failed to create the sidecar directory | (Not shown unless it matters) | Fall back to `.kobra/` in the game folder; mention it in diagnostics (Section 5.3) |
| E27 | Update interrupted | `update.state` found at boot | (Launcher dialog) "The last update didn't finish." | Roll back to `game.old` (FR-UPD-4) or complete the swap |
| E28 | Update archive invalid | Hash or size mismatch before extraction | "The update file is damaged." | Delete partial download; re-download; game remains runnable |
| E29 | Launcher too old for release | `launcher_min` check | "This update needs a newer launcher." | Link to launcher download; keep current release runnable |
| E30 | Update tried to touch `data/` | Post-extraction verification (FR-UPD-7) | "The update was rejected because it would modify your saves." | Abort the swap; keep the current release; report to the publisher |
| E31 | Two tabs, save conflict | Revision mismatch across sessions | "Another window saved more recently." | Conflict prompt (E8); read-only session for the loser |
| E32 | Not enough disk space to install an update | Free-space preflight before download (Updater spec §10) | "There isn't enough free space to install this update." | Keep the current release; keep the update pending; report the bytes required |
| E33 | Update download refused by policy, or its size unknown | Declared size missing, above the ceiling, or a non-secure address (Updater spec §8.6) | "This update cannot be downloaded." | Keep the current release; report to the publisher |
| E34 | Update cancelled by the user | Cancel request before the swap begins (Updater spec §12) | "The update was cancelled." | Keep the current release; leave no partial state; the install is untouched |
| E35 | Update changed after confirmation | Marker/manifest reconciliation at apply time (Updater spec §9.2 step 5) | "The update changed since you confirmed it. Check for updates again." | Keep the current release; discard the recorded intent |
| E36 | Update download failed | Network, HTTP status, or truncation (Updater spec §8, §16.2) | "The update could not be downloaded. The launcher may be offline." | Keep the current release; keep the update pending for a bounded number of retries |
| E37 | Update manifest unreadable | Release manifest fetch, decode, or field validation fails, including an unreadable `launcher_min` (Updater spec §16.2) | "The update manifest was not readable." | Keep the current release runnable; report to the publisher |

---

## 17. Performance Targets

Targets are budgets, not aspirations: a build that misses a budget MUST be treated as a defect, and the numbers are measured on the reference machine defined in 17.4.

### 17.1 Boot, Memory, and I/O

| Metric | Target | Budget (fail above) | Measurement |
|--------|--------|---------------------|-------------|
| Launcher start → server listening | < 150 ms | 400 ms | Wall clock, cold cache |
| Server listening → first paint (splash) | < 400 ms | 1 000 ms | Navigation timing |
| Splash → engine instantiated (warm cache) | < 1 200 ms | 2 500 ms | Custom mark |
| Splash → interactive (warm cache) | < 2 000 ms | 4 000 ms | Custom mark; the headline target, improved from v2.0 by removing the permission step |
| WASM compile (release build) | < 800 ms | 1 500 ms | `instantiateStreaming` duration |
| Engine memory ceiling (baseline play) | < 800 MiB | 1 200 MiB | `performance.measureUserAgentSpecificMemory()` |
| Asset throughput (local SSD, HTTP) | ≥ 300 MB/s | 120 MB/s | 256 MiB read benchmark |
| Asset throughput (USB 3 / SD) | ≥ 60 MB/s | 20 MB/s | Same, on media |
| Small-file count at boot (root requests) | ≤ 150 | 400 | Requires FR-AST-10 bundling |
| Launcher resident memory (idle) | < 20 MiB | 40 MiB | RSS during play |

### 17.2 Streaming, Seeking, and Responsiveness

| Metric | Target |
|--------|--------|
| Audio seek (Range request) round trip | < 50 ms on local SSD |
| Video start-to-first-frame | < 300 ms local; < 1 500 ms USB |
| Level unload → in-flight fetches aborted | ≤ 1 frame (FR-AST-3) |
| Frame-time impact of an autosave | ≤ 1 ms on the game thread (writes are server-side, off-thread) |
| UI interaction (port change, conflict prompt) | < 100 ms feedback, no blocking dialog |

### 17.3 Save and Config Latency

Measured as browser-request to response, and separately as server-side write time.

| Operation | Target (round trip) | Budget | Server-side budget |
|-----------|--------------------|--------|--------------------|
| Save write, 1 MiB slot, local SSD | < 80 ms | 200 ms | 40 ms |
| Save write, 1 MiB slot, USB/SD | < 350 ms | 900 ms | 250 ms |
| Save write, 1 MiB slot, network share | < 1 600 ms | 4 000 ms | 1 400 ms |
| Save load, 1 MiB slot | < 40 ms local | 120 ms | — |
| Config read at boot | < 10 ms | 40 ms | — |
| Slot index (50 slots) | < 100 ms | 300 ms | 60 ms |
| Save migration chain, 3 steps | < 200 ms | 600 ms | — |
| In-memory session write | < 5 ms | 20 ms | — |

### 17.4 Reference Environment

Measurements are taken on: 4-core x86-64 at ≥ 2.5 GHz, 16 GiB RAM, NVMe SSD system drive, Chrome stable with a clean profile, release build, no devtools attached. Deviations MUST be reported with the reference numbers, and every optimization claim MUST name the counter it improves.

---

## 18. Accessibility and Internationalization

### 18.1 Accessibility

**FR-A11Y-1.** All shell UI MUST meet **WCAG 2.2 AA**: 4.5:1 contrast for text, 3:1 for UI components and focus indicators, full keyboard operability, visible focus, and no keyboard traps.

**FR-A11Y-2.** The shell MUST be operable entirely by keyboard: `Enter`/`Space` activate, `Escape` dismisses non-destructive dialogs, focus is moved to the dialog on open and restored on close, and the game canvas exposes a documented pause key.

**FR-A11Y-3.** Every control MUST have an accessible name; status changes (save written, save failed, session degraded) MUST be announced via an ARIA live region (`polite` for success, `assertive` for save failure).

**FR-A11Y-4.** The shell MUST respect `prefers-reduced-motion` (no splash animation) and `prefers-color-scheme`, and MUST scale to 200% browser zoom without loss of function.

**FR-A11Y-5.** Where the launcher renders native UI, it MUST expose a non-visual path: `--port`, `--yes`, and `--data-dir` (Appendix D) so the whole flow can be scripted and screen-reader users are not trapped in a dialog. The port-confirmation prompt MUST be answerable from the keyboard and MUST time out to the proposed default.

**FR-A11Y-6.** A text-only diagnostics mode SHOULD be available, and the shell MUST NOT depend on colour alone to convey any state. In-Memory Session mode MUST be conveyable both visually and to assistive technology.

### 18.2 Internationalization

**FR-I18N-1.** All user-facing strings MUST be externalised into `game/locales/<lang>.json`, loaded at boot; no string literals in shell code.

**FR-I18N-2.** Locale resolution order: user setting in `data/config/settings.json` → `navigator.languages` → `en`. A missing key falls back to English and is logged; it MUST NOT render a raw key.

**FR-I18N-3.** Dates, numbers, and file sizes MUST use `Intl.DateTimeFormat`, `Intl.NumberFormat`, and `Intl.RelativeTimeFormat`. Timestamps in save files MUST be ISO-8601 UTC, never locale-formatted.

**FR-I18N-4.** Layout MUST support RTL and 30% text expansion; UI MUST NOT assume Latin glyph widths.

**FR-I18N-5.** Persisted data MUST be locale-independent: save files and configs MUST use numeric IDs and stable slot slugs, never display names. A user changing language MUST NOT affect saves.

**FR-I18N-6.** Fonts MUST have a fallback chain covering CJK and Cyrillic, and the engine MUST NOT assume a single font provides all glyphs. Text rendering failures MUST degrade to a replacement glyph, not a crash.

**FR-I18N-7.** Filenames in the distribution and in identifiers MUST be ASCII-only, so extraction and path validation on any locale and any filesystem are deterministic.

---

## 19. Rollout and Testing Plan

### 19.1 Rollout Phases

| Phase | Scope | Exit criteria |
|-------|-------|---------------|
| **0 — Internal** | Launcher, server, data API, and shell on one developer machine | End-to-end: extract → launch → play → save → relaunch with no prompt; origin stable across 10 restarts; kill-injection leaves a loadable save |
| **1 — Closed beta, modders** | 10–20 asset authors on mixed hardware | Hot-reload works; a mod overlay shadows a base asset; a mod can be disabled from `mods.json`; no mod can reach `engine/` |
| **2 — Portability beta** | USB 2.0/3.0, SD card, external HDD, network share, read-only media | Move folder across machines and continue from the same saves; read-only medium produces a clear In-Memory Session with a working export |
| **3 — Public beta** | Broad downloads | Error matrix (Section 16) exercised in the wild; support load on the port-confirmation flow measured; telemetry-free crash reports reviewed |
| **4 — Launch** | Signed, notarised, checksummed releases | Signing verified on clean Windows 11 and macOS 14+; SmartScreen clean or documented; binary runs on Ubuntu LTS and Fedora |

### 19.2 Test Matrix

**OS × browser** (all cells: boot, grant-free start, play 10 min, autosave, relaunch, move folder):

| OS | Chrome | Edge | Opera | Brave | Firefox | Safari |
|----|--------|------|-------|-------|---------|--------|
| Windows 10 22H2 | ✔ required | ✔ required | ✔ required | ✔ required | smoke only | n/a |
| Windows 11 23H2+ | ✔ required | ✔ required | ✔ required | ✔ required | smoke only | n/a |
| macOS 13 / 14 / 15 | ✔ required | ✔ required | ✔ required | ✔ required | smoke only | smoke only |
| Ubuntu 24.04 LTS | ✔ required | ✔ required | ✔ required | ✔ required | smoke only | n/a |
| Fedora 41+ | ✔ required | — | — | ✔ required | smoke only | n/a |

"Smoke only" means the shell loads and the engine instantiates; it is not a support commitment (Section 15.1).

**Storage media:** internal NVMe, USB 2.0 FAT32, USB 3.0 exFAT, SD card exFAT, external HDD NTFS, SMB share, NFS share, **read-only mounted medium**, **`noexec` medium**, path containing spaces and non-ASCII characters, path at the filesystem root, deeply nested path (>200 chars on Windows), case-insensitive filesystem, folder on a different filesystem than the sidecar state, folder moved while the game is closed.

**Port and origin scenarios:** saved port free; saved port taken by another app; saved port taken by the same game; user edits the port; user chooses a port from origin history; two games running simultaneously; two copies of the same game; port below 1024; port on the deny list; firewall prompt on Windows; port change followed by a save (verifying no data loss).

**Data API and failure injection:** kill the browser mid-save; kill the launcher mid-save; pull the USB stick mid-save; fill the disk; make `data/` read-only mid-session; corrupt a save; truncate a save; leave a `.tmp` file; leave a `.bak` file; delete `index.json`; request a traversal slot name (`../x`); request an oversized body; exceed the write rate limit; expire the session mid-play; send a write with a stale `if_revision`; send a write with a mismatched `Origin`; send a write without the CSRF token; corrupt the WASM binary; delete an asset.

**Multi-tab:** two tabs same game; writer closes; read-only tab takes over; two tabs save the same slot concurrently; two tabs write config concurrently.

**Update:** manual ZIP replacement; patch archive; interrupted patch; rollback to previous release; newer save opened by older build; migration chain 1→4; an update archive that attempts to write `data/` (must be rejected, E30). The assisted path additionally: records intent and applies at the next start; leaves the install untouched when cancelled; cannot be cancelled once the swap begins; resumes a partial download; restarts one whose server ignored a `Range`; refuses a manifest that drifted after confirmation (E35); refuses an announced or declared size beyond the manifest's (E33); refuses a download that stalls (E36); refuses for want of disk space (E32); preserves the local `update.*` settings across a swap; and keeps `game.old` until the new release has started twice, then retires it. The executable form of this list is the apply phase of `testgame/run.sh` (Updater spec §18).

**Accessibility:** keyboard-only play through the shell; screen reader (NVDA on Windows, VoiceOver on macOS) through first launch and the port prompt; 200% zoom; high-contrast mode; reduced-motion; In-Memory Session announced.

**i18n:** en, de (expansion), ja (CJK), ar (RTL); missing-key fallback; locale change between sessions.

### 19.3 Beta Feedback Instrumentation (Privacy-Preserving)

- **FR-BETA-1.** No automatic telemetry. The beta build ships an explicit **Send diagnostics** action (Section 13.5) that the user reviews and copies; nothing is transmitted automatically, consistent with the CSP and the offline-first goal.
- **FR-BETA-2.** The diagnostics payload MUST be shown in full before copying, MUST contain no filesystem paths or usernames, and MUST be documented in the beta instructions.

---

## 20. Implementation Roadmap

| Phase | Weeks | Deliverables | Exit test |
|-------|-------|--------------|-----------|
| **1 — Launcher, port, server** | 1–3 | Go launcher skeleton, port allocation with user confirmation, deny list, sidecar state, loopback server with Host/Origin/CSP/range/MIME, heartbeat lifecycle, probe/health | Port proposal survives restart; server rejects a bad `Host`; server exits ~90 s after the tab closes; `Range` returns `206` |
| **2 — Data API and storage engine** | 4–6 | Session/CSRF auth, identifier validation, path confinement, atomic write (temp→fsync→verify→rotate→rename), revision tracking, write lock, quotas, `--data-dir`, startup write probe, In-Memory Session | Kill-injection leaves a loadable save or a clean `.bak`; traversal attempts rejected with `400`; read-only medium produces a working degraded session |
| **3 — Shell and assets** | 7–9 | `index.html`, shell, token exchange, WASM load over HTTP, manifest-driven asset loader, config/save bootstrapping, conflict prompt, CI guard against storage APIs | Engine starts from `/engine/game.wasm`; second launch has no prompt; conflict prompt appears on a stale revision |
| **4 — Saves, config, mods** | 10–12 | Save format, backup rotation, quarantine on corruption, migration chain, config merge-write, mod install/enable/disable, export/import, single-writer election | Migration 1→4 works; two tabs cannot corrupt a slot; a disabled mod stops shadowing; export round-trips |
| **5 — Updates and launcher polish** | 13–15 | Release manifest, manual-update path, patch path with rollback, `data/` protection check, signing/notarisation, browser detection, diagnostics | Interrupted patch rolls back; an archive touching `data/` is rejected; clean Windows/macOS install with no surprise (or a documented path) |
| **6 — Accessibility, i18n, hardening** | 16–18 | WCAG 2.2 AA pass, locale catalogs, RTL, reduced motion, performance budgets, threat-model verification | Accessibility audit passes; budgets in Section 17 met; T1–T9, T14 verified by test |
| **7 — Beta and launch** | 19–22 | Modder beta, portability beta, docs (README, troubleshooting, modding guide), signed release | Section 19.2 matrix executed; support runbook for Section 16 exists |

---

## 21. Summary of Decisions

| Decision | v1.0 | v2.1 | Rationale |
|----------|------|------|-----------|
| **Persistence** | FSA handle on the game root | Authenticated HTTP data API on the native launcher | The launcher already holds the user's permissions; FSA added prompt, handle store, async bridge, and an OPFS fallback for no gain |
| **Permission prompt** | Required on first launch and after every move | **Does not exist** | No browser permission is needed to accomplish anything the game does |
| **Saves location** | User-selected folder | `<game folder>/data/saves/` | Portability becomes a property of the filesystem, not of browser state |
| **Save atomicity** | FSA temp/verify/copy dance | Go temp→fsync→verify→rotate→rename | Real atomic rename instead of an approximation |
| **Conflict detection** | `modified` timestamp comparison | Server-owned monotonic revision | FAT32's 2-second timestamp granularity made the timestamp check unreliable |
| **Read-only media** | FSA picker bailed out | Startup probe + In-Memory Session + export + `--data-dir` | Explicit, recoverable failure mode |
| **Port** | Random free port | Deterministic, user-confirmed, persisted per game | Origin stability; games are isolated |
| **Origin** | `localhost:PORT` | `127.0.0.1:PORT`, loopback-only, session + CSRF gated | No proxy ambiguity; DNS-rebinding and CSRF defence on a server that now writes |
| **Assets** | FSA streaming with checksums | HTTP with advisory hashes and content-addressed caching | Removes the async/sync blocker; gains ranges and immutable caching |
| **Writes** | `readwrite` on the game root | Server-confined writes to `data/` only, validated identifiers | Least privilege is now literally enforceable |
| **Checksums** | Implied enforcement | Advisory for assets; mandatory for release payloads | Modding and integrity are different problems |
| **Server lifecycle** | Shut down on tab close | Heartbeat + idle timeout + drain before exit | Observable, reliable, and safe for in-flight writes |
| **Folder preselection** | "Default folder presented" | Not applicable — no picker exists | FSA could not pre-select; the problem is gone |
| **Updates** | Unspecified | Three paths, manifests, migrations, rollback, `data/` protection | "Own forever" needs a maintenance story |
| **Patch apply point** | In-session download and swap | **Restart-to-apply**: the next launch downloads, verifies, extracts and swaps before the server binds | The server is serving `game/` when an inline swap would replace it; restarting is the only moment the folder is not in use, and it makes the swap crash-safe for free. Cost: in-browser progress is deferred (Updater spec §19.1) |
| **Patch signing** | Required for the patch channel | **Recommended, not required**; the launcher still fails closed on a declared signature it cannot verify | No OpenPGP parser ships (FS §13.3, Packaging §8.6), so requiring a signature made the assisted path unusable. The residual risk — no publisher authentication over the patch channel — is published instead of implied |
| **Launcher runtime** | Tauri | Single Go binary | No WebView runtime to serve a web page; real atomic file primitives |
| **Browser minimum** | 86 vs 105 (inconsistent) | 105, matrix kept Chromium-focused | Aligns with tested reality; Firefox/Safari have no API blocker but are unverified |

**Closing statement.** The v1.0 vision is unchanged and remains the point of the document: a game the user genuinely owns — storable on a USB stick, movable between machines, modifiable, and playable offline forever, with an opaque engine and transparent assets. v2.0 gave each claim a mechanism, a failure mode, and an acceptance test. v2.1 removes the largest remaining piece of accidental complexity by asking what each mechanism was actually for: the answer for the File System Access API was "nothing the native launcher did not already provide." Portability is delivered by a game folder that contains its own saves; opacity is delivered by WASM with its limits stated honestly; ownership is delivered by ordinary files the user can copy; and friction is delivered by a journey with no permission prompt in it at all.

---

## Appendix A. Sequence Diagrams

### A.1 First Launch

```
User          Launcher (Go)          Loopback server        Browser/shell
 │               │                        │                     │
 │ run launcher  │                        │                     │
 ├──────────────►│ read sidecar/port      │                     │
 │               │ probe → free           │                     │
 │               │ probe write access to data/                  │
 │  confirm port │ bind 127.0.0.1:8771    │                     │
 │◄─────────────►├───────────────────────►│                     │
 │               │ write port.json        │                     │
 │               │ exec browser #t=tok     │                     │
 │               ├────────────────────────┼────────────────────►│
 │               │                        │  GET /index.html    │
 │               │                        │◄────────────────────┤
 │               │                        │  POST /__kobra/session (token)
 │               │                        │────────────────────►│  set HttpOnly session
 │               │                        │                     │  + CSRF cookie
 │               │                        │                     │  replaceState() (erase #t)
 │               │                        │  GET /engine/game.wasm, /assets/*
 │               │                        │◄────────────────────┤  instantiateStreaming
 │               │                        │  GET /api/state     │
 │               │                        │◄────────────────────┤  api_version, save_version
 │               │                        │  GET /api/data/config, /api/data/saves
 │               │                        │◄────────────────────┤  buffers → engine worker
 │               │                        │  POST /__kobra/heartbeat
 │               │◄───────────────────────┤◄────────────────────┤
 │  plays        │                        │                     │
```

No permission prompt, no directory picker, no handle storage.

### A.2 Subsequent Launch (and after moving the folder)

```
User          Launcher                     Shell
 │ run launcher  │                          │
 ├──────────────►│ port.json → 8771 (free)  │
 │               │ probe data/ writable     │
 │               │ bind, open browser       │
 │               ├─────────────────────────►│
 │               │                          │ session exchange (no user gesture)
 │               │                          │ GET /api/state → data_writable: true
 │               │                          │ GET /api/data/config, /api/data/saves
 │               │                          │ GET /api/data/saves/slot1 → newest
 │               │                          │ start engine → play
 │  plays immediately (target: < 2.0 s warm)│
 │                                          │
 │  ── user copies the whole folder to a USB stick and runs it on another machine ──
 │               │ port.json absent → probe 8771                       
 │               │ occupied → propose 8772, user confirms             
 │               │ probe data/ writable (on the stick)                
 │               │ start, open browser                                
 │  continues from the same saves — nothing to re-grant or re-select  
```

### A.3 Save Writing (server-side atomic protocol)

```
Engine (worker)      Shell                 Launcher (Go)            data/saves/
 │ save(state)        │                        │                       │
 ├───────────────────►│ POST /api/save         │                       │
 │                    │  {slot, if_revision,   │                       │
 │                    │   payload}             │                       │
 │                    ├───────────────────────►│ validate slot id      │
 │                    │                        │ lock slot mutex       │
 │                    │                        │ check if_revision ────►│
 │                    │                        │ write .tmp-<pid>-<n> ─►│
 │                    │                        │ fsync + close         │
 │                    │                        │ re-read + verify ─────►│
 │                    │                        │ rotate .json → .bak ──►│
 │                    │                        │ rename .tmp → .json ──►│
 │                    │                        │ bump revision, index ►│
 │                    │ 200 {revision, modified}│                      │
 │                    │◄───────────────────────┤                       │
 │                    │ update in-memory view  │                       │
 │  save-complete     │                        │                       │
 │◄───────────────────┤                        │                       │
 │                    │ on 409 → conflict prompt (default: new slot)  │
 │                    │ on 5xx/ENOSPC → E5/E7, keep dirty state       │
```

### A.4 Read-Only Media: Degraded Startup

```
Launcher start
  │ probe: create data/.write-test → fsync → rename → remove
  │   ├─ success ──► data_writable: true
  │   └─ failure ──► log reason (EROFS / EACCES / noexec)
  │                  data_writable: false
  ▼
Server serves game read-only; /api/state reports data_writable: false
  ▼
Shell enters IN-MEMORY SESSION
  • persistent banner: "Saves will not be kept after you quit"
  • saves held in server memory for this session
  • GET /api/export/{slot} remains available
  • offers --data-dir instructions for a writable location
```

### A.5 Mod Overlay Boot

```
Shell boot
  │ GET /api/data/config/mods.json   → [ {id:"hd-textures", enabled:true}, … ]
  │ GET /engine/game.wasm  (always base — mods cannot shadow engine/)
  │ for each asset in asset.manifest.json:
  │     resolve order: /mods/<id>/assets/<path>   (priority 100, enabled only)
  │                   /assets/<path>              (base)
  │     first 200 wins; hash mismatch → warn, continue (FR-AST-5/8)
  │ start engine with the resolved URL table
```

---

## Appendix B. Manifest Schemas

Nine standalone JSON Schema (2020-12) files ship alongside this document:

| Schema | File | Purpose |
|--------|------|---------|
| `kobra.engine-manifest/1` | `architecture/schemas/engine.manifest.schema.json` | Engine compatibility and preload guidance |
| `kobra.asset-manifest/1` | `architecture/schemas/asset.manifest.schema.json` | Asset index with advisory hashes |
| `kobra.save/1` | `architecture/schemas/save.schema.json` | Save header and payload envelope |
| `kobra.launcher-config/1` | `architecture/schemas/launcher.config.schema.json` | Launcher configuration, including the data API policy |
| `kobra.mod-manifest/1` | `architecture/schemas/mod.manifest.schema.json` | Mod declaration and override assertions |
| `kobra.release-manifest/1` | `architecture/schemas/release.manifest.schema.json` | Release file index and migration chain |
| `kobra.data-api/1` | `architecture/schemas/data-api.schema.json` | Data API request/response envelopes |
| `kobra.port-deny-list/1` | `architecture/schemas/port-deny-list.json` (+ `architecture/schemas/port-deny-list.schema.json`) | Shared deny list consumed by the launcher (Section 6.4) |

### B.1 Version Compatibility and Migration Rules

- **FR-SCH-1.** Every manifest carries a `schema` field of the form `kobra.<name>/<major>`. A reader MUST reject an unknown major version with an explanatory message, and MUST accept a higher minor version while ignoring unknown fields.
- **FR-SCH-2.** Unknown fields MUST be preserved on rewrite (same rule as saves, FR-SAVE-4) so that an older tool does not silently strip a newer tool's data.
- **FR-SCH-3.** Schema files MUST be validated in CI against every manifest in a release; a release build MUST fail if any manifest does not validate. The data API request/response schemas MUST also be validated against the contract tests in Phase 2.
- **FR-SCH-4.** Manifest migration (e.g. `asset-manifest/1` → `/2`) MUST ship as a documented, one-way upgrade step in `release.manifest.json`'s `schema_migrations`, with the pre-migration file preserved.
- **FR-SCH-5.** `engine.manifest.json` MUST declare `save_version`; a mismatch with the release manifest's `save_version` MUST fail the release build.
- **FR-SCH-6.** Constraints that JSON Schema cannot express MUST be enforced by a semantic validator in the build, and the build MUST fail on violation. Known cases: the migration chain must be contiguous and advance (`from < to`); the sum of declared asset sizes must match `total_size`; and no release file path may be under `data/`.

### B.2 `launcher.config.json` and `engine.manifest.json` (abridged)

```json
{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.kobra.stardrifter",
  "game_name": "Stardrifter",
  "port": { "base": 8765, "span": 100, "require_confirmation": true },
  "data_api": {
    "max_request_bytes": 67108864,
    "writes_per_minute": 100,
    "bytes_per_minute": 67108864,
    "keep_revisions": 8,
    "trash_retention_days": 30
  },
  "server": {
    "bind": "127.0.0.1",
    "serve_mods": true,
    "idle_timeout_seconds": 90,
    "heartbeat_interval_seconds": 5
  }
}
```

```json
{
  "schema": "kobra.engine-manifest/1",
  "engine_version": "2.4.0",
  "wasm": "engine/game.wasm",
  "glue": "engine/game.js",
  "wasm_hash": "sha256:…",
  "save_version": 3,
  "required_features": ["wasm", "simd"]
}
```

Note: v2.0's `bridge` block and `preload` list are **removed**. They existed only to work around asynchronous FSA reads; with the data API, config and the initial save arrive as HTTP responses in one round trip.

---

## Appendix C. Data API Reference

This appendix is normative. The behavioural requirements are in Sections 7 and 8.

### C.1 Authentication

1. The launcher opens `http://127.0.0.1:<port>/index.html#t=<token>`.
2. The shell reads `location.hash`, calls `POST /__kobra/session` with `{"token": "<token>"}`, and erases the fragment.
3. The server responds with `Set-Cookie: kobra_session=<opaque>; HttpOnly; SameSite=Strict; Path=/` and `Set-Cookie: kobra_csrf=<opaque>; SameSite=Strict; Path=/` (not `HttpOnly`), and returns `{"session_id": "…", "csrf_token": "…", "api_version": 1}`.
4. Every write MUST send `X-Kobra-CSRF: <csrf_token>`; the server compares it to the cookie in constant time.

**FR-API-13.** The bootstrap token MUST be single-use and MUST expire 120 s after the launcher generates it. A failed exchange MUST be logged with the requester's `Origin` and MUST NOT reveal whether the token existed.

### C.2 Read Endpoints

| Request | Response |
|---------|----------|
| `GET /api/state` | `{api_version, game_id, release, engine_version, save_version, data_writable, data_dir_kind: "game"\|"override", writer, uptime_seconds}` |
| `GET /api/data/saves` | `{saves: [{slot, save_version, game_version, modified, revision, playtime_seconds, size}], rebuilt_index: bool}` |
| `GET /api/data/saves/{slot}` | The save document verbatim (`application/json`) |
| `GET /api/data/config` | The config document verbatim |
| `GET /api/data/config/mods` | `{enabled: ["hd-textures"], available: [{id, name, version, enabled, priority}]}` |
| `GET /api/export/{slot}` | `Content-Disposition: attachment; filename="<slot>.json"` |

### C.3 Write Endpoints

**`POST /api/save`**

```json
{
  "slot": "slot1",
  "if_revision": 41,
  "claim": false,
  "compression": "none",
  "payload": { "level": 4 },
  "meta": { "playtime_seconds": 8412, "title": "Run 4" }
}
```

Response `200`: `{"slot":"slot1","revision":42,"modified":"2026-09-13T15:02:11Z","bytes":91234}`

- `if_revision` omitted → unconditional overwrite (shell MUST use only after an explicit user choice, FR-SAVE-9).
- `claim: true` → request the writer role (FR-SHELL-5); returns `409` if another session holds it.
- `409` body: `{"error":"conflict","current_revision":43,"modified":"…","slot":"slot1"}`

**`POST /api/config`** — merge-write one or more top-level keys:

```json
{ "if_revision": 7, "merge": { "language": "de", "audio": { "volume": 0.6 } } }
```

Response `200`: `{"revision":8,"modified":"…"}`

**`POST /api/mod`** — `{"id":"hd-textures","action":"enable"|"disable"|"install"}`; `install` requires `archive_bytes` (base64) or `source_url: null` (offline-only; the endpoint exists for user-supplied archives).

**`DELETE /api/save/{slot}`** and **`DELETE /api/mod/{id}`** move the target to `data/.trash-<timestamp>/` rather than unlinking it, and return `204`.

### C.4 Error Envelope

```json
{ "error": "conflict", "message": "human-readable, no paths", "detail": { "current_revision": 43 } }
```

| HTTP | `error` | Meaning |
|------|---------|---------|
| 400 | `bad_identifier` | Slot/id failed `^[a-z0-9][a-z0-9._-]{0,63}$` |
| 400 | `malformed_body` | Unparseable or schema-invalid JSON |
| 401 | `no_session` | Missing or expired session |
| 403 | `bad_origin` | `Origin`/`Host` mismatch — logged at warn level |
| 403 | `bad_csrf` | CSRF cookie/header mismatch — logged at warn level |
| 404 | `not_found` | Slot, key, or mod does not exist |
| 409 | `conflict` | `if_revision` stale, or writer role already held |
| 413 | `too_large` | Body exceeds `max_request_bytes` |
| 415 | `bad_content_type` | Not `application/json` or `application/octet-stream` |
| 429 | `rate_limited` | Quota exceeded; `Retry-After` set |
| 500 | `io_error` | Disk error; `detail.reason` is a non-path errno name |
| 503 | `read_only` | `data_writable: false`; only permitted on non-save endpoints |

---

## Appendix D. Launcher CLI and Config

### D.1 CLI

| Flag | Meaning |
|------|---------|
| *(none)* | Interactive: propose the port, confirm with the user, start, open the browser |
| `--port <n>` | Use port `n` without prompting; validated against Sections 6.4/6.5 |
| `--check-port [n]` | Report whether a candidate port is free and who owns it; exit |
| `--yes` | Accept the proposed port without prompting (scripting, accessibility, CI) |
| `--data-dir <path>` | Place `data/` outside the game folder (Section 11.8); warns once that saves will not travel |
| `--browser <path>` | Use a specific browser executable |
| `--no-open` | Start the server without opening a browser |
| `--print-url` | Print the origin (with token) to stdout and exit |
| `--diagnostics` | Print the diagnostics payload to stdout and exit |
| `--reset-origin-state` | Clear the remembered port for this game (does not touch browser storage) |
| `--repair` | Roll back an interrupted update; rebuild `data/` scaffolding if absent |
| `--log-level <level>` | `error\|warn\|info\|debug` |

### D.2 `launcher.config.json`

```json
{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.kobra.stardrifter",
  "game_name": "Stardrifter",
  "port": { "base": 8765, "span": 100, "require_confirmation": true },
  "data_api": {
    "max_request_bytes": 67108864,
    "writes_per_minute": 100,
    "bytes_per_minute": 67108864,
    "keep_revisions": 8,
    "trash_retention_days": 30
  },
  "server": {
    "bind": "127.0.0.1",
    "serve_mods": true,
    "idle_timeout_seconds": 90,
    "heartbeat_interval_seconds": 5,
    "max_range_bytes": 8388608,
    "session_ttl_seconds": 28800,
    "bootstrap_token_ttl_seconds": 120,
    "probe_path": "/__kobra/probe"
  },
  "browser_preference": ["chrome", "msedge", "brave", "opera"],
  "mime_types": {
    ".wasm": "application/wasm",
    ".js": "text/javascript",
    ".json": "application/json",
    ".ogg": "audio/ogg",
    ".webp": "image/webp"
  },
  "locales": ["en"],
  "diagnostics": { "log_retention_files": 5, "log_max_bytes": 1048576, "expose_filesystem_paths": false }
}
```

Note: v1.0's `default_port: 0` and v2.0's FSA-oriented options are removed.

---

## Appendix E. Requirements Traceability

### E.1 Review and Directive Mapping

| Source | Requirements |
|--------|--------------|
| Port must be proposed and user-editable, one port per game (user directive) | FR-PORT-1…4, 6.2, 6.3, 6.6 |
| HTTP is sufficient for asset loading (user directive) | FR-AST-1…3, 9.1 |
| Drop FSA in favour of API routes (user directive) | Section 8, FR-API-1…13, Appendix C |
| Go binary, not Tauri (user directive) | 13.2, FR-LNCH-2…4 |
| Review #1 — random port breaks persistence | FR-PORT-1…4, 6.2, 6.3, 6.6 |
| Review #2 — cannot pre-select the folder | Removed with the picker; no longer applicable (Appendix F) |
| Review #3 — async FSA vs sync WASM | Eliminated: FR-API-1, 8.1, Appendix B note |
| Review #4 — server security and lifecycle | FR-SRV-1…25, Section 14 |
| Review #5 — redundant asset loading | FR-AST-1…3, 9.1 |
| Review #6 — checksums vs modding | FR-AST-4…8 |
| Review #7 — permission gesture required | Removed with FSA (FR-SHELL-1 forbids storage APIs) |
| Review #8 — no update mechanism | FR-UPD-1…9, Section 12 |
| Autoinstaller details | FR-LNCH-1…8, 13.2, 13.3 |
| Manifest schemas | FR-SCH-1…6, Appendix B |
| Save robustness | FR-SAVE-1…20, 11.4, 11.5, 11.6 |
| Error matrix | Section 16 (E1…E37) |
| Performance | Section 17 |
| Testing matrix | Section 19.2 |
| Accessibility and i18n | FR-A11Y-1…6, FR-I18N-1…7 |
| Portability / browser-storage caveat | FR-SAVE-1, FR-SAVE-18, 11.8 (in-memory mode is explicit) |
| Least privilege | FR-API-4…7, 8.2 |
| Multiple tabs/instances | FR-SHELL-5…8, FR-SAVE-9, 6.7 |

### E.2 Retired v2.0 Requirements

The `FR-FSA` family is retired. Each requirement's concern is now covered as follows; the mapping is kept so that review history and any external references remain resolvable.

| Retired | Concern | Now |
|---------|---------|-----|
| FR-FSA-1 | Namespaced handle key in IndexedDB | Not applicable — no handles exist |
| FR-FSA-2, FR-FSA-3 | Least privilege on the granted folder | FR-API-4, FR-API-6 (server-enforced) |
| FR-FSA-4, FR-FSA-5 | Permission prompt UX and honesty | Not applicable — no prompt exists |
| FR-FSA-6 | Read config/save before the worker starts | FR-API-2 |
| FR-FSA-7 | Asynchronous save writes | FR-SAVE-5, FR-API-2 |
| FR-FSA-8, FR-FSA-9 | `EAGAIN` cache, preload-all, Asyncify | Eliminated — HTTP responses are inherently async and complete before the worker starts |
| FR-FSA-10 | OPFS `FileSystemSyncAccessHandle` | Eliminated — non-goal (1.3) |

---

## Appendix F. Review Finding Resolution

| # | Finding | Disposition |
|---|---------|-------------|
| 1 | Random port breaks permission persistence | **Fixed in v2.0; simplified in v2.1.** Deterministic per-game port, user-confirmed and persisted (Section 6). Persistence no longer depends on origin at all, so a port change is now a cache-invalidation event rather than a permission loss (6.6). |
| 2 | Cannot pre-select the game folder | **Resolved by removal.** There is no directory picker and no folder-selection step (C4 retired, 10.1). |
| 3 | Async FSA vs sync WASM glossed over | **Eliminated.** No filesystem API is used from the page; the async/sync bridge, `EAGAIN` protocol, preload budgets, and Asyncify fallback are all deleted (Appendix B note, E.2). |
| 4 | Server security and lifecycle lacked detail | **Fixed and strengthened.** Loopback bind, `Host`/`Origin` on every request, double-submit CSRF, session gating, CSP, opaque probes, ranges, MIME policy, heartbeat + drain lifecycle (Sections 7, 14). Weighted toward writes because the server now writes. |
| 5 | Asset loading redundant | **Fixed as recommended.** HTTP is normative and sole (9.1). |
| 6 | Checksums conflict with modding | **Fixed.** Advisory for assets, mandatory for release payloads, with a mod-override assertion (FR-AST-4…8). |
| 7 | Subsequent permission needs a gesture | **Resolved by removal.** No permission is requested at any point (FR-SHELL-1, 10.2). |
| 8 | No update mechanism | **Fixed.** Three paths, release manifest, migrations, rollback, `launcher_min`, zip-slip/bomb defence, plus `data/` protection (Section 12). |
| — | Autoinstaller details | **Added.** Go recommendation with the Tauri rejection rationale, Authenticode/EV, SmartScreen, notarisation and stapling, executable-bit and `noexec` guidance, MOTW note (13.2, 13.3). |
| — | Manifest schemas and compatibility | **Added.** Nine JSON Schemas, version/migration rules, CI validation, and an explicit list of constraints JSON Schema cannot express (Appendix B, E.1). |
| — | Save robustness | **Added, and improved by the move server-side.** Atomic rename with fsync, backup rotation, single writer, revision-based conflict detection, quarantine on corruption (Section 11). |
| — | Error handling matrix | **Added.** 31 enumerated conditions with detection, message, and recovery (Section 16). |
| — | Performance | **Added.** Boot/memory/throughput/save budgets with round-trip and server-side splits (Section 17). |
| — | Testing matrix | **Added.** OS × browser × media × failure-injection × a11y × i18n, including read-only and `noexec` media (19.2). |
| — | Accessibility and i18n | **Added.** WCAG 2.2 AA and locale rules (Section 18). |
| — | OPFS fallback caveat | **Resolved by removal.** No OPFS fallback exists; the degraded path is in-memory with a mandatory banner and export (11.8, FR-SAVE-18). |
| — | Least privilege | **Now fully achieved.** Paths are built from validated identifiers and confined to `data/`, which the FSA model could not express (8.2, FR-API-4). |
| — | Multiple tabs/instances | **Added.** In-process write lock, writer election, read-only sessions, takeover, stale-lock reclamation, duplicate-folder prompt (6.7, 7.5, 10.4, 11.5). |
| — | Sequence diagrams | **Added.** Five flows including folder migration, atomic save, degraded startup, and mod overlay (Appendix A). |
| — | JS/WASM bridge API | **Superseded.** The bridge existed for FSA; the data API replaces it (Appendix C), and the engine receives config/save buffers by transfer instead of reading files. |
| — | Threat model | **Added and re-weighted.** Fourteen threats, trust boundaries, and an honest statement of WASM's limits; write-specific threats (T2, T3, T5) added for v2.1 (Section 14). |
| — | Stable origin strategy | **Adopted with a user-controlled twist.** Stable by default, user-editable on request, never silently changed (Section 6). |
| — | Update strategy | **Added.** Manual replacement as the always-supported baseline; optional patch path (Section 12). |
| — | Permission UX improvement | **Resolved by removal.** There is no permission UX left to improve (8.4 explains why FSA was not kept as an option). |
| — | Performance targets | **Added** (Section 17). |
| — | Rollout plan | **Added.** Five phases with exit criteria and privacy-preserving beta feedback (19.1, 19.3). |
| — | Chrome 86 vs 105 inconsistency | **Fixed.** 105 is the minimum; the Chromium-only stance is retained deliberately as tested scope, not as an API requirement (15.1). |
| — | `showSaveFilePicker()` mischaracterised | **Resolved by removal.** Saves are API calls; export uses a plain download (FR-SAVE-14, Appendix C.2). |
| — | "Default game folder is presented" contradiction | **Removed** (10.1). |
| — | "Only needs to serve static files" undersells the server | **Fixed.** Section 7.6 enumerates the real surface, including writes, and the non-goals. |

### F.1 Deliberate Non-Adoptions

| Finding or option | Why not adopted |
|-------------------|-----------------|
| Keep FSA as an optional advanced mode | It would preserve the prompt, the handle store, both code paths, and the Chromium-only requirement to serve a case that `--data-dir` and export cover in a few lines of Go (8.4). |
| `localhost` as the canonical host | May resolve to `::1` and can be remapped by hosts files or proxies; a literal `127.0.0.1` removes the ambiguity while remaining a secure context (FR-SRV-2). |
| WebSocket for lifecycle or saves | A heartbeat POST, a drain-on-shutdown sequence, and ordinary request/response writes achieve the same result without adding a framing protocol, a second parser, or an upgrade path to the attack surface (FR-SRV-18…20, 7.6). |
| Background in-game update checks | Incompatible with the offline-first non-goal and with `connect-src 'self'`; the check is user-initiated and proxied by the launcher (Section 12.2C, FR-UPD-2). |
| Mod code signing or sandboxing | Mods are trusted user data in this revision; sandboxing them needs a capability model with no current requirement. Documented as a residual risk (T14) rather than silently assumed. |
| Committing to Firefox/Safari support | No API blocker remains, but supporting them requires verifying the engine's rendering backend on Gecko and WebKit — a real test and maintenance cost that the project has not yet taken on (15.1). |
| Timestamp-based save conflict detection | FAT32's 2-second granularity makes it unreliable on the exact media this architecture targets; a server-owned revision counter is exact (11.5, 21). |
