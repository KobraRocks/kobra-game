# Technical Specification: Per-Game Packaging and Release

> **Companion documents.** This specification states how a game becomes a
> distributable package and how a release is published. The product requirements are in
> `functional-specification.md`; the consumer of these artefacts is specified in
> `Launcher-spec.md`; and the mechanism that installs an update is specified in
> `Updater-spec.md`.

**Document Version:** 1.1
**Companion to:** Functional Specification v2.2 (September 2026), Launcher Technical Specification v1.0
**Date:** September 2026
**Status:** Implementation-Ready
**Classification:** Technical Architecture Specification
**Audience:** Release engineers, game developers, build/CI maintainers, security reviewers

> **Revision note (v1.1).** The release manifest's place in the two artefacts is now stated rather than implied, because the implied version was contradictory. §6.1 put the manifest inside `game/`, §6.4 had it index every shipped file "except itself", and §8.2.2 required it to declare the archive's hash — three rules that cannot hold at once, since a manifest inside the archive would need the input to its own hash. The resolution is that **the manifest is a published document and is never a member of the archive it describes** (§1.2, §6.4, §8.2.2, §9.1); the ZIP carries a byte-identical copy for support inspection, and the updated install needs no copy at all because `current` falls back to `launcher.config.json`. §8.2.3's fail-closed rule is now implemented in the launcher, and the schema amendments it depended on (A1–A3) are applied. Nothing here changes what a publisher ships; it names the thing the pipeline was already forced to build.
>
> **Scope note.** The Functional Specification (FS) defines *what* the product must do; the Launcher specification defines *how the launcher process is built*. This document defines *how a game becomes a distributable package*: what goes into the archive, how the release is identified, how its manifests and hashes are generated, how it is signed and published, and how an update is delivered to a running install.
>
> **Subordination.** This document is subordinate to the FS. Where the FS and this document conflict, **the FS wins and this document is wrong**. Where the FS is silent, this document decides, and the decision is marked *(unspecified in FS — decided here)* so the choice is reviewable.
>
> **What is not in scope.** The engine's build, the game's gameplay code, the shell's behaviour (`shell.js`), and the launcher's runtime implementation. This document covers the *producer* side: the tree a developer keeps, the pipeline that turns it into an archive, and the artefacts that ship beside it.

---

## Table of Contents

1. [Overview and Design Principles](#1-overview-and-design-principles)
2. [The Development Tree and the Package: Two Different Things](#2-the-development-tree-and-the-package-two-different-things)
3. [Package Layout and Naming](#3-package-layout-and-naming)
4. [Release Identity](#4-release-identity)
5. [Build Pipeline](#5-build-pipeline)
6. [Manifest Generation](#6-manifest-generation)
7. [Integrity, Checksums, and Signing](#7-integrity-checksums-and-signing)
8. [Channels and Publishing](#8-channels-and-publishing)
9. [Update Artefacts](#9-update-artefacts)
10. [Per-Platform Packaging](#10-per-platform-packaging)
11. [Release Checklist and Gates](#11-release-checklist-and-gates)
12. [Schema Amendment Backlog](#12-schema-amendment-backlog)
13. [Handoff to the Launcher and Verification](#13-handoff-to-the-launcher-and-verification)
14. [Repository and Tooling Layout](#14-repository-and-tooling-layout)
15. [Open Questions and Deferred Decisions](#15-open-questions-and-deferred-decisions)

**Appendices**

- [A. Release Manifest Reference](#appendix-a-release-manifest-reference)
- [B. Event and Error Catalogue for the Build](#appendix-b-event-and-error-catalogue-for-the-build)
- [C. Requirement Coverage Map](#appendix-c-requirement-coverage-map)

---

## 1. Overview and Design Principles

### 1.1 What This Document Covers

A Kobra game ships as a **single ZIP that a user extracts and runs** — no installer, no package manager, no store client (FR-LNCH-2, FS §13.2). Producing that ZIP is the subject of this document.

The pipeline has one input and one output:

```
INPUT                                    OUTPUT
<pkgroot>/                               GameName-<release>-<platform>.zip
├── game/          (source tree)   ──►   ├── SHA256SUMS
└── launcher/                             ├── SHA256SUMS.sig
    └── launcher.config.json              ├── GameName-<release>.release.manifest.json
                                          └── GameName-<release>.tar.zst   (update archive)
```

The launcher binary is **not** built here. It is a per-game artefact only by virtue of the config file placed beside it (§2.3); the binary itself is game-agnostic and built once (Launcher spec §25).

### 1.2 Two Artefact Classes

Every release produces **two** archives with different rules, and conflating them
is the source of the `data/.keep` ambiguity in the source material:

| | Distribution ZIP | Update archive |
|---|---|---|
| Audience | A human, once, by hand | The launcher, over the update path |
| Format | `.zip` | `.tar.zst` |
| Contains `data/` | **Yes — the skeleton only**: `data/saves/`, `data/config/`, `data/mods/` and `data/.keep` | **No `data/` entry of any kind** |
| Contains `README.txt`, `LICENSES/` | Yes | No |
| Contains `game/release.manifest.json` | Yes — the manifest, byte-identical to the copy published at the base URL | **No** — a manifest cannot be a member of the archive it indexes (§6.4, §8.2.2) |
| Covered by `files[]` | No — the file index covers `game/` and `launcher/` only | Yes, for the paths it replaces |
| Integrity | `SHA256SUMS` (+ `SHA256SUMS.sig`) | `archive.hash` and `archive.size`, then the `files[]` index and `total_size` |

"Releases never contain `data/`" (FR-UPD-7) is a statement about the **update
archive** and about the manifest's file index: a release never *writes* a user's
saves. The distribution ZIP necessarily creates the empty `data/` skeleton,
because a copy-and-run package that lacks `data/saves/` would force the user to
create it by hand. The two rules are compatible once the artefacts are named
separately, which this document does throughout.

The release manifest is subject to the same discipline, for a structural reason
rather than a product one. It indexes `game/` and `launcher/`, so it cannot
index itself — and the launcher rejects any archive member its index does not
list. A manifest inside the archive it describes would therefore be rejected on
extraction, and it could not declare that archive's hash in any case, because
that hash would depend on the manifest. So the update archive omits it exactly
as it omits `data/`, `README.txt` and `LICENSES/`, and the manifest is a
*published* document: `<slug>-<release>.release.manifest.json` at the base URL,
with a byte-identical copy in the ZIP for whoever holds the ZIP and no launcher
(§8.2.2, §6.4). Nothing breaks by its absence in an updated install: §6.1's
`current` lookup falls back to `launcher.config.json`'s `release`.

### 1.3 Precedence

The Functional Specification is normative. The Launcher specification and this
document are both subordinate to it, and the Launcher spec says so itself — every
section header carries "*Satisfies:* FR-…; FS §…" and Appendix D maps each `FR-*`
family to the section that implements it. **Where the FS and this document
conflict, the FS wins and this document is wrong.**

**A rule about the sources.** The FS, the Launcher spec and the vendored schemas
contain cross-reference and identifier errors in their prose. This document
therefore cites requirements by their *behaviour* and cross-checks the schema
where one exists, and it does **not** reproduce a citation merely because a
source document contains it. In particular, the following source defects are
known and MUST NOT be copied into downstream work:

| Source claim | Correct identifier |
|--------------|--------------------|
| `release.manifest.schema.json`'s `signatures` cites FR-LNCH-3 | FR-LNCH-5 (per-file checksums and detached GPG) |
| `release.manifest.schema.json` cites FR-AST-6 for advisory asset hashes | FR-AST-4 |
| `engine.manifest.schema.json`'s `required_features` cites E1/E12 | E2 |
| `save.schema.json` and `release.manifest.schema.json` swap FR-SAVE-12 and FR-SAVE-13 | FR-SAVE-12 = migration chain; FR-SAVE-13 = no "continue anyway" |
| `launcher.config.schema.json`'s `game_id` cites FR-FSA-1 | retired identifier; the live requirement is FR-LNCH-1 |
| `launcher.config.schema.json`'s `retain_previous_release` cites FR-UPD-7 | FR-UPD-8 |
| `data-api.schema.json` / FR-API-12 cites E22 | E3 |
| FS §4 stage 2 cites FS §17.1 for a 500 MB download target | not present in §17.1; treat the 500 MB figure as declined |
| FR-SCH-1 promises minor-version acceptance | every `schema` property is a `const` with no minor component, so acceptance is by exact major only |

A requirement inherited from a source document is applied **only** after its
behaviour is confirmed against the FS text; a citation alone is not evidence.

### 1.4 Design Principles

1. **`data/` is sacred.** No release artefact, in any form — ZIP, patch, tar, checksum list, or manifest file index — may contain, move, or delete anything under `data/`. The user's saves are the product's only irreplaceable state (FS §11, FR-UPD-7).
2. **The manifest is the release.** A release is not "the ZIP"; it is the ZIP plus a manifest whose file index is mandatory and hashed. A ZIP without a matching manifest is not a release and MUST NOT be published (FR-UPD-1).
3. **A build is reproducible from its inputs.** The package is a pure function of `(source tree, release id, launcher binaries, manifest)`. Two builds from the same inputs produce byte-identical output, so a hash is a meaningful statement about the game (Launcher spec §25.2).
4. **The developer's tree is not the shipped tree.** Source files, test fixtures, build scripts, editor state and `.git` never reach a package. The transformation is explicit and reviewed (§2).
5. **Fail the build, never the user.** Every packaging error — a save file under `game/`, a missing manifest, a hash mismatch, a `launcher_min` that exceeds the shipped launcher — is a build failure, caught before publication. A package that reaches a user is expected to run.
6. **Sign what you can, publish hashes always.** SHA-256 checksums are mandatory for every release; Authenticode, Developer ID notarisation and a detached GPG signature over `SHA256SUMS` are layered on where the platform and the team's signing authority allow (FR-LNCH-5, FS §13.3).
7. **One release id, everywhere.** A single `<release>` string identifies the ZIP, the manifest, the patch archive, the checksum file, the changelog and the support conversation. Any artefact whose embedded `release` disagrees is a build failure (§4.4).

---

## 2. The Development Tree and the Package: Two Different Things

**Satisfies:** FR-LNCH-2, FR-UPD-7; FS §5.1, §13.2.

### 2.1 The Two Trees

A game exists in two shapes, and conflating them is the most common packaging defect:

| | Development tree | Package (installed game folder) |
|---|---|---|
| Purpose | Where a developer edits | What a user extracts and runs |
| Contents | Sources, tests, tooling, assets, generated engine output | `game/`, `launcher/`, an empty `data/` skeleton, and human-facing files |
| Tracked in VCS | Yes | No — a build artefact |
| `data/` | A scratch directory, frequently dirty with real saves | A skeleton: `data/saves/`, `data/config/`, `data/mods/`, `data/.keep` |
| Symlinks | Allowed | Forbidden (see §2.4) |
| Launcher binary | Absent or a symlink to a shared build | Present, with this game's `launcher.config.json` |

### 2.2 What `pkgroot` Is

The build is driven by a **package root** (`pkgroot`): the directory that contains the `game/` tree to be shipped.

- In the simplest case `pkgroot` *is* the working tree root, and the developer keeps `game/` at the top level.
- When the game's sources live elsewhere — a separate repository, a `src/` subdirectory, a monorepo path — `pkgroot` points at the directory containing the built `game/` output, not at the source root. `make run-dev GAME=<dir>` (Launcher README) is the development analogue of this same indirection.
- `pkgroot` MAY be a symlink to the real tree; the build resolves it once, at the start, and operates on the resolved absolute path so that every path recorded in a manifest is stable and symlink-free.

**The build MUST NOT modify anything under `pkgroot/game/`.** Manifests are generated into a staging directory, not written back into the source tree (§5.3).

### 2.3 The Launcher Directory

The `launcher/` directory in a package contains:

| File | Required | Source |
|------|----------|--------|
| `launcher` / `launcher.exe` | Yes | Built once from the launcher repository, per platform (§10) |
| `launcher.config.json` | Yes | **Owned by the game**, authored by hand or generated from the game's metadata |
| `port-deny-list.json` | Yes | Copied from the shared launcher repository; the launcher validates against it (FS §6.4) |
| `VERSION` | Yes | Plain text, the launcher's own version, e.g. `1.4.2\n` (FS §5.1) |

`launcher.config.json` is the *only* file that makes a launcher game-specific (Launcher README, "Per-game launchers and development"). Its `game_id` is the game's identity for the sidecar directory and the deterministic port; it MUST be reverse-DNS lowercase matching `^[a-z0-9]+(\.[a-z0-9-]+)+$`.

### 2.4 Forbidden in a Package

The following MUST be absent from every package, and the build MUST fail if any is found:

| Forbidden | Why |
|-----------|-----|
| Any file under `data/` other than `data/.keep` and the empty skeleton directories | The user's saves, config and mods are not the publisher's to ship (FR-UPD-7) |
| Any symlink, anywhere | Archive extraction rejects symlinks (FR-UPD-5); a package that needs one cannot be extracted |
| Any file under `game/` or `launcher/` whose name is a Windows device name (`CON`, `NUL`, `AUX`, `PRN`, `COM1`–`COM9`, `LPT1`–`LPT9`), case-insensitively | The launcher rejects these at extraction (FR-UPD-5) and Windows cannot create them |
| Version-control metadata (`.git`, `.svn`, `.hg`) | Leaks history and bloats the archive |
| Editor and OS metadata (`.DS_Store`, `Thumbs.db`, `*~`, `*.swp`, `.idea/`, `.vscode/`) | Noise; non-deterministic |
| Sources, tests, build scripts, CI config, debug symbols, `.map` files | Not needed at runtime; a source-disclosure and size liability |
| Files with a trailing dot or space in any path segment | Windows strips them, so the extracted name would not match the manifest |
| Absolute paths, `..` segments, or drive-qualified names | Zip-slip (FR-UPD-5) |
| A `game/release.manifest.json` whose `release` disagrees with the package's release id | §4.4 |

### 2.5 The One Permitted Empty Directory

`data/.keep` exists because ZIP does not represent empty directories reliably across tools; its presence guarantees the extractor creates `data/` (FS §5.1). The build MUST create `data/.keep` and MUST create `data/saves/`, `data/config/` and `data/mods/` as real directories in the ZIP.

`data/.keep` MUST be empty (zero bytes). It is the only file under `data/` in any package.

---

## 3. Package Layout and Naming

**Satisfies:** FR-LNCH-2; FS §5.1.

### 3.1 Normative Package Layout

```
GameName/                                 # the single top-level directory in the ZIP
├── launcher/
│   ├── launcher.exe  (Windows)  |  launcher  (macOS/Linux)
│   ├── launcher.config.json
│   ├── port-deny-list.json
│   └── VERSION
├── game/                                 # replaced wholesale by an update
│   ├── index.html
│   ├── shell.js
│   ├── engine/
│   │   ├── game.wasm
│   │   ├── game.js
│   │   └── engine.manifest.json
│   ├── assets/
│   │   ├── textures/  audio/  models/  data/
│   │   └── asset.manifest.json
│   ├── locales/
│   └── release.manifest.json
├── data/                                 # never part of an update
│   ├── saves/                            # empty
│   ├── config/                           # empty
│   ├── mods/                             # empty
│   └── .keep
├── README.txt
└── LICENSES/
    └── …                                 # third-party licence texts
```

This is FS §5.1 verbatim, with two additions decided here:

- **`port-deny-list.json` in `launcher/`** *(unspecified in FS — decided here)*. FS §6.4 makes the deny list shared between the launcher and the shell, and the Launcher spec ships it beside the binary. It is therefore part of the package, not of the launcher's embedded assets, so that it can be updated with the game.
- **`LICENSES/`** *(unspecified in FS — decided here)*. The tree names it in FS §5.1 but no requirement governs it. This document requires it to exist when the package contains any third-party component (engine runtime, fonts, audio, WASM toolchain output).

### 3.2 Archive Names

| Artefact | Name | Example |
|----------|------|---------|
| First full package | `<slug>-<release>-<platform>.zip` | `stardrifter-2026.09.1-win64.zip` |
| Update archive | `<slug>-<release>-<platform>.tar.zst` | `stardrifter-2026.10.1-linux-x64.tar.zst` |
| Patch archive | `<slug>-patch-<from>-to-<to>-<platform>.tar.zst` | `stardrifter-patch-2026.09.1-to-2026.10.1-linux-x64.tar.zst` |
| Release manifest | `<slug>-<release>.release.manifest.json` | `stardrifter-2026.09.1.release.manifest.json` |
| Checksums | `SHA256SUMS` | — |
| Detached signature | `SHA256SUMS.sig` | — |

- `<slug>` is lowercase ASCII, `[a-z0-9-]+`, and MUST equal the last segment of `game_id` (so `com.kobra.stardrifter` → `stardrifter`). *(unspecified in FS — decided here.)*
- `<platform>` is one of `win64`, `win-arm64`, `macos-universal`, `linux-x64`, `linux-arm64`.
- The extension is `.zip` for the user-facing package and `.tar.zst` for anything the launcher consumes directly. The launcher's extractor reads a zstd-compressed tar (Launcher spec §19.3); the ZIP exists for the human.

### 3.3 Two Formats, Two Jobs

**Decided: both archives are published, because they serve different consumers.**
This is not a choice between formats.

| | Distribution ZIP | Update archive |
|---|---|---|
| Consumer | A **human**, with an extractor, once | The **launcher**, over the assisted patch path |
| Format | `.zip` | `.tar.zst` |
| Why this format | It is the only container every OS opens without extra software, which is what copy-and-run requires (FR-LNCH-2). A user who buys once and owns the copy forever must be able to unpack it with tools they already have | zstd + tar streams, decompresses fast, and carries POSIX modes natively, which matters for the executable bit on Linux and macOS (§10.2) |
| Contents | `game/`, `launcher/`, the empty `data/` skeleton, `README.txt`, `LICENSES/` | `game/` and, when it changed, `launcher/` — never `data/` (§1.2) |
| Integrity | `SHA256SUMS` (+ `SHA256SUMS.sig`) | `archive.hash` in the manifest (§8.2), then the `files[]` index |
| Required? | Yes — it is the product the user buys | Only when `update.channel` is `patch`; a `manual`-channel game publishes the manifest and the ZIP |

Both are listed in `SHA256SUMS`, so one verification step covers whichever the
user holds. The ZIP is the artefact of record: **if a user can hold only one
thing, it is the ZIP**, because a ZIP plus `README.txt` is a complete, runnable
game with no server involved — which is the ownership guarantee (FS §12.2A).

A game that ships `update.channel: manual` still publishes the `.tar.zst` **only
if** it wants the assisted patch path available later; a pure manual-channel game
may publish the ZIP, the manifest and `SHA256SUMS` alone. `latest.json` and the
manifest remain mandatory in both cases, because they are the discovery path
(§8.5).

*(This resolves the format question. The FS requires the ZIP for the manual path
and the Launcher spec requires tar.zst for the patch path; neither document
frames them as alternatives, and this one confirms they are complements.)*

### 3.4 Package Size Is Not a Build Constraint

**There is no maximum package size and no maximum file size.** A game may ship a
package larger than 4 GiB, and may contain an individual file larger than 4 GiB.
Where the user stores and extracts it — an internal SSD, an SD card, a USB stick,
a network share — is the user's decision and the user's responsibility. The build
MUST NOT reject, truncate, warn-and-fail, or otherwise constrain a package on the
basis of its size.

This supersedes the "target size under 500 MB" figure in FS §4 stage 2, which is
in any case a dangling reference: it cites FS §17.1, which specifies boot, memory
and I/O budgets and contains no size target *(FS §4 stage 2 is defective; see
§1.3)*. Size is a product and publishing decision, not an integrity or correctness
property, and this document does not impose one.

**What the build still owes the user.** Size is not a build failure, but it is
not invisible either. The build MUST:

| Requirement | Reason |
|-------------|--------|
| Emit ZIP64 structures whenever any file or the archive itself crosses the classic 4 GiB or 65535-entry limits | A ZIP without ZIP64 simply cannot represent the package; the failure would surface on the user's machine, not in CI |
| Record the package size, the largest single file, and the total entry count in the build log and the release record | Lets support answer "will this fit on my card" with a number instead of a guess |
| Document, in `README.txt`, the package's download size, extracted size, and free-space requirement | The user chooses the device; the publisher must state what the choice costs |
| Publish the download size on the download page beside the checksums | A user on a metered or small device should learn the size before downloading |

**Media-capability guidance is documentation, not enforcement.** Some filesystems
and some very old extractors cannot handle files at or above 4 GiB (FAT32 is the
common case). The publisher MAY state a recommended filesystem in `README.txt`
and MUST state the actual sizes, but the build MUST NOT fail because a file is
large: a game legitimately shipping a multi-gigabyte asset pack is not a defect,
and a user who extracts it to an unsuitable device needs a documented
requirement, not a build error.

*(The FAT32/extractor caveat above is a documentation obligation. If the project
later decides that FAT32 must be supported as a hard target, that is a product
decision requiring a size budget — see §15.2 — and it would then be enforced
here.)*

### 3.5 Archive Mechanics That Are Still Mandatory

Size is unconstrained; the archive's *structure* is not, because a structural
defect also fails only on the user's machine.

| Constraint | Rule |
|------------|------|
| Compression | Deflate at a fixed level; skip compression on already-compressed formats (`.wasm`, `.ogg`, `.mp3`, `.png`, `.webp`, `.woff2`) to avoid build cost for no gain |
| Entry order and timestamps | Sorted by path; a single fixed mtime (§5.2) |
| Path separators | Forward slashes only; no leading `/`; no `..`; no backslashes |
| Filenames | ASCII-safe subset `[A-Za-z0-9._/-]`; a UTF-8 name requires the publisher to have confirmed the target extractors handle it |
| Entry count | Large asset sets SHOULD be bundled (FR-AST-10 bundles) rather than shipped as thousands of tiny entries, which extractors handle slowly. This is a performance preference, not a size limit |
| Directory entries | Emit explicit directory entries so empty directories (`data/saves/`) survive extraction |

A package whose *entries* violate any of the above is a build failure, because the
failure would surface only on a user's machine. A package that is merely large is
not.

### 3.6 One Release Per Platform, One Manifest Per Release

A multi-platform release publishes one ZIP per platform and **one release manifest shared by all of them**, because the manifest's file index covers `game/` — which is identical across platforms — and `launcher/`, which differs only in the binary.

**Requirement.** The manifest's `files` index MUST list every shipped path for the platform it accompanies. Where platform binaries differ, the publisher MAY publish one manifest per platform with the same `release` value; both are valid releases, and the launcher verifies against the manifest that describes its own archive.

---

## 4. Release Identity

**Satisfies:** FR-UPD-1, FR-SCH-5; Launcher spec §19.5.

### 4.1 The Version Families

Every identifier in the product, with its pattern and its owner. The four axes a
publisher must reason about are separated in §4.4.

| Identifier | Pattern | Owner | Changes when |
|------------|---------|-------|--------------|
| `release` | `^[0-9]{4}\.[0-9]{2}\.[0-9]+$` | Release engineering | Any shipped change; unique per publication |
| `engine_version` | `^[0-9]+\.[0-9]+\.[0-9]+$` | Engine team | Engine code changes |
| `game_version` | `^[0-9]+\.[0-9]+\.[0-9]+$` | **Game / publisher** | The game's content series advances (§4.4) |
| `save_version` | integer ≥ 1 | Engine team | The save body's shape changes incompatibly |
| `launcher_min` | `^[0-9]+\.[0-9]+\.[0-9]+$` | Launcher team | The release needs capabilities of a newer launcher |
| launcher version | `^[0-9]+\.[0-9]+\.[0-9]+$` | Launcher team | The launcher binary changes |
| `api_version` | integer const `1` | Launcher team | The data API breaks |
| `schema` tags | `kobra.<name>/<n>` | Whoever owns the document | The document's shape changes |

### 4.2 Release Identifier Rules

- The release id is **calendar-versioned**: `YYYY.MM.N`, where `N` starts at `1` and increments for every publication in that month. `2026.09.1`, `2026.09.2`, `2026.10.1`.
- Comparison follows §13.3: the schema calls it lexicographic on the zero-padded
  components, which is sound for the year and month; tooling that must *order*
  releases compares the segments as integers.
- A release id is **immutable**. Re-publishing changed content under an existing id is forbidden: the launcher's conflict detection, rollback retention and save-migration chain all assume an id names one exact tree.
- A hotfix is a new `N`. A backport to an older month is a new `N` in that month, and it MUST declare `previous_release` explicitly so the update path is unambiguous.

### 4.3 `launcher_min` Discipline

`launcher_min` is the *oldest launcher* that can run the release (FR-UPD-6). It MUST be:

- equal to the launcher version shipped in the package, or lower; and
- greater than the previous release's `launcher_min` **only** when the new release genuinely requires new launcher behaviour.

The build MUST fail when `launcher_min` exceeds the version of the launcher binary being packaged. Shipping a release that its own bundled launcher refuses is the packaging equivalent of a self-inflicted brick (Launcher spec §19.5).

### 4.4 The Four Independent Version Axes

A Kobra game is distributed by its own publisher, to its own audience, from its
own server. There is no storefront, no platform operator, and no external
authority that assigns or arbitrates versions (FS §1: buy once, own forever;
FR-LNCH-2: copy-and-run). **The publisher therefore owns every version namespace
in the product.** Nothing here defers to an app store's review cadence, a
platform's release train, or a third party's content id.

That freedom is why four version numbers coexist, and they MUST NOT be
collapsed into one:

| Axis | Identifier | Answers | Changes when | Owner |
|------|------------|---------|--------------|-------|
| **Publication** | `release` (calver `YYYY.MM.N`) | "Which build did the user download?" | Any shipped change | Release engineering |
| **Runtime** | `engine_version` (semver) | "Which WASM core is running?" | Engine code changes | Engine team |
| **Content** | `game_version` (semver) | "Which game is this?" | The game's own content series advances | **The game / publisher** |
| **Save format** | `save_version` (int) | "Can this build read that save?" | The save body's shape changes incompatibly | Engine team |

They move independently and each one answers a question the others cannot:

- `release` is a **publication** fact. It exists so a support conversation, a
  download page and a checksum file can name one exact tree. It says nothing
  about compatibility.
- `engine_version` is a **runtime** fact, shared by every game built on that
  engine build, and is what `engine.manifest.json`'s `wasm_hash` pins.
- `game_version` is a **content** fact and belongs to the game. It is the only
  one of the four a player would recognise as "the version of my game", and it is
  the value recorded in every save header (`save.schema.json`) so that a support
  engineer can tell which content wrote a save.
- `save_version` is a **format** fact and is the only one that gates loading
  (FS §12.3).

**Requirements.**

1. **`game_version` is publisher-owned and MUST be declared.** The game MUST
   declare its own semver series in the package (a `game_version` field in
   `game/engine/engine.manifest.json`, or a `game_version` key in the game's
   build metadata). It MUST match `^[0-9]+\.[0-9]+\.[0-9]+$`.
2. **The build MUST stamp the declared `game_version` into every save header it
   ships**, and MUST NOT derive it from `release` or `engine_version`. A save
   written under `game_version 1.2.0` keeps saying `1.2.0` forever, which is what
   makes the field diagnostic rather than decorative.
3. **Changing `game_version` MUST NOT change `save_version`.** A content release
   that alters no save field keeps the same `save_version`; the two axes are
   independent by construction.
4. **`release` and `game_version` MUST NOT be required to move together.** A
   publisher MAY ship two `release`s at one `game_version` (a packaging fix, a
   re-sign) and MAY advance `game_version` without a new engine. Cross-document
   consistency (§4.5) checks agreement, not lockstep.
5. Where no `game_version` is declared, the build MUST fail rather than default
   it. A silently-invented content version is worse than a missing one, because
   it would be written permanently into every save header.

*(This resolves open question 14.11. FS §11.2 requires the field in a save
header but never defines its relationship to `release`; the FS leaves the axis
unowned, and this document assigns it to the publisher, which is the party the
architecture places in control.)*

Four axes are normative here because packaging owns them. Two more exist outside
this document — `api_version` (FS FR-API-12) and the launcher's own version,
which §4.3 turns into the `launcher_min` floor. `VERSIONING.md` at the repository
root names all six together and records the repository's pre-1.0 policy: what a
`0.y.z` tool version does and does not promise, that pre-release status is carried
by `channel` rather than by a SemVer prerelease suffix, and that an unparseable
version fails its gate closed. Where this document and `VERSIONING.md` disagree,
this document wins.

### 4.5 How `game_version` Is Declared

*(Technical decision — resolves the "where does the build read it from" half of the
version-axis question. Chosen to agree with the code the launcher already ships.)*

`game/engine/engine.manifest.json` carries an **optional** `release` field, and
the schema permits exactly one extra string there in practice. Two candidate
forms existed; the file that already exists wins:

1. **`game/engine/engine.manifest.json` → a `game_version` field.** Rejected:
   `release.manifest.schema.json`'s `files[].path` pattern is restrictive, and
   more importantly the engine manifest is *engine-owned* — the engine team owns
   its schema and would have to add a game-content field to it.
2. **A dedicated `game/game.version` text file.** Rejected: one more file to
   forget, with no schema to validate it.
3. **`game/release.manifest.json` → `game_version`.** **Chosen**, with the value
   authored in the game's build metadata and passed to the packaging pipeline.

The pipeline's `pkg.toml` (§14.1) MUST declare `game_version`, the build MUST
reject anything not matching `^[0-9]+\.[0-9]+\.[0-9]+$`, and the generated
`release.manifest.json` MUST carry it. This requires one schema addition:

```json
"game_version": {
  "type": "string",
  "pattern": "^[0-9]+\.[0-9]+\.[0-9]+$",
  "description": "The game's own content version. Independent of engine_version and release (§4.4). Stamped into every save header."
}
```

Rationale for putting it on the release manifest rather than inventing a file:
the release manifest is already the document that the build generates, that the
launcher reads, and that a support engineer inspects. `game_version` is a
property *of the release*, so it belongs there and nowhere else.

### 4.6 Cross-Document Consistency

The build MUST verify, and fail on violation:

| Check | Rule |
|-------|------|
| Release agreement | `game/release.manifest.json`'s `release` == the package's release id |
| Engine agreement | `game/engine/engine.manifest.json`'s `engine_version` == the release manifest's `engine_version` |
| Content agreement | A `game_version` is declared and matches `^[0-9]+\.[0-9]+\.[0-9]+$`; the same value is used everywhere the build stamps it (§4.4) |
| Save-version agreement | `game/engine/engine.manifest.json`'s `save_version` == the release manifest's `save_version` (FR-SCH-5) |
| Launcher agreement | `launcher/launcher.config.json`'s `release` == the package's release id |
| Launcher version agreement | `launcher/VERSION` == the version embedded in the packaged launcher binary (obtained by running it with `--version`). `VERSION` is **generated by the build from the binary**, never hand-edited, so the two cannot drift *(unspecified in FS — decided here; FS §5.1 requires the file and names no consumer)* |
| Identity agreement | `launcher.config.json`'s `game_id` matches the game's registered id, and its last segment is the archive `<slug>` |
| Asset agreement | `game/assets/asset.manifest.json`'s `release` == the package's release id |
| Launcher floor | `launcher_min` ≤ the packaged launcher's version (§4.3) |
| File-index completeness | Every shipped path appears in the release manifest's `files`, and every path in `files` exists |

---

## 5. Build Pipeline

**Satisfies:** FR-LNCH-2, FR-UPD-1, FR-SCH-3.

### 5.1 Stages

The pipeline is a fixed, ordered sequence. Each stage fails loudly and refuses to produce output.

```
1.  Resolve inputs:            pkgroot, release id, platform, launcher binaries, signing identity
2.  Validate identity:         §4.4 cross-document consistency checks
3.  Collect the game tree:     game/ from pkgroot, launcher/ assembled, data/ skeleton created
4.  Scan for forbidden content: §2.4 — symlinks, save files, device names, VCS/editor metadata
5.  Generate manifests:        asset, engine, release (§6)
6.  Stage the package:         assemble <stage>/GameName/ with manifests in place
7.  Build the user package:    ZIP from the staged tree, deterministic order and timestamps
8.  Build the update archive:  tar.zst of game/ + launcher/ only (§9)
9.  Hash:                      SHA256SUMS over ZIP + tar.zst + manifest
10. Sign:                      SHA256SUMS.sig (GPG), platform code signing (§7, §10)
11. Publish:                   upload to the channel location, then update latest.json (§8)
12. Verify:                    re-download and re-verify every hash (§11)
```

Stages 1–6 are pure and produce no publishable artefact; stage 11 is the only irreversible step.

### 5.2 Determinism

For reproducible hashes (Launcher spec §25.2, FS §13.3), the build MUST:

- set every archive entry's mtime to a single fixed timestamp — the release's publication time, or the epoch *(unspecified in FS — decided here: the release's `published` timestamp)*;
- sort archive entries by path, byte-wise, before writing;
- normalise file modes to `0644` for files and `0755` for executables and directories;
- strip ownership and extended attributes;
- compress with a fixed level and a fixed zstd/tar implementation version, recorded in the build log;
- exclude any input whose contents vary between runs (timestamps embedded by a generator, absolute paths in debug output).

The build log MUST record the tool versions used, so a hash can be reproduced later or its irreproducibility explained.

### 5.3 Staging, Not In-Place

Manifests are generated into a staging directory, never written into `pkgroot/game/`. Rationale: the manifest's own hash would otherwise be self-referential, and a build must not dirty a developer's working tree.

The staging root MUST live on a filesystem with enough free space for the package, and MUST be discarded on failure.

### 5.4 Preconditions the Build Enforces

| Precondition | Failure |
|--------------|---------|
| `pkgroot/game/index.html` exists | Build error: the game tree is not shippable |
| `pkgroot/game/shell.js` exists | Build error |
| `launcher/launcher.config.json` exists and validates against `kobra.launcher-config/1` | Build error (FR-SCH-3) |
| A launcher binary exists for the target platform | Build error |
| The launcher binary's version ≥ `launcher_min` | Build error (§4.3) |
| The engine's WASM and glue paths named by `engine.manifest.json` exist | Build error |
| `game/engine/game.wasm` will be served as `application/wasm` by the launcher's `mime_types` | Build error (FR-AST-2); a wrong MIME type breaks `instantiateStreaming` |

---

## 6. Manifest Generation

**Satisfies:** FR-UPD-1, FR-SCH-3, FR-SCH-4, FR-SCH-5; FS Appendix B.

Three manifests ship inside `game/`. Two are generated; one may be authored.

### 6.1 `release.manifest.json` — Generated, Mandatory

Required by FR-UPD-1. The build generates it **after the update archive**, because the manifest declares that archive's identity (`archive.hash`, `archive.size`, §8.2.2) and its `files` index covers the other two manifests and every other shipped path. It is written into the staged `game/` — so it ships in the distribution ZIP — and copied byte-identically to `<slug>-<release>.release.manifest.json` for publication. It is **never** a member of the `.tar.zst` it describes (§1.2, §6.4).

| Field | Required | Generated value |
|-------|----------|-----------------|
| `schema` | Yes | `kobra.release-manifest/1` |
| `release` | Yes | The package's release id |
| `previous_release` | No | The release this one supersedes, or `null` for a first release |
| `published` | No | The publication timestamp, UTC RFC 3339 |
| `launcher_min` | Yes | From the launcher's declared minimum (§4.3), or the packaged launcher's version |
| `engine_version` | Yes | From `engine.manifest.json` |
| `save_version` | Yes | From `engine.manifest.json` |
| `channel` | No | `stable` \| `beta` \| `rc`; defaults to `stable` |
| `scope` | No | `full` \| `game-only` \| `patch`; defaults to `full` |
| `game_version` | Yes | The game's own content version (§4.5). **Schema amendment A3 required** |
| `total_size` | Yes | Sum of every `files[].size` — the **extracted** size, not the archive size (§8.2.3) |
| `archive` | Yes | The archive's `hash` and `size` (§8.2). **Schema amendment A2 required** |
| `files` | Yes | The file index (§6.4) |
| `migrations` | No | The save-migration chain (FR-SAVE-12) |
| `schema_migrations` | No | One-way document migrations (FR-SCH-4) |
| `min_browser` | No | Per-browser major-version floor (FR-COMPAT-1). The schema permits **only** `chrome`, `edge` and `opera`; Brave has no field. A game that needs a Brave floor sets it in `launcher.config.json`'s `min_browser_version` instead *(schema/FS discrepancy: the manifest cannot express it, so it stays a launcher-config setting)* |
| `signatures` | No | Detached signatures over the manifest (§7.4) |
| `notes`, `release_notes_url` | No | Human-facing release notes |

### 6.2 `asset.manifest.json` — Generated, Advisory Hashes

| Field | Required | Notes |
|-------|----------|-------|
| `schema` | Yes | `kobra.asset-manifest/1` |
| `release` | Yes | Must equal the package's release id |
| `generated` | No | Generation timestamp |
| `content_addressed` | No | Defaults `false`. When `true`, assets MUST be published under `assets/<content-hash>/…` and the launcher caches them immutably (Launcher spec §13.6) |
| `bundles` | No | Logical groupings for progressive download |
| `assets` | Yes | The asset index; hashes here are **advisory** — the launcher does not verify them per request |

Because asset hashes are advisory, `content_addressed: true` is the only mechanism that makes an asset immutability claim. A build that sets it MUST rewrite asset paths to include the content hash and MUST update every reference (HTML, CSS, JS, engine glue).

### 6.3 `engine.manifest.json` — Generated or Authored

| Field | Required | Notes |
|-------|----------|-------|
| `schema` | Yes | `kobra.engine-manifest/1` |
| `engine_version` | Yes | Must equal the release manifest's |
| `wasm` | Yes | Path within `game/` |
| `glue` | Yes | Path within `game/` |
| `save_version` | Yes | Must equal the release manifest's (FR-SCH-5) |
| `wasm_hash`, `glue_hash` | No | Recommended: lets the build detect a stale WASM against its glue |
| `release`, `required_features`, `min_browser`, `memory`, `notes` | No | Compatibility metadata |

A mismatch between the engine's `save_version` and the release manifest's is a **build failure**, not a warning (FR-SCH-5).

### 6.4 The File Index

`files` is the release's integrity backbone. Each entry requires `path`, `size` and `hash`.

- `path` MUST match `^(game|launcher)/(?!\.\.?(/|$))[A-Za-z0-9._-]+(/(?!\.\.?(/|$))[A-Za-z0-9._-]+)*$` — that is, a relative path under `game/` or `launcher/`, with a restricted character set and no dot segments. **A release never contains and never writes `data/`** (FR-UPD-7), and the schema enforces it structurally.
- `hash` MUST be `sha256:<64 lowercase hex>` over the file's exact bytes.
- `size` MUST be the file's exact byte length.
- `mode` MAY carry the POSIX mode as 3–4 octal digits; the build MUST set it for executables so the executable bit survives where the archive format carries it.

Generation order: enumerate the staged tree, sort by path, hash each file, then write the manifest. The manifest MUST list every shipped file except itself. The launcher rejects an archive member that its index does not list (Launcher spec §19.3, `internal/update/verify.go`), so an incomplete index is a release-blocking defect.

**Why the manifest is not one of those members.** "Except itself" and "rejects an
unlisted member" are compatible only because the manifest is not in the archive
it indexes. If it were, the index would have to contain the manifest's own hash,
which depends on the index — a fixed point that does not exist. The launcher
therefore treats a member named `game/release.manifest.json` as a specific,
named failure (`release_manifest_in_archive`) rather than reporting a generic
unsafe entry, and the build must not put it there. See §1.2 for the artefact
split and §9.1 for the archive's contents.

### 6.5 Asset Content Addressing and Bundling

*(Technical decisions. Both were unspecified; both are resolved in the direction
that requires no new launcher behaviour, verified against the shipped
implementation.)*

#### 6.5.1 Prefix length: 16 lowercase hex characters

When `content_addressed: true`, the build MUST rewrite every asset path to
`assets/<hash>/<original-relative-path>`, where `<hash>` is the **first 16
lowercase hex characters** of the asset's SHA-256.

Sixteen hex characters is 64 bits, which bounds a same-prefix collision across a
game's asset set at roughly 10⁻¹¹ even for a million assets — comfortably below
the probability of the download being corrupted by other means. It is also
exactly the length the launcher already treats as content-addressed: its static
handler marks a first path segment immutable when it is **at least 16** hex
characters. A longer prefix would still work; a shorter one would silently fall
back to the one-hour cache, defeating the purpose. Publishing the number here
removes that trap.

The build MUST also record the *full* hash in `asset.manifest.json`'s
`assets[].hash`, so the prefix in the path is a cache key and the full hash is
the integrity value.

#### 6.5.2 Per-asset cache hints are not honoured by the launcher

**Verified finding.** `asset.manifest.json` declares per-asset `cache`
(`immutable` | `revalidate` | `no-store`) and `mutable` fields, but the launcher
**never reads `asset.manifest.json` at all** — it is an engine/producer document.
Its static handler chooses a cache policy from the *request path* and the launcher
config:

| Path | Policy the launcher applies | Source |
|------|-----------------------------|--------|
| `/engine/*` | `public, max-age=31536000, immutable` | hardcoded namespace |
| `/assets/<16+ hex>/…` | `public, max-age=31536000, immutable` | path-shape detection |
| other `/assets/…` | `public, max-age=3600` | hardcoded namespace |
| `index.html`, `shell.js`, `locales/*`, `*.manifest.json` | `no-cache` | hardcoded namespace |

Therefore a publisher **MUST NOT rely on a per-asset `cache` hint to produce a
particular `Cache-Control` header.** To get immutable caching, name the path
content-addressed (§6.5.1) or put the file under `engine/`. The per-asset hint is
advisory metadata for tooling and human readers.

*(Implementing manifest-driven caching is a real feature with real cost — the
launcher would have to load, validate and index the asset manifest, and re-check
it on change. Least friction here is to document the actual behaviour and keep the
launcher's small, fully tested cache model. Tracked as a launcher enhancement, not
a packaging requirement.)*

#### 6.5.3 Bundling: producer-side, launcher-opaque

The launcher does not read manifests, so it cannot and does not resolve bundles:
a bundled asset simply **is** a file at the path the engine asks for. Bundling is
therefore entirely a producer-side decision with two rules and no launcher
impact:

- The build MAY bundle assets into `data/*.pak` entries (FR-AST-10 suggests
  < 32 KiB per bundled asset) and MUST list each bundle in `assets`/`bundles`
  with its hash.
- The build SHOULD keep the total entry count under a few thousand, because
  extractors handle thousands of tiny entries slowly and the ZIP mechanism
  notices (§3.5). This is the only reason to bundle.

**Progressive download is explicitly out of scope.** It contradicts the
architecture's offline-first, single-player, own-your-copy model (FS §1, FS §3):
a game the user owns outright should be complete on disk after extraction. The
`bundles` array remains available for entry-count reasons alone.

### 6.6 Migrations

- `migrations` is an ordered chain of save migrations. Each entry requires `from`, `to` and `fn` (an exported function name matching `^[A-Za-z_][A-Za-z0-9_.]*$`), and MAY set `reversible` (default `false`). The schema cannot express chain contiguity, so the **build's semantic validator** MUST check it: the steps MUST form a contiguous chain from the oldest supported `save_version` to the release's, with no gaps, and the highest `to` MUST equal the release manifest's `save_version`. A gap is a build failure (FR-SAVE-12, FR-SCH-6).
- `schema_migrations` records one-way document upgrades. Each entry requires `manifest` (one of `asset-manifest`, `engine-manifest`, `launcher-config`, `mod-manifest`), `from` and `to`, with `from ≥ 1` and `to ≥ 2` (FR-SCH-4). Each MUST be documented, and the pre-migration file MUST be preserved.
- A release that raises `save_version` without extending `migrations` is a **build failure**.

---

## 7. Integrity, Checksums, and Signing

**Satisfies:** FR-LNCH-5, FR-LNCH-3; FS §13.3, §13.4; Launcher spec §19.3.

### 7.1 Mandatory Hashes

| Artefact | Requirement |
|----------|-------------|
| Every file in the package | SHA-256 in `release.manifest.json`'s `files` (mandatory) |
| Every asset | SHA-256 in `asset.manifest.json` (advisory) |
| The public ZIP and the update archive | SHA-256 in `SHA256SUMS` |
| `SHA256SUMS` | Detached GPG signature in `SHA256SUMS.sig`, where signing authority exists |

`SHA256SUMS` MUST use the conventional `<hex><space><space><filename>` format so that `sha256sum -c SHA256SUMS` works on Linux and `shasum -a 256 -c` on macOS. It MUST name only files published alongside it.

### 7.2 The Three Independent Verifications

A user, a mirror, and the launcher verify different things, and none of them substitutes for another:

| Verifier | Verifies | Detects |
|----------|----------|---------|
| The launcher, at update time | `release.manifest.json`'s `files` index against the extracted archive, plus the declared total size | Corruption in transit, a tampered archive, zip bombs (FR-UPD-5) |
| The user, before extracting | `SHA256SUMS` against the downloaded ZIP, optionally `SHA256SUMS.sig` against the team's GPG key | A tampered download (FR-LNCH-5) |
| The OS, at launch | Authenticode on Windows, Developer ID + notarisation on macOS | An untrusted or modified binary (FS §13.3) |

### 7.3 Transport Trust Is Not Integrity

The manifest's `files` index protects a *release* against corruption and tampering *within the archive*. It does not authenticate the publisher, because the manifest travels with the archive. Publisher authentication therefore requires one of:

1. a detached GPG signature over `SHA256SUMS` (FR-LNCH-5);
2. a signature over the manifest itself, carried in `release.manifest.json`'s `signatures` object as `gpg` (armored detached) with `gpg_key_fingerprint`, and/or `sigstore_bundle`;
3. platform code signing of the launcher binary (§10).

### 7.4 Manifest Signatures

When `signatures.gpg` is present, `signatures.gpg_key_fingerprint` MUST be set to the 40-hex uppercase fingerprint of the signing key, and the signature MUST cover the manifest document exactly as published.

**Known limitation, stated plainly.** The launcher's current implementation **fails closed**: no GPG verifier ships with it, so a manifest that declares a signature is **refused** rather than trusted (Launcher spec §19.3 step 2 deferral, documented in `internal/update/verify.go`). Consequences for packaging:

- A release whose manifest declares a signature MUST also publish `SHA256SUMS` and `SHA256SUMS.sig`, so the user can verify by hand.
- Until a verifier is installed in the launcher, a signed-manifest release cannot be applied by the launcher-assisted patch path. Publishing `signatures` MUST therefore be paired with either (a) implementing the verifier, or (b) setting the update channel to `manual` for that release.
- No release may rely on an unsigned transport as its only integrity claim.

### 7.5 What Must Never Be Signed Over

A signature MUST NOT be taken over a file that is generated after signing (the ZIP, if the ZIP embeds the manifest), over a directory listing, or over a checksum file that names files not published in the same release. Order of operations is fixed: **generate → hash → sign → publish → verify**.

---

## 8. Channels and Publishing

**Satisfies:** FR-UPD-1, FR-UPD-2; Launcher spec §19.2, §27.7.

### 8.1 Two Channel Axes, and Why They Are Not the Same Thing

Two unrelated settings both use the word "channel", and no source document maps
them to each other:

| Axis | Values | Owner | Meaning |
|------|--------|-------|---------|
| `release.manifest.json` → `channel` | `stable` \| `beta` \| `rc` (default `stable`) | Publisher | What kind of release this is, for humans and release tooling |
| `launcher.config.json` → `update.channel` | `manual` \| `patch` \| `none` (default `manual`) | Per-game config | *Which update mechanism* the launcher may use |

They are orthogonal: a `beta` release can be delivered by the `manual` path or
the `patch` path. This document uses them as follows.

- **`channel` is a label, not a routing decision.** Nothing in the launcher reads
  it. It MUST NOT be used to decide whether an update is offered, because the
  launcher has no channel setting to compare it against; doing so would require a
  client-side opt-in that does not exist.
- **A pre-release `channel` MUST be published to a different base URL** (a
  distinct `patch_base_url` per game build), because that is the only mechanism
  available for separating audiences. Publishing an `rc` to the same `latest.json`
  as `stable` offers it to every user (FR-UPD-2 governs *when* a check happens,
  not *who* is eligible).
- **`update.channel` keeps its implemented meaning** (Launcher spec §27.7):
  `manual` = the launcher assists with discovery but the user replaces files from
  the ZIP; `patch` = launcher-assisted download, verify, extract and swap;
  `none` = no update surface at all.
- Until the deployment model distinguishes stable from pre-release audiences,
  **this document requires every published `stable` release to be the newest
  release at its base URL**, and any `beta`/`rc` release to live at a base URL no
  `stable` client is configured with *(unspecified in FS — decided here)*.

### 8.2 Archive Identity and Integrity in the Manifest

**Decided.** The release manifest is the release's authority document, and it MUST
identify the archive it describes. A manifest that cannot name its own archive
cannot be verified offline by anyone — which is incompatible with a
distribution model built on publisher control and user-owned copies.

#### 8.2.1 The defect this fixes

`release.manifest.json` as vendored has **no property** naming the archive, its
URL, or its hash, yet Launcher spec §19.3 step 3 requires the launcher to verify
"the downloaded archive's SHA-256 against the manifest's declared hash". The
implemented launcher tolerates the gap in two ways, and the second is a genuine
weakness:

- it accepts an optional `archive` object and a flat `archive_sha256` alias; and
- **it silently continues when neither is present**, falling back to the
  per-file `files[]` index and `total_size`.

A fallback that is invisible to the publisher is not a design. It means a
publisher who follows the schema exactly gets weaker verification than one who
happens to know about the launcher's extension, and neither party learns which
they got.

#### 8.2.2 Required schema amendment

`release.manifest.schema.json` MUST gain an `archive` object, and `files[]` MUST
NOT be treated as a substitute for it:

```json
"archive": {
  "type": "object",
  "additionalProperties": false,
  "required": ["hash", "size"],
  "description": "The archive this manifest describes (FR-UPD-5, Launcher spec §19.3 step 3). Required so that a downloaded archive can be verified as a whole, including truncation, before extraction begins.",
  "properties": {
    "name":   { "type": "string", "description": "Base filename as published." },
    "url":    { "type": "string", "format": "uri", "description": "Absolute, or relative to the manifest's own location. https only." },
    "size":   { "type": "integer", "minimum": 1, "description": "Exact compressed archive size in bytes. This is NOT total_size, which is the extracted size." },
    "hash":   { "$ref": "#/$defs/hash", "description": "SHA-256 of the archive's exact bytes." },
    "format": { "enum": ["tar.zst"], "description": "Archive container the launcher must extract." }
  }
}
```

with the shared definition, matching every other hash in the product:

```json
"$defs": {
  "hash": { "type": "string", "pattern": "^sha256:[0-9a-f]{64}$" }
}
```

Two decisions are embedded here and both are deliberate:

- **`hash` carries the `sha256:` prefix**, matching `$defs/hash` in the save,
  engine and asset schemas. This is not a stylistic preference: it was verified
  against the implementation. `internal/update`'s `parseHash` accepts
  `sha256:<64 hex>` and **rejects a bare 64-character hex string** as "not a
  sha256 hash", while `declaredArchiveHash` passes the raw field through
  untouched. A publisher who emitted bare hex would therefore compute the correct
  bytes and have the launcher reject the manifest as malformed — the worst
  outcome, because the file is right and the release is unappliable. One format
  everywhere, and it is the prefixed one.
- **`hash` and `size` are required; `url` is not.** A publisher who distributes
  by other means (a storefront upload, a mirrored CDN, a hand-delivered file)
  still publishes a manifest that verifies. `url` is a convenience for the
  assisted patch path, not an integrity claim.

#### 8.2.3 The build MUST emit it, and verification MUST fail closed

1. The build MUST write `archive.hash` and `archive.size` for every published
   archive, computed over the file as published, **before** signing the manifest.
2. A manifest without `archive.hash` is a **build failure**. There is no
   supported release that omits it.
3. The launcher MUST treat a missing `archive.hash` as a hard failure of the
   assisted patch path, not as a silent downgrade to per-file verification.
   *(Implemented: `internal/update/verify.go` refuses a manifest that declares
   neither `archive.hash` nor the `archive_sha256` alias, with the reason
   `archive_hash_missing` and a message that blames the release rather than the
   user's download. The packaging pipeline is expected to catch this first —
   the packaging pipeline's own `verify_output` does, as `pack.verify.fail`.)*
4. `archive.size` and `total_size` are **different quantities** and MUST NOT be
   confused: `archive.size` is the compressed archive's byte length, `total_size`
   is the sum of extracted file sizes (§6.1). A mismatch in either is a
   distinct failure with a distinct message.

#### 8.2.4 Verification chain this enables

With the field present, a user who trusts nothing but a public key can verify a
release end to end with no network access to the publisher and no trust in the
transport:

```
fetch release.manifest.json (+ signatures.gpg)
  → verify the manifest signature against the published fingerprint
    → read archive.hash from the (now authenticated) manifest
      → hash the downloaded archive and compare
        → launcher extracts and verifies every files[] entry and total_size
```

The manifest authenticates the archive; the `files[]` index authenticates the
contents; the signature authenticates the manifest; the code signature
authenticates the binary (§10). That chain is the whole point of a
publisher-controlled distribution, and it does not close without §8.2.2.

### 8.3 Publication Locations


The publisher MUST provide, at a stable base URL (`patch_base_url` in `launcher.config.json`):

```
<base>/latest.json                                  # the pointer document
<base>/<slug>-<release>.release.manifest.json       # the manifest
<base>/<slug>-<release>-<platform>.tar.zst          # the update archive
<base>/<slug>-<release>-<platform>.zip              # the user-facing package
<base>/SHA256SUMS                                   # checksums
<base>/SHA256SUMS.sig                               # detached signature (where available)
```

### 8.4 The `latest.json` Contract


`latest.json` is the only document the launcher reads to discover an update. It MUST be small and MUST resolve every URL absolutely or relative to its own location:

```json
{
  "release": "2026.10.1",
  "manifest_url": "stardrifter-2026.10.1.release.manifest.json",
  "notes_url": "https://kobra.games/stardrifter/notes/2026.10.1"
}
```

Rules:

- `release` MUST match the manifest it points at.
- `manifest_url` MUST use `https`, or be relative to the base URL.
- Updating `latest.json` is the **last** publishing step and the rollback lever: reverting it to the previous release withdraws an update without deleting any artefact.
- `latest.json` MUST NOT be cached for longer than a few minutes; publish it with a short cache lifetime so a withdrawn release stops being offered promptly.
- The launcher MUST NOT poll it in the background (FR-UPD-2): it is fetched only when the user asks.

### 8.5 The Launcher's Update Surface


The publisher depends on two launcher endpoints, specified only in the Launcher
spec and absent from the FS and from `data-api.schema.json`:

| Endpoint | Method | Semantics |
|----------|--------|-----------|
| `/api/update/check` | `GET` | Session-authenticated, **user-initiated only**. Reads `game/release.manifest.json`, fetches `<base>/latest.json`, compares releases, returns `{current, latest, available, notes_url}`. It does **not** download the patch (Launcher spec §19.2). Fails silently when offline (FR-UPD-2) |
| `/api/update/apply` | `POST` | Starts the download, verify, extract and swap. Rate-limited to one per session. Requires the `update.channel: patch` mechanism |

Packaging consequences:

- The publisher MUST ensure `GET /api/update/check` returns a truthful
  `current` value, which means `launcher.config.json`'s `release` and
  `game/release.manifest.json`'s `release` MUST agree with the installed package
  (§4.4). A mismatch makes the launcher report a phantom update.
- Because the check is user-initiated and fails silently, **an unreachable update
  server is not a support incident** and MUST NOT be surfaced as an error dialog.
- A release published with `update.channel: manual` MUST still ship a working
  `latest.json` and manifest, because the check endpoint is the discovery path
  even when the launcher does not apply the patch.

### 8.6 Release Signatures: Inverted Trust, Two Layers

*(Technical decision — resolves the open question about the launcher's GPG
verifier by removing the need for one. Chosen to keep the dependency set at three
vendored modules and to serve a publisher who hosts their own releases.)*

The tension was: Launcher spec §19.3 step 2 requires verifying a manifest's GPG
signature when `signatures.gpg` is present, but Go's standard library has **no**
OpenPGP implementation and `x/crypto/openpgp` is deprecated and unmaintained.
Adding a third-party OpenPGP parser is exactly the kind of dependency that brings
a decade of parser CVEs into a program whose job is reading untrusted archives —
significant technical debt for a check that is not the primary trust anchor.

**Decision: invert the trust model instead of adding a parser.**

The publisher is the key holder. The publisher hashes the archive; the manifest
is the authenticated root; the manifest carries `archive.hash` (§8.2). Therefore:

1. **Signing is the publisher's job, done with the publisher's own GPG key.**
   The build MUST emit `SHA256SUMS` and MUST emit `SHA256SUMS.sig` when the
   publisher has a signing key (§7.1). It MUST also populate
   `release.manifest.json`'s `signatures.gpg` with an armored detached signature
   over the exact manifest bytes, plus `signatures.gpg_key_fingerprint`.
2. **The user verifies before extracting**, with a tool they already have:
   `gpg --verify SHA256SUMS.sig SHA256SUMS`, then `sha256sum -c SHA256SUMS`.
   This is the FR-LNCH-5 path, and it needs nothing from the launcher.
3. **The launcher does not verify GPG, and MUST NOT pretend to.** It continues to
   **fail closed**: a manifest that declares `signatures.gpg` with no verifier
   installed is refused with the E29-adjacent message, rather than silently
   trusted.
4. **A conforming launcher build MAY install a verifier** via the existing
   `update.SetSignatureVerifier` hook. If it does, the verifier MUST be a vetted
   implementation, and its absence MUST degrade to rule 3, never to acceptance.
5. **A publisher who wants in-launcher verification signs the manifest and
   installs a verifier.** Otherwise the `manual` channel (which is the default and
   the baseline, FS §12.2A) is unaffected: the launcher never needs to verify
   anything, because the user replaces files themselves.

   **Amended for the assisted patch path.** Signing is **recommended, not
   required**. Rule 3's fail-closed behaviour means a manifest that declares a
   signature is refused by a launcher with no verifier installed, so a signed
   patch release is unappliable by exactly the build it is meant for. The
   `patch` channel therefore accepts an **unsigned** manifest. The integrity
   claim this leaves is stated rather than implied, and MUST be published
   wherever the channel is described:

   > With `update.channel: patch`, the launcher verifies that the downloaded
   > archive matches the hash in the release manifest. This detects corruption,
   > a truncated download, and a mirror that served a different file. It does
   > **not** prove who published the release: whoever controls the update server
   > controls both the manifest and the archive. If that server is compromised,
   > so is the update.

   Packaging MUST NOT publish `signatures.gpg` on a release intended for the
   assisted patch path unless the shipping launcher build installs a verifier
   through `update.SetSignatureVerifier`. The build gate of §11.2 enforces this.
   A verifier, when installed, MUST be a vetted implementation, and its absence
   MUST degrade to rule 3's refusal, never to acceptance. Updater spec §13 is the
   launcher-side contract; §15.3 below is the corrected channel decision.

**Consequence for the channels.** The `manual` channel remains the baseline and
`patch` the convenience, and the `manual` channel requires no in-launcher
cryptography at all, so the launcher keeps its three-dependency footprint and its
small attack surface. What changed with the amendment above is only the *pricing*
of the patch path: it no longer requires a verifier the project has decided not to
build, so a publisher who cannot sign can still ship one-click updates — at the
cost of publisher authentication, which the published warning states plainly.

**What the publisher MUST publish out of band:** the signing key's fingerprint
(uppercase, 40 hex — the schema's `gpg_key_fingerprint` pattern) on the download
page and in the release notes, because a signature is worthless if the key is
only discoverable from the thing being signed.

**Deliberately not built:** a signing tool, a keyring manager, or a
launcher-side GPG implementation. Source-verified releases use `gpg` on the
maintainer's machine and `gpg` on the user's, which is the least friction that
does not import a parser.

### 8.7 Mirrors and CDNs

Mirrors MAY serve the archives and `SHA256SUMS` but MUST NOT re-sign, MUST NOT serve a modified `latest.json`, and MUST be listed in the release record. A mirror's copy is trustworthy only via `SHA256SUMS` (§7.2).

### 8.8 Retention

| Artefact | Retention |
|----------|-----------|
| `latest.json` | Current release only |
| Current + previous release archives and manifests | Indefinitely: these are the rollback path (FR-UPD-8, FR-UPD-9) |
| Older releases | At least until no supported `previous_release` chain references them; keep the migration chain intact |
| `SHA256SUMS`, `SHA256SUMS.sig` | As long as the artefacts they describe |

Deleting an artefact that a published `previous_release` chain references is forbidden: it breaks rollback for users who hold that release.

---

## 9. Update Artefacts

**Satisfies:** FR-UPD-1, FR-UPD-4, FR-UPD-7; Launcher spec §19.1, §19.4.

### 9.1 What an Update Archive Contains

An update archive is a **zstd-compressed tar** containing exactly:

```
game/…        # the whole tree as shipped, EXCEPT game/release.manifest.json
launcher/…    # only when the launcher changed
```

It MUST NOT contain `data/`, `README.txt`, `LICENSES/`, or
`game/release.manifest.json`. The launcher rejects any entry outside `game/` and
`launcher/`, any symlink or hardlink, any entry whose first segment is `data/`
(FR-UPD-7), and — by the same index rule — any member the manifest does not
list, which is why the manifest itself cannot be one (§1.2, §6.4). The manifest
above describes this archive; it is never *in* it.

### 9.2 Full Versus Patch

| `scope` | Content | Use |
|---------|---------|-----|
| `full` | Every file in `game/` (+ `launcher/` if changed) | Default; always safe |
| `game-only` | Every file in `game/`, never `launcher/` | Content changes that need no launcher change |
| `patch` | Changed and added files only, keyed by the source release | Smaller downloads; requires a `previous_release` to patch from |

A `patch` release MUST declare `previous_release`: the schema enforces this with an `allOf` clause, so a patch manifest without it does not validate. The publisher MUST publish a patch for every supported source release — or ship a `full` archive alongside, because a user two releases behind cannot apply a single patch.

### 9.3 The Swap Protocol the Package Must Tolerate

The launcher's update is a crash-safe directory swap (FR-UPD-4, Launcher spec §19.4):

```
update.state written →  game → game.old  →  game.new → game  →  update.state removed
```

Packaging consequences:

- The archive MUST be self-contained under `game/`: the swap replaces the whole directory, so a file omitted from the archive is a file deleted from the install.
- `launcher/` MUST be replaced in place, not swapped away: the running binary must survive its own release.

  **Amendment (Updater spec §14).** Because `launcher/launcher.config.json` is a
  member of the archive and also holds machine-local state, the launcher merges
  the archive's copy with the installed one rather than overwriting it: local
  values win, release-owned values (notably `release` itself) come from the
  archive. A package author MUST therefore assume that the config file it ships
  is a *default* for a fresh install, not the final state of an upgraded one.
  Concretely, a release MUST NOT rely on its `update.channel` value taking effect
  on an existing install, and MUST NOT use the config file to withdraw a setting
  the user has chosen.
- `game.old/` is retained until the new release has started successfully **twice** and then removed (FR-UPD-8). A release that fails on the second start will be rolled back, so packaging MUST NOT depend on one-shot first-run behaviour.
- Because rollback restores `game/` from `game.old/` and never touches `data/`, a newer save stays untouched and unreadable by the older build (FR-UPD-9). The package MUST NOT ship a "downgrade migration".

### 9.4 Update Experience Requirements

*(Resolves the open question about update copy, progress and cancellation.)*

> **Status amendment (Updater spec §19.1).** The launcher applies updates at the
> **next start**, not in the running session: the user's action records the
> request, and the following launch downloads, verifies, extracts and swaps before
> the server binds. Requirements **2, 3, 6, 7 and 8** below are therefore
> **deferred, not satisfied**, until the shell update panel exists. What ships is
> the substrate: the download size is in the `202` response, the phase and byte
> counts are recorded, and the outcome and the retained backup are readable
> through `GET /api/update/progress`. Requirements **1, 4, 5 and 9** hold as
> written. The cost of the deferral is that a user cannot watch the download and
> learns the result on the next launch; that is a real regression against this
> section and is recorded as such rather than dropped.

The shell drives the update UI; the launcher supplies the facts. Certain
behaviours are requirements rather than polish, because they are what makes a
self-hosted, publisher-controlled update trustworthy:

| # | Requirement | Rationale |
|---|-------------|-----------|
| 1 | **No background checks.** The check happens only on an explicit user action (FR-UPD-2). The UI MUST NOT poll, and MUST NOT show an update badge the user did not ask for | Offline-first is a product promise, not a default |
| 2 | **Confirm before downloading.** The user sees the current release, the available release, and the download size, and chooses. §3.4 requires the size to be known, so it MUST be shown | The user may be on a metered or small device |
| 3 | **Progress is reported** in bytes and percent while downloading, and the UI states that canceling is safe | An update that appears hung gets killed mid-write |
| 4 | **Cancellation is safe and complete.** A canceled download MUST leave the install untouched; a partial archive MUST NOT be left where the launcher could mistake it for a complete one | §9.5; the swap is crash-safe, so a cancel is equivalent to a crash before the swap begins |
| 5 | **Failure is specific and actionable**, using the FS §16 error texts verbatim: E27 "The last update didn't finish.", E28 "The update file is damaged.", E29 "This update needs a newer launcher.", E30 "The update was rejected because it would modify your saves.", E32 "There isn't enough free space to install this update.", E34 "The update was cancelled.", E35 "The update changed since you confirmed it. Check for updates again.", E36 "The update could not be downloaded. The launcher may be offline." | FR-I18N ×support: the user must be able to act without a support ticket |
| 6 | **Release notes are shown before the update is applied**, from `release.manifest.json`'s `notes` or `release_notes_url` | The user decides to update based on what changed |
| 7 | **The result is stated plainly**: the new release id, and that saves were not touched | FR-UPD-7 is the product's central promise; say it |
| 8 | **Rollback is offered after a failed start**, not buried. FR-UPD-8 retains `game.old` until the new release has started twice; while it is retained, the UI MUST offer "Revert to previous version" | FR-UPD-8's retention window is the only rollback the user has |
| 9 | **The launcher never downloads without an explicit action in the current session** | FR-UPD-2; the `POST /api/update/apply` rate limit of one per session is a backstop, not the control |

Requirements 1, 5, 7 and 8 are the ones a solo developer is most likely to omit,
because they are invisible until a user is confused or a release misbehaves.

### 9.5 `launcher_min` and the Refusal Path

A release whose `launcher_min` exceeds the user's launcher is **refused**, with the message *"This update needs a newer launcher."* and a download link (E29, FR-UPD-6). The user's current release keeps working. Packaging MUST therefore distribute launcher upgrades through the ZIP, not only through the update archive: a launcher that is too old by definition cannot update itself through the patch path alone.

---

## 10. Per-Platform Packaging

**Satisfies:** FR-LNCH-2, FR-LNCH-3, FR-LNCH-4, FR-LNCH-5; FS §13.2, §13.3.

| Platform | Binary | Signing | Package notes |
|----------|--------|---------|---------------|
| Windows x64 | `launcher/launcher.exe` | Authenticode with an **EV certificate** (or OV + Microsoft submission) for immediate SmartScreen reputation (FS §13.3) | Document **More info → Run anyway**; MOTW is inherited from the ZIP and can add an "Open File" dialog even when signed. The launcher MUST NOT attempt to bypass SmartScreen |
| Windows arm64 | `launcher/launcher.exe` | As above | Same package rules |
| macOS (universal) | `launcher/launcher` | Developer ID Application + hardened runtime + `notarytool` + **staple**; needs the network-client entitlement for the loopback listener and no sandbox entitlement (FS §13.3) | Ship a `.app` inside the ZIP or a `.dmg`; on Apple silicon the binary MUST be arm64 or universal. Document right-click → Open for a failed-notarisation case |
| Linux x64 / arm64 | `launcher/launcher` | None | Ensure the executable bit survives: README MUST include `chmod +x launcher`. Ship a `.desktop` file and an icon. Document `noexec`/read-only USB media, where the launcher runs degraded (FS §13.3) |

### 10.1 What the README Must State

Every package's `README.txt` MUST carry, in plain language:

| Statement | Reason |
|-----------|--------|
| The download size and the extracted size | The user chooses the target device (§3.3) |
| The free-space requirement, including headroom for saves and an update's `game.new/` staging copy | An update momentarily needs room for two copies of `game/` (Launcher spec §19.4) |
| The recommended filesystem, and the caveat that some filesystems and old extractors cannot handle a file of 4 GiB or more | Documentation, not enforcement (§3.3) |
| `chmod +x launcher` for Linux and macOS | FS §13.3: ZIP does not preserve the executable bit reliably |
| The install path requirement: the game folder must be writable, and `noexec`/read-only media run degraded | FS §13.3 |

The README MUST NOT promise a size the package does not have, and MUST NOT
instruct the user to work around a limit that this document does not impose.

### 10.2 Executable Bit

ZIP does not preserve the executable bit reliably across extractors and platforms (FS §13.3). Therefore:

- the build MUST set the mode in the release manifest's `files[].mode` where the archive format carries it;
- the README MUST tell the user `chmod +x launcher` for Linux and macOS;
- the build MUST NOT depend on the bit surviving.

### 10.3 `.desktop` File

*(unspecified in FS — decided here.)* A Linux package SHOULD include `GameName.desktop` at the package root, with:

```
[Desktop Entry]
Type=Application
Name=<game_name>
Exec=./launcher/launcher
Path=<install dir, left for the user to adjust>
Terminal=false
Categories=Game;
```

The `.desktop` file MUST NOT hard-code an absolute path, because the package is copy-and-run.

### 10.4 Cross-Building

Windows and Linux packages are cross-compiled from Linux with `CGO_ENABLED=0` (Launcher spec §3.3, §25.1). macOS packaging requires a macOS runner for `codesign`, `notarytool` and `stapler`. The build MUST fail rather than emit an unsigned macOS binary when the target is macOS and a signing identity is required.

---

## 11. Release Checklist and Gates

### 11.1 Pre-Build

- [ ] Release id chosen, unused, and recorded
- [ ] `launcher_min` decided and ≤ the packaged launcher's version
- [ ] `save_version` and the migration chain agreed; chain contiguous if raised
- [ ] Release notes written and `release_notes_url` live

### 11.2 Build-Time Gates (automated, all blocking)

The gates include the signature/channel consistency rule of §8.6: a release whose
manifest carries `signatures.gpg` MUST NOT declare an `update.channel` of `patch`
unless the launcher binary being packaged installs a signature verifier. Without
the gate a publisher can produce a release that the assisted path is guaranteed to
refuse, which is the packaging equivalent of the self-inflicted brick §4.3 already
guards against for `launcher_min`.

- [ ] Game tree present: `game/index.html`, `game/shell.js`
- [ ] `launcher.config.json` validates against `kobra.launcher-config/1`
- [ ] Cross-document consistency passes (§4.4)
- [ ] No forbidden content (§2.4): no saves, no symlinks, no device names, no VCS/editor metadata
- [ ] Manifests generated; `files` index complete and hashes verified
- [ ] `.wasm` served as `application/wasm` by the packaged config (FR-AST-2)
- [ ] Archive builds deterministically: two builds compare byte-identical

### 11.3 Post-Build, Pre-Publish

Two checks that follow from the launcher's execution model:

- The archive MUST list `launcher/launcher.config.json` whenever `release` changed,
  because the launcher reads the installed release from it after a swap: under
  `scope: full` the archive deliberately omits `game/release.manifest.json`
  (§9.1), so the config is the release-of-record on the installed folder.
- The config file MUST NOT be relied upon to deliver a *changed* `update.*`
  setting to an existing install; the merge of §9.3 preserves the local value.

- [ ] Extract the ZIP into a **clean** directory on each target OS and run it: the game starts, the browser opens, `/api/state` reports `data_writable: true`
- [ ] No `data/` file other than `.keep` in the package
- [ ] `SHA256SUMS` verifies against the published files
- [ ] `SHA256SUMS.sig` verifies against the team's GPG key
- [ ] Windows: SmartScreen outcome recorded (clean, or the documented *More info → Run anyway* path)
- [ ] macOS: `spctl --assess` and `stapler validate` pass on a clean machine
- [ ] Linux: executable bit set, `.desktop` file present, `chmod +x` documented
- [ ] Update path exercised: previous release → this release, including a mid-swap kill that recovers (FR-UPD-4)
- [ ] Rollback exercised: `game.old` restored, saves untouched (FR-UPD-9)

### 11.4 Publish

- [ ] Upload archives, manifests, `SHA256SUMS`, `SHA256SUMS.sig` to every mirror
- [ ] Re-download from the public URL and verify every hash
- [ ] Publish `latest.json` **last**
- [ ] Announce

### 11.5 Post-Publish

- [ ] Withdraw by reverting `latest.json` if a defect is found; never delete a referenced artefact (§8.5)
- [ ] Record the release in the release log with its manifest hash and tool versions

---

## 12. Schema Amendment Backlog

This document requires several changes to the vendored schemas. They are
collected here so they can be applied in one pass rather than discovered one at a
time. Each is normative for a packaging pipeline; the schemas are the authority
for validation, so a pipeline cannot implement them until the schema agrees.

| # | Document | Amendment | Required by |
|---|----------|-----------|-------------|
| A1 | `release.manifest.schema.json` | **Applied (v1.1).** Add `$defs.hash` = `{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}`. The schema currently inlines that pattern in `files[].hash` and references a `$defs/hash` that does not exist, so an amendment that copies the idiom from the save/engine/asset schemas would point at nothing | §8.2.2 |
| A2 | `release.manifest.schema.json` | **Applied (v1.1).** Add the `archive` object: `hash` and `size` **required**, `name`/`url`/`format` optional. Without it a publisher cannot declare an archive hash. The launcher now *fails closed* rather than downgrading (v1.1), and the field is optional in the schema only so that a document written before the amendment still validates — the packaging pipeline is what requires it, and the launcher enforces it on the download path | §8.2.2 |
| A3 | `release.manifest.schema.json` | **Applied (v1.1).** Add `game_version` (`^[0-9]+\.[0-9]+\.[0-9]+$`). The version axis has no home in any schema today | §4.5 |
| A4 | `release.manifest.schema.json` | Add `"mod-manifest"` to `schema_migrations.items.properties.manifest`'s enum. The enum already lists `mod-manifest`; if a future revision narrows it, the mod-migration path breaks. *(Verified: the enum currently includes it — this row guards the regression rather than fixing a defect)* | §6.6 |
| A5 | `release.manifest.schema.json` | Correct prose citations that point at the wrong requirement: `signatures` cites FR-LNCH-3 (should be FR-LNCH-5); `files` cites FR-AST-6 for advisory hashes (should be FR-AST-4). The *behaviour* in the schema is right; only the citations mislead | §1.3 |
| A6 | `launcher.config.schema.json` | Add `release.manifest` policy if per-game manifest placement is ever needed beyond the fixed `game/release.manifest.json`. *(Not required today; listed so the option is not lost)* | §8.5 |
| A7 | `engine.manifest.schema.json` | Correct `required_features`' citation (E1/E12 → E2) | §1.3 |
| A8 | Any schema using a negative lookahead | Replace `(?!\.\.?(/|$))` with an RE2-compilable equivalent, or accept that Go tooling cannot validate those documents | §12 A8, below |

**A8 is the one that blocks a Go build.** Five schemas — `release`, `asset`,
`engine`, `mod` manifests and `save` — cannot be compiled by
`github.com/santhosh-tekuri/jsonschema/v5` because lookahead is not expressible
in Go's `regexp`. A packaging pipeline written in Go therefore cannot delegate
validation to those schemas, and must implement the rel-path rules itself. The
cheapest fix that preserves the schemas' intent is to express the rule without
lookahead:

```json
"pattern": "^(game|launcher)/([A-Za-z0-9._-]|(?<![.][.]))+(/[A-Za-z0-9._-]+)*$"
```

is **not** the answer either — Go has no lookbehind. The rule must be enforced by
a **semantic validator** (which FR-SCH-6 already requires for the migration
chain), not by `pattern`. The schema SHOULD keep a permissive
character-class pattern and the build MUST apply the dot-segment and
`data/`-exclusion rules in code. That is more honest than a pattern that no
conforming validator can compile.

## 13. Handoff to the Launcher and Verification

### 13.1 What the Launcher Requires of a Package

| Launcher expectation | Where it is satisfied |
|---------------------|-----------------------|
| `launcher/launcher.config.json` exists and validates | §3.1, §5.4 |
| `game/index.html` exists | §3.1, §5.4 |
| `data/` exists or can be created | §2.5 |
| A browser meeting the version floor | `min_browser` / `min_browser_version` (§6.1) |
| `release.manifest.json` with a complete `files` index | §6.1, §6.4 |
| Nothing under `data/` in an update archive | §9.1 |
| `launcher_min` ≤ the installed launcher | §4.3, §9.4 |

### 13.2 Verifying a Package Without the Launcher

A support engineer, a mirror operator, or a curious user can verify a published package with standard tools:

```sh
# 1. Publisher authenticity (optional, strongest)
gpg --verify SHA256SUMS.sig SHA256SUMS

# 2. Transport integrity
sha256sum -c SHA256SUMS

# 3. No saves were shipped
unzip -l stardrifter-2026.09.1-linux-x64.zip | grep -E 'data/' | grep -v '\.keep$'   # must print nothing

# 4. Manifests agree
unzip -p stardrifter-2026.09.1-linux-x64.zip '*/game/release.manifest.json' | head

# 5. Every file in the index matches the archive
# (the build's own verify stage; see §11.2)
```

### 13.3 Version Comparison

`release.manifest.schema.json` states that ordering is **lexicographic on the
zero-padded components**. That holds for the year and month, which the pattern
`^[0-9]{4}\.[0-9]{2}\.[0-9]+$` fixes at 4 and 2 digits. The trailing counter `N`
has **no fixed width**, so lexicographic and numeric comparison agree only while
`N` stays within a single digit width relationship:

| Pair | Lexicographic | Numeric | Agree? |
|------|---------------|---------|--------|
| `2026.09.9` vs `2026.10.1` | `09… < 10…` → first is older | same | yes |
| `2026.09.9` vs `2026.09.10` | `"9" > "1…"` → second is older | `9 < 10`, second is newer | **no** |
| `2026.09.9` vs `2026.09.100` | `"9" > "1…"` → second is older | `9 < 100`, second is newer | **no** |

So: consumers MAY compare lexicographically, as the schema says and as the
launcher does; but any tooling whose job is to *order* releases — a release
index, a "newest N" page, a mirror pruner, a rollback chooser — MUST parse the
segments and compare them as integers, because the schema's own rule is only
sound for the year and month.

Build tooling MUST therefore:

- emit `N` without leading zeros (`2026.09.9`, never `2026.09.09`), so no
  ambiguity is introduced where none is needed; and
- keep `N` below 10 whenever the publisher's tooling relies on the schema's
  lexicographic rule *(unspecified in FS — decided here: prefer integer
  comparison everywhere and treat the lexicographic rule as a schema
  convenience, not a contract to build on)*.

A release id that does not match the pattern is a build failure (§4.1).

---

## 14. Repository and Tooling Layout

*(unspecified in FS — decided here.)*

```
packaging/
├── Makefile or build script        # the pipeline of §5.1
├── pkg.toml                        # slug, game_id, channel, platform matrix, launcher_min policy
├── launcher.config.template.json   # rendered per game from pkg.toml
├── verify.sh                       # §13.2, runnable by a user
├── prune/                          # archive, prune, manifest, hash, sign, publish steps
└── README.md                       # operator documentation
```

### 14.1 Configuration the Build Needs

| Key | Example | Purpose |
|-----|---------|---------|
| `slug` | `stardrifter` | Archive naming (§3.2) |
| `game_id` | `com.kobra.stardrifter` | Rendered into `launcher.config.json` |
| `game_name` | `Star Drifter` | Rendered into `launcher.config.json` |
| `pkgroot` | `../stardrifter` | Where `game/` lives (§2.2) |
| `launcher_artifacts` | `dist/{linux-amd64,windows-amd64}/launcher` | Per-platform binaries (§10) |
| `launcher_min` | `1.4.0` | §4.3 |
| `channel` | `stable` | §8.1 |
| `publish_base` | `https://kobra.games/stardrifter` | §8.2 |
| `gpg_key` | fingerprint | §7.4 |
| `platforms` | `[linux-x64, win64, macos-universal]` | Platform matrix |

---

## 15. Open Questions and Deferred Decisions

| # | Question | Current position | Owner |
|---|----------|------------------|-------|
| 15.1 | Storefront packaging (Steam, itch.io, Epic) | **Out of scope for v1, and it does not need this document to change.** The architecture is deliberately publisher-hosted: a ZIP the user extracts and owns (FS §1, FR-LNCH-2). A storefront would impose its own layout, its own updater and its own DRM, which is the lock-in the product exists to avoid. It is listed only so the choice is visible: adopting one later would be a product strategy change, not a packaging change | Product |
| 15.2 | Is FAT32 a supported target? | **No size budget is imposed** (§3.4): a package may exceed 4 GiB and may contain a file larger than 4 GiB, with the storage device left to the user. If FAT32 must become a hard target, that requires an explicit size budget and would then be enforced in the build. It MUST NOT be reintroduced as an implicit constraint | Product |
| 15.3 | Which update channel ships by default? | `manual` is still the baseline and the default. A publisher who wants one-click updates sets `update.channel: patch` and hosts a `.tar.zst` (§9.2). **Amended:** the patch channel no longer *obliges* signing, because the launcher fails closed on a declared signature with no verifier installed (§8.6 rule 3) and no verifier ships; requiring a signature would make the assisted path unusable. Signing is recommended, and a signed patch release MUST be paired with a verifier in the launcher build. The residual risk of an unsigned patch — no publisher authentication — MUST be published wherever the channel is offered | Product |

## Appendix A. Release Manifest Reference

The normative schema is `architecture/schemas/release.manifest.schema.json`. This is a reading guide for the fields a packaging pipeline must produce.

```json
{
  "schema": "kobra.release-manifest/1",
  "release": "2026.10.1",
  "previous_release": "2026.09.1",
  "published": "2026-10-01T12:00:00Z",
  "launcher_min": "1.4.0",
  "engine_version": "2.4.0",
  "game_version": "1.2.0",
  "save_version": 3,
  "archive": {
    "name": "stardrifter-2026.10.1-linux-x64.tar.zst",
    "size": 118374402,
    "hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
    "format": "tar.zst"
  },
  "channel": "stable",
  "scope": "full",
  "total_size": 483920117,
  "min_browser": { "chrome": 105, "edge": 105, "opera": 91 },
  "files": [
    { "path": "game/index.html",   "size": 2048, "hash": "sha256:…", "mode": "644" },
    { "path": "launcher/launcher", "size": 8769799, "hash": "sha256:…", "mode": "755" }
  ],
  "migrations": [
    { "from": 1, "to": 2, "fn": "migrate_v1_to_v2" },
    { "from": 2, "to": 3, "fn": "migrate_v2_to_v3" }
  ],
  "schema_migrations": [
    { "manifest": "asset-manifest", "from": 1, "to": 2 }
  ],
  "signatures": {
    "gpg": "-----BEGIN PGP SIGNATURE-----…",
    "gpg_key_fingerprint": "0123456789ABCDEF0123456789ABCDEF01234567"
  },
  "notes": "Adds the Outer Rim region.",
  "release_notes_url": "https://kobra.games/stardrifter/notes/2026.10.1"
}
```

`files` entries MUST cover `/^game\//` and `/^launcher\//` only. The schema's `path` pattern structurally forbids `data/`, which is how FR-UPD-7 is enforced at the document level as well as at extraction time.

The `archive` object and `game_version` are required by this document (§8.2.2,
§4.5) and are not yet in the schema — see the amendment backlog in §12. The hash
is shown zeroed for illustration only; a real manifest carries the actual digest
with its `sha256:` prefix.

---

## Appendix B. Event and Error Catalogue for the Build

Build-side events, structured the same way as the launcher's log (Launcher spec §21). These are produced by the packaging pipeline, not the launcher.

| Event | Level | Fields | Meaning |
|-------|-------|--------|---------|
| `pack.start` | info | `release`, `platform`, `pkgroot_kind` | Pipeline started |
| `pack.identity.ok` | info | `release`, `engine_version`, `save_version`, `launcher_min` | §4.4 passed |
| `pack.identity.fail` | error | `check`, `expected`, `actual` | A cross-document disagreement; build fails |
| `pack.scan.reject` | error | `path`, `reason` | Forbidden content found (§2.4) |
| `pack.manifest.generated` | info | `kind`, `entries`, `bytes` | A manifest was written |
| `pack.archive.built` | info | `kind`, `bytes`, `sha256` | ZIP or tar.zst produced |
| `pack.hash` | info | `file`, `sha256` | A file was hashed into `SHA256SUMS` |
| `pack.sign` | info | `kind`, `key_fingerprint` | Signing succeeded |
| `pack.sign.skipped` | warn | `kind`, `reason` | No signing authority; hash-only release |
| `pack.publish` | info | `base`, `files` | Artefacts uploaded |
| `pack.verify.ok` | info | `release`, `files` | Re-download verified |
| `pack.verify.fail` | error | `file`, `expected`, `actual` | Verification failed; withdraw |

Build failure classes reuse the launcher's error discipline: a message a human can act on, with paths and hashes in structured fields rather than prose.

---

## Appendix C. Requirement Coverage Map

| Requirement | Sections |
|-------------|----------|
| FR-LNCH-2 (single file, no installer, copy-and-run) | §1.1, §2, §3.1, §10 |
| FR-LNCH-3 (no admin, no writes outside the folder and sidecar) | §2.4, §10 |
| FR-LNCH-4 (no runtime dependency) | §10.3 |
| FR-LNCH-5 (per-file SHA-256 and detached GPG) | §7.1, §7.2, §7.3, §7.4, §8.6 |
| FR-UPD-1 (every release carries a release manifest) | §1.2, §4.4, §6.1, Appendix A |
| FR-UPD-4 (crash-safe swaps) | §9.3 |
| FR-UPD-5 (zip-slip and zip-bomb defence) | §2.4, §6.4, §9.1 |
| FR-UPD-6 (`launcher_min` enforcement) | §4.3, §9.4 |
| FR-UPD-7 (never touch `data/`) | §1.2, §2.4, §2.5, §6.4, §9.1, §11.3 |
| FR-UPD-8, FR-UPD-9 (rollback, saves untouched) | §8.5, §9.3 |
| FR-SCH-3 (schema validation at build) | §5.4, §11.2 |
| FR-SCH-4 (document migrations) | §6.5 |
| FR-SCH-5 (engine/release `save_version` agreement) | §4.4, §6.3 |
| FR-SCH-6 (semantic validation of the migration chain) | §6.5 |
| FR-SAVE-12 (migration chain) | §6.5 |
| FR-AST-2 (`.wasm` MIME type) | §5.4, §11.2 |
| FR-COMPAT-1 (browser floor) | §6.1 |
| FS §5.1 (distribution layout) | §3.1 |
| FS §12.1–§12.4 (update machinery) | §9, §13 |
| FS §13.2, §13.3 (launcher packaging, signing) | §10 |
| FS §16 error texts (E27–E30, E32, E34–E36) in the update UI | §9.4 |
| FS §11.2 `game_version` in the save header | §4.4, §4.5 |
| FR-AST-4 (advisory asset hashes) | §6.2, §6.5.2 |
| FR-AST-9, FR-AST-10 (content-addressed paths, bundling) | §6.5.1, §6.5.3 |
| FR-SCH-6 (semantic validation the schemas cannot express) | §6.6, §12 A8 |
| FS §4 stage 2 (download size target) | §3.3 — superseded; no size budget imposed |
| FR-LNCH-2 (single file, no installer, copy-and-run) | §3.3 (size is the user's device decision) |
| FS Appendix B (document schemas) | §6, Appendix A |

---

*End of Per-Game Packaging and Release Specification.*
