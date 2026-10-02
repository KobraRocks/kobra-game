//! The canonical powers as Rust kernels (`AD-16`, `02:02.4`).
//!
//! Each kernel is small on purpose: most of the spec's powers are one clause, and
//! the ones that are not get a named struct rather than a flag. The registry is
//! an array so the id, the kernel and the coverage test's enumeration cannot
//! drift apart — a test asserts the array has exactly
//! [`CANONICAL_POWER_COUNT`](super::CANONICAL_POWER_COUNT) entries and that every
//! id is unique.
//!
//! `Magic` is the 52nd kernel and is deliberately **not** in [`IDS`]: it has no
//! effect of its own (it instantiates a `Template` per spell), so it is covered
//! by its own test rather than by the expressiveness test (`02:02.4`).

use super::{
    fast_attack_count, rank_floor, speed_tiles, swim_tiles, tenths_up, AcquirePlan, ActionEconomy,
    AttackMods, AttackShape, EffectTarget, HookEffect, MoveMode, MoveProfile, PowerCtx, PowerInst,
    PowerKernel, TurnPlan,
};
use crate::l0::{Amount, Colour};
use crate::l1::items::Hands;
use crate::l1::status::ConditionKind;
use crate::l1::traits::Trait;

/// Every canonical power's kebab-case name, in the order the coverage test
/// enumerates them: the Basic selection table's 25 (`4c:309-335`), then the 25
/// the Advanced table adds (`4c:343-394`), then
/// `Amplified Elemental/Energy Control`, which is in neither (`4c:430-434`).
pub const IDS: [&str; 51] = [
    // The Basic selection table.
    "animal-command",
    "body-armor",
    "claws",
    "contaminant-resistance",
    "elasticity",
    "elemental-energy-control",
    "extra-body-parts",
    "fast-attack",
    "flight",
    "force-field",
    "growth-shrinking",
    "invisibility",
    "mind-control",
    "one-of-a-kind-weapon",
    "phasing",
    "physical-metamorphosis",
    "regeneration",
    "shapeshift",
    "super-leap",
    "supersense",
    "superspeed",
    "telekinesis",
    "telepathy",
    "teleportation",
    "wall-crawling",
    // What the Advanced table adds.
    "absorption",
    "alter-ego",
    "astoundingly-wealthy",
    "burrowing",
    "celebrity",
    "chameleon",
    "combat-awareness",
    "detection",
    "dimension-jump",
    "elemental-energy-generation",
    "headquarters",
    "improved-skills",
    "mind-shield",
    "nine-lives",
    "nullification",
    "paralyzing-touch",
    "plant-control",
    "protected-sense",
    "reflection",
    "sidekick",
    "trait-boost",
    "trait-increase",
    "vehicle",
    "water-native",
    "weapon",
    // Implemented but in no selection table (`4c:430-434`).
    "amplified-elemental-control",
];

/// The registry: one entry per canonical name, plus `Magic`.
static KERNELS: [(&str, &dyn PowerKernel); 52] = [
    ("animal-command", &TraitFloor::ANIMAL_COMMAND),
    ("body-armor", &ArmourBonus::BODY_ARMOR),
    ("claws", &Claws),
    (
        "contaminant-resistance",
        &TraitFloor::CONTAMINANT_RESISTANCE,
    ),
    ("elasticity", &Elasticity),
    ("elemental-energy-control", &ElementalControl),
    ("extra-body-parts", &ExtraBodyParts),
    ("fast-attack", &FastAttack),
    ("flight", &Movement::FLIGHT),
    ("force-field", &ForceField),
    ("growth-shrinking", &GrowthShrinking),
    ("invisibility", &Invisibility),
    ("mind-control", &MindControl),
    ("one-of-a-kind-weapon", &OneOfAKindWeapon),
    ("phasing", &ArmourBonus::PHASING),
    ("physical-metamorphosis", &PhysicalMetamorphosis),
    ("regeneration", &Regeneration),
    ("shapeshift", &ContentBearing::SHAPESHIFT),
    ("super-leap", &Movement::SUPER_LEAP),
    ("supersense", &TraitFloor::SUPERSENSE),
    ("superspeed", &Movement::SUPERSPEED),
    ("telekinesis", &Telekinesis),
    ("telepathy", &TraitFloor::TELEPATHY),
    ("teleportation", &Teleportation),
    ("wall-crawling", &Movement::WALL_CRAWLING),
    ("absorption", &Absorption),
    ("alter-ego", &ContentBearing::ALTER_EGO),
    ("astoundingly-wealthy", &AstoundinglyWealthy),
    ("burrowing", &Movement::BURROWING),
    ("celebrity", &Celebrity),
    ("chameleon", &Chameleon),
    ("combat-awareness", &TraitFloor::COMBAT_AWARENESS),
    ("detection", &ContentBearing::DETECTION),
    ("dimension-jump", &DimensionJump),
    ("elemental-energy-generation", &EnergyGeneration),
    ("headquarters", &ContentBearing::HEADQUARTERS),
    ("improved-skills", &ImprovedSkills),
    ("mind-shield", &MindShield),
    ("nine-lives", &NineLives),
    ("nullification", &Nullification),
    ("paralyzing-touch", &ParalyzingTouch),
    ("plant-control", &PlantControl),
    ("protected-sense", &ProtectedSense),
    ("reflection", &Reflection),
    ("sidekick", &ContentBearing::SIDEKICK),
    ("trait-boost", &TraitBoost),
    ("trait-increase", &TraitIncrease),
    ("vehicle", &VehiclePower),
    ("water-native", &Movement::WATER_NATIVE),
    ("weapon", &ContentBearing::WEAPON),
    ("amplified-elemental-control", &AmplifiedControl),
    ("magic", &Magic),
];

/// The kernel for a canonical name, `magic` included.
pub fn kernel(name: &str) -> Option<&'static dyn PowerKernel> {
    KERNELS
        .iter()
        .find(|entry| entry.0 == name)
        .map(|entry| entry.1)
}

/// The trait a power's attack rolls on, by shape (`4c:502`).
fn control_trait(shape: AttackShape) -> Trait {
    if shape.is_ranged() {
        Trait::Coordination
    } else {
        Trait::Melee
    }
}

/// A power that reads `max(its Rank Value, trait + bonus)` wherever the trait is
/// used (`4c:470`, `4c:474`, `4c:751`, `4c:801`).
struct TraitFloor {
    id: &'static str,
    trait_: Trait,
    bonus: i32,
    note: &'static str,
}

impl TraitFloor {
    const ANIMAL_COMMAND: TraitFloor = TraitFloor {
        id: "animal-command",
        trait_: Trait::Willpower,
        bonus: 10,
        note: "animal command: communication uses Willpower +10 or the power's RV (4c:438)",
    };
    const CONTAMINANT_RESISTANCE: TraitFloor = TraitFloor {
        id: "contaminant-resistance",
        trait_: Trait::Fortitude,
        bonus: 10,
        note: "contaminant resistance: resistance uses Fortitude +10 or the power's RV (4c:474)",
    };
    const COMBAT_AWARENESS: TraitFloor = TraitFloor {
        id: "combat-awareness",
        trait_: Trait::Awareness,
        bonus: 10,
        note: "combat awareness: Awareness uses the power's RV or Awareness +10 (4c:470)",
    };
    const SUPERSENSE: TraitFloor = TraitFloor {
        id: "supersense",
        trait_: Trait::Awareness,
        bonus: 10,
        note: "supersense: the heightened sense uses the power's RV or Awareness +10 (4c:751)",
    };
    const TELEPATHY: TraitFloor = TraitFloor {
        id: "telepathy",
        trait_: Trait::Willpower,
        bonus: 10,
        note: "telepathy: the greater of the power's RV and Willpower +10 (4c:801)",
    };
}

impl PowerKernel for TraitFloor {
    fn id(&self) -> &'static str {
        self.id
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        if trait_ != self.trait_ {
            return value;
        }
        value.max(rank_floor(ctx, self.trait_, self.bonus))
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.note(self.note);
    }
}

/// A flat armour bonus: `Body Armor` (`4c:448`) and `Phasing` (`4c:688`), which
/// the spec words identically.
struct ArmourBonus {
    id: &'static str,
}

impl ArmourBonus {
    const BODY_ARMOR: ArmourBonus = ArmourBonus { id: "body-armor" };
    const PHASING: ArmourBonus = ArmourBonus { id: "phasing" };
}

impl PowerKernel for ArmourBonus {
    fn id(&self) -> &'static str {
        self.id
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        value + ctx.power.rv
    }
}

/// `Claws` (`4c:462-466`): a one-handed slashing attack at the power's or
/// Melee's Rank Value.
struct Claws;

impl PowerKernel for Claws {
    fn id(&self) -> &'static str {
        "claws"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        if !attack.shape.is_melee() {
            return mods;
        }
        let rv = rank_floor(ctx, Trait::Melee, 0);
        let mut out = mods;
        out.trait_ = Some(Trait::Melee);
        out.rv = Some(rv);
        out.hands = Some(Hands::One);
        // 4c:464: "treated as a one-handed weapon for purposes of damage", and
        // one hand is `+5` (4c:1074).
        out.damage_override = Some(rv + 5);
        out.note("claws: one-handed slashing at the claw's Rank Value (4c:464)");
        out
    }
}

/// `Elasticity` (`4c:492-494`): reach is `ceil(RV / 10)` legacy sectors.
struct Elasticity;

impl PowerKernel for Elasticity {
    fn id(&self) -> &'static str {
        "elasticity"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        if !attack.shape.is_melee() {
            return mods;
        }
        let mut out = mods;
        out.reach = Some(tenths_up(ctx.power.rv) * 3);
        out.note("elasticity: reach is ceil(RV/10) legacy sectors (4c:494)");
        out
    }
}

/// `Elemental/Energy Control` (`4c:496-537`): attack rolls at `max(RV, trait+10)`
/// and damage equals the power's Rank Value.
struct ElementalControl;

impl PowerKernel for ElementalControl {
    fn id(&self) -> &'static str {
        "elemental-energy-control"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        let trait_ = control_trait(attack.shape);
        let mut out = mods;
        out.trait_ = Some(trait_);
        out.rv = Some(rank_floor(ctx, trait_, 10));
        out.damage_override = Some(ctx.power.rv);
        out.note("elemental control: damage equals the power's Rank Value (4c:502)");
        out
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        // 4c:502 names *attacks*; a bare Coordination read (a dodge, a movement
        // allowance) is not an elemental attack and must not be lifted.
        match ctx.attack {
            Some(attack) if control_trait(attack.shape) == trait_ => {
                value.max(rank_floor(ctx, trait_, 10))
            }
            _ => value,
        }
    }
}

/// `Extra Body Parts` (`4c:545-558`): each advanced part grants a power or a
/// flat benefit.
struct ExtraBodyParts;

impl PowerKernel for ExtraBodyParts {
    fn id(&self) -> &'static str {
        "extra-body-parts"
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        match ctx.power.param("part") {
            "claws" => plan
                .grants_powers
                .push(PowerInst::new("wsp.power.claws", 10)),
            "shell" => plan
                .grants_powers
                .push(PowerInst::new("wsp.power.body-armor", 10)),
            "wings" => plan
                .grants_powers
                .push(PowerInst::new("wsp.power.flight", ctx.power.rv)),
            "extra-arms" | "tail" => plan.note("extra limbs grant a bonus attack (4c:554)"),
            "extra-legs" => plan.note("extra legs add one sector of movement (4c:555)"),
            _ => plan.note("extra body parts are a content choice (4c:551)"),
        }
    }

    fn action_economy(&self, ctx: &PowerCtx<'_>, _who: u32) -> Option<ActionEconomy> {
        match ctx.power.param("part") {
            "extra-arms" | "tail" => Some(ActionEconomy {
                attacks_per_panel: 2,
                bonus_tiles: 0,
            }),
            "extra-legs" => Some(ActionEconomy {
                attacks_per_panel: 1,
                bonus_tiles: 3,
            }),
            _ => None,
        }
    }
}

/// `Fast Attack` (`4c:560-568`).
struct FastAttack;

impl PowerKernel for FastAttack {
    fn id(&self) -> &'static str {
        "fast-attack"
    }

    fn action_economy(&self, ctx: &PowerCtx<'_>, _who: u32) -> Option<ActionEconomy> {
        Some(ActionEconomy {
            attacks_per_panel: fast_attack_count(ctx.power.rv),
            bonus_tiles: 0,
        })
    }
}

/// A movement power (`4c:452`, `4c:570`, `4c:728`, `4c:755`, `4c:825`,
/// `4c:829`).
struct Movement {
    id: &'static str,
    mode: MoveMode,
    /// Whether the allowance replaces the ordinary one. `Wall-Crawling` keeps
    /// ordinary movement and adds a surface, so it does not (`4c:827`).
    replaces: bool,
}

impl Movement {
    const FLIGHT: Movement = Movement {
        id: "flight",
        mode: MoveMode::Fly,
        replaces: true,
    };
    const SUPER_LEAP: Movement = Movement {
        id: "super-leap",
        mode: MoveMode::Leap,
        replaces: true,
    };
    const SUPERSPEED: Movement = Movement {
        id: "superspeed",
        mode: MoveMode::Run,
        replaces: true,
    };
    const BURROWING: Movement = Movement {
        id: "burrowing",
        mode: MoveMode::Burrow,
        replaces: false,
    };
    const WALL_CRAWLING: Movement = Movement {
        id: "wall-crawling",
        mode: MoveMode::Cling,
        replaces: false,
    };
    const WATER_NATIVE: Movement = Movement {
        id: "water-native",
        mode: MoveMode::Swim,
        replaces: true,
    };
}

impl PowerKernel for Movement {
    fn id(&self) -> &'static str {
        self.id
    }

    fn movement(&self, ctx: &PowerCtx<'_>, who: u32) -> Option<MoveProfile> {
        let tiles = match self.mode {
            MoveMode::Fly | MoveMode::Leap => speed_tiles(ctx.power.rv),
            MoveMode::Swim => swim_tiles(ctx.power.rv),
            // 4c:757: Superspeed uses the greater of the power's RV and
            // Coordination +10.
            MoveMode::Run => speed_tiles(
                ctx.power
                    .rv
                    .max(ctx.stored_rv(who, Trait::Coordination) + 10),
            ),
            // 4c:452: burrowing moves at the character's normal running speed, so
            // the ordinary allowance stands and only the medium changes.
            MoveMode::Burrow => 0,
            MoveMode::Cling | MoveMode::Walk => 0,
        };
        Some(MoveProfile {
            mode: self.mode,
            tiles: if self.replaces { Some(tiles) } else { None },
            max_material: if self.mode == MoveMode::Burrow {
                // 4c:452: any terrain at or below the power's Rank Value.
                Some(ctx.power.rv)
            } else {
                None
            },
        })
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        if self.mode == MoveMode::Burrow {
            plan.note(&format!(
                "burrowing: any terrain with material rank at or below {} (4c:452)",
                ctx.power.rv
            ));
        }
    }
}

/// `Force Field` (`4c:591-597`): armour with two modes and two failure rules
/// (`D5`).
struct ForceField;

/// The field's Rank Value: the power's, or `Willpower + 10` for the mental mode
/// (`4c:597`).
fn force_field_rv(ctx: &PowerCtx<'_>) -> i32 {
    if ctx.power.param("mode") == "mental" {
        ctx.power
            .rv
            .max(ctx.stored_rv(ctx.owner, Trait::Willpower) + 10)
    } else {
        ctx.power.rv
    }
}

impl PowerKernel for ForceField {
    fn id(&self) -> &'static str {
        "force-field"
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        // D5: absorbing armour sits before worn armour, and the pipeline is
        // additive, so the field is a delta like any other.
        value + force_field_rv(ctx)
    }

    fn on_absorb(&self, ctx: &PowerCtx<'_>, absorbed: i32, out: &mut Vec<HookEffect>) {
        if absorbed <= force_field_rv(ctx) {
            return;
        }
        if ctx.power.param("mode") == "mental" {
            // 4c:597: a Black Fortitude roll leaves the character dazed for
            // 1-10 panels.
            out.push(HookEffect::Resist {
                trait_: Trait::Fortitude,
                rv: None,
                on: Colour::Blck,
                then: vec![HookEffect::ApplyCondition {
                    target: EffectTarget::Self_,
                    kind: ConditionKind::Dazed,
                    panels: Amount::dice(1, 10).at_least(1),
                }],
            });
        } else {
            // 4c:595: the device shorts out for 1-10 turns.
            out.push(HookEffect::ApplyCondition {
                target: EffectTarget::Self_,
                kind: ConditionKind::Shorted,
                panels: Amount::dice(1, 10).at_least(1),
            });
        }
    }
}

/// `Growth/Shrinking` (`4c:599-620`).
struct GrowthShrinking;

impl PowerKernel for GrowthShrinking {
    fn id(&self) -> &'static str {
        "growth-shrinking"
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        if trait_ != Trait::Brawn || ctx.power.param("mode") == "shrink" {
            // 4c:620: Shrinking leaves Brawn untouched.
            return value;
        }
        value.max(rank_floor(ctx, Trait::Brawn, 10))
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        let grown = ctx.power.param("mode") != "shrink";
        if ctx.owner_is_actor() {
            if grown {
                return mods;
            }
            // 4c:620: "the character gains a +2 RS bonus to attacks".
            let mut out = mods;
            out.steps += 2;
            out.note("shrinking: +2 RS to attacks (4c:620)");
            return out;
        }
        if !ctx.owner_is_target() {
            return mods;
        }
        let mut out = mods;
        if grown {
            out.steps += 1;
            out.note("the target is grown: attackers gain +1 RS (4c:618)");
        } else {
            out.steps -= 1;
            out.note("the target is shrunk: attackers suffer −1 RS (4c:620)");
        }
        out
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.note(if ctx.power.param("mode") == "shrink" {
            "shrinking: Brawn is unaffected, attackers suffer −1 RS (4c:620)"
        } else {
            "growth: Brawn becomes the power's RV or Brawn +10 (4c:618)"
        });
    }
}

/// `Invisibility` (`4c:642-644`).
struct Invisibility;

impl PowerKernel for Invisibility {
    fn id(&self) -> &'static str {
        "invisibility"
    }

    fn on_own_turn(&self, _ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        plan.effects.push(HookEffect::ApplyCondition {
            target: EffectTarget::Self_,
            kind: ConditionKind::Invisible,
            panels: Amount::constant(-1),
        });
    }
}

/// `Chameleon` (`4c:458-460`): simpler than Invisibility, but the same condition
/// with an opposed roll to see through it (`D27`).
struct Chameleon;

impl PowerKernel for Chameleon {
    fn id(&self) -> &'static str {
        "chameleon"
    }

    fn on_own_turn(&self, _ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        plan.effects.push(HookEffect::ApplyCondition {
            target: EffectTarget::Self_,
            kind: ConditionKind::Invisible,
            panels: Amount::constant(-1),
        });
        plan.note("chameleon is an opposed d% roll (4c:460, D27)");
    }
}

/// `Mind Control` (`4c:652-656`, `D3`).
struct MindControl;

impl PowerKernel for MindControl {
    fn id(&self) -> &'static str {
        "mind-control"
    }

    fn on_hit(&self, ctx: &PowerCtx<'_>, out: &mut Vec<HookEffect>) {
        if ctx.colour < Colour::Red {
            return;
        }
        // 4c:654: the target's Willpower must be lower than the power's RV or the
        // controller's Willpower +10.
        let own = ctx
            .power
            .rv
            .max(ctx.stored_rv(ctx.owner, Trait::Willpower) + 10);
        if ctx.stored_rv(ctx.target, Trait::Willpower) >= own {
            return;
        }
        out.push(HookEffect::ApplyCondition {
            target: EffectTarget::Target,
            kind: ConditionKind::MindControlled,
            panels: Amount::constant(-1),
        });
    }
}

/// `One-of-a-Kind Weapon` (`4c:672-680`).
struct OneOfAKindWeapon;

impl PowerKernel for OneOfAKindWeapon {
    fn id(&self) -> &'static str {
        "one-of-a-kind-weapon"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        let trait_ = control_trait(attack.shape);
        let rv = rank_floor(ctx, trait_, 10);
        let mut out = mods;
        out.trait_ = Some(trait_);
        out.rv = Some(rv);
        out.damage_override = Some(rv);
        out.note("one-of-a-kind weapon: damage equals its Rank Value (4c:674)");
        out
    }
}

/// `Physical Metamorphosis` (`4c:690-702`).
struct PhysicalMetamorphosis;

impl PowerKernel for PhysicalMetamorphosis {
    fn id(&self) -> &'static str {
        "physical-metamorphosis"
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        value + ctx.power.rv
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        if trait_ != Trait::Brawn || ctx.power.param("material") != "metal" {
            return value;
        }
        value.max(rank_floor(ctx, Trait::Brawn, 10))
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let mut out = mods;
        out.damage_override = Some(ctx.power.rv);
        out.note("metamorphosed attacks may use the power's Rank Value (4c:694)");
        out
    }
}

/// `Regeneration` (`4c:716-718`).
struct Regeneration;

impl PowerKernel for Regeneration {
    fn id(&self) -> &'static str {
        "regeneration"
    }

    fn on_own_turn(&self, ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        plan.effects.push(HookEffect::Regenerate {
            amount: Amount::constant(ctx.power.rv),
        });
        plan.consumes_action = true;
        plan.note("regeneration: recovery takes the panel's action (4c:718)");
    }
}

/// `Telekinesis` (`4c:778-797`).
struct Telekinesis;

impl PowerKernel for Telekinesis {
    fn id(&self) -> &'static str {
        "telekinesis"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        if !attack.shape.is_ranged() {
            return mods;
        }
        let mut out = mods;
        // 4c:797: Willpower replaces Coordination and damage is the power's RV.
        out.trait_ = Some(Trait::Willpower);
        out.damage_override = Some(ctx.power.rv);
        out.note("telekinesis: Willpower substitutes for Coordination (4c:797)");
        out
    }
}

/// `Teleportation` (`4c:807-811`).
struct Teleportation;

impl PowerKernel for Teleportation {
    fn id(&self) -> &'static str {
        "teleportation"
    }

    fn on_own_turn(&self, ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        plan.effects.push(HookEffect::Teleport {
            target: EffectTarget::Self_,
            tiles: ctx.power.rv * 3,
        });
        // 4c:809: a Black roll arrives dazed and costs the next panel.
        plan.effects.push(HookEffect::Resist {
            trait_: Trait::Awareness,
            rv: Some(ctx.power.rv),
            on: Colour::Blck,
            then: vec![HookEffect::ApplyCondition {
                target: EffectTarget::Self_,
                kind: ConditionKind::Dazed,
                panels: Amount::constant(1),
            }],
        });
        plan.consumes_action = true;
        plan.note("teleportation: a Black roll arrives dazed (4c:809)");
    }
}

/// `Absorption` (`4c:417-424`).
struct Absorption;

impl PowerKernel for Absorption {
    fn id(&self) -> &'static str {
        "absorption"
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        let Some(attack) = ctx.attack else {
            return value;
        };
        if !attack.has_tag(ctx.power.param("tag")) {
            return value;
        }
        value + ctx.power.rv
    }

    fn on_absorb(&self, ctx: &PowerCtx<'_>, absorbed: i32, out: &mut Vec<HookEffect>) {
        let absorbed = absorbed.min(ctx.power.rv).max(0);
        if absorbed == 0 {
            return;
        }
        out.push(HookEffect::SetCharged(Amount::constant(absorbed)));
        if ctx.power.param("use") == "attack" {
            // 4c:424: "on his next turn, may unleash the absorbed energy".
            out.push(HookEffect::ApplyCondition {
                target: EffectTarget::Self_,
                kind: ConditionKind::Charged,
                panels: Amount::constant(-1),
            });
        } else {
            // 4c:423: healing, capped at the character's maximum.
            out.push(HookEffect::Heal {
                target: EffectTarget::Self_,
                amount: Amount::constant(absorbed),
            });
        }
    }
}

/// A content-bearing power: it enforces its creation constraint and declares the
/// catalogue the campaign must author (`4c:426-432`, `D30`).
struct ContentBearing {
    id: &'static str,
    constraint: &'static str,
}

impl ContentBearing {
    const ALTER_EGO: ContentBearing = ContentBearing {
        id: "alter-ego",
        constraint: "alter-ego: the second form has no powers and traits at most 30 (4c:428)",
    };
    const SHAPESHIFT: ContentBearing = ContentBearing {
        id: "shapeshift",
        constraint: "shapeshift retains the character's original size (4c:722)",
    };
    const DETECTION: ContentBearing = ContentBearing {
        id: "detection",
        constraint: "detection names one energy type; range is its RV in sectors (4c:478)",
    };
    const HEADQUARTERS: ContentBearing = ContentBearing {
        id: "headquarters",
        constraint: "headquarters is authored catalogue content (4c:622, D30)",
    };
    const SIDEKICK: ContentBearing = ContentBearing {
        id: "sidekick",
        constraint: "sidekick traits are capped by the main character's (4c:726)",
    };
    const WEAPON: ContentBearing = ContentBearing {
        id: "weapon",
        constraint: "weapon is authored catalogue content (4c:839, D30)",
    };
}

impl PowerKernel for ContentBearing {
    fn id(&self) -> &'static str {
        self.id
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.note(self.constraint);
        plan.content_requests.push(self.id.to_string());
    }
}

/// `Astoundingly Wealthy` (`4c:442-444`).
struct AstoundinglyWealthy;

impl PowerKernel for AstoundinglyWealthy {
    fn id(&self) -> &'static str {
        "astoundingly-wealthy"
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.lifestyle_bonus += 50;
        plan.repute_bonus += 20;
        plan.note("astoundingly wealthy: Lifestyle +50, Repute +20 (4c:444)");
    }
}

/// `Celebrity` (`4c:454-456`).
struct Celebrity;

impl PowerKernel for Celebrity {
    fn id(&self) -> &'static str {
        "celebrity"
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.repute_bonus += 30;
        plan.note("celebrity: Repute +30, gains and losses doubled (4c:456)");
    }
}

/// `Dimension Jump` (`4c:488-490`).
struct DimensionJump;

impl PowerKernel for DimensionJump {
    fn id(&self) -> &'static str {
        "dimension-jump"
    }

    fn on_own_turn(&self, ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        // 4c:490: a new dimension needs a roll; black arrives dazed.
        plan.effects.push(HookEffect::Resist {
            trait_: Trait::Awareness,
            rv: Some(ctx.power.rv),
            on: Colour::Blck,
            then: vec![HookEffect::ApplyCondition {
                target: EffectTarget::Self_,
                kind: ConditionKind::Dazed,
                panels: Amount::constant(1),
            }],
        });
        plan.consumes_action = true;
    }
}

/// `Elemental/Energy Generation` (`4c:539-543`).
struct EnergyGeneration;

impl PowerKernel for EnergyGeneration {
    fn id(&self) -> &'static str {
        "elemental-energy-generation"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        let trait_ = control_trait(attack.shape);
        // 4c:541: control is at half the power's Rank Value; a matching
        // Elemental Control adds a permanent +10 to both (4c:543).
        let mut rv = ctx.power.rv / 2;
        if let Some(control) = ctx.owner_power("wsp.power.elemental-energy-control") {
            if control.param("tag") == ctx.power.param("tag") {
                rv += 10;
            }
        }
        let rv = rv.max(ctx.stored_rv(ctx.owner, trait_) + 10);
        let mut out = mods;
        out.trait_ = Some(trait_);
        out.rv = Some(rv);
        out.damage_override = Some(rv);
        out.note("generation: control at half the power's Rank Value (4c:541)");
        out
    }
}

/// `Improved Skills` (`4c:638-640`, `D13`).
struct ImprovedSkills;

impl PowerKernel for ImprovedSkills {
    fn id(&self) -> &'static str {
        "improved-skills"
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.bonus_skills += 2;
        // D13: the +3 *replaces* the skill's step value rather than stacking.
        plan.skill_step_override = Some(3);
        plan.note("improved skills: two bonus skills, one at +3 (4c:640, D13)");
    }
}

/// `Mind Shield` (`4c:658-662`).
struct MindShield;

impl PowerKernel for MindShield {
    fn id(&self) -> &'static str {
        "mind-shield"
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        if trait_ != Trait::Willpower {
            return value;
        }
        // 4c:660: Willpower is *increased by* the power's Rank Value.
        value + ctx.power.rv
    }

    fn modify_armor(&self, ctx: &PowerCtx<'_>, value: i32) -> i32 {
        // 4c:662: a mental Force Field at half the power's Rank Value.
        value + ctx.power.half_rv_up()
    }
}

/// `Nine Lives` (`4c:664-666`, `D12`).
struct NineLives;

impl PowerKernel for NineLives {
    fn id(&self) -> &'static str {
        "nine-lives"
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.grant_fortune += ctx.power.rv * 2;
        plan.note("nine lives: a session pool of twice the power's Rank Value (4c:666, D12)");
    }
}

/// `Nullification` (`4c:668-670`).
struct Nullification;

impl PowerKernel for Nullification {
    fn id(&self) -> &'static str {
        "nullification"
    }

    fn on_power_used(&self, ctx: &PowerCtx<'_>, _power: &PowerInst, out: &mut Vec<HookEffect>) {
        // The resolver rolls the three-way outcome at the power's Rank Value
        // (4c:670): black fails and costs the nullifier half the target power's
        // Rank Value, red halves it, anything else negates it.
        out.push(HookEffect::Nullify {
            target: EffectTarget::Target,
            rv: ctx.power.rv,
        });
    }
}

/// `Paralyzing Touch` (`4c:682-684`).
struct ParalyzingTouch;

impl PowerKernel for ParalyzingTouch {
    fn id(&self) -> &'static str {
        "paralyzing-touch"
    }

    fn on_hit(&self, ctx: &PowerCtx<'_>, out: &mut Vec<HookEffect>) {
        if ctx.colour < Colour::Red {
            return;
        }
        // 4c:684: the hit deals no damage, then Fortitude resists or the target
        // is paralyzed for ceil(RV/10) panels. With no attack in scope the
        // negation is zero, so the hook is still declared.
        out.push(HookEffect::ModifyDamage(
            -ctx.attack.map_or(0, |attack| attack.damage),
        ));
        out.push(HookEffect::Resist {
            trait_: Trait::Fortitude,
            rv: None,
            on: Colour::Blck,
            then: vec![HookEffect::ApplyCondition {
                target: EffectTarget::Target,
                kind: ConditionKind::Paralyzed,
                panels: Amount::constant(tenths_up(ctx.power.rv)),
            }],
        });
    }
}

/// `Plant Control` (`4c:704-706`).
struct PlantControl;

impl PowerKernel for PlantControl {
    fn id(&self) -> &'static str {
        "plant-control"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        if !attack.shape.is_melee() {
            return mods;
        }
        let rv = rank_floor(ctx, Trait::Melee, 10);
        let mut out = mods;
        out.trait_ = Some(Trait::Melee);
        out.rv = Some(rv);
        out.damage_override = Some(rv);
        out.reach = Some(tenths_up(ctx.power.rv) * 3);
        out.note("plant control: Melee +10 or the power's Rank Value (4c:706)");
        out
    }
}

/// `Protected Sense` (`4c:708-710`).
struct ProtectedSense;

impl PowerKernel for ProtectedSense {
    fn id(&self) -> &'static str {
        "protected-sense"
    }

    fn on_resist(&self, ctx: &PowerCtx<'_>, colour: &mut Colour) {
        // 4c:710: an attack at or below the power's Rank Value cannot harm the
        // protected sense, so the resistance cannot fail.
        if let Some(attack) = ctx.attack {
            if attack.rv <= ctx.power.rv {
                *colour = Colour::Blck;
            }
        }
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.note(&format!(
            "protected sense: immune to sense attacks at or below RV {} (4c:710)",
            ctx.power.rv
        ));
    }
}

/// `Reflection` (`4c:712-714`).
struct Reflection;

impl PowerKernel for Reflection {
    fn id(&self) -> &'static str {
        "reflection"
    }

    fn on_power_used(&self, ctx: &PowerCtx<'_>, _power: &PowerInst, out: &mut Vec<HookEffect>) {
        out.push(HookEffect::Reflect {
            target: EffectTarget::Target,
            rv: ctx.power.rv,
        });
    }
}

/// `Trait Boost` (`4c:813-815`).
struct TraitBoost;

impl PowerKernel for TraitBoost {
    fn id(&self) -> &'static str {
        "trait-boost"
    }

    fn on_own_turn(&self, ctx: &PowerCtx<'_>, plan: &mut TurnPlan) {
        let trait_ = Trait::from_id(ctx.power.param("trait")).unwrap_or(Trait::Brawn);
        let boosted = ctx.stored_rv(ctx.owner, trait_) + ctx.power.rv;
        // 4c:815: "for a number of turns equal to one-tenth the newly boosted
        // value (round up)".
        plan.effects.push(HookEffect::BoostTrait {
            trait_,
            rv: boosted,
            panels: Amount::constant(tenths_up(boosted)),
        });
        plan.note("trait boost: the trait falls to half for 1d10 panels (4c:815)");
    }
}

/// `Trait Increase` (`4c:817-819`).
struct TraitIncrease;

impl PowerKernel for TraitIncrease {
    fn id(&self) -> &'static str {
        "trait-increase"
    }

    fn modify_trait(&self, ctx: &PowerCtx<'_>, _who: u32, trait_: Trait, value: i32) -> i32 {
        let first = Trait::from_id(ctx.power.param("first"));
        let second = Trait::from_id(ctx.power.param("second"));
        if Some(trait_) != first && Some(trait_) != second {
            return value;
        }
        value.max(ctx.stored_rv(ctx.owner, trait_) + 15)
    }
}

/// `Vehicle` (`4c:821-823`, `D17`). The buff is a modifier layer, never baked
/// into the vehicle's stored traits, so removing the power removes the buff.
struct VehiclePower;

impl PowerKernel for VehiclePower {
    fn id(&self) -> &'static str {
        "vehicle"
    }

    fn on_acquire(&self, ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.content_requests.push("vehicle".to_string());
        plan.note(&format!(
            "vehicle: Durability, Handling and Velocity +{} as a modifier layer (4c:823, D17)",
            ctx.power.half_rv_up()
        ));
    }
}

/// `Amplified Elemental/Energy Control` (`4c:430-434`), in no selection table.
struct AmplifiedControl;

impl PowerKernel for AmplifiedControl {
    fn id(&self) -> &'static str {
        "amplified-elemental-control"
    }

    fn modify_attack(&self, ctx: &PowerCtx<'_>, mods: AttackMods) -> AttackMods {
        // Offensive: only the attacker's own instance amends the attack. The
        // encounter folds the target's powers through the same hook so a
        // defensive power can react (`4c:618`), which is why this guard exists.
        if !ctx.owner_is_actor() {
            return mods;
        }
        let Some(attack) = ctx.attack else {
            return mods;
        };
        let trait_ = control_trait(attack.shape);
        let mut out = mods;
        out.trait_ = Some(trait_);
        let control = ctx
            .owner_power("wsp.power.elemental-energy-control")
            .filter(|control| control.param("tag") == ctx.power.param("tag"));
        if let Some(control) = control {
            // 4c:432: the matching control power's RV rises by half this
            // power's, and the roll uses three dice keeping two.
            out.rv = Some(control.rv + ctx.power.half_rv_up());
            out.damage_override = Some(control.rv + ctx.power.half_rv_up());
            out.keep_two_of_three = true;
        } else {
            // 4c:434: with generation but not control, control is half this
            // power's Rank Value +10.
            let rv = ctx.power.half_rv_up() + 10;
            out.rv = Some(rv);
            out.damage_override = Some(rv);
        }
        out.note("amplified control: three dice, keep two (4c:432)");
        out
    }
}

/// `Magic` (`4c:646-650`): a kernel with no effect of its own. It instantiates a
/// `Template` per spell at the Magic Rank Value, which is why the DSL must be
/// able to express the canonical powers.
struct Magic;

impl PowerKernel for Magic {
    fn id(&self) -> &'static str {
        "magic"
    }

    fn on_acquire(&self, _ctx: &PowerCtx<'_>, plan: &mut AcquirePlan) {
        plan.note("magic: one spell per turn, at the Magic Rank Value (4c:648)");
        plan.content_requests.push("spell".to_string());
    }
}
