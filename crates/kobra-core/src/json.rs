//! A tiny, dependency-free JSON reader and writer (01:01.5).
//!
//! Everything the ABI carries that is not the render packet crosses as UTF-8
//! JSON: commands, events, previews, validation reports, content hand-offs and
//! the save envelope (`02:02.10`, `01:01.5`). The core ships with an empty
//! dependency list on purpose (`Cargo.toml`), so this module exists rather than
//! `serde_json`.
//!
//! # Integers only, and that is a rule rather than a limitation
//!
//! Numbers are `i64`. A JSON number carrying a fraction or an exponent is a
//! **parse error**, not a float that is silently truncated. The simulation has
//! no floats on any path that can change state (AD-6, 02:02.9 rule 4), and the
//! cheapest way to keep that true is to make `f64` unreachable from the wire.
//! A caller that wants a float can put it in a string.
//!
//! # Object order is preserved
//!
//! Objects keep insertion order, and the writer emits that order. A save is
//! therefore **byte-stable** across a save/load/save round trip, which is the
//! property `tests/replay` asserts (05:05.4). Parsing an object does not sort
//! its keys, so the order in the bytes is the order in the bytes.

use std::fmt::Write as _;

/// A JSON value, integer-only.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Json {
    /// `null`.
    Null,
    /// `true` / `false`.
    Bool(bool),
    /// A whole number.
    Num(i64),
    /// A string.
    Str(String),
    /// An array, in order.
    Arr(Vec<Json>),
    /// An object, in insertion order.
    Obj(Vec<(String, Json)>),
}

/// Why a JSON document could not be read.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JsonError {
    /// The byte offset the reader stopped at.
    pub offset: usize,
    /// A short, path-free explanation.
    pub message: &'static str,
}

impl std::fmt::Display for JsonError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{} at byte {}", self.message, self.offset)
    }
}

impl std::error::Error for JsonError {}

type Result<T> = std::result::Result<T, JsonError>;

impl Json {
    /// An empty object.
    pub fn object() -> Json {
        Json::Obj(Vec::new())
    }

    /// An object from `(key, value)` pairs, in order.
    pub fn obj<const N: usize>(fields: [(&str, Json); N]) -> Json {
        Json::Obj(
            fields
                .into_iter()
                .map(|(key, value)| (key.to_string(), value))
                .collect(),
        )
    }

    /// A string value from anything string-like.
    pub fn string(value: impl Into<String>) -> Json {
        Json::Str(value.into())
    }

    /// An array of values.
    pub fn arr(values: impl IntoIterator<Item = Json>) -> Json {
        Json::Arr(values.into_iter().collect())
    }

    /// The value at `key` in an object, or `None` for another kind.
    pub fn get(&self, key: &str) -> Option<&Json> {
        match self {
            Json::Obj(fields) => fields
                .iter()
                .find(|(name, _)| name == key)
                .map(|(_, value)| value),
            _ => None,
        }
    }

    /// The value at `key`, which must be an object.
    pub fn field(&self, key: &str) -> Result<&Json> {
        self.get(key).ok_or(JsonError {
            offset: 0,
            message: "a required field is missing",
        })
    }

    /// Add or replace `key` in an object, keeping a replaced key's position.
    pub fn set(&mut self, key: &str, value: Json) {
        if let Json::Obj(fields) = self {
            if let Some(slot) = fields.iter_mut().find(|(name, _)| name == key) {
                slot.1 = value;
                return;
            }
            fields.push((key.to_string(), value));
        }
    }

    /// The number, when this is one.
    pub fn as_i64(&self) -> Option<i64> {
        match self {
            Json::Num(value) => Some(*value),
            _ => None,
        }
    }

    /// The number, or `fallback` for another kind. For optional tuning keys,
    /// where the shipped default lives in code.
    pub fn i64_or(&self, fallback: i64) -> i64 {
        self.as_i64().unwrap_or(fallback)
    }

    /// The string, when this is one.
    pub fn as_str(&self) -> Option<&str> {
        match self {
            Json::Str(value) => Some(value),
            _ => None,
        }
    }

    /// The boolean, when this is one.
    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Json::Bool(value) => Some(*value),
            _ => None,
        }
    }

    /// The array, when this is one.
    pub fn as_arr(&self) -> Option<&[Json]> {
        match self {
            Json::Arr(values) => Some(values),
            _ => None,
        }
    }

    /// The object's fields, when this is one.
    pub fn as_obj(&self) -> Option<&[(String, Json)]> {
        match self {
            Json::Obj(fields) => Some(fields),
            _ => None,
        }
    }

    /// Read this document from UTF-8 bytes.
    pub fn parse(text: &str) -> Result<Json> {
        let mut reader = Reader {
            bytes: text.as_bytes(),
            at: 0,
        };
        reader.skip_space();
        let value = reader.value()?;
        reader.skip_space();
        if reader.at != reader.bytes.len() {
            return Err(reader.fail("trailing bytes after the document"));
        }
        Ok(value)
    }
}

impl std::fmt::Display for Json {
    /// Write this document as compact UTF-8 JSON.
    ///
    /// There is deliberately no inherent `to_string` beside this: one that
    /// shadowed `Display` could call itself through `ToString`, and clippy's
    /// `inherent_to_string_shadow_display` is what caught it.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let mut out = String::new();
        write_value(&mut out, self);
        f.write_str(&out)
    }
}

/// Write `value` compactly: no spaces, objects in insertion order.
fn write_value(out: &mut String, value: &Json) {
    match value {
        Json::Null => out.push_str("null"),
        Json::Bool(true) => out.push_str("true"),
        Json::Bool(false) => out.push_str("false"),
        Json::Num(number) => {
            let _ = write!(out, "{number}");
        }
        Json::Str(text) => write_string(out, text),
        Json::Arr(values) => {
            out.push('[');
            for (index, item) in values.iter().enumerate() {
                if index > 0 {
                    out.push(',');
                }
                write_value(out, item);
            }
            out.push(']');
        }
        Json::Obj(fields) => {
            out.push('{');
            for (index, (key, item)) in fields.iter().enumerate() {
                if index > 0 {
                    out.push(',');
                }
                write_string(out, key);
                out.push(':');
                write_value(out, item);
            }
            out.push('}');
        }
    }
}

/// Write a JSON string with the two mandatory escapes and the short forms.
fn write_string(out: &mut String, text: &str) {
    out.push('"');
    for ch in text.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// The recursive-descent reader.
struct Reader<'a> {
    bytes: &'a [u8],
    at: usize,
}

impl Reader<'_> {
    fn fail(&self, message: &'static str) -> JsonError {
        JsonError {
            offset: self.at,
            message,
        }
    }

    fn peek(&self) -> Option<u8> {
        self.bytes.get(self.at).copied()
    }

    fn skip_space(&mut self) {
        while matches!(self.peek(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.at += 1;
        }
    }

    fn value(&mut self) -> Result<Json> {
        self.skip_space();
        match self.peek() {
            Some(b'{') => self.object(),
            Some(b'[') => self.array(),
            Some(b'"') => Ok(Json::Str(self.string()?)),
            Some(b't') => self.literal("true", Json::Bool(true)),
            Some(b'f') => self.literal("false", Json::Bool(false)),
            Some(b'n') => self.literal("null", Json::Null),
            Some(b'-' | b'0'..=b'9') => self.number(),
            _ => Err(self.fail("expected a JSON value")),
        }
    }

    fn literal(&mut self, text: &str, value: Json) -> Result<Json> {
        if self.bytes[self.at..].starts_with(text.as_bytes()) {
            self.at += text.len();
            Ok(value)
        } else {
            Err(self.fail("expected a JSON literal"))
        }
    }

    fn object(&mut self) -> Result<Json> {
        self.at += 1; // '{'
        let mut fields = Vec::new();
        self.skip_space();
        if self.peek() == Some(b'}') {
            self.at += 1;
            return Ok(Json::Obj(fields));
        }
        loop {
            self.skip_space();
            if self.peek() != Some(b'"') {
                return Err(self.fail("expected an object key"));
            }
            let key = self.string()?;
            self.skip_space();
            if self.peek() != Some(b':') {
                return Err(self.fail("expected ':' after an object key"));
            }
            self.at += 1;
            let value = self.value()?;
            // A duplicate key is refused rather than silently ordered: the
            // registry merges by id and a duplicate would make "which won"
            // depend on the parser.
            if fields.iter().any(|(name, _): &(String, Json)| name == &key) {
                return Err(self.fail("duplicate object key"));
            }
            fields.push((key, value));
            self.skip_space();
            match self.peek() {
                Some(b',') => self.at += 1,
                Some(b'}') => {
                    self.at += 1;
                    return Ok(Json::Obj(fields));
                }
                _ => return Err(self.fail("expected ',' or '}' in an object")),
            }
        }
    }

    fn array(&mut self) -> Result<Json> {
        self.at += 1; // '['
        let mut values = Vec::new();
        self.skip_space();
        if self.peek() == Some(b']') {
            self.at += 1;
            return Ok(Json::Arr(values));
        }
        loop {
            values.push(self.value()?);
            self.skip_space();
            match self.peek() {
                Some(b',') => self.at += 1,
                Some(b']') => {
                    self.at += 1;
                    return Ok(Json::Arr(values));
                }
                _ => return Err(self.fail("expected ',' or ']' in an array")),
            }
        }
    }

    fn string(&mut self) -> Result<String> {
        self.at += 1; // opening quote
        let mut out = String::new();
        loop {
            let byte = self.peek().ok_or(self.fail("unterminated string"))?;
            match byte {
                b'"' => {
                    self.at += 1;
                    return Ok(out);
                }
                b'\\' => {
                    self.at += 1;
                    let escape = self.peek().ok_or(self.fail("unterminated escape"))?;
                    self.at += 1;
                    match escape {
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => out.push(self.unicode_escape()?),
                        _ => return Err(self.fail("unknown escape")),
                    }
                }
                b if b < 0x20 => return Err(self.fail("a control character in a string")),
                _ => {
                    // Copy one whole UTF-8 sequence. The document is already a
                    // `&str`, so the bytes are valid by construction.
                    let start = self.at;
                    let len = utf8_len(self.bytes[start]);
                    let end = start + len;
                    if end > self.bytes.len() {
                        return Err(self.fail("truncated UTF-8 sequence"));
                    }
                    out.push_str(std::str::from_utf8(&self.bytes[start..end]).map_err(|_| {
                        JsonError {
                            offset: start,
                            message: "invalid UTF-8 in a string",
                        }
                    })?);
                    self.at = end;
                }
            }
        }
    }

    fn unicode_escape(&mut self) -> Result<char> {
        let hex = self
            .bytes
            .get(self.at..self.at + 4)
            .ok_or(self.fail("truncated \\u escape"))?;
        let text = std::str::from_utf8(hex).map_err(|_| self.fail("bad \\u escape"))?;
        let code = u32::from_str_radix(text, 16).map_err(|_| self.fail("bad \\u escape"))?;
        self.at += 4;
        char::from_u32(code).ok_or(self.fail("\\u escape is not a character"))
    }

    fn number(&mut self) -> Result<Json> {
        let start = self.at;
        if self.peek() == Some(b'-') {
            self.at += 1;
        }
        let digits_start = self.at;
        while matches!(self.peek(), Some(b'0'..=b'9')) {
            self.at += 1;
        }
        if self.at == digits_start {
            return Err(self.fail("expected a digit"));
        }
        if matches!(self.peek(), Some(b'.' | b'e' | b'E')) {
            // Integers only: a float on the wire would put an `f64` on a path
            // that can change state (AD-6, 02:02.9 rule 4).
            return Err(self.fail("numbers must be integers"));
        }
        let text = std::str::from_utf8(&self.bytes[start..self.at]).map_err(|_| JsonError {
            offset: start,
            message: "invalid number",
        })?;
        let value: i64 = text.parse().map_err(|_| JsonError {
            offset: start,
            message: "number is out of range",
        })?;
        Ok(Json::Num(value))
    }
}

/// The length of the UTF-8 sequence starting with `first`.
fn utf8_len(first: u8) -> usize {
    if first < 0x80 {
        1
    } else if first >> 5 == 0b110 {
        2
    } else if first >> 4 == 0b1110 {
        3
    } else {
        4
    }
}

/// A test-only convenience: parse a literal, panicking on a defect in the test.
#[cfg(test)]
impl Json {
    /// Parse a literal document, for expressing fixtures readably.
    pub fn from_text(text: &str) -> Json {
        Json::parse(text).expect("test document must parse")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_shapes_the_abi_crosses() {
        let value = Json::parse(r#"{"a":1,"b":[true,null,"x"],"c":{}}"#).expect("parse");
        assert_eq!(value.get("a").and_then(Json::as_i64), Some(1));
        assert_eq!(
            value.get("b").and_then(Json::as_arr).map(<[Json]>::len),
            Some(3)
        );
        assert_eq!(
            value.get("b").and_then(Json::as_arr).unwrap()[1],
            Json::Null
        );
        assert_eq!(Json::parse("[]").expect("parse"), Json::Arr(vec![]));
        assert_eq!(Json::parse("null").expect("parse"), Json::Null);
    }

    #[test]
    fn refuses_a_float_rather_than_truncating_it() {
        // AD-6: no floats on any state-visible path, and the wire is where one
        // would sneak in.
        for text in ["1.5", "1e3", "0.0", "-2.25"] {
            let error = Json::parse(text).expect_err("must refuse a float");
            assert_eq!(error.message, "numbers must be integers", "{text}");
        }
    }

    #[test]
    fn refuses_a_duplicate_key() {
        assert!(Json::parse(r#"{"a":1,"a":2}"#).is_err());
    }

    #[test]
    fn round_trips_byte_for_byte_and_keeps_key_order() {
        let text = r#"{"z":1,"a":[1,2,{"m":"n"}],"s":"a\"b\\c"}"#;
        let value = Json::parse(text).expect("parse");
        assert_eq!(value.to_string(), text);
    }

    #[test]
    fn escapes_control_characters() {
        let value = Json::Str("a\nb\tc\u{1}".to_string());
        assert_eq!(value.to_string(), r#""a\nb\tc\u0001""#);
        assert_eq!(Json::parse(&value.to_string()).expect("parse"), value);
    }

    #[test]
    fn decodes_a_unicode_escape() {
        assert_eq!(
            Json::parse(r#""\u00e9""#).expect("parse"),
            Json::Str("é".to_string())
        );
    }

    #[test]
    fn rejects_trailing_bytes() {
        assert!(Json::parse("{} x").is_err());
    }
}
