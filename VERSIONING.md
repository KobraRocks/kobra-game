# Versioning policy

Six version numbers coexist in this product. They are not interchangeable, and
collapsing any two of them loses a question that only one of them can answer.

This document names them, records which one carries which promise, and states the
repository's pre-1.0 policy. It does **not** overrule the specs: where this file
and a document under `architecture/` disagree, the spec wins and this file is the
bug. The four axes below are normatively defined in Packaging spec §4.4; the
launcher floor in §4.3; the API version in FS FR-API-12.

## The six axes

| Axis | Identifier | Shape | Answers | Gate |
|---|---|---|---|---|
| **Publication** | `release` | calver `YYYY.MM.N` | Which build did the user download? | Update selection, lexicographic |
| **Runtime** | `engine_version` | semver `X.Y.Z` | Which WASM core is running? | `wasm_hash` pin |
| **Content** | `game_version` | semver `X.Y.Z` | Which game wrote this save? | Stamped in save headers (diagnostic) |
| **Save format** | `save_version` | integer | Can this build read that save? | **Gates loading** (FS §12.3, FR-SAVE-12) |
| **Player API** | `api_version` | integer (`const 1`) | Does the shell understand this launcher? | **Shell refuses an unknown major** (FR-API-12, E22) |
| **Tool** | launcher / packager version | bare semver `X.Y.Z` | Is this launcher new enough? | `launcher_min` (FR-UPD-6, E29) |

Where each is enforced:

| Axis | Code | Schema |
|---|---|---|
| `release` | `launcher/internal/update/manifest.go` (`ReleaseNewer`, `validRelease`) | `release.manifest.schema.json` |
| `engine_version` | `launcher/cmd/kobra-launcher/main.go`, `packaging/internal/pack/config.go` | `engine.manifest.schema.json` |
| `game_version` | `packaging/internal/pack/config.go`, `identity.go` | `save.schema.json`, `release.manifest.schema.json` |
| `save_version` | `launcher/cmd/kobra-launcher/main.go` (`saveVersion`) | `save.schema.json` |
| `api_version` | `launcher/internal/dataapi/handlers.go`, `internal/server/server.go` | `data-api.schema.json` (`$defs.apiVersion`) |
| tool version | `launcher/Makefile` (`VERSION`), `packaging/internal/pack/identity.go` (`LauncherVersion`, `CompareSemver`), `launcher/internal/update/manifest.go` (`MeetsLauncherMin`) | — |

`launcher/VERSION` is not a seventh axis: it is a **generated** copy of the tool
version, written into a package from the launcher binary's own `--version` output
so the two cannot drift (Packaging spec §4.6). Never hand-edit it.

## Pre-1.0 policy

The repository ships at **`0.y.z`**, and stays there deliberately.

Semantic Versioning §4 defines major version zero as initial development: anything
MAY change at any time, and the public API MUST NOT be considered stable. That is
exactly the signal this project wants to send while it is still collecting
publisher and player feedback. **Major versions are cheap to defer and expensive
to retract**: `1.0.0` is a compatibility promise, and a promise made before there
is evidence to keep it is a liability, not an achievement.

Staying at `0.y.z` is therefore not a reason to postpone adopting SemVer — it *is*
SemVer. The two are the same decision.

### What a `0.x` bump may and may not break

The tool version governs the **publisher-facing** surface: the `kobra-pack` CLI,
its event lines and exit codes, the manifest fields it expects, and the internal
packages. At `0.y.z` those may change without a major bump.

| Bump | May change | Must not |
|---|---|---|
| `0.0.z` | Docs, messages, behaviour-neutral fixes | Change any contract |
| `0.y.0` | CLI surface, event names, exit codes, manifest expectations, internal APIs | Corrupt, or render unloadable, an existing `data/` tree |
| `1.0.0` | Nothing without a major bump | — |

### The player-facing invariant

**A launcher self-update within one `api_version` major MUST NOT make an existing
`data/` tree unloadable, and MUST NOT lose it.**

This promise is carried by `save_version` and `api_version`, **not** by the tool
version. The reason is architectural rather than stylistic: the launcher replaces
itself in place on a machine that may already hold years of saves, and the player
never agreed to a SemVer contract — they simply have saves on disk. SemVer's
`0.y.z` latitude excuses churn in the publisher's toolchain; it cannot excuse
breaking a player's game.

Backups, server-owned revisions, `.trash-<ts>/`, and `--repair` exist to make that
invariant true in practice. Any change to the save writer must keep it true, and
`make -C launcher faultinject` is the gate that proves it.

## Pre-release: `channel`, not a version suffix

Pre-release status is carried by the release manifest's **`channel`**
(`stable` | `beta` | `rc`, Packaging spec §4.2), and **not** by a SemVer
prerelease suffix on any version field.

Version fields are bare `X.Y.Z` — `^[0-9]+\.[0-9]+\.[0-9]+$`, with no `-rc.1` and
no `+build` metadata. This is a decision, not an accident:

1. **One axis, one mechanism.** `channel` already exists and already validates. A
   prerelease suffix on `launcher_min` or `game_version` would be a second way to
   say the same thing, which is the dead machinery CONTRIBUTING rule 3 forbids.
2. **The schemas are published contracts.** Each version field is pinned by a
   regex in `architecture/schemas/` — eight occurrences across five schema files.
   Loosening them changes what every existing publisher's document may contain,
   for a feature no consumer has asked for.
3. **Ordering would have to be implemented twice.** See the rule below.

### Consequence: SemVer §11 precedence is out of scope

Implementing prerelease precedence is deliberately **not** done. If it is ever
required, it is not a local change — it needs all three of:

- the version patterns in `architecture/schemas/` loosened (a contract change,
  with the spec updated in the same pull request);
- §11 precedence implemented in *both* comparators (they are independent by
  design — the launcher ships to users and the packager does not);
- regression vectors added to both modules' tests.

Until then, `channel` is the only supported way to express "this is a beta".

## Rule: an unparseable version fails its gate closed

**A version string the code cannot parse MUST fail its gate closed. It MUST NOT be
silently ordered, and MUST NOT have an unparseable component coerced to zero.**

The two comparators live in different modules and cannot share code — the launcher
keeps a three-dependency footprint and the packager is a separate tool — so the
rule is enforced by matching tests in both, with shared vectors:

| Input | Outcome | Why |
|---|---|---|
| `1.4.0` vs `1.4.0` | equal | Numeric comparison |
| `1.3.9` vs `1.4.0` | older | Numeric comparison |
| `0.1.0-dev` vs `0.1.0` | **invalid → refuse** | Not bare `X.Y.Z`; a bare `go build` is unstamped |
| `1.0.0-rc.1` vs `1.0.0` | **invalid → refuse** | Prerelease suffixes are not supported; use `channel` |

`LauncherVersion` (`packaging/internal/pack/identity.go`) already refuses to read a
version out of a launcher binary that does not print a bare semver, and the
schemas already pin the fields, so valid configurations never reach the invalid
row. That is defence in depth: the comparators fail closed on their own, rather
than relying on a regex in another package to keep them honest.

## Leaving `0.x`

`1.0.0` is a publishing decision, not a technical milestone, and it should be
reached by declaring an API stable rather than by accumulating features. The
precondition is evidence: real publishers shipping real games, and real players
whose save trees have survived several updates. Until that evidence exists,
`0.y.z` is the honest number.
