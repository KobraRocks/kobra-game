//! L3 — the sector map (02:02.6, AD-33, 02:02.12).
//!
//! The world is a graph of **sectors**, each one a **1 m x 1 m tile** (`AD-33`).
//! *Sector* is the graph's word for a cell and *tile* is the same cell when a
//! rule counts distance. One Blender grid square is one sector, which is why the
//! tile is structural: a save's coordinates, and every mod's, are in it.
//!
//! Height is mechanical (`AD-28`): each sector carries an integer **level**
//! (0 = grade, +1 per storey) and a **blocking height** used for occlusion, and
//! elevation feeds row steps and line of sight. Both belong to the world content
//! pack, never to the renderer, so combat, AI and the editor read the same
//! integers and a save's meaning never depends on what the art looks like.
//!
//! **Occupancy is one entity per tile** (`AD-33`): a tile is roughly
//! shoulder-width personal space, and entering an occupied tile is a contest,
//! not a free move. M1 resolves the contest through the Brawn tables when a move
//! is attempted; the displacement rule arrives with the full wrestling tables in
//! M2.

use crate::json::Json;

/// One sector.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Tile {
    /// Storeys above grade; negative is a basement or pit (`AD-28`).
    pub level: i8,
    /// How tall the terrain here is, in levels, for occlusion (`02:02.12`).
    pub blocking: i8,
    /// A campaign terrain tag; the engine only stores it.
    pub terrain: u8,
    /// Material value, for knock-through and collisions (`4c:1131-1146`).
    pub material: i32,
}

/// A grid position: tile coordinates plus a level.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Position {
    /// Tile column.
    pub x: i32,
    /// Tile row.
    pub y: i32,
    /// The sector's level, copied from the tile for convenience in rules.
    pub z: i8,
}

/// Where an entity is, keyed by entity id.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Placement {
    /// The entity id.
    pub id: u32,
    /// Where it stands.
    pub at: Position,
}

/// What a sight line through the sectors gives (`02:02.12`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Sight {
    /// Nothing rises above the lower eye level.
    Clear,
    /// Something blocks the lower eye but not the higher one: the target is in
    /// cover, worth `rules.cover_steps` row steps.
    Covered {
        /// The row-step penalty to the attacker, negative.
        steps: i32,
    },
    /// Something rises above both eye levels.
    Blocked,
}

/// The map and who is standing on it.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct World {
    /// Width in tiles.
    pub width: i32,
    /// Height in tiles.
    pub height: i32,
    /// The tiles, row-major (`y * width + x`).
    pub tiles: Vec<Tile>,
    /// Where each entity stands, sorted by entity id.
    pub placements: Vec<Placement>,
}

impl World {
    /// An empty map of the given size.
    pub fn new(width: i32, height: i32) -> World {
        let count = (width.max(0) * height.max(0)) as usize;
        World {
            width,
            height,
            tiles: vec![Tile::default(); count],
            placements: Vec::new(),
        }
    }

    /// Whether a coordinate is on the map.
    pub fn in_bounds(&self, x: i32, y: i32) -> bool {
        x >= 0 && y >= 0 && x < self.width && y < self.height
    }

    /// The tile at a coordinate, if it is on the map.
    pub fn tile(&self, x: i32, y: i32) -> Option<&Tile> {
        if !self.in_bounds(x, y) {
            return None;
        }
        self.tiles.get((y * self.width + x) as usize)
    }

    /// Replace the tile at a coordinate.
    pub fn set_tile(&mut self, x: i32, y: i32, tile: Tile) {
        if self.in_bounds(x, y) {
            let index = (y * self.width + x) as usize;
            self.tiles[index] = tile;
        }
    }

    /// The entity standing at a coordinate, if any (`AD-33`: one per tile).
    pub fn occupant(&self, x: i32, y: i32) -> Option<u32> {
        self.placements
            .iter()
            .find(|placement| placement.at.x == x && placement.at.y == y)
            .map(|placement| placement.id)
    }

    /// Where an entity stands.
    pub fn position(&self, id: u32) -> Option<Position> {
        self.placements
            .iter()
            .find(|placement| placement.id == id)
            .map(|placement| placement.at)
    }

    /// Put an entity on the map, replacing any previous position.
    pub fn place(&mut self, id: u32, at: Position) {
        match self
            .placements
            .iter_mut()
            .find(|placement| placement.id == id)
        {
            Some(existing) => existing.at = at,
            None => {
                self.placements.push(Placement { id, at });
                // A stable order makes the state hash and the save independent
                // of the order entities were introduced.
                self.placements.sort_by_key(|placement| placement.id);
            }
        }
    }

    /// Remove an entity from the map.
    pub fn remove(&mut self, id: u32) {
        self.placements.retain(|placement| placement.id != id);
    }

    /// The distance between two positions in tiles, Chebyshev (`AD-33`).
    ///
    /// A keystep of one tile in any of the eight directions costs one tile of
    /// movement and one tile of distance.
    pub fn distance(a: Position, b: Position) -> i32 {
        (a.x - b.x).abs().max((a.y - b.y).abs())
    }

    /// The distance between two entities, when both are placed.
    pub fn distance_between(&self, a: u32, b: u32) -> Option<i32> {
        let first = self.position(a)?;
        let second = self.position(b)?;
        Some(World::distance(first, second))
    }

    /// Whether two entities are close enough for a melee strike.
    pub fn adjacent(&self, a: u32, b: u32) -> bool {
        self.distance_between(a, b).is_some_and(|tiles| tiles <= 1)
    }

    /// The elevation row steps an attacker gains or suffers against a target
    /// (`02:02.12`).
    ///
    /// `eye_level` is a level number, and `cap` is `rules.elevation_cap`.
    pub fn elevation_steps(attacker_eye: i32, target_eye: i32, cap: i32) -> i32 {
        let above = attacker_eye - target_eye;
        above.clamp(-cap, cap)
    }

    /// The line of sight between two positions (`02:02.12`).
    ///
    /// A supercover traversal compares each intervening sector's blocking height
    /// against the *lower* participant's eye level: nothing above it is clear;
    /// something above the lower but not the higher is cover; above both is
    /// blocked.
    pub fn sight(&self, from: Position, to: Position, cover_steps: i32) -> Sight {
        let lower_eye = from.z.min(to.z);
        let mut rises_above_lower = false;
        let mut rises_above_higher = false;
        for (x, y) in supercover(from.x, from.y, to.x, to.y) {
            if (x, y) == (from.x, from.y) || (x, y) == (to.x, to.y) {
                continue;
            }
            let Some(tile) = self.tile(x, y) else {
                // Off the map is a wall for a shot: the map's edge is solid.
                rises_above_lower = true;
                rises_above_higher = true;
                continue;
            };
            let top = i32::from(tile.level) + i32::from(tile.blocking);
            if top > i32::from(lower_eye) {
                rises_above_lower = true;
            }
            if top > i32::from(from.z.max(to.z)) {
                rises_above_higher = true;
            }
        }
        if rises_above_higher {
            Sight::Blocked
        } else if rises_above_lower {
            Sight::Covered { steps: cover_steps }
        } else {
            Sight::Clear
        }
    }
}

/// The tiles a line from `(x0,y0)` to `(x1,y1)` passes through, inclusive.
///
/// A supercover walk: every tile the segment touches is returned, which is what
/// makes a sight line conservative. Integer-only and symmetric in its endpoints,
/// so `sight(a,b)` and `sight(b,a)` see the same tiles.
pub fn supercover(x0: i32, y0: i32, x1: i32, y1: i32) -> Vec<(i32, i32)> {
    let mut tiles = Vec::new();
    let dx = (x1 - x0).abs();
    let dy = (y1 - y0).abs();
    let sx = if x0 < x1 { 1 } else { -1 };
    let sy = if y0 < y1 { 1 } else { -1 };
    let mut x = x0;
    let mut y = y0;
    let mut error = dx - dy;
    loop {
        tiles.push((x, y));
        if x == x1 && y == y1 {
            break;
        }
        let doubled = error * 2;
        if doubled > -dy {
            error -= dy;
            x += sx;
        }
        if doubled < dx {
            error += dx;
            y += sy;
        }
    }
    tiles
}

impl World {
    /// Read a map record (`03:03.3`).
    ///
    /// The record carries an explicit `tiles` array in row-major order, which is
    /// verbose for a large map and exactly right for M1's handful: the editor
    /// writes it, a mod can read it, and a diff shows the tile that changed.
    pub fn parse(data: &Json) -> Result<World, &'static str> {
        let width = data
            .get("width")
            .and_then(Json::as_i64)
            .ok_or("a map needs width")? as i32;
        let height = data
            .get("height")
            .and_then(Json::as_i64)
            .ok_or("a map needs height")? as i32;
        if width <= 0 || height <= 0 || width > 512 || height > 512 {
            return Err("a map must be between 1x1 and 512x512");
        }
        let Json::Arr(records) = data.get("tiles").ok_or("a map needs a tiles array")? else {
            return Err("tiles must be an array");
        };
        if records.len() != (width * height) as usize {
            return Err("tiles must have width * height entries");
        }
        let mut world = World::new(width, height);
        for (index, record) in records.iter().enumerate() {
            world.tiles[index] = Tile {
                level: record.get("level").and_then(Json::as_i64).unwrap_or(0) as i8,
                blocking: record.get("blocking").and_then(Json::as_i64).unwrap_or(0) as i8,
                terrain: record.get("terrain").and_then(Json::as_i64).unwrap_or(0) as u8,
                material: record.get("material").and_then(Json::as_i64).unwrap_or(10) as i32,
            };
        }
        Ok(world)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_keystep_in_any_direction_costs_one_tile() {
        let a = Position { x: 0, y: 0, z: 0 };
        let straight = Position { x: 3, y: 0, z: 0 };
        let diagonal = Position { x: 3, y: 3, z: 0 };
        assert_eq!(World::distance(a, straight), 3);
        assert_eq!(World::distance(a, diagonal), 3);
    }

    #[test]
    fn occupancy_is_one_entity_per_tile() {
        let mut world = World::new(4, 4);
        world.place(1, Position { x: 1, y: 1, z: 0 });
        world.place(2, Position { x: 2, y: 2, z: 0 });
        assert_eq!(world.occupant(1, 1), Some(1));
        assert_eq!(world.occupant(0, 0), None);
        // Moving onto an occupied tile leaves the occupancy unambiguous: the
        // caller resolves the contest first; the map itself keeps one id.
        world.place(1, Position { x: 2, y: 2, z: 0 });
        assert_eq!(world.occupant(2, 2), Some(1));
        assert_eq!(world.occupant(1, 1), None);
    }

    #[test]
    fn elevation_clamps_at_the_cap() {
        assert_eq!(World::elevation_steps(2, 0, 2), 2);
        assert_eq!(World::elevation_steps(0, 2, 2), -2);
        assert_eq!(World::elevation_steps(9, 0, 2), 2);
        assert_eq!(World::elevation_steps(1, 1, 2), 0);
    }

    #[test]
    fn a_supercover_walk_is_inclusive_and_symmetric() {
        let forward = supercover(0, 0, 3, 1);
        let mut backward = supercover(3, 1, 0, 0);
        backward.reverse();
        assert_eq!(forward, backward);
        assert_eq!(forward.first(), Some(&(0, 0)));
        assert_eq!(forward.last(), Some(&(3, 1)));
    }

    #[test]
    fn a_wall_between_two_equals_blocks_the_shot() {
        let mut world = World::new(5, 3);
        world.set_tile(
            2,
            1,
            Tile {
                level: 0,
                blocking: 3,
                terrain: 1,
                material: 30,
            },
        );
        let left = Position { x: 0, y: 1, z: 0 };
        let right = Position { x: 4, y: 1, z: 0 };
        assert_eq!(world.sight(left, right, -2), Sight::Blocked);
    }

    #[test]
    fn a_waist_high_wall_is_cover_for_an_equal_eye_and_clear_from_above() {
        let mut world = World::new(5, 3);
        world.set_tile(
            2,
            1,
            Tile {
                level: 0,
                blocking: 1,
                terrain: 1,
                material: 30,
            },
        );
        let low = Position { x: 0, y: 1, z: 0 };
        let other_low = Position { x: 4, y: 1, z: 0 };
        // With both eyes at grade, a waist-high wall rises above both, so the
        // shot is blocked rather than merely covered.
        assert_eq!(world.sight(low, other_low, -2), Sight::Blocked);
        // From a storey up, the same wall covers the lower participant without
        // blocking the higher one.
        let high = Position { x: 0, y: 1, z: 2 };
        assert_eq!(
            world.sight(high, other_low, -2),
            Sight::Covered { steps: -2 }
        );
        // Two participants on the high ground see over it.
        let other_high = Position { x: 4, y: 1, z: 2 };
        assert_eq!(world.sight(high, other_high, -2), Sight::Clear);
    }

    #[test]
    fn parses_a_map_record() {
        let world = World::parse(&Json::from_text(
            r#"{"width":2,"height":1,"tiles":[{"level":0,"blocking":0},{"level":1,"blocking":0,"material":30}]}"#,
        ))
        .expect("parse");
        assert_eq!(world.width, 2);
        assert_eq!(world.tile(1, 0).expect("tile").level, 1);
        assert_eq!(world.tile(1, 0).expect("tile").material, 30);
        assert!(World::parse(&Json::from_text(r#"{"width":2,"height":2,"tiles":[]}"#)).is_err());
    }
}
