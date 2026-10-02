//! The one seeded PRNG (AD-6, 02:02.9 rule 3).
//!
//! All gameplay randomness comes from here, and its state is part of the save.
//! There is exactly one gameplay stream; the cosmetic stream the renderer uses
//! is a separate type instance that is **never saved and never read by the
//! sim**, so a cosmetic draw can never change state.
//!
//! The generator is PCG-XSH-RR 64/32 (the PCG family AD-6 names). Two
//! properties matter more than statistical quality here:
//!
//! - it is **exactly specified** — integer arithmetic with no implementation
//!   latitude, so two builds on two machines agree bit for bit;
//! - it is **splittable** — [`Rng::fork`] derives a named substream, which is
//!   how M2's per-`(tick, system, partition)` seeding works without ever
//!   sharing a mutable cursor across threads.
//!
//! `d%` is `next_u32() % 100`, so `00` is **zero, not one hundred** — the
//! single easiest rule in 4C to get wrong (`4c:41`, 02:02.2).

/// A PCG-XSH-RR 64/32 generator with a splittable seed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Rng {
    /// The LCG state.
    state: u64,
    /// The LCG increment; must be odd.
    inc: u64,
}

/// The LCG multiplier, from the PCG reference implementation.
const MULT: u64 = 6_364_136_223_846_793_005;

impl Rng {
    /// A stream from a 64-bit seed. The same seed always gives the same stream.
    pub fn new(seed: u64) -> Rng {
        let mut rng = Rng {
            state: 0,
            inc: (seed << 1) | 1,
        };
        // The reference seeding procedure: advance twice with the increment
        // mixed in, so a zero seed is not a degenerate state.
        rng.next_u32();
        rng.state = rng.state.wrapping_add(seed);
        rng.next_u32();
        rng
    }

    /// The generator's complete state, for the save (`02:02.9`).
    pub fn save(&self) -> (u64, u64) {
        (self.state, self.inc)
    }

    /// Restore a generator from [`Rng::save`].
    pub fn restore(state: u64, inc: u64) -> Rng {
        Rng {
            state,
            // The increment must be odd for the LCG to have full period; a
            // corrupt save with an even increment is forced back rather than
            // producing a short cycle.
            inc: inc | 1,
        }
    }

    /// The next 32 bits.
    pub fn next_u32(&mut self) -> u32 {
        let old = self.state;
        self.state = old.wrapping_mul(MULT).wrapping_add(self.inc);
        let xorshifted = (((old >> 18) ^ old) >> 27) as u32;
        let rot = (old >> 59) as u32;
        xorshifted.rotate_right(rot)
    }

    /// The next 64 bits, from two draws.
    pub fn next_u64(&mut self) -> u64 {
        let hi = u64::from(self.next_u32());
        let lo = u64::from(self.next_u32());
        (hi << 32) | lo
    }

    /// A percentile roll: `0..=99`, where `00` is zero (`4c:41`).
    pub fn d100(&mut self) -> u8 {
        (self.next_u32() % 100) as u8
    }

    /// A die roll: `1..=sides`. `sides == 0` is a caller bug and returns 1,
    /// because there is no honest value for a zero-sided die.
    pub fn die(&mut self, sides: u32) -> u32 {
        if sides == 0 {
            return 1;
        }
        1 + self.next_u32() % sides
    }

    /// An integer in `lo..=hi`, inclusive. Returns `lo` for an empty range.
    pub fn range(&mut self, lo: i32, hi: i32) -> i32 {
        if hi <= lo {
            return lo;
        }
        let span = (hi as i64 - lo as i64 + 1) as u64;
        lo + (self.next_u64() % span) as i32
    }

    /// Derive an independent stream with a domain name.
    ///
    /// The child is a deterministic function of the parent's state, so a replay
    /// reproduces it, and it does not disturb the parent's cursor. M2 seeds one
    /// substream per `(tick, system, partition)` this way (07:07.4).
    pub fn fork(&self, domain: u64) -> Rng {
        let mut mixed = self.state ^ self.inc.rotate_left(17) ^ domain.wrapping_mul(MULT);
        mixed = splitmix64(mixed);
        Rng::new(splitmix64(mixed ^ domain))
    }
}

/// The SplitMix64 finaliser, used to derive a well-mixed child seed.
fn splitmix64(mut value: u64) -> u64 {
    value = value.wrapping_add(0x9e37_79b9_7f4a_7c15);
    let mut mixed = value;
    mixed = (mixed ^ (mixed >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    mixed = (mixed ^ (mixed >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    mixed ^ (mixed >> 31)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_seed_always_gives_the_same_stream() {
        // The determinism contract's foundation (02:02.9 rule 1). If this test
        // ever changes value, every committed replay fixture is invalid and the
        // change is a save-format event, not a refactor.
        let mut a = Rng::new(2026);
        let first: Vec<u32> = (0..8).map(|_| a.next_u32()).collect();
        let mut b = Rng::new(2026);
        let second: Vec<u32> = (0..8).map(|_| b.next_u32()).collect();
        assert_eq!(first, second);
        assert_ne!(first, vec![0; 8], "the stream must not be degenerate");
    }

    #[test]
    fn a_percentile_roll_is_zero_to_ninety_nine() {
        let mut rng = Rng::new(7);
        let mut seen_low = false;
        let mut seen_high = false;
        for _ in 0..10_000 {
            let roll = rng.d100();
            assert!(roll <= 99, "d% returned {roll}");
            seen_low |= roll == 0;
            seen_high |= roll >= 90;
        }
        assert!(seen_low, "00 must be reachable and mean zero");
        assert!(seen_high);
    }

    #[test]
    fn a_die_is_one_to_its_sides() {
        let mut rng = Rng::new(3);
        for _ in 0..1_000 {
            let roll = rng.die(10);
            assert!((1..=10).contains(&roll), "d10 returned {roll}");
        }
    }

    #[test]
    fn save_and_restore_continues_the_same_stream() {
        let mut rng = Rng::new(99);
        for _ in 0..10 {
            rng.next_u32();
        }
        let (state, inc) = rng.save();
        let expected: Vec<u32> = (0..5).map(|_| rng.next_u32()).collect();
        let mut restored = Rng::restore(state, inc);
        let actual: Vec<u32> = (0..5).map(|_| restored.next_u32()).collect();
        assert_eq!(expected, actual);
    }

    #[test]
    fn a_fork_is_stable_and_does_not_disturb_the_parent() {
        let mut rng = Rng::new(11);
        rng.next_u32();
        let child_a = rng.fork(1);
        let mut child_b = rng.fork(1);
        let mut child_c = rng.fork(2);
        assert_eq!(child_a.clone().next_u32(), child_b.next_u32());
        assert_ne!(child_a.clone().next_u32(), child_c.next_u32());
        // The parent's next value is unaffected by having forked.
        let mut quiet = Rng::new(11);
        quiet.next_u32();
        assert_eq!(rng.next_u32(), quiet.next_u32());
    }
}
