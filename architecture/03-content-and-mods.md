# 03 — Content pipeline and moddability

> **Partly superseded by AD-39 and AD-40.** The pipeline, the overlay map, the one validator
> and Tier 1 are unchanged. §03.7's tier list is rewritten: the script tier is Lua, not ES
> modules, and a native plugin tier sits between it and the engine.

This document specifies how content is authored, shipped, discovered, validated,
overridden, and extended — by us, and by modders. It implements `AD-7` (content is
data), `AD-8` (two modding tiers), `AD-9` (conventional mod index), `AD-18` (asset
the overlay), and `AD-22` (strings under the game's content tree).

Platform facts used here are verified in the [README](README.md#what-the-platform-gives-us-and-what-it-forbids);
the load-bearing ones are restated in §03.1 because a mod system that ignores them
does not work.

## 03.1 What shapes modding

Five facts, and every one of them is the engine's now rather than a server's:

1. **A mod is a directory.** The host hands the engine a game folder and the mods enabled
   in it; there is no route, no serving switch and no HTTP (`AD-42`).
2. **The engine does not enumerate a mod's tree.** It is given a mod's *index* and reads
   what that index declares, so discovery is by a **conventional index inside the mod's own
   tree** (`AD-9`) — never by listing files.
3. **Assets are shadowed whole; records merge by id.** Two regimes, kept strictly apart
   (§03.2), because one is replacement and the other is a merge.
4. **Nothing in the base game is off-limits to an override.** An asset that cannot be
   replaced is a support burden, not a protection.
5. **A mod cannot shadow the engine or the entry point.** Code is not content, and a mod
   that could replace the engine would not be a mod.

From (1) and (2): the engine can learn *which* mods are enabled and *what they declare*,
and nothing else. Everything a mod provides is therefore declared, which is what makes an
override auditable (§03.9).

## 03.2 Two resolution regimes, deliberately

Mixing these up is the single most common way a mod system becomes useless, so the
engine keeps them strictly separate.

| | Assets (bytes) | Content records (data) |
|---|---|---|
| Examples | `textures/*.png`, `audio/*.ogg`, `shaders/*.wgsl`, `models/*.glb` | items, powers, origins, sectors, NPCs, dialogue, quests |
| Override semantics | **File shadowing.** The mod's file at the same relative path wins outright. | **Record merge by id**, with an explicit `override` declaration. |
| Why | A texture is a whole artifact; replacing it wholesale is what the author means. | A mod that adds one item must not have to reproduce the base file it lives in. |
| Owner | `vfs` module (§03.4) | `ContentRegistry` (§03.5) |

If content were file-shadowed like assets, "add one pistol" would mean "take over
`data/items.json` forever and break on every game update". Explicit record merge is
what makes small mods survive patches.

## 03.3 The content tree

Everything a game loads is either content or code, and the two do not mix: the engine is a
crate a host links (`AD-38`), and content is files.

```text
a game's content
├── content/data/
│   ├── packs.json              # the base pack load list (ordered)
│   └── packs/
│       ├── core-rules.pack.json     # ladders, traits, rank bands, dice vocabulary, distance (AD-33)
│       ├── core-powers.pack.json    # the canonical powers as DSL + parameters
│       ├── core-gear.pack.json      # weapons, armour, grenades, gear, potions
│       ├── core-vehicles.pack.json
│       ├── campaign-*.pack.json     # the game's own: origins, world, NPCs, quests, dialogue
│       └── …
├── content/locales/*.json      # game strings (moddable — AD-22)
├── content/shaders/*.wgsl      # WGSL: the one shader language (AD-38)
├── content/textures/*
├── content/models/*.glb        # meshes, rigs, LODs (the art contract: 08)
├── content/anim/*.glb          # shared clip libraries, retargeted to the humanoid rig
└── content/audio/*
```

A mod ships the same shape under its own tree, plus the index that declares what it
provides (`AD-9`).

**The folder around the content is the host's business.** Where a game's folder sits, how a
game is selected, where saves and mods live, and what the entry point is called are the
*game folder contract* the native host owes (`AD-38`, `AD-42`). It is not written yet, and
nothing here assumes it.

## 03.4 The overlay

The overlay is the whole of `AD-18`: **one map from a content path to the mod that provides
it**, built once before the first file is read, and consulted instead of probing.

```text
build the map once, before anything is read:

  base  = the game's declared packs and assets
  mods  = the mods the host reports as enabled, with their indices,
          sorted by priority descending, ties by id
  map   = { declared path -> mod id }   one entry per override and per declared asset

resolve(path):
  return map[path] ?? base
```

Rules, each of which exists because of a failure mode it prevents:

- **Resolution is a lookup, never a probe.** Probing costs `N_mods × M_assets` file reads
  for the same content, and it gets worse with every mod the player installs — precisely
  backwards. The map does not depend on boot time being a problem (`AD-25`); it is the
  shape we would have to build anyway.
- **The declaration is auditable.** Because resolution is map-based, a mod's index must
  declare what it provides, and the validator cross-checks the index against the mod's
  actual tree and reports `undeclared_override` (§03.9) — a caught mistake rather than a
  silent non-override.
- **Enabled means enabled.** The host decides which mods are enabled, including a mod it
  disabled for a missing dependency; the engine never second-guesses it.
- **Nothing reads a file behind the overlay's back.** A direct path read is a bug.

**The load document.** The host assembles the map and the pack list and hands the engine
**one** document, `kobra.content-load/1`: each pack carries `source` (the base game or a mod
id), `priority`, `pack`, `kind`, `path`, `engine_min`, its `records`, and — for a mod — the
index's declared `overrides` and, where the caller knows the file list, the pack's `assets`.
The engine merges, applies the structural rules, validates, and returns a
`kobra.load-report/1` (`{ok, packs, skipped, record_count, items, rules_hash,
presentation_hash}`, each item a `{severity, code, path, args}`).

**The merge order and the semantics are the engine's**, not the loader's (`01:01.2`). The
host reads files; it decides nothing. Two consequences worth stating, both learned by
building it:

- The document is JSON rather than a packed struct, because the engine already parses JSON
  for commands, previews and saves, and a second encoder would be a second format to keep
  in step. The seam is "one document in, one registry and one report out", and a binary
  encoding is a later change behind it.
- The base asset list is **optional at load**. It is the input to the validator's
  `undeclared_override` cross-check, so a game that ships none gets a warning rather than a
  failure.

## 03.5 Content packs and the record merge

A **content pack** is one JSON file holding a list of typed records. Packs are the
unit of authoring, review, and mod distribution.

```json
{
  "schema": "kobra.content-pack/1",
  "pack": "wsp.core.gear",
  "kind": "content",                    // "content" | "presentation" (AD-29)
  "engine_min": "0.1.0",
  "strings": "locales/gear",            // optional: a string table this pack owns
  "records": [
    { "id": "wsp.item.pistol",  "type": "item",   "data": { "…": "…" } },
    { "id": "wsp.item.rifle",   "type": "item",   "data": { "…": "…" } },
    { "id": "wsp.origin.robot", "type": "origin", "data": { "…": "…" } }
  ]
}
```

The game's `content/data/packs.json` lists its packs in load order. Each
enabled mod's index lists its packs. The registry then applies, in order: base
packs → mods ascending by `priority` (ties by id ascending, so the highest priority
is applied last and wins).

Merge rules, checked by the validator and reported per mod:

| Case | Result |
|---|---|
| New id | Added. |
| Id already present, record has `"override": true` | Replaces the earlier record. Recorded in the load report with both sources. |
| Id already present, no `override` | **Error for that mod.** The mod is disabled with `duplicate_id` naming both sources — it never silently clobbers. |
| Record has `"remove": true`, `override: true` | Removes the record. Only permitted for non-structural types; structural removals are a load error. |
| Id not namespaced under the owning pack | Error (`bad_namespace`). Every id is prefixed with its pack id, so two mods cannot collide by accident. |

**Structural types** (`ladder`, `trait_order`, `dice_vocab`, `effect_op`) and the rules
pack's **granularity fields** (`tile_mm`, `legacy_sector_tiles`, `02:02.6`) may not be
overridden or removed by a mod at all. That is `AD-14`: the resolution table is not
moddable, because a save's meaning depends on it, and under `AD-33` the tile size is the
same kind of thing — changing it would invalidate every saved world and every other mod's
coordinates. Everything else — including every power, item, origin, sector, quest and
dialogue node — is moddable.

The **distance parameters** split along that same line (`02:02.6`). A mod may retune
movement, weapon ranges and the per-distance steps — 4C adapted to a vision is exactly what
the content model exists for — but not the tile size or the legacy-sector conversion.

**The record types, as shipped at M3.** The table is the whole vocabulary; a pack that
names anything else is refused with `bad_record_type` rather than ignored, because a
record nothing reads is a record whose author believes something false.

| Type | What it is | Read by |
|---|---|---|
| `ladder`, `trait_order`, `dice_vocab`, `effect_op` | **Structural**: not moddable (`AD-14`). | `02:02.2` |
| `rules` | The rules pack's own blocks: granularity, distances, elevation, cover, creation, generation and skill tables (`02:02.6`). | `l0`, `l1`, `l2`, `l4` |
| `item` | Equipment: hands, melee kind, reach, damage, armour, modifiers, tags, and now `slot`, `price` and `kind` (`AD-29` rule 3). | `l1::items`, `l4::economy` |
| `skill` | A named row-step bonus and the action tags it applies to (`4c:275`). | `l1::skills` |
| `power` | An effect-DSL template (`AD-16`). | `dsl` |
| `origin` | The six canonical modifiers of `4c:124-131`. | `l1::character` |
| `character` | An authored sheet: NPCs, adversaries, the editor's template. | `l1::character` |
| `vehicle` | The three Vehicle Traits and their records (`4c:1300-1365`). | `l2::vehicle` |
| `outcome_table` | The four-colour effects for one action (`02:02.5`). | `l2::actions` |
| `sector_map` | A tile map: dimensions, and per-tile level, blocking height, terrain and material (`02:02.12`). | `l3::world` |
| `encounter` | A map, party spawns, enemies, and now `on_win`/`on_loss`/`epilogue` bindings. | `l2::encounter` |
| `world` | A **generated region**: the generator's parameters, the authored maps overlaid on it, the arrival point, the start hour, and the stations (`02:02.6`). | `l3::gen`, `sim` |
| `faction` | Standing bands, a default standing, and `reaction_polarity` (`02:02.6`, `D4`). | `l4::factions` |
| `dialogue` | A node graph: nodes, options, `when` conditions, `check`s and effects (`02:02.7`). | `l4::dialogue` |
| `quest` | Steps with entry and completion conditions and effects, plus completion bindings. | `l4::quests` |
| `shop` | A merchant's stock, the item kinds it buys, and its margin (`02:02.7`). | `l4::economy` |
| `signal` | A **named condition** the engine evaluates for UI gating (`09:09.4`). Presentation: readable by a layout, never an input to resolution. | the engine, the shell |
| `visual`, `string_table` | Presentation: merged and hashed, never interpreted (`AD-29`). | `vfs`, the shell |

A `world` record and a `sector_map` are not the same thing, and the difference is
`02:02.6`'s decision made concrete: a `world` **generates** terrain from a seed and
treats an authored map as an override at an origin, while a `sector_map` that no world
record names is **authored outright** and is its own baseline. Both go through one delta
path, so a save's world half is the same shape either way.

**Two identities, not one (`AD-29`).** After the merge the registry computes:

- **`rules_hash`** — the structural tables plus every rules-bearing record. This is what
  a save binds (`AD-11`), and a mismatch is a hard, explained refusal, because the save's
  meaning depends on it.
- **`presentation_hash`** — visual descriptors, string tables and their assets. A change
  here is recorded in the load report and the save header and **never** invalidates a
  save or blocks a load.

That split is what makes a cosmetic pack safe to try: a 3D character pack, a reskin, or a
translation can be enabled, compared and disabled without any save ever being reported as
mismatched. It also lets a pack be applied **partially** — the party but not the
townsfolk — because the descriptor is per record.

**Pack kinds.** A pack declares `kind: "content"` or `kind: "presentation"`. A
presentation pack may define visual descriptors, strings and assets; it **may not** define
a rules record, and the validator rejects one that tries (§03.9). One pack cannot be both,
so "does enabling this change my game or only my game's looks?" is answerable from the
manifest alone — which is exactly the question the mod manager has to answer for the
player.

## 03.6 Mod anatomy

A mod is a directory. Its **tree is its own** — there is no subtree the host must expose and
none it may hide — and one file in it is required:

```text
<mod>/
├── mod.manifest.json       # REQUIRED: identity, engine range, the asset list
├── data/mod.json           # REQUIRED by the loader: the conventional index (AD-9)
├── data/packs/*.pack.json  # this mod's content packs
├── locales/<lang>.json     # this mod's strings
├── textures/…  audio/…  shaders/…  models/…   # asset overrides and additions
├── ui/*.layout.json  ui/*.lua                 # layout and renderer (09)
└── scripts/*.lua           # Tier-2 hooks (AD-39)
```

The **index** is the contract that matters: it declares the mod's packs and every file it
overrides, and the loader builds the overlay map from it (§03.4). An undeclared file is never
applied, which is why the validator treats it as an error (§03.9 rule 7).

`mod.manifest.json` carries identity and dependency information — `id`, `name`, `version`,
`priority`, `requires`, `conflicts`, `base_release`, `requires_engine`, `expected_base_hash`
and the asset list. It is the **loader's** document: the engine is handed a built load
document and never reads a mod's tree or its manifest itself.

Where a mod is installed, how it is removed, and what the shipping archive looks like are the
host's business, part of the game folder contract (`AD-42`). What the engine requires is the
index below.

## 03.7 The modding tiers

Four tiers, normatively in `AD-40`, with the script tier in `AD-39`:

| Tier | Ships | Trust | Determinism |
|---|---|---|---|
| **1 — Data** | packs, locales, assets | data, always | its hash is recorded in the save |
| **2 — Lua** | Tier 1 + `scripts/*.lua` | sandboxed and scoped | the script set and its hashes are recorded in the save |
| **3 — Native** | a shared library against the engine's versioned C ABI | **trusted code, in-process** | the plugin set and its hashes are recorded in the save |
| **4 — Engine** | a forked engine | us | — |

A Tier-2 script answers the engine's hooks, and `AD-8`'s contract survives: pure functions of
integer state plus the seeded RNG, no wall clock, no floats on state paths, and a script that
throws is disabled rather than fatal. A Tier-3 plugin *registers primitives* — effect kernels,
hooks, functions for the script sandbox — and never reaches into simulation memory; it is
loaded from an allowlist and its hashes go into the save.

Both tiers are recorded in the save, so a save taken with one loads without it and says what
is missing. The retired runtime's JavaScript tiers, its hook import and its worker dispatcher
are `AD-38`/`AD-39` history.

## 03.8 Load sequence

```text
1. the host reads the game's content/data/packs.json and the enabled mods' indices
2. it builds the load list: base packs + mod packs, ordered by priority
3. kobra_validate_content(load list)      → kobra.load-report/1
       a pack with errors is dropped, and its mod is disabled (fail-soft)
4. kobra_new_campaign(config) | kobra_load(save)
5. register the script tier (Lua, AD-39); record the set's hashes in the save
6. the host's loop begins: input → commands → ticks → frames
```

Every step is reported to a **content load report** the host surfaces: mod id, version, what
loaded, what was skipped, and why. That report is the support artifact, and it is the only
place a load problem is visible.

Dependency handling belongs to the loader: a mod whose `requires` are unsatisfied is never
offered to the engine, so `enabled` is authoritative and the engine never second-guesses it.
`conflicts` are informational, and become a warning in the report rather than a refusal.

## 03.9 Validation

`AD-15`: one validator, in the engine, called by the game at load and by the editor before
it saves or publishes. It is reported as data, never as an exception:

```json
{ "ok": false,
  "items": [
    { "severity": "error", "code": "unknown_ref",
      "path": "data/packs/noir-gear.pack.json#/records/4/data/grants_power",
      "args": { "ref": "wsp.power.fireball", "did_you_mean": ["wsp.power.fire-bolt"] } },
    { "severity": "warning", "code": "balance_outlier",
      "path": "…#/records/9/data/damage", "args": { "value": 90, "p99": 25 } }
  ]}
```

Rules the validator enforces beyond shape:

1. Every `type` is known, every id is namespaced, every structural field is present.
2. Every reference resolves **after the merge**, not before — so a mod may
   reference a base record and a base record may not reference a mod record.
3. An `override` replaces a record of the same `type`; changing a record's type is
   an error.
4. Every DSL trigger/condition/effect is known, its operand types are checked, and
   every dice term is legal.
5. Every string id referenced exists in the string tables; missing strings are
   warnings, because partial translation is allowed.
6. A **`kind` violation**: a pack declared `presentation` that defines a rules-bearing
   record type, or a pack declared `content` that defines only visuals. The rule exists so
   that a cosmetic pack can never silently change balance or invalidate a save (`AD-29`).
7. **An `undeclared_override`**: a file in the mod's tree whose path shadows a base asset
   but which the mod's index does not declare. This is an *error*, not a warning, because the
   overlay would silently never apply it (`AD-18`) — a mod that appears to do nothing is the
   worst failure mode a mod system can have.
8. Balance outliers are warnings derived from the content's own distribution
   (a damage value beyond the 99th percentile of its class), so the check stays
   useful as balance changes. Never a hard error.
9. A pack that declares `engine_min` above the running engine is skipped whole,
   with a message, rather than half-loaded.
10. A **structural record that disagrees with the build**: the `ladder` record's
    variant must equal the ladder compiled into the engine, and `trait_order` must
    equal the engine's trait order. Structural content is not moddable (`AD-14`),
    so a disagreement is a package that would resolve rolls differently from the
    table in the binary, and it is an error rather than a tunable.
11. **A UI declaration that cannot work** (`09:09.5`): a layout key outside the vocabulary,
    a path into a projection the entry does not subscribe to, a `text-key` that does not
    exist, a `when-signal` no `signal` record declares, an `action` outside `02:02.13`'s
    vocabulary, a `commands` scope outside the command vocabulary, a renderer or layout file
    not declared in `overrides`, or a `ui.api` major the host does not speak. At publish
    these are errors; at mount they are fail-soft per node and per entry. The engine's pass
    is `kobra_check_ui`, which takes the layout documents and the mod's declared overrides
    and assets, and returns the same `{severity, code, path, args}` items the rest of the
    validator does.

**Where the file list exists.** Rule 7 needs a mod's actual tree, and the engine cannot
enumerate one — it is handed a document. So the check runs where the list is known: at
publish, and in a loader that has walked the tree. It is silent at runtime rather than
wrong; the validator's `check_overrides` reads the per-pack `assets` array when
the caller supplies it, and `check_ui` reads the same list for a pack's layout,
renderer and theme files. At runtime the map is the enforcement: an undeclared
file is simply never resolved. That is exactly why the publish-time verdict is an
*error* and not a warning.

## 03.10 What a mod can never do

Stated as guarantees, each with its mechanism — this list is what makes it reasonable to
say "install mods, they are trusted":

| Cannot | Mechanism |
|---|---|
| Fetch or transmit anything | The runtime has no network (`AD-41`); the game is offline |
| Shadow engine code or the entry point | Engine code is not a content path; the loader resolves content only |
| Write outside its own data | The host owns the filesystem; the engine writes only through the save API it is given |
| Delete the player's data | Retention is the host's rule (`AD-11`); the engine never unlinks |
| Change the rules of resolution | Structural content types are not overridable (`AD-14`) |
| Break a save silently | `content_hash` and the mod set are recorded in the save; a mismatch is reported |
| Prevent the game from starting | Every content failure is per-mod and fail-soft |

What it *can* do, honestly: run code the engine executes, corrupt its own session, ship a bad
pack, or crash — a Tier-2 script within its sandbox, and a Tier-3 plugin as trusted in-process
code (`AD-40`). The mitigations are the capability object, a declared command scope defaulting
to read-only, per-entry isolation, per-mod fail-soft, and a mod manager that says what a pack
declares before it is enabled: recovery rather than prevention, exactly as for a simulation
script. The platform's threat note (T14) is the same statement from the other side.

## 03.11 The author loop

**Authoring is headless-first** (`AD-42`): project files, a schema-driven validate step, a Lua
lint and a packer, so the loop a modder — or an agent — runs is a command line rather than a
page. A graphical editor is a modder-facing tool built later.

**The mod manager is a host surface.** Choosing which mods are enabled, resolving a conflict,
and showing the override report are the loader's jobs; what the engine owes is the load report
it already returns (§03.8), per mod, with a reason.

Enabling a mod takes effect the next time the engine loads content, so the host either
restarts it with the new set or tells the player when it will apply.

## 03.12 Upstream requests (retired)

**Retired by `AD-42`.** This section listed improvements the engine wanted from the launcher:
a mod-manifest route, a published archive-layout schema, and a `.zip` or loose-file mod
install. There is no launcher, no server and no archive layout to publish, so there is nothing
left to request from one.
