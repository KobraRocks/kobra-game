//! L4 — dialogue (`02:02.7`, 05:05.5 M3).
//!
//! > **Dialogue** — a node graph with conditions, player options, skill checks
//! > that resolve through L0, and effects. Skill checks reuse the resolution
//! > primitive; "Persuade" is a skill tag, not a special case.
//!
//! That last clause is the whole design. A dialogue check does not have its own
//! difficulty system: it rolls the Master Table on a Primary Trait, adds the
//! character's skill steps for the tag the option names, and compares the colour
//! to the colour the option requires — exactly what [`meets`] does for an attack
//! with a required colour. An option that needs a `Persuade` check and an attack
//! that needs a Blue both call the same primitive, so the two cannot disagree
//! about what "difficult" means.
//!
//! Two structural properties matter for a CRPG and are enforced here rather than
//! left to the author:
//!
//! - **An unavailable option is shown as unavailable, not hidden.** That is what
//!   makes a skill check legible: the player sees "Persuade (difficult)" greyed
//!   out and understands they lack the skill, instead of wondering why the
//!   conversation has no such branch. [`available_options`] returns both lists.
//! - **A conversation is simulation state.** Which node is open, and for which
//!   character, is in the save (`AD-11`), because a conversation is not a UI
//!   mode — it is where the party is standing and what they have said.

use crate::json::Json;
use crate::l0::Colour;
use crate::l1::character::Character;
use crate::l1::skills::ActionTag;
use crate::l4::condition::{parse as parse_condition, Condition};
use crate::l4::journal::{CampaignEffect, EvaluationContext};
use crate::l4::quests::QuestStatus;
use crate::tables;

/// The trait a dialogue check rolls when an option does not name one.
fn default_check_trait() -> crate::l1::traits::Trait {
    crate::l1::traits::Trait::Awareness
}

/// A skill check that gates a dialogue option (`02:02.7`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct SkillCheck {
    /// The action tag the check's skill bonus comes from (`4c:275`).
    pub tag: ActionTag,
    /// The trait the check rolls.
    pub trait_: crate::l1::traits::Trait,
    /// The colour the roll must reach (`4c:1199`).
    pub required: Colour,
    /// A row-step penalty or bonus the author applies to the check, so a
    /// conversation can be easier for a reason the rules do not know about.
    pub steps: i32,
}

impl SkillCheck {
    /// Read a check record.
    pub fn parse(value: &Json) -> Result<SkillCheck, &'static str> {
        let trait_ = match value.get("trait").and_then(Json::as_str) {
            None => default_check_trait(),
            Some(name) => crate::l1::traits::Trait::from_id(name).ok_or("unknown check trait")?,
        };
        Ok(SkillCheck {
            tag: value
                .get("tag")
                .and_then(Json::as_str)
                .and_then(ActionTag::from_id)
                .ok_or("a check needs an action tag")?,
            trait_,
            required: value
                .get("required")
                .and_then(Json::as_str)
                .and_then(|name| crate::l1::items::colour_from_id(name).ok())
                .unwrap_or(Colour::Blue),
            steps: value.get("steps").and_then(Json::as_i64).unwrap_or(0) as i32,
        })
    }

    /// The roll this check makes, and the colour it produced.
    ///
    /// The band comes from the character's *effective* trait — so a power that
    /// raises Awareness raises the check for free, which is the same path an
    /// attack takes (`02:02.3`).
    pub fn resolve(&self, character: &Character, d100: u8) -> Colour {
        let ladder = tables::CAMPAIGN_LADDER;
        let effective = character.effective(ladder);
        let band = effective.trait_band(self.trait_);
        let steps = self.steps + character.skill_steps(self.tag);
        let adjusted = ladder.row_step(band, steps);
        ladder
            .resolve(ladder.bands[adjusted].lo, d100)
            .unwrap_or(Colour::Blck)
    }
}

/// What an option does that an effect cannot (`02:02.13`).
///
/// An effect moves campaign state; starting a fight needs the whole sim, which
/// the journal's effect applier deliberately does not have. A conversation that
/// offers violence therefore names it here, and only here — which is what makes
/// "Fight is offered during a conversation only when the conversation invites
/// it" an engine rule rather than a UI convention.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum DialogueAction {
    /// Close the conversation and begin this encounter. An empty id is a wild
    /// one, resolved against whoever is standing nearby (`02:02.5`).
    Fight {
        /// The encounter record id, or empty for a wild fight.
        encounter: String,
    },
}

impl DialogueAction {
    /// Read an option's action, or `None` when it has none.
    pub fn parse(value: Option<&Json>) -> Result<Option<DialogueAction>, &'static str> {
        let Some(value) = value else {
            return Ok(None);
        };
        let kind = value
            .get("kind")
            .and_then(Json::as_str)
            .ok_or("an action needs a kind")?;
        match kind {
            "fight" => Ok(Some(DialogueAction::Fight {
                encounter: value
                    .get("encounter")
                    .and_then(Json::as_str)
                    .unwrap_or("")
                    .to_string(),
            })),
            _ => Err("an action kind must be fight"),
        }
    }
}

/// One player option on a node.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DialogueOption {
    /// The stable id.
    pub id: String,
    /// The player-readable line (`AD-22`).
    pub text_key: String,
    /// The conditions under which the option appears at all.
    pub when: Condition,
    /// The check that gates it, when there is one.
    pub check: Option<SkillCheck>,
    /// What taking the option does.
    pub effects: Vec<CampaignEffect>,
    /// The one action that is not an effect: an invited fight (`02:02.13`).
    pub action: Option<DialogueAction>,
    /// The node it moves to, or `None` to end the conversation.
    pub next: Option<String>,
}

/// One node of a conversation.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DialogueNode {
    /// The stable id.
    pub id: String,
    /// The node's own line (`AD-22`).
    pub text_key: String,
    /// The entity id of the speaker, when the line is spoken by someone.
    pub speaker: Option<u32>,
    /// The options, in authored order.
    pub options: Vec<DialogueOption>,
    /// The node to move to when no option is available.
    pub fallback: Option<String>,
}

/// A whole conversation.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Dialogue {
    /// The record id.
    pub id: String,
    /// The display string id, for the UI's title.
    pub name_key: String,
    /// The party member or NPC the conversation is with, as a character record.
    pub partner: String,
    /// The node the conversation opens on.
    pub entry: String,
    /// Every node.
    pub nodes: Vec<DialogueNode>,
}

impl Dialogue {
    /// The node an id names.
    pub fn node(&self, id: &str) -> Option<&DialogueNode> {
        self.nodes.iter().find(|node| node.id == id)
    }

    /// Read a dialogue record.
    pub fn parse(id: &str, data: &Json) -> Result<Dialogue, &'static str> {
        let entry = data
            .get("entry")
            .and_then(Json::as_str)
            .ok_or("a dialogue needs an entry node")?
            .to_string();
        let Json::Arr(items) = data.get("nodes").ok_or("a dialogue needs nodes")? else {
            return Err("a dialogue needs nodes");
        };
        if items.is_empty() {
            return Err("a dialogue needs at least one node");
        }
        let mut nodes = Vec::new();
        for item in items {
            let mut options = Vec::new();
            if let Some(Json::Arr(entries)) = item.get("options") {
                for entry in entries {
                    options.push(DialogueOption {
                        id: entry
                            .get("id")
                            .and_then(Json::as_str)
                            .ok_or("a dialogue option needs an id")?
                            .to_string(),
                        text_key: entry
                            .get("text_key")
                            .and_then(Json::as_str)
                            .ok_or("a dialogue option needs a text_key")?
                            .to_string(),
                        when: match entry.get("when") {
                            None => Condition::default(),
                            Some(value) => parse_condition(value)?,
                        },
                        check: match entry.get("check") {
                            None => None,
                            Some(value) => Some(SkillCheck::parse(value)?),
                        },
                        effects: crate::l4::journal::parse_effects(entry.get("effects"))?,
                        action: DialogueAction::parse(entry.get("action"))?,
                        next: entry.get("next").and_then(Json::as_str).map(str::to_string),
                    });
                }
            }
            if options.is_empty() && item.get("fallback").is_none() {
                return Err("a dialogue node needs options or a fallback");
            }
            nodes.push(DialogueNode {
                id: item
                    .get("id")
                    .and_then(Json::as_str)
                    .ok_or("a dialogue node needs an id")?
                    .to_string(),
                text_key: item
                    .get("text_key")
                    .and_then(Json::as_str)
                    .ok_or("a dialogue node needs a text_key")?
                    .to_string(),
                speaker: item.get("speaker").and_then(Json::as_i64).map(|v| v as u32),
                options,
                fallback: item
                    .get("fallback")
                    .and_then(Json::as_str)
                    .map(str::to_string),
            });
        }
        if !nodes.iter().any(|node| node.id == entry) {
            return Err("a dialogue's entry node must exist");
        }
        for node in &nodes {
            for option in &node.options {
                if let Some(next) = &option.next {
                    if !nodes.iter().any(|candidate| &candidate.id == next) {
                        return Err("a dialogue option names a missing node");
                    }
                }
            }
            if let Some(fallback) = &node.fallback {
                if !nodes.iter().any(|candidate| &candidate.id == fallback) {
                    return Err("a dialogue fallback names a missing node");
                }
            }
        }
        Ok(Dialogue {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            partner: data
                .get("partner")
                .and_then(Json::as_str)
                .unwrap_or("")
                .to_string(),
            entry,
            nodes,
        })
    }
}

/// Evaluate a predicate against the campaign state (`02:02.7`).
///
/// The one evaluator: `quests`, `journal` and `dialogue` all call it, so a
/// condition cannot mean one thing to a quest and another to an option.
pub fn evaluate(condition: &Condition, context: &EvaluationContext<'_>) -> bool {
    if !condition.all.is_empty() && !condition.all.iter().all(|item| evaluate(item, context)) {
        return false;
    }
    if !condition.any.is_empty() && !condition.any.iter().any(|item| evaluate(item, context)) {
        return false;
    }
    if let Some(inner) = &condition.not {
        if evaluate(inner, context) {
            return false;
        }
    }
    if let Some((name, value)) = &condition.flag {
        if context.flags.get(name).copied().unwrap_or(0) != *value {
            return false;
        }
    }
    if let Some(name) = &condition.flag_set {
        if !context.flags.contains_key(name) {
            return false;
        }
    }
    if let Some((faction, required)) = &condition.standing_at_least {
        let standing = match context.faction(faction) {
            Some(record) => context.standings.effective(record),
            None => context.standings.get(faction).unwrap_or(0),
        };
        if standing < *required {
            return false;
        }
    }
    if let Some(required) = condition.repute_at_least {
        let repute = context.actor.map(|actor| actor.repute).unwrap_or(0);
        if repute < required {
            return false;
        }
    }
    if let Some(required) = condition.character_repute_at_least {
        let repute = context.actor.map(|actor| actor.repute).unwrap_or(0);
        if repute < required {
            return false;
        }
    }
    if let Some((faction, lo, hi)) = &condition.standing_between {
        let standing = match context.faction(faction) {
            Some(record) => context.standings.effective(record),
            None => context.standings.get(faction).unwrap_or(0),
        };
        if standing < *lo || standing > *hi {
            return false;
        }
    }
    if let Some(item) = &condition.has_item {
        if !item_is_held(context, item) {
            return false;
        }
    }
    if let Some(item) = &condition.lacks_item {
        if item_is_held(context, item) {
            return false;
        }
    }
    if let Some(power) = &condition.knows_power {
        let knows = context
            .actor
            .map(|actor| actor.powers.iter().any(|held| &held.id == power))
            .unwrap_or(false);
        if !knows {
            return false;
        }
    }
    if let Some(skill) = &condition.has_skill {
        let has = context
            .actor
            .map(|actor| actor.skills.iter().any(|held| &held.skill == skill))
            .unwrap_or(false);
        if !has {
            return false;
        }
    }
    if let Some((quest, statuses)) = &condition.quest_status {
        let status = context.journal.status(quest);
        if !statuses
            .iter()
            .any(|name| QuestStatus::from_id(name) == Some(status))
        {
            return false;
        }
    }
    if let Some((quest, step)) = &condition.step_done {
        let done = context
            .journal
            .get(quest)
            .map(|page| page.fired.contains(step))
            .unwrap_or(false);
        if !done {
            return false;
        }
    }
    if let Some(text_key) = &condition.read {
        let read = context
            .journal
            .lines
            .iter()
            .any(|(_, line)| &line.text_key == text_key && line.read);
        if !read {
            return false;
        }
    }
    if let Some(day) = condition.day_at_least {
        if context.clock.day < day {
            return false;
        }
    }
    if let Some(slots) = &condition.time_of_day {
        if !slots.contains(&context.clock.slot()) {
            return false;
        }
    }
    if let Some(alive) = condition.alive {
        let is_alive = context.actor.map(|actor| !actor.dead).unwrap_or(true);
        if is_alive != alive {
            return false;
        }
    }
    true
}

/// Whether the acting character carries an item.
fn item_is_held(context: &EvaluationContext<'_>, item: &str) -> bool {
    context
        .actor
        .map(|actor| actor.items.iter().any(|held| held == item))
        .unwrap_or(false)
}

/// An option as the UI sees it: available, or unavailable and why.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct OptionView {
    /// The option's id.
    pub id: String,
    /// Its line (`AD-22`).
    pub text_key: String,
    /// Whether the author's `when` conditions hold, so the option is offered at
    /// all.
    pub offered: bool,
    /// The colour a check requires, when the option has one.
    pub required: Option<Colour>,
    /// The action tag the check draws its skill bonus from.
    pub tag: Option<ActionTag>,
    /// The fight the option invites, when it invites one (`02:02.13`).
    pub action: Option<DialogueAction>,
}

impl OptionView {
    /// Whether the character can take the option right now.
    ///
    /// A check never makes an option *unavailable* — that is what `when` is for.
    /// A check changes what happens when it is taken, which is why it is reported
    /// and resolved rather than pre-filtered.
    pub fn available(&self) -> bool {
        self.offered
    }
}

/// The options a node offers a character, split into offered and withheld.
///
/// Both lists are returned on purpose. An option whose `when` conditions do not
/// hold is *withheld* — the player is not shown a branch the story has not
/// reached — while an option that is offered but carries a check is shown with
/// the colour it needs, so the player learns the rule rather than guessing why
/// the conversation has no such line (`02:02.11` rule 2).
pub fn available_options(
    node: &DialogueNode,
    context: &EvaluationContext<'_>,
) -> (Vec<OptionView>, Vec<OptionView>) {
    let mut offered = Vec::new();
    let mut withheld = Vec::new();
    for option in &node.options {
        let holds = evaluate(&option.when, context);
        let view = OptionView {
            id: option.id.clone(),
            text_key: option.text_key.clone(),
            offered: holds,
            required: option.check.as_ref().map(|check| check.required),
            tag: option.check.as_ref().map(|check| check.tag),
            action: option.action.clone(),
        };
        if holds {
            offered.push(view);
        } else {
            withheld.push(view);
        }
    }
    (offered, withheld)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l1::skills::ActionTag;
    use crate::l3::clock::Clock;
    use crate::l4::factions::{Faction, Standings};
    use crate::l4::journal::Journal;

    fn dialogue() -> Dialogue {
        Dialogue::parse(
            "wsp.dialogue.watchman",
            &Json::from_text(
                r#"{"name_key":"dialogue.watchman","partner":"wsp.character.watchman","entry":"open",
                    "nodes":[
                      {"id":"open","text_key":"dialogue.watchman.greet","options":[
                         {"id":"ask","text_key":"dialogue.watchman.ask",
                          "effects":[{"flag":{"heard_rumour":1}}],"next":"rumour"},
                         {"id":"persuade","text_key":"dialogue.watchman.persuade",
                          "check":{"tag":"persuade","trait":"willpower","required":"Blue"},
                          "effects":[{"standing":{"wsp.faction.watch":5}}],"next":"rumour"},
                         {"id":"leave","text_key":"dialogue.leave"}]},
                      {"id":"rumour","text_key":"dialogue.watchman.rumour","fallback":null,
                       "options":[{"id":"thanks","text_key":"dialogue.thanks"}]}]}"#,
            ),
        )
        .expect("dialogue")
    }

    #[test]
    fn a_dialogue_graph_refuses_a_dangling_edge() {
        assert!(Dialogue::parse(
            "d",
            &Json::from_text(
                r#"{"entry":"a","nodes":[{"id":"a","text_key":"t","options":[
                    {"id":"x","text_key":"x","next":"missing"}]}]}"#
            )
        )
        .is_err());
        assert!(Dialogue::parse(
            "d",
            &Json::from_text(r#"{"entry":"missing","nodes":[{"id":"a","text_key":"t","fallback":null,"options":[]}]}"#)
        )
        .is_err());
        assert!(Dialogue::parse("d", &Json::from_text(r#"{"entry":"a","nodes":[]}"#)).is_err());
    }

    #[test]
    fn the_entry_node_exists_and_its_options_are_read() {
        let dialogue = dialogue();
        let node = dialogue.node("open").expect("node");
        assert_eq!(node.options.len(), 3);
        assert_eq!(
            node.options[1].check.as_ref().map(|c| c.required),
            Some(Colour::Blue)
        );
        assert_eq!(node.options[0].next.as_deref(), Some("rumour"));
        assert_eq!(node.options[2].next, None, "leaving ends the conversation");
    }

    #[test]
    fn a_gated_option_is_unavailable_not_invisible() {
        let dialogue = dialogue();
        let node = dialogue.node("open").expect("node");
        let actor = Character::authored(1, "hero", "", 0, [10; 7], 10, 0, vec![], vec![], vec![]);
        let flags = std::collections::BTreeMap::new();
        let standings = Standings::default();
        let journal = Journal::default();
        let clock = Clock::new(1440);
        let factions: Vec<Faction> = Vec::new();
        let items: Vec<String> = Vec::new();
        let context = EvaluationContext {
            actor: Some(&actor),
            flags: &flags,
            standings: &standings,
            journal: &journal,
            clock: &clock,
            factions: &factions,
            item_ids: &items,
        };
        let (available, unavailable) = available_options(node, &context);
        assert_eq!(available.len(), 3, "all three are offered to the player");
        assert!(unavailable.is_empty());
        assert_eq!(available[1].required, Some(Colour::Blue));
        assert_eq!(available[1].tag, Some(ActionTag::Persuade));

        // A condition that does not hold withholds the option entirely, which is
        // the difference between "gated by a check" and "the story has not got
        // here yet".
        let gated = DialogueNode {
            id: "n".into(),
            text_key: "t".into(),
            speaker: None,
            options: vec![DialogueOption {
                id: "secret".into(),
                text_key: "s".into(),
                when: Condition {
                    flag_set: Some("knows_secret".into()),
                    ..Condition::default()
                },
                check: None,
                effects: vec![],
                action: None,
                next: None,
            }],
            fallback: None,
        };
        let (available, withheld) = available_options(&gated, &context);
        assert!(available.is_empty());
        assert_eq!(withheld.len(), 1, "the option exists but is not offered");
        assert!(!withheld[0].available());
    }

    #[test]
    fn a_skill_check_rolls_through_the_resolution_primitive() {
        // 4c:275: a skill is a row-step bonus, and a dialogue check uses the same
        // primitive an attack does, so the two cannot disagree.
        let check = SkillCheck {
            tag: ActionTag::Persuade,
            trait_: crate::l1::traits::Trait::Willpower,
            required: Colour::Blue,
            steps: 0,
        };
        let plain = Character::authored(
            1,
            "hero",
            "",
            0,
            [10, 10, 10, 10, 10, 10, 10],
            10,
            0,
            vec![],
            vec![],
            vec![],
        );
        let mut skilled = plain.clone();
        skilled.skills.push(crate::l1::skills::SkillInstance {
            skill: "wsp.skill.negotiation".into(),
            steps: 2,
            applies_to: vec![ActionTag::Persuade],
        });
        // Every roll is enumerable, so the skilled character is never worse.
        let mut better = 0;
        let mut worse = 0;
        for roll in 0u8..100 {
            let plain_colour = check.resolve(&plain, roll).code();
            let skilled_colour = check.resolve(&skilled, roll).code();
            if skilled_colour > plain_colour {
                better += 1;
            }
            if skilled_colour < plain_colour {
                worse += 1;
            }
        }
        assert!(better > 0, "the skill helps on some rolls");
        assert_eq!(worse, 0, "the skill never hurts");
    }
}
