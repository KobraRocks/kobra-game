//! L1 — statuses and conditions (02:02.5 "Damage and conditions", `4c:1161`).
//!
//! A condition is an entity with a declared expiry rule, and **condition
//! interaction is where simulator bugs live**, so the precedence between them is
//! an explicit table rather than a chain of `if`s scattered across the combat
//! path (02:02.5). M1's entry paths are the melee, ranged and dodge outcomes;
//! the remaining kinds are declared here because a save and a content record can
//! name them, and M2 gives them entry paths.
//!
//! Conditions are owned by the character they affect rather than by a separate
//! pool. The architecture's L2 table calls them entities; M1's entities are one
//! active encounter, so a per-character list is the same set with one less
//! indirection, and `02:02.9`'s save stores them the same way either way.

/// One kind of condition.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug, Hash)]
pub enum ConditionKind {
    /// Pound Black, or a knock-back into an obstacle (`4c:1175`).
    KnockedDown,
    /// Concuss Black, or Scream/Stench Black (`4c:1161`).
    KnockedOut,
    /// Damage reduced to 0 by a slashing or ranged Yellow (`4c:1161`).
    Dying,
    /// Scream/Stench Blue: one panel, `-20` on `d%` rolls (`4c:1426`).
    Stunned,
    /// Force Field mental Black: `1..=10` panels, no actions (`4c:597`).
    Dazed,
    /// Paralyzing Touch or a Paralytic grenade (`4c:668-670`).
    Paralyzed,
    /// Wrestling Hold: Brawn damage per panel while held (`4c:1030`).
    Held,
    /// Drowning Red: Fortitude drops one row step, cumulative (`4c:929`).
    Winded,
    /// Stench Red: no actions (`4c:1435`).
    Nauseous,
    /// Stench Blue: `-20` on rolls (`4c:1435`).
    Queasy,
    /// Exhaustion Black/Red: rest `3-30` or `2-20` panels (`4c:946`).
    Collapsed,
    /// Invisibility or Chameleon (`4c:642`).
    Invisible,
    /// Physical Metamorphosis into energy (`4c:696`).
    InEnergyForm,
    /// The Growth half of Growth/Shrinking (`4c:618`).
    Grown,
    /// The Shrinking half (`4c:620`).
    Shrunk,
    /// Absorbed energy held for the next attack (`4c:424`).
    Charged,
    /// Mind Control holds the character (`4c:656`).
    MindControlled,
    /// A force-field device shorted out for `1-10` panels (`4c:595`).
    Shorted,
}

impl ConditionKind {
    /// Every kind, so a validator and the precedence test can enumerate the
    /// domain.
    pub const ALL: [ConditionKind; 18] = [
        ConditionKind::KnockedDown,
        ConditionKind::KnockedOut,
        ConditionKind::Dying,
        ConditionKind::Stunned,
        ConditionKind::Dazed,
        ConditionKind::Paralyzed,
        ConditionKind::Held,
        ConditionKind::Winded,
        ConditionKind::Nauseous,
        ConditionKind::Queasy,
        ConditionKind::Collapsed,
        ConditionKind::Invisible,
        ConditionKind::InEnergyForm,
        ConditionKind::Grown,
        ConditionKind::Shrunk,
        ConditionKind::Charged,
        ConditionKind::MindControlled,
        ConditionKind::Shorted,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ConditionKind::KnockedDown => "knocked_down",
            ConditionKind::KnockedOut => "knocked_out",
            ConditionKind::Dying => "dying",
            ConditionKind::Stunned => "stunned",
            ConditionKind::Dazed => "dazed",
            ConditionKind::Paralyzed => "paralyzed",
            ConditionKind::Held => "held",
            ConditionKind::Winded => "winded",
            ConditionKind::Nauseous => "nauseous",
            ConditionKind::Queasy => "queasy",
            ConditionKind::Collapsed => "collapsed",
            ConditionKind::Invisible => "invisible",
            ConditionKind::InEnergyForm => "in_energy_form",
            ConditionKind::Grown => "grown",
            ConditionKind::Shrunk => "shrunk",
            ConditionKind::Charged => "charged",
            ConditionKind::MindControlled => "mind_controlled",
            ConditionKind::Shorted => "shorted",
        }
    }

    /// The kind a content id names.
    pub fn from_id(id: &str) -> Option<ConditionKind> {
        ConditionKind::ALL.into_iter().find(|kind| kind.id() == id)
    }

    /// The display string id (`AD-22`).
    pub const fn string_id(self) -> &'static str {
        match self {
            ConditionKind::KnockedDown => "condition.knocked_down",
            ConditionKind::KnockedOut => "condition.knocked_out",
            ConditionKind::Dying => "condition.dying",
            ConditionKind::Stunned => "condition.stunned",
            ConditionKind::Dazed => "condition.dazed",
            ConditionKind::Paralyzed => "condition.paralyzed",
            ConditionKind::Held => "condition.held",
            ConditionKind::Winded => "condition.winded",
            ConditionKind::Nauseous => "condition.nauseous",
            ConditionKind::Queasy => "condition.queasy",
            ConditionKind::Collapsed => "condition.collapsed",
            ConditionKind::Invisible => "condition.invisible",
            ConditionKind::InEnergyForm => "condition.in_energy_form",
            ConditionKind::Grown => "condition.grown",
            ConditionKind::Shrunk => "condition.shrunk",
            ConditionKind::Charged => "condition.charged",
            ConditionKind::MindControlled => "condition.mind_controlled",
            ConditionKind::Shorted => "condition.shorted",
        }
    }

    /// Whether this condition counts down with the panel clock.
    ///
    /// `Dying` advances its own way each panel (`4c:1161`); `Winded` is a
    /// cumulative row-step loss that ends when the character reaches breathable
    /// air (`4c:929`, `D10`); `Held` ends when the hold is broken (`4c:1030`);
    /// and the power states end when the power does, not on a counter.
    pub const fn has_duration(self) -> bool {
        !matches!(
            self,
            ConditionKind::Dying
                | ConditionKind::Winded
                | ConditionKind::Held
                | ConditionKind::Invisible
                | ConditionKind::InEnergyForm
                | ConditionKind::Grown
                | ConditionKind::Shrunk
                | ConditionKind::Charged
                | ConditionKind::MindControlled
        )
    }
}

/// One active condition.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Condition {
    /// Which condition.
    pub kind: ConditionKind,
    /// Panels remaining; `-1` means "until something else ends it".
    pub panels: i32,
    /// The entity that applied it, for the event log.
    pub source: u32,
}

impl Condition {
    /// A condition lasting `panels` panels.
    pub fn new(kind: ConditionKind, panels: i32, source: u32) -> Condition {
        Condition {
            kind,
            panels,
            source,
        }
    }

    /// An indefinite condition, ended by a rule rather than by a counter.
    pub fn indefinite(kind: ConditionKind, source: u32) -> Condition {
        Condition {
            kind,
            panels: -1,
            source,
        }
    }
}

/// What a character may do this panel, given its conditions.
///
/// The most restrictive condition wins, which is the precedence table
/// (02:02.5) expressed as a lattice rather than as an order of checks.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub enum Permission {
    /// May act normally.
    Allowed,
    /// May only roll to resist or recover (`Dazed` permits resistance rolls).
    ResistOnly,
    /// May only attempt to break free.
    BreakOnly,
    /// May only stand up.
    StandOnly,
    /// May take no action at all.
    Forbidden,
}

impl Permission {
    /// Whether the character may take an ordinary action.
    pub fn can_act(self) -> bool {
        self == Permission::Allowed
    }
}

/// The permission a set of conditions leaves.
pub fn permission(conditions: &[Condition]) -> Permission {
    let mut worst = Permission::Allowed;
    for condition in conditions {
        let each = match condition.kind {
            ConditionKind::KnockedOut | ConditionKind::Paralyzed | ConditionKind::Nauseous => {
                Permission::Forbidden
            }
            ConditionKind::Collapsed => Permission::Forbidden,
            ConditionKind::KnockedDown => Permission::StandOnly,
            ConditionKind::Held => Permission::BreakOnly,
            ConditionKind::Dazed => Permission::ResistOnly,
            // These do not stop an action; they make it worse, or they are
            // resolved by another system: `MindControlled` hands the action to
            // the controller, which is an AI override rather than a suppression
            // (`02:02.6`), and `Shorted` costs the force field, not the turn.
            ConditionKind::Dying
            | ConditionKind::Stunned
            | ConditionKind::Winded
            | ConditionKind::Queasy
            | ConditionKind::Invisible
            | ConditionKind::InEnergyForm
            | ConditionKind::Grown
            | ConditionKind::Shrunk
            | ConditionKind::Charged
            | ConditionKind::MindControlled
            | ConditionKind::Shorted => Permission::Allowed,
        };
        worst = worst.max(each);
    }
    worst
}

/// The flat `d%` penalty from conditions (`4c:1426`, `4c:1435`).
///
/// `Stunned` and `Queasy` are `-20`; the penalty is declared here rather than in
/// the resolution path so a content parameter can replace it later.
pub fn roll_penalty(conditions: &[Condition]) -> i32 {
    conditions
        .iter()
        .map(|condition| match condition.kind {
            ConditionKind::Stunned | ConditionKind::Queasy => -20,
            _ => 0,
        })
        .sum()
}

/// The row steps conditions impose on a trait.
///
/// `Winded` drops Fortitude one row step and is cumulative (`4c:929`, `D10`).
pub fn trait_steps(conditions: &[Condition]) -> Vec<crate::l1::traits::TraitStep> {
    let winded = conditions
        .iter()
        .filter(|condition| condition.kind == ConditionKind::Winded)
        .count() as i32;
    if winded == 0 {
        Vec::new()
    } else {
        vec![crate::l1::traits::TraitStep {
            target: crate::l1::traits::Trait::Fortitude,
            steps: -winded,
        }]
    }
}

/// Decrement every finite condition by one panel, dropping the expired ones.
///
/// Returns the kinds that expired, so the RESOLVE phase can emit an event.
pub fn tick(conditions: &mut Vec<Condition>) -> Vec<ConditionKind> {
    let mut expired = Vec::new();
    for condition in conditions.iter_mut() {
        if condition.kind.has_duration() && condition.panels > 0 {
            condition.panels -= 1;
        }
    }
    conditions.retain(|condition| {
        if condition.kind.has_duration() && condition.panels == 0 {
            expired.push(condition.kind);
            false
        } else {
            true
        }
    });
    expired
}

/// Add or refresh a condition, keeping the longer duration.
pub fn apply(conditions: &mut Vec<Condition>, condition: Condition) {
    if let Some(existing) = conditions
        .iter_mut()
        .find(|active| active.kind == condition.kind)
    {
        if condition.panels < 0 || existing.panels < 0 {
            existing.panels = -1;
        } else {
            existing.panels = existing.panels.max(condition.panels);
        }
        existing.source = condition.source;
        return;
    }
    conditions.push(condition);
}

/// Remove a condition entirely.
pub fn remove(conditions: &mut Vec<Condition>, kind: ConditionKind) -> bool {
    let before = conditions.len();
    conditions.retain(|condition| condition.kind != kind);
    conditions.len() != before
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_precedence_table_is_most_restrictive_wins() {
        let knocked_out = [Condition::new(ConditionKind::KnockedOut, 3, 1)];
        assert_eq!(permission(&knocked_out), Permission::Forbidden);
        let down_and_dazed = [
            Condition::new(ConditionKind::KnockedDown, 3, 1),
            Condition::new(ConditionKind::Dazed, 3, 1),
        ];
        assert_eq!(permission(&down_and_dazed), Permission::StandOnly);
        let held = [Condition::new(ConditionKind::Held, -1, 1)];
        assert_eq!(permission(&held), Permission::BreakOnly);
        assert_eq!(permission(&[]), Permission::Allowed);
        // Every ordered pair resolves to the same lattice, so no pair has an
        // order-dependent verdict.
        for a in ConditionKind::ALL {
            for b in ConditionKind::ALL {
                let pair = [Condition::new(a, 2, 1), Condition::new(b, 2, 1)];
                let reversed = [Condition::new(b, 2, 1), Condition::new(a, 2, 1)];
                assert_eq!(permission(&pair), permission(&reversed), "{a:?}/{b:?}");
            }
        }
    }

    #[test]
    fn stunned_and_queasy_are_twenty_point_penalties() {
        let conditions = [
            Condition::new(ConditionKind::Stunned, 1, 1),
            Condition::new(ConditionKind::Queasy, 2, 1),
        ];
        assert_eq!(roll_penalty(&conditions), -40);
    }

    #[test]
    fn winded_stacks_one_row_step_per_condition() {
        let conditions = [
            Condition::new(ConditionKind::Winded, -1, 1),
            Condition::new(ConditionKind::Winded, -1, 1),
        ];
        let steps = trait_steps(&conditions);
        assert_eq!(steps.len(), 1);
        assert_eq!(steps[0].steps, -2);
    }

    #[test]
    fn a_timed_condition_expires_and_an_indefinite_one_does_not() {
        let mut conditions = vec![
            Condition::new(ConditionKind::Stunned, 1, 1),
            Condition::indefinite(ConditionKind::Dying, 2),
        ];
        let expired = tick(&mut conditions);
        assert_eq!(expired, vec![ConditionKind::Stunned]);
        assert_eq!(conditions.len(), 1);
        assert_eq!(conditions[0].kind, ConditionKind::Dying);
    }

    #[test]
    fn applying_a_condition_keeps_the_longer_duration() {
        let mut conditions = vec![Condition::new(ConditionKind::Stunned, 2, 1)];
        apply(
            &mut conditions,
            Condition::new(ConditionKind::Stunned, 1, 2),
        );
        assert_eq!(conditions[0].panels, 2);
        apply(
            &mut conditions,
            Condition::new(ConditionKind::Stunned, 5, 2),
        );
        assert_eq!(conditions[0].panels, 5);
        apply(
            &mut conditions,
            Condition::indefinite(ConditionKind::Stunned, 2),
        );
        assert_eq!(conditions[0].panels, -1);
        assert_eq!(conditions[0].source, 2);
    }
}
