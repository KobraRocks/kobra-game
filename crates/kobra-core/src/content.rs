//! The content registry: pack merge, structural rules and the two identities
//! (03:03.5, AD-14, AD-29).
//!
//! A **content pack** is one JSON file holding a list of typed records, and the
//! registry applies them in order: **base packs, then mods ascending by
//! `priority`** (ties by id ascending, so the highest priority is applied last
//! and wins). The merge semantics live here, in the core, not in the loader: the
//! architecture is explicit that reading files is a browser job and the
//! *semantics of the merge* are checked in WASM (`01:01.2`), so a putter-together
//! of packs cannot disagree with the game.
//!
//! Two identities fall out of the merge (`AD-29`):
//!
//! - **`rules_hash`** — the structural tables plus every rules-bearing record.
//!   This is what a save binds (`AD-11`), and a mismatch is a hard, explained
//!   refusal.
//! - **`presentation_hash`** — visual descriptors, string tables and their
//!   assets. A change here is recorded and **never** invalidates a save.
//!
//! **Structural types may not be overridden or removed at all** (`AD-14`): the
//! resolution table is not moddable, because a save's meaning depends on it, and
//! under `AD-33` the tile size is the same kind of thing.

use std::collections::BTreeMap;

use crate::dsl::Template;
use crate::json::Json;
use crate::l1::character::{Character, CreationContext, Origin};
use crate::l1::items::Item;
use crate::l1::powers::{self, PowerInst, PowerSource};
use crate::l1::skills::Skill;
use crate::l1::skills::SkillInstance;
use crate::l2::actions::{ActionKind, OutcomeTable};
use crate::l2::encounter::RulesContent;
use crate::l3::gen::Generator;
use crate::l3::world::World;
use crate::l4::dialogue::Dialogue;
use crate::l4::economy::Shop;
use crate::l4::factions::Faction;
use crate::l4::journal::CampaignEffect;
use crate::l4::quests::Quest;
use crate::l4::signals::SignalRecord;
use crate::rules::Rules;
use crate::tables;

/// The load-document schema version the core accepts.
pub const CONTENT_LOAD_SCHEMA: &str = "kobra.content-load/1";

/// Record types that are **structural**: never overridable, never removable
/// (`AD-14`, 03:03.5).
pub const STRUCTURAL_TYPES: [&str; 4] = ["ladder", "trait_order", "dice_vocab", "effect_op"];

/// Record types that carry presentation only (`AD-29`).
///
/// `signal` is here rather than in [`STRUCTURAL_TYPES`] because a display gate
/// changes only what the player sees: a HUD's gating logic cannot invalidate a
/// save (`09:09.4`).
pub const PRESENTATION_TYPES: [&str; 3] = ["visual", "string_table", "signal"];

/// One authored character sheet from content.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct AuthoredSheet {
    /// The record id.
    pub id: String,
    /// The display string id.
    pub name_key: String,
    /// The origin record id.
    pub origin: String,
    /// The stored traits.
    pub traits: [i32; 7],
    /// The Lifestyle Rank Value.
    pub lifestyle_rv: i32,
    /// The Repute score.
    pub repute: i32,
    /// Skill record ids.
    pub skills: Vec<String>,
    /// Item record ids.
    pub items: Vec<String>,
    /// The powers the sheet carries (`02:02.4`).
    pub powers: Vec<PowerInst>,
}

/// A spawn point in an encounter record.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Spawn {
    /// The authored character record id.
    pub character: String,
    /// The tile to stand on.
    pub x: i32,
    /// The tile to stand on.
    pub y: i32,
}

/// An encounter record (`03:03.3`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct EncounterRecord {
    /// The record id.
    pub id: String,
    /// The map record id.
    pub map: String,
    /// Where the player's characters stand, in creation order.
    pub party_spawns: Vec<(i32, i32)>,
    /// The opposition.
    pub enemies: Vec<Spawn>,
    /// What winning the encounter does (`02:02.7`: an event authors its own
    /// Fortune, Repute and standing movement).
    pub on_win: Vec<CampaignEffect>,
    /// What losing it does.
    pub on_loss: Vec<CampaignEffect>,
    /// The dialogue that opens when the fight ends, when the campaign wants a
    /// scene rather than a summary.
    pub epilogue: String,
}

/// A station a campaign places in its world (`02:02.6`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct StationRecord {
    /// The character sheet record id.
    pub character: String,
    /// The faction the station belongs to.
    pub faction: String,
    /// Where it belongs.
    pub at: (i32, i32),
    /// How it behaves (`l3::ai::BehaviourRecord`).
    pub behaviour: crate::l3::ai::BehaviourRecord,
    /// Its schedule (`02:02.7`).
    pub schedule: crate::l3::clock::Schedule,
    /// The dialogue that opens when the party talks to it, if any.
    pub dialogue: String,
    /// The merchant counter this station keeps, if any (`02:02.13`).
    ///
    /// A shop with no station is a shop nobody sells at: the trade surface is
    /// anchored to a person standing in the world, so "spend Fortune" is offered
    /// where a merchant actually is rather than everywhere at once.
    pub shop: String,
    /// Whether it joins the party rather than standing in the world.
    pub party: bool,
}

/// The campaign's binding to its generated region (`02:02.6`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct WorldRecord {
    /// The record id.
    pub id: String,
    /// The generator that produced the baseline.
    pub generator: Generator,
    /// The authored maps overlaid on the baseline, with their origins.
    pub overlays: Vec<(String, i32, i32)>,
    /// Where the party arrives.
    pub start: (i32, i32),
    /// The minute of day the campaign opens at (`02:02.7`).
    ///
    /// A campaign that begins at midnight puts every scheduled NPC at its night
    /// post and makes its own town look abandoned; the shipped default is dawn,
    /// which is where a schedule reads best. It is content, because a campaign
    /// set at night is a decision rather than a bug.
    pub start_minute: u32,
    /// The stations the campaign places.
    pub stations: Vec<StationRecord>,
    /// The conversation the party leader has with themselves, if the campaign
    /// authors one (`02:02.13`).
    ///
    /// Talk needs a partner or a reason to speak alone. An empty id means the
    /// leader has nothing to say with nobody in reach, so the action is simply
    /// not offered; a campaign that authors one makes the diary a place.
    pub self_dialogue: String,
}

/// A map record.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct MapRecord {
    /// The record id.
    pub id: String,
    /// The map itself.
    pub world: World,
}

/// One pack, as the loader described it.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct PackInfo {
    /// `base`, or a mod id.
    pub source: String,
    /// The pack's own id.
    pub pack: String,
    /// `content` or `presentation`.
    pub kind: String,
    /// The file path, for a validation message.
    pub path: String,
    /// Sort key among mods.
    pub priority: i32,
}

/// The merged content the sim plays from.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct ContentRegistry {
    /// The effective rules pack.
    pub rules: Rules,
    /// Every item, by id.
    pub items: Vec<Item>,
    /// Every skill, by id.
    pub skills: Vec<Skill>,
    /// Every authored character sheet, by id.
    pub characters: Vec<AuthoredSheet>,
    /// Every map, by id.
    pub maps: Vec<MapRecord>,
    /// Every encounter, by id.
    pub encounters: Vec<EncounterRecord>,
    /// Every content-declared outcome table, by action.
    pub outcomes: Vec<OutcomeTable>,
    /// Every content-defined power, compiled from the effect DSL (`AD-16`).
    pub templates: Vec<Template>,
    /// Every origin, by id (`4c:124-131`).
    pub origins: Vec<Origin>,
    /// Every vehicle record, by id (`4c:1367-1375`).
    pub vehicles: Vec<crate::l2::vehicle::Vehicle>,
    /// Every faction (`02:02.6`).
    pub factions: Vec<Faction>,
    /// Every dialogue graph (`02:02.7`).
    pub dialogues: Vec<Dialogue>,
    /// Every quest (`02:02.7`).
    pub quests: Vec<Quest>,
    /// Every merchant (`02:02.7`).
    pub shops: Vec<Shop>,
    /// Every generated world the campaign declares (`02:02.6`).
    pub worlds: Vec<WorldRecord>,
    /// Every named display gate content declares (`09:09.4`).
    pub signals: Vec<SignalRecord>,
    /// The packs that were applied, in application order.
    pub packs: Vec<PackInfo>,
    /// The hash a save binds (`AD-11`, `AD-29`).
    pub rules_hash: u64,
    /// The cosmetic identity, which never invalidates a save (`AD-29`).
    pub presentation_hash: u64,
}

impl RulesContent for ContentRegistry {
    fn item(&self, id: &str) -> Option<&Item> {
        self.items.iter().find(|item| item.id == id)
    }

    fn outcome(&self, action: ActionKind) -> &OutcomeTable {
        self.outcomes
            .iter()
            .find(|table| table.action == action)
            .unwrap_or_else(|| canonical_outcome(action))
    }

    fn skill(&self, id: &str) -> Option<&Skill> {
        self.skills.iter().find(|skill| skill.id == id)
    }

    fn power_kernel(&self, id: &str) -> Option<&dyn crate::l1::powers::PowerKernel> {
        <Self as PowerSource>::kernel_for(self, id)
    }
}

impl PowerSource for ContentRegistry {
    /// Resolve a power id: a canonical kernel first, then a compiled template.
    ///
    /// `AD-16`: canonical powers stay Rust kernels "exactly as written", and a
    /// content-defined id becomes a `Template` driven by the effect DSL. A
    /// content record may not shadow a canonical kernel — the validator reports
    /// it rather than letting a mod redefine `Body Armor`.
    fn kernel_for(&self, id: &str) -> Option<&dyn crate::l1::powers::PowerKernel> {
        powers::kernel(id).or_else(|| {
            self.templates
                .iter()
                .find(|template| template.power_id() == id)
                .map(|template| template as &dyn crate::l1::powers::PowerKernel)
        })
    }
}

/// A canonical table for an action, kept alive for the borrow in
/// [`RulesContent::outcome`].
///
/// Content always declares the four M1 tables, so this is the fallback the trait
/// needs rather than the normal path; `validate` reports a missing table so the
/// fallback is never silently load-bearing.
fn canonical_outcome(action: ActionKind) -> &'static OutcomeTable {
    use std::sync::OnceLock;
    static TABLES: OnceLock<Vec<OutcomeTable>> = OnceLock::new();
    let tables = TABLES.get_or_init(|| {
        ActionKind::ALL
            .into_iter()
            .map(OutcomeTable::canonical)
            .collect()
    });
    tables
        .iter()
        .find(|table| table.action == action)
        .expect("one canonical table per action")
}

impl ContentRegistry {
    /// An empty registry with the shipped defaults, for tests and the editor.
    pub fn empty() -> ContentRegistry {
        let mut registry = ContentRegistry {
            rules: Rules::default(),
            items: Vec::new(),
            skills: Vec::new(),
            characters: Vec::new(),
            maps: Vec::new(),
            encounters: Vec::new(),
            outcomes: Vec::new(),
            templates: Vec::new(),
            origins: Vec::new(),
            vehicles: Vec::new(),
            factions: Vec::new(),
            dialogues: Vec::new(),
            quests: Vec::new(),
            shops: Vec::new(),
            worlds: Vec::new(),
            signals: Vec::new(),
            packs: Vec::new(),
            rules_hash: 0,
            presentation_hash: 0,
        };
        registry.rehash();
        registry
    }

    /// The item named by an id.
    pub fn item(&self, id: &str) -> Option<&Item> {
        self.items.iter().find(|item| item.id == id)
    }

    /// The skill named by an id.
    pub fn skill(&self, id: &str) -> Option<&Skill> {
        self.skills.iter().find(|skill| skill.id == id)
    }

    /// The display gate named by an id (`09:09.4`).
    pub fn signal(&self, id: &str) -> Option<&SignalRecord> {
        self.signals.iter().find(|signal| signal.id == id)
    }

    /// The authored sheet named by an id.
    pub fn sheet(&self, id: &str) -> Option<&AuthoredSheet> {
        self.characters.iter().find(|sheet| sheet.id == id)
    }

    /// The map named by an id.
    pub fn map(&self, id: &str) -> Option<&MapRecord> {
        self.maps.iter().find(|map| map.id == id)
    }

    /// The encounter named by an id.
    pub fn encounter(&self, id: &str) -> Option<&EncounterRecord> {
        self.encounters.iter().find(|encounter| encounter.id == id)
    }

    /// The origin named by an id (`4c:124-131`).
    pub fn origin(&self, id: &str) -> Option<&Origin> {
        self.origins.iter().find(|origin| origin.id == id)
    }

    /// The vehicle record named by an id (`4c:1367-1375`).
    pub fn vehicle(&self, id: &str) -> Option<&crate::l2::vehicle::Vehicle> {
        self.vehicles.iter().find(|vehicle| vehicle.record == id)
    }

    /// The faction named by an id (`02:02.6`).
    pub fn faction(&self, id: &str) -> Option<&Faction> {
        self.factions.iter().find(|faction| faction.id == id)
    }

    /// The dialogue graph named by an id (`02:02.7`).
    pub fn dialogue(&self, id: &str) -> Option<&Dialogue> {
        self.dialogues.iter().find(|dialogue| dialogue.id == id)
    }

    /// The quest named by an id (`02:02.7`).
    pub fn quest(&self, id: &str) -> Option<&Quest> {
        self.quests.iter().find(|quest| quest.id == id)
    }

    /// The merchant named by an id (`02:02.7`).
    pub fn shop(&self, id: &str) -> Option<&Shop> {
        self.shops.iter().find(|shop| shop.id == id)
    }

    /// The world record named by an id (`02:02.6`).
    pub fn world(&self, id: &str) -> Option<&WorldRecord> {
        self.worlds.iter().find(|world| world.id == id)
    }

    /// Every faction's declared default standing, as a snapshot.
    ///
    /// A **snapshot rather than a borrowing closure**, because the caller holds it
    /// across a mutable borrow of the sim: `Journal::apply_plan` needs the
    /// defaults and a `&mut Character` at the same time, and a closure that
    /// borrowed the registry would keep the whole sim borrowed with it.
    pub fn standing_defaults(&self) -> std::collections::BTreeMap<String, i32> {
        self.factions
            .iter()
            .map(|faction| (faction.id.clone(), faction.default_standing))
            .collect()
    }

    /// Every item record id, for the predicate context.
    pub fn item_ids(&self) -> Vec<String> {
        self.items.iter().map(|item| item.id.clone()).collect()
    }

    /// The resolution an encounter's outcome binds (`02:02.7`).
    pub fn encounter_outcome(&self, id: &str) -> Option<&EncounterRecord> {
        self.encounter(id)
    }

    /// Build a [`CreationContext`] for the character factory.
    pub fn creation_context(&self) -> CreationContext<'_> {
        CreationContext {
            ladder: tables::CAMPAIGN_LADDER,
            generation: &self.rules.generation,
            skills_count: &self.rules.skill_table,
            skill_pool: self.skill_ids(),
            budget_points: self.rules.budget_points,
            skill_cost: self.rules.skill_cost,
        }
    }

    /// The skill ids a rolled character may draw from, in a stable order.
    pub fn skill_ids(&self) -> Vec<String> {
        self.skills.iter().map(|skill| skill.id.clone()).collect()
    }

    /// Resolve a rolled skill id to a full instance, copying the record's tags.
    ///
    /// A rolled skill with no tags would be inert, so the instance is built from
    /// the record rather than from the pool.
    pub fn skill_instance(&self, id: &str) -> Option<SkillInstance> {
        let skill = self.skill(id)?;
        Some(SkillInstance {
            skill: skill.id.clone(),
            steps: skill.steps,
            applies_to: skill.applies_to.clone(),
        })
    }

    /// Build an authored character from a sheet record (`02:02.3`).
    pub fn authored_character(
        &self,
        sheet_id: &str,
        id: u32,
        side: u8,
    ) -> Result<Character, &'static str> {
        let sheet = self.sheet(sheet_id).ok_or("no such character record")?;
        let mut skills = Vec::new();
        for skill_id in &sheet.skills {
            skills.push(
                self.skill_instance(skill_id)
                    .ok_or("a character references an unknown skill")?,
            );
        }
        let mut character = Character::authored(
            id,
            &sheet.name_key,
            &sheet.origin,
            side,
            sheet.traits,
            sheet.lifestyle_rv,
            sheet.repute,
            skills,
            sheet.items.clone(),
            sheet.powers.clone(),
        );
        // Equip the first weapon and every piece of armour, so a sheet is
        // playable without an explicit equip step.
        if let Some(weapon) = sheet
            .items
            .iter()
            .find(|item_id| self.item(item_id).is_some_and(Item::is_weapon))
        {
            character.weapon = Some(weapon.clone());
        }
        character.armour = sheet
            .items
            .iter()
            .filter(|item_id| self.item(item_id).is_some_and(|item| item.armour.is_some()))
            .cloned()
            .collect();
        character.recompute_power_effects(self);
        Ok(character)
    }

    /// Compute the two hashes (`AD-29`).
    pub fn rehash(&mut self) {
        let mut rules = Fnv::new();
        rules.write(b"wsp.rules/1");
        for item in &self.items {
            rules.write(item.id.as_bytes());
            rules.write(item.name_key.as_bytes());
        }
        for skill in &self.skills {
            rules.write(skill.id.as_bytes());
            rules.write(&skill.steps.to_le_bytes());
        }
        for sheet in &self.characters {
            rules.write(sheet.id.as_bytes());
            for value in sheet.traits {
                rules.write(&value.to_le_bytes());
            }
        }
        for map in &self.maps {
            rules.write(map.id.as_bytes());
            for tile in &map.world.tiles {
                rules.write(&[tile.level as u8, tile.blocking as u8, tile.terrain]);
            }
        }
        for encounter in &self.encounters {
            rules.write(encounter.id.as_bytes());
            rules.write(encounter.map.as_bytes());
        }
        for table in &self.outcomes {
            rules.write(table.action.id().as_bytes());
            for row in &table.rows {
                for effect in row {
                    rules.write(effect.id().as_bytes());
                    if let crate::l2::actions::Effect::DodgeSteps(steps) = effect {
                        rules.write(&steps.to_le_bytes());
                    }
                }
            }
        }
        // A faction's bands and defaults are rules-bearing: a save's standing is
        // read through them (`02:02.6`).
        for faction in &self.factions {
            rules.write(faction.id.as_bytes());
            rules.write(&faction.default_standing.to_le_bytes());
            rules.write(&[u8::from(faction.criminal)]);
            for band in &faction.bands {
                rules.write(band.id.as_bytes());
                rules.write(&band.lo.to_le_bytes());
                rules.write(&band.hi.to_le_bytes());
                rules.write(&[u8::from(band.hostile)]);
            }
        }
        // A quest's steps, its entry conditions and its bindings are the campaign.
        // The predicates' *shape* is hashed rather than every field, because the
        // point is to notice that a quest changed, and a stable per-step digest
        // does that without depending on a JSON field order.
        for quest in &self.quests {
            rules.write(quest.id.as_bytes());
            rules.write(quest.name_key.as_bytes());
            rules.write(&(quest.steps.len() as u32).to_le_bytes());
            for step in &quest.steps {
                rules.write(step.id.as_bytes());
                rules.write(step.text_key.as_bytes());
                rules.write(&[u8::from(step.auto_advance)]);
            }
            rules.write(&(quest.on_complete.len() as u32).to_le_bytes());
            rules.write(&(quest.on_fail.len() as u32).to_le_bytes());
            rules.write(quest.faction.as_bytes());
        }
        // A dialogue's graph shape, for the same reason.
        for dialogue in &self.dialogues {
            rules.write(dialogue.id.as_bytes());
            rules.write(dialogue.entry.as_bytes());
            rules.write(dialogue.partner.as_bytes());
            for node in &dialogue.nodes {
                rules.write(node.id.as_bytes());
                rules.write(node.text_key.as_bytes());
                rules.write(&(node.options.len() as u32).to_le_bytes());
                for option in &node.options {
                    rules.write(option.id.as_bytes());
                    rules.write(option.text_key.as_bytes());
                    rules.write(option.next.as_deref().unwrap_or("").as_bytes());
                    rules.write(&[u8::from(option.check.is_some())]);
                }
            }
        }
        // A shop's stock and a world's generator both move what a save means.
        for shop in &self.shops {
            rules.write(shop.id.as_bytes());
            for item in &shop.stock {
                rules.write(item.as_bytes());
            }
            for kind in &shop.buys {
                rules.write(kind.id().as_bytes());
            }
            rules.write(&shop.buy_percent.to_le_bytes());
        }
        for world in &self.worlds {
            rules.write(world.id.as_bytes());
            rules.write(&world.generator.width.to_le_bytes());
            rules.write(&world.generator.height.to_le_bytes());
            rules.write(&world.generator.seed.to_le_bytes());
            rules.write(&world.generator.rise_percent.to_le_bytes());
            rules.write(&world.generator.water_percent.to_le_bytes());
            rules.write(&world.generator.cover_percent.to_le_bytes());
            rules.write(&world.generator.feature_tiles.to_le_bytes());
            rules.write(&(world.stations.len() as u32).to_le_bytes());
            rules.write(&world.start.0.to_le_bytes());
            rules.write(&world.start.1.to_le_bytes());
            rules.write(crate::l3::gen::GENERATOR_VERSION.to_le_bytes().as_slice());
        }
        // A merchant's prices and the advancement curve move with the rules too.
        rules.write(&self.rules.advancement.power_cost.to_le_bytes());
        rules.write(&self.rules.advancement.skill_cost.to_le_bytes());
        rules.write(&self.rules.impact.fortune[0].to_le_bytes());
        rules.write(&self.rules.carry_slots.to_le_bytes());
        rules.write(&self.rules.panels_per_day.to_le_bytes());
        // The rules record's own keys, so a balance tweak moves the hash.
        rules.write(self.rules.attack_penalty.steps.to_le_bytes().as_slice());
        rules.write(self.rules.knockback_tiles.to_le_bytes().as_slice());
        self.rules_hash = rules.finish();

        let mut presentation = Fnv::new();
        presentation.write(b"wsp.presentation/1");
        for pack in &self.packs {
            presentation.write(pack.pack.as_bytes());
        }
        // A signal is presentation, so a mod that changes one moves this hash and
        // never `rules_hash` (`09:09.4`, `AD-29`). The body is hashed as authored
        // rather than as parsed, so a cosmetic change to a condition that does not
        // alter its meaning still changes the pack's presentation identity.
        for signal in &self.signals {
            presentation.write(signal.id.as_bytes());
            presentation.write(signal.data.to_string().as_bytes());
        }
        self.presentation_hash = presentation.finish();
    }
}

/// FNV-1a, the same construction the state hash uses.
struct Fnv(u64);

impl Fnv {
    fn new() -> Fnv {
        Fnv(0xcbf2_9ce4_8422_2325)
    }
    fn write(&mut self, bytes: &[u8]) {
        for byte in bytes {
            self.0 ^= u64::from(*byte);
            self.0 = self.0.wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
    fn finish(&self) -> u64 {
        self.0
    }
}

// ---------------------------------------------------------------------------
// Loading and merging (`03:03.5`, `03:03.8`, `03:03.9`)
// ---------------------------------------------------------------------------

/// One record after the merge, with provenance for a message.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct RecordEntry {
    /// The record id.
    pub id: String,
    /// The record type.
    pub type_: String,
    /// `base` or the mod id that supplied it.
    pub source: String,
    /// The pack file it came from.
    pub path: String,
    /// Whether the id was first defined by a **base** pack.
    ///
    /// Preserved across an override: a mod that replaces a base record supplies
    /// the new data but does not turn the id into a mod-only record, which is
    /// what keeps "a base record may not reference a mod record" from firing on
    /// every legitimate override (`03:03.9` rule 2).
    pub base_defined: bool,
    /// Its data.
    pub data: Json,
}

/// The result of a load: the registry, the report, and whether the runtime may
/// use it.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Loaded {
    /// The merged registry, even when `ok` is false, so the editor can show what
    /// it would have loaded.
    pub registry: ContentRegistry,
    /// The load report as JSON, available to the UI (`03:03.8`).
    pub report: Json,
    /// Whether the registry is usable by the runtime.
    pub ok: bool,
    /// The merged records, for a validator that wants provenance.
    pub records: BTreeMap<String, RecordEntry>,
}

/// Load a content document: merge the packs, then validate the result.
///
/// The document is `kobra.content-load/1`: the packs the JS loader read
/// off disk, each with its `source`, its `priority`, and its records. The merge
/// order and every structural rule are applied here.
pub fn load(document: &Json) -> Loaded {
    let mut problems: Vec<Json> = Vec::new();
    let mut skipped: Vec<Json> = Vec::new();
    let mut applied: Vec<PackInfo> = Vec::new();
    let mut records: BTreeMap<String, RecordEntry> = BTreeMap::new();
    let mut disabled_sources: Vec<String> = Vec::new();

    if document.get("schema").and_then(Json::as_str) != Some(CONTENT_LOAD_SCHEMA) {
        problems.push(problem(
            "error",
            "bad_schema",
            "",
            &[("expected", Json::string(CONTENT_LOAD_SCHEMA))],
        ));
        return finish(
            ContentRegistry::empty(),
            problems,
            applied,
            skipped,
            records,
            false,
        );
    }
    let Some(Json::Arr(packs)) = document.get("packs") else {
        problems.push(problem("error", "no_packs", "", &[]));
        return finish(
            ContentRegistry::empty(),
            problems,
            applied,
            skipped,
            records,
            false,
        );
    };

    // Base first, then mods by priority ascending, ties by source id ascending
    // (03:03.5). The loader may have read them in any order.
    let mut order: Vec<&Json> = packs.iter().collect();
    order.sort_by(|a, b| {
        let key = |pack: &Json| {
            let source = pack.get("source").and_then(Json::as_str).unwrap_or("base");
            let priority = pack.get("priority").and_then(Json::as_i64).unwrap_or(0);
            let base = if source == "base" { 0 } else { 1 };
            (base, priority, source.to_string())
        };
        key(a).cmp(&key(b))
    });

    for pack in order {
        let source = pack
            .get("source")
            .and_then(Json::as_str)
            .unwrap_or("base")
            .to_string();
        let pack_id = pack
            .get("pack")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string();
        let path = pack
            .get("path")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string();
        let kind = pack
            .get("kind")
            .and_then(Json::as_str)
            .unwrap_or("content")
            .to_string();

        if disabled_sources.contains(&source) {
            skipped.push(skip(&source, &pack_id, "source_disabled"));
            continue;
        }
        if !matches!(kind.as_str(), "content" | "presentation") {
            problems.push(problem("error", "bad_pack_kind", &path, &[]));
            if source != "base" {
                disabled_sources.push(source.clone());
            }
            continue;
        }
        if let Some(engine_min) = pack.get("engine_min").and_then(Json::as_str) {
            if version_above(engine_min, crate::ENGINE_VERSION) {
                // 03:03.9 rule 9: skip the whole pack, never half-load it.
                skipped.push(skip(&source, &pack_id, "engine_min"));
                continue;
            }
        }

        let mut pack_problems: Vec<Json> = Vec::new();
        let mut staged: Vec<RecordEntry> = Vec::new();
        let Some(Json::Arr(items)) = pack.get("records") else {
            pack_problems.push(problem("error", "no_records", &path, &[]));
            problem_for_source(
                &source,
                &pack_problems,
                &mut problems,
                &mut disabled_sources,
            );
            continue;
        };
        let mut presentation_only = true;
        for record in items {
            let id = record.get("id").and_then(Json::as_str).unwrap_or("");
            let type_ = record.get("type").and_then(Json::as_str).unwrap_or("");
            let data = record.get("data").cloned().unwrap_or(Json::Null);
            let override_ = record
                .get("override")
                .and_then(Json::as_bool)
                .unwrap_or(false);
            let remove = record
                .get("remove")
                .and_then(Json::as_bool)
                .unwrap_or(false);
            if id.is_empty() || type_.is_empty() {
                pack_problems.push(problem("error", "missing_id_or_type", &path, &[]));
                continue;
            }
            if !known_type(type_) {
                pack_problems.push(problem(
                    "error",
                    "unknown_type",
                    &path,
                    &[("type", Json::string(type_)), ("id", Json::string(id))],
                ));
                continue;
            }
            if !PRESENTATION_TYPES.contains(&type_) {
                presentation_only = false;
            }
            if STRUCTURAL_TYPES.contains(&type_) && (override_ || remove) {
                // AD-14: the resolution table is not moddable.
                pack_problems.push(problem(
                    "error",
                    "structural_override",
                    &path,
                    &[("type", Json::string(type_)), ("id", Json::string(id))],
                ));
                continue;
            }
            let existing = records.get(id);
            if existing.is_some() && !override_ {
                // 03:03.5: an id that is already present without an explicit
                // override is an error for that mod — it never silently
                // clobbers, and the check comes before the namespace rule so
                // the message names the real problem.
                pack_problems.push(problem(
                    "error",
                    "duplicate_id",
                    &path,
                    &[("id", Json::string(id))],
                ));
                continue;
            }
            if !override_ && !namespace_matches(id, &pack_id) {
                pack_problems.push(problem(
                    "error",
                    "bad_namespace",
                    &path,
                    &[("id", Json::string(id)), ("pack", Json::string(&pack_id))],
                ));
                continue;
            }
            if remove {
                records.remove(id);
                continue;
            }
            let base_defined = records
                .get(id)
                .map(|existing| existing.base_defined)
                .unwrap_or(source == "base");
            staged.push(RecordEntry {
                id: id.to_string(),
                type_: type_.to_string(),
                source: source.clone(),
                path: path.clone(),
                base_defined,
                data,
            });
        }
        if kind == "presentation" && !presentation_only {
            // AD-29: a cosmetic pack may not change balance or invalidate a save.
            pack_problems.push(problem("error", "kind_violation", &path, &[]));
        }
        if kind == "content" && presentation_only && !staged.is_empty() {
            pack_problems.push(problem("error", "kind_violation", &path, &[]));
        }
        if !pack_problems.is_empty() {
            problem_for_source(
                &source,
                &pack_problems,
                &mut problems,
                &mut disabled_sources,
            );
            continue;
        }
        for entry in staged {
            records.insert(entry.id.clone(), entry);
        }
        applied.push(PackInfo {
            source,
            pack: pack_id,
            kind,
            path,
            priority: pack.get("priority").and_then(Json::as_i64).unwrap_or(0) as i32,
        });
    }

    let mut registry = build_registry(&records, applied, &mut problems);
    registry.rehash();
    problems.extend(crate::validate::check(&registry, &records));
    problems.extend(crate::validate::check_overrides(document));
    let ok = !problems
        .iter()
        .any(|item| item.get("severity").and_then(Json::as_str) == Some("error"));
    let packs = registry.packs.clone();
    finish(registry, problems, packs, skipped, records, ok)
}

/// Turn merged records into the typed registry.
fn build_registry(
    records: &BTreeMap<String, RecordEntry>,
    applied: Vec<PackInfo>,
    problems: &mut Vec<Json>,
) -> ContentRegistry {
    let mut registry = ContentRegistry {
        rules: Rules::default(),
        items: Vec::new(),
        skills: Vec::new(),
        characters: Vec::new(),
        maps: Vec::new(),
        encounters: Vec::new(),
        outcomes: Vec::new(),
        templates: Vec::new(),
        origins: Vec::new(),
        vehicles: Vec::new(),
        factions: Vec::new(),
        dialogues: Vec::new(),
        quests: Vec::new(),
        shops: Vec::new(),
        worlds: Vec::new(),
        signals: Vec::new(),
        packs: applied,
        rules_hash: 0,
        presentation_hash: 0,
    };
    for entry in records.values() {
        match entry.type_.as_str() {
            "rules" => match Rules::parse(&entry.data) {
                Ok(rules) => registry.rules = rules,
                Err(message) => problems.push(problem(
                    "error",
                    "bad_rules",
                    &entry.path,
                    &[("message", Json::string(message))],
                )),
            },
            "item" => match Item::parse(&entry.id, &entry.data) {
                Ok(item) => registry.items.push(item),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_item",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "skill" => match Skill::parse(&entry.id, &entry.data) {
                Ok(skill) => registry.skills.push(skill),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_skill",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "character" => match parse_sheet(&entry.id, &entry.data) {
                Ok(sheet) => registry.characters.push(sheet),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_character",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "sector_map" => match World::parse(&entry.data) {
                Ok(world) => registry.maps.push(MapRecord {
                    id: entry.id.clone(),
                    world,
                }),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_map",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "encounter" => match parse_encounter(&entry.id, &entry.data) {
                Ok(encounter) => registry.encounters.push(encounter),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_encounter",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "vehicle" => match crate::l2::vehicle::Vehicle::parse(&entry.id, &entry.data) {
                Ok(vehicle) => registry.vehicles.push(vehicle),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_vehicle",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "origin" => match Origin::parse(&entry.id, &entry.data) {
                Ok(origin) => registry.origins.push(origin),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_origin",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "power" => match Template::compile(&entry.id, &entry.data) {
                Ok(template) => registry.templates.push(template),
                Err(errors) => {
                    for error in errors {
                        let mut record = error.to_json();
                        if let Json::Obj(fields) = &mut record {
                            fields.insert(0, ("severity".to_string(), Json::string("error")));
                        }
                        problems.push(record);
                    }
                }
            },
            "outcome_table" => {
                let action = entry
                    .data
                    .get("action")
                    .and_then(Json::as_str)
                    .and_then(ActionKind::from_id);
                match action {
                    Some(action) => match OutcomeTable::parse(action, &entry.data) {
                        Ok(table) => registry.outcomes.push(table),
                        Err(message) => problems.push(problem(
                            "error",
                            "bad_outcome_table",
                            &entry.path,
                            &[("message", Json::string(message))],
                        )),
                    },
                    None => problems.push(problem(
                        "error",
                        "bad_outcome_action",
                        &entry.path,
                        &[("id", Json::string(&entry.id))],
                    )),
                }
            }
            "faction" => match Faction::parse(&entry.id, &entry.data) {
                Ok(faction) => registry.factions.push(faction),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_faction",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "dialogue" => match Dialogue::parse(&entry.id, &entry.data) {
                Ok(dialogue) => registry.dialogues.push(dialogue),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_dialogue",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "quest" => match Quest::parse(&entry.id, &entry.data) {
                Ok(quest) => registry.quests.push(quest),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_quest",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "shop" => match Shop::parse(&entry.id, &entry.data) {
                Ok(shop) => registry.shops.push(shop),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_shop",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "world" => match parse_world(&entry.id, &entry.data) {
                Ok(world) => registry.worlds.push(world),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_world",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            // Presentation records are merged and hashed, never interpreted —
            // except a signal, whose condition the engine publishes a truth
            // value for (`09:09.4`).
            "signal" => match crate::l4::signals::parse(&entry.id, &entry.data) {
                Ok(signal) => registry.signals.push(signal),
                Err(message) => problems.push(problem(
                    "error",
                    "bad_signal",
                    &entry.path,
                    &[
                        ("id", Json::string(&entry.id)),
                        ("message", Json::string(message)),
                    ],
                )),
            },
            "visual" | "string_table" => {}
            _ => {}
        }
    }
    registry
}

/// Read an authored sheet (`02:02.3`).
fn parse_sheet(id: &str, data: &Json) -> Result<AuthoredSheet, &'static str> {
    let mut traits = [1i32; 7];
    for trait_ in crate::l1::traits::TRAITS {
        let value = data
            .get("traits")
            .and_then(|traits| traits.get(trait_.id()))
            .and_then(Json::as_i64)
            .ok_or("a character sheet needs all seven traits")?;
        if value < 1 {
            return Err("no character may have a stored Rank Value of 0");
        }
        traits[trait_.index()] = value as i32;
    }
    Ok(AuthoredSheet {
        id: id.to_string(),
        name_key: data
            .get("name_key")
            .and_then(Json::as_str)
            .unwrap_or(id)
            .to_string(),
        origin: data
            .get("origin")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        traits,
        lifestyle_rv: data.get("lifestyle_rv").and_then(Json::as_i64).unwrap_or(6) as i32,
        repute: data.get("repute").and_then(Json::as_i64).unwrap_or(0) as i32,
        skills: string_list(data.get("skills"))?,
        items: string_list(data.get("items"))?,
        powers: parse_powers(data.get("powers"))?,
    })
}

/// Read a sheet's powers (`02:02.4`).
fn parse_powers(value: Option<&Json>) -> Result<Vec<PowerInst>, &'static str> {
    match value {
        None => Ok(Vec::new()),
        Some(Json::Arr(items)) => {
            let mut out = Vec::new();
            for item in items {
                out.push(powers::parse_power(item)?);
            }
            Ok(out)
        }
        _ => Err("powers must be an array"),
    }
}

/// Read an encounter record.
fn parse_encounter(id: &str, data: &Json) -> Result<EncounterRecord, &'static str> {
    let map = data
        .get("map")
        .and_then(Json::as_str)
        .ok_or("an encounter needs a map")?
        .to_string();
    let mut party_spawns = Vec::new();
    if let Some(Json::Arr(spawns)) = data.get("party_spawns") {
        for spawn in spawns {
            let Json::Arr(pair) = spawn else {
                return Err("a party spawn is an [x, y] pair");
            };
            if pair.len() != 2 {
                return Err("a party spawn is an [x, y] pair");
            }
            party_spawns.push((
                pair[0].as_i64().ok_or("x must be a number")? as i32,
                pair[1].as_i64().ok_or("y must be a number")? as i32,
            ));
        }
    }
    let mut enemies = Vec::new();
    if let Some(Json::Arr(spawns)) = data.get("enemies") {
        for spawn in spawns {
            let Json::Arr(pair) = spawn.get("at").ok_or("an enemy spawn needs an at pair")? else {
                return Err("an enemy spawn's at is an [x, y] pair");
            };
            if pair.len() != 2 {
                return Err("an enemy spawn's at is an [x, y] pair");
            }
            enemies.push(Spawn {
                character: spawn
                    .get("character")
                    .and_then(Json::as_str)
                    .ok_or("an enemy spawn needs a character")?
                    .to_string(),
                x: pair[0].as_i64().ok_or("x must be a number")? as i32,
                y: pair[1].as_i64().ok_or("y must be a number")? as i32,
            });
        }
    }
    Ok(EncounterRecord {
        id: id.to_string(),
        map,
        party_spawns,
        enemies,
        on_win: crate::l4::journal::parse_effects(data.get("on_win"))?,
        on_loss: crate::l4::journal::parse_effects(data.get("on_loss"))?,
        epilogue: data
            .get("epilogue")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
    })
}

/// Read a station record (`02:02.6`).
fn parse_station(data: &Json) -> Result<StationRecord, &'static str> {
    let Json::Arr(pair) = data.get("at").ok_or("a station needs an at pair")? else {
        return Err("a station's at is an [x, y] pair");
    };
    if pair.len() != 2 {
        return Err("a station's at is an [x, y] pair");
    }
    let at = (
        pair[0].as_i64().ok_or("x must be a number")? as i32,
        pair[1].as_i64().ok_or("y must be a number")? as i32,
    );
    let mut schedule = crate::l3::clock::Schedule {
        entries: Vec::new(),
        home: Some(at),
    };
    if let Some(Json::Arr(entries)) = data.get("schedule") {
        for entry in entries {
            let Json::Arr(post) = entry.get("at").ok_or("a schedule entry needs an at pair")?
            else {
                return Err("a schedule entry's at is an [x, y] pair");
            };
            if post.len() != 2 {
                return Err("a schedule entry's at is an [x, y] pair");
            }
            schedule.entries.push(crate::l3::clock::ScheduleEntry {
                slot: match entry.get("slot").and_then(Json::as_str) {
                    None => None,
                    Some(name) => Some(
                        crate::l3::clock::DaySlot::from_id(name)
                            .ok_or("a schedule slot must be night, morning, day or evening")?,
                    ),
                },
                at: (
                    post[0].as_i64().ok_or("x must be a number")? as i32,
                    post[1].as_i64().ok_or("y must be a number")? as i32,
                ),
            });
        }
    }
    Ok(StationRecord {
        character: data
            .get("character")
            .and_then(Json::as_str)
            .ok_or("a station needs a character")?
            .to_string(),
        faction: data
            .get("faction")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        at,
        behaviour: crate::l3::ai::BehaviourRecord::parse(data)?,
        schedule,
        dialogue: data
            .get("dialogue")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        shop: data
            .get("shop")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        party: data.get("party").and_then(Json::as_bool).unwrap_or(false),
    })
}

/// Read a world record (`02:02.6`).
fn parse_world(id: &str, data: &Json) -> Result<WorldRecord, &'static str> {
    let generator = Generator::parse(
        data.get("generator")
            .and_then(Json::as_str)
            .unwrap_or("wsp.world.region"),
        data.get("generate")
            .ok_or("a world record needs a generate block")?,
    )?;
    let mut overlays = Vec::new();
    if let Some(Json::Arr(items)) = data.get("overlays") {
        for item in items {
            let Json::Arr(origin) = item.get("origin").unwrap_or(&Json::Null) else {
                return Err("an overlay's origin is an [x, y] pair");
            };
            if origin.len() != 2 {
                return Err("an overlay's origin is an [x, y] pair");
            }
            overlays.push((
                item.get("map")
                    .and_then(Json::as_str)
                    .ok_or("an overlay needs a map")?
                    .to_string(),
                origin[0].as_i64().ok_or("x must be a number")? as i32,
                origin[1].as_i64().ok_or("y must be a number")? as i32,
            ));
        }
    }
    let Json::Arr(start) = data.get("start").ok_or("a world needs a start pair")? else {
        return Err("a world's start is an [x, y] pair");
    };
    if start.len() != 2 {
        return Err("a world's start is an [x, y] pair");
    }
    let mut stations = Vec::new();
    if let Some(Json::Arr(items)) = data.get("stations") {
        for item in items {
            stations.push(parse_station(item)?);
        }
    }
    let start_minute = data
        .get("start_minute")
        .and_then(Json::as_i64)
        .unwrap_or(360)
        .clamp(0, 1439) as u32;
    Ok(WorldRecord {
        id: id.to_string(),
        generator,
        overlays,
        start: (
            start[0].as_i64().ok_or("x must be a number")? as i32,
            start[1].as_i64().ok_or("y must be a number")? as i32,
        ),
        start_minute,
        stations,
        self_dialogue: data
            .get("self_dialogue")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
    })
}

/// Read a list of strings, or an empty list when the field is absent.
fn string_list(value: Option<&Json>) -> Result<Vec<String>, &'static str> {
    match value {
        None => Ok(Vec::new()),
        Some(Json::Arr(items)) => {
            let mut out = Vec::new();
            for item in items {
                out.push(item.as_str().ok_or("entries must be strings")?.to_string());
            }
            Ok(out)
        }
        _ => Err("expected an array of strings"),
    }
}

/// Whether a record type is one the core understands.
fn known_type(type_: &str) -> bool {
    matches!(
        type_,
        "rules"
            | "item"
            | "power"
            | "origin"
            | "vehicle"
            | "skill"
            | "character"
            | "sector_map"
            | "encounter"
            | "faction"
            | "dialogue"
            | "quest"
            | "shop"
            | "world"
            | "outcome_table"
            | "visual"
            | "string_table"
            | "signal"
    ) || STRUCTURAL_TYPES.contains(&type_)
}

/// Whether a new record's id is namespaced under its pack (`03:03.5`).
///
/// The rule exists so two mods cannot collide by accident: a new record must
/// live in the pack's own namespace, and touching a record outside it requires
/// an explicit `override`.
fn namespace_matches(id: &str, pack: &str) -> bool {
    let id_ns = id.split('.').next().unwrap_or("");
    let pack_ns = pack.split('.').next().unwrap_or("");
    !id_ns.is_empty() && id_ns == pack_ns
}

/// Whether `left` is a higher version than `right`, both bare `X.Y.Z`.
fn version_above(left: &str, right: &str) -> bool {
    fn parts(text: &str) -> Option<(u32, u32, u32)> {
        let mut fields = text.split('.');
        let major = fields.next()?.parse().ok()?;
        let minor = fields.next()?.parse().ok()?;
        let patch = fields.next()?.parse().ok()?;
        if fields.next().is_some() {
            return None;
        }
        Some((major, minor, patch))
    }
    match (parts(left), parts(right)) {
        // An unparseable version fails its gate closed: it is treated as
        // unsatisfiable rather than silently ordered (VERSIONING.md).
        (Some(left), Some(right)) => left > right,
        _ => true,
    }
}

/// A problem record (`03:03.9`).
pub fn problem(severity: &str, code: &str, path: &str, args: &[(&str, Json)]) -> Json {
    let mut fields = vec![
        ("severity".to_string(), Json::string(severity)),
        ("code".to_string(), Json::string(code)),
        ("path".to_string(), Json::string(path)),
    ];
    let mut arguments = Json::object();
    for (key, value) in args {
        arguments.set(key, value.clone());
    }
    fields.push(("args".to_string(), arguments));
    Json::Obj(fields)
}

/// A skip note.
fn skip(source: &str, pack: &str, reason: &str) -> Json {
    Json::obj([
        ("source", Json::string(source)),
        ("pack", Json::string(pack)),
        ("reason", Json::string(reason)),
    ])
}

/// Move a pack's problems into the report, disabling a mod that caused them.
fn problem_for_source(
    source: &str,
    pack_problems: &[Json],
    problems: &mut Vec<Json>,
    disabled_sources: &mut Vec<String>,
) {
    problems.extend_from_slice(pack_problems);
    if source != "base" && !disabled_sources.iter().any(|known| known == source) {
        disabled_sources.push(source.to_string());
    }
}

/// Assemble the [`Loaded`] value and the report.
fn finish(
    registry: ContentRegistry,
    problems: Vec<Json>,
    applied: Vec<PackInfo>,
    skipped: Vec<Json>,
    records: BTreeMap<String, RecordEntry>,
    ok: bool,
) -> Loaded {
    let report = Json::obj([
        ("schema", Json::string("kobra.load-report/1")),
        ("ok", Json::Bool(ok)),
        (
            "packs",
            Json::arr(applied.iter().map(|pack| {
                Json::obj([
                    ("source", Json::string(&pack.source)),
                    ("pack", Json::string(&pack.pack)),
                    ("kind", Json::string(&pack.kind)),
                ])
            })),
        ),
        ("skipped", Json::Arr(skipped)),
        ("record_count", Json::Num(records.len() as i64)),
        ("items", Json::Arr(problems)),
        (
            "rules_hash",
            Json::string(format!("{:016x}", registry.rules_hash)),
        ),
        (
            "presentation_hash",
            Json::string(format!("{:016x}", registry.presentation_hash)),
        ),
    ]);
    Loaded {
        registry,
        report,
        ok,
        records,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn document() -> Json {
        Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","priority":0,"pack":"wsp.core","kind":"content","path":"base.pack.json",
               "records":[
                 {"id":"wsp.core.rules","type":"rules","data":{"knockback_tiles":3}},
                 {"id":"wsp.item.bat","type":"item","data":{"hands":"one","melee":"bashing"}},
                 {"id":"wsp.sector.test","type":"sector_map","data":{"width":2,"height":1,"tiles":[{},{}]}}
               ]}
            ]}"#,
        )
    }

    #[test]
    fn merges_a_base_pack_and_hashes_it() {
        let loaded = load(&document());
        assert!(loaded.ok, "report: {}", loaded.report);
        assert_eq!(loaded.registry.items.len(), 1);
        assert_eq!(loaded.registry.maps.len(), 1);
        assert_ne!(loaded.registry.rules_hash, 0);
    }

    #[test]
    fn a_mod_may_override_a_base_record_and_add_one_of_its_own() {
        let mut doc = document();
        let mod_pack = Json::from_text(
            r#"{"source":"noir","priority":10,"pack":"noir.gear","kind":"content","path":"noir.pack.json",
                "records":[
                  {"id":"wsp.item.bat","type":"item","override":true,"data":{"hands":"two","melee":"bashing"}},
                  {"id":"noir.item.blackjack","type":"item","data":{"hands":"one","melee":"bashing"}}
                ]}"#,
        );
        if let Json::Obj(fields) = &mut doc {
            if let Some((_, Json::Arr(packs))) = fields.iter_mut().find(|(name, _)| name == "packs")
            {
                packs.push(mod_pack);
            }
        }
        let loaded = load(&doc);
        assert!(loaded.ok, "report: {}", loaded.report);
        assert_eq!(loaded.registry.items.len(), 2);
        let bat = loaded.registry.item("wsp.item.bat").expect("bat");
        assert_eq!(bat.hands, crate::l1::items::Hands::Two, "the override wins");
        assert!(loaded.registry.item("noir.item.blackjack").is_some());
    }

    #[test]
    fn a_duplicate_without_override_disables_the_mod_and_never_clobbers() {
        let mut doc = document();
        let mod_pack = Json::from_text(
            r#"{"source":"noir","priority":10,"pack":"noir.gear","kind":"content","path":"noir.pack.json",
                "records":[{"id":"wsp.item.bat","type":"item","data":{"hands":"two"}}]}"#,
        );
        if let Json::Obj(fields) = &mut doc {
            if let Some((_, Json::Arr(packs))) = fields.iter_mut().find(|(name, _)| name == "packs")
            {
                packs.push(mod_pack);
            }
        }
        let loaded = load(&doc);
        assert!(!loaded.ok);
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("duplicate_id")));
        // The base record survives untouched.
        assert_eq!(
            loaded.registry.item("wsp.item.bat").expect("bat").hands,
            crate::l1::items::Hands::One
        );
        assert!(loaded
            .report
            .get("skipped")
            .and_then(Json::as_arr)
            .is_some());
    }

    #[test]
    fn a_structural_record_may_not_be_overridden() {
        let mut doc = document();
        let mod_pack = Json::from_text(
            r#"{"source":"noir","priority":10,"pack":"noir.gear","kind":"content","path":"noir.pack.json",
                "records":[{"id":"wsp.core.ladder","type":"ladder","override":true,"data":{}}]}"#,
        );
        if let Json::Obj(fields) = &mut doc {
            if let Some((_, Json::Arr(packs))) = fields.iter_mut().find(|(name, _)| name == "packs")
            {
                packs.push(mod_pack);
            }
        }
        let loaded = load(&doc);
        assert!(!loaded.ok);
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("structural_override")));
    }

    #[test]
    fn a_presentation_pack_may_not_define_a_rules_record() {
        let mut doc = document();
        let mod_pack = Json::from_text(
            r#"{"source":"noir","priority":10,"pack":"noir.skin","kind":"presentation","path":"noir.pack.json",
                "records":[{"id":"noir.item.blackjack","type":"item","data":{"hands":"one"}}]}"#,
        );
        if let Json::Obj(fields) = &mut doc {
            if let Some((_, Json::Arr(packs))) = fields.iter_mut().find(|(name, _)| name == "packs")
            {
                packs.push(mod_pack);
            }
        }
        let loaded = load(&doc);
        assert!(!loaded.ok);
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("kind_violation")));
    }

    #[test]
    fn a_pack_above_the_engine_version_is_skipped_whole() {
        let document = Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.core","kind":"content","path":"b","records":[]},
              {"source":"future","priority":1,"pack":"future.gear","kind":"content","path":"f","engine_min":"9.0.0",
               "records":[{"id":"future.item.x","type":"item","data":{}}]}]}"#,
        );
        let loaded = load(&document);
        assert!(loaded
            .report
            .get("skipped")
            .and_then(Json::as_arr)
            .expect("skipped")
            .iter()
            .any(|skip| skip.get("reason").and_then(Json::as_str) == Some("engine_min")));
        assert!(loaded.registry.item("future.item.x").is_none());
    }

    #[test]
    fn a_new_record_outside_the_pack_namespace_is_refused() {
        let document = Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"noir","priority":1,"pack":"noir.gear","kind":"content","path":"n",
               "records":[{"id":"wsp.item.sneaky","type":"item","data":{}}]}]}"#,
        );
        let loaded = load(&document);
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("bad_namespace")));
    }
}
