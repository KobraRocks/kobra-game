//! Structural guard on the generated ladder.
//!
//! This is not the property test — that is `specs/verify_master_tables.py`, run
//! by `make check`, and it is the implementation AD-14 names. This file proves
//! the *generated Rust* is consumable and internally consistent: the right
//! number of rows and buckets, a bucket list that tiles `0..=99`, rows the same
//! width as the bucket list, and the accessors behaving at both ends of the
//! ladder. If the generator emits garbage, this fails before anything else does.

use kobra_core::l0::Colour;
use kobra_core::tables;

#[test]
fn the_generated_ladder_has_thirteen_rows_over_twenty_two_buckets() {
    let ladder = tables::CAMPAIGN_LADDER;
    assert_eq!(ladder.name, "basic");
    assert_eq!(ladder.bands.len(), 13, "the Basic game has 13 rank bands");
    assert_eq!(
        ladder.buckets.len(),
        22,
        "the Basic table has 22 d% buckets"
    );
}

#[test]
fn every_row_is_as_wide_as_the_bucket_list_and_starts_black() {
    let ladder = tables::CAMPAIGN_LADDER;
    for (index, band) in ladder.bands.iter().enumerate() {
        assert_eq!(
            band.colours.len(),
            ladder.buckets.len(),
            "band {index} has the wrong width"
        );
        // 02:02.2 property 4: the irreducible failure band. `00-04` is Black in
        // every row of both tables — the cell the as-exported CSV removed.
        assert_eq!(
            band.colours[0],
            Colour::Blck,
            "band {index} lost the irreducible {}-{} failure band",
            ladder.buckets[0].lo,
            ladder.buckets[0].hi
        );
    }
}

#[test]
fn the_buckets_tile_the_percentile_roll_once() {
    let ladder = tables::CAMPAIGN_LADDER;
    let mut covered = [0u8; 100];
    for bucket in ladder.buckets {
        assert!(bucket.lo <= bucket.hi, "inverted bucket {bucket:?}");
        for roll in bucket.lo..=bucket.hi {
            covered[roll as usize] += 1;
        }
    }
    assert!(
        covered.iter().all(|count| *count == 1),
        "the buckets must cover 0..=99 exactly once each"
    );
}

#[test]
fn rows_are_monotone_left_to_right() {
    // AD-14's property 1. The full property suite is M1's `tests/rust/tables`;
    // this is the cheap version that runs with the unit tests.
    for (index, band) in tables::CAMPAIGN_LADDER.bands.iter().enumerate() {
        for pair in band.colours.windows(2) {
            assert!(
                pair[0] <= pair[1],
                "band {index} decreases: {:?} -> {:?}",
                pair[0],
                pair[1]
            );
        }
    }
}

#[test]
fn rank_values_clamp_at_both_ends_of_the_ladder() {
    let ladder = tables::CAMPAIGN_LADDER;
    assert_eq!(ladder.band_index(0), 0);
    assert_eq!(
        ladder.band_index(-1),
        0,
        "below rank 0 clamps to the rank-0 row"
    );
    assert_eq!(ladder.band_index(1), 1);
    assert_eq!(ladder.band(1000).lo, 1000, "1000 has its own Basic row");
    assert_eq!(ladder.band_index(1000), ladder.bands.len() - 1);
    assert_eq!(
        ladder.band_index(9999),
        ladder.bands.len() - 1,
        "Basic has no row above 1000, so everything clamps to the top row"
    );
    assert_eq!(ladder.band_index(i32::MAX), ladder.bands.len() - 1);
}

#[test]
fn a_row_step_is_a_band_shift_and_saturates() {
    let ladder = tables::CAMPAIGN_LADDER;
    // 4c:864 — `-2` from the `20-29` band lands on `6-9`.
    let from = ladder.band_index(20);
    let to = ladder.row_step(from, -2);
    assert_eq!(ladder.bands[to].lo, 6);
    assert_eq!(ladder.bands[to].hi, 9);
    assert_eq!(ladder.row_step(0, -5), 0, "clamps at the bottom");
    let top = ladder.bands.len() - 1;
    assert_eq!(ladder.row_step(top, 5), top, "clamps at the top");
}

#[test]
fn the_resolution_primitive_reads_00_as_zero() {
    let ladder = tables::CAMPAIGN_LADDER;
    // 4c:41 — `00` is zero, not one hundred, so it is the lowest bucket.
    assert_eq!(ladder.resolve(0, 0), Some(Colour::Blck));
    assert_eq!(ladder.resolve(1000, 0), Some(Colour::Blck));
    assert_eq!(ladder.resolve(1000, 99), Some(Colour::Yel));
    assert_eq!(ladder.resolve(0, 99), Some(Colour::Yel));
    assert_eq!(ladder.resolve(1000, 100), None, "100 is not a d% result");
}
