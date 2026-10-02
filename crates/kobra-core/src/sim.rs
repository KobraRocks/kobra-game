//! Sim handles, the engine's global state, and the one mutation entry point
//! (02:02.9, 02:02.10).
//!
//! A `Sim` is the single serialised value (02:02.9) and an opaque handle is how
//! the caller refers to it (01:01.5 rule 1). Handles, not globals, are what let
//! the editor hold a scratch sim next to the live one and the game hold a
//! preview sim, with no shared mutable state.
//!
//! # What M1 simulates
//!
//! The vertical slice (05:05.5, M1): a campaign built from a scenario content
//! record, characters made through the one factory's three modes, one small
//! sector map, and a panel-driven encounter with melee, ranged, dodge, damage,
//! armour and conditions. Powers, vehicles and the script host are M2.
//!
//! # The command stream is the only mutation
//!
//! `kobra_command` is the one entry point (`02:02.10`): the shell, the editor's
//! playtest and the replay harness all drive the game the same way, which is
//! what makes the golden replay a real oracle rather than a parallel
//! implementation.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use crate::content::{self, ContentRegistry};
use crate::error::{self, Status};
use crate::json::Json;
use crate::l0::{Ladder, Ruleset};
use crate::l1::character::{Character, CreationMode};
use crate::l1::status::{self, ConditionKind};
use crate::l1::traits::Trait;
use crate::l2::actions::{Action, ActionKind};
use crate::l2::encounter::{self, Battle, Encounter, Phase};
use crate::l3::ai::{self, BehaviourRecord};
use crate::l3::clock::{Clock, DaySlot, Station};
use crate::l3::gen::{Baseline, GENERATOR_VERSION};
use crate::l3::world::{Position, World};
use crate::l4::dialogue;
use crate::l4::factions::Standings;
use crate::l4::journal::{Applied, EvaluationContext, Journal};
use crate::l4::progression;
use crate::packet::{self, DrawItem, PacketHeader, Ring};
use crate::rng::Rng;
use crate::save::{SaveData, SAVE_VERSION};
use crate::tables;

/// `q16` fixed point: the value of `1.0`.
pub const Q16_ONE: i32 = 65_536;

/// The engine's process-wide state, set once by `kobra_init`.
struct Engine {
    /// The seed a campaign defaults to.
    seed: u32,
    /// The ladder this build is compiled against (AD-14).
    ladder: &'static Ladder,
    /// Whether `kobra_init` has run.
    ready: bool,
    /// The merged content, once `kobra_load_content` has been called.
    content: Option<Arc<ContentRegistry>>,
    /// The load report, for the shell's diagnostics.
    report: Option<Json>,
}

static ENGINE: Mutex<Engine> = Mutex::new(Engine {
    seed: 0,
    ladder: tables::CAMPAIGN_LADDER,
    ready: false,
    content: None,
    report: None,
});

/// The live sims, indexed by `handle - 1`. `None` marks a freed slot.
static SIMS: Mutex<Vec<Option<Sim>>> = Mutex::new(Vec::new());

/// The last buffer an export produced, for the `(ptr << 32) | len` returns.
///
/// One buffer rather than one per export, because the ABI's contract is that
/// the returned bytes are valid until the next export call (01:01.5).
static OUTPUT: Mutex<Vec<u8>> = Mutex::new(Vec::new());

/// One simulation.
pub struct Sim {
    /// The handle the caller holds.
    pub handle: u32,
    /// The ladder the campaign was created under. Part of the save's identity.
    pub ruleset: Ruleset,
    /// The world seed (AD-6).
    pub seed: u32,
    /// Ticks advanced so far.
    pub tick: u32,
    /// Frames published so far. Also the ring slot selector.
    pub frame_id: u32,
    /// Viewport width in device pixels, as last reported by the renderer.
    pub viewport_w: u32,
    /// Viewport height in device pixels.
    pub viewport_h: u32,
    /// The one gameplay RNG (AD-6).
    pub rng: Rng,
    /// The merged content this sim plays from.
    pub content: Arc<ContentRegistry>,
    /// The map and who stands on it.
    pub world: World,
    /// Every combatant.
    pub characters: Vec<Character>,
    /// Every vehicle on the map (`4c:1300-1365`).
    pub vehicles: Vec<crate::l2::vehicle::Vehicle>,
    /// The party's entity ids.
    pub party: Vec<u32>,
    /// Campaign flags.
    pub flags: BTreeMap<String, i64>,
    /// The running encounter.
    pub encounter: Option<Encounter>,
    /// The scenario record id.
    pub scenario: String,
    /// The next entity id to hand out.
    pub next_entity: u32,
    /// Events produced since the caller last drained the outbox.
    pub outbox: Vec<Json>,
    /// How many party spawn points have been used.
    spawn_cursor: usize,

    // ---- the CRPG frame (M3, 05:05.5) -------------------------------------
    /// The world clock: day, panel, and the length of a day (`02:02.7`).
    pub clock: Clock,
    /// The baseline a save's per-sector delta is measured against (`02:02.6`).
    pub baseline: Baseline,
    /// The generator version that produced the baseline, recorded in the save.
    pub generator_version: u32,
    /// The world record the campaign was built from, when it was generated.
    pub world_record: String,
    /// The NPCs standing in the world, sorted by id (`02:02.6`).
    pub stations: Vec<Station>,
    /// The faction standing (`02:02.6`).
    pub standings: Standings,
    /// The quest journal, which is also the quest state (`02:02.7`).
    pub journal: Journal,
    /// The conversation currently open, if any: `(character, dialogue record)`.
    pub talking: Option<(u32, String)>,
    /// The node the open conversation is on.
    pub dialogue_node: String,
    /// The number of impact events applied, for the diagnostics panel.
    pub impacts: u32,
    /// Who won the last encounter, for the campaign bindings that fire when one
    /// ends (`02:02.7`). `None` while it is running.
    pub last_outcome: Option<u8>,
    /// The encounter that is running or was just concluded, so its binding can be
    /// looked up after the `Encounter` value itself has been dropped.
    pub concluded: Option<String>,
    /// Tiles the party leader has walked since the last panel, which the
    /// per-distance attack penalty reads (`4c:880`).
    ///
    /// Distinct from a character's own `moved_tiles`, which the encounter machine
    /// resets at each panel boundary: this is the exploration half of the same
    /// rule, and it is what a panel's `advance_panels` consumes.
    pub moved_tiles: i32,
}

impl Sim {
    /// The determinism oracle (02:02.9, 07:07.8).
    ///
    /// FNV-1a over the state a replay must reproduce, in a fixed field order.
    /// Presentation state — the viewport, the frame counter — is deliberately
    /// **not** included: a replay must be identical across machines and window
    /// sizes, and the renderer is disposable (01:01.3 property 1).
    pub fn hash_state(&self) -> u64 {
        let mut hash = Fnv::new();
        hash.write(self.ruleset.name().as_bytes());
        hash.write(&self.seed.to_le_bytes());
        hash.write(&self.tick.to_le_bytes());
        let (rng_state, rng_inc) = self.rng.save();
        hash.write(&rng_state.to_le_bytes());
        hash.write(&rng_inc.to_le_bytes());
        hash.write(&(self.characters.len() as u32).to_le_bytes());
        for character in &self.characters {
            hash.write(&character.id.to_le_bytes());
            hash.write(&[character.side]);
            hash.write(character.mode.id().as_bytes());
            for value in character.traits {
                hash.write(&value.to_le_bytes());
            }
            hash.write(&character.damage.to_le_bytes());
            hash.write(&character.max_damage.to_le_bytes());
            hash.write(&character.fortune.to_le_bytes());
            hash.write(&character.repute.to_le_bytes());
            hash.write(&character.lifestyle_rv.to_le_bytes());
            hash.write(&character.dying_steps.to_le_bytes());
            hash.write(&[u8::from(character.dead)]);
            hash.write(&character.moved_tiles.to_le_bytes());
            hash.write(&character.dodge_steps.to_le_bytes());
            hash.write(character.weapon.as_deref().unwrap_or("").as_bytes());
            for item in &character.items {
                hash.write(item.as_bytes());
            }
            for item in &character.armour {
                hash.write(item.as_bytes());
            }
            for skill in &character.skills {
                hash.write(skill.skill.as_bytes());
                hash.write(&skill.steps.to_le_bytes());
                for tag in &skill.applies_to {
                    hash.write(tag.id().as_bytes());
                }
            }
            for condition in &character.conditions {
                hash.write(condition.kind.id().as_bytes());
                hash.write(&condition.panels.to_le_bytes());
                hash.write(&condition.source.to_le_bytes());
            }
            for power in &character.powers {
                hash.write(power.id.as_bytes());
                hash.write(&power.rv.to_le_bytes());
                for (key, value) in &power.params {
                    hash.write(key.as_bytes());
                    hash.write(value.as_bytes());
                }
            }
            for boost in &character.trait_boosts {
                hash.write(boost.trait_.id().as_bytes());
                hash.write(&boost.rv.to_le_bytes());
                hash.write(&boost.panels.to_le_bytes());
            }
        }
        hash.write(&self.world.width.to_le_bytes());
        hash.write(&self.world.height.to_le_bytes());
        for tile in &self.world.tiles {
            hash.write(&[tile.level as u8, tile.blocking as u8, tile.terrain]);
            hash.write(&tile.material.to_le_bytes());
        }
        hash.write(&(self.vehicles.len() as u32).to_le_bytes());
        for vehicle in &self.vehicles {
            hash.write(&vehicle.id.to_le_bytes());
            hash.write(vehicle.record.as_bytes());
            hash.write(&vehicle.durability.to_le_bytes());
            hash.write(&vehicle.max_durability.to_le_bytes());
            hash.write(&vehicle.handling.to_le_bytes());
            hash.write(&vehicle.velocity.to_le_bytes());
            hash.write(&vehicle.operator.to_le_bytes());
            for passenger in &vehicle.passengers {
                hash.write(&passenger.to_le_bytes());
            }
            hash.write(&[u8::from(vehicle.destroyed)]);
        }
        for placement in &self.world.placements {
            hash.write(&placement.id.to_le_bytes());
            hash.write(&placement.at.x.to_le_bytes());
            hash.write(&placement.at.y.to_le_bytes());
            hash.write(&[placement.at.z as u8]);
        }
        match &self.encounter {
            None => hash.write(b"none"),
            Some(encounter) => {
                hash.write(encounter.id.as_bytes());
                hash.write(&encounter.panel.to_le_bytes());
                hash.write(encounter.phase.id().as_bytes());
                hash.write(&[u8::from(encounter.finished)]);
                hash.write(&[encounter.winner.unwrap_or(255)]);
                hash.write(&encounter.initiative_roll);
                for bonus in encounter.initiative_bonus {
                    hash.write(&bonus.to_le_bytes());
                }
                for queued in &encounter.queued {
                    hash.write(&queued.actor.to_le_bytes());
                    hash.write(queued.action.kind.id().as_bytes());
                    hash.write(&queued.action.target.to_le_bytes());
                    hash.write(&queued.action.fortune.to_le_bytes());
                }
            }
        }
        for (key, value) in &self.flags {
            hash.write(key.as_bytes());
            hash.write(&value.to_le_bytes());
        }
        // ---- the CRPG frame (M3) -------------------------------------------
        //
        // Everything a replay must reproduce and a save must restore. The
        // generator *version* is in the hash and the generator's parameters are
        // not: the parameters are content, so they move `rules_hash` (which the
        // save already binds), while the version is what decides whether a saved
        // delta applies at all (`02:02.6`).
        hash.write(&self.clock.day.to_le_bytes());
        hash.write(&self.clock.panel.to_le_bytes());
        hash.write(&self.clock.panels_per_day.to_le_bytes());
        hash.write(&self.generator_version.to_le_bytes());
        hash.write(&(self.stations.len() as u32).to_le_bytes());
        for station in &self.stations {
            hash.write(&station.id.to_le_bytes());
            hash.write(station.behaviour.id().as_bytes());
            hash.write(station.faction.as_bytes());
            hash.write(&station.home.0.to_le_bytes());
            hash.write(&station.home.1.to_le_bytes());
            hash.write(&station.notice_tiles.to_le_bytes());
            for entry in &station.schedule.entries {
                hash.write(&[match entry.slot {
                    None => 255,
                    Some(slot) => match slot {
                        DaySlot::Night => 0,
                        DaySlot::Morning => 1,
                        DaySlot::Day => 2,
                        DaySlot::Evening => 3,
                    },
                }]);
                hash.write(&entry.at.0.to_le_bytes());
                hash.write(&entry.at.1.to_le_bytes());
            }
            match station.schedule.home {
                Some((x, y)) => {
                    hash.write(&[1]);
                    hash.write(&x.to_le_bytes());
                    hash.write(&y.to_le_bytes());
                }
                None => hash.write(&[0, 0, 0, 0, 0, 0, 0, 0, 0]),
            }
        }
        for (faction, standing) in self.standings.all() {
            hash.write(faction.as_bytes());
            hash.write(&standing.to_le_bytes());
        }
        hash.write(&(self.journal.entries.len() as u32).to_le_bytes());
        for entry in &self.journal.entries {
            hash.write(entry.quest.as_bytes());
            hash.write(&[crate::l4::journal::status_code(entry.status)]);
            hash.write(&(entry.step as u32).to_le_bytes());
            for index in &entry.fired {
                hash.write(&(*index as u32).to_le_bytes());
            }
        }
        for (quest, line) in &self.journal.lines {
            hash.write(quest.as_bytes());
            hash.write(line.text_key.as_bytes());
            hash.write(&[u8::from(line.read)]);
        }
        for (quest, step) in &self.journal.pauses {
            hash.write(b"pause");
            hash.write(quest.as_bytes());
            hash.write(&(*step as u32).to_le_bytes());
        }
        for (quest, step) in &self.journal.released {
            hash.write(b"released");
            hash.write(quest.as_bytes());
            hash.write(&(*step as u32).to_le_bytes());
        }
        // `impacts` is deliberately **not** here. It is a diagnostics counter for
        // the status panel, not state a rule reads: the standing and the journal it
        // counts are both hashed above, so a replay's trajectory is already pinned
        // by the facts rather than by how many times they moved. Including it would
        // make the oracle depend on a counter that is reset on load, which is
        // exactly the kind of "the save does not round-trip" bug the oracle exists
        // to catch — and it would be a false positive, not a real one.
        hash.finish()
    }

    /// Advance the sim by `elapsed` ticks. Returns the ticks actually advanced.
    ///
    /// Ticks are integers, so "how much time passed" is the caller's decision —
    /// the sim never reads a clock (AD-1, 02:02.9 rule 2).
    pub fn tick(&mut self, elapsed: u32) -> u32 {
        let advanced = elapsed.min(MAX_TICKS_PER_CALL);
        self.tick = self.tick.saturating_add(advanced);
        advanced
    }

    /// Tell the sim the viewport it is drawing into.
    pub fn resize(&mut self, width: u32, height: u32) {
        self.viewport_w = width;
        self.viewport_h = height;
    }

    /// Build and publish a frame. Returns `(slot_address, dropped_items)`.
    pub fn publish_frame(&mut self, ring: Ring) -> (usize, u32) {
        let items = self.draw_items();
        let header = self.packet_header(&items);
        let dropped = ring.publish(&header, &items);
        if dropped > 0 {
            ring.note_overflow(dropped);
        }
        let addr = ring.slot((self.frame_id as usize) % packet::SLOT_COUNT) as usize;
        self.frame_id = self.frame_id.saturating_add(1);
        (addr, dropped)
    }

    /// The packet header for this frame.
    fn packet_header(&self, items: &[DrawItem]) -> PacketHeader {
        let flags = if items.len() > packet::DRAW_CAPACITY {
            packet::flags::PACKET_OVERFLOW
        } else {
            0
        };
        let hash = self.hash_state();
        PacketHeader {
            packet_version: packet::PACKET_VERSION,
            frame_id: self.frame_id,
            tick: self.tick,
            flags,
            camera_asset_hint: 0,
            camera_x_q16: 0,
            camera_y_q16: 0,
            // One third of a world unit per NDC unit, so the whole 8x6 alley fits
            // the viewport with margin: the packet's camera is simulation state
            // (AD-30), and a slice nobody can see is not a vertical slice.
            camera_zoom_q16: Q16_ONE / 3,
            camera_rot_q16: 0,
            camera_pitch_q16: 0,
            viewport_w: self.viewport_w,
            viewport_h: self.viewport_h,
            item_count: items.len().min(packet::DRAW_CAPACITY) as u32,
            text_count: 0,
            vfx_count: 0,
            offset_items: packet::SLOT_HEADER_BYTES as u32,
            offset_text: 0,
            offset_vfx: 0,
            // Presentation-only. Never saved, never read by the sim (AD-6).
            rng_cosmetic_state: 0,
            state_hash_lo: (hash & 0xffff_ffff) as u32,
            state_hash_hi: (hash >> 32) as u32,
        }
    }

    /// The map and its occupants, as draw items (`01:01.3`).
    ///
    /// The packet carries `asset_id`s and `q16` integers, never a GPU handle and
    /// never a float. M1 uses the built-in untextured quad for a tile and a
    /// character; the real meshes arrive with the asset pipeline, and the sim's job
    /// — emitting rows of draw items — does not change.
    fn draw_items(&self) -> Vec<DrawItem> {
        let mut items = Vec::with_capacity(self.world.tiles.len() + self.characters.len());
        let half_w = self.world.width / 2;
        let half_h = self.world.height / 2;
        for y in 0..self.world.height {
            for x in 0..self.world.width {
                let Some(tile) = self.world.tile(x, y) else {
                    continue;
                };
                let tint = if tile.blocking > 0 {
                    0x5050_50ff
                } else if tile.level > 0 {
                    0xb0a080ff
                } else {
                    0x2f4f3aff
                };
                items.push(self.quad(
                    (x - half_w) * Q16_ONE,
                    (y - half_h) * Q16_ONE,
                    Q16_ONE,
                    0,
                    tint,
                ));
            }
        }
        for character in &self.characters {
            let Some(at) = self.world.position(character.id) else {
                continue;
            };
            let tint = if character.dead {
                0x333333ff
            } else if character.side == 0 {
                0x4da3ffff
            } else {
                0xd4504aff
            };
            items.push(self.quad(
                (at.x - half_w) * Q16_ONE,
                (at.y - half_h) * Q16_ONE,
                (Q16_ONE * 3) / 4,
                1,
                tint,
            ));
        }
        items
    }

    /// One built-in quad.
    fn quad(&self, x_q16: i32, y_q16: i32, size_q16: i32, layer: u16, tint: u32) -> DrawItem {
        DrawItem {
            asset_id: ASSET_BUILTIN_QUAD,
            atlas_slot: 0,
            layer,
            kind: packet::kind::QUAD,
            blend: packet::blend::NORMAL,
            x_q16,
            y_q16,
            z_q16: 0,
            sx_q16: size_q16,
            sy_q16: size_q16,
            rot_q16: 0,
            tint_rgba8: tint,
            flags: 0,
            clip_id: 0,
            clip_time_q16: 0,
            clip_blend_id: 0,
            clip_blend_q16: 0,
            lod: 0,
            skin_id: 0,
        }
    }

    /// Snapshot the state a save carries (`AD-11`).
    pub fn to_save(&self) -> SaveData {
        SaveData {
            save_version: SAVE_VERSION,
            engine_version: crate::ENGINE_VERSION.to_string(),
            ruleset: self.ruleset.name().to_string(),
            content_hash: format!("{:016x}", self.content.rules_hash),
            seed: self.seed,
            tick: self.tick,
            rng_state: self.rng.save(),
            characters: self.characters.clone(),
            party: self.party.clone(),
            vehicles: self.vehicles.clone(),
            flags: self.flags.clone(),
            encounter: self.encounter.clone(),
            map: self
                .encounter
                .as_ref()
                .map(|encounter| encounter.map.clone())
                .unwrap_or_default(),
            placements: self
                .world
                .placements
                .iter()
                .map(|placement| (placement.id, placement.at.x, placement.at.y, placement.at.z))
                .collect(),
            mods: Vec::new(),
            scripts: crate::script::recorded(),
            clock: self.clock,
            generator_version: self.generator_version,
            world_record: self.world_record.clone(),
            generator: self.baseline.generator.clone(),
            // AD-11: only what differs from the regenerated baseline. A campaign
            // that has not touched its terrain pays two bytes for the whole map.
            tile_delta: self.baseline.delta(&self.world),
            stations: self.stations.clone(),
            standings: self.standings.clone(),
            journal: self.journal.clone(),
        }
    }

    /// Rebuild a sim from a save (`AD-11`).
    pub fn from_save(
        handle: u32,
        data: SaveData,
        registry: Arc<ContentRegistry>,
    ) -> Result<Sim, Status> {
        let ruleset = match data.ruleset.as_str() {
            "basic" => Ruleset::Basic,
            "advanced" => Ruleset::Advanced,
            _ => {
                return Err(Status::InvalidArgument);
            }
        };
        if ruleset != tables::CAMPAIGN_LADDER.ruleset {
            // 02:02.9: the two ladders resolve the same Rank Value differently
            // at the top of the scale, so a mismatch is a hard refusal.
            error::set(
                Status::InvalidArgument,
                "this save was created under a different Master Table",
                Some("the compiled ladder does not match the save's ruleset"),
            );
            return Err(Status::InvalidArgument);
        }
        let expected = format!("{:016x}", registry.rules_hash);
        if data.content_hash != expected {
            // AD-11: a mismatch is detected and reported, never silently loaded.
            error::set(
                Status::InvalidArgument,
                "the save was made with different content",
                Some("the enabled content no longer matches the save's content hash"),
            );
            return Err(Status::InvalidArgument);
        }
        // 02:02.6: **silently mis-placing the player's world is the one outcome
        // that is not allowed.** A save whose generator version differs from this
        // build's is refused with an explanation rather than loaded against a
        // different baseline — the alternative is a player standing inside a wall
        // that used to be a doorway.
        if data.generator_version != GENERATOR_VERSION {
            error::set(
                Status::InvalidArgument,
                "this save was made with a different world generator",
                Some("the terrain baseline changed, so the save's world delta no longer applies"),
            );
            return Err(Status::InvalidArgument);
        }
        // The baseline a delta is measured against is the *generated* terrain with
        // the campaign's authored overlays already baked in (`02:02.6`). Rebuilding
        // it the same way on load is what makes the delta meaningful: the overlay
        // is content, so it belongs to the baseline, and the delta carries only
        // what play changed.
        // The baseline is rebuilt the same way creation built it (`02:02.6`): a
        // world record's generator with its overlays baked in, or — for an authored
        // `sector_map`, which is its own baseline — the map's own tiles. Rebuilding
        // it from the map record rather than from the saved generator is what makes
        // an authored world's delta empty and its round trip exact; the saved
        // generator is an identity for the record, not a recipe to re-run.
        let baseline = match registry.world(&data.world_record) {
            Some(record) => {
                let mut baseline = Baseline::generate(&record.generator);
                for (map_id, x, y) in &record.overlays {
                    match registry.map(map_id) {
                        Some(map) => baseline.overlay(&map.world, (*x, *y)),
                        None => log_warn("a world overlay names a map that is not in the content"),
                    }
                }
                baseline
            }
            None => match registry.map(&data.map) {
                Some(map) => {
                    Baseline::authored(&world_baseline_generator(&map.world, data.seed), &map.world)
                }
                None => Baseline::generate(&data.generator),
            },
        };
        let (mut world, skipped) = baseline.apply(&data.tile_delta);
        if skipped > 0 {
            // Not a refusal: a delta entry off the map means the generator's
            // parameters moved in a way the version did not record, which is a
            // content bug worth reporting rather than a reason to refuse a save.
            let message = format!("{skipped} saved sector(s) were off the generated map");
            log_warn(&message);
        }
        for (id, x, y, z) in &data.placements {
            world.place(
                *id,
                Position {
                    x: *x,
                    y: *y,
                    z: *z,
                },
            );
        }
        let mut characters = data.characters;
        // A save stores powers but not their derived layers (`D17`), so the
        // registry recomputes them here rather than trusting the file.
        for character in &mut characters {
            character.recompute_power_effects(registry.as_ref());
        }
        Ok(Sim {
            handle,
            ruleset,
            seed: data.seed,
            tick: data.tick,
            frame_id: 0,
            viewport_w: 0,
            viewport_h: 0,
            rng: Rng::restore(data.rng_state.0, data.rng_state.1),
            content: registry,
            world,
            characters,
            vehicles: data.vehicles,
            party: data.party,
            flags: data.flags,
            encounter: data.encounter,
            scenario: String::new(),
            next_entity: 1 + data
                .placements
                .iter()
                .map(|(id, ..)| *id)
                .max()
                .unwrap_or(0),
            clock: data.clock,
            baseline,
            generator_version: data.generator_version,
            world_record: data.world_record,
            stations: data.stations,
            standings: data.standings,
            journal: data.journal,
            talking: None,
            dialogue_node: String::new(),
            impacts: 0,
            last_outcome: None,
            concluded: None,
            moved_tiles: 0,
            outbox: Vec::new(),
            spawn_cursor: 0,
        })
    }

    /// Apply one command, appending events to the outbox (`02:02.10`).
    ///
    /// The reason a command gives for refusing is **per command**: it is cleared
    /// here, at the start, so a refusal that does not name one cannot inherit the
    /// previous command's. That is not hypothetical — a blocked step followed by a
    /// fight with nobody in it reported `blocked`, because the reason was a static
    /// that only ever got written.
    pub fn command(&mut self, command: &Json) -> Result<(), Status> {
        clear_failure_reason();
        let name = command
            .get("cmd")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        match name {
            "create_character" => self.create_character(command),
            "begin_encounter" => self.begin_encounter(command),
            "plan" => self.plan(command),
            "commit" => self.commit_panel(),
            "stabilize" => self.stabilize(command),
            "spawn_vehicle" => self.spawn_vehicle(command),
            "set_flag" => {
                let key = command
                    .get("key")
                    .and_then(Json::as_str)
                    .ok_or(Status::InvalidArgument)?;
                let value = command.get("value").and_then(Json::as_i64).unwrap_or(0);
                self.flags.insert(key.to_string(), value);
                self.emit(Json::obj([
                    ("event", Json::string("flag")),
                    ("key", Json::string(key)),
                    ("value", Json::Num(value)),
                ]));
                Ok(())
            }
            "generate_world" => self.generate_world(command),
            "move" => self.move_character(command),
            "advance" => self.advance_time(command),
            "talk" => self.talk(command),
            "choose" => self.choose(command),
            "end_dialogue" => self.end_dialogue(),
            "give" => self.give_item(command),
            "take" => self.take_item(command),
            "equip" => self.equip_item(command),
            "buy" => self.buy(command),
            "sell" => self.sell(command),
            "advance_trait" => self.advance_trait_command(command),
            "recompute_quests" => {
                self.run_quests();
                Ok(())
            }
            "release_quest" => self.release_quest(command),
            "add_party" => self.add_party(command),
            "remove_party" => self.remove_party(command),
            _ => {
                self.emit_failure("unknown_command", name);
                Err(Status::InvalidArgument)
            }
        }
    }

    // -----------------------------------------------------------------------
    // M3 — the CRPG frame (05:05.5)
    // -----------------------------------------------------------------------

    /// Build the campaign's generated region (`02:02.6`).
    ///
    /// The baseline is the generator's terrain **with the campaign's authored
    /// overlays baked in**, and it is what a save's per-sector delta is measured
    /// against. Building it here rather than at load is what makes the overlay
    /// content rather than saved state: a designer changes a district by editing a
    /// record, and an existing save picks the change up without a migration.
    fn generate_world(&mut self, command: &Json) -> Result<(), Status> {
        let id = command
            .get("record")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(record) = self.content.world(id).cloned() else {
            self.emit_failure("unknown_world", id);
            return Err(Status::InvalidArgument);
        };
        let mut baseline = Baseline::generate(&record.generator);
        for (map_id, x, y) in &record.overlays {
            match self.content.map(map_id) {
                Some(map) => baseline.overlay(&map.world, (*x, *y)),
                None => {
                    self.emit_failure("unknown_world_overlay", map_id);
                    return Err(Status::InvalidArgument);
                }
            }
        }
        self.world = baseline.world();
        self.baseline = baseline;
        self.generator_version = GENERATOR_VERSION;
        self.world_record = record.id.clone();
        // The campaign opens at the hour its record names (02:02.7), and the
        // clock is set *before* the stations are placed so they start at the post
        // that hour puts them at.
        let per_day = self.clock.panels_per_day.max(1);
        self.clock = Clock {
            day: 1,
            panel: (u64::from(record.start_minute) * u64::from(per_day) / 1440) as u32,
            panels_per_day: per_day,
        };
        self.stations.clear();
        // The campaign's stations arrive with the world, in record order, so their
        // ids are stable across a regenerate (`02:02.6`).
        for station in &record.stations {
            let id = self.next_entity;
            self.next_entity += 1;
            let mut character = match self.content.authored_character(
                &station.character,
                id,
                if station.party { 0 } else { 1 },
            ) {
                Ok(character) => character,
                Err(_) => {
                    self.emit_failure("bad_station_character", &station.character);
                    continue;
                }
            };
            if station.party {
                character.side = 0;
                self.party.push(id);
            }
            // A station starts the campaign at the post its schedule names for
            // this hour, not at its `home`: the shipped campaign opens at dawn,
            // and a watchman who begins the day wherever he happens to sleep
            // walks home over the first minutes of play (`02:02.7`).
            let start = station
                .schedule
                .post_at(self.clock.slot())
                .unwrap_or(station.at);
            let at = Position {
                x: start.0,
                y: start.1,
                z: self
                    .world
                    .tile(start.0, start.1)
                    .map(|tile| tile.level)
                    .unwrap_or(0),
            };
            self.world.place(id, at);
            self.characters.push(character);
            self.stations.push(Station {
                id,
                behaviour: station.behaviour.kind,
                faction: station.faction.clone(),
                home: station.at,
                schedule: station.schedule.clone(),
                notice_tiles: station.behaviour.notice_tiles,
            });
        }
        // One party member starts at the authored arrival point, so a fresh
        // campaign is standing somewhere sensible rather than at the origin.
        if let Some(leader) = self.party.first().copied() {
            let at = Position {
                x: record.start.0,
                y: record.start.1,
                z: self
                    .world
                    .tile(record.start.0, record.start.1)
                    .map(|tile| tile.level)
                    .unwrap_or(0),
            };
            self.world.place(leader, at);
        } else if let Some(first) = self.stations.first().map(|station| station.id) {
            let _ = first;
        }
        self.emit(Json::obj([
            ("event", Json::string("world_generated")),
            ("record", Json::string(&record.id)),
            ("width", Json::Num(i64::from(record.generator.width))),
            ("height", Json::Num(i64::from(record.generator.height))),
            ("stations", Json::Num(self.stations.len() as i64)),
        ]));
        self.run_quests();
        Ok(())
    }

    /// Walk a party member one step, which costs a panel (`02:02.5`).
    ///
    /// Movement is the exploration driver: one move is one panel, so a buff that
    /// lasts three panels decays as the player walks (`02:02.5`). The step itself
    /// is the same greedy keystep an NPC takes, so a player and an NPC navigate
    /// identically.
    fn move_character(&mut self, command: &Json) -> Result<(), Status> {
        if self.encounter_running() {
            self.emit_failure("in_encounter", "");
            return Err(Status::InvalidArgument);
        }
        // A conversation is modal (`02:02.13`): walking away mid-sentence would
        // leave the dialogue open with the speaker out of reach, so the engine
        // refuses rather than letting the shell paper over it.
        if self.talking.is_some() {
            self.emit_failure("in_dialogue", "");
            return Err(Status::InvalidArgument);
        }
        let actor = match command.get("actor").and_then(Json::as_i64) {
            Some(id) => id as u32,
            None => match self.party.first().copied() {
                Some(id) => id,
                None => {
                    // "No party yet" is the normal state before New campaign, and
                    // a refusal that names it is a refusal the player can act on
                    // (`01:01.12` rule 9).
                    self.emit_failure("no_party", "");
                    return Err(Status::InvalidArgument);
                }
            },
        };
        if !self.party.contains(&actor) {
            self.emit_failure("not_in_party", "");
            return Err(Status::InvalidArgument);
        }
        let to = match command.get("to").and_then(Json::as_arr) {
            Some(pair) if pair.len() == 2 => Some((
                pair[0].as_i64().ok_or(Status::InvalidArgument)? as i32,
                pair[1].as_i64().ok_or(Status::InvalidArgument)? as i32,
            )),
            _ => None,
        };
        let Some(from) = self.world.position(actor) else {
            self.emit_failure("not_placed", "");
            return Err(Status::InvalidArgument);
        };
        // The destination is the next keystep toward the target, so a long walk is
        // a sequence of panels rather than a teleport — which is what makes the
        // clock and the schedules legible to the player.
        let step = match to {
            Some(target) => {
                let dx = (target.0 - from.x).signum();
                let dy = (target.1 - from.y).signum();
                let candidates = [
                    (from.x + dx, from.y + dy),
                    (from.x + dx, from.y),
                    (from.x, from.y + dy),
                ];
                candidates.into_iter().find(|(x, y)| {
                    self.world
                        .tile(*x, *y)
                        .is_some_and(|tile| tile.blocking == 0)
                        && self.world.occupant(*x, *y).is_none()
                })
            }
            None => None,
        };
        let Some((x, y)) = step else {
            // A blocked step is a refusal the player can see, not a silent no-op.
            self.emit_failure("blocked", "");
            return Ok(());
        };
        let z = self.world.tile(x, y).map(|tile| tile.level).unwrap_or(0);
        self.world.place(actor, Position { x, y, z });
        self.moved_tiles = self.moved_tiles.saturating_add(1);
        self.emit(Json::obj([
            ("event", Json::string("moved")),
            ("actor", Json::Num(i64::from(actor))),
            ("x", Json::Num(i64::from(x))),
            ("y", Json::Num(i64::from(y))),
        ]));
        // One walk is one panel.
        self.advance_panels(1, actor);
        Ok(())
    }

    /// Advance the world clock by whole panels (`02:02.7`).
    ///
    /// This is the exploration clock and the combat clock at once (`02:02.5`): a
    /// panel is the quantum for every rules duration, so a bulk advance decays a
    /// buff exactly as three separate panels would. Schedules move stations and may
    /// produce a hostile contact, which is emitted as an event rather than acted
    /// on — the AI proposes, the command stream disposes (`02:02.6`).
    fn advance_time(&mut self, command: &Json) -> Result<(), Status> {
        let panels = command
            .get("panels")
            .and_then(Json::as_i64)
            .unwrap_or(1)
            .clamp(0, MAX_PANELS_PER_CALL as i64) as u32;
        let actor = command
            .get("actor")
            .and_then(Json::as_i64)
            .map(|value| value as u32)
            .or_else(|| self.party.first().copied());
        self.advance_panels(panels, actor.unwrap_or(0));
        Ok(())
    }

    /// The one panel loop: clock, schedules, AI and quests.
    fn advance_panels(&mut self, panels: u32, actor: u32) {
        if self.encounter_running() {
            return;
        }
        for _ in 0..panels {
            self.clock.advance(1);
            self.moved_tiles = 0;
            self.tick_conditions();
            self.run_schedules();
            self.run_quests();
            if self.encounter.is_some() {
                // A contact started a fight, so the rest of the bulk advance is
                // the encounter's business rather than the clock's.
                break;
            }
        }
        let _ = actor;
        self.emit(Json::obj([
            ("event", Json::string("time")),
            ("day", Json::Num(i64::from(self.clock.day))),
            ("panel", Json::Num(i64::from(self.clock.panel))),
            ("slot", Json::string(self.clock.slot().id())),
        ]));
    }

    /// Count down every character's panel-scoped conditions and boosts.
    ///
    /// The same decrement the combat panel runs (`02:02.5` RESOLVE), which is why
    /// a buff's duration does not depend on whether the player was fighting.
    fn tick_conditions(&mut self) {
        for character in &mut self.characters {
            if character.dead {
                continue;
            }
            character.moved_tiles = 0;
            character.dodge_steps = 0;
            character.temporary_steps.clear();
            let expired = status::tick(&mut character.conditions);
            for kind in expired {
                let message = format!("condition {} expired", kind.id());
                let _ = message;
            }
            // A power's boost is a panel countdown too, and it decays on the same
            // clock (02:02.5), so a buff bought before a walk does not survive it.
            character.tick_boosts();
        }
    }

    /// Move every station along its schedule and report what each decided
    /// (`02:02.6`, `l3::ai`).
    fn run_schedules(&mut self) {
        if self.stations.is_empty() {
            return;
        }
        let party: Vec<(u32, Position, i32)> = self
            .party
            .iter()
            .filter_map(|id| {
                let character = self.characters.iter().find(|entry| entry.id == *id)?;
                let at = self.world.position(*id)?;
                Some((*id, at, character.max_damage - character.damage))
            })
            .collect();
        let records = |kind| BehaviourRecord::canonical(kind);
        let slot = self.clock.slot();
        let decisions = ai::advance_stations(
            &mut self.world,
            &mut self.stations,
            &party,
            &records,
            &self.standings,
            &self.content.factions,
            slot,
        );
        let mut contacts = Vec::new();
        for decision in &decisions {
            if let Some((x, y)) = decision.moved_to {
                self.emit(Json::obj([
                    ("event", Json::string("station_moved")),
                    ("station", Json::Num(i64::from(decision.station))),
                    ("x", Json::Num(i64::from(x))),
                    ("y", Json::Num(i64::from(y))),
                ]));
            }
            if decision.intent == ai::Intent::Engage {
                contacts.push(decision.clone());
            }
        }
        // The AI proposes a fight; the sim does not start one behind the player's
        // back. A hostile contact is an event with the station named, and
        // `begin_encounter` with `"encounter": "wild"` is what acts on it — the
        // same command the player's own attack uses (`02:02.6`).
        for contact in contacts {
            self.emit(Json::obj([
                ("event", Json::string("hostile_contact")),
                ("station", Json::Num(i64::from(contact.station))),
                (
                    "target",
                    match contact.target {
                        Some(id) => Json::Num(i64::from(id)),
                        None => Json::Null,
                    },
                ),
                ("intent", Json::string(contact.intent.id())),
            ]));
        }
    }

    /// Evaluate the quest graph and emit what changed (`02:02.7`).
    fn run_quests(&mut self) {
        let Some(actor) = self.party.first().copied() else {
            return;
        };
        let scale = self.content.rules.impact.clone();
        let defaults = self.content.standing_defaults();
        let defaults = |faction: &str| defaults.get(faction).copied().unwrap_or(0);
        let item_ids = self.content.item_ids();
        let quests = self.content.quests.clone();
        let planned = {
            let character = self.characters.iter().find(|entry| entry.id == actor);
            let context = EvaluationContext {
                actor: character,
                flags: &self.flags,
                standings: &self.standings,
                journal: &self.journal,
                clock: &self.clock,
                factions: &self.content.factions,
                item_ids: &item_ids,
            };
            self.journal.plan(&quests, &context)
        };
        let applied = {
            let character = self.characters.iter_mut().find(|entry| entry.id == actor);
            self.journal.apply_plan(
                &planned,
                character,
                &mut self.flags,
                &mut self.standings,
                &scale,
                &defaults,
                &quests,
            )
        };
        self.emit_journal_events(&applied);
    }

    /// Turn a journal batch into events, so a replay's stream explains itself.
    fn emit_journal_events(&mut self, applied: &Applied) {
        for (quest, status) in &applied.quests {
            self.emit(Json::obj([
                ("event", Json::string("quest")),
                ("quest", Json::string(quest)),
                ("status", Json::string(status.id())),
            ]));
        }
        for (quest, step) in &applied.steps {
            self.emit(Json::obj([
                ("event", Json::string("quest_step")),
                ("quest", Json::string(quest)),
                ("step", Json::Num(*step as i64)),
            ]));
        }
        for line in &applied.written {
            self.emit(Json::obj([
                ("event", Json::string("journal")),
                ("text_key", Json::string(line)),
            ]));
        }
        for marker in &applied.markers {
            self.emit(Json::obj([
                ("event", Json::string("campaign")),
                ("what", Json::string(marker)),
            ]));
            if marker.starts_with("standing:") {
                self.impacts = self.impacts.saturating_add(1);
            }
        }
    }

    /// Release the step a quest is paused on (`QuestStep::auto_advance: false`).
    ///
    /// The one manual step in the quest runtime, and it takes a **quest id**, not
    /// a step index: "which step am I on" is the journal's own fact, so a caller
    /// cannot close the wrong one (`02:02.7`).
    fn release_quest(&mut self, command: &Json) -> Result<(), Status> {
        let quest = command
            .get("quest")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        match self.journal.release_paused(quest) {
            Ok(step) => {
                self.emit(Json::obj([
                    ("event", Json::string("quest_step_released")),
                    ("quest", Json::string(quest)),
                    ("step", Json::Num(step as i64)),
                ]));
                // The released step may unblock the next one immediately.
                self.run_quests();
                Ok(())
            }
            Err(reason) => {
                self.emit_failure("cannot_release", reason);
                Err(Status::InvalidArgument)
            }
        }
    }

    /// Open a conversation with a station (`02:02.7`).
    fn talk(&mut self, command: &Json) -> Result<(), Status> {
        if self.encounter_running() {
            self.emit_failure("in_encounter", "");
            return Err(Status::InvalidArgument);
        }
        let actor = command
            .get("actor")
            .and_then(Json::as_i64)
            .map(|value| value as u32)
            .or_else(|| self.party.first().copied())
            .ok_or(Status::InvalidArgument)?;
        let requested = command
            .get("partner")
            .and_then(Json::as_i64)
            .map(|value| value as u32);
        // The partner defaults to the nearest station the actor can see, so the UI
        // does not have to resolve targets itself. With nobody in reach the leader
        // may still have the campaign's authored self-dialogue (`02:02.13`).
        let partner = match requested {
            Some(id) => Some(id),
            None => {
                let from = self.world.position(actor).ok_or(Status::InvalidArgument)?;
                self.stations
                    .iter()
                    .filter_map(|station| {
                        let at = self.world.position(station.id)?;
                        Some((crate::l3::world::World::distance(from, at), station.id))
                    })
                    .filter(|(distance, _)| *distance <= 1)
                    .min()
                    .map(|(_, id)| id)
            }
        };
        let (partner, dialogue_id) = match partner {
            Some(partner) => {
                let Some(station_faction) = self
                    .stations
                    .iter()
                    .find(|entry| entry.id == partner)
                    .map(|entry| entry.faction.clone())
                else {
                    self.emit_failure("not_a_station", "");
                    return Err(Status::InvalidArgument);
                };
                let dialogue_id =
                    self.content
                        .world(&self.world_record)
                        .and_then(|record| {
                            record.stations.iter().find(|candidate| {
                                candidate.character == self.character_sheet(partner)
                            })
                        })
                        .map(|candidate| candidate.dialogue.clone())
                        .filter(|id| !id.is_empty());
                match dialogue_id {
                    Some(id) => (partner, id),
                    None => {
                        self.emit_failure("no_dialogue", &station_faction);
                        return Err(Status::InvalidArgument);
                    }
                }
            }
            None => {
                // Nobody in reach: the leader speaks to themselves, when the
                // campaign authored something to say (`02:02.13`).
                let self_dialogue = self
                    .content
                    .world(&self.world_record)
                    .map(|record| record.self_dialogue.clone())
                    .filter(|id| !id.is_empty());
                match self_dialogue {
                    Some(id) => (actor, id),
                    None => {
                        self.emit_failure("no_partner", "");
                        return Err(Status::InvalidArgument);
                    }
                }
            }
        };
        let dialogue = self
            .content
            .dialogue(&dialogue_id)
            .ok_or(Status::InvalidArgument)?
            .clone();
        self.talking = Some((partner, dialogue_id));
        self.dialogue_node = dialogue.entry.clone();
        self.emit(Json::obj([
            ("event", Json::string("dialogue_open")),
            ("actor", Json::Num(i64::from(actor))),
            ("partner", Json::Num(i64::from(partner))),
            ("dialogue", Json::string(&dialogue.id)),
            ("node", Json::string(&self.dialogue_node)),
        ]));
        // Opening the conversation is a panel, like any other deliberate act.
        self.advance_panels(1, actor);
        Ok(())
    }

    /// The baseline a world's delta is measured against (`02:02.6`).
    ///
    /// A world record's generator produces the terrain and its overlays are baked
    /// in; an authored `sector_map` with no world record *is* the baseline. Both
    /// end up as a `Baseline`, because a save's delta has to be measured against
    /// something that can be rebuilt from the seed and the record alone.
    fn baseline_for(&self, map_id: &str, world: &World) -> Baseline {
        // A world record that *names this map* is the region the campaign
        // generates, so its generator is the baseline and the map is an overlay on
        // it. A map no world record names is authored outright (`02:02.6`), and it
        // is its own baseline — generating terrain underneath it would invent
        // solid edges the author never wrote, which is the opposite of an override.
        let record = self.content.worlds.iter().find(|record| {
            record
                .overlays
                .iter()
                .any(|(overlay, _, _)| overlay == map_id)
        });
        match record {
            Some(record) => {
                let mut baseline = Baseline::generate(&record.generator);
                for (overlay_map, x, y) in &record.overlays {
                    if let Some(map) = self.content.map(overlay_map) {
                        baseline.overlay(&map.world, (*x, *y));
                    }
                }
                baseline
            }
            None => Baseline::authored(&world_baseline_generator(world, self.seed), world),
        }
    }

    /// Start a fight in the world the party is already in (`02:02.6`).
    ///
    /// The stations that are hostile become the opposition; the party keeps its
    /// positions and its own map. Nothing is spawned and nothing is moved, so the
    /// fight's outcome lands on the world the player has been walking through —
    /// and a defeated NPC is a body where it fell (`rules.corpse_occupies`).
    fn begin_wild_encounter(&mut self) -> Result<(), Status> {
        if self.encounter.is_some() {
            return Err(Status::InvalidArgument);
        }
        if self.world_record.is_empty() && self.stations.is_empty() {
            self.emit_failure("no_world", "");
            return Err(Status::InvalidArgument);
        }
        let default = self.standings.clone();
        let hostile: Vec<u32> = self
            .stations
            .iter()
            .filter(|station| {
                !station.faction.is_empty()
                    && crate::l3::ai::hostile(station, &default, &self.content.factions)
            })
            .map(|station| station.id)
            .collect();
        if hostile.is_empty() {
            // A refusal the player can see, rather than an empty encounter.
            self.emit_failure("no_hostiles", "");
            return Err(Status::InvalidArgument);
        }
        // The encounter names its map for the same reason every other encounter
        // does: a save has to be readable without the registry in hand.
        let map_id = if self.world_record.is_empty() {
            "wsp.sector.alley".to_string()
        } else {
            self.world_record.clone()
        };
        self.encounter = Some(Encounter::new("wild", &map_id));
        self.concluded = Some("wild".to_string());
        self.last_outcome = None;
        for id in &hostile {
            if let Some(character) = self.characters.iter_mut().find(|entry| entry.id == *id) {
                character.side = 1;
            }
        }
        self.emit(Json::obj([
            ("event", Json::string("encounter_start")),
            ("encounter", Json::string("wild")),
            ("map", Json::string(&map_id)),
            (
                "hostiles",
                Json::arr(hostile.iter().map(|id| Json::Num(i64::from(*id)))),
            ),
        ]));
        Ok(())
    }

    /// The character sheet a station was built from.
    fn character_sheet(&self, id: u32) -> String {
        self.stations
            .iter()
            .find(|station| station.id == id)
            .and_then(|station| {
                self.content
                    .world(&self.world_record)
                    .and_then(|record| {
                        record
                            .stations
                            .iter()
                            .find(|candidate| candidate.at == station.home)
                    })
                    .map(|candidate| candidate.character.clone())
            })
            .unwrap_or_default()
    }

    /// Take a dialogue option (`02:02.7`).
    ///
    /// A check is resolved through the ordinary resolution primitive, so a
    /// conversation cannot disagree with an attack about what a colour means. A
    /// failed check still takes the option where the author sent it: the option's
    /// `next` is the branch, and a check that only *gates* an option is expressed
    /// with `when` instead. A check's colour is reported so a scene can branch on
    /// it with an effect.
    fn choose(&mut self, command: &Json) -> Result<(), Status> {
        let (actor, dialogue_id) = self.talking.clone().ok_or(Status::InvalidArgument)?;
        let option_id = command
            .get("option")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(dialogue) = self.content.dialogue(&dialogue_id).cloned() else {
            self.emit_failure("unknown_dialogue", &dialogue_id);
            return Err(Status::InvalidArgument);
        };
        let Some(node) = dialogue.node(&self.dialogue_node).cloned() else {
            let node = self.dialogue_node.clone();
            self.emit_failure("unknown_dialogue_node", &node);
            return Err(Status::InvalidArgument);
        };
        let Some(option) = node
            .options
            .iter()
            .find(|option| option.id == option_id)
            .cloned()
        else {
            self.emit_failure("unknown_option", option_id);
            return Err(Status::InvalidArgument);
        };
        let item_ids = self.content.item_ids();
        let offered = {
            let character = self.characters.iter().find(|entry| entry.id == actor);
            let context = EvaluationContext {
                actor: character,
                flags: &self.flags,
                standings: &self.standings,
                journal: &self.journal,
                clock: &self.clock,
                factions: &self.content.factions,
                item_ids: &item_ids,
            };
            dialogue::evaluate(&option.when, &context)
        };
        if !offered {
            self.emit_failure("option_not_offered", option_id);
            return Err(Status::InvalidArgument);
        }
        // A check draws from the seeded stream, so a conversation is as
        // reproducible as a fight.
        let colour = match &option.check {
            None => None,
            Some(check) => {
                let character = self
                    .characters
                    .iter()
                    .find(|entry| entry.id == actor)
                    .ok_or(Status::InvalidArgument)?;
                let roll = self.rng.d100();
                Some(check.resolve(character, roll))
            }
        };
        let scale = self.content.rules.impact.clone();
        let defaults = self.content.standing_defaults();
        let defaults = |faction: &str| defaults.get(faction).copied().unwrap_or(0);
        let applied = {
            let character = self.characters.iter_mut().find(|entry| entry.id == actor);
            self.journal.apply(
                &option.effects,
                character,
                &mut self.flags,
                &mut self.standings,
                &scale,
                &defaults,
            )
        };
        self.emit_journal_events(&applied);
        if let Some(colour) = colour {
            self.emit(Json::obj([
                ("event", Json::string("check")),
                ("actor", Json::Num(i64::from(actor))),
                ("option", Json::string(&option.id)),
                (
                    "colour",
                    Json::string(crate::l2::actions::colour_name(colour)),
                ),
                (
                    "required",
                    Json::string(crate::l2::actions::colour_name(
                        option.check.as_ref().map(|c| c.required).unwrap_or(colour),
                    )),
                ),
            ]));
        }
        self.emit(Json::obj([
            ("event", Json::string("dialogue_choice")),
            ("actor", Json::Num(i64::from(actor))),
            ("option", Json::string(&option.id)),
        ]));
        // An invited fight is the only way a fight starts from inside a
        // conversation (`02:02.13`): the option ends the talk and hands the same
        // encounter machine a fight, so it resolves by one path rather than two.
        if let Some(dialogue::DialogueAction::Fight { encounter }) = &option.action {
            let encounter = encounter.clone();
            self.end_dialogue()?;
            let start = if encounter.is_empty() {
                Json::obj([("encounter", Json::string("wild"))])
            } else {
                Json::obj([("encounter", Json::string(&encounter))])
            };
            self.begin_encounter(&start)?;
            self.run_quests();
            return Ok(());
        }
        match &option.next {
            Some(next) if dialogue.node(next).is_some() => {
                self.dialogue_node = next.clone();
            }
            _ => self.end_dialogue()?,
        }
        // A quest may have moved as a result of the choice.
        self.run_quests();
        Ok(())
    }

    /// Close the open conversation.
    fn end_dialogue(&mut self) -> Result<(), Status> {
        let Some((partner, _)) = self.talking.take() else {
            return Err(Status::InvalidArgument);
        };
        self.dialogue_node.clear();
        self.emit(Json::obj([
            ("event", Json::string("dialogue_close")),
            ("partner", Json::Num(i64::from(partner))),
        ]));
        Ok(())
    }

    /// Hand an item to a character (`02:02.7`).
    fn give_item(&mut self, command: &Json) -> Result<(), Status> {
        let item = command
            .get("item")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        if self.content.item(item).is_none() {
            self.emit_failure("unknown_item", item);
            return Err(Status::InvalidArgument);
        }
        let actor = self.actor_id(command)?;
        let slots = self.content.rules.carry_slots;
        let character = self
            .characters
            .iter_mut()
            .find(|entry| entry.id == actor)
            .ok_or(Status::InvalidArgument)?;
        if !crate::l4::economy::can_carry(character, slots) {
            self.emit_failure("carrying_too_much", item);
            return Err(Status::InvalidArgument);
        }
        if !character.items.iter().any(|held| held == item) {
            character.items.push(item.to_string());
        }
        self.emit(Json::obj([
            ("event", Json::string("item_given")),
            ("actor", Json::Num(i64::from(actor))),
            ("item", Json::string(item)),
        ]));
        self.run_quests();
        Ok(())
    }

    /// Take an item from a character.
    fn take_item(&mut self, command: &Json) -> Result<(), Status> {
        let item = command
            .get("item")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let actor = self.actor_id(command)?;
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == actor) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        crate::l4::economy::drop_item(character, item);
        self.emit(Json::obj([
            ("event", Json::string("item_taken")),
            ("actor", Json::Num(i64::from(actor))),
            ("item", Json::string(item)),
        ]));
        Ok(())
    }

    /// Wield or wear an item (`4c:1084`).
    fn equip_item(&mut self, command: &Json) -> Result<(), Status> {
        let item_id = command
            .get("item")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(item) = self.content.item(item_id).cloned() else {
            self.emit_failure("unknown_item", item_id);
            return Err(Status::InvalidArgument);
        };
        let actor = self.actor_id(command)?;
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == actor) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        let result = if item.armour_rv() > 0 {
            crate::l4::economy::wear(character, &item)
        } else {
            crate::l4::economy::equip(character, &item)
        };
        if let Err(reason) = result {
            self.emit_failure("cannot_equip", reason);
            return Err(Status::InvalidArgument);
        }
        self.emit(Json::obj([
            ("event", Json::string("equipped")),
            ("actor", Json::Num(i64::from(actor))),
            ("item", Json::string(item_id)),
        ]));
        Ok(())
    }

    /// Buy an item from a merchant (`4c:1255-1272`).
    ///
    /// Lifestyle gates procurement: the price is the shop's price for *this*
    /// character's band, and a character who cannot afford it is refused rather
    /// than given a partial purchase.
    fn buy(&mut self, command: &Json) -> Result<(), Status> {
        let shop_id = command
            .get("shop")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let item_id = command
            .get("item")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(shop) = self.content.shop(shop_id).cloned() else {
            self.emit_failure("unknown_shop", shop_id);
            return Err(Status::InvalidArgument);
        };
        let Some(item) = self.content.item(item_id).cloned() else {
            self.emit_failure("unknown_item", item_id);
            return Err(Status::InvalidArgument);
        };
        let actor = self.actor_id(command)?;
        let slots = self.content.rules.carry_slots;
        let prices = self.content.rules.prices.clone();
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == actor) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        let Some(price) = shop.price_for(&item, character.lifestyle_rv, &prices) else {
            self.emit_failure("not_in_stock", item_id);
            return Err(Status::InvalidArgument);
        };
        if !crate::l4::economy::can_carry(character, slots) {
            self.emit_failure("carrying_too_much", item_id);
            return Err(Status::InvalidArgument);
        }
        if character.fortune < price {
            self.emit_failure("cannot_afford", item_id);
            return Err(Status::InvalidArgument);
        }
        character.fortune -= price;
        character.items.push(item.id.clone());
        self.emit(Json::obj([
            ("event", Json::string("bought")),
            ("actor", Json::Num(i64::from(actor))),
            ("shop", Json::string(shop_id)),
            ("item", Json::string(item_id)),
            ("price", Json::Num(i64::from(price))),
        ]));
        Ok(())
    }

    /// Sell an item to a merchant (`4c:1255-1272`).
    fn sell(&mut self, command: &Json) -> Result<(), Status> {
        let shop_id = command
            .get("shop")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let item_id = command
            .get("item")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(shop) = self.content.shop(shop_id).cloned() else {
            self.emit_failure("unknown_shop", shop_id);
            return Err(Status::InvalidArgument);
        };
        let Some(item) = self.content.item(item_id).cloned() else {
            self.emit_failure("unknown_item", item_id);
            return Err(Status::InvalidArgument);
        };
        let actor = self.actor_id(command)?;
        let prices = self.content.rules.prices.clone();
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == actor) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        let Some(offer) = shop.offer_for(&item, &prices, character.lifestyle_rv) else {
            self.emit_failure("not_bought_here", item_id);
            return Err(Status::InvalidArgument);
        };
        if !character.items.iter().any(|held| held == item_id) {
            self.emit_failure("not_carried", item_id);
            return Err(Status::InvalidArgument);
        }
        crate::l4::economy::drop_item(character, item_id);
        character.fortune += offer;
        self.emit(Json::obj([
            ("event", Json::string("sold")),
            ("actor", Json::Num(i64::from(actor))),
            ("shop", Json::string(shop_id)),
            ("item", Json::string(item_id)),
            ("price", Json::Num(i64::from(offer))),
        ]));
        Ok(())
    }

    /// Spend Fortune to raise a stored Rank Value (`4c:1381-1391`).
    fn advance_trait_command(&mut self, command: &Json) -> Result<(), Status> {
        let trait_ = command
            .get("trait")
            .and_then(Json::as_str)
            .and_then(Trait::from_id)
            .ok_or(Status::InvalidArgument)?;
        let steps = command.get("steps").and_then(Json::as_i64).unwrap_or(1) as i32;
        let advancement = self.content.rules.advancement.clone();
        let actor = self.actor_id(command)?;
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == actor) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        match progression::advance_trait(&advancement, character, trait_, steps) {
            Ok(cost) => {
                let value = character.stored(trait_);
                self.emit(Json::obj([
                    ("event", Json::string("advanced")),
                    ("actor", Json::Num(i64::from(actor))),
                    ("trait", Json::string(trait_.id())),
                    ("rv", Json::Num(i64::from(value))),
                    ("cost", Json::Num(i64::from(cost))),
                ]));
                Ok(())
            }
            Err(refusal) => {
                let reason = refusal.id().to_string();
                self.emit_failure("cannot_advance", &reason);
                Err(Status::InvalidArgument)
            }
        }
    }

    /// The nearest free, walkable tile to an anchor, the anchor first.
    ///
    /// One tile per ring, so a party forming up does not stack on the leader and
    /// does not scatter across the district. A world with nowhere left returns
    /// `None`, and the character is simply unplaced — which the roster reports
    /// rather than the sim inventing a position.
    fn free_tile_near(&self, anchor: (i32, i32)) -> Option<(i32, i32)> {
        if self
            .world
            .tile(anchor.0, anchor.1)
            .is_some_and(|tile| tile.blocking == 0)
            && self.world.occupant(anchor.0, anchor.1).is_none()
        {
            return Some(anchor);
        }
        for radius in 1..=8i32 {
            for dy in -radius..=radius {
                for dx in -radius..=radius {
                    let (x, y) = (anchor.0 + dx, anchor.1 + dy);
                    if !self.world.in_bounds(x, y) {
                        continue;
                    }
                    let Some(tile) = self.world.tile(x, y) else {
                        continue;
                    };
                    if tile.blocking == 0 && self.world.occupant(x, y).is_none() {
                        return Some((x, y));
                    }
                }
            }
        }
        None
    }

    /// Whether a fight is running.
    ///
    /// **Not the same as "an encounter exists".** A concluded encounter stays in
    /// the sim until its outcome has been applied and the world has been handed
    /// back, because the status document reports it (`finished`) and the save
    /// carries it. Treating the two as one is how a player who won a fight ends up
    /// unable to walk — the encounter is over, but the world is still refused.
    fn encounter_running(&self) -> bool {
        self.encounter
            .as_ref()
            .is_some_and(|encounter| !is_finished(encounter))
    }

    /// The character a command acts on: the named one, or the party's first.
    fn actor_id(&self, command: &Json) -> Result<u32, Status> {
        match command.get("actor").and_then(Json::as_i64) {
            Some(id) => Ok(id as u32),
            None => self.party.first().copied().ok_or(Status::InvalidArgument),
        }
    }

    /// Put a character in the party (`02:02.7`).
    fn add_party(&mut self, command: &Json) -> Result<(), Status> {
        let id = command
            .get("actor")
            .and_then(Json::as_i64)
            .ok_or(Status::InvalidArgument)? as u32;
        let Some(character) = self.characters.iter_mut().find(|entry| entry.id == id) else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        character.side = 0;
        if !self.party.contains(&id) {
            self.party.push(id);
            self.party.sort_unstable();
        }
        self.emit(Json::obj([
            ("event", Json::string("party_joined")),
            ("actor", Json::Num(i64::from(id))),
        ]));
        Ok(())
    }

    /// Remove a character from the party.
    fn remove_party(&mut self, command: &Json) -> Result<(), Status> {
        let Some(id) = command.get("actor").and_then(Json::as_i64) else {
            self.emit_failure("no_actor", "");
            return Err(Status::InvalidArgument);
        };
        let id = id as u32;
        self.party.retain(|member| *member != id);
        if let Some(character) = self.characters.iter_mut().find(|entry| entry.id == id) {
            character.side = 1;
        }
        self.emit(Json::obj([
            ("event", Json::string("party_left")),
            ("actor", Json::Num(i64::from(id))),
        ]));
        Ok(())
    }

    /// Push an event.
    fn emit(&mut self, event: Json) {
        self.outbox.push(event);
    }

    /// Record a refusal as an event, so a replay's event stream explains itself.
    ///
    /// The reason is also remembered as the **last failure**, so the ABI can hand
    /// it back as the error's detail and a caller's diagnostic can name why rather
    /// than saying "refused" (`01:01.12` rule 9). A reason is a string id the
    /// locale resolves (`AD-22`), never English.
    fn emit_failure(&mut self, reason: &str, detail: &str) {
        record_failure_reason(reason);
        self.emit(Json::obj([
            ("event", Json::string("failed")),
            ("reason", Json::string(reason)),
            ("detail", Json::string(detail)),
        ]));
    }

    /// The scenario's map and opposition (`02:02.6`).
    fn begin_encounter(&mut self, command: &Json) -> Result<(), Status> {
        let id = command
            .get("encounter")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        // A fight is not something a conversation does behind its own back: it is
        // *offered* by the conversation (`02:02.13`), and the inviting option ends
        // the talk before it starts one. Reaching here while talking therefore
        // means a client tried to begin a fight mid-sentence, which is a refusal.
        if self.talking.is_some() {
            self.emit_failure("in_dialogue", "");
            return Err(Status::InvalidArgument);
        }
        // `"wild"` is the exploration hand-off (`02:02.6`): the fight happens on
        // the map the party is already standing on, against the stations that are
        // hostile to them, and the world is exactly as it was when the last panel
        // ended. It is the same `Encounter` machine, the same map and the same
        // entities, which is what makes "an AI action and a player action resolve
        // identically" true rather than aspirational.
        if id == "wild" {
            return self.begin_wild_encounter();
        }
        if self.encounter_running() {
            self.emit_failure("in_encounter", "");
            return Err(Status::InvalidArgument);
        }
        let Some(record) = self.content.encounter(id).cloned() else {
            self.emit_failure("unknown_encounter", id);
            return Err(Status::InvalidArgument);
        };
        let Some(map) = self.content.map(&record.map).map(|map| map.world.clone()) else {
            self.emit_failure("unknown_map", &record.map);
            return Err(Status::InvalidArgument);
        };
        // An authored map is its own baseline (`02:02.6`): M1 and M2 ship one, and
        // M3's generated region is what a campaign uses instead. Either way the
        // save stores a delta from *something*, so an authored map goes through the
        // same path and a save taken on it round-trips exactly.
        self.baseline = self.baseline_for(&record.map, &map);
        self.world = map;
        self.scenario = record.id.clone();
        self.stations.clear();
        self.world_record.clear();
        self.spawn_cursor = 0;
        self.encounter = Some(Encounter::new(&record.id, &record.map));
        self.concluded = Some(record.id.clone());
        self.last_outcome = None;
        // The opposition is authored content, so it is spawned here; the party
        // arrives through `create_character`, in creation order.
        for spawn in &record.enemies {
            let id = self.next_entity;
            self.next_entity += 1;
            match self.content.authored_character(&spawn.character, id, 1) {
                Ok(character) => {
                    let at = Position {
                        x: spawn.x,
                        y: spawn.y,
                        z: self
                            .world
                            .tile(spawn.x, spawn.y)
                            .map(|tile| tile.level)
                            .unwrap_or(0),
                    };
                    self.world.place(id, at);
                    self.characters.push(character);
                }
                Err(_) => self.emit_failure("bad_encounter_record", &spawn.character),
            }
        }
        self.emit(Json::obj([
            ("event", Json::string("encounter_start")),
            ("encounter", Json::string(&record.id)),
            ("map", Json::string(&record.map)),
        ]));
        Ok(())
    }

    /// The one character factory, over the command stream (`02:02.3`).
    fn create_character(&mut self, command: &Json) -> Result<(), Status> {
        let mode = command
            .get("mode")
            .and_then(Json::as_str)
            .and_then(CreationMode::from_id)
            .ok_or(Status::InvalidArgument)?;
        let id = self.next_entity;
        let side = command.get("side").and_then(Json::as_i64).unwrap_or(0) as u8;
        let name_key = command
            .get("name_key")
            .and_then(Json::as_str)
            .unwrap_or("character")
            .to_string();
        let registry = Arc::clone(&self.content);
        let mut character = match mode {
            CreationMode::Rolled => {
                // The pool must carry the record's tags, or a rolled skill
                // would be inert; resolve the instance through the registry.
                let ctx = registry.creation_context();
                let mut character = Character::rolled(&ctx, &mut self.rng, id, &name_key, side);
                for skill in &mut character.skills {
                    if let Some(resolved) = registry.skill_instance(&skill.skill) {
                        *skill = resolved;
                    }
                }
                character
            }
            CreationMode::Budgeted => {
                let mut traits = [1i32; 7];
                for trait_ in crate::l1::traits::TRAITS {
                    let value = command
                        .get("traits")
                        .and_then(|t| t.get(trait_.id()))
                        .and_then(Json::as_i64)
                        .ok_or(Status::InvalidArgument)?;
                    traits[trait_.index()] = value as i32;
                }
                let mut skills = Vec::new();
                if let Some(Json::Arr(ids)) = command.get("skills") {
                    for value in ids {
                        let skill_id = value.as_str().ok_or(Status::InvalidArgument)?;
                        skills.push(
                            registry
                                .skill_instance(skill_id)
                                .ok_or(Status::InvalidArgument)?,
                        );
                    }
                }
                let ctx = registry.creation_context();
                Character::budgeted(
                    &ctx,
                    id,
                    &name_key,
                    side,
                    traits,
                    command
                        .get("lifestyle_rv")
                        .and_then(Json::as_i64)
                        .unwrap_or(6) as i32,
                    command.get("repute").and_then(Json::as_i64).unwrap_or(0) as i32,
                    skills,
                    command_powers(command)?,
                )
                .map_err(|_| Status::InvalidArgument)?
            }
            CreationMode::Authored => {
                let record = command
                    .get("record")
                    .and_then(Json::as_str)
                    .ok_or(Status::InvalidArgument)?;
                let mut character = registry
                    .authored_character(record, id, side)
                    .map_err(|_| Status::InvalidArgument)?;
                // A command may add powers to an authored sheet, which is how
                // the editor's playtest equips a template with a power it has
                // not saved yet (`02:02.3`).
                for power in command_powers(command)? {
                    // `acquire_power_with` records as well as running the hook;
                    // `acquire_power_from` alone would run the plan and drop the
                    // power, which is exactly the bug this test caught.
                    character.acquire_power_with(&power, registry.as_ref());
                }
                character
            }
        };
        // `4c:124-131`: an origin's modifiers apply after roll or spend, and
        // after the powers are known, because `Mutant` and `Alien` change the
        // power count. The chosen trait for `Changed Human` is the player's.
        let origin_id = command
            .get("origin")
            .and_then(Json::as_str)
            .unwrap_or(&character.origin)
            .to_string();
        if !origin_id.is_empty() {
            let chosen = command
                .get("chosen_trait")
                .and_then(Json::as_str)
                .and_then(crate::l1::traits::Trait::from_id);
            match registry.origin(&origin_id).cloned() {
                Some(origin) => {
                    let pool = registry.skill_ids();
                    character.apply_origin(&origin, chosen, &mut self.rng, &pool);
                    character.origin = origin_id.clone();
                }
                None => self.emit_failure("unknown_origin", &origin_id),
            }
        }
        // The derived layers are recomputed once the full power list is known
        // (`D17`), not per acquisition.
        character.recompute_power_effects(registry.as_ref());
        self.next_entity += 1;
        // Put the character on the map. An encounter has authored spawns; an
        // exploration world places the party at the first free tile next to its
        // leader, or at the world's own arrival point for the first member — a
        // character standing at `-1,-1` is one the UI cannot draw and the rules
        // cannot move (`02:02.6`).
        if let Some(encounter) = &self.encounter {
            let record = self.content.encounter(&encounter.id).cloned();
            if let Some(record) = record {
                if let Some((x, y)) = record.party_spawns.get(self.spawn_cursor).copied() {
                    self.spawn_cursor += 1;
                    let at = Position {
                        x,
                        y,
                        z: self.world.tile(x, y).map(|tile| tile.level).unwrap_or(0),
                    };
                    self.world.place(id, at);
                }
            }
        } else if side == 0 {
            let anchor = self
                .party
                .iter()
                .find_map(|member| self.world.position(*member))
                .map(|at| (at.x, at.y))
                .unwrap_or_else(|| {
                    self.content
                        .world(&self.world_record)
                        .map(|record| record.start)
                        .unwrap_or((1, 1))
                });
            if let Some((x, y)) = self.free_tile_near(anchor) {
                let z = self.world.tile(x, y).map(|tile| tile.level).unwrap_or(0);
                self.world.place(id, Position { x, y, z });
            }
        }
        if side == 0 {
            self.party.push(id);
        }
        let mode_id = character.mode.id();
        self.emit(Json::obj([
            ("event", Json::string("character_created")),
            ("actor", Json::Num(i64::from(id))),
            ("mode", Json::string(mode_id)),
            ("name_key", Json::string(&name_key)),
        ]));
        self.characters.push(character);
        Ok(())
    }

    /// Queue an action for the panel (`02:02.5` SELECT).
    fn plan(&mut self, command: &Json) -> Result<(), Status> {
        let Some(actor) = command.get("actor").and_then(Json::as_i64) else {
            self.emit_failure("no_actor", "");
            return Err(Status::InvalidArgument);
        };
        let actor = actor as u32;
        let Some(action) = command
            .get("action")
            .and_then(|value| Action::parse(value).ok())
        else {
            self.emit_failure("bad_action", "");
            return Err(Status::InvalidArgument);
        };
        match &mut self.encounter {
            Some(encounter) if !is_finished(encounter) => {
                encounter.queue(actor, action);
                Ok(())
            }
            // "Plan in an exploration world" is the normal mistake a UI makes
            // between panels, and it says so (`01:01.12` rule 9).
            _ => {
                self.emit_failure("no_encounter", "");
                Err(Status::InvalidArgument)
            }
        }
    }

    /// Run one panel (`02:02.5`).
    fn commit_panel(&mut self) -> Result<(), Status> {
        let Some(mut encounter) = self.encounter.take() else {
            self.emit_failure("no_encounter", "");
            return Err(Status::InvalidArgument);
        };
        let mut events = Vec::new();
        {
            let mut battle = Battle {
                ladder: tables::CAMPAIGN_LADDER,
                rules: &self.content.rules,
                content: self.content.as_ref(),
                rng: &mut self.rng,
                world: &mut self.world,
                characters: &mut self.characters,
                vehicles: &mut self.vehicles,
                events: &mut events,
            };
            encounter::commit(&mut encounter, &mut battle);
        }
        self.outbox.extend(events);
        if is_finished(&encounter) {
            self.last_outcome = encounter.winner;
            self.encounter = None;
            // The campaign binding fires once, on the panel that ended the fight,
            // and the world is handed straight back (`02:02.7`).
            self.conclude_encounter();
        } else {
            self.encounter = Some(encounter);
        }
        Ok(())
    }

    /// Apply the campaign binding for the encounter that just ended (`02:02.7`).
    ///
    /// An authored encounter declares its own `on_win`/`on_loss`; a **wild** one is
    /// the world's own opposition, so its binding is the standing its faction keeps
    /// and nothing else — a fight in the street is not a scene.
    fn conclude_encounter(&mut self) {
        let winner = self.last_outcome;
        let Some(encounter_id) = self.concluded.clone().or_else(|| self.encounter_id()) else {
            return;
        };
        let won = winner == Some(0);
        let effects = self
            .content
            .encounter(&encounter_id)
            .map(|record| {
                if won {
                    record.on_win.clone()
                } else {
                    record.on_loss.clone()
                }
            })
            .unwrap_or_default();
        self.emit(Json::obj([
            ("event", Json::string("encounter_resolved")),
            ("encounter", Json::string(&encounter_id)),
            (
                "winner",
                match winner {
                    Some(side) => Json::Num(i64::from(side)),
                    None => Json::Null,
                },
            ),
        ]));
        if effects.is_empty() {
            self.run_quests();
            return;
        }
        let scale = self.content.rules.impact.clone();
        let defaults = self.content.standing_defaults();
        let defaults = |faction: &str| defaults.get(faction).copied().unwrap_or(0);
        let actor = self.party.first().copied();
        {
            let character =
                actor.and_then(|id| self.characters.iter_mut().find(|entry| entry.id == id));
            let applied = self.journal.apply(
                &effects,
                character,
                &mut self.flags,
                &mut self.standings,
                &scale,
                &defaults,
            );
            self.emit_journal_events(&applied);
        }
        self.run_quests();
    }

    /// The id of the encounter the sim is holding, if any.
    fn encounter_id(&self) -> Option<String> {
        self.encounter
            .as_ref()
            .map(|encounter| encounter.id.clone())
    }

    /// Spend a panel's aid to stabilise a dying character (`4c:1161`).
    fn stabilize(&mut self, command: &Json) -> Result<(), Status> {
        let target = command
            .get("target")
            .and_then(Json::as_i64)
            .ok_or(Status::InvalidArgument)? as u32;
        let Some(character) = self
            .characters
            .iter_mut()
            .find(|character| character.id == target)
        else {
            self.emit_failure("no_such_character", "");
            return Err(Status::InvalidArgument);
        };
        let removed = status::remove(&mut character.conditions, ConditionKind::Dying);
        character.dying_steps = 0;
        character.dead = false;
        self.emit(Json::obj([
            ("event", Json::string("stabilized")),
            ("actor", Json::Num(i64::from(target))),
            ("was_dying", Json::Bool(removed)),
        ]));
        Ok(())
    }

    /// Put an authored vehicle on the map under an operator (`4c:1300-1365`).
    fn spawn_vehicle(&mut self, command: &Json) -> Result<(), Status> {
        let record_id = command
            .get("record")
            .and_then(Json::as_str)
            .ok_or(Status::InvalidArgument)?;
        let Some(record) = self.content.vehicle(record_id).cloned() else {
            self.emit_failure("unknown_vehicle", record_id);
            return Err(Status::InvalidArgument);
        };
        let x = command.get("x").and_then(Json::as_i64).unwrap_or(0) as i32;
        let y = command.get("y").and_then(Json::as_i64).unwrap_or(0) as i32;
        let operator = command.get("operator").and_then(Json::as_i64).unwrap_or(0) as u32;
        let id = self.next_entity;
        self.next_entity += 1;
        let mut vehicle = record;
        vehicle.id = id;
        vehicle.operator = operator;
        self.world.place(
            id,
            Position {
                x,
                y,
                z: self.world.tile(x, y).map(|tile| tile.level).unwrap_or(0),
            },
        );
        self.vehicles.push(vehicle);
        self.emit(Json::obj([
            ("event", Json::string("vehicle_spawned")),
            ("actor", Json::Num(i64::from(id))),
            ("record", Json::string(record_id)),
        ]));
        Ok(())
    }

    /// A prediction of an action, with no state change and no RNG draw
    /// (`02:02.10`).
    ///
    /// An attack runs the **same** [`crate::l2::encounter::plan_attack`] the
    /// resolver runs; a dodge runs the same row-step arithmetic
    /// [`resolve_dodge`](crate::l2::encounter) uses. Then the roll is
    /// *enumerated* instead of drawn, so the UI's odds and the editor's
    /// playtest consequence cannot disagree with the real resolution. That
    /// sharing is why the plan is a separate function.
    pub fn preview(&self, command: &Json) -> Json {
        let actor = command.get("actor").and_then(Json::as_i64).unwrap_or(0) as u32;
        let Some(action) = command
            .get("action")
            .and_then(|value| Action::parse(value).ok())
        else {
            return Json::obj([("error", Json::string("bad_action"))]);
        };
        let ladder = tables::CAMPAIGN_LADDER;
        let Some(character) = self
            .characters
            .iter()
            .find(|character| character.id == actor)
        else {
            return Json::obj([("error", Json::string("no_such_actor"))]);
        };
        if !action.kind.is_roll() {
            return Json::obj([
                ("actor", Json::Num(i64::from(actor))),
                ("kind", Json::string(action.kind.id())),
                ("roll", Json::Bool(false)),
            ]);
        }

        let is_attack = matches!(
            action.kind,
            ActionKind::MeleeBash | ActionKind::MeleeSlash | ActionKind::Ranged
        );
        let plan = if is_attack {
            let plan = encounter::plan_attack(
                ladder,
                &self.content.rules,
                self.content.as_ref(),
                &self.world,
                &self.characters,
                actor,
                &action,
            );
            if let Some(reason) = plan.refusal {
                return Json::obj([
                    ("actor", Json::Num(i64::from(actor))),
                    ("kind", Json::string(action.kind.id())),
                    ("refused", Json::string(reason)),
                ]);
            }
            plan
        } else {
            // Dodge: a Coordination roll, plus the skill bonus and the
            // movement penalty, with no target (4c:1044-1053).
            let mut plan = encounter::plan_attack(
                ladder,
                &self.content.rules,
                self.content.as_ref(),
                &self.world,
                &self.characters,
                actor,
                &Action::simple(action.kind),
            );
            plan.refusal = None;
            plan.trait_ = Trait::Coordination;
            plan.base_band = character.effective(ladder).trait_band(Trait::Coordination);
            plan.steps = character.skill_steps(action.kind.tag())
                + self
                    .content
                    .rules
                    .dodge_penalty
                    .steps_for(character.moved_tiles);
            plan.roll_penalty = character.roll_penalty();
            plan.demote = Vec::new();
            plan.punch_cap = action.declared.punch_cap;
            plan.piercing = false;
            plan.target = 0;
            plan.distance = 0;
            plan.reach = 0;
            plan.cover_steps = 0;
            plan.elevation_steps = 0;
            plan
        };

        // The Fortune window is declared with the action, exactly as the
        // resolver consumes it; an unaffordable declaration is reported rather
        // than silently ignored.
        let affordable = action.fortune <= character.fortune;
        let fortune_steps = if affordable { action.fortune / 25 } else { 0 };
        let mut odds = [0i64; 4];
        for roll in 0..100u8 {
            let adjusted = (i32::from(roll) + plan.roll_penalty).clamp(0, 99) as u8;
            let Some(rolled) = ladder.resolve(ladder.bands[plan.base_band].lo, adjusted) else {
                continue;
            };
            let mut colour = rolled.shift(fortune_steps);
            if let Some(cap) = plan.punch_cap {
                if colour > cap {
                    colour = cap;
                }
            }
            for demoted in &plan.demote {
                if colour == *demoted {
                    colour = colour.shift(-1);
                }
            }
            odds[colour.code() as usize] += 1;
        }

        // The reasons, so a tooltip can say *why* the odds are what they are
        // (`02:02.11` rule 2: a disputed rule is never silent).
        let mut notes: Vec<Json> = Vec::new();
        let skill = character.skill_steps(action.kind.tag());
        if skill != 0 {
            notes.push(Json::string(format!("skill {skill:+}")));
        }
        let moved = self
            .content
            .rules
            .attack_penalty
            .steps_for(character.moved_tiles);
        if moved != 0 {
            notes.push(Json::string(format!("moved {moved:+}")));
        }
        let target_dodge = self
            .characters
            .iter()
            .find(|other| other.id == plan.target)
            .map(|other| other.dodge_steps)
            .unwrap_or(0);
        if target_dodge != 0 {
            notes.push(Json::string(format!("target dodge {target_dodge:+}")));
        }
        if plan.elevation_steps != 0 {
            notes.push(Json::string(format!(
                "elevation {:+}",
                plan.elevation_steps
            )));
        }
        if plan.cover_steps != 0 {
            notes.push(Json::string(format!("cover {:+}", plan.cover_steps)));
        }
        if plan.roll_penalty != 0 {
            notes.push(Json::string(format!(
                "condition {:+} on d%",
                plan.roll_penalty
            )));
        }
        if plan.piercing {
            notes.push(Json::string("armour piercing"));
        }
        if !affordable {
            notes.push(Json::string("fortune is not affordable"));
        }

        Json::obj([
            ("actor", Json::Num(i64::from(actor))),
            ("kind", Json::string(action.kind.id())),
            ("target", Json::Num(i64::from(plan.target))),
            ("trait", Json::string(plan.trait_.id())),
            ("band", Json::Num(plan.base_band as i64)),
            ("steps", Json::Num(i64::from(plan.steps))),
            ("distance", Json::Num(i64::from(plan.distance))),
            ("reach", Json::Num(i64::from(plan.reach))),
            ("roll_penalty", Json::Num(i64::from(plan.roll_penalty))),
            ("fortune_steps", Json::Num(i64::from(fortune_steps))),
            ("notes", Json::Arr(notes)),
            (
                "odds",
                Json::obj([
                    ("Black", Json::Num(odds[0])),
                    ("Red", Json::Num(odds[1])),
                    ("Blue", Json::Num(odds[2])),
                    ("Yellow", Json::Num(odds[3])),
                ]),
            ),
        ])
    }

    /// The journal as the shell renders it (`02:02.7`, `AD-22`).
    ///
    /// A projection of state the sim already owns: the quest pages with their
    /// status and the lines' **string ids**, which the shell resolves through the
    /// VFS so a translation mod can replace them (`AD-22`).
    pub fn journal_view(&self) -> Json {
        let mut journal = self.journal.to_json();
        if let Json::Obj(fields) = &mut journal {
            fields.push((
                "clock".to_string(),
                Json::obj([
                    ("day", Json::Num(i64::from(self.clock.day))),
                    ("panel", Json::Num(i64::from(self.clock.panel))),
                    ("slot", Json::string(self.clock.slot().id())),
                    (
                        "minute_of_day",
                        Json::Num(i64::from(self.clock.minute_of_day())),
                    ),
                ]),
            ));
            fields.push(("standings".to_string(), self.standings.to_json()));
        }
        journal
    }

    /// The open conversation, or a document saying there is none (`02:02.7`).
    ///
    /// The options are split into **offered** and **withheld** rather than
    /// pre-filtered: a check makes an option harder, not invisible, and a `when`
    /// that does not hold is the story not having got there yet. The shell shows
    /// both, which is what makes a rule the player can learn (`02:02.11` rule 2).
    pub fn dialogue_view(&self) -> Json {
        let Some((partner, dialogue_id)) = &self.talking else {
            return Json::obj([("open", Json::Bool(false))]);
        };
        let Some(dialogue) = self.content.dialogue(dialogue_id) else {
            return Json::obj([("open", Json::Bool(false))]);
        };
        let Some(node) = dialogue.node(&self.dialogue_node) else {
            return Json::obj([("open", Json::Bool(false))]);
        };
        let item_ids = self.content.item_ids();
        let (offered, withheld) = {
            let actor = self
                .characters
                .iter()
                .find(|entry| entry.id == *partner)
                .or_else(|| {
                    self.party
                        .first()
                        .and_then(|id| self.characters.iter().find(|entry| entry.id == *id))
                });
            let context = EvaluationContext {
                actor,
                flags: &self.flags,
                standings: &self.standings,
                journal: &self.journal,
                clock: &self.clock,
                factions: &self.content.factions,
                item_ids: &item_ids,
            };
            crate::l4::dialogue::available_options(node, &context)
        };
        let option_json = |view: &crate::l4::dialogue::OptionView| {
            let mut object = Json::object();
            object.set("id", Json::string(&view.id));
            object.set("text_key", Json::string(&view.text_key));
            object.set("offered", Json::Bool(view.offered));
            if let Some(required) = view.required {
                object.set(
                    "required",
                    Json::string(crate::l2::actions::colour_name(required)),
                );
            }
            if let Some(tag) = view.tag {
                object.set("tag", Json::string(tag.id()));
            }
            // An invited fight is labelled so the UI can mark the option as the
            // one that starts something, rather than looking like a polite reply
            // (`02:02.13`).
            if let Some(crate::l4::dialogue::DialogueAction::Fight { .. }) = &view.action {
                object.set("action", Json::string("fight"));
            }
            object
        };
        Json::obj([
            ("open", Json::Bool(true)),
            ("dialogue", Json::string(&dialogue.id)),
            ("node", Json::string(&node.id)),
            ("text_key", Json::string(&node.text_key)),
            ("partner", Json::Num(i64::from(*partner))),
            ("options", Json::arr(offered.iter().map(option_json))),
            ("withheld", Json::arr(withheld.iter().map(option_json))),
        ])
    }

    /// The world as the shell and the editor read it (`02:02.6`).
    ///
    /// The party's positions, the stations, the clock and the terrain summary — no
    /// tiles, because a caller that wants the map wants the map record, and one
    /// that wants to draw wants the packet.
    pub fn world_view(&self) -> Json {
        let stations: Vec<Json> = self
            .stations
            .iter()
            .map(|station| {
                let at = self.world.position(station.id);
                let known = self
                    .characters
                    .iter()
                    .find(|character| character.id == station.id)
                    .map(|character| character.name_key.clone())
                    .unwrap_or_default();
                Json::obj([
                    ("id", Json::Num(i64::from(station.id))),
                    ("name_key", Json::string(&known)),
                    ("faction", Json::string(&station.faction)),
                    ("behaviour", Json::string(station.behaviour.id())),
                    (
                        "home",
                        Json::arr([
                            Json::Num(i64::from(station.home.0)),
                            Json::Num(i64::from(station.home.1)),
                        ]),
                    ),
                    ("x", Json::Num(at.map(|p| i64::from(p.x)).unwrap_or(-1))),
                    ("y", Json::Num(at.map(|p| i64::from(p.y)).unwrap_or(-1))),
                    (
                        "post",
                        match station.schedule.post_at(self.clock.slot()) {
                            Some((x, y)) => {
                                Json::arr([Json::Num(i64::from(x)), Json::Num(i64::from(y))])
                            }
                            None => Json::Null,
                        },
                    ),
                ])
            })
            .collect();
        let party: Vec<Json> = self
            .party
            .iter()
            .map(|id| {
                let at = self.world.position(*id);
                let name_key = self
                    .characters
                    .iter()
                    .find(|character| character.id == *id)
                    .map(|character| character.name_key.clone())
                    .unwrap_or_default();
                Json::obj([
                    ("id", Json::Num(i64::from(*id))),
                    ("name_key", Json::string(&name_key)),
                    ("x", Json::Num(at.map(|p| i64::from(p.x)).unwrap_or(-1))),
                    ("y", Json::Num(at.map(|p| i64::from(p.y)).unwrap_or(-1))),
                ])
            })
            .collect();
        Json::obj([
            ("record", Json::string(&self.world_record)),
            (
                "generator_version",
                Json::Num(i64::from(self.generator_version)),
            ),
            ("width", Json::Num(i64::from(self.world.width))),
            ("height", Json::Num(i64::from(self.world.height))),
            ("clock", self.clock.to_json()),
            ("party", Json::Arr(party)),
            ("stations", Json::Arr(stations)),
            ("standings", self.standings.to_json()),
            (
                "delta_sectors",
                Json::Num(self.baseline.delta(&self.world).len() as i64),
            ),
        ])
    }

    /// The merchant counters and what this character would pay (`4c:1255-1272`).
    pub fn shops_view(&self) -> Json {
        let actor = self
            .party
            .first()
            .and_then(|id| self.characters.iter().find(|entry| entry.id == *id));
        let prices = &self.content.rules.prices;
        // A counter is where its merchant stands (`02:02.13`): the trade surface
        // is offered beside the person who keeps it, so the Shop panel and the
        // "spend Fortune" action cannot disagree about whether a merchant is here.
        let reach = self
            .party
            .first()
            .map(|leader| self.stations_in_reach(*leader))
            .unwrap_or_default();
        let mut out = Vec::new();
        for shop in &self.content.shops {
            let kept_here = reach.iter().any(|id| {
                self.station_record(*id)
                    .is_some_and(|record| record.shop == shop.id)
            });
            if !kept_here {
                continue;
            }
            let mut stock = Vec::new();
            for item_id in &shop.stock {
                let Some(item) = self.content.item(item_id) else {
                    continue;
                };
                let lifestyle = actor.map(|character| character.lifestyle_rv).unwrap_or(1);
                let price = prices.price_for(item, lifestyle);
                stock.push(Json::obj([
                    ("item", Json::string(&item.id)),
                    ("name_key", Json::string(&item.name_key)),
                    ("kind", Json::string(item.kind.id())),
                    ("price", Json::Num(i64::from(price))),
                    (
                        "affordable",
                        Json::Bool(
                            actor
                                .map(|character| character.fortune >= price)
                                .unwrap_or(false),
                        ),
                    ),
                ]));
            }
            out.push(Json::obj([
                ("shop", Json::string(&shop.id)),
                ("name_key", Json::string(&shop.name_key)),
                ("stock", Json::Arr(stock)),
            ]));
        }
        let inventory: Vec<Json> = actor
            .map(|character| {
                character
                    .items
                    .iter()
                    .map(|id| {
                        self.content
                            .item(id)
                            .map(|item| {
                                Json::obj([
                                    ("item", Json::string(&item.id)),
                                    ("name_key", Json::string(&item.name_key)),
                                    ("kind", Json::string(item.kind.id())),
                                    ("slot", Json::string(item.slot.id())),
                                    (
                                        "equipped",
                                        Json::Bool(
                                            character.weapon.as_deref() == Some(item.id.as_str())
                                                || character.armour.contains(&item.id),
                                        ),
                                    ),
                                ])
                            })
                            .unwrap_or(Json::obj([("item", Json::string(id))]))
                    })
                    .collect()
            })
            .unwrap_or_default();
        Json::obj([
            ("shops", Json::Arr(out)),
            ("inventory", Json::Arr(inventory)),
            (
                "fortune",
                Json::Num(i64::from(
                    actor.map(|character| character.fortune).unwrap_or(0),
                )),
            ),
            (
                "carry_slots",
                Json::Num(i64::from(self.content.rules.carry_slots)),
            ),
        ])
    }

    /// The content record behind a placed station (`02:02.6`).
    ///
    /// The same lookup `talk` uses to find a station's dialogue, so the action
    /// list and the command agree about what a station offers.
    fn station_record(&self, id: u32) -> Option<&crate::content::StationRecord> {
        let sheet = self.character_sheet(id);
        self.content.world(&self.world_record).and_then(|record| {
            record
                .stations
                .iter()
                .find(|candidate| candidate.character == sheet)
        })
    }

    /// The stations standing within reach of an actor, nearest first (`02:02.13`).
    fn stations_in_reach(&self, actor: u32) -> Vec<u32> {
        let Some(from) = self.world.position(actor) else {
            return Vec::new();
        };
        let mut near: Vec<(i32, u32)> = self
            .stations
            .iter()
            .filter_map(|station| {
                let at = self.world.position(station.id)?;
                let distance = crate::l3::world::World::distance(from, at);
                (distance <= 1).then_some((distance, station.id))
            })
            .collect();
        near.sort_unstable();
        near.into_iter().map(|(_, id)| id).collect()
    }

    /// Whether an actor could buy a Rank Value with the Fortune in hand
    /// (`4c:1381-1391`).
    ///
    /// `+1 RV costs the current value`, so the cheapest purchase is the lowest
    /// stored trait; a character who cannot afford their cheapest trait cannot
    /// afford anything, and the action says so rather than opening a panel with
    /// nothing to do in it.
    fn can_advance(&self, actor: u32) -> bool {
        let Some(character) = self.characters.iter().find(|entry| entry.id == actor) else {
            return false;
        };
        let advancement = &self.content.rules.advancement;
        crate::l1::traits::TRAITS.iter().any(|trait_| {
            let current = character.stored(*trait_);
            let at_ceiling =
                advancement.max_rank_value > 0 && current + 1 > advancement.max_rank_value;
            !at_ceiling
                && progression::trait_step_cost(advancement, current, 1) <= character.fortune
        })
    }

    /// The actions the party may take here, and why (`02:02.13`).
    ///
    /// The engine answers this, not the shell: whether Wait is legal in a fight
    /// and whether Talk has anyone to talk to are rules, and a view that guessed
    /// them would be a second, quieter rules engine. Every entry is present with
    /// an `enabled` flag and a `reason_key`, so a disabled action is a rule the
    /// player can read rather than a control that vanished (`02:02.11` rule 2).
    pub fn actions_view(&self) -> Json {
        let in_encounter = self.encounter_running();
        let in_dialogue = self.talking.is_some();
        let leader = self.party.first().copied();
        let reach = leader
            .map(|id| self.stations_in_reach(id))
            .unwrap_or_default();
        let partner = reach.iter().copied().find(|id| {
            self.station_record(*id)
                .is_some_and(|record| !record.dialogue.is_empty())
        });
        let merchant = reach.iter().copied().find(|id| {
            self.station_record(*id)
                .is_some_and(|record| !record.shop.is_empty())
        });
        let self_dialogue = self
            .content
            .world(&self.world_record)
            .is_some_and(|record| !record.self_dialogue.is_empty());

        let exploring = leader.is_some() && !in_encounter && !in_dialogue;
        // Why nothing at all is legal, when that is the answer. A dialogue is
        // modal (`02:02.13`), so it blocks the whole exploration set.
        let blocked = if in_encounter {
            Some("in_encounter")
        } else if in_dialogue {
            Some("in_dialogue")
        } else if leader.is_none() {
            Some("no_party")
        } else {
            None
        };

        let talk = exploring && (partner.is_some() || self_dialogue);
        let advanceable = leader.is_some_and(|id| self.can_advance(id));
        let spend = exploring && (merchant.is_some() || advanceable);
        // A paused quest step is the one manual step in the runtime (`02:02.7`),
        // so releasing it is an action like any other rather than a hidden
        // meaning for some other control.
        let paused = self.journal.pauses.first().map(|(quest, _)| quest.clone());
        let can_continue = exploring && paused.is_some();
        let paused_name = paused
            .as_deref()
            .and_then(|id| self.content.quest(id))
            .map(|quest| quest.name_key.clone())
            .unwrap_or_default();
        // Fortune is both the currency and the advancement cost (`4c:1381`), so
        // the surface is whatever the context actually offers: a counter when a
        // merchant is here, the character sheet when only a trait is affordable.
        let spend_surface = if merchant.is_some() {
            "trade"
        } else {
            "advancement"
        };
        let reason = |legal: bool, otherwise: &'static str| -> Json {
            // The disabled reason is a string id like every other player-readable
            // line (`AD-22`), so a translation mod can reach it.
            Json::string(if legal {
                String::new()
            } else {
                format!("reason.{}", blocked.unwrap_or(otherwise))
            })
        };
        // The self-dialogue's partner is the leader, matching what `talk` does.
        let talk_target = partner.or(leader);

        Json::obj([(
            "actions",
            Json::arr([
                Json::obj([
                    ("action", Json::string("talk")),
                    ("label_key", Json::string("action.talk")),
                    ("enabled", Json::Bool(talk)),
                    ("reason_key", reason(talk, "no_partner")),
                    (
                        "target",
                        talk_target
                            .map(|id| Json::Num(i64::from(id)))
                            .unwrap_or(Json::Null),
                    ),
                    // Whether the target is the speaker rather than a station, so
                    // the shell sends `talk` with no partner and lets the engine
                    // open the campaign's self-dialogue (`02:02.13`).
                    ("self", Json::Bool(talk && partner.is_none())),
                ]),
                Json::obj([
                    ("action", Json::string("wait")),
                    ("label_key", Json::string("action.wait")),
                    ("enabled", Json::Bool(exploring)),
                    ("reason_key", reason(exploring, "no_party")),
                ]),
                Json::obj([
                    ("action", Json::string("fight")),
                    ("label_key", Json::string("action.fight")),
                    ("enabled", Json::Bool(exploring)),
                    ("reason_key", reason(exploring, "no_party")),
                ]),
                Json::obj([
                    ("action", Json::string("spend_fortune")),
                    ("label_key", Json::string("action.spend_fortune")),
                    ("enabled", Json::Bool(spend)),
                    ("reason_key", reason(spend, "nothing_to_spend")),
                    ("surface", Json::string(spend_surface)),
                ]),
                Json::obj([
                    ("action", Json::string("continue_quest")),
                    ("label_key", Json::string("action.continue_quest")),
                    ("enabled", Json::Bool(can_continue)),
                    ("reason_key", reason(can_continue, "no_pause")),
                    (
                        "quest",
                        match &paused {
                            Some(id) if can_continue => Json::string(id),
                            _ => Json::Null,
                        },
                    ),
                    ("name_key", Json::string(&paused_name)),
                ]),
            ]),
        )])
    }

    /// The declared display gates, as their engine-computed truth values
    /// (`09:09.4`).
    ///
    /// The shell gates a node on a boolean and never evaluates a predicate: the
    /// value here is the engine's evaluation of the signal's condition against
    /// the live campaign, through the same evaluator a quest or a dialogue option
    /// uses, so a HUD cannot disagree with a quest about what a condition means.
    pub fn signals_view(&self) -> Json {
        let item_ids = self.content.item_ids();
        let actor = self
            .party
            .first()
            .and_then(|id| self.characters.iter().find(|entry| entry.id == *id));
        let context = EvaluationContext {
            actor,
            flags: &self.flags,
            standings: &self.standings,
            journal: &self.journal,
            clock: &self.clock,
            factions: &self.content.factions,
            item_ids: &item_ids,
        };
        let mut out = Json::object();
        for signal in &self.content.signals {
            out.set(
                &signal.id,
                Json::Bool(dialogue::evaluate(&signal.when, &context)),
            );
        }
        out
    }

    /// The roster: one entry per character, for the shell's panel.
    ///
    /// A read-only projection of state the sim already owns, so the UI never has
    /// to parse a save to know who is standing. Names are string ids, never
    /// English (`AD-22`).
    pub fn roster(&self) -> Json {
        let mut out = Vec::new();
        for character in &self.characters {
            let at = self.world.position(character.id);
            let item = self.content.item(character.weapon.as_deref().unwrap_or(""));
            out.push(Json::obj([
                ("id", Json::Num(i64::from(character.id))),
                ("side", Json::Num(i64::from(character.side))),
                ("name_key", Json::string(&character.name_key)),
                (
                    "weapon",
                    match &character.weapon {
                        Some(id) => Json::string(id),
                        None => Json::Null,
                    },
                ),
                (
                    "weapon_hands",
                    item.map(|item| {
                        Json::string(match item.hands {
                            crate::l1::items::Hands::None => "none",
                            crate::l1::items::Hands::One => "one",
                            crate::l1::items::Hands::Two => "two",
                            crate::l1::items::Hands::Ranged => "ranged",
                        })
                    })
                    .unwrap_or(Json::Null),
                ),
                (
                    "weapon_melee",
                    item.and_then(|item| item.melee)
                        .map(|kind| {
                            Json::string(match kind {
                                crate::l1::items::MeleeKind::Bashing => "bashing",
                                crate::l1::items::MeleeKind::Slashing => "slashing",
                            })
                        })
                        .unwrap_or(Json::Null),
                ),
                (
                    "weapon_reach",
                    item.map(|item| {
                        Json::Num(i64::from(
                            item.reach.tiles(
                                tables::CAMPAIGN_LADDER,
                                character
                                    .effective(tables::CAMPAIGN_LADDER)
                                    .trait_rv(crate::l1::traits::Trait::Coordination),
                            ),
                        ))
                    })
                    .unwrap_or(Json::Num(1)),
                ),
                ("dead", Json::Bool(character.dead)),
                ("damage", Json::Num(i64::from(character.damage))),
                ("max_damage", Json::Num(i64::from(character.max_damage))),
                ("fortune", Json::Num(i64::from(character.fortune))),
                ("repute", Json::Num(i64::from(character.repute))),
                ("x", Json::Num(at.map(|p| i64::from(p.x)).unwrap_or(-1))),
                ("y", Json::Num(at.map(|p| i64::from(p.y)).unwrap_or(-1))),
                (
                    "conditions",
                    Json::arr(character.conditions.iter().map(|condition| {
                        Json::obj([
                            ("kind", Json::string(condition.kind.id())),
                            ("panels", Json::Num(i64::from(condition.panels))),
                        ])
                    })),
                ),
                (
                    "traits",
                    Json::obj(crate::l1::traits::TRAITS.map(|trait_| {
                        (trait_.id(), Json::Num(i64::from(character.stored(trait_))))
                    })),
                ),
                // What one row step on each trait would cost, and whether the
                // character can afford it (`4c:1381-1391`). The *curve* is
                // content, so the shell cannot compute a price without copying a
                // rule; the engine quotes it instead (`02:02.13`).
                (
                    "advancement",
                    Json::arr(crate::l1::traits::TRAITS.iter().map(|trait_| {
                        let advancement = &self.content.rules.advancement;
                        let current = character.stored(*trait_);
                        let cost = progression::trait_step_cost(advancement, current, 1);
                        let at_ceiling = advancement.max_rank_value > 0
                            && current + 1 > advancement.max_rank_value;
                        Json::obj([
                            ("trait", Json::string(trait_.id())),
                            ("cost", Json::Num(i64::from(cost))),
                            ("at_ceiling", Json::Bool(at_ceiling)),
                            (
                                "affordable",
                                Json::Bool(!at_ceiling && cost <= character.fortune),
                            ),
                        ])
                    })),
                ),
            ]));
        }
        Json::Arr(out)
    }
}

/// Turn a batch of events into the lines a player reads (`AD-22`, `02:02.7`).
///
/// Every event the core emits names its own kinds, and every kind has a stable
/// string id in the campaign's string table. Keeping the mapping here rather than
/// in the shell is what lets a mod translate the game with a string-table
/// override and no code (`AD-22`), and what keeps the shell free of rules.
///
/// The answer is a list of `{event, text_key, args}`. An event kind this function
/// does not know is passed through with an empty `text_key` and its own fields
/// under `args`, so a newer core against an older shell still says something
/// truthful rather than nothing.
pub fn narrate(events: &Json) -> Json {
    let Json::Arr(items) = events else {
        return Json::Arr(Vec::new());
    };
    let mut out = Vec::new();
    for event in items {
        let kind = event.get("event").and_then(Json::as_str).unwrap_or("");
        let mut object = Json::object();
        object.set("event", Json::string(kind));
        object.set("text_key", Json::string(narration_key(kind)));
        let mut args = Json::object();
        for field in [
            "actor",
            "target",
            "partner",
            "item",
            "quest",
            "step",
            "status",
            "trait",
            "rv",
            "cost",
            "price",
            "shop",
            "station",
            "x",
            "y",
            "day",
            "panel",
            "slot",
            "colour",
            "required",
            "option",
            "mode",
            "name_key",
            "text_key",
            "record",
            "what",
            "reason",
            "detail",
            "intent",
            "encounter",
            "map",
            "width",
            "height",
            "stations",
            "hostiles",
            "was_dying",
            "value",
            "key",
        ] {
            if let Some(value) = event.get(field) {
                args.set(field, value.clone());
            }
        }
        object.set("args", args);
        out.push(object);
    }
    Json::Arr(out)
}

/// The string id a narration line uses for an event kind.
///
/// A table rather than a `format!`, so the ids are greppable: an author writing a
/// string table can find every line the engine can ask for, and a missing one is
/// visible as a gap rather than a runtime surprise.
pub fn narration_key(kind: &str) -> &'static str {
    match kind {
        "character_created" => "log.character_created",
        "encounter_start" => "log.encounter_start",
        "moved" => "log.moved",
        "blocked" => "log.blocked",
        "time" => "log.time",
        "station_moved" => "log.station_moved",
        "hostile_contact" => "log.hostile_contact",
        "dialogue_open" => "log.dialogue_open",
        "dialogue_choice" => "log.dialogue_choice",
        "dialogue_close" => "log.dialogue_close",
        "check" => "log.check",
        "quest" => "log.quest",
        "quest_step" => "log.quest_step",
        "quest_step_released" => "log.quest_step_released",
        "journal" => "log.journal",
        "campaign" => "log.campaign",
        "item_given" => "log.item_given",
        "item_taken" => "log.item_taken",
        "equipped" => "log.equipped",
        "bought" => "log.bought",
        "sold" => "log.sold",
        "advanced" => "log.advanced",
        "party_joined" => "log.party_joined",
        "party_left" => "log.party_left",
        "world_generated" => "log.world_generated",
        "vehicle_spawned" => "log.vehicle_spawned",
        "flag" => "log.flag",
        "stabilized" => "log.stabilized",
        "failed" => "log.failed",
        "encounter_resolved" => "log.encounter_resolved",
        "attack" => "log.attack",
        "nail" => "log.nail",
        "absorb" => "log.absorb",
        "resist" => "log.resist",
        "initiative" => "log.initiative",
        "damage" => "log.damage",
        "condition" => "log.condition",
        "condition_end" => "log.condition_end",
        "death" => "log.death",
        "heal" => "log.heal",
        "dodge" => "log.dodge",
        "move" => "log.move",
        "stand" => "log.stand",
        "wait" => "log.wait",
        "forfeit" => "log.forfeit",
        "panel_end" => "log.panel_end",
        "encounter_end" => "log.encounter_end",
        "refused" => "log.refused",
        "trait_boost" => "log.trait_boost",
        "boost_end" => "log.boost_end",
        "charged" => "log.charged",
        "regenerate" => "log.regenerate",
        "teleport" => "log.teleport",
        "displaced" => "log.displaced",
        "collision" => "log.collision",
        "crash" => "log.crash",
        "ram_avoided" => "log.ram_avoided",
        "manoeuvre" => "log.manoeuvre",
        "passenger_damage" => "log.passenger_damage",
        "power_damage" => "log.power_damage",
        "power_negated" => "log.power_negated",
        "vehicle_destroyed" => "log.vehicle_destroyed",
        _ => "",
    }
}

/// The reason the last refused command gave, as a string id (`AD-22`).
static LAST_FAILURE_REASON: Mutex<String> = Mutex::new(String::new());

/// Remember why a command was refused.
fn record_failure_reason(reason: &str) {
    if let Ok(mut slot) = LAST_FAILURE_REASON.lock() {
        *slot = reason.to_string();
    }
}

/// Forget the last reason.
///
/// Called at the *start* of every command, so the reason a caller reads belongs to
/// the command that failed rather than to whichever one failed last. A reason that
/// outlives its command is worse than no reason: it is confidently wrong, and the
/// diagnostic names something the player did not do.
fn clear_failure_reason() {
    if let Ok(mut slot) = LAST_FAILURE_REASON.lock() {
        slot.clear();
    }
}

/// The reason the last refused command gave, or `""` when none was recorded.
pub fn last_failure_reason() -> String {
    LAST_FAILURE_REASON
        .lock()
        .map(|slot| slot.clone())
        .unwrap_or_default()
}

/// The powers a `create_character` command declares (`02:02.4`).
fn command_powers(command: &Json) -> Result<Vec<crate::l1::powers::PowerInst>, Status> {
    match command.get("powers") {
        None => Ok(Vec::new()),
        Some(Json::Arr(items)) => {
            let mut out = Vec::new();
            for item in items {
                out.push(
                    crate::l1::powers::parse_power(item).map_err(|_| Status::InvalidArgument)?,
                );
            }
            Ok(out)
        }
        Some(_) => Err(Status::InvalidArgument),
    }
}

/// The baseline generator for an authored map (`02:02.6`).
///
/// A map with no world record is authored outright, so the "generator" recorded in
/// the save is only an identity: the size, the seed, and terrain densities of zero.
/// That is what makes the delta of an authored world empty and its round trip
/// exact, and it is what [`Baseline::authored`] documents.
pub fn world_baseline_generator(world: &World, seed: u32) -> crate::l3::gen::Generator {
    crate::l3::gen::Generator {
        width: world.width,
        height: world.height,
        seed,
        rise_percent: 0,
        water_percent: 0,
        cover_percent: 0,
        feature_tiles: 8,
    }
}

/// Report a warning the player should see.
///
/// The engine never formats a message for a player and never includes a path
/// (`01:01.12` rule 2); it goes to the host's diagnostic sink
/// ([`crate::abi::set_log_sink`]), which decides what to do with it. A host that
/// installs no sink gets silence — the load path is identical either way, only
/// the reporting differs.
fn log_warn(message: &str) {
    crate::abi::warn(message);
}

/// `asset_id` 0 is the built-in untextured quad.
pub const ASSET_BUILTIN_QUAD: u32 = 0;

/// The most ticks one `kobra_tick` call may advance.
pub const MAX_TICKS_PER_CALL: u32 = 600;

/// The most panels one `advance` command may skip.
///
/// A bound rather than a policy: a bulk advance is how a client skips a quiet
/// stretch without one message per panel, and it must not become a way to run an
/// unbounded amount of work in one call. A full day is 1440 panels, so this
/// allows a whole day in one command and refuses more.
pub const MAX_PANELS_PER_CALL: u32 = 1440;

/// FNV-1a, the hash the determinism oracle and the content identity both use.
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
// Engine and handle table
// ---------------------------------------------------------------------------

/// Initialise the engine: record the seed and select the compiled ladder.
pub fn init(seed: u32) -> Result<(), Status> {
    let mut engine = ENGINE.lock().map_err(|_| Status::Internal)?;
    engine.seed = seed;
    engine.ladder = tables::CAMPAIGN_LADDER;
    engine.ready = true;
    packet::ring().init();
    Ok(())
}

/// Whether `kobra_init` has run.
pub fn is_ready() -> bool {
    ENGINE.lock().map(|engine| engine.ready).unwrap_or(false)
}

/// The ladder the running engine uses.
pub fn ladder() -> &'static Ladder {
    match ENGINE.lock() {
        Ok(engine) => engine.ladder,
        Err(poisoned) => poisoned.into_inner().ladder,
    }
}

/// The seed `kobra_init` recorded.
pub fn seed() -> u32 {
    match ENGINE.lock() {
        Ok(engine) => engine.seed,
        Err(poisoned) => poisoned.into_inner().seed,
    }
}

/// Merge and install a content document; returns the load report (`03:03.8`).
pub fn load_content(document: &Json) -> Json {
    let loaded = content::load(document);
    let report = loaded.report.clone();
    match ENGINE.lock() {
        Ok(mut engine) => {
            engine.report = Some(report.clone());
            if loaded.ok {
                engine.content = Some(Arc::new(loaded.registry));
            }
        }
        Err(_) => {
            error::set(Status::Internal, "the engine state is unavailable", None);
        }
    }
    report
}

/// The last content load report.
pub fn content_report() -> Option<Json> {
    ENGINE.lock().ok().and_then(|engine| engine.report.clone())
}

/// The installed content's rules hash.
pub fn content_hash() -> u64 {
    ENGINE
        .lock()
        .ok()
        .and_then(|engine| engine.content.as_ref().map(|content| content.rules_hash))
        .unwrap_or(0)
}

/// Validate layout documents against the installed content (`09:09.5`).
///
/// The signals a layout may gate on are declared in content, so this needs the
/// installed registry rather than a handle: a layout is checked before any
/// campaign exists, exactly as content is.
pub fn check_ui(request: &Json) -> Json {
    let registry = ENGINE
        .lock()
        .ok()
        .and_then(|engine| engine.content.clone())
        .unwrap_or_else(|| Arc::new(ContentRegistry::empty()));
    crate::validate::check_ui(&registry, request)
}

/// Create a campaign and return its handle (`02:02.10`).
pub fn new_campaign(config: &Json) -> Result<u32, Status> {
    clear_failure_reason();
    if !is_ready() {
        return Err(Status::NotInitialised);
    }
    let registry = {
        let engine = ENGINE.lock().map_err(|_| Status::Internal)?;
        engine.content.clone()
    };
    let Some(registry) = registry else {
        error::set(
            Status::InvalidArgument,
            "no content is loaded",
            Some("call kobra_load_content before creating a campaign"),
        );
        return Err(Status::InvalidArgument);
    };
    let ruleset = match config.get("ruleset").and_then(Json::as_str) {
        None | Some("basic") => Ruleset::Basic,
        Some("advanced") => {
            error::set(
                Status::InvalidArgument,
                "this build compiles only the Basic Master Table",
                Some("the Advanced ladder is transcribed and property-tested but not compiled in yet"),
            );
            return Err(Status::InvalidArgument);
        }
        Some(_) => {
            error::set(Status::InvalidArgument, "unknown ruleset", None);
            return Err(Status::InvalidArgument);
        }
    };
    if ruleset != tables::CAMPAIGN_LADDER.ruleset {
        return Err(Status::InvalidArgument);
    }
    let seed = config
        .get("seed")
        .and_then(Json::as_i64)
        .map(|value| value as u32)
        .unwrap_or_else(seed);
    let mut sim = Sim {
        handle: 0,
        ruleset,
        seed,
        tick: 0,
        frame_id: 0,
        viewport_w: 0,
        viewport_h: 0,
        rng: Rng::new(u64::from(seed)),
        content: Arc::clone(&registry),
        world: World::new(1, 1),
        characters: Vec::new(),
        vehicles: Vec::new(),
        party: Vec::new(),
        flags: BTreeMap::new(),
        encounter: None,
        scenario: String::new(),
        next_entity: 1,
        outbox: Vec::new(),
        spawn_cursor: 0,
        clock: Clock::new(registry.rules.panels_per_day.max(1) as u32),
        baseline: Baseline::generate(&crate::l3::gen::Generator::district(1, 1, seed)),
        generator_version: GENERATOR_VERSION,
        world_record: String::new(),
        stations: Vec::new(),
        standings: Standings::default(),
        journal: Journal::default(),
        talking: None,
        dialogue_node: String::new(),
        impacts: 0,
        last_outcome: None,
        concluded: None,
        moved_tiles: 0,
    };
    if let Some(scenario) = config.get("scenario").and_then(Json::as_str) {
        sim.begin_encounter(&Json::obj([("encounter", Json::string(scenario))]))?;
    }
    let mut sims = SIMS.lock().map_err(|_| Status::Internal)?;
    let handle = match sims.iter().position(Option::is_none) {
        Some(index) => {
            let handle = index as u32 + 1;
            sim.handle = handle;
            sims[index] = Some(sim);
            handle
        }
        None => {
            let handle = sims.len() as u32 + 1;
            sim.handle = handle;
            sims.push(Some(sim));
            handle
        }
    };
    Ok(handle)
}

/// Restore a sim from a save (`kobra_load`).
pub fn load_save(bytes: &str) -> Result<u32, Status> {
    clear_failure_reason();
    let data = match crate::save::parse(bytes) {
        Ok(data) => data,
        Err(message) => {
            error::set(Status::InvalidArgument, message, None);
            return Err(Status::InvalidArgument);
        }
    };
    let registry = {
        let engine = ENGINE.lock().map_err(|_| Status::Internal)?;
        engine.content.clone()
    };
    let Some(registry) = registry else {
        error::set(Status::InvalidArgument, "no content is loaded", None);
        return Err(Status::InvalidArgument);
    };
    let mut sims = SIMS.lock().map_err(|_| Status::Internal)?;
    let handle = match sims.iter().position(Option::is_none) {
        Some(index) => index as u32 + 1,
        None => sims.len() as u32 + 1,
    };
    let sim = Sim::from_save(handle, data, registry)?;
    if handle as usize > sims.len() {
        sims.push(Some(sim));
    } else {
        sims[handle as usize - 1] = Some(sim);
    }
    Ok(handle)
}

/// Run `f` against the sim `handle` refers to.
pub fn with_sim<T>(handle: u32, f: impl FnOnce(&mut Sim) -> T) -> Result<T, Status> {
    if handle == 0 {
        return Err(Status::BadHandle);
    }
    let mut sims = SIMS.lock().map_err(|_| Status::Internal)?;
    match sims.get_mut(handle as usize - 1).and_then(Option::as_mut) {
        Some(sim) => Ok(f(sim)),
        None => Err(Status::BadHandle),
    }
}

/// Free a sim handle. Freeing an unknown handle is not an error.
pub fn free_sim(handle: u32) -> Result<(), Status> {
    if handle == 0 {
        return Ok(());
    }
    let mut sims = SIMS.lock().map_err(|_| Status::Internal)?;
    if let Some(slot) = sims.get_mut(handle as usize - 1) {
        *slot = None;
    }
    Ok(())
}

/// Report a failure and return its status code.
pub fn fail(status: Status, message: &str) -> u32 {
    error::set(status, message, None);
    status.code()
}

/// Report a failure with a reason, and return its status code.
pub fn fail_with_reason(status: Status, message: &str, reason: String) -> u32 {
    if reason.is_empty() {
        error::set(status, message, None);
    } else {
        error::set(status, message, Some(&reason));
    }
    status.code()
}

/// Store an output buffer and return its address and length.
pub fn set_output(bytes: Vec<u8>) -> (usize, usize) {
    match OUTPUT.lock() {
        Ok(mut slot) => {
            *slot = bytes;
            (slot.as_ptr() as usize, slot.len())
        }
        Err(poisoned) => {
            let mut slot = poisoned.into_inner();
            *slot = bytes;
            (slot.as_ptr() as usize, slot.len())
        }
    }
}

/// The phase name of a running encounter, for diagnostics.
pub fn phase_name(encounter: &Encounter) -> &'static str {
    encounter.phase.id()
}

/// Whether an encounter has ended.
pub fn is_finished(encounter: &Encounter) -> bool {
    encounter.finished || encounter.phase == Phase::Ended
}

/// A status document for the shell's diagnostics panel.
pub fn status(sim: &Sim) -> Json {
    Json::obj([
        ("tick", Json::Num(i64::from(sim.tick))),
        ("frame_id", Json::Num(i64::from(sim.frame_id))),
        ("ruleset", Json::string(sim.ruleset.name())),
        ("scenario", Json::string(&sim.scenario)),
        (
            "phase",
            match &sim.encounter {
                Some(encounter) => Json::string(phase_name(encounter)),
                None => Json::Null,
            },
        ),
        (
            "panel",
            match &sim.encounter {
                Some(encounter) => Json::Num(i64::from(encounter.panel)),
                None => Json::Null,
            },
        ),
        (
            "finished",
            Json::Bool(sim.encounter.as_ref().is_some_and(is_finished)),
        ),
        ("characters", Json::Num(sim.characters.len() as i64)),
        ("party", Json::Num(sim.party.len() as i64)),
        // The CRPG frame (M3): the clock, the world, the standing and the
        // journal, so the diagnostics panel reports what the loop is doing
        // (05:05.5, 02:02.7).
        ("clock", sim.clock.to_json()),
        ("day", Json::Num(i64::from(sim.clock.day))),
        ("slot", Json::string(sim.clock.slot().id())),
        ("world", Json::string(&sim.world_record)),
        (
            "generator_version",
            Json::Num(i64::from(sim.generator_version)),
        ),
        (
            "delta_sectors",
            Json::Num(sim.baseline.delta(&sim.world).len() as i64),
        ),
        ("stations", Json::Num(sim.stations.len() as i64)),
        ("unread", Json::Num(sim.journal.unread() as i64)),
        (
            "talking",
            match &sim.talking {
                Some((partner, dialogue)) => Json::obj([
                    ("partner", Json::Num(i64::from(*partner))),
                    ("dialogue", Json::string(dialogue)),
                    ("node", Json::string(&sim.dialogue_node)),
                ]),
                None => Json::Null,
            },
        ),
        (
            "content_hash",
            Json::string(format!("{:016x}", sim.content.rules_hash)),
        ),
        (
            "state_hash",
            Json::string(format!("{:016x}", sim.hash_state())),
        ),
    ])
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The engine state and the sim table are process-wide, so these tests
    /// serialize rather than racing each other for them.
    static TEST_LOCK: Mutex<()> = Mutex::new(());

    fn content_document() -> Json {
        Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.core","kind":"content","path":"base.pack.json",
               "records":[
                 {"id":"wsp.core.rules","type":"rules","data":{"creation":{"budget_points":300,"skill_cost":25}}},
                 {"id":"wsp.item.bat","type":"item","data":{"name_key":"item.bat","hands":"one","melee":"bashing","reach":{"kind":"adjacent"}}},
                 {"id":"wsp.character.thug","type":"character","data":{
                    "name_key":"npc.thug",
                    "traits":{"melee":10,"coordination":10,"brawn":10,"fortitude":10,"intellect":10,"awareness":10,"willpower":10},
                    "items":["wsp.item.bat"]}},
                 {"id":"wsp.sector.alley","type":"sector_map","data":{"width":4,"height":3,"tiles":[
                    {},{},{},{},{},{},{},{},{},{},{},{"level":1}]}},
                 {"id":"wsp.encounter.alley","type":"encounter","data":{
                    "map":"wsp.sector.alley","party_spawns":[[0,0],[1,0]],
                    "enemies":[{"character":"wsp.character.thug","at":[3,2]}]}}
               ]}
            ]}"#,
        )
    }

    fn reset(content: Json) {
        init(7).expect("init");
        let report = load_content(&content);
        assert_eq!(
            report.get("ok").and_then(Json::as_bool),
            Some(true),
            "report: {report}"
        );
    }

    #[test]
    fn a_campaign_builds_from_a_scenario_record() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
            .expect("campaign");
        with_sim(handle, |sim| {
            assert_eq!(sim.characters.len(), 1, "the opposition spawned");
            assert!(sim.encounter.is_some());
            assert_eq!(sim.world.width, 4);
        })
        .expect("sim");
        free_sim(handle).expect("free");
    }

    #[test]
    fn the_three_creation_modes_all_produce_a_playable_character() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
            .expect("campaign");
        with_sim(handle, |sim| {
            sim.command(&Json::from_text(r#"{"cmd":"create_character","mode":"rolled","name_key":"a"}"#))
                .expect("rolled");
            sim.command(&Json::from_text(
                r#"{"cmd":"create_character","mode":"budgeted","name_key":"b",
                    "traits":{"melee":10,"coordination":10,"brawn":10,"fortitude":10,"intellect":10,"awareness":10,"willpower":10},
                    "lifestyle_rv":10,"repute":3}"#,
            ))
            .expect("budgeted");
            sim.command(&Json::from_text(
                r#"{"cmd":"create_character","mode":"authored","record":"wsp.character.thug","name_key":"c"}"#,
            ))
            .expect("authored");
            assert_eq!(sim.characters.len(), 4, "three party plus one enemy");
            assert_eq!(sim.party.len(), 3);
            let modes: Vec<&str> = sim.party.iter().map(|id| {
                sim.characters.iter().find(|c| c.id == *id).expect("character").mode.id()
            }).collect();
            assert_eq!(modes, vec!["rolled", "budgeted", "authored"]);
        })
        .expect("sim");
        free_sim(handle).expect("free");
    }

    #[test]
    fn a_command_stream_replays_to_the_same_hash() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let commands = [
            r#"{"cmd":"create_character","mode":"rolled","name_key":"a"}"#,
            r#"{"cmd":"create_character","mode":"authored","record":"wsp.character.thug","name_key":"b"}"#,
            r#"{"cmd":"plan","actor":1,"action":{"kind":"melee_bash","target":4}}"#,
            r#"{"cmd":"plan","actor":4,"action":{"kind":"melee_bash","target":1}}"#,
            r#"{"cmd":"commit"}"#,
            r#"{"cmd":"commit"}"#,
        ];
        let run = || {
            let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
                .expect("campaign");
            let mut hashes = Vec::new();
            with_sim(handle, |sim| {
                for command in commands {
                    sim.command(&Json::from_text(command)).expect("command");
                    hashes.push(sim.hash_state());
                }
            })
            .expect("sim");
            free_sim(handle).expect("free");
            hashes
        };
        let first = run();
        let second = run();
        assert_eq!(
            first, second,
            "an identical command stream replays identically"
        );
        assert_ne!(first[2], first[4], "the panel must change the state");
    }

    #[test]
    fn a_preview_does_not_consume_the_rng() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
            .expect("campaign");
        // Assertions happen **outside** `with_sim`: a panic inside the closure
        // would poison the sim table and fail every later test as "internal".
        let (preview, before, after) = with_sim(handle, |sim| {
            let before = sim.hash_state();
            let preview = sim.preview(&Json::from_text(r#"{"actor":1,"action":{"kind":"dodge"}}"#));
            (preview, before, sim.hash_state())
        })
        .expect("sim");
        assert!(preview.get("odds").is_some(), "preview: {preview}");
        assert!(preview.get("notes").is_some());
        assert_eq!(before, after, "a preview must not mutate state");
        free_sim(handle).expect("free");
    }

    #[test]
    fn a_save_load_save_round_trip_is_byte_stable_and_keeps_the_hash() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
            .expect("campaign");
        let (hash, first) = with_sim(handle, |sim| {
            sim.command(&Json::from_text(
                r#"{"cmd":"create_character","mode":"rolled","name_key":"a"}"#,
            ))
            .expect("character");
            sim.command(&Json::from_text(
                r#"{"cmd":"plan","actor":2,"action":{"kind":"melee_bash","target":1}}"#,
            ))
            .expect("plan");
            sim.command(&Json::from_text(r#"{"cmd":"commit"}"#))
                .expect("commit");
            let text = crate::save::envelope(&sim.to_save()).to_string();
            (sim.hash_state(), text)
        })
        .expect("sim");
        let restored = load_save(&first).expect("load");
        let (second_hash, second) = with_sim(restored, |sim| {
            (
                sim.hash_state(),
                crate::save::envelope(&sim.to_save()).to_string(),
            )
        })
        .expect("sim");
        assert_eq!(hash, second_hash, "the state hash survives a round trip");
        assert_eq!(first, second, "save -> load -> save is byte-stable");
        free_sim(handle).expect("free");
        free_sim(restored).expect("free");
    }

    #[test]
    fn a_save_from_different_content_is_refused_with_an_explanation() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        let handle = new_campaign(&Json::from_text(r#"{"scenario":"wsp.encounter.alley"}"#))
            .expect("campaign");
        let mut text = with_sim(handle, |sim| {
            crate::save::envelope(&sim.to_save()).to_string()
        })
        .expect("sim");
        text = text.replace("\"content_hash\":\"", "\"content_hash\":\"deadbeef");
        free_sim(handle).expect("free");
        error::clear();
        assert!(load_save(&text).is_err());
        let (_, len) = error::current();
        assert!(len > 0, "the refusal must explain itself");
    }

    #[test]
    fn the_advanced_ladder_is_refused_rather_than_clamped() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        reset(content_document());
        assert!(new_campaign(&Json::from_text(
            r#"{"scenario":"wsp.encounter.alley","ruleset":"advanced"}"#
        ))
        .is_err());
    }

    #[test]
    fn narration_passes_an_unknown_event_through_rather_than_dropping_it() {
        // A newer core against an older shell must still say something truthful.
        let narrated = narrate(&Json::from_text(
            r#"[{"event":"attack","actor":1,"colour":"Blue"},{"event":"invented","x":7}]"#,
        ));
        let Json::Arr(items) = &narrated else {
            panic!("narrate returns a list");
        };
        assert_eq!(items.len(), 2);
        assert_eq!(
            items[0].get("text_key").and_then(Json::as_str),
            Some("log.attack")
        );
        assert_eq!(
            items[1].get("text_key").and_then(Json::as_str),
            Some(""),
            "an unknown kind keeps its own fields and says so"
        );
        assert_eq!(
            items[1]
                .get("args")
                .and_then(|args| args.get("x"))
                .and_then(Json::as_i64),
            Some(7)
        );
        assert_eq!(narrate(&Json::Null), Json::Arr(Vec::new()));
    }

    #[test]
    fn a_campaign_needs_an_initialised_engine() {
        let _guard = TEST_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let mut engine = ENGINE.lock().expect("engine");
        let was_ready = engine.ready;
        engine.ready = false;
        drop(engine);
        assert_eq!(
            new_campaign(&Json::Null).err(),
            Some(Status::NotInitialised)
        );
        let mut engine = ENGINE.lock().expect("engine");
        engine.ready = was_ready;
    }
}
