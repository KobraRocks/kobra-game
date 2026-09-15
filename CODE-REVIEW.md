# Code review — Kobra Games (Launcher, Packager, Updater)

Review method: read the specs against the code, run every gate, and **reproduce**
suspected defects instead of inferring them. Findings carry a verification tag:

- `[repro]` — I ran a reproduction and observed the failure.
- `[read]` — the code path makes the outcome deterministic; verified by reading.
- `[reported]` — found by a delegated deep dive with its own repro; consistent
  with the code I read, but I did not personally re-run it.

## Gates I ran

| Gate | Result |
|---|---|
| `make -C launcher vet` / `gofmt -l` | clean |
| `make -C launcher test` | all packages pass |
| `go test -race` (storage, update, sidecar, port) | pass |
| `make -C launcher cross` (linux/windows/darwin × amd64/arm64) | builds, exit 0 |
| `make -C packaging test` / `vet` | pass / clean |
| `cd testgame && make verify` | "two builds are byte-identical" |
| `make -C packaging race` | **broken**: `-race requires cgo` (CGO_ENABLED=0) |

Test volume is healthy — 7.4k test LOC against 14.4k implementation LOC in
`launcher/` (~0.51), ~90 test functions in `internal/update` alone — and tests are
named as behavioural claims. That is above average, and it is why several of the
findings below are surprising: they are all in paths the suite does not touch.

## Verdict

The discipline is well above typical product code: spec traceability, structured
event logs, a §26.5 negative-property test for path leakage, a genuine
reproducibility guarantee, and crash-safe write protocols that were actually
reasoned about. It is **not** over-engineered in the usual sense — there is almost
no single-implementation interface ceremony. The problems cluster:

1. **One production hang.** The log-rotation path self-deadlocks on the default
   configuration and freezes the whole launcher (P0-1).
2. **The updater's "atomic apply" is not atomic.** Non-transactional work happens
   before the transactional swap; a failed swap silently loses the update and
   corrupts the reported release (P0-2). Cross-volume installs cannot update at
   all (P0-4).
3. **The packager's gate is not a gate.** `check` passes trees `build` rejects,
   and can ship a package the launcher refuses to start (P0-3).
4. **Defensive machinery that is defined but never armed** — the stall watchdog,
   five port event loggers, the panic exit code, three config knobs. Each is a
   documented guarantee that does not run.

---

## Status after the fix pass

Every P0 and P1 finding is fixed and guarded by a regression test, plus the P2
items that were deletions, deduplication or comment corrections. Two areas are
deliberately deferred and are listed at the end with their reasoning. `gofmt`,
`go vet`, `go test`, `go test -race` (both modules), `testgame make verify` and
the `kobra-pack check`/`verify` gates all pass.

| Finding | Status | Regression test |
|---|---|---|
| P0-1 rotation deadlock | fixed | `TestRotateDoesNotDeadlock`, `TestForceRotateDoesNotDeadlock` |
| P0-2 failed swap loses the update | fixed (swap now precedes the launcher merge) | `TestApplyFailedSwapKeepsTheInstalledRelease` |
| P0-3 `check` ≠ `build`; missing root files | fixed | `TestCheckAndBuildAgree`, `TestRequiredRootFilesAreEnforced` |
| P0-4 cross-device swap | fixed (`promoteDir`, also used by `Recover`) | `TestSwapCrossDevicePromotion` |
| P0-5 unarmed stall watchdog | fixed (default applied in `newStallGuard`) | `TestStallGuardDefaultsToStallTimeout`, `TestStallGuardFires` |
| P1-1 exit code 4 unreachable | fixed (`Server.Panicked`; dev builds re-panic) | `TestRecoveredPanicIsReportedForExit4` |
| P1-2 headers missing on 421/403 | fixed (headers applied outermost) | `TestSecurityHeadersOnGateRejections` |
| P1-3 `con.txt` accepted | fixed (one `paths.ReservedName` for engine, static and archives) | `TestValidateIdentifierRejectsEveryForbiddenShape`, `TestReservedName` |
| P1-4 mod confine root | fixed (`<DataDir>/mods`) | `TestModAssetsConfinedToModsDir` |
| P1-5 durable write reported as failure | fixed | `TestCancelledContextWritesNothing` |
| P1-6 `settings.json` had no `.bak` recovery | fixed (config and saves) | `TestReadConfigRecoversFromBackupWhenLiveIsMissing` |
| P1-7 extracted files never fsynced | fixed | existing extraction suite |
| P1-8 finished swap rolled back by `Recover` | fixed (`promoted` marker) | `TestRecoverMatrixPromotedMarker` |
| P1-9 revert offer named the wrong release | fixed (reads `game.old/`) | `TestBackupReleaseNamesTheBackup` |
| P1-10 R10.6 byte counts | fixed (record and wire type) | `TestPreflightRefusalRecordsByteCounts` |
| P1-11 R14.2 owner table | fixed (all six keys, objects included) | `TestMergeReleaseOwnedKeysFollowTheRelease` |
| P1-12 inert knobs | heartbeat published and used by the shell; `probe_path` enforced | `TestStatePublishesTheHeartbeatInterval`, `TestValidateProbePathRejectsAnUnservedEndpoint` |
| P1-13 five port events never emitted | fixed (`*Logged` helpers wired in `main`) | `TestAvailableLoggedEmitsProbeBindFailedScanAndChange` |
| P1-14 watchdog log-only | fixed (`OnWatchdogTimeout` surfaces E22) | `TestWatchdogFiresOnceAndNotices` |
| P1-15 false invariants | comments corrected; the destructive Windows replace now moves the old file aside | `TestReadSaveRecoversFromBackupWhenLiveIsMissing` (sibling case) |
| P1-16 dead gates / weak `verify` | fixed (`race`, `fmt`, index hashes, incomplete dist) | `TestVerifyRejectsTamperedZipMember`, `TestVerifyRejectsMissingReleaseManifests` |
| P2-1 atomic-write duplication | **deferred** — see below | — |
| P2-2 predicate / `writeError` duplication | fixed (`paths.ReservedName`, `kobraerr.WriteEnvelope`) | `TestWriteEnvelope*` |
| P2-3 schema copies | dead copy deleted; drift test added for the launcher's copies | `TestSchemaCopiesMatchThePublishedSet` |
| P2-4 dead symbols | deleted (incl. `RingLines`; its three call sites use `Tail(0)`) | build plus existing suites |
| P2-5 `spacePath` duplicated | fixed; a portable `freeSpace` fallback also makes BSD/solaris/illumos build | cross-build check |
| P2-7 smells | `parsePort`, `pendingRecovery`, `stale_pid`, `confirmPort`, `.e2e` paths/vendor, Makefile `fmt`, docs | `TestParsePortRequiresTheWholeString` |
| P2-8 latent | `port.Scan` deadline and `Tail` triplication fixed; the rest remain | `TestScanDeadlineBoundsProbing` |

### Two judgement calls worth recording

- **R5.1's rename is not implemented.** The spec says the archive is renamed after
  `Verify`; the code verifies in place and never renames. The comments now say so
  rather than claiming a rename that does not happen. The requirement's purpose —
  no consumer reads unverified bytes — holds, and a second artefact name would add
  cleanup surface for no safety gain. Recorded as a deliberate deviation.
- **P1-12's remaining reserved keys** (`game_name`, `locales`,
  `allow_out_of_range`, `update_check_user_initiated_only`,
  `retain_previous_release`, `allow_data_dir_override`) are still unread. They are
  not deleted because the config schema is a published contract and a publisher's
  config may already set them; removing a key would reject configs that are valid
  today. The two keys that could silently *misbehave* — a heartbeat nobody honours
  and a probe path the launcher does not serve — are fixed.

### Deferred, with reasoning

- **P2-1: one atomic-write primitive.** `sidecar.WriteAtomic`,
  `storage.writeAtomic`, `update.writeState`, `update.writeFileAtomic*` and
  `extract.copyFilePreservingMode` remain separate implementations. This is a pure
  refactor of the most safety-critical code in the project; it changes four
  packages at once and has no behavioural payoff by itself. It belongs in its own
  change, landed with the `kobra_faultinject` scenarios of §26.4 (crash after
  fsync, crash after rename, EXDEV, disk-full) so the unified primitive is tested
  by injection rather than by inspection. Deferred deliberately.
- **Remaining P2-7/P2-8 cosmetics** — the `browser` package-level search plan,
  hardcoded session TTLs in `server.New`, the unreachable `uint16` bound in
  `sidecar.Load`, `decodeJSON`'s second copy, shared e2e helpers, the static-file
  TOCTOU window, `.part` resume, and R13.6 for manifests — are unchanged. None is
  a correctness or security defect at the shipped threat model, and each is small
  and independent.

---

## Release readiness (after the repository-hardening pass)

The review's release blockers were, in order: no version control or CI, no
licence, no crash-safety evidence, and the unverified Windows/macOS paths. The
first three are now closed.

| Blocker | Status |
|---|---|
| No version control | Closed: the tree is committed and pushed to `github.com/KobraRocks/kobra-game` (`main`). |
| No CI | Closed: `.github/workflows/ci.yml` runs exactly the make targets verified locally — `fmt vet test race` and `faultinject` for the launcher, `check race` and the byte-identical-archive check for the packager, plus both end-to-end drives with `zstd` installed for the fixture's `.tar.zst` assertions. Confirmed green on GitHub (run 35003789673: all three jobs, zero failed steps), which is the only way to know the workflow itself is correct. |
| No licence | Closed: `LICENSE` (MIT) plus `THIRD_PARTY_NOTICES.md`, generated from the dependency licence files rather than transcribed. The notices also ship *inside* every package (`LICENSES/THIRD_PARTY_NOTICES.md`), because a package embeds `launcher/launcher` and therefore statically links BSD-3-Clause and Apache-2.0 code — the notices were previously absent from the distributed artefact, which was a real compliance gap. |
| Crash-safety evidence absent | Closed: the `kobra_faultinject` harness of §26.4 is implemented — a build-tagged injection package with wired points, tagged Go tests for the in-process scenarios, and `launcher/test/faultinject.sh` (`make -C launcher faultinject`) for the ones that need a real process. |
| Windows/macOS unexecuted | **Open.** Unchanged by this pass: no machine here runs either. The self-replacement path is the specific risk. |
| Code signing / notarisation | **Open.** Not addressed. |
| P2-1 atomic-write consolidation | **Open**, deliberately deferred (see above). |

### What building the harness found

Two defects, both of which the harness caught rather than the review:

1. **The spec's quarantine did not exist.** §26.4 asserts that a crash between
   `fsync` and `rename` leaves the temp file *quarantined*; nothing moved or
   cleaned it, so `.<slot>.json.tmp-<pid>-<seq>` accumulated in `data/saves/`
   forever. `Engine.QuarantineStrayTemps` now runs at startup, moves those files
   into `data/.trash-<ts>/<area>/` (never deletes them — FR-SHELL-4), and logs
   `save.recover` with `from=quarantine`.
2. **My own injection ordering was wrong.** The first version injected the EXDEV
   error *after* the real `os.Rename` had already moved the file, so the fallback
   ran with no source and failed for the wrong reason — a test that would have
   "proven" the fallback while exercising a different path. Both call sites now
   inject before the real rename. `TestPromoteStaysAtomicWhenTheRenameWorks` is
   the control that keeps the fallback a fallback.

The harness also made two previously untestable scenarios deterministic: the
`EXDEV` promotion (no longer depends on the machine having two filesystems) and
the disk-full mapping (no longer needs a real full disk).

---

## P0 — fix before shipping

### P0-1. Log rotation self-deadlocks the launcher `[repro]`

`write` takes `l.mu` and then calls `rotateLocked` while holding it
(`diagnostics.go:217`, `:235`); `rotateLocked` calls `l.Event(...)` at
`diagnostics.go:290`; `Event` calls `Enabled` at `:184`, which takes `l.mu` again
at `:175`. `sync.Mutex` is not reentrant, so the rotating goroutine blocks
forever **holding the mutex**, and every later log call from every goroutine
(including HTTP handlers) blocks behind it. `ForceRotate` (`:255-257`, SIGHUP)
takes the same path.

Repro: `Open(Options{LogDir: t.TempDir(), MaxBytes: 64})` then ~50 `Event` calls
→ `test timed out after 30s`, goroutine dump shows `goroutine 19 [sync.Mutex.Lock]`.

Impact: rotation triggers at the default 1 MiB (`main.go:484` passes
`cfg.Diagnostics.LogMaxBytes`) or on SIGHUP — i.e. on any long session. The
launcher then hangs, cannot drain, and cannot be stopped except with SIGKILL.
Highest-severity finding in the review, and the smallest fix: emit the
`log.rotate` record through a lock-free path, or drop the event.

There is no log-rotation test, which is exactly why this shipped.

### P0-2. A failed update swap silently loses the update `[repro]`

`Apply` replaces `launcher/` **in place before** the swap:
`MergeLauncherInto(stagingRoot, gameFolder)` at `apply.go:455`, then
`Swap(...)` at `apply.go:464`. `MergeLauncherInto` (`extract.go:235-268`) copies
the release's merged config onto the live folder, and that config carries the
release-owned `release` key (`configmerge.go:48-50`). `InstalledReleaseIn`
prefers `game/release.manifest.json` but falls back to the config
(`retention.go:139-149`) — and the fallback is the *normal* path, because the
full archive deliberately omits the manifest (`server/update.go:20-28`).

So a failed swap leaves the folder reporting the **new** release while `game/`
is still the old one; the retry short-circuits at `ApplyAlreadyCurrent` and the
update is silently gone. Repro:

```
first apply : outcome=failed reason=swap_failed   (config now says 2026.10.1)
second apply: outcome=already_current             (update silently lost)
```

Note this root cause is in Updater spec §9.2 (step 12a before 13) and conflicts
with Launcher spec §19.3 step 7. Fix: make the launcher replacement part of the
transaction (stage it, swap it after the game tree, roll the config back on
`swap_failed`), or stop deriving the installed release from a config file the
updater itself rewrites.

### P0-3. `check` passes what `build` rejects, and the packager can ship an unrunnable game `[repro]`

`README`/`main.go` present `kobra-pack check` as the pre-commit gate, and
`pack.go:347-349` claims "both `check` and `build` go through here, so a gate
cannot exist in one and not the other". It can:

```
$ touch 'testgame/game/engine/a|b.js'
$ kobra-pack check ... -> exit 0
$ kobra-pack build ... -> exit 1  '/files/6/path' does not validate ... pattern
```

`stageRelease` validates only the asset manifest and launcher config
(`pack.go:415-431`); the whole-tree path pattern is enforced only in `Run`
(`pack.go:237-254`).

Separately, nothing requires `game/index.html` to exist —
`CheckServable` (`collect.go:183-215`) only inspects files that are present:

```
$ mv testgame/game/index.html /tmp/
$ kobra-pack check -> exit 0 ; kobra-pack build -> exit 0 ; zip contains no index.html
```

The launcher then rejects that install at `paths.go:150-152` with "The game files
are missing." Packaging spec §5.4/§11.2 requires the gate. Fix: `Check` must build
the file index, run the release-manifest schema gate, and assert the required root
files — without archiving.

### P0-4. Updates are impossible when the sidecar is on another volume `[repro]`

Staging lives under the sidecar (`<sidecar>/update/game.new`, `swap.go:377-391`),
which is machine-local (`XDG_STATE_HOME`, `%LOCALAPPDATA%`), while the game folder
is wherever the user put it. `Swap` uses bare `os.Rename` for both moves
(`swap.go:213`, `:219`) with no `EXDEV` fallback — confirmed
`invalid cross-device link` between a tmpfs and the workspace filesystem.

R10.4 anticipates exactly this layout, and the rest of the codebase treats it as
first-class: `storage.writeAtomic` has an `isEXDEV` branch and
`renameCrossDevice` (`storage.go:365-374`), and `sidecar.WriteAtomic` has its own
(`sidecar/atomic.go:60-67`). The updater — the one place the two roots are
guaranteed to be independently located — is the one that forgot. The rollback
works, so the folder survives, but every attempt costs a download and an
extraction.

### P0-5. The download stall watchdog is never armed in production `[repro]`

`download.go:98-100` documents `StallTimeout`: "Zero means `dlStallTimeout`".
`stall.go:40-44` returns immediately when `timeout <= 0`. Nothing defaults it:
`download.go:234` passes `opts.StallTimeout`; `apply.go:309` forwards it;
`main.go:228` passes `ApplyOptions{LauncherVersion: version}` → zero.
`dlStallTimeout` (`download.go:47`) is referenced nowhere.

A black-holed connection is therefore bounded only by `downloadTimeout(size)` =
`max(5 min, size/64 KiB/s)` (`download.go:195-203`) — hours for a multi-GiB
archive, blocking startup. The existing test passes an explicit 200 ms timeout,
which is why the gap is invisible. Fix: default it in `newStallGuard` or at the
`DownloadArchive` boundary, and add a test that omits the option.

---

## P1 — correctness and spec conformance

### P1-1. Exit code 4 is unreachable; the panic path exits 0 `[read]`
`main.go:43` declares `exitPanic = 4`; it is referenced nowhere. Launcher spec
§22.3 (line 1780) requires "initiates a drain and exits 4" and §26.4 lists a test
asserting it. `recovery` calls `Shutdown("panic")` (`server.go:498-515`), `Serve`
returns `nil` for a shutdown reason (`server.go:405-418`), `run` returns `exitOK`
(`main.go:452-457`). The debug-build re-panic is also absent. No test asserts 4.

### P1-2. Security headers are missing from gate rejections `[read]`
`securityHeaders` is *inside* both gates: `recovery(logging(hostGate(originGate(securityHeaders(mux)))))`
(`server.go:330-339`), so their early returns never reach it. Spec §13.7 and the
comment at `server.go:557` both say "every response, including errors". A bad Host
gets 421 and a bad Origin 403 with no `nosniff`/CSP/XFO. Impact is low (fixed
bodies, no reflection) but the stated invariant is false.

### P1-3. `storage.isWindowsDeviceName` accepts `con.txt` — a duplicated predicate that is already wrong `[repro]`
The storage copy (`storage.go:1252-1267`) trims one trailing dot and then
exact-matches the whole string, so `con.txt`, `nul.json`, `aux.bak`, `prn.dat`
pass. The static copy (`static.go:234-256`) strips at the *first* dot and rejects
them. Windows-only impact, but severe: a save slot `con` writes `con.json`, which
`CreateFile` resolves to the CON device, and `nul` silently discards the write —
silent save loss. This is the concrete cost of the duplication in P2-1: one copy
is wrong and nothing compares them.

### P1-4. The mod confinement re-check does not confine to `data/mods` `[reported]`
`serve.go:44-46` confines mod assets to `filepath.Dir(h.root.DataDir)` — the whole
game folder, including `data/saves` and `data/config`. With a provider that
returns `<game>/data/saves/slot1.json`, `GET /mods/hd/assets/anything.png`
returned 200 with the save bytes. Today the real provider
(`storage.ModAssetPath`, `mods.go:223-237`) is airtight by construction, so this
is a defence-in-depth failure rather than a live escape — but the static layer has
no independent notion of "mods only", and `static_test.go:19-21` uses exactly the
weak provider that would turn it into a disclosure. Fix: pass `<DataDir>/mods` as
the confine root.

### P1-5. A durable write is reported as a failure when the context is cancelled `[read]`
`writeAtomic` ends with `return ctx.Err()` (`storage.go:379`) *after* the rename
and directory fsync succeeded. Callers convert that into a 500
(`storage.go:617-623`, `:1064-1065`) for a write that is already durable. For
saves, the early return also skips `setRevision` (`storage.go:629`), so the
in-memory revision lags the file's own `revision` field; the next write reuses the
same revision number and a client's `if_revision` that was already superseded
still matches. Either treat post-rename cancellation as success or stop
cancelling there — do not report a durable write as failed.

### P1-6. A failed config write can leave `settings.json` missing with no recovery `[read]`
`writeAtomic` rotates `target` to `.bak` (`storage.go:357-364`) and then renames
the temp into place; if that rename fails, the live file is gone and only `.bak`
remains. `ReadSave` handles this (`storage.go:776-787`); `ReadConfig` does not
(`storage.go:1023-1027`) and reports a missing file as `{}`, and `MergeConfig`
treats a missing file as an empty document (`storage.go:1051-1055`). Saves survive
a torn write; settings silently reset.

### P1-7. Extracted files are never fsynced before the tree is promoted `[read]`
`extract.go:143-156` opens each member with `O_TRUNC`, copies, and closes — no
`f.Sync()`. `Swap` then renames the tree into place and the apply records success.
On a power loss after the rename, the directory entry can be durable while file
contents are not, so zero-length files get installed and reported as a successful
update. Launcher spec §15.3 step 6 calls the analogous directory fsync
non-optional, and the updater's own single-file writer (`runtime.go:104`) does
fsync. This is the same class of bug the spec's fault-injection scenarios exist to
catch.

### P1-8. `update.state` is not directory-fsynced, and its removal cannot fail loudly `[read]`
`writeState` fsyncs the file but not its directory (`swap.go:95-124`), unlike
`writeFileAtomic` (`runtime.go:116`). `removeState` ignores the error
(`swap.go:141-144`). If the unlink is lost while the renames survive, the next
start takes the `gameOK && oldOK && statePresent` branch (`swap.go:299-328`) and
**silently rolls back a completed install** — a crash after a successful swap
undoes the update. Choosing rollback on ambiguity is defensible; ignoring the
unlink failure that creates the ambiguity is not.

### P1-9. `BackupRelease` names the wrong release, so the revert offer is wrong or absent `[read]`
`retention.go:48-54` returns `update.state`'s `release`, which per its own
documentation (`swap.go:82-90`) is *the release being installed*, not the backup —
and the marker is deleted after every successful swap (`swap.go:229`) and by
`Recover`. So in the normal steady state it returns `""` and
`server/update.go:149-156` never offers the revert R15.11 says MUST be exposed;
when a marker exists it names the release already installed. The true value is in
`game.old/release.manifest.json`.

### P1-10. `result.json` omits the byte counts R10.6 requires `[read]`
R10.6 (spec line 749): "When the preflight fails … `result.json` MUST carry the
required and available byte counts so the UI can [explain]". `Result`
(`progress.go:49-59`) has no such fields, and `diskSpaceError` (`runtime.go:324`)
is dead code. The UI cannot render E-code numbers.

### P1-11. The config merge ignores most of the R14.2 owner table `[read]`
R14.2 (spec line 957) marks `game_id`, `game_name`, `mime_types`, `locales` and
`schema` as **release-owned** ("Release wins on conflict").
`releaseOwnedKeys` (`configmerge.go:48-50`) contains only `release`, so a local
`game_name`/`game_id`/`mime_types` beats the release's. The merge machinery is
right; the table is incomplete.

### P1-12. Three config knobs are parsed, defaulted and ignored `[read]`
Publishers can set these and observe nothing:

- `server.heartbeat_interval_seconds` — schema `:87`, defaulted and clamped in
  `config.go:128`, `:232-236`; `ServerConfig.HeartbeatInterval()` (`:50-52`) has no
  caller. The server ticks at a hardcoded 1 s (`server.go:424`) and the shell
  heartbeats at a hardcoded 5 s (`game/shell.js:12`).
- `server.probe_path` — the path is hardcoded in three places
  (`port/probe.go:17`, `dataapi/handlers.go:75`, `sidecar/health.go:46`).
- plus `ProbePath`, `GameName`, `Locales`, `AllowOutOfRange`,
  `UpdateCheckUserInitiatedOnly`, `RetainPreviousRelease`, `AllowDataDirOverride`
  are never read, and `ExposeFilesystemPaths` exists only to be rejected when true
  (`config.go:247`).

### P1-13. Five port events are defined and never emitted `[read]`
`port/events.go` defines seven loggers; only `LogCandidate` and `LogConfirmed` are
called. `LogProbe` (:35), `LogBindFailed` (:52), `LogScan` (:65), `LogDenyMismatch`
(:75) and `LogChange` (:84) have no call sites — yet `port.probe`,
`port.bind.failed`, `port.scan`, `port.deny.mismatch` and `port.change` are all
named in the spec's event catalogue. These are missing observability for exactly
the diagnostics needed when a port is squatted, not just dead helpers.

### P1-14. The connection watchdog is log-only, but specified as user-facing `[read]`
§20.5 (spec line 1661) requires surfacing E22 with the log path and a
copy-diagnostics action. `server.go:436-444` writes `browser.watchdog.timeout` and
does nothing else. The user is never told the game opened but cannot reach the
launcher.

### P1-15. False invariants in comments and docs `[read]`
These read as verified facts and are the biggest hazard for the next reader or
agent:

- `extract.go:318-326` removes the destination and *then* renames on Windows; if
  the second rename fails the live launcher file is gone, though the comment
  claims the destination is never missing.
- R5.1's "the archive is renamed only after Verify" never happens: the target is
  already `download.part` (`runtime.go:35`) and nothing renames it, while
  `download.go:14-16`, `:119-120` and `runtime.go:30-32` assert that it does.
- `manifest.go:16-24` claims the schema has no archive hash and that `Verify`
  falls back to the per-file index; the schema makes `archive.hash`/`size`
  required, and `verify.go:137-141` refuses instead.
- `main.go:516` says `firstLine` exists "for the OS dialog"; no dialog path exists
  (`grep -i messagebox|dialog` finds only comments).
- `storage.errnoName` (`:1270-1276`) documents "without leaking a path" but falls
  through to `fmt.Sprintf("%v", err)`, which renders both paths for
  `*os.LinkError`/`*os.SyscallError` into a log field.
- `lock.go:252` documents `lock_other.go`; the file does not exist.
- `kobraerr.go:1-2` claims to be "the single error taxonomy shared by every
  subsystem", but it is used only at the HTTP-envelope layer
  (server/dataapi/static/storage); port, sidecar, browser and paths use plain
  errors and sentinels. Defensible design, inaccurate doc.

### P1-16. `make race` is a dead gate, `verify` is weaker than it reads, and an unguarded platform claim
`packaging/Makefile:23` sets `CGO_ENABLED=0`, so `make race` fails with
`-race requires cgo` — the Makefile advertises a gate that cannot run.
`kobra-pack verify` (`archive.go:293-351`) never checks that a manifest's
`files[].hash` matches the archived bytes (a tampered `index.html` inside the ZIP
with regenerated `SHA256SUMS` passes), and accepts a dist with both
`*.release.manifest.json` files deleted. And the documented platform matrix
(cross-builds clean for the six targets) excludes illumos/solaris/plan9, where
`internal/sidecar` does not build because `lock_other.go` is missing — only
relevant if those platforms are in scope, but the code claims to support them.

---

## P2 — duplication, dead code, hygiene

### P2-1. The durability primitive exists five or six times, and has drifted
| Implementation | Directory fsync | Temp naming |
|---|---|---|
| `sidecar/atomic.go:22` `WriteAtomic` | yes (:69) | `O_EXCL` + pid + counter |
| `storage/storage.go:320` `writeAtomic` | yes (:376) | `O_EXCL` + pid + seq |
| `update/runtime.go:82` | yes (:155) | — |
| `update/configmerge.go:248` | — | — |
| `update/swap.go:95` `writeState` | **no** | fixed name, `O_TRUNC` |
| `update/extract.go:292` | yes | `os.CreateTemp` |

`sidecar/atomic.go` is a near-verbatim copy of `storage.writeAtomic` (~70 lines
each), justified in its header as "implemented here as well as in the storage
engine because port.json is written before the engine exists" (`:15-16`). That
justification does not hold — neither package imports the other, so a small leaf
package is available to both. The copies have **already diverged** (storage
branches on `isEXDEV` first, sidecar does not) and `swap.go`'s copy is missing the
directory fsync (P1-8). The `os.CreateTemp` variant in `extract.go` is the
safest of the six, which is itself the argument for one implementation. This is
the highest-leverage cleanup in the repo.

### P2-2. Two `isWindowsDeviceName` implementations with different semantics
`storage.go:1252-1267` vs `static.go:234-256` — see P1-3, where the divergence is
already a bug. `writeError` is likewise duplicated three times
(`server.go:600`, `static.go:222`, `dataapi/service.go:143`), and the data-api copy
sets `Retry-After` while the static one silently does not.

### P2-3. Schema copies: five locations, one guarded
All copies are currently byte-identical (verified by md5). `packaging/internal/pack/schemas/`
is embedded and guarded by `TestEmbeddedSchemasMatchArchitecture`
(`pack_test.go:420`) — keep it. `launcher/internal/config/schema/` and
`launcher/internal/server/testschema/` are embedded and needed. But
`launcher/internal/dataapi/schema/data-api.schema.json` is **referenced by
nothing**, and `launcher/schemas/` (13 files, 76 KB) is read by no code, Makefile
or script and has no drift test. Delete both, or wire them to a documented purpose
and copy packaging's drift test.

### P2-4. Dead exported surface
Zero references outside their own file/package (verified by scan):
`diagnostics.SetLevel`, `diagnostics.RingLines`, `storage.ConfigRevision`,
`storage.ModsManifestPath`, `update.ShouldApply`, `update.diskSpaceError`,
`update.formatBytes`, `update.clearResult`, `update.dirWritable` (a copy of
`paths.dirWritable`), `port.Log*` (P1-13), `port.SourceUser`,
`static.ModAssetProvider.ReadLock`, `browser.ErrNoBrowser`/`ErrTooOld`
(both collapsed to `("","")` at `main.go:789-792`), `browser.Candidate.Major`,
`browser.LaunchError.Cause`, `paths.ErrDataDirMissing`, `server.Started`,
`server.Log`, `server.Port` (tests only), `progressWriter.lastPhase` (written,
never read), plus in packaging `ReadLauncherConfig`, `MarshalCanonical`,
`SchemaBase`, `EvSchemaMismatch`, `SchemaModManifest`, `SchemaSave`,
`IdentityInput.Schema`, `stagedRelease.launcher`. Individually trivial;
collectively this is the answer to "did I over-engineer?" — and each one is a
place a future agent will waste a cycle deciding whether it is load-bearing.

### P2-5. Duplicated logic that is *not* justified
`spacePath` is byte-identical in `space_unix.go:37` and `space_windows.go:32`
(verified by diff) yet has no platform dependency — the build-tag split is
pointless there. `hashFile` (`verify.go:263`) ≈ `pack.HashFile`
(`packaging/archive.go:45`), and `sha256:` is re-parsed ad hoc in
`pack.VerifyOutput` (`:307`). `sidecar/health.go:24-39` `probeClient` is
near-identical to `port/probe.go:77-97` `newProbeClient`. `paths/gameid.go:14`
duplicates the schema's game-id pattern. The copied reserved-name rule table
(`pack/collect.go:28-35` vs `update/archive.go:50-56`) is currently identical and
would silently drift.

### P2-6. Duplication that *is* justified (leave it alone)
The three archive/extract implementations (`packaging/archive.go`,
`update/extract.go`, `storage/archive.go`) sit on different trust boundaries in
two Go modules that cannot import each other. Keep. The schema copies that are
embedded and drift-tested are also correct practice. `Update`'s
`SignatureVerifier` seam (`verify.go:55-70`) has no implementation today but R13.4
mandates the seam — keep.

### P2-7. Smaller smells
- `sidecar/lock_unix.go:32` — `var _ = syscall.Getpid` exists only to keep an
  otherwise unused `syscall` import. Delete both.
- `browser/browser.go:108` `var plan searchPlan` is package-level mutable state
  populated from four `init()` functions; platform behaviour discovered in
  `init()` is invisible to a reader and awkward to test. A function returning the
  plan per `GOOS` is clearer.
- `main.go:718` `var pendingRecovery string` passes one value between two phases
  of `run()` via hidden global state, making `run()` non-reentrant.
- `main.go:761-764` logs `"stale_pid": 0`, a placeholder that reads as real data;
  `runRepair` (`:289-292`) and `persistPortState` (`:389-391`) discard the
  underlying error, so the log says a write failed but not why.
- `parsePort` uses `fmt.Sscanf(s, "%d")` (`main.go:126-135`), so `--port 8080abc`
  is accepted as 8080.
- `confirmPort` (`main.go:608-647`) silently keeps the proposed port on invalid
  input instead of re-prompting, and leaks a goroutine blocked in `fmt.Scanln` on
  the timeout path.
- `server.go:135-136` hardcodes `Inactivity: 30m` and `MaxSessions: 16` while
  other TTLs come from config. Pick one policy.
- `sidecar.Load:69`'s `st.Port > 65535` is unreachable (`Port` is `uint16`), and
  its `< 1024` bound duplicates `port.Validate`.
- `pipeline.go:194/204` `decodeJSON` reads ≤`max` then does `string(raw)` +
  `strings.NewReader`: a second copy of up to 64 MiB per request, ~4 GB transient
  at 32 concurrent writers. `bytes.NewReader` avoids it.
- `browser/browser.go:289-306` and `port/validate.go` carry more comment lines
  than code; `static.go` re-checks NUL/backslash/device-name that `resolveUnder`
  and `server.normalisePath` already rejected — three layers for one predicate.
- `Result.Outcome` is tagged `release` (`progress.go:51`) while the wire type uses
  `outcome` (`apitypes.go:112`). Spec-mandated (R11.5), but worth a one-line
  comment at both sites.
- `.e2e/run.sh:8` hardcodes the absolute checkout path, so the `smoke` target only
  works here; `testgame/run.sh` already derives it portably. It also exports
  `GOFLAGS=-mod=mod`, so it never exercises the vendored build that ships. The two
  e2e harnesses duplicate the `check`/`checkcontains` helpers and the whole HTTP
  drive; a shared `lib.sh` would pay for itself given the drive is the most
  valuable asset here.
- `packaging/Makefile:56` `fmt` returns 0 even when `tee /dev/stderr` fails.

---

### P2-8. Latent and low-severity issues (recorded so they are not lost)

- **`port.Scan` has no overall deadline** (`scan.go:65-77`): up to 47,127 ports are
  probed with a per-port timeout, called synchronously from `available`
  (`bind.go:81`). Fast `ECONNREFUSED` in the normal case, but a filtering firewall
  turns this into a multi-hour startup hang. No test bounds it.
- **Symlink TOCTOU in static serving**: `statConfined` resolves symlinks and
  confines (`serve.go:63`), but `:99`/`:156` reopen the *unresolved* `fullPath`, so
  a symlink swapped in that window escapes. Requires write access inside the served
  tree. Same class in the updater's staging root: `paths.Confine` is purely lexical
  (`paths.go:274`), so a planted symlinked parent is followed by `MkdirAll`/
  `OpenFile` (`extract.go:140-143`, `:255-262`).
- **Static serving has no per-connection budget**: `WriteTimeout: 0` by design
  plus a 32-slot concurrency limit (`server.go:385`, `:571-581`) means a slow client
  can occupy a slot indefinitely. A cross-origin GET **without** an `Origin` header
  is accepted (browsers omit it on plain `<img>`/`<script>` loads), so a hostile
  page can drive static serving — unreadable to it (no CORS), but able to consume
  slots. Slot exhaustion was not demonstrated, so this stays speculative.
- **ETag/Last-Modified inconsistency**: `serve.go:79-80` pairs a session-cached
  ETag with a freshly read `ModTime`, so an in-place edit mid-session yields a 304
  for changed bytes. §13.6 sanctions the cached ETag; the pairing is internally
  inconsistent.
- **A complete `.part` is never resumed** (`download.go:158` requires
  `BytesWritten < size`), contradicting the comment at `:434-436`.
- **R13.6 (refuse plain-http remotes) is enforced only for the archive**:
  `latest.json` and manifests may come over remote plain HTTP (`http.go:54`) and
  are refused only later, at download (`download.go:527`).
- **Cancellation semantics are coarser than they look**: any parent-context
  cancellation is reported as a *user* cancel and clears the marker
  (`download.go:506-513`, `apply.go:317-330`). Unreachable today because
  `main.go:225` uses `context.Background()`, but it would mislabel a timeout.
  Relatedly, `_ = cause` (`apply.go:542`) discards the reason on every retryable
  failure, and `_ = err` (`apply.go:393`) is a no-op.
- **`Tail`/`tailLocked`/`RingLines`**: the same ring arithmetic appears three times
  (`diagnostics.go:295`, `:310`, `:316`), and `RingLines` has no callers.

---

## Answers to your three questions

### "Did I over-engineer?"

Mostly no. There are no single-implementation interfaces, no DI ceremony, no
generic frameworks; `Options`/`Result`/`prepared` are all used, and `packaging` is
genuinely lean. Two real cases:

1. **Machinery that is defined but never armed.** The stall watchdog (P0-5), five
   port loggers (P1-13), three config knobs (P1-12), `exitPanic` (P1-1), the
   `kobra_faultinject` build tag (specified in §26.4, present nowhere), four
   update knobs nobody sets outside tests. Each is a documented guarantee that
   does not run — worse than an absent feature, because the specs and comments
   assert it works. Deleting a knob from the schema beats keeping a dead default
   and a clamp for it.
2. **Spec surface larger than the product.** ~6.9k lines of spec for 28k LOC, with
   §-citations in most comments. That traceability is a real asset for humans and
   agents *while it is true*; P1-1..P1-16 are where the spec has outrun the code.
   A cheap periodic check: every event name and config key cited in the spec
   should have a call site and a reader, or it should be deleted.

### "Is the style clear enough for an AI agent or a human?"

Yes — this is the strongest part of the codebase. Names are explicit, package docs
state invariants, error messages are written for users, and comments explain *why*
(the `writeAtomic` ordering note, the `Swap` recovery matrix, the
`pruneRevisions` "live revision is never evicted" argument) rather than restating
code. gofmt/vet clean, ~18% comment lines, spec anchors on exported symbols.

What impedes navigation:

- **Three very long functions**: `update.Apply` (417 lines, `apply.go:81-498`,
  with eight near-identical "clear marker → log → `WriteResult` → return" blocks
  that one `fail(...)` helper would collapse), `main.run` (317 lines,
  `main.go:141-458`), and `pack.Pipeline.Run` (151 lines). All are linear pipelines
  with numbered step comments, so they are followable — but at that length a
  reviewer cannot see a missing `return` and an agent cannot hold the state.
  `Apply`'s steps 8-11 and `run`'s port/storage/server block are clean seams.
- **One 1307-line file**, `storage/storage.go`, holds save CRUD, config merge,
  revision management, trash cleanup, a hand-rolled canonical JSON encoder,
  identifier validation and errno mapping. The rest of the repo is deliberately
  small single-purpose files; this is the exception and the hardest file to review.
- **Comment rot**: P1-15's false invariants read as verified facts. In a
  spec-driven codebase, a stale comment is a correctness hazard, not cosmetics.

### "Did I duplicate code? Is it justified?"

Yes, in three places — two unjustified:

1. **Unjustified: the atomic-write primitive (P2-1).** Six implementations, two
   near-identical, already diverged, covering the most safety-critical operation
   in the product, with the updater's copy missing a directory fsync. Extract one
   package.
2. **Unjustified: the duplicated predicates (P2-2).** `isWindowsDeviceName` ×2
   (already wrong in one — P1-3), `writeError` ×3 (already diverged on
   `Retry-After`), `spacePath` ×2 identical with no platform dependency.
3. **Justified: schema copies that are embedded and drift-tested (P2-3)**, and the
   three archive/extract implementations on different trust boundaries in
   separate modules (P2-6). Keep those. Delete the two unguarded/unused launcher
   copies.

---

## Test gaps worth closing

- **Nothing asserts `check` and `build` agree on the same tree** — that is how
  P0-3 shipped, despite `pack.go` asserting the invariant in a comment.
- **No log-rotation test** — that is how P0-1 shipped, and it hangs the product.
- **No test executes the disk preflight.** `apply_test.go:873` and
  `configmerge_test.go:443` re-implement `!(free < required)` inside the test and
  never call `Apply`; the fixture always sets `SkipDownloadCheckForTest`. R10.1/
  R10.2/R10.6 are therefore unverified, which hides P1-10.
- **No test for a failed `Swap`** leaving `launcher/` intact or reporting the true
  installed release (P0-2), and none with a cross-filesystem staging root (P0-4) —
  the fixture puts both under one `t.TempDir()`.
- **No test uses the default stall timeout** (P0-5).
- **`internal/server/update.go` has zero tests** (verified: no test file
  references `UpdateCheck`, `UpdateProgress`, `CancelUpdateIntent`,
  `RecordUpdateIntent`), and **`internal/dataapi` has no package tests** (covered
  only indirectly by `contract_test.go`, which is genuinely good).
- `kobra-pack verify` never compares a manifest's `files[].hash` to the archived
  bytes (P1-16).
- No test asserts exit code 4 (P1-1) or the E22 surface (P1-14); nothing bounds
  `port.Scan`'s 47k-probe worst case; no fuzz targets for `parseRange`/`routePath`/
  `resolveUnder`.
- `TestPipelineIsDeterministic` (`pack_test.go:281`) re-runs in one process, so it
  cannot catch Go map-iteration nondeterminism; only `testgame make verify` (two
  processes) tests the real claim.
- The `kobra_faultinject` scenarios of §26.4 (crash between fsync and rename,
  EXDEV, disk-full, panic exit 4) are specified and absent. Several findings above
  are precisely the class of bug those injection points exist to catch.
- `.e2e/run.sh` (15 checks) and `testgame/run.sh` (40 checks) give genuinely good
  e2e coverage: Host/Origin gates, CSRF, traversal, revision conflict, drain. The
  gap is process, not assertions — **there is no CI configuration in the repo at
  all**, and no git history (the tree is untracked). `make check` exists per
  module; nothing runs it.

---

## What is genuinely well done (keep this)

1. **Reproducibility is real, not claimed.** `testgame make verify` gives
   byte-identical archives across two runs; the implementation sorts every map,
   takes time from the release id rather than the clock, pins zstd to
   single-threaded best compression, and builds PAX headers with zero
   uid/gid/uname/gname. I ran it, and it also survived adversarial changes to
   umask, locale, cwd, `$HOME` and `TZ`.
2. **Traversal and confinement defence holds under attack.** Encoded,
   double-encoded, overlong-UTF-8, fullwidth-dot, `..;/`, backslash, NUL,
   `::$DATA`, trailing-dot/space, device-name and four symlink variants all 404;
   no directory listing; `data/` and `launcher/` unreachable. `normalisePath`
   deliberately 404s dot segments rather than letting `ServeMux` 307 them, closing
   the window before confinement runs. `statConfined` re-checks the symlink-resolved
   target. Worth recording explicitly: `server.normalisePath` is **load-bearing** —
   on a bare mux, `ServeMux` answers mirror-image paths such as
   `/engine/../../launcher/launcher.config.json` with a 307 pointing at
   `/launcher/launcher.config.json`, leaking path structure. Removing that
   middleware would reintroduce the redirect.
3. **Port allocation has no squatting race.** `Bind` returns an open,
   exclusively-held listener and `Available` hands that same listener to the
   caller (`bind.go:25-28`, `:59-61`, used at `main.go:559`); probing is explicitly
   advisory, `SO_REUSEADDR` only, never `SO_REUSEPORT`, and the Windows no-op
   reasons correctly about Windows' inverted `SO_REUSEADDR`.
4. **`Recover` is marker-independent** (`swap.go:260-365`): it reasons from which
   directories exist rather than trusting the marker's `step`, explicitly because
   the marker write can itself be interrupted, and its interpretation of ambiguous
   spec bullets is recorded in the comments.
5. **`configmerge`'s local-wins/release-wins merge** (`configmerge.go:1-40`) solves
   a real, subtle problem the fixture would otherwise hit: a naive swap reverts
   `update.channel` and kills the patch path.
6. **`browser.Launch` has no injection**: `exec.Command` with a real argv (no
   shell), absolute well-known paths probed before `$PATH` (`browser.go:200-223`),
   `HideWindow` on Windows, path-free error text enforced by a test.
7. **The §26.5 "no paths/usernames in error envelopes" test** is a real negative
   property test over every error constructor, and `kobraerr` is built so it
   cannot leak by accident (fixed messages, cause kept internal). The session/CSRF
   asymmetry (`pipeline.go:78-92`) is deliberate and tested.
8. **`data/` protection is enforced by construction *and* by an independent
   pre/post assertion** (`apply.go:640-696`), not decoratively; a hand-edited
   archive that touches `data/` is rejected twice.

---

## Suggested order of work

1. **P0-1** (log-rotation deadlock, one-line fix, hangs the product) — do this first.
2. **P0-2 + P0-4** share one fix: stage inside the game folder's filesystem *and*
   swap before merging `launcher/`. This also shrinks P1-15's blast radius.
3. **P0-3** (make `check` the same gate as `build`) and **P0-5** (default the stall
   timeout) — small, mechanical, high payoff, each with an obvious test.
4. **P1-3** (`con.txt` device names, silent save loss on Windows) and **P1-7/P1-8**
   (fsync before promotion) — write-path durability.
5. **P1-1, P1-10, P1-11, P1-12, P1-13, P1-14** — make the spec and the code agree,
   or delete the claim. Prefer deletion.
6. **P2-1** (one atomic-write package), then **P2-3** (schema copies) — the cleanup
   that stops this class of defect recurring.
7. **Add CI** that runs the `check` targets plus `testgame make e2e`, and put the
   tree under version control. Every finding above is cheaper to prevent than to
   find, and none of the existing gates currently runs automatically.
