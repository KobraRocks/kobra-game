//! The sample game: the engine's end-to-end case, on its own (`AD-38`).
//!
//! Every engine crate can be unit-tested, and the first game tests the engine
//! through *its* data — but neither answers "can the engine run a game that is not
//! the first game?" This does. The fixture in `tests/fixtures/` is a complete,
//! self-contained game: the core rules, one item, one procedurally generated
//! world, and a four-command stream whose state hashes are pinned.
//!
//! It is deliberately small, and deliberately *not* the first game's content. It
//! is the engine's own check that the public surface — content load, validation,
//! a campaign, the RNG, the command stream, the save envelope — works end to end
//! without borrowing a game.

use std::path::PathBuf;

use kobra_core::json::Json;
use kobra_core::save;
use kobra_core::sim;

/// The fixture directory: two files, one content document and one command stream.
fn fixture(name: &str) -> Json {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name);
    let text =
        std::fs::read_to_string(&path).unwrap_or_else(|_| panic!("{} must exist", path.display()));
    Json::parse(&text).unwrap_or_else(|_| panic!("{} must be valid JSON", path.display()))
}

/// The commands, and the hashes the fixture pins for them.
fn commands() -> (i64, Vec<Json>, Vec<String>) {
    let wire = fixture("sample-game.commands.json");
    let seed = wire.get("seed").and_then(Json::as_i64).expect("seed");
    let commands = wire
        .get("commands")
        .and_then(Json::as_arr)
        .expect("commands")
        .to_vec();
    let hashes = wire
        .get("hashes")
        .and_then(Json::as_arr)
        .expect("hashes")
        .iter()
        .filter_map(Json::as_str)
        .map(str::to_string)
        .collect();
    (seed, commands, hashes)
}

/// Boot the sample game and return its handle.
fn boot(seed: i64) -> u32 {
    sim::init(seed as u32).expect("init");
    let report = sim::load_content(&fixture("sample-game.content.json"));
    assert_eq!(
        report.get("ok").and_then(Json::as_bool),
        Some(true),
        "the sample game's content must validate: {report}"
    );
    sim::new_campaign(&Json::object()).expect("campaign")
}

/// Run the stream, returning the state hash after every command.
fn run(handle: u32, commands: &[Json]) -> Vec<String> {
    let mut hashes = Vec::new();
    for command in commands {
        if let Err(status) = sim::with_sim(handle, |sim| sim.command(command)).expect("sim") {
            panic!("the sample game's command is legal: {status:?} for {command}");
        }
        let hash = sim::with_sim(handle, |sim| sim.hash_state()).expect("sim");
        hashes.push(format!("{hash:016x}"));
    }
    hashes
}

/// The whole sample game, twice: identical, and pinned.
#[test]
fn the_sample_game_runs_and_hashes_identically() {
    let (seed, commands, expected) = commands();

    let first = run(boot(seed), &commands);
    let second = run(boot(seed), &commands);
    assert_eq!(
        first, second,
        "the same stream from the same seed is the same trajectory"
    );

    assert_eq!(
        first, expected,
        "the sample game's hashes changed. If that is deliberate, paste these into \
         tests/fixtures/sample-game.commands.json:\n{first:#?}"
    );
}

/// A save taken mid-stream reloads to the same state (`AD-11`, `AD-21`).
///
/// The save envelope is the engine's, and this is the smallest complete check of
/// it: a generated world with its delta, a character, and the clock.
#[test]
fn the_sample_game_round_trips_through_its_save() {
    let (seed, commands, _) = commands();
    let handle = boot(seed);
    run(handle, &commands);

    let (before, text) = sim::with_sim(handle, |sim| {
        (sim.hash_state(), save::envelope(&sim.to_save()).to_string())
    })
    .expect("sim");
    sim::free_sim(handle).expect("free");

    let restored = sim::load_save(&text).expect("the save loads");
    let after = sim::with_sim(restored, |sim| sim.hash_state()).expect("sim");
    sim::free_sim(restored).expect("free");

    assert_eq!(
        before, after,
        "a generated world, its delta and the clock round-trip through the save"
    );
}
