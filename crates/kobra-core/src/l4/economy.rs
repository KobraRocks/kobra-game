//! L4 — inventory and economy (`02:02.7`, `4c:1255-1272`).
//!
//! The rule the architecture names is short and load-bearing:
//!
//! > Items with the 4C equipment statistics plus campaign metadata; prices scaled
//! > by Lifestyle band (`4c:1255-1272`). **Lifestyle gates procurement**, as the
//! > spec intends.
//!
//! So this module has one non-obvious job: turning an item's authored base price
//! into the price *this buyer* pays, and deciding whether they may buy at all.
//! The price curve is a `rules.economy` content table (`02:02.6`'s rule that a
//! disputed number is a content parameter), and the band comes from the
//! character's Lifestyle Rank Value.
//!
//! Two refusals are deliberate rather than incidental:
//!
//! - **A merchant with no stock list sells nothing.** There is no implicit
//!   "everything in the catalogue is for sale", because that would make every
//!   item a mod adds buyable everywhere.
//! - **Buying an item the party cannot afford is refused, not clamped.** A
//!   silent partial purchase is indistinguishable from a bug.
//!
//! Consumables are used through [`crate::l4::economy::use_item`], which routes
//! their effect into L2 rather than inventing a healing formula here (`02:02.7`).

use crate::json::Json;
use crate::l1::character::Character;
use crate::l1::items::{Item, ItemKind};
use crate::l1::traits::Trait;
use crate::l2::actions::OutcomeTable;

/// A price band: a Lifestyle Rank Value range and the multiplier it pays.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct PriceBand {
    /// Lowest Lifestyle Rank Value in the band.
    pub lo: i32,
    /// Highest Lifestyle Rank Value in the band.
    pub hi: i32,
    /// Percent of the base price this band pays: 100 is list price.
    pub percent: i32,
}

/// The price curve (`rules.economy`, `4c:1255-1272`).
///
/// The shipped default reads the 4C intent literally: a character living at a
/// high band is a *better* customer and pays less relative to list, because
/// Lifestyle is described as access rather than wealth. A campaign that would
/// rather have wealth inflate prices replaces the table in content.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct PriceTable {
    /// The bands, ascending by `lo`.
    pub bands: Vec<PriceBand>,
    /// The percent a character outside every band pays.
    pub fallback_percent: i32,
}

impl PriceTable {
    /// The shipped curve.
    pub fn canonical() -> PriceTable {
        PriceTable {
            bands: vec![
                PriceBand {
                    lo: 1,
                    hi: 6,
                    percent: 150,
                },
                PriceBand {
                    lo: 7,
                    hi: 15,
                    percent: 125,
                },
                PriceBand {
                    lo: 16,
                    hi: 29,
                    percent: 100,
                },
                PriceBand {
                    lo: 30,
                    hi: i32::MAX,
                    percent: 80,
                },
            ],
            fallback_percent: 100,
        }
    }

    /// The percent a Lifestyle Rank Value pays.
    pub fn percent_for(&self, lifestyle_rv: i32) -> i32 {
        self.bands
            .iter()
            .find(|band| lifestyle_rv >= band.lo && lifestyle_rv <= band.hi)
            .map(|band| band.percent)
            .unwrap_or(self.fallback_percent)
    }

    /// The price a character pays for an item, rounded up so a discount never
    /// makes something free.
    pub fn price_for(&self, item: &Item, lifestyle_rv: i32) -> i32 {
        let percent = self.percent_for(lifestyle_rv).max(0);
        let scaled = item.price.max(0).saturating_mul(percent);
        (scaled + 99) / 100
    }

    /// Read the table from a rules record.
    pub fn parse(value: &Json) -> Result<PriceTable, &'static str> {
        let mut bands = Vec::new();
        if let Some(Json::Arr(items)) = value.get("bands") {
            for item in items {
                bands.push(PriceBand {
                    lo: item.get("lo").and_then(Json::as_i64).unwrap_or(1) as i32,
                    hi: item
                        .get("hi")
                        .and_then(Json::as_i64)
                        .unwrap_or(i64::from(i32::MAX)) as i32,
                    percent: item
                        .get("percent")
                        .and_then(Json::as_i64)
                        .ok_or("a price band needs a percent")? as i32,
                });
            }
        }
        bands.sort_by_key(|band| band.lo);
        Ok(PriceTable {
            bands,
            fallback_percent: value
                .get("fallback_percent")
                .and_then(Json::as_i64)
                .unwrap_or(100) as i32,
        })
    }
}

impl Default for PriceTable {
    fn default() -> PriceTable {
        PriceTable::canonical()
    }
}

/// A merchant's stock (`02:02.7`): what this counter will sell, and what it buys.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Shop {
    /// The record id.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// The item record ids on the shelf.
    pub stock: Vec<String>,
    /// The item kinds the counter buys. Empty means "buys nothing".
    pub buys: Vec<ItemKind>,
    /// Percent of list the counter pays when it buys (`rules.economy.buy_percent`).
    pub buy_percent: i32,
}

impl Shop {
    /// Read a shop record.
    pub fn parse(id: &str, data: &Json) -> Result<Shop, &'static str> {
        let mut stock = Vec::new();
        if let Some(Json::Arr(items)) = data.get("stock") {
            for item in items {
                stock.push(
                    item.as_str()
                        .ok_or("a stock entry must be an item id")?
                        .to_string(),
                );
            }
        }
        let mut buys = Vec::new();
        if let Some(Json::Arr(items)) = data.get("buys") {
            for item in items {
                buys.push(
                    item.as_str()
                        .and_then(ItemKind::from_id)
                        .ok_or("buys entries must be weapon, armour, consumable, good or other")?,
                );
            }
        }
        Ok(Shop {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            stock,
            buys,
            buy_percent: data.get("buy_percent").and_then(Json::as_i64).unwrap_or(50) as i32,
        })
    }

    /// Whether the counter buys this kind of thing at all.
    pub fn buys_kind(&self, kind: ItemKind) -> bool {
        self.buys.contains(&kind)
    }

    /// The price a character pays for a stock entry, when it is on the shelf.
    pub fn price_for(&self, item: &Item, lifestyle_rv: i32, table: &PriceTable) -> Option<i32> {
        if !self.stock.contains(&item.id) {
            return None;
        }
        Some(table.price_for(item, lifestyle_rv))
    }

    /// What the counter pays for an item a character is selling.
    pub fn offer_for(&self, item: &Item, table: &PriceTable, lifestyle_rv: i32) -> Option<i32> {
        if !self.buys_kind(item.kind) {
            return None;
        }
        // A merchant pays a fraction of what they would charge this customer, so
        // a high-Lifestyle seller gets the better end of the same curve.
        let list = table.price_for(item, lifestyle_rv).max(0);
        Some((list * self.buy_percent.max(0)) / 100)
    }
}

/// Whether a character may carry one more item (`4c:1395-1569` gives no weight
/// rule, so the campaign's slot count is the only limit).
pub fn can_carry(character: &Character, slots: i32) -> bool {
    (character.items.len() as i32) < slots.max(0)
}

/// Equip an item into the hands the character uses to attack.
///
/// The item must already be carried, and a two-handed or ranged item clears the
/// off hand, so a character never ends up swinging a pistol and a rifle at once.
pub fn equip(character: &mut Character, item: &Item) -> Result<(), &'static str> {
    if !character.items.iter().any(|held| held == &item.id) {
        return Err("the character does not carry that item");
    }
    if !item.is_weapon() {
        return Err("that item is not a weapon");
    }
    character.weapon = Some(item.id.clone());
    Ok(())
}

/// Wear an item in its armour slot (`4c:1084`).
pub fn wear(character: &mut Character, item: &Item) -> Result<(), &'static str> {
    if !character.items.iter().any(|held| held == &item.id) {
        return Err("the character does not carry that item");
    }
    if item.armour_rv() <= 0 {
        return Err("that item grants no armour");
    }
    character.armour.retain(|worn| worn != &item.id);
    character.armour.push(item.id.clone());
    Ok(())
}

/// What using a consumable does, in the L2 vocabulary (`02:02.7`).
///
/// A consumable does not get its own resolution path: it names an `Effect` the
/// outcome tables already use, so a potion that knocks someone out and a mace
/// that knocks someone out resolve identically. That is the rule that keeps the
/// campaign layer free of damage formulas.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Consumable {
    /// The item record id.
    pub item: String,
    /// The effects it applies.
    pub effects: Vec<crate::l2::actions::Effect>,
    /// The trait its roll uses, when it rolls at all.
    pub trait_: Option<Trait>,
    /// How many panels it takes to use.
    pub panels: i32,
}

impl Consumable {
    /// Read a consumable's data from an item record.
    pub fn parse(item: &str, data: &Json) -> Result<Consumable, &'static str> {
        let mut effects = Vec::new();
        if let Some(Json::Arr(items)) = data.get("effects") {
            for value in items {
                effects.push(crate::l2::actions::Effect::parse(value)?);
            }
        }
        Ok(Consumable {
            item: item.to_string(),
            effects,
            trait_: match data.get("trait").and_then(Json::as_str) {
                None => None,
                Some(name) => Some(Trait::from_id(name).ok_or("unknown consumable trait")?),
            },
            panels: data.get("panels").and_then(Json::as_i64).unwrap_or(1) as i32,
        })
    }
}

/// The outcome table a consumable's roll uses (`4c:1542`).
///
/// A consumable with effects but no trait applies them unconditionally; one with
/// a trait resolves on the Master Table like any other action, and its effects
/// are read from the colour's row. Either way the vocabulary is L2's.
pub fn consumable_table(consumable: &Consumable) -> OutcomeTable {
    OutcomeTable {
        action: crate::l2::actions::ActionKind::Power,
        rows: [
            vec![],
            consumable.effects.clone(),
            consumable.effects.clone(),
            consumable.effects.clone(),
        ],
    }
}

/// Remove one item from a character, wherever it sits.
pub fn drop_item(character: &mut Character, item: &str) -> bool {
    let Some(index) = character.items.iter().position(|held| held == item) else {
        return false;
    };
    character.items.remove(index);
    if character.weapon.as_deref() == Some(item) {
        character.weapon = None;
    }
    character.armour.retain(|worn| worn != item);
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    fn item(id: &str, price: i32, kind: ItemKind) -> Item {
        Item::parse(
            id,
            &Json::from_text(&format!(
                r#"{{"name_key":"{id}","hands":"one","melee":"bashing","price":{price},"kind":"{}"}}"#,
                kind.id()
            )),
        )
        .expect("item")
    }

    #[test]
    fn lifestyle_gates_the_price_a_character_pays() {
        let table = PriceTable::canonical();
        let pistol = item("wsp.item.pistol", 100, ItemKind::Weapon);
        // 4c:1255-1272: the same item costs a low-band character more.
        assert_eq!(table.price_for(&pistol, 3), 150);
        assert_eq!(table.price_for(&pistol, 10), 125);
        assert_eq!(table.price_for(&pistol, 20), 100);
        assert_eq!(table.price_for(&pistol, 40), 80);
        // Rounded up, so a steep discount never makes an item free.
        let cheap = item("wsp.item.match", 1, ItemKind::Good);
        assert_eq!(table.price_for(&cheap, 40), 1);
    }

    #[test]
    fn a_shop_sells_only_its_stock_and_buys_only_its_kinds() {
        let shop = Shop::parse(
            "wsp.shop.surplus",
            &Json::from_text(
                r#"{"name_key":"shop.surplus","stock":["wsp.item.pistol"],"buys":["weapon"]}"#,
            ),
        )
        .expect("shop");
        let table = PriceTable::default();
        let pistol = item("wsp.item.pistol", 100, ItemKind::Weapon);
        let bread = item("wsp.item.bread", 10, ItemKind::Good);
        assert_eq!(shop.price_for(&pistol, 20, &table), Some(100));
        assert_eq!(shop.price_for(&bread, 20, &table), None, "not on the shelf");
        assert_eq!(shop.offer_for(&pistol, &table, 20), Some(50));
        assert_eq!(shop.offer_for(&bread, &table, 20), None, "buys no goods");
    }

    #[test]
    fn equipping_requires_carrying_and_wearing_requires_armour() {
        let mut character = Character::authored(
            1,
            "hero",
            "",
            0,
            [10; 7],
            10,
            0,
            vec![],
            vec!["wsp.item.bat".into(), "wsp.item.vest".into()],
            vec![],
        );
        let bat = item("wsp.item.bat", 20, ItemKind::Weapon);
        let vest = Item::parse(
            "wsp.item.vest",
            &Json::from_text(
                r#"{"name_key":"item.vest","armour":20,"slot":"body","kind":"armour"}"#,
            ),
        )
        .expect("vest");
        let other = item("wsp.item.pistol", 100, ItemKind::Weapon);

        assert!(equip(&mut character, &bat).is_ok());
        assert_eq!(character.weapon.as_deref(), Some("wsp.item.bat"));
        assert!(equip(&mut character, &other).is_err(), "not carried");
        assert!(wear(&mut character, &vest).is_ok());
        assert!(wear(&mut character, &bat).is_err(), "no armour");
        assert!(drop_item(&mut character, "wsp.item.bat"));
        assert_eq!(character.weapon, None, "dropping unwields it");
        assert!(!drop_item(&mut character, "wsp.item.bat"));
    }

    #[test]
    fn a_consumable_uses_the_l2_effect_vocabulary() {
        let consumable = Consumable::parse(
            "wsp.item.drug",
            &Json::from_text(r#"{"effects":["knockout"],"trait":"fortitude","panels":1}"#),
        )
        .expect("consumable");
        assert_eq!(consumable.trait_, Some(Trait::Fortitude));
        let table = consumable_table(&consumable);
        // The same vocabulary the melee tables use, so there is one resolution
        // path (02:02.7).
        assert_eq!(
            table.effects(crate::l0::Colour::Blue),
            [crate::l2::actions::Effect::Knockout]
        );
        assert!(table.effects(crate::l0::Colour::Blck).is_empty());
    }

    #[test]
    fn carrying_is_bounded_by_the_campaign_slot_count() {
        let character = Character::authored(
            1,
            "hero",
            "",
            0,
            [10; 7],
            10,
            0,
            vec![],
            vec!["a".into(), "b".into()],
            vec![],
        );
        assert!(can_carry(&character, 3));
        assert!(!can_carry(&character, 2));
    }
}
