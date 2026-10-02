//! The WASM/JS boundary: the hand-written C ABI (AD-2, 01:01.5).
//!
//! Everything crosses as integers, plus a length-prefixed `memcpy` of UTF-8
//! JSON in linear memory for the things that are not hot. There are two imports
//! (`kobra_log`, `kobra_script_hook`) and a version-stamped set of exports. A small
//! boundary is what makes it possible to keep both sides honest.
//!
//! # The projections
//!
//! M3 adds four read-only projections the shell's pages are built from —
//! [`kobra_journal`], [`kobra_dialogue`], [`kobra_world`] and [`kobra_shops`] — plus
//! [`kobra_narrate`], which turns the events a command produced into the string ids
//! a player reads. They are projections of state the sim already owns, so a UI
//! never has to parse a save or re-derive a rule, and they carry **string ids**
//! rather than English (`AD-22`). [`kobra_actions`] is the fifth, and the one that
//! keeps the interface honest: which contextual actions exist, and whether each
//! is legal right now, is a rule and therefore an engine answer (`02:02.13`).
//!
//! # Rules that make it safe to call
//!
//! 1. **Handles, not globals.** [`kobra_new_campaign`] returns an opaque handle,
//!    so several sims coexist with no shared mutable state.
//! 2. **Status codes, never exceptions.** Every `u32` return is a status; `0` is
//!    success. On failure [`kobra_last_error`] returns a JSON `{code, message,
//!    detail}` in memory.
//! 3. **A trap is fatal to that sim handle only.** This crate is built with
//!    `panic = "abort"` (05:05.2), so a panic is a WebAssembly trap rather than
//!    something `catch_unwind` could convert into a status. The worker therefore
//!    treats a trap as it treats any other trap: it discards the handle and
//!    rebuilds from the last save, and never keeps running on a suspect sim
//!    (01:01.5 rule 4).
//! 4. **ABI version is checked first.** [`kobra_abi_version`] must equal what the
//!    glue was built against; a mismatch is a packaging error and fails cleanly
//!    rather than misbehaving.
//!
//! # Buffers with a lifetime
//!
//! The exports that return `(ptr << 32) | len` — the ring, the outbox, the save,
//! a preview, a status, a validation report, the error — hand back bytes that
//! stay valid until the **next** export call. That is one shared buffer rather
//! than one per export, which is what the ABI's contract has always said
//! (01:01.5); the caller copies out immediately.
//!
//! **`kobra_alloc` and `kobra_free` do not clear the last error.** They are the calls
//! a caller makes *between* a failure and reading it — `kobra_alloc` to place the
//! next argument — so clearing on them would erase the diagnostic one line before
//! it is read, which is what "the command was refused" with no reason was. An
//! allocation that fails records its own error; one that succeeds changes
//! nothing.

use std::alloc::{alloc, dealloc, Layout};
use std::sync::Mutex;

use crate::error::{self, Status};
use crate::json::Json;
use crate::packet::{self, Ring};
use crate::sim;

/// Diagnostic severities for a host's log sink.
pub mod log_level {
    /// Something went wrong.
    pub const ERROR: u32 = 0;
    /// Something is probably wrong.
    pub const WARN: u32 = 1;
    /// Progress and configuration.
    pub const INFO: u32 = 2;
    /// Developer detail.
    pub const DEBUG: u32 = 3;
}

/// A host's diagnostic sink: `(severity, message)`.
pub type LogSink = fn(u32, &str);

/// The host's diagnostic sink, installed with [`set_log_sink`].
///
/// A registered sink rather than a link-time import. The engine is native now
/// (AD-38), and a link-time import would make every host that calls a logging
/// entry point — `kobra_init` is one — a link error unless it happened to export
/// the symbol. A host that wants diagnostics installs a sink; one that does not
/// gets silence, which is a choice rather than a defect.
static LOG_SINK: Mutex<Option<LogSink>> = Mutex::new(None);

/// Install the host's diagnostic sink.
///
/// Called once at boot. The engine never formats a message for a player and never
/// includes a path (`01:01.12` rule 2); the host decides where a message goes.
pub fn set_log_sink(sink: LogSink) {
    if let Ok(mut slot) = LOG_SINK.lock() {
        *slot = Some(sink);
    }
}

/// Send one warning to the host's diagnostic sink, if one is installed.
pub fn warn(message: &str) {
    log(log_level::WARN, message);
}

/// Send one message to the host's diagnostic sink, if one is installed.
fn log(level: u32, message: &str) {
    if let Ok(slot) = LOG_SINK.lock() {
        if let Some(sink) = *slot {
            sink(level, message);
        }
    }
}

/// Borrow a UTF-8 string the caller placed in linear memory.
///
/// # Safety
///
/// `ptr`/`len` must describe a live, initialised, UTF-8 region of this
/// instance's linear memory, which is the ABI's documented contract for every
/// byte argument (`kobra_alloc` writes such a region and hands it over).
unsafe fn borrow_str<'a>(ptr: u32, len: u32) -> Result<&'a str, Status> {
    if ptr == 0 || len == 0 {
        return Err(Status::InvalidArgument);
    }
    // SAFETY: the caller promises the region is live and initialised for `len`
    // bytes, and the host writes UTF-8 into it.
    let bytes = unsafe { std::slice::from_raw_parts(ptr as *const u8, len as usize) };
    std::str::from_utf8(bytes).map_err(|_| Status::InvalidArgument)
}

/// Pack an output buffer's address and length into the ABI's `u64`.
fn packed(bytes: Vec<u8>) -> u64 {
    let (ptr, len) = sim::set_output(bytes);
    (ptr as u64) << 32 | len as u64
}

/// The alignment `kobra_alloc` hands out.
const ALLOC_ALIGN: usize = 8;

/// `kobra_abi_version()` — the version of this ABI.
///
/// The worker compares it with its own constant at boot (01:01.8 step 8).
#[no_mangle]
pub extern "C" fn kobra_abi_version() -> u32 {
    crate::ABI_VERSION
}

/// `kobra_alloc(len) -> ptr` — allocate `len` bytes the caller can write into.
///
/// Returns 0 when `len` is 0 or the allocator refuses. The caller must hand the
/// same `(ptr, len)` pair to [`kobra_free`].
#[no_mangle]
pub extern "C" fn kobra_alloc(len: u32) -> u32 {
    if len == 0 {
        return 0;
    }
    match Layout::from_size_align(len as usize, ALLOC_ALIGN) {
        // SAFETY: `layout` has a non-zero size, which is `alloc`'s only
        // precondition beyond validity, and it came from `Layout`.
        Ok(layout) => unsafe { alloc(layout) as u32 },
        Err(_) => {
            error::set(
                Status::Internal,
                "the engine could not allocate a buffer",
                None,
            );
            0
        }
    }
}

/// `kobra_free(ptr, len)` — release a buffer from [`kobra_alloc`].
#[no_mangle]
pub extern "C" fn kobra_free(ptr: u32, len: u32) {
    if ptr == 0 || len == 0 {
        return;
    }
    if let Ok(layout) = Layout::from_size_align(len as usize, ALLOC_ALIGN) {
        // SAFETY: the caller promises `ptr` came from `kobra_alloc(len)` and has
        // not been freed, which is the pair contract documented above.
        unsafe { dealloc(ptr as *mut u8, layout) };
    }
}

/// `kobra_init(seed, cfg_ptr, cfg_len) -> status` — initialise the engine.
///
/// `seed` seeds the campaign's PRNG (AD-6). `cfg` is reserved; this build takes
/// no engine configuration document and refuses a non-empty one rather than
/// ignoring it, because silently dropping configuration is how a build ends up
/// disagreeing with its own manifest.
#[no_mangle]
pub extern "C" fn kobra_init(seed: u32, cfg_ptr: u32, cfg_len: u32) -> u32 {
    error::clear();
    if cfg_len != 0 || cfg_ptr != 0 {
        return sim::fail(
            Status::Unimplemented,
            "this build takes no engine configuration document",
        );
    }
    match sim::init(seed) {
        Ok(()) => {
            let message = format!(
                "worldspiracy core: abi {}, ladder {}",
                crate::ABI_VERSION,
                sim::ladder().name
            );
            log(log_level::INFO, &message);
            Status::Ok.code()
        }
        Err(status) => sim::fail(status, "the engine could not be initialised"),
    }
}

/// `kobra_load_content(ptr, len) -> (ptr << 32) | len` — merge and install
/// content, returning the load report (`03:03.8`, `03:03.9`).
#[no_mangle]
pub extern "C" fn kobra_load_content(ptr: u32, len: u32) -> u64 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let document = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => {
            sim::fail(status, "the content document is not valid UTF-8");
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    let value = match Json::parse(document) {
        Ok(value) => value,
        Err(_) => {
            sim::fail(
                Status::InvalidArgument,
                "the content document is not valid JSON",
            );
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    let report = sim::load_content(&value);
    packed(report.to_string().into_bytes())
}

/// `kobra_validate_content(ptr, len) -> (ptr << 32) | len` — validate without
/// installing (`AD-15`).
///
/// The editor calls this before a save or a publish; the game calls
/// [`kobra_load_content`], which runs the same validator.
#[no_mangle]
pub extern "C" fn kobra_validate_content(ptr: u32, len: u32) -> u64 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let document = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => {
            sim::fail(status, "the content document is not valid UTF-8");
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    let value = match Json::parse(document) {
        Ok(value) => value,
        Err(_) => {
            sim::fail(
                Status::InvalidArgument,
                "the content document is not valid JSON",
            );
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    packed(crate::content::load(&value).report.to_string().into_bytes())
}

/// `kobra_content_hash() -> hash` — the installed content's rules hash.
#[no_mangle]
pub extern "C" fn kobra_content_hash() -> u64 {
    error::clear();
    sim::content_hash()
}

/// `kobra_new_campaign(cfg_ptr, cfg_len) -> sim | 0` — create a campaign.
///
/// The configuration document names the scenario (`{"scenario": "…"}`) and may
/// override the seed. Returns a handle, or `0` on failure.
#[no_mangle]
pub extern "C" fn kobra_new_campaign(cfg_ptr: u32, cfg_len: u32) -> u32 {
    error::clear();
    let config = if cfg_len == 0 {
        Json::object()
    } else {
        // SAFETY: the ABI contract for a byte argument.
        match unsafe { borrow_str(cfg_ptr, cfg_len) }
            .and_then(|text| Json::parse(text).map_err(|_| Status::InvalidArgument))
        {
            Ok(value) => value,
            Err(status) => {
                return sim::fail(status, "the campaign configuration is not valid JSON")
            }
        }
    };
    match sim::new_campaign(&config) {
        Ok(handle) => handle,
        Err(status) => {
            // 0, not the status: a handle is an index and 3 is a perfectly good
            // handle, so returning the status here would hand the caller a live
            // sim it never asked for. The reason is in `kobra_last_error`, and a
            // specific one is never overwritten by a generic fallback.
            if !error::is_set() {
                sim::fail(status, "no campaign could be created");
            }
            0
        }
    }
}

/// `kobra_load(ptr, len) -> sim | 0` — restore a save (`AD-11`).
#[no_mangle]
pub extern "C" fn kobra_load(ptr: u32, len: u32) -> u32 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => return sim::fail(status, "the save is not valid UTF-8"),
    };
    match sim::load_save(text) {
        Ok(handle) => handle,
        Err(status) => {
            if !error::is_set() {
                sim::fail(status, "the save could not be loaded");
            }
            0
        }
    }
}

/// `kobra_save(handle) -> (ptr << 32) | len` — serialise the sim (`AD-11`).
#[no_mangle]
pub extern "C" fn kobra_save(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| {
        crate::save::envelope(&sim.to_save()).to_string()
    }) {
        Ok(text) => packed(text.into_bytes()),
        Err(status) => {
            sim::fail(status, "the sim handle is not live");
            packed(Vec::new())
        }
    }
}

/// `kobra_free_sim(handle)` — release a sim handle.
#[no_mangle]
pub extern "C" fn kobra_free_sim(handle: u32) {
    error::clear();
    let _ = sim::free_sim(handle);
}

/// `kobra_command(handle, ptr, len) -> status` — the one mutation entry point
/// (`02:02.10`).
#[no_mangle]
pub extern "C" fn kobra_command(handle: u32, ptr: u32, len: u32) -> u32 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => return sim::fail(status, "the command is not valid UTF-8"),
    };
    let command = match Json::parse(text) {
        Ok(value) => value,
        Err(_) => return sim::fail(Status::InvalidArgument, "the command is not valid JSON"),
    };
    match sim::with_sim(handle, |sim| sim.command(&command)) {
        Ok(Ok(())) => Status::Ok.code(),
        Ok(Err(status)) => {
            // The **reason** is carried as the detail, because the caller's
            // diagnostic has to name why: "the command was refused" is the same
            // sentence for a move into a wall, a purchase the party cannot afford
            // and a walk taken mid-fight, and only the reason tells them apart.
            // It is a string id the locale resolves (`AD-22`), never English.
            sim::fail_with_reason(
                status,
                "the command was refused",
                sim::last_failure_reason(),
            )
        }
        Err(status) => sim::fail(status, "the sim handle is not live"),
    }
}

/// `kobra_outbox(handle) -> (ptr << 32) | len` — drain the events since the last
/// call, as a JSON array.
#[no_mangle]
pub extern "C" fn kobra_outbox(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| {
        let events = Json::Arr(std::mem::take(&mut sim.outbox));
        events.to_string()
    }) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"[]".to_vec()),
    }
}

/// `kobra_preview(handle, ptr, len) -> (ptr << 32) | len` — predict an action
/// without committing it or drawing from the RNG (`02:02.10`).
#[no_mangle]
pub extern "C" fn kobra_preview(handle: u32, ptr: u32, len: u32) -> u64 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(_) => return packed(b"{\"error\":\"bad_request\"}".to_vec()),
    };
    let request = match Json::parse(text) {
        Ok(value) => value,
        Err(_) => return packed(b"{\"error\":\"bad_request\"}".to_vec()),
    };
    match sim::with_sim(handle, |sim| sim.preview(&request).to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"error\":\"bad_handle\"}".to_vec()),
    }
}

/// `kobra_status(handle) -> (ptr << 32) | len` — a diagnostics document
/// (tick, panel, phase, counts, hashes).
#[no_mangle]
pub extern "C" fn kobra_status(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim::status(sim).to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"error\":\"bad_handle\"}".to_vec()),
    }
}

/// `kobra_roster(handle) -> (ptr << 32) | len` — the characters, for the UI.
#[no_mangle]
pub extern "C" fn kobra_roster(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.roster().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"[]".to_vec()),
    }
}

/// `kobra_tick(handle, elapsed_ticks) -> status` — advance the sim.
#[no_mangle]
pub extern "C" fn kobra_tick(handle: u32, elapsed_ticks: u32) -> u32 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.tick(elapsed_ticks)) {
        Ok(_) => Status::Ok.code(),
        Err(status) => sim::fail(status, "the sim handle is not live"),
    }
}

/// `kobra_resize(handle, width, height) -> status` — report the drawing surface.
#[no_mangle]
pub extern "C" fn kobra_resize(handle: u32, width: u32, height: u32) -> u32 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.resize(width, height)) {
        Ok(()) => Status::Ok.code(),
        Err(status) => sim::fail(status, "the sim handle is not live"),
    }
}

/// `kobra_ring() -> (addr << 32) | size` — the shared frame ring.
#[no_mangle]
pub extern "C" fn kobra_ring() -> u64 {
    error::clear();
    let ring: Ring = packet::ring();
    (ring.addr() as u64) << 32 | u64::from(ring.size())
}

/// `kobra_render_packet(handle) -> (addr << 32) | len` — build and publish a
/// frame.
#[no_mangle]
pub extern "C" fn kobra_render_packet(handle: u32) -> u64 {
    error::clear();
    let ring = packet::ring();
    match sim::with_sim(handle, |sim| sim.publish_frame(ring)) {
        Ok((addr, dropped)) => {
            if dropped > 0 {
                let message = format!("packet overflow: {dropped} draw item(s) dropped");
                log(log_level::WARN, &message);
            }
            let len = packet::SLOT_HEADER_BYTES;
            (addr as u64) << 32 | len as u64
        }
        Err(_) => 0,
    }
}

/// `kobra_hash_state(handle) -> hash` — the determinism oracle (07:07.8).
#[no_mangle]
pub extern "C" fn kobra_hash_state(handle: u32) -> u64 {
    error::clear();
    sim::with_sim(handle, |sim| sim.hash_state()).unwrap_or(0)
}

/// `kobra_script_record(ptr, len) -> status` — record the enabled Tier-2 scripts
/// (`AD-8`, `03:03.8` step 9).
#[no_mangle]
pub extern "C" fn kobra_script_record(ptr: u32, len: u32) -> u32 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => return sim::fail(status, "the script set is not valid UTF-8"),
    };
    let value = match Json::parse(text) {
        Ok(value) => value,
        Err(_) => return sim::fail(Status::InvalidArgument, "the script set is not valid JSON"),
    };
    let mut scripts = Vec::new();
    if let Some(Json::Arr(items)) = value.get("scripts") {
        for item in items {
            let id = item.get("id").and_then(Json::as_str).unwrap_or("");
            let hash = item.get("hash").and_then(Json::as_str).unwrap_or("");
            scripts.push((id.to_string(), hash.to_string()));
        }
    }
    crate::script::record(scripts);
    Status::Ok.code()
}

/// `kobra_script_report() -> (ptr << 32) | len` — which scripts are live and which
/// broke (`03:03.7`).
#[no_mangle]
pub extern "C" fn kobra_script_report() -> u64 {
    error::clear();
    packed(crate::script::report().to_string().into_bytes())
}

/// `kobra_journal(handle) -> (ptr << 32) | len` — the quest journal, its lines and
/// the clock, as the shell renders them (`02:02.7`).
#[no_mangle]
pub extern "C" fn kobra_journal(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.journal_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"error\":\"bad_handle\"}".to_vec()),
    }
}

/// `kobra_dialogue(handle) -> (ptr << 32) | len` — the open conversation, or
/// `{"open": false}` (`02:02.7`).
#[no_mangle]
pub extern "C" fn kobra_dialogue(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.dialogue_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"open\":false}".to_vec()),
    }
}

/// `kobra_world(handle) -> (ptr << 32) | len` — the generated world, its clock, its
/// stations and the party's positions (`02:02.6`).
#[no_mangle]
pub extern "C" fn kobra_world(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.world_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"error\":\"bad_handle\"}".to_vec()),
    }
}

/// `kobra_shops(handle) -> (ptr << 32) | len` — the merchant counters, the party's
/// inventory, and the prices **this** character's Lifestyle band pays
/// (`4c:1255-1272`).
#[no_mangle]
pub extern "C" fn kobra_shops(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.shops_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"error\":\"bad_handle\"}".to_vec()),
    }
}

/// `kobra_actions(handle) -> (ptr << 32) | len` — the contextual actions the
/// party may take here, each with an `enabled` flag and a reason (`02:02.13`).
///
/// This is the engine's answer, not the shell's: whether Wait is legal in a
/// fight and whether Talk has anyone to talk to are rules, and a view that
/// derived them would be a second rules engine quietly disagreeing with the
/// first (`AD-15`).
#[no_mangle]
pub extern "C" fn kobra_actions(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.actions_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{\"actions\":[]}".to_vec()),
    }
}

/// `kobra_signals(handle) -> (ptr << 32) | len` — the declared display gates and
/// their engine-computed truth values (`09:09.4`).
///
/// The shell gates a layout node on one of these booleans and never evaluates a
/// predicate, which is what keeps the interface from becoming a second rules
/// engine: the condition vocabulary lives in the engine, and so does its answer.
#[no_mangle]
pub extern "C" fn kobra_signals(handle: u32) -> u64 {
    error::clear();
    match sim::with_sim(handle, |sim| sim.signals_view().to_string()) {
        Ok(text) => packed(text.into_bytes()),
        Err(_) => packed(b"{}".to_vec()),
    }
}

/// `kobra_check_ui(ptr, len) -> (ptr << 32) | len` — validate the layout documents
/// the caller supplies (`09:09.5`).
///
/// A browser cannot enumerate a mod's tree, so the cross-references that need the
/// file list — is the layout declared in `overrides`, does it exist under
/// `assets/` — run where the list is known, exactly as `check_overrides` takes the
/// `assets` array. The installed content supplies the declared signals.
#[no_mangle]
pub extern "C" fn kobra_check_ui(ptr: u32, len: u32) -> u64 {
    error::clear();
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(status) => {
            sim::fail(status, "the UI check request is not valid UTF-8");
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    let request = match Json::parse(text) {
        Ok(value) => value,
        Err(_) => {
            sim::fail(
                Status::InvalidArgument,
                "the UI check request is not valid JSON",
            );
            return packed(b"{\"ok\":false,\"items\":[]}".to_vec());
        }
    };
    packed(sim::check_ui(&request).to_string().into_bytes())
}

/// `kobra_narrate(handle, ptr, len) -> (ptr << 32) | len` — the string ids for a
/// batch of events, so the UI can render a log without knowing the rules.
///
/// The events cross as the *outbox document* a `kobra_command` produced; the answer
/// is a list of `{event, text_key, args}`, with `args` carrying the string ids of
/// the things the line names (a quest, an item, a faction). Every kind the core
/// emits has a line, and a kind it does not know is passed through unchanged
/// rather than dropped, so a UI can always say *something* happened.
#[no_mangle]
pub extern "C" fn kobra_narrate(handle: u32, ptr: u32, len: u32) -> u64 {
    error::clear();
    if handle == 0 {
        return packed(b"[]".to_vec());
    }
    // SAFETY: the ABI contract for a byte argument.
    let text = match unsafe { borrow_str(ptr, len) } {
        Ok(text) => text,
        Err(_) => return packed(b"[]".to_vec()),
    };
    let value = match Json::parse(text) {
        Ok(value) => value,
        Err(_) => return packed(b"[]".to_vec()),
    };
    packed(sim::narrate(&value).to_string().into_bytes())
}

/// `kobra_last_error() -> (addr << 32) | len` — the last error as UTF-8 JSON.
#[no_mangle]
pub extern "C" fn kobra_last_error() -> u64 {
    let (ptr, len) = error::current();
    (ptr as u64) << 32 | len as u64
}

#[cfg(test)]
mod tests {
    use super::*;

    /// What the installed sink recorded. A `fn` pointer cannot capture, so the
    /// recording has to happen in a static.
    static RECEIVED: Mutex<Vec<(u32, String)>> = Mutex::new(Vec::new());

    fn record(level: u32, message: &str) {
        if let Ok(mut log) = RECEIVED.lock() {
            log.push((level, message.to_string()));
        }
    }

    /// Regression, `AD-38`. The diagnostics channel used to be a link-time import
    /// (`kobra_log`) that only a wasm host provided, so a native host calling a
    /// logging entry point — `kobra_init` is one — was an unresolved symbol. A
    /// sink the host installs is the fix; this is what says so.
    ///
    /// The assertion is a containment check rather than an exact one: other tests
    /// run in parallel and `kobra_init` logs too, so the list can hold entries this
    /// test did not write.
    #[test]
    fn a_warning_reaches_the_installed_sink() {
        if let Ok(mut log) = RECEIVED.lock() {
            log.clear();
        }
        set_log_sink(record);

        warn("a saved sector was off the map");

        let received = RECEIVED.lock().expect("the sink's list");
        assert!(
            received.iter().any(|(level, message)| {
                *level == log_level::WARN && message == "a saved sector was off the map"
            }),
            "the warning reached the sink: {received:?}"
        );
    }
}
