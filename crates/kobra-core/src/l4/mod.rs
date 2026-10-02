//! The campaign layer (L4) — the CRPG frame (`02:02.7`, 05:05.5 M3).
//!
//! 4C deliberately says nothing about campaigns, quests or dialogue
//! (`4c:1188-1190`), and a CRPG is mostly this. The layer is **content-driven and
//! stored as state**, and it has exactly one invariant that matters:
//!
//! > **The campaign layer never contains a damage formula** (`02:02.7`).
//!
//! If a quest needs to hurt someone it emits an [`Attack`](crate::l2::actions)
//! or an effect into L2, through the same command stream the player's UI drives.
//! That is what keeps one resolution path in the game, and it is why every module
//! here is *data plus predicates* rather than mechanics.
//!
//! | Module | Owns |
//! |---|---|
//! | [`economy`] | Prices by Lifestyle band, what a merchant will buy, what the party can afford (`4c:1255-1272`) |
//! | [`progression`] | Fortune as the currency of advancement, and the Fortune/Repute gain tables (`4c:1229-1251`, `4c:1381-1391`) |
//! | [`factions`] | Per-faction standing, and the public-reaction table with its criminal polarity (`4c:1274-1292`) |
//! | [`condition`] | The one predicate vocabulary dialogue, quests and schedules share |
//! | [`quests`] | The quest graph: steps, entry and completion conditions, effects |
//! | [`journal`] | The player-readable projection of quest and dialogue state |
//! | [`dialogue`] | The node graph, its options, and the checks that gate them |
//! | [`signals`] | Named display gates content declares and the engine evaluates (`09:09.4`) |

pub mod condition;
pub mod dialogue;
pub mod economy;
pub mod factions;
pub mod journal;
pub mod progression;
pub mod quests;
pub mod signals;
