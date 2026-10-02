# 01 — Runtime and language split (retired)

**This document described the browser runtime, which `AD-38` removed.** There is no
wasm module, no worker, no shell script and no WebGPU page: the engine is a Rust
crate that a native host links (`AD-38`), and Lua is the script tier (`AD-39`).

It is a stub rather than a deletion because code and specifications cite its
sections. Those citations mean the following, and each has a live statement:

| Cited as | It described | Live statement |
|---|---|---|
| `01:01.2` | the artifact split: what is the core and what is the host | `AD-38`; the invariants in [README.md](README.md) |
| `01:01.3` | the frame description the simulation hands the renderer | `crates/kobra-core/src/packet.rs` |
| `01:01.5` | the version-stamped ABI, and that a mismatch fails cleanly | `AD-2`, `crates/kobra-core/src/abi.rs` |
| `01:01.6` | integer-only wire formats and no floats on state paths | `AD-6` |
| `01:01.8`, `01:01.9` | boot, and the session the page held | retired: `AD-41`, `AD-42` |
| `01:01.12` | diagnostics never carry a path, a username or a stack trace | `crates/kobra-core/src/abi.rs` — the host installs a log sink and decides |
| `01:01.13` | the development-mode signal a page read from the launcher | retired with the launcher (`AD-42`) |
