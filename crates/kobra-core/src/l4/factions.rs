//! L4 — factions and standing (`02:02.6`, `4c:1274-1292`).
//!
//! The layering is explicit in the architecture and worth restating, because
//! collapsing it is the obvious mistake:
//!
//! > **Faction standing is L4 campaign state; Repute is the L1 trait**, and
//! > reputation-affecting events update both according to content rules.
//!
//! So a character carries a `repute` score — a number the 4C rules define, on the
//! same 0–33 scale the public-reaction table expects — and the *campaign* carries
//! a standing per faction. An authored event can move either or both, and the
//! gain/loss amounts are content (`D20`), never a constant in Rust.
//!
//! The public-reaction table itself (`4c:1276-1292`) is reused for faction
//! reactions with a per-faction modifier, and the "reversed for criminals" rule
//! is a content flag on the faction — `reaction_polarity: criminal` — not a code
//! branch (`02:02.6`, `4c:1292`).
//!
//! Two readings of that table are decisions, recorded in the register:
//!
//! - **`D4`** — the table's roll "never names a Rank Value", so the reaction roll
//!   resolves at the **Repute score treated as a Rank Value**, clamped into the
//!   ladder. Repute is 0–33, so it lands in the low bands and reads correctly at
//!   the table.
//! - **`D20`** — the Repute gain/loss table is missing from this edition, so one
//!   is authored as content in the same shape as the Fortune scale.

use crate::json::Json;
use crate::l0::Colour;

/// A faction record (`02:02.6`).
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Faction {
    /// The record id.
    pub id: String,
    /// The display string id (`AD-22`).
    pub name_key: String,
    /// The standing a character starts at when nothing else says.
    pub default_standing: i32,
    /// Whether the reaction scale is reversed for this faction (`4c:1292`).
    pub criminal: bool,
    /// A flat shift to the reaction roll, so a faction can read as suspicious.
    pub reaction_modifier: i32,
    /// The bands that name a standing, for the UI and for conditions.
    pub bands: Vec<StandingBand>,
}

/// One named band of standing, so "hostile" is content rather than a threshold
/// in Rust.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct StandingBand {
    /// The band's id, e.g. `hostile`.
    pub id: String,
    /// Lowest standing in the band, inclusive.
    pub lo: i32,
    /// Highest standing in the band, inclusive.
    pub hi: i32,
    /// The faction's disposition here: whether its members engage on sight.
    pub hostile: bool,
}

impl Faction {
    /// Read a faction record.
    pub fn parse(id: &str, data: &Json) -> Result<Faction, &'static str> {
        let mut bands = Vec::new();
        if let Some(Json::Arr(items)) = data.get("bands") {
            for item in items {
                bands.push(StandingBand {
                    id: item
                        .get("id")
                        .and_then(Json::as_str)
                        .ok_or("a standing band needs an id")?
                        .to_string(),
                    lo: item.get("lo").and_then(Json::as_i64).unwrap_or(0) as i32,
                    hi: item.get("hi").and_then(Json::as_i64).unwrap_or(0) as i32,
                    hostile: item.get("hostile").and_then(Json::as_bool).unwrap_or(false),
                });
            }
        }
        Ok(Faction {
            id: id.to_string(),
            name_key: data
                .get("name_key")
                .and_then(Json::as_str)
                .unwrap_or(id)
                .to_string(),
            default_standing: data
                .get("default_standing")
                .and_then(Json::as_i64)
                .unwrap_or(0) as i32,
            criminal: data
                .get("reaction_polarity")
                .and_then(Json::as_str)
                .map(|value| value == "criminal")
                .unwrap_or(false),
            reaction_modifier: data
                .get("reaction_modifier")
                .and_then(Json::as_i64)
                .unwrap_or(0) as i32,
            bands,
        })
    }

    /// The band a standing falls in.
    pub fn band(&self, standing: i32) -> Option<&StandingBand> {
        self.bands
            .iter()
            .find(|band| standing >= band.lo && standing <= band.hi)
    }

    /// Whether this faction's members engage the party on sight at a standing.
    pub fn hostile_at(&self, standing: i32) -> bool {
        self.band(standing).is_some_and(|band| band.hostile)
    }
}

/// What the party's standing is with every faction, and who knows them.
///
/// Sorted by faction id, because a `BTreeMap`'s order is the state hash's and the
/// save's order — an unordered map would make a replay depend on hashing.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Standings {
    entries: std::collections::BTreeMap<String, i32>,
    /// Factions whose members have met the party, for the journal and the UI.
    known: std::collections::BTreeSet<String>,
}

impl Standings {
    /// The standing with a faction, or `None` when the campaign has never
    /// recorded one.
    pub fn get(&self, faction: &str) -> Option<i32> {
        self.entries.get(faction).copied()
    }

    /// The standing with a faction, falling back to the record's default.
    pub fn effective(&self, faction: &Faction) -> i32 {
        self.get(&faction.id).unwrap_or(faction.default_standing)
    }

    /// Every recorded standing, in faction-id order.
    pub fn all(&self) -> impl Iterator<Item = (&String, &i32)> {
        self.entries.iter()
    }

    /// Whether the party has met a faction at all.
    pub fn is_known(&self, faction: &str) -> bool {
        self.known.contains(faction)
    }

    /// Move a standing, recording the faction as met. Returns the new value.
    ///
    /// `D20`: the amount is authored on the event that causes it, exactly as the
    /// Fortune gain table is; there is no built-in "killing a guard costs 10".
    pub fn adjust(&mut self, faction: &str, delta: i32, default: i32) -> i32 {
        let entry = self.entries.entry(faction.to_string()).or_insert(default);
        *entry += delta;
        self.known.insert(faction.to_string());
        *entry
    }

    /// Set a standing outright.
    pub fn set(&mut self, faction: &str, value: i32) {
        self.entries.insert(faction.to_string(), value);
        self.known.insert(faction.to_string());
    }

    /// Write the standings into a save stream.
    pub fn write(&self, out: &mut Vec<u8>) {
        crate::save::write_u16(out, self.entries.len().min(u16::MAX as usize) as u16);
        for (faction, standing) in &self.entries {
            crate::save::write_text(out, faction);
            out.extend_from_slice(&standing.to_le_bytes());
        }
        crate::save::write_u16(out, self.known.len().min(u16::MAX as usize) as u16);
        for faction in &self.known {
            crate::save::write_text(out, faction);
        }
    }

    /// Read the standings from a save stream.
    pub fn read(reader: &mut crate::save::Reader<'_>) -> Result<Standings, &'static str> {
        let mut standings = Standings::default();
        let count = reader.u16()? as usize;
        for _ in 0..count {
            let faction = reader.text()?;
            let standing = reader.i32()?;
            standings.entries.insert(faction, standing);
        }
        let known = reader.u16()? as usize;
        for _ in 0..known {
            standings.known.insert(reader.text()?);
        }
        Ok(standings)
    }

    /// The standings as a document, for the shell's faction panel.
    pub fn to_json(&self) -> Json {
        Json::obj([
            (
                "standings",
                Json::arr(self.entries.iter().map(|(faction, standing)| {
                    Json::obj([
                        ("faction", Json::string(faction)),
                        ("standing", Json::Num(i64::from(*standing))),
                        ("known", Json::Bool(self.known.contains(faction))),
                    ])
                })),
            ),
            ("known", Json::arr(self.known.iter().map(Json::string))),
        ])
    }
}

/// The public-reaction outcome, read from the colours of `4c:1283-1292`.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Reaction {
    /// The faction turns hostile.
    Unfavourable,
    /// It tolerates the party.
    Favourable,
    /// It helps.
    VeryFavourable,
    /// It helps gladly.
    ExtremelyFavourable,
}

impl Reaction {
    /// The stable content id.
    pub const fn id(self) -> &'static str {
        match self {
            Reaction::Unfavourable => "unfavourable",
            Reaction::Favourable => "favourable",
            Reaction::VeryFavourable => "very_favourable",
            Reaction::ExtremelyFavourable => "extremely_favourable",
        }
    }
}

/// Read a public-reaction roll (`4c:1276-1292`).
///
/// The colour comes from the ordinary resolution primitive at the **Repute score
/// treated as a Rank Value** (`D4`), and the criminal polarity reverses the
/// scale (`4c:1292`) — the one place in the rulebook where a high result is bad.
pub fn reaction(colour: Colour, criminal: bool) -> Reaction {
    let step = colour.code() as i32;
    let stepped = if criminal { 3 - step } else { step };
    match stepped {
        0 => Reaction::Unfavourable,
        1 => Reaction::Favourable,
        2 => Reaction::VeryFavourable,
        _ => Reaction::ExtremelyFavourable,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::l4::condition;

    fn faction() -> Faction {
        Faction::parse(
            "wsp.faction.watch",
            &Json::from_text(
                r#"{"name_key":"faction.watch","default_standing":0,
                    "bands":[{"id":"hostile","lo":-100,"hi":-1,"hostile":true},
                             {"id":"neutral","lo":0,"hi":9},
                             {"id":"friendly","lo":10,"hi":100}]}"#,
            ),
        )
        .expect("parse")
    }

    #[test]
    fn a_standing_band_decides_whether_a_faction_engages_on_sight() {
        let faction = faction();
        assert!(faction.hostile_at(-5));
        assert!(!faction.hostile_at(0));
        assert!(!faction.hostile_at(50));
        assert_eq!(
            faction.band(12).map(|band| band.id.as_str()),
            Some("friendly")
        );
    }

    #[test]
    fn the_criminal_polarity_reverses_the_reaction_scale() {
        // 4c:1292: the same roll reads the opposite way for a criminal faction.
        assert_eq!(reaction(Colour::Yel, false), Reaction::ExtremelyFavourable);
        assert_eq!(reaction(Colour::Yel, true), Reaction::Unfavourable);
        assert_eq!(reaction(Colour::Blck, true), Reaction::ExtremelyFavourable);
        assert_eq!(reaction(Colour::Blck, false), Reaction::Unfavourable);
    }

    #[test]
    fn an_adjustment_records_the_faction_as_met() {
        let mut standings = Standings::default();
        assert_eq!(standings.effective(&faction()), 0, "the record's default");
        assert_eq!(standings.adjust("wsp.faction.watch", -3, 0), -3);
        assert!(standings.is_known("wsp.faction.watch"));
        assert_eq!(standings.adjust("wsp.faction.watch", 10, 0), 7);
    }

    #[test]
    fn standings_round_trip_through_a_save_stream() {
        let mut standings = Standings::default();
        standings.adjust("wsp.faction.watch", 4, 0);
        standings.set("noir.guild", -9);
        let mut bytes = Vec::new();
        standings.write(&mut bytes);
        let mut reader = crate::save::Reader::new(&bytes);
        let restored = Standings::read(&mut reader).expect("read");
        assert_eq!(restored, standings);
        assert!(reader.at_end());
    }

    #[test]
    fn a_condition_reads_a_standing() {
        let document = Json::from_text(r#"{"standing_at_least":{"wsp.faction.watch":5}}"#);
        let parsed = condition::parse(&document).expect("parse");
        assert_eq!(
            parsed.standing_at_least,
            Some(("wsp.faction.watch".to_string(), 5))
        );
    }
}
