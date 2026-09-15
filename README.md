# Kobra Games

A launcher, a packaging toolchain and the specifications that hold them together,
for shipping web-technology games as ordinary folders a player can extract and
run.

A Kobra game is a folder containing an HTTP-served game (`game/`), the launcher
that serves it (`launcher/`), and the player's data (`data/`). The launcher binds
a deterministic loopback port derived from the game's id, serves the game to the
player's own browser, and brokers save files, configuration and updates over a
small local data API. Nothing is installed, no service runs, no data leaves the
machine.

```text
GameFolder/
├── launcher/     launcher binary, launcher.config.json, port deny list
├── game/         index.html, shell.js, engine/, assets/, locales/
└── data/         saves/, config/   (the player's, backed up and never deleted)
```

## Status

The launcher, the packager and the updater are implemented and tested. What is
verified on every commit is listed under [Repository gates](#repository-gates);
`CODE-REVIEW.md` records an adversarial review of all three, the fixes applied to
it, and what remains.

Known gaps, stated plainly:

- **Windows and macOS are compile-verified, never executed.** The build matrix
  produces binaries for both, and the platform-specific code (locking, volume
  space, browser detection, self-replacement) has no test that has run on those
  systems. Treat a Windows or macOS package as untested until someone runs one.
- **The launcher replaces itself in place.** On Windows a running image can be
  renamed but not overwritten; the code moves the old file aside rather than
  removing it, which is the behaviour that should make this work, but it has not
  been observed on a real Windows machine.
- **No code signing.** Nothing here signs or notarises a binary; if you ship a
  signed app bundle, replacing `launcher/launcher` in place invalidates it.
- **The write path has several implementations.** The atomic write exists in more
  than one package; consolidating them is deliberately deferred to its own change
  (see `CODE-REVIEW.md`).

## What is in here

| Path | What it is |
|---|---|
| `launcher/` | The launcher: a single static Go binary per platform. Loopback HTTP server, session and CSRF gates, the data API, the save and config engine, port allocation, browser detection, and the self-update path. See `launcher/README.md`. |
| `packaging/` | `kobra-pack`, the publisher's toolchain: validates a game folder, builds reproducible `.zip` and `.tar.zst` archives, writes release manifests, verifies a published directory. Also a `dev` command that serves a source tree without packaging it. See `packaging/README.md`. |
| `testgame/` | A complete fixture game — the conformance suite for both tools. The real packager builds it and the real launcher runs it, end to end, including an update. See `testgame/README.md`. |
| `architecture/` | The specifications: functional, launcher, packaging, updater, plus the published JSON schemas in `architecture/schemas/`. |
| `docs/` | Guides for people building on this: [the index](docs/README.md), [authoring a game](docs/authoring-a-game.md), and [how the launcher serves your game](docs/launcher-architecture.md) for contributors to the internals. |
| `.e2e/` | The launcher smoke drive: build, serve, exercise the API and the gates, drain. |
| `.github/workflows/` | CI: the same make targets a contributor runs locally. |
| `Makefile` | Repository-level chores only: `make sync-schemas` and `make check-schemas`. The gates live in the module Makefiles. |
| `scripts/` | `sync-schemas.sh`, which keeps `architecture/schemas/` and its vendored copies byte-identical. |
| `CONTRIBUTING.md` | How to set up, which gates to run, and the rules of the house. |
| `SECURITY.md` | The threat model and how to report a vulnerability privately. |
| `CODE-REVIEW.md` | The review record, including deferred items and why. |

## Quick start

Requirements: Go (the version in `launcher/go.mod`), `make`, and — for the test
drives — `curl`, `python3`, `unzip`, `tar`, `zstd`.

```bash
git clone https://github.com/KobraRocks/kobra-game.git
cd kobra-game

# Build both tools and produce the fixture's packages, unpacking release 1
make -C testgame package

# Run it: serves the game on loopback and opens your browser
./testgame/dist/install/Pkgtest/launcher/launcher

# Or serve a game source tree without packaging it (symlinks, no copying)
make -C launcher run-dev GAME=/path/to/your/game/src
```

Useful launcher flags: `--print-url` (print the origin with its one-time token
and block; used by tests and headless setups), `--no-open`, `--diagnostics`
(print the support payload and exit), `--repair` (roll back an interrupted update
and rebuild the data scaffolding), `--check-port`, `--port`, `--data-dir`,
`--browser`, `--version`.

### Making your own game

A game is a folder with `game/index.html`, `game/shell.js` and a `pkg.toml`
describing its releases. `testgame/` is the worked example.

**[Authoring a game](docs/authoring-a-game.md)** is the full guide: the folder
contract, the shell boot sequence, the data API (saves, settings, mods, updates),
what the packager generates versus what you write, and every rule `check`
enforces. The short version:

```bash
kobra-pack check --config pkg.toml --platform linux-x64   # the gate your CI runs
kobra-pack build --config pkg.toml --platform linux-x64 --out dist --install
kobra-pack verify dist                                    # index hashes, completeness
```

`check` and `build` run the same validation; `check` just does not write
archives, so it belongs in a pre-commit hook and in CI.

## Repository gates

Everything below is what CI runs, and all of it works locally.

| Command | What it proves |
|---|---|
| `make -C launcher fmt vet test race` | Formatting, vet, unit/contract tests, and the same suite under the race detector. |
| `make -C launcher faultinject` | The §26.4 crash-safety scenarios: a crash between `fsync` and `rename` (the temp file is quarantined on the next start, nothing is lost), a crash between `rename` and the directory `fsync`, an `EXDEV` promotion, a disk-full write, a panicking handler exiting 4, and a signal during drain. |
| `make -C packaging check race` | The packager's formatting, vet, tests and race suite. |
| `make -C testgame verify` | Two packaging runs produce **byte-identical** archives. |
| `make -C testgame e2e` | The fixture drive: package, install, run, update, and assert what happened on disk. |
| `bash .e2e/run.sh` | The launcher smoke drive: gates, traversal attempts, session/CSRF, saves, drain. |
| `make -C launcher cross` | The linux/windows/darwin × amd64/arm64 build matrix with `CGO_ENABLED=0`. |

## The specifications are the source of truth

`architecture/` holds the normative documents, and the code cites them
(`§14.2`, `FR-SAVE-11`, `R10.6`) at the point where each rule is implemented. When
code and spec disagree, one of them is wrong and the disagreement is the bug —
several of the review's findings were exactly that. A change that alters
behaviour the specs describe should update the spec in the same pull request.

Published schemas live in `architecture/schemas/`, which is the only place one is
edited. The same bytes are vendored into the two Go modules, because `//go:embed`
cannot reach outside its package, so edit the published file and run
`make sync-schemas` from the repository root rather than copying by hand —
`make check-schemas` reports drift without writing. Tests in both modules fail if
a copy drifts from the original, in either direction, and `kobra-pack` refuses to
build a package that violates one.

## Security model

The launcher assumes the machine is hostile to a local web server and behaves
accordingly:

- The socket is **loopback only** and the `Host` header must match the port
  exactly, which defeats DNS rebinding; cross-origin requests are rejected.
- A page must present a **single-use bootstrap token** (delivered in the URL
  fragment, never in a query string) to get a session, and every write carries a
  CSRF token. Bearer-token clients skip the double-submit check because a token
  they send explicitly is not ambient authority the browser attaches for them.
- Static serving is **confined** to the game tree: dot segments are rejected
  rather than normalised, symlinks are resolved and re-checked, and Windows
  device names are refused. `data/` and `launcher/` are not reachable over HTTP.
- Error envelopes contain **no filesystem path, username, token or stack trace**;
  a test walks every error constructor and asserts the negative property.
- There is **no telemetry and no network access** except the update server the
  game declares, and update archives must be HTTPS unless they are loopback.

## Reproducibility

`make -C testgame verify` builds the same release twice and compares SHA-256
digests of every published archive. The packager sorts every map, takes
timestamps from the release id rather than the clock, pins its compressor, builds
archive headers with zero uid/gid/name fields, and normalises file modes, so the
bytes depend only on the inputs. Reproducibility is only meaningful for a fixed
toolchain: bumping Go or the compressor can change the output.

## Platform support

| Platform | Build | Status |
|---|---|---|
| Linux (amd64, arm64) | yes | Tested. The fixture drive runs here. |
| Windows (amd64, arm64) | yes | Compile-verified only. See Status. |
| macOS (amd64, arm64) | yes | Compile-verified only. See Status. |
| FreeBSD, OpenBSD | yes | The whole binary builds (checked) and the other BSDs share the same implementations; never executed. |
| Solaris, illumos | partial | The updater and storage packages build, but the sidecar lock has no process-liveness implementation, so the launcher binary does not. Unsupported. |
| plan9 | no | Does not build. |

`make -C launcher cross` covers the six platforms the build matrix ships; the
rest of the table is best-effort and worth checking before you rely on it.

## Contributing

Issues and pull requests are welcome — see **[CONTRIBUTING.md](CONTRIBUTING.md)**
for setup, the gates, and the rules of the house. The short version:

1. Run the gates for whatever you touched (table above). A change to the updater
   or the storage engine should also run `make -C launcher faultinject`.
2. Keep the specs and the code in step; cite section numbers as the surrounding
   code does, and update the spec in the same pull request if behaviour it
   describes changes.
3. Add a regression test for a fixed bug. Every fix in `CODE-REVIEW.md` has one,
   and it is usually named after the failure rather than the function.
4. Prefer deleting a claim over keeping dead machinery: if a config key, event or
   build tag does nothing, remove it rather than documenting it.

Building a game rather than changing the launcher? Start with
[authoring a game](docs/authoring-a-game.md).

## Licence

MIT — see `LICENSE`. Third-party components are attributed in
`THIRD_PARTY_NOTICES.md`, and every package built by `kobra-pack` ships that file
in its `LICENSES/` directory, because the launcher binary statically links
BSD-3-Clause and Apache-2.0 code.
