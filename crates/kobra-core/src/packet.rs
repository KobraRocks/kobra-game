//! The RenderPacket and the shared frame ring.
//!
//! The packet was one half of a wire format whose other half was
//! `src/web/shared/packet.ts`, kept honest by two tests that asserted the same
//! numbers from both sides. The TypeScript half was removed with the web runtime
//! (AD-38), so `src/core/tests/packet.rs` is now the only assertion — but the
//! format is still a contract with whatever consumes it, so a field that moves
//! here is a [`PACKET_VERSION`] bump.
//!
//! Layout, and why it is this shape (01:01.3, 01:01.6, AD-4, 07:07.3):
//!
//! ```text
//! ring header (64 bytes)   geometry + one publication counter per slot
//!   slot 0 ─┐
//!   slot 1  ├ 96-byte slot header, then DRAW_CAPACITY draw items
//!   slot 2 ─┘
//! ```
//!
//! The ring lives in the sim's **shared** linear memory, so the render worker
//! reads a slot in place with no per-frame copy. Three slots means the sim never
//! blocks on the renderer; a publication counter per slot (a seqlock) means the
//! renderer never reads a torn packet; and a slot the renderer did not consume
//! is overwritten rather than queued, so the system degrades by skipping a
//! *render* frame instead of accumulating latency.
//!
//! Nothing here is float. The packet is integer-only and carries `q16` fixed
//! point; the renderer converts, and no float ever flows back (AD-6, 01:01.3).

use core::cell::UnsafeCell;
use core::sync::atomic::{AtomicU32, Ordering};

/// The packet's wire version. A mismatch is a packaging error: the engine and
/// the host that reads a frame ship together (01:01.5 rule 5).
pub const PACKET_VERSION: u32 = 1;

/// The ring header's wire version.
pub const RING_VERSION: u32 = 1;

/// How many slots the frame ring has. Triple buffering (AD-4, 07:07.3).
pub const SLOT_COUNT: usize = 3;

/// Bytes of ring header before slot 0.
pub const RING_HEADER_BYTES: usize = 64;

/// `PacketHeader` as it appears on the wire.
pub const PACKET_HEADER_BYTES: usize = 84;

/// Bytes from a slot's start to its first draw item: the packet header plus
/// padding to a 16-byte boundary.
pub const SLOT_HEADER_BYTES: usize = 96;

/// `DrawItem` as it appears on the wire.
pub const DRAW_ITEM_BYTES: usize = 68;

/// Draw items a slot can hold. Draw items past this are *dropped* and counted,
/// never grown into (01:01.6, 07:07.5).
pub const DRAW_CAPACITY: usize = 1024;

/// Bytes in one slot.
pub const SLOT_STRIDE: usize = SLOT_HEADER_BYTES + DRAW_CAPACITY * DRAW_ITEM_BYTES;

/// Bytes in the whole ring.
pub const RING_BYTES: usize = RING_HEADER_BYTES + SLOT_COUNT * SLOT_STRIDE;

// ---------------------------------------------------------------------------
// Ring header byte offsets
// ---------------------------------------------------------------------------

/// `u32` — [`RING_VERSION`].
pub const RING_OFF_VERSION: usize = 0;
/// `u32` — [`SLOT_COUNT`].
pub const RING_OFF_SLOT_COUNT: usize = 4;
/// `u32` — [`SLOT_STRIDE`].
pub const RING_OFF_SLOT_STRIDE: usize = 8;
/// `u32` — bytes from a slot's start to its first draw item, i.e.
/// [`SLOT_HEADER_BYTES`].
///
/// The reader needs the *offset*, not the raw struct size: the packet header is
/// [`PACKET_HEADER_BYTES`] long and then padded to a 16-byte boundary.
pub const RING_OFF_ITEM_OFFSET: usize = 12;
/// `u32` — [`DRAW_ITEM_BYTES`].
pub const RING_OFF_DRAW_ITEM_BYTES: usize = 16;
/// `u32` — [`DRAW_CAPACITY`].
pub const RING_OFF_DRAW_CAPACITY: usize = 20;
/// `u32` — cumulative draw items dropped because a slot was full.
pub const RING_OFF_OVERFLOW_ITEMS: usize = 24;
/// `u32` — reserved, always 0. Present so the geometry block is 8 u32.
pub const RING_OFF_RESERVED: usize = 28;
/// `u32` × [`SLOT_COUNT`] — the publication counter of each slot.
///
/// Written with `Release`, read with `Acquire`. Odd means "being written"; even
/// means settled, and the settled frame is `value / 2 - 1`.
pub const RING_OFF_SETTLE: usize = 32;

// ---------------------------------------------------------------------------
// Packet header byte offsets
// ---------------------------------------------------------------------------

/// `u32` — [`PACKET_VERSION`].
pub const HDR_OFF_PACKET_VERSION: usize = 0;
/// `u32` — monotonically increasing frame id, per sim.
pub const HDR_OFF_FRAME_ID: usize = 4;
/// `u32` — the sim tick this frame was published on.
pub const HDR_OFF_TICK: usize = 8;
/// `u32` — [`flags::PACKET_OVERFLOW`] and friends.
pub const HDR_OFF_FLAGS: usize = 12;
/// `u32` — a hint for the renderer's camera asset, or 0.
pub const HDR_OFF_CAMERA_ASSET_HINT: usize = 16;
/// `i32` — camera x, `q16` world units.
pub const HDR_OFF_CAMERA_X: usize = 20;
/// `i32` — camera y, `q16` world units.
pub const HDR_OFF_CAMERA_Y: usize = 24;
/// `i32` — camera zoom, `q16`; `65536` is 1:1.
pub const HDR_OFF_CAMERA_ZOOM: usize = 28;
/// `i32` — camera rotation, `q16` radians.
pub const HDR_OFF_CAMERA_ROT: usize = 32;
/// `i32` — camera pitch, `q16` radians.
pub const HDR_OFF_CAMERA_PITCH: usize = 36;
/// `u32` — viewport width in device pixels.
pub const HDR_OFF_VIEWPORT_W: usize = 40;
/// `u32` — viewport height in device pixels.
pub const HDR_OFF_VIEWPORT_H: usize = 44;
/// `u32` — draw items written.
pub const HDR_OFF_ITEM_COUNT: usize = 48;
/// `u32` — text items written.
pub const HDR_OFF_TEXT_COUNT: usize = 52;
/// `u32` — vfx items written.
pub const HDR_OFF_VFX_COUNT: usize = 56;
/// `u32` — byte offset of the draw items, from the packet header.
pub const HDR_OFF_OFFSET_ITEMS: usize = 60;
/// `u32` — byte offset of the text items, from the packet header.
pub const HDR_OFF_OFFSET_TEXT: usize = 64;
/// `u32` — byte offset of the vfx items, from the packet header.
pub const HDR_OFF_OFFSET_VFX: usize = 68;
/// `u32` — presentation-only PRNG state; never saved, never read by the sim.
pub const HDR_OFF_RNG_COSMETIC: usize = 72;
/// `u32` — low half of the sim's state hash.
pub const HDR_OFF_STATE_HASH_LO: usize = 76;
/// `u32` — high half of the sim's state hash.
pub const HDR_OFF_STATE_HASH_HI: usize = 80;

/// Packet header flag bits.
pub mod flags {
    /// At least one draw item was dropped because the slot was full.
    pub const PACKET_OVERFLOW: u32 = 1 << 0;
}

/// `DrawItem::kind` values (01:01.3).
pub mod kind {
    /// An axis-aligned rectangle: the basic primitive, and a billboard's core.
    pub const QUAD: u8 = 0;
    /// A map tile.
    pub const TILE: u8 = 1;
    /// A skinned mesh; the renderer samples the clip named by `clip_id`.
    pub const MESH: u8 = 2;
    /// A nine-slice UI panel.
    pub const NINE_SLICE: u8 = 3;
    /// A trail.
    pub const TRAIL: u8 = 4;
}

/// `DrawItem::blend` values.
pub mod blend {
    /// Straight alpha-over.
    pub const NORMAL: u8 = 0;
    /// Additive.
    pub const ADD: u8 = 1;
    /// Multiplicative.
    pub const MULTIPLY: u8 = 2;
    /// Alpha-tested cutout: writes depth, so 3D geometry occludes it correctly
    /// (AD-27).
    pub const CUTOUT: u8 = 3;
}

/// `DrawItem::flags` bits.
pub mod item_flags {
    /// Mirror horizontally.
    pub const FLIP_X: u32 = 1 << 0;
    /// Mirror vertically.
    pub const FLIP_Y: u32 = 1 << 1;
    /// Ignore scene lighting.
    pub const IGNORE_LIGHT: u32 = 1 << 2;
    /// Position and size are in UI space, not world space.
    pub const UI_SPACE: u32 = 1 << 3;
}

/// The fixed-size packet header.
///
/// `#[repr(C)]` and asserted against [`PACKET_HEADER_BYTES`] by a test: this
/// struct *is* the wire format, not a description of it.
#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct PacketHeader {
    /// [`PACKET_VERSION`].
    pub packet_version: u32,
    /// Monotonically increasing per sim.
    pub frame_id: u32,
    /// The tick this frame was built on.
    pub tick: u32,
    /// [`flags`] bits.
    pub flags: u32,
    /// Hint for the renderer's camera resource, or 0.
    pub camera_asset_hint: u32,
    /// Camera x, `q16`.
    pub camera_x_q16: i32,
    /// Camera y, `q16`.
    pub camera_y_q16: i32,
    /// Camera zoom, `q16`.
    pub camera_zoom_q16: i32,
    /// Camera rotation, `q16` radians.
    pub camera_rot_q16: i32,
    /// Camera pitch, `q16` radians.
    pub camera_pitch_q16: i32,
    /// Viewport width in device pixels.
    pub viewport_w: u32,
    /// Viewport height in device pixels.
    pub viewport_h: u32,
    /// Draw items written.
    pub item_count: u32,
    /// Text items written.
    pub text_count: u32,
    /// Vfx items written.
    pub vfx_count: u32,
    /// Byte offset of the draw items from the packet header.
    pub offset_items: u32,
    /// Byte offset of the text items from the packet header.
    pub offset_text: u32,
    /// Byte offset of the vfx items from the packet header.
    pub offset_vfx: u32,
    /// Presentation-only PRNG state. Never saved, never read by the sim.
    pub rng_cosmetic_state: u32,
    /// Low half of the state hash.
    pub state_hash_lo: u32,
    /// High half of the state hash.
    pub state_hash_hi: u32,
}

/// One draw item. `x`/`y` are the **centre**; `sx`/`sy` are the size.
///
/// `kind == kind::MESH` items additionally carry a clip, its time, a cross-fade
/// partner and an LOD, because the renderer samples animation and no pose ever
/// enters the simulation (AD-30).
#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct DrawItem {
    /// Interned content id, never a GPU handle (01:01.3 property 1).
    pub asset_id: u32,
    /// Frame index within the asset's atlas or animation set.
    pub atlas_slot: u32,
    /// Z-order.
    pub layer: u16,
    /// [`kind`].
    pub kind: u8,
    /// [`blend`].
    pub blend: u8,
    /// Centre x, `q16`.
    pub x_q16: i32,
    /// Centre y, `q16`.
    pub y_q16: i32,
    /// Centre z, `q16`.
    pub z_q16: i32,
    /// Width, `q16`.
    pub sx_q16: i32,
    /// Height, `q16`.
    pub sy_q16: i32,
    /// Rotation, `q16` radians.
    pub rot_q16: i32,
    /// Tint, `0xRRGGBBAA` (red in the high byte).
    pub tint_rgba8: u32,
    /// [`item_flags`] bits.
    pub flags: u32,
    /// Current animation clip id (`kind::MESH` only).
    pub clip_id: u32,
    /// Clip time, `q16` seconds.
    pub clip_time_q16: u32,
    /// Cross-fade partner clip id.
    pub clip_blend_id: u32,
    /// Cross-fade weight, `q16`.
    pub clip_blend_q16: u32,
    /// LOD tier.
    pub lod: u32,
    /// Skin/palette handle for GPU skinning.
    pub skin_id: u32,
}

/// A read/write view of the one frame ring this instance owns.
///
/// Constructed only by [`ring`]. The pointer is into this module's static
/// storage, so it is valid for the life of the instance.
#[derive(Clone, Copy)]
pub struct Ring {
    base: *mut u8,
}

// The ring is a fixed region of the instance's shared linear memory. One writer
// (the sim thread) and many readers (the render worker, through the SAB) is the
// design; interior mutation is what the raw pointer is for, and every accessor
// below is documented with its own invariant.
unsafe impl Send for Ring {}
unsafe impl Sync for Ring {}

#[repr(C, align(16))]
struct RingStorage(UnsafeCell<[u8; RING_BYTES]>);

// SAFETY: `RingStorage` is a byte array with no invariants to violate, and every
// access to it goes through the seqlock in `Ring::publish` and the host
// renderer's readers.
unsafe impl Sync for RingStorage {}

/// The ring.
///
/// The `UnsafeCell` is load-bearing: without it the compiler is entitled to
/// place an all-zero static in a read-only section, and `Ring::init` — which
/// writes the geometry header — would fault instead of running.
static RING: RingStorage = RingStorage(UnsafeCell::new([0; RING_BYTES]));

/// The frame ring, at a fixed address in this instance's linear memory.
///
/// `kobra_ring` reports this address to the worker, which posts the underlying
/// `SharedArrayBuffer` to the render worker. The ring is never moved and never
/// grown (07:07.5).
pub fn ring() -> Ring {
    Ring {
        base: RING.0.get() as *mut u8,
    }
}

impl Ring {
    /// The ring's address in linear memory, for `kobra_ring`.
    ///
    /// A `usize`: the ABI narrows it to 32 bits at the C boundary, but a Rust
    /// host links this crate directly (AD-38) and reads the ring in place, where
    /// a truncated address would be a dangling pointer.
    pub fn addr(self) -> usize {
        self.base as usize
    }

    /// The ring's size in bytes, for `kobra_ring`.
    pub fn size(self) -> u32 {
        RING_BYTES as u32
    }

    /// Write the geometry header and mark every slot unpublished.
    ///
    /// Called once from `kobra_init`. Slots start at an odd (in-progress) counter
    /// so a renderer that somehow runs before the first tick finds nothing
    /// settled rather than reading a slot of zeros.
    pub fn init(self) {
        // SAFETY: `self.base` points at `RING_BYTES` bytes of this instance's
        // memory and the ring is the only writer of its own header.
        unsafe {
            write_u32(self.base, RING_OFF_VERSION, RING_VERSION);
            write_u32(self.base, RING_OFF_SLOT_COUNT, SLOT_COUNT as u32);
            write_u32(self.base, RING_OFF_SLOT_STRIDE, SLOT_STRIDE as u32);
            write_u32(self.base, RING_OFF_ITEM_OFFSET, SLOT_HEADER_BYTES as u32);
            write_u32(self.base, RING_OFF_DRAW_ITEM_BYTES, DRAW_ITEM_BYTES as u32);
            write_u32(self.base, RING_OFF_DRAW_CAPACITY, DRAW_CAPACITY as u32);
            write_u32(self.base, RING_OFF_OVERFLOW_ITEMS, 0);
            write_u32(self.base, RING_OFF_RESERVED, 0);
            for slot in 0..SLOT_COUNT {
                settle_at(self.base, slot).store(1, Ordering::Relaxed);
            }
        }
    }

    /// The address of slot `slot`'s packet header.
    pub fn slot(self, slot: usize) -> *mut u8 {
        debug_assert!(slot < SLOT_COUNT, "slot index out of range");
        // SAFETY: `slot < SLOT_COUNT` and the ring is `RING_BYTES` long, which
        // is exactly the header plus `SLOT_COUNT` strides.
        unsafe { self.base.add(RING_HEADER_BYTES + slot * SLOT_STRIDE) }
    }

    /// Publish `header` and `items` into slot `header.frame_id % SLOT_COUNT`.
    ///
    /// Seqlock write: the counter goes odd, the body is written, then the
    /// counter goes even with `Release`, which is what stops the renderer from
    /// reading a half-written packet (AD-4, 07:07.3).
    ///
    /// Returns the number of items that did not fit. The caller records them; a
    /// dropped draw item is always preferable to a hitch or a crash (01:01.6).
    pub fn publish(self, header: &PacketHeader, items: &[DrawItem]) -> u32 {
        let slot = (header.frame_id as usize) % SLOT_COUNT;
        let base = self.slot(slot);
        // SAFETY: `slot < SLOT_COUNT`, so the counter is inside the ring header.
        let settle = unsafe { settle_at(self.base, slot) };
        let seq = header.frame_id.wrapping_mul(2);

        settle.store(seq | 1, Ordering::Relaxed);
        // SAFETY: `base` addresses a full slot, and a write of the header plus
        // at most `DRAW_CAPACITY` items stays inside it by construction.
        unsafe {
            let hdr = base as *mut PacketHeader;
            std::ptr::write_unaligned(hdr, *header);
            let items_base = base.add(SLOT_HEADER_BYTES) as *mut DrawItem;
            for (i, item) in items.iter().take(DRAW_CAPACITY).enumerate() {
                std::ptr::write_unaligned(items_base.add(i), *item);
            }
        }
        settle.store(seq.wrapping_add(2), Ordering::Release);
        items.len().saturating_sub(DRAW_CAPACITY) as u32
    }

    /// Add to the ring's cumulative dropped-item counter and return the total.
    pub fn note_overflow(self, dropped: u32) -> u32 {
        let cell = unsafe {
            // SAFETY: the counter lives in the ring header, which this instance
            // owns; it is atomic so a renderer read is well defined.
            &*(self.base.add(RING_OFF_OVERFLOW_ITEMS) as *const AtomicU32)
        };
        // Relaxed is enough: the counter is a diagnostic, not a synchronisation
        // point, and the packet's own flag bit is the per-frame signal.
        cell.fetch_add(dropped, Ordering::Relaxed) + dropped
    }
}

/// The seqlock counter of `slot`.
///
/// # Safety
///
/// `base` must point at the ring header of a ring with at least `slot + 1`
/// slots.
unsafe fn settle_at(base: *mut u8, slot: usize) -> &'static AtomicU32 {
    debug_assert!(slot < SLOT_COUNT);
    // SAFETY: the caller guarantees `base` addresses a ring header, and
    // `RING_OFF_SETTLE + slot * 4` lies inside the 64-byte header for every
    // `slot < SLOT_COUNT`.
    unsafe { &*(base.add(RING_OFF_SETTLE + slot * 4) as *const AtomicU32) }
}

/// Write a `u32` at `base + offset`.
///
/// # Safety
///
/// `base + offset .. base + offset + 4` must lie inside the ring.
unsafe fn write_u32(base: *mut u8, offset: usize, value: u32) {
    // SAFETY: the caller guarantees the range; `write_unaligned` avoids
    // requiring the ring to be 4-aligned beyond its 16-byte base alignment.
    unsafe { std::ptr::write_unaligned(base.add(offset) as *mut u32, value) };
}
