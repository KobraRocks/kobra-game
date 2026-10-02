//! L1 — primary and secondary traits (02:02.3).
//!
//! The seven Primary Traits are in a **fixed order**, because two derived values
//! are defined as "the first four" and "the last three" (`4c:190`, `4c:196`).
//! That ordering is part of the rules, so it is asserted by a test rather than
//! left to a struct's field order.
//!
//! An **effective** trait is distinct from a **stored** trait: powers, items and
//! temporary statuses shift the effective value a roll uses (02:02.3). The
//! resolution path therefore asks for [`Effective::trait_band`], which folds the
//! modifiers in; nothing else may read the stored value on a roll.

use crate::l0::Ladder;

/// One of the seven Primary Traits, in the rules' fixed order.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug, Hash)]
#[repr(u8)]
pub enum Trait {
    /// Physical attacks in close combat.
    Melee = 0,
    /// Aim, agility and fine control.
    Coordination = 1,
    /// Strength and grip.
    Brawn = 2,
    /// Endurance and resistance.
    Fortitude = 3,
    /// Reason and knowledge.
    Intellect = 4,
    /// Perception and instinct.
    Awareness = 5,
    /// Mental strength.
    Willpower = 6,
}

/// Every Primary Trait, in the rules' order.
pub const TRAITS: [Trait; 7] = [
    Trait::Melee,
    Trait::Coordination,
    Trait::Brawn,
    Trait::Fortitude,
    Trait::Intellect,
    Trait::Awareness,
    Trait::Willpower,
];

impl Trait {
    /// The index into a `[i32; 7]` sheet.
    pub const fn index(self) -> usize {
        self as usize
    }

    /// The stable content id, used by skills, items and the DSL.
    pub const fn id(self) -> &'static str {
        match self {
            Trait::Melee => "melee",
            Trait::Coordination => "coordination",
            Trait::Brawn => "brawn",
            Trait::Fortitude => "fortitude",
            Trait::Intellect => "intellect",
            Trait::Awareness => "awareness",
            Trait::Willpower => "willpower",
        }
    }

    /// The trait a content id names.
    pub fn from_id(id: &str) -> Option<Trait> {
        TRAITS.into_iter().find(|trait_| trait_.id() == id)
    }

    /// The display string id (`AD-22`): the engine never contains English, so
    /// this is a key into a string table, not a label.
    pub const fn string_id(self) -> &'static str {
        match self {
            Trait::Melee => "trait.melee",
            Trait::Coordination => "trait.coordination",
            Trait::Brawn => "trait.brawn",
            Trait::Fortitude => "trait.fortitude",
            Trait::Intellect => "trait.intellect",
            Trait::Awareness => "trait.awareness",
            Trait::Willpower => "trait.willpower",
        }
    }

    /// The traits summed for the Damage score: the first four (`4c:190`).
    pub const DAMAGE_TRAITS: [Trait; 4] = [
        Trait::Melee,
        Trait::Coordination,
        Trait::Brawn,
        Trait::Fortitude,
    ];

    /// The traits summed for the starting Fortune score: the last three
    /// (`4c:196`).
    pub const FORTUNE_TRAITS: [Trait; 3] = [Trait::Intellect, Trait::Awareness, Trait::Willpower];
}

/// One situational row-step modifier on a trait (`02:02.3`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct TraitStep {
    /// The trait shifted.
    pub target: Trait,
    /// How many rows, signed.
    pub steps: i32,
}

/// A read of stored traits plus the modifiers currently in force.
///
/// This is the only path a roll may use to read a trait (02:02.3): it folds in
/// item modifiers, status effects and the chosen parameters of powers, so a
/// modified trait is a row-step shift of the stored value rather than a second
/// arithmetic.
///
/// The steps are owned rather than borrowed because their source — a character's
/// condition list — lives in the same value as the sheet, so a borrowing version
/// would need a self-referential struct. The vector is built when a roll is
/// made, not per tick, and a character with no conditions produces an empty one.
pub struct Effective<'a> {
    /// The ladder the row steps move on.
    pub ladder: &'a Ladder,
    /// The stored Rank Values, indexed by [`Trait::index`].
    pub stored: &'a [i32; 7],
    /// The row-step modifiers in force.
    pub steps: Vec<TraitStep>,
    /// Rank Values a power sets outright.
    ///
    /// A power's clause is a *value* ("use the power's Rank Value or Awareness
    /// +10, whichever is greater", `4c:470`), not a row step: a row-step reading
    /// would move an uneven ladder by an even amount and give the wrong band at
    /// the top of the scale. Conditions are row steps, powers are floors, and
    /// the two are kept apart on purpose.
    pub floors: Vec<(Trait, i32)>,
}

impl Effective<'_> {
    /// The stored Rank Value, unmodified.
    pub fn stored_rv(&self, trait_: Trait) -> i32 {
        self.stored[trait_.index()]
    }

    /// The effective Rank Value a roll uses.
    ///
    /// The row steps move the band; a power's floor then raises the value if it
    /// is higher; the result is re-banded, so a floor between two bands lands on
    /// the band it names rather than the one below it.
    pub fn trait_rv(&self, trait_: Trait) -> i32 {
        let base = self.ladder.band_index(self.stored_rv(trait_));
        let steps: i32 = self
            .steps
            .iter()
            .filter(|step| step.target == trait_)
            .map(|step| step.steps)
            .sum();
        let band = self.ladder.row_step(base, steps);
        let mut value = self.ladder.bands[band].lo;
        for (target, floor) in &self.floors {
            if *target == trait_ {
                value = value.max(*floor);
            }
        }
        self.ladder.bands[self.ladder.band_index(value)].lo
    }

    /// The band index a roll on `trait_` uses, after every modifier.
    pub fn trait_band(&self, trait_: Trait) -> usize {
        self.ladder.band_index(self.trait_rv(trait_))
    }

    /// The Damage score: the sum of the first four stored Rank Values (`4c:190`).
    pub fn damage_score(&self) -> i32 {
        Trait::DAMAGE_TRAITS
            .iter()
            .map(|trait_| self.stored_rv(*trait_))
            .sum()
    }

    /// The starting Fortune score: the sum of the last three (`4c:196`).
    pub fn fortune_score(&self) -> i32 {
        Trait::FORTUNE_TRAITS
            .iter()
            .map(|trait_| self.stored_rv(*trait_))
            .sum()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tables;

    #[test]
    fn the_trait_order_is_the_rules_order() {
        // 02:02.3: "the trait array order is part of the rules, so the ordering
        // is asserted by test".
        assert_eq!(TRAITS.len(), 7);
        for (index, trait_) in TRAITS.iter().enumerate() {
            assert_eq!(trait_.index(), index);
        }
        assert_eq!(
            Trait::DAMAGE_TRAITS,
            [
                Trait::Melee,
                Trait::Coordination,
                Trait::Brawn,
                Trait::Fortitude
            ]
        );
        assert_eq!(
            Trait::FORTUNE_TRAITS,
            [Trait::Intellect, Trait::Awareness, Trait::Willpower]
        );
    }

    #[test]
    fn every_trait_round_trips_through_its_id() {
        for trait_ in TRAITS {
            assert_eq!(Trait::from_id(trait_.id()), Some(trait_));
        }
        assert_eq!(Trait::from_id("luck"), None);
    }

    #[test]
    fn derived_scores_sum_the_documented_halves() {
        let stored = [10, 20, 30, 40, 3, 6, 10];
        let effective = Effective {
            ladder: tables::CAMPAIGN_LADDER,
            stored: &stored,
            steps: Vec::new(),
            floors: Vec::new(),
        };
        assert_eq!(effective.damage_score(), 100);
        assert_eq!(effective.fortune_score(), 19);
    }

    #[test]
    fn a_row_step_moves_the_band_and_saturates() {
        let stored = [20, 6, 6, 6, 6, 6, 6];
        let steps = [TraitStep {
            target: Trait::Melee,
            steps: 1,
        }];
        let effective = Effective {
            ladder: tables::CAMPAIGN_LADDER,
            stored: &stored,
            steps: steps.to_vec(),
            floors: Vec::new(),
        };
        // 20-29 shifted +1 is 30-39.
        assert_eq!(effective.trait_rv(Trait::Melee), 30);
        let down = [TraitStep {
            target: Trait::Melee,
            steps: -2,
        }];
        let effective = Effective {
            ladder: tables::CAMPAIGN_LADDER,
            stored: &stored,
            steps: down.to_vec(),
            floors: Vec::new(),
        };
        // 20-29 shifted -2 is 6-9 (4c:864).
        assert_eq!(effective.trait_rv(Trait::Melee), 6);
        let far = [TraitStep {
            target: Trait::Melee,
            steps: -99,
        }];
        let effective = Effective {
            ladder: tables::CAMPAIGN_LADDER,
            stored: &stored,
            steps: far.to_vec(),
            floors: Vec::new(),
        };
        assert_eq!(effective.trait_rv(Trait::Melee), 0, "clamps at rank 0");
    }
}
