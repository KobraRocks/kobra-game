//! L3 — the panel clock, time of day, and schedules (02:02.5, 02:02.7).
//!
//! **One time unit: the panel.** A panel is the quantum for every rules duration
//! ("1d10 turns", "3 turns", "1-10 turns"), and the encounter machine has two
//! drivers over the same clock (`02:02.5`):
//!
//! ```text
//! exploration: real-time — 1 panel elapses per EXPLORATION_PANEL_MS (content)
//! combat:      panel-driven — a panel advances when both sides have acted
//! ```
//!
//! So a buff that lasts three panels decays while the player walks around and
//! lasts exactly three combat panels in a fight. That is deliberate: it removes
//! the classic CRPG bug where a "3 turn" spell is effectively infinite because
//! the player walked away.
//!
//! The clock is **simulation state**, never wall-clock state (`AD-1`, `AD-6`):
//! the caller decides how many panels elapsed, and the sim turns them into a
//! time of day, fires the schedules that come due, and moves the world's
//! stations accordingly. A day is `panels_per_day` panels (content, default
//! 1440 — one panel per minute), so a full day/night cycle is a content number
//! rather than a constant in the core.
//!
//! **Schedules are the world's source of motion.** `02:02.6` lists schedules
//! among the things a campaign seed deterministically generates, and `02:02.7`
//! keys them to the clock. The unit here is a [`Station`]: an NPC that holds a
//! post, walks to the post its current schedule names, and behaves there. Its
//! behaviour comes from [`Behaviour`], which is the same *record* the AI reads in
//! combat — so the town guard who patrols at noon and the one who fights in the
//! alley are the same entity with the same numbers.

use crate::json::Json;
use crate::l3::world::{Position, World};

/// A full day of panels when no `rules.time.panels_per_day` is declared.
///
/// One panel per minute is the shipped reading: it makes `4c`'s "1-10 turns"
/// durations read as minutes, and it is a **content** number, so a campaign that
/// wants a different pace changes a record rather than the engine.
pub const DEFAULT_PANELS_PER_DAY: u32 = 1440;

/// The parts of a day a schedule or a condition can name.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub enum DaySlot {
    /// Before dawn.
    Night,
    /// Dawn to noon.
    Morning,
    /// Noon to dusk.
    Day,
    /// Dusk to midnight.
    Evening,
}

impl DaySlot {
    /// Every slot, so a validator can enumerate the domain.
    pub const ALL: [DaySlot; 4] = [
        DaySlot::Night,
        DaySlot::Morning,
        DaySlot::Day,
        DaySlot::Evening,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            DaySlot::Night => "night",
            DaySlot::Morning => "morning",
            DaySlot::Day => "day",
            DaySlot::Evening => "evening",
        }
    }

    /// The slot a content id names.
    pub fn from_id(id: &str) -> Option<DaySlot> {
        DaySlot::ALL.into_iter().find(|slot| slot.id() == id)
    }
}

/// The simulation clock: an integer day and an integer panel within it.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Clock {
    /// Days elapsed since the campaign began; day 1 is the first day.
    pub day: u32,
    /// Panels elapsed within the current day, `0..panels_per_day`.
    pub panel: u32,
    /// A full day's length in panels (`rules.time.panels_per_day`).
    pub panels_per_day: u32,
}

impl Clock {
    /// A clock at dawn on day one.
    pub fn new(panels_per_day: u32) -> Clock {
        Clock {
            day: 1,
            panel: 0,
            panels_per_day: panels_per_day.max(1),
        }
    }

    /// Panels since the campaign began.
    pub fn total(&self) -> u64 {
        u64::from(self.day.saturating_sub(1)) * u64::from(self.panels_per_day)
            + u64::from(self.panel)
    }

    /// Advance by whole panels, carrying into days.
    pub fn advance(&mut self, panels: u32) {
        let length = self.panels_per_day.max(1);
        let total = self.panel + panels;
        self.day = self.day.saturating_add(total / length);
        self.panel = total % length;
    }

    /// The minute of the day this panel lands on, for the UI clock.
    ///
    /// A day maps onto 24 hours, so the panel counter and the wall clock stay
    /// one linear quantity rather than two (`02:02.7`).
    pub fn minute_of_day(&self) -> u32 {
        (u64::from(self.panel) * 1440 / u64::from(self.panels_per_day.max(1))) as u32
    }

    /// Which quarter of the day this panel is in.
    pub fn slot(&self) -> DaySlot {
        match self.minute_of_day() {
            0..=299 => DaySlot::Night,
            300..=719 => DaySlot::Morning,
            720..=1079 => DaySlot::Day,
            _ => DaySlot::Evening,
        }
    }

    /// The clock as a document, for the shell and for diagnostics.
    pub fn to_json(&self) -> Json {
        Json::obj([
            ("day", Json::Num(i64::from(self.day))),
            ("panel", Json::Num(i64::from(self.panel))),
            ("panels_per_day", Json::Num(i64::from(self.panels_per_day))),
            ("minute_of_day", Json::Num(i64::from(self.minute_of_day()))),
            ("slot", Json::string(self.slot().id())),
        ])
    }

    /// Write the clock into a save stream.
    pub fn write(&self, out: &mut Vec<u8>) {
        push_u32(out, self.day);
        push_u32(out, self.panel);
        push_u32(out, self.panels_per_day);
    }

    /// Read a clock from a save stream.
    pub fn read(reader: &mut crate::save::Reader<'_>) -> Result<Clock, &'static str> {
        let day = reader.u32()?;
        let panel = reader.u32()?;
        let panels_per_day = reader.u32()?.max(1);
        Ok(Clock {
            day,
            panel,
            panels_per_day,
        })
    }
}

/// One station's standing orders (`02:02.6`): where it belongs, and when.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct ScheduleEntry {
    /// The day slot this post applies to.
    pub slot: Option<DaySlot>,
    /// The post, in tiles.
    pub at: (i32, i32),
}

/// An NPC's deterministic pattern of life (`02:02.6`, `02:02.7`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Schedule {
    /// The ordered posts. An empty schedule means "stay put".
    pub entries: Vec<ScheduleEntry>,
    /// The post to hold when no entry's slot matches.
    pub home: Option<(i32, i32)>,
}

impl Schedule {
    /// The post that applies at a time of day.
    ///
    /// The **last** matching entry wins, so a schedule reads as a diary: a
    /// general morning post followed by a specific noon exception.
    pub fn post_at(&self, slot: DaySlot) -> Option<(i32, i32)> {
        self.entries
            .iter()
            .rfind(|entry| entry.slot.is_none() || entry.slot == Some(slot))
            .map(|entry| entry.at)
            .or(self.home)
    }
}

/// How an NPC behaves where it stands (`02:02.6`).
///
/// This is the same record the combat AI reads, which is what makes "the guard
/// who patrols by day and fights in the alley" one entity rather than two.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Behaviour {
    /// Holds its post and never moves on its own.
    Stationary,
    /// Walks the posts its schedule names.
    Patrol,
    /// Moves toward the nearest party member it can see, and attacks on contact.
    Aggressive,
    /// Attacks only when attacked first.
    Defensive,
    /// Flees toward its home post when hurt.
    Skittish,
}

impl Behaviour {
    /// Every behaviour, so a validator can enumerate the domain.
    pub const ALL: [Behaviour; 5] = [
        Behaviour::Stationary,
        Behaviour::Patrol,
        Behaviour::Aggressive,
        Behaviour::Defensive,
        Behaviour::Skittish,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            Behaviour::Stationary => "stationary",
            Behaviour::Patrol => "patrol",
            Behaviour::Aggressive => "aggressive",
            Behaviour::Defensive => "defensive",
            Behaviour::Skittish => "skittish",
        }
    }

    /// The behaviour a content id names.
    pub fn from_id(id: &str) -> Option<Behaviour> {
        Behaviour::ALL.into_iter().find(|kind| kind.id() == id)
    }

    /// Whether this behaviour closes on the party without being provoked.
    ///
    /// The L3-AI half of `02:02.6`: an action the AI decides is the same
    /// `ActionPlan` the player's UI produces, so it is emitted as an event and
    /// driven through the command stream rather than mutating the world here.
    pub const fn engages_unprovoked(self) -> bool {
        matches!(self, Behaviour::Aggressive)
    }
}

/// One NPC standing in the world, with its post and its orders.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Station {
    /// The entity id.
    pub id: u32,
    /// The behaviour record (`02:02.6`).
    pub behaviour: Behaviour,
    /// The faction the station belongs to (`02:02.6`).
    pub faction: String,
    /// Where it belongs when nothing else applies.
    pub home: (i32, i32),
    /// What it does with its day.
    pub schedule: Schedule,
    /// How far it will notice the party, in tiles.
    pub notice_tiles: i32,
}

impl Station {
    /// A station that holds one post.
    pub fn posted(id: u32, faction: &str, at: (i32, i32), behaviour: Behaviour) -> Station {
        Station {
            id,
            behaviour,
            faction: faction.to_string(),
            home: at,
            schedule: Schedule {
                entries: Vec::new(),
                home: Some(at),
            },
            // One legacy sector: the distance at which a posted NPC reacts.
            notice_tiles: 3,
        }
    }

    /// Whether the station is close enough to notice the party.
    pub fn notices(&self, from: Position, to: Position) -> bool {
        World::distance(from, to) <= self.notice_tiles.max(0)
    }
}

/// Move one station a single tile toward its post, if it is not already there.
///
/// A **greedy keystep**, not a path search: one tile per panel, preferring the
/// larger axis and refusing an occupied or blocked tile. That bounds the work
/// per panel, keeps the motion reproducible from the clock alone, and means an
/// NPC that cannot reach its post simply holds the closest tile it can — which
/// is a legible behaviour rather than a pathfinding stall.
///
/// Returns the tile it moved to, or `None` when it was already there or had no
/// legal step.
pub fn step_toward(world: &mut World, id: u32, post: (i32, i32)) -> Option<(i32, i32)> {
    let at = world.position(id)?;
    let dx = post.0 - at.x;
    let dy = post.1 - at.y;
    if dx == 0 && dy == 0 {
        return None;
    }
    // The longer axis first, then the shorter: a diagonal step when both are
    // free, a single-axis step otherwise.
    let horizontal = (dx.abs() >= dy.abs() && dx != 0) || dy == 0;
    let candidates = if horizontal {
        [(at.x + dx.signum(), at.y), (at.x, at.y + dy.signum())]
    } else {
        [(at.x, at.y + dy.signum()), (at.x + dx.signum(), at.y)]
    };
    for (x, y) in candidates {
        if x == at.x && y == at.y {
            continue;
        }
        let Some(tile) = world.tile(x, y) else {
            continue;
        };
        if tile.blocking > 0 || world.occupant(x, y).is_some() {
            continue;
        }
        let level = tile.level;
        world.place(id, Position { x, y, z: level });
        return Some((x, y));
    }
    None
}

/// Append a little-endian `u32`.
pub fn push_u32(out: &mut Vec<u8>, value: u32) {
    out.extend_from_slice(&value.to_le_bytes());
}

/// Append a little-endian `i32`.
pub fn push_i32(out: &mut Vec<u8>, value: i32) {
    out.extend_from_slice(&value.to_le_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_day_is_a_content_number_and_panels_carry_into_it() {
        let mut clock = Clock::new(10);
        assert_eq!(clock.day, 1);
        clock.advance(10);
        assert_eq!((clock.day, clock.panel), (2, 0));
        clock.advance(25);
        assert_eq!(
            (clock.day, clock.panel),
            (4, 5),
            "25 panels is two days and five"
        );
        assert_eq!(clock.total(), 35);
    }

    #[test]
    fn the_time_of_day_quarters_tile_the_day_once() {
        let mut clock = Clock::new(1440);
        let mut seen = [0usize; 4];
        for panel in 0..1440 {
            clock.panel = panel;
            seen[clock.slot() as usize] += 1;
        }
        assert_eq!(seen.iter().sum::<usize>(), 1440);
        assert!(
            seen.iter().all(|count| *count > 0),
            "every slot is reachable"
        );
        clock.panel = 720;
        assert_eq!(clock.minute_of_day(), 720);
        assert_eq!(clock.slot(), DaySlot::Day);
    }

    #[test]
    fn the_last_matching_schedule_entry_wins() {
        let schedule = Schedule {
            entries: vec![
                ScheduleEntry {
                    slot: None,
                    at: (1, 1),
                },
                ScheduleEntry {
                    slot: Some(DaySlot::Day),
                    at: (5, 5),
                },
            ],
            home: Some((0, 0)),
        };
        assert_eq!(schedule.post_at(DaySlot::Morning), Some((1, 1)));
        assert_eq!(schedule.post_at(DaySlot::Day), Some((5, 5)));
    }

    #[test]
    fn a_station_walks_one_tile_per_step_and_stops_at_its_post() {
        let mut world = World::new(6, 6);
        world.place(7, Position { x: 0, y: 0, z: 0 });
        for _ in 0..3 {
            step_toward(&mut world, 7, (3, 0));
        }
        assert_eq!(world.position(7).map(|at| (at.x, at.y)), Some((3, 0)));
        // Already there: no step, and no error.
        assert_eq!(step_toward(&mut world, 7, (3, 0)), None);
    }

    #[test]
    fn a_station_does_not_walk_into_a_wall_or_a_body() {
        let mut world = World::new(4, 4);
        world.set_tile(
            1,
            0,
            crate::l3::world::Tile {
                level: 0,
                blocking: 3,
                terrain: 1,
                material: 30,
            },
        );
        world.place(7, Position { x: 0, y: 0, z: 0 });
        world.place(8, Position { x: 0, y: 1, z: 0 });
        // Both the direct step (a wall) and the fallback (a body) are refused.
        assert_eq!(step_toward(&mut world, 7, (3, 0)), None);
    }
}
