//! The Tier-2 script host (`03:03.7`, `AD-8`).
//!
//! Scripts run **in the engine worker**, alongside the sim, because hooks must be
//! synchronous: the core asks for a modifier during resolution and needs an
//! answer now (`03:03.7`). The core therefore imports one function from JS:
//!
//! ```text
//! kobra_script_hook(hook: u32, ctx_ptr: u32, ctx_len: u32) -> (ptr << 32) | len
//! ```
//!
//! and this module is the Rust half of the contract.
//!
//! # The two rules that make it safe
//!
//! - **The context is a read-only snapshot plus a command queue.** The core hands
//!   the host a frozen JSON view; the host answers with the *same* effect
//!   document a DSL template uses. The core validates and applies it, so a script
//!   cannot tear a resolution in half and cannot ask for an effect that does not
//!   exist (`03:03.7`).
//! - **A script that throws is disabled, not fatal.** The JS side catches, drops
//!   the script for the session and reports it; the core simply sees "no answer"
//!   and continues. A script cannot take down the boot (`03:03.7`).
//!
//! # The determinism contract, applied
//!
//! Scripts are held to the same contract as the core (`02:02.9` rule 5): no
//! wall clock, no locale, no `Math.random`, integers only. The core's part is
//! that it **names** a script that broke a replay: [`report`] carries the script
//! set and every failure, the save records the set's hashes (`AD-8`), and a
//! replay that differs because a script changed says so.
//!
//! # The host is installed, not linked
//!
//! There is no script host unless one is [`install`]ed. That is what lets a
//! host-side test drive the real dispatch and effect path, and it is where the
//! Lua host (AD-39) attaches: a production seam, not a test-only shortcut.

use std::sync::Mutex;

use crate::json::Json;
use crate::l0::Colour;
use crate::l1::powers::{AttackMods, AttackView, HookEffect, PowerCtx};

/// The hooks a script may answer, in the ABI's numeric order (`03:03.7`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ScriptHook {
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

impl ScriptHook {
    /// Every hook, so the ABI and the report can enumerate the domain.
    ///
    /// The set is the kernel hook set **minus `on_acquire` and `modify_trait`**.
    /// Those two change a *plan* or a stored floor rather than queuing a write,
    /// so a script reaches their effect through a queued command instead
    /// (`boost_trait`, `modify_trait_steps`, or an acquisition record). Shipping
    /// a hook the engine never consults would be a promise the ABI does not keep.
    pub const ALL: [ScriptHook; 10] = [
        ScriptHook::ModifyAttack,
        ScriptHook::ModifyDamage,
        ScriptHook::ModifyArmor,
        ScriptHook::Movement,
        ScriptHook::ActionEconomy,
        ScriptHook::Hit,
        ScriptHook::Resist,
        ScriptHook::OwnTurn,
        ScriptHook::Absorb,
        ScriptHook::PowerUsed,
    ];

    /// The wire code the host's dispatcher receives (`03:03.7`).
    pub const fn code(self) -> u32 {
        match self {
            ScriptHook::ModifyAttack => 0,
            ScriptHook::ModifyDamage => 1,
            ScriptHook::ModifyArmor => 2,
            ScriptHook::Movement => 3,
            ScriptHook::ActionEconomy => 4,
            ScriptHook::Hit => 5,
            ScriptHook::Resist => 6,
            ScriptHook::OwnTurn => 7,
            ScriptHook::Absorb => 8,
            ScriptHook::PowerUsed => 9,
        }
    }

    /// The stable name, for a report.
    pub const fn id(self) -> &'static str {
        match self {
            ScriptHook::ModifyAttack => "modify_attack",
            ScriptHook::ModifyDamage => "modify_damage",
            ScriptHook::ModifyArmor => "modify_armor",
            ScriptHook::Movement => "movement",
            ScriptHook::ActionEconomy => "action_economy",
            ScriptHook::Hit => "hit",
            ScriptHook::Resist => "resist",
            ScriptHook::OwnTurn => "own_turn",
            ScriptHook::Absorb => "absorb",
            ScriptHook::PowerUsed => "power_used",
        }
    }
}

/// The host a script answer comes from (`03:03.7`).
pub trait ScriptHost: Sync {
    /// Answer a hook. `context` is the frozen snapshot; the answer is the effect
    /// document a DSL template uses, or `None` for "nothing to add".
    fn call(&self, hook: ScriptHook, context: &Json) -> Option<Json>;
}

/// The installed host, if any.
static HOST: Mutex<Option<&'static dyn ScriptHost>> = Mutex::new(None);

/// The script set the worker recorded, as `(id, hash)` (`AD-8`).
static SCRIPTS: Mutex<Vec<(String, String)>> = Mutex::new(Vec::new());

/// Failures the host reported, as `{id, hook, code}` (`03:03.7`).
static FAILURES: Mutex<Vec<Json>> = Mutex::new(Vec::new());

/// Install the host. The last one wins, so a worker restart re-installs.
pub fn install(host: &'static dyn ScriptHost) {
    if let Ok(mut slot) = HOST.lock() {
        *slot = Some(host);
    }
}

/// Remove the host, so a replay can run without scripts.
pub fn uninstall() {
    if let Ok(mut slot) = HOST.lock() {
        *slot = None;
    }
}

/// Whether a host is installed.
pub fn is_installed() -> bool {
    HOST.lock().map(|slot| slot.is_some()).unwrap_or(false)
}

/// Record the script set the loader found (`AD-8`, `03:03.8` step 9).
pub fn record(scripts: Vec<(String, String)>) {
    if let Ok(mut slot) = SCRIPTS.lock() {
        *slot = scripts;
    }
}

/// The recorded script set.
pub fn recorded() -> Vec<(String, String)> {
    SCRIPTS.lock().map(|slot| slot.clone()).unwrap_or_default()
}

/// Record a script failure the host reported.
pub fn note_failure(id: &str, hook: &str, code: &str) {
    if let Ok(mut slot) = FAILURES.lock() {
        slot.push(Json::obj([
            ("id", Json::string(id)),
            ("hook", Json::string(hook)),
            ("code", Json::string(code)),
        ]));
    }
}

/// The script report: which scripts are live and which broke (`03:03.7`).
pub fn report() -> Json {
    let scripts = recorded();
    let failures = FAILURES.lock().map(|slot| slot.clone()).unwrap_or_default();
    Json::obj([
        ("schema", Json::string("kobra.script-report/1")),
        (
            "scripts",
            Json::arr(scripts.iter().map(|(id, hash)| {
                Json::obj([("id", Json::string(id)), ("hash", Json::string(hash))])
            })),
        ),
        ("failures", Json::Arr(failures)),
    ])
}

/// Build the frozen context document a script reads (`03:03.7`).
///
/// It carries the identities, the derived numbers a hook needs and a compact
/// scene, and **nothing mutable**: writes go back as commands.
pub fn context(hook: ScriptHook, ctx: &PowerCtx<'_>, result: i32) -> Json {
    let character = |which: u32| -> Json {
        match ctx.character(which) {
            None => Json::Null,
            Some(character) => Json::obj([
                ("id", Json::Num(i64::from(character.id))),
                ("side", Json::Num(i64::from(character.side))),
                ("damage", Json::Num(i64::from(character.damage))),
                ("max_damage", Json::Num(i64::from(character.max_damage))),
                (
                    "conditions",
                    Json::arr(
                        character
                            .conditions
                            .iter()
                            .map(|condition| Json::string(condition.kind.id())),
                    ),
                ),
                (
                    "powers",
                    Json::arr(character.powers.iter().map(|power| {
                        Json::obj([
                            ("id", Json::string(&power.id)),
                            ("rv", Json::Num(i64::from(power.rv))),
                        ])
                    })),
                ),
            ]),
        }
    };
    let attack = match ctx.attack {
        None => Json::Null,
        Some(view) => Json::obj([
            ("shape", Json::string(shape_id(view))),
            ("trait", Json::string(view.trait_.id())),
            ("rv", Json::Num(i64::from(view.rv))),
            ("damage", Json::Num(i64::from(view.damage))),
            ("distance", Json::Num(i64::from(view.distance))),
            ("reach", Json::Num(i64::from(view.reach))),
            ("piercing", Json::Bool(view.piercing)),
            ("tags", Json::arr(view.tags.iter().map(Json::string))),
        ]),
    };
    Json::obj([
        ("hook", Json::string(hook.id())),
        // The hook's incoming value, so a script that adds to it can read it
        // rather than guess (`03:03.7`).
        ("result", Json::Num(i64::from(result))),
        ("actor", Json::Num(i64::from(ctx.actor))),
        ("target", Json::Num(i64::from(ctx.target))),
        ("owner", Json::Num(i64::from(ctx.owner))),
        ("distance", Json::Num(i64::from(ctx.distance))),
        (
            "colour",
            Json::string(crate::l2::actions::colour_name(ctx.colour)),
        ),
        ("charged", Json::Num(i64::from(ctx.charged))),
        (
            "power",
            Json::obj([
                ("id", Json::string(&ctx.power.id)),
                ("rv", Json::Num(i64::from(ctx.power.rv))),
                (
                    "params",
                    Json::Obj(
                        ctx.power
                            .params
                            .iter()
                            .map(|(key, value)| (key.clone(), Json::string(value)))
                            .collect(),
                    ),
                ),
            ]),
        ),
        ("attack", attack),
        ("owner_entity", character(ctx.owner)),
        ("actor_entity", character(ctx.actor)),
        ("target_entity", character(ctx.target)),
    ])
}

/// The attack shape's content id.
fn shape_id(view: &AttackView) -> &'static str {
    use crate::l1::powers::AttackShape;
    match view.shape {
        AttackShape::MeleeBash => "melee_bash",
        AttackShape::MeleeSlash => "melee_slash",
        AttackShape::Ranged => "ranged",
        AttackShape::Power => "power",
        AttackShape::Manoeuvre => "manoeuvre",
    }
}

/// Ask the host, if there is one.
fn call(hook: ScriptHook, ctx: &PowerCtx<'_>, result: i32) -> Option<Json> {
    let host = HOST.lock().ok().and_then(|slot| *slot)?;
    host.call(hook, &context(hook, ctx, result))
}

/// The queued writes a script asked for, compiled by the DSL's own compiler.
///
/// Sharing the compiler is the point: a script cannot name an effect a data mod
/// cannot, so the two tiers have one vocabulary (`AD-16`, `03:03.7`). An effect
/// that does not compile is reported and dropped, never partially applied.
pub fn effects(hook: ScriptHook, ctx: &PowerCtx<'_>, result: i32) -> Vec<HookEffect> {
    let Some(answer) = call(hook, ctx, result) else {
        return Vec::new();
    };
    let Some(Json::Arr(items)) = answer.get("effects") else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for (index, item) in items.iter().enumerate() {
        match crate::dsl::resolve_effect(item, &format!("/effects/{index}")) {
            Ok(effects) => {
                for effect in effects {
                    out.push(effect.resolve(ctx, result, None));
                }
            }
            Err(error) => note_failure("<script>", hook.id(), error.code),
        }
    }
    out.into_iter().flatten().collect()
}

/// A value-returning hook: the script's answer, or the incoming value.
pub fn value(hook: ScriptHook, ctx: &PowerCtx<'_>, incoming: i32) -> i32 {
    let Some(answer) = call(hook, ctx, incoming) else {
        return incoming;
    };
    match answer.get("result").and_then(Json::as_i64) {
        Some(value) => value as i32,
        None => incoming,
    }
}

/// A colour-returning hook (`on_resist`).
pub fn colour(hook: ScriptHook, ctx: &PowerCtx<'_>, incoming: Colour) -> Colour {
    let Some(answer) = call(hook, ctx, incoming.code() as i32) else {
        return incoming;
    };
    match answer.get("result").and_then(Json::as_i64) {
        Some(value) => Colour::from_code(value.clamp(0, 3) as u8).unwrap_or(incoming),
        None => incoming,
    }
}

/// The movement a script declares (`4c:570`), if any.
///
/// The answer is `{"move": {"mode": "fly", "tiles": 9, "max_material": null}}`,
/// which is the same shape a DSL `move` effect compiles to.
pub fn movement(hook: ScriptHook, ctx: &PowerCtx<'_>) -> Option<crate::l1::powers::MoveProfile> {
    let answer = call(hook, ctx, 0)?;
    let document = answer.get("move")?;
    let mode = document
        .get("mode")
        .and_then(Json::as_str)
        .and_then(crate::l1::powers::MoveMode::from_id)?;
    Some(crate::l1::powers::MoveProfile {
        mode,
        tiles: document
            .get("tiles")
            .and_then(Json::as_i64)
            .map(|v| v as i32),
        max_material: document
            .get("max_material")
            .and_then(Json::as_i64)
            .map(|v| v as i32),
    })
}

/// The action economy a script declares (`4c:560`), if any.
///
/// The answer is `{"economy": {"attacks": 3, "bonus_tiles": 0}}`.
pub fn economy(hook: ScriptHook, ctx: &PowerCtx<'_>) -> Option<crate::l1::powers::ActionEconomy> {
    let answer = call(hook, ctx, 0)?;
    let document = answer.get("economy")?;
    Some(crate::l1::powers::ActionEconomy {
        attacks_per_panel: document.get("attacks").and_then(Json::as_i64).unwrap_or(1) as i32,
        bonus_tiles: document
            .get("bonus_tiles")
            .and_then(Json::as_i64)
            .unwrap_or(0) as i32,
    })
}

/// An attack-modifying hook: merge the script's `attack` document.
pub fn attack(hook: ScriptHook, ctx: &PowerCtx<'_>, incoming: AttackMods) -> AttackMods {
    let Some(answer) = call(hook, ctx, 0) else {
        return incoming;
    };
    let Some(document) = answer.get("attack") else {
        return incoming;
    };
    match crate::dsl::resolve_attack_spec(document, "/attack", ctx) {
        Ok(Some(spec)) => merge_attack(incoming, spec),
        Ok(None) => incoming,
        Err(error) => {
            note_failure("<script>", hook.id(), error.code);
            incoming
        }
    }
}

/// Merge a resolved attack spec over the modifiers already in force.
fn merge_attack(mut out: AttackMods, spec: AttackMods) -> AttackMods {
    if spec.trait_.is_some() {
        out.trait_ = spec.trait_;
    }
    if spec.rv.is_some() {
        out.rv = spec.rv;
    }
    if spec.damage_override.is_some() {
        out.damage_override = spec.damage_override;
    }
    if spec.hands.is_some() {
        out.hands = spec.hands;
    }
    if spec.reach.is_some() {
        out.reach = spec.reach;
    }
    out.steps += spec.steps;
    out.armour += spec.armour;
    out.piercing |= spec.piercing;
    if spec.colour_cap.is_some() {
        out.colour_cap = spec.colour_cap;
    }
    out.extra_attacks += spec.extra_attacks;
    out.keep_two_of_three |= spec.keep_two_of_three;
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l1::powers::PowerInst;
    use crate::tables;

    struct TestHost;

    impl ScriptHost for TestHost {
        fn call(&self, hook: ScriptHook, _context: &Json) -> Option<Json> {
            if hook != ScriptHook::ModifyDamage {
                return None;
            }
            Some(Json::parse(r#"{"result": 99}"#).expect("json"))
        }
    }

    static HOST: TestHost = TestHost;

    fn context_for<'a>(
        power: &'a PowerInst,
        characters: &'a [crate::l1::character::Character],
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
            attack: None,
            charged: 0,
        }
    }

    #[test]
    fn a_script_answer_replaces_a_hook_value() {
        let characters = [crate::l1::character::Character::authored(
            1,
            "a",
            "",
            0,
            [10; 7],
            10,
            0,
            vec![],
            vec![],
            vec![],
        )];
        let power = PowerInst::new("wsp.power.absorption", 20);
        let ctx = context_for(&power, &characters);
        assert_eq!(value(ScriptHook::ModifyDamage, &ctx, 5), 5, "no host");
        install(&HOST);
        assert_eq!(value(ScriptHook::ModifyDamage, &ctx, 5), 99);
        // The context carries the hook's identity and the frozen numbers.
        let document = context(ScriptHook::ModifyDamage, &ctx, 5);
        assert_eq!(
            document.get("hook").and_then(Json::as_str),
            Some("modify_damage")
        );
        assert_eq!(document.get("result").and_then(Json::as_i64), Some(5));
        assert_eq!(document.get("distance").and_then(Json::as_i64), Some(3));
        uninstall();
        assert_eq!(value(ScriptHook::ModifyDamage, &ctx, 5), 5);
    }
}
