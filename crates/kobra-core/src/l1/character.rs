//! L1 — characters and the one factory with three creation modes (02:02.3).
//!
//! 4C generates characters by rolling every trait and power (`4c:169`,
//! `4c:264`, `4c:400`), but a CRPG cannot ship only that: players want to build
//! a character, and the editor must author NPCs. The decision is **one character
//! factory with three input modes over the same result** (`02:02.3`), so nothing
//! downstream cares which one was used.
//!
//! The generation table is reproduced **literally**, including its
//! non-monotone bottom band (`D1`: `00-04 -> 10`, `05-09 -> 3`, `10-19 -> 6`)
//! and surfaced in [`Character::creation_log`] so a player sees why a low roll
//! was lucky (`02:02.11` rule 2).

use crate::json::Json;
use crate::l0::{Amount, Ladder};
use crate::l1::items::{Hands, Item};
use crate::l1::powers::{PowerInst, PowerSource};
use crate::l1::skills::{ActionTag, SkillInstance};
use crate::l1::status::{self, Condition};
use crate::l1::traits::{Effective, Trait, TRAITS};
use crate::rng::Rng;

/// One band of the determining-Rank-Value table (`4c:171-180`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct GenerationBand {
    /// Lowest `d%` in the band.
    pub lo: u8,
    /// Highest `d%` in the band.
    pub hi: u8,
    /// The Rank Value it grants.
    pub rv: i32,
}

/// The determining-Rank-Value table (`4c:171-180`, `D1`).
///
/// Reproduced literally, non-monotone bottom band and all: the published
/// distribution is part of the system, and "fixing" it is a campaign decision
/// (`rules.generation_table`), not an engine one.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct GenerationTable {
    /// The bands, in ascending `d%` order.
    pub bands: Vec<GenerationBand>,
}

impl GenerationTable {
    /// The table as the Libre Edition prints it (`4c:171-180`).
    pub fn canonical() -> GenerationTable {
        GenerationTable {
            bands: vec![
                GenerationBand {
                    lo: 0,
                    hi: 4,
                    rv: 10,
                },
                GenerationBand {
                    lo: 5,
                    hi: 9,
                    rv: 3,
                },
                GenerationBand {
                    lo: 10,
                    hi: 19,
                    rv: 6,
                },
                GenerationBand {
                    lo: 20,
                    hi: 39,
                    rv: 10,
                },
                GenerationBand {
                    lo: 40,
                    hi: 59,
                    rv: 20,
                },
                GenerationBand {
                    lo: 60,
                    hi: 79,
                    rv: 30,
                },
                GenerationBand {
                    lo: 80,
                    hi: 95,
                    rv: 40,
                },
                GenerationBand {
                    lo: 96,
                    hi: 99,
                    rv: 50,
                },
            ],
        }
    }

    /// Roll on the table.
    pub fn roll(&self, rng: &mut Rng) -> i32 {
        let roll = rng.d100();
        self.rv_for(roll)
    }

    /// The Rank Value a roll grants, or the lowest band's value for a roll the
    /// table does not cover (a table must tile `0..=99`; the validator says so).
    pub fn rv_for(&self, roll: u8) -> i32 {
        self.bands
            .iter()
            .find(|band| roll >= band.lo && roll <= band.hi)
            .map(|band| band.rv)
            .unwrap_or_else(|| self.bands.first().map(|band| band.rv).unwrap_or(1))
    }

    /// Read a table from content.
    pub fn parse(value: &Json) -> Result<GenerationTable, &'static str> {
        let Json::Arr(items) = value else {
            return Err("generation_table must be an array");
        };
        let mut bands = Vec::new();
        for item in items {
            bands.push(GenerationBand {
                lo: item
                    .get("lo")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs lo")? as u8,
                hi: item
                    .get("hi")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs hi")? as u8,
                rv: item
                    .get("rv")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs rv")? as i32,
            });
        }
        if bands.is_empty() {
            return Err("generation_table must not be empty");
        }
        Ok(GenerationTable { bands })
    }
}

impl Default for GenerationTable {
    fn default() -> GenerationTable {
        GenerationTable::canonical()
    }
}

/// A band of the skill-count table (`4c:264-270`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct CountBand {
    /// Lowest `d%`.
    pub lo: u8,
    /// Highest `d%`.
    pub hi: u8,
    /// How many skills.
    pub count: i32,
}

/// The skill-count table (`4c:264-270`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct CountTable {
    /// The bands, ascending.
    pub bands: Vec<CountBand>,
}

impl CountTable {
    /// The table as printed: `00-19:1`, `20-59:2`, `60-89:3`, `90-99:4`.
    pub fn canonical() -> CountTable {
        CountTable {
            bands: vec![
                CountBand {
                    lo: 0,
                    hi: 19,
                    count: 1,
                },
                CountBand {
                    lo: 20,
                    hi: 59,
                    count: 2,
                },
                CountBand {
                    lo: 60,
                    hi: 89,
                    count: 3,
                },
                CountBand {
                    lo: 90,
                    hi: 99,
                    count: 4,
                },
            ],
        }
    }

    /// Roll on the table.
    pub fn roll(&self, rng: &mut Rng) -> i32 {
        let roll = rng.d100();
        self.bands
            .iter()
            .find(|band| roll >= band.lo && roll <= band.hi)
            .map(|band| band.count)
            .unwrap_or(0)
    }

    /// Read a table from content.
    pub fn parse(value: &Json) -> Result<CountTable, &'static str> {
        let Json::Arr(items) = value else {
            return Err("skill_table must be an array");
        };
        let mut bands = Vec::new();
        for item in items {
            bands.push(CountBand {
                lo: item
                    .get("lo")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs lo")? as u8,
                hi: item
                    .get("hi")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs hi")? as u8,
                count: item
                    .get("count")
                    .and_then(Json::as_i64)
                    .ok_or("a band needs count")? as i32,
            });
        }
        Ok(CountTable { bands })
    }
}

impl Default for CountTable {
    fn default() -> CountTable {
        CountTable::canonical()
    }
}

/// Which of the three creation modes produced a character (`02:02.3`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum CreationMode {
    /// The exact spec procedure, fully seeded (`4c:169`).
    Rolled,
    /// A point-buy spending the same expected budget.
    Budgeted,
    /// A fixed sheet from content: NPCs, adversaries, the editor's template.
    Authored,
}

impl CreationMode {
    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            CreationMode::Rolled => "rolled",
            CreationMode::Budgeted => "budgeted",
            CreationMode::Authored => "authored",
        }
    }

    /// The mode a content id names.
    pub fn from_id(id: &str) -> Option<CreationMode> {
        match id {
            "rolled" => Some(CreationMode::Rolled),
            "budgeted" => Some(CreationMode::Budgeted),
            "authored" => Some(CreationMode::Authored),
            _ => None,
        }
    }
}

/// A character: the sheet a save records and the encounter mutates.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Character {
    /// The runtime entity id, assigned by the sim.
    pub id: u32,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// The origin record id, or empty for none.
    pub origin: String,
    /// Which side the character fights on: `0` is the party, `1` the opposition.
    pub side: u8,
    /// How the character was made.
    pub mode: CreationMode,
    /// The stored Primary Trait Rank Values, indexed by [`Trait::index`].
    pub traits: [i32; 7],
    /// Current Damage points (`4c:190`).
    pub damage: i32,
    /// Starting Damage points, for reporting and healing caps.
    pub max_damage: i32,
    /// Fortune points (`4c:196`), spent to shift rolls.
    pub fortune: i32,
    /// The Repute score (`4c:208`), a number rather than a Rank Value.
    pub repute: i32,
    /// The Lifestyle Rank Value (`4c:202`).
    pub lifestyle_rv: i32,
    /// Skills.
    pub skills: Vec<SkillInstance>,
    /// Item record ids carried.
    pub items: Vec<String>,
    /// The item id used for attacks, when any.
    pub weapon: Option<String>,
    /// Armour item ids worn.
    pub armour: Vec<String>,
    /// Active conditions.
    pub conditions: Vec<Condition>,
    /// The character's powers (`02:02.4`).
    pub powers: Vec<PowerInst>,
    /// Rank Values a power sets outright, recomputed on create and on load.
    ///
    /// Derived, never stored in a save (`D17`): a power that buffs a trait is a
    /// modifier layer, so removing the power removes the buff and the save needs
    /// no "was buffed" flag. `recompute_power_effects` is the only writer.
    pub power_floors: Vec<(Trait, i32)>,
    /// Armour granted by powers, as a modifier layer (`4c:448`, `4c:688`).
    pub power_armor: i32,
    /// Row-step shifts that last only for this panel, cleared at end of panel.
    ///
    /// A power's `ModifyTraitSteps` is a resolution-scoped shift, not a lasting
    /// state, which is why it is not written to a save.
    pub temporary_steps: Vec<crate::l1::traits::TraitStep>,
    /// Rank Value boosts with a panel countdown (`4c:815`).
    pub trait_boosts: Vec<TraitBoost>,
    /// Row steps Fortitude has dropped while dying (`4c:1161`).
    pub dying_steps: i32,
    /// Whether the character is dead; a dead character takes no further action
    /// and is left where it fell (`rules.corpse_occupies` is M2).
    pub dead: bool,
    /// Tiles entered this panel, for the per-distance attack penalty (`4c:880`).
    pub moved_tiles: i32,
    /// The row-step penalty attackers suffer this panel from a Dodge (`4c:1044`).
    pub dodge_steps: i32,
    /// Decisions the factory made, surfaced to the player (`02:02.11` rule 2).
    pub creation_log: Vec<String>,
}

/// A temporary Rank Value boost a power applied (`4c:815`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct TraitBoost {
    /// The trait boosted.
    pub trait_: Trait,
    /// The Rank Value it is boosted to.
    pub rv: i32,
    /// Panels remaining.
    pub panels: i32,
}

/// What the factory needs that is not the character itself.
pub struct CreationContext<'a> {
    /// The ladder, for row steps and derived bands.
    pub ladder: &'a Ladder,
    /// The determining-Rank-Value table.
    pub generation: &'a GenerationTable,
    /// The skill-count table.
    pub skills_count: &'a CountTable,
    /// The skill record ids a rolled character may draw from.
    ///
    /// Owned rather than borrowed: the registry builds it on demand, and a
    /// borrowed slice would tie the context to a temporary allocation.
    pub skill_pool: Vec<String>,
    /// Point-buy budget (`creation.budget_points`).
    pub budget_points: i32,
    /// Point-buy cost of each skill (`creation.skill_cost`).
    pub skill_cost: i32,
}

impl Character {
    /// Roll a character exactly as the spec does (`4c:169`, `4c:264`).
    pub fn rolled(
        ctx: &CreationContext<'_>,
        rng: &mut Rng,
        id: u32,
        name_key: &str,
        side: u8,
    ) -> Character {
        let mut traits = [1i32; 7];
        let mut log = Vec::new();
        for trait_ in TRAITS {
            let roll = rng.d100();
            let rv = ctx.generation.rv_for(roll);
            traits[trait_.index()] = rv;
            log.push(format!(
                "{}: rolled {} -> rank value {}",
                trait_.id(),
                roll,
                rv
            ));
        }
        log.push(
            "generation table reproduced literally, including the non-monotone 00-04 band (D1)"
                .to_string(),
        );
        let lifestyle_rv = ctx.generation.roll(rng);
        let repute = Amount::repute().roll(rng);
        let skill_count = ctx.skills_count.roll(rng);
        let skills = roll_skills(&ctx.skill_pool, skill_count, rng);
        let mut character = Character::from_sheet(
            id,
            name_key,
            "",
            side,
            CreationMode::Rolled,
            traits,
            lifestyle_rv,
            repute,
            skills,
            Vec::new(),
            Vec::new(),
            log,
        );
        character.creation_log.push(format!(
            "lifestyle {lifestyle_rv}, repute {repute}, {skill_count} skill slot(s)"
        ));
        character
    }

    /// Build a character from a spent point budget (02:02.3).
    ///
    /// The cost is the sum of the purchased Rank Values plus the Lifestyle Rank
    /// Value plus `skill_cost` per skill. Overspending is refused rather than
    /// clamped, because a budget that silently truncates is indistinguishable
    /// from a bug.
    #[allow(clippy::too_many_arguments)]
    pub fn budgeted(
        ctx: &CreationContext<'_>,
        id: u32,
        name_key: &str,
        side: u8,
        traits: [i32; 7],
        lifestyle_rv: i32,
        repute: i32,
        skills: Vec<SkillInstance>,
        powers: Vec<PowerInst>,
    ) -> Result<Character, &'static str> {
        for trait_ in TRAITS {
            if traits[trait_.index()] < 1 {
                return Err("no character may have a stored Rank Value of 0");
            }
        }
        if lifestyle_rv < 1 {
            return Err("lifestyle must be a positive Rank Value");
        }
        let spent: i32 =
            traits.iter().sum::<i32>() + lifestyle_rv + skills.len() as i32 * ctx.skill_cost;
        if spent > ctx.budget_points {
            return Err("the character spends more than the point-buy budget");
        }
        let mut log = vec![format!(
            "point-buy spent {spent} of {} (skills cost {} each)",
            ctx.budget_points, ctx.skill_cost
        )];
        log.push(
            "budgeted characters are validated against the rolled distribution by test (02:02.3)"
                .to_string(),
        );
        Ok(Character::from_sheet(
            id,
            name_key,
            "",
            side,
            CreationMode::Budgeted,
            traits,
            lifestyle_rv,
            repute,
            skills,
            Vec::new(),
            powers,
            log,
        ))
    }

    /// Build a character from an authored sheet (02:02.3).
    #[allow(clippy::too_many_arguments)]
    pub fn authored(
        id: u32,
        name_key: &str,
        origin: &str,
        side: u8,
        traits: [i32; 7],
        lifestyle_rv: i32,
        repute: i32,
        skills: Vec<SkillInstance>,
        items: Vec<String>,
        powers: Vec<PowerInst>,
    ) -> Character {
        Character::from_sheet(
            id,
            name_key,
            origin,
            side,
            CreationMode::Authored,
            traits,
            lifestyle_rv,
            repute,
            skills,
            items,
            powers,
            vec!["authored sheet: every value comes from content".to_string()],
        )
    }

    /// The shared tail of the three modes, so they cannot diverge.
    #[allow(clippy::too_many_arguments)]
    fn from_sheet(
        id: u32,
        name_key: &str,
        origin: &str,
        side: u8,
        mode: CreationMode,
        traits: [i32; 7],
        lifestyle_rv: i32,
        repute: i32,
        skills: Vec<SkillInstance>,
        items: Vec<String>,
        powers: Vec<PowerInst>,
        creation_log: Vec<String>,
    ) -> Character {
        let mut character = Character {
            id,
            name_key: name_key.to_string(),
            origin: origin.to_string(),
            side,
            mode,
            traits,
            damage: 0,
            max_damage: 0,
            fortune: 0,
            repute,
            lifestyle_rv,
            skills,
            items,
            weapon: None,
            armour: Vec::new(),
            conditions: Vec::new(),
            powers,
            power_floors: Vec::new(),
            power_armor: 0,
            temporary_steps: Vec::new(),
            trait_boosts: Vec::new(),
            dying_steps: 0,
            dead: false,
            moved_tiles: 0,
            dodge_steps: 0,
            creation_log,
        };
        character.recompute_derived();
        character.damage = character.max_damage;
        character
    }

    /// Recompute the Damage ceiling and starting Fortune from the stored traits
    /// (`4c:190`, `4c:196`).
    ///
    /// This is a **recompute**, not a preserve: it is the fresh-character path and
    /// the load path, both of which want the ceilings and want the pools filled
    /// from them. The one caller that must not refill is advancement, and it says
    /// so itself ([`crate::l4::progression::advance_trait`]) rather than making
    /// every other caller remember.
    ///
    /// The current Damage pool is deliberately *not* touched by the recompute
    /// itself: a loaded character with 0 Damage is dying, not fresh, and
    /// [`Character::from_sheet`] is what fills a new one.
    pub fn recompute_derived(&mut self) {
        let ladder = crate::tables::CAMPAIGN_LADDER;
        let effective = Effective {
            ladder,
            stored: &self.traits,
            steps: Vec::new(),
            floors: Vec::new(),
        };
        self.max_damage = effective.damage_score();
        self.fortune = effective.fortune_score();
    }

    /// The stored trait Rank Value.
    pub fn stored(&self, trait_: Trait) -> i32 {
        self.traits[trait_.index()]
    }

    /// The effective-trait reader a roll must use (`02:02.3`).
    pub fn effective<'a>(&'a self, ladder: &'a Ladder) -> Effective<'a> {
        let mut steps = status::trait_steps(&self.conditions);
        if self.dying_steps != 0 {
            // 4c:1161: Fortitude drops one row step per panel while dying.
            steps.push(crate::l1::traits::TraitStep {
                target: Trait::Fortitude,
                steps: -self.dying_steps,
            });
        }
        steps.extend(self.temporary_steps.iter().copied());
        let mut floors = self.power_floors.clone();
        for boost in &self.trait_boosts {
            floors.push((boost.trait_, boost.rv));
        }
        Effective {
            ladder,
            stored: &self.traits,
            steps,
            floors,
        }
    }

    /// Count a panel down on every temporary boost, dropping the expired ones.
    ///
    /// Returns the traits that fell back, so the panel can emit an event
    /// (`4c:815`: "at the end of this time the trait is reduced to one-half").
    pub fn tick_boosts(&mut self) -> Vec<Trait> {
        let mut expired = Vec::new();
        for boost in self.trait_boosts.iter_mut() {
            if boost.panels > 0 {
                boost.panels -= 1;
            }
        }
        self.trait_boosts.retain(|boost| {
            if boost.panels == 0 {
                expired.push(boost.trait_);
                false
            } else {
                true
            }
        });
        expired
    }

    /// The power instance with an id, when the character carries it.
    pub fn power(&self, id: &str) -> Option<&PowerInst> {
        self.powers.iter().find(|power| power.id == id)
    }

    /// Take a power: record it and run its acquisition hook.
    ///
    /// The one entry point, so a power's `on_acquire` cannot be skipped by a
    /// caller that added it directly and then wondered why `Astoundingly
    /// Wealthy` did nothing.
    pub fn acquire_power(&mut self, power: PowerInst) {
        let source = crate::l1::powers::CanonicalPowers;
        self.acquire_power_with(&power, &source);
    }

    /// Take a power, resolving content-defined templates through `source`.
    ///
    /// The one path that **records** a power: it runs the acquisition hook and
    /// then stores the instance. A caller that only wants the hook's effect — an
    /// acquisition plan being replayed — uses [`Self::acquire_power_from`], which
    /// deliberately does not record, so a grant cannot recurse into itself.
    pub fn acquire_power_with(&mut self, power: &PowerInst, source: &dyn PowerSource) {
        self.acquire_power_from(power, source);
        self.powers.push(power.clone());
    }

    /// Run a power's acquisition hook without recording the instance.
    ///
    /// Used by [`Self::acquire_power_with`] and by the plan applier; a power that
    /// granted itself here would loop, which is why the recording half is
    /// separate.
    pub fn acquire_power_from(&mut self, power: &PowerInst, source: &dyn PowerSource) {
        let Some(kernel) = source.kernel_for(&power.id) else {
            self.creation_log
                .push(format!("power {} has no kernel; skipped", power.id));
            return;
        };
        let ladder = crate::tables::CAMPAIGN_LADDER;
        let owned = power.clone();
        let mut plan = crate::l1::powers::AcquirePlan::default();
        {
            let ctx = crate::l1::powers::PowerCtx {
                ladder,
                power: &owned,
                characters: std::slice::from_ref(self),
                owner: self.id,
                actor: self.id,
                target: self.id,
                distance: 0,
                colour: crate::l0::Colour::Blck,
                attack: None,
                charged: 0,
            };
            kernel.on_acquire(&ctx, &mut plan);
        }
        crate::l1::powers::apply_acquire_plan(self, &plan);
    }

    /// Recompute the modifier layers powers contribute (`D17`).
    ///
    /// Called after creation and after a load, never on a hot path. `D17` is
    /// explicit that a vehicle's (and here, a trait's) power buff is a modifier
    /// **layer** recomputed on load rather than baked into stored values, so
    /// removing the power removes the buff and the save needs no flag.
    pub fn recompute_power_effects(&mut self, source: &dyn PowerSource) {
        let ladder = crate::tables::CAMPAIGN_LADDER;
        let mut floors: Vec<(Trait, i32)> = Vec::new();
        let mut armour = 0;
        for index in 0..self.powers.len() {
            let power = self.powers[index].clone();
            let Some(kernel) = source.kernel_for(&power.id) else {
                continue;
            };
            for trait_ in TRAITS {
                let base = self.traits[trait_.index()];
                let current = floors
                    .iter()
                    .find(|(target, _)| *target == trait_)
                    .map(|(_, value)| *value)
                    .unwrap_or(base);
                let ctx = crate::l1::powers::PowerCtx {
                    ladder,
                    power: &power,
                    characters: std::slice::from_ref(self),
                    owner: self.id,
                    actor: self.id,
                    target: self.id,
                    distance: 0,
                    colour: crate::l0::Colour::Blck,
                    attack: None,
                    charged: 0,
                };
                let next = kernel.modify_trait(&ctx, self.id, trait_, current);
                if next != current {
                    match floors.iter_mut().find(|(target, _)| *target == trait_) {
                        Some(entry) => entry.1 = next,
                        None => floors.push((trait_, next)),
                    }
                }
            }
            let ctx = crate::l1::powers::PowerCtx {
                ladder,
                power: &power,
                characters: std::slice::from_ref(self),
                owner: self.id,
                actor: self.id,
                target: self.id,
                distance: 0,
                colour: crate::l0::Colour::Blck,
                attack: None,
                charged: 0,
            };
            armour = kernel.modify_armor(&ctx, armour);
        }
        self.power_floors = floors;
        self.power_armor = armour;
    }

    /// The row steps the character's conditions impose, materialised.
    pub fn trait_steps(&self) -> Vec<crate::l1::traits::TraitStep> {
        status::trait_steps(&self.conditions)
    }

    /// The permission the character's conditions leave (02:02.5).
    pub fn permission(&self) -> status::Permission {
        status::permission(&self.conditions)
    }

    /// The flat `d%` penalty from conditions.
    pub fn roll_penalty(&self) -> i32 {
        status::roll_penalty(&self.conditions)
    }

    /// Whether the character is dying (`4c:1161`).
    pub fn is_dying(&self) -> bool {
        self.conditions
            .iter()
            .any(|condition| condition.kind == status::ConditionKind::Dying)
    }

    /// Whether the character is out of the fight.
    pub fn is_out(&self) -> bool {
        self.conditions.iter().any(|condition| {
            matches!(
                condition.kind,
                status::ConditionKind::KnockedOut | status::ConditionKind::Paralyzed
            )
        })
    }

    /// The skill row-step bonus for an action tag (`4c:275`).
    pub fn skill_steps(&self, tag: ActionTag) -> i32 {
        self.skills.iter().map(|skill| skill.steps_for(tag)).sum()
    }

    /// Whether any carried item is a weapon.
    pub fn has_weapon(&self) -> bool {
        self.weapon.is_some()
    }

    /// Apply an origin's modifiers (`4c:124-131`).
    ///
    /// `chosen` is the trait `Changed Human` lifts, and the bonus skills are
    /// drawn from the campaign's pool so an origin cannot invent a skill.
    pub fn apply_origin(
        &mut self,
        origin: &Origin,
        chosen: Option<Trait>,
        rng: &mut Rng,
        pool: &[String],
    ) {
        if origin.all_traits != 0 {
            for trait_ in TRAITS {
                self.traits[trait_.index()] += origin.all_traits;
            }
        }
        for (trait_, bonus) in &origin.trait_bonuses {
            self.traits[trait_.index()] += bonus;
        }
        if let Some(trait_) = chosen {
            self.traits[trait_.index()] += origin.chosen_trait_bonus;
        }
        self.lifestyle_rv = (self.lifestyle_rv + origin.lifestyle_bonus).max(1);
        if let Some(repute) = origin.repute_set {
            self.repute = repute;
        }
        if origin.bonus_skills > 0 {
            let granted = roll_skills(pool, origin.bonus_skills, rng);
            let count = granted.len();
            self.skills.extend(granted);
            self.creation_log
                .push(format!("origin grants {count} bonus skill(s) (4c:128)"));
        }
        if let Some(trait_) = chosen {
            self.creation_log.push(format!(
                "origin lifts {} by {} (4c:129)",
                trait_.id(),
                origin.chosen_trait_bonus
            ));
        }
        if origin.power_delta != 0 {
            // The choice is the player's, so it is reported rather than rolled
            // (`02:02.11` rule 2, `4c:127`).
            self.creation_log.push(format!(
                "origin changes the power count by {:+} (4c:127)",
                origin.power_delta
            ));
        }
        self.creation_log
            .push(format!("origin {} applied (4c:124-131)", origin.id));
        self.recompute_derived();
    }
}

/// An origin's mechanical modifiers (`4c:124-131`).
///
/// One struct with optional fields rather than six kernels: the spec prints the
/// origins as a table of modifiers, so the engine reads a table. The two
/// selection-shaped entries (`Changed Human`'s "select one", `Mutant`'s "gains
/// one bonus power") are surfaced in the creation log and the events, because the
/// choice belongs to the player, not to a roll (`02:02.11` rule 2).
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Origin {
    /// The record id.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// A bonus to every Primary Trait.
    pub all_traits: i32,
    /// Bonuses to named traits.
    pub trait_bonuses: Vec<(Trait, i32)>,
    /// A bonus the player applies to one chosen trait.
    pub chosen_trait_bonus: i32,
    /// A bonus to the Lifestyle Rank Value.
    pub lifestyle_bonus: i32,
    /// An absolute Repute score, when the origin fixes it.
    pub repute_set: Option<i32>,
    /// Bonus skills granted.
    pub bonus_skills: i32,
    /// A change to the number of powers the character may hold.
    pub power_delta: i32,
}

impl Origin {
    /// Read an origin record (`03:03.3`).
    pub fn parse(id: &str, data: &Json) -> Result<Origin, &'static str> {
        let mut trait_bonuses = Vec::new();
        if let Some(Json::Obj(fields)) = data.get("traits") {
            for (key, value) in fields {
                let trait_ = Trait::from_id(key).ok_or("an origin names an unknown trait")?;
                trait_bonuses.push((
                    trait_,
                    value.as_i64().ok_or("a trait bonus must be a number")? as i32,
                ));
            }
        }
        Ok(Origin {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            all_traits: data.get("all_traits").and_then(Json::as_i64).unwrap_or(0) as i32,
            trait_bonuses,
            chosen_trait_bonus: data.get("chosen_trait").and_then(Json::as_i64).unwrap_or(0) as i32,
            lifestyle_bonus: data.get("lifestyle").and_then(Json::as_i64).unwrap_or(0) as i32,
            repute_set: data.get("repute").and_then(Json::as_i64).map(|n| n as i32),
            bonus_skills: data.get("bonus_skills").and_then(Json::as_i64).unwrap_or(0) as i32,
            power_delta: data.get("power_delta").and_then(Json::as_i64).unwrap_or(0) as i32,
        })
    }
}

/// Pick `count` distinct skill ids from `pool`, deterministically.
///
/// A partial Fisher-Yates draw, so the same seed gives the same sheet and no
/// skill repeats.
fn roll_skills(pool: &[String], count: i32, rng: &mut Rng) -> Vec<SkillInstance> {
    let mut remaining: Vec<&String> = pool.iter().collect();
    let mut chosen = Vec::new();
    for _ in 0..count.max(0) {
        if remaining.is_empty() {
            break;
        }
        let index = (rng.next_u32() as usize) % remaining.len();
        let id = remaining.swap_remove(index).clone();
        chosen.push(SkillInstance {
            skill: id,
            steps: 1,
            // The registry fills the tags in when it resolves the record; a
            // rolled skill with no tags would be inert, so the pool is only
            // built from records that have them.
            applies_to: Vec::new(),
        });
    }
    chosen
}

/// The melee damage bonus a hand count grants (`4c:1074`).
pub fn melee_hands_bonus(hands: Hands) -> i32 {
    match hands {
        Hands::Two => 10,
        Hands::One => 5,
        Hands::None | Hands::Ranged => 0,
    }
}

/// Whether an item record can be equipped as armour.
pub fn is_armour(item: &Item) -> bool {
    item.armour.is_some()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tables;

    fn context<'a>(
        generation: &'a GenerationTable,
        count: &'a CountTable,
        pool: &'a [String],
    ) -> CreationContext<'a> {
        CreationContext {
            ladder: tables::CAMPAIGN_LADDER,
            generation,
            skills_count: count,
            skill_pool: pool.to_vec(),
            budget_points: 200,
            skill_cost: 25,
        }
    }

    #[test]
    fn the_generation_table_is_reproduced_literally_including_its_odd_band() {
        // D1 and 02:02.3: `00-04 -> 10` is *better* than `05-09 -> 3`.
        let table = GenerationTable::canonical();
        assert_eq!(table.rv_for(3), 10);
        assert_eq!(table.rv_for(7), 3);
        assert_eq!(table.rv_for(15), 6);
        assert_eq!(table.rv_for(30), 10);
        assert_eq!(table.rv_for(99), 50);
        // The table tiles 0..=99 with no hole.
        let mut covered = [false; 100];
        for band in &table.bands {
            for roll in band.lo..=band.hi {
                assert!(!covered[roll as usize], "roll {roll} covered twice");
                covered[roll as usize] = true;
            }
        }
        assert!(covered.iter().all(|seen| *seen));
    }

    #[test]
    fn every_creation_mode_produces_the_same_record_shape() {
        let generation = GenerationTable::canonical();
        let count = CountTable::canonical();
        let pool = vec![
            "wsp.skill.martial-arts".to_string(),
            "wsp.skill.investigation".to_string(),
        ];
        let ctx = context(&generation, &count, &pool);

        let mut rng = Rng::new(42);
        let rolled = Character::rolled(&ctx, &mut rng, 1, "hero.rolled", 0);
        assert_eq!(rolled.mode, CreationMode::Rolled);
        assert_eq!(rolled.damage, rolled.max_damage);
        assert!(rolled.creation_log.iter().any(|line| line.contains("D1")));

        let budgeted = Character::budgeted(
            &ctx,
            2,
            "hero.budgeted",
            0,
            [10, 10, 10, 10, 10, 10, 10],
            10,
            5,
            vec![],
            vec![],
        )
        .expect("within budget");
        assert_eq!(budgeted.mode, CreationMode::Budgeted);
        assert_eq!(budgeted.max_damage, 40);
        assert_eq!(budgeted.fortune, 30);

        let authored = Character::authored(
            3,
            "npc.thug",
            "wsp.origin.none",
            1,
            [6, 6, 6, 6, 6, 6, 6],
            6,
            4,
            vec![],
            vec!["wsp.item.bat".to_string()],
            vec![],
        );
        assert_eq!(authored.mode, CreationMode::Authored);
        assert_eq!(authored.items.len(), 1);
    }

    #[test]
    fn a_budgeted_character_may_not_overspend_or_store_a_zero_rank_value() {
        let generation = GenerationTable::canonical();
        let count = CountTable::canonical();
        let pool: Vec<String> = Vec::new();
        let ctx = context(&generation, &count, &pool);
        assert!(Character::budgeted(
            &ctx,
            1,
            "x",
            0,
            [50, 50, 50, 50, 50, 50, 50],
            50,
            1,
            vec![],
            vec![],
        )
        .is_err());
        assert!(Character::budgeted(
            &ctx,
            1,
            "x",
            0,
            [0, 10, 10, 10, 10, 10, 10],
            10,
            1,
            vec![],
            vec![],
        )
        .is_err());
    }

    #[test]
    fn a_rolled_character_never_stores_a_zero_rank_value() {
        let generation = GenerationTable::canonical();
        let count = CountTable::canonical();
        let pool: Vec<String> = Vec::new();
        let ctx = context(&generation, &count, &pool);
        for seed in 0..500u64 {
            let mut rng = Rng::new(seed);
            let rolled = Character::rolled(&ctx, &mut rng, 1, "hero", 0);
            for trait_ in TRAITS {
                assert!(rolled.stored(trait_) >= 1, "4c:212");
            }
        }
    }

    #[test]
    fn the_rolled_skill_count_is_deterministic_and_distinct() {
        let pool: Vec<String> = (0..6).map(|n| format!("wsp.skill.s{n}")).collect();
        let mut a = Rng::new(9);
        let mut b = Rng::new(9);
        let first = roll_skills(&pool, 4, &mut a);
        let second = roll_skills(&pool, 4, &mut b);
        assert_eq!(first, second);
        let mut ids: Vec<&str> = first.iter().map(|skill| skill.skill.as_str()).collect();
        ids.sort_unstable();
        ids.dedup();
        assert_eq!(ids.len(), first.len(), "a skill may not be drawn twice");
    }
}
