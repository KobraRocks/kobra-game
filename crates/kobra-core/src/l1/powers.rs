//! L1 — powers: kernels plus the effect DSL's hook set (02:02.4, AD-16).
//!
//! A power is a **kernel**: a fixed hook set, registered by id, with the
//! canonical 4C powers as Rust kernels and content-defined powers as `Template`
//! kernels driven by the effect DSL (`AD-16`, `02:02.4`). The hook set is a
//! public API from day one, so it is small, documented here, and pinned by tests
//! rather than discovered.
//!
//! Three decisions shape this module, and each answers a failure mode the
//! architecture names:
//!
//! - **Reads are a frozen view, writes are a queue.** A hook receives a
//!   [`PowerCtx`] — an immutable view over the ladder, the power instance and the
//!   entities — and appends [`HookEffect`]s. The encounter applies them after the
//!   hook returns. That is the same contract the Tier-2 script host uses
//!   (`03:03.7`), so a kernel and a script cannot tear a resolution in half, and
//!   the core validates every write either of them asks for.
//! - **One effect vocabulary.** A kernel's `on_hit`, a DSL `then` list, an
//!   authored outcome row and a script's queued command all produce the same
//!   [`HookEffect`] values. That is what makes the coverage test (`tests/dsl.rs`)
//!   a comparison of behaviour rather than of syntax.
//! - **Dice are declared, not drawn.** A hook cannot roll: the RNG belongs to the
//!   resolver, and a hook that drew would make the draw order depend on which
//!   powers were equipped (`AD-6`, `02:02.9` rule 3). An effect that needs a
//!   duration carries an [`Amount`], and the encounter draws it in a fixed order.
//!
//! # The 51 canonical powers, and Magic
//!
//! The two selection tables union to **50** powers (`4c:309-335`, `4c:343-394`),
//! and `Amplified Elemental/Energy Control` (`4c:430-434`) appears in neither, so
//! **51** powers are expressible as DSL templates and compared against their
//! kernels. `Magic` (`4c:646-650`) is a 52nd kernel with no effect of its own —
//! it instantiates a `Template` per spell at the Magic Rank Value — so it is
//! covered by its own test rather than by the expressiveness test.
//!
//! `CANONICAL_POWER_COUNT` is the one place the number lives, and a test asserts
//! the registry has exactly that many entries, so a power cannot be dropped or
//! double-counted silently.

use crate::json::Json;
use crate::l0::{Amount, Colour, Ladder};
use crate::l1::character::Character;
use crate::l1::items::Hands;
use crate::l1::status::ConditionKind;
use crate::l1::traits::Trait;

/// The number of canonical powers the coverage test defines as DSL templates.
///
/// The union of both selection tables (50) plus `Amplified Elemental/Energy
/// Control`, which is in neither (`02:02.4`).
pub const CANONICAL_POWER_COUNT: usize = 51;

/// A chosen power, as it lives on a character.
///
/// `params` carries the selections the spec leaves to the player — the element
/// or energy type, the animal type, whether Growth or Shrinking was taken. They
/// are strings because they are content: `D24` says a naming mismatch must never
/// be able to break a lookup, and a parameter the engine does not know is a
/// validator question, not a parse error.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct PowerInst {
    /// The id, e.g. `wsp.power.force-field`.
    pub id: String,
    /// The power's Rank Value.
    pub rv: i32,
    /// The selections the player made at acquisition.
    pub params: std::collections::BTreeMap<String, String>,
}

impl PowerInst {
    /// A power with no parameters.
    pub fn new(id: &str, rv: i32) -> PowerInst {
        PowerInst {
            id: id.to_string(),
            rv,
            params: std::collections::BTreeMap::new(),
        }
    }

    /// The same power with one parameter set.
    pub fn with(mut self, key: &str, value: &str) -> PowerInst {
        self.params.insert(key.to_string(), value.to_string());
        self
    }

    /// A parameter, or the empty string.
    pub fn param(&self, key: &str) -> &str {
        self.params.get(key).map(String::as_str).unwrap_or("")
    }

    /// Half the Rank Value, rounded up — the spec's most common modifier
    /// (`4c:662`, `4c:823`).
    pub fn half_rv_up(&self) -> i32 {
        (self.rv + 1) / 2
    }
}

/// Which entity an effect applies to.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum EffectTarget {
    /// The character whose power or script produced the effect.
    Self_,
    /// The other party in the resolution.
    Target,
}

impl EffectTarget {
    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            EffectTarget::Self_ => "self",
            EffectTarget::Target => "target",
        }
    }

    /// The target a content id names.
    pub fn from_id(id: &str) -> Option<EffectTarget> {
        match id {
            "self" => Some(EffectTarget::Self_),
            "target" => Some(EffectTarget::Target),
            _ => None,
        }
    }
}

/// One queued write a kernel or a script asked for (`02:02.4`, `03:03.7`).
///
/// The encounter applies these in order after the hook returns, so every write is
/// a command the core owns and can validate. Durations are [`Amount`]s, not
/// integers, because a hook may not draw from the RNG.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum HookEffect {
    /// Damage outside the attack pipeline, tagged with an effect tag (`D25`) so
    /// `Absorption` and `Nullification` can match it.
    Damage {
        /// Who is hurt.
        target: EffectTarget,
        /// How much.
        amount: Amount,
        /// The effect tag, when the source declares one.
        tag: Option<String>,
    },
    /// Recover Damage points, capped at the maximum (`4c:423`, `4c:718`).
    Heal {
        /// Who is healed.
        target: EffectTarget,
        /// How much.
        amount: Amount,
    },
    /// A delta to the damage the current attack inflicts.
    ModifyDamage(i32),
    /// A delta to the armour the current attack meets.
    ModifyArmour(i32),
    /// A row-step shift on a trait for the rest of the resolution.
    ModifyTraitSteps {
        /// Whose trait.
        target: EffectTarget,
        /// Which trait.
        trait_: Trait,
        /// How many rows, signed.
        steps: i32,
    },
    /// A temporary Rank Value boost to a trait (`4c:815`).
    BoostTrait {
        /// Which trait.
        trait_: Trait,
        /// The Rank Value it is boosted to.
        rv: i32,
        /// How long the boost lasts.
        panels: Amount,
    },
    /// A colour shift applied by a power after the roll.
    ModifyColour(i32),
    /// Apply or refresh a condition with a declared duration; `-1` is
    /// indefinite.
    ApplyCondition {
        /// Who is affected.
        target: EffectTarget,
        /// Which condition.
        kind: ConditionKind,
        /// How long.
        panels: Amount,
    },
    /// End a condition early.
    RemoveCondition {
        /// Who is affected.
        target: EffectTarget,
        /// Which condition.
        kind: ConditionKind,
    },
    /// Add Fortune points (`4c:666`).
    GrantFortune {
        /// Who gains.
        target: EffectTarget,
        /// How many.
        amount: Amount,
    },
    /// Store the value a power carries between panels (`4c:421-424`).
    SetCharged(Amount),
    /// A resisted rider: roll the trait and apply `then` when the colour is at or
    /// below `on` (`4c:597`, `4c:684`).
    Resist {
        /// The trait that resists.
        trait_: Trait,
        /// The Rank Value to roll at, when the source names one instead of a
        /// trait (`4c:490`: "roll d% using the Rank Value of this power").
        rv: Option<i32>,
        /// The highest colour that still fails.
        on: Colour,
        /// What happens on a failure.
        then: Vec<HookEffect>,
    },
    /// Push a target back, in tiles.
    Displace {
        /// Who is pushed.
        target: EffectTarget,
        /// How far.
        tiles: i32,
    },
    /// Knock a target down (`4c:1175`).
    Knockdown {
        /// Who is knocked down.
        target: EffectTarget,
    },
    /// Knock a target out.
    Knockout {
        /// Who is knocked out.
        target: EffectTarget,
        /// How long.
        panels: Amount,
    },
    /// Move a target up to `tiles` tiles, deterministically (`4c:807-811`).
    Teleport {
        /// Who moves.
        target: EffectTarget,
        /// The maximum distance.
        tiles: i32,
    },
    /// The three-way nullification outcome (`4c:670`), rolled by the resolver at
    /// `rv`.
    Nullify {
        /// The power the attempt targets.
        target: EffectTarget,
        /// The Rank Value the attempt rolls at.
        rv: i32,
    },
    /// Reflect the resolved power back at its originator (`4c:714`), rolled by
    /// the resolver at `rv`.
    Reflect {
        /// Whose power is reflected back.
        target: EffectTarget,
        /// The Rank Value the attempt rolls at.
        rv: i32,
    },
    /// Recover Damage as an action (`4c:718`).
    Regenerate {
        /// How much.
        amount: Amount,
    },
}

impl HookEffect {
    /// The stable content id, for an event.
    pub const fn id(&self) -> &'static str {
        match self {
            HookEffect::Damage { .. } => "damage",
            HookEffect::Heal { .. } => "heal",
            HookEffect::ModifyDamage(_) => "modify_damage",
            HookEffect::ModifyArmour(_) => "modify_armour",
            HookEffect::ModifyTraitSteps { .. } => "modify_trait_steps",
            HookEffect::BoostTrait { .. } => "boost_trait",
            HookEffect::ModifyColour(_) => "modify_colour",
            HookEffect::ApplyCondition { .. } => "apply_status",
            HookEffect::RemoveCondition { .. } => "remove_status",
            HookEffect::GrantFortune { .. } => "grant_fortune",
            HookEffect::SetCharged(_) => "set_charged",
            HookEffect::Resist { .. } => "resist",
            HookEffect::Displace { .. } => "displace",
            HookEffect::Knockdown { .. } => "knockdown",
            HookEffect::Knockout { .. } => "knockout",
            HookEffect::Teleport { .. } => "teleport",
            HookEffect::Nullify { .. } => "nullify",
            HookEffect::Reflect { .. } => "reflect",
            HookEffect::Regenerate { .. } => "regenerate",
        }
    }
}

/// The shape of the attack a hook is looking at.
///
/// A local enum rather than `l2::actions::ActionKind`, because L1 may not depend
/// on L2 (`02:02.1`); the encounter converts at the boundary.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum AttackShape {
    /// A blunt close-combat attack.
    MeleeBash,
    /// A sharp close-combat attack.
    MeleeSlash,
    /// An attack across a distance.
    Ranged,
    /// A power use.
    Power,
    /// A vehicle manoeuvre or ram.
    Manoeuvre,
}

impl AttackShape {
    /// Whether the attack is made in close combat.
    pub const fn is_melee(self) -> bool {
        matches!(self, AttackShape::MeleeBash | AttackShape::MeleeSlash)
    }

    /// Whether the attack crosses a distance.
    pub const fn is_ranged(self) -> bool {
        matches!(self, AttackShape::Ranged | AttackShape::Power)
    }
}

/// A frozen description of the attack under resolution.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct AttackView {
    /// What kind of attack it is.
    pub shape: AttackShape,
    /// The trait the roll uses.
    pub trait_: Trait,
    /// The Rank Value the attack rolls at.
    pub rv: i32,
    /// The raw damage before armour.
    pub damage: i32,
    /// The distance in tiles.
    pub distance: i32,
    /// The attacker's reach in tiles.
    pub reach: i32,
    /// Effect tags the attack carries (`D25`).
    pub tags: Vec<String>,
    /// Whether the attack halves the target's armour (`4c:1480`).
    pub piercing: bool,
    /// How many hands the weapon needs, for the damage bonus (`4c:1074`).
    pub hands: Hands,
}

impl AttackView {
    /// A close-combat attack.
    pub fn melee(shape: AttackShape, trait_: Trait, rv: i32, damage: i32) -> AttackView {
        AttackView {
            shape,
            trait_,
            rv,
            damage,
            distance: 1,
            reach: 1,
            tags: Vec::new(),
            piercing: false,
            hands: Hands::None,
        }
    }

    /// Whether the attack carries a tag.
    pub fn has_tag(&self, tag: &str) -> bool {
        !tag.is_empty() && self.tags.iter().any(|candidate| candidate == tag)
    }
}

/// The modifier set a power may change (`02:02.4`).
///
/// It carries the whole attack rather than a number because `Growth/Shrinking`
/// and `Claws` modify *other entities'* attacks — the reason the architecture
/// gives for passing the full `Attack` (`02:02.4`).
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct AttackMods {
    /// A trait substitution (`Telekinesis`, `4c:797`).
    pub trait_: Option<Trait>,
    /// A substituted attack Rank Value (`Claws`, `4c:464`).
    pub rv: Option<i32>,
    /// Damage set outright, rather than by a delta (`4c:502`).
    pub damage_override: Option<i32>,
    /// A substituted hand count, which is also the damage bonus (`4c:464`).
    pub hands: Option<Hands>,
    /// A substituted reach, in tiles (`4c:494`, `4c:706`).
    pub reach: Option<i32>,
    /// Situational row steps.
    pub steps: i32,
    /// An armour delta against this attack.
    pub armour: i32,
    /// Whether the attack now halves armour.
    pub piercing: bool,
    /// A success-level cap (`4c:1088-1090`).
    pub colour_cap: Option<Colour>,
    /// Extra attacks this panel.
    pub extra_attacks: i32,
    /// The attack rolls three dice and keeps two (`4c:432`).
    pub keep_two_of_three: bool,
    /// Reasons, for a tooltip (`02:02.11` rule 2). Presentation, never compared
    /// by the coverage test.
    pub notes: Vec<String>,
}

impl AttackMods {
    /// The mechanical fields only: everything the coverage test compares.
    pub fn mechanical(&self) -> AttackMods {
        AttackMods {
            notes: Vec::new(),
            ..self.clone()
        }
    }

    /// Note a reason without changing the numbers.
    pub fn note(&mut self, note: &str) {
        self.notes.push(note.to_string());
    }
}

/// How a power lets a character move (`4c:452`, `4c:570`, `4c:825`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum MoveMode {
    /// Ordinary ground movement.
    Walk,
    /// Through the air.
    Fly,
    /// A single leap.
    Leap,
    /// At superhuman speed.
    Run,
    /// Through earth or stone.
    Burrow,
    /// Across walls and ceilings.
    Cling,
    /// Through water.
    Swim,
}

impl MoveMode {
    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            MoveMode::Walk => "walk",
            MoveMode::Fly => "fly",
            MoveMode::Leap => "leap",
            MoveMode::Run => "run",
            MoveMode::Burrow => "burrow",
            MoveMode::Cling => "cling",
            MoveMode::Swim => "swim",
        }
    }

    /// The mode a content id names.
    pub fn from_id(id: &str) -> Option<MoveMode> {
        [
            MoveMode::Walk,
            MoveMode::Fly,
            MoveMode::Leap,
            MoveMode::Run,
            MoveMode::Burrow,
            MoveMode::Cling,
            MoveMode::Swim,
        ]
        .into_iter()
        .find(|mode| mode.id() == id)
    }
}

/// A movement allowance a power grants.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct MoveProfile {
    /// The mode.
    pub mode: MoveMode,
    /// Tiles per panel, when the power replaces the ordinary allowance.
    pub tiles: Option<i32>,
    /// The material rank the mode can pass through (`4c:452`).
    pub max_material: Option<i32>,
}

/// What a power does to a character's action economy (`4c:560`, `4c:545`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct ActionEconomy {
    /// Attacks per panel.
    pub attacks_per_panel: i32,
    /// Movement tiles granted on top of the ordinary allowance.
    pub bonus_tiles: i32,
}

/// The per-panel plan an `on_own_turn` hook may amend (`02:02.4`).
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct TurnPlan {
    /// Effects the turn produces.
    pub effects: Vec<HookEffect>,
    /// Attacks granted on top of the ordinary one.
    pub extra_attacks: i32,
    /// Whether the power consumed the panel's action.
    pub consumes_action: bool,
    /// Reasons, for the panel log.
    pub notes: Vec<String>,
}

impl TurnPlan {
    /// Note a reason.
    pub fn note(&mut self, line: &str) {
        self.notes.push(line.to_string());
    }
}

/// What acquisition changed, so creation can report it (`02:02.11` rule 2).
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct AcquirePlan {
    /// Flat arithmetic bonuses to stored traits.
    pub trait_bonuses: Vec<(Trait, i32)>,
    /// A bonus to the Lifestyle Rank Value.
    pub lifestyle_bonus: i32,
    /// A bonus to the Repute score.
    pub repute_bonus: i32,
    /// Extra skills granted.
    pub bonus_skills: i32,
    /// A skill step value set outright (`4c:640`, `D13`).
    pub skill_step_override: Option<i32>,
    /// Fortune granted at acquisition (`4c:666`).
    pub grant_fortune: i32,
    /// Powers granted outright (`Extra Body Parts`).
    pub grants_powers: Vec<PowerInst>,
    /// Sub-entities the power declares but the campaign must author (`D30`).
    pub content_requests: Vec<String>,
    /// Lines for the character's creation log.
    pub log: Vec<String>,
}

impl AcquirePlan {
    /// Note a creation-log line.
    pub fn note(&mut self, line: &str) {
        self.log.push(line.to_string());
    }

    /// Grant a flat trait bonus.
    pub fn add_trait(&mut self, trait_: Trait, bonus: i32) {
        self.trait_bonuses.push((trait_, bonus));
    }
}

/// The immutable view a hook reads (`02:02.4`).
///
/// No RNG, no mutable state, no clock. `owner` is the entity carrying
/// [`Self::power`], which is not always the actor: the encounter iterates the
/// *target's* powers through the same hook when a defensive power reacts, which
/// is how `Growth` and `Shrinking` change an attacker's roll (`4c:618-620`).
pub struct PowerCtx<'a> {
    /// The ladder, for every row-step and band read.
    pub ladder: &'a Ladder,
    /// The power instance whose kernel is running.
    pub power: &'a PowerInst,
    /// Every entity.
    pub characters: &'a [Character],
    /// The entity carrying `power`.
    pub owner: u32,
    /// The acting entity.
    pub actor: u32,
    /// The other party, or `0`.
    pub target: u32,
    /// The distance between them in tiles.
    pub distance: i32,
    /// The colour the roll produced, when there is one.
    pub colour: Colour,
    /// The attack under resolution, when there is one.
    pub attack: Option<&'a AttackView>,
    /// The value this power has stored, e.g. `Absorption`'s charge.
    pub charged: i32,
}

impl PowerCtx<'_> {
    /// A character by id.
    pub fn character(&self, id: u32) -> Option<&Character> {
        self.characters.iter().find(|character| character.id == id)
    }

    /// The stored Rank Value of an entity's trait.
    ///
    /// Stored, not effective: the spec's `+10` clauses read the character's
    /// printed Rank Value (`4c:470`), and reading the effective value would make
    /// a power's floor depend on its own result.
    pub fn stored_rv(&self, who: u32, trait_: Trait) -> i32 {
        self.character(who).map_or(1, |c| c.stored(trait_))
    }

    /// A power the owner carries, by id.
    pub fn owner_power(&self, id: &str) -> Option<&PowerInst> {
        self.character(self.owner)
            .and_then(|character| character.power(id))
    }

    /// Whether the owner is the acting entity.
    pub fn owner_is_actor(&self) -> bool {
        self.owner == self.actor
    }

    /// Whether the owner is the other party.
    pub fn owner_is_target(&self) -> bool {
        self.owner == self.target && self.owner != self.actor
    }
}

/// `max(this power's Rank Value, the trait's stored value + bonus)` — the shape
/// a third of the spec's powers share (`4c:470`, `4c:474`, `4c:751`, `4c:801`).
pub fn rank_floor(ctx: &PowerCtx<'_>, trait_: Trait, bonus: i32) -> i32 {
    ctx.power.rv.max(ctx.stored_rv(ctx.owner, trait_) + bonus)
}

/// The fixed hook set (`02:02.4`, `AD-16`).
///
/// Every method has a default that changes nothing, so a content-bearing power
/// implements only the constraint it enforces at acquisition and the combat
/// powers implement only the hooks they touch.
pub trait PowerKernel: Sync {
    /// The canonical id.
    fn id(&self) -> &'static str;

    /// Run once, when the power is acquired.
    fn on_acquire(&self, _ctx: &PowerCtx<'_>, _plan: &mut AcquirePlan) {}

    /// Fold this power into an effective trait read (`4c:470`).
    fn modify_trait(&self, _ctx: &PowerCtx<'_>, _who: u32, _trait_: Trait, value: i32) -> i32 {
        value
    }

    /// Fold this power into an attack (`4c:464`, `4c:797`).
    fn modify_attack(&self, _ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        mods
    }

    /// Fold this power into the damage an attack inflicts (`4c:502`).
    fn modify_damage(&self, _ctx: &PowerCtx<'_>, value: i32) -> i32 {
        value
    }

    /// Fold this power into the armour an attack meets (`4c:448`, `4c:688`).
    fn modify_armor(&self, _ctx: &PowerCtx<'_>, value: i32) -> i32 {
        value
    }

    /// The movement this power grants (`4c:570`, `4c:755`).
    fn movement(&self, _ctx: &PowerCtx<'_>, _who: u32) -> Option<MoveProfile> {
        None
    }

    /// The action economy this power grants (`4c:560`).
    fn action_economy(&self, _ctx: &PowerCtx<'_>, _who: u32) -> Option<ActionEconomy> {
        None
    }

    /// What happens after a hit lands (`4c:652`, `4c:682`).
    fn on_hit(&self, _ctx: &PowerCtx<'_>, _out: &mut Vec<HookEffect>) {}

    /// A resistance roll a power changes (`4c:658`, `4c:708`).
    fn on_resist(&self, _ctx: &PowerCtx<'_>, _colour: &mut Colour) {}

    /// What happens at the start of the owner's own panel (`4c:718`).
    fn on_own_turn(&self, _ctx: &PowerCtx<'_>, _plan: &mut TurnPlan) {}

    /// Damage diverted rather than inflicted (`4c:419-424`).
    fn on_absorb(&self, _ctx: &PowerCtx<'_>, _absorbed: i32, _out: &mut Vec<HookEffect>) {}

    /// A reaction to another entity's power use (`4c:670`, `4c:714`).
    fn on_power_used(&self, _ctx: &PowerCtx<'_>, _power: &PowerInst, _out: &mut Vec<HookEffect>) {}
}

/// Resolves a power id to a kernel: canonical first, then a compiled template.
///
/// The content registry implements this, which is what makes a data mod's power a
/// first-class kernel rather than a special case in the encounter (`AD-16`).
pub trait PowerSource {
    /// The kernel for a power id.
    fn kernel_for(&self, id: &str) -> Option<&dyn PowerKernel>;
}

/// A source that knows only the canonical powers.
///
/// The character factory uses it when no registry is in scope — a test's
/// three-record sheet, or a character built before content loads.
pub struct CanonicalPowers;

impl PowerSource for CanonicalPowers {
    fn kernel_for(&self, id: &str) -> Option<&dyn PowerKernel> {
        kernel(id)
    }
}

/// The canonical id of a power from its kebab-case name (`D24`).
pub fn power_id(name: &str) -> String {
    format!("wsp.power.{name}")
}

/// The kernel for a power id, canonical or content-defined.
///
/// A canonical id resolves to its Rust kernel; anything else is a `Template` and
/// resolves through a [`PowerSource`]. `None` is a validator problem, never a
/// panic (`AD-15`).
pub fn kernel(id: &str) -> Option<&'static dyn PowerKernel> {
    id.strip_prefix("wsp.power.").and_then(canonical::kernel)
}

/// Whether an id names a canonical power.
pub fn is_canonical(id: &str) -> bool {
    kernel(id).is_some()
}

/// Every canonical power id, in the order the coverage test enumerates them.
pub fn canonical_ids() -> Vec<&'static str> {
    canonical::IDS.to_vec()
}

/// Read a power instance from content (`03:03.3`).
///
/// The record is `{ "id": …, "rv": …, "params": { … } }`. A missing `rv` is a
/// content error rather than a default: a power with no Rank Value does nothing,
/// and an author who wrote one meant something by it.
pub fn parse_power(value: &Json) -> Result<PowerInst, &'static str> {
    let id = value
        .get("id")
        .and_then(Json::as_str)
        .ok_or("a power needs an id")?
        .to_string();
    let rv = value
        .get("rv")
        .and_then(Json::as_i64)
        .ok_or("a power needs a rank value")? as i32;
    if rv < 1 {
        return Err("a power's rank value must be positive");
    }
    let mut params = std::collections::BTreeMap::new();
    if let Some(Json::Obj(fields)) = value.get("params") {
        for (key, param) in fields {
            params.insert(
                key.clone(),
                param
                    .as_str()
                    .ok_or("a power parameter must be a string")?
                    .to_string(),
            );
        }
    }
    Ok(PowerInst { id, rv, params })
}

/// The movement tiers the spec prints for Flight, Superleap and Superspeed
/// (`4c:572-588`, `4c:730-746`, `4c:757-774`), in 1 m tiles.
///
/// One table, because the three powers print the same one: a shared shape is one
/// implementation (`02:02.4`). The source counts legacy sectors, converted once
/// (`AD-33`, `D33`).
pub fn speed_tiles(rv: i32) -> i32 {
    let sectors = match rv {
        i32::MIN..=2 => 1,
        3..=5 => 2,
        6..=9 => 3,
        10..=19 => 4,
        20..=29 => 5,
        30..=39 => 6,
        40..=49 => 7,
        50..=74 => 8,
        75..=99 => 9,
        100..=149 => 10,
        150..=999 => 15,
        // 4c:587: the character can circle the world in a single turn. The map is
        // the bound, and a 300-tile allowance reaches every edge of one.
        _ => 300,
    };
    sectors * 3
}

/// The swim tiers for Water Native (`4c:833-837`), in 1 m tiles.
pub fn swim_tiles(rv: i32) -> i32 {
    match rv {
        i32::MIN..=2 => 3,
        3..=29 => 6,
        _ => 9,
    }
}

/// The attacks per panel Fast Attack grants (`4c:562-568`).
pub fn fast_attack_count(rv: i32) -> i32 {
    match rv {
        i32::MIN..=29 => 2,
        30..=49 => 3,
        _ => 4,
    }
}

/// `ceil(value / 10)`, the spec's paralysis and trait-boost duration (`4c:684`).
pub fn tenths_up(value: i32) -> i32 {
    (value.max(0) + 9) / 10
}

/// Apply an acquisition plan to a character.
///
/// The one place an `on_acquire` result becomes state, so a power cannot apply
/// its bonus twice by being handled in two call sites.
pub fn apply_acquire_plan(character: &mut Character, plan: &AcquirePlan) {
    for (trait_, bonus) in &plan.trait_bonuses {
        character.traits[trait_.index()] += bonus;
    }
    character.lifestyle_rv = (character.lifestyle_rv + plan.lifestyle_bonus).max(1);
    character.repute += plan.repute_bonus;
    character.fortune += plan.grant_fortune;
    if let Some(steps) = plan.skill_step_override {
        // D13: the override replaces the skill's bonus, it does not stack.
        if let Some(skill) = character.skills.first_mut() {
            skill.steps = steps;
        }
    }
    for power in &plan.grants_powers {
        character.acquire_power(power.clone());
    }
    for line in &plan.log {
        character.creation_log.push(line.clone());
    }
}

mod canonical;
