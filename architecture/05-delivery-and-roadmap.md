# 05 — Delivery, toolchain, and roadmap (retired)

**This document described the web toolchain — its repository layout, build
pipeline, packaging, launcher configuration and browser-era gates — which `AD-38`
and `AD-42` removed.** It also carried the first game's milestone history, and that
history belongs to the game: it is in that game's `CHANGELOG.md`, in its own
repository.

The engine's delivery is described by what runs it: `README.md`, `Makefile`, and
`.github/workflows/ci.yml`. This is a stub rather than a deletion because code and
specifications cite its sections:

| Cited as | It described | Live statement |
|---|---|---|
| `05:05.2` | the build pipeline | `Makefile`; `tools/gen-tables/` |
| `05:05.4` | the gates | `make check`; this repository's CI |
| `05:05.5` | the milestone list | the first game's `CHANGELOG.md` |
| `05:05.6`, `05:05.7` | open risks | the open questions in [README.md](README.md) |
