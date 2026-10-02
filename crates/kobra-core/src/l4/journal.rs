//! L4 — the journal, and the campaign effect vocabulary (`02:02.7`).
//!
//! > **Journal** — a projection of quest/dialogue state into player-readable
//! > text. Human-readable by design — the journal is part of `core` in the save
//! > (`AD-11`).
//!
//! The journal is therefore **two things at once** and it is worth being precise
//! about which: it is the player-facing log (a list of `text_key`s, in the order
//! the game produced them, with the string ids resolved by the shell through the
//! VFS), and it is the **quest state** (each entry is a quest and where it has got
//! to). Keeping them one object is what makes a save readable: a support process
//! can read the `core` half and see exactly which quest steps fired, because that
//! is the same list the player sees.
//!
//! # The quest runtime is a plan, then a write
//!
//! [`Journal::plan`] evaluates the quest graph against a **frozen** journal and
//! returns what should happen; [`Journal::apply_plan`] writes it. The split is not
//! ceremony — it is what makes the following true rather than hoped for:
//!
//! - **A condition cannot see this pass's writes.** If evaluation and application
//!   were one pass, whether step 2 could read step 1's line would depend on the
//!   order the author happened to list the steps in.
//! - **A plan is idempotent.** A step already in `fired` is never planned again, so
//!   re-planning after a save/reload is a no-op.
//! - **The borrow checker enforces it.** `plan` takes `&self`, `apply_plan` takes
//!   `&mut self`, and [`EvaluationContext`] borrows the journal immutably — so a
//!   predicate that tried to write would not compile.
//!
//! A step the author wants the player to close (`QuestStep::auto_advance: false`)
//! becomes a **pause**: it fires and writes its line like any other step, and it
//! stops the chain until [`Journal::release_paused`]. That is the only manual step
//! in the runtime, which is why a quest cannot grow a second one by accident.

use crate::json::Json;
use crate::l1::character::Character;
use crate::l4::factions::Standings;
use crate::l4::progression::{self, Impact, ImpactScale};
use crate::l4::quests::{Quest, QuestStatus};

/// One write a quest step or a dialogue option may make (`02:02.7`).
///
/// The vocabulary is deliberately small, and it has one rule:
/// **the campaign layer never contains a damage formula.** An effect here moves a
/// flag, a standing, an item or a quest; anything that has to hurt someone is
/// resolved by L2 through the command stream.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum CampaignEffect {
    /// Move a faction's standing (`D20`: the amount is authored here).
    Standing {
        /// The faction record id.
        faction: String,
        /// How far to move it.
        delta: i32,
    },
    /// Move the acting character's Repute score (`4c:204-208`).
    Repute(i32),
    /// Move the acting character's Fortune.
    Fortune(i32),
    /// Move Fortune and Repute together by an authored impact (`4c:1229-1251`).
    Impact {
        /// The rung of the impact scale.
        impact: Impact,
        /// Whether it is a gain or a loss.
        gain: bool,
    },
    /// Set a campaign flag.
    Flag {
        /// The flag's name.
        name: String,
        /// The value to set.
        value: i64,
    },
    /// Clear a campaign flag.
    ClearFlag(String),
    /// Give the acting character an item record.
    Give(String),
    /// Take an item record away.
    Take(String),
    /// Start a quest, or move it straight to a status.
    Quest {
        /// The quest record id.
        quest: String,
        /// The status to set.
        status: QuestStatus,
    },
    /// Write a line into the journal.
    Journal {
        /// The line's string id (`AD-22`).
        text_key: String,
    },
}

/// Read a list of effects from content.
pub fn parse_effects(value: Option<&Json>) -> Result<Vec<CampaignEffect>, &'static str> {
    let Some(value) = value else {
        return Ok(Vec::new());
    };
    let Json::Arr(items) = value else {
        return Err("effects must be an array");
    };
    let mut out = Vec::new();
    for item in items {
        out.push(parse_effect(item)?);
    }
    Ok(out)
}

/// Read one effect.
///
/// Each record is a single-key object, which keeps the vocabulary readable in a
/// pack and makes an unknown key a refusal rather than a no-op — the same rule the
/// condition vocabulary follows (`AD-15`).
pub fn parse_effect(value: &Json) -> Result<CampaignEffect, &'static str> {
    let Json::Obj(fields) = value else {
        return Err("an effect must be an object");
    };
    let (key, argument) = fields.iter().next().ok_or("an effect must not be empty")?;
    match key.as_str() {
        "standing" => {
            let Json::Obj(pairs) = argument else {
                return Err("standing must be {\"faction\": delta}");
            };
            let (faction, delta) = pairs.iter().next().ok_or("standing needs one faction")?;
            Ok(CampaignEffect::Standing {
                faction: faction.clone(),
                delta: delta.as_i64().ok_or("a standing delta must be a number")? as i32,
            })
        }
        "repute" => Ok(CampaignEffect::Repute(
            argument.as_i64().ok_or("repute needs a number")? as i32,
        )),
        "fortune" => Ok(CampaignEffect::Fortune(
            argument.as_i64().ok_or("fortune needs a number")? as i32,
        )),
        "impact" => {
            let rung = argument
                .get("impact")
                .and_then(Json::as_str)
                .and_then(Impact::from_id)
                .ok_or("impact needs a rung")?;
            Ok(CampaignEffect::Impact {
                impact: rung,
                gain: argument
                    .get("gain")
                    .and_then(Json::as_bool)
                    .ok_or("impact needs gain: true or false")?,
            })
        }
        "flag" => {
            let Json::Obj(pairs) = argument else {
                return Err("flag must be {\"name\": value}");
            };
            let (name, setting) = pairs.iter().next().ok_or("flag needs one name")?;
            Ok(CampaignEffect::Flag {
                name: name.clone(),
                value: setting.as_i64().unwrap_or(0),
            })
        }
        "clear_flag" => Ok(CampaignEffect::ClearFlag(
            argument
                .as_str()
                .ok_or("clear_flag needs a flag name")?
                .to_string(),
        )),
        "give" => Ok(CampaignEffect::Give(
            argument
                .as_str()
                .ok_or("give needs an item id")?
                .to_string(),
        )),
        "take" => Ok(CampaignEffect::Take(
            argument
                .as_str()
                .ok_or("take needs an item id")?
                .to_string(),
        )),
        "quest" => {
            let quest = argument
                .get("id")
                .and_then(Json::as_str)
                .ok_or("quest needs an id")?;
            let status = argument
                .get("status")
                .and_then(Json::as_str)
                .and_then(QuestStatus::from_id)
                .ok_or("quest needs a status")?;
            Ok(CampaignEffect::Quest {
                quest: quest.to_string(),
                status,
            })
        }
        "journal" => Ok(CampaignEffect::Journal {
            text_key: argument
                .as_str()
                .ok_or("journal needs a string id")?
                .to_string(),
        }),
        _ => Err("unknown campaign effect"),
    }
}

/// One journal line, as the player reads it.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct JournalLine {
    /// The string id (`AD-22`), resolved by the shell through the VFS.
    pub text_key: String,
    /// Whether the player has opened the journal since it was written.
    pub read: bool,
}

/// One quest's progress, which is also its journal page.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct JournalEntry {
    /// The quest record id.
    pub quest: String,
    /// Where the quest has got to.
    pub status: QuestStatus,
    /// The index of the step that is current.
    pub step: usize,
    /// The steps that have fired, ascending.
    pub fired: Vec<usize>,
    /// The lines this quest has written.
    pub lines: Vec<JournalLine>,
}

impl JournalEntry {
    /// A fresh, inactive page.
    pub fn new(quest: &str) -> JournalEntry {
        JournalEntry {
            quest: quest.to_string(),
            status: QuestStatus::Inactive,
            step: 0,
            fired: Vec::new(),
            lines: Vec::new(),
        }
    }
}

/// The campaign's memory: every quest's progress, and the order it happened in.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Journal {
    /// Every quest the campaign has ever touched, in first-touch order.
    pub entries: Vec<JournalEntry>,
    /// Every line the campaign has written, in order, with the quest it belongs to
    /// (`""` for a line that belongs to none).
    pub lines: Vec<(String, JournalLine)>,
    /// The paused steps waiting on the player, as `(quest, step)`.
    ///
    /// A pause is a step the author wants the player to close (`QuestStep::
    /// auto_advance: false`). The step fires and writes its line like any other;
    /// what it does not do is release the chain that follows it. So this list is
    /// separate from `fired` — "it happened" and "I am done with it" are different
    /// facts.
    pub pauses: Vec<(String, usize)>,
    /// Pauses the player has closed, so a reload does not re-pause them.
    pub released: Vec<(String, usize)>,
}

/// One thing the quest graph wants to do, decided by [`Journal::plan`].
///
/// A plan item is one of three things: "start this quest" (`start`), "fire this
/// step" (`step: Some`), or "the quest has run out of steps" (`start: false`,
/// `step: None`). Keeping it a flat list rather than an enum means the caller can
/// log the plan in order and a test can assert on it directly.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct PlannedStep {
    /// The quest record id.
    pub quest: String,
    /// Whether this item starts the quest.
    pub start: bool,
    /// The step to fire, or `None` for "the quest's steps have run out".
    pub step: Option<usize>,
    /// The line the journal gains.
    pub text_key: String,
    /// The effects the step applies.
    pub effects: Vec<CampaignEffect>,
    /// Where the quest's pointer moves after this item, or `None` to leave it.
    pub next_step: Option<usize>,
    /// Whether this step is a pause the player has to close.
    pub pauses: bool,
}

/// What changed while effects were applied, so the sim can emit events.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Applied {
    /// Lines that were written.
    pub written: Vec<String>,
    /// Quests whose status changed, with the new status.
    pub quests: Vec<(String, QuestStatus)>,
    /// Steps that fired, as `(quest, step index)`.
    pub steps: Vec<(String, usize)>,
    /// Markers for state that changed but has no line of its own: a standing, a
    /// flag, an item. The sim turns these into events.
    pub markers: Vec<String>,
}

/// The read-only state a predicate is evaluated against.
///
/// Borrowed rather than owned, and a struct rather than a long argument list,
/// because every predicate needs the same things and getting the order wrong once
/// would be a silent bug.
pub struct EvaluationContext<'a> {
    /// The acting character, when the evaluation has one.
    pub actor: Option<&'a Character>,
    /// Campaign flags.
    pub flags: &'a std::collections::BTreeMap<String, i64>,
    /// Faction standing.
    pub standings: &'a Standings,
    /// The quest journal.
    pub journal: &'a Journal,
    /// The clock (`02:02.7`).
    pub clock: &'a crate::l3::clock::Clock,
    /// The factions' records, so a condition can read a band.
    pub factions: &'a [crate::l4::factions::Faction],
    /// Every item record the content defines, so a condition that names an item
    /// the campaign does not ship can be reported by the validator rather than
    /// silently never holding.
    pub item_ids: &'a [String],
}

impl EvaluationContext<'_> {
    /// A faction record by id.
    pub fn faction(&self, id: &str) -> Option<&crate::l4::factions::Faction> {
        self.factions.iter().find(|faction| faction.id == id)
    }
}

impl Journal {
    /// A page for a quest, created on first touch.
    fn page(&mut self, quest: &str) -> &mut JournalEntry {
        if let Some(index) = self.entries.iter().position(|entry| entry.quest == quest) {
            return &mut self.entries[index];
        }
        self.entries.push(JournalEntry::new(quest));
        self.entries.last_mut().expect("a page was just pushed")
    }

    /// A page, when one exists.
    pub fn get(&self, quest: &str) -> Option<&JournalEntry> {
        self.entries.iter().find(|entry| entry.quest == quest)
    }

    /// A quest's status, with `Inactive` for a quest never touched.
    pub fn status(&self, quest: &str) -> QuestStatus {
        self.get(quest)
            .map(|entry| entry.status)
            .unwrap_or(QuestStatus::Inactive)
    }

    /// Write a line into the journal.
    pub fn write(&mut self, quest: &str, text_key: &str) {
        self.lines.push((
            quest.to_string(),
            JournalLine {
                text_key: text_key.to_string(),
                read: false,
            },
        ));
    }

    /// Mark every line read, returning how many were new.
    pub fn mark_read(&mut self) -> usize {
        let mut count = 0;
        for (_, line) in &mut self.lines {
            if !line.read {
                line.read = true;
                count += 1;
            }
        }
        count
    }

    /// How many lines a player has not read.
    pub fn unread(&self) -> usize {
        self.lines.iter().filter(|(_, line)| !line.read).count()
    }

    /// The step a quest is paused on, when it is waiting on the player.
    pub fn pause_of(&self, quest: &str) -> Option<usize> {
        self.pauses
            .iter()
            .find(|(name, _)| name == quest)
            .map(|(_, step)| *step)
    }

    /// Whether a paused step has been released by the player.
    pub fn is_released(&self, quest: &str, step: usize) -> bool {
        self.released
            .iter()
            .any(|(name, index)| name == quest && *index == step)
    }

    /// Release the step a quest is paused on (`QuestStep::auto_advance: false`).
    ///
    /// A paused step has already fired and its line is already written; what is
    /// missing is the player's decision to move on. So this changes nothing about
    /// the record except consent — the next pass sees the following step's
    /// conditions and fires it if they hold.
    ///
    /// **It takes no step index, deliberately.** "Which step am I on" is the
    /// journal's own fact, and a caller that passed the wrong index would release
    /// the wrong step. The paused step is returned so the UI can name it.
    pub fn release_paused(&mut self, quest: &str) -> Result<usize, &'static str> {
        let Some(index) = self.pauses.iter().position(|(name, _)| name == quest) else {
            return Err("that quest is not paused");
        };
        let (name, step) = self.pauses.remove(index);
        self.released.push((name, step));
        Ok(step)
    }

    /// Apply a list of effects.
    ///
    /// `actor` is the party member the effects land on — the one who took the
    /// dialogue option, or the first party member when a step fires from the world.
    #[allow(clippy::too_many_arguments)]
    pub fn apply(
        &mut self,
        effects: &[CampaignEffect],
        mut actor: Option<&mut Character>,
        flags: &mut std::collections::BTreeMap<String, i64>,
        standings: &mut Standings,
        scale: &ImpactScale,
        defaults: &dyn Fn(&str) -> i32,
    ) -> Applied {
        let mut applied = Applied::default();
        for effect in effects {
            match effect {
                CampaignEffect::Standing { faction, delta } => {
                    let default = defaults(faction);
                    standings.adjust(faction, *delta, default);
                    applied.markers.push(format!("standing:{faction}"));
                }
                CampaignEffect::Impact { impact, gain } => {
                    if let Some(character) = actor.as_deref_mut() {
                        progression::apply_impact(scale, character, *impact, *gain);
                    }
                    applied.markers.push(format!("impact:{}", impact.id()));
                }
                CampaignEffect::Repute(delta) => {
                    if let Some(character) = actor.as_deref_mut() {
                        character.repute = (character.repute + *delta).max(0);
                    }
                }
                CampaignEffect::Fortune(delta) => {
                    if let Some(character) = actor.as_deref_mut() {
                        character.fortune += *delta;
                    }
                }
                CampaignEffect::Flag { name, value } => {
                    flags.insert(name.clone(), *value);
                    applied.markers.push(format!("flag:{name}"));
                }
                CampaignEffect::ClearFlag(name) => {
                    flags.remove(name);
                    applied.markers.push(format!("clear_flag:{name}"));
                }
                CampaignEffect::Give(item) => {
                    if let Some(character) = actor.as_deref_mut() {
                        if !character.items.iter().any(|held| held == item) {
                            character.items.push(item.clone());
                        }
                    }
                    applied.markers.push(format!("give:{item}"));
                }
                CampaignEffect::Take(item) => {
                    if let Some(character) = actor.as_deref_mut() {
                        crate::l4::economy::drop_item(character, item);
                    }
                    applied.markers.push(format!("take:{item}"));
                }
                CampaignEffect::Quest { quest, status } => {
                    let page = self.page(quest);
                    if page.status != *status {
                        page.status = *status;
                        applied.quests.push((quest.clone(), *status));
                    }
                }
                CampaignEffect::Journal { text_key } => {
                    self.write("", text_key);
                    applied.written.push(text_key.clone());
                }
            }
        }
        applied
    }

    /// Evaluate the quest graph against the frozen journal.
    ///
    /// `plan` asks the questions; [`Journal::apply_plan`] writes the answers.
    pub fn plan(&self, quests: &[Quest], context: &EvaluationContext<'_>) -> Vec<PlannedStep> {
        let released = |quest: &str, step: usize| self.is_released(quest, step);
        let released: &dyn Fn(&str, usize) -> bool = &released;
        let mut plan = Vec::new();
        for quest in quests {
            let status = self.status(&quest.id);
            if status == QuestStatus::Inactive
                && crate::l4::dialogue::evaluate(&quest.entry, context)
            {
                plan.push(PlannedStep {
                    quest: quest.id.clone(),
                    start: true,
                    step: None,
                    text_key: quest.name_key.clone(),
                    effects: Vec::new(),
                    next_step: Some(0),
                    pauses: false,
                });
                // A quest that starts in this pass is evaluated for its first step
                // in the same pass, so a campaign does not need a panel between the
                // two.
                plan.extend(plan_steps(quest, 0, context, released));
                continue;
            }
            if status != QuestStatus::Active {
                continue;
            }
            // A quest waiting on the player is not evaluated further. That is what
            // makes a pause a pause: the pointer has already moved past the paused
            // step, so without this gate the *next* step would fire on the very
            // next pass and the pause would mean nothing.
            if self.pause_of(&quest.id).is_some() {
                continue;
            }
            let step = self.get(&quest.id).map(|page| page.step).unwrap_or(0);
            plan.extend(plan_steps(quest, step, context, released));
        }
        plan
    }

    /// Write what [`Journal::plan`] decided, and return everything that changed.
    ///
    /// Effects are applied in plan order, so a quest's completion effects see the
    /// world the earlier steps left behind — the order the player experienced.
    /// Idempotent by construction: a step already in `fired` is never planned
    /// twice.
    #[allow(clippy::too_many_arguments)]
    pub fn apply_plan(
        &mut self,
        plan: &[PlannedStep],
        actor: Option<&mut Character>,
        flags: &mut std::collections::BTreeMap<String, i64>,
        standings: &mut Standings,
        scale: &ImpactScale,
        defaults: &dyn Fn(&str) -> i32,
        quests: &[Quest],
    ) -> Applied {
        let mut all = Applied::default();
        for quest in quests {
            self.page(&quest.id);
        }
        let mut actor = actor;
        for item in plan {
            if item.start {
                if self.status(&item.quest) == QuestStatus::Active {
                    continue;
                }
                {
                    let page = self.page(&item.quest);
                    page.status = QuestStatus::Active;
                    page.step = 0;
                    page.fired.clear();
                }
                self.write(&item.quest, &item.text_key);
                all.written.push(item.text_key.clone());
                all.quests.push((item.quest.clone(), QuestStatus::Active));
                continue;
            }
            if self.status(&item.quest) != QuestStatus::Active {
                continue;
            }
            let Some(index) = item.step else {
                // The quest has run out of steps: it is complete.
                let effects = quests
                    .iter()
                    .find(|quest| quest.id == item.quest)
                    .map(|quest| quest.on_complete.clone())
                    .unwrap_or_default();
                let applied = self.apply(
                    &effects,
                    actor.as_deref_mut(),
                    flags,
                    standings,
                    scale,
                    defaults,
                );
                merge(&mut all, applied);
                if self.status(&item.quest) == QuestStatus::Active {
                    self.page(&item.quest).status = QuestStatus::Completed;
                    // Completion answers any question the quest was still asking.
                    self.pauses.retain(|(name, _)| name != &item.quest);
                    all.quests
                        .push((item.quest.clone(), QuestStatus::Completed));
                }
                continue;
            };
            let applied = self.apply(
                &item.effects,
                actor.as_deref_mut(),
                flags,
                standings,
                scale,
                defaults,
            );
            merge(&mut all, applied);
            {
                let page = self.page(&item.quest);
                if !page.fired.contains(&index) {
                    page.fired.push(index);
                    page.fired.sort_unstable();
                }
                if let Some(next) = item.next_step {
                    page.step = next;
                }
            }
            if item.pauses
                && !self
                    .pauses
                    .iter()
                    .any(|(name, step)| name == &item.quest && *step == index)
            {
                self.pauses.push((item.quest.clone(), index));
            }
            self.write(&item.quest, &item.text_key);
            all.steps.push((item.quest.clone(), index));
            all.written.push(item.text_key.clone());
        }
        all
    }

    /// Write the journal into a save stream.
    ///
    /// Named `write_into` rather than `write` because the journal has two writers
    /// and the other one — a player-readable line — is what callers mean when they
    /// say "write".
    pub fn write_into(&self, out: &mut Vec<u8>) {
        crate::save::write_u16(out, self.entries.len().min(u16::MAX as usize) as u16);
        for entry in &self.entries {
            crate::save::write_text(out, &entry.quest);
            out.push(status_code(entry.status));
            out.extend_from_slice(&(entry.step as u32).to_le_bytes());
            crate::save::write_u16(out, entry.fired.len().min(u16::MAX as usize) as u16);
            for index in &entry.fired {
                out.extend_from_slice(&(*index as u32).to_le_bytes());
            }
            crate::save::write_u16(out, entry.lines.len().min(u16::MAX as usize) as u16);
            for line in &entry.lines {
                crate::save::write_text(out, &line.text_key);
                out.push(u8::from(line.read));
            }
        }
        crate::save::write_u16(out, self.lines.len().min(u16::MAX as usize) as u16);
        for (quest, line) in &self.lines {
            crate::save::write_text(out, quest);
            crate::save::write_text(out, &line.text_key);
            out.push(u8::from(line.read));
        }
        crate::save::write_u16(out, self.pauses.len().min(u16::MAX as usize) as u16);
        for (quest, step) in &self.pauses {
            crate::save::write_text(out, quest);
            out.extend_from_slice(&(*step as u32).to_le_bytes());
        }
        crate::save::write_u16(out, self.released.len().min(u16::MAX as usize) as u16);
        for (quest, step) in &self.released {
            crate::save::write_text(out, quest);
            out.extend_from_slice(&(*step as u32).to_le_bytes());
        }
    }

    /// Read the journal from a save stream.
    pub fn read(reader: &mut crate::save::Reader<'_>) -> Result<Journal, &'static str> {
        let mut journal = Journal::default();
        let entries = reader.u16()? as usize;
        for _ in 0..entries {
            let quest = reader.text()?;
            let status = status_from_code(reader.u8()?)?;
            let step = reader.u32()? as usize;
            let fired_count = reader.u16()? as usize;
            let mut fired = Vec::with_capacity(fired_count);
            for _ in 0..fired_count {
                fired.push(reader.u32()? as usize);
            }
            let line_count = reader.u16()? as usize;
            let mut lines = Vec::with_capacity(line_count);
            for _ in 0..line_count {
                let text_key = reader.text()?;
                let read = reader.u8()? != 0;
                lines.push(JournalLine { text_key, read });
            }
            journal.entries.push(JournalEntry {
                quest,
                status,
                step,
                fired,
                lines,
            });
        }
        let lines = reader.u16()? as usize;
        for _ in 0..lines {
            let quest = reader.text()?;
            let text_key = reader.text()?;
            let read = reader.u8()? != 0;
            journal.lines.push((quest, JournalLine { text_key, read }));
        }
        let pauses = reader.u16()? as usize;
        for _ in 0..pauses {
            let quest = reader.text()?;
            let step = reader.u32()? as usize;
            journal.pauses.push((quest, step));
        }
        let released = reader.u16()? as usize;
        for _ in 0..released {
            let quest = reader.text()?;
            let step = reader.u32()? as usize;
            journal.released.push((quest, step));
        }
        Ok(journal)
    }

    /// The journal as a document, for the shell's journal page.
    pub fn to_json(&self) -> Json {
        Json::obj([
            (
                "quests",
                Json::arr(self.entries.iter().map(|entry| {
                    Json::obj([
                        ("quest", Json::string(&entry.quest)),
                        ("status", Json::string(entry.status.id())),
                        ("step", Json::Num(entry.step as i64)),
                        (
                            "fired",
                            Json::arr(entry.fired.iter().map(|index| Json::Num(*index as i64))),
                        ),
                        (
                            "paused_on",
                            match self.pause_of(&entry.quest) {
                                Some(step) => Json::Num(step as i64),
                                None => Json::Null,
                            },
                        ),
                    ])
                })),
            ),
            (
                "lines",
                Json::arr(self.lines.iter().map(|(quest, line)| {
                    let mut object = Json::object();
                    object.set("text_key", Json::string(&line.text_key));
                    object.set("read", Json::Bool(line.read));
                    if !quest.is_empty() {
                        object.set("quest", Json::string(quest));
                    }
                    object
                })),
            ),
            ("unread", Json::Num(self.unread() as i64)),
            (
                "pauses",
                Json::arr(self.pauses.iter().map(|(quest, step)| {
                    Json::obj([
                        ("quest", Json::string(quest)),
                        ("step", Json::Num(*step as i64)),
                    ])
                })),
            ),
        ])
    }
}

/// The steps a quest would fire from `step` onward, given the frozen state.
///
/// The loop is what makes a chain of already-true steps resolve in one pass, and
/// the `auto_advance` flag is what lets an author put a deliberate pause in it.
fn plan_steps(
    quest: &Quest,
    step: usize,
    context: &EvaluationContext<'_>,
    released: &dyn Fn(&str, usize) -> bool,
) -> Vec<PlannedStep> {
    let mut plan = Vec::new();
    let mut index = step;
    loop {
        let Some(current) = quest.step(index) else {
            plan.push(PlannedStep {
                quest: quest.id.clone(),
                start: false,
                step: None,
                text_key: String::new(),
                effects: Vec::new(),
                next_step: None,
                pauses: false,
            });
            break;
        };
        // The step's entry conditions gate whether it is current at all.
        if !crate::l4::dialogue::evaluate(&current.entry, context) {
            break;
        }
        if !crate::l4::dialogue::evaluate(&current.complete, context) {
            break;
        }
        plan.push(PlannedStep {
            quest: quest.id.clone(),
            start: false,
            step: Some(index),
            text_key: current.text_key.clone(),
            effects: current.effects.clone(),
            // The pointer always advances past a step that fired; it is the
            // *chain* that stops at a pause, and only until the player releases it.
            next_step: Some(index + 1),
            pauses: !current.auto_advance,
        });
        if !current.auto_advance && !released(&quest.id, index) {
            break;
        }
        index += 1;
    }
    plan
}

/// Merge one applied batch into another.
fn merge(into: &mut Applied, from: Applied) {
    into.written.extend(from.written);
    into.quests.extend(from.quests);
    into.steps.extend(from.steps);
    into.markers.extend(from.markers);
}

/// The wire code for a quest status, for the world delta.
pub fn status_code(status: QuestStatus) -> u8 {
    match status {
        QuestStatus::Inactive => 0,
        QuestStatus::Active => 1,
        QuestStatus::Completed => 2,
        QuestStatus::Failed => 3,
    }
}

/// Read a quest status wire code.
pub fn status_from_code(code: u8) -> Result<QuestStatus, &'static str> {
    match code {
        0 => Ok(QuestStatus::Inactive),
        1 => Ok(QuestStatus::Active),
        2 => Ok(QuestStatus::Completed),
        3 => Ok(QuestStatus::Failed),
        _ => Err("an unknown quest status in the save"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l4::quests::Quest;

    fn hero() -> Character {
        Character::authored(1, "hero", "", 0, [10; 7], 10, 0, vec![], vec![], vec![])
    }

    fn quest() -> Quest {
        Quest::parse(
            "wsp.quest.missing",
            &Json::from_text(
                r#"{"name_key":"journal.missing.started","faction":"wsp.faction.watch",
                    "entry":{"flag_set":"heard_rumour"},
                    "steps":[
                      {"id":"ask","text_key":"journal.missing.ask","complete":{"flag_set":"asked"}},
                      {"id":"find","text_key":"journal.missing.found","complete":{"has_item":"wsp.item.ledger"}}],
                    "on_complete":[{"standing":{"wsp.faction.watch":10}},{"journal":"journal.missing.done"}]}"#,
            ),
        )
        .expect("quest")
    }

    /// One quest pass: plan against the frozen state, then write the plan.
    ///
    /// This is the sim's own two-phase call in one helper, so the tests drive the
    /// real API rather than a simplified one.
    #[allow(clippy::too_many_arguments)]
    fn pass(
        journal: &mut Journal,
        quests: &[Quest],
        actor: &mut Character,
        flags: &mut std::collections::BTreeMap<String, i64>,
        standings: &mut Standings,
        clock: &crate::l3::clock::Clock,
        factions: &[crate::l4::factions::Faction],
        item_ids: &[String],
    ) -> Applied {
        let scale = ImpactScale::canonical();
        let defaults = |_: &str| 0;
        let planned = {
            let context = EvaluationContext {
                actor: Some(&*actor),
                flags,
                standings,
                journal,
                clock,
                factions,
                item_ids,
            };
            journal.plan(quests, &context)
        };
        journal.apply_plan(
            &planned,
            Some(actor),
            flags,
            standings,
            &scale,
            &defaults,
            quests,
        )
    }

    #[test]
    fn a_quest_starts_when_its_entry_conditions_hold_and_fires_each_step_once() {
        let mut journal = Journal::default();
        let mut flags = std::collections::BTreeMap::new();
        let mut standings = Standings::default();
        let mut actor = hero();
        let clock = crate::l3::clock::Clock::new(1440);
        let quests = vec![quest()];

        // Nothing yet: the quest is known but inactive.
        let before = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert!(before.quests.is_empty());
        assert_eq!(journal.status("wsp.quest.missing"), QuestStatus::Inactive);

        // The rumour starts it.
        flags.insert("heard_rumour".into(), 1);
        let started = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(
            started.quests,
            vec![("wsp.quest.missing".to_string(), QuestStatus::Active)]
        );
        assert_eq!(journal.unread(), 1, "the quest's own line");
        assert_eq!(journal.status("wsp.quest.missing"), QuestStatus::Active);

        // Step one completes when the flag is set.
        flags.insert("asked".into(), 1);
        let stepped = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(stepped.steps, vec![("wsp.quest.missing".to_string(), 0)]);
        assert_eq!(
            journal.get("wsp.quest.missing").map(|page| page.step),
            Some(1)
        );

        // Re-evaluating changes nothing: a step fires once.
        let again = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert!(again.steps.is_empty(), "progress is idempotent");

        // The ledger finishes the quest, and its completion effects land.
        actor.items.push("wsp.item.ledger".into());
        let done = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(done.steps, vec![("wsp.quest.missing".to_string(), 1)]);
        assert_eq!(journal.status("wsp.quest.missing"), QuestStatus::Completed);
        assert_eq!(standings.get("wsp.faction.watch"), Some(10));
        assert!(journal
            .lines
            .iter()
            .any(|(_, line)| line.text_key == "journal.missing.done"));
    }

    #[test]
    fn a_pause_stops_the_chain_until_the_player_releases_it() {
        let quest = Quest::parse(
            "wsp.quest.talk",
            &Json::from_text(
                r#"{"name_key":"journal.talk.started","entry":{"flag_set":"go"},
                    "steps":[{"id":"greet","text_key":"journal.talk.greet","auto_advance":false},
                             {"id":"leave","text_key":"journal.talk.leave"}]}"#,
            ),
        )
        .expect("quest");
        let quests = vec![quest];
        let mut journal = Journal::default();
        let mut flags = std::collections::BTreeMap::new();
        flags.insert("go".into(), 1);
        let mut standings = Standings::default();
        let mut actor = hero();
        let clock = crate::l3::clock::Clock::new(1440);

        // The entry condition starts the quest; the first step is a deliberate
        // pause, so it fires and the chain stops there.
        let first = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(first.steps, vec![("wsp.quest.talk".to_string(), 0)]);
        assert_eq!(journal.pause_of("wsp.quest.talk"), Some(0));

        // The pause holds across passes: nothing moves until the player closes it.
        let held = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert!(held.steps.is_empty(), "the pause is not a poll");
        assert!(held.quests.is_empty(), "and it does not complete the quest");

        // Releasing it lets the last step fire, and the quest completes.
        assert_eq!(journal.release_paused("wsp.quest.talk"), Ok(0));
        assert_eq!(journal.pause_of("wsp.quest.talk"), None);
        let second = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(second.steps, vec![("wsp.quest.talk".to_string(), 1)]);
        assert_eq!(journal.status("wsp.quest.talk"), QuestStatus::Completed);
    }

    #[test]
    fn a_pause_survives_a_save_and_a_second_release_is_refused() {
        let quest = Quest::parse(
            "wsp.quest.talk",
            &Json::from_text(
                r#"{"name_key":"journal.talk.started","entry":{"flag_set":"go"},
                    "steps":[{"id":"greet","text_key":"journal.talk.greet","auto_advance":false},
                             {"id":"leave","text_key":"journal.talk.leave"}]}"#,
            ),
        )
        .expect("quest");
        let quests = vec![quest];
        let mut journal = Journal::default();
        let mut flags = std::collections::BTreeMap::new();
        flags.insert("go".into(), 1);
        let mut standings = Standings::default();
        let mut actor = hero();
        let clock = crate::l3::clock::Clock::new(1440);
        pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(journal.pause_of("wsp.quest.talk"), Some(0));

        // A save taken mid-pause remembers the question, so a reload does not
        // silently skip it.
        let mut bytes = Vec::new();
        journal.write_into(&mut bytes);
        let mut reader = crate::save::Reader::new(&bytes);
        let mut restored = Journal::read(&mut reader).expect("read");
        assert!(reader.at_end());
        assert_eq!(restored.pause_of("wsp.quest.talk"), Some(0));
        assert_eq!(restored, journal);

        // Releasing is once-only, and it is answered from the pause itself.
        assert_eq!(restored.release_paused("wsp.quest.talk"), Ok(0));
        assert!(restored.pause_of("wsp.quest.talk").is_none());
        assert!(restored.release_paused("wsp.quest.talk").is_err());
        assert!(restored.release_paused("wsp.quest.missing").is_err());

        // The release survives a reload too.
        let mut bytes = Vec::new();
        restored.write_into(&mut bytes);
        let mut reader = crate::save::Reader::new(&bytes);
        let again = Journal::read(&mut reader).expect("read");
        assert!(again.is_released("wsp.quest.talk", 0));
        assert_eq!(again, restored);
    }

    #[test]
    fn a_step_condition_is_evaluated_against_a_frozen_journal() {
        // The two-phase split exists so a step's condition cannot read a line a
        // later step has not written. Here the second step is gated on the first
        // having fired, so it takes a second pass — which is the documented
        // reading and is visible rather than accidental.
        let quest = Quest::parse(
            "wsp.quest.chain",
            &Json::from_text(
                r#"{"name_key":"journal.chain.started","entry":{"flag_set":"go"},
                    "steps":[
                      {"id":"one","text_key":"journal.chain.one","complete":{"flag_set":"go"}},
                      {"id":"two","text_key":"journal.chain.two",
                       "complete":{"step_done":{"quest":"wsp.quest.chain","step":0}}}]}"#,
            ),
        )
        .expect("quest");
        let quests = vec![quest];
        let mut journal = Journal::default();
        let mut flags = std::collections::BTreeMap::new();
        flags.insert("go".into(), 1);
        let mut standings = Standings::default();
        let mut actor = hero();
        let clock = crate::l3::clock::Clock::new(1440);
        let applied = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(applied.steps, vec![("wsp.quest.chain".to_string(), 0)]);
        let second = pass(
            &mut journal,
            &quests,
            &mut actor,
            &mut flags,
            &mut standings,
            &clock,
            &[],
            &[],
        );
        assert_eq!(second.steps, vec![("wsp.quest.chain".to_string(), 1)]);
    }

    #[test]
    fn a_line_is_unread_until_the_player_opens_the_journal() {
        let mut journal = Journal::default();
        journal.write("q", "a");
        journal.write("", "b");
        assert_eq!(journal.unread(), 2);
        assert_eq!(journal.mark_read(), 2);
        assert_eq!(journal.unread(), 0);
        assert_eq!(journal.mark_read(), 0);
    }

    #[test]
    fn the_effect_vocabulary_refuses_an_unknown_key() {
        assert!(parse_effect(&Json::from_text(r#"{"heal":10}"#)).is_err());
        assert!(parse_effect(&Json::from_text(r#"{}"#)).is_err());
        assert!(parse_effect(&Json::from_text(r#""damage""#)).is_err());
        assert!(parse_effects(Some(&Json::from_text(r#"[{"flag":{"a":1}}]"#))).is_ok());
        assert_eq!(parse_effects(None).expect("none"), Vec::new());
    }

    #[test]
    fn the_journal_round_trips_through_a_save_stream() {
        let mut journal = Journal::default();
        journal.write("wsp.quest.missing", "a");
        journal.write("", "b");
        journal.mark_read();
        journal.pauses.push(("wsp.quest.talk".into(), 1));
        journal.released.push(("wsp.quest.other".into(), 2));
        {
            let page = journal.page("wsp.quest.missing");
            page.status = QuestStatus::Active;
            page.step = 2;
            page.fired.push(0);
            page.fired.push(1);
            page.lines.push(JournalLine {
                text_key: "c".into(),
                read: false,
            });
        }

        let mut bytes = Vec::new();
        journal.write_into(&mut bytes);
        let mut reader = crate::save::Reader::new(&bytes);
        let restored = Journal::read(&mut reader).expect("read");
        assert_eq!(restored, journal);
        assert!(reader.at_end());
    }

    #[test]
    fn a_status_round_trips_through_its_wire_code() {
        for status in QuestStatus::ALL {
            assert_eq!(status_from_code(status_code(status)), Ok(status));
        }
        assert!(status_from_code(9).is_err());
    }
}
