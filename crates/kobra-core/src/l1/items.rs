//! L1 — items, their reach, and their modifiers (02:02.4 range vocabulary,
//! 02:02.8 equipment catalogue).
//!
//! A weapon's reach is **data, never an id**: the engine implements the four
//! *kinds* of reach and the content supplies the kind and its numbers, so adding
//! or retuning a weapon is a record rather than a release (`4c:983-992`). No
//! weapon id appears in Rust. The same rule covers armour, thrown material
//! values and item modifiers, which is what makes M1's data mod able to change
//! the encounter without a line of engine code.

use crate::json::Json;
use crate::l0::{Amount, Rounding};

/// The four kinds of reach (`02:02.4`, `4c:983-992`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Reach {
    /// A constant number of tiles (`4c:983`: bow, pistol, rifle, shotgun).
    Fixed {
        /// The reach, in 1 m tiles.
        tiles: i32,
    },
    /// One row of the Master Table per step from a starting Rank Value
    /// (`4c:990`, a thrown object).
    TableRow {
        /// The Rank Value below which the object cannot leave its tile.
        start_rv: i32,
        /// Tiles added per row.
        tiles_per_row: i32,
    },
    /// The user's Rank Value divided by a divisor, rounded as declared
    /// (`4c:991`, powers).
    RankFraction {
        /// The divisor.
        divisor: i32,
        /// Which way the division rounds.
        round: Rounding,
        /// Tiles per whole unit.
        tiles_per_unit: i32,
    },
    /// A same-tile weapon (an unarmed strike, or a thrown object at Rank Value
    /// `0..=5`, which `4c:990` says cannot leave the sector).
    Adjacent,
}

impl Reach {
    /// The reach in tiles for an actor whose Rank Value is `rv`.
    ///
    /// `ladder` is needed only by [`Reach::TableRow`], which counts rows.
    pub fn tiles(&self, ladder: &crate::l0::Ladder, rv: i32) -> i32 {
        match *self {
            Reach::Fixed { tiles } => tiles,
            Reach::Adjacent => 1,
            Reach::TableRow {
                start_rv,
                tiles_per_row,
            } => {
                if rv < start_rv {
                    1
                } else {
                    let start = ladder.band_index(start_rv);
                    let here = ladder.band_index(rv);
                    1 + (here as i32 - start as i32) * tiles_per_row
                }
            }
            Reach::RankFraction {
                divisor,
                round,
                tiles_per_unit,
            } => {
                if divisor <= 0 {
                    return 1;
                }
                let quotient = rv / divisor;
                let remainder = rv % divisor;
                let units = match round {
                    Rounding::Up if remainder != 0 => quotient + 1,
                    _ => quotient,
                };
                (units * tiles_per_unit).max(1)
            }
        }
    }

    /// Read a reach record (`02:02.4`).
    pub fn parse(value: &Json) -> Result<Reach, &'static str> {
        match value.get("kind").and_then(Json::as_str) {
            Some("fixed") => Ok(Reach::Fixed {
                tiles: value
                    .get("tiles")
                    .and_then(Json::as_i64)
                    .ok_or("a fixed reach needs tiles")? as i32,
            }),
            Some("adjacent") => Ok(Reach::Adjacent),
            Some("table_row") => Ok(Reach::TableRow {
                start_rv: value
                    .get("start_rv")
                    .and_then(Json::as_i64)
                    .ok_or("a table_row reach needs start_rv")? as i32,
                tiles_per_row: value
                    .get("tiles_per_row")
                    .and_then(Json::as_i64)
                    .unwrap_or(1) as i32,
            }),
            Some("rank_fraction") => Ok(Reach::RankFraction {
                divisor: value
                    .get("divisor")
                    .and_then(Json::as_i64)
                    .ok_or("a rank_fraction reach needs divisor")? as i32,
                round: match value.get("round").and_then(Json::as_str) {
                    Some("down") | None => Rounding::Down,
                    Some("up") => Rounding::Up,
                    Some(_) => return Err("round must be \"up\" or \"down\""),
                },
                tiles_per_unit: value
                    .get("tiles_per_unit")
                    .and_then(Json::as_i64)
                    .unwrap_or(1) as i32,
            }),
            _ => Err("unknown reach kind"),
        }
    }
}

/// A modifier an item carries (`02:02.4`, `02:02.8`).
///
/// M1 ships the two the equipment catalogue needs and the architecture names:
/// a within-distance value/colour effect (Range Drawback, `4c:1484`) and armour
/// piercing (half the target's armour, `4c:1480`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct ItemModifier {
    /// The stable id, so the load report can name it.
    pub id: String,
    /// Only applies within this many tiles, when present.
    pub within_tiles: Option<i32>,
    /// Divides the effective Rank Value by this, when present.
    pub rv_divisor: Option<(i32, Rounding)>,
    /// Colours demoted one step by this modifier.
    pub demote: Vec<crate::l0::Colour>,
    /// This attack halves the target's armour (`4c:1480`).
    pub armour_piercing: bool,
}

impl ItemModifier {
    /// Read a modifier record.
    pub fn parse(value: &Json) -> Result<ItemModifier, &'static str> {
        let id = value
            .get("id")
            .and_then(Json::as_str)
            .ok_or("a modifier needs an id")?
            .to_string();
        let rv_divisor = value
            .get("rv_divisor")
            .and_then(Json::as_i64)
            .map(|divisor| {
                (
                    divisor as i32,
                    match value.get("rv_round").and_then(Json::as_str) {
                        Some("up") => Rounding::Up,
                        _ => Rounding::Down,
                    },
                )
            });
        let demote = match value.get("demote") {
            Some(Json::Arr(colours)) => {
                let mut out = Vec::new();
                for colour in colours {
                    out.push(colour_from_id(
                        colour
                            .as_str()
                            .ok_or("demote entries must be colour names")?,
                    )?);
                }
                out
            }
            None => Vec::new(),
            _ => return Err("demote must be an array"),
        };
        Ok(ItemModifier {
            id,
            within_tiles: value
                .get("within_tiles")
                .and_then(Json::as_i64)
                .map(|n| n as i32),
            rv_divisor,
            demote,
            armour_piercing: value
                .get("armour_piercing")
                .and_then(Json::as_bool)
                .unwrap_or(false),
        })
    }

    /// Whether this modifier applies at `distance` tiles.
    pub fn applies_at(&self, distance: i32) -> bool {
        match self.within_tiles {
            Some(limit) => distance <= limit,
            None => true,
        }
    }
}

/// Parse a colour name from content.
pub fn colour_from_id(id: &str) -> Result<crate::l0::Colour, &'static str> {
    match id {
        "Black" | "black" | "Blck" => Ok(crate::l0::Colour::Blck),
        "Red" | "red" => Ok(crate::l0::Colour::Red),
        "Blue" | "blue" => Ok(crate::l0::Colour::Blue),
        "Yellow" | "yellow" | "Yel" => Ok(crate::l0::Colour::Yel),
        _ => Err("unknown colour name"),
    }
}

/// How many hands a weapon needs, which is also its damage bonus (`4c:1074`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Hands {
    /// Not a weapon.
    None,
    /// One-handed: `Brawn + 5`.
    One,
    /// Two-handed: `Brawn + 10`.
    Two,
    /// A ranged weapon, whose damage comes from its own record.
    Ranged,
}

/// The kind of melee attack a weapon makes.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum MeleeKind {
    /// Blunt: Red hit, Blue pound, Yellow concuss (`4c:952-959`).
    Bashing,
    /// Sharp: Red hit, Blue concuss, Yellow dying (`4c:961-968`).
    Slashing,
}

/// An item record (`02:02.8`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Item {
    /// The content id, e.g. `wsp.item.pistol`.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// Which hand(s) it occupies.
    pub hands: Hands,
    /// The melee attack kind, for a melee weapon.
    pub melee: Option<MeleeKind>,
    /// How far it reaches.
    pub reach: Reach,
    /// Damage for a ranged weapon or a thrown object, when it is not `Brawn`
    /// based.
    pub damage: Option<Amount>,
    /// Material value, for a thrown object (`4c:1065-1072`) and for knock-through.
    pub material_value: Option<i32>,
    /// The armour Rank Value this item grants (`4c:1084`).
    pub armour: Option<i32>,
    /// Effect tags for a later power/absorption system (`D25`).
    pub tags: Vec<String>,
    /// Modifiers the item carries.
    pub modifiers: Vec<ItemModifier>,
    /// The equipment slot it occupies (`AD-29` rule 3).
    ///
    /// The slot exists in the data from day one even where nothing reads it, so a
    /// later rig can attach a mesh to it without retrofitting every item and every
    /// save.
    pub slot: ItemSlot,
    /// The base price in the campaign's own currency (`4c:1255-1272`).
    ///
    /// Scaled by the buyer's Lifestyle band when it is bought, so this is the
    /// number a catalogue author writes rather than the number a player pays.
    pub price: i32,
    /// What kind of thing it is, for a merchant's stock list (`02:02.8`).
    pub kind: ItemKind,
}

/// Which equipment slot an item occupies (`AD-29` rule 3).
///
/// The names are the anchors a rig exposes — `mainhand`, `offhand`, `head`,
/// `body`, `back` — so an item's slot and a character's attachment point use one
/// vocabulary in the data, whether or not a v1 renderer reads it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ItemSlot {
    /// Carried, not worn.
    None,
    /// The primary hand.
    Mainhand,
    /// The secondary hand.
    Offhand,
    /// Worn on the body.
    Body,
    /// Worn on the head.
    Head,
    /// Carried on the back.
    Back,
}

impl ItemSlot {
    /// Every slot, so a validator can enumerate the domain.
    pub const ALL: [ItemSlot; 6] = [
        ItemSlot::None,
        ItemSlot::Mainhand,
        ItemSlot::Offhand,
        ItemSlot::Body,
        ItemSlot::Head,
        ItemSlot::Back,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ItemSlot::None => "none",
            ItemSlot::Mainhand => "mainhand",
            ItemSlot::Offhand => "offhand",
            ItemSlot::Body => "body",
            ItemSlot::Head => "head",
            ItemSlot::Back => "back",
        }
    }

    /// The slot a content id names.
    pub fn from_id(id: &str) -> Option<ItemSlot> {
        ItemSlot::ALL.into_iter().find(|slot| slot.id() == id)
    }

    /// Whether wearing this slot is what the armour rules read (`4c:1084`).
    pub const fn is_worn(self) -> bool {
        matches!(self, ItemSlot::Body | ItemSlot::Head)
    }
}

/// What an item is, so a merchant and an action can name a class of thing
/// (`02:02.8`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ItemKind {
    /// A weapon, held.
    Weapon,
    /// Worn protection.
    Armour,
    /// A consumable.
    Consumable,
    /// A trade good with no mechanical effect.
    Good,
    /// Something else the campaign defines.
    Other,
}

impl ItemKind {
    /// Every kind, so a validator can enumerate the domain.
    pub const ALL: [ItemKind; 5] = [
        ItemKind::Weapon,
        ItemKind::Armour,
        ItemKind::Consumable,
        ItemKind::Good,
        ItemKind::Other,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ItemKind::Weapon => "weapon",
            ItemKind::Armour => "armour",
            ItemKind::Consumable => "consumable",
            ItemKind::Good => "good",
            ItemKind::Other => "other",
        }
    }

    /// The kind a content id names.
    pub fn from_id(id: &str) -> Option<ItemKind> {
        ItemKind::ALL.into_iter().find(|kind| kind.id() == id)
    }
}

/// The default reach of an unarmed strike: the attacker's own tile and its
/// neighbours, i.e. one tile of separation.
pub const UNARMED_REACH: Reach = Reach::Adjacent;

impl Item {
    /// Read an item record from a content pack.
    pub fn parse(id: &str, data: &Json) -> Result<Item, &'static str> {
        let hands = match data.get("hands").and_then(Json::as_str) {
            Some("one") => Hands::One,
            Some("two") => Hands::Two,
            Some("ranged") => Hands::Ranged,
            Some("none") | None => Hands::None,
            Some(_) => return Err("hands must be none, one, two or ranged"),
        };
        let melee = match data.get("melee").and_then(Json::as_str) {
            Some("bashing") => Some(MeleeKind::Bashing),
            Some("slashing") => Some(MeleeKind::Slashing),
            None => None,
            Some(_) => return Err("melee must be bashing or slashing"),
        };
        let reach = match data.get("reach") {
            Some(value) => Reach::parse(value)?,
            None => Reach::Adjacent,
        };
        let damage = match data.get("damage") {
            Some(value) => Some(Amount::parse(value)?),
            None => None,
        };
        let mut modifiers = Vec::new();
        if let Some(Json::Arr(items)) = data.get("modifiers") {
            for value in items {
                modifiers.push(ItemModifier::parse(value)?);
            }
        }
        let mut tags = Vec::new();
        if let Some(Json::Arr(items)) = data.get("tags") {
            for value in items {
                tags.push(value.as_str().ok_or("a tag must be a string")?.to_string());
            }
        }
        Ok(Item {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            hands,
            melee,
            reach,
            damage,
            material_value: data
                .get("material_value")
                .and_then(Json::as_i64)
                .map(|n| n as i32),
            armour: data.get("armour").and_then(Json::as_i64).map(|n| n as i32),
            tags,
            modifiers,
            slot: match data.get("slot").and_then(Json::as_str) {
                None => ItemSlot::None,
                Some(name) => ItemSlot::from_id(name)
                    .ok_or("slot must be none, mainhand, offhand, body, head or back")?,
            },
            price: data.get("price").and_then(Json::as_i64).unwrap_or(0) as i32,
            kind: match data.get("kind").and_then(Json::as_str) {
                // An item with no declared kind is read from its own numbers,
                // so a catalogue written before `kind` existed still says
                // something true about itself.
                None => {
                    if data.get("melee").is_some() || data.get("damage").is_some() {
                        ItemKind::Weapon
                    } else if data.get("armour").is_some() {
                        ItemKind::Armour
                    } else {
                        ItemKind::Good
                    }
                }
                Some(name) => ItemKind::from_id(name)
                    .ok_or("kind must be weapon, armour, consumable, good or other")?,
            },
        })
    }

    /// The armour Rank Value this item contributes.
    pub fn armour_rv(&self) -> i32 {
        self.armour.unwrap_or(0)
    }

    /// Whether the item is a weapon at all.
    pub fn is_weapon(&self) -> bool {
        self.melee.is_some() || matches!(self.hands, Hands::Ranged) || self.damage.is_some()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tables;

    #[test]
    fn a_fixed_reach_is_a_constant_in_tiles() {
        // 4c:983: a rifle's 8 legacy sectors become 24 tiles (AD-33).
        let reach = Reach::Fixed { tiles: 24 };
        assert_eq!(reach.tiles(tables::CAMPAIGN_LADDER, 6), 24);
        assert_eq!(reach.tiles(tables::CAMPAIGN_LADDER, 999), 24);
    }

    #[test]
    fn a_thrown_object_gains_a_row_of_reach_per_rank_row() {
        // 4c:990: "1 sector per row on the Master Table starting with Rank
        // Value 6-9; lower Rank Values can only throw an object in the same
        // sector."
        let reach = Reach::TableRow {
            start_rv: 6,
            tiles_per_row: 3,
        };
        let ladder = tables::CAMPAIGN_LADDER;
        assert_eq!(reach.tiles(ladder, 5), 1, "below the start: same tile only");
        assert_eq!(reach.tiles(ladder, 6), 1);
        assert_eq!(reach.tiles(ladder, 10), 4);
        assert_eq!(reach.tiles(ladder, 20), 7);
    }

    #[test]
    fn a_power_reach_is_a_rank_fraction_rounded_up() {
        // 4c:991: 1/10 the power's Rank Value, round up.
        let reach = Reach::RankFraction {
            divisor: 10,
            round: Rounding::Up,
            tiles_per_unit: 3,
        };
        let ladder = tables::CAMPAIGN_LADDER;
        assert_eq!(reach.tiles(ladder, 10), 3);
        assert_eq!(reach.tiles(ladder, 11), 6, "rounds up");
        assert_eq!(
            reach.tiles(ladder, 1),
            3,
            "a positive power reaches one unit"
        );
    }

    #[test]
    fn parses_the_four_reach_kinds() {
        let fixed = Reach::parse(&Json::from_text(r#"{"kind":"fixed","tiles":24}"#)).unwrap();
        assert_eq!(fixed, Reach::Fixed { tiles: 24 });
        let row = Reach::parse(&Json::from_text(
            r#"{"kind":"table_row","start_rv":6,"tiles_per_row":3}"#,
        ))
        .unwrap();
        assert_eq!(
            row,
            Reach::TableRow {
                start_rv: 6,
                tiles_per_row: 3
            }
        );
        let fraction = Reach::parse(&Json::from_text(
            r#"{"kind":"rank_fraction","divisor":10,"round":"up","tiles_per_unit":3}"#,
        ))
        .unwrap();
        assert_eq!(
            fraction,
            Reach::RankFraction {
                divisor: 10,
                round: Rounding::Up,
                tiles_per_unit: 3
            }
        );
        assert!(Reach::parse(&Json::from_text(r#"{"kind":"teleport"}"#)).is_err());
    }

    #[test]
    fn parses_a_range_drawback_modifier() {
        let modifier = ItemModifier::parse(&Json::from_text(
            r#"{"id":"range_drawback","within_tiles":36,"rv_divisor":2,"rv_round":"down","demote":["Blue","Yellow"]}"#,
        ))
        .expect("parse");
        assert_eq!(modifier.within_tiles, Some(36));
        assert_eq!(modifier.rv_divisor, Some((2, Rounding::Down)));
        assert_eq!(
            modifier.demote,
            vec![crate::l0::Colour::Blue, crate::l0::Colour::Yel]
        );
        assert!(modifier.applies_at(36));
        assert!(!modifier.applies_at(37));
    }

    #[test]
    fn parses_an_item_record() {
        let item = Item::parse(
            "wsp.item.rifle",
            &Json::from_text(
                r#"{"name_key":"item.rifle","hands":"ranged","damage":15,"reach":{"kind":"fixed","tiles":24}}"#,
            ),
        )
        .expect("parse");
        assert_eq!(item.hands, Hands::Ranged);
        assert_eq!(item.damage, Some(Amount::constant(15)));
        assert_eq!(item.reach, Reach::Fixed { tiles: 24 });
        assert!(item.is_weapon());
    }
}
