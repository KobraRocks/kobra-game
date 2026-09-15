# Second-release fixtures

`testgame/game/` is the **2026.09.1** development tree. Each directory here is a
later release, stored as an **overlay** rather than a full copy:

```
releases/2026.10.1/
├── release.toml      # release identity: id, game_version, previous_release, scope
└── overlay/game/…    # only the files that changed
```

`kobra-pack` copies `pkgroot/game/` and then applies the overlay on top, so
the fixture stays small enough to read while still producing two genuinely
different releases. This mirrors how a real repository would tag a release: the
tree is the same, the diff is what identifies the version.

## Why the overlay is not `game-only`

`release.toml` sets `scope = "full"`, and that is deliberate.

Packaging spec §4.6 requires `launcher/launcher.config.json`'s `release` to equal
the package's release id, and `/api/state.release` is served from that file
(`internal/server/server.go`). So a release that advances `release` *always*
changes `launcher/launcher.config.json`, whatever else it does.

That makes `scope: game-only` — defined in §9.2 as "Every file in `game/`, never
`launcher/`" — inconsistent with §4.6 for any release that bumps the id. Either
the release id changes and `launcher/` changes with it, or the launcher keeps
reporting the previous release forever.

The packager resolves this by **rendering** `launcher/launcher.config.json` per
release (taking `testgame/launcher/launcher.config.json` as the template and
overriding `release`), rather than copying it verbatim. See deviation `D3` in
`packaging/internal/pack`.

## What 2026.10.1 changes

| File | Change | Why |
|------|--------|-----|
| `game/engine/engine.manifest.json` | `release` → `2026.10.1` | §4.6 release agreement |
| `game/assets/data/levels.json` | adds a `second_release` level | Proves the swap replaced `game/` wholesale: the level is absent before the update and present after |
| `game/locales/en.json` | adds `update.release2` | Exercises a locale change, which is `no-cache` and therefore visible immediately |
| *(rendered)* `launcher/launcher.config.json` | `release` → `2026.10.1` | §4.6 launcher agreement |

`engine_version` and `save_version` are **deliberately unchanged** (both `0.1.0`
and `1`). That makes this a content-only release: it exercises the update path
without raising the save format, which is the common case and the one §4.4 says
must not be conflated with a save migration.

A future fixture for the *save migration* path (§6.6, FR-SAVE-12) would raise
`save_version` to `2` and add a contiguous `migrations` chain from `1` to `2`.
The packager's semantic validator does not check chain contiguity yet, because
there is no chain to check — that gate is part of the real packager's job.

## Adding a release

1. `mkdir releases/<YYYY.MM.N>/overlay/game/…`
2. Write `release.toml` with the new id, `previous_release`, `game_version` and
   `scope`.
3. Copy in only the files that changed.
4. `make -C testgame e2e` — `latest.json` is rewritten to point at the newest
   release automatically, and `run.sh` phase 14 re-checks update discovery.

Nothing needs registering: the packager discovers `releases/*/release.toml` by
directory scan, which is also why the directory name must match the `release`
value inside it.
