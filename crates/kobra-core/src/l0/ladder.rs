//! The rank ladder and the resolution primitive (02:02.2).
//!
//! The *data* lives in [`crate::tables`], which is generated from
//! `specs/4c_system_master_tables.csv` by `tools/gen-tables` (AD-14). This module
//! is the arithmetic: which band a Rank Value falls in, what a row step does,
//! and what a `d%` roll resolves to.
//!
//! Two rules from 02:02.2 are implemented deliberately rather than discovered:
//!
//! - **A row step is a band-index shift, not an arithmetic addition.** The bands
//!   are uneven in width, so `+1` moves the index up one band and `-2` from
//!   `20-29` lands on `6-9` (4c:864). There is no arithmetic shortcut.
//! - **Clamping is saturating at both ends.** Repeated `-n` below rank 0 clamps
//!   at the rank-0 row; repeated `+n` above the top band clamps at the top row.
//!   Rank 0 is a reachable *effective* rank and an illegal *stored* rank
//!   (4c:212), which is why the callers take an effective value here.

/// A Master Table outcome: the four colours, ordered as the rules order them.
///
/// `Black < Red < Blue < Yellow`, so monotonicity is a comparison rather than a
/// hand-written rank function (02:02.2, property test 1). "Failure" through
/// "major success" for an action roll, and a difficulty or reaction scale for
/// other readers — the interpretation belongs to the consumer, not to this type.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug, Hash)]
#[repr(u8)]
pub enum Colour {
    /// Black — the irreducible failure band is Black in every row.
    Blck = 0,
    /// Red — minor success.
    Red = 1,
    /// Blue — success.
    Blue = 2,
    /// Yellow — major success.
    Yel = 3,
}

impl Colour {
    /// The wire/diagnostic code (`0..=3`), stable across the ABI and the save.
    pub const fn code(self) -> u8 {
        self as u8
    }

    /// The colour for a wire code, or `None` if it is out of range.
    pub const fn from_code(code: u8) -> Option<Colour> {
        match code {
            0 => Some(Colour::Blck),
            1 => Some(Colour::Red),
            2 => Some(Colour::Blue),
            3 => Some(Colour::Yel),
            _ => None,
        }
    }

    /// The colour `steps` colours away, saturating at both ends.
    ///
    /// This is the Fortune shift (4c:874) and every other colour-step rule, so
    /// it lives on the type rather than in each subsystem.
    pub const fn shift(self, steps: i32) -> Colour {
        let idx = self as i32 + steps;
        if idx <= 0 {
            Colour::Blck
        } else if idx >= 3 {
            Colour::Yel
        } else {
            // `idx` is in `1..=2`; the match is exhaustive and const-friendly.
            match idx {
                1 => Colour::Red,
                _ => Colour::Blue,
            }
        }
    }
}

/// One `d%` bucket: the inclusive range of roll values it covers.
///
/// The buckets tile `0..=99` exactly in every ladder, with the irregular
/// `90-93 / 94-96 / 97-98 / 99` tail being intentional finer granularity at the
/// top rather than a hole (02:02.2, property test 3).
#[derive(Clone, Copy, Debug)]
pub struct Bucket {
    /// Lowest roll in the bucket, inclusive.
    pub lo: u8,
    /// Highest roll in the bucket, inclusive.
    pub hi: u8,
}

impl Bucket {
    /// Whether `d100` falls in this bucket.
    pub const fn contains(&self, d100: u8) -> bool {
        d100 >= self.lo && d100 <= self.hi
    }
}

/// One rank band: a contiguous Rank Value range and its row of the Master Table.
#[derive(Clone, Copy, Debug)]
pub struct Band {
    /// Lowest Rank Value in the band, inclusive.
    pub lo: i32,
    /// Highest Rank Value in the band, inclusive. `i32::MAX` when open-ended.
    pub hi: i32,
    /// The band's colours, one per bucket, ascending by bucket.
    pub colours: &'static [Colour],
}

/// Which Master Table a save was created under.
///
/// The two ladders are **not nested**: Basic has a row that is exactly `1000`
/// and no row at all above it, while Advanced has `1000-1499` and splits Basic's
/// `150-999` into three rows. So the same Rank Value resolves differently under
/// each, the ladder is part of a save's identity, and a mismatch is a hard
/// refusal rather than a clamp (02:02.2, 02:02.9).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Ruleset {
    /// The 13-row Basic game.
    Basic,
    /// The 18-row Advanced game.
    Advanced,
}

impl Ruleset {
    /// The stable wire name, recorded in the save's `envelope.ruleset`.
    pub const fn name(self) -> &'static str {
        match self {
            Ruleset::Basic => "basic",
            Ruleset::Advanced => "advanced",
        }
    }
}

/// One ladder: its bands ascending, and the bucket list they share.
#[derive(Debug)]
pub struct Ladder {
    /// Which of the two games this ladder is.
    pub ruleset: Ruleset,
    /// The wire name, for diagnostics.
    pub name: &'static str,
    /// The `d%` buckets, ascending. Every band's `colours` has one entry each.
    pub buckets: &'static [Bucket],
    /// The rank bands, ascending by Rank Value. Index 0 is rank 0.
    pub bands: &'static [Band],
}

impl Ladder {
    /// The index of the band `rv` resolves in.
    ///
    /// A binary search over the band bounds — no arithmetic shortcut exists
    /// because the bands are uneven (02:02.2). Values below the lowest band clamp
    /// to it, and values above the highest clamp to the top row, which is what
    /// makes Basic's `1001..9999` resolve on the `1000` row rather than fail.
    ///
    /// # Panics
    ///
    /// Panics if the ladder has no bands. A generated table always has 13 or 18,
    /// and the property tests under `tests/tables.rs` would fail first; a ladder
    /// with no rows is a build defect, not a runtime condition.
    pub fn band_index(&self, rv: i32) -> usize {
        assert!(!self.bands.is_empty(), "ladder has no bands");
        let mut lo = 0usize;
        let mut hi = self.bands.len() - 1;
        if rv <= self.bands[0].lo {
            return 0;
        }
        if rv >= self.bands[hi].hi {
            return hi;
        }
        while lo < hi {
            let mid = lo + (hi - lo).div_ceil(2);
            if self.bands[mid].lo <= rv {
                lo = mid;
            } else {
                hi = mid - 1;
            }
        }
        lo
    }

    /// The band `rv` resolves in, clamped as [`Ladder::band_index`] describes.
    pub fn band(&self, rv: i32) -> &Band {
        &self.bands[self.band_index(rv)]
    }

    /// Shift a band index by `steps` bands, saturating at both ends.
    ///
    /// A row step is an index shift, never an arithmetic addition to the Rank
    /// Value (02:02.2). Saturating at both ends is the spec's explicit clamping
    /// rule (4c:864, 4c:212).
    pub fn row_step(&self, index: usize, steps: i32) -> usize {
        let last = self.bands.len() - 1;
        let moved = index as i64 + steps as i64;
        if moved <= 0 {
            0
        } else if moved >= last as i64 {
            last
        } else {
            moved as usize
        }
    }

    /// The bucket index `d100` falls in, or `None` when `d100 > 99`.
    ///
    /// `00` is zero, not one hundred (4c:41). A value above 99 is a caller bug,
    /// not a roll: the buckets tile `0..=99` exactly, so there is no bucket to
    /// return and no honest default. Failing closed here is deliberate — this is
    /// the single easiest rule in the system to get wrong (02:02.2).
    pub fn bucket_index(&self, d100: u8) -> Option<usize> {
        if d100 > 99 {
            return None;
        }
        self.buckets.iter().position(|b| b.contains(d100))
    }

    /// Resolve a `d%` roll at an effective Rank Value: the game's one primitive.
    ///
    /// Returns `None` only when `d100 > 99`.
    pub fn resolve(&self, effective_rv: i32, d100: u8) -> Option<Colour> {
        let band = self.band(effective_rv);
        let bucket = self.bucket_index(d100)?;
        band.colours.get(bucket).copied()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn colour_is_ordered_black_to_yellow() {
        assert!(Colour::Blck < Colour::Red);
        assert!(Colour::Red < Colour::Blue);
        assert!(Colour::Blue < Colour::Yel);
    }

    #[test]
    fn shift_saturates_at_both_ends_and_is_the_inverse_of_itself() {
        assert_eq!(Colour::Red.shift(1), Colour::Blue);
        assert_eq!(Colour::Blue.shift(-1), Colour::Red);
        assert_eq!(Colour::Blck.shift(-9), Colour::Blck);
        assert_eq!(Colour::Yel.shift(9), Colour::Yel);
        assert_eq!(Colour::Blck.shift(99), Colour::Yel);
        for code in 0u8..=3 {
            let c = Colour::from_code(code).expect("in range");
            assert_eq!(c.shift(0), c);
            assert_eq!(Colour::from_code(c.code()), Some(c));
        }
        assert_eq!(Colour::from_code(4), None);
    }

    #[test]
    fn bucket_lookup_rejects_a_roll_above_99() {
        assert_eq!(crate::tables::CAMPAIGN_LADDER.bucket_index(100), None);
        assert!(crate::tables::CAMPAIGN_LADDER.bucket_index(99).is_some());
    }
}
