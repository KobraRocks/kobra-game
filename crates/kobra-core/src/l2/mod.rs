//! L2 — the encounter machine (02:02.5).
//!
//! Time is measured in **panels**, initiative is per side, actions resolve
//! through the Master Table, and damage is a pipeline. It depends on L1 and L0,
//! and the world it fights over arrives from L3.

pub mod actions;
pub mod damage;
pub mod encounter;
pub mod vehicle;

pub use actions::{colour_name, Action, ActionKind, Declared, Effect, OutcomeTable};
pub use damage::Damage;
pub use encounter::{commit, Battle, Encounter, Phase, Queued, RulesContent};
pub use vehicle::{Difficulty, Struck, Vehicle};
