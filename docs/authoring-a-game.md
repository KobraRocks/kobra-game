# Authoring a game

This is the practical guide to shipping a game with the Kobra launcher: what you
write, what the packager generates, and what the launcher gives you for free. It
assumes nothing about the specifications; where a rule comes from one, the section
is cited so you can read the normative text.

`testgame/` is the worked example throughout — every snippet here exists there in
a runnable form.

- [What you are building](#what-you-are-building)
- [The minimum viable game](#the-minimum-viable-game)
- [The shell contract](#the-shell-contract)
- [Saves](#saves)
- [Player settings](#player-settings)
- [Declaring the package](#declaring-the-package)
- [Manifests: what you write, what is generated](#manifests-what-you-write-what-is-generated)
- [Building a package](#building-a-package)
- [Publishing updates](#publishing-updates)
- [Testing your game](#testing-your-game)
- [The rules the packager enforces](#the-rules-the-packager-enforces)

## What you are building

A game is a folder. The launcher is game-agnostic: the binary is the same for
every game, and everything that makes it *your* launcher is a config file. That
means there is no engine to port to and no plugin ABI — your game is web content,
served over loopback to the player's own browser.

```text
YourGame/                     ← the package root (pkgroot)
├── game/                     ← what gets served to the browser
│   ├── index.html            ← required
│   ├── shell.js              ← required (ES module; your boot code)
│   ├── engine/               ← your wasm/js engine + engine.manifest.json
│   ├── assets/               ← art, audio, data (indexed by the packager)
│   └── locales/              ← translations
├── launcher/
│   ├── launcher.config.json  ← your game's identity and policy
│   └── port-deny-list.json   ← copy of the shipped list
├── data/                     ← the player's saves and settings (created at run time)
├── pkg.toml                  ← packaging input
└── releases/<release>/       ← later releases as overlays (optional)
```

The browser sees only `game/`. `data/` is the player's and is never served over
HTTP; `launcher/` is never served either. The launcher's own surface is described
in [The shell contract](#the-shell-contract).

Two files at the root of `game/` are special: `index.html` and `shell.js` must
exist, and the root may contain nothing else except `release.manifest.json`. Put
everything else under `engine/`, `assets/` or `locales/` — those are the only
prefixes the static server will serve.

## The minimum viable game

Three files and a config will run. `game/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>My Game</title>
</head>
<body>
  <div id="app"></div>
  <script type="module" src="/shell.js"></script>
</body>
</html>
```

`game/shell.js` — the smallest useful shell:

```js
const session = { id: null, csrf: null };
const ENDPOINTS = {
  session: '/__kobra/session',
  heartbeat: '/__kobra/heartbeat',
  state: '/api/state',
  save: '/api/save',
};

async function api(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (session.csrf) headers['X-Kobra-CSRF'] = session.csrf;
  const res = await fetch(path, {
    method, headers, credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}

// 1. The launcher opens the page with a one-time token in the URL fragment.
const token = new URLSearchParams(location.hash.slice(1)).get('t');
history.replaceState(null, '', location.pathname + location.search); // erase it

// 2. Exchange it for a session. The reply carries the CSRF token.
const s = await api(ENDPOINTS.session, { method: 'POST', body: { token } });
session.id = s.session_id;
session.csrf = s.csrf_token;

// 3. Ask what the launcher can do. `data_writable` is false on a read-only
//    data folder, which is your cue to run an in-memory session instead.
const state = await api(ENDPOINTS.state);
if (state.api_version !== 1) throw new Error('launcher too old');

// 4. Load your engine, then start heartbeating.
//    await loadEngine();
setInterval(() => api(ENDPOINTS.heartbeat, { method: 'POST' }), 5000);
```

`game/engine/engine.manifest.json` — you write this; it describes the engine to
the launcher and to itself:

```json
{
  "schema": "kobra.engine-manifest/1",
  "release": "2026.09.1",
  "engine_version": "0.1.0",
  "wasm": "engine/game.wasm",
  "glue": "engine/game.js",
  "save_version": 1,
  "required_features": ["wasm"],
  "min_browser": { "chrome": 105, "edge": 105, "opera": 91 }
}
```

Every path that manifest names must exist — the packager checks it and fails with
`pack.identity.fail … engine file exists`. For a first smoke test, two stubs are
enough:

```bash
printf '\x00\x61\x73\x6d\x01\x00\x00\x00' > game/engine/game.wasm   # empty wasm module
printf 'export function tick() { return 0; }\n' > game/engine/game.js
```

`launcher/launcher.config.json` — your game's identity:

```json
{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.example.mygame",
  "game_name": "My Game",
  "release": "2026.09.1",
  "port": { "base": 18900, "span": 50, "require_confirmation": false },
  "browser_preference": ["chromium", "chrome", "brave", "firefox"],
  "update": { "channel": "manual" }
}
```

And a first `pkg.toml` is covered under
[Declaring the package](#declaring-the-package).

## The shell contract

### Boot sequence

The launcher opens `index.html` with a **single-use bootstrap token in the URL
fragment** (`http://127.0.0.1:18915/index.html#t=…`). The fragment never reaches
the server, so the token stays out of logs and history. Your shell must:

1. read `t` from `location.hash` and erase it with `history.replaceState`;
2. `POST /__kobra/session` with that token to get `{session_id, csrf_token,
   api_version}`;
3. echo `csrf_token` in the `X-Kobra-CSRF` header on every write;
4. heartbeat so the launcher knows the page is alive;
5. send a goodbye when the page goes away, if you can.

### Endpoints

Loopback only, `Host` must match the port. `auth` below means "needs a session and,
for writes, the CSRF header".

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/__kobra/probe` | no | Liveness for the launcher's own port check. Returns `{app, game_id, instance, release, port}` and nothing else. |
| GET | `/__kobra/health` | no | Same body as the probe; used for instance handoff. |
| POST | `/__kobra/session` | token | Exchange the bootstrap token. `{token}` → `{session_id, csrf_token, api_version}`. Single use. |
| POST | `/__kobra/heartbeat` | yes | 204. Resets the idle timer. |
| POST | `/__kobra/goodbye` | yes | 204. Tells the launcher to drain now rather than wait for the idle timeout. |
| POST | `/__kobra/shutdown` | yes | 202, then the launcher drains. |
| GET | `/__kobra/diagnostics` | yes | The support payload: versions, port, origins, log tail, mod and save summaries, browser. No paths, no username. |
| GET | `/api/state` | yes | `{api_version, game_id, release, engine_version, save_version, data_writable, data_dir_kind, writer, uptime_seconds, heartbeat_interval_seconds}`. |
| GET | `/api/data/saves` | yes | `{saves: [{slot, revision, modified, size, playtime_seconds, save_version, corrupt}]}`. |
| GET | `/api/data/saves/{slot}` | yes | The save's **payload**, exactly as you wrote it. |
| POST | `/api/save` | yes | Write a save. See [Saves](#saves). |
| DELETE | `/api/save/{slot}` | yes | Move the slot to the trash area (never unlinked). |
| GET | `/api/export/{slot}` | yes | The whole save file as a download attachment. |
| GET | `/api/data/config` | yes | The settings document, or `{}`. |
| POST | `/api/config` | yes | Merge keys into settings. See [Player settings](#player-settings). |
| GET | `/api/data/config/mods` | yes | Mod state: available, enabled. |
| POST | `/api/mod` | yes | `{id, action: install\|enable\|disable}`. |
| GET | `/api/update/check` | yes | Is a newer release published? `{current, latest, available, notes_url, detail}`. |
| POST | `/api/update/apply` | yes | Record the intent to update. The swap itself happens on the **next launch**. |
| GET | `/api/update/progress` | yes | Phase, bytes, percent, and the terminal result. |
| POST | `/api/update/cancel` | yes | Cancel a recorded or running apply. |

Every error is a JSON envelope: `{"error": "code", "message": "...",
"detail": {...}}` with a matching HTTP status. The codes you will meet most are
`no_session` (401), `bad_csrf` (403), `bad_origin` (403), `bad_identifier` (400),
`conflict` (409), `too_large` (413), `rate_limited` (429), `io_error` (500) and
`read_only` (500).

### Heartbeat and goodbye

The launcher shuts down when the page has been silent for
`server.idle_timeout_seconds` (default 90 s). `/api/state` publishes
`heartbeat_interval_seconds` (default 5) — heartbeat at that rate while visible
and **six times** that while hidden, so a backgrounded tab does not keep the
launcher alive. Read the value rather than hard-coding it; a publisher can change
it.

`/__kobra/goodbye` is a POST and therefore CSRF-gated. `navigator.sendBeacon`
cannot set the `X-Kobra-CSRF` header, so use `fetch(url, {method: 'POST',
keepalive: true})` with the header. If you use `sendBeacon` you will get a 403 and
the launcher will fall back to its idle timeout.

### CSRF and sessions, exactly

The server checks three things on a write (`internal/session.CheckCSRF`): the
`kobra_csrf` cookie, the `X-Kobra-CSRF` header, and the session's own token.

- `kobra_sid` (the session) is `HttpOnly`.
- `kobra_csrf` is deliberately **not** `HttpOnly`, because the page must read it.
  Prefer reading it from `document.cookie` at request time over caching it in a
  variable: a stale copy fails the check rather than silently succeeding.

Do not make browser storage authoritative. Your session id may live in
`sessionStorage` so a reload can tell whether it still holds the writer role, but
losing it must lose nothing: saves and settings come from the launcher
(FR-API-3).

## Saves

### Writing

```js
const result = await api('/api/save', {
  method: 'POST',
  body: {
    slot: 'slot1',                 // ^[a-z0-9][a-z0-9._-]{0,63}$ and not a device name
    payload: {level: 4, hp: 90},   // any JSON value; stored verbatim
    if_revision: currentRevision,  // 0 for "create"; omit to force a write
    claim: false,                  // true to take the single-writer role
    meta: {playtime_seconds: 3600} // optional, shown in the list
  },
});
// -> {slot, revision, modified, bytes}
```

`if_revision` is optimistic concurrency. If the slot's current revision is not
what you sent, the write is refused with 409 `conflict`, and the body carries
`current_revision` so you can decide whether to reload or overwrite:

```json
{"error":"conflict","message":"This slot was changed by another tab.",
 "current_revision":7,"modified":"2026-09-15T17:00:00Z","slot":"slot1"}
```

Send `current_revision` back to overwrite, or re-read and merge. Use
`claim: true` on the first write from a tab to take the **writer role**: a second
tab that tries to claim gets 409 `writer_held`, which is how two tabs are kept
from interleaving writes (FR-SHELL-3). `release_writer` happens automatically when
your session ends.

The payload is canonicalised before hashing, so re-saving an unchanged body does
not change the checksum, and unknown top-level fields in the stored header are
preserved for forward compatibility (FR-SAVE-4).

One caveat if you plan save migrations: the header's `game_version` field is
currently written from the launcher's own compile-time engine version, not from
your `pkg.toml` `game_version`. Use `save_version` and your own payload fields for
migration decisions; `game_version` in a save is a known open item (see
`testgame/README.md`).

### What the launcher gives you

You get durability and recovery without implementing any of it:

- **Atomic writes.** Every save is written to a temp file, fsynced, then renamed
  over the target. A crash can lose the newest write; it cannot leave a
  half-written file in place.
- **One backup generation.** The previous revision is kept as `<slot>.json.bak`
  and is what the reader falls back to if the live file is corrupt or fails its
  checksum.
- **Revision sidecars.** `<slot>.json.<n>` files record history; the newest are
  kept up to `data_api.keep_revisions` and pruned oldest-first. The live
  revision's sidecar is never pruned, so the next conflict check has something to
  compare against.
- **Quarantine, never deletion.** A damaged save and an interrupted write's temp
  file are moved into `data/.trash-<timestamp>/`, kept for
  `data_api.trash_retention_days`, and reported in the log. No recovery path
  deletes anything of the player's (FR-SHELL-4).
- **Recovery reporting.** When a read was served from `.bak` or a file was
  quarantined, the event is logged (`save.recover` with `from: bak|quarantine`),
  so a support request can see what happened. Surface it to the player when you
  can.

What you get back from `/api/data/saves/{slot}` is the **payload you wrote**, not
the header, so your load path does not deal with checksums or revisions.

## Player settings

Settings live in `data/config/settings.json` and are merged, not replaced:

```js
await api('/api/config', {
  method: 'POST',
  body: { merge: { audio: { volume: 0.8 } }, if_revision: settingsRevision },
});
// -> {revision, modified}
```

The merge is recursive and preserves keys you do not mention, so adding a setting
never drops the player's other choices.

**The top-level keys are a fixed whitelist.** A key outside it is refused with
`malformed_body` ("That settings key is not recognised."), because settings.json
is engine-owned and an unknown key is a bug or an injection attempt:

```
audio  video  controls  gameplay  accessibility  locale
language  display  input  network  misc
```

Put your settings inside one of those groups (`{"gameplay": {"difficulty": "hard"}}`).
If you need a new group, that is a launcher change, not a config write.

## Declaring the package

`pkg.toml` is the single declared input (`kobra-pack` reads nothing else about
your intent). A first release:

```toml
[package]
slug        = "mygame"                        # must equal the last segment of game_id
game_id     = "com.example.mygame"
game_name   = "My Game"
pkgroot     = "."                             # the directory containing game/

[release]
release          = "2026.09.1"                # calver YYYY.MM.N, immutable
game_version     = "1.0.0"                    # your content version
engine_version   = "0.1.0"                    # must equal engine.manifest.json
save_version     = 1                          # must equal engine.manifest.json
launcher_min     = "0.1.0"                    # oldest launcher that may run this
channel          = "stable"
scope            = "full"
previous_release = ""                         # a patch release must set this

[launcher]
config         = "launcher/launcher.config.json"
deny_list      = "../launcher/port-deny-list.json"
binary_dir     = "../launcher/dist"           # from `make -C launcher cross`
version_source = "binary"                     # VERSION comes from `--version`

[platforms.linux-x64]
goos             = "linux"
goarch           = "amd64"
binary           = "launcher"
archive_platform = "linux-x64"

[platforms.win64]
goos             = "windows"
goarch           = "amd64"
binary           = "launcher.exe"
archive_platform = "win64"

[publish]
base    = "https://updates.example.com/mygame"  # where releases are served
gpg_key = ""                                    # empty = hash-only release
```

Later releases are overlays, so the diff is what identifies the version:

```text
releases/2026.10.1/
├── release.toml            # release, game_version, previous_release, channel, scope
└── overlay/game/…          # only the files that changed
```

`kobra-pack` copies `pkgroot/game/` and applies the overlay on top. Note that the
launcher's `release` field must equal the release id, so every release that
advances the id also touches `launcher/launcher.config.json` — the packager
renders that file per release rather than copying it verbatim.

## Manifests: what you write, what is generated

| File | Who writes it |
|---|---|
| `game/engine/engine.manifest.json` | **You.** Engine identity, wasm/glue paths, `save_version`, required features, minimum browser. |
| `game/assets/asset.manifest.json` | **The packager.** It walks `game/assets/`, hashes every file, assigns types and cache policy. Never hand-write this; it will be regenerated and your copy ignored. |
| `game/release.manifest.json` | **The packager.** The release identity and the file index. It is deliberately never inside the archive it indexes. |

## Building a package

```bash
make -C launcher cross          # launcher binaries for the whole matrix
make -C packaging build         # the kobra-pack tool itself

kobra-pack check  --config pkg.toml --platform linux-x64            # validate only
kobra-pack build  --config pkg.toml --platform linux-x64 --out dist --install
kobra-pack verify dist                                              # check what you built
```

- `check` runs exactly the same validation as `build` and writes nothing, so it
  belongs in a pre-commit hook and in CI.
- `build --install` also extracts release 1, which gives you a runnable folder at
  `dist/install/<Slug>/` — the slug with its first letter capitalised, so
  `mygame` becomes `Mygame`.
- `verify` re-reads the published directory: archive hashes, every `files[].hash`
  against the archived bytes, and that each archive has its manifest. Run it in
  the job that uploads.
- `kobra-pack dev --config pkg.toml --platform linux-x64 --port 18915` serves a
  source tree without packaging it, for the edit-and-run loop.

The output is a `.zip` (what a player downloads and extracts) and a `.tar.zst`
(what the updater downloads), plus the release manifest, `SHA256SUMS` and
`latest.json`.

## Publishing updates

Set the channel and the base URL in your launcher config:

```json
"update": { "channel": "patch", "patch_base_url": "https://updates.example.com/mygame" }
```

`manual` (the default) means "a human replaces the folder": `/api/update/check`
answers "No update server is configured for this game." and `/api/update/apply`
refuses with 409 `manual_channel`. (`channel: "none"` goes further and disables
the check itself: "Update checks are disabled for this game.") With `patch`:

1. your shell asks `GET /api/update/check`;
2. the player accepts, and your shell `POST /api/update/apply` — this only
   **records the intent**, it does not download anything while the game is
   running;
3. the launcher exits and the next launch downloads, verifies, extracts and
   swaps **before** the server binds, so no client can observe a half-swapped
   folder;
4. the previous release is kept as `game.old/`; if the new release fails to start
   twice, or the player asks to revert, it is restored.

`data/` is never touched by an update (`protect_data_dir`), the archive must be
HTTPS unless it is loopback, and the archive's hash and size come from the
release manifest and are checked before anything is written. Your shell can watch
`GET /api/update/progress` for phase, bytes and the terminal outcome; report it
on the next boot, because the apply happens between sessions.

## Testing your game

| What | Command |
|---|---|
| Validate the package (no archives written) | `kobra-pack check --config pkg.toml --platform linux-x64` |
| Full build and a runnable folder | `kobra-pack build --config pkg.toml --platform linux-x64 --out dist --install` |
| Two builds must match byte for byte | build twice, compare `SHA256SUMS` |
| Published directory is complete and consistent | `kobra-pack verify dist` |
| Serve a source tree, no packaging | `kobra-pack dev --config pkg.toml --platform linux-x64 --port 18915` |
| The launcher against another folder | `make -C launcher run-dev GAME=/path/to/game/src` |

For the launcher itself: `--print-url` prints the origin with its token and
blocks (headless testing), `--diagnostics` prints the support payload and exits,
`--repair` rolls back an interrupted update and rebuilds the `data/` scaffolding,
and `--check-port` reports whether the game's port is free.

## The rules the packager enforces

Each of these fails `check` and `build` with a message naming the file:

1. **`game/index.html` and `game/shell.js` must exist.**
2. **The game root serves exactly those two files** (plus `release.manifest.json`).
   Anything else at the root must move under `assets/`, `engine/` or `locales/`.
3. **Filenames in the index must match `[A-Za-z0-9._-]+` per path segment.** No
   spaces, no `|`, no accents, no `#`. This is the one that surprises people: a
   file named `level|2.json` is rejected by the manifest schema for its prefix
   (`asset.manifest.json` under `assets/`, otherwise the release manifest),
   because such a path cannot survive the archive formats and the URL space
   intact.
4. **`game_id` must match `^[a-z0-9]+(\.[a-z0-9-]+)+$`** and `slug` must be its
   last segment.
5. **`release` must be calver** (`YYYY.MM.N`) and immutable, and it must equal
   `launcher/launcher.config.json`'s `release` after the package is built.
6. **`engine_version` and `save_version` in `pkg.toml` must equal
   `engine.manifest.json`'s**, and `launcher_min` must not exceed the packaged
   launcher's version.
7. **`launcher.config.json` must validate against the schema**, which rejects
   unknown keys (`additionalProperties: false`). Adding a key is a launcher
   change.
8. **Every path an engine manifest names must exist** (`pack.identity.fail … file
   exists`), and `wasm`/`glue` must be relative to `game/`.
9. **The archive must not contain the release manifest**, and must not touch
   `data/`.

If `check` fails, read the event line: it names the path and the rule
(`pack.scan.reject path=game/index.html reason=required by §5.4 but missing`).

## Where to read more

| Document | For |
|---|---|
| `architecture/functional-specification.md` | The product contract: boot sequence (§10.1), saves (§11), the data API (§12). |
| `architecture/Launcher-spec.md` | Launcher internals: ports, sidecar, security headers, diagnostics, exit codes. |
| `architecture/Packaging-spec.md` | Every packaging rule, `pkg.toml` key set (§14.1), archive layout. |
| `architecture/Updater-spec.md` | The update protocol, retention, rollback. |
| `architecture/schemas/*.json` | The published schemas: save, engine manifest, asset manifest, data API, launcher config, mod manifest, release manifest. |
| `testgame/` | The worked example, including a second release as an overlay and the update path end to end. |
