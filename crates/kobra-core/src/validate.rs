//! The one validator (`AD-15`, 03:03.9).
//!
//! The editor's publish check and the runtime's load check are the same code, so
//! the editor cannot author something the game rejects. It is reported as
//! **data** (`{severity, code, path, args}`), never as an exception, because the
//! editor localises the message and the game logs it.
//!
//! M1 checks, in the order `03:03.9` lists them:
//!
//! 1. every reference resolves **after the merge**, not before — so a mod may
//!    reference a base record and a base record may not reference a mod record;
//! 2. an outcome table exists for every action M1 resolves;
//! 3. a spawn stands on a real, passable tile of the map it names;
//! 4. an **`undeclared_override`**: a file under a mod's `assets/` that shadows a
//!    base asset but which the mod's index does not declare. This is an *error*,
//!    not a warning, because the VFS would silently never apply it (`AD-18`).

use std::collections::BTreeMap;

use crate::content::{problem, ContentRegistry, RecordEntry};
use crate::json::Json;
use crate::l2::actions::ActionKind;

/// Every semantic check over a merged registry.
pub fn check(registry: &ContentRegistry, records: &BTreeMap<String, RecordEntry>) -> Vec<Json> {
    let mut problems = Vec::new();
    if records.is_empty() {
        return problems;
    }
    check_structural(records, &mut problems);
    check_outcome_tables(registry, &mut problems);
    check_character_references(registry, records, &mut problems);
    check_encounters(registry, &mut problems);
    check_campaign(registry, &mut problems);
    problems
}

/// Every reference the campaign layer makes must resolve (`03:03.9` rule 2).
///
/// The M3 records reference each other more than the M1 set did — a station names
/// a character, a faction and a dialogue; a shop's stock names items; a quest names
/// a faction; a dialogue's options name journal lines — so this is where a pack
/// that would have failed silently at play time is reported at load instead.
fn check_campaign(registry: &ContentRegistry, problems: &mut Vec<Json>) {
    for world in &registry.worlds {
        if world.generator.width <= 0 || world.generator.height <= 0 {
            problems.push(problem(
                "error",
                "bad_world_size",
                &world.id,
                &[("id", Json::string(&world.id))],
            ));
        }
        if world.start.0 < 0
            || world.start.1 < 0
            || world.start.0 >= world.generator.width
            || world.start.1 >= world.generator.height
        {
            problems.push(problem(
                "error",
                "start_off_map",
                &world.id,
                &[
                    ("x", Json::Num(i64::from(world.start.0))),
                    ("y", Json::Num(i64::from(world.start.1))),
                ],
            ));
        }
        for (overlay, x, y) in &world.overlays {
            let Some(map) = registry.map(overlay) else {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &world.id,
                    &[
                        ("ref", Json::string(overlay)),
                        ("field", Json::string("overlays")),
                    ],
                ));
                continue;
            };
            // An overlay that hangs off the generated map would be silently
            // clipped by `Baseline::overlay`, so it is reported instead.
            if *x < 0
                || *y < 0
                || x + map.world.width > world.generator.width
                || y + map.world.height > world.generator.height
            {
                problems.push(problem(
                    "error",
                    "overlay_off_map",
                    &world.id,
                    &[("map", Json::string(overlay))],
                ));
            }
        }
        for station in &world.stations {
            if registry.sheet(&station.character).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &world.id,
                    &[
                        ("ref", Json::string(&station.character)),
                        ("field", Json::string("stations.character")),
                    ],
                ));
            }
            if !station.faction.is_empty() && registry.faction(&station.faction).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &world.id,
                    &[
                        ("ref", Json::string(&station.faction)),
                        ("field", Json::string("stations.faction")),
                    ],
                ));
            }
            if !station.dialogue.is_empty() && registry.dialogue(&station.dialogue).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &world.id,
                    &[
                        ("ref", Json::string(&station.dialogue)),
                        ("field", Json::string("stations.dialogue")),
                    ],
                ));
            }
            // A merchant counter is spatial (`02:02.13`), so a station that names
            // a shop no record defines is a merchant nobody can trade with.
            if !station.shop.is_empty() && registry.shop(&station.shop).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &world.id,
                    &[
                        ("ref", Json::string(&station.shop)),
                        ("field", Json::string("stations.shop")),
                    ],
                ));
            }
            if station.at.0 < 0
                || station.at.1 < 0
                || station.at.0 >= world.generator.width
                || station.at.1 >= world.generator.height
            {
                problems.push(problem(
                    "error",
                    "station_off_map",
                    &world.id,
                    &[
                        ("character", Json::string(&station.character)),
                        ("x", Json::Num(i64::from(station.at.0))),
                        ("y", Json::Num(i64::from(station.at.1))),
                    ],
                ));
            }
        }
        // Talk with nobody in reach is the campaign's own authored line
        // (`02:02.13`); naming a dialogue that does not exist is a dead end.
        if !world.self_dialogue.is_empty() && registry.dialogue(&world.self_dialogue).is_none() {
            problems.push(problem(
                "error",
                "unknown_ref",
                &world.id,
                &[
                    ("ref", Json::string(&world.self_dialogue)),
                    ("field", Json::string("self_dialogue")),
                ],
            ));
        }
    }
    for shop in &registry.shops {
        for item in &shop.stock {
            if registry.item(item).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &shop.id,
                    &[
                        ("ref", Json::string(item)),
                        ("field", Json::string("stock")),
                    ],
                ));
            }
        }
    }
    for quest in &registry.quests {
        if !quest.faction.is_empty() && registry.faction(&quest.faction).is_none() {
            problems.push(problem(
                "error",
                "unknown_ref",
                &quest.id,
                &[
                    ("ref", Json::string(&quest.faction)),
                    ("field", Json::string("faction")),
                ],
            ));
        }
        for effect in quest.on_complete.iter().chain(&quest.on_fail) {
            check_effect(registry, &quest.id, effect, problems);
        }
        for step in &quest.steps {
            for effect in &step.effects {
                check_effect(registry, &quest.id, effect, problems);
            }
        }
    }
    for dialogue in &registry.dialogues {
        if !dialogue.partner.is_empty() && registry.sheet(&dialogue.partner).is_none() {
            problems.push(problem(
                "error",
                "unknown_ref",
                &dialogue.id,
                &[
                    ("ref", Json::string(&dialogue.partner)),
                    ("field", Json::string("partner")),
                ],
            ));
        }
        for node in &dialogue.nodes {
            for option in &node.options {
                for effect in &option.effects {
                    check_effect(registry, &dialogue.id, effect, problems);
                }
                // An invited fight (`02:02.13`): an empty id is a wild fight and
                // needs no record, but a named one must exist or the invitation
                // leads nowhere.
                if let Some(crate::l4::dialogue::DialogueAction::Fight { encounter }) =
                    &option.action
                {
                    if !encounter.is_empty() && registry.encounter(encounter).is_none() {
                        problems.push(problem(
                            "error",
                            "unknown_ref",
                            &dialogue.id,
                            &[
                                ("ref", Json::string(encounter)),
                                ("field", Json::string("options.action.encounter")),
                            ],
                        ));
                    }
                }
            }
        }
    }
    // A quest nobody can start is a content bug that looks exactly like a broken
    // one to a player, so it is reported rather than left to be discovered.
    for quest in &registry.quests {
        let startable = registry
            .dialogues
            .iter()
            .flat_map(|dialogue| dialogue.nodes.iter())
            .flat_map(|node| node.options.iter())
            .flat_map(|option| option.effects.iter())
            .chain(registry.quests.iter().flat_map(|other| {
                other
                    .steps
                    .iter()
                    .flat_map(|step| step.effects.iter())
                    .chain(other.on_complete.iter())
                    .chain(other.on_fail.iter())
            }))
            .any(|effect| {
                matches!(
                    effect,
                    crate::l4::journal::CampaignEffect::Quest { quest: id, .. } if id == &quest.id
                )
            });
        let has_entry = !quest.entry.all.is_empty()
            || !quest.entry.any.is_empty()
            || quest.entry.not.is_some()
            || quest.entry.flag.is_some()
            || quest.entry.flag_set.is_some()
            || quest.entry.standing_at_least.is_some()
            || quest.entry.quest_status.is_some()
            || quest.entry.has_item.is_some()
            || quest.entry.day_at_least.is_some()
            || quest.entry.time_of_day.is_some()
            || quest.entry.step_done.is_some()
            || quest.entry.read.is_some()
            || quest.entry.knows_power.is_some()
            || quest.entry.has_skill.is_some()
            || quest.entry.lacks_item.is_some()
            || quest.entry.alive.is_some()
            || quest.entry.repute_at_least.is_some()
            || quest.entry.character_repute_at_least.is_some()
            || quest.entry.standing_between.is_some();
        if !startable && !has_entry {
            problems.push(problem(
                "warning",
                "quest_never_starts",
                &quest.id,
                &[("id", Json::string(&quest.id))],
            ));
        }
    }
}

/// An effect's references must resolve (`03:03.9`).
fn check_effect(
    registry: &ContentRegistry,
    path: &str,
    effect: &crate::l4::journal::CampaignEffect,
    problems: &mut Vec<Json>,
) {
    use crate::l4::journal::CampaignEffect;
    match effect {
        CampaignEffect::Standing { faction, .. } => {
            if registry.faction(faction).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    path,
                    &[
                        ("ref", Json::string(faction)),
                        ("field", Json::string("effects.standing")),
                    ],
                ));
            }
        }
        CampaignEffect::Give(item) | CampaignEffect::Take(item) => {
            if registry.item(item).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    path,
                    &[
                        ("ref", Json::string(item)),
                        ("field", Json::string("effects.item")),
                    ],
                ));
            }
        }
        CampaignEffect::Quest { quest, .. } => {
            let known = registry.quest(quest).is_some();
            if !known {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    path,
                    &[
                        ("ref", Json::string(quest)),
                        ("field", Json::string("effects.quest")),
                    ],
                ));
            }
        }
        _ => {}
    }
}

/// The structural records must agree with what the build compiled in.
///
/// Structural content is not moddable (`AD-14`), so a disagreement here is not
/// a tunable: it is a package that would resolve rolls differently from the
/// table compiled into the engine, and it is refused.
fn check_structural(records: &BTreeMap<String, RecordEntry>, problems: &mut Vec<Json>) {
    let ladder = crate::tables::CAMPAIGN_LADDER;
    for entry in records.values() {
        match entry.type_.as_str() {
            "ladder" => {
                let variant = entry.data.get("variant").and_then(Json::as_str);
                if variant != Some(ladder.ruleset.name()) {
                    problems.push(problem(
                        "error",
                        "ladder_mismatch",
                        &entry.path,
                        &[
                            ("declared", Json::string(variant.unwrap_or(""))),
                            ("compiled", Json::string(ladder.ruleset.name())),
                        ],
                    ));
                }
            }
            "trait_order" => {
                let declared: Vec<&str> = entry
                    .data
                    .get("order")
                    .and_then(Json::as_arr)
                    .map(|order| order.iter().filter_map(Json::as_str).collect())
                    .unwrap_or_default();
                let expected: Vec<&str> = crate::l1::traits::TRAITS
                    .iter()
                    .map(|trait_| trait_.id())
                    .collect();
                if declared != expected {
                    problems.push(problem(
                        "error",
                        "trait_order_mismatch",
                        &entry.path,
                        &[("id", Json::string(&entry.id))],
                    ));
                }
            }
            _ => {}
        }
    }
}

/// The cross-check the VFS depends on (`03:03.9` rule 7).
pub fn check_overrides(document: &Json) -> Vec<Json> {
    let mut problems = Vec::new();
    let base_assets: Vec<&str> = document
        .get("base_assets")
        .and_then(Json::as_arr)
        .map(|assets| assets.iter().filter_map(Json::as_str).collect())
        .unwrap_or_default();
    if base_assets.is_empty() {
        return problems;
    }
    let Some(Json::Arr(packs)) = document.get("packs") else {
        return problems;
    };
    for pack in packs {
        let source = pack.get("source").and_then(Json::as_str).unwrap_or("base");
        if source == "base" {
            continue;
        }
        let path = pack.get("path").and_then(Json::as_str).unwrap_or("");
        let declared: Vec<&str> = pack
            .get("overrides")
            .and_then(Json::as_arr)
            .map(|list| list.iter().filter_map(Json::as_str).collect())
            .unwrap_or_default();
        let Some(Json::Arr(assets)) = pack.get("assets") else {
            continue;
        };
        for asset in assets {
            let Some(asset) = asset.as_str() else {
                continue;
            };
            if base_assets.contains(&asset) && !declared.contains(&asset) {
                problems.push(problem(
                    "error",
                    "undeclared_override",
                    path,
                    &[
                        ("mod", Json::string(source)),
                        ("asset", Json::string(asset)),
                    ],
                ));
            }
        }
    }
    problems
}

/// Every action M1 resolves must have a table; the canonical one is a fallback,
/// and a missing table is a warning rather than a refusal.
fn check_outcome_tables(registry: &ContentRegistry, problems: &mut Vec<Json>) {
    for action in ActionKind::ALL {
        if !action.is_roll() {
            continue;
        }
        if !registry.outcomes.iter().any(|table| table.action == action) {
            problems.push(problem(
                "warning",
                "missing_outcome_table",
                "",
                &[("action", Json::string(action.id()))],
            ));
        }
    }
}

/// A character sheet's skills and items must resolve, and a base record may not
/// reach into a mod (`03:03.9` rule 2).
fn check_character_references(
    registry: &ContentRegistry,
    records: &BTreeMap<String, RecordEntry>,
    problems: &mut Vec<Json>,
) {
    for sheet in &registry.characters {
        let source = records
            .get(&sheet.id)
            .map(|entry| entry.source.as_str())
            .unwrap_or("base");
        let path = records
            .get(&sheet.id)
            .map(|entry| entry.path.as_str())
            .unwrap_or("");
        for skill in &sheet.skills {
            if registry.skill(skill).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    path,
                    &[
                        ("ref", Json::string(skill)),
                        ("field", Json::string("skills")),
                    ],
                ));
            }
        }
        if !sheet.origin.is_empty() && registry.origin(&sheet.origin).is_none() {
            problems.push(problem(
                "error",
                "unknown_ref",
                path,
                &[
                    ("ref", Json::string(&sheet.origin)),
                    ("field", Json::string("origin")),
                ],
            ));
        }
        for item in &sheet.items {
            match registry.item(item) {
                None => problems.push(problem(
                    "error",
                    "unknown_ref",
                    path,
                    &[
                        ("ref", Json::string(item)),
                        ("field", Json::string("items")),
                    ],
                )),
                Some(found) => {
                    if source == "base" {
                        // Only a record a mod *introduced* is off limits to a base
                        // record; a mod's override of a base record is still a base
                        // record for this rule.
                        if let Some(entry) =
                            records.get(&found.id).filter(|entry| !entry.base_defined)
                        {
                            problems.push(problem(
                                "error",
                                "base_references_mod",
                                path,
                                &[
                                    ("ref", Json::string(item)),
                                    ("mod", Json::string(&entry.source)),
                                ],
                            ));
                        }
                    }
                }
            }
        }
    }
}

/// Every encounter must name a real map, real characters, and stand on real
/// ground.
fn check_encounters(registry: &ContentRegistry, problems: &mut Vec<Json>) {
    for encounter in &registry.encounters {
        let Some(map) = registry.map(&encounter.map) else {
            problems.push(problem(
                "error",
                "unknown_ref",
                &encounter.id,
                &[
                    ("ref", Json::string(&encounter.map)),
                    ("field", Json::string("map")),
                ],
            ));
            continue;
        };
        for (index, (x, y)) in encounter.party_spawns.iter().enumerate() {
            check_spawn(
                map,
                *x,
                *y,
                &encounter.id,
                &format!("party_spawns/{index}"),
                problems,
            );
        }
        for spawn in &encounter.enemies {
            if registry.sheet(&spawn.character).is_none() {
                problems.push(problem(
                    "error",
                    "unknown_ref",
                    &encounter.id,
                    &[
                        ("ref", Json::string(&spawn.character)),
                        ("field", Json::string("enemies")),
                    ],
                ));
            }
            check_spawn(map, spawn.x, spawn.y, &encounter.id, "enemies", problems);
        }
    }
}

/// A spawn must be on the map and not inside a wall.
fn check_spawn(
    map: &crate::content::MapRecord,
    x: i32,
    y: i32,
    path: &str,
    field: &str,
    problems: &mut Vec<Json>,
) {
    match map.world.tile(x, y) {
        None => problems.push(problem(
            "error",
            "spawn_off_map",
            path,
            &[
                ("field", Json::string(field)),
                ("x", Json::Num(i64::from(x))),
                ("y", Json::Num(i64::from(y))),
            ],
        )),
        Some(tile) if tile.blocking > 0 => problems.push(problem(
            "error",
            "spawn_in_wall",
            path,
            &[
                ("field", Json::string(field)),
                ("x", Json::Num(i64::from(x))),
                ("y", Json::Num(i64::from(y))),
            ],
        )),
        Some(_) => {}
    }
}

/// The projection names a layout path may start with (`09:09.3`).
///
/// A layout's first path segment **is** its projection, which is why subscription
/// and dependency tracking are derived from a layout rather than declared beside
/// it.
pub const UI_PROJECTIONS: &[&str] = &[
    "status", "roster", "journal", "dialogue", "world", "shops", "actions", "signals",
];

/// The engine verbs a layout `action` may name (`02:02.13`).
pub const UI_ENGINE_VERBS: &[&str] = &["talk", "wait", "fight", "spend_fortune", "continue_quest"];

/// The navigation verbs the shell owns (`09:09.5`).
///
/// Navigation decides nothing, so it needs no engine answer; an engine verb is
/// always gated by the action projection.
pub const UI_NAV_VERBS: &[&str] = &["open", "close", "toggle", "focus"];

/// The elements a layout may create.
///
/// A closed set rather than any tag name: `script`, `style`, `iframe`, `object`,
/// `link`, `base` and `form` are not markup a data-only pack has any business
/// creating, and the launcher's CSP is a second line of defence rather than the
/// first (`09:09.5`).
pub const UI_ELEMENTS: &[&str] = &[
    "section", "div", "span", "p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "dl",
    "dt", "dd", "table", "thead", "tbody", "tr", "th", "td", "button", "strong", "em", "small",
    "header", "footer", "nav", "article", "aside",
];

/// The closed command vocabulary an entry's `commands` scope is checked against
/// (`02:02.10`, `09:09.6`).
///
/// It mirrors the `match name` arms of `Sim::command`; a scope naming a verb the
/// engine does not speak is a load error rather than a runtime refusal.
pub const UI_COMMAND_VERBS: &[&str] = &[
    "add_party",
    "advance",
    "advance_trait",
    "begin_encounter",
    "buy",
    "choose",
    "commit",
    "create_character",
    "end_dialogue",
    "equip",
    "generate_world",
    "give",
    "move",
    "plan",
    "recompute_quests",
    "release_quest",
    "remove_party",
    "sell",
    "set_flag",
    "spawn_vehicle",
    "stabilize",
    "take",
    "talk",
];

/// Validate the layout documents a caller supplies (`09:09.5`).
///
/// This is the check the shell cannot do at mount and a browser cannot do at all:
/// a mod's file list is not enumerable, so the cross-references that need it — is
/// this layout declared in `overrides`, does it exist under `assets/` — run where
/// the list *is* known, exactly as [`check_overrides`] takes the `assets` array.
/// The request is:
///
/// ```json
/// { "layouts": [ { "id": "character", "path": "ui/character.layout.json",
///                  "layout": { "schema": "kobra.ui-layout/1", "root": { … } } } ],
///   "overrides": ["ui/character.layout.json"],
///   "assets": ["ui/character.layout.json", "ui/character.js"] }
/// ```
///
/// The vocabulary is closed, so an unknown key is an error here and a fail-soft
/// node disable in the shell — the same verdict at the two places it can be
/// reached (`AD-15`).
pub fn check_ui(registry: &ContentRegistry, request: &Json) -> Json {
    let mut problems = Vec::new();
    let signals: Vec<&str> = registry
        .signals
        .iter()
        .map(|signal| signal.id.as_str())
        .collect();
    let declared: Option<Vec<String>> =
        request.get("overrides").and_then(Json::as_arr).map(|list| {
            list.iter()
                .filter_map(Json::as_str)
                .map(str::to_string)
                .collect()
        });
    let known_assets: Option<Vec<String>> =
        request.get("assets").and_then(Json::as_arr).map(|list| {
            list.iter()
                .filter_map(Json::as_str)
                .map(str::to_string)
                .collect()
        });
    let layouts = request
        .get("layouts")
        .and_then(Json::as_arr)
        .map(<[Json]>::to_vec)
        .unwrap_or_default();
    for layout in &layouts {
        let path = layout.get("path").and_then(Json::as_str).unwrap_or("");
        let id = layout.get("id").and_then(Json::as_str).unwrap_or("");
        if !path.is_empty() {
            // `03:03.9` rule 7: an undeclared UI file is silently never applied,
            // which is the worst failure mode a mod system can have. An empty
            // `overrides` list is a declaration that nothing was declared, while
            // an absent one means the caller has no file list to check against.
            if let Some(declared) = &declared {
                if !declared.iter().any(|entry| entry == path) {
                    problems.push(problem(
                        "error",
                        "undeclared_override",
                        path,
                        &[("mod", Json::string(id)), ("asset", Json::string(path))],
                    ));
                }
            }
            if let Some(assets) = &known_assets {
                if !assets.iter().any(|entry| entry == path) {
                    problems.push(problem(
                        "error",
                        "missing_asset",
                        path,
                        &[("asset", Json::string(path))],
                    ));
                }
            }
        }
        let Some(document) = layout.get("layout") else {
            problems.push(problem("error", "no_layout", path, &[]));
            continue;
        };
        if let Some(schema) = document.get("schema").and_then(Json::as_str) {
            if schema != "kobra.ui-layout/1" {
                problems.push(problem(
                    "error",
                    "bad_layout_schema",
                    path,
                    &[("schema", Json::string(schema))],
                ));
            }
        }
        if let Some(api) = document.get("api").and_then(Json::as_str) {
            // The shell speaks exactly one major; a pack declaring another is
            // disabled with a reason rather than half-mounted (`09:09.7`).
            if api != "kobra.ui/v1" {
                problems.push(problem(
                    "error",
                    "bad_ui_api",
                    path,
                    &[("api", Json::string(api))],
                ));
            }
        }
        // An entry's declared command scope is checked against the closed
        // vocabulary here, so a typo is a load error rather than a runtime
        // refusal (`09:09.6`).
        if let Some(Json::Arr(commands)) = layout.get("commands") {
            for command in commands {
                let Some(name) = command.as_str() else {
                    problems.push(problem(
                        "error",
                        "bad_command_scope",
                        path,
                        &[("id", Json::string(id))],
                    ));
                    continue;
                };
                if !UI_COMMAND_VERBS.contains(&name) {
                    problems.push(problem(
                        "error",
                        "unknown_command",
                        path,
                        &[("command", Json::string(name)), ("id", Json::string(id))],
                    ));
                }
            }
        }
        let root = document.get("root").unwrap_or(document);
        let mut scopes: Vec<String> = Vec::new();
        check_layout_node(
            root,
            path,
            "root".to_string(),
            &signals,
            &mut scopes,
            &mut problems,
        );
    }
    let ok = !problems
        .iter()
        .any(|item| item.get("severity").and_then(Json::as_str) == Some("error"));
    Json::obj([("ok", Json::Bool(ok)), ("items", Json::Arr(problems))])
}

/// Check one layout node and its descendants (`09:09.5`).
///
/// `scopes` is the stack of `each`-declared aliases in scope, so `{c.name_key}`
/// inside `each: "roster.characters", as: "c"` resolves while `{c.name_key}`
/// outside it does not.
fn check_layout_node(
    node: &Json,
    layout_path: &str,
    trail: String,
    signals: &[&str],
    scopes: &mut Vec<String>,
    problems: &mut Vec<Json>,
) {
    let node_path = format!("{layout_path}#{trail}");
    let Json::Obj(fields) = node else {
        problems.push(problem("error", "bad_layout_node", &node_path, &[]));
        return;
    };
    let mut element: Option<String> = None;
    let mut each: Option<String> = None;
    let mut alias: Option<String> = None;
    let mut action: Option<String> = None;
    let mut from: Option<String> = None;
    let mut target: Option<String> = None;
    let mut children: Option<&Json> = None;
    for (key, value) in fields {
        match key.as_str() {
            "element" => match value.as_str() {
                Some(name) if UI_ELEMENTS.contains(&name) => element = Some(name.to_string()),
                Some(name) => problems.push(problem(
                    "error",
                    "unknown_layout_element",
                    &node_path,
                    &[("element", Json::string(name))],
                )),
                None => problems.push(problem("error", "bad_layout_value", &node_path, &[])),
            },
            "class" | "id" | "title-key" | "text" | "text-key" | "when-signal" | "as"
            | "target" => {
                if value.as_str().is_none() {
                    problems.push(problem(
                        "error",
                        "bad_layout_value",
                        &node_path,
                        &[("key", Json::string(key))],
                    ));
                    continue;
                }
                match key.as_str() {
                    "text" | "text-key" => {
                        if !template_paths_ok(value.as_str().unwrap_or(""), scopes) {
                            problems.push(problem(
                                "error",
                                "unbound_layout_path",
                                &node_path,
                                &[("key", Json::string(key)), ("value", value.clone())],
                            ));
                        }
                    }
                    "when-signal" => {
                        let name = value.as_str().unwrap_or("");
                        if !signals.contains(&name) {
                            problems.push(problem(
                                "error",
                                "unknown_signal",
                                &node_path,
                                &[("signal", Json::string(name))],
                            ));
                        }
                    }
                    "as" => alias = value.as_str().map(str::to_string),
                    "target" => target = value.as_str().map(str::to_string),
                    _ => {}
                }
            }
            "each" | "from" => {
                let Some(path) = value.as_str() else {
                    problems.push(problem(
                        "error",
                        "bad_layout_value",
                        &node_path,
                        &[("key", Json::string(key))],
                    ));
                    continue;
                };
                if key == "from" {
                    from = Some(path.to_string());
                } else {
                    each = Some(path.to_string());
                }
                if !path_ok(path, scopes) {
                    problems.push(problem(
                        "error",
                        "unbound_layout_path",
                        &node_path,
                        &[("key", Json::string(key)), ("value", value.clone())],
                    ));
                }
            }
            "action" => match value.as_str() {
                Some(name) => action = Some(name.to_string()),
                None => problems.push(problem(
                    "error",
                    "bad_layout_value",
                    &node_path,
                    &[("key", Json::string("action"))],
                )),
            },
            "children" => match value {
                Json::Arr(_) => children = Some(value),
                _ => problems.push(problem(
                    "error",
                    "bad_layout_value",
                    &node_path,
                    &[("key", Json::string("children"))],
                )),
            },
            _ => problems.push(problem(
                "error",
                "unknown_layout_key",
                &node_path,
                &[("key", Json::string(key))],
            )),
        }
    }
    if element.is_none() {
        problems.push(problem("error", "missing_element", &node_path, &[]));
    }
    if let Some(name) = &action {
        let engine = UI_ENGINE_VERBS.contains(&name.as_str());
        let navigation = UI_NAV_VERBS.contains(&name.as_str());
        if !engine && !navigation {
            problems.push(problem(
                "error",
                "unknown_layout_verb",
                &node_path,
                &[("action", Json::string(name))],
            ));
        }
        if engine {
            let expected = format!("actions.{name}");
            if from.as_deref() != Some(expected.as_str()) {
                problems.push(problem(
                    "error",
                    "unbound_action",
                    &node_path,
                    &[("action", Json::string(name))],
                ));
            }
        }
        if navigation && target.is_none() {
            problems.push(problem(
                "error",
                "missing_target",
                &node_path,
                &[("action", Json::string(name))],
            ));
        }
    } else if from.is_some() {
        problems.push(problem("error", "orphan_from", &node_path, &[]));
    }
    if each.is_some() && alias.is_none() {
        problems.push(problem("error", "missing_alias", &node_path, &[]));
    }
    if each.is_none() && alias.is_some() {
        problems.push(problem("error", "orphan_alias", &node_path, &[]));
    }
    if let (Some(_), Some(name)) = (&each, &alias) {
        scopes.push(name.clone());
    }
    if let Some(Json::Arr(items)) = children {
        for (index, child) in items.iter().enumerate() {
            check_layout_node(
                child,
                layout_path,
                format!("{trail}/children/{index}"),
                signals,
                scopes,
                problems,
            );
        }
    }
    if each.is_some() {
        scopes.pop();
    }
}

/// Whether a dotted path's first segment names a projection or an alias in scope.
fn path_ok(path: &str, scopes: &[String]) -> bool {
    let Some(first) = path.split('.').next() else {
        return false;
    };
    !first.is_empty() && (UI_PROJECTIONS.contains(&first) || scopes.iter().any(|s| s == first))
}

/// Whether every `{path}` token in a `text`/`text-key` value resolves.
fn template_paths_ok(template: &str, scopes: &[String]) -> bool {
    let mut rest = template;
    while let Some(start) = rest.find('{') {
        let Some(end) = rest[start + 1..].find('}') else {
            return false;
        };
        let token = rest[start + 1..start + 1 + end].trim();
        if token.is_empty() || !path_ok(token, scopes) {
            return false;
        }
        rest = &rest[start + 1 + end + 1..];
    }
    // A closing brace with no opening one is a malformed template, not a literal.
    !rest.contains('}')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::content::load;

    #[test]
    fn an_undeclared_override_is_an_error() {
        // The VFS would silently never apply it, which is the worst failure mode
        // a mod system can have (03:03.9 rule 7).
        let document = Json::from_text(
            r#"{"schema":"kobra.content-load/1","base_assets":["textures/ui/frame.png","shaders/q.wgsl"],
                "packs":[
                  {"source":"noir","priority":1,"pack":"noir.skin","kind":"presentation","path":"n",
                   "assets":["textures/ui/frame.png"],
                   "overrides":[],
                   "records":[{"id":"noir.visual.frame","type":"visual","data":{}}]}]}"#,
        );
        let problems = check_overrides(&document);
        assert!(problems.iter().any(|item| {
            item.get("code").and_then(Json::as_str) == Some("undeclared_override")
        }));
        // A declared override is silent.
        let declared = Json::from_text(
            r#"{"schema":"kobra.content-load/1","base_assets":["textures/ui/frame.png"],
                "packs":[
                  {"source":"noir","priority":1,"pack":"noir.skin","kind":"presentation","path":"n",
                   "assets":["textures/ui/frame.png"],
                   "overrides":["textures/ui/frame.png"],
                   "records":[{"id":"noir.visual.frame","type":"visual","data":{}}]}]}"#,
        );
        assert!(check_overrides(&declared).is_empty());
    }

    #[test]
    fn an_encounter_that_names_a_missing_map_is_an_error() {
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.core","kind":"content","path":"b","records":[
                {"id":"wsp.encounter.x","type":"encounter","data":{"map":"wsp.sector.missing"}}]}]}"#,
        ));
        assert!(!loaded.ok);
        assert!(loaded.records.contains_key("wsp.encounter.x"));
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("unknown_ref")));
    }

    #[test]
    fn a_campaign_reference_that_does_not_resolve_is_an_error() {
        // A station naming a missing character, a shop stocking a missing item and
        // a quest moving a missing faction are all silent at play time and obvious
        // here, which is the whole point of AD-15.
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.campaign","kind":"content","path":"c","records":[
                {"id":"wsp.world.region","type":"world","data":{
                  "generate":{"width":8,"height":8}, "start":[1,1],
                  "stations":[{"character":"wsp.character.missing","at":[2,2],
                               "faction":"wsp.faction.missing",
                               "dialogue":"wsp.dialogue.missing"}]}},
                {"id":"wsp.shop.s","type":"shop","data":{"stock":["wsp.item.missing"]}},
                {"id":"wsp.quest.q","type":"quest","data":{
                  "name_key":"q","faction":"wsp.faction.missing",
                  "entry":{"flag_set":"go"},
                  "steps":[{"text_key":"t"}]}}]}]}"#,
        ));
        assert!(!loaded.ok);
        let codes: Vec<&str> = loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .filter_map(|item| item.get("code").and_then(Json::as_str))
            .collect();
        let unknown = codes.iter().filter(|code| **code == "unknown_ref").count();
        assert!(
            unknown >= 5,
            "every unresolved reference is reported, got {codes:?}"
        );
    }

    #[test]
    fn an_overlay_that_hangs_off_the_generated_map_is_refused() {
        // `Baseline::overlay` clips silently; a designer's mistake should not be
        // silent, or half a district disappears with no message.
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.campaign","kind":"content","path":"c","records":[
                {"id":"wsp.sector.small","type":"sector_map","data":{"width":4,"height":4,
                  "tiles":[{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{}]}},
                {"id":"wsp.world.region","type":"world","data":{
                  "generate":{"width":8,"height":8}, "start":[1,1],
                  "overlays":[{"map":"wsp.sector.small","origin":[6,6]}]}}]}]}"#,
        ));
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("overlay_off_map")));
    }

    #[test]
    fn a_quest_that_nothing_can_start_is_a_warning_not_a_refusal() {
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.campaign","kind":"content","path":"c","records":[
                {"id":"wsp.quest.orphan","type":"quest","data":{
                  "name_key":"q","entry":{},"steps":[{"text_key":"t"}]}}]}]}"#,
        ));
        assert!(loaded.ok, "a warning does not disable the pack");
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| { item.get("code").and_then(Json::as_str) == Some("quest_never_starts") }));
    }

    #[test]
    fn a_base_record_may_not_reference_a_mod_record() {
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.core","kind":"content","path":"b","records":[
                {"id":"wsp.character.hero","type":"character","data":{
                  "traits":{"melee":10,"coordination":10,"brawn":10,"fortitude":10,"intellect":10,"awareness":10,"willpower":10},
                  "items":["noir.item.blackjack"]}}]},
              {"source":"noir","priority":1,"pack":"noir.gear","kind":"content","path":"n","records":[
                {"id":"noir.item.blackjack","type":"item","data":{"hands":"one"}}]}]}"#,
        ));
        assert!(loaded
            .report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .any(|item| item.get("code").and_then(Json::as_str) == Some("base_references_mod")));
    }

    /// The signals a layout may gate on come from the loaded registry, and the
    /// vocabulary is closed (`09:09.5`).
    fn ui_registry() -> ContentRegistry {
        let loaded = load(&Json::from_text(
            r#"{"schema":"kobra.content-load/1","packs":[
              {"source":"base","pack":"wsp.campaign","kind":"presentation","path":"c","records":[
                {"id":"wsp.signal.rumour","type":"signal","data":{"when":{"flag_set":"heard_rumour"}}}]}]}"#,
        ));
        assert!(loaded.ok, "the fixture validates: {}", loaded.report);
        loaded.registry
    }

    #[test]
    fn a_layout_that_speaks_the_vocabulary_passes_check_ui() {
        let request = Json::from_text(
            r#"{"layouts":[{"id":"character","path":"ui/character.layout.json",
                 "commands":["advance_trait"],"layout":{
                 "schema":"kobra.ui-layout/1","api":"kobra.ui/v1",
                 "root":{"element":"section","children":[
                   {"element":"p","text-key":"journal.rumour","when-signal":"wsp.signal.rumour"},
                   {"element":"ul","each":"roster.characters","as":"c","children":[
                     {"element":"li","text":"{c.name_key} {c.damage}/{c.max_damage}"}]},
                   {"element":"button","action":"talk","from":"actions.talk"}]}}}],
               "overrides":["ui/character.layout.json"],
               "assets":["ui/character.layout.json","ui/character.js"]}"#,
        );
        let report = check_ui(&ui_registry(), &request);
        assert_eq!(
            report.get("ok").and_then(Json::as_bool),
            Some(true),
            "{report}"
        );
    }

    #[test]
    fn a_layout_that_breaks_the_vocabulary_is_reported_by_check_ui() {
        // Every key the shell disables a node for at mount is an error here, so
        // the editor and the game give the same verdict (AD-15).
        let request = Json::from_text(
            r#"{"layouts":[{"id":"bad","path":"ui/bad.layout.json",
                 "commands":["advance_trait","teleport"],
                 "layout":{
                 "root":{"element":"section","children":[
                   {"element":"p","nonsense":true},
                   {"element":"p","when-signal":"noir.signal.missing"},
                   {"element":"button","action":"teleport"},
                   {"element":"p","text":"{nope.thing}"}]}}}],
               "overrides":[],"assets":["ui/bad.layout.json"]}"#,
        );
        let report = check_ui(&ui_registry(), &request);
        assert_eq!(report.get("ok").and_then(Json::as_bool), Some(false));
        let codes: Vec<&str> = report
            .get("items")
            .and_then(Json::as_arr)
            .expect("items")
            .iter()
            .filter_map(|item| item.get("code").and_then(Json::as_str))
            .collect();
        for code in [
            "unknown_layout_key",
            "unknown_signal",
            "unknown_layout_verb",
            "unbound_layout_path",
            "undeclared_override",
            "unknown_command",
        ] {
            assert!(codes.contains(&code), "{code} is reported: {codes:?}");
        }
    }
}
