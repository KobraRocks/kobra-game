//! L1 — the entity layer (02:02.3–02:02.4, 02:02.8).
//!
//! Traits, secondary traits, origins, skills, items, statuses and powers. It
//! depends only on L0 (`02:02.1`), and everything above it — the encounter, the
//! world, the campaign — mutates these values rather than re-deriving them.

pub mod character;
pub mod items;
pub mod powers;
pub mod skills;
pub mod status;
pub mod traits;

pub use character::{
    melee_hands_bonus, Character, CountTable, CreationContext, CreationMode, GenerationTable,
    TraitBoost,
};
pub use items::{Hands, Item, ItemModifier, MeleeKind, Reach};
pub use powers::{
    AcquirePlan, ActionEconomy, AttackMods, AttackShape, AttackView, EffectTarget, HookEffect,
    MoveMode, MoveProfile, PowerCtx, PowerInst, PowerKernel, PowerSource, TurnPlan,
};
pub use skills::{ActionTag, Skill, SkillInstance};
pub use status::{Condition, ConditionKind, Permission};
pub use traits::{Effective, Trait, TraitStep, TRAITS};
