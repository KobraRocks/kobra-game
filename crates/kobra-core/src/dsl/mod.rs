//! The effect DSL: triggers, conditions and effects compiled to a `Template`
//! kernel (`02:02.4`, `AD-16`).
//!
//! The DSL is what lets a data mod create a power, so it is deliberately **not**
//! a programming language: it is a list of triggers, conditions and effects over
//! the entity model, with integer expressions and dice terms (`02:02.4`). The
//! compiler resolves a record to a [`Template`] at load and reports errors as
//! `{path, code, args}` — never as a panic and never as a partially applied
//! power (`AD-15`).
//!
//! The expressiveness target is explicit and is the project's strongest modding
//! claim: **the DSL must be able to express every canonical power in the spec.**
//! `tests/dsl.rs` defines all 51 as templates from the shipped
//! `core-powers.pack.json` and compares their hook-for-hook behaviour against the
//! Rust kernels.
//!
//! # Shape
//!
//! ```json
//! {
//!   "id": "power.piercing-strike",
//!   "triggers": [
//!     { "on": "hit",
//!       "when": { "all": [ { "colour_at_least": "Blue" },
//!                          { "shape": "melee" } ] },
//!       "then": [ { "modify_damage": { "add": ["$rv"] } },
//!                 { "apply_status": { "id": "stunned",
//!                                     "panels": { "dice": "1d10", "div": 2, "round": "down", "min": 1 } } } ] }
//!   ]
//! }
//! ```
//!
//! A trigger names one hook; `when` is a condition over the frozen
//! [`PowerCtx`](crate::l1::powers::PowerCtx); `then` is a list of effects. Dice
//! are declared (an `Amount`), never drawn, because the RNG belongs to the
//! resolver (`02:02.9` rule 3).

use crate::json::Json;
use crate::l0::{Amount, Colour, Rounding};
use crate::l1::items::Hands;
use crate::l1::powers::{
    AcquirePlan, ActionEconomy, AttackMods, AttackShape, EffectTarget, HookEffect, MoveMode,
    MoveProfile, PowerCtx, PowerInst, PowerKernel, TurnPlan,
};
use crate::l1::status::ConditionKind;
use crate::l1::traits::Trait;

/// One compilation problem, reported as data (`AD-15`, `03:03.9`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DslError {
    /// Where in the record the problem is, as a JSON pointer under the record.
    pub path: String,
    /// A stable code, never English.
    pub code: &'static str,
    /// The arguments a localised message needs.
    pub args: Vec<(String, String)>,
}

impl DslError {
    /// A problem at `path`.
    pub fn new(path: &str, code: &'static str, args: &[(&str, &str)]) -> DslError {
        DslError {
            path: path.to_string(),
            code,
            args: args
                .iter()
                .map(|(key, value)| ((*key).to_string(), (*value).to_string()))
                .collect(),
        }
    }

    /// The problem as the validator's record shape.
    pub fn to_json(&self) -> Json {
        let mut arguments = Json::object();
        for (key, value) in &self.args {
            arguments.set(key, Json::string(value));
        }
        Json::obj([
            ("path", Json::string(&self.path)),
            ("code", Json::string(self.code)),
            ("args", arguments),
        ])
    }
}

/// The hook a trigger fires on (`02:02.4`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Hook {
    /// Once, at acquisition.
    Acquire,
    /// An effective trait read.
    ModifyTrait,
    /// An attack being planned.
    ModifyAttack,
    /// The damage an attack inflicts.
    ModifyDamage,
    /// The armour an attack meets.
    ModifyArmor,
    /// The movement a power grants.
    Movement,
    /// The action economy a power grants.
    ActionEconomy,
    /// After a hit lands.
    Hit,
    /// A resistance roll.
    Resist,
    /// The owner's own panel.
    OwnTurn,
    /// Damage diverted rather than inflicted.
    Absorb,
    /// Another entity's power use.
    PowerUsed,
}

impl Hook {
    /// The id a record names.
    pub fn from_id(id: &str) -> Option<Hook> {
        Some(match id {
            "acquire" => Hook::Acquire,
            "modify_trait" => Hook::ModifyTrait,
            "modify_attack" => Hook::ModifyAttack,
            "modify_damage" => Hook::ModifyDamage,
            "modify_armor" | "modify_armour" => Hook::ModifyArmor,
            "movement" => Hook::Movement,
            "action_economy" => Hook::ActionEconomy,
            "hit" | "on_hit" => Hook::Hit,
            "resist" | "on_resist" => Hook::Resist,
            "own_turn" | "on_own_turn" => Hook::OwnTurn,
            "absorb" | "on_absorb" => Hook::Absorb,
            "power_used" | "on_power_used" => Hook::PowerUsed,
            _ => return None,
        })
    }

    /// The stable id.
    pub const fn id(self) -> &'static str {
        match self {
            Hook::Acquire => "acquire",
            Hook::ModifyTrait => "modify_trait",
            Hook::ModifyAttack => "modify_attack",
            Hook::ModifyDamage => "modify_damage",
            Hook::ModifyArmor => "modify_armor",
            Hook::Movement => "movement",
            Hook::ActionEconomy => "action_economy",
            Hook::Hit => "hit",
            Hook::Resist => "resist",
            Hook::OwnTurn => "own_turn",
            Hook::Absorb => "absorb",
            Hook::PowerUsed => "power_used",
        }
    }
}

/// Which entity an expression reads.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Who {
    /// The entity carrying the power.
    Owner,
    /// The acting entity.
    Actor,
    /// The other party.
    Target,
}

impl Who {
    /// The id a record names.
    pub fn from_id(id: &str) -> Option<Who> {
        Some(match id {
            "owner" | "self" => Who::Owner,
            "actor" | "attacker" => Who::Actor,
            "target" => Who::Target,
            _ => return None,
        })
    }

    /// The entity id in a context.
    fn resolve(self, ctx: &PowerCtx<'_>) -> u32 {
        match self {
            Who::Owner => ctx.owner,
            Who::Actor => ctx.actor,
            Who::Target => ctx.target,
        }
    }
}

/// An integer expression, evaluated without the RNG.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum Expr {
    /// A literal.
    Const(i32),
    /// The power's Rank Value.
    Rv,
    /// `ceil(rv / 2)`.
    HalfRvUp,
    /// `floor(rv / 2)`.
    HalfRvDown,
    /// `ceil(rv / 10)`.
    TenthsUpRv,
    /// The distance between the parties, in tiles.
    Distance,
    /// The hook's incoming value.
    Result,
    /// The damage absorbed in this resolution.
    Absorbed,
    /// The value stored on the power.
    Charged,
    /// The attack's Rank Value.
    AttackRv,
    /// The attack's raw damage.
    AttackDamage,
    /// An entity's stored trait.
    Stored(Who, Trait),
    /// `ceil(value / 10)`.
    TenthsUp(Box<Expr>),
    /// The movement tiers Flight and Superleap print, in tiles (`4c:572`).
    SpeedTiles(Box<Expr>),
    /// The Water Native swim tiers, in tiles (`4c:833`).
    SwimTiles(Box<Expr>),
    /// The Fast Attack attacks-per-panel tiers (`4c:562`).
    FastAttacks(Box<Expr>),
    /// Another power's Rank Value, when the owner carries it (`4c:432`).
    OwnerPowerRv(String),
    /// An entity's stored trait, named by a power parameter (`4c:815`).
    StoredParam(Who, String),
    /// An entity's stored value for the trait the hook is reading (`4c:819`).
    StoredHook(Who),
    /// A sum.
    Add(Vec<Expr>),
    /// `left - right`.
    Sub(Box<Expr>, Box<Expr>),
    /// A product.
    Mul(Vec<Expr>),
    /// `left / right`, rounded as declared.
    Div(Box<Expr>, Box<Expr>, Rounding),
    /// The largest operand.
    Max(Vec<Expr>),
    /// The smallest operand.
    Min(Vec<Expr>),
}

impl Expr {
    /// Evaluate against a context.
    pub fn eval(&self, ctx: &PowerCtx<'_>, result: i32, trait_: Option<Trait>) -> i32 {
        match self {
            Expr::Const(value) => *value,
            Expr::Rv => ctx.power.rv,
            Expr::HalfRvUp => ctx.power.half_rv_up(),
            Expr::HalfRvDown => ctx.power.rv / 2,
            Expr::TenthsUpRv => crate::l1::powers::tenths_up(ctx.power.rv),
            Expr::Distance => ctx.distance,
            Expr::Result => result,
            Expr::Absorbed => ctx.charged,
            Expr::Charged => ctx.charged,
            Expr::AttackRv => ctx.attack.map_or(0, |attack| attack.rv),
            Expr::AttackDamage => ctx.attack.map_or(0, |attack| attack.damage),
            Expr::Stored(who, trait_) => ctx.stored_rv(who.resolve(ctx), *trait_),
            Expr::TenthsUp(inner) => crate::l1::powers::tenths_up(inner.eval(ctx, result, trait_)),
            Expr::SpeedTiles(inner) => {
                crate::l1::powers::speed_tiles(inner.eval(ctx, result, trait_))
            }
            Expr::SwimTiles(inner) => {
                crate::l1::powers::swim_tiles(inner.eval(ctx, result, trait_))
            }
            Expr::FastAttacks(inner) => {
                crate::l1::powers::fast_attack_count(inner.eval(ctx, result, trait_))
            }
            Expr::OwnerPowerRv(id) => ctx.owner_power(id).map_or(0, |power| power.rv),
            Expr::StoredParam(who, key) => Trait::from_id(ctx.power.param(key))
                .map_or(0, |named| ctx.stored_rv(who.resolve(ctx), named)),
            Expr::StoredHook(who) => {
                trait_.map_or(0, |named| ctx.stored_rv(who.resolve(ctx), named))
            }
            Expr::Add(items) => items
                .iter()
                .map(|item| item.eval(ctx, result, trait_))
                .sum(),
            Expr::Sub(left, right) => {
                left.eval(ctx, result, trait_) - right.eval(ctx, result, trait_)
            }
            Expr::Mul(items) => items
                .iter()
                .map(|item| item.eval(ctx, result, trait_))
                .product(),
            Expr::Div(left, right, round) => {
                let divisor = right.eval(ctx, result, trait_);
                if divisor == 0 {
                    return 0;
                }
                let value = left.eval(ctx, result, trait_);
                let quotient = value / divisor;
                let remainder = value % divisor;
                match round {
                    Rounding::Up if remainder != 0 && (value < 0) == (divisor < 0) => quotient + 1,
                    Rounding::Down if remainder != 0 && (value < 0) != (divisor < 0) => {
                        quotient - 1
                    }
                    _ => quotient,
                }
            }
            Expr::Max(items) => items
                .iter()
                .map(|item| item.eval(ctx, result, trait_))
                .max()
                .unwrap_or(0),
            Expr::Min(items) => items
                .iter()
                .map(|item| item.eval(ctx, result, trait_))
                .min()
                .unwrap_or(0),
        }
    }

    /// Compile an expression, reporting the path on failure.
    fn compile(value: &Json, path: &str) -> Result<Expr, DslError> {
        match value {
            Json::Num(number) => Ok(Expr::Const(*number as i32)),
            Json::Str(text) => match text.as_str() {
                "$rv" => Ok(Expr::Rv),
                "$half_rv_up" => Ok(Expr::HalfRvUp),
                "$half_rv_down" => Ok(Expr::HalfRvDown),
                "$tenths_up_rv" => Ok(Expr::TenthsUpRv),
                "$distance" => Ok(Expr::Distance),
                "$result" => Ok(Expr::Result),
                "$absorbed" => Ok(Expr::Absorbed),
                "$charged" => Ok(Expr::Charged),
                "$attack_rv" => Ok(Expr::AttackRv),
                "$attack_damage" => Ok(Expr::AttackDamage),
                "$speed_tiles" => Ok(Expr::SpeedTiles(Box::new(Expr::Rv))),
                "$swim_tiles" => Ok(Expr::SwimTiles(Box::new(Expr::Rv))),
                "$fast_attacks" => Ok(Expr::FastAttacks(Box::new(Expr::Rv))),
                other => Err(DslError::new(
                    path,
                    "unknown_expression",
                    &[("value", other)],
                )),
            },
            Json::Obj(_) => {
                for (key, make) in [
                    ("tenths_up", Expr::TenthsUp as fn(Box<Expr>) -> Expr),
                    ("speed_tiles", Expr::SpeedTiles as fn(Box<Expr>) -> Expr),
                    ("swim_tiles", Expr::SwimTiles as fn(Box<Expr>) -> Expr),
                    ("fast_attacks", Expr::FastAttacks as fn(Box<Expr>) -> Expr),
                ] {
                    if let Some(inner) = value.get(key) {
                        return Ok(make(Box::new(Expr::compile(
                            inner,
                            &format!("{path}/{key}"),
                        )?)));
                    }
                }
                if let Some(id) = value.get("power_rv").and_then(Json::as_str) {
                    return Ok(Expr::OwnerPowerRv(id.to_string()));
                }
                if let Some(field) = value.get("stored_param") {
                    let who = field
                        .get("who")
                        .and_then(Json::as_str)
                        .and_then(Who::from_id)
                        .ok_or_else(|| DslError::new(path, "bad_stored_who", &[]))?;
                    let key = field
                        .get("key")
                        .and_then(Json::as_str)
                        .ok_or_else(|| DslError::new(path, "stored_param_needs_a_key", &[]))?;
                    return Ok(Expr::StoredParam(who, key.to_string()));
                }
                if let Some(who) = value.get("stored_hook") {
                    let who = who
                        .as_str()
                        .and_then(Who::from_id)
                        .ok_or_else(|| DslError::new(path, "bad_stored_who", &[]))?;
                    return Ok(Expr::StoredHook(who));
                }
                if let Some(stored) = value.get("stored") {
                    let who = stored
                        .get("who")
                        .and_then(Json::as_str)
                        .and_then(Who::from_id)
                        .ok_or_else(|| DslError::new(path, "bad_stored_who", &[]))?;
                    let trait_ = stored
                        .get("trait")
                        .and_then(Json::as_str)
                        .and_then(Trait::from_id)
                        .ok_or_else(|| DslError::new(path, "unknown_trait", &[]))?;
                    return Ok(Expr::Stored(who, trait_));
                }
                for (key, op) in [
                    ("add", "add"),
                    ("sub", "sub"),
                    ("mul", "mul"),
                    ("div", "div"),
                    ("max", "max"),
                    ("min", "min"),
                ] {
                    let _ = op;
                    if let Some(operands) = value.get(key) {
                        let Json::Arr(items) = operands else {
                            return Err(DslError::new(path, "expression_needs_array", &[]));
                        };
                        let mut compiled = Vec::new();
                        for (index, item) in items.iter().enumerate() {
                            compiled.push(Expr::compile(item, &format!("{path}/{key}/{index}"))?);
                        }
                        return match key {
                            "add" => Ok(Expr::Add(compiled)),
                            "mul" => Ok(Expr::Mul(compiled)),
                            "max" => Ok(Expr::Max(compiled)),
                            "min" => Ok(Expr::Min(compiled)),
                            "sub" | "div" => {
                                if compiled.len() != 2 {
                                    return Err(DslError::new(path, "expression_needs_two", &[]));
                                }
                                let mut iter = compiled.into_iter();
                                let left = Box::new(iter.next().expect("two"));
                                let right = Box::new(iter.next().expect("two"));
                                if key == "sub" {
                                    Ok(Expr::Sub(left, right))
                                } else {
                                    let round = match value.get("round").and_then(Json::as_str) {
                                        Some("up") => Rounding::Up,
                                        _ => Rounding::Down,
                                    };
                                    Ok(Expr::Div(left, right, round))
                                }
                            }
                            _ => unreachable!("the loop's own keys"),
                        };
                    }
                }
                Err(DslError::new(path, "unknown_expression", &[]))
            }
            _ => Err(DslError::new(path, "bad_expression", &[])),
        }
    }
}

/// A condition over the frozen context (`02:02.4`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum Condition {
    /// Every child holds.
    All(Vec<Condition>),
    /// Any child holds.
    Any(Vec<Condition>),
    /// The child does not hold.
    Not(Box<Condition>),
    /// The resolved colour is at least this.
    ColourAtLeast(Colour),
    /// The resolved colour is exactly this.
    ColourIs(Colour),
    /// The attack has this shape.
    Shape(bool, bool),
    /// The hook's trait is this one.
    TraitIs(Trait),
    /// The power's owner is the actor, or the target.
    OwnerIs(Who),
    /// The attack carries this tag.
    HasTag(String),
    /// A power parameter equals this.
    ParamIs(String, String),
    /// The owner carries this power id.
    OwnerHasPower(String),
    /// An entity's stored trait is below an expression.
    StoredBelow(Who, Trait, Expr),
    /// The incoming result is at least an expression.
    ResultAtLeast(Expr),
    /// The incoming result is below an expression.
    ResultBelow(Expr),
    /// The distance is at most an expression.
    DistanceAtMost(Expr),
    /// The absorbed amount is at least an expression.
    AbsorbedAtLeast(Expr),
    /// The attack's Rank Value is at most an expression.
    AttackRvAtMost(Expr),
    /// The power's own Rank Value is at most an expression.
    RvAtMost(Expr),
    /// The power's own Rank Value is at least an expression.
    RvAtLeast(Expr),
    /// The owner carries a power whose parameter equals this power's.
    MatchingParam(String, String),
    /// The hook's trait is the one a parameter names (`4c:815`).
    TraitIsParam(String),
    /// The attack carries the tag a parameter names (`4c:419`, `D25`).
    HasParamTag(String),
}

impl Condition {
    /// Evaluate against a context.
    pub fn eval(&self, ctx: &PowerCtx<'_>, result: i32, trait_: Option<Trait>) -> bool {
        match self {
            Condition::All(items) => items.iter().all(|item| item.eval(ctx, result, trait_)),
            Condition::Any(items) => items.iter().any(|item| item.eval(ctx, result, trait_)),
            Condition::Not(item) => !item.eval(ctx, result, trait_),
            Condition::ColourAtLeast(colour) => ctx.colour >= *colour,
            Condition::ColourIs(colour) => ctx.colour == *colour,
            Condition::Shape(melee, ranged) => ctx.attack.is_some_and(|attack| {
                (attack.shape.is_melee() && *melee) || (attack.shape.is_ranged() && *ranged)
            }),
            Condition::TraitIs(named) => trait_ == Some(*named),
            Condition::OwnerIs(who) => who.resolve(ctx) == ctx.owner,
            Condition::HasTag(tag) => ctx.attack.is_some_and(|attack| attack.has_tag(tag)),
            Condition::ParamIs(key, value) => ctx.power.param(key) == value,
            Condition::OwnerHasPower(id) => ctx
                .character(ctx.owner)
                .is_some_and(|owner| owner.power(id).is_some()),
            Condition::StoredBelow(who, named, limit) => {
                ctx.stored_rv(who.resolve(ctx), *named) < limit.eval(ctx, result, trait_)
            }
            Condition::ResultAtLeast(limit) => result >= limit.eval(ctx, result, trait_),
            Condition::ResultBelow(limit) => result < limit.eval(ctx, result, trait_),
            Condition::DistanceAtMost(limit) => ctx.distance <= limit.eval(ctx, result, trait_),
            Condition::AbsorbedAtLeast(limit) => ctx.charged >= limit.eval(ctx, result, trait_),
            // An attack-shaped read is false when there is no attack: `4c:710`
            // is about a sense *being attacked*, and a bare trait read is not.
            Condition::AttackRvAtMost(limit) => ctx
                .attack
                .is_some_and(|attack| attack.rv <= limit.eval(ctx, result, trait_)),
            Condition::RvAtMost(limit) => ctx.power.rv <= limit.eval(ctx, result, trait_),
            Condition::RvAtLeast(limit) => ctx.power.rv >= limit.eval(ctx, result, trait_),
            Condition::MatchingParam(power, key) => ctx
                .owner_power(power)
                .is_some_and(|other| other.param(key) == ctx.power.param(key)),
            Condition::TraitIsParam(key) => {
                Trait::from_id(ctx.power.param(key)).is_some_and(|named| Some(named) == trait_)
            }
            Condition::HasParamTag(key) => {
                let tag = ctx.power.param(key);
                !tag.is_empty() && ctx.attack.is_some_and(|attack| attack.has_tag(tag))
            }
        }
    }

    /// Compile a condition, reporting the path on failure.
    fn compile(value: &Json, path: &str) -> Result<Vec<Condition>, DslError> {
        let Json::Obj(fields) = value else {
            return Err(DslError::new(path, "bad_condition", &[]));
        };
        let mut out = Vec::new();
        for (key, argument) in fields {
            let at = format!("{path}/{key}");
            match key.as_str() {
                "all" | "any" => {
                    let Json::Arr(items) = argument else {
                        return Err(DslError::new(&at, "condition_needs_array", &[]));
                    };
                    let mut compiled = Vec::new();
                    for (index, item) in items.iter().enumerate() {
                        compiled.extend(Condition::compile(item, &format!("{at}/{index}"))?);
                    }
                    out.push(if key == "all" {
                        Condition::All(compiled)
                    } else {
                        Condition::Any(compiled)
                    });
                }
                "not" => {
                    let compiled = Condition::compile(argument, &at)?;
                    out.push(Condition::Not(Box::new(Condition::All(compiled))));
                }
                "colour_at_least" => out.push(Condition::ColourAtLeast(colour_of(argument, &at)?)),
                "colour_is" => out.push(Condition::ColourIs(colour_of(argument, &at)?)),
                "shape" => {
                    let shape = argument
                        .as_str()
                        .ok_or_else(|| DslError::new(&at, "shape_needs_a_name", &[]))?;
                    out.push(match shape {
                        "melee" | "melee_bash" | "melee_slash" => Condition::Shape(true, false),
                        "ranged" | "power" => Condition::Shape(false, true),
                        "attack" => Condition::Shape(true, true),
                        _ => return Err(DslError::new(&at, "unknown_shape", &[("shape", shape)])),
                    });
                }
                "trait" => {
                    let trait_ = argument
                        .as_str()
                        .and_then(Trait::from_id)
                        .ok_or_else(|| DslError::new(&at, "unknown_trait", &[]))?;
                    out.push(Condition::TraitIs(trait_));
                }
                "owner_is" => {
                    let who = argument
                        .as_str()
                        .and_then(Who::from_id)
                        .ok_or_else(|| DslError::new(&at, "bad_owner", &[]))?;
                    out.push(Condition::OwnerIs(who));
                }
                "has_tag" => out.push(Condition::HasTag(
                    argument
                        .as_str()
                        .ok_or_else(|| DslError::new(&at, "tag_needs_a_name", &[]))?
                        .to_string(),
                )),
                "param_is" => {
                    let key = argument
                        .get("key")
                        .and_then(Json::as_str)
                        .ok_or_else(|| DslError::new(&at, "param_needs_a_key", &[]))?;
                    let expected = argument
                        .get("value")
                        .and_then(Json::as_str)
                        .ok_or_else(|| DslError::new(&at, "param_needs_a_value", &[]))?;
                    out.push(Condition::ParamIs(key.to_string(), expected.to_string()));
                }
                "owner_has_power" => out.push(Condition::OwnerHasPower(
                    argument
                        .as_str()
                        .ok_or_else(|| DslError::new(&at, "power_needs_an_id", &[]))?
                        .to_string(),
                )),
                "stored_below" => {
                    let who = argument
                        .get("who")
                        .and_then(Json::as_str)
                        .and_then(Who::from_id)
                        .ok_or_else(|| DslError::new(&at, "bad_stored_who", &[]))?;
                    let trait_ = argument
                        .get("trait")
                        .and_then(Json::as_str)
                        .and_then(Trait::from_id)
                        .ok_or_else(|| DslError::new(&at, "unknown_trait", &[]))?;
                    let limit = Expr::compile(
                        argument
                            .get("value")
                            .ok_or_else(|| DslError::new(&at, "stored_below_needs_a_value", &[]))?,
                        &format!("{at}/value"),
                    )?;
                    out.push(Condition::StoredBelow(who, trait_, limit));
                }
                "result_at_least" => {
                    out.push(Condition::ResultAtLeast(Expr::compile(argument, &at)?))
                }
                "result_below" => out.push(Condition::ResultBelow(Expr::compile(argument, &at)?)),
                "distance_at_most" => {
                    out.push(Condition::DistanceAtMost(Expr::compile(argument, &at)?))
                }
                "absorbed_at_least" => {
                    out.push(Condition::AbsorbedAtLeast(Expr::compile(argument, &at)?))
                }
                "attack_rv_at_most" => {
                    out.push(Condition::AttackRvAtMost(Expr::compile(argument, &at)?))
                }
                "rv_at_most" => out.push(Condition::RvAtMost(Expr::compile(argument, &at)?)),
                "rv_at_least" => out.push(Condition::RvAtLeast(Expr::compile(argument, &at)?)),
                "matching_param" => {
                    let power = argument
                        .get("power")
                        .and_then(Json::as_str)
                        .ok_or_else(|| DslError::new(&at, "matching_param_needs_a_power", &[]))?;
                    let key = argument
                        .get("key")
                        .and_then(Json::as_str)
                        .ok_or_else(|| DslError::new(&at, "matching_param_needs_a_key", &[]))?;
                    out.push(Condition::MatchingParam(power.to_string(), key.to_string()));
                }
                "trait_is_param" => out.push(Condition::TraitIsParam(
                    argument
                        .as_str()
                        .ok_or_else(|| DslError::new(&at, "trait_is_param_needs_a_key", &[]))?
                        .to_string(),
                )),
                "has_param_tag" => out.push(Condition::HasParamTag(
                    argument
                        .as_str()
                        .ok_or_else(|| DslError::new(&at, "has_param_tag_needs_a_key", &[]))?
                        .to_string(),
                )),
                _ => return Err(DslError::new(&at, "unknown_condition", &[("key", key)])),
            }
        }
        Ok(out)
    }
}

/// A duration an effect carries: a declared dice expression, or an expression
/// evaluated against the context.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum AmountSpec {
    /// A literal or dice expression (`Amount`).
    Dice(Amount),
    /// An integer expression, applied as a constant.
    Expr(Expr),
}

impl AmountSpec {
    /// Resolve to the `Amount` the resolver draws.
    pub fn resolve(&self, ctx: &PowerCtx<'_>, result: i32, trait_: Option<Trait>) -> Amount {
        match self {
            AmountSpec::Dice(amount) => *amount,
            AmountSpec::Expr(expr) => Amount::constant(expr.eval(ctx, result, trait_)),
        }
    }

    /// Compile an `amount`/`panels` field.
    fn compile(value: &Json, path: &str) -> Result<AmountSpec, DslError> {
        // An object with a `dice` key is a dice expression; anything else is an
        // integer expression. That keeps the two vocabularies disjoint, so a
        // typo cannot silently mean the other one.
        if value.get("dice").is_some() || value.get("const").is_some() {
            return Amount::parse(value)
                .map(AmountSpec::Dice)
                .map_err(|message| DslError::new(path, "bad_dice", &[("message", message)]));
        }
        if matches!(value, Json::Num(_)) || matches!(value, Json::Str(_)) {
            // A bare number is an integer amount; a bare string is a dice
            // expression, which is how the source prints durations (`"1d10"`).
            if let Json::Num(number) = value {
                return Ok(AmountSpec::Dice(Amount::constant(*number as i32)));
            }
        }
        Expr::compile(value, path).map(AmountSpec::Expr)
    }
}

/// A compiled effect (`02:02.4`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum Effect {
    /// Replace the hook's return value.
    SetResult(Expr),
    /// Add to the hook's return value.
    AddResult(Expr),
    /// Amend an attack.
    Attack(AttackSpec),
    /// Declare movement.
    Move(MoveSpec),
    /// Declare an action economy.
    Economy(EconomySpec),
    /// Acquire-time writes.
    Acquire(AcquireEffect),
    /// A queued write.
    Emit(EmitSpec),
    /// The power spends the panel's action (`4c:718`).
    ConsumesAction,
}

/// The attack fields a template may set (`02:02.4`).
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct AttackSpec {
    /// A trait substitution.
    pub trait_: Option<Trait>,
    /// A Rank Value substitution.
    pub rv: Option<Expr>,
    /// A damage value set outright.
    pub damage: Option<Expr>,
    /// A hand-count substitution.
    pub hands: Option<Hands>,
    /// A reach substitution.
    pub reach: Option<Expr>,
    /// Row steps.
    pub steps: Option<Expr>,
    /// An armour delta.
    pub armour: Option<Expr>,
    /// Whether the attack pierces armour.
    pub piercing: Option<bool>,
    /// A success cap.
    pub cap: Option<Colour>,
    /// Extra attacks.
    pub extra_attacks: Option<Expr>,
    /// Whether the roll keeps two of three dice.
    pub keep_two_of_three: Option<bool>,
}

/// The movement a template declares.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct MoveSpec {
    /// The mode.
    pub mode: MoveMode,
    /// Tiles per panel, when it replaces the ordinary allowance.
    pub tiles: Option<Expr>,
    /// The material rank the mode passes through.
    pub max_material: Option<Expr>,
}

/// The action economy a template declares.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct EconomySpec {
    /// Attacks per panel.
    pub attacks: Expr,
    /// Bonus movement tiles.
    pub bonus_tiles: Expr,
}

/// An acquisition-time write.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum AcquireEffect {
    /// A flat bonus to a stored trait.
    Trait(Trait, Expr),
    /// A bonus to Lifestyle.
    Lifestyle(Expr),
    /// A bonus to Repute.
    Repute(Expr),
    /// Extra skills.
    BonusSkills(Expr),
    /// A skill step override.
    SkillSteps(Expr),
    /// Fortune granted.
    Fortune(Expr),
    /// A power granted outright.
    GrantPower(String, Expr),
    /// A catalogue the campaign must author.
    ContentRequest(String),
    /// A creation-log line.
    Note(String),
}

/// A trait a template names, either fixed or taken from a parameter.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum TraitSpec {
    /// A fixed trait.
    Fixed(Trait),
    /// The trait a power parameter names (`4c:815`).
    Param(String),
}

impl TraitSpec {
    /// Resolve against a context, or `None` when the parameter is unknown.
    pub fn resolve(&self, ctx: &PowerCtx<'_>) -> Option<Trait> {
        match self {
            TraitSpec::Fixed(trait_) => Some(*trait_),
            TraitSpec::Param(key) => Trait::from_id(ctx.power.param(key)),
        }
    }

    /// Compile a `trait` field, which is either a name or `{"param": "..."}`.
    fn compile(value: &Json, path: &str) -> Result<TraitSpec, DslError> {
        if let Some(key) = value.get("param").and_then(Json::as_str) {
            return Ok(TraitSpec::Param(key.to_string()));
        }
        value
            .as_str()
            .and_then(Trait::from_id)
            .map(TraitSpec::Fixed)
            .ok_or_else(|| DslError::new(path, "unknown_trait", &[]))
    }
}

/// A queued write, with its expressions still unevaluated.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum EmitSpec {
    /// Damage with a tag.
    Damage(EffectTarget, AmountSpec, Option<String>),
    /// Healing.
    Heal(EffectTarget, AmountSpec),
    /// A damage delta.
    ModifyDamage(Expr),
    /// An armour delta.
    ModifyArmour(Expr),
    /// A row-step shift.
    ModifyTraitSteps(EffectTarget, Trait, Expr),
    /// A Rank Value boost.
    BoostTrait(TraitSpec, Expr, AmountSpec),
    /// A colour shift.
    ModifyColour(Expr),
    /// A condition.
    ApplyCondition(EffectTarget, ConditionKind, AmountSpec),
    /// Ending a condition.
    RemoveCondition(EffectTarget, ConditionKind),
    /// Fortune.
    GrantFortune(EffectTarget, AmountSpec),
    /// A stored value.
    SetCharged(AmountSpec),
    /// A binary resistance rider.
    Resist {
        /// The resisting trait.
        trait_: Trait,
        /// A Rank Value to roll at instead.
        rv: Option<Expr>,
        /// The highest colour that still fails.
        on: Colour,
        /// What happens on a failure.
        then: Vec<EmitSpec>,
    },
    /// A push back.
    Displace(EffectTarget, Expr),
    /// A knockdown.
    Knockdown(EffectTarget),
    /// A knockout.
    Knockout(EffectTarget, AmountSpec),
    /// A teleport.
    Teleport(EffectTarget, Expr),
    /// A nullification attempt.
    Nullify(EffectTarget, Expr),
    /// A reflection attempt.
    Reflect(EffectTarget, Expr),
    /// Regeneration.
    Regenerate(AmountSpec),
}

impl EmitSpec {
    /// Resolve to the effect the encounter applies, or `None` when a
    /// parameter the effect depends on is missing.
    pub fn resolve(
        &self,
        ctx: &PowerCtx<'_>,
        result: i32,
        trait_: Option<Trait>,
    ) -> Option<HookEffect> {
        Some(match self {
            EmitSpec::Damage(target, amount, tag) => HookEffect::Damage {
                target: *target,
                amount: amount.resolve(ctx, result, trait_),
                tag: tag.clone(),
            },
            EmitSpec::Heal(target, amount) => HookEffect::Heal {
                target: *target,
                amount: amount.resolve(ctx, result, trait_),
            },
            EmitSpec::ModifyDamage(expr) => {
                HookEffect::ModifyDamage(expr.eval(ctx, result, trait_))
            }
            EmitSpec::ModifyArmour(expr) => {
                HookEffect::ModifyArmour(expr.eval(ctx, result, trait_))
            }
            EmitSpec::ModifyTraitSteps(target, step_trait, expr) => HookEffect::ModifyTraitSteps {
                target: *target,
                trait_: *step_trait,
                steps: expr.eval(ctx, result, trait_),
            },
            EmitSpec::BoostTrait(boost_trait, expr, panels) => HookEffect::BoostTrait {
                trait_: boost_trait.resolve(ctx)?,
                rv: expr.eval(ctx, result, trait_),
                panels: panels.resolve(ctx, result, trait_),
            },
            EmitSpec::ModifyColour(expr) => {
                HookEffect::ModifyColour(expr.eval(ctx, result, trait_))
            }
            EmitSpec::ApplyCondition(target, kind, panels) => HookEffect::ApplyCondition {
                target: *target,
                kind: *kind,
                panels: panels.resolve(ctx, result, trait_),
            },
            EmitSpec::RemoveCondition(target, kind) => HookEffect::RemoveCondition {
                target: *target,
                kind: *kind,
            },
            EmitSpec::GrantFortune(target, amount) => HookEffect::GrantFortune {
                target: *target,
                amount: amount.resolve(ctx, result, trait_),
            },
            EmitSpec::SetCharged(amount) => {
                HookEffect::SetCharged(amount.resolve(ctx, result, trait_))
            }
            EmitSpec::Resist {
                trait_: resist_trait,
                rv,
                on,
                then,
            } => HookEffect::Resist {
                trait_: *resist_trait,
                rv: rv.as_ref().map(|expr| expr.eval(ctx, result, trait_)),
                on: *on,
                then: then
                    .iter()
                    .filter_map(|item| item.resolve(ctx, result, trait_))
                    .collect(),
            },
            EmitSpec::Displace(target, expr) => HookEffect::Displace {
                target: *target,
                tiles: expr.eval(ctx, result, trait_),
            },
            EmitSpec::Knockdown(target) => HookEffect::Knockdown { target: *target },
            EmitSpec::Knockout(target, panels) => HookEffect::Knockout {
                target: *target,
                panels: panels.resolve(ctx, result, trait_),
            },
            EmitSpec::Teleport(target, expr) => HookEffect::Teleport {
                target: *target,
                tiles: expr.eval(ctx, result, trait_),
            },
            EmitSpec::Nullify(target, expr) => HookEffect::Nullify {
                target: *target,
                rv: expr.eval(ctx, result, trait_),
            },
            EmitSpec::Reflect(target, expr) => HookEffect::Reflect {
                target: *target,
                rv: expr.eval(ctx, result, trait_),
            },
            EmitSpec::Regenerate(amount) => HookEffect::Regenerate {
                amount: amount.resolve(ctx, result, trait_),
            },
        })
    }
}

/// One trigger.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Trigger {
    /// The hook it fires on.
    pub on: Hook,
    /// The conditions that must all hold.
    pub when: Vec<Condition>,
    /// The effects it applies.
    pub then: Vec<Effect>,
}

/// A power compiled from the effect DSL (`AD-16`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Template {
    id: String,
    triggers: Vec<Trigger>,
}

impl Template {
    /// The power id this template implements.
    ///
    /// Named `power_id` rather than `id` so it cannot shadow the
    /// [`PowerKernel::id`] method every kernel implements.
    pub fn power_id(&self) -> &str {
        &self.id
    }

    /// Compile a power record: `{ "id": …, "parameters": […], "triggers": [ … ] }`.
    ///
    /// Every error is reported, not just the first, so the editor can show a
    /// complete list (`03:03.9`).
    pub fn compile(id: &str, data: &Json) -> Result<Template, Vec<DslError>> {
        let mut errors = Vec::new();
        let mut triggers = Vec::new();
        match data.get("triggers") {
            Some(Json::Arr(items)) => {
                if items.is_empty() {
                    errors.push(DslError::new("/triggers", "no_triggers", &[]));
                }
                for (index, item) in items.iter().enumerate() {
                    let at = format!("/triggers/{index}");
                    match compile_trigger(item, &at) {
                        Ok(trigger) => triggers.push(trigger),
                        Err(error) => errors.push(error),
                    }
                }
            }
            Some(_) => errors.push(DslError::new("/triggers", "bad_triggers", &[])),
            None => errors.push(DslError::new("/triggers", "no_triggers", &[])),
        }
        if errors.is_empty() {
            Ok(Template {
                id: id.to_string(),
                triggers,
            })
        } else {
            Err(errors)
        }
    }

    /// The triggers firing on a hook, with their conditions satisfied.
    fn firing<'a>(
        &'a self,
        hook: Hook,
        ctx: &PowerCtx<'_>,
        result: i32,
        trait_: Option<Trait>,
    ) -> Vec<&'a Trigger> {
        self.triggers
            .iter()
            .filter(|trigger| trigger.on == hook)
            .filter(|trigger| {
                trigger
                    .when
                    .iter()
                    .all(|item| item.eval(ctx, result, trait_))
            })
            .collect()
    }

    /// Apply the `SetResult`/`AddResult` effects of a value-returning hook.
    fn value(&self, hook: Hook, ctx: &PowerCtx<'_>, incoming: i32, trait_: Option<Trait>) -> i32 {
        let mut result = incoming;
        for trigger in self.firing(hook, ctx, result, trait_) {
            for effect in &trigger.then {
                match effect {
                    Effect::SetResult(expr) => result = expr.eval(ctx, result, trait_),
                    Effect::AddResult(expr) => result += expr.eval(ctx, result, trait_),
                    _ => {}
                }
            }
        }
        result
    }

    /// Collect the queued writes a hook produces.
    fn emitted(&self, hook: Hook, ctx: &PowerCtx<'_>, result: i32) -> Vec<HookEffect> {
        let trait_: Option<Trait> = None;
        let mut out = Vec::new();
        for trigger in self.firing(hook, ctx, result, None) {
            for effect in &trigger.then {
                if let Effect::Emit(spec) = effect {
                    if let Some(resolved) = spec.resolve(ctx, result, trait_) {
                        out.push(resolved);
                    }
                }
            }
        }
        out
    }

    /// The attack spec a `modify_attack` hook applies, if any.
    fn attack(&self, ctx: &PowerCtx<'_>) -> AttackMods {
        let mut mods = AttackMods::default();
        for trigger in self.firing(Hook::ModifyAttack, ctx, 0, None) {
            for effect in &trigger.then {
                if let Effect::Attack(spec) = effect {
                    let each = attack_mods(spec, ctx);
                    if each.trait_.is_some() {
                        mods.trait_ = each.trait_;
                    }
                    if each.rv.is_some() {
                        mods.rv = each.rv;
                    }
                    if each.damage_override.is_some() {
                        mods.damage_override = each.damage_override;
                    }
                    if each.hands.is_some() {
                        mods.hands = each.hands;
                    }
                    if each.reach.is_some() {
                        mods.reach = each.reach;
                    }
                    mods.steps += each.steps;
                    mods.armour += each.armour;
                    mods.piercing |= each.piercing;
                    if each.colour_cap.is_some() {
                        mods.colour_cap = each.colour_cap;
                    }
                    mods.extra_attacks += each.extra_attacks;
                    mods.keep_two_of_three |= each.keep_two_of_three;
                }
            }
        }
        mods
    }

    /// The acquisition plan the `acquire` hook produces.
    fn acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        for trigger in self.firing(Hook::Acquire, ctx, 0, None) {
            for effect in &trigger.then {
                let Effect::Acquire(spec) = effect else {
                    continue;
                };
                match spec {
                    AcquireEffect::Trait(trait_, expr) => {
                        plan.add_trait(*trait_, expr.eval(ctx, 0, None));
                    }
                    AcquireEffect::Lifestyle(expr) => {
                        plan.lifestyle_bonus += expr.eval(ctx, 0, None)
                    }
                    AcquireEffect::Repute(expr) => plan.repute_bonus += expr.eval(ctx, 0, None),
                    AcquireEffect::BonusSkills(expr) => {
                        plan.bonus_skills += expr.eval(ctx, 0, None)
                    }
                    AcquireEffect::SkillSteps(expr) => {
                        plan.skill_step_override = Some(expr.eval(ctx, 0, None));
                    }
                    AcquireEffect::Fortune(expr) => plan.grant_fortune += expr.eval(ctx, 0, None),
                    AcquireEffect::GrantPower(id, rv) => plan
                        .grants_powers
                        .push(PowerInst::new(id, rv.eval(ctx, 0, None))),
                    AcquireEffect::ContentRequest(name) => plan.content_requests.push(name.clone()),
                    AcquireEffect::Note(line) => plan.note(line),
                }
            }
        }
    }
}

impl PowerKernel for Template {
    fn id(&self) -> &'static str {
        // The trait's signature is `&'static str`, but a template's id is owned.
        // The encounter never uses this method for a template — it looks kernels
        // up by id — so it returns a stable placeholder rather than leaking.
        "<template>"
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        self.acquire(ctx, plan);
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        self.value(Hook::ModifyTrait, ctx, value, Some(trait_))
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        let mut out = mods;
        let templated = self.attack(ctx);
        if templated.trait_.is_some() {
            out.trait_ = templated.trait_;
        }
        if templated.rv.is_some() {
            out.rv = templated.rv;
        }
        if templated.damage_override.is_some() {
            out.damage_override = templated.damage_override;
        }
        if templated.hands.is_some() {
            out.hands = templated.hands;
        }
        if templated.reach.is_some() {
            out.reach = templated.reach;
        }
        out.steps += templated.steps;
        out.armour += templated.armour;
        out.piercing |= templated.piercing;
        if templated.colour_cap.is_some() {
            out.colour_cap = templated.colour_cap;
        }
        out.extra_attacks += templated.extra_attacks;
        out.keep_two_of_three |= templated.keep_two_of_three;
        out
    }

    fn modify_damage(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        self.value(Hook::ModifyDamage, ctx, value, None)
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        self.value(Hook::ModifyArmor, ctx, value, None)
    }

    fn movement(&self, ctx: &PowerCtx<'_>, _who: u32) -> Option<MoveProfile> {
        for trigger in self.firing(Hook::Movement, ctx, 0, None) {
            for effect in &trigger.then {
                if let Effect::Move(spec) = effect {
                    return Some(MoveProfile {
                        mode: spec.mode,
                        tiles: spec.tiles.as_ref().map(|expr| expr.eval(ctx, 0, None)),
                        max_material: spec
                            .max_material
                            .as_ref()
                            .map(|expr| expr.eval(ctx, 0, None)),
                    });
                }
            }
        }
        None
    }

    fn action_economy(&self, ctx: &PowerCtx<'_>, _who: u32) -> Option<ActionEconomy> {
        for trigger in self.firing(Hook::ActionEconomy, ctx, 0, None) {
            for effect in &trigger.then {
                if let Effect::Economy(spec) = effect {
                    return Some(ActionEconomy {
                        attacks_per_panel: spec.attacks.eval(ctx, 0, None),
                        bonus_tiles: spec.bonus_tiles.eval(ctx, 0, None),
                    });
                }
            }
        }
        None
    }

    fn on_hit(&self, ctx: &PowerCtx<'_>, out: &mut Vec<HookEffect>) {
        out.extend(self.emitted(Hook::Hit, ctx, 0));
    }

    fn on_resist(&self, ctx: &PowerCtx<'_>, colour: &mut Colour) {
        // A template's resistance hook shifts the colour by the `set_result`
        // convention: `Blck` means "cannot resist".
        let result = self.value(Hook::Resist, ctx, colour.code() as i32, None);
        if result != colour.code() as i32 {
            *colour = Colour::from_code(result.clamp(0, 3) as u8).unwrap_or(*colour);
        }
    }

    fn on_own_turn(&self, ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        for trigger in self.firing(Hook::OwnTurn, ctx, 0, None) {
            for effect in &trigger.then {
                if matches!(effect, Effect::ConsumesAction) {
                    plan.consumes_action = true;
                }
            }
        }
        plan.effects.extend(self.emitted(Hook::OwnTurn, ctx, 0));
    }

    fn on_absorb(&self, ctx: &PowerCtx<'_>, absorbed: i32, out: &mut Vec<HookEffect>) {
        out.extend(self.emitted(Hook::Absorb, ctx, absorbed));
    }

    fn on_power_used(&self, ctx: &PowerCtx<'_>, _power: &PowerInst, out: &mut Vec<HookEffect>) {
        out.extend(self.emitted(Hook::PowerUsed, ctx, 0));
    }
}

/// Compile one effect document, as the script host hands one back (`03:03.7`).
///
/// The two tiers share this compiler on purpose: a script cannot name an effect
/// a data mod cannot (`AD:16`).
pub fn resolve_effect(value: &Json, path: &str) -> Result<Vec<EmitSpec>, DslError> {
    match compile_effect(value, path)? {
        Effect::Emit(spec) => Ok(vec![spec]),
        _ => Err(DslError::new(path, "not_a_queued_write", &[])),
    }
}

/// Resolve an attack document into modifiers, as a script or a template sets
/// them (`02:02.4`).
pub fn resolve_attack_spec(
    value: &Json,
    path: &str,
    ctx: &PowerCtx<'_>,
) -> Result<Option<AttackMods>, DslError> {
    let spec = compile_attack(value, path)?;
    Ok(Some(attack_mods(&spec, ctx)))
}

/// The modifiers an [`AttackSpec`] sets, evaluated against a context.
pub fn attack_mods(spec: &AttackSpec, ctx: &PowerCtx<'_>) -> AttackMods {
    let mut mods = AttackMods::default();
    if let Some(trait_) = spec.trait_ {
        mods.trait_ = Some(trait_);
    }
    if let Some(expr) = &spec.rv {
        mods.rv = Some(expr.eval(ctx, 0, None));
    }
    if let Some(expr) = &spec.damage {
        mods.damage_override = Some(expr.eval(ctx, 0, None));
    }
    if let Some(hands) = spec.hands {
        mods.hands = Some(hands);
    }
    if let Some(expr) = &spec.reach {
        mods.reach = Some(expr.eval(ctx, 0, None));
    }
    if let Some(expr) = &spec.steps {
        mods.steps += expr.eval(ctx, 0, None);
    }
    if let Some(expr) = &spec.armour {
        mods.armour += expr.eval(ctx, 0, None);
    }
    if let Some(piercing) = spec.piercing {
        mods.piercing |= piercing;
    }
    if let Some(cap) = spec.cap {
        mods.colour_cap = Some(cap);
    }
    if let Some(expr) = &spec.extra_attacks {
        mods.extra_attacks += expr.eval(ctx, 0, None);
    }
    if let Some(keep) = spec.keep_two_of_three {
        mods.keep_two_of_three |= keep;
    }
    mods
}

/// Compile one trigger.
fn compile_trigger(value: &Json, path: &str) -> Result<Trigger, DslError> {
    let on = value
        .get("on")
        .and_then(Json::as_str)
        .and_then(Hook::from_id)
        .ok_or_else(|| DslError::new(&format!("{path}/on"), "unknown_hook", &[]))?;
    let when = match value.get("when") {
        None => Vec::new(),
        Some(condition) => Condition::compile(condition, &format!("{path}/when"))?,
    };
    let then = match value.get("then") {
        Some(Json::Arr(items)) => {
            let mut out = Vec::new();
            for (index, item) in items.iter().enumerate() {
                out.push(compile_effect(item, &format!("{path}/then/{index}"))?);
            }
            out
        }
        _ => return Err(DslError::new(&format!("{path}/then"), "bad_then", &[])),
    };
    Ok(Trigger { on, when, then })
}

/// Compile one effect.
fn compile_effect(value: &Json, path: &str) -> Result<Effect, DslError> {
    let Json::Obj(fields) = value else {
        return Err(DslError::new(path, "bad_effect", &[]));
    };
    if fields.len() != 1 {
        return Err(DslError::new(path, "effect_needs_one_key", &[]));
    }
    let (key, argument) = &fields[0];
    let at = format!("{path}/{key}");
    match key.as_str() {
        "set_result" => Ok(Effect::SetResult(Expr::compile(argument, &at)?)),
        "add_result" => Ok(Effect::AddResult(Expr::compile(argument, &at)?)),
        "attack" => Ok(Effect::Attack(compile_attack(argument, &at)?)),
        "move" => Ok(Effect::Move(compile_move(argument, &at)?)),
        "economy" => Ok(Effect::Economy(EconomySpec {
            attacks: Expr::compile(
                argument
                    .get("attacks")
                    .ok_or_else(|| DslError::new(&at, "economy_needs_attacks", &[]))?,
                &format!("{at}/attacks"),
            )?,
            bonus_tiles: Expr::compile(
                argument.get("bonus_tiles").unwrap_or(&Json::Num(0)),
                &format!("{at}/bonus_tiles"),
            )?,
        })),
        "acquire" => Ok(Effect::Acquire(compile_acquire(argument, &at)?)),
        "consumes_action" => Ok(Effect::ConsumesAction),
        _ => Ok(Effect::Emit(compile_emit(key, argument, &at)?)),
    }
}

/// Compile the `acquire` effect's fields.
fn compile_acquire(value: &Json, path: &str) -> Result<AcquireEffect, DslError> {
    let Json::Obj(fields) = value else {
        return Err(DslError::new(path, "acquire_needs_an_object", &[]));
    };
    if fields.len() != 1 {
        return Err(DslError::new(path, "acquire_needs_one_key", &[]));
    }
    let (key, argument) = &fields[0];
    let at = format!("{path}/{key}");
    Ok(match key.as_str() {
        "add_trait" => {
            let trait_ = argument
                .get("trait")
                .and_then(Json::as_str)
                .and_then(Trait::from_id)
                .ok_or_else(|| DslError::new(&at, "unknown_trait", &[]))?;
            AcquireEffect::Trait(
                trait_,
                Expr::compile(
                    argument
                        .get("value")
                        .ok_or_else(|| DslError::new(&at, "add_trait_needs_a_value", &[]))?,
                    &format!("{at}/value"),
                )?,
            )
        }
        "lifestyle" => AcquireEffect::Lifestyle(Expr::compile(argument, &at)?),
        "repute" => AcquireEffect::Repute(Expr::compile(argument, &at)?),
        "bonus_skills" => AcquireEffect::BonusSkills(Expr::compile(argument, &at)?),
        "skill_steps" => AcquireEffect::SkillSteps(Expr::compile(argument, &at)?),
        "fortune" => AcquireEffect::Fortune(Expr::compile(argument, &at)?),
        "grant_power" => {
            let id = argument
                .get("id")
                .and_then(Json::as_str)
                .ok_or_else(|| DslError::new(&at, "grant_power_needs_an_id", &[]))?;
            AcquireEffect::GrantPower(
                id.to_string(),
                Expr::compile(
                    argument.get("rv").unwrap_or(&Json::Num(10)),
                    &format!("{at}/rv"),
                )?,
            )
        }
        "content_request" => AcquireEffect::ContentRequest(
            argument
                .as_str()
                .ok_or_else(|| DslError::new(&at, "content_request_needs_a_name", &[]))?
                .to_string(),
        ),
        "note" => AcquireEffect::Note(
            argument
                .as_str()
                .ok_or_else(|| DslError::new(&at, "note_needs_text", &[]))?
                .to_string(),
        ),
        _ => {
            return Err(DslError::new(
                &at,
                "unknown_acquire_effect",
                &[("key", key)],
            ))
        }
    })
}

/// Compile the `attack` effect.
fn compile_attack(value: &Json, path: &str) -> Result<AttackSpec, DslError> {
    let mut spec = AttackSpec::default();
    let Some(Json::Obj(fields)) = Some(value) else {
        unreachable!("the caller passes an object")
    };
    for (key, argument) in fields {
        let at = format!("{path}/{key}");
        match key.as_str() {
            "trait" => {
                spec.trait_ = Some(
                    argument
                        .as_str()
                        .and_then(Trait::from_id)
                        .ok_or_else(|| DslError::new(&at, "unknown_trait", &[]))?,
                )
            }
            "rv" => spec.rv = Some(Expr::compile(argument, &at)?),
            "damage" => spec.damage = Some(Expr::compile(argument, &at)?),
            "hands" => {
                spec.hands = Some(match argument.as_str() {
                    Some("none") => Hands::None,
                    Some("one") => Hands::One,
                    Some("two") => Hands::Two,
                    Some("ranged") => Hands::Ranged,
                    _ => return Err(DslError::new(&at, "unknown_hands", &[])),
                })
            }
            "reach" => spec.reach = Some(Expr::compile(argument, &at)?),
            "steps" => spec.steps = Some(Expr::compile(argument, &at)?),
            "armour" | "armor" => spec.armour = Some(Expr::compile(argument, &at)?),
            "piercing" => {
                spec.piercing = Some(
                    argument
                        .as_bool()
                        .ok_or_else(|| DslError::new(&at, "piercing_needs_a_bool", &[]))?,
                )
            }
            "cap" => spec.cap = Some(colour_of(argument, &at)?),
            "extra_attacks" => spec.extra_attacks = Some(Expr::compile(argument, &at)?),
            "keep_two_of_three" => {
                spec.keep_two_of_three = Some(
                    argument
                        .as_bool()
                        .ok_or_else(|| DslError::new(&at, "keep_needs_a_bool", &[]))?,
                )
            }
            _ => return Err(DslError::new(&at, "unknown_attack_field", &[("key", key)])),
        }
    }
    Ok(spec)
}

/// Compile the `move` effect.
fn compile_move(value: &Json, path: &str) -> Result<MoveSpec, DslError> {
    let mode = value
        .get("mode")
        .and_then(Json::as_str)
        .and_then(MoveMode::from_id)
        .ok_or_else(|| DslError::new(&format!("{path}/mode"), "unknown_move_mode", &[]))?;
    let tiles = match value.get("tiles") {
        Some(Json::Null) | None => None,
        Some(expr) => Some(Expr::compile(expr, &format!("{path}/tiles"))?),
    };
    let max_material = match value.get("max_material") {
        Some(Json::Null) | None => None,
        Some(expr) => Some(Expr::compile(expr, &format!("{path}/max_material"))?),
    };
    Ok(MoveSpec {
        mode,
        tiles,
        max_material,
    })
}

/// Compile one queued write, dispatched by its key.
fn compile_emit(key: &str, argument: &Json, path: &str) -> Result<EmitSpec, DslError> {
    let target = |value: &Json, at: &str| -> Result<EffectTarget, DslError> {
        match value.get("target").and_then(Json::as_str) {
            None => Ok(EffectTarget::Target),
            Some(name) => {
                EffectTarget::from_id(name).ok_or_else(|| DslError::new(at, "bad_target", &[]))
            }
        }
    };
    Ok(match key {
        "damage" => EmitSpec::Damage(
            target(argument, path)?,
            AmountSpec::compile(
                argument
                    .get("amount")
                    .ok_or_else(|| DslError::new(path, "damage_needs_an_amount", &[]))?,
                &format!("{path}/amount"),
            )?,
            argument
                .get("tag")
                .and_then(Json::as_str)
                .map(str::to_string),
        ),
        "heal" => EmitSpec::Heal(
            target(argument, path)?,
            AmountSpec::compile(
                argument
                    .get("amount")
                    .ok_or_else(|| DslError::new(path, "heal_needs_an_amount", &[]))?,
                &format!("{path}/amount"),
            )?,
        ),
        "modify_damage" => EmitSpec::ModifyDamage(Expr::compile(argument, path)?),
        "modify_armour" | "modify_armor" => EmitSpec::ModifyArmour(Expr::compile(argument, path)?),
        "modify_trait_steps" => EmitSpec::ModifyTraitSteps(
            target(argument, path)?,
            trait_of(argument, path)?,
            Expr::compile(
                argument
                    .get("steps")
                    .ok_or_else(|| DslError::new(path, "steps_needed", &[]))?,
                &format!("{path}/steps"),
            )?,
        ),
        "boost_trait" => EmitSpec::BoostTrait(
            TraitSpec::compile(
                argument
                    .get("trait")
                    .ok_or_else(|| DslError::new(path, "unknown_trait", &[]))?,
                path,
            )?,
            Expr::compile(
                argument
                    .get("rv")
                    .ok_or_else(|| DslError::new(path, "rv_needed", &[]))?,
                &format!("{path}/rv"),
            )?,
            AmountSpec::compile(
                argument.get("panels").unwrap_or(&Json::Num(1)),
                &format!("{path}/panels"),
            )?,
        ),
        "modify_colour" => EmitSpec::ModifyColour(Expr::compile(argument, path)?),
        "apply_status" => EmitSpec::ApplyCondition(
            target(argument, path)?,
            condition_of(argument)?,
            AmountSpec::compile(
                argument.get("panels").unwrap_or(&Json::Num(-1)),
                &format!("{path}/panels"),
            )?,
        ),
        "remove_status" => {
            EmitSpec::RemoveCondition(target(argument, path)?, condition_of(argument)?)
        }
        "grant_fortune" => EmitSpec::GrantFortune(
            target(argument, path)?,
            AmountSpec::compile(
                argument
                    .get("amount")
                    .ok_or_else(|| DslError::new(path, "amount_needed", &[]))?,
                &format!("{path}/amount"),
            )?,
        ),
        "set_charged" => EmitSpec::SetCharged(AmountSpec::compile(argument, path)?),
        "resist" => {
            let then = match argument.get("then") {
                Some(Json::Arr(items)) => {
                    let mut out = Vec::new();
                    for (index, item) in items.iter().enumerate() {
                        let Json::Obj(fields) = item else {
                            return Err(DslError::new(path, "bad_effect", &[]));
                        };
                        let (inner_key, inner) = &fields[0];
                        out.push(compile_emit(
                            inner_key,
                            inner,
                            &format!("{path}/then/{index}/{inner_key}"),
                        )?);
                    }
                    out
                }
                _ => return Err(DslError::new(&format!("{path}/then"), "bad_then", &[])),
            };
            EmitSpec::Resist {
                trait_: trait_of(argument, path)?,
                rv: match argument.get("rv") {
                    Some(Json::Null) | None => None,
                    Some(expr) => Some(Expr::compile(expr, &format!("{path}/rv"))?),
                },
                on: colour_of(
                    argument
                        .get("on")
                        .ok_or_else(|| DslError::new(path, "resist_needs_on", &[]))?,
                    &format!("{path}/on"),
                )?,
                then,
            }
        }
        "displace" => EmitSpec::Displace(
            target(argument, path)?,
            Expr::compile(
                argument.get("tiles").ok_or_else(|| {
                    // The code needs its arguments: `tiles_needed` alone names
                    // neither the effect nor the missing field, and a diagnostic
                    // a modder cannot act on is the defect this reports (`AD-15`).
                    DslError::new(
                        path,
                        "tiles_needed",
                        &[("effect", "displace"), ("field", "tiles")],
                    )
                })?,
                &format!("{path}/tiles"),
            )?,
        ),
        "knockdown" => EmitSpec::Knockdown(target(argument, path)?),
        "knockout" => EmitSpec::Knockout(
            target(argument, path)?,
            AmountSpec::compile(
                argument.get("panels").unwrap_or(&Json::Num(1)),
                &format!("{path}/panels"),
            )?,
        ),
        "teleport" => EmitSpec::Teleport(
            target(argument, path)?,
            Expr::compile(
                argument.get("tiles").ok_or_else(|| {
                    DslError::new(
                        path,
                        "tiles_needed",
                        &[("effect", "teleport"), ("field", "tiles")],
                    )
                })?,
                &format!("{path}/tiles"),
            )?,
        ),
        "nullify" => EmitSpec::Nullify(
            target(argument, path)?,
            Expr::compile(
                argument.get("rv").unwrap_or(&Json::string("$rv")),
                &format!("{path}/rv"),
            )?,
        ),
        "reflect" => EmitSpec::Reflect(
            target(argument, path)?,
            Expr::compile(
                argument.get("rv").unwrap_or(&Json::string("$rv")),
                &format!("{path}/rv"),
            )?,
        ),
        "regenerate" => EmitSpec::Regenerate(AmountSpec::compile(
            argument.get("amount").unwrap_or(&Json::string("$rv")),
            &format!("{path}/amount"),
        )?),
        _ => return Err(DslError::new(path, "unknown_effect", &[("key", key)])),
    })
}

/// The trait a `trait` field names.
fn trait_of(value: &Json, path: &str) -> Result<Trait, DslError> {
    value
        .get("trait")
        .and_then(Json::as_str)
        .and_then(Trait::from_id)
        .ok_or_else(|| DslError::new(path, "unknown_trait", &[]))
}

/// The condition an `id` field names.
fn condition_of(value: &Json) -> Result<ConditionKind, DslError> {
    value
        .get("id")
        .and_then(Json::as_str)
        .and_then(ConditionKind::from_id)
        .ok_or_else(|| DslError::new("", "unknown_condition_id", &[]))
}

/// A colour name from content.
fn colour_of(value: &Json, path: &str) -> Result<Colour, DslError> {
    crate::l1::items::colour_from_id(value.as_str().unwrap_or(""))
        .map_err(|_| DslError::new(path, "unknown_colour", &[]))
}

/// The attack shape a name describes, for the DSL's own `shape` condition.
pub fn shape_from_id(id: &str) -> Option<AttackShape> {
    Some(match id {
        "melee_bash" => AttackShape::MeleeBash,
        "melee_slash" => AttackShape::MeleeSlash,
        "ranged" => AttackShape::Ranged,
        "power" => AttackShape::Power,
        "manoeuvre" => AttackShape::Manoeuvre,
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l1::character::Character;
    use crate::l1::powers::PowerCtx;
    use crate::tables;

    fn character(id: u32, traits: [i32; 7]) -> Character {
        Character::authored(id, "test", "", 0, traits, 10, 5, vec![], vec![], vec![])
    }

    fn context<'a>(
        power: &'a PowerInst,
        characters: &'a [Character],
        attack: Option<&'a crate::l1::powers::AttackView>,
    ) -> PowerCtx<'a> {
        PowerCtx {
            ladder: tables::CAMPAIGN_LADDER,
            power,
            characters,
            owner: 1,
            actor: 1,
            target: 2,
            distance: 3,
            colour: Colour::Blue,
            attack,
            charged: 0,
        }
    }

    #[test]
    fn a_template_compiles_and_fires_its_hook() {
        let template = Template::compile(
            "wsp.power.piercing-strike",
            &Json::from_text(
                r#"{"triggers":[{"on":"modify_damage","when":{"all":[{"colour_at_least":"Blue"}]},
                    "then":[{"add_result":{"add":["$rv",5]}}]}]}"#,
            ),
        )
        .expect("compiles");
        let power = PowerInst::new("wsp.power.piercing-strike", 20);
        let characters = [character(1, [10; 7]), character(2, [10; 7])];
        let ctx = context(&power, &characters, None);
        assert_eq!(template.modify_damage(&ctx, 0), 25);
        // A Red roll does not satisfy `colour_at_least: Blue`.
        let mut red = context(&power, &characters, None);
        red.colour = Colour::Red;
        assert_eq!(template.modify_damage(&red, 0), 0);
    }

    #[test]
    fn a_partial_template_is_refused_with_every_error() {
        let errors = Template::compile(
            "wsp.power.broken",
            &Json::from_text(r#"{"triggers":[{"on":"nope","then":[]}]}"#),
        )
        .expect_err("must refuse");
        assert!(errors.iter().any(|error| error.code == "unknown_hook"));
    }

    #[test]
    fn an_unknown_effect_is_reported_with_its_path() {
        let errors = Template::compile(
            "wsp.power.broken",
            &Json::from_text(r#"{"triggers":[{"on":"hit","then":[{"explode":1}]}]}"#),
        )
        .expect_err("must refuse");
        assert_eq!(errors[0].code, "unknown_effect");
        assert_eq!(errors[0].path, "/triggers/0/then/0/explode");
    }

    #[test]
    fn a_missing_effect_tiles_names_the_effect_and_the_field() {
        // A bare `tiles_needed` told a modder that something was missing without
        // saying which effect or which field. Both sites carry the arguments now,
        // so a revert of either fails here.
        for effect in ["teleport", "displace"] {
            let body = format!(
                r#"{{"triggers":[{{"on":"hit","then":[{{"{effect}":{{"target":"self"}}}}]}}]}}"#
            );
            let errors = Template::compile("wsp.power.broken", &Json::from_text(&body))
                .expect_err("a tiles effect without tiles must be refused");
            assert_eq!(errors[0].code, "tiles_needed");
            assert_eq!(errors[0].path, format!("/triggers/0/then/0/{effect}"));
            assert!(
                errors[0]
                    .args
                    .iter()
                    .any(|(key, value)| key == "effect" && value == effect),
                "the effect is named: {:?}",
                errors[0].args
            );
            assert!(
                errors[0]
                    .args
                    .iter()
                    .any(|(key, value)| key == "field" && value == "tiles"),
                "the missing field is named: {:?}",
                errors[0].args
            );
        }
    }
}
