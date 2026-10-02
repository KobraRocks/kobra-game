//! L1 — skills (`4c:275-283`, 02:02.3).
//!
//! A skill is a named row-step bonus on "an action appropriate to the skill",
//! which the rules leave to the Gamemaster and a CRPG must make data. A skill is
//! `{id, name, steps, applies_to}` where `applies_to` is a closed enum owned by
//! the engine (`02:02.3`): the `+2` advanced skill is the same mechanism with
//! `steps: 2`, so a campaign decides the slot *cost* and the engine owns the
//! *effect*.

use crate::json::Json;

/// The closed set of action tags a skill can apply to (`02:02.3`).
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug, Hash)]
pub enum ActionTag {
    /// Close combat.
    Melee,
    /// Attacks across a distance.
    Ranged,
    /// Avoiding an attack.
    Dodge,
    /// Talking someone round.
    Persuade,
    /// Searching, reading a scene, research.
    Investigate,
    /// Driving or piloting a vehicle.
    Pilot,
    /// Moving unseen.
    Stealth,
    /// Technical or academic work.
    Science,
}

impl ActionTag {
    /// Every tag, for a validator to enumerate.
    pub const ALL: [ActionTag; 8] = [
        ActionTag::Melee,
        ActionTag::Ranged,
        ActionTag::Dodge,
        ActionTag::Persuade,
        ActionTag::Investigate,
        ActionTag::Pilot,
        ActionTag::Stealth,
        ActionTag::Science,
    ];

    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            ActionTag::Melee => "melee",
            ActionTag::Ranged => "ranged",
            ActionTag::Dodge => "dodge",
            ActionTag::Persuade => "persuade",
            ActionTag::Investigate => "investigate",
            ActionTag::Pilot => "pilot",
            ActionTag::Stealth => "stealth",
            ActionTag::Science => "science",
        }
    }

    /// The tag a content id names.
    pub fn from_id(id: &str) -> Option<ActionTag> {
        ActionTag::ALL.into_iter().find(|tag| tag.id() == id)
    }
}

/// A skill record (`4c:275`, `4c:283`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Skill {
    /// The content id, e.g. `wsp.skill.martial-arts`.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// The row-step bonus: `1` for a skill, `2` for an advanced skill, `3` for
    /// a skill promoted by `Improved Skills` (`D13`: absolute, never stacked).
    pub steps: i32,
    /// The actions it applies to.
    pub applies_to: Vec<ActionTag>,
    /// Slot cost, so `Improved Skills` can rewrite it (`4c:638`).
    pub slots: i32,
}

impl Skill {
    /// Read a skill record.
    pub fn parse(id: &str, data: &Json) -> Result<Skill, &'static str> {
        let mut applies_to = Vec::new();
        match data.get("applies_to") {
            Some(Json::Arr(values)) => {
                for value in values {
                    let tag = ActionTag::from_id(value.as_str().ok_or("tags must be strings")?)
                        .ok_or("unknown action tag")?;
                    if !applies_to.contains(&tag) {
                        applies_to.push(tag);
                    }
                }
            }
            _ => return Err("a skill needs an applies_to list"),
        }
        if applies_to.is_empty() {
            return Err("a skill must apply to at least one action");
        }
        Ok(Skill {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            steps: data.get("steps").and_then(Json::as_i64).unwrap_or(1) as i32,
            applies_to,
            slots: data.get("slots").and_then(Json::as_i64).unwrap_or(1) as i32,
        })
    }
}

/// A skill on a character sheet.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct SkillInstance {
    /// The record id.
    pub skill: String,
    /// The applied row-step bonus (absolute, `D13`).
    pub steps: i32,
    /// The actions it applies to, copied from the record so a save is
    /// self-describing.
    pub applies_to: Vec<ActionTag>,
}

impl SkillInstance {
    /// The row step this skill contributes to `tag`.
    pub fn steps_for(&self, tag: ActionTag) -> i32 {
        if self.applies_to.contains(&tag) {
            self.steps
        } else {
            0
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_skill_and_its_tags() {
        let skill = Skill::parse(
            "wsp.skill.martial-arts",
            &Json::from_text(
                r#"{"name_key":"skill.martial-arts","steps":1,"applies_to":["melee","dodge"]}"#,
            ),
        )
        .expect("parse");
        assert_eq!(skill.steps, 1);
        assert_eq!(skill.applies_to, vec![ActionTag::Melee, ActionTag::Dodge]);
        assert!(Skill::parse("x", &Json::from_text(r#"{"applies_to":["flying"]}"#)).is_err());
        assert!(Skill::parse("x", &Json::from_text(r#"{"applies_to":[]}"#)).is_err());
    }

    #[test]
    fn an_instance_only_contributes_to_its_own_actions() {
        let instance = SkillInstance {
            skill: "s".into(),
            steps: 2,
            applies_to: vec![ActionTag::Melee],
        };
        assert_eq!(instance.steps_for(ActionTag::Melee), 2);
        assert_eq!(instance.steps_for(ActionTag::Ranged), 0);
    }
}
