//! L2 — vehicles (`4c:1300-1365`).
//!
//! A vehicle is a **first-class entity**, not an item with stats: it has three
//! Vehicle Traits of its own, it acts on its operator's initiative, and it adds
//! one rule the character model lacks — **passengers take 10 damage per legacy
//! sector the vehicle moved before a collision** (`4c:1359`). That, plus the
//! `Vehicle` power buffing an *existing* vehicle (`4c:821-823`), is what the
//! architecture means by first-class (`02:02.5`).
//!
//! Two of the three traits are **scores**, not Rank Values (`4c:1310`,
//! `4c:1338`): Durability is both the damage pool and its own armour, and
//! Velocity is a plain count of sectors per turn. Handling is a Rank Value and
//! resolves on the Master Table like any other trait. Keeping the three apart is
//! the whole of this module's job, because a "fix" that made Durability a Rank
//! Value would silently change every vehicle in the game.
//!
//! `D15` decides two things the source leaves open: a vehicle acts on its
//! operator's initiative and has no attacks of its own, and a collision resolves
//! attacker-first in a single pass, so two identical vehicles trade symmetric
//! damage.
//!
//! `D17` decides the third: the `Vehicle` power is a **modifier layer**,
//! recomputed on load, never baked into the stored traits — so removing the power
//! removes the buff and a save needs no "was buffed" flag.

use crate::json::Json;

/// The sample vehicles' three traits (`4c:1367-1375`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Vehicle {
    /// The runtime entity id, from the same space as characters.
    pub id: u32,
    /// The record id this was built from.
    pub record: String,
    /// The display string id (`AD_22`).
    pub name_key: String,
    /// Current Durability: the damage pool, and the armour it projects.
    pub durability: i32,
    /// Starting Durability, for the cap on repair (`4c:1310`).
    pub max_durability: i32,
    /// Handling, a Rank Value (`4c:1314`).
    pub handling: i32,
    /// Velocity, sectors per turn (`4c:1338`).
    pub velocity: i32,
    /// The entity driving it, or `0`.
    pub operator: u32,
    /// The entities riding inside (`4c:1359`).
    pub passengers: Vec<u32>,
    /// Tiles moved this panel, for the passenger-damage rule.
    pub moved_tiles: i32,
    /// Whether Durability reached 0; a destroyed vehicle cannot be repaired
    /// (`4c:1365`).
    pub destroyed: bool,
}

impl Vehicle {
    /// Read a vehicle record (`03:03.3`).
    pub fn parse(id: &str, data: &Json) -> Result<Vehicle, &'static str> {
        let durability = data
            .get("durability")
            .and_then(Json::as_i64)
            .ok_or("a vehicle needs a Durability score")? as i32;
        if durability < 1 {
            return Err("a vehicle's Durability must be positive");
        }
        let handling = data
            .get("handling")
            .and_then(Json::as_i64)
            .ok_or("a vehicle needs a Handling Rank Value")? as i32;
        let velocity = data
            .get("velocity")
            .and_then(Json::as_i64)
            .ok_or("a vehicle needs a Velocity score")? as i32;
        Ok(Vehicle {
            id: 0,
            record: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            durability,
            max_durability: durability,
            handling,
            velocity,
            operator: 0,
            passengers: Vec::new(),
            moved_tiles: 0,
            destroyed: false,
        })
    }

    /// An empty vehicle record, for a test.
    pub fn sample(id: u32, durability: i32, handling: i32, velocity: i32) -> Vehicle {
        Vehicle {
            id,
            record: format!("wsp.vehicle.test{id}"),
            name_key: "vehicle.test".to_string(),
            durability,
            max_durability: durability,
            handling,
            velocity,
            operator: 0,
            passengers: Vec::new(),
            moved_tiles: 0,
            destroyed: false,
        }
    }

    /// Durability as armour after the `Vehicle` power's layer (`D17`).
    pub fn armour(&self, bonus: i32) -> i32 {
        self.durability + bonus
    }

    /// Handling after the power's layer.
    pub fn handling_rv(&self, bonus: i32) -> i32 {
        self.handling + bonus
    }

    /// Velocity in 1 m tiles after the power's layer (`AD-33`).
    pub fn velocity_tiles(&self, bonus: i32) -> i32 {
        (self.velocity + bonus).max(0) * crate::rules::LEGACY_SECTOR_TILES
    }

    /// Apply damage, flooring at 0 and marking the wreck (`4c:1365`).
    pub fn damage(&mut self, points: i32) -> i32 {
        let before = self.durability;
        self.durability = (self.durability - points.max(0)).max(0);
        if self.durability == 0 {
            self.destroyed = true;
        }
        before - self.durability
    }

    /// What a collision does to whoever was hit (`4c:1353`).
    pub fn collision_damage(&self, bonus: i32) -> i32 {
        self.durability + bonus
    }
}

/// What the striking vehicle suffers, by what it hit (`4c:1355-1357`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Struck {
    /// A character: the vehicle suffers the armour's Rank Value.
    Character {
        /// The armour Rank Value, worn or natural.
        armour: i32,
    },
    /// Another vehicle: the vehicle suffers the other one's Durability.
    Vehicle {
        /// The other vehicle's Durability.
        durability: i32,
    },
    /// An object: the vehicle suffers the object's Material Value.
    Object {
        /// The Material Value (`4c:1131-1146`).
        material: i32,
    },
}

impl Struck {
    /// The damage the striking vehicle suffers.
    pub fn damage_to_striker(self) -> i32 {
        match self {
            Struck::Character { armour } => armour,
            Struck::Vehicle { durability } => durability,
            Struck::Object { material } => material,
        }
    }
}

/// The ground's Material Value when a crash hits nothing else (`4c:1334`).
pub const GROUND_MATERIAL: i32 = 50;

/// Damage a passenger takes from a collision (`4c:1359`).
///
/// "10 points of damage for every sector the vehicle moved that turn prior to
/// the collision" — a sector is three tiles (`AD-33`, `D33`), so the count is in
/// legacy sectors and the conversion happens once, here.
pub fn passenger_damage(moved_tiles: i32) -> i32 {
    (moved_tiles / crate::rules::LEGACY_SECTOR_TILES).max(0) * 10
}

/// The difficulty of a manoeuvre, in the colour the driver must reach
/// (`4c:1316-1322`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Difficulty {
    /// A standard turn.
    Easy,
    /// A sharp turn.
    Average,
    /// Jumping a broken bridge.
    Difficult,
    /// Two wheels through a narrow alley.
    Ridiculous,
}

impl Difficulty {
    /// The colour the roll must reach (`4c:1323`).
    pub const fn required(self) -> crate::l0::Colour {
        match self {
            Difficulty::Easy => crate::l0::Colour::Blck,
            Difficulty::Average => crate::l0::Colour::Red,
            Difficulty::Difficult => crate::l0::Colour::Blue,
            Difficulty::Ridiculous => crate::l0::Colour::Yel,
        }
    }

    /// The id a record names.
    pub const fn id(self) -> &'static str {
        match self {
            Difficulty::Easy => "easy",
            Difficulty::Average => "average",
            Difficulty::Difficult => "difficult",
            Difficulty::Ridiculous => "ridiculous",
        }
    }

    /// The difficulty a name describes.
    pub fn from_id(id: &str) -> Option<Difficulty> {
        Some(match id {
            "easy" => Difficulty::Easy,
            "average" => Difficulty::Average,
            "difficult" => Difficulty::Difficult,
            "ridiculous" => Difficulty::Ridiculous,
            _ => return None,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rules::LEGACY_SECTOR_TILES;

    #[test]
    fn passengers_take_ten_per_legacy_sector_moved() {
        // 4c:1359, with AD-33's conversion applied once.
        assert_eq!(passenger_damage(0), 0);
        assert_eq!(passenger_damage(2), 0, "two tiles is under a sector");
        assert_eq!(passenger_damage(LEGACY_SECTOR_TILES), 10);
        assert_eq!(passenger_damage(LEGACY_SECTOR_TILES * 4), 40);
    }

    #[test]
    fn durability_is_both_the_pool_and_the_armour() {
        let vehicle = Vehicle::sample(1, 10, 6, 6);
        assert_eq!(vehicle.armour(0), 10);
        assert_eq!(vehicle.collision_damage(0), 10);
        // D17: the power's buff is a layer, not a stored value.
        assert_eq!(vehicle.armour(15), 25);
        assert_eq!(vehicle.durability, 10, "the stored score is untouched");
    }

    #[test]
    fn a_vehicle_is_destroyed_at_zero_and_cannot_go_below() {
        let mut vehicle = Vehicle::sample(1, 10, 6, 6);
        assert_eq!(vehicle.damage(4), 4);
        assert!(!vehicle.destroyed);
        assert_eq!(vehicle.damage(99), 6);
        assert!(vehicle.destroyed);
        assert_eq!(vehicle.durability, 0);
        assert_eq!(vehicle.damage(5), 0);
    }

    #[test]
    fn velocity_converts_from_legacy_sectors_to_tiles() {
        let vehicle = Vehicle::sample(1, 10, 6, 6);
        assert_eq!(vehicle.velocity_tiles(0), 18);
        assert_eq!(vehicle.handling_rv(15), 21);
    }

    #[test]
    fn a_collision_charges_the_striker_by_what_it_hit() {
        assert_eq!(Struck::Character { armour: 12 }.damage_to_striker(), 12);
        assert_eq!(Struck::Vehicle { durability: 20 }.damage_to_striker(), 20);
        assert_eq!(Struck::Object { material: 30 }.damage_to_striker(), 30);
        assert_eq!(GROUND_MATERIAL, 50);
    }

    #[test]
    fn the_manoeuvre_table_is_the_source_table() {
        assert_eq!(Difficulty::Easy.required(), crate::l0::Colour::Blck);
        assert_eq!(Difficulty::Average.required(), crate::l0::Colour::Red);
        assert_eq!(Difficulty::Difficult.required(), crate::l0::Colour::Blue);
        assert_eq!(Difficulty::Ridiculous.required(), crate::l0::Colour::Yel);
        for id in ["easy", "average", "difficult", "ridiculous"] {
            assert_eq!(Difficulty::from_id(id).map(Difficulty::id), Some(id));
        }
    }
}
