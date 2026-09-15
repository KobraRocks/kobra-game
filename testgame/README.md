# testgame — a fixture game for the packager and the launcher

A minimal but *real* Kobra game, small enough to read in one sitting, used to
exercise two things that had no test subject before:

- **the packager** — `architecture/Packaging-spec.md` had no input tree to build
  from, so no packaging gate could be run end to end;
- **the launcher** — `.e2e/run.sh` fabricates a throwaway folder of stubs, which
  proves the server starts but not that a *packaged* game works.

Everything here is real. The WebAssembly core is a valid module that exports
`tick`, the PNG and Ogg are decodable files, the shell implements the boot
sequence of FS §10.1 against the live API, and `run.sh` asserts 79 properties of
the result. It is a fixture, not a demo: the point is that it fails loudly when
the pipeline or the launcher regresses.

```
testgame/
├── pkg.toml                     # the packager's declared input (§14.1)
├── game/                        # the shipped tree (FS §5.1)
│   ├── index.html               #   — no inline script, no inline style
│   ├── shell.js                 #   — the reference shell (ES module)
│   ├── engine/
│   │   ├── game.wasm            #   — valid, 55 bytes, exports memory + tick
│   │   ├── game.js              #   — glue; uses instantiateStreaming (FR-AST-2)
│   │   └── engine.manifest.json
│   ├── assets/
│   │   ├── shell.css            #   — under assets/ because the root serves
│   │   │                        #     only index.html and shell.js (see below)
│   │   ├── textures/checker.png #   — real 8×8 PNG
│   │   ├── audio/click.ogg      #   — real Vorbis
│   │   ├── models/box.obj
│   │   └── data/levels.json
│   └── locales/en.json
├── launcher/
│   └── launcher.config.json     # game-owned; the packager assembles the rest
├── releases/2026.10.1/          # a second release as an overlay, for updates
├── run.sh                       # the end-to-end drive
└── Makefile
```

## Development tree vs package

`testgame/` is a **development tree** (a `pkgroot`), not a game folder. It has no
launcher binary, no `data/` skeleton, and no generated manifests, because those
are build outputs. `kobra-pack` produces the packaged form under `dist/`.

```
make package     # packages both releases, then extracts release 1
make e2e         # packages and drives the whole contract (79 checks)
make verify      # packages twice and requires byte-identical archives
make launch      # packages, then serves release 1 in a browser
make clean
```

`make package` writes:

```
dist/
├── pkgtest-2026.09.1-linux-x64.zip            # the human-facing package
├── pkgtest-2026.10.1-linux-x64.zip
├── pkgtest-2026.09.1-linux-x64.tar.zst        # the launcher-facing archive
├── pkgtest-2026.10.1-linux-x64.tar.zst
├── pkgtest-2026.09.1.release.manifest.json    # published manifests
├── pkgtest-2026.10.1.release.manifest.json
├── latest.json
├── SHA256SUMS
└── install/Pkgtest/                           # release 1, extracted and runnable
```

## Identity

| Field | Value | Why this value |
|-------|-------|----------------|
| `game_id` | `com.kobra.pkgtest` | Must not collide with `.e2e`'s `com.kobra.testgame` (18765) or `dev/`'s `dev.kobra.localgame` (19700): the id keys the sidecar directory **and** the port hash |
| Deterministic port | **18915** | `18900 + fnv1a32("com.kobra.pkgtest") % 50`; the fixture pins `base`/`span` so the port is predictable in tests |
| `release` | `2026.09.1`, then `2026.10.1` | Calver, §4.2 |
| `game_version` | `1.0.0`, then `1.1.0` | Publisher-owned content axis, §4.4 |
| `engine_version` | `0.1.0` | **Must** match the launcher binary's compile-time constant, see finding 5 |
| `save_version` | `1` | Same reason |

`require_confirmation` is `false` in `launcher.config.json`. The schema permits
that "only for automation"; a fixture that talks to a test harness is exactly
that case. A real release would leave it `true`.

## What it deliberately exercises

- **CSP compliance without shortcuts.** `index.html` contains no `<script>` body
  and no `style=""` attribute; every handler is attached from `shell.js` and the
  CSS is a separate resource. Under `script-src 'self'` and `style-src 'self'`,
  an inline shortcut would not run.
- **The real boot sequence.** Token → `POST /__kobra/session` →
  `history.replaceState()` → `/api/state` → config → save index → newest save →
  `instantiateStreaming` → heartbeat → goodbye.
- **The real save protocol.** Optimistic `if_revision`, the `409 conflict`
  envelope with `current_revision`, config merge that preserves unmentioned
  keys, and the on-disk save shape (`kobra.save/1`, checksum, `.bak`, revision
  sidecars).
- **MIME discipline.** `.wasm` is fetched and instantiated in `node` by
  `run.sh`, so a wrong media type fails the suite rather than silently falling
  back to a buffer.
- **The per-game MIME table.** `.obj` is not in the launcher's defaults and would
  be served as an attachment; `launcher.config.json` maps it explicitly.
- **Determinism.** `make verify` packages twice and requires byte-identical
  archives (§5.2).

## Findings

Building the fixture against the real launcher and the real spec surfaced seven
things. They are the most valuable output of this directory, so they are listed
rather than buried. **The first four are fixed** — in the launcher, in the
packager, and in both specifications; **the last three are open**, being
behaviours worth knowing rather than defects.

### 1. The game root serves exactly two files — fixed

The launcher registers an explicit route allowlist
(`launcher/internal/static/static.go`):

```
/*  and  /index.html  ->  game/index.html
/shell.js            ->  game/shell.js
/engine/...          ->  game/engine/...
/assets/...          ->  game/assets/...
/locales/...         ->  game/locales/...
```

Everything else under `game/` is a `404`. This is not stated in the FS §5.1
tree, which lists only `index.html` and `shell.js` at the root without saying
the list is closed. The fixture hit it for real: `game/shell.css` was packaged,
hashed into the file index, and served as `404 application/json`, which left the
page unstyled with no error anywhere.

**Resolved as a convention:** a file that must be served over HTTP goes under
`game/assets/`. `game/release.manifest.json` is the one exemption — the launcher
reads it from disk for the update check and never serves it. `kobra-pack`
enforces the convention by failing the build on any unservable path under
`game/`.

### 2. `release.manifest.json` is never inside the archive it indexes — fixed

Packaging spec §6.4 has the manifest index every shipped file *"except itself"*,
and the launcher rejects any archive member its index does not list
(`internal/update/verify.go`). Those two rules are compatible only if the
manifest is **not in the archive**, because an index cannot contain the input to
its own hash.

**Resolved by model M1** (implemented in `packaging/internal/pack`; see Packaging
spec §1.2, §6.4, §9.1): the release manifest is a *published* document. The update archive omits
it exactly as it omits `data/`, `README.txt` and `LICENSES/`; the distribution
ZIP carries it for support inspection.

The launcher now says so in as many words. A member named
`game/release.manifest.json` fails with the reason
`release_manifest_in_archive` and the message *"The update was refused because
the release manifest was packaged inside the update archive."* — not the generic
`unsafe_entry` that used to make a publisher guess.

Nothing breaks when the file is absent from an updated install: §19.2's
`current` lookup falls back to `launcher.config.json`'s `release`.

### 3. `archive.hash` has a fixed point again — fixed

§8.2.2 requires the manifest to declare the archive's hash; §6.1 put the
manifest inside the archive. The archive's hash depends on the manifest, and the
manifest was in the archive, so the two could not both hold.

**Resolved by model M2:** the build writes the manifest *after* the update
archive exists, and the manifest never enters it. `archive.hash` therefore
describes bytes that do not contain the document describing them. The ZIP copy
and the published copy are byte-identical, so one signature covers both.

The launcher's half of this was a silent downgrade: a manifest declaring no
archive hash fell back to per-file verification, which is the behaviour §8.2.1
explicitly rejects. `Verify` now fails closed with the reason
`archive_hash_missing` and a message that blames the release rather than the
user's download. The packaging pipeline catches it first
(`kobra-pack`'s `verify_output`).

### 4. `sendBeacon` could not deliver the goodbye, and bearer writes could not pass CSRF — fixed

`internal/session.CheckCSRF` requires a **cookie value, a header value, and the
session's own token** to be pairwise equal:

```go
ok := sess != nil && cookieValue != "" && headerValue != "" &&
      ConstantTimeCompare(cookieValue, headerValue) == 1 &&
      ConstantTimeCompare(cookieValue, sess.CSRF) == 1
```

Two things followed from that, and both are now fixed:

- **The goodbye was unreachable.** `navigator.sendBeacon()` cannot set a request
  header, so FR-SRV-19's specified goodbye always answered `403` and the idle
  timeout did all the work. FR-SRV-19 now specifies
  `fetch(..., {keepalive: true})`, which has the same unload delivery guarantee
  and *can* set the header. `run.sh` asserts both the `403` and the working
  `204`, so the distinction stays pinned.
- **The bearer path could read but never write.** `checkSession` accepted
  `Authorization: Bearer`, and the CSRF gate then demanded a cookie that a
  non-browser client does not have. CSRF now applies **only to
  cookie-authenticated requests** (FR-SRV-6a, clarified in FS v2.2): the
  double-submit check defends an *ambient* credential, and a bearer id is
  attached explicitly by a caller that already holds it. Covered by
  `TestBearerSessionWritesWithoutCSRF`, which also asserts that the same write
  through the cookie with the header removed is still `403`.

### 5. `engine_version` and `save_version` are compile-time constants — open

`/api/state` reports `engine_version` and `save_version` from `var` declarations
in `cmd/kobra-launcher/main.go`, not from `game/engine/engine.manifest.json`. The
launcher never reads the engine manifest. A package whose engine manifest
disagrees with the binary is therefore **silently** inconsistent: the file index
and the engine's own metadata say one thing, and every save header written says
another. `run.sh` asserts the two agree, which is the only place that check
exists today.

### 6. `scope: game-only` is inconsistent with §4.6 — open

§4.6 requires `launcher/launcher.config.json`'s `release` to equal the package's
release id, and `/api/state.release` is served from that config. So every release
that advances `release` also changes `launcher/`, which makes `game-only`
("never `launcher/`", §9.2) contradictory for any release that bumps the id. The
fixture uses `scope: full` for both releases and renders the config per release
rather than copying it verbatim (deviation D1).

### 7. A non-content-addressed stylesheet is cached for an hour — open

§6.5.2 documents that `/assets/...` without a 16-hex-character prefix is served
`public, max-age=3600`. `game/assets/shell.css` is exactly that, so a stylesheet
change can take up to an hour to reach a returning player. Setting
`content_addressed: true` in the packaging build — and rewriting the
`<link href>` to the hashed path — is the fix. The fixture leaves it off so the
cache policy is visible, and `run.sh` asserts the header.

### Also worth knowing: the fixture is stricter than the launcher

The served policy is:

```
default-src 'none'; script-src 'self' 'wasm-unsafe-eval';
style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; ...
```

FS §7.4 specifies `style-src 'self'` with no `'unsafe-inline'`, and
`default-src 'self'` rather than `'none'`. The launcher is more permissive than
the spec on styles. The fixture does not use the allowance, so it keeps working
if the launcher is tightened to match the FS.

## The packager is now real

This fixture was originally driven by a throwaway Python packager, because
`packaging/` did not exist. It now drives **`kobra-pack`** — the real Go tool in
[`packaging/`](../packaging/README.md) — and the Python script is gone.

That makes the fixture what it was meant to be: a conformance suite. `run.sh`
asserts on the tool's output, and the tool's own tests in
`packaging/internal/pack/pack_test.go` build a hermetic pkgroot in a temp
directory so the pipeline can be tested without this fixture at all.

What `kobra-pack` enforces on this tree, and therefore what this fixture proves:

- §4.6 cross-document identity, including `launcher_min ≤ launcher version`,
  `save_version` agreement, and `.wasm → application/wasm`
- §2.4 forbidden content: symlinks, saves under `data/`, device names, VCS and
  editor metadata, trailing dots or spaces
- §2.5 `data/.keep` empty and `data/{saves,config,mods}/` present as real entries
- §6.4 file index completeness, and that every archive member is indexed
- §9.1 no `data/`, `README.txt`, `LICENSES/` or release manifest in the update
  archive
- §5.2 determinism, verified by rebuilding
- servability of every path under `game/` (finding 1)

```
make check      # every gate, writes nothing
make package    # dist/ + dist/install/
make verify     # two builds, byte-identical
make dev        # edit-and-run loop
make e2e        # the full drive
```

## Not covered

- **No signing.** `pkg.toml`'s `gpg_key` is empty, so releases are hash-only and
  `pack.sign.skipped` is the honest outcome (§7.4). The launcher fails closed on
  a declared-but-unverifiable signature, so a signed fixture would need a
  verifier installed via `update.SetSignatureVerifier`.
- **The assisted patch is applied, but not watched.** `run.sh` phase 14 drives
  `/api/update/check` against a real `dist/` served over loopback; phase 15 then
  POSTs `/api/update/apply`, restarts the launcher, and asserts the swap that
  happens at boot: the new release's level file is served, `data/` is
  byte-identical, `game.old/` holds the previous release, the local
  `update.channel` survived, the staging trees are gone, and no download is
  attempted without a marker. What is *not* exercised is the in-browser
  experience: no browser connects under `--print-url`, so the retention counter
  never advances and the confirmation/progress UI does not exist yet. See
  Updater spec §19.1 for that deferral.
- **Rollback retention at two starts is unit-tested, not driven.** Phase 15
  asserts that nothing is retired without a heartbeat; the second-heartbeat
  retirement is covered by `internal/update/retention` tests, which can raise a
  heartbeat directly.
- **Windows and macOS are not run.** Linux only, matching the launcher's own
  status.
