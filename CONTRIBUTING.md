# Contributing

This repository holds the engine for a single-player, offline, deterministic CRPG on
the 4C System. Its first game, Worldspiracy, lives in its own repository and is built
on the engine's public surface (`AD-38`–`AD-42`).

## Setup

- The pinned Rust toolchain, via `rustup` (`rust-toolchain.toml` pins 1.98.0).
- `python3`, for the Master Table property test.

Until the extraction lands, the engine crate is checked out under `games/` — see
`AGENTS.md` — and the gates run from there.

## The gates

```sh
cd games/worldspiracy && make check     # tables, formatting, tests
cd games/worldspiracy && make clippy    # lints, warnings denied
cd games/worldspiracy && make tables    # regenerate from the authoritative CSV
```

Run the ones that cover what you touched, and say which in your pull request.

## Pull requests

Keep them small and single-purpose. A good description answers:

- **What** changed, in one or two sentences.
- **Why** — the bug, the spec rule, or the user-facing problem.
- **Which gates you ran** and their result.
- **What you did not do**, if you deliberately left something out.

New behaviour needs a test. New *documented* behaviour needs the specification
updated in the same change. A behaviour change with neither will be sent back.

## What we do not want

- A second implementation of a rule the engine already resolves.
- A dependency in the engine core without a stated reason.
- A game's name in engine code or in an engine wire contract.
- Machinery with no consumer, kept "for later".
- Configuration or extension points for features that do not exist yet.

## Reporting a bug

Include what you did, what you expected, what happened, and the smallest input that
reproduces it. For a rules bug, the command stream and the state hash are worth more
than a screenshot — the replay fixtures are the same shape (`AD-21`).
