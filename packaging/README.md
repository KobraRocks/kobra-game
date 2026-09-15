# packaging — the publisher's toolchain

`kobra-pack` turns a studio's development tree into a publishable release. It is
the producer side of the architecture: it runs on the studio's machine and in the
studio's CI, and nothing it produces is ever shipped *as part of* the launcher.

It implements `architecture/Packaging-spec.md`. Where this file and that
specification disagree, the specification wins and this file is wrong.

```
studio repo                       kobra-pack build              published
───────────                       ────────────────              ─────────
pkg.toml          ──┐
game/               ├──►  dist/   ──►  <slug>-<release>-<platform>.zip
launcher/           │                  <slug>-<release>-<platform>.tar.zst
releases/*/         ──┘                <slug>-<release>.release.manifest.json
                                       latest.json
                                       SHA256SUMS  (+ .sig)
```

## The five-minute path

A studio repo needs three things: a `game/` tree, a launcher config, and a
`pkg.toml`.

```toml
# pkg.toml — the single declared input (§14.1)
[package]
slug     = "stardrifter"
game_id  = "com.kobra.stardrifter"
game_name = "Star Drifter"
pkgroot  = "."                       # the directory holding game/

[release]
release        = "2026.09.1"         # calver, immutable once published
game_version   = "1.0.0"            # the game's own content version
engine_version = "0.1.0"            # must equal engine.manifest.json
save_version   = 1                  # must equal engine.manifest.json
launcher_min   = "0.1.0"            # oldest launcher that can run this
channel        = "stable"
scope          = "full"

[launcher]
config     = "launcher/launcher.config.json"
deny_list  = "vendor/kobra/port-deny-list.json"
binary_dir = "vendor/kobra/dist"     # where the game-agnostic binaries live

[platforms.linux-x64]
goos = "linux"
goarch = "amd64"
binary = "launcher"
archive_platform = "linux-x64"

[publish]
base    = "https://stardrifter.example/releases"
gpg_key = ""
```

```
studio/
├── pkg.toml
├── game/                            # the shipped tree
│   ├── index.html                   # ─┐ the only two files served
│   ├── shell.js                     # ─┘ from the game root
│   ├── engine/{game.wasm,game.js,engine.manifest.json}
│   ├── assets/…                     # everything else servable lives here
│   └── locales/
├── launcher/launcher.config.json    # game-owned; the packager assembles the rest
└── vendor/kobra/dist/linux-amd64/launcher
```

Then:

```
kobra-pack check      # every gate, writes nothing      — run on every commit
kobra-pack build      # dist/                            — run on a release tag
kobra-pack dev        # edit-and-run loop
kobra-pack verify dist  # re-verify without the launcher
```

## The development loop

The launcher derives its game folder from its own executable path and has no
`--game-dir` in a release build (FR-LNCH-1), so a developer cannot point it at a
source tree. `kobra-pack dev` builds the layout it needs around the tree you are
editing:

```
<repo>/.kobra-dev/
├── launcher/          the real binary + this game's rendered config
├── game -> <repo>/game    a symlink: edits are live, no copy step
└── data/              a real directory, so saves never enter the source tree
```

The launcher follows symlinks and re-checks confinement, so `git status` stays
clean and a link to `/etc/passwd` is still a 404. Sidecar state (chosen port,
logs) is kept under the shim, not in your repo.

## What `check` gates, and why it is separate

`build` and `check` share one code path, so a gate cannot exist in one and not
the other. `check` exists because validating a multi-gigabyte tree must not mean
building a multi-gigabyte archive — it stops before the archive stage.

| Gate | Rule |
|------|------|
| Config shape | `pkg.toml` parses; slug, game_id, calver and semver patterns |
| Cross-document identity | Launcher config release == engine manifest release == `pkg.toml`; `save_version` agreement; `launcher_min ≤ launcher binary` |
| Schema validation | `launcher.config.json`, `engine.manifest.json`, and the generated manifests validate against the vendored schemas |
| Forbidden content | Symlinks, Windows device names, VCS/editor metadata, trailing dots, anything but `.keep` under `data/` |
| File index safety | Every indexed path is a relative path under `game/` or `launcher/`, with no dot segments — never `data/` |
| Servability | Every file under `game/` is reachable through the launcher's static route allowlist |
| Engine files | The wasm and glue named by the engine manifest exist, and `.wasm` is served as `application/wasm` |
| Determinism | Two `build` runs produce byte-identical archives |

Two of these are worth calling out because they are the ones a game most often
trips:

- **Servability.** The launcher serves exactly two files from the game root —
  `index.html` and `shell.js` — plus everything under `engine/`, `assets/` and
  `locales/`. A stylesheet at `game/shell.css` is packaged, indexed, and then
  404s in the browser with no error anywhere. Put servable files under
  `assets/`.
- **Schema validation.** Five of the vendored schemas use negative lookahead,
  which Go's regexp cannot compile (Packaging spec §12 A8). `kobra-pack` strips
  the lookahead at load time, reports `pack.schema.relaxed` for each pattern it
  touched, and enforces the rule those patterns carried in code. The relaxation
  is visible in the build log rather than silent.

## Releases after the first

A later release stores only what changed, in `releases/<id>/`:

```
releases/2026.10.1/
├── release.toml        # release, game_version, previous_release, scope
└── overlay/game/…      # only the files that differ
```

`kobra-pack build` copies `game/` and applies each overlay on top, so the repo
does not carry N copies of the tree. `latest.json` is written for the newest
release automatically.

A release that advances `release` **always** changes `launcher/launcher.config.json`
too, because §4.6 requires its `release` field to track the release id — the
packager renders that file per release rather than copying it verbatim. So
`scope = "game-only"` is not achievable for an id bump; use `full`.

## CI

```yaml
- run: kobra-pack check --config pkg.toml --platform linux-x64 --json
- run: kobra-pack build --config pkg.toml --platform linux-x64 --out dist
- run: rsync -a --delete dist/ publisher@host:/srv/releases/stardrifter/
```

`--json` emits one JSON object per line, so a CI job parses events rather than
prose. Every failure carries a stable event name (`pack.identity.fail`,
`pack.scan.reject`, `pack.schema.fail`, `pack.verify.fail`) with the offending
path in a field.

Publishing is a directory. `kobra-pack` writes the exact layout §8.3 defines and
stops; the transport is your choice of `rsync`, `scp`, or a bucket sync. The tool
holds no credentials and makes no network requests.

## Linux first, other platforms best effort

`linux-x64` is the default and the only platform exercised end to end. Add
another `[platforms.*]` table and a matching launcher binary and it builds; what
it cannot do is produce signed platform binaries:

| Platform | What is missing |
|----------|-----------------|
| `linux-arm64` | Nothing. Cross-compile and add the table. |
| `win64` | An Authenticode certificate; SmartScreen therefore warns |
| `macos-universal` | A Developer ID identity and a notarisation submission |

Community-contributed platform support should land as additional `[platforms.*]`
tables plus, where signing is involved, a documented manual step — not as code
paths in the pipeline.

## What is not built yet

- **Manifest signatures.** `SHA256SUMS.sig` is produced when `gpg_key` is set.
  The manifest's `signatures` object is deliberately **not** populated: §7.4
  requires the signature to cover the manifest "exactly as published", and a
  signature stored inside that document cannot cover it. That needs a stated
  canonical form (the manifest with `signatures` removed) before it can be
  implemented honestly.
- **Pruning and retention** (§8.8). The layout supports it; no tool prunes.
- **Content addressing** (§6.5.1). The asset manifest declares
  `content_addressed: false`. Turning it on requires rewriting every asset
  reference in HTML, CSS, JS and engine glue, which needs to know how a given
  game refers to its assets.
- **`migrations` chain validation** (§6.6, FR-SCH-6). The field is generated
  empty; contiguity checking belongs with the first release that raises
  `save_version`.
- **`launcher_min` from the binary.** It is declared in `pkg.toml` and checked
  against the binary. §4.3 calls it "the launcher's declared minimum", but no
  launcher build currently declares one.

## Design notes for whoever maintains this

The plumbing is expected to be maintained by agents, so the properties that make
that safe are deliberate:

- **One declared input.** `pkg.toml` states every choice; nothing is inferred
  from ambient state, the clock, or the working directory. Relative paths resolve
  against `pkg.toml`, so CI and a shell behave identically.
- **One setup path.** `check` and `build` both call `prepare` and `stageRelease`,
  so a gate cannot be present in one and absent from the other.
- **Events, not prose.** Gates assert on `pack.*` names; messages can be reworded
  without breaking a test.
- **Schemas are embedded and drift-checked.** The binary carries its own copy, so
  a studio needs no schema files, and `TestEmbeddedSchemasMatchArchitecture` fails
  the test suite if the copy diverges from `architecture/schemas/`.
- **No clock in the output.** Timestamps come from the release id, so a rebuild
  is byte-identical (§5.2). `TestPipelineIsDeterministic` enforces it.
- **The fixture is the conformance suite.** `testgame/` drives this tool end to
  end via `make -C testgame e2e`.

```
make build     # kobra-pack
make test      # unit + pipeline conformance tests
make check     # gofmt, vet, test
```
