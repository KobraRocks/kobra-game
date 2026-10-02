//! L3 — the procedural baseline and the delta (`02:02.6`, `AD-11`, 05:05.5 M3).
//!
//! > A whole authored world is expensive to ship and to mod; a purely procedural
//! > world is not authorable. **Decision.** The world is a **procedural baseline
//! > with authored overrides.**
//!
//! Three consequences, and all three are implemented here rather than described:
//!
//! 1. **A campaign seed deterministically generates the baseline.** [`Baseline`]
//!    is a pure function of `(seed, width, height)` through the same seeded PRNG
//!    the rules use (`AD-6`) — no wall clock, no floats, no `Math.random`. The
//!    same seed and size give the same terrain on every machine forever.
//! 2. **Content authors named places that override it.** An authored
//!    `sector_map` record is an *override*, applied tile by tile, so a designer
//!    changes the three tiles they care about instead of authoring 65 000.
//! 3. **The save stores only the delta.** [`Baseline::delta`] records the sectors
//!    that differ from what the generator would produce, so a 256×256 world that
//!    is 99.9 % baseline costs a few hundred bytes in the save (`AD-11`, `R6`).
//!
//! And the rule that makes it safe:
//!
//! > **Changing the world generator invalidates deltas**, so the generator has its
//! > own version field inside the save, and a mismatch triggers either a rebase or
//! > a clear refusal. **Silently mis-placing the player's world is the one outcome
//! > that is not allowed.**
//!
//! [`GENERATOR_VERSION`] is that field. A save whose `generator_version` differs
//! from this build's is refused with an explanation rather than loaded against a
//! different baseline — because the alternative is a player standing inside a wall
//! that used to be a doorway.
//!
//! **The generator is deliberately simple and deliberately legible.** It produces
//! terrain in a handful of kinds with a coherent large-scale structure — ground, a
//! raised band, water, and scattered cover — from a value-noise field built out of
//! integer hashing. It is placeholder content in the same sense the demo's art is:
//! it exists so the CRPG *frame* can be exercised, and M5's content replaces the
//! numbers rather than the mechanism.

use crate::l3::world::{Position, Tile, World};
use crate::rng::Rng;

/// The generator's own version, recorded in every save (`02:02.6`).
///
/// Bump this whenever the baseline changes shape. A save carrying a different
/// number is **refused with an explanation**, never rebased silently.
pub const GENERATOR_VERSION: u32 = 1;

/// Terrain kind codes the shipped generator writes into `Tile::terrain`.
///
/// The engine only stores the number (`l3::world`), so a campaign may reuse it for
/// its own meaning; these are the codes the *shipped* baseline produces, and the
/// validator reports an authored map that uses one it does not know as a warning
/// rather than an error, because terrain is presentation-adjacent.
pub mod terrain {
    /// Dry ground.
    pub const GROUND: u8 = 0;
    /// Raised ground: one level up, walkable.
    pub const RISE: u8 = 1;
    /// Water: blocking, level 0.
    pub const WATER: u8 = 2;
    /// Scattered cover: blocking, one level high.
    pub const COVER: u8 = 3;
    /// A built surface: ground, but flagged as structure.
    pub const BUILT: u8 = 4;
    /// The map's edge, which is always solid.
    pub const EDGE: u8 = 5;
}

/// The parameters a baseline is generated from (`02:02.6`).
///
/// These are **world records**, not rules: a campaign declares the size and the
/// density of its generated district, and they are part of `rules_hash` because a
/// change here changes the terrain a save's coordinates mean.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Generator {
    /// Width in tiles.
    pub width: i32,
    /// Height in tiles.
    pub height: i32,
    /// The campaign seed.
    pub seed: u32,
    /// How much of the map is raised ground, in percent.
    pub rise_percent: i32,
    /// How much of the map is water, in percent.
    pub water_percent: i32,
    /// How much of the map is scattered cover, in percent.
    pub cover_percent: i32,
    /// The largest feature the noise field produces, in tiles.
    pub feature_tiles: i32,
}

impl Generator {
    /// A generator with the shipped defaults for a district of a given size.
    pub fn district(width: i32, height: i32, seed: u32) -> Generator {
        Generator {
            width,
            height,
            seed,
            rise_percent: 18,
            water_percent: 6,
            cover_percent: 10,
            feature_tiles: 24,
        }
    }

    /// Read a world-generator record.
    pub fn parse(id: &str, data: &crate::json::Json) -> Result<Generator, &'static str> {
        let width = data
            .get("width")
            .and_then(crate::json::Json::as_i64)
            .ok_or("a generator needs a width")? as i32;
        let height = data
            .get("height")
            .and_then(crate::json::Json::as_i64)
            .ok_or("a generator needs a height")? as i32;
        if width <= 0 || height <= 0 || width > 512 || height > 512 {
            return Err("a generated world must be between 1x1 and 512x512");
        }
        let _ = id;
        Ok(Generator {
            width,
            height,
            seed: data
                .get("seed")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(0) as u32,
            rise_percent: data
                .get("rise_percent")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(18) as i32,
            water_percent: data
                .get("water_percent")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(6) as i32,
            cover_percent: data
                .get("cover_percent")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(10) as i32,
            feature_tiles: data
                .get("feature_tiles")
                .and_then(crate::json::Json::as_i64)
                .unwrap_or(24) as i32,
        })
    }
}

/// A generated baseline: the tiles the generator produces, and nothing else.
///
/// It is **not** a `World`: it carries no placements and no mutable state, because
/// a baseline is a pure function that a save's delta is measured against. The
/// `World` a sim plays on is the baseline with the delta applied.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Baseline {
    /// The generator that produced it, so a delta can be checked against it.
    pub generator: Generator,
    /// The tiles, row-major.
    pub tiles: Vec<Tile>,
}

impl Baseline {
    /// Generate a baseline (`02:02.6`).
    ///
    /// Deterministic in `(seed, width, height, percentages, feature_tiles)`: the
    /// PRNG is seeded from the campaign seed folded with the geometry, so a save
    /// that records the seed and the size regenerates exactly this world.
    pub fn generate(generator: &Generator) -> Baseline {
        let width = generator.width.max(1);
        let height = generator.height.max(1);
        let mut rng = Rng::new(fold_seed(generator));
        let mut tiles = vec![Tile::default(); (width * height) as usize];
        // Two noise octaves: a coarse one for the large shapes, a fine one for
        // the edges. Both come from integer hashing, so there is no float on the
        // path (AD-6).
        let coarse = generator.feature_tiles.max(4);
        let fine = (coarse / 4).max(2);
        for y in 0..height {
            for x in 0..width {
                let index = (y * width + x) as usize;
                let edge = x == 0 || y == 0 || x == width - 1 || y == height - 1;
                if edge {
                    tiles[index] = Tile {
                        level: 0,
                        blocking: 3,
                        terrain: terrain::EDGE,
                        material: 60,
                    };
                    continue;
                }
                let big = noise(generator.seed, x, y, coarse);
                let small = noise(generator.seed ^ 0x9e37_79b9, x, y, fine);
                let value = (big * 3 + small) / 4;
                let roll = (rng.d100() as i32 + value / 4).rem_euclid(100);
                tiles[index] = if roll < generator.water_percent {
                    Tile {
                        level: 0,
                        blocking: 1,
                        terrain: terrain::WATER,
                        material: 5,
                    }
                } else if roll < generator.water_percent + generator.rise_percent {
                    Tile {
                        level: 1,
                        blocking: 0,
                        terrain: terrain::RISE,
                        material: 30,
                    }
                } else if roll
                    < generator.water_percent + generator.rise_percent + generator.cover_percent
                {
                    Tile {
                        level: 0,
                        blocking: 1,
                        terrain: terrain::COVER,
                        material: 20,
                    }
                } else {
                    Tile {
                        level: 0,
                        blocking: 0,
                        terrain: terrain::GROUND,
                        material: 10,
                    }
                };
            }
        }
        Baseline {
            generator: generator.clone(),
            tiles,
        }
    }

    /// A baseline over an authored map's tiles, for the M1/M2 world record.
    ///
    /// An authored `sector_map` is a complete small world rather than an override
    /// on generated terrain, which is what M1 shipped and what the tests drive.
    /// It is still a baseline in the save's sense: the delta is measured against
    /// it, so an authored map and a generated one go through one delta path.
    pub fn authored(generator: &Generator, world: &World) -> Baseline {
        Baseline {
            generator: generator.clone(),
            tiles: world.tiles.clone(),
        }
    }

    /// The world this baseline describes, with no placements.
    pub fn world(&self) -> World {
        let mut world = World::new(self.generator.width, self.generator.height);
        world.tiles = self.tiles.clone();
        world
    }

    /// The tile at a coordinate.
    pub fn tile(&self, x: i32, y: i32) -> Option<&Tile> {
        if x < 0 || y < 0 || x >= self.generator.width || y >= self.generator.height {
            return None;
        }
        self.tiles.get((y * self.generator.width + x) as usize)
    }

    /// Overlay an authored map onto the baseline (`02:02.6`).
    ///
    /// Only the authored map's **in-bounds** tiles are applied, so a designer
    /// authors a district-sized record over a generated region and everything
    /// outside it keeps its generated terrain. A tile the authored map leaves at
    /// the default `Tile` value *is* applied, deliberately: `{}` in an authored
    /// map means "grade, open ground", which is how a designer clears a generated
    /// wall.
    pub fn overlay(&mut self, authored: &World, origin: (i32, i32)) {
        for y in 0..authored.height {
            for x in 0..authored.width {
                let Some(tile) = authored.tile(x, y).copied() else {
                    continue;
                };
                let target_x = origin.0 + x;
                let target_y = origin.1 + y;
                if target_x < 0
                    || target_y < 0
                    || target_x >= self.generator.width
                    || target_y >= self.generator.height
                {
                    continue;
                }
                let index = (target_y * self.generator.width + target_x) as usize;
                self.tiles[index] = tile;
            }
        }
    }

    /// The sectors where a world differs from this baseline, row-major.
    ///
    /// This is the delta `AD-11` stores: the map changes the player's play has
    /// made, plus the authored override baked in at campaign creation. A world
    /// that has not been touched produces an empty delta, which costs two bytes.
    pub fn delta(&self, world: &World) -> Vec<(i32, i32, Tile)> {
        let mut changed = Vec::new();
        if world.width != self.generator.width || world.height != self.generator.height {
            // A differently-sized world is not a delta from this baseline at all,
            // so the delta is empty and the caller keeps the world whole. That
            // case is reported by the validator, not silently absorbed here.
            return changed;
        }
        for y in 0..world.height {
            for x in 0..world.width {
                let index = (y * world.width + x) as usize;
                match (world.tiles.get(index), self.tiles.get(index)) {
                    (Some(current), Some(base)) if current == base => {}
                    (Some(current), _) => changed.push((x, y, *current)),
                    (None, _) => {}
                }
            }
        }
        changed
    }

    /// Apply a delta to a fresh copy of the baseline.
    ///
    /// A delta entry outside the map is **skipped and counted**, never wrapped or
    /// clamped: a save that disagrees with the baseline about geometry is a bug
    /// worth reporting, and `02:02.6` is explicit that silently mis-placing the
    /// player's world is not allowed.
    pub fn apply(&self, delta: &[(i32, i32, Tile)]) -> (World, usize) {
        let mut world = self.world();
        let mut skipped = 0usize;
        for (x, y, tile) in delta {
            if world.in_bounds(*x, *y) {
                world.set_tile(*x, *y, *tile);
            } else {
                skipped += 1;
            }
        }
        (world, skipped)
    }
}

/// Fold a generator's parameters into one seed.
fn fold_seed(generator: &Generator) -> u64 {
    let mut hash = 0xcbf2_9ce4_8422_2325u64;
    for byte in generator.seed.to_le_bytes() {
        hash ^= u64::from(byte);
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    for value in [
        generator.width as u32,
        generator.height as u32,
        generator.feature_tiles as u32,
    ] {
        for byte in value.to_le_bytes() {
            hash ^= u64::from(byte);
            hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
    hash
}

/// Integer value noise: a hash of the *cell* a tile falls in, smoothed to the
/// tile.
///
/// This is the whole terrain function. It is an integer hash rather than a
/// float interpolation on purpose: `AD-6` puts no floats on any path that can
/// alter state, and terrain alters state.
fn noise(seed: u32, x: i32, y: i32, cell: i32) -> i32 {
    let cell = cell.max(1);
    let cx = x.div_euclid(cell);
    let cy = y.div_euclid(cell);
    let fx = x.rem_euclid(cell);
    let fy = y.rem_euclid(cell);
    // Bilinear blend of four corner hashes, in integer arithmetic with Q8
    // weights, so the result is reproducible everywhere.
    let weight_x = (fx * 256) / cell;
    let weight_y = (fy * 256) / cell;
    let top = lerp(hash_at(seed, cx, cy), hash_at(seed, cx + 1, cy), weight_x);
    let bottom = lerp(
        hash_at(seed, cx, cy + 1),
        hash_at(seed, cx + 1, cy + 1),
        weight_x,
    );
    lerp(top, bottom, weight_y) % 100
}

/// Blend two Q8-weighted values.
fn lerp(left: i32, right: i32, weight: i32) -> i32 {
    (left * (256 - weight) + right * weight) / 256
}

/// A deterministic value in `0..=99` for a cell.
fn hash_at(seed: u32, x: i32, y: i32) -> i32 {
    let mut hash = u64::from(seed) ^ 0x9e37_79b9_7f4a_7c15;
    for value in [x as i64 as u64, y as i64 as u64] {
        for byte in value.to_le_bytes() {
            hash ^= u64::from(byte);
            hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
    hash ^= hash >> 29;
    (hash % 100) as i32
}

/// A spawn point the generator proposes on the baseline (`02:02.6`).
///
/// The generator names *places*; content names the people who stand in them. That
/// split is what keeps a campaign able to override "the square is here" without
/// authoring every tile.
pub fn spawn_points(baseline: &Baseline, count: usize) -> Vec<Position> {
    let mut out = Vec::new();
    let width = baseline.generator.width;
    for index in 0..count {
        // A deterministic scatter along the middle of the map, on ground that is
        // walkable, which is what a "somewhere sensible to put an NPC" means.
        let mut cursor = (index as i32 * 7) % width.max(1);
        let y = ((index as i32 * 5) % baseline.generator.height.max(1)).max(1);
        for _ in 0..width {
            let x = cursor;
            if let Some(tile) = baseline.tile(x, y) {
                if tile.blocking == 0 {
                    out.push(Position {
                        x,
                        y,
                        z: tile.level,
                    });
                    break;
                }
            }
            cursor = (cursor + 1) % width.max(1);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_same_seed_generates_the_same_world_on_every_run() {
        let generator = Generator::district(48, 32, 2026);
        let first = Baseline::generate(&generator);
        let second = Baseline::generate(&generator);
        assert_eq!(first, second, "generation is a pure function of the record");
    }

    #[test]
    fn a_different_seed_generates_a_different_world() {
        let first = Baseline::generate(&Generator::district(48, 32, 1));
        let second = Baseline::generate(&Generator::district(48, 32, 2));
        assert_ne!(first.tiles, second.tiles);
    }

    #[test]
    fn the_map_edge_is_solid_so_nothing_walks_off_it() {
        let baseline = Baseline::generate(&Generator::district(16, 12, 5));
        for x in 0..16 {
            assert!(baseline.tile(x, 0).expect("tile").blocking > 0);
            assert!(baseline.tile(x, 11).expect("tile").blocking > 0);
        }
        for y in 0..12 {
            assert!(baseline.tile(0, y).expect("tile").blocking > 0);
            assert!(baseline.tile(15, y).expect("tile").blocking > 0);
        }
    }

    #[test]
    fn an_authored_map_overrides_the_generated_baseline_tile_by_tile() {
        let generator = Generator::district(16, 16, 7);
        let mut baseline = Baseline::generate(&generator);
        let authored = World::parse(&crate::json::Json::from_text(
            r#"{"width":3,"height":3,"tiles":[{"terrain":4},{},{},{},{},{},{},{},{}]}"#,
        ))
        .expect("map");
        baseline.overlay(&authored, (4, 4));
        assert_eq!(baseline.tile(4, 4).expect("tile").terrain, terrain::BUILT);
        assert_eq!(
            baseline.tile(4, 4).expect("tile").level,
            0,
            "the authored tile wins outright"
        );
        // Outside the overlay the generated terrain stands.
        let untouched = Baseline::generate(&Generator::district(16, 16, 7));
        assert_eq!(baseline.tile(15, 15), untouched.tile(15, 15));
    }

    #[test]
    fn a_delta_carries_only_what_changed_and_survives_a_round_trip() {
        let generator = Generator::district(12, 12, 11);
        let baseline = Baseline::generate(&generator);
        let (mut world, _) = baseline.apply(&[]);
        // A fresh baseline produces an empty delta, which is the whole point of
        // AD-11's per-sector delta.
        assert!(baseline.delta(&world).is_empty());
        world.set_tile(
            5,
            5,
            Tile {
                level: 2,
                blocking: 0,
                terrain: terrain::BUILT,
                material: 40,
            },
        );
        world.place(9, Position { x: 5, y: 5, z: 2 });
        let delta = baseline.delta(&world);
        assert_eq!(delta.len(), 1, "one sector changed, so one entry");
        let (restored, skipped) = baseline.apply(&delta);
        assert_eq!(skipped, 0);
        assert_eq!(restored.tile(5, 5), world.tile(5, 5));
    }

    #[test]
    fn a_delta_entry_off_the_map_is_skipped_and_counted_never_clamped() {
        let baseline = Baseline::generate(&Generator::district(8, 8, 3));
        let delta = vec![(
            99,
            99,
            Tile {
                level: 9,
                blocking: 0,
                terrain: 0,
                material: 0,
            },
        )];
        let (world, skipped) = baseline.apply(&delta);
        assert_eq!(skipped, 1, "02:02.6: never silently mis-placed");
        assert!(world.tile(99, 99).is_none());
    }

    #[test]
    fn the_generator_version_is_recorded_not_assumed() {
        // 02:02.6: a save whose generator version differs is refused, so the
        // constant has to be a real number rather than a placeholder.
        assert_eq!(GENERATOR_VERSION, 1);
    }

    #[test]
    fn spawn_points_land_on_walkable_ground() {
        let baseline = Baseline::generate(&Generator::district(24, 24, 13));
        let points = spawn_points(&baseline, 6);
        assert!(!points.is_empty());
        for point in &points {
            let tile = baseline.tile(point.x, point.y).expect("on the map");
            assert_eq!(tile.blocking, 0, "an NPC is not put inside a wall");
            assert_eq!(tile.level, point.z);
        }
    }

    #[test]
    fn a_generator_record_reads_its_shape_from_content() {
        let generator = Generator::parse(
            "wsp.world.district",
            &crate::json::Json::from_text(
                r#"{"width":64,"height":48,"seed":99,"rise_percent":25,"water_percent":0,
                    "cover_percent":5,"feature_tiles":12}"#,
            ),
        )
        .expect("generator");
        assert_eq!(generator.width, 64);
        assert_eq!(generator.water_percent, 0);
        let baseline = Baseline::generate(&generator);
        for tile in &baseline.tiles {
            assert_ne!(tile.terrain, terrain::WATER, "no water was asked for");
        }
        assert!(Generator::parse(
            "g",
            &crate::json::Json::from_text(r#"{"width":0,"height":4}"#)
        )
        .is_err());
    }
}
