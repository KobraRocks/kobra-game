# Documentation

Three audiences, three entry points.

## I want to ship a game

- **[Authoring a game](authoring-a-game.md)** — the folder contract, the shell
  boot sequence, the data API you call from game code (sessions, CSRF, saves,
  settings, mods, updates), what the packager generates versus what you write,
  `pkg.toml`, publishing updates, and every rule `kobra-pack check` enforces.
- **[`testgame/`](../testgame/README.md)** — the worked example: a complete game
  that the real packager builds and the real launcher runs, including a second
  release and the update path end to end.

## I want to work on the launcher

- **[How the launcher serves your game](launcher-architecture.md)** — a
  contributor's tour: the startup sequence, the request pipeline and why each
  gate sits where it does, how a static file and an API call are served, the
  storage engine's atomic write protocol and its crash windows, the concurrency
  model and the drain, the update path, and a map of which file to open for which
  kind of change.
- **[CONTRIBUTING.md](../CONTRIBUTING.md)** — setup, the gate table, and the
  rules of the house. Read this before your first pull request; the gates are
  cheap and the rules are not negotiable.
- **[CODE-REVIEW.md](../CODE-REVIEW.md)** — an adversarial review of the launcher,
  packager and updater with every finding, the fixes, and the items deliberately
  deferred. Useful as a map of what is load-bearing and why.
- **[SECURITY.md](../SECURITY.md)** — the threat model and how to report a
  vulnerability privately.

## I want to know what is specified

`architecture/` holds the normative documents; the code cites them by section at
the point where each rule is implemented.

- **[functional-specification.md](../architecture/functional-specification.md)** —
  the product contract: boot sequence, saves, the data API, the shell.
- **[Launcher-spec.md](../architecture/Launcher-spec.md)** — the launcher: folders,
  ports, the sidecar, security headers, diagnostics, exit codes, the fault
  injection and "no paths" test requirements.
- **[Packaging-spec.md](../architecture/Packaging-spec.md)** — every packaging
  rule, the `pkg.toml` key set, the archive layout.
- **[Updater-spec.md](../architecture/Updater-spec.md)** — the update protocol,
  retention, rollback, crash recovery.
- **[schemas/](../architecture/schemas/)** — the published JSON schemas: save,
  engine manifest, asset manifest, data API, launcher config, mod manifest,
  release manifest, port deny list.

When code and specification disagree, one of them is wrong and the disagreement
is the bug. Several review findings were exactly that, so a change that alters
described behaviour should update the spec in the same pull request.

## Conventions in these documents

- `§12.3` is a section of the specification under discussion; `FR-SAVE-11` and
  `R10.6` are requirement identifiers inside it.
- Code references name the file and the symbol rather than a line number, because
  line numbers rot and symbols do not.
- Where a decision was made against the obvious alternative, the reason is
  stated; where something is deferred, it is marked deferred rather than left
  silent.
