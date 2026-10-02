//! L0 — the foundation every roll resolves through (02:02.2).
//!
//! The Master Table, rank bands, row steps and colours. This is the layer the
//! rest of the game is built on, so it is the one place where a wrong value is
//! wrong everywhere and invisibly (05:05.7, R1).

pub mod dice;
pub mod ladder;
pub mod resolve;

pub use dice::{Amount, Dice, Rounding};
pub use ladder::{Band, Bucket, Colour, Ladder, Ruleset};
pub use resolve::{
    apply_fortune, fortune_steps, resolve_with_fortune, ColourSemantics, FortuneCommit, Resolution,
};
