# Working in this repository (agents)

`CONTRIBUTING.md` is the source of truth for how to work here — setup, the
package layout, and **the rules of the house**. Read it, and treat its rules as
binding on you. This file does not repeat them; it records the things that are
easy to get wrong when editing this tree with tools rather than by hand, and it
points at the docs that already answer most questions.

## What this is

A launcher, a packaging toolchain, and the specifications that hold them
together, for shipping web-technology games as ordinary folders. Two Go modules
and a fixture game; `README.md` has the real overview.

## Layout

| Path | What it is |
|---|---|
| `launcher/` | Go module `kobragames.local/launcher` — the launcher binary. |
| `packaging/` | Go module `kobragames.local/packaging` — `kobra-pack`, the publisher toolchain. |
| `testgame/` | The fixture game; the conformance suite both tools are tested against. |
| `architecture/` | **Normative specs** plus the published JSON schemas. |
| `VERSIONING.md` | The six version axes, the pre-1.0 policy, and the fail-closed rule (see Traps 10). |
| `docs/` | Guides: authoring a game, launcher architecture. |
| `.e2e/` | The launcher smoke drive (`run.sh`). |
| `.github/workflows/ci.yml` | CI — the same `make` targets you run locally. |
| `Makefile` (root) | Repo-level chores only — `help`, `sync-schemas`, `check-schemas`. Gates are per module. |
| `scripts/` | `sync-schemas.sh`, which the root schema targets run. |

## Commands

The root `Makefile` carries **only repository-level schema chores**. Every gate
lives in a module Makefile, there is no `go.work`, and `go test ./...` at the root
fails — always name the module directory for gates.

```sh
make -C launcher fmt vet test race     # the launcher suite
make -C packaging check race            # the packager suite (check = fmt vet test)
bash .e2e/run.sh                        # launcher smoke drive
make -C testgame package                # build both tools + package the fixture

# Conditional — run these based on what you touched, not just the suite:
make -C launcher faultinject    # REQUIRED for storage/, update/, or the server request path
make -C testgame verify         # REQUIRED for anything touching the packager
make -C testgame e2e            # full package -> install -> run -> update drive
make -C launcher cross          # cross-compile matrix; run if you touched *_windows.go,
                                # *_unix.go, or a build tag

# Repo-level chores — the only targets at the root (see Traps 1):
make sync-schemas               # re-copy architecture/schemas into all five copies
make check-schemas              # report schema drift without writing
make                            # list what the root offers
```

`make test` at the root still fails — the root has no `test` target. Run the
gates for what you touched; a green `test` alone is a false pass.

## Traps

1. **Schemas are duplicated — use `make sync-schemas`.** `architecture/schemas/`
   is the published, normative set and the only place a schema is edited, but the
   code cannot read it at build time. The same bytes therefore live in five
   places: the full mirrors `launcher/schemas/` and
   `packaging/internal/pack/schemas/`, the two `//go:embed` subsets
   `launcher/internal/config/schema/` and
   `launcher/internal/server/testschema/`, and the loose
   `launcher/port-deny-list.json` that `make package` copies into shipped game
   folders. So: **edit `architecture/schemas/`, then run `make sync-schemas` at
   the repository root.** Never hand-copy.
   `make check-schemas` reports drift without writing. Drift fails
   `TestSchemaCopiesMatchThePublishedSet` (launcher) and
   `TestEmbeddedSchemasMatchArchitecture` (packager), both in *both* directions —
   a file present in one place and missing in the other also fails.

2. **`launcher/vendor/` is committed and generated.** Never hand-edit it; run
   `go mod vendor`. The launcher deliberately holds a three-dependency footprint
   — a new dependency needs a stated reason. The packager may be more liberal;
   it never ships to a user.

3. **The Go caches are redirected into the workspace.** Each Makefile sets
   `GOCACHE=<repo>/.gocache/build`, `GOMODCACHE=<repo>/.gocache/mod` and
   `GOSUMDB=off` (absolute paths, via `$(abspath)`) so the tree builds where the
   system caches are read-only — a sandbox consequence, not a recommendation.
   Override on the `make` command line if you prefer your own. If you want to
   invoke `go` directly, pass an **absolute** `GOCACHE`: a relative one is
   rejected with `build cache is required, but could not be located`. Prefer the
   `make` targets.

4. **`make race` re-enables cgo.** Every other target builds with
   `CGO_ENABLED=0`, but `-race` needs a C toolchain and fails with an explicit
   message when `gcc`/`cc` is absent.

5. **Build outputs are git-ignored, and some are already on disk** —
   `launcher/launcher`, `packaging/kobra-pack`, `testgame/dist/`, `.e2e/state/`,
   plus `.dev/` and `launcher/dist/` when the dev or dist targets have run. Do
   not commit them, and do not treat an existing one as current: it may predate
   your change.

6. **Cite the spec where the rule lives.** The convention is `§14.2`,
   `FR-SAVE-11`, `R10.6` in the comment at the point of implementation. If a
   change alters behaviour the specs describe, update the spec in the same
   change; a code/spec disagreement *is* the bug.

7. **User-facing text never contains a path, username, token, or stack trace.**
   `kobraerr` is built so this cannot leak by accident, and
   `TestNoErrorLeaksPathsOrUsername` walks every constructor. Diagnostics are
   surfaced to the page, so they count as user-facing.

8. **`--game-dir` exists only under the `kobra_dev` build tag.** A release
   binary must never accept a game folder from the command line (FR-LNCH-1).
   Keep that property intact.

9. **Do not add test-only overrides that mask production behaviour.** If a test
   needs a seam, add the seam to production code and exercise the real path.

10. **Version fields are bare `X.Y.Z`, not full SemVer.** `game_version`,
    `engine_version`, `launcher_min` and the launcher's own version all match
    `^[0-9]+\.[0-9]+\.[0-9]+$`. There is no `-rc.1` and no `+build`: pre-release
    status is the release manifest's `channel`. Do not "fix" this by adding SemVer
    §11 prerelease precedence — it needs the published schema regexes loosened, both
    comparators changed, and a spec update. Read `VERSIONING.md` before touching any
    version comparison. The rule that matters: **an unparseable version fails its
    gate closed**, and it must never be silently ordered or coerced to zero.

## Definition of done

- The gates for what you touched pass, and you can name which ones you ran.
- A fixed bug has a regression test **named after the failure**, not the
  function — and it fails on the old code.
- Described behaviour that changed has its spec updated in the same change.
- `gofmt` is clean (`make -C launcher fmt` / `make -C packaging fmt`).
- No dead machinery: a config key, event, build tag, or exported symbol with no
  consumer gets deleted rather than documented. See rule 3 of `CONTRIBUTING.md`.
