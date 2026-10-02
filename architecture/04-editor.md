# 04 — The shipped editor (retired)

**This document described an editor served in the browser from the launcher's
origin, which `AD-38` and `AD-42` removed.** Authoring is headless-first now —
project files, a schema-driven validate step, a Lua lint and a packer — and the
graphical editor is a modder-facing tool built later (`AD-42`). The ideas worth
re-implementing, and the reason each one exists, are recorded in `AD-42`.

It is a stub rather than a deletion because code and specifications cite its
sections:

| Cited as | It described | Live statement |
|---|---|---|
| `04:04.4` | a project is a versioned document | `AD-42` |
| `04:04.5` | project storage as save slots, and revision conflicts | `AD-11`, `AD-42` |
| `04:04.6` | validation surfaced through the engine's own validator | `AD-15` |
| `04:04.7` | publish: one archive writer, a size guard, and re-reading what was written | `AD-13`, `AD-42` |
