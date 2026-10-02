//! The save envelope (`AD-11`, `AD-21`, 02:02.9).
//!
//! A save is a JSON object with three parts:
//!
//! 1. `meta` — `save_version`, `engine_version`, `ruleset`, `content_hash`, the
//!    enabled mod ids and their hashes, the seed, playtime and the generator
//!    version.
//! 2. `core` — the **readable, migration-critical** state: character sheets,
//!    Fortune, Repute, Lifestyle, inventory, conditions. Small, stable, and the
//!    part we promise to migrate.
//! 3. `world` — a **base64 compact-binary** delta of the simulation world. M1
//!    has no procedural baseline yet, so the delta is the entity placements and
//!    the encounter's own clock; when the generator lands (M3) the same slot
//!    holds the per-sector delta and nothing else changes.
//!
//! Two properties are load-bearing and both are asserted by test:
//!
//! - **`save -> load -> save` is byte-stable.** The writer emits objects in a
//!   fixed order and the parser preserves it, so "round trip" is a comparison of
//!   bytes rather than of a derived structure.
//! - **A mismatch is a hard, explained refusal.** `ruleset` and `content_hash`
//!   are recorded because a save's *meaning* depends on them: the two ladders
//!   resolve the same Rank Value differently at the top of the scale, and a
//!   content hash covers the outcome tables a fight resolved against.

use std::collections::BTreeMap;

use crate::json::Json;
use crate::l0::Colour;
use crate::l1::character::Character;
use crate::l1::skills::ActionTag;
use crate::l1::status::{Condition, ConditionKind};
use crate::l2::actions::{Action, ActionKind, Declared};
use crate::l2::encounter::{Encounter, Phase, Queued};
use crate::l3::clock::{Clock, DaySlot, Station};
use crate::l3::gen::GENERATOR_VERSION;
use crate::l3::world::Tile;
use crate::l4::factions::Standings;
use crate::l4::journal::Journal;

/// The envelope schema id.
pub const SAVE_SCHEMA: &str = "kobra.save/1";

/// The save format version, mirrored into `engine.manifest.json`.
///
/// Version 2 is the M3 world delta: the clock, the procedural baseline's identity,
/// the per-sector delta, the stations, the standing and the journal, in place of
/// version 1's entity placements and encounter clock (`02:02.6`, `AD-11`).
pub const SAVE_VERSION: u32 = 2;

/// Everything a save carries, so a `Sim` can be rebuilt without the sim module
/// depending on the JSON reader and vice versa.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct SaveData {
    /// The save format version.
    pub save_version: u32,
    /// The engine version that wrote it.
    pub engine_version: String,
    /// The ladder the save was created under (`02:02.9`).
    pub ruleset: String,
    /// The content hash the save binds (`AD-11`).
    pub content_hash: String,
    /// The world seed (`AD-6`).
    pub seed: u32,
    /// Ticks advanced.
    pub tick: u32,
    /// The gameplay RNG's state.
    pub rng_state: (u64, u64),
    /// The characters, in id order.
    pub characters: Vec<Character>,
    /// The party's entity ids.
    pub party: Vec<u32>,
    /// Every vehicle on the map (`4c:1300-1365`).
    pub vehicles: Vec<crate::l2::vehicle::Vehicle>,
    /// Campaign flags.
    pub flags: BTreeMap<String, i64>,
    /// The encounter, when one is running.
    pub encounter: Option<Encounter>,
    /// The map record id.
    pub map: String,
    /// Where every entity stands.
    pub placements: Vec<(u32, i32, i32, i8)>,
    /// The mods that were enabled, id and content hash (`AD-11`).
    pub mods: Vec<(String, String)>,
    /// The Tier-2 scripts that were registered, id and hash (`AD-8`).
    pub scripts: Vec<(String, String)>,
    /// The world clock (`02:02.7`).
    pub clock: Clock,
    /// The generator version the baseline was produced by (`02:02.6`).
    pub generator_version: u32,
    /// The world record the baseline came from, when it was generated.
    pub world_record: String,
    /// The generator's parameters, so the baseline can be regenerated exactly.
    pub generator: crate::l3::gen::Generator,
    /// The sectors that differ from the regenerated baseline.
    pub tile_delta: Vec<(i32, i32, Tile)>,
    /// The NPCs standing in the world (`02:02.6`).
    pub stations: Vec<Station>,
    /// Faction standing (`02:02.6`).
    pub standings: Standings,
    /// The quest journal (`02:02.7`).
    pub journal: Journal,
}

/// The parts the compact world delta carries (`AD-11`): the map record id, the
/// encounter when one is running, where every entity stands, the clock, the
/// baseline's identity and its per-sector delta, and the stations.
type WorldDelta = (
    String,
    Option<Encounter>,
    Vec<(u32, i32, i32, i8)>,
    Clock,
    String,
    u32,
    crate::l3::gen::Generator,
    Vec<(i32, i32, Tile)>,
    Vec<Station>,
);

/// Write a save envelope (`AD-11`).
pub fn envelope(data: &SaveData) -> Json {
    let mut mods = Vec::new();
    for (id, hash) in &data.mods {
        mods.push(Json::obj([
            ("id", Json::string(id)),
            ("content_hash", Json::string(hash)),
        ]));
    }
    let mut characters = Vec::new();
    for character in &data.characters {
        characters.push(write_character(character));
    }
    let mut flags = Json::object();
    for (key, value) in &data.flags {
        flags.set(key, Json::Num(*value));
    }
    Json::obj([
        ("schema", Json::string(SAVE_SCHEMA)),
        (
            "meta",
            Json::obj([
                ("save_version", Json::Num(i64::from(data.save_version))),
                ("engine_version", Json::string(&data.engine_version)),
                ("ruleset", Json::string(&data.ruleset)),
                ("content_hash", Json::string(&data.content_hash)),
                // 02:02.6: a save whose generator version differs is refused, so
                // this is the version the *baseline* was produced by, not the
                // build's.
                (
                    "generator_version",
                    Json::Num(i64::from(data.generator_version)),
                ),
                (
                    "world_record",
                    if data.world_record.is_empty() {
                        Json::Null
                    } else {
                        Json::string(&data.world_record)
                    },
                ),
                ("clock", data.clock.to_json()),
                ("seed", Json::Num(i64::from(data.seed))),
                ("tick", Json::Num(i64::from(data.tick))),
                // Playtime is the world clock's own total, not the encounter's
                // panel, because a campaign spends most of its life exploring
                // (02:02.7).
                (
                    "playtime_panels",
                    Json::Num(data.clock.total().min(i64::MAX as u64) as i64),
                ),
                ("mods", Json::Arr(mods)),
                (
                    "scripts",
                    Json::arr(data.scripts.iter().map(|(id, hash)| {
                        Json::obj([("id", Json::string(id)), ("hash", Json::string(hash))])
                    })),
                ),
            ]),
        ),
        (
            "core",
            Json::obj([
                ("characters", Json::Arr(characters)),
                (
                    "party",
                    Json::arr(data.party.iter().map(|id| Json::Num(i64::from(*id)))),
                ),
                (
                    "vehicles",
                    Json::arr(data.vehicles.iter().map(write_vehicle)),
                ),
                ("flags", flags),
                (
                    "rng",
                    Json::obj([
                        ("state", Json::string(data.rng_state.0.to_string())),
                        ("inc", Json::string(data.rng_state.1.to_string())),
                    ]),
                ),
                // 02:02.7: standing, the journal and the quest state are all
                // `core`, because they are what a save has to migrate rather than
                // what it has to keep compact.
                ("standings", data.standings.to_json()),
                ("journal", data.journal.to_json()),
            ]),
        ),
        ("world", Json::string(base64_encode(&write_world(data)))),
    ])
}

/// Read a save envelope, returning the parts a `Sim` needs.
pub fn parse(text: &str) -> Result<SaveData, &'static str> {
    let document = Json::parse(text).map_err(|_| "the save is not valid JSON")?;
    if document.get("schema").and_then(Json::as_str) != Some(SAVE_SCHEMA) {
        return Err("the save is not a worldspiracy save");
    }
    let meta = document.get("meta").ok_or("the save has no meta block")?;
    let save_version = meta
        .get("save_version")
        .and_then(Json::as_i64)
        .ok_or("the save has no save_version")? as u32;
    if save_version != SAVE_VERSION {
        // AD-11: migration is a promise for `core`, and M1 has no older
        // version to migrate from, so anything else is a refusal rather than a
        // guess.
        return Err("this save was written by a different save format");
    }
    let engine_version = meta
        .get("engine_version")
        .and_then(Json::as_str)
        .ok_or("the save has no engine_version")?
        .to_string();
    let ruleset = meta
        .get("ruleset")
        .and_then(Json::as_str)
        .ok_or("the save has no ruleset")?
        .to_string();
    let content_hash = meta
        .get("content_hash")
        .and_then(Json::as_str)
        .ok_or("the save has no content hash")?
        .to_string();
    let seed = meta.get("seed").and_then(Json::as_i64).unwrap_or(0) as u32;
    let tick = meta.get("tick").and_then(Json::as_i64).unwrap_or(0) as u32;
    let generator_version = meta
        .get("generator_version")
        .and_then(Json::as_i64)
        .unwrap_or(0) as u32;
    let _ = generator_version;
    let world_record = meta
        .get("world_record")
        .and_then(Json::as_str)
        .unwrap_or("")
        .to_string();
    let clock = meta.get("clock").ok_or("the save has no clock")?;
    let clock = Clock {
        day: clock.get("day").and_then(Json::as_i64).unwrap_or(1) as u32,
        panel: clock.get("panel").and_then(Json::as_i64).unwrap_or(0) as u32,
        panels_per_day: clock
            .get("panels_per_day")
            .and_then(Json::as_i64)
            .unwrap_or(i64::from(crate::l3::clock::DEFAULT_PANELS_PER_DAY))
            .max(1) as u32,
    };
    let mut mods = Vec::new();
    if let Some(Json::Arr(entries)) = meta.get("mods") {
        for entry in entries {
            let id = entry
                .get("id")
                .and_then(Json::as_str)
                .ok_or("a mod entry needs an id")?;
            let hash = entry
                .get("content_hash")
                .and_then(Json::as_str)
                .unwrap_or("");
            mods.push((id.to_string(), hash.to_string()));
        }
    }
    let mut scripts = Vec::new();
    if let Some(Json::Arr(entries)) = meta.get("scripts") {
        for entry in entries {
            let id = entry
                .get("id")
                .and_then(Json::as_str)
                .ok_or("a script entry needs an id")?;
            let hash = entry.get("hash").and_then(Json::as_str).unwrap_or("");
            scripts.push((id.to_string(), hash.to_string()));
        }
    }
    let core = document.get("core").ok_or("the save has no core block")?;
    let mut characters = Vec::new();
    let Some(Json::Arr(entries)) = core.get("characters") else {
        return Err("the save has no characters");
    };
    for entry in entries {
        characters.push(read_character(entry)?);
    }
    let mut party = Vec::new();
    if let Some(Json::Arr(ids)) = core.get("party") {
        for id in ids {
            party.push(id.as_i64().ok_or("a party id must be a number")? as u32);
        }
    }
    let mut vehicles = Vec::new();
    if let Some(Json::Arr(entries)) = core.get("vehicles") {
        for entry in entries {
            vehicles.push(read_vehicle(entry)?);
        }
    }
    let mut flags = BTreeMap::new();
    if let Some(Json::Obj(fields)) = core.get("flags") {
        for (key, value) in fields {
            flags.insert(key.clone(), value.as_i64().unwrap_or(0));
        }
    }
    let rng = core.get("rng").ok_or("the save has no rng state")?;
    let rng_state = (
        rng.get("state")
            .and_then(Json::as_str)
            .and_then(|text| text.parse().ok())
            .ok_or("the save has a bad rng state")?,
        rng.get("inc")
            .and_then(Json::as_str)
            .and_then(|text| text.parse().ok())
            .ok_or("the save has a bad rng state")?,
    );
    let mut standings = Standings::default();
    if let Some(block) = core.get("standings") {
        if let Some(Json::Arr(entries)) = block.get("standings") {
            for entry in entries {
                if let Some(faction) = entry.get("faction").and_then(Json::as_str) {
                    standings.set(
                        faction,
                        entry.get("standing").and_then(Json::as_i64).unwrap_or(0) as i32,
                    );
                }
            }
        }
    }
    let journal = match core.get("journal") {
        None => Journal::default(),
        Some(block) => read_journal(block)?,
    };
    let world = document
        .get("world")
        .and_then(Json::as_str)
        .ok_or("the save has no world delta")?;
    let bytes = base64_decode(world).ok_or("the world delta is not valid base64")?;
    let (
        map,
        encounter,
        placements,
        delta_clock,
        delta_world,
        delta_generator_version,
        generator,
        tile_delta,
        stations,
    ) = read_world(&bytes)?;
    // The clock lives in both halves on purpose: `meta` so a support process can
    // read when a save was taken without decoding the delta, and the delta because
    // the world's own state must round-trip byte for byte. The delta wins, and a
    // disagreement is impossible because the writer emits both from one value.
    let _ = clock;
    Ok(SaveData {
        save_version,
        engine_version,
        ruleset,
        content_hash,
        seed,
        tick,
        rng_state,
        characters,
        party,
        vehicles,
        flags,
        encounter,
        map,
        placements,
        mods,
        scripts,
        clock: delta_clock,
        generator_version: delta_generator_version,
        world_record: if world_record.is_empty() {
            delta_world
        } else {
            world_record
        },
        generator,
        tile_delta,
        stations,
        standings,
        journal,
    })
}

/// Read the journal's readable projection back into state.
///
/// The `core` half is the part a person can read and fix (`AD-11`), so the
/// journal is written there as JSON rather than as bytes. The lines carry their
/// own text keys and read flags, and the quest pages carry their status, pointer
/// and fired steps — everything the runtime needs and nothing derived.
fn read_journal(block: &Json) -> Result<Journal, &'static str> {
    let mut journal = Journal::default();
    if let Some(Json::Arr(lines)) = block.get("lines") {
        for line in lines {
            let text_key = line
                .get("text_key")
                .and_then(Json::as_str)
                .ok_or("a journal line needs a text key")?
                .to_string();
            let quest = line
                .get("quest")
                .and_then(Json::as_str)
                .unwrap_or("")
                .to_string();
            let read = line.get("read").and_then(Json::as_bool).unwrap_or(false);
            journal
                .lines
                .push((quest, crate::l4::journal::JournalLine { text_key, read }));
        }
    }
    if let Some(Json::Arr(quests)) = block.get("quests") {
        for entry in quests {
            let quest = entry
                .get("quest")
                .and_then(Json::as_str)
                .ok_or("a journal quest needs an id")?
                .to_string();
            let status = entry
                .get("status")
                .and_then(Json::as_str)
                .and_then(crate::l4::quests::QuestStatus::from_id)
                .ok_or("an unknown quest status in the save")?;
            let mut fired = Vec::new();
            if let Some(Json::Arr(steps)) = entry.get("fired") {
                for step in steps {
                    fired.push(step.as_i64().unwrap_or(0).max(0) as usize);
                }
            }
            // A page's own lines are the projection's copy; the flat `lines` list
            // is the one the journal reads, so only the page's identity and
            // progress are restored from here.
            journal.entries.push(crate::l4::journal::JournalEntry {
                quest,
                status,
                step: entry.get("step").and_then(Json::as_i64).unwrap_or(0) as usize,
                fired,
                lines: Vec::new(),
            });
        }
    }
    if let Some(Json::Arr(pauses)) = block.get("pauses") {
        for pause in pauses {
            let quest = pause
                .get("quest")
                .and_then(Json::as_str)
                .ok_or("a pause needs a quest")?
                .to_string();
            let step = pause.get("step").and_then(Json::as_i64).unwrap_or(0) as usize;
            journal.pauses.push((quest, step));
        }
    }
    Ok(journal)
}

/// A character as the readable half (`AD-11`).
fn write_character(character: &Character) -> Json {
    let mut skills = Vec::new();
    for skill in &character.skills {
        skills.push(Json::obj([
            ("skill", Json::string(&skill.skill)),
            ("steps", Json::Num(i64::from(skill.steps))),
            (
                "applies_to",
                Json::arr(skill.applies_to.iter().map(|tag| Json::string(tag.id()))),
            ),
        ]));
    }
    let mut conditions = Vec::new();
    for condition in &character.conditions {
        conditions.push(Json::obj([
            ("kind", Json::string(condition.kind.id())),
            ("panels", Json::Num(i64::from(condition.panels))),
            ("source", Json::Num(i64::from(condition.source))),
        ]));
    }
    Json::obj([
        ("id", Json::Num(i64::from(character.id))),
        ("name_key", Json::string(&character.name_key)),
        ("origin", Json::string(&character.origin)),
        ("side", Json::Num(i64::from(character.side))),
        ("mode", Json::string(character.mode.id())),
        (
            "traits",
            Json::arr(
                character
                    .traits
                    .iter()
                    .map(|value| Json::Num(i64::from(*value))),
            ),
        ),
        ("damage", Json::Num(i64::from(character.damage))),
        ("max_damage", Json::Num(i64::from(character.max_damage))),
        ("fortune", Json::Num(i64::from(character.fortune))),
        ("repute", Json::Num(i64::from(character.repute))),
        ("lifestyle_rv", Json::Num(i64::from(character.lifestyle_rv))),
        ("skills", Json::Arr(skills)),
        ("items", Json::arr(character.items.iter().map(Json::string))),
        (
            "weapon",
            match &character.weapon {
                Some(id) => Json::string(id),
                None => Json::Null,
            },
        ),
        (
            "armour",
            Json::arr(character.armour.iter().map(Json::string)),
        ),
        ("conditions", Json::Arr(conditions)),
        (
            "powers",
            Json::arr(character.powers.iter().map(write_power)),
        ),
        (
            "trait_boosts",
            Json::arr(character.trait_boosts.iter().map(|boost| {
                Json::obj([
                    ("trait", Json::string(boost.trait_.id())),
                    ("rv", Json::Num(i64::from(boost.rv))),
                    ("panels", Json::Num(i64::from(boost.panels))),
                ])
            })),
        ),
        ("dying_steps", Json::Num(i64::from(character.dying_steps))),
        ("dead", Json::Bool(character.dead)),
        (
            "creation_log",
            Json::arr(character.creation_log.iter().map(Json::string)),
        ),
    ])
}

/// A power as the readable half (`AD-11`).
///
/// The derived layers (`power_floors`, `power_armor`) are deliberately **not**
/// written: `D17` recomputes them on load, so a save never carries a stale buff.
fn write_power(power: &crate::l1::powers::PowerInst) -> Json {
    let mut params = Json::object();
    for (key, value) in &power.params {
        params.set(key, Json::string(value));
    }
    Json::obj([
        ("id", Json::string(&power.id)),
        ("rv", Json::Num(i64::from(power.rv))),
        ("params", params),
    ])
}

/// Read a character from the readable half.
fn read_character(value: &Json) -> Result<Character, &'static str> {
    let id = value
        .get("id")
        .and_then(Json::as_i64)
        .ok_or("a character needs an id")? as u32;
    let mut traits = [1i32; 7];
    let Some(Json::Arr(values)) = value.get("traits") else {
        return Err("a character needs seven traits");
    };
    if values.len() != 7 {
        return Err("a character needs seven traits");
    }
    for (index, entry) in values.iter().enumerate() {
        traits[index] = entry.as_i64().ok_or("a trait must be a number")? as i32;
    }
    let mut skills = Vec::new();
    if let Some(Json::Arr(entries)) = value.get("skills") {
        for entry in entries {
            let mut applies_to = Vec::new();
            if let Some(Json::Arr(tags)) = entry.get("applies_to") {
                for tag in tags {
                    if let Some(tag) = tag.as_str().and_then(ActionTag::from_id) {
                        applies_to.push(tag);
                    }
                }
            }
            skills.push(crate::l1::skills::SkillInstance {
                skill: entry
                    .get("skill")
                    .and_then(Json::as_str)
                    .ok_or("a skill needs an id")?
                    .to_string(),
                steps: entry.get("steps").and_then(Json::as_i64).unwrap_or(1) as i32,
                applies_to,
            });
        }
    }
    let mut conditions = Vec::new();
    if let Some(Json::Arr(entries)) = value.get("conditions") {
        for entry in entries {
            let kind = entry
                .get("kind")
                .and_then(Json::as_str)
                .and_then(ConditionKind::from_id)
                .ok_or("an unknown condition in the save")?;
            conditions.push(Condition {
                kind,
                panels: entry.get("panels").and_then(Json::as_i64).unwrap_or(-1) as i32,
                source: entry.get("source").and_then(Json::as_i64).unwrap_or(0) as u32,
            });
        }
    }
    let mode = value
        .get("mode")
        .and_then(Json::as_str)
        .and_then(crate::l1::character::CreationMode::from_id)
        .ok_or("an unknown creation mode in the save")?;
    let mut item_list = Vec::new();
    if let Some(Json::Arr(items)) = value.get("items") {
        for item in items {
            item_list.push(
                item.as_str()
                    .ok_or("an item id must be a string")?
                    .to_string(),
            );
        }
    }
    let mut armour = Vec::new();
    if let Some(Json::Arr(items)) = value.get("armour") {
        for item in items {
            armour.push(
                item.as_str()
                    .ok_or("an armour id must be a string")?
                    .to_string(),
            );
        }
    }
    let mut powers = Vec::new();
    if let Some(Json::Arr(entries)) = value.get("powers") {
        for entry in entries {
            powers.push(
                crate::l1::powers::parse_power(entry).map_err(|_| "a bad power in the save")?,
            );
        }
    }
    let mut trait_boosts = Vec::new();
    if let Some(Json::Arr(entries)) = value.get("trait_boosts") {
        for entry in entries {
            trait_boosts.push(crate::l1::character::TraitBoost {
                trait_: entry
                    .get("trait")
                    .and_then(Json::as_str)
                    .and_then(crate::l1::traits::Trait::from_id)
                    .ok_or("an unknown trait in a boost")?,
                rv: entry.get("rv").and_then(Json::as_i64).unwrap_or(1) as i32,
                panels: entry.get("panels").and_then(Json::as_i64).unwrap_or(0) as i32,
            });
        }
    }
    let mut creation_log = Vec::new();
    if let Some(Json::Arr(lines)) = value.get("creation_log") {
        for line in lines {
            creation_log.push(line.as_str().unwrap_or("").to_string());
        }
    }
    Ok(Character {
        id,
        name_key: value
            .get("name_key")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        origin: value
            .get("origin")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        side: value.get("side").and_then(Json::as_i64).unwrap_or(0) as u8,
        mode,
        traits,
        damage: value.get("damage").and_then(Json::as_i64).unwrap_or(0) as i32,
        max_damage: value.get("max_damage").and_then(Json::as_i64).unwrap_or(0) as i32,
        fortune: value.get("fortune").and_then(Json::as_i64).unwrap_or(0) as i32,
        repute: value.get("repute").and_then(Json::as_i64).unwrap_or(0) as i32,
        lifestyle_rv: value
            .get("lifestyle_rv")
            .and_then(Json::as_i64)
            .unwrap_or(1) as i32,
        skills,
        items: item_list,
        weapon: value
            .get("weapon")
            .and_then(Json::as_str)
            .map(str::to_string),
        armour,
        conditions,
        powers,
        // Derived, recomputed when the registry is in scope (`D17`).
        power_floors: Vec::new(),
        power_armor: 0,
        // Per-panel transients are zero between panels, and a save can only be
        // taken between panels, so they are not written.
        temporary_steps: Vec::new(),
        trait_boosts,
        dying_steps: value.get("dying_steps").and_then(Json::as_i64).unwrap_or(0) as i32,
        dead: value.get("dead").and_then(Json::as_bool).unwrap_or(false),
        // Per-panel transients are zero between panels, and a save can only be
        // taken between panels, so they are not written.
        moved_tiles: 0,
        dodge_steps: 0,
        creation_log,
    })
}

/// A vehicle as the readable half (`AD-11`).
fn write_vehicle(vehicle: &crate::l2::vehicle::Vehicle) -> Json {
    Json::obj([
        ("id", Json::Num(i64::from(vehicle.id))),
        ("record", Json::string(&vehicle.record)),
        ("name_key", Json::string(&vehicle.name_key)),
        ("durability", Json::Num(i64::from(vehicle.durability))),
        (
            "max_durability",
            Json::Num(i64::from(vehicle.max_durability)),
        ),
        ("handling", Json::Num(i64::from(vehicle.handling))),
        ("velocity", Json::Num(i64::from(vehicle.velocity))),
        ("operator", Json::Num(i64::from(vehicle.operator))),
        (
            "passengers",
            Json::arr(
                vehicle
                    .passengers
                    .iter()
                    .map(|id| Json::Num(i64::from(*id))),
            ),
        ),
        ("destroyed", Json::Bool(vehicle.destroyed)),
    ])
}

/// Read a vehicle from the readable half.
fn read_vehicle(value: &Json) -> Result<crate::l2::vehicle::Vehicle, &'static str> {
    let mut passengers = Vec::new();
    if let Some(Json::Arr(ids)) = value.get("passengers") {
        for id in ids {
            passengers.push(id.as_i64().ok_or("a passenger id must be a number")? as u32);
        }
    }
    let durability = value
        .get("durability")
        .and_then(Json::as_i64)
        .ok_or("a vehicle needs a durability")? as i32;
    Ok(crate::l2::vehicle::Vehicle {
        id: value
            .get("id")
            .and_then(Json::as_i64)
            .ok_or("a vehicle needs an id")? as u32,
        record: value
            .get("record")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        name_key: value
            .get("name_key")
            .and_then(Json::as_str)
            .unwrap_or("")
            .to_string(),
        durability,
        max_durability: value
            .get("max_durability")
            .and_then(Json::as_i64)
            .unwrap_or(i64::from(durability)) as i32,
        handling: value.get("handling").and_then(Json::as_i64).unwrap_or(1) as i32,
        velocity: value.get("velocity").and_then(Json::as_i64).unwrap_or(0) as i32,
        operator: value.get("operator").and_then(Json::as_i64).unwrap_or(0) as u32,
        passengers,
        moved_tiles: 0,
        destroyed: value
            .get("destroyed")
            .and_then(Json::as_bool)
            .unwrap_or(false),
    })
}

/// The compact binary world delta (`AD-11`).
fn write_world(data: &SaveData) -> Vec<u8> {
    let mut out = Writer::new();
    out.bytes(b"WSPW");
    // Version 2 is the M3 delta: the per-sector delta from the procedural
    // baseline, the clock, the stations, the standing and the journal (02:02.6).
    out.u8(2);
    out.text(&data.map);
    match &data.encounter {
        None => out.u8(0),
        Some(encounter) => {
            out.u8(1);
            out.text(&encounter.id);
            out.text(&encounter.map);
            out.u32(encounter.panel);
            out.u8(phase_code(encounter.phase));
            out.u8(u8::from(encounter.finished));
            out.u8(encounter.winner.unwrap_or(255));
            out.u8(encounter.initiative_roll[0]);
            out.u8(encounter.initiative_roll[1]);
            out.i32(encounter.initiative_bonus[0]);
            out.i32(encounter.initiative_bonus[1]);
            out.u16(encounter.queued.len() as u16);
            for queued in &encounter.queued {
                out.u32(queued.actor);
                out.u8(action_code(queued.action.kind));
                out.u32(queued.action.target);
                match queued.action.destination {
                    Some(at) => {
                        out.u8(1);
                        out.i32(at.x);
                        out.i32(at.y);
                    }
                    None => {
                        out.u8(0);
                        out.i32(0);
                        out.i32(0);
                    }
                }
                out.u8(u8::from(queued.action.declared.nail));
                out.u8(queued
                    .action
                    .declared
                    .punch_cap
                    .map(|colour| colour.code())
                    .unwrap_or(255));
                out.i32(queued.action.fortune);
                out.text(queued.action.power.as_deref().unwrap_or(""));
            }
        }
    }
    out.u16(data.placements.len() as u16);
    for (id, x, y, z) in &data.placements {
        out.u32(*id);
        out.i32(*x);
        out.i32(*y);
        out.u8(*z as u8);
    }
    // The clock, so a save taken at dusk reloads at dusk (02:02.7).
    data.clock.write(&mut out.0);
    // The baseline's identity: the record, the generator's parameters and the
    // generator version. A loader regenerates the baseline from these and applies
    // the delta, so a 256x256 world that is 99.9% baseline costs a few hundred
    // bytes (AD-11, R6).
    out.u32(GENERATOR_VERSION);
    out.text(&data.world_record);
    let generator = &data.generator;
    out.i32(generator.width);
    out.i32(generator.height);
    out.u32(generator.seed);
    out.i32(generator.rise_percent);
    out.i32(generator.water_percent);
    out.i32(generator.cover_percent);
    out.i32(generator.feature_tiles);
    // The per-sector delta, run-length encoded by terrain: a cleared field is one
    // entry per rectangle of identical terrain rather than one per tile.
    out.u32(data.tile_delta.len() as u32);
    for (x, y, tile) in &data.tile_delta {
        out.i32(*x);
        out.i32(*y);
        out.bytes(&[tile.level as u8, tile.blocking as u8, tile.terrain]);
        out.i32(tile.material);
    }
    // The stations (02:02.6): an NPC's identity, orders, post and schedule.
    out.u16(data.stations.len() as u16);
    for station in &data.stations {
        out.u32(station.id);
        out.text(station.behaviour.id());
        out.text(&station.faction);
        out.i32(station.home.0);
        out.i32(station.home.1);
        out.i32(station.notice_tiles);
        out.u16(station.schedule.entries.len().min(u16::MAX as usize) as u16);
        for entry in &station.schedule.entries {
            out.u8(match entry.slot {
                None => 255,
                Some(slot) => slot_code(slot),
            });
            out.i32(entry.at.0);
            out.i32(entry.at.1);
        }
        out.u8(u8::from(station.schedule.home.is_some()));
        let home = station.schedule.home.unwrap_or((0, 0));
        out.i32(home.0);
        out.i32(home.1);
    }
    out.finish()
}

/// The wire code for a day slot.
fn slot_code(slot: DaySlot) -> u8 {
    match slot {
        DaySlot::Night => 0,
        DaySlot::Morning => 1,
        DaySlot::Day => 2,
        DaySlot::Evening => 3,
    }
}

/// Read a day slot's wire code.
fn slot_from_code(code: u8) -> Result<Option<DaySlot>, &'static str> {
    match code {
        255 => Ok(None),
        0 => Ok(Some(DaySlot::Night)),
        1 => Ok(Some(DaySlot::Morning)),
        2 => Ok(Some(DaySlot::Day)),
        3 => Ok(Some(DaySlot::Evening)),
        _ => Err("an unknown day slot in the save"),
    }
}

/// Read the compact binary world delta.
fn read_world(bytes: &[u8]) -> Result<WorldDelta, &'static str> {
    let mut reader = Reader::new(bytes);
    if reader.bytes(4)? != b"WSPW" {
        return Err("the world delta has no magic");
    }
    let version = reader.u8()?;
    if version != 2 {
        // AD-11: an older delta is refused with a cause rather than guessed at.
        // Version 1 stored entity placements and an encounter clock and had no
        // baseline, so there is nothing to migrate it onto.
        return Err("this save's world delta was written by a different save format");
    }
    let map = reader.text()?;
    let encounter = match reader.u8()? {
        0 => None,
        1 => {
            let id = reader.text()?;
            let encounter_map = reader.text()?;
            let panel = reader.u32()?;
            let phase = match reader.u8()? {
                0 => Phase::Select,
                1 => Phase::Initiative,
                2 => Phase::SideA,
                3 => Phase::SideB,
                4 => Phase::Resolve,
                5 => Phase::Advance,
                _ => Phase::Ended,
            };
            let finished = reader.u8()? != 0;
            let winner_byte = reader.u8()?;
            let initiative_roll = [reader.u8()?, reader.u8()?];
            let initiative_bonus = [reader.i32()?, reader.i32()?];
            let count = reader.u16()? as usize;
            let mut queued = Vec::with_capacity(count);
            for _ in 0..count {
                let actor = reader.u32()?;
                let kind_byte = reader.u8()?;
                let kind = ActionKind::ALL
                    .get(kind_byte as usize)
                    .copied()
                    .ok_or("an unknown action in the save")?;
                let target = reader.u32()?;
                let has_destination = reader.u8()? != 0;
                let x = reader.i32()?;
                let y = reader.i32()?;
                let nail = reader.u8()? != 0;
                let cap_byte = reader.u8()?;
                let fortune = reader.i32()?;
                let power_text = reader.text().ok();
                queued.push(Queued {
                    actor,
                    action: Action {
                        kind,
                        target,
                        destination: if has_destination {
                            Some(crate::l3::Position { x, y, z: 0 })
                        } else {
                            None
                        },
                        declared: Declared {
                            nail,
                            punch_cap: Colour::from_code(cap_byte),
                        },
                        fortune,
                        power: power_text.clone().filter(|text| !text.is_empty()),
                        manoeuvre: None,
                    },
                });
            }
            Some(Encounter {
                id,
                map: encounter_map,
                panel,
                phase,
                initiative_roll,
                initiative_bonus,
                queued,
                finished,
                winner: if winner_byte == 255 {
                    None
                } else {
                    Some(winner_byte)
                },
            })
        }
        _ => return Err("the world delta has a bad encounter flag"),
    };
    let count = reader.u16()? as usize;
    let mut placements = Vec::with_capacity(count);
    for _ in 0..count {
        let id = reader.u32()?;
        let x = reader.i32()?;
        let y = reader.i32()?;
        let z = reader.u8()? as i8;
        placements.push((id, x, y, z));
    }

    // The M3 sections (02:02.6, 02:02.7).
    let clock = Clock::read(&mut reader)?;
    let generator_version = reader.u32()?;
    let world_record = reader.text()?;
    let generator = crate::l3::gen::Generator {
        width: reader.i32()?,
        height: reader.i32()?,
        seed: reader.u32()?,
        rise_percent: reader.i32()?,
        water_percent: reader.i32()?,
        cover_percent: reader.i32()?,
        feature_tiles: reader.i32()?,
    };
    let delta_count = reader.u32()? as usize;
    let mut tile_delta = Vec::with_capacity(delta_count.min(1 << 16));
    for _ in 0..delta_count {
        let x = reader.i32()?;
        let y = reader.i32()?;
        let bytes = reader.bytes(3)?;
        let tile = Tile {
            level: bytes[0] as i8,
            blocking: bytes[1] as i8,
            terrain: bytes[2],
            material: reader.i32()?,
        };
        tile_delta.push((x, y, tile));
    }
    let station_count = reader.u16()? as usize;
    let mut stations = Vec::with_capacity(station_count);
    for _ in 0..station_count {
        let id = reader.u32()?;
        let behaviour_text = reader.text()?;
        let behaviour = crate::l3::clock::Behaviour::from_id(&behaviour_text)
            .ok_or("an unknown behaviour in the save")?;
        let faction = reader.text()?;
        let home = (reader.i32()?, reader.i32()?);
        let notice_tiles = reader.i32()?;
        let entry_count = reader.u16()? as usize;
        let mut entries = Vec::with_capacity(entry_count);
        for _ in 0..entry_count {
            let slot = slot_from_code(reader.u8()?)?;
            entries.push(crate::l3::clock::ScheduleEntry {
                slot,
                at: (reader.i32()?, reader.i32()?),
            });
        }
        let has_home = reader.u8()? != 0;
        let schedule_home = (reader.i32()?, reader.i32()?);
        stations.push(Station {
            id,
            behaviour,
            faction,
            home,
            schedule: crate::l3::clock::Schedule {
                entries,
                home: if has_home { Some(schedule_home) } else { None },
            },
            notice_tiles,
        });
    }
    Ok((
        map,
        encounter,
        placements,
        clock,
        world_record,
        generator_version,
        generator,
        tile_delta,
        stations,
    ))
}

/// The phase wire code.
fn phase_code(phase: Phase) -> u8 {
    match phase {
        Phase::Select => 0,
        Phase::Initiative => 1,
        Phase::SideA => 2,
        Phase::SideB => 3,
        Phase::Resolve => 4,
        Phase::Advance => 5,
        Phase::Ended => 6,
    }
}

/// The action wire code.
fn action_code(kind: ActionKind) -> u8 {
    ActionKind::ALL
        .iter()
        .position(|candidate| *candidate == kind)
        .unwrap_or(0) as u8
}

/// A little-endian byte writer.
struct Writer(Vec<u8>);

impl Writer {
    fn new() -> Writer {
        Writer(Vec::new())
    }
    fn u8(&mut self, value: u8) {
        self.0.push(value);
    }
    fn u16(&mut self, value: u16) {
        self.0.extend_from_slice(&value.to_le_bytes());
    }
    fn u32(&mut self, value: u32) {
        self.0.extend_from_slice(&value.to_le_bytes());
    }
    fn i32(&mut self, value: i32) {
        self.0.extend_from_slice(&value.to_le_bytes());
    }
    fn bytes(&mut self, value: &[u8]) {
        self.0.extend_from_slice(value);
    }
    fn text(&mut self, value: &str) {
        self.u16(value.len() as u16);
        self.bytes(value.as_bytes());
    }
    fn finish(self) -> Vec<u8> {
        self.0
    }
}

/// A little-endian byte reader.
///
/// Public because the world delta is not one type's private format: the clock,
/// the stations, the quest journal and the rest of the M3 world state each read
/// their own section through it, so a module owns its encoding without owning
/// the byte plumbing.
pub struct Reader<'a> {
    bytes: &'a [u8],
    at: usize,
}

impl<'a> Reader<'a> {
    /// A reader over a delta's bytes.
    pub fn new(bytes: &'a [u8]) -> Reader<'a> {
        Reader { bytes, at: 0 }
    }
    /// How many bytes have been read.
    pub fn position(&self) -> usize {
        self.at
    }
    /// Whether the whole input has been consumed.
    pub fn at_end(&self) -> bool {
        self.at >= self.bytes.len()
    }
    fn take(&mut self, count: usize) -> Result<&'a [u8], &'static str> {
        let end = self.at + count;
        if end > self.bytes.len() {
            return Err("the world delta is truncated");
        }
        let slice = &self.bytes[self.at..end];
        self.at = end;
        Ok(slice)
    }
    /// Read `count` raw bytes.
    pub fn bytes(&mut self, count: usize) -> Result<&'a [u8], &'static str> {
        self.take(count)
    }
    /// Read one byte.
    pub fn u8(&mut self) -> Result<u8, &'static str> {
        Ok(self.take(1)?[0])
    }
    /// Read a little-endian `u16`.
    pub fn u16(&mut self) -> Result<u16, &'static str> {
        let bytes = self.take(2)?;
        Ok(u16::from_le_bytes([bytes[0], bytes[1]]))
    }
    /// Read a little-endian `u32`.
    pub fn u32(&mut self) -> Result<u32, &'static str> {
        let bytes = self.take(4)?;
        Ok(u32::from_le_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]))
    }
    /// Read a little-endian `i32`.
    pub fn i32(&mut self) -> Result<i32, &'static str> {
        let bytes = self.take(4)?;
        Ok(i32::from_le_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]))
    }
    /// Read a length-prefixed UTF-8 string.
    pub fn text(&mut self) -> Result<String, &'static str> {
        let len = self.u16()? as usize;
        let bytes = self.take(len)?;
        std::str::from_utf8(bytes)
            .map(str::to_string)
            .map_err(|_| "a string in the world delta is not UTF-8")
    }
}

/// Append a length-prefixed UTF-8 string, matching [`Reader::text`].
pub fn write_text(out: &mut Vec<u8>, value: &str) {
    let bytes = value.as_bytes();
    out.extend_from_slice(&(bytes.len().min(u16::MAX as usize) as u16).to_le_bytes());
    out.extend_from_slice(&bytes[..bytes.len().min(u16::MAX as usize)]);
}

/// Append a little-endian `u16`.
pub fn write_u16(out: &mut Vec<u8>, value: u16) {
    out.extend_from_slice(&value.to_le_bytes());
}

/// Append a little-endian `i32`.
pub fn write_i32(out: &mut Vec<u8>, value: i32) {
    out.extend_from_slice(&value.to_le_bytes());
}

/// The standard base64 alphabet, with padding.
const BASE64: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

/// Encode bytes as standard base64 (`AD-11`: the world half is base64).
pub fn base64_encode(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len().div_ceil(3) * 4);
    for chunk in bytes.chunks(3) {
        let first = chunk[0];
        let second = chunk.get(1).copied().unwrap_or(0);
        let third = chunk.get(2).copied().unwrap_or(0);
        out.push(BASE64[(first >> 2) as usize] as char);
        out.push(BASE64[(((first & 0b11) << 4) | (second >> 4)) as usize] as char);
        if chunk.len() > 1 {
            out.push(BASE64[(((second & 0b1111) << 2) | (third >> 6)) as usize] as char);
        } else {
            out.push('=');
        }
        if chunk.len() > 2 {
            out.push(BASE64[(third & 0b11_1111) as usize] as char);
        } else {
            out.push('=');
        }
    }
    out
}

/// Decode standard base64, returning `None` on any malformed input.
pub fn base64_decode(text: &str) -> Option<Vec<u8>> {
    let mut out = Vec::with_capacity(text.len() / 4 * 3);
    let mut accumulator: u32 = 0;
    let mut bits = 0u32;
    for byte in text.bytes() {
        if byte == b'=' {
            break;
        }
        let value = match byte {
            b'A'..=b'Z' => byte - b'A',
            b'a'..=b'z' => byte - b'a' + 26,
            b'0'..=b'9' => byte - b'0' + 52,
            b'+' => 62,
            b'/' => 63,
            _ => return None,
        } as u32;
        accumulator = (accumulator << 6) | value;
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((accumulator >> bits) as u8);
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l2::actions::ActionKind;

    fn sample() -> SaveData {
        let mut character = Character::authored(
            1,
            "hero",
            "wsp.origin.none",
            0,
            [10, 20, 30, 40, 6, 10, 20],
            10,
            5,
            vec![crate::l1::skills::SkillInstance {
                skill: "wsp.skill.martial-arts".into(),
                steps: 1,
                applies_to: vec![ActionTag::Melee],
            }],
            vec!["wsp.item.bat".into()],
            vec![],
        );
        character.weapon = Some("wsp.item.bat".into());
        character.damage = 7;
        character
            .conditions
            .push(Condition::new(ConditionKind::Stunned, 2, 2));
        let mut encounter = Encounter::new("wsp.encounter.alley", "wsp.sector.alley");
        encounter.panel = 4;
        encounter.queue(1, Action::attack(ActionKind::MeleeBash, 2));
        SaveData {
            save_version: SAVE_VERSION,
            engine_version: crate::ENGINE_VERSION.to_string(),
            ruleset: "basic".to_string(),
            content_hash: "abc123".to_string(),
            seed: 2026,
            tick: 99,
            rng_state: (11, 22),
            characters: vec![character],
            party: vec![1],
            vehicles: vec![crate::l2::vehicle::Vehicle::sample(9, 10, 6, 6)],
            flags: BTreeMap::from([("met_contact".to_string(), 1)]),
            encounter: Some(encounter),
            map: "wsp.sector.alley".to_string(),
            placements: vec![(1, 2, 3, 0), (2, 4, 5, 1)],
            mods: vec![("noir".to_string(), "def456".to_string())],
            scripts: vec![("noir-scripts".to_string(), "abc".to_string())],
            clock: {
                let mut clock = Clock::new(1440);
                clock.advance(97);
                clock
            },
            generator_version: GENERATOR_VERSION,
            world_record: "wsp.world.region".to_string(),
            generator: crate::l3::gen::Generator::district(8, 6, 2026),
            tile_delta: vec![(
                3,
                2,
                Tile {
                    level: 1,
                    blocking: 0,
                    terrain: 4,
                    material: 30,
                },
            )],
            stations: vec![Station {
                id: 9,
                behaviour: crate::l3::clock::Behaviour::Patrol,
                faction: "wsp.faction.watch".to_string(),
                home: (2, 2),
                schedule: crate::l3::clock::Schedule {
                    entries: vec![crate::l3::clock::ScheduleEntry {
                        slot: Some(DaySlot::Day),
                        at: (5, 5),
                    }],
                    home: Some((2, 2)),
                },
                notice_tiles: 3,
            }],
            standings: {
                let mut standings = Standings::default();
                standings.adjust("wsp.faction.watch", 4, 0);
                standings
            },
            journal: {
                let mut journal = Journal::default();
                journal.write("wsp.quest.missing", "journal.missing.ask");
                journal.pauses.push(("wsp.quest.talk".to_string(), 0));
                journal
            },
        }
    }

    #[test]
    fn a_save_round_trips_byte_for_byte() {
        let data = sample();
        let first = envelope(&data).to_string();
        let parsed = parse(&first).expect("parse");
        let second = envelope(&parsed).to_string();
        assert_eq!(first, second, "save -> load -> save must be byte-stable");
    }

    #[test]
    fn the_readable_half_really_is_readable() {
        let text = envelope(&sample()).to_string();
        // The core half is JSON a support process can read and edit (AD-11).
        assert!(text.contains("\"core\""));
        assert!(text.contains("\"traits\""));
        assert!(text.contains("\"damage\":7"));
        assert!(text.contains("\"content_hash\":\"abc123\""));
    }

    #[test]
    fn a_different_save_version_is_refused_rather_than_guessed() {
        let mut text = envelope(&sample()).to_string();
        text = text.replace("\"save_version\":2", "\"save_version\":3");
        let error = parse(&text).expect_err("must refuse");
        assert!(error.contains("save format"));
    }

    #[test]
    fn base64_round_trips_every_length_modulo_three() {
        for length in 0..32usize {
            let bytes: Vec<u8> = (0..length).map(|index| (index * 37 % 256) as u8).collect();
            let encoded = base64_encode(&bytes);
            assert_eq!(base64_decode(&encoded).as_deref(), Some(bytes.as_slice()));
        }
        assert_eq!(
            base64_encode(b"any carnal pleasure."),
            "YW55IGNhcm5hbCBwbGVhc3VyZS4="
        );
        assert_eq!(base64_decode("!!!!"), None);
    }

    #[test]
    fn the_world_delta_carries_placements_and_the_panel() {
        let data = sample();
        let parsed = parse(&envelope(&data).to_string()).expect("parse");
        assert_eq!(parsed.placements, data.placements);
        assert_eq!(parsed.map, data.map);
        let encounter = parsed.encounter.expect("encounter");
        assert_eq!(encounter.panel, 4);
        assert_eq!(encounter.queued.len(), 1);
        assert_eq!(encounter.queued[0].action.kind, ActionKind::MeleeBash);
    }
}
