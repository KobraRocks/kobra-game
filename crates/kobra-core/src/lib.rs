//! The Kobra engine: a deterministic simulation core for 4C System CRPGs.
//!
//! This crate is the engine (AD-1, AD-38). It contains the 4C rules, the
//! simulation, the seeded PRNG, the content model and the save format — and
//! nothing else: no window, no GPU, no network, no wall clock, and no floats on
//! any path that can alter state (AD-1, AD-6).
//!
//! A Rust host links it as an `rlib` and calls it directly (AD-38). The
//! hand-written C ABI in [`abi`] is for a host that is not Rust, and it is the
//! boundary a native plugin is loaded across (AD-2, AD-40); the ABI is
//! version-stamped and every change to it is deliberate.
//!
//! # What exists
//!
//! The CRPG frame: the panel clock with time of day and schedules, the procedural
//! baseline plus per-sector delta with its generator version, the party and its
//! standing, inventory and prices, dialogue, quests, the journal, factions and
//! progression — so a player can create a character, explore, talk, fight, level
//! and save.
//!
//! Content and golden replays belong to the *game*, and live with the game. This
//! crate's own suite runs without one (`README.md`).

#![deny(unsafe_op_in_unsafe_fn)]
#![deny(missing_docs)]

pub mod abi;
pub mod content;
pub mod dsl;
pub mod error;
pub mod json;
pub mod l0;
pub mod l1;
pub mod l2;
pub mod l3;
pub mod l4;
pub mod packet;
pub mod rng;
pub mod rules;
pub mod save;
pub mod script;
pub mod sim;
pub mod tables;
pub mod validate;

/// The ABI version this build exports.
///
/// A host compares it against the version it was built against (`01:01.5` rule
/// 5). A mismatch is a packaging error — the engine and its host ship together —
/// and must fail cleanly rather than misbehave.
///
/// M3 adds the exploration, dialogue, inventory, journal and progression
/// projections and commands, so the surface grew: version 4. The interface
/// milestone adds the available-action projection the shell's contextual bar is
/// built from: version 5. The moddable interface adds `kobra_signals`, the display
/// gates a layout may key on, and `kobra_check_ui` (`09`): version 6.
pub const ABI_VERSION: u32 = 6;

/// The engine version, mirrored into `engine.manifest.json`.
///
/// Bare `X.Y.Z`, never SemVer with a prerelease suffix (VERSIONING.md, AGENTS.md
/// trap 10): the release manifest's `channel` carries pre-release status.
pub const ENGINE_VERSION: &str = "0.2.0";

/// The save format version, mirrored into `engine.manifest.json`.
///
/// Version 2 carries the M3 world delta: the per-sector delta from the procedural
/// baseline, the clock, the stations, the standings, the journal and the party's
/// inventory, where version 1 carried entity placements and the encounter clock
/// (`save.rs`).
pub const SAVE_VERSION: u32 = 2;
