//! L2 — actions and their outcome rows (02:02.5).
//!
//! Every action is one of a closed set, each with its own table from the spec.
//! The engine models this as one `Action` enum, one `resolve_action`
//! dispatcher, and **authored outcome tables** — because the prose for each
//! colour is a *sequence of effects*, and keeping it in content is what lets a
//! campaign reskin combat without touching Rust (02:02.5).
//!
//! Two structural requirements are easy to miss and are both modelled here:
//!
//! - **Declarations are pre-roll state.** Nail and Pulling Your Punch exist in
//!   the rules as things the player states *before* rolling, so the action
//!   record carries declared intent rather than the resolver inferring it.
//! - **Outcome rows are content.** Each cell is an effect list from the same
//!   vocabulary as powers, so a mod can change what a colour does.

use crate::json::Json;
use crate::l0::Colour;
use crate::l1::skills::ActionTag;
use crate::l1::traits::Trait;

/// The closed set of actions M1 resolves (02:02.5).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ActionKind {
    /// A blunt melee attack (`4c:952-959`).
    MeleeBash,
    /// A sharp melee attack (`4c:961-968`).
    MeleeSlash,
    /// An attack across a distance (`4c:970-979`).
    Ranged,
    /// Avoiding attacks for the panel (`4c:1044-1053`).
    Dodge,
    /// Moving up to the movement allowance.
    Move,
    /// Standing up from `knocked_down` (`4c:1175`).
    Stand,
    /// Doing nothing, deliberately (`4c:1127-1129`).
    Wait,
    /// Using an offensive power (`02:02.4`). The instance is named by
    /// [`Action::power`], because a character may hold two powers that both
    /// attack (`Telekinesis` and `Elemental/Energy Control`, say).
    Power,
    /// Driving a vehicle into something (`4c:1344-1357`).
    Ram,
    /// A sharp turn or other unusual manoeuvre (`4c:1314-1334`).
    Manoeuvre,
}

impl ActionKind {
    /// Every kind, so a validator can enumerate the domain.
    ///
    /// Order matters: `save.rs` writes an index into this array, so a new kind
    /// is appended rather than inserted.
    pub const ALL: [ActionKind; 10] = [
        ActionKind::MeleeBash,
        ActionKind::MeleeSlash,
        ActionKind::Ranged,
        ActionKind::Dodge,
        ActionKind::Move,
        ActionKind::Stand,
        ActionKind::Wait,
        ActionKind::Power,
        ActionKind::Ram,
        ActionKind::Manoeuvre,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ActionKind::MeleeBash => "melee_bash",
            ActionKind::MeleeSlash => "melee_slash",
            ActionKind::Ranged => "ranged",
            ActionKind::Dodge => "dodge",
            ActionKind::Move => "move",
            ActionKind::Stand => "stand",
            ActionKind::Wait => "wait",
            ActionKind::Power => "power",
            ActionKind::Ram => "ram",
            ActionKind::Manoeuvre => "manoeuvre",
        }
    }

    /// The kind a content id names.
    pub fn from_id(id: &str) -> Option<ActionKind> {
        ActionKind::ALL.into_iter().find(|kind| kind.id() == id)
    }

    /// The Primary Trait the roll uses (`02:02.5`).
    pub const fn trait_(self) -> Trait {
        match self {
            ActionKind::MeleeBash | ActionKind::MeleeSlash => Trait::Melee,
            ActionKind::Ranged | ActionKind::Dodge | ActionKind::Move => Trait::Coordination,
            ActionKind::Stand => Trait::Coordination,
            ActionKind::Wait => Trait::Coordination,
            // A power's attack trait is substituted by its own kernel
            // (`4c:502`, `4c:797`); Coordination is the base a ranged power use
            // starts from.
            ActionKind::Power => Trait::Coordination,
            // A vehicle manoeuvre rolls on Handling, which is the vehicle's
            // Rank Value rather than the driver's (`4c:1314`); the resolver
            // substitutes the band.
            ActionKind::Ram | ActionKind::Manoeuvre => Trait::Coordination,
        }
    }

    /// The skill tag that applies (`02:02.3`).
    pub const fn tag(self) -> ActionTag {
        match self {
            ActionKind::MeleeBash | ActionKind::MeleeSlash => ActionTag::Melee,
            ActionKind::Ranged => ActionTag::Ranged,
            ActionKind::Dodge => ActionTag::Dodge,
            ActionKind::Move | ActionKind::Stand | ActionKind::Wait | ActionKind::Power => {
                ActionTag::Melee
            }
            ActionKind::Ram | ActionKind::Manoeuvre => ActionTag::Pilot,
        }
    }

    /// Whether the action rolls on the Master Table at all.
    pub const fn is_roll(self) -> bool {
        !matches!(
            self,
            ActionKind::Move | ActionKind::Stand | ActionKind::Wait
        )
    }

    /// Whether the action needs a vehicle to be operating (`4c:1342`).
    pub const fn needs_a_vehicle(self) -> bool {
        matches!(self, ActionKind::Ram | ActionKind::Manoeuvre)
    }

    /// The shape a power use's attack takes, for the hook context (`02:02.4`).
    pub const fn power_shape(self) -> Option<crate::l1::powers::AttackShape> {
        match self {
            ActionKind::Power => Some(crate::l1::powers::AttackShape::Power),
            _ => None,
        }
    }
}

/// What the player stated before rolling (`4c:970-979`, `4c:1088-1090`).
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Declared {
    /// Trying for a Nail on a ranged attack (`4c:974`).
    pub nail: bool,
    /// The highest colour this attack may achieve — Pulling Your Punch
    /// (`4c:1088-1090`).
    pub punch_cap: Option<Colour>,
}

/// One effect an outcome cell can apply (02:02.5).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Effect {
    /// The target suffers damage through the pipeline (`4c:1074-1084`).
    Damage,
    /// A Brawn contest, then a Fortitude roll, may knock the target down
    /// (`4c:1175`).
    Knockdown,
    /// A Brawn-versus-Fortitude contest, then a Fortitude roll, may knock the
    /// target out (`4c:1161`).
    Knockout,
    /// The target's Damage drops to 0 and it begins dying (`4c:1161`).
    Dying,
    /// The acting character's Dodge: attackers suffer these row steps this
    /// panel (`4c:1044-1053`). Negative.
    DodgeSteps(i32),
}

impl Effect {
    /// The stable content id, for the event log.
    pub const fn id(self) -> &'static str {
        match self {
            Effect::Damage => "damage",
            Effect::Knockdown => "knockdown",
            Effect::Knockout => "knockout",
            Effect::Dying => "dying",
            Effect::DodgeSteps(_) => "dodge_steps",
        }
    }

    /// Read an effect from a content outcome row.
    pub fn parse(value: &Json) -> Result<Effect, &'static str> {
        match value {
            Json::Str(text) => match text.as_str() {
                "damage" => Ok(Effect::Damage),
                "knockdown" => Ok(Effect::Knockdown),
                "knockout" => Ok(Effect::Knockout),
                "dying" => Ok(Effect::Dying),
                _ => Err("unknown outcome effect"),
            },
            Json::Obj(_) => match value.get("dodge_steps").and_then(Json::as_i64) {
                Some(steps) => Ok(Effect::DodgeSteps(steps as i32)),
                None => Err("unknown outcome effect object"),
            },
            _ => Err("an outcome effect must be a string or an object"),
        }
    }
}

/// One action's four outcome rows, indexed by [`Colour::code`].
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct OutcomeTable {
    /// The action this table resolves.
    pub action: ActionKind,
    /// The effect list for each colour, Black first.
    pub rows: [Vec<Effect>; 4],
}

impl OutcomeTable {
    /// The canonical 4C table for an action (02:02.5).
    ///
    /// These are the shipped defaults; a content pack may replace any of them
    /// with an `outcome_table` record, because the outcome rows are content.
    pub fn canonical(action: ActionKind) -> OutcomeTable {
        let rows = match action {
            ActionKind::MeleeBash => [
                vec![],
                vec![Effect::Damage],
                vec![Effect::Damage, Effect::Knockdown],
                vec![Effect::Damage, Effect::Knockout],
            ],
            ActionKind::MeleeSlash => [
                vec![],
                vec![Effect::Damage],
                vec![Effect::Damage, Effect::Knockout],
                vec![Effect::Damage, Effect::Dying],
            ],
            ActionKind::Ranged => [
                vec![],
                vec![Effect::Damage],
                vec![Effect::Damage],
                vec![Effect::Damage, Effect::Dying],
            ],
            ActionKind::Dodge => [
                vec![],
                vec![Effect::DodgeSteps(-3)],
                vec![Effect::DodgeSteps(-6)],
                vec![Effect::DodgeSteps(-9)],
            ],
            // A power use has no canonical table: what it does is its kernel's
            // job (`02:02.4`). The row here is the ordinary "it connected"
            // damage, so an authored power with no `on_hit` still lands one.
            ActionKind::Power => [
                vec![],
                vec![Effect::Damage],
                vec![Effect::Damage],
                vec![Effect::Damage],
            ],
            // A collision's own table is the defender's dodge (`4c:1344`), not
            // an outcome row, so these carry none.
            ActionKind::Ram
            | ActionKind::Manoeuvre
            | ActionKind::Move
            | ActionKind::Stand
            | ActionKind::Wait => [vec![], vec![], vec![], vec![]],
        };
        OutcomeTable { action, rows }
    }

    /// The effects a colour applies.
    pub fn effects(&self, colour: Colour) -> &[Effect] {
        &self.rows[colour.code() as usize]
    }

    /// Read an outcome table from content.
    pub fn parse(action: ActionKind, data: &Json) -> Result<OutcomeTable, &'static str> {
        let rows = data.get("rows").ok_or("an outcome table needs rows")?;
        let mut parsed: [Vec<Effect>; 4] = [vec![], vec![], vec![], vec![]];
        for colour in [Colour::Blck, Colour::Red, Colour::Blue, Colour::Yel] {
            // Both the readable name and the source document's abbreviation are
            // accepted, so a transcript of the printed table reads straight in.
            let abbreviated = match colour {
                Colour::Blck => "Blck",
                Colour::Yel => "Yel",
                other => colour_name(other),
            };
            let Some(Json::Arr(effects)) = rows
                .get(colour_name(colour))
                .or_else(|| rows.get(abbreviated))
            else {
                return Err("every outcome row must be an array");
            };
            for effect in effects {
                parsed[colour.code() as usize].push(Effect::parse(effect)?);
            }
        }
        Ok(OutcomeTable {
            action,
            rows: parsed,
        })
    }
}

/// The content name of a colour.
///
/// Content and events use the readable names; the parser also accepts the
/// source document's own abbreviations (`Blck`, `Yel`), so a transcript of the
/// printed table reads straight into a pack.
pub const fn colour_name(colour: Colour) -> &'static str {
    match colour {
        Colour::Blck => "Black",
        Colour::Red => "Red",
        Colour::Blue => "Blue",
        Colour::Yel => "Yellow",
    }
}

/// One queued action.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Action {
    /// What the actor will do this panel.
    pub kind: ActionKind,
    /// The target entity, for an attack.
    pub target: u32,
    /// Where the actor is moving, for [`ActionKind::Move`].
    pub destination: Option<crate::l3::Position>,
    /// What the player declared before rolling.
    pub declared: Declared,
    /// Fortune points committed to this roll's window (`4c:874`).
    pub fortune: i32,
    /// The power instance a [`ActionKind::Power`] use names.
    pub power: Option<String>,
    /// The manoeuvre difficulty a [`ActionKind::Manoeuvre`] declares
    /// (`4c:1316-1322`).
    pub manoeuvre: Option<String>,
}

impl Action {
    /// An attack against a target.
    pub fn attack(kind: ActionKind, target: u32) -> Action {
        Action {
            kind,
            target,
            destination: None,
            declared: Declared::default(),
            fortune: 0,
            power: None,
            manoeuvre: None,
        }
    }

    /// An offensive power use against a target.
    pub fn use_power(power: &str, target: u32) -> Action {
        Action {
            kind: ActionKind::Power,
            target,
            destination: None,
            declared: Declared::default(),
            fortune: 0,
            power: Some(power.to_string()),
            manoeuvre: None,
        }
    }

    /// A move to a destination.
    pub fn move_to(destination: crate::l3::Position) -> Action {
        Action {
            kind: ActionKind::Move,
            target: 0,
            destination: Some(destination),
            declared: Declared::default(),
            fortune: 0,
            power: None,
            manoeuvre: None,
        }
    }

    /// A non-targeted action.
    pub fn simple(kind: ActionKind) -> Action {
        Action {
            kind,
            target: 0,
            destination: None,
            declared: Declared::default(),
            fortune: 0,
            power: None,
            manoeuvre: None,
        }
    }

    /// Read an action from a command document.
    pub fn parse(value: &Json) -> Result<Action, &'static str> {
        let kind = value
            .get("kind")
            .and_then(Json::as_str)
            .and_then(ActionKind::from_id)
            .ok_or("unknown action kind")?;
        let declared = Declared {
            nail: value.get("nail").and_then(Json::as_bool).unwrap_or(false),
            punch_cap: match value.get("punch_cap").and_then(Json::as_str) {
                Some(name) => Some(crate::l1::items::colour_from_id(name)?),
                None => None,
            },
        };
        let destination = match value.get("to").and_then(Json::as_arr) {
            Some(pair) if pair.len() == 2 => Some(crate::l3::Position {
                x: pair[0].as_i64().ok_or("to needs two numbers")? as i32,
                y: pair[1].as_i64().ok_or("to needs two numbers")? as i32,
                z: 0,
            }),
            Some(_) => return Err("to must be exactly [x, y]"),
            None => None,
        };
        let power = value
            .get("power")
            .and_then(Json::as_str)
            .map(str::to_string);
        if kind == ActionKind::Power && power.is_none() {
            return Err("a power action must name a power");
        }
        Ok(Action {
            kind,
            target: value.get("target").and_then(Json::as_i64).unwrap_or(0) as u32,
            destination,
            declared,
            fortune: value.get("fortune").and_then(Json::as_i64).unwrap_or(0) as i32,
            power,
            manoeuvre: value
                .get("difficulty")
                .and_then(Json::as_str)
                .map(str::to_string),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_canonical_tables_match_the_spec_rows() {
        // 4c:952-968, 4c:970-979, 4c:1044-1053.
        let bash = OutcomeTable::canonical(ActionKind::MeleeBash);
        assert!(bash.effects(Colour::Blck).is_empty());
        assert_eq!(bash.effects(Colour::Red), [Effect::Damage]);
        assert_eq!(
            bash.effects(Colour::Blue),
            [Effect::Damage, Effect::Knockdown]
        );
        assert_eq!(
            bash.effects(Colour::Yel),
            [Effect::Damage, Effect::Knockout]
        );
        let slash = OutcomeTable::canonical(ActionKind::MeleeSlash);
        assert_eq!(slash.effects(Colour::Yel), [Effect::Damage, Effect::Dying]);
        let dodge = OutcomeTable::canonical(ActionKind::Dodge);
        assert_eq!(dodge.effects(Colour::Blue), [Effect::DodgeSteps(-6)]);
    }

    #[test]
    fn parses_a_content_outcome_table() {
        let table = OutcomeTable::parse(
            ActionKind::Ranged,
            &Json::from_text(
                r#"{"rows":{"Blck":[],"Red":["damage"],"Blue":["damage"],"Yellow":["damage","dying"]}}"#,
            ),
        )
        .expect("parse");
        assert_eq!(table, OutcomeTable::canonical(ActionKind::Ranged));
        assert!(OutcomeTable::parse(
            ActionKind::Ranged,
            &Json::from_text(
                r#"{"rows":{"Blck":[],"Red":["damage"],"Blue":["damage"],"Yellow":["explode"]}}"#
            )
        )
        .is_err());
    }

    #[test]
    fn parses_a_declared_ranged_attack() {
        let action = Action::parse(&Json::from_text(
            r#"{"kind":"ranged","target":7,"nail":true,"punch_cap":"Red","fortune":50}"#,
        ))
        .expect("parse");
        assert_eq!(action.kind, ActionKind::Ranged);
        assert_eq!(action.target, 7);
        assert!(action.declared.nail);
        assert_eq!(action.declared.punch_cap, Some(Colour::Red));
        assert_eq!(action.fortune, 50);
        assert!(Action::parse(&Json::from_text(r#"{"kind":"teleport"}"#)).is_err());
    }

    #[test]
    fn a_move_carries_its_destination() {
        let action = Action::parse(&Json::from_text(r#"{"kind":"move","to":[3,4]}"#)).unwrap();
        assert_eq!(
            action.destination,
            Some(crate::l3::Position { x: 3, y: 4, z: 0 })
        );
        assert!(Action::parse(&Json::from_text(r#"{"kind":"move","to":[1]}"#)).is_err());
    }
}
