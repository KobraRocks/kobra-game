# Kobra Launcher

The single native Go binary at the centre of the Kobra Games architecture. It
resolves the game folder from its own executable path, allocates a loopback
port, serves `game/` read-only and `data/` through an authenticated data API,
and persists saves with atomic writes and server-owned revisions.

This implementation follows `architecture/Launcher-spec.md` (v1.0). Where the
spec and the functional specification disagree, the functional specification
wins and this code is wrong.

## Status

| Area | State |
|------|-------|
| Startup sequence, exit codes, fast paths | implemented |
| Path resolution, sidecar, instance lock, port allocation | implemented |
| HTTP server, validation pipeline, sessions, CSRF | implemented |
| Data API, storage engine, revisions, trash, quotas | implemented |
| Static serving, ranges, caching, security headers | implemented |
| Browser detection and launch | implemented (Linux-first) |
| Update subsystem (verify, extract, swap, recover) | implemented |
| Diagnostics, structured log, ring buffer | implemented |
| Native OS port dialogs (Windows/macOS) | terminal prompt only |

Linux is the target platform; macOS and Windows are best effort and are
compile-checked, not run, from this checkout.

## Building

The module is `kobragames.local/launcher` and lives in `launcher/`.

```sh
cd launcher
make build          # produces launcher/launcher with -trimpath -buildvcs=false
make test           # unit + contract tests
make cross          # the §25.1 matrix, CGO disabled
make smoke          # the end-to-end drive in ../.e2e/run.sh
```

`CGO_ENABLED=0` is forced for every target triple (§3.3): the binary is
self-contained and depends on no shared library.

### Build environment note

This checkout is developed in a sandbox where the system Go caches are
read-only, so the Makefile redirects them into the workspace:

```
GOCACHE    = ../.gocache/build
GOMODCACHE = ../.gocache/mod
GOSUMDB    = off
```

Override `GOCACHE`/`GOMODCACHE` on the `make` command line to use the system
defaults. `GOSUMDB=off` means module hashes come from the proxy without
checksum-database verification, which is a sandbox consequence, not a
recommendation.

### Vendoring

`go mod vendor` is intended to be committed (§3.3) so the release build never
touches the network. The three dependencies are deliberate:

| Dependency | Purpose |
|------------|---------|
| `github.com/santhosh-tekuri/jsonschema/v5` | JSON Schema 2020-12 validation (FR-SCH-3) |
| `github.com/klauspost/compress/zstd` | zstd patch and mod archives |
| `golang.org/x/sys` | POSIX advisory locks, `SO_REUSEADDR`, `statfs` |

## Running

```
launcher/launcher [flags]
```

| Flag | Effect |
|------|--------|
| `--port N` | Use `N` without prompting. Validated; a failure is fatal, never silently substituted (§8.7). |
| `--check-port[=N]` | Report whether `N` (or the deterministic default) is free as one line of JSON, then exit. |
| `--yes` | Accept the proposed port without prompting; on conflict take the first free port. |
| `--data-dir PATH` | Place `data/` outside the game folder; reported as `data_dir_kind: "override"`. |
| `--browser PATH` | Use a specific browser executable. |
| `--no-open` | Serve without opening a browser. |
| `--print-url` | Print `http://127.0.0.1:<port>/index.html#t=<token>` and block on the server. Implies `--no-open`. |
| `--diagnostics` | Print the diagnostics payload and exit. |
| `--reset-origin-state` | Clear the remembered port. |
| `--repair` | Roll back an interrupted update and rebuild `data/` scaffolding. |
| `--log-level LVL` | `error\|warn\|info\|debug`. |
| `--max-lifetime DUR` | Testing only: force a drain after `DUR`. |
| `--version` | Print the build metadata and exit. |

Exit codes (§2.5): `0` normal exit or handoff, `1` startup failure, `3` update
failure with the game still runnable, `4` recovered panic.

## Layout

```
launcher/
├── cmd/kobra-launcher/main.go   flag parsing, startup sequence, exit codes
├── internal/
│   ├── apitypes/     wire shapes shared by server and dataapi
│   ├── browser/      detection, version floor, launch
│   ├── config/       launcher.config.json load + schema validation
│   ├── dataapi/      /api/* handlers, validation pipeline
│   ├── diagnostics/  structured JSON log, ring buffer, crash dumps
│   ├── kobraerr/     the single error taxonomy (§22)
│   ├── paths/        game folder + sidecar resolution
│   ├── port/         deterministic default, probe, deny list, bind
│   ├── server/       listener, middleware order, heartbeat, drain
│   ├── session/      bootstrap tokens, sessions, CSRF, quotas
│   ├── sidecar/      port.json, instance.lock, atomic sidecar writes
│   ├── static/       game/ and mod asset serving
│   ├── storage/      atomic writes, revisions, trash, mods
│   └── update/       manifest check, archive download, verification,
│                     extraction, directory swap, trigger state
├── schemas/          vendored JSON Schemas (a copy of architecture/schemas)
├── port-deny-list.json
├── Makefile
└── go.mod / go.sum
```

The dependency graph is acyclic and flows one way (§3.2). The data API depends
on interfaces (`dataapi.Service`, `dataapi.Host`) that the server implements, so
neither package imports the other. `storage` is the only package that writes
game data.

## Security properties worth knowing

- **Host is compared exactly** to `127.0.0.1:<port>`. `localhost`, `[::1]` and
  every hostname are rejected with `421` before any handler runs (§12.1). This
  is the DNS-rebinding defence.
- **Origin is checked on every request**, including reads (§12.2).
- **The session gate runs before the CSRF gate**, so an unauthenticated request
  never causes a CSRF comparison (§12.6).
- **CSRF applies to cookie-authenticated requests only** (FR-SRV-6a, clarified in
  FS v2.2). The double-submit check defends an *ambient* credential, and the
  session cookie is the only one: a browser attaches it without the page asking.
  A request authenticated with `Authorization: Bearer <session-id>` carries
  nothing ambient — the caller attached the id, and a cross-origin page cannot
  read it — so it is exempt. Before this rule the bearer path could read but
  never write, because the CSRF check demanded a cookie that a non-browser client
  does not have. `internal/server/contract_test.go`
  (`TestBearerSessionWritesWithoutCSRF`) pins both halves: the bearer write
  succeeds, and the same write through the cookie with the header removed is
  still `403`.
- **A missing `archive.hash` fails closed.** `internal/update/verify.go` refuses
  a manifest that declares neither `archive.hash` nor the `archive_sha256`
  alias, instead of silently downgrading to per-file verification (§8.2.3.3).
  The message blames the release, not the user's download.
- **The release manifest is refused inside an update archive**, with the reason
  `release_manifest_in_archive`. A manifest cannot index its own hash, so it can
  never be a member of the archive it describes (§6.4, §1.2).
- **Every identifier is regex-validated before it becomes a path component**,
  and the engine re-checks confinement afterwards (§16).
- **`data/` is never statically served.** `/data/saves/slot1.json` is a 404, not
  a file read (§13.1).
- **Symlinks are followed but re-checked**, so a link to `/etc/passwd` is a 404
  (§13.3).
- **No error message contains a filesystem path, the OS username, or a stack
  trace.** `internal/kobraerr/kobraerr_test.go` enforces this on every
  constructor (§26.5).
- **Deletion moves to `.trash-<ts>/`, never unlinks** (§14.7).
- **The launcher's only outbound request is the user-initiated update check**
  (§1.1, FR-UPD-2). There is no telemetry.

## Testing

```
make test     # all packages
make race     # under the race detector
make smoke    # end-to-end: build, serve, drive the API, drain
```

`internal/server/contract_test.go` validates every response shape against
`data-api.schema.json`, the normative contract, including the `409 conflict`
envelope. `internal/storage/storage_test.go` covers the atomic write, backup
rotation, revision recovery and pruning, trash, and confinement.
`internal/port/port_test.go` pins the deterministic FNV-1a hash (§26.3), so a
code change cannot silently move every existing install to a new origin.

## Per-game launchers and development

### A per-game launcher is a config file, not a binary

The binary never changes per game. Everything that makes a launcher "belong" to
a game lives in `<GameFolder>/launcher/launcher.config.json`:

| Field | Why it is per-game |
|-------|--------------------|
| `game_id` | Keys the sidecar directory **and** the deterministic port hash, so two games never collide on a port or share state |
| `game_name` | Shown in user-facing messages |
| `release` | Reported as `/api/state.release`; compared by the update check |
| `port.base`/`port.span` | The candidate window for this game |
| `browser_preference`, `min_browser_version` | Per-game compatibility floor |
| `mime_types`, `server.csp`, `data_api.*` | Per-game serving and write policy |

So "creating a launcher for a specific game" means creating the game folder
layout and writing that config:

```
OtherGame/
├── launcher/
│   ├── launcher            # the same binary, copied
│   ├── launcher.config.json  # game_id: com.kobra.othergame, ...
│   └── port-deny-list.json
├── game/index.html
└── data/
```

```sh
make package GAME=/path/to/OtherGame
```

`game_id` must be reverse-DNS lowercase (`^[a-z0-9]+(\.[a-z0-9-]+)+$`) and the
schema rejects anything else. Because it feeds the FNV-1a port hash, **changing
it changes the port and the sidecar location**.

### Why the launcher cannot be pointed at a game folder

FR-LNCH-1 is load-bearing: the game folder is derived *exclusively* from the
launcher's own executable path, never from the working directory, `$PWD`, or an
environment variable. That is a deliberate security property — a launcher that
takes its root from ambient state is a path-confusion and rebinding target — so
there is deliberately no `--game-dir`. When the game lives at another path, you
satisfy the layout instead of overriding it.

### Developing a game that lives in another folder

`make run-dev` builds a shim game folder whose `game/` entry is a symlink to the
real source tree:

```sh
make run-dev GAME=../my-game/src            # interactive
make run-dev GAME=../my-game/src ARGS="--yes --no-open"
make run-dev GAME=../my-game/src DATA_DIR=~/saves/mygame
make run-dev-clean
```

It produces:

```
.dev/                      # shim, gitignored
├── launcher/
│   ├── launcher
│   ├── launcher.config.json   # from dev/launcher.config.dev.json
│   └── port-deny-list.json
├── game -> /absolute/path/to/my-game/src   # symlink: edit in place, no copy
└── data/                  # a real directory, so saves never enter the source tree
```

Why this works with no launcher changes:

- `paths.Resolve` evaluates symlinks on the **launcher executable**, so a
  symlinked launcher resolves back into the real tree.
- Static serving follows symlinks but re-checks the resolved target against the
  root (§13.3), so a symlink to `/etc/passwd` is still a `404` — verified by
  `internal/static/static_test.go`.
- Edits to the source tree are visible on the next request; there is no copy
  step and no reload.

Two alternatives, both verified:

```
# 1. Symlink the launcher instead of the game tree. The game folder stays the
#    real source tree; the launcher is reached through a link.
ln -s /path/to/GameFolders/launcher/launcher /usr/local/bin/my-game
my-game --yes

# 2. Run the launcher from a shim folder whose game/ is a symlink (this is what
#    `make run-dev` automates).
```

`--data-dir` is the supported way to keep saves out of a tree: it sets
`data_dir_kind: "override"` in `/api/state` and prints a one-time warning,
because saves no longer travel with the game folder.

### The dev-only `--game-dir`

For conveniences the symlink workflows do not cover — no shim directory, an IDE
debug target, an absolute path straight from a shell history — a **development
build** accepts an explicit game folder:

```sh
make build-dev
./launcher-dev --game-dir /path/to/my-game/src --yes --no-open
./launcher-dev --game-dir /path/to/my-game/src --data-dir ~/saves/mygame
```

The flag exists only under the `kobra_dev` build tag, so:

- a **release** binary has no `--game-dir` at all — it fails with `flag provided
  but not defined: -game-dir`, which the end-to-end suite asserts;
- the override applies to the §19.4 update-recovery pass as well as to serving,
  so an interrupted update in a dev tree is still recovered;
- the startup log records `launcher.folder.override`, and the shim's
  `FolderKind` is `override`, so a dev run is distinguishable in diagnostics;
- a developer banner is printed to stderr on every dev run.

This is the spec-clean way to relax FR-LNCH-1: the property still holds for
every binary a user receives. Prefer the symlink workflows when they fit,
because they exercise the same code path a release build does.

## Known gaps

- macOS and Windows are compile-checked (`make cross`) but not executed here.
- The native port-confirmation dialog of §8.5 is a terminal prompt on Linux;
  Windows and macOS fall back to the same prompt.
- GPG signature verification of a release manifest **fails closed**: no verifier
  ships with the launcher, so a manifest that declares a signature is refused
  rather than trusted (§19.3 step 2, FR-LNCH-5).
- Mod archive extraction accepts the same `.tar.zst` layout as updates; the mod
  archive format is deferred in the spec (§27.5).
- `update.channel: patch` is now exercised end-to-end: `testgame/run.sh`
  phase 15 records an update, restarts the launcher, and asserts the swap that
  happens at boot (Updater spec §18). The apply point is the next start, never
  the running session (Updater spec decision D1).
- In-browser update progress is deferred: `GET /api/update/progress` reports the
  recorded phase and outcome, but no shell panel reads it yet, so the
  confirm-with-size, live-progress and rollback-offer requirements of Packaging
  spec §9.4 are not met (Updater spec §19.1).
- Rollback retention advances on the first shell heartbeat, not at startup, so a
  release that starts and dies before the page connects is not counted. The
  retirement at two starts is covered by unit tests; `--print-url` starts no
  browser and therefore never retires `game.old`.
- An unrecognised key in a local `launcher.config.json` makes an update abort
  with E28 rather than being carried forward, because the config schema sets
  `additionalProperties: false`. The merge itself preserves such keys; see
  Updater spec R14.4 and open item P5.
