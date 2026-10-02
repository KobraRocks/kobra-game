//! The RenderPacket wire format, asserted from the Rust side.
//!
//! The TypeScript half of this contract — `tests/web/packet.test.mjs` against
//! `src/web/shared/packet.ts` — was removed with the web runtime (AD-38), so this
//! is now the only assertion. The format still crosses a boundary: it is a byte
//! layout a host renderer reads, so a field that moves without a version bump is a
//! silent corruption rather than a compile error (01:01.3, 01:01.6).

use std::mem::{offset_of, size_of};
use std::sync::atomic::{AtomicU32, Ordering};

use kobra_core::packet::{
    self, blend, flags, item_flags, kind, DrawItem, PacketHeader, Ring, DRAW_CAPACITY,
    DRAW_ITEM_BYTES, PACKET_HEADER_BYTES, RING_OFF_DRAW_CAPACITY, RING_OFF_DRAW_ITEM_BYTES,
    RING_OFF_ITEM_OFFSET, RING_OFF_OVERFLOW_ITEMS, RING_OFF_SETTLE, RING_OFF_SLOT_COUNT,
    RING_OFF_SLOT_STRIDE, RING_OFF_VERSION, SLOT_COUNT, SLOT_HEADER_BYTES, SLOT_STRIDE,
};

#[test]
fn wire_sizes_are_what_the_shared_contract_says() {
    assert_eq!(size_of::<PacketHeader>(), PACKET_HEADER_BYTES);
    assert_eq!(size_of::<DrawItem>(), DRAW_ITEM_BYTES);
    assert_eq!(SLOT_HEADER_BYTES % 16, 0, "slots start 16-byte aligned");
    assert_eq!(
        SLOT_STRIDE,
        SLOT_HEADER_BYTES + packet::DRAW_CAPACITY * DRAW_ITEM_BYTES
    );
}

#[test]
fn packet_header_fields_are_at_the_documented_offsets() {
    // A sample of the offsets, including every one the renderer reads.
    assert_eq!(offset_of!(PacketHeader, packet_version), 0);
    assert_eq!(offset_of!(PacketHeader, frame_id), 4);
    assert_eq!(offset_of!(PacketHeader, tick), 8);
    assert_eq!(offset_of!(PacketHeader, flags), 12);
    assert_eq!(offset_of!(PacketHeader, camera_x_q16), 20);
    assert_eq!(offset_of!(PacketHeader, viewport_w), 40);
    assert_eq!(offset_of!(PacketHeader, item_count), 48);
    assert_eq!(offset_of!(PacketHeader, offset_items), 60);
    assert_eq!(offset_of!(PacketHeader, state_hash_lo), 76);
    assert_eq!(offset_of!(PacketHeader, state_hash_hi), 80);
}

#[test]
fn draw_item_fields_are_at_the_documented_offsets() {
    assert_eq!(offset_of!(DrawItem, asset_id), 0);
    assert_eq!(offset_of!(DrawItem, layer), 8);
    assert_eq!(offset_of!(DrawItem, kind), 10);
    assert_eq!(offset_of!(DrawItem, blend), 11);
    assert_eq!(offset_of!(DrawItem, x_q16), 12);
    assert_eq!(offset_of!(DrawItem, sy_q16), 28);
    assert_eq!(offset_of!(DrawItem, tint_rgba8), 36);
    assert_eq!(offset_of!(DrawItem, flags), 40);
    assert_eq!(offset_of!(DrawItem, skin_id), 64);
    // Enum-like discriminants are part of the wire format, so pin them.
    assert_eq!(kind::QUAD, 0);
    assert_eq!(blend::NORMAL, 0);
    assert_eq!(blend::CUTOUT, 3);
    assert_eq!(item_flags::FLIP_X, 1);
    assert_eq!(flags::PACKET_OVERFLOW, 1);
}

/// One test owns the process-wide ring, because the ring is a singleton by
/// design and `cargo test` runs tests in parallel threads.
#[test]
fn the_ring_publishes_a_frame_a_renderer_can_read_in_place() {
    let ring: Ring = packet::ring();
    ring.init();

    // Geometry, as the renderer reads it.
    // SAFETY: the ring header is `RING_HEADER_BYTES` long and these offsets are
    // inside it; nothing else in this process writes them.
    unsafe {
        let base = ring.addr() as *const u8;
        assert_eq!(read_u32(base, RING_OFF_VERSION), packet::RING_VERSION);
        assert_eq!(read_u32(base, RING_OFF_SLOT_COUNT), SLOT_COUNT as u32);
        assert_eq!(read_u32(base, RING_OFF_SLOT_STRIDE), SLOT_STRIDE as u32);
        // The reader needs the item offset and the item stride, not the raw
        // packet-header size: the header is padded to a 16-byte boundary.
        assert_eq!(
            read_u32(base, RING_OFF_ITEM_OFFSET),
            SLOT_HEADER_BYTES as u32
        );
        assert_eq!(
            read_u32(base, RING_OFF_DRAW_ITEM_BYTES),
            DRAW_ITEM_BYTES as u32
        );
        assert_eq!(read_u32(base, RING_OFF_DRAW_CAPACITY), DRAW_CAPACITY as u32);
    }

    let header = PacketHeader {
        packet_version: packet::PACKET_VERSION,
        frame_id: 0,
        tick: 3,
        flags: 0,
        camera_asset_hint: 0,
        camera_x_q16: -1,
        camera_y_q16: 2,
        camera_zoom_q16: 65_536,
        camera_rot_q16: 0,
        camera_pitch_q16: 0,
        viewport_w: 1280,
        viewport_h: 720,
        item_count: 1,
        text_count: 0,
        vfx_count: 0,
        offset_items: SLOT_HEADER_BYTES as u32,
        offset_text: 0,
        offset_vfx: 0,
        rng_cosmetic_state: 7,
        state_hash_lo: 0xdead_beef,
        state_hash_hi: 0x0123_4567,
    };
    let item = DrawItem {
        asset_id: 0,
        atlas_slot: 0,
        layer: 3,
        kind: kind::QUAD,
        blend: blend::NORMAL,
        x_q16: 100,
        y_q16: -200,
        z_q16: 0,
        sx_q16: 32_768,
        sy_q16: 19_660,
        rot_q16: 0,
        tint_rgba8: 0xe8a3_3dff,
        flags: 0,
        clip_id: 0,
        clip_time_q16: 0,
        clip_blend_id: 0,
        clip_blend_q16: 0,
        lod: 0,
        skin_id: 0,
    };

    assert_eq!(ring.publish(&header, &[item]), 0, "the item fits");

    // The settle counter is even, so the slot is settled and readable.
    let settle = settle_at(ring.addr(), 0);
    let seq = settle.load(Ordering::Acquire);
    assert_eq!(seq % 2, 0, "a published slot has an even counter");
    assert_eq!(seq, 2, "frame 0 settles at counter 2");

    // SAFETY: slot 0's header is `PACKET_HEADER_BYTES` long and was just written.
    unsafe {
        let slot = ring.slot(0);
        assert_eq!(read_u32(slot, offset_of!(PacketHeader, frame_id)), 0);
        assert_eq!(read_u32(slot, offset_of!(PacketHeader, tick)), 3);
        assert_eq!(read_u32(slot, offset_of!(PacketHeader, item_count)), 1);
        assert_eq!(read_u32(slot, offset_of!(PacketHeader, viewport_w)), 1280);
        assert_eq!(
            read_u32(slot, offset_of!(PacketHeader, state_hash_hi)),
            0x0123_4567
        );
        let items = slot.add(SLOT_HEADER_BYTES) as *const DrawItem;
        // SAFETY: one item was published at exactly this address.
        let read_back = std::ptr::read_unaligned(items);
        assert_eq!(read_back.x_q16, 100);
        assert_eq!(read_back.sy_q16, 19_660);
        assert_eq!(read_back.tint_rgba8, 0xe8a3_3dff);
        assert_eq!(read_back.layer, 3);
        assert_eq!(read_back.kind, kind::QUAD);
    }

    // The next frame lands in the next slot, and the counter follows it.
    let mut second = header;
    second.frame_id = 1;
    ring.publish(&second, &[]);
    let seq1 = settle_at(ring.addr(), 1).load(Ordering::Acquire);
    assert_eq!(seq1, 4);
    assert_eq!(settle.load(Ordering::Acquire), 2, "slot 0 is untouched");

    // The dropped-item counter is cumulative and readable from the ring header,
    // which is where diagnostics gets it from (01:01.6, 07:07.5).
    assert_eq!(ring.note_overflow(2), 2);
    assert_eq!(ring.note_overflow(3), 5);
    // SAFETY: the counter lives at a fixed offset in the ring header.
    let total = unsafe { read_u32(ring.addr() as *const u8, RING_OFF_OVERFLOW_ITEMS) };
    assert_eq!(total, 5);
}

/// The seqlock counter of `slot`.
fn settle_at(ring_addr: usize, slot: usize) -> &'static AtomicU32 {
    // SAFETY: `RING_OFF_SETTLE + slot * 4` is inside the 64-byte ring header.
    unsafe { &*((ring_addr as *const u8).add(RING_OFF_SETTLE + slot * 4) as *const AtomicU32) }
}

/// # Safety
///
/// `base + offset .. + 4` must be readable.
unsafe fn read_u32(base: *const u8, offset: usize) -> u32 {
    unsafe { std::ptr::read_unaligned(base.add(offset) as *const u32) }
}
