//! L3 — the world (02:02.6, 02:02.12, 05:05.5 M3).
//!
//! Sectors, movement, occupancy, elevation and sight; the clock, the schedules
//! and the stations that live on it; the procedural baseline the save's delta is
//! measured against; and the behaviour records AI reads. M1's world was one small
//! authored map of 1 m tiles (`AD-33`); M3 adds the generator, the clock and the
//! AI, and the authored map becomes an *override* on a baseline rather than the
//! whole world.

pub mod ai;
pub mod clock;
pub mod gen;
pub mod world;

pub use clock::{
    Behaviour, Clock, DaySlot, Schedule, ScheduleEntry, Station, DEFAULT_PANELS_PER_DAY,
};
pub use gen::{Baseline, Generator};
pub use world::{supercover, Placement, Position, Sight, Tile, World};
