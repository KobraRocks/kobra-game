//! The dice algebra (02:02.2, "Dice algebra").
//!
//! 4C uses four shapes, and all four are exact integer draws from the seeded
//! RNG with explicit rounding recorded in the content:
//!
//! | Shape | Example | Meaning |
//! |---|---|---|
//! | `d%` | `"d%"` | the resolution roll, `00` is zero |
//! | `NdM` | `"1d10"`, `"3d10"` | `N` dice of `M` sides, each `1..=M` |
//! | `Na-M` | `"1-10"`, `"3-30"`, `"5"` | explicit spellings of `1d10`, `3d10`, and a constant |
//! | `d%/N` rounded up | `{ "dice": "d%", "div": 3, "round": "up", "min": 1 }` | Repute (`4c:208`) |
//!
//! The only genuinely composite case is `1d10/2 (round down, minimum of 1)`
//! (`4c:1425`), which is why [`Amount`] carries `div`, `round` and `min` rather
//! than being a bare die. A modded item uses the same vocabulary as a shipped
//! one (`02:02.8`).

use super::super::json::Json;
use crate::rng::Rng;

/// Which way a division rounds.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Rounding {
    /// Toward negative infinity.
    Down,
    /// Toward positive infinity.
    Up,
}

/// One die shape.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Dice {
    /// A fixed number.
    Constant(i32),
    /// The percentile roll, `0..=99`.
    Percent,
    /// `count` dice of `sides` sides each, each `1..=sides`.
    NdM {
        /// How many dice.
        count: u32,
        /// How many sides each.
        sides: u32,
    },
}

/// A dice expression with its declared rounding (02:02.2).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Amount {
    /// The base draw.
    pub dice: Dice,
    /// Divide the draw by this, when present.
    pub div: Option<i32>,
    /// How the division rounds.
    pub round: Rounding,
    /// A floor applied after rounding.
    pub min: Option<i32>,
}

impl Amount {
    /// A constant.
    pub const fn constant(value: i32) -> Amount {
        Amount {
            dice: Dice::Constant(value),
            div: None,
            round: Rounding::Down,
            min: None,
        }
    }

    /// A percentile roll divided by `div`, rounded up, minimum 1 — Repute
    /// (`4c:208`).
    pub const fn repute() -> Amount {
        Amount {
            dice: Dice::Percent,
            div: Some(3),
            round: Rounding::Up,
            min: Some(1),
        }
    }

    /// `count` dice of `sides` sides, with no divisor or floor.
    ///
    /// The constructor a kernel uses for a duration the spec prints (`1d10`,
    /// `3-30`), so no Rust code has to build a JSON document to describe a die.
    pub const fn dice(count: u32, sides: u32) -> Amount {
        Amount {
            dice: Dice::NdM { count, sides },
            div: None,
            round: Rounding::Down,
            min: None,
        }
    }

    /// A dice expression with a floor, e.g. `1d10` minimum 1.
    pub const fn at_least(self, min: i32) -> Amount {
        Amount {
            min: Some(min),
            ..self
        }
    }

    /// Draw this expression.
    pub fn roll(&self, rng: &mut Rng) -> i32 {
        let base: i64 = match self.dice {
            Dice::Constant(value) => i64::from(value),
            Dice::Percent => i64::from(rng.d100()),
            Dice::NdM { count, sides } => {
                let mut total = 0i64;
                for _ in 0..count {
                    total += i64::from(rng.die(sides));
                }
                total
            }
        };
        let mut value = match self.div {
            Some(divisor) if divisor != 0 => {
                let divisor = i64::from(divisor);
                let quotient = base / divisor;
                let remainder = base % divisor;
                match self.round {
                    // `div_euclid`/`rem_euclid` keep the direction honest for a
                    // negative base, which a penalty expression can produce.
                    Rounding::Down if remainder != 0 && (base < 0) != (divisor < 0) => quotient - 1,
                    Rounding::Up if remainder != 0 && (base < 0) == (divisor < 0) => quotient + 1,
                    _ => quotient,
                }
            }
            _ => base,
        };
        if let Some(min) = self.min {
            if value < i64::from(min) {
                value = i64::from(min);
            }
        }
        value as i32
    }

    /// Read an expression from content (`02:02.2`).
    ///
    /// The three accepted forms are a whole number, a string (`"d%"`, `"1d10"`,
    /// `"3-30"`), and an object that adds `div`/`round`/`min` to either.
    pub fn parse(value: &Json) -> Result<Amount, &'static str> {
        match value {
            Json::Num(number) => Ok(Amount::constant(*number as i32)),
            Json::Str(text) => Ok(Amount {
                dice: parse_dice(text)?,
                div: None,
                round: Rounding::Down,
                min: None,
            }),
            Json::Obj(_) => {
                let dice = match value.get("dice").or_else(|| value.get("const")) {
                    Some(Json::Num(number)) => Dice::Constant(*number as i32),
                    Some(Json::Str(text)) => parse_dice(text)?,
                    // A bare object with only a divisor is a percentile draw,
                    // which is what `d%/N` means when the numerator is implicit.
                    None if value.get("div").is_some() => Dice::Percent,
                    _ => return Err("a dice expression needs a dice or const field"),
                };
                let div = value.get("div").map(|field| {
                    field
                        .as_i64()
                        .ok_or("div must be a whole number")
                        .map(|n| n as i32)
                });
                let div = match div {
                    Some(Ok(number)) => Some(number),
                    Some(Err(message)) => return Err(message),
                    None => None,
                };
                let round = match value.get("round").and_then(Json::as_str) {
                    None | Some("down") => Rounding::Down,
                    Some("up") => Rounding::Up,
                    Some(_) => return Err("round must be \"up\" or \"down\""),
                };
                let min = value.get("min").and_then(Json::as_i64).map(|n| n as i32);
                Ok(Amount {
                    dice,
                    div,
                    round,
                    min,
                })
            }
            _ => Err("a dice expression must be a number, a string or an object"),
        }
    }
}

/// Parse the string forms: `d%`, `1d10`, `3-30`, `5`.
fn parse_dice(text: &str) -> Result<Dice, &'static str> {
    if text == "d%" || text == "d100" {
        return Ok(Dice::Percent);
    }
    if let Some((count, sides)) = text.split_once('d') {
        let count: u32 = count.parse().map_err(|_| "bad dice count")?;
        let sides: u32 = sides.parse().map_err(|_| "bad dice sides")?;
        if count == 0 || sides == 0 {
            return Err("dice count and sides must be positive");
        }
        return Ok(Dice::NdM { count, sides });
    }
    if let Some((low, high)) = text.split_once('-') {
        // `3-30` is the source's explicit spelling of `3d10` (02:02.2).
        let low: u32 = low.parse().map_err(|_| "bad range")?;
        let high: u32 = high.parse().map_err(|_| "bad range")?;
        if low == 0 || high == 0 || high < low || !high.is_multiple_of(low) {
            return Err("a range must be N dice of an equal number of sides");
        }
        return Ok(Dice::NdM {
            count: low,
            sides: high / low,
        });
    }
    let constant: i32 = text
        .parse()
        .map_err(|_| "a dice string must be d%, NdM, Na-M or a number")?;
    Ok(Dice::Constant(constant))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_range_spelling_is_exactly_the_dice_spelling() {
        let mut a = Rng::new(4);
        let mut b = Rng::new(4);
        let range = Amount::parse(&Json::string("3-30")).expect("parse");
        let dice = Amount::parse(&Json::string("3d10")).expect("parse");
        assert_eq!(range.dice, dice.dice);
        for _ in 0..100 {
            assert_eq!(range.roll(&mut a), dice.roll(&mut b));
        }
    }

    #[test]
    fn a_bare_number_is_a_constant() {
        let mut rng = Rng::new(1);
        let amount = Amount::parse(&Json::Num(5)).expect("parse");
        assert_eq!(amount.dice, Dice::Constant(5));
        assert_eq!(amount.roll(&mut rng), 5);
    }

    #[test]
    fn the_composite_form_rounds_and_floors() {
        // `1d10/2 (round down, minimum of 1)` (4c:1425).
        let amount = Amount::parse(&Json::from_text(
            r#"{"dice":"1d10","div":2,"round":"down","min":1}"#,
        ))
        .expect("parse");
        let mut rng = Rng::new(5);
        for _ in 0..1_000 {
            let value = amount.roll(&mut rng);
            assert!((1..=5).contains(&value), "1d10/2 min 1 returned {value}");
        }
    }

    #[test]
    fn repute_is_a_percentile_divided_by_three_rounded_up_minimum_one() {
        let amount = Amount::repute();
        let mut rng = Rng::new(6);
        for _ in 0..1_000 {
            let value = amount.roll(&mut rng);
            assert!((1..=33).contains(&value), "Repute returned {value}");
        }
    }

    #[test]
    fn a_named_dice_object_needs_a_dice_field() {
        assert!(Amount::parse(&Json::from_text(r#"{"min":1}"#)).is_err());
        assert!(Amount::parse(&Json::from_text(r#"{"dice":"nope"}"#)).is_err());
        assert!(Amount::parse(&Json::from_text(r#"{"dice":"1d0"}"#)).is_err());
        assert!(Amount::parse(&Json::from_text(r#"{"dice":"5-7"}"#)).is_err());
    }
}
