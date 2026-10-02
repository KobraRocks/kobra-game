//! L4 — the one predicate vocabulary (`02:02.7`, 05:05.5 M3).
//!
//! Quests, dialogue options and schedules all need to ask the same questions:
//! *is this flag set, is the party's standing with that faction high enough, has
//! this step already happened, does this character carry that item.* Rather than
//! three predicate languages that drift apart, there is one, and it is **data**.
//!
//! Two structural facts make it small and worth having:
//!
//! - **It is evaluated against the merged state, never against content.** A
//!   predicate can only read what is already simulation state — the flags, the
//!   standing, the journal, the party's sheets — so a mod cannot use a quest
//!   condition to reach into another pack's records (`03:03.9` rule 2).
//! - **A predicate is never a formula.** There is no arithmetic here beyond a
//!   comparison against an integer the author wrote. Anything that needs to hurt
//!   or heal someone is an effect resolved through L2 (`02:02.7`), not a
//!   condition.
//!
//! The vocabulary is deliberately closed and enumerable, so the validator can
//! reject an unknown key at load instead of silently treating it as `false` —
//! a quest that never starts because a condition key was misspelled is exactly
//! the class of bug `AD-15` exists to prevent.

use crate::json::Json;
use crate::l3::clock::DaySlot;

/// One predicate over the campaign state.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Condition {
    /// Every listed predicate must hold.
    pub all: Vec<Condition>,
    /// At least one listed predicate must hold.
    pub any: Vec<Condition>,
    /// This predicate must not hold.
    pub not: Option<Box<Condition>>,
    /// A campaign flag equals this value.
    pub flag: Option<(String, i64)>,
    /// A campaign flag is set at all, whatever its value.
    pub flag_set: Option<String>,
    /// A faction's standing is at least this.
    pub standing_at_least: Option<(String, i32)>,
    /// A faction's reputation toward the party, which the public-reaction table
    /// reads.
    pub repute_at_least: Option<i32>,
    /// The acting character's Repute score (`4c:204-208`) is at least this.
    pub character_repute_at_least: Option<i32>,
    /// The character carries this item record.
    pub has_item: Option<String>,
    /// The character does not carry this item record.
    pub lacks_item: Option<String>,
    /// The character knows this power record.
    pub knows_power: Option<String>,
    /// The character has had this skill.
    pub has_skill: Option<String>,
    /// The quest has reached one of these statuses.
    pub quest_status: Option<(String, Vec<String>)>,
    /// This quest step has fired.
    pub step_done: Option<(String, usize)>,
    /// Someone has read this journal entry.
    pub read: Option<String>,
    /// A faction has a standing within these bounds (inclusive), for a band.
    pub standing_between: Option<(String, i32, i32)>,
    /// The clock's day is at least this.
    pub day_at_least: Option<u32>,
    /// The clock's time of day is one of these quarters (`02:02.7`).
    pub time_of_day: Option<Vec<DaySlot>>,
    /// The character is dead or alive; `true` means "must be alive".
    pub alive: Option<bool>,
}

/// Read one predicate from content.
///
/// An unknown key is an error, not an ignored line: a silently dropped condition
/// turns a guarded option into an unguarded one, which is how a dialogue tree
/// ends up offering an option the story has not reached.
pub fn parse(value: &Json) -> Result<Condition, &'static str> {
    let Json::Obj(fields) = value else {
        return Err("a condition must be an object");
    };
    let mut condition = Condition::default();
    for (key, child) in fields {
        match key.as_str() {
            "all" => condition.all = parse_list(child)?,
            "any" => condition.any = parse_list(child)?,
            "not" => condition.not = Some(Box::new(parse(child)?)),
            "flag" => condition.flag = Some(parse_flag(child)?),
            "flag_set" => condition.flag_set = Some(string(child)?.to_string()),
            "standing_at_least" => condition.standing_at_least = Some(parse_standing(child)?),
            "repute_at_least" => condition.repute_at_least = Some(int(child)?),
            "character_repute_at_least" => condition.character_repute_at_least = Some(int(child)?),
            "has_item" => condition.has_item = Some(string(child)?.to_string()),
            "lacks_item" => condition.lacks_item = Some(string(child)?.to_string()),
            "knows_power" => condition.knows_power = Some(string(child)?.to_string()),
            "has_skill" => condition.has_skill = Some(string(child)?.to_string()),
            "quest_status" => condition.quest_status = Some(parse_status_list(child)?),
            "step_done" => condition.step_done = Some(parse_step(child)?),
            "read" => condition.read = Some(string(child)?.to_string()),
            "standing_between" => condition.standing_between = Some(parse_between(child)?),
            "day_at_least" => {
                let value = int(child)?;
                if value < 0 {
                    return Err("day_at_least must not be negative");
                }
                condition.day_at_least = Some(value as u32);
            }
            "time_of_day" => {
                let Json::Arr(items) = child else {
                    return Err("time_of_day must be an array of slots");
                };
                let mut slots = Vec::new();
                for item in items {
                    slots.push(
                        item.as_str()
                            .and_then(DaySlot::from_id)
                            .ok_or("time_of_day entries must be night, morning, day or evening")?,
                    );
                }
                condition.time_of_day = Some(slots);
            }
            "alive" => {
                condition.alive = Some(child.as_bool().ok_or("alive must be true or false")?)
            }
            _ => return Err("unknown condition key"),
        }
    }
    Ok(condition)
}

/// Read a list of nested predicates.
fn parse_list(value: &Json) -> Result<Vec<Condition>, &'static str> {
    let Json::Arr(items) = value else {
        return Err("all/any must be an array of conditions");
    };
    let mut out = Vec::new();
    for item in items {
        out.push(parse(item)?);
    }
    Ok(out)
}

/// Read a `{flag: value}` pair, accepting either spelling of the object form.
fn parse_flag(value: &Json) -> Result<(String, i64), &'static str> {
    let Json::Obj(fields) = value else {
        return Err("flag must be {\"name\": value}");
    };
    let (name, setting) = fields.iter().next().ok_or("flag must name one flag")?;
    Ok((name.clone(), setting.as_i64().unwrap_or(0)))
}

/// Read a `{faction: standing}` pair.
fn parse_standing(value: &Json) -> Result<(String, i32), &'static str> {
    let Json::Obj(fields) = value else {
        return Err("standing_at_least must be {\"faction\": standing}");
    };
    let (faction, standing) = fields
        .iter()
        .next()
        .ok_or("standing_at_least must name one faction")?;
    Ok((
        faction.clone(),
        standing.as_i64().ok_or("a standing must be a number")? as i32,
    ))
}

/// Read a `{faction, lo, hi}` band.
fn parse_between(value: &Json) -> Result<(String, i32, i32), &'static str> {
    Ok((
        value
            .get("faction")
            .and_then(Json::as_str)
            .ok_or("standing_between needs a faction")?
            .to_string(),
        value
            .get("lo")
            .and_then(Json::as_i64)
            .ok_or("standing_between needs lo")? as i32,
        value
            .get("hi")
            .and_then(Json::as_i64)
            .ok_or("standing_between needs hi")? as i32,
    ))
}

/// Read a `{quest: [statuses]}` pair.
fn parse_status_list(value: &Json) -> Result<(String, Vec<String>), &'static str> {
    let Json::Obj(fields) = value else {
        return Err("quest_status must be {\"quest\": [statuses]}");
    };
    let (quest, list) = fields
        .iter()
        .next()
        .ok_or("quest_status must name one quest")?;
    let Json::Arr(items) = list else {
        return Err("quest_status needs an array of statuses");
    };
    let mut statuses = Vec::new();
    for item in items {
        statuses.push(
            item.as_str()
                .ok_or("a quest status must be a string")?
                .to_string(),
        );
    }
    Ok((quest.clone(), statuses))
}

/// Read a `{quest, step}` pair.
fn parse_step(value: &Json) -> Result<(String, usize), &'static str> {
    let step = value
        .get("step")
        .and_then(Json::as_i64)
        .ok_or("step_done needs a step index")?;
    if step < 0 {
        return Err("step_done needs a step index that is not negative");
    }
    Ok((
        value
            .get("quest")
            .and_then(Json::as_str)
            .ok_or("step_done needs a quest")?
            .to_string(),
        step as usize,
    ))
}

/// A required string.
fn string(value: &Json) -> Result<&str, &'static str> {
    value.as_str().ok_or("expected a string")
}

/// A required integer.
fn int(value: &Json) -> Result<i32, &'static str> {
    Ok(value.as_i64().ok_or("expected a number")? as i32)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_condition_key_round_trips_through_the_parser() {
        let document = Json::from_text(
            r#"{"all":[
                 {"flag":{"met_contact":1}},
                 {"flag_set":"met_contact"},
                 {"standing_at_least":{"wsp.faction.watch":5}},
                 {"repute_at_least":10},
                 {"character_repute_at_least":3},
                 {"has_item":"wsp.item.bat"},
                 {"lacks_item":"wsp.item.pistol"},
                 {"knows_power":"wsp.power.body-armor"},
                 {"has_skill":"wsp.skill.marksmanship"},
                 {"quest_status":{"wsp.quest.missing":["active","completed"]}},
                 {"step_done":{"quest":"wsp.quest.missing","step":1}},
                 {"read":"journal.missing.found"},
                 {"standing_between":{"faction":"wsp.faction.watch","lo":0,"hi":10}},
                 {"day_at_least":2},
                 {"time_of_day":["night","evening"]},
                 {"alive":true},
                 {"not":{"flag_set":"dead"}}
               ]}"#,
        );
        let condition = parse(&document).expect("parse");
        assert_eq!(condition.all.len(), 17);
        assert_eq!(condition.all[0].flag, Some(("met_contact".into(), 1)));
        assert_eq!(condition.all[15].alive, Some(true));
        assert!(condition.all[16].not.is_some());
    }

    #[test]
    fn an_unknown_condition_key_is_refused_rather_than_ignored() {
        // A silently dropped condition turns a guarded option into an unguarded
        // one (AD-15).
        assert!(parse(&Json::from_text(r#"{"standing_ate_least":{"x":1}}"#)).is_err());
        assert!(parse(&Json::from_text(r#"{"all":{"flag_set":"x"}}"#)).is_err());
        assert!(parse(&Json::from_text(r#"{"time_of_day":["teatime"]}"#)).is_err());
        assert!(parse(&Json::from_text(r#"{"day_at_least":-1}"#)).is_err());
        assert!(parse(&Json::from_text(r#"[]"#)).is_err());
    }
}
