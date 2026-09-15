# Contributing

Thanks for wanting to help. This is an open-source project: the launcher, the
packager and the specifications are MIT-licensed, and the value is in the games
people ship with them.

There are four useful things you can do, in rough order of how often they help:

1. **Report a bug** with a reproduction — including "the packager rejected my
   game and I do not understand why".
2. **Improve the docs**, especially the game-authoring guide. If a step did not
   work for you, that is a documentation bug and worth an issue.
3. **Send a patch** — code, tests, or both.
4. **Tell us what a launcher cannot currently do** that your game needs. Design
   feedback before a patch is much cheaper than after.

Issues and pull requests are welcome on
[github.com/KobraRocks/kobra-game](https://github.com/KobraRocks/kobra-game).

## Getting set up

```bash
git clone https://github.com/KobraRocks/kobra-game.git
cd kobra-game

make -C testgame package      # builds both tools, packages the fixture
make -C launcher test         # the launcher suite
```

Requirements: Go (the version `launcher/go.mod` pins), `make`, and for the test
drives `curl`, `python3`, `unzip`, `tar` and `zstd`.

The Makefiles redirect `GOCACHE` and `GOMODCACHE` into the repo's `.gocache/`
(git-ignored) so the project builds in sandboxes where the system caches are
read-only. Override them on the command line if you prefer your own.

## The gates

Run what you touched. CI runs all of it.

| Command | What it covers |
|---|---|
| `make -C launcher fmt vet test race` | The launcher: formatting, vet, the suite, and the suite under `-race`. |
| `make -C packaging check race` | The packager: same, via its `check` target. |
| `make -C launcher faultinject` | The §26.4 crash-safety scenarios. **Required for anything touching storage, the updater, or the server's request path.** |
| `make -C testgame verify` | Two packaging runs produce byte-identical archives. **Required for anything touching the packager.** |
| `make -C testgame e2e` | Package → install → run → update, asserted against a real server. |
| `bash .e2e/run.sh` | The launcher smoke drive: gates, traversal, sessions, saves, drain. |

`make -C launcher cross` checks the linux/windows/darwin × amd64/arm64 build
matrix; run it if you touched a platform-specific file (`*_windows.go`,
`*_unix.go`, a build tag).

## How the code is laid out

| Where | What lives there |
|---|---|
| `launcher/cmd/kobra-launcher/` | The startup sequence of §4, flags, exit codes. |
| `launcher/internal/server/` | The HTTP server: middleware order, gates, drain, watchdog. |
| `launcher/internal/dataapi/` | The `/api/*` handlers and the request pipeline. |
| `launcher/internal/storage/` | The only writer of `data/`: saves, config, revisions, trash. |
| `launcher/internal/update/` | Download, verify, extract, swap, recover, retention. |
| `launcher/internal/{sidecar,port,browser,static,paths,config,session,diagnostics,kobraerr}/` | One concern each. |
| `packaging/internal/pack/` | Validation, staging, archives, manifests. |
| `testgame/` | The fixture both tools are tested against. |

Start with the package that already does something similar, and read its package
doc comment first: they state the invariants and cite the spec sections.

## Rules of the house

These are not style preferences; they are what the review of this codebase found
mattered.

1. **The specs are normative.** `architecture/*.md` describes required behaviour,
   and the code cites it (`§14.2`, `FR-SAVE-11`, `R10.6`) where the rule lives. If
   your change alters described behaviour, update the spec in the same pull
   request. If code and spec disagree today, that disagreement *is* the bug.
2. **Every fixed bug gets a regression test**, named after the failure rather than
   the function (`TestApplyFailedSwapKeepsTheInstalledRelease`, not
   `TestApply3`). The test should fail on the old code.
3. **Prefer deleting a claim over keeping dead machinery.** A config key, event,
   build tag or exported symbol with no consumer is worse than nothing: a reader
   has to decide whether it matters. Several review findings were exactly this.
4. **Do not widen a security gate without a test that shows why.** The gates
   (host, origin, session, CSRF, confinement, the identifier rules) each exist for
   a reason recorded in the spec. If you relax one, say which threat you are
   accepting.
5. **User-facing text never contains a filesystem path, a username, a token or a
   stack trace.** `kobraerr` is built so you cannot leak one by accident, and
   `TestNoErrorLeaksPathsOrUsername` walks every error constructor to prove it.
   Diagnostics logs are surfaced to the page, so treat them the same way.
6. **The launcher stays small.** It ships to players and keeps a three-dependency
   footprint with a vendored module tree. A new dependency needs a reason in the
   pull request; the packager is free to be more liberal because it runs on a
   publisher's machine.
7. **Comments explain why, not what.** The interesting decisions here are
   ordering decisions ("the marker is written before the first move because…"),
   and they are the ones worth writing down.
8. **No test-only overrides that mask production behaviour.** A test that sets a
   timeout, a preflight flag or a fault specifically to avoid the real path is how
   the unarmed stall watchdog and the quarantined temp file went unnoticed. If a
   test needs a seam, add the seam to production code and exercise it.

## Pull requests

Keep them small and single-purpose. A good description answers:

- **What** changed, in one or two sentences.
- **Why** — the bug, the spec rule, or the user-facing problem.
- **Which gates you ran** and their result. Paste the output if it is short.
- **What you did not do**, if you deliberately left something out.

If the change affects a shipped contract — the data API (`architecture/schemas/data-api.schema.json`),
the config schema, a manifest, an event name, an exit code — say so explicitly.
Those are the changes that break other people's games.

New behaviour needs tests. New *documented* behaviour needs the spec updated. A
behaviour change with neither will be sent back.

## Reporting a bug well

Include:

- what you did, what happened, and what you expected;
- the OS and architecture, and `launcher/launcher --version`;
- the release id (in `launcher/launcher.config.json`);
- for a launcher problem, the output of `--diagnostics` (it deliberately contains
  no paths or usernames, so it is safe to paste);
- for a packaging problem, the `kobra-pack` event line, which names the file and
  the rule it broke.

A reproduction against `testgame/` is ideal: `make -C testgame package && make -C
testgame e2e` gives a known-good baseline on your machine.

## Licence and attribution

Contributions are accepted under the MIT licence (see `LICENSE`). By opening a
pull request you confirm you have the right to submit the work under that
licence. If you add a dependency, add its notice to `THIRD_PARTY_NOTICES.md` —
the packager ships that file inside every package, and `make -C packaging test`
fails if it drifts from the repository copy.

## Conduct

Be decent to other contributors: critique the code, not the person, and assume
the other side is acting in good faith. Behaviour that makes the project
unwelcome to others is not tolerated, and maintainers may close a conversation
that has stopped being about the work.
