//! L4 — the quest graph (`02:02.7`, 05:05.5 M3).
//!
//! > **Quests** — a directed graph of steps; each step has entry conditions,
//! > completion conditions, and effects, all from the DSL vocabulary plus
//! > campaign predicates. The DSL is shared with powers so authors learn one
//! > vocabulary.
//!
//! The one design decision worth stating is what a step *is* once the game is
//! running. A step is not a script: it is a pair of predicate lists. The sim
//! evaluates the **current** step's completion conditions against the state that
//! already exists — flags, standing, journal, the party's sheets, the clock — and
//! applies its effects when they hold. That has three consequences, and all three
//! are why it is built this way:
//!
//! 1. **A quest cannot fork the rules.** There is no quest runtime that could
//!    damage someone; an effect names a flag, an item or a standing, and anything
//!    that hurts goes through L2 (`02:02.7`).
//! 2. **Progress is idempotent.** The journal records which steps have fired, so
//!    re-evaluating is a no-op, and a save can be loaded and re-evaluated freely.
//! 3. **The editor can author it.** A step is data a form can render, which is
//!    what M4's quest editor needs (`04`).
//!
//! A step whose entry conditions never hold is *reported* by the validator rather
//! than silently skipped, because "the quest never starts" and "the quest is
//! broken" look identical to a player.

use crate::json::Json;
use crate::l4::condition::{parse as parse_condition, Condition};

/// What a quest is doing.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum QuestStatus {
    /// The record exists but the quest has not started.
    Inactive,
    /// Running.
    Active,
    /// Finished successfully.
    Completed,
    /// Finished unsuccessfully.
    Failed,
}

impl QuestStatus {
    /// Every status, so a condition can name one without a typo surviving.
    pub const ALL: [QuestStatus; 4] = [
        QuestStatus::Inactive,
        QuestStatus::Active,
        QuestStatus::Completed,
        QuestStatus::Failed,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            QuestStatus::Inactive => "inactive",
            QuestStatus::Active => "active",
            QuestStatus::Completed => "completed",
            QuestStatus::Failed => "failed",
        }
    }

    /// The status a content id names.
    pub fn from_id(id: &str) -> Option<QuestStatus> {
        QuestStatus::ALL
            .into_iter()
            .find(|status| status.id() == id)
    }
}

/// One step of a quest.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct QuestStep {
    /// The step's stable id, used for its journal line.
    pub id: String,
    /// The player-readable line the journal gains when the step fires.
    pub text_key: String,
    /// The conditions under which this step becomes current.
    pub entry: Condition,
    /// The conditions under which this step is done.
    pub complete: Condition,
    /// What finishing the step does.
    pub effects: Vec<crate::l4::journal::CampaignEffect>,
    /// Whether finishing the step moves straight on, rather than waiting for a
    /// `complete_step` command.
    ///
    /// `true` for a step whose completion *is* the transition (walking somewhere,
    /// reading something); `false` for one the author wants the player to close
    /// deliberately (a conversation the player chooses to end).
    pub auto_advance: bool,
}

/// A quest record (`02:02.7`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Quest {
    /// The record id.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// The conditions under which the quest starts by itself.
    pub entry: Condition,
    /// The ordered steps.
    pub steps: Vec<QuestStep>,
    /// What finishing the whole quest does.
    pub on_complete: Vec<crate::l4::journal::CampaignEffect>,
    /// What failing it does.
    pub on_fail: Vec<crate::l4::journal::CampaignEffect>,
    /// The faction whose standing the quest's outcome moves, if any.
    pub faction: String,
}

impl Quest {
    /// Read a quest record.
    pub fn parse(id: &str, data: &Json) -> Result<Quest, &'static str> {
        let mut steps = Vec::new();
        let Json::Arr(items) = data.get("steps").ok_or("a quest needs a steps array")? else {
            return Err("a quest needs a steps array");
        };
        for (index, item) in items.iter().enumerate() {
            steps.push(QuestStep {
                id: item
                    .get("id")
                    .and_then(Json::as_str)
                    .map(str::to_string)
                    .unwrap_or_else(|| format!("step-{index}")),
                text_key: item
                    .get("text_key")
                    .and_then(Json::as_str)
                    .ok_or("a quest step needs a text_key")?
                    .to_string(),
                entry: match item.get("entry") {
                    None => Condition::default(),
                    Some(value) => parse_condition(value)?,
                },
                complete: match item.get("complete") {
                    None => Condition::default(),
                    Some(value) => parse_condition(value)?,
                },
                effects: crate::l4::journal::parse_effects(item.get("effects"))?,
                auto_advance: item
                    .get("auto_advance")
                    .and_then(Json::as_bool)
                    .unwrap_or(true),
            });
        }
        if steps.is_empty() {
            return Err("a quest needs at least one step");
        }
        Ok(Quest {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            entry: match data.get("entry") {
                None => Condition::default(),
                Some(value) => parse_condition(value)?,
            },
            steps,
            on_complete: crate::l4::journal::parse_effects(data.get("on_complete"))?,
            on_fail: crate::l4::journal::parse_effects(data.get("on_fail"))?,
            faction: data
                .get("faction")
                .and_then(Json::as_str)
                .unwrap_or("")
                .to_string(),
        })
    }

    /// The step at an index, when it exists.
    pub fn step(&self, index: usize) -> Option<&QuestStep> {
        self.steps.get(index)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_quest_needs_steps_and_each_needs_a_line() {
        let quest = Quest::parse(
            "wsp.quest.missing",
            &Json::from_text(
                r#"{"name_key":"quest.missing","faction":"wsp.faction.watch",
                    "entry":{"flag_set":"heard_rumour"},
                    "steps":[{"id":"ask","text_key":"journal.missing.ask"},
                             {"text_key":"journal.missing.find","auto_advance":false}],
                    "on_complete":[{"standing":{"wsp.faction.watch":10}}]}"#,
            ),
        )
        .expect("parse");
        assert_eq!(quest.steps.len(), 2);
        assert_eq!(quest.steps[0].id, "ask");
        assert_eq!(
            quest.steps[1].id, "step-1",
            "an unnamed step still has an id"
        );
        assert!(!quest.steps[1].auto_advance);
        assert_eq!(quest.on_complete.len(), 1);
        assert_eq!(quest.entry.flag_set.as_deref(), Some("heard_rumour"));
    }

    #[test]
    fn a_quest_with_no_steps_is_refused() {
        assert!(Quest::parse("q", &Json::from_text(r#"{"steps":[]}"#)).is_err());
        assert!(Quest::parse("q", &Json::from_text(r#"{"name_key":"q"}"#)).is_err());
        assert!(Quest::parse("q", &Json::from_text(r#"{"steps":[{"id":"a"}]}"#)).is_err());
    }

    #[test]
    fn a_status_round_trips_through_its_id() {
        for status in QuestStatus::ALL {
            assert_eq!(QuestStatus::from_id(status.id()), Some(status));
        }
        assert_eq!(QuestStatus::from_id("nearly"), None);
    }
}
