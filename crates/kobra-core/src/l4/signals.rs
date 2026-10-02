//! L4 — named signals: content-declared display gates (`09:09.4`).
//!
//! A signal is the engine's answer to "should this piece of interface be shown",
//! and it exists so the shell never evaluates a predicate itself. The obvious
//! alternative — a layout carrying a condition the shell interprets — would be a
//! second implementation of [`crate::l4::condition`] in TypeScript, drifting from
//! the engine, which is the failure `AD-15` exists to prevent.
//!
//! So a signal is a **content record** whose body is the same condition
//! vocabulary a quest or a dialogue option uses, and the engine publishes the
//! truth values:
//!
//! ```json
//! { "id": "noir.signal.heard-rumour", "type": "signal",
//!   "data": { "when": { "flag_set": "heard_rumour" } } }
//! ```
//!
//! Three properties follow from that and are the whole reason for the design:
//!
//! - **One evaluator.** The engine's, so a quest and a HUD cannot disagree about
//!   what `standing >= 10` means.
//! - **Save-neutral.** `signal` is a presentation record type (`AD-29`), so it
//!   moves `presentation_hash` and never `rules_hash`.
//! - **Bounded.** Signals are declared, not authored at runtime: a mod cannot
//!   invent a predicate, which is what keeps this from becoming an expression
//!   language by stealth.
//!
//! A signal is a *display* gate and never a resolution input: an effect cannot
//! read one, and a mod that wants to change what happens writes a quest or a
//! Tier-2 hook instead.

use crate::json::Json;
use crate::l4::condition::{self, Condition};

/// One declared display gate.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct SignalRecord {
    /// The record id, the key `kobra_signals` reports.
    pub id: String,
    /// The condition that decides whether the signal is true.
    pub when: Condition,
    /// The raw record body, kept so `presentation_hash` covers the record
    /// exactly as authored rather than a lossy re-rendering of the parsed form.
    pub data: Json,
}

/// Read a signal record.
///
/// An unknown condition key is an error rather than a silently-false signal, for
/// the same reason the condition parser refuses one: a HUD panel that never
/// appears because a key was misspelled is the class of bug `AD-15` prevents.
pub fn parse(id: &str, data: &Json) -> Result<SignalRecord, &'static str> {
    let when = data.get("when").ok_or("a signal needs a when condition")?;
    let when = condition::parse(when)?;
    Ok(SignalRecord {
        id: id.to_string(),
        when,
        data: data.clone(),
    })
}
