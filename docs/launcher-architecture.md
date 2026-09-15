# How the launcher serves your game

A contributor's tour of the launcher: what happens between double-clicking the
binary and a save file reaching `data/`, and where to change what. It is written
for someone about to modify the internals — for building a game instead, read
[authoring a game](authoring-a-game.md).

Everything here is a summary of behaviour the specifications in `architecture/`
define normatively. Where a decision is surprising, the reason is given; where a
decision is deferred, it is marked as such rather than hidden.

## 1. Process start: the §4 sequence

`cmd/kobra-launcher/main.go` is the whole startup path, in `run()`. It is a long
function on purpose: the order is the contract, and the numbered steps correspond
to Launcher spec §4.

| Step | What happens | Why it is here |
|---|---|---|
| 1 | Parse flags. Dev-only flags (`--game-dir`) are registered only in a `kobra_dev` build. | A release binary has no way to take a game folder from the command line (FR-LNCH-1). |
| 2 | `os.Executable()` | The game folder is derived from where the binary is, never from the working directory or an environment variable. |
| 3a/3b | **Recovery, then the update apply** — before the layout check. | §19.4 recovery can be asked to run in a state where `game/` is missing, which is exactly what the layout check rejects. Recovery must be able to restore a runnable release *before* anything insists on finding one. The apply runs in the same window, before the instance lock, the port and any socket exist, so no client can observe a half-swapped folder. |
| 3c | Resolve the game folder | `paths.Resolve` walks at most one directory up from the executable; `--check-port` and `--diagnostics` stop mutating here. |
| 4 | Load and validate `launcher.config.json` | Schema-validated; the probe path is additionally checked against the endpoint actually served. |
| 5 | Open the sidecar and the log | A sidecar failure degrades to the in-folder `.kobra` fallback rather than failing startup. |
| 6 | Fast paths: `--check-port`, `--diagnostics`, `--repair`, `--reset-origin-state` | These answer "can I run?" without mutating the install. |
| 7 | Acquire or detect the instance lock | A live instance of the same game means handoff: open its origin and exit 0. |
| 8 | Allocate the port | Validate against the deny list, optionally confirm with the user, probe, then **bind and hold** the listener. |
| 9 | Probe `data/` write access, rebuild revisions, quarantine strays, clean trash | Failures are recorded, not fatal: the game runs in a degraded mode and `/api/state` reports it. |
| 10–11 | Build the server on the bound socket, complete the lock, write `port.json` | The lock and the port record are completed *after* binding, in place. |
| 12–14 | Detect a browser, issue a bootstrap token, open the page | The token is minted before the browser opens, so the URL fragment always carries one. |
| 15–18 | Wait for a drain, then exit | Exit code 4 if a handler panicked (FR-SRV-24), otherwise 0. |

Two details worth internalising:

- **The lock is not the port check.** The lock is `O_CREATE|O_EXCL`, with an
  advisory `flock` and a liveness check (PID, PID start time, boot time, and a
  health probe) as backstops for a stale file. The election is the exclusive
  create; everything else only decides whether the existing file is *stale*.
- **The port is held, not just chosen.** `port.Available` returns an already-open
  `net.Listener` and the server adopts that same listener, so there is no window
  between "the port is free" and "we own it". `SO_REUSEADDR` only, never
  `SO_REUSEPORT`.

## 2. The request pipeline

`server.buildHandler` composes the middleware. Execution order is outermost
first:

| # | Middleware | Responsibility |
|---|---|---|
| 1 | `securityHeaders` | The §13.7 header set (nosniff, referrer policy, frame denial, COOP, CSP). |
| 2 | `recovery` | `recover()`, write a `crash-<ts>.txt`, answer 500 `io_error`, record that a panic happened, request a drain. In a `kobra_dev` build it re-panics on a fresh goroutine so the developer sees it. |
| 3 | `logging` | Request id, debug-level access log, and the §26.4 panic injection seam. |
| 4 | `drainGate` | §10.6: once the drain has begun, a request that starts after it is 503 with `Connection: close`, before any gate below does work. |
| 5 | `hostGate` | `Host` must be exactly `127.0.0.1:<port>`; anything else is 421. This is the anti-DNS-rebinding gate and it runs before the session store or the filesystem. |
| 6 | `originGate` | A present `Origin` must be our origin; otherwise 403. Applied to reads as well as writes. |
| 7 | `normalisePath` | Rejects `\`, `//` and any dot segment with 404, then cleans the path. **Load-bearing**: without it `http.ServeMux` answers mirror-image paths with a 307 pointing at the cleaned path, leaking structure before the static resolver's confinement check runs. |
| 8 | `trackInFlight` | The counter the drain waits on. |
| 9 | `concurrencyLimit` | A 32-slot semaphore; excess is 503 with `Retry-After`, never queued. |
| — | mux | `static` and `dataapi` routes. |

`securityHeaders` is outermost although §10.3 numbers it fifth, because the two
gates below it can answer *before* the rest of the chain runs: a 421 or 403 that
carried no CSP would contradict both §13.7 and the promise in the code comment.

## 3. Two kinds of request

### Static: `internal/static`

1. `resolveUnder` rejects NUL bytes, backslashes and Windows device names, then
   `path.Clean` anchored at `/`, then `paths.Confine` — the whole path must stay
   under the served root.
2. Only `/`, `/index.html`, `/shell.js`, `/engine/*`, `/assets/*`, `/locales/*`
   and (when `serve_mods` is on) `/mods/<id>/assets/*` are routed.
3. `serveFile` opens the file, and `statConfined` re-resolves symlinks on **both**
   the root and the target and re-checks confinement. A symlink that escapes is a
   404, not a disclosure. The check is a `stat` then an open, so a swap in that
   window requires write access to the served tree — a known, accepted residual
   (see `CODE-REVIEW.md`).
4. Ranges, `ETag` (computed once per session), cache policy per file class and
   `304` handling live in `serve.go`.

### Data API: `internal/dataapi`

Every route goes through one pipeline: method and path shape, `Content-Type` for
bodies, a `Content-Length` ceiling with a capped reader, `DisallowUnknownFields`
and trailing-content rejection, then the session and CSRF checks, then the
identifier rules, then the handler. The handler talks to `storage` and nothing
else.

`dataapi` holds no state: it reaches the launcher through the `Service` and
`Host` interfaces that `server.Server` implements, which is what keeps the HTTP
shapes separate from the storage engine.

## 4. The storage engine: the only writer of `data/`

`internal/storage` is the single writer for saves, settings and mods. Everything
else reads through it. That is enforced by convention plus the confinement rules,
not by the type system.

### The atomic write protocol

`Engine.writeAtomic` is the §15.3 protocol, and every writer in the package uses
it:

1. check the context, then the disk-space guard (`checkSpace`);
2. create `.<name>.tmp-<pid>-<seq>` with `O_EXCL` in the target directory;
3. write, then `fsync` the file — **crash window 1**: the payload is durable, the
   previous revision is untouched;
4. `stat` the temp and compare its length to what was written;
5. rotate the existing target to `<name>.bak` (one generation, removed first);
6. rename the temp onto the target — `renameCrossDevice` if that comes back
   `EXDEV`, which records `AtomicityDegraded()` — **crash window 2**: the new
   revision is in place, the directory entry is not yet fsynced;
7. `fsync` the containing directory;
8. return success. A cancellation that arrived during the rename is **not**
   reported as failure: the data is already durable, and reporting failure made
   callers retry a completed write and desynchronised the revision store.

A crash in window 1 leaves the old revision live and a temp file behind; the temp
file is moved to the trash area at the next start by
`Engine.QuarantineStrayTemps`, never deleted (FR-SHELL-4). A crash in window 2
leaves the new revision live and the previous one in `.bak`.

### Around the write

- **Revisions** live in memory (`revMu`) and on disk as `<name>.<n>` sidecars.
  `InitRevisions` rebuilds them by scanning at startup, preferring the sidecar
  maximum and falling back to the header's own `revision`.
- **Reads** fall back to `.bak` when the live file is missing, unparseable, or
  fails its checksum, and log `save.recover` with `from: bak`.
- **Deletes** move to `data/.trash-<ts>/<area>/`, never unlink, and
  `CleanupTrash` collects trash older than the retention window at startup.
- **Writer election** is in-memory (`writerMu`): first claim wins, a second tab
  gets `writer_held`.
- **Checksums** are over a canonical JSON re-encoding (sorted keys, no
  insignificant whitespace) so re-saving an unchanged body is byte-stable.
- **`readConfigBytes`** is the settings-only reader with the same `.bak`
  recovery; settings merges are restricted to a fixed whitelist of top-level
  groups because the file is engine-owned.

Every write takes `writeMu` exclusively and every read takes it shared. That is a
conservative superset of the per-slot concurrency the spec permits; it is
deliberate, because the cheap version is where corruption bugs live.

## 5. Concurrency and lifecycle

Goroutines the launcher runs:

| Where | What | Ends when |
|---|---|---|
| `server.Serve` | `http.Server.Serve` on the held listener | the listener closes |
| `server.watchLoop` | 1 s ticker: session reaper, idle check, §20.5 connection watchdog | the drain starts |
| `main.run` | `--max-lifetime` timer | process exit |
| `main.run` | signal handler: first signal drains, later ones are logged | process exit |
| `diagnostics.WatchSIGHUP` | log rotation on SIGHUP | the stop func runs |
| `update.stallGuard.loop` | download idle deadline | the transfer ends |
| `main.confirmPort` | the 60 s port prompt read | the prompt is answered |

Locks: `storage.writeMu` (data writes), `storage.revMu` (revision map),
`storage.writerMu` (writer role), `storage.probeMu`/`degradedMu` (flags),
`sidecar`'s lock file (cross-process), `session.Store`'s own mutex, and
`diagnostics`'s log mutex — the last of which must never be re-entered, which is
why `rotateLocked` emits its record through a lock-free path.

**Drain order** (`server.drain`, §10.6/§23.4): mark draining so the middleware
rejects new work → wait for in-flight handlers up to `drain_timeout` → cancel the
base context (cancels handler work and downloads) → `http.Server.Shutdown` then
close the listener → clear sessions. The listener stays open until the drain
finishes so a mid-response client is not cut off.

## 6. The update path

`internal/update` is the only package allowed to make an outbound request.

1. `POST /api/update/apply` **records intent** — a `pending.json` marker and a
   retry budget. Nothing is downloaded while the game runs.
2. The next start's §3b pass calls `Startup`, which calls `Recover` first (finish
   or roll back an interrupted swap) and then `Apply`.
3. `Apply` is one linear pipeline: validate the marker → what is installed →
   already current? → fetch the manifest → reconcile the marker with it → check
   `launcher_min` → disk preflight → download (idempotent, resumable, stall-guarded)
   → verify → extract into the staging root under the sidecar → assert `data/` is
   unchanged → merge the release's config with the local one → **swap** → replace
   `launcher/` in place → record the result and arm retention.
4. `Swap` writes `update.state` before the first move, moves `game/` to
   `game.old/`, promotes the staged tree (via `promoteDir`, which falls back to a
   copy-and-rename across filesystems), then rewrites the marker as `promoted` and
   removes it. `Recover` decides from which directories exist rather than from the
   marker's contents, because the marker's own write can be interrupted.

The one ordering decision worth knowing: **`launcher/` is replaced after the
swap**, not before. `launcher.config.json` carries the release-owned `release`
key, and when the archive omits `game/release.manifest.json` (the normal full
archive) that config is what reports the installed release — so replacing it
first meant a failed swap left the folder claiming a release it had never
installed, and the retry short-circuited as `already_current`.

## 7. Where the invariants are enforced

| Invariant | Enforced by | Proved by |
|---|---|---|
| No path, username, token or stack trace in a user-facing message | `kobraerr` constructors take fixed text; the cause is internal | `TestNoErrorLeaksPathsOrUsername`, which walks every constructor |
| The game folder comes from the executable | `paths.Resolve` | release-binary tests that `--game-dir` is absent |
| Loopback only, exact `Host` | `port.Available` binds `tcp4`; `hostGate` | `.e2e/run.sh` (421 for wrong Host, 403 for bad Origin) |
| Every response carries the §13.7 headers | `securityHeaders` outermost | `TestSecurityHeadersOnGateRejections` |
| No `data/` change during an update | snapshot/compare around the pipeline | `apply_test.go`, plus the fixture drive's `data/ is untouched` |
| Crash safety of the write path | the protocol above | `make -C launcher faultinject` |
| Schema copies cannot drift | embedded copies | drift tests in both modules |
| Packaging is reproducible | sorted maps, release-derived timestamps | `make -C testgame verify` |

## 8. If you are changing X, look at Y

| Change | Start here | Do not forget |
|---|---|---|
| A data API endpoint | `dataapi/handlers.go` (`Register`), then the pipeline in `pipeline.go` | The schema in `architecture/schemas/data-api.schema.json` **and** the `internal/server/testschema` copy; the contract test validates responses against it. |
| A config key | `config/config.go`, the schema, then the feature | `additionalProperties: false` means the schema is the contract. A key nobody reads is a bug (see `CODE-REVIEW.md`). |
| Middleware behaviour | `server/server.go` | The order comment; gates that answer must still carry headers. |
| Storage behaviour | `storage/storage.go` | The atomic protocol's two crash windows, and `make -C launcher faultinject`. |
| An updater step | `update/apply.go` | The marker/recovery matrix in `swap.go`, and the `promoteDir` fallback. |
| A packaging rule | `packaging/internal/pack/collect.go` | `check` and `build` must stay the same gate; a rule enforced in one only is a bug. |
| A platform file | the `*_windows.go` / `*_unix.go` pair | `make -C launcher cross`. |

## 9. What is deliberately not abstracted

- **No interfaces with one implementation.** Packages are concrete types; the two
  interfaces that exist (`dataapi.Service`/`Host`) exist to break a dependency
  cycle, not to enable substitution.
- **Platform code is per-OS files**, not runtime switches, so the compiler proves
  each platform's code exists.
- **Some duplication is deliberate.** `packaging` and `launcher` have separate
  archive/extract implementations because they sit on different trust boundaries
  in two modules that cannot import each other. The rule tables that mirror each
  other have comments and tests pinning them.
- **One duplication is deferred, not blessed.** The atomic-write primitive exists
  in several packages and has already diverged once; consolidating it is recorded
  as a deferred item in `CODE-REVIEW.md`, to be done with the fault-injection
  seams rather than before them.

## Further reading

- `architecture/Launcher-spec.md` — the normative source for everything above.
- `README.md` — the security model in one page.
- `CODE-REVIEW.md` — what was found, what was fixed, and what is deferred.
- `CONTRIBUTING.md` — the gates and the rules of the house.
