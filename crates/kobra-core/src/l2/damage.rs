//! L2 — the damage pipeline (02:02.5, `4c:1074-1084`).
//!
//! Damage resolution is a pipeline, not a subtraction, because multiple rules
//! intercept it:
//!
//! ```text
//! raw        = Brawn (unarmed) | Brawn+5 (one-handed) | Brawn+10 (two-handed)
//!            | material value (thrown) | weapon table | power RV
//! modified   = kernels: modify_damage          (M2: powers)
//! armour     = kernels: modify_armor           (M2: powers, plus worn armour)
//! inflicted  = max(0, modified - armour)       (4c:1084)
//! ```
//!
//! The result carries the **split**, not just a number, because `Force Field`
//! and `Absorption` need to know what was absorbed and what was inflicted
//! (02:02.5). M1 has no power kernels, so `absorbed` is always zero — but the
//! shape is here so adding one does not change every caller.

use crate::l1::items::{Hands, Item};

/// The outcome of one damage application.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Damage {
    /// The raw damage before armour.
    pub raw: i32,
    /// The armour actually applied, after any piercing.
    pub armour: i32,
    /// Damage diverted by an absorbing defence; always zero at M1.
    pub absorbed: i32,
    /// Damage taken off the Damage pool.
    pub inflicted: i32,
}

/// Raw melee damage: the attacker's Brawn, plus the weapon's hand bonus
/// (`4c:1074`).
pub fn raw_melee(brawn_rv: i32, hands: Hands) -> i32 {
    brawn_rv + crate::l1::character::melee_hands_bonus(hands)
}

/// Raw ranged damage from a weapon record, or a thrown object's material value
/// (`4c:1065-1082`).
pub fn raw_ranged(item: &Item) -> Option<i32> {
    if let Some(value) = item.material_value {
        return Some(value);
    }
    item.damage.map(|amount| match amount.dice {
        crate::l0::Dice::Constant(value) => value,
        // A dice-based weapon damage is drawn by the caller with the RNG so the
        // draw order stays explicit; this helper only covers the constant case
        // the catalogue uses.
        _ => 0,
    })
}

/// Armour after any piercing (`4c:1480`: half the target's armour).
pub fn armour_after_piercing(armour: i32, piercing: bool) -> i32 {
    if piercing {
        armour / 2
    } else {
        armour
    }
}

/// Apply the pipeline (`4c:1084`).
pub fn apply(raw: i32, armour: i32, piercing: bool) -> Damage {
    let applied = armour_after_piercing(armour, piercing);
    let after = (raw - applied).max(0);
    Damage {
        raw,
        armour: applied,
        absorbed: 0,
        inflicted: after,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn melee_damage_uses_the_hand_bonus() {
        assert_eq!(raw_melee(20, Hands::None), 20);
        assert_eq!(raw_melee(20, Hands::One), 25);
        assert_eq!(raw_melee(20, Hands::Two), 30);
    }

    #[test]
    fn armour_subtracts_and_never_heals() {
        // 4c:1084's own example: armour 10 against 20 points inflicts 10.
        let damage = apply(20, 10, false);
        assert_eq!(damage.inflicted, 10);
        assert_eq!(damage.armour, 10);
        // More armour than damage inflicts nothing, not negative damage.
        assert_eq!(apply(5, 10, false).inflicted, 0);
    }

    #[test]
    fn armour_piercing_halves_the_armour() {
        assert_eq!(apply(20, 10, true).inflicted, 15);
        assert_eq!(armour_after_piercing(11, true), 5);
    }

    #[test]
    fn a_thrown_object_uses_its_material_value() {
        let item = Item::parse(
            "wsp.item.block",
            &crate::json::Json::from_text(r#"{"material_value":20}"#),
        )
        .expect("parse");
        assert_eq!(raw_ranged(&item), Some(20));
    }
}
