//! The resolution primitive: one function every rules question reduces to
//! (02:02.2).
//!
//! A roll becomes a [`Colour`](super::Colour) through the Master Table, and the
//! colour's *meaning* belongs to whoever is reading it. That is why this module
//! returns a colour rather than a boolean: a boolean would force every caller to
//! re-derive the ordering, and the public-reaction reversal for criminals would
//! scatter across the dialogue layer instead of being one `polarity` flag
//! (02:02.2, `4c:1278-1292`).
//!
//! Fortune is a **post-roll window**, not a pre-roll modifier (`4c:874`):
//!
//! ```text
//! resolve -> [any participant commits 25-point increments] -> final colour
//! ```
//!
//! The window is applied in descending commit order, ties by actor id
//! ascending, so the events shown to the player are independent of arrival
//! order. The *total* is order-independent anyway (colour shifts are index
//! arithmetic), but the intermediate outcomes are not — hence the ordering rule.

use super::ladder::{Colour, Ladder};

/// What a colour means to this reader (02:02.2).
///
/// The same four colours carry three meanings; the primitive returns a colour
/// and the consumer supplies this so the interpretation is explicit.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ColourSemantics {
    /// Action outcome: Black failed → Yellow major success (`4c:855-860`).
    Outcome,
    /// Difficulty: Black easy → Yellow ridiculous (`4c:1199`).
    Difficulty,
    /// Public or faction reaction (`4c:1278-1292`).
    Reaction {
        /// A criminal's reaction scale is reversed (`4c:1292`).
        criminal: bool,
    },
}

impl ColourSemantics {
    /// Whether a `rolled` colour satisfies a `required` colour.
    ///
    /// For an outcome or a difficulty, "meets" means at least as high a colour.
    /// For a reaction the comparison is the same unless the reader is a
    /// criminal, whose scale is reversed.
    pub fn meets(self, rolled: Colour, required: Colour) -> bool {
        let at_least = (rolled as u8) >= (required as u8);
        match self {
            ColourSemantics::Outcome | ColourSemantics::Difficulty => at_least,
            ColourSemantics::Reaction { criminal } => at_least != criminal,
        }
    }
}

/// One participant's Fortune commitment to a roll (`4c:874`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct FortuneCommit {
    /// The entity spending; the tie-break key, ascending.
    pub actor: u32,
    /// Points committed. A negative value shifts the colour the other way.
    pub points: i32,
}

/// The colour shifts `commits` buy, in the deterministic order the rules name.
///
/// 25 points buy one colour shift, and `4c:874`'s own example spends 37 and gets
/// one, so the division truncates toward zero and the remainder is kept by the
/// spender.
pub fn fortune_steps(commits: &mut [FortuneCommit]) -> i32 {
    // Descending by points, ties by actor id ascending. A stable sort would also
    // work, but the two-key comparison states the rule rather than relying on
    // the input order.
    commits.sort_by(|a, b| b.points.cmp(&a.points).then(a.actor.cmp(&b.actor)));
    commits.iter().map(|commit| commit.points / 25).sum()
}

/// The final colour after a Fortune window.
pub fn apply_fortune(rolled: Colour, commits: &mut [FortuneCommit]) -> Colour {
    rolled.shift(fortune_steps(commits))
}

/// One resolved roll, kept as a record so a preview can explain itself.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Resolution {
    /// The effective Rank Value the row was read at.
    pub effective_rv: i32,
    /// The percentile roll, `0..=99`.
    pub d100: u8,
    /// The colour the table gave, before any Fortune.
    pub rolled: Colour,
    /// The colour after the Fortune window.
    pub colour: Colour,
}

/// Resolve `d100` at `effective_rv`, then apply the Fortune window.
///
/// Returns `None` only when `d100 > 99`, which is a caller bug and not a roll
/// (02:02.2). Nothing is clamped silently: a caller that wants a clamped Rank
/// Value asks [`Ladder::band_index`] for one.
pub fn resolve_with_fortune(
    ladder: &Ladder,
    effective_rv: i32,
    d100: u8,
    commits: &mut [FortuneCommit],
) -> Option<Resolution> {
    let rolled = ladder.resolve(effective_rv, d100)?;
    let colour = apply_fortune(rolled, commits);
    Some(Resolution {
        effective_rv,
        d100,
        rolled,
        colour,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn outcome_and_difficulty_meet_at_or_above() {
        let semantics = ColourSemantics::Outcome;
        assert!(semantics.meets(Colour::Blue, Colour::Red));
        assert!(semantics.meets(Colour::Red, Colour::Red));
        assert!(!semantics.meets(Colour::Blck, Colour::Red));
        assert!(!ColourSemantics::Difficulty.meets(Colour::Red, Colour::Blue));
    }

    #[test]
    fn a_criminals_reaction_scale_is_reversed() {
        // 02:02.2: a test asserts `meets` works for Outcome and Difficulty and
        // is inverted for Reaction with `criminal`.
        let honest = ColourSemantics::Reaction { criminal: false };
        let criminal = ColourSemantics::Reaction { criminal: true };
        assert!(honest.meets(Colour::Blue, Colour::Red));
        assert!(!criminal.meets(Colour::Blue, Colour::Red));
        assert!(criminal.meets(Colour::Blck, Colour::Red));
    }

    #[test]
    fn twenty_five_points_buy_one_shift_and_the_remainder_is_kept() {
        // The spec's own example: 37 points buys one shift and leaves 12.
        let mut commits = [FortuneCommit {
            actor: 1,
            points: 37,
        }];
        assert_eq!(fortune_steps(&mut commits), 1);
        let mut commits = [FortuneCommit {
            actor: 1,
            points: 50,
        }];
        assert_eq!(fortune_steps(&mut commits), 2);
    }

    #[test]
    fn the_window_is_independent_of_arrival_order() {
        let mut forward = [
            FortuneCommit {
                actor: 1,
                points: 25,
            },
            FortuneCommit {
                actor: 2,
                points: 50,
            },
            FortuneCommit {
                actor: 3,
                points: 25,
            },
        ];
        let mut backward = [
            FortuneCommit {
                actor: 3,
                points: 25,
            },
            FortuneCommit {
                actor: 1,
                points: 25,
            },
            FortuneCommit {
                actor: 2,
                points: 50,
            },
        ];
        assert_eq!(
            apply_fortune(Colour::Red, &mut forward),
            apply_fortune(Colour::Red, &mut backward),
        );
        // Descending spend, so the player sees the largest commitment first;
        // equal spends break on the lower actor id.
        assert_eq!(forward[0].actor, 2);
        assert_eq!(forward[1].actor, 1);
        assert_eq!(forward[2].actor, 3);
    }

    #[test]
    fn a_negative_commit_shifts_the_other_way() {
        let mut commits = [FortuneCommit {
            actor: 1,
            points: -25,
        }];
        assert_eq!(apply_fortune(Colour::Blue, &mut commits), Colour::Red);
    }

    #[test]
    fn resolve_reports_both_the_rolled_and_the_shifted_colour() {
        let ladder = crate::tables::CAMPAIGN_LADDER;
        // At Rank Value 1000, a roll of 5 lands in the `05-09` bucket, which is
        // Red; one 25-point commitment shifts it one colour right, to Blue.
        let mut commits = [FortuneCommit {
            actor: 9,
            points: 25,
        }];
        let resolution = resolve_with_fortune(ladder, 1000, 5, &mut commits).expect("resolve");
        assert_eq!(resolution.rolled, Colour::Red);
        assert_eq!(resolution.colour, Colour::Blue);
        assert_eq!(resolution.d100, 5);
        // A shift at the top saturates rather than wrapping.
        let mut commits = [FortuneCommit {
            actor: 9,
            points: 75,
        }];
        let resolution = resolve_with_fortune(ladder, 1000, 99, &mut commits).expect("resolve");
        assert_eq!(resolution.rolled, Colour::Yel);
        assert_eq!(resolution.colour, Colour::Yel);
    }
}
