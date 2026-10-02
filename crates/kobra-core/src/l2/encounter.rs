//! L2 — the encounter machine (02:02.5).
//!
//! 4C resolves combat in **turns**, one turn being "an abstract amount of time
//! equal to the action depicted in a single comic book panel", with initiative
//! rolled per side and one side acting at a time (`4c:880-889`). The quantum for
//! every rules duration is the **panel**, and the encounter machine's phases are
//! the spec's:
//!
//! ```text
//! SELECT      player plans actions (paused; the UI is the frame clock here)
//! INITIATIVE  each side rolls d% (+ highest Awareness if the campaign rule is on)
//! SIDE_A      acting side executes its queued actions in a fixed order
//! SIDE_B      the other side executes
//! RESOLVE     end-of-panel: durations decrement, dying advances
//! ADVANCE     panel counter increments; encounter may end
//! ```
//!
//! M1 drives the machine from an ordered command stream (the replay harness's
//! input) rather than from a UI, which is why `SELECT` is simply "actions are
//! queued". The phase names are kept because a diagnostic and a preview both
//! report them.
//!
//! Everything here reads **frozen** state and draws from the one seeded RNG in a
//! fixed order, so a replay reproduces it exactly (`02:02.9`).

use crate::json::Json;
use crate::l0::{apply_fortune, Amount, Colour, FortuneCommit, Ladder};
use crate::l1::character::Character;
use crate::l1::items::{Hands, Item};
use crate::l1::powers::{
    AttackMods, AttackShape, AttackView, EffectTarget, HookEffect, PowerCtx, PowerKernel,
};
use crate::l1::skills::Skill;
use crate::l1::status::{self, Condition, ConditionKind};
use crate::l1::traits::Trait;
use crate::l2::actions::{Action, ActionKind, Effect, OutcomeTable};
use crate::l2::damage;
use crate::l2::vehicle::{Difficulty, Struck, Vehicle, GROUND_MATERIAL};
use crate::l3::world::{Position, Sight, World};
use crate::rng::Rng;
use crate::rules::{ElevationTarget, Rules};

/// Which phase of a panel the machine is in (`02:02.5`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Phase {
    /// Actions are being queued.
    Select,
    /// Sides are rolling for initiative.
    Initiative,
    /// The side that won initiative acts.
    SideA,
    /// The side that lost acts.
    SideB,
    /// End of panel: durations and dying.
    Resolve,
    /// The panel counter increments.
    Advance,
    /// The encounter is over.
    Ended,
}

impl Phase {
    /// The stable content id, for a diagnostic.
    pub const fn id(self) -> &'static str {
        match self {
            Phase::Select => "select",
            Phase::Initiative => "initiative",
            Phase::SideA => "side_a",
            Phase::SideB => "side_b",
            Phase::Resolve => "resolve",
            Phase::Advance => "advance",
            Phase::Ended => "ended",
        }
    }
}

/// One queued action.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Queued {
    /// The acting entity.
    pub actor: u32,
    /// What it will do.
    pub action: Action,
}

/// The encounter's own state: the panel clock, the phases, and what is queued.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Encounter {
    /// The encounter record id, so a save names what it was playing.
    pub id: String,
    /// The map record id.
    pub map: String,
    /// Panels elapsed.
    pub panel: u32,
    /// The current phase.
    pub phase: Phase,
    /// Each side's raw initiative roll this panel.
    pub initiative_roll: [u8; 2],
    /// Each side's Awareness bonus this panel.
    pub initiative_bonus: [i32; 2],
    /// The queued actions, in queue order.
    pub queued: Vec<Queued>,
    /// Whether the encounter has ended.
    pub finished: bool,
    /// The winning side, when one side was wiped out.
    pub winner: Option<u8>,
}

impl Encounter {
    /// A fresh encounter on a map.
    pub fn new(id: &str, map: &str) -> Encounter {
        Encounter {
            id: id.to_string(),
            map: map.to_string(),
            panel: 0,
            phase: Phase::Select,
            initiative_roll: [0, 0],
            initiative_bonus: [0, 0],
            queued: Vec::new(),
            finished: false,
            winner: None,
        }
    }

    /// Queue an actor's action, replacing any earlier one for that actor.
    pub fn queue(&mut self, actor: u32, action: Action) {
        match self.queued.iter_mut().find(|queued| queued.actor == actor) {
            Some(existing) => existing.action = action,
            None => self.queued.push(Queued { actor, action }),
        }
    }

    /// The action queued for an actor, if any.
    pub fn queued_for(&self, actor: u32) -> Option<&Action> {
        self.queued
            .iter()
            .find(|queued| queued.actor == actor)
            .map(|queued| &queued.action)
    }
}

/// What the encounter needs from the content registry.
///
/// A trait rather than the registry itself, so the L2 layer does not depend on
/// content loading and a test can supply three records instead of a pack tree.
pub trait RulesContent {
    /// The item record named by an id.
    fn item(&self, id: &str) -> Option<&Item>;
    /// The outcome table for an action: content when it declares one, the
    /// canonical 4C table otherwise (`02:02.5`).
    fn outcome(&self, action: ActionKind) -> &OutcomeTable;
    /// The skill record named by an id.
    fn skill(&self, id: &str) -> Option<&Skill>;
    /// The kernel a power id resolves to, canonical or a compiled template
    /// (`AD-16`). `None` is a validator problem, and a hook simply skips it.
    fn power_kernel(&self, id: &str) -> Option<&dyn PowerKernel>;
}

/// Everything a panel needs to mutate.
pub struct Battle<'a> {
    /// The ladder, for every band lookup.
    pub ladder: &'a Ladder,
    /// The rules pack's effective values.
    pub rules: &'a Rules,
    /// The content records an action names.
    pub content: &'a dyn RulesContent,
    /// The one seeded RNG (`AD-6`).
    pub rng: &'a mut Rng,
    /// The map and who stands on it.
    pub world: &'a mut World,
    /// Every combatant.
    pub characters: &'a mut Vec<Character>,
    /// Every vehicle on the map (`4c:1300-1365`).
    pub vehicles: &'a mut Vec<Vehicle>,
    /// The event outbox for this call.
    pub events: &'a mut Vec<Json>,
}

impl Battle<'_> {
    /// The index of a character by entity id.
    fn index_of(&self, id: u32) -> Option<usize> {
        self.characters
            .iter()
            .position(|character| character.id == id)
    }

    /// Push an event.
    fn emit(&mut self, event: Json) {
        self.events.push(event);
    }

    /// The entities on a side, ascending by id.
    fn side_actors(&self, side: u8) -> Vec<u32> {
        let mut ids: Vec<u32> = self
            .characters
            .iter()
            .filter(|character| character.side == side)
            .map(|character| character.id)
            .collect();
        ids.sort_unstable();
        ids
    }

    /// Whether a side has anyone still able to act.
    fn side_defeated(&self, side: u8) -> bool {
        !self
            .characters
            .iter()
            .any(|character| character.side == side && !character.dead && !character.is_out())
    }
}

/// Advance one panel (`02:02.5`).
///
/// Returns the events the panel produced. The whole panel is synchronous, so a
/// replay and the live game take exactly the same path.
pub fn commit(encounter: &mut Encounter, battle: &mut Battle<'_>) {
    if encounter.finished {
        return;
    }
    initiative(encounter, battle);
    let order: [u8; 2] = {
        let first = &encounter.initiative_roll;
        let bonus = &encounter.initiative_bonus;
        let total0 = i32::from(first[0]) + bonus[0];
        let total1 = i32::from(first[1]) + bonus[1];
        // A tie goes to the party, so the outcome is stated rather than
        // arbitrary (the spec does not say; a deterministic tie-break is a
        // reading, and it is recorded here).
        if total0 >= total1 {
            [0, 1]
        } else {
            [1, 0]
        }
    };
    for (index, side) in order.into_iter().enumerate() {
        encounter.phase = if index == 0 {
            Phase::SideA
        } else {
            Phase::SideB
        };
        act_side(encounter, battle, side);
    }
    encounter.phase = Phase::Resolve;
    resolve_end_of_panel(encounter, battle);
    encounter.phase = Phase::Advance;
    encounter.panel += 1;
    encounter.queued.clear();
    let party_out = battle.side_defeated(0);
    let opposition_out = battle.side_defeated(1);
    battle.emit(Json::obj([
        ("event", Json::string("panel_end")),
        ("panel", Json::Num(i64::from(encounter.panel - 1))),
    ]));
    if party_out || opposition_out {
        encounter.finished = true;
        encounter.phase = Phase::Ended;
        encounter.winner = match (party_out, opposition_out) {
            (true, false) => Some(1),
            (false, true) => Some(0),
            _ => None,
        };
        battle.emit(Json::obj([
            ("event", Json::string("encounter_end")),
            (
                "winner",
                match encounter.winner {
                    Some(side) => Json::Num(i64::from(side)),
                    None => Json::Null,
                },
            ),
        ]));
    } else {
        encounter.phase = Phase::Select;
    }
}

/// Roll initiative per side (`4c:891`).
fn initiative(encounter: &mut Encounter, battle: &mut Battle<'_>) {
    encounter.phase = Phase::Initiative;
    for side in 0..2u8 {
        let roll = battle.rng.d100();
        let bonus = if battle.rules.initiative_awareness {
            battle
                .characters
                .iter()
                .filter(|character| character.side == side && !character.dead)
                .map(|character| {
                    character
                        .effective(battle.ladder)
                        .trait_rv(Trait::Awareness)
                })
                .max()
                .unwrap_or(0)
        } else {
            0
        };
        encounter.initiative_roll[side as usize] = roll;
        encounter.initiative_bonus[side as usize] = bonus;
        battle.emit(Json::obj([
            ("event", Json::string("initiative")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("side", Json::Num(i64::from(side))),
            ("roll", Json::Num(i64::from(roll))),
            ("bonus", Json::Num(i64::from(bonus))),
        ]));
    }
}

/// One side acts, in ascending entity id (`02:02.5`: a fixed order).
fn act_side(encounter: &mut Encounter, battle: &mut Battle<'_>, side: u8) {
    for actor in battle.side_actors(side) {
        let Some(index) = battle.index_of(actor) else {
            continue;
        };
        if battle.characters[index].dead || battle.characters[index].is_out() {
            continue;
        }
        // 4c:718, 4c:815: a power that acts on its own turn runs first, and may
        // spend the panel's action (`Regeneration` does).
        if run_own_turn(encounter, battle, actor) {
            battle.emit(Json::obj([
                ("event", Json::string("forfeit")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
                ("reason", Json::string("power_consumed_action")),
            ]));
            continue;
        }
        let action = encounter.queued_for(actor).cloned();
        match action {
            Some(action) => {
                // 4c:560: `Fast Attack` and `Extra Body Parts` grant more than
                // one attack in a panel. The cap is the largest allowance any
                // power declares, so a power cannot multiply another's.
                let allowance = if action.kind.is_roll() {
                    attack_allowance(battle, actor)
                } else {
                    1
                };
                for _ in 0..allowance {
                    if battle.characters[index].dead || battle.characters[index].is_out() {
                        break;
                    }
                    resolve_action(encounter, battle, actor, &action);
                }
            }
            None => {
                battle.emit(Json::obj([
                    ("event", Json::string("forfeit")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(actor))),
                ]));
            }
        }
    }
}

/// Run every `on_own_turn` hook the actor has. Returns whether the panel's
/// action is spent (`4c:718`).
fn run_own_turn(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32) -> bool {
    let mut effects: Vec<(u32, HookEffect)> = Vec::new();
    let mut consumes = false;
    {
        let characters: &[Character] = battle.characters;
        let Some(character) = characters.iter().find(|character| character.id == actor) else {
            return false;
        };
        for power in &character.powers {
            let Some(kernel) = battle.content.power_kernel(&power.id) else {
                continue;
            };
            let ctx = PowerCtx {
                ladder: battle.ladder,
                power,
                characters,
                owner: actor,
                actor,
                target: actor,
                distance: 0,
                colour: Colour::Blck,
                attack: None,
                charged: 0,
            };
            let mut plan = crate::l1::powers::TurnPlan::default();
            kernel.on_own_turn(&ctx, &mut plan);
            consumes |= plan.consumes_action;
            for effect in plan.effects {
                effects.push((actor, effect));
            }
        }
        if let Some(ctx) = script_ctx(
            battle.ladder,
            characters,
            actor,
            actor,
            actor,
            0,
            Colour::Blck,
            None,
            0,
        ) {
            for effect in crate::script::effects(crate::script::ScriptHook::OwnTurn, &ctx, 0) {
                effects.push((actor, effect));
            }
        }
    }
    for (owner, effect) in effects {
        apply_hook_effect(battle, encounter, owner, actor, actor, effect);
    }
    consumes
}

/// How many attacks a character may make this panel (`4c:560`, `4c:554`).
fn attack_allowance(battle: &Battle<'_>, actor: u32) -> i32 {
    let characters: &[Character] = battle.characters;
    let Some(character) = characters.iter().find(|character| character.id == actor) else {
        return 1;
    };
    let mut attacks = 1;
    for power in &character.powers {
        let Some(kernel) = battle.content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder: battle.ladder,
            power,
            characters,
            owner: actor,
            actor,
            target: actor,
            distance: 0,
            colour: Colour::Blck,
            attack: None,
            charged: 0,
        };
        if let Some(economy) = kernel.action_economy(&ctx, actor) {
            attacks = attacks.max(economy.attacks_per_panel);
        }
    }
    if let Some(ctx) = script_ctx(
        battle.ladder,
        characters,
        actor,
        actor,
        actor,
        0,
        Colour::Blck,
        None,
        0,
    ) {
        if let Some(economy) =
            crate::script::economy(crate::script::ScriptHook::ActionEconomy, &ctx)
        {
            attacks = attacks.max(economy.attacks_per_panel);
        }
    }
    attacks.clamp(1, 4)
}

/// Resolve one queued action.
fn resolve_action(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32, action: &Action) {
    let Some(index) = battle.index_of(actor) else {
        return;
    };
    let permission = battle.characters[index].permission();
    match action.kind {
        ActionKind::Stand => {
            // Standing is always legal: it is how `knocked_down` ends.
            let removed = status::remove(
                &mut battle.characters[index].conditions,
                ConditionKind::KnockedDown,
            );
            battle.emit(Json::obj([
                ("event", Json::string("stand")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
                ("stood", Json::Bool(removed)),
            ]));
            return;
        }
        ActionKind::Wait => {
            battle.emit(Json::obj([
                ("event", Json::string("wait")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
            ]));
            return;
        }
        _ => {}
    }
    if !permission.can_act() {
        refuse(
            battle,
            encounter,
            actor,
            action.kind,
            "condition_prevents_action",
        );
        return;
    }
    match action.kind {
        ActionKind::Move => resolve_move(encounter, battle, actor, action),
        ActionKind::MeleeBash | ActionKind::MeleeSlash | ActionKind::Ranged => {
            resolve_attack(encounter, battle, actor, action);
        }
        ActionKind::Dodge => resolve_dodge(encounter, battle, actor, action),
        ActionKind::Power => resolve_attack(encounter, battle, actor, action),
        ActionKind::Ram => resolve_ram(encounter, battle, actor, action),
        ActionKind::Manoeuvre => resolve_manoeuvre(encounter, battle, actor, action),
        ActionKind::Stand | ActionKind::Wait => unreachable!("handled above"),
    }
}

/// Emit a refusal with a code, never English (`AD-22`).
fn refuse(
    battle: &mut Battle<'_>,
    encounter: &Encounter,
    actor: u32,
    kind: ActionKind,
    reason: &str,
) {
    battle.emit(Json::obj([
        ("event", Json::string("refused")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("kind", Json::string(kind.id())),
        ("reason", Json::string(reason)),
    ]));
}

/// The row steps and final colour of a roll, shared by attacks and dodges.
struct RollOutcome {
    colour: Colour,
    rolled: Colour,
    d100: u8,
    band: usize,
}

/// Roll on the Master Table with a set of row steps and a Fortune window.
#[allow(clippy::too_many_arguments)]
fn roll_with(
    battle: &mut Battle<'_>,
    base_band: usize,
    steps: i32,
    roll_penalty: i32,
    commits: &mut [FortuneCommit],
    punch_cap: Option<Colour>,
) -> RollOutcome {
    let d100 = battle.rng.d100();
    let adjusted = (i32::from(d100) + roll_penalty).clamp(0, 99) as u8;
    let band = battle.ladder.row_step(base_band, steps);
    let rolled = battle
        .ladder
        .resolve(battle.ladder.bands[band].lo, adjusted)
        .unwrap_or(Colour::Blck);
    let mut colour = apply_fortune(rolled, commits);
    if let Some(cap) = punch_cap {
        // 4c:1088-1090: the success level is capped, not just the damage.
        if colour > cap {
            colour = cap;
        }
    }
    RollOutcome {
        colour,
        rolled,
        d100,
        band,
    }
}

/// The equipped item, when there is one.
fn weapon<'a>(content: &'a dyn RulesContent, character: &Character) -> Option<&'a Item> {
    character.weapon.as_deref().and_then(|id| content.item(id))
}

/// Everything an attack roll is made of, computed **once**.
///
/// The resolver and [`crate::sim::Sim::preview`] both go through this, which is
/// the whole point of `02:02.10`: "the UI can show exact odds and the editor can
/// show a playtest consequence, and neither can disagree with the real
/// resolution". Two copies of this arithmetic would disagree eventually, and the
/// disagreement would look like a bug in the game rather than in the preview.
#[derive(Clone, Debug)]
pub struct AttackPlan {
    /// Why the attack is illegal, when it is; a code, never English.
    pub refusal: Option<&'static str>,
    /// The acting entity.
    pub actor: u32,
    /// The target entity.
    pub target: u32,
    /// The trait the roll uses, after any power substitution.
    pub trait_: Trait,
    /// The band the roll starts from.
    pub base_band: usize,
    /// Situational row steps: skills, movement, Dodge, elevation, cover and
    /// powers.
    pub steps: i32,
    /// The flat `d%` penalty from conditions.
    pub roll_penalty: i32,
    /// Colours a modifier demotes after the roll.
    pub demote: Vec<Colour>,
    /// The declared success cap, if any.
    pub punch_cap: Option<Colour>,
    /// Whether the attack halves the target's armour.
    pub piercing: bool,
    /// The distance in tiles.
    pub distance: i32,
    /// The attacker's reach in tiles.
    pub reach: i32,
    /// The cover row steps, if the target is in cover.
    pub cover_steps: i32,
    /// The elevation row steps.
    pub elevation_steps: i32,
    /// The raw damage the attack inflicts before armour.
    pub damage: i32,
    /// The attack rolls three dice and keeps two (`4c:432`).
    pub keep_two_of_three: bool,
    /// The attack a hook sees (`02:02.4`).
    pub view: AttackView,
    /// Reasons, for a tooltip (`02:02.11` rule 2).
    pub notes: Vec<String>,
}

impl AttackPlan {
    /// A plan that refuses, before anything else is computed.
    fn refused(actor: u32, target: u32, reason: &'static str) -> AttackPlan {
        AttackPlan {
            refusal: Some(reason),
            actor,
            target,
            trait_: Trait::Melee,
            base_band: 0,
            steps: 0,
            roll_penalty: 0,
            demote: Vec::new(),
            punch_cap: None,
            piercing: false,
            distance: 0,
            reach: 0,
            cover_steps: 0,
            elevation_steps: 0,
            damage: 0,
            keep_two_of_three: false,
            view: AttackView::melee(AttackShape::MeleeBash, Trait::Melee, 1, 0),
            notes: Vec::new(),
        }
    }
}

/// The hook shape an action's attack takes (`02:02.4`).
fn attack_shape(kind: ActionKind) -> AttackShape {
    match kind {
        ActionKind::MeleeBash => AttackShape::MeleeBash,
        ActionKind::MeleeSlash => AttackShape::MeleeSlash,
        ActionKind::Ranged => AttackShape::Ranged,
        _ => AttackShape::Power,
    }
}

/// The unmodified attack the actor's equipment and action produce.
fn base_attack(
    content: &dyn RulesContent,
    ladder: &Ladder,
    actor_character: &Character,
    action: &Action,
    distance: i32,
) -> Result<AttackView, &'static str> {
    let effective = actor_character.effective(ladder);
    let coordination = effective.trait_rv(Trait::Coordination);
    let item = weapon(content, actor_character);
    match action.kind {
        ActionKind::Power => {
            // 4c:991: a power's reach is its Rank Value / 10, rounded up, in
            // legacy sectors.
            let id = action.power.as_deref().ok_or("no_such_power")?;
            let power = actor_character.power(id).ok_or("no_such_power")?;
            Ok(AttackView {
                shape: AttackShape::Power,
                trait_: Trait::Coordination,
                rv: coordination,
                damage: 0,
                distance,
                reach: crate::l1::powers::tenths_up(power.rv) * 3,
                tags: Vec::new(),
                piercing: false,
                hands: Hands::None,
            })
        }
        kind => {
            let trait_ = kind.trait_();
            let rv = effective.trait_rv(trait_);
            let hands = item.map(|item| item.hands).unwrap_or(Hands::None);
            let reach = match item {
                Some(item) => item.reach.tiles(ladder, coordination),
                None => 1,
            };
            let damage = match kind {
                ActionKind::Ranged => match item {
                    Some(item) => damage::raw_ranged(item).unwrap_or(rv),
                    None => effective.trait_rv(Trait::Brawn),
                },
                _ => damage::raw_melee(effective.trait_rv(Trait::Brawn), hands),
            };
            let tags = item.map(|item| item.tags.clone()).unwrap_or_default();
            Ok(AttackView {
                shape: attack_shape(kind),
                trait_,
                rv,
                damage,
                distance,
                reach,
                tags,
                piercing: false,
                hands,
            })
        }
    }
}

/// Every power instance that may react, the actor's first and then the target's.
///
/// The order is fixed so the RNG-free fold is deterministic; a power that only
/// reacts in one direction guards itself with
/// [`PowerCtx::owner_is_actor`](crate::l1::powers::PowerCtx::owner_is_actor).
fn reacting_powers(
    characters: &[Character],
    actor: u32,
    target: u32,
) -> Vec<(u32, crate::l1::powers::PowerInst)> {
    let mut out = Vec::new();
    for who in [actor, target] {
        if let Some(character) = characters.iter().find(|character| character.id == who) {
            for power in &character.powers {
                out.push((who, power.clone()));
            }
        }
    }
    out
}

/// The context a Tier-2 script hook reads (`03:03.7`).
///
/// A script extends a **power**, so the hook runs only when the entity carries at
/// least one; that keeps a script from being consulted for an entity that has
/// nothing for it to modify.
#[allow(clippy::too_many_arguments)]
fn script_ctx<'a>(
    ladder: &'a Ladder,
    characters: &'a [Character],
    owner: u32,
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: Option<&'a AttackView>,
    charged: i32,
) -> Option<PowerCtx<'a>> {
    let power = characters
        .iter()
        .find(|character| character.id == owner)?
        .powers
        .first()?;
    Some(PowerCtx {
        ladder,
        power,
        characters,
        owner,
        actor,
        target,
        distance,
        colour,
        attack,
        charged,
    })
}

/// Fold every power's `modify_attack` into an attack (`4c:464`, `4c:618`).
#[allow(clippy::too_many_arguments)]
fn fold_attack(
    content: &dyn RulesContent,
    ladder: &Ladder,
    characters: &[Character],
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: &AttackView,
    incoming: AttackMods,
) -> AttackMods {
    let mut mods = incoming;
    for (owner, power) in reacting_powers(characters, actor, target) {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power: &power,
            characters,
            owner,
            actor,
            target,
            distance,
            colour,
            attack: Some(attack),
            charged: 0,
        };
        mods = kernel.modify_attack(&ctx, mods);
    }
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        actor,
        actor,
        target,
        distance,
        colour,
        Some(attack),
        0,
    ) {
        mods = crate::script::attack(crate::script::ScriptHook::ModifyAttack, &ctx, mods);
    }
    mods
}

/// Fold every offensive power's `modify_damage` into a damage value.
#[allow(clippy::too_many_arguments)]
fn fold_damage(
    content: &dyn RulesContent,
    ladder: &Ladder,
    characters: &[Character],
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: &AttackView,
    incoming: i32,
) -> i32 {
    let mut value = incoming;
    let owner_character = characters.iter().find(|character| character.id == actor);
    let Some(owner_character) = owner_character else {
        return value;
    };
    for power in &owner_character.powers {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power,
            characters,
            owner: actor,
            actor,
            target,
            distance,
            colour,
            attack: Some(attack),
            charged: 0,
        };
        value = kernel.modify_damage(&ctx, value);
    }
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        actor,
        actor,
        target,
        distance,
        colour,
        Some(attack),
        0,
    ) {
        value = crate::script::value(crate::script::ScriptHook::ModifyDamage, &ctx, value);
    }
    value
}

/// Fold every defensive power's `modify_armor` into an armour value.
#[allow(clippy::too_many_arguments)]
fn fold_armor(
    content: &dyn RulesContent,
    ladder: &Ladder,
    characters: &[Character],
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: &AttackView,
    incoming: i32,
) -> i32 {
    let mut value = incoming;
    let Some(defender) = characters.iter().find(|character| character.id == target) else {
        return value;
    };
    for power in &defender.powers {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power,
            characters,
            owner: target,
            actor,
            target,
            distance,
            colour,
            attack: Some(attack),
            charged: 0,
        };
        value = kernel.modify_armor(&ctx, value);
    }
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        target,
        actor,
        target,
        distance,
        colour,
        Some(attack),
        0,
    ) {
        value = crate::script::value(crate::script::ScriptHook::ModifyArmor, &ctx, value);
    }
    value
}

/// Collect the queued writes every `on_hit` hook produces, owner-tagged.
#[allow(clippy::too_many_arguments)]
fn collect_hit_hooks(
    content: &dyn RulesContent,
    ladder: &Ladder,
    characters: &[Character],
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: &AttackView,
) -> Vec<(u32, HookEffect)> {
    let mut out = Vec::new();
    let Some(attacker) = characters.iter().find(|character| character.id == actor) else {
        return out;
    };
    for power in &attacker.powers {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power,
            characters,
            owner: actor,
            actor,
            target,
            distance,
            colour,
            attack: Some(attack),
            charged: 0,
        };
        let mut effects = Vec::new();
        kernel.on_hit(&ctx, &mut effects);
        for effect in effects {
            out.push((actor, effect));
        }
    }
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        actor,
        actor,
        target,
        distance,
        colour,
        Some(attack),
        0,
    ) {
        for effect in crate::script::effects(crate::script::ScriptHook::Hit, &ctx, 0) {
            out.push((actor, effect));
        }
    }
    out
}

/// Collect the `on_absorb` reactions to damage a defence diverted (`4c:419-424`).
#[allow(clippy::too_many_arguments)]
fn collect_absorb_hooks(
    content: &dyn RulesContent,
    ladder: &Ladder,
    characters: &[Character],
    actor: u32,
    target: u32,
    distance: i32,
    colour: Colour,
    attack: &AttackView,
    absorbed: i32,
) -> Vec<(u32, HookEffect)> {
    let mut out = Vec::new();
    let Some(defender) = characters.iter().find(|character| character.id == target) else {
        return out;
    };
    for power in &defender.powers {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power,
            characters,
            owner: target,
            actor,
            target,
            distance,
            colour,
            attack: Some(attack),
            charged: absorbed,
        };
        let mut effects = Vec::new();
        kernel.on_absorb(&ctx, absorbed, &mut effects);
        for effect in effects {
            out.push((target, effect));
        }
    }
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        target,
        actor,
        target,
        distance,
        colour,
        Some(attack),
        absorbed,
    ) {
        for effect in crate::script::effects(crate::script::ScriptHook::Absorb, &ctx, absorbed) {
            out.push((target, effect));
        }
    }
    out
}

/// Plan a melee, ranged or power attack (`4c:952-979`, `4c:1074-1084`).
pub fn plan_attack(
    ladder: &Ladder,
    rules: &Rules,
    content: &dyn RulesContent,
    world: &World,
    characters: &[Character],
    actor: u32,
    action: &Action,
) -> AttackPlan {
    let Some(actor_character) = characters.iter().find(|character| character.id == actor) else {
        return AttackPlan::refused(actor, action.target, "no_such_actor");
    };
    let target = action.target;
    if target == actor {
        return AttackPlan::refused(actor, target, "self_target");
    }
    let Some(target_character) = characters.iter().find(|character| character.id == target) else {
        return AttackPlan::refused(actor, target, "no_such_target");
    };
    if target_character.dead {
        return AttackPlan::refused(actor, target, "target_is_dead");
    }
    let (Some(actor_pos), Some(target_pos)) = (world.position(actor), world.position(target))
    else {
        return AttackPlan::refused(actor, target, "not_on_the_map");
    };
    let distance = World::distance(actor_pos, target_pos);

    // The base attack, then every power's amendment. The fold happens before
    // range and band are read, because a power substitutes both (`4c:464`,
    // `4c:797`) and the reach it grants decides whether the attack is legal at
    // all.
    let mut view = match base_attack(content, ladder, actor_character, action, distance) {
        Ok(view) => view,
        Err(reason) => return AttackPlan::refused(actor, target, reason),
    };
    for modifier in weapon(content, actor_character)
        .map(|item| item.modifiers.clone())
        .unwrap_or_default()
    {
        if !modifier.applies_at(distance) {
            continue;
        }
        if modifier.armour_piercing {
            view.piercing = true;
        }
    }
    let mods = fold_attack(
        content,
        ladder,
        characters,
        actor,
        target,
        distance,
        Colour::Blck,
        &view,
        AttackMods::default(),
    );
    if let Some(trait_) = mods.trait_ {
        view.trait_ = trait_;
    }
    if let Some(rv) = mods.rv {
        view.rv = rv;
    }
    if let Some(hands) = mods.hands {
        view.hands = hands;
    }
    if let Some(reach) = mods.reach {
        view.reach = reach;
    }
    if let Some(over) = mods.damage_override {
        view.damage = over;
    }
    if mods.piercing {
        view.piercing = true;
    }

    // Range and reach. A weapon's reach is data (`02:02.4`); unarmed is adjacent.
    if action.kind == ActionKind::Ranged && distance > view.reach {
        return AttackPlan::refused(actor, target, "out_of_range");
    }
    if action.kind == ActionKind::Power && distance > view.reach {
        return AttackPlan::refused(actor, target, "out_of_range");
    }
    if matches!(action.kind, ActionKind::MeleeBash | ActionKind::MeleeSlash)
        && distance > view.reach
    {
        return AttackPlan::refused(actor, target, "not_adjacent");
    }

    // Line of sight and cover (02:02.12), for an attack across a distance.
    let mut cover_steps = 0;
    if matches!(action.kind, ActionKind::Ranged | ActionKind::Power) {
        match world.sight(actor_pos, target_pos, rules.cover_steps) {
            Sight::Blocked => return AttackPlan::refused(actor, target, "sight_blocked"),
            Sight::Covered { steps } => cover_steps = steps,
            Sight::Clear => {}
        }
    }

    // Situational row steps, from `4c:880`, the target's Dodge, elevation,
    // cover and the powers' own steps.
    let trait_ = view.trait_;
    let mut steps = actor_character.skill_steps(action.kind.tag());
    steps += rules.attack_penalty.steps_for(actor_character.moved_tiles);
    steps += target_character.dodge_steps;
    let elevation_steps = if rules.elevation_applies(elevation_target(action.kind)) {
        World::elevation_steps(
            i32::from(actor_pos.z),
            i32::from(target_pos.z),
            rules.elevation_cap,
        ) * rules.elevation_steps
    } else {
        0
    };
    steps += (elevation_steps + cover_steps).max(rules.cover_cap);
    steps += mods.steps;

    // An item's Rank Value divisor applies only inside its declared distance
    // (`02:02.4`, `4c:1484`).
    let mut rv_divisor = None;
    let mut demote: Vec<Colour> = Vec::new();
    if let Some(item) = weapon(content, actor_character) {
        for modifier in &item.modifiers {
            if !modifier.applies_at(distance) {
                continue;
            }
            if let Some((divisor, _round)) = modifier.rv_divisor {
                rv_divisor = Some(divisor);
            }
            demote.extend(modifier.demote.iter().copied());
        }
    }
    let effective_rv = match rv_divisor {
        Some(divisor) if divisor > 0 => view.rv / divisor,
        _ => view.rv,
    };

    AttackPlan {
        refusal: None,
        actor,
        target,
        trait_,
        base_band: ladder.band_index(effective_rv),
        steps,
        roll_penalty: actor_character.roll_penalty(),
        demote,
        punch_cap: action.declared.punch_cap,
        piercing: view.piercing,
        distance,
        reach: view.reach,
        cover_steps,
        elevation_steps,
        damage: view.damage,
        keep_two_of_three: mods.keep_two_of_three,
        notes: mods.notes,
        view,
    }
}

/// Resolve a melee, ranged or power attack (`4c:952-979`, `4c:1074-1084`).
fn resolve_attack(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32, action: &Action) {
    let plan = plan_attack(
        battle.ladder,
        battle.rules,
        battle.content,
        battle.world,
        battle.characters,
        actor,
        action,
    );
    if let Some(reason) = plan.refusal {
        refuse(battle, encounter, actor, action.kind, reason);
        return;
    }
    let target = plan.target;
    let Some(actor_index) = battle.index_of(actor) else {
        return;
    };

    // Fortune: the attacker commits points, which are spent whether the roll
    // lands or not (`4c:874` is post-roll; a declaration is spent on the roll).
    let mut commits = Vec::new();
    if action.fortune > 0 {
        let available = battle.characters[actor_index].fortune;
        if action.fortune > available {
            refuse(battle, encounter, actor, action.kind, "not_enough_fortune");
            return;
        }
        battle.characters[actor_index].fortune -= action.fortune;
        commits.push(FortuneCommit {
            actor,
            points: action.fortune,
        });
    }

    let outcome = roll_with(
        battle,
        plan.base_band,
        plan.steps,
        plan.roll_penalty,
        &mut commits,
        plan.punch_cap,
    );
    // A colour cap from a modifier demotes after Fortune, one step per listing.
    let mut colour = outcome.colour;
    for demoted in &plan.demote {
        if colour == *demoted {
            colour = colour.shift(-1);
        }
    }
    // `on_resist` and a script's `resist`: a defensive power may change the
    // colour the attack resolved at, before the row is read (`4c:658`, `4c:710`).
    // The borrows are scoped so the mutable borrows below are unambiguous.
    {
        let characters: &[Character] = battle.characters;
        let target_powers: Vec<crate::l1::powers::PowerInst> = characters
            .iter()
            .find(|character| character.id == target)
            .map(|character| character.powers.clone())
            .unwrap_or_default();
        for power in &target_powers {
            let Some(kernel) = battle.content.power_kernel(&power.id) else {
                continue;
            };
            let ctx = PowerCtx {
                ladder: battle.ladder,
                power,
                characters,
                owner: target,
                actor,
                target,
                distance: plan.distance,
                colour,
                attack: Some(&plan.view),
                charged: 0,
            };
            kernel.on_resist(&ctx, &mut colour);
        }
        if let Some(ctx) = script_ctx(
            battle.ladder,
            characters,
            target,
            actor,
            target,
            plan.distance,
            colour,
            Some(&plan.view),
            0,
        ) {
            colour = crate::script::colour(crate::script::ScriptHook::Resist, &ctx, colour);
        }
    }

    // 4c:670/714: a target with `Nullification` or `Reflection` reacts *before*
    // the power's effects apply, which is why this happens here rather than with
    // the other hit hooks.
    let mut negated = false;
    let mut halved = false;
    let mut reflected = false;
    let mut reactions: Vec<(u32, HookEffect)> = Vec::new();
    if let Some(used) = action.power.as_deref() {
        for (owner, power) in reacting_powers(battle.characters, actor, target) {
            if owner != target {
                continue;
            }
            let Some(kernel) = battle.content.power_kernel(&power.id) else {
                continue;
            };
            let used_power = battle
                .characters
                .iter()
                .find(|character| character.id == actor)
                .and_then(|character| character.power(used))
                .cloned()
                .unwrap_or_else(|| crate::l1::powers::PowerInst::new(used, 1));
            let ctx = PowerCtx {
                ladder: battle.ladder,
                power: &power,
                characters: battle.characters,
                owner: target,
                actor,
                target,
                distance: plan.distance,
                colour,
                attack: Some(&plan.view),
                charged: 0,
            };
            let mut effects = Vec::new();
            kernel.on_power_used(&ctx, &used_power, &mut effects);
            for effect in effects {
                match effect {
                    HookEffect::Nullify { rv, .. } => {
                        let band = battle.ladder.band_index(rv);
                        let rolled = roll_with(battle, band, 0, 0, &mut [], None);
                        match rolled.colour {
                            Colour::Blck => {
                                negated = true;
                                // 4c:670: the nullifier suffers half the target
                                // power's Rank Value.
                                reactions.push((
                                    target,
                                    HookEffect::Damage {
                                        target: EffectTarget::Self_,
                                        amount: Amount::constant((used_power.rv + 1) / 2),
                                        tag: None,
                                    },
                                ));
                            }
                            Colour::Red => halved = true,
                            _ => negated = true,
                        }
                    }
                    HookEffect::Reflect { rv, .. } => {
                        let band = battle.ladder.band_index(rv);
                        let rolled = roll_with(battle, band, 0, 0, &mut [], None);
                        match rolled.colour {
                            Colour::Blck => {}
                            Colour::Red => {
                                reflected = true;
                                halved = true;
                            }
                            _ => reflected = true,
                        }
                    }
                    other => reactions.push((target, other)),
                }
            }
        }
        if let Some(ctx) = script_ctx(
            battle.ladder,
            battle.characters,
            target,
            actor,
            target,
            plan.distance,
            colour,
            Some(&plan.view),
            0,
        ) {
            for effect in crate::script::effects(crate::script::ScriptHook::PowerUsed, &ctx, 0) {
                reactions.push((target, effect));
            }
        }
    }
    for (owner, effect) in reactions {
        apply_hook_effect(battle, encounter, owner, actor, target, effect);
    }
    if negated {
        battle.emit(Json::obj([
            ("event", Json::string("power_negated")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(actor))),
            ("target", Json::Num(i64::from(target))),
        ]));
        return;
    }

    // The hit hooks, collected before the rows are read so a colour shift
    // changes which row applies, and recollected once if it does.
    let mut hooks = collect_hit_hooks(
        battle.content,
        battle.ladder,
        battle.characters,
        actor,
        target,
        plan.distance,
        colour,
        &plan.view,
    );
    let shift: i32 = hooks
        .iter()
        .filter_map(|(_, effect)| match effect {
            HookEffect::ModifyColour(steps) => Some(*steps),
            _ => None,
        })
        .sum();
    if shift != 0 {
        colour = colour.shift(shift);
        hooks = collect_hit_hooks(
            battle.content,
            battle.ladder,
            battle.characters,
            actor,
            target,
            plan.distance,
            colour,
            &plan.view,
        );
    }

    let table = battle.content.outcome(action.kind).clone();
    let effects = table.effects(colour).to_vec();
    let nail = action.declared.nail && colour == Colour::Blue;
    battle.emit(Json::obj([
        ("event", Json::string(if nail { "nail" } else { "attack" })),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("target", Json::Num(i64::from(target))),
        ("kind", Json::string(action.kind.id())),
        (
            "power",
            match &action.power {
                Some(id) => Json::string(id),
                None => Json::Null,
            },
        ),
        ("roll", Json::Num(i64::from(outcome.d100))),
        ("band", Json::Num(outcome.band as i64)),
        ("steps", Json::Num(i64::from(plan.steps))),
        ("distance", Json::Num(i64::from(plan.distance))),
        (
            "colour",
            Json::string(crate::l2::actions::colour_name(colour)),
        ),
        (
            "rolled_colour",
            Json::string(crate::l2::actions::colour_name(outcome.rolled)),
        ),
        (
            "effects",
            Json::arr(effects.iter().map(|effect| Json::string(effect.id()))),
        ),
    ]));

    let damage_delta: i32 = hooks
        .iter()
        .filter_map(|(_, effect)| match effect {
            HookEffect::ModifyDamage(delta) => Some(*delta),
            _ => None,
        })
        .sum();
    let armour_delta: i32 = hooks
        .iter()
        .filter_map(|(_, effect)| match effect {
            HookEffect::ModifyArmour(delta) => Some(*delta),
            _ => None,
        })
        .sum();

    // Effects apply in row order, so the RNG draw order is fixed.
    for effect in effects {
        match effect {
            Effect::Damage => {
                let multiplier = if halved { 1 } else { 2 };
                apply_damage(
                    battle,
                    encounter,
                    actor,
                    target,
                    &plan,
                    colour,
                    damage_delta * multiplier / 2,
                    armour_delta,
                );
                if reflected {
                    apply_damage(
                        battle,
                        encounter,
                        target,
                        actor,
                        &plan,
                        colour,
                        damage_delta,
                        armour_delta,
                    );
                }
            }
            Effect::Knockdown => apply_knockdown(battle, encounter, actor, target),
            Effect::Knockout => apply_knockout(battle, encounter, actor, target),
            Effect::Dying => {
                let Some(index) = battle.index_of(target) else {
                    continue;
                };
                battle.characters[index].damage = 0;
                status::apply(
                    &mut battle.characters[index].conditions,
                    Condition::indefinite(ConditionKind::Dying, actor),
                );
                battle.emit(Json::obj([
                    ("event", Json::string("condition")),
                    ("actor", Json::Num(i64::from(target))),
                    ("condition", Json::string(ConditionKind::Dying.id())),
                    ("panels", Json::Num(-1)),
                ]));
            }
            Effect::DodgeSteps(_) => {}
        }
    }

    // Then the queued writes, minus the three the pipeline already consumed.
    for (owner, effect) in hooks {
        if matches!(
            effect,
            HookEffect::ModifyDamage(_) | HookEffect::ModifyArmour(_) | HookEffect::ModifyColour(_)
        ) {
            continue;
        }
        apply_hook_effect(battle, encounter, owner, actor, target, effect);
    }
}

/// The elevation target an action kind uses.
fn elevation_target(kind: ActionKind) -> ElevationTarget {
    match kind {
        ActionKind::Ranged | ActionKind::Power => ElevationTarget::Ranged,
        ActionKind::MeleeBash | ActionKind::MeleeSlash => ElevationTarget::Melee,
        _ => ElevationTarget::Perception,
    }
}

/// Apply one queued write (`02:02.4`, `03:03.7`).
///
/// `owner` is the entity whose power produced it, so `Self_` resolves to the
/// power's carrier and `Target` to the other party — which is the actor for a
/// reaction hook and the defender for an attack hook.
fn apply_hook_effect(
    battle: &mut Battle<'_>,
    encounter: &Encounter,
    owner: u32,
    actor: u32,
    target: u32,
    effect: HookEffect,
) {
    let other = if owner == actor { target } else { actor };
    let who = |which: EffectTarget| match which {
        EffectTarget::Self_ => owner,
        EffectTarget::Target => other,
    };
    match effect {
        HookEffect::Damage {
            target: which,
            amount,
            tag: _,
        } => {
            let id = who(which);
            let value = amount.roll(battle.rng);
            if let Some(index) = battle.index_of(id) {
                battle.characters[index].damage -= value;
                if battle.characters[index].damage <= 0 {
                    battle.characters[index].damage = 0;
                    status::apply(
                        &mut battle.characters[index].conditions,
                        Condition::indefinite(ConditionKind::Dying, actor),
                    );
                }
                battle.emit(Json::obj([
                    ("event", Json::string("power_damage")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(id))),
                    ("amount", Json::Num(i64::from(value))),
                ]));
            }
        }
        HookEffect::Heal {
            target: which,
            amount,
        } => {
            let id = who(which);
            let value = amount.roll(battle.rng);
            if let Some(index) = battle.index_of(id) {
                let max = battle.characters[index].max_damage;
                battle.characters[index].damage =
                    (battle.characters[index].damage + value).min(max);
                battle.emit(Json::obj([
                    ("event", Json::string("heal")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(id))),
                    ("amount", Json::Num(i64::from(value))),
                ]));
            }
        }
        HookEffect::ModifyDamage(_) | HookEffect::ModifyArmour(_) | HookEffect::ModifyColour(_) => {
            // Consumed by the attack pipeline before this point.
        }
        HookEffect::ModifyTraitSteps {
            target: which,
            trait_,
            steps,
        } => {
            let id = who(which);
            if let Some(index) = battle.index_of(id) {
                battle.characters[index]
                    .temporary_steps
                    .push(crate::l1::traits::TraitStep {
                        target: trait_,
                        steps,
                    });
            }
        }
        HookEffect::BoostTrait { trait_, rv, panels } => {
            let count = panels.roll(battle.rng);
            if let Some(index) = battle.index_of(owner) {
                battle.characters[index]
                    .trait_boosts
                    .push(crate::l1::character::TraitBoost {
                        trait_,
                        rv,
                        panels: count,
                    });
                battle.emit(Json::obj([
                    ("event", Json::string("trait_boost")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(owner))),
                    ("trait", Json::string(trait_.id())),
                    ("rv", Json::Num(i64::from(rv))),
                    ("panels", Json::Num(i64::from(count))),
                ]));
            }
        }
        HookEffect::ApplyCondition {
            target: which,
            kind,
            panels,
        } => {
            let id = who(which);
            let count = if panels.dice == crate::l0::Dice::Constant(-1) {
                -1
            } else {
                panels.roll(battle.rng)
            };
            if let Some(index) = battle.index_of(id) {
                status::apply(
                    &mut battle.characters[index].conditions,
                    if count < 0 {
                        Condition::indefinite(kind, owner)
                    } else {
                        Condition::new(kind, count, owner)
                    },
                );
                battle.emit(Json::obj([
                    ("event", Json::string("condition")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(id))),
                    ("condition", Json::string(kind.id())),
                    ("panels", Json::Num(i64::from(count))),
                ]));
            }
        }
        HookEffect::RemoveCondition {
            target: which,
            kind,
        } => {
            let id = who(which);
            if let Some(index) = battle.index_of(id) {
                status::remove(&mut battle.characters[index].conditions, kind);
            }
        }
        HookEffect::GrantFortune {
            target: which,
            amount,
        } => {
            let id = who(which);
            let value = amount.roll(battle.rng);
            if let Some(index) = battle.index_of(id) {
                battle.characters[index].fortune += value;
            }
        }
        HookEffect::SetCharged(_amount) => {
            // The charge is a property of the power instance, which M2's save
            // does not yet carry per power; the event records the intent so a
            // later revision can persist it. Absorption's heal-or-attack split
            // is applied in the same resolution, so nothing is lost today.
            battle.emit(Json::obj([
                ("event", Json::string("charged")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(owner))),
            ]));
        }
        HookEffect::Resist {
            trait_,
            rv,
            on,
            then,
        } => {
            let band = match rv {
                Some(value) => battle.ladder.band_index(value),
                None => {
                    let Some(index) = battle.index_of(owner) else {
                        return;
                    };
                    battle.characters[index]
                        .effective(battle.ladder)
                        .trait_band(trait_)
                }
            };
            let penalty = match battle.index_of(owner) {
                Some(index) => battle.characters[index].roll_penalty(),
                None => 0,
            };
            let rolled = roll_with(battle, band, 0, penalty, &mut [], None);
            if rolled.colour <= on {
                for nested in then {
                    apply_hook_effect(battle, encounter, owner, actor, target, nested);
                }
            }
        }
        HookEffect::Displace {
            target: which,
            tiles,
        } => {
            let id = who(which);
            push_back(battle, encounter, owner, id, tiles);
        }
        HookEffect::Knockdown { target: which } => {
            knock_down(battle, encounter, owner, who(which), false);
        }
        HookEffect::Knockout {
            target: which,
            panels,
        } => {
            let id = who(which);
            let count = panels.roll(battle.rng);
            if let Some(index) = battle.index_of(id) {
                status::apply(
                    &mut battle.characters[index].conditions,
                    Condition::new(ConditionKind::KnockedOut, count, owner),
                );
            }
        }
        HookEffect::Teleport {
            target: which,
            tiles,
        } => {
            let id = who(which);
            teleport(battle, encounter, id, tiles);
        }
        HookEffect::Nullify { .. } | HookEffect::Reflect { .. } => {
            // Consumed by the power-resolution step in `resolve_attack`.
        }
        HookEffect::Regenerate { amount } => {
            let value = amount.roll(battle.rng);
            if let Some(index) = battle.index_of(owner) {
                let max = battle.characters[index].max_damage;
                battle.characters[index].damage =
                    (battle.characters[index].damage + value).min(max);
                battle.emit(Json::obj([
                    ("event", Json::string("regenerate")),
                    ("panel", Json::Num(i64::from(encounter.panel))),
                    ("actor", Json::Num(i64::from(owner))),
                    ("amount", Json::Num(i64::from(value))),
                ]));
            }
        }
    }
}

/// Move an entity deterministically away from where it stands (`4c:807`).
fn teleport(battle: &mut Battle<'_>, encounter: &Encounter, id: u32, tiles: i32) {
    let Some(from) = battle.world.position(id) else {
        return;
    };
    let directions: [(i32, i32); 8] = [
        (1, 0),
        (-1, 0),
        (0, 1),
        (0, -1),
        (1, 1),
        (1, -1),
        (-1, 1),
        (-1, -1),
    ];
    let step = (tiles.max(1) + 2) / 3;
    let start = (id as usize) % directions.len();
    for offset in 0..directions.len() {
        let (dx, dy) = directions[(start + offset) % directions.len()];
        let to = Position {
            x: from.x + dx * step,
            y: from.y + dy * step,
            z: from.z,
        };
        if battle.world.in_bounds(to.x, to.y)
            && battle.world.occupant(to.x, to.y).is_none()
            && battle
                .world
                .tile(to.x, to.y)
                .is_some_and(|tile| tile.blocking <= 0)
        {
            battle.world.place(id, to);
            battle.emit(Json::obj([
                ("event", Json::string("teleport")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(id))),
                ("x", Json::Num(i64::from(to.x))),
                ("y", Json::Num(i64::from(to.y))),
            ]));
            return;
        }
    }
}

/// Push a target back, or knock it down when there is nowhere to go.
fn push_back(battle: &mut Battle<'_>, encounter: &Encounter, actor: u32, id: u32, tiles: i32) {
    let Some(at) = battle.world.position(id) else {
        return;
    };
    let directions: [(i32, i32); 8] = [
        (1, 0),
        (-1, 0),
        (0, 1),
        (0, -1),
        (1, 1),
        (1, -1),
        (-1, 1),
        (-1, -1),
    ];
    let start = (actor as usize) % directions.len();
    for offset in 0..directions.len() {
        let (dx, dy) = directions[(start + offset) % directions.len()];
        let to = Position {
            x: at.x + dx * tiles.max(1),
            y: at.y + dy * tiles.max(1),
            z: at.z,
        };
        if battle.world.in_bounds(to.x, to.y)
            && battle.world.occupant(to.x, to.y).is_none()
            && battle
                .world
                .tile(to.x, to.y)
                .is_some_and(|tile| tile.blocking <= 0)
        {
            battle.world.place(id, to);
            battle.emit(Json::obj([
                ("event", Json::string("displaced")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
                ("target", Json::Num(i64::from(id))),
                ("x", Json::Num(i64::from(to.x))),
                ("y", Json::Num(i64::from(to.y))),
            ]));
            return;
        }
    }
    knock_down(battle, encounter, actor, id, false);
}

/// Apply the damage pipeline (`4c:1074-1084`).
///
/// The pipeline returns the absorbed/inflicted split because `Force Field` and
/// `Absorption` need to know what was diverted (`02:02.5`).
#[allow(clippy::too_many_arguments)]
fn apply_damage(
    battle: &mut Battle<'_>,
    encounter: &Encounter,
    actor: u32,
    target: u32,
    plan: &AttackPlan,
    colour: Colour,
    damage_delta: i32,
    armour_delta: i32,
) {
    let Some(actor_index) = battle.index_of(actor) else {
        return;
    };
    let Some(target_index) = battle.index_of(target) else {
        return;
    };
    // The raw damage: a power's `damage_override` when it set one, else the
    // weapon's, then every offensive power's `modify_damage` (`4c:587`).
    let mut raw = plan.damage;
    if plan.view.shape == AttackShape::Power {
        raw = plan.view.damage.max(plan.view.rv);
    }
    raw = fold_damage(
        battle.content,
        battle.ladder,
        battle.characters,
        actor,
        target,
        plan.distance,
        colour,
        &plan.view,
        raw,
    );
    raw += damage_delta;
    // Worn armour, then the defender's power armour, then the defensive powers'
    // own armour (`4c:1084`, `D5`).
    let worn: i32 = battle.characters[target_index]
        .armour
        .iter()
        .filter_map(|id| battle.content.item(id))
        .map(|item| item.armour_rv())
        .sum();
    let mut armour = worn + battle.characters[target_index].power_armor;
    armour = fold_armor(
        battle.content,
        battle.ladder,
        battle.characters,
        actor,
        target,
        plan.distance,
        colour,
        &plan.view,
        armour,
    );
    armour += armour_delta;
    let result = damage::apply(raw, armour, plan.piercing);
    battle.characters[target_index].damage -= result.inflicted;
    battle.emit(Json::obj([
        ("event", Json::string("damage")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("target", Json::Num(i64::from(target))),
        ("raw", Json::Num(i64::from(result.raw))),
        ("armour", Json::Num(i64::from(result.armour))),
        ("inflicted", Json::Num(i64::from(result.inflicted))),
        (
            "remaining",
            Json::Num(i64::from(battle.characters[target_index].damage.max(0))),
        ),
    ]));
    // The split is what an absorbing defence reacts to (`4c:419-424`,
    // `4c:595-597`).
    let absorbed = (raw - result.inflicted).max(0);
    if absorbed > 0 {
        let hooks = collect_absorb_hooks(
            battle.content,
            battle.ladder,
            battle.characters,
            actor,
            target,
            plan.distance,
            colour,
            &plan.view,
            absorbed,
        );
        for (owner, effect) in hooks {
            apply_hook_effect(battle, encounter, owner, actor, target, effect);
        }
    }
    // 4c:1161: when Damage reaches 0 the character is dying.
    if battle.characters[target_index].damage <= 0 {
        battle.characters[target_index].damage = 0;
        status::apply(
            &mut battle.characters[target_index].conditions,
            Condition::indefinite(ConditionKind::Dying, actor),
        );
    }
    let _ = actor_index;
}

/// The Pound result: a Brawn contest, then a Fortitude roll (`4c:1175`).
fn apply_knockdown(battle: &mut Battle<'_>, encounter: &Encounter, actor: u32, target: u32) {
    let (Some(actor_index), Some(target_index)) = (battle.index_of(actor), battle.index_of(target))
    else {
        return;
    };
    let attacker_brawn = battle.characters[actor_index]
        .effective(battle.ladder)
        .trait_rv(Trait::Brawn);
    let defender_brawn = battle.characters[target_index]
        .effective(battle.ladder)
        .trait_rv(Trait::Brawn);
    if attacker_brawn <= defender_brawn {
        return;
    }
    let band = battle.characters[target_index]
        .effective(battle.ladder)
        .trait_band(Trait::Fortitude);
    let penalty = battle.characters[target_index].roll_penalty();
    let outcome = roll_with(battle, band, 0, penalty, &mut [], None);
    match outcome.colour {
        Colour::Blck => knock_down(battle, encounter, actor, target, true),
        Colour::Red => knock_down(battle, encounter, actor, target, false),
        _ => {}
    }
}

/// The Concuss result: a Brawn-versus-Fortitude contest, then a roll
/// (`4c:1161`).
fn apply_knockout(battle: &mut Battle<'_>, encounter: &Encounter, actor: u32, target: u32) {
    let (Some(actor_index), Some(target_index)) = (battle.index_of(actor), battle.index_of(target))
    else {
        return;
    };
    let attacker_brawn = battle.characters[actor_index]
        .effective(battle.ladder)
        .trait_rv(Trait::Brawn);
    let defender_fortitude = battle.characters[target_index]
        .effective(battle.ladder)
        .trait_rv(Trait::Fortitude);
    if attacker_brawn <= defender_fortitude {
        return;
    }
    let band = battle.characters[target_index]
        .effective(battle.ladder)
        .trait_band(Trait::Fortitude);
    let penalty = battle.characters[target_index].roll_penalty();
    let outcome = roll_with(battle, band, 0, penalty, &mut [], None);
    if outcome.colour == Colour::Blck {
        let panels = battle.rng.die(10) as i32;
        status::apply(
            &mut battle.characters[target_index].conditions,
            Condition::new(ConditionKind::KnockedOut, panels, actor),
        );
        battle.emit(Json::obj([
            ("event", Json::string("condition")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(target))),
            ("condition", Json::string(ConditionKind::KnockedOut.id())),
            ("panels", Json::Num(i64::from(panels))),
        ]));
    }
}

/// Knock a defender down, optionally back (`4c:1175`).
fn knock_down(battle: &mut Battle<'_>, encounter: &Encounter, actor: u32, target: u32, push: bool) {
    let Some(target_index) = battle.index_of(target) else {
        return;
    };
    status::apply(
        &mut battle.characters[target_index].conditions,
        Condition::new(ConditionKind::KnockedDown, 1, actor),
    );
    let mut pushed = false;
    if push {
        if let Some(at) = battle.world.position(target) {
            // The knock-back direction is determined by actor id, so it is
            // reproducible without a vector (`D16`'s rule for scatter).
            let directions: [(i32, i32); 8] = [
                (1, 0),
                (-1, 0),
                (0, 1),
                (0, -1),
                (1, 1),
                (1, -1),
                (-1, 1),
                (-1, -1),
            ];
            let start = (actor as usize) % directions.len();
            for step in 0..directions.len() {
                let (dx, dy) = directions[(start + step) % directions.len()];
                let to = Position {
                    x: at.x + dx * battle.rules.knockback_tiles,
                    y: at.y + dy * battle.rules.knockback_tiles,
                    z: at.z,
                };
                if battle.world.in_bounds(to.x, to.y) && battle.world.occupant(to.x, to.y).is_none()
                {
                    battle.world.place(target, to);
                    pushed = true;
                    break;
                }
            }
        }
    }
    battle.emit(Json::obj([
        ("event", Json::string("condition")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(target))),
        ("condition", Json::string(ConditionKind::KnockedDown.id())),
        ("panels", Json::Num(1)),
        ("pushed", Json::Bool(pushed)),
    ]));
}

/// Resolve a Dodge (`4c:1044-1053`).
fn resolve_dodge(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32, action: &Action) {
    let Some(index) = battle.index_of(actor) else {
        return;
    };
    let band = battle.characters[index]
        .effective(battle.ladder)
        .trait_band(Trait::Coordination);
    let mut steps = battle.characters[index].skill_steps(action.kind.tag());
    steps += battle
        .rules
        .dodge_penalty
        .steps_for(battle.characters[index].moved_tiles);
    let penalty = battle.characters[index].roll_penalty();
    let outcome = roll_with(battle, band, steps, penalty, &mut [], None);
    let table = battle.content.outcome(ActionKind::Dodge).clone();
    let effects = table.effects(outcome.colour).to_vec();
    let mut applied = 0;
    for effect in &effects {
        if let Effect::DodgeSteps(steps) = effect {
            applied += steps;
        }
    }
    battle.characters[index].dodge_steps += applied;
    battle.emit(Json::obj([
        ("event", Json::string("dodge")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("roll", Json::Num(i64::from(outcome.d100))),
        (
            "colour",
            Json::string(crate::l2::actions::colour_name(outcome.colour)),
        ),
        ("steps", Json::Num(i64::from(applied))),
    ]));
}

/// The movement allowance a character has this panel, after its powers.
///
/// Returns the allowance and whether the character may ignore blocking terrain
/// (`Wall-Crawling` does not ignore it; `Burrowing` passes through anything at
/// or below the power's Rank Value, `4c:452`).
fn movement_profile(
    content: &dyn RulesContent,
    ladder: &Ladder,
    rules: &Rules,
    characters: &[Character],
    vehicles: &[Vehicle],
    actor: u32,
) -> (i32, Option<i32>, i32) {
    let Some(character) = characters.iter().find(|character| character.id == actor) else {
        return (0, None, 0);
    };
    let base = rules.movement_tiles(character.effective(ladder).trait_rv(Trait::Coordination));
    let mut tiles = base;
    // A vehicle replaces the driver's allowance with its Velocity (`4c:1338`).
    if let Some(vehicle) = vehicles
        .iter()
        .find(|vehicle| vehicle.operator == actor && !vehicle.destroyed)
    {
        tiles = tiles.max(vehicle.velocity_tiles(vehicle_power_bonus(characters, actor)));
    }
    let mut material = None;
    let mut bonus = 0;
    for power in &character.powers {
        let Some(kernel) = content.power_kernel(&power.id) else {
            continue;
        };
        let ctx = PowerCtx {
            ladder,
            power,
            characters,
            owner: actor,
            actor,
            target: actor,
            distance: 0,
            colour: Colour::Blck,
            attack: None,
            charged: 0,
        };
        if let Some(profile) = kernel.movement(&ctx, actor) {
            if let Some(allowance) = profile.tiles {
                if allowance > tiles {
                    tiles = allowance;
                }
            }
            if let Some(limit) = profile.max_material {
                material = Some(limit);
            }
        }
        if let Some(economy) = kernel.action_economy(&ctx, actor) {
            bonus += economy.bonus_tiles;
        }
    }
    // Tier-2 scripts declare movement and economy the same way a template does
    // (`03:03.7`), so a scripted power can grant a mode the kernel set does not.
    if let Some(ctx) = script_ctx(
        ladder,
        characters,
        actor,
        actor,
        actor,
        0,
        Colour::Blck,
        None,
        0,
    ) {
        if let Some(profile) = crate::script::movement(crate::script::ScriptHook::Movement, &ctx) {
            if let Some(allowance) = profile.tiles {
                tiles = tiles.max(allowance);
            }
            if let Some(limit) = profile.max_material {
                material = Some(limit);
            }
        }
        if let Some(economy) =
            crate::script::economy(crate::script::ScriptHook::ActionEconomy, &ctx)
        {
            bonus += economy.bonus_tiles;
        }
    }
    (tiles + bonus, material, base)
}

/// Resolve movement (`4c:897-914`, `AD-33` occupancy, `4c:452` burrowing).
fn resolve_move(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32, action: &Action) {
    let Some(index) = battle.index_of(actor) else {
        return;
    };
    let Some(destination) = action.destination else {
        refuse(battle, encounter, actor, ActionKind::Move, "no_destination");
        return;
    };
    let Some(from) = battle.world.position(actor) else {
        refuse(battle, encounter, actor, ActionKind::Move, "not_on_the_map");
        return;
    };
    if !battle.world.in_bounds(destination.x, destination.y) {
        refuse(battle, encounter, actor, ActionKind::Move, "off_the_map");
        return;
    }
    let (allowance, material, _base) = movement_profile(
        battle.content,
        battle.ladder,
        battle.rules,
        battle.characters,
        battle.vehicles,
        actor,
    );
    let to = Position {
        x: destination.x,
        y: destination.y,
        z: battle
            .world
            .tile(destination.x, destination.y)
            .map(|tile| tile.level)
            .unwrap_or(0),
    };
    let distance = World::distance(from, to);
    if distance == 0 {
        refuse(battle, encounter, actor, ActionKind::Move, "already_there");
        return;
    }
    if battle.characters[index].moved_tiles + distance > allowance {
        refuse(
            battle,
            encounter,
            actor,
            ActionKind::Move,
            "beyond_movement",
        );
        return;
    }
    // The path must be passable: a blocking tile is a wall (`02:02.6`) unless a
    // power lets the character through it (`4c:452`).
    for (x, y) in crate::l3::supercover(from.x, from.y, to.x, to.y) {
        if (x, y) == (from.x, from.y) {
            continue;
        }
        match battle.world.tile(x, y) {
            Some(tile) if tile.blocking <= 0 => {}
            Some(tile) if material.is_some_and(|limit| tile.material <= limit) => {}
            _ => {
                refuse(battle, encounter, actor, ActionKind::Move, "path_blocked");
                return;
            }
        }
    }
    // Occupancy is one entity per tile, and entering an occupied tile is a
    // contest resolved through the Brawn tables (`AD-33`, `4c:1013-1042`).
    if let Some(occupant) = battle.world.occupant(to.x, to.y) {
        if occupant != actor {
            let band = battle.characters[index]
                .effective(battle.ladder)
                .trait_band(Trait::Brawn);
            let penalty = battle.characters[index].roll_penalty();
            let outcome = roll_with(battle, band, 0, penalty, &mut [], None);
            if outcome.colour < Colour::Blue {
                refuse(
                    battle,
                    encounter,
                    actor,
                    ActionKind::Move,
                    "occupant_contests",
                );
                return;
            }
            // Success displaces the occupant into an adjacent free tile, or
            // knocks them down when there is nowhere to go.
            displace(battle, encounter, actor, occupant, to);
        }
    }
    battle.world.place(actor, to);
    // A vehicle the actor drives moves with them (`4c:1359` counts its travel).
    if let Some(vehicle) = battle
        .vehicles
        .iter_mut()
        .find(|vehicle| vehicle.operator == actor && !vehicle.destroyed)
    {
        vehicle.moved_tiles += distance;
        battle.world.place(vehicle.id, to);
    }
    battle.characters[index].moved_tiles += distance;
    battle.characters[index].dodge_steps += battle.rules.dodge_penalty.steps_for(distance);
    battle.emit(Json::obj([
        ("event", Json::string("move")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("x", Json::Num(i64::from(to.x))),
        ("y", Json::Num(i64::from(to.y))),
        ("distance", Json::Num(i64::from(distance))),
    ]));
}

/// Move an occupant out of the way, or knock them down (`AD-33`).
fn displace(
    battle: &mut Battle<'_>,
    encounter: &Encounter,
    actor: u32,
    occupant: u32,
    from: Position,
) {
    let directions: [(i32, i32); 8] = [
        (1, 0),
        (-1, 0),
        (0, 1),
        (0, -1),
        (1, 1),
        (1, -1),
        (-1, 1),
        (-1, -1),
    ];
    for (dx, dy) in directions {
        let to = Position {
            x: from.x + dx,
            y: from.y + dy,
            z: from.z,
        };
        if !battle.world.in_bounds(to.x, to.y) {
            continue;
        }
        if battle
            .world
            .tile(to.x, to.y)
            .is_none_or(|tile| tile.blocking > 0)
        {
            continue;
        }
        if battle.world.occupant(to.x, to.y).is_some() {
            continue;
        }
        battle.world.place(occupant, to);
        battle.emit(Json::obj([
            ("event", Json::string("displaced")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(actor))),
            ("target", Json::Num(i64::from(occupant))),
            ("x", Json::Num(i64::from(to.x))),
            ("y", Json::Num(i64::from(to.y))),
        ]));
        return;
    }
    if let Some(index) = battle.index_of(occupant) {
        status::apply(
            &mut battle.characters[index].conditions,
            Condition::new(ConditionKind::KnockedDown, 1, actor),
        );
        battle.emit(Json::obj([
            ("event", Json::string("condition")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(occupant))),
            ("condition", Json::string(ConditionKind::KnockedDown.id())),
            ("panels", Json::Num(1)),
        ]));
    }
}

/// The end-of-panel step: durations, dying, boosts and per-panel modifiers
/// (`4c:1161`, `4c:815`).
fn resolve_end_of_panel(encounter: &mut Encounter, battle: &mut Battle<'_>) {
    let ladder = battle.ladder;
    let mut deaths = Vec::new();
    for index in 0..battle.characters.len() {
        let character = &mut battle.characters[index];
        let expired = status::tick(&mut character.conditions);
        let id = character.id;
        for kind in expired {
            battle.events.push(Json::obj([
                ("event", Json::string("condition_end")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(id))),
                ("condition", Json::string(kind.id())),
            ]));
        }
        for trait_ in character.tick_boosts() {
            battle.events.push(Json::obj([
                ("event", Json::string("boost_end")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(id))),
                ("trait", Json::string(trait_.id())),
            ]));
        }
        if character.is_dying() && !character.dead {
            // "Your Fortitude Rank Value drops by one Row Step ... until it
            // reaches Rank Value 0, at which point you are dead."
            character.dying_steps += 1;
            let effective = character.effective(ladder);
            if effective.trait_band(Trait::Fortitude) == 0 {
                character.dead = true;
                deaths.push(character.id);
            }
        }
        character.moved_tiles = 0;
        character.dodge_steps = 0;
        // A power's row-step shift lasts for the resolution that made it, never
        // across a panel boundary (`02:02.4`'s read-only snapshot discipline).
        character.temporary_steps.clear();
    }
    for vehicle in battle.vehicles.iter_mut() {
        vehicle.moved_tiles = 0;
    }
    for id in deaths {
        battle.events.push(Json::obj([
            ("event", Json::string("death")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(id))),
        ]));
        battle.world.remove(id);
    }
}

/// The `Vehicle` power's modifier layer (`4c:823`, `D17`): half the power's Rank
/// Value, rounded up, applied to Durability, Handling and Velocity.
fn vehicle_power_bonus(characters: &[Character], operator: u32) -> i32 {
    characters
        .iter()
        .find(|character| character.id == operator)
        .and_then(|character| character.power("wsp.power.vehicle"))
        .map_or(0, |power| power.half_rv_up())
}

/// Charge a vehicle's passengers for the distance it travelled (`4c:1359`).
fn charge_passengers(
    battle: &mut Battle<'_>,
    encounter: &Encounter,
    vehicle_index: usize,
    moved_tiles: i32,
) {
    let damage = crate::l2::vehicle::passenger_damage(moved_tiles);
    if damage <= 0 {
        return;
    }
    let passengers = battle.vehicles[vehicle_index].passengers.clone();
    for passenger in passengers {
        let Some(index) = battle.index_of(passenger) else {
            continue;
        };
        battle.characters[index].damage -= damage;
        if battle.characters[index].damage <= 0 {
            battle.characters[index].damage = 0;
            status::apply(
                &mut battle.characters[index].conditions,
                Condition::indefinite(ConditionKind::Dying, 0),
            );
        }
        battle.emit(Json::obj([
            ("event", Json::string("passenger_damage")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(passenger))),
            ("amount", Json::Num(i64::from(damage))),
        ]));
    }
}

/// Resolve a ram (`4c:1344-1357`).
///
/// `D15`: one pass, attacker first, so two identical vehicles trade symmetric
/// damage. The target dodges on Coordination if on foot, or on the vehicle's
/// Handling if driving.
fn resolve_ram(encounter: &mut Encounter, battle: &mut Battle<'_>, actor: u32, action: &Action) {
    let Some(vehicle_index) = battle
        .vehicles
        .iter()
        .position(|vehicle| vehicle.operator == actor && !vehicle.destroyed)
    else {
        refuse(battle, encounter, actor, ActionKind::Ram, "no_vehicle");
        return;
    };
    let target = action.target;
    let bonus = vehicle_power_bonus(battle.characters, actor);

    // The target's defence roll: Red or better gets out of the way (4c:1346).
    let target_vehicle = battle
        .vehicles
        .iter()
        .position(|vehicle| vehicle.id == target && !vehicle.destroyed);
    let (band, penalty) = match target_vehicle {
        Some(index) => {
            let handling = battle.vehicles[index].handling_rv(vehicle_power_bonus(
                battle.characters,
                battle.vehicles[index].operator,
            ));
            (battle.ladder.band_index(handling), 0)
        }
        None => match battle.index_of(target) {
            Some(index) => (
                battle.characters[index]
                    .effective(battle.ladder)
                    .trait_band(Trait::Coordination),
                battle.characters[index].roll_penalty(),
            ),
            None => {
                refuse(battle, encounter, actor, ActionKind::Ram, "no_such_target");
                return;
            }
        },
    };
    let defence = roll_with(battle, band, 0, penalty, &mut [], None);
    let struck_damage = battle.vehicles[vehicle_index].collision_damage(bonus);
    let moved = battle.vehicles[vehicle_index].moved_tiles;
    if defence.colour != Colour::Blck {
        battle.emit(Json::obj([
            ("event", Json::string("ram_avoided")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(actor))),
            ("target", Json::Num(i64::from(target))),
            (
                "colour",
                Json::string(crate::l2::actions::colour_name(defence.colour)),
            ),
        ]));
        return;
    }
    match target_vehicle {
        Some(index) => {
            let other_durability = battle.vehicles[index].durability;
            let dealt = battle.vehicles[index].damage(struck_damage);
            let taken = battle.vehicles[vehicle_index].damage(other_durability);
            charge_passengers(battle, encounter, vehicle_index, moved);
            charge_passengers(battle, encounter, index, moved);
            battle.emit(Json::obj([
                ("event", Json::string("collision")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
                ("target", Json::Num(i64::from(target))),
                ("struck", Json::Num(i64::from(dealt))),
                ("striker", Json::Num(i64::from(taken))),
                ("struck_kind", Json::string("vehicle")),
            ]));
        }
        None => {
            let armour = match battle.index_of(target) {
                Some(index) => {
                    let worn: i32 = battle.characters[index]
                        .armour
                        .iter()
                        .filter_map(|id| battle.content.item(id))
                        .map(|item| item.armour_rv())
                        .sum();
                    worn + battle.characters[index].power_armor
                }
                None => 0,
            };
            if let Some(index) = battle.index_of(target) {
                battle.characters[index].damage -= struck_damage;
                if battle.characters[index].damage <= 0 {
                    battle.characters[index].damage = 0;
                    status::apply(
                        &mut battle.characters[index].conditions,
                        Condition::indefinite(ConditionKind::Dying, actor),
                    );
                }
            }
            let striker_damage = Struck::Character { armour }.damage_to_striker();
            battle.vehicles[vehicle_index].damage(striker_damage);
            charge_passengers(battle, encounter, vehicle_index, moved);
            battle.emit(Json::obj([
                ("event", Json::string("collision")),
                ("panel", Json::Num(i64::from(encounter.panel))),
                ("actor", Json::Num(i64::from(actor))),
                ("target", Json::Num(i64::from(target))),
                ("struck", Json::Num(i64::from(struck_damage))),
                ("striker", Json::Num(i64::from(striker_damage))),
                ("struck_kind", Json::string("character")),
            ]));
        }
    }
    // 4c:1361: a vehicle involved in a collision moves no further that turn.
    battle.vehicles[vehicle_index].moved_tiles = 0;
    if battle.vehicles[vehicle_index].destroyed {
        battle.emit(Json::obj([
            ("event", Json::string("vehicle_destroyed")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            (
                "actor",
                Json::Num(i64::from(battle.vehicles[vehicle_index].id)),
            ),
        ]));
        battle.world.remove(battle.vehicles[vehicle_index].id);
    }
}

/// Resolve a manoeuvre, and the crash a failure produces (`4c:1314-1334`).
fn resolve_manoeuvre(
    encounter: &mut Encounter,
    battle: &mut Battle<'_>,
    actor: u32,
    action: &Action,
) {
    let Some(vehicle_index) = battle
        .vehicles
        .iter()
        .position(|vehicle| vehicle.operator == actor && !vehicle.destroyed)
    else {
        refuse(
            battle,
            encounter,
            actor,
            ActionKind::Manoeuvre,
            "no_vehicle",
        );
        return;
    };
    let difficulty = action
        .manoeuvre
        .as_deref()
        .and_then(Difficulty::from_id)
        .unwrap_or(Difficulty::Easy);
    let handling =
        battle.vehicles[vehicle_index].handling_rv(vehicle_power_bonus(battle.characters, actor));
    let band = battle.ladder.band_index(handling);
    let outcome = roll_with(battle, band, 0, 0, &mut [], None);
    if outcome.colour >= difficulty.required() {
        battle.emit(Json::obj([
            ("event", Json::string("manoeuvre")),
            ("panel", Json::Num(i64::from(encounter.panel))),
            ("actor", Json::Num(i64::from(actor))),
            ("difficulty", Json::string(difficulty.id())),
            (
                "colour",
                Json::string(crate::l2::actions::colour_name(outcome.colour)),
            ),
        ]));
        return;
    }
    // 4c:1325: the crash's severity is the operator's Coordination roll. With
    // nothing else authored in the sector, the vehicle hits the ground
    // (`4c:1334`, Material Value 50).
    let (severity_band, penalty) = match battle.index_of(actor) {
        Some(index) => (
            battle.characters[index]
                .effective(battle.ladder)
                .trait_band(Trait::Coordination),
            battle.characters[index].roll_penalty(),
        ),
        None => (0, 0),
    };
    let severity = roll_with(battle, severity_band, 0, penalty, &mut [], None);
    let moved = battle.vehicles[vehicle_index].moved_tiles;
    battle.vehicles[vehicle_index].damage(GROUND_MATERIAL);
    charge_passengers(battle, encounter, vehicle_index, moved);
    battle.emit(Json::obj([
        ("event", Json::string("crash")),
        ("panel", Json::Num(i64::from(encounter.panel))),
        ("actor", Json::Num(i64::from(actor))),
        ("difficulty", Json::string(difficulty.id())),
        (
            "severity",
            Json::string(crate::l2::actions::colour_name(severity.colour)),
        ),
        ("material", Json::Num(i64::from(GROUND_MATERIAL))),
    ]));
    battle.vehicles[vehicle_index].moved_tiles = 0;
    if battle.vehicles[vehicle_index].destroyed {
        battle.world.remove(battle.vehicles[vehicle_index].id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l1::character::Character;
    use crate::l2::actions::OutcomeTable;
    use crate::rules::Rules;
    use crate::tables;

    struct TestContent {
        items: Vec<Item>,
        outcomes: Vec<OutcomeTable>,
    }

    impl RulesContent for TestContent {
        fn item(&self, id: &str) -> Option<&Item> {
            self.items.iter().find(|item| item.id == id)
        }
        fn outcome(&self, action: ActionKind) -> &OutcomeTable {
            self.outcomes
                .iter()
                .find(|table| table.action == action)
                .expect("a table per action")
        }
        fn skill(&self, _id: &str) -> Option<&Skill> {
            None
        }
        fn power_kernel(&self, id: &str) -> Option<&dyn PowerKernel> {
            crate::l1::powers::kernel(id)
        }
    }

    fn content() -> TestContent {
        TestContent {
            items: vec![
                Item::parse(
                    "wsp.item.bat",
                    &Json::from_text(
                        r#"{"hands":"one","melee":"bashing","reach":{"kind":"adjacent"}}"#,
                    ),
                )
                .expect("bat"),
                Item::parse(
                    "wsp.item.rifle",
                    &Json::from_text(
                        r#"{"hands":"ranged","damage":15,"reach":{"kind":"fixed","tiles":24}}"#,
                    ),
                )
                .expect("rifle"),
            ],
            outcomes: ActionKind::ALL
                .into_iter()
                .map(OutcomeTable::canonical)
                .collect(),
        }
    }

    struct Harness {
        encounter: Encounter,
        world: World,
        characters: Vec<Character>,
        vehicles: Vec<Vehicle>,
        rng: Rng,
        events: Vec<Json>,
        rules: Rules,
        content: TestContent,
    }

    impl Harness {
        fn new(seed: u64) -> Harness {
            let mut world = World::new(6, 4);
            for x in 0..6 {
                world.set_tile(
                    x,
                    3,
                    crate::l3::Tile {
                        level: 0,
                        blocking: 2,
                        terrain: 1,
                        material: 30,
                    },
                );
            }
            let mut characters = vec![
                Character::authored(
                    1,
                    "hero",
                    "",
                    0,
                    [20, 20, 20, 20, 20, 20, 20],
                    10,
                    5,
                    vec![],
                    vec![],
                    vec![],
                ),
                Character::authored(
                    2,
                    "thug",
                    "",
                    1,
                    [20, 20, 20, 20, 20, 20, 20],
                    10,
                    5,
                    vec![],
                    vec![],
                    vec![],
                ),
            ];
            characters[0].weapon = Some("wsp.item.bat".to_string());
            world.place(1, Position { x: 0, y: 0, z: 0 });
            world.place(2, Position { x: 1, y: 0, z: 0 });
            Harness {
                encounter: Encounter::new("wsp.encounter.test", "wsp.sector.test"),
                world,
                vehicles: Vec::new(),
                characters,
                rng: Rng::new(seed),
                events: Vec::new(),
                rules: Rules::default(),
                content: content(),
            }
        }

        /// Run one panel. The borrows are disjoint fields of `self`, which is
        /// what lets the encounter and the battlefield be mutable together.
        fn step(&mut self) {
            let mut battle = Battle {
                ladder: tables::CAMPAIGN_LADDER,
                rules: &self.rules,
                content: &self.content,
                rng: &mut self.rng,
                world: &mut self.world,
                characters: &mut self.characters,
                vehicles: &mut self.vehicles,
                events: &mut self.events,
            };
            commit(&mut self.encounter, &mut battle);
        }

        fn has_event(&self, name: &str) -> bool {
            self.events
                .iter()
                .any(|event| event.get("event").and_then(Json::as_str) == Some(name))
        }

        fn has_reason(&self, reason: &str) -> bool {
            self.events
                .iter()
                .any(|event| event.get("reason").and_then(Json::as_str) == Some(reason))
        }
    }

    #[test]
    fn a_melee_attack_lands_damage_and_is_deterministic() {
        let mut a = Harness::new(5);
        a.encounter
            .queue(1, Action::attack(ActionKind::MeleeBash, 2));
        a.encounter
            .queue(2, Action::attack(ActionKind::MeleeBash, 1));
        a.step();
        let mut b = Harness::new(5);
        b.encounter
            .queue(1, Action::attack(ActionKind::MeleeBash, 2));
        b.encounter
            .queue(2, Action::attack(ActionKind::MeleeBash, 1));
        b.step();
        assert!(a.has_event("attack"));
        assert_eq!(a.events, b.events, "the same seed replays identically");
        assert_eq!(a.characters, b.characters);
        assert_eq!(a.world, b.world);
        assert_eq!(a.rng, b.rng);
    }

    #[test]
    fn a_dodge_gives_attackers_a_row_step_penalty() {
        let mut a = Harness::new(11);
        a.encounter.queue(2, Action::simple(ActionKind::Dodge));
        a.step();
        assert!(
            a.characters[1].dodge_steps <= 0,
            "a dodge never helps the attacker"
        );
    }

    #[test]
    fn a_ranged_attack_out_of_reach_is_refused_with_a_code() {
        let mut a = Harness::new(3);
        a.characters[0].weapon = Some("wsp.item.bat".to_string());
        a.world.place(2, Position { x: 5, y: 0, z: 0 });
        a.encounter.queue(1, Action::attack(ActionKind::Ranged, 2));
        a.step();
        assert!(a.has_reason("out_of_range"));
    }

    #[test]
    fn a_wall_blocks_a_shot() {
        let mut a = Harness::new(3);
        a.characters[0].weapon = Some("wsp.item.rifle".to_string());
        // Both stand on the row that has the wall between them.
        a.world.place(1, Position { x: 0, y: 3, z: 0 });
        a.world.place(2, Position { x: 5, y: 3, z: 0 });
        a.encounter.queue(1, Action::attack(ActionKind::Ranged, 2));
        a.step();
        assert!(a.has_reason("sight_blocked"));
    }

    #[test]
    fn movement_costs_the_distance_and_respects_the_allowance() {
        // A Coordination of 20 allows 6 tiles, so a four-tile move lands.
        let mut a = Harness::new(1);
        a.encounter
            .queue(1, Action::move_to(Position { x: 4, y: 1, z: 0 }));
        a.step();
        assert_eq!(a.world.position(1), Some(Position { x: 4, y: 1, z: 0 }));

        // A Coordination of 1 allows 3 tiles, so a five-tile move is refused.
        let mut b = Harness::new(1);
        b.characters[0].traits[Trait::Coordination.index()] = 1;
        b.encounter
            .queue(1, Action::move_to(Position { x: 5, y: 1, z: 0 }));
        b.step();
        assert_eq!(b.world.position(1), Some(Position { x: 0, y: 0, z: 0 }));
        assert!(b.has_reason("beyond_movement"));

        // The same character may still move within the allowance.
        let mut c = Harness::new(1);
        c.characters[0].traits[Trait::Coordination.index()] = 1;
        c.encounter
            .queue(1, Action::move_to(Position { x: 1, y: 1, z: 0 }));
        c.step();
        assert_eq!(c.world.position(1), Some(Position { x: 1, y: 1, z: 0 }));
    }

    #[test]
    fn entering_an_occupied_tile_is_a_brawn_contest() {
        // The mover must roll Blue or better; with a high Brawn it usually does,
        // and the occupant is displaced. Assert that the outcome is one of the
        // two documented branches, never a silent overlap.
        let mut a = Harness::new(1);
        a.encounter
            .queue(1, Action::move_to(Position { x: 1, y: 0, z: 0 }));
        a.step();
        let (at_one, at_other) = (a.world.occupant(1, 0), a.world.position(2));
        assert!(
            at_one == Some(1) || at_one == Some(2),
            "exactly one entity occupies the tile"
        );
        if at_one == Some(1) {
            assert!(
                at_other.is_some(),
                "the occupant was displaced, not deleted"
            );
        } else {
            assert!(a.has_reason("occupant_contests"));
        }
    }

    #[test]
    fn the_encounter_ends_when_a_side_is_wiped_out() {
        let mut a = Harness::new(1);
        a.characters[1].dead = true;
        a.step();
        assert!(a.encounter.finished);
        assert_eq!(a.encounter.winner, Some(0));
    }

    #[test]
    fn dying_advances_one_fortitude_row_step_per_panel() {
        let mut a = Harness::new(1);
        status::apply(
            &mut a.characters[1].conditions,
            Condition::indefinite(ConditionKind::Dying, 1),
        );
        a.step();
        // The panel may have killed and removed the character; either way dying
        // advanced while it lived.
        assert!(a
            .events
            .iter()
            .any(|event| { event.get("event").and_then(Json::as_str) == Some("panel_end") }));
    }

    #[test]
    fn a_knockdown_applies_the_condition() {
        let mut a = Harness::new(1);
        // The block ends the `Battle`'s borrows, so the assertion can read the
        // characters again.
        {
            let mut battle = Battle {
                ladder: tables::CAMPAIGN_LADDER,
                rules: &a.rules,
                content: &a.content,
                rng: &mut a.rng,
                world: &mut a.world,
                characters: &mut a.characters,
                vehicles: &mut a.vehicles,
                events: &mut a.events,
            };
            knock_down(&mut battle, &a.encounter, 1, 2, false);
        }
        assert!(a.characters[1]
            .conditions
            .iter()
            .any(|condition| condition.kind == ConditionKind::KnockedDown));
    }

    #[test]
    fn a_down_character_may_only_stand() {
        let mut a = Harness::new(1);
        status::apply(
            &mut a.characters[0].conditions,
            Condition::new(ConditionKind::KnockedDown, 5, 2),
        );
        a.encounter
            .queue(1, Action::attack(ActionKind::MeleeBash, 2));
        a.step();
        assert!(a.has_reason("condition_prevents_action"));
        // Standing is always legal and clears it.
        a.encounter.queue(1, Action::simple(ActionKind::Stand));
        a.step();
        assert!(!a.characters[0]
            .conditions
            .iter()
            .any(|condition| condition.kind == ConditionKind::KnockedDown));
    }
}
