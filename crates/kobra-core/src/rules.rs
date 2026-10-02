//! The rules pack: every distance-bearing number, as content (02:02.6, AD-33).
//!
//! Every distance-bearing rule is a content parameter in **integer tiles**,
//! shipped in the rules pack alongside the ladders and rank bands. The
//! simulation never reads metres and never learns a weapon's name.
//!
//! Three properties make the set safe to edit, and all three are enforced here
//! rather than documented:
//!
//! - **One granularity block, locked.** `tile_mm` and `legacy_sector_tiles` are
//!   structural and **not moddable** (`AD-14`, `AD-33`): changing them changes
//!   the world's geometry, which is a generator-class change, not a balance
//!   tweak. [`Rules::parse`] refuses anything but the shipped values.
//! - **The 4C tables are authored verbatim in legacy sectors and converted
//!   once.** A deviation from `4c` is a deviation in the *source's* unit.
//! - **Per-distance modifiers encode their own unit in the key.** `4c:880`'s
//!   attack and dodge penalties, `4c:995`'s range penalty and `4c:1004`'s rush
//!   bonus are all per distance, so a naive "sector to tile" rewrite would
//!   silently triple them. They are stored as `{steps, per_tiles}`, and
//!   `per_tiles` **must** equal `legacy_sector_tiles`.

use crate::json::Json;
use crate::l1::character::{CountTable, GenerationTable};
use crate::l4::economy::PriceTable;
use crate::l4::progression::ImpactScale;

/// The one true tile size, in millimetres (`AD-33`).
pub const TILE_MM: i32 = 1000;

/// The one true legacy-sector conversion, in tiles (`AD-33`).
///
/// 10 ft is 3.048 m, rounded to 3 so every derived distance stays an integer.
pub const LEGACY_SECTOR_TILES: i32 = 3;

/// A row-step modifier expressed per distance (`02:02.6`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct PerDistance {
    /// Row steps per `per_tiles` tiles, signed.
    pub steps: i32,
    /// The distance each step spans, in tiles. Must equal
    /// [`LEGACY_SECTOR_TILES`] for the shipped profile.
    pub per_tiles: i32,
}

impl PerDistance {
    /// The row steps this modifier imposes after moving `tiles` tiles.
    ///
    /// Truncating division: the first `per_tiles - 1` tiles are free, which is
    /// what "for every sector you move into" means when a sector is three tiles.
    pub fn steps_for(&self, tiles: i32) -> i32 {
        if self.per_tiles <= 0 {
            return 0;
        }
        (tiles / self.per_tiles) * self.steps
    }

    /// Read a `{steps, per_tiles}` record.
    pub fn parse(value: &Json) -> Result<PerDistance, &'static str> {
        Ok(PerDistance {
            steps: value
                .get("steps")
                .and_then(Json::as_i64)
                .ok_or("a per-distance modifier needs steps")? as i32,
            per_tiles: value
                .get("per_tiles")
                .and_then(Json::as_i64)
                .ok_or("a per-distance modifier needs per_tiles")? as i32,
        })
    }
}

/// A movement allowance band (`4c:897-914`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct MovementBand {
    /// Highest Coordination Rank Value this band covers.
    pub max_rv: i32,
    /// Tiles of movement per panel.
    pub tiles: i32,
}

/// What an elevation shift applies to (`02:02.12`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ElevationTarget {
    /// Close combat. Excluded by default, because 4C's exclusion is about a
    /// blow's geometry over one legacy sector; under `AD-33` a campaign that
    /// wants clifftop duels should add it, and the key already supports it.
    Melee,
    /// Attacks across a distance.
    Ranged,
    /// Power use.
    Power,
    /// Perception and detection.
    Perception,
}

impl ElevationTarget {
    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ElevationTarget::Melee => "melee",
            ElevationTarget::Ranged => "ranged",
            ElevationTarget::Power => "power",
            ElevationTarget::Perception => "perception",
        }
    }

    /// The target a content id names.
    pub fn from_id(id: &str) -> Option<ElevationTarget> {
        [
            ElevationTarget::Melee,
            ElevationTarget::Ranged,
            ElevationTarget::Power,
            ElevationTarget::Perception,
        ]
        .into_iter()
        .find(|target| target.id() == id)
    }
}

/// The rules pack's effective values.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Rules {
    /// Tile size in millimetres; structural.
    pub tile_mm: i32,
    /// Tiles per legacy sector; structural.
    pub legacy_sector_tiles: i32,
    /// `4c:880`: the attack penalty for moving.
    pub attack_penalty: PerDistance,
    /// `4c:880`: the dodge penalty for moving.
    pub dodge_penalty: PerDistance,
    /// Movement allowances by Coordination Rank Value (`4c:897-914`).
    pub movement: Vec<MovementBand>,
    /// Row steps per level of elevation (`02:02.12`).
    pub elevation_steps: i32,
    /// The elevation cap, so high ground cannot make a target unhittable.
    pub elevation_cap: i32,
    /// What elevation applies to.
    pub elevation_applies_to: Vec<ElevationTarget>,
    /// The cover row-step penalty (`02:02.12`).
    pub cover_steps: i32,
    /// The cover cap, so cover and elevation cannot stack without limit.
    pub cover_cap: i32,
    /// Tiles a Pound Black knocks a defender back (`4c:1175`).
    pub knockback_tiles: i32,
    /// Whether initiative adds the side's highest Awareness (`4c:891`).
    pub initiative_awareness: bool,
    /// Point-buy budget (`02:02.3`).
    pub budget_points: i32,
    /// Point-buy cost of one skill (`02:02.3`).
    pub skill_cost: i32,
    /// The determining-Rank-Value table (`D1`).
    pub generation: GenerationTable,
    /// The skill-count table (`4c:264-270`).
    pub skill_table: CountTable,
    /// How many panels make a day (`02:02.7`).
    ///
    /// One panel per minute by default, so `4c`'s "1-10 turns" durations read as
    /// minutes and a full day/night cycle is 1440 panels.
    pub panels_per_day: i32,
    /// The Fortune cost curve for advancement (`4c:1381-1391`).
    pub advancement: crate::l4::progression::Advancement,
    /// The Fortune and Repute an event of a given impact moves (`4c:1229-1251`,
    /// `D20`).
    pub impact: ImpactScale,
    /// The price curve by Lifestyle band (`4c:1255-1272`).
    pub prices: PriceTable,
    /// How many items a character may carry (`02:02.7`).
    pub carry_slots: i32,
}

impl Default for Rules {
    fn default() -> Rules {
        Rules {
            tile_mm: TILE_MM,
            legacy_sector_tiles: LEGACY_SECTOR_TILES,
            // 4c:880: "-1 Row Step penalty to your attack for every sector you
            // move into". A sector is three tiles, so the step is per three.
            attack_penalty: PerDistance {
                steps: -1,
                per_tiles: LEGACY_SECTOR_TILES,
            },
            dodge_penalty: PerDistance {
                steps: -1,
                per_tiles: LEGACY_SECTOR_TILES,
            },
            movement: vec![
                MovementBand {
                    max_rv: 2,
                    tiles: 3,
                },
                MovementBand {
                    max_rv: 29,
                    tiles: 6,
                },
                MovementBand {
                    max_rv: i32::MAX,
                    tiles: 9,
                },
            ],
            elevation_steps: 1,
            elevation_cap: 2,
            elevation_applies_to: vec![
                ElevationTarget::Ranged,
                ElevationTarget::Power,
                ElevationTarget::Perception,
            ],
            cover_steps: -2,
            cover_cap: -4,
            knockback_tiles: LEGACY_SECTOR_TILES,
            initiative_awareness: true,
            budget_points: 200,
            skill_cost: 25,
            generation: GenerationTable::canonical(),
            skill_table: CountTable::canonical(),
            panels_per_day: crate::l3::clock::DEFAULT_PANELS_PER_DAY as i32,
            advancement: crate::l4::progression::Advancement::canonical(),
            impact: ImpactScale::canonical(),
            prices: PriceTable::canonical(),
            carry_slots: 12,
        }
    }
}

impl Rules {
    /// The movement allowance for a Coordination Rank Value.
    pub fn movement_tiles(&self, coordination_rv: i32) -> i32 {
        band_tiles(&self.movement, coordination_rv)
    }

    /// Whether elevation applies to an action kind.
    pub fn elevation_applies(&self, target: ElevationTarget) -> bool {
        self.elevation_applies_to.contains(&target)
    }

    /// Read the rules record (`03:03.5`).
    ///
    /// Anything the record omits keeps its shipped default, so a campaign tunes
    /// one key without restating the block. The structural fields are checked,
    /// not merged: a record that changes `tile_mm` or `legacy_sector_tiles` is
    /// refused rather than honoured (`AD-14`, `AD-33`).
    pub fn parse(data: &Json) -> Result<Rules, &'static str> {
        let mut rules = Rules::default();
        if let Some(granularity) = data.get("granularity") {
            let tile_mm = granularity
                .get("tile_mm")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(TILE_MM));
            if tile_mm != i64::from(TILE_MM) {
                return Err("granularity.tile_mm is structural and may not be changed (AD-33)");
            }
            let legacy = granularity
                .get("legacy_sector_tiles")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(LEGACY_SECTOR_TILES));
            if legacy != i64::from(LEGACY_SECTOR_TILES) {
                return Err(
                    "granularity.legacy_sector_tiles is structural and may not be changed (AD-33)",
                );
            }
        }
        if let Some(per_distance) = data.get("per_distance") {
            for (key, field) in [
                ("attack_penalty", &mut rules.attack_penalty),
                ("dodge_penalty", &mut rules.dodge_penalty),
            ] {
                if let Some(value) = per_distance.get(key) {
                    *field = PerDistance::parse(value)?;
                    if field.per_tiles != rules.legacy_sector_tiles {
                        // 02:02.6: this is the most dangerous key in the set;
                        // the loader asserts it.
                        return Err(
                            "per_distance.per_tiles must equal legacy_sector_tiles (AD-33)",
                        );
                    }
                }
            }
        }
        if let Some(legacy) = data.get("legacy") {
            if let Some(Json::Arr(sectors)) = legacy.get("movement_sectors") {
                rules.movement = movement_bands(sectors, &[2, 29])?;
            }
        }
        if let Some(elevation) = data.get("elevation") {
            rules.elevation_steps = elevation
                .get("steps_per_level")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.elevation_steps))
                as i32;
            rules.elevation_cap = elevation
                .get("cap")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.elevation_cap)) as i32;
            if let Some(Json::Arr(targets)) = elevation.get("applies_to") {
                let mut parsed = Vec::new();
                for target in targets {
                    parsed.push(
                        ElevationTarget::from_id(
                            target
                                .as_str()
                                .ok_or("applies_to entries must be strings")?,
                        )
                        .ok_or("unknown elevation target")?,
                    );
                }
                rules.elevation_applies_to = parsed;
            }
        }
        if let Some(cover) = data.get("cover") {
            rules.cover_steps = cover
                .get("steps")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.cover_steps)) as i32;
            rules.cover_cap = cover
                .get("cap")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.cover_cap)) as i32;
        }
        for (key, field) in [("knockback_tiles", &mut rules.knockback_tiles)] {
            if let Some(value) = data.get(key).and_then(Json::as_i64) {
                *field = value as i32;
            }
        }
        if let Some(value) = data.get("initiative_awareness").and_then(Json::as_bool) {
            rules.initiative_awareness = value;
        }
        if let Some(creation) = data.get("creation") {
            rules.budget_points = creation
                .get("budget_points")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.budget_points)) as i32;
            rules.skill_cost = creation
                .get("skill_cost")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.skill_cost)) as i32;
        }
        if let Some(value) = data.get("generation_table") {
            rules.generation = GenerationTable::parse(value)?;
        }
        if let Some(value) = data.get("skill_table") {
            rules.skill_table = CountTable::parse(value)?;
        }
        if let Some(time) = data.get("time") {
            let panels = time
                .get("panels_per_day")
                .and_then(Json::as_i64)
                .unwrap_or(i64::from(rules.panels_per_day));
            if panels <= 0 {
                return Err("rules.time.panels_per_day must be positive");
            }
            rules.panels_per_day = panels as i32;
        }
        if let Some(value) = data.get("advancement") {
            rules.advancement = crate::l4::progression::Advancement::parse(value)?;
        }
        if let Some(value) = data.get("impact") {
            rules.impact = ImpactScale::parse(value)?;
        }
        if let Some(value) = data.get("economy") {
            rules.prices = PriceTable::parse(value)?;
            if let Some(slots) = value.get("carry_slots").and_then(Json::as_i64) {
                if slots < 1 {
                    return Err("rules.economy.carry_slots must be at least one");
                }
                rules.carry_slots = slots as i32;
            }
        }
        Ok(rules)
    }
}

/// Turn `legacy` sector counts into tile bands with the RV thresholds the
/// source's own table names.
fn movement_bands(sectors: &[Json], thresholds: &[i32]) -> Result<Vec<MovementBand>, &'static str> {
    let mut counts = Vec::new();
    for value in sectors {
        counts.push(value.as_i64().ok_or("a movement count must be a number")? as i32);
    }
    if counts.is_empty() {
        return Err("a movement table must not be empty");
    }
    let mut bands = Vec::new();
    for (index, count) in counts.iter().enumerate() {
        let max_rv = thresholds.get(index).copied().unwrap_or(i32::MAX);
        bands.push(MovementBand {
            max_rv,
            tiles: count * LEGACY_SECTOR_TILES,
        });
    }
    // The last band must be open-ended, or a high Rank Value would fall off the
    // end of the table and move zero tiles.
    if let Some(last) = bands.last_mut() {
        last.max_rv = i32::MAX;
    }
    Ok(bands)
}

/// The tiles a band grants.
fn band_tiles(bands: &[MovementBand], rv: i32) -> i32 {
    bands
        .iter()
        .find(|band| rv <= band.max_rv)
        .map(|band| band.tiles)
        .unwrap_or_else(|| bands.last().map(|band| band.tiles).unwrap_or(0))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_defaults_convert_legacy_sectors_once() {
        let rules = Rules::default();
        // 4c:897-914: 1/2/3 sectors become 3/6/9 tiles.
        assert_eq!(rules.movement_tiles(2), 3);
        assert_eq!(rules.movement_tiles(6), 6);
        assert_eq!(rules.movement_tiles(20), 6);
        assert_eq!(rules.movement_tiles(30), 9);
        assert_eq!(rules.movement_tiles(1000), 9);
        // Every per-distance key keeps its own unit and matches the conversion.
        for modifier in [rules.attack_penalty, rules.dodge_penalty] {
            assert_eq!(modifier.per_tiles, LEGACY_SECTOR_TILES);
        }
    }

    #[test]
    fn a_per_distance_penalty_does_not_triple_by_accident() {
        // 02:02.6: a naive sector-to-tile rewrite would triple this.
        let rules = Rules::default();
        assert_eq!(rules.attack_penalty.steps_for(2), 0);
        assert_eq!(rules.attack_penalty.steps_for(3), -1);
        assert_eq!(rules.attack_penalty.steps_for(8), -2);
        assert_eq!(rules.dodge_penalty.steps_for(6), -2);
    }

    #[test]
    fn granularity_is_locked_even_for_a_mod() {
        assert!(Rules::parse(&Json::from_text(
            r#"{"granularity":{"tile_mm":2000,"legacy_sector_tiles":3}}"#
        ))
        .is_err());
        assert!(Rules::parse(&Json::from_text(
            r#"{"granularity":{"tile_mm":1000,"legacy_sector_tiles":1}}"#
        ))
        .is_err());
        assert!(Rules::parse(&Json::from_text(
            r#"{"granularity":{"tile_mm":1000,"legacy_sector_tiles":3}}"#
        ))
        .is_ok());
    }

    #[test]
    fn a_per_distance_override_with_the_wrong_unit_is_refused() {
        assert!(Rules::parse(&Json::from_text(
            r#"{"per_distance":{"attack_penalty":{"steps":-1,"per_tiles":1}}}"#
        ))
        .is_err());
    }

    #[test]
    fn a_record_keeps_every_default_it_omits() {
        let rules = Rules::parse(&Json::from_text(r#"{"knockback_tiles":6}"#)).expect("parse");
        assert_eq!(rules.knockback_tiles, 6);
        assert_eq!(rules.elevation_cap, 2);
        assert!(rules.elevation_applies(ElevationTarget::Ranged));
        assert!(!rules.elevation_applies(ElevationTarget::Melee));
    }

    #[test]
    fn the_m3_blocks_are_content_with_shipped_defaults() {
        // 02:02.7: the campaign layer's numbers are content, so a campaign tunes a
        // key without restating the block.
        let default = Rules::default();
        assert_eq!(default.panels_per_day, 1440);
        assert_eq!(default.advancement.power_cost, 1000);
        assert_eq!(default.carry_slots, 12);

        let tuned = Rules::parse(&Json::from_text(
            r#"{"time":{"panels_per_day":288},
                "advancement":{"power_cost":500,"skill_cost":100,"max_rank_value":40},
                "impact":{"fortune":[1,2,3,4,5],"repute":[5,4,3,2,1]},
                "economy":{"carry_slots":4,"fallback_percent":75}}"#,
        ))
        .expect("parse");
        assert_eq!(tuned.panels_per_day, 288);
        assert_eq!(tuned.advancement.power_cost, 500);
        assert_eq!(tuned.advancement.max_rank_value, 40);
        assert_eq!(tuned.impact.fortune, [1, 2, 3, 4, 5]);
        assert_eq!(tuned.carry_slots, 4);
        assert_eq!(tuned.prices.fallback_percent, 75);
        assert_eq!(tuned.advancement.skill_cost, 100);

        assert!(Rules::parse(&Json::from_text(r#"{"time":{"panels_per_day":0}}"#)).is_err());
        assert!(Rules::parse(&Json::from_text(r#"{"economy":{"carry_slots":0}}"#)).is_err());
    }

    #[test]
    fn a_campaign_can_add_melee_to_the_elevation_list() {
        // 02:02.12 says the key already supports it.
        let rules = Rules::parse(&Json::from_text(
            r#"{"elevation":{"applies_to":["melee","ranged"]}}"#,
        ))
        .expect("parse");
        assert!(rules.elevation_applies(ElevationTarget::Melee));
        assert!(!rules.elevation_applies(ElevationTarget::Perception));
    }
}
