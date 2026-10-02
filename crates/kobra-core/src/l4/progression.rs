//! L4 — progression (`02:02.7`, `4c:1229-1251`, `4c:1381-1391`).
//!
//! The architecture states the model in one line:
//!
//! > **Progression** — Fortune as the currency of advancement: `+1 RV costs
//! > current value`, new power `1000`, new skill `250` (`4c:1381-1391`). Costs
//! > are content, so a campaign can tune pacing.
//!
//! and, separately, that **Fortune gain/loss is scaled by impact** — Personal
//! ±5 … Global ±100 (`4c:1229-1251`) — where the amounts are *authored on the
//! events that cause them*.
//!
//! Two readings matter and both are in the register:
//!
//! - **`D18`** — advancement is open-ended. The ladder clamps *effective* rows at
//!   the top, so an extreme Rank Value is harmless, and a campaign that wants a
//!   ceiling sets `gameplay.max_rank_value`.
//! - **`D21`** — a duplicate power roll may be taken as `+20 RV` with no stated
//!   cap; the creation UI shows the resulting value so an absurd stack is
//!   visible, and `gameplay.max_power_rv` is the ceiling when a campaign wants
//!   one.
//!
//! Nothing here mutates a roll. Advancement buys a *stored* Rank Value, and the
//! resolution path reads it through the ladder like every other number.

use crate::json::Json;
use crate::l1::character::Character;
use crate::l1::traits::Trait;

/// The cost curve for spending Fortune on advancement (`rules.advancement`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Advancement {
    /// What one row step on a trait costs, per unit: `cost = rv * per_rv + flat`.
    pub trait_step_per_rv: i32,
    /// A flat addition to each trait row step's cost.
    pub trait_step_flat: i32,
    /// What a new power costs.
    pub power_cost: i32,
    /// What a new skill costs.
    pub skill_cost: i32,
    /// What a Rank Value added to an existing power costs, per unit.
    pub power_rv_per_rv: i32,
    /// The most a stored Rank Value may reach, or `0` for no ceiling (`D18`).
    pub max_rank_value: i32,
    /// The most a power's Rank Value may reach, or `0` for no ceiling (`D21`).
    pub max_power_rv: i32,
}

impl Advancement {
    /// The shipped curve, which reproduces `4c:1381-1391` literally.
    pub fn canonical() -> Advancement {
        Advancement {
            trait_step_per_rv: 1,
            trait_step_flat: 0,
            power_cost: 1000,
            skill_cost: 250,
            power_rv_per_rv: 1,
            max_rank_value: 0,
            max_power_rv: 0,
        }
    }

    /// The Fortune cost of one row step on a trait: **the current value**
    /// (`4c:1383`).
    pub fn trait_step_cost(&self, current_rv: i32) -> i32 {
        current_rv.max(0) * self.trait_step_per_rv.max(0) + self.trait_step_flat.max(0)
    }

    /// The Fortune cost of lifting an existing power by one step.
    pub fn power_step_cost(&self, current_rv: i32) -> i32 {
        current_rv.max(0) * self.power_rv_per_rv.max(0)
    }

    /// Read the curve from a rules record.
    pub fn parse(value: &Json) -> Result<Advancement, &'static str> {
        let mut advancement = Advancement::canonical();
        if let Some(steps) = value.get("trait_step") {
            advancement.trait_step_per_rv =
                steps.get("per_rv").and_then(Json::as_i64).unwrap_or(1) as i32;
            advancement.trait_step_flat =
                steps.get("flat").and_then(Json::as_i64).unwrap_or(0) as i32;
        }
        if let Some(cost) = value.get("power_cost").and_then(Json::as_i64) {
            advancement.power_cost = cost as i32;
        }
        if let Some(cost) = value.get("skill_cost").and_then(Json::as_i64) {
            advancement.skill_cost = cost as i32;
        }
        if let Some(steps) = value.get("power_step") {
            advancement.power_rv_per_rv =
                steps.get("per_rv").and_then(Json::as_i64).unwrap_or(1) as i32;
        }
        if let Some(max) = value.get("max_rank_value").and_then(Json::as_i64) {
            advancement.max_rank_value = max as i32;
        }
        if let Some(max) = value.get("max_power_rv").and_then(Json::as_i64) {
            advancement.max_power_rv = max as i32;
        }
        Ok(advancement)
    }
}

impl Default for Advancement {
    fn default() -> Advancement {
        Advancement::canonical()
    }
}

/// Why an advancement was refused.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Refusal {
    /// The character already has the highest stored Rank Value the campaign
    /// allows (`D18`).
    AtCeiling,
    /// The power is already at its ceiling (`D21`).
    PowerAtCeiling,
    /// The character does not know that power.
    NoSuchPower,
    /// There is not enough Fortune.
    NotEnoughFortune,
    /// A stored Rank Value may never be 0 (`4c:212`).
    IllegalValue,
}

impl Refusal {
    /// The stable reason id, for an event and a UI string.
    pub const fn id(self) -> &'static str {
        match self {
            Refusal::AtCeiling => "at_ceiling",
            Refusal::PowerAtCeiling => "power_at_ceiling",
            Refusal::NoSuchPower => "no_such_power",
            Refusal::NotEnoughFortune => "not_enough_fortune",
            Refusal::IllegalValue => "illegal_value",
        }
    }
}

/// The cost of lifting one trait by `steps` row steps, and the Fortune spent.
///
/// The cost is the sum of the *current values* along the way, because
/// `4c:1383` prices a step at the value it leaves — so buying two steps is not
/// twice one step's price. That is the whole reason this is a loop rather than a
/// multiplication.
pub fn trait_step_cost(advancement: &Advancement, current_rv: i32, steps: i32) -> i32 {
    let mut total = 0;
    let mut value = current_rv;
    let mut remaining = steps.max(0);
    while remaining > 0 {
        total += advancement.trait_step_cost(value);
        // A row step is a band shift, so the cost curve follows the band and not
        // the arithmetic value; `+1` on a Rank Value is not always `+1` RV.
        value += 1;
        remaining -= 1;
    }
    total
}

/// Advance one trait, spending Fortune (`4c:1381-1391`).
///
/// The one mutation entry point for advancement, so a UI, a quest effect and a
/// script all price it identically.
pub fn advance_trait(
    advancement: &Advancement,
    character: &mut Character,
    trait_: Trait,
    steps: i32,
) -> Result<i32, Refusal> {
    if steps <= 0 {
        return Err(Refusal::IllegalValue);
    }
    let current = character.stored(trait_);
    if advancement.max_rank_value > 0 && current + steps > advancement.max_rank_value {
        return Err(Refusal::AtCeiling);
    }
    let cost = trait_step_cost(advancement, current, steps);
    if cost > character.fortune {
        return Err(Refusal::NotEnoughFortune);
    }
    character.fortune -= cost;
    character.traits[trait_.index()] = current + steps;
    // The ceilings are derived from the stored traits (`4c:190/196`), so they move
    // with the purchase. The **spent** Fortune does not come back: `recompute_
    // derived` is the fresh-character path and refills, so the unspent balance is
    // carried across it here, at the one call site that spends (`02:02.7`).
    let unspent = character.fortune;
    let was_full = character.damage >= character.max_damage;
    character.recompute_derived();
    character.fortune = unspent;
    // The Damage ceiling rises with the purchase. A character at full health
    // stays at full, and a wounded one keeps the wound but gains the new
    // headroom — which is the reading a player expects from "you got tougher".
    character.damage = if was_full {
        character.max_damage
    } else {
        character.damage + steps
    };
    Ok(cost)
}

/// Advance a power's Rank Value (`4c:1381-1391`, `D21`).
pub fn advance_power(
    advancement: &Advancement,
    character: &mut Character,
    power_id: &str,
    steps: i32,
) -> Result<i32, Refusal> {
    if steps <= 0 {
        return Err(Refusal::IllegalValue);
    }
    let Some(index) = character
        .powers
        .iter()
        .position(|power| power.id == power_id)
    else {
        return Err(Refusal::NoSuchPower);
    };
    let current = character.powers[index].rv;
    if advancement.max_power_rv > 0 && current + steps > advancement.max_power_rv {
        return Err(Refusal::PowerAtCeiling);
    }
    let mut cost = 0;
    let mut value = current;
    let mut remaining = steps;
    while remaining > 0 {
        cost += advancement.power_step_cost(value);
        value += 1;
        remaining -= 1;
    }
    if cost > character.fortune {
        return Err(Refusal::NotEnoughFortune);
    }
    character.fortune -= cost;
    character.powers[index].rv = current + steps;
    Ok(cost)
}

/// One rung of the impact scale (`4c:1229-1251`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Impact {
    /// Something that concerns one person.
    Personal,
    /// A neighbourhood or a small group.
    Local,
    /// A city or a faction.
    Faction,
    /// A region.
    Regional,
    /// The world.
    Global,
}

impl Impact {
    /// Every rung, so an authoring tool can enumerate the scale.
    pub const ALL: [Impact; 5] = [
        Impact::Personal,
        Impact::Local,
        Impact::Faction,
        Impact::Regional,
        Impact::Global,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            Impact::Personal => "personal",
            Impact::Local => "local",
            Impact::Faction => "faction",
            Impact::Regional => "regional",
            Impact::Global => "global",
        }
    }

    /// The rung a content id names.
    pub fn from_id(id: &str) -> Option<Impact> {
        Impact::ALL.into_iter().find(|impact| impact.id() == id)
    }
}

/// The Fortune and Repute an event of a given impact moves (`4c:1229-1251`).
///
/// `02:02.7` is explicit that this is *authored on the events that cause them*,
/// so the table lives in content (`rules.impact`) and this struct is only the
/// reader. `D20` extends the same shape to Repute, because this edition of 4C
/// omits the Repute table entirely.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct ImpactScale {
    /// The Fortune delta per rung, in [`Impact::ALL`] order.
    pub fortune: [i32; 5],
    /// The Repute delta per rung, in [`Impact::ALL`] order (`D20`).
    pub repute: [i32; 5],
}

impl ImpactScale {
    /// The shipped scale: `4c`'s Personal ±5 to Global ±100, with a matching
    /// Repute ladder authored in the same shape (`D20`).
    pub fn canonical() -> ImpactScale {
        ImpactScale {
            fortune: [5, 10, 25, 50, 100],
            repute: [1, 2, 4, 6, 10],
        }
    }

    /// The Fortune an event of this impact moves, with the sign of `gain`.
    pub fn fortune_for(&self, impact: Impact, gain: bool) -> i32 {
        let amount = self.fortune[impact as usize];
        if gain {
            amount
        } else {
            -amount
        }
    }

    /// The Repute an event of this impact moves, with the sign of `gain`.
    pub fn repute_for(&self, impact: Impact, gain: bool) -> i32 {
        let amount = self.repute[impact as usize];
        if gain {
            amount
        } else {
            -amount
        }
    }

    /// Read the scale from a rules record.
    pub fn parse(value: &Json) -> Result<ImpactScale, &'static str> {
        let mut scale = ImpactScale::canonical();
        if let Some(Json::Arr(items)) = value.get("fortune") {
            if items.len() != 5 {
                return Err("impact.fortune must list five rungs");
            }
            for (index, item) in items.iter().enumerate() {
                scale.fortune[index] = item
                    .as_i64()
                    .ok_or("impact.fortune entries must be numbers")?
                    as i32;
            }
        }
        if let Some(Json::Arr(items)) = value.get("repute") {
            if items.len() != 5 {
                return Err("impact.repute must list five rungs");
            }
            for (index, item) in items.iter().enumerate() {
                scale.repute[index] =
                    item.as_i64()
                        .ok_or("impact.repute entries must be numbers")? as i32;
            }
        }
        Ok(scale)
    }
}

impl Default for ImpactScale {
    fn default() -> ImpactScale {
        ImpactScale::canonical()
    }
}

/// Apply an impact to a character's Fortune and Repute (`4c:1229-1251`, `D20`).
///
/// Repute is floored at 0, because `4c:208` defines it as a score on a 0–33
/// scale; Fortune is not floored, because it may legitimately be spent to 0 and a
/// penalty that cannot be paid is a story consequence rather than a clamp.
pub fn apply_impact(scale: &ImpactScale, character: &mut Character, impact: Impact, gain: bool) {
    character.fortune += scale.fortune_for(impact, gain);
    character.repute = (character.repute + scale.repute_for(impact, gain)).max(0);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hero() -> Character {
        let mut character = Character::authored(
            1,
            "hero",
            "",
            0,
            [10, 10, 10, 10, 10, 10, 10],
            10,
            4,
            vec![],
            vec![],
            vec![],
        );
        character.fortune = 500;
        character
    }

    #[test]
    fn a_row_step_costs_the_current_value() {
        let advancement = Advancement::canonical();
        assert_eq!(advancement.trait_step_cost(10), 10);
        // Two steps are priced along the way, not twice the first price.
        assert_eq!(trait_step_cost(&advancement, 10, 2), 21);
        assert_eq!(trait_step_cost(&advancement, 10, 0), 0);
    }

    #[test]
    fn advancing_a_trait_spends_fortune_and_keeps_the_stored_value_legal() {
        let advancement = Advancement::canonical();
        let mut character = hero();
        // `hero()` is fresh, so its pools are empty and the first recompute fills
        // them; advancing then spends from the filled pool.
        character.recompute_derived();
        let before = character.fortune;
        let spent = advance_trait(&advancement, &mut character, Trait::Melee, 2).expect("advance");
        assert_eq!(spent, 21);
        assert_eq!(character.stored(Trait::Melee), 12);
        assert_eq!(character.fortune, before - 21);
        // Damage is the sum of the first four traits, so a Melee step raises the
        // ceiling; a character who was at full health stays there.
        assert_eq!(character.max_damage, 42);
        assert_eq!(character.damage, 42);
    }

    #[test]
    fn advancement_is_refused_rather_than_clamped() {
        let mut advancement = Advancement::canonical();
        let mut character = hero();
        character.fortune = 0;
        assert_eq!(
            advance_trait(&advancement, &mut character, Trait::Melee, 1),
            Err(Refusal::NotEnoughFortune)
        );
        assert_eq!(character.stored(Trait::Melee), 10, "nothing moved");
        advancement.max_rank_value = 10;
        character.fortune = 500;
        assert_eq!(
            advance_trait(&advancement, &mut character, Trait::Melee, 1),
            Err(Refusal::AtCeiling)
        );
        assert_eq!(
            advance_trait(&advancement, &mut character, Trait::Melee, 0),
            Err(Refusal::IllegalValue)
        );
    }

    #[test]
    fn a_power_advances_only_when_it_is_known() {
        let advancement = Advancement::canonical();
        let mut character = hero();
        assert_eq!(
            advance_power(&advancement, &mut character, "wsp.power.absorption", 1),
            Err(Refusal::NoSuchPower)
        );
        character.powers.push(crate::l1::powers::PowerInst::new(
            "wsp.power.absorption",
            20,
        ));
        let spent = advance_power(&advancement, &mut character, "wsp.power.absorption", 2)
            .expect("advance");
        assert_eq!(spent, 41, "20 then 21");
        assert_eq!(character.powers[0].rv, 22);
    }

    #[test]
    fn the_impact_scale_moves_both_fortune_and_repute() {
        let scale = ImpactScale::canonical();
        assert_eq!(scale.fortune_for(Impact::Global, true), 100);
        assert_eq!(scale.fortune_for(Impact::Global, false), -100);
        assert_eq!(scale.fortune_for(Impact::Personal, true), 5);
        let mut character = hero();
        let fortune = character.fortune;
        apply_impact(&scale, &mut character, Impact::Regional, false);
        assert_eq!(character.fortune, fortune - 50);
        assert_eq!(character.repute, 0, "repute is floored at zero");
        apply_impact(&scale, &mut character, Impact::Local, true);
        assert_eq!(character.repute, 2);
    }

    #[test]
    fn a_campaign_can_tune_the_curve_from_content() {
        let advancement = Advancement::parse(&Json::from_text(
            r#"{"trait_step":{"per_rv":2,"flat":5},"power_cost":500,"skill_cost":100,
                "power_step":{"per_rv":3},"max_rank_value":40,"max_power_rv":60}"#,
        ))
        .expect("parse");
        assert_eq!(advancement.power_cost, 500);
        assert_eq!(advancement.skill_cost, 100);
        assert_eq!(advancement.trait_step_cost(10), 25);
        assert_eq!(advancement.power_step_cost(10), 30);
        assert_eq!(advancement.max_rank_value, 40);
        assert_eq!(advancement.max_power_rv, 60);

        let scale = ImpactScale::parse(&Json::from_text(
            r#"{"fortune":[1,2,3,4,5],"repute":[9,8,7,6,5]}"#,
        ))
        .expect("parse");
        assert_eq!(scale.fortune, [1, 2, 3, 4, 5]);
        assert_eq!(scale.repute, [9, 8, 7, 6, 5]);
        assert!(ImpactScale::parse(&Json::from_text(r#"{"fortune":[1,2]}"#)).is_err());
    }
}
