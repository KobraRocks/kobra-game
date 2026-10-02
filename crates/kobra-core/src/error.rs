//! Status codes and the last-error buffer.
//!
//! Every `u32` an export returns is a status (01:01.5 rule 3): `0` is success,
//! and on failure `kobra_last_error` hands back a length-prefixed UTF-8 JSON
//! `{code, message, detail}` read out of linear memory. Nothing is thrown across
//! the boundary.
//!
//! The messages here are diagnostics for a developer reading a console, not
//! player-facing text: the shell decides what a player is told, and
//! `TestNoErrorLeaksPathsOrUsername` is about the launcher's `kobraerr`, not
//! this buffer. Keep them free of absolute paths anyway — a diagnostics payload
//! is surfaced to the page.

use std::sync::Mutex;

/// `0` — success. Exports return this or one of the failure codes below.
pub const OK: u32 = 0;

/// A [`crate::error::Status`] in its wire form.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u32)]
pub enum Status {
    /// The call succeeded.
    Ok = 0,
    /// The arguments were out of range or inconsistent.
    InvalidArgument = 1,
    /// The caller passed a handle that is not live.
    BadHandle = 2,
    /// The engine is not initialised, or already is.
    NotInitialised = 3,
    /// The request is valid but this build does not implement it yet.
    Unimplemented = 4,
    /// An internal invariant failed.
    Internal = 5,
}

impl Status {
    /// The wire value.
    pub const fn code(self) -> u32 {
        self as u32
    }
}

/// The most recent error, as the JSON bytes `kobra_last_error` hands back.
///
/// A `Mutex` rather than a thread-local: the core is compiled with `+atomics`
/// and will grow a pthread pool (AD-5, 07:07.4), and a thread-local error would
/// make "which thread's error?" a question the ABI cannot answer. The lock is
/// uncontended in the single-threaded path.
static LAST_ERROR: Mutex<Option<Vec<u8>>> = Mutex::new(None);

/// Record an error for the next `kobra_last_error` call.
///
/// The JSON is assembled by hand: this module must not depend on a serializer
/// (see `Cargo.toml`), and the shape is three fields.
pub fn set(status: Status, message: &str, detail: Option<&str>) {
    let mut payload = String::with_capacity(message.len() + 64);
    payload.push_str("{\"code\":\"");
    payload.push_str(code_name(status));
    payload.push_str("\",\"message\":");
    push_json_string(&mut payload, message);
    payload.push_str(",\"detail\":");
    match detail {
        Some(d) => push_json_string(&mut payload, d),
        None => payload.push_str("null"),
    }
    payload.push('}');
    if let Ok(mut slot) = LAST_ERROR.lock() {
        *slot = Some(payload.into_bytes());
    }
}

/// Whether an error is currently recorded.
///
/// Exports use this to add a fallback message **only** when the layer below did
/// not already explain itself: overwriting a specific cause with "it failed" is
/// how a diagnostics payload becomes useless.
pub fn is_set() -> bool {
    match LAST_ERROR.lock() {
        Ok(slot) => slot.is_some(),
        Err(poisoned) => poisoned.into_inner().is_some(),
    }
}

/// Clear a previous error. Called at the start of every export that can fail,
/// so `kobra_last_error` is never stale.
pub fn clear() {
    if let Ok(mut slot) = LAST_ERROR.lock() {
        *slot = None;
    }
}

/// Borrow the current error bytes and their address, for `kobra_last_error`.
///
/// The returned pointer stays valid until the next `set`/`clear`, which is the
/// documented lifetime of the ABI's error buffer: the caller copies the string
/// out immediately.
///
/// The address is a `usize`, not a `u32`: the ABI narrows addresses to 32 bits
/// at the C boundary, but inside the crate a host address is 64-bit, and
/// truncating it would produce a dangling pointer rather than a compile error.
pub fn current() -> (usize, usize) {
    let guard = match LAST_ERROR.lock() {
        Ok(g) => g,
        // A poisoned lock means another thread panicked while holding it. There
        // is no useful recovery, and panicking again would trap the whole
        // instance, so report "no error" — the caller's status code is already
        // the authoritative failure signal.
        Err(poisoned) => poisoned.into_inner(),
    };
    match guard.as_ref() {
        Some(bytes) => (bytes.as_ptr() as usize, bytes.len()),
        None => (0, 0),
    }
}

/// The stable wire name of a status, used as the `code` field.
const fn code_name(status: Status) -> &'static str {
    match status {
        Status::Ok => "ok",
        Status::InvalidArgument => "invalid_argument",
        Status::BadHandle => "bad_handle",
        Status::NotInitialised => "not_initialised",
        Status::Unimplemented => "unimplemented",
        Status::Internal => "internal",
    }
}

/// Append `value` as a JSON string literal, escaping what JSON requires.
fn push_json_string(out: &mut String, value: &str) {
    out.push('"');
    for ch in value.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                out.push_str("\\u");
                out.push_str(&format!("{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn last_error_round_trips_and_clears() {
        clear();
        assert_eq!(current().1, 0);
        set(Status::InvalidArgument, "bad \"len\"", Some("len=0"));
        let (ptr, len) = current();
        assert_ne!(ptr, 0);
        // SAFETY: `current` returns a live slice's address and length.
        let bytes = unsafe { std::slice::from_raw_parts(ptr as *const u8, len) };
        assert_eq!(
            std::str::from_utf8(bytes).expect("utf-8"),
            "{\"code\":\"invalid_argument\",\"message\":\"bad \\\"len\\\"\",\"detail\":\"len=0\"}"
        );
        clear();
        assert_eq!(current().1, 0);
    }

    #[test]
    fn status_ok_is_zero_because_zero_is_success() {
        assert_eq!(Status::Ok.code(), 0);
    }
}
