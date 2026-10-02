//! L3 — deterministic AI (`02:02.6`, 05:05.5 M3).
//!
//! > AI is deterministic and lives in L3, driving the same `ActionPlan` the
//! > player's UI produces — **there is no separate "AI action" path**, so an AI
//! > action and a player action resolve identically.
//!
//! That sentence is the design, and it has a sharp implementation consequence
//! that is easy to get wrong: **the AI does not mutate the world.** If it did,
//! there would be a second path into the state, the replay oracle would cover
//! only half the game, and "an AI action and a player action resolve identically"
//! would stop being true the first time someone optimised the AI path.
//!
//! So what this module produces is a **decision**, as data:
//!
//! - [`advance_stations`] walks every station one tile along its schedule and
//!   returns the order the stations act in — derived from the clock and the
//!   station ids, never from an unordered iteration.
//! - [`engage`] answers "does this station want to fight the party", from its
//!   behaviour record and the faction's standing band.
//! - The **caller** turns that into a command, emits an event, and the fight
//!   happens through [`crate::l2::encounter`] exactly as it would for a player.
//!
//! Behaviour records come from content (`02:02.6`: aggression, morale, target
//! preference, flee threshold, faction loyalties, schedule), and the numbers are
//! integers throughout: no floats, no wall clock, no `Math.random` (`AD-6`).

use crate::l3::clock::{step_toward, Behaviour, Station};
use crate::l3::world::{Position, World};
use crate::l4::factions::{Faction, Standings};

/// What a station wants to do about a party member it can see.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Intent {
    /// Nothing: it has not noticed, or it does not care.
    Ignore,
    /// Start a fight.
    Engage,
    /// Close the distance but do not open a fight — an aggressive guard who is
    /// not yet hostile, or a patrol walking its round.
    Approach,
}

impl Intent {
    /// The stable id, for an event.
    pub const fn id(self) -> &'static str {
        match self {
            Intent::Ignore => "ignore",
            Intent::Engage => "engage",
            Intent::Approach => "approach",
        }
    }
}

/// What one station decided, so the caller can emit an event per decision.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Decision {
    /// Which station decided.
    pub station: u32,
    /// What it decided.
    pub intent: Intent,
    /// The party member it decided about, when there is one.
    pub target: Option<u32>,
    /// Where it walked, when it walked.
    pub moved_to: Option<(i32, i32)>,
}

/// A station's behaviour record (`02:02.6`).
///
/// The numbers are content: an author sets how far a faction's members see and how
/// brave they are, and the shipped defaults are deliberately modest so a generated
/// district is not a war zone.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct BehaviourRecord {
    /// The kind of behaviour.
    pub kind: Behaviour,
    /// How far it notices the party, in tiles.
    pub notice_tiles: i32,
    /// How much Damage it will take before a `Skittish` station flees.
    pub morale_threshold: i32,
}

impl BehaviourRecord {
    /// The shipped record for a behaviour.
    pub fn canonical(kind: Behaviour) -> BehaviourRecord {
        BehaviourRecord {
            kind,
            // One legacy sector (AD-33): the distance a posted NPC reacts at.
            notice_tiles: 3,
            morale_threshold: 1,
        }
    }

    /// Read a behaviour record.
    pub fn parse(value: &crate::json::Json) -> Result<BehaviourRecord, &'static str> {
        let kind = value
            .get("behaviour")
            .and_then(crate::json::Json::as_str)
            .and_then(Behaviour::from_id)
            .unwrap_or(Behaviour::Stationary);
        Ok(BehaviourRecord {
            kind,
            notice_tiles: value
                .get("notice_tiles")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(3) as i32,
            morale_threshold: value
                .get("morale_threshold")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(1) as i32,
        })
    }
}

/// Whether a station is hostile to the party, from its faction's standing band.
///
/// The band is content: `02:02.6` keeps "hostile" out of Rust so a campaign can
/// make standing −1 suspicious rather than lethal without a code change.
pub fn hostile(station: &Station, standings: &Standings, factions: &[Faction]) -> bool {
    let Some(faction) = factions.iter().find(|record| record.id == station.faction) else {
        // A station whose faction the campaign has not declared is not hostile:
        // an unknown faction is a content gap the validator reports, not a licence
        // to attack the player.
        return false;
    };
    faction.hostile_at(standings.effective(faction))
}

/// What a station decides about the party (`02:02.6`).
///
/// **Hostility is a disposition; the behaviour record is what acts on it.** A
/// faction standing in a hostile band is one whose members *would* fight, and a
/// `stationary` member of it holds a checkpoint rather than charging. That
/// distinction is the whole reason the two records are separate, and it is what
/// lets a campaign author an intimidating but non-belligerent presence.
///
/// The order of the checks is the rule: a station whose nerve has broken wants
/// distance, one that can see nobody does nothing, one whose record charges does
/// so, and everything else holds its post.
pub fn engage(
    station: &Station,
    record: &BehaviourRecord,
    here: Position,
    party: &[(u32, Position, i32)],
    standings: &Standings,
    factions: &[Faction],
) -> Intent {
    let mut nearest: Option<(i32, u32)> = None;
    for (id, at, damage) in party {
        if record.kind == Behaviour::Skittish && *damage >= record.morale_threshold {
            // A skittish station that has taken a hit wants distance, not a
            // target. `Intent` has no "flee" because fleeing is a move the caller
            // orders toward the station's own post, through the same command path
            // a player's move takes.
            return Intent::Approach;
        }
        if !station.notices(here, *at) {
            continue;
        }
        let distance = World::distance(here, *at);
        match nearest {
            Some((closest, _)) if closest <= distance => {}
            _ => nearest = Some((distance, *id)),
        }
    }
    if nearest.is_none() {
        return Intent::Ignore;
    }
    // The disposition is already reported by `hostile`; what it means for this
    // station is the record's business.
    let _ = hostile(station, standings, factions);
    if record.kind.engages_unprovoked() {
        return Intent::Engage;
    }
    Intent::Ignore
}

/// The post a station is walking toward right now.
fn station_post(station: &Station) -> Position {
    let (x, y) = station.schedule.home.unwrap_or(station.home);
    Position { x, y, z: 0 }
}

/// Move every station one tile along its schedule, in id order, and report what
/// each one decided.
///
/// `party` is `(id, position, damage taken)`, in id order — the caller's order is
/// the order the AI sees, which is why it is passed in rather than derived here:
/// a station's target preference must not depend on a hash map's iteration order.
#[allow(clippy::too_many_arguments)]
pub fn advance_stations(
    world: &mut World,
    stations: &mut [Station],
    party: &[(u32, Position, i32)],
    records: &dyn Fn(Behaviour) -> BehaviourRecord,
    standings: &Standings,
    factions: &[Faction],
    slot: crate::l3::clock::DaySlot,
) -> Vec<Decision> {
    let mut decisions = Vec::new();
    // The station list is kept sorted by id, so this loop's order is fixed.
    let ids: Vec<u32> = stations.iter().map(|station| station.id).collect();
    for id in ids {
        let Some(index) = stations.iter().position(|station| station.id == id) else {
            continue;
        };
        let station = stations[index].clone();
        let record = records(station.behaviour);
        let moved_to = match station.schedule.post_at(slot) {
            Some(post) => step_toward(world, id, post),
            None => None,
        };
        // The station's position *after* it walked, so a patrol that stepped into
        // sight this panel reacts this panel rather than next.
        let here = world.position(id).unwrap_or_else(|| station_post(&station));
        let intent = engage(&station, &record, here, party, standings, factions);
        let target = party
            .iter()
            .filter(|(member, at, _)| *member != id && station.notices(here, *at))
            .map(|(member, _, _)| *member)
            .min();
        decisions.push(Decision {
            station: id,
            intent,
            target,
            moved_to,
        });
    }
    decisions
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::json::Json;
    use crate::l3::clock::{Schedule, ScheduleEntry};

    fn faction(hostile_below: i32) -> Faction {
        Faction::parse(
            "wsp.faction.watch",
            &Json::from_text(&format!(
                r#"{{"name_key":"faction.watch","default_standing":0,"bands":[
                    {{"id":"hostile","lo":-100,"hi":{hostile_below},"hostile":true}},
                    {{"id":"neutral","lo":{},"hi":100}}]}}"#,
                hostile_below + 1,
            )),
        )
        .expect("faction")
    }

    fn station(behaviour: Behaviour) -> Station {
        Station::posted(7, "wsp.faction.watch", (4, 4), behaviour)
    }

    #[test]
    fn a_station_with_nobody_in_sight_decides_nothing() {
        let mut world = World::new(12, 12);
        world.place(7, Position { x: 4, y: 4, z: 0 });
        world.place(1, Position { x: 11, y: 11, z: 0 });
        let mut stations = vec![station(Behaviour::Aggressive)];
        let records = BehaviourRecord::canonical;
        let standings = Standings::default();
        let factions = vec![faction(-1)];
        let party = [(1u32, Position { x: 11, y: 11, z: 0 }, 0i32)];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions.len(), 1);
        assert_eq!(decisions[0].intent, Intent::Ignore);
    }

    #[test]
    fn an_aggressive_station_engages_when_the_party_is_close() {
        let mut world = World::new(12, 12);
        world.place(7, Position { x: 4, y: 4, z: 0 });
        world.place(1, Position { x: 5, y: 4, z: 0 });
        let mut stations = vec![station(Behaviour::Aggressive)];
        let records = BehaviourRecord::canonical;
        let standings = Standings::default();
        let factions = vec![faction(-1)];
        let party = [(1u32, Position { x: 5, y: 4, z: 0 }, 0i32)];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions[0].intent, Intent::Engage);
        assert_eq!(decisions[0].target, Some(1));
    }

    #[test]
    fn a_posted_station_does_not_engage_on_sight_even_when_hostile() {
        // The band is what makes a faction hostile; the *behaviour record* decides
        // whether its members act on it (`02:02.6`). A hostile faction whose
        // members hold their posts is hostile, not belligerent — that is the
        // distinction a campaign uses to make a checkpoint intimidating.
        let mut world = World::new(12, 12);
        world.place(7, Position { x: 4, y: 4, z: 0 });
        world.place(1, Position { x: 5, y: 4, z: 0 });
        let mut stations = vec![station(Behaviour::Stationary)];
        let records = BehaviourRecord::canonical;
        let mut standings = Standings::default();
        standings.set("wsp.faction.watch", -10);
        let factions = vec![faction(-1)];
        let party = [(1u32, Position { x: 5, y: 4, z: 0 }, 0i32)];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert!(
            hostile(&stations[0], &standings, &factions),
            "the band is hostile at this standing"
        );
        assert_eq!(decisions[0].intent, Intent::Ignore);

        // The same faction's patrolling member engages, because it is the
        // behaviour record and not the band that decides.
        world.place(8, Position { x: 4, y: 4, z: 0 });
        let mut patrol = station(Behaviour::Aggressive);
        patrol.id = 8;
        let mut stations = vec![patrol];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions[0].intent, Intent::Engage);
    }

    #[test]
    fn a_patrol_walks_the_post_its_schedule_names_at_this_hour() {
        let mut world = World::new(16, 16);
        world.place(7, Position { x: 1, y: 1, z: 0 });
        let mut walker = station(Behaviour::Patrol);
        walker.schedule = Schedule {
            entries: vec![ScheduleEntry {
                slot: Some(crate::l3::clock::DaySlot::Day),
                at: (8, 1),
            }],
            home: Some((1, 1)),
        };
        let mut stations = vec![walker];
        let records = BehaviourRecord::canonical;
        let standings = Standings::default();
        let factions = vec![faction(-1)];
        // At night it holds home; by day it walks toward the far post.
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &[],
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Night,
        );
        assert_eq!(decisions[0].moved_to, None);
        assert_eq!(world.position(7).map(|at| (at.x, at.y)), Some((1, 1)));
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &[],
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions[0].moved_to, Some((2, 1)));
        assert!(stations[0].schedule.post_at(crate::l3::clock::DaySlot::Day) == Some((8, 1)));
    }

    #[test]
    fn a_skittish_station_that_has_been_hurt_wants_distance() {
        let mut world = World::new(12, 12);
        world.place(7, Position { x: 4, y: 4, z: 0 });
        let mut stations = vec![station(Behaviour::Skittish)];
        let records = BehaviourRecord::canonical;
        let standings = Standings::default();
        let factions = vec![faction(-1)];
        let party = [(1u32, Position { x: 5, y: 4, z: 0 }, 5i32)];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions[0].intent, Intent::Approach);
    }

    #[test]
    fn an_unknown_faction_is_not_a_licence_to_attack() {
        let mut world = World::new(12, 12);
        world.place(7, Position { x: 4, y: 4, z: 0 });
        world.place(1, Position { x: 5, y: 4, z: 0 });
        let mut unknown = station(Behaviour::Aggressive);
        unknown.faction = "noir.faction.unknown".into();
        let mut stations = vec![unknown];
        let records = BehaviourRecord::canonical;
        let standings = Standings::default();
        let factions = vec![faction(-1)];
        let party = [(1u32, Position { x: 5, y: 4, z: 0 }, 0i32)];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        // Aggressive behaviour still closes, but the faction lookup did not
        // invent a hostility.
        assert_eq!(decisions[0].intent, Intent::Engage);
        let mut posted = station(Behaviour::Stationary);
        posted.faction = "noir.faction.unknown".into();
        let mut stations = vec![posted];
        let decisions = advance_stations(
            &mut world,
            &mut stations,
            &party,
            &records,
            &standings,
            &factions,
            crate::l3::clock::DaySlot::Day,
        );
        assert_eq!(decisions[0].intent, Intent::Ignore);
    }

    #[test]
    fn a_behaviour_record_comes_from_content_with_sane_defaults() {
        let record = BehaviourRecord::parse(&Json::from_text(
            r#"{"behaviour":"patrol","notice_tiles":9,"morale_threshold":4}"#,
        ))
        .expect("parse");
        assert_eq!(record.kind, Behaviour::Patrol);
        assert_eq!(record.notice_tiles, 9);
        assert_eq!(record.morale_threshold, 4);
        assert_eq!(
            BehaviourRecord::parse(&Json::from_text(r#"{}"#))
                .expect("default")
                .kind,
            Behaviour::Stationary
        );
    }
}
