# 02 — The 4C rules engine and simulation core

This document specifies what `game.wasm` computes and how the 4C System
(`specs/4c_system.md`) becomes a deterministic simulation. It assumes `AD-1`
(WASM is the authoritative core), `AD-6` (integer-only, one seeded RNG) and
`AD-15` (one validator).

Line references like `4c:853` point at `specs/4c_system.md`. `csv:39` points at
`specs/4c_system_master_tables.csv`.

## 02.1 Decomposition

The core is five layers, each with a narrow interface to the next. The split
matters because it is also the test split.

| Layer | Owns | Depends on | Tested by |
|---|---|---|---|
| **L0 Resolution** | Rank ladder, Master Table, row steps, colour comparison, d%/dice, Fortune shifts | nothing | table property tests, exhaustive roll enumeration |
| **L1 Entities** | Traits, secondary traits, origins, skills, powers as kernels, items, vehicles, statuses | L0 | unit tests per rule, seeded fixtures |
| **L2 Encounter** | The panel/turn machine: initiative, actions, attacks, damage, conditions, recovery | L1 | scenario tests with fixed seeds and asserted outcomes |
| **L3 World** | Sectors, movement, occupancy, schedules, AI, factions, procedural baseline + delta | L1, L2 | replay determinism tests, world invariants |
| **L4 Campaign** | Quests, dialogue, journal, faction standing, progression, shops, campaign flags | L1–L3 | content-driven integration tests |
| **Validation** | Content validation and the effect DSL compiler | L0–L4 types | shared with the editor (`AD-15`) |

L0 is pure functions over integers with no allocation. L1–L4 all mutate one
`Sim` value that is the single thing serialised into a save (`AD-11`). Nothing in
any layer allocates during a tick: pools and arenas are sized at load and
reused, because a GC pause in the middle of a panel is a determinism hazard and a
stutter.

## 02.2 L0 — the Master Table, ranks and row steps

### The rank ladder

A Rank Value is an integer `0..1000+` that is *always* reduced to a **band** (a
row) before it is used. The bands are fixed by the rules (`4c:214-227`): 13 rows
in the Basic game, 18 in the Advanced game. The campaign picks one ladder and
fixes it; the ladder is compiled into the wasm as a table, never loaded from
content (AD-14).

```rust
struct Ladder { bands: &'static [Band] }   // ascending, index 0 = rank 0
fn band_of(rv: i32) -> u8                  // binary search over band bounds
fn step(idx: u8, steps: i32) -> u8         // saturating clamp at both ends
```

Two consequences that must be implemented deliberately rather than discovered:

- **A row step is a band-index shift, not an arithmetic addition.** `+1` moves
  the index up one band; `-2` from the band `20-29` lands on `6-9` (`4c:864`).
  Because the bands are uneven in width, no arithmetic shortcut exists — this is a
  table lookup, which is also why it is fast and why it is exhaustively testable.
- **Clamping is saturating at both ends.** Repeated `-n` steps below rank 0 clamp
  at 0 and the roll still resolves on the rank-0 row. Repeated `+n` steps above
  the top band clamp at the top row. The spec explicitly allows a character to be
  *reduced to* rank 0 in special situations but "no character may have a Rank
  Value of 0" (`4c:212`), so 0 is a reachable *effective* rank and an illegal
  *stored* rank. The type system should make stored RV `NonZero` and effective RV
  a distinct newtype. That single distinction prevents a whole class of bugs.

### The table itself is generated, and the CSV cannot be used as-is

`csv` is the only machine-readable copy of the table in the repo, and it is not
usable. The audit found three separate problems, and the fixes are all mechanical
once you know them.

**Problem 1 — two of its three blocks contain no colours at all.** The CSV holds
three tables. The first two (`csv:2-15` Basic, `csv:18-36` Advanced) have
**canonical** headers but every data cell is literally the bucket label
(`00-04`, `05-09`, …) because the source document stored the colours as *cell
shading*, and the text-only export dropped the shading. Only the third block
(`csv:39-52`, "Basic Master Table (Colors)") has colours — and it is the only one
whose header is scrambled.

**Problem 2 — the header permutation is confined to ten columns.** In the Colors
block the **data** columns are in canonical `d%` order and the **header** is the
broken artifact (the source document's header is canonical). The permutation cycles
are:

| Canonical bucket | Header position | Cycle |
|---|---|---|
| `00-04`, `05-09`, `15-19` | 2, 4, 1 | 3-cycle |
| `10-14`, `20-24` | 5, 3 | 2-cycle |
| `80-84`, `85-89`, `90-93` | 19, 17, 18 | 3-cycle |

Every other column matches. So a reader who trusts the header gets a scrambled
table, and a reader who trusts the data gets the right answer — for the 264 of 286
cells that are correct at all.

**Problem 3 — the colour data itself is corrupt in 22 cells**, and **no** column
permutation repairs it (the best-fit permutation still leaves ten wrong). The
defects are adjacent-pair swaps concentrated in the top rows. The most consequential
one is a deliberate-looking "correction" that is in fact the corruption: the CSV
moved the black band rightward at high rank, whereas the source keeps a **black
`00-04` cell even at Rank Value 1000** — an irreducible 5 % failure band that is a
real and important property of how the game feels.

**The authoritative source.** The Libre Edition *Master Tables* document encodes
every colour **twice** — as cell text (`Blck`/`Red`/`Blue`/`Yel`) *and* as cell
shading (`#000000`/`#800000`/`#0000FF`/`#FFFF00`) — and the two encodings agree
cell-for-cell. That self-validating document is the ground truth, and it is archived
publicly (Internet Archive item `4cSystemSuperheroRoleplayinglibreEdition`, file
`MasterTables.docx`; the OCR `MasterTables_djvu.txt` agrees).

### The authoritative tables

These are the tables the generator must produce. `K`=Black (fail), `R`=Red (minor),
`B`=Blue (success), `Y`=Yellow (major).

Basic — 13 rows × 22 buckets, buckets
`00-04 05-09 10-14 15-19 20-24 25-29 30-34 35-39 40-44 45-49 50-54 55-59 60-64 65-69 70-74 75-79 80-84 85-89 90-93 94-96 97-98 99`:

```
1000      K R R R R R R B B B B B B B B Y Y Y Y Y Y Y
150-999   K K R R R R R R B B B B B B B B Y Y Y Y Y Y
100-149   K K K R R R R R R B B B B B B B B Y Y Y Y Y
75-99     K K K K R R R R R R B B B B B B B Y Y Y Y Y
50-74     K K K K K R R R R R R B B B B B B B Y Y Y Y
40-49     K K K K K K R R R R R R B B B B B B Y Y Y Y
30-39     K K K K K K K R R R R R R B B B B B B Y Y Y
20-29     K K K K K K K K R R R R R R B B B B B Y Y Y
10-19     K K K K K K K K K R R R R R R B B B B B Y Y
6-9       K K K K K K K K K K R R R R R R B B B B Y Y
3-5       K K K K K K K K K K K R R R R R R B B B B Y
1-2       K K K K K K K K K K K K R R R R R R B B B Y
0         K K K K K K K K K K K K K R R R R R R B B Y
```

Advanced — 18 rows × 24 buckets, buckets
`00 01-02 03-05 06-09 10-14 15-19 20-24 25-29 30-34 35-39 40-44 45-49 50-54 55-59 60-64 65-69 70-74 75-79 80-84 85-89 90-93 94-96 97-98 99`:

```
5000+     K R R R R R B B B B B B B B Y Y Y Y Y Y Y Y Y Y
2500-4999 K R R R R R R B B B B B B B B Y Y Y Y Y Y Y Y Y
1500-2499 K R R R R R R R B B B B B B B B Y Y Y Y Y Y Y Y
1000-1499 K R R R R R R R R B B B B B B B B Y Y Y Y Y Y Y
500-999   K K R R R R R R R B B B B B B B B Y Y Y Y Y Y Y
250-499   K K K R R R R R R R B B B B B B B B Y Y Y Y Y Y
150-249   K K K K R R R R R R B B B B B B B B Y Y Y Y Y Y
100-149   K K K K K R R R R R R B B B B B B B B Y Y Y Y Y
75-99     K K K K K K R R R R R R B B B B B B B Y Y Y Y Y
50-74     K K K K K K K R R R R R R B B B B B B B Y Y Y Y
40-49     K K K K K K K K R R R R R R B B B B B B Y Y Y Y
30-39     K K K K K K K K K R R R R R R B B B B B B Y Y Y
20-29     K K K K K K K K K K R R R R R R B B B B B Y Y Y
10-19     K K K K K K K K K K K R R R R R R B B B B B Y Y
6-9       K K K K K K K K K K K K R R R R R R B B B B Y Y
3-5       K K K K K K K K K K K K K R R R R R R B B B B Y
1-2       K K K K K K K K K K K K K K R R R R R R B B B Y
0         K K K K K K K K K K K K K K K R R R R R R B B Y
```

Three structural facts fall out of these and each has an implementation
consequence:

1. **The buckets tile `00..99` exactly** in both tables — Basic is
   `18×5 + 4+3+2+1 = 100`, Advanced is `(1+2+3+4) + 16×5 + (4+3+2+1) = 100`. The
   irregular `90-93 / 94-96 / 97-98 / 99` tail is intentional finer granularity at
   the top, not a hole to be "fixed".
2. **There is an irreducible failure band**: `00-04` is Black in *every* row of both
   tables. No character ever succeeds more than 95 % of the time. This is a game-feel
   property worth stating in the design docs and asserting in a test, because it is
   exactly the cell the CSV "corrected" away.
3. **The two ladders are not nested.** Basic has a row that is exactly `1000`;
   Advanced has `1000-1499`, and splits `150-999` into three rows. The same Rank
   Value therefore resolves differently under the two ladders, and Rank Values
   `1001..9999` have **no** row in Basic at all. So the ladder choice is not a
   display option: **it is part of a save's identity**, and a save must record which
   ladder it was created under (`§02.9`). A single `rv -> row` resolver cannot be
   shared between the variants; it is ladder-parameterised, and it clamps at the top
   row for out-of-range values.

**Decision (AD-14).** The table is a *generated* artifact. The two grids above are
transcribed into `specs/4c_system_master_tables.csv` — the authoritative,
machine-readable source — and the generator emits Rust tables from it. The
as-exported file is kept unmodified as `4c_system_master_tables.as-exported.csv` so
the corruption is auditable rather than merely described. The generated tables are
then covered by property tests:

1. **Per-row monotonicity**: colours are non-decreasing left-to-right in
   `Black < Red < Blue < Yellow`. Encode colour as an ordered enum so this is a
   comparison, not a manual rank function.
2. **Per-column monotonicity**: colours are non-decreasing as the Rank Value row
   rises.
3. **Exact coverage**: the buckets tile `00..99` once, with no gap, no overlap, and
   100 outcomes per row.
4. **The irreducible band**: `00-04` is Black in every row of both tables, and Rank
   Value 1000 in Basic resolves on its own row rather than being clamped into
   `150-999`.
5. **Ladder divergence**: a test that pins the specific cells where Basic and
   Advanced disagree for the same Rank Value, so the difference is intentional and
   visible rather than discovered by a player.
6. **Row-band coverage**: the ladder definitions are complete over their declared
   domain, and out-of-domain Rank Values clamp explicitly (Basic: everything above
   `1000` clamps to the `1000` row).
7. A **golden snapshot** of both tables, reviewed by a human once, so any future edit
   is visible in a diff.

**Know what those tests are worth.** Tests 1 and 2 are a *regression guard*, not a
derivation. Measured against the corrupt export, they flag only **8 of its 22** wrong
cells; the other 14 are masked, because a pair-swap inside an otherwise monotone run
stays monotone. What establishes correctness is the **dual-encoded source
transcription** (every cell carries its colour both as text and as shading, and the
two agree) plus the reviewed golden snapshot. The runnable form of tests 1–6 already
exists at `specs/verify_master_tables.py`.

Note that the repo currently has **no machine-readable copy of the Advanced colour
table at all** — it exists only in the source document. Transcribing it is part of
task R1.

### The resolution primitive

Every rules question reduces to one function. It takes the effective rank, the
roll, and the *declared intent* (used by Nail and by "pulling your punch"), and
returns a colour plus the authored outcome row for the current action.

```rust
struct Outcome { colour: Colour }           // Black < Red < Blue < Yellow
fn resolve(ladder: &Ladder, eff_rv: i32, d100: u8) -> Colour
fn shift(c: Colour, steps: i32) -> Colour   // used by Fortune spending (4c:874)
fn meets(c: Colour, required: Colour) -> bool
```

`d100` is produced from the RNG and is `0..=99` — **`00` is zero, not one
hundred** (`4c:41`). This is stated once in the spec and is the single easiest
rule in the system to get wrong; it gets a dedicated test.

The same primitive serves both use cases the spec describes: an *action* roll
(GM sets the required colour, `4c:1199`) and a *table-driven* roll (the colour
indexes an authored outcome row, `4c:853-860`). Keeping one primitive with one
colour ordering means Fortune can shift any roll in the game
(`4c:874`) without every subsystem knowing about Fortune.

**The colour is context-typed, and that is deliberate.** The same four colours carry
three different meanings depending on who is reading them:

| Context | Black | Red | Blue | Yellow |
|---|---|---|---|---|
| Action outcome (`4c:855-860`) | failed | minor success | success | major success |
| Difficulty (`4c:1199`) | easy | average | difficult | ridiculous |
| Public reaction (`4c:1278-1283`) | unfavourable | favourable | very favourable | extremely favourable, **reversed for criminals** (`4c:1292`) |

So the primitive returns a `Colour`, and *interpretation* is a property of the
consumer and the roll's declared `ColourSemantics` (`Outcome` | `Difficulty` |
`Reaction`). This is why `resolve` returns a colour rather than a boolean: a boolean
would force every caller to re-derive the ordering, and the public-reaction reversal
would become a special case scattered across the dialogue layer instead of one
`polarity` flag on the roll. A test asserts that `meets(rolled, required)` works for
`Outcome` and `Difficulty` and is inverted for `Reaction` with
`reaction_polarity: criminal`.

### Fortune is a resource that mutates rolls

Fortune shifts a resolved roll by one colour per 25 points, cumulatively,
possibly contributed by several characters, possibly against an enemy's roll
(`4c:874`). This is modelled as a **post-roll window**, not a pre-roll modifier:

```
resolve → [Fortune window: any participant may commit 25-point increments] → final colour
```

The ordering rule is explicit: shifts are applied in descending commit order
(higher spend first), ties broken by actor id ascending, so the final colour is
independent of arrival order. Because shifts are colour-index arithmetic, the
result is order-independent for the *total* anyway, but the intermediate
`Outcome` events shown to the player are not — hence the deterministic ordering
rule. Nine Lives (`4c:664-666`) is a Fortune *pool* with a session scope and one
special declaration right; it is modelled as a per-panel-scoped resource with a
flag, not as a new mechanism.

### Dice algebra

The rules use four shapes, and all four are implemented as exact integer
distribution draws from the seeded RNG, with **explicit rounding rules recorded in
the content**:

| Shape | Where | Rounding |
|---|---|---|
| `d%` | every resolution | n/a, `00 = 0` |
| `NdM` (`1d10`, `2d10`, `3d10`) | durations, potions, recharges | n/a |
| `Na-M` (`1-10`, `3-30`, `2-20`, `5`) | durations and turn counts | explicit spellings of `1d10`, `3d10`, `2d10`, a constant |
| `d%/N` rounded up | Repute (`4c:208`) | round up, minimum 1 |

`1d10/2 (round down, minimum of 1)` (`4c:1425`) is the only genuinely composite
case; it is expressed in content as an `int` expression, not as ad-hoc Rust, so a
modded item can use the same vocabulary.

## 02.3 L1 — characters

### Traits

Seven Primary Traits in a fixed order, because two derived values are defined as
"the first four" and "the last three" (`4c:190,196`):

```
[0] Melee [1] Coordination [2] Brawn [3] Fortitude | [4] Intellect [5] Awareness [6] Willpower
Damage    = sum(rv[0..4])       // a numerical score, decreases with damage
Fortune   = sum(rv[4..7])       // a numerical score, spent
Lifestyle = band-scored via the RV table   (4c:202)
Repute    = ceil(d% / 3)                   (4c:208)
```

Damage being "the first four" is not an implementation detail to be reordered:
the trait array order is part of the rules, so the ordering is asserted by test.

An **effective trait** is distinct from a **stored trait**: powers modify the
effective value used for a roll (`Trait Increase`, `Growth`, `Combat Awareness`,
`Contaminant Resistance`, `Supersense`, `Telepathy`, `Telekinesis`, `Animal
Command`, `Mind Shield`). The resolution path therefore asks for
`effective_trait(who, trait, context) -> i32`, which folds in power kernels, item
modifiers, temporary statuses, and the *chosen-at-creation* specialisation
parameters that several powers require (animal class, element type, detection
type, primary trait for `Trait Boost`). Storing those parameters as authored
fields on the power instance — not as strings interpreted at roll time — is what
makes a modded power work like a core one.

### Origins, and how a CRPG reuses them

The Basic game rolls an Origin on a table (`4c:102-109`) and the Advanced game
applies per-origin modifiers (`4c:124-131`). The spec is explicit that 4C is a
*toolkit* and that a publisher should "strip out the powers and drop in
appropriate origins and classes" (`4c:31`).

**Decision.** Origins are **campaign content**, not compiled rules. The campaign
declares its own origin list and its own modifier set in
`content/rules/origins.json`, defaulting to the six canonical ones. The
canonical modifiers are data. This is the first place a total conversion can
change the game, and it costs nothing because the modifiers are already a small
algebra: `{trait: +n}`, `{secondary: +n}`, `{power: +n}`, `{skills: +n}`,
`{repute: set 0}`, `{powers_innate: true}`, `{lose_one_power_min_1}`. Anything the
algebra cannot express belongs to a campaign script, not to the origin data.

### Generation: random, point-buy, and authored

4C generates characters by rolling every trait and power (`4c:169`, `4c:264`,
`4c:293`, `4c:400`). A CRPG cannot ship only that — players want to build a
character, and the editor must author NPCs.

**Decision.** One *character factory* with three input modes over the same
result:

- **Rolled** — the exact spec procedure, fully seeded and replayable, with the
  spec's "roll the same power twice → +20 or reroll" choice (`4c:337`) surfaced as
  a decision the player makes.
- **Budgeted** — a point-buy that spends the same *expected* budget as the rolled
  tables. The budget curve lives in content so the campaign can tune it, and it is
  validated against the rolled distribution by a statistical test, so
  point-buy characters are not strictly better than rolled ones.
- **Authored** — a fixed sheet from content. This is how NPCs, adversaries and the
  editor's "new character" template work.

All three produce the same `Character` record, so nothing downstream cares which
was used. Campaign policy (`gameplay.creation_mode`) decides which are offered.

**The generation tables are not monotone, and that is a decision, not a bug to
quietly fix.** The rank-value table (`4c:171-180`, reused unchanged for powers at
`4c:400-411` and for Lifestyle at `4c:202`) maps `00-04 → 10`, `05-09 → 3`,
`10-19 → 6`, `20-39 → 10`. So a roll of `00-04` is *better* than `05-09` or
`10-19`, and `20-39` collides with `00-04`. A player who rolls `03` gets Rank Value
10 — low superhuman — while a player who rolls `07` gets 3. Options: reproduce
literally (preserves the published distribution, which is a selling point of a
licensed-ish retro system), or normalise the bottom band. **Recommendation:
reproduce literally, expose it as a content flag (`rules.generation_table`), and
document it in the character-creation UI** so a player understands why their low
roll was lucky. The distribution is asserted by a test either way, so the choice is
reversible and visible (`§02.11`, D1).

### Skills, contacts, advanced skills

A skill is a named `+1` row-step bonus on "an action appropriate to the skill"
(`4c:275`); an advanced skill is `+2` and costs two skill slots (`4c:283`); a
contact is a skill-slot substitute handled by the dialogue/campaign layer, not the
combat layer. The rules leave *which* actions a skill applies to to the
Gamemaster — in a CRPG that must be data.

**Decision.** A skill is `{id, name, steps: 1|2, applies_to: [ActionTag]}` where
`ActionTag` is a closed enum owned by the engine (`Melee`, `Ranged`, `Dodge`,
`Persuade`, `Investigate`, `Pilot`, `Stealth`, `Science`, …). The `+2` advanced
skill is expressed as `steps: 2` from content declared as advanced, so the
slot *cost* is a campaign rule and the *effect* is uniform. `Improved Skills`
(`4c:638`) is a power kernel that grants two slots and promotes one skill to
`+3`.

## 02.4 L1 — powers as kernels plus a DSL

The Basic Selection table has **25** powers (`4c:309-335`, 4-wide buckets) and the
Advanced table has **50** (`4c:343-394`, 2-wide buckets), a strict superset of the
Basic 25 — both tile `00-99` exactly. Two entries need care:

- `Amplified Elemental/Energy Control` (`4c:430-434`) appears in **neither**
  selection table. It is reachable only through devices such as the
  Voltaic-Concussion Fist (`4c:1534`), so it must still be implemented.
- `Magic` (`4c:646-650`) is reachable only by rolling `99` on the power-count table
  (`4c:301`), and must be the character's **sole** power.

That gives **51 distinct powers** to implement. `AD-16` fixes the mechanism: a **kernel** trait
implementing a fixed hook set, registered by id, with canonical powers as Rust
kernels and content-defined powers as `Template` kernels driven by the effect DSL.

The hook set is the public API, so it is deliberately small and stated here
rather than discovered:

```rust
trait PowerKernel {
    fn on_acquire(&self, s: &mut Sim, who: Ent, p: PowerInst) {}
    fn modify_trait(&self, s:&Sim, who:Ent, t:Trait, ctx:&Ctx, v:i32) -> i32 { v }
    fn modify_attack(&self, s:&Sim, atk:&Attack, v:AttackMods) -> AttackMods { v }
    fn modify_damage(&self, s:&Sim, atk:&Attack, v:i32) -> i32 { v }
    fn modify_armor(&self, s:&Sim, who:Ent, a:&Attack, v:i32) -> i32 { v }
    fn on_hit(&self, s:&mut Sim, atk:&Attack, out:Colour) {}
    fn on_own_turn(&self, s:&mut Sim, who:Ent, act:&mut ActionPlan) {}
    fn on_resist(&self, s:&mut Sim, who:Ent, src:&Effect, colour:&mut Colour) {}
    fn movement(&self, s:&Sim, who:Ent) -> Option<MoveProfile> { None }
    fn action_economy(&self, s:&Sim, who:Ent) -> Option<ActionEconomy> { None }
}
```

Every canonical power maps onto these without special-casing the engine. A few
that shape the hook set and are worth naming, because they are the ones a naive
design gets wrong:

| Power | Why it matters to the architecture |
|---|---|
| `Force Field` (`4c:591-597`) | Two modes (device vs mental) with **different failure rules**: device shorts out 1–10 turns; mental forces a Fortitude roll whose black result dazes 1–10 turns. Needs `modify_armor` *and* post-damage state, so damage resolution must return the absorbed/inflicted split, not just a number. |
| `Absorption` (`4c:417-424`) | Converts absorbed damage into either healing or a next-turn attack whose RV *equals the absorbed amount*. Needs a stored "charged" value on the instance and a delayed effect. |
| `Growth/Shrinking` (`4c:599-620`) | Changes Brawn **and** the attacker's row step (+1 RS to attackers when grown, −1 when shrunk, +2 RS to the small one's attacks). Exercises `modify_trait` and `modify_attack` on *other* entities, which is why `modify_attack` receives the full `Attack` rather than a number. |
| `Mind Control` (`4c:652-656`) | Persistent control with a break-free re-roll on an "out of the ordinary" order. Needs an ownership/control relation in the world model and an AI override, so control is an entity relation, not a flag. |
| `Telekinesis` (`4c:778-797`) | Substitutes Willpower for Coordination on ranged attacks and uses the power RV as damage. Needs a per-attack trait substitution, i.e. `Attack` carries `trait_source`. |
| `Nine Lives` (`4c:664-666`) | Session-scoped Fortune pool plus a "declare the tens die after the roll" right — a *resolution* change, not a modifier. The only power that reaches into L0, so L0 exposes a declarative-intent parameter rather than a special case. |
| `Nullification`, `Reflection` (`4c:668-670`, `712-714`) | React to another entity's power use, with three-way outcomes including partial effect and self-damage. Needs a power-resolution event that is interceptable *before* effects apply. |
| `Magic` (`4c:646-650`) | Duplicates any other power at the Magic RV, one spell per turn. Implemented as a kernel that instantiates a `Template` kernel per spell from content — which is exactly why the DSL must be able to express the canonical powers. |
| `Alter-Ego`, `Sidekick`, `Headquarters`, `Vehicle`, `Weapon`, `Animal Command`, `Detection`, `Supersense` | Not combat modifiers at all: they are **content-bearing** powers. Each declares an authored sub-entity or a specialisation parameter. They are kernels only to enforce their constraints (e.g. Alter-Ego's "no power, traits ≤ 30" rules) at creation and validation. |

### The effect DSL

The DSL is what lets a data mod create a power. It is deliberately not a
programming language; it is a list of triggers, conditions, and effects over the
entity model, with integer expressions and dice terms.

```json
{
  "id": "power.piercing-strike",
  "name": "Piercing Strike",
  "parameters": [{ "name": "element", "type": "enum", "of": "element_type" }],
  "triggers": [
    { "on": "attack_resolved", "when": { "all": [
        { "colour_at_least": "Blue" },
        { "attacker_has_power": "self" },
        { "target_within_tiles": { "div": ["$rv", 10], "round": "up" } }
    ]},
      "then": [
        { "modify_damage": { "add": "$rv" } },
        { "apply_status": { "id": "stunned", "turns": { "dice": "1d10", "div": 2, "round": "down", "min": 1 } } },
        { "resist": { "trait": "Fortitude", "on": "Black", "then": [
            { "apply_status": { "id": "knocked_out", "turns": { "dice": "1d10" } } } ] } }
      ]}
  ]
}
```

The compiler (`AD-15`) resolves this to a `Template` kernel at load, and reports
errors as `{path, code, args}` — never as a panic and never as a partially applied
power. The DSL's expressiveness target is explicit: **it must be able to express
every canonical power in the spec**, verified by a test that defines all 51 as DSL
templates and compares their simulated behaviour against the Rust kernels. This is
the single most valuable test in the project for the moddability goal, because it
proves the modding surface is as powerful as the shipped rules.

### The range vocabulary — a weapon's reach is data, never an id

Reach is the clearest test of the moddability promise, because 4C expresses it four
different ways and the obvious implementation hardcodes the weapon table (`4c:983-992`).
The engine implements the *kinds*; the content supplies the kind and its numbers. No weapon
id appears in Rust, and adding a weapon is a record, not a release (`§02.8`).

| Kind | Meaning | Source |
|---|---|---|
| `fixed` | a constant reach in tiles | `4c:983` (shotgun, bow, crossbow, pistol, rifle), `4c:1486` (Omphalos IX, 20 sectors → 60 tiles) |
| `table_row` | reach grows one step per row of the Master Table from a starting Rank Value | `4c:990` (thrown object) |
| `rank_fraction` | reach is the user's Rank Value divided by a divisor, rounded up | `4c:991` (powers) |
| `threshold` | a distance inside a modifier, not a reach of its own | `4c:1484` (Range Drawback: halve RV and demote Blue/Yellow to Red within 12 sectors → 36 tiles) |

```json
{ "range": { "kind": "fixed",         "tiles": 24 } }
{ "range": { "kind": "table_row",     "start_rv": 6, "tiles_per_row": 3 } }
{ "range": { "kind": "rank_fraction", "divisor": 10, "round": "up", "tiles_per_unit": 3 } }
{ "modifier": { "id": "range_drawback", "within_tiles": 36, "rv_divisor": 2, "rv_round": "down", "demote": ["Blue", "Yellow"] } }
```

Every number is content, in tiles, and overridable by a mod (`§02.6`, `03:03.5`). A
genuinely new *kind* of reach — something the four above cannot express — is a **DSL
extension**, not a data override. That is the boundary the compiler enforces (`AD-15`), and
it is why the expressiveness test defines all 51 canonical powers as templates (`§02.4`). A
mod that needs a formula beyond the DSL registers a hook instead (`AD-8`), which the
determinism contract holds to the same standard (`§02.9`).

## 02.5 L2 — the encounter machine

### Time: panels, and the real-time/turn-based seam

4C resolves combat in **turns**, one turn being "an abstract amount of time equal
to the action depicted in a single comic book panel", with initiative rolled per
side and one side acting at a time (`4c:880-889`). A CRPG also needs real-time
exploration. These are the same clock at different granularity.

**Decision.** One time unit: the **panel**. A panel is the quantum for every rules
duration ("1d10 turns", "3 turns", "1-10 turns"). The encounter machine has two
drivers over the same clock:

```
exploration: real-time — 1 panel elapses per EXPLORATION_PANEL_MS (content, default 6000)
combat:      panel-driven — a panel advances when both sides have acted (or forfeited)
```

So a buff that lasts 3 panels decays while the player walks around, and lasts
exactly 3 combat panels in a fight. This removes the classic CRPG bug where a
"3 turn" spell is effectively infinite because you walked away. `EXPLORATION_PANEL_MS`
is a balance parameter in content, not a constant in the core.

The sim tick itself is fixed and decoupled from rendering: `SIM_TICK_MS` (default
16, 60 Hz) drives movement, animation-relevant state, and the panel clock; the
renderer interpolates between the last two sim states so render rate and sim rate
are independent. Render interpolation is the **only** place floats are used
(`AD-6`).

### The panel state machine

```
SELECT      player plans actions (paused; the UI is the frame clock here)
INITIATIVE  each side rolls d% (+ highest Awareness if the campaign rule is on, 4c:891)
SIDE_A      acting side executes its queued actions in a fixed order
SIDE_B      the other side executes
RESOLVE     end-of-panel: durations decrement, statuses expire, dying advances (4c:1161), regeneration ticks
ADVANCE     panel counter increments; encounter may end
```

`SELECT` is where a CRPG differs from the tabletop: the spec has players *state*
actions before initiative (`4c:884-886`), which a CRPG implements as a planning
phase plus interruption support ("waiting", `4c:1127-1129`). The machine
therefore supports a queued action with a trigger condition, which is also what
makes `Waiting` and `Evade`-into-counterattack natural.

### Actions and their resolution tables

Every action is one of a closed set, each with its own table from the spec. The
engine models this as one `Action` enum, one `resolve_action` dispatcher, and
authored outcome tables. The exact tables are the spec's; what matters
architecturally is the *shape* they share:

| Action | Trait | Colour → outcome | Notes |
|---|---|---|---|
| Melee, Bashing (`4c:952-959`) | Melee | miss / hit / pound / concuss | pound→Brawn-vs-Fortitude check; concuss→Brawn-vs-Fortitude check |
| Melee, Slashing (`4c:961-968`) | Melee | miss / hit / concuss / dying | |
| Ranged (`4c:970-979`) | Coordination | miss / hit / nail / dying | `nail` requires a **pre-roll declaration**; otherwise it degrades to `hit` |
| Rushing (`4c:997-1011`) | Fortitude | miss / hit / pound / concuss | `+1` row step per `rules.per_distance.rush_bonus.per_tiles` open tiles entered; two preconditions |
| Wrestling, Seizing/Slipping/Struggling (`4c:1013-1042`) | Brawn | three distinct 4-colour tables | a *held* status, and `Hold` damage per panel |
| Dodging (`4c:1044-1053`) | Coordination | −3 / −6 / −9 row steps to attackers this panel | a *pending modifier* on the defender, consumed by attackers |
| Block (`4c:1094-1103`) | Brawn | −1 / −3 / −6 row steps to the attacker's **Brawn for damage only** | the table/prose disagreement noted in §02.2 must be resolved |
| Evade (`4c:1116-1125`) | Melee | evade / evade+1 RS / evade+2 RS next panel | grants a stored bonus |
| Catching (`4c:1105-1114`) | Coordination | miss / miss / catch+second roll / perfect | needs a follow-up resolution |
| Multi-target (`4c:1055-1057`) | Melee | only Yellow affects everyone, as if Red | melee only |
| Pulling your punch (`4c:1088-1090`) | — | caps both damage **and** outcome colour | a pre-roll declaration |
| Movement (`4c:897-914`) | Coordination | 1/2/3 legacy sectors → **3/6/9 tiles**; climbing 1/2 → 3/6 | **−1 row step to an attack per `rules.per_distance.attack_penalty.per_tiles` tiles moved into**, and −1 to a dodge likewise (`4c:880`); rushing is exempt and instead gains +1 per the same span (`4c:1004`) |
| Exhaustion, Drowning (`4c:922-946`) | Fortitude | listed 4-colour tables, with cumulative row-step penalties | long-horizon state |
| Vehicle manoeuvre/crash/collision (`4c:1314-1361`) | Handling / Coordination | listed tables | |

Two structural requirements fall out of that table and are easy to miss:

- **Declarations are pre-roll state.** Nail, Pulling Your Punch, and the
  declaration of a Nail *before* rolling exist in three separate rules. The action
  record must carry declared intent, and the UI must be able to express it, or
  those rules are unimplementable.
- **Outcome rows are content.** The prose for each colour ("Pound. Your opponent
  suffers damage and may be knocked down") is a *sequence of effects*. Every cell
  in that table is an authored effect list from the same DSL vocabulary as powers.
  That is what makes a campaign able to reskin combat without touching Rust, and
  it keeps the engine free of English.

### Damage and conditions

Damage resolution is a pipeline, not a subtraction, because multiple rules
intercept it:

```
raw        = Brawn (unarmed) | Brawn+5 (one-handed) | Brawn+10 (two-handed)
           | material value (thrown, 4c:1065-1072) | weapon table (4c:1074-1082) | power RV
modified   = kernels: modify_damage (Trait Boost, Growth, Amplified Control, …)
armour     = kernels: modify_armor   (Body Armor, Force Field, Phasing, Metamorphosis, …)
inflicted  = max(0, modified - armour)          // 4c:1084
```

with three special cases promoted to first-class because the rules name them:
**armour-piercing** (halve the target's armour, `4c:1480`), **elemental armour
bypass** (incendiary/lightning grenades ignore non-matching armour, `4c:1415-1417`),
and **absorption** (divert instead of inflict, `4c:417-424`).

Conditions are entities owned by the sim, each with a declared expiry rule:

| Condition | Entered by | Exit |
|---|---|---|
| `knocked_down` | Pound (Black→knocked back one legacy sector, 3 tiles; Red→in place) | spends the next panel standing |
| `knocked_out` | Concuss black, Scream/Stench black | `1d10` panels; on waking, Fortitude is restored to its RV |
| `stunned` | Scream/Stench blue | 1 panel, −20 on d% rolls |
| `dazed` | Force Field mental (Black→1–10, else 1), Teleport/Dimension Jump black | 1–10 panels, no actions |
| `paralyzed` | Paralyzing Touch, Paralytic grenade | 1/10 RV panels rounded up, or 5 panels |
| `held` | Wrestling Hold | Brawn damage per panel while held; Escape/Turnabout to break |
| `dying` | Slashing/ranged Yellow | Fortitude drops one row step per panel to 0 = dead; stabilized by aid or 10 Fortune/panel at rank 01-02 |
| `winded` | Drowning red | Fortitude one row step down, cumulative |
| `nauseous` / `queasy` | Stench gas red/blue | 1–many panels, no actions / −20 on rolls |
| `collapsed` | Exhaustion black/red | rest `3-30` / `2-20` panels |
| `invisible`, `in_energy_form`, `grown`, `shrunk`, `charged`, `mind_controlled` | powers | per power |

Condition *interaction* is where simulator bugs live, so the engine enforces an
explicit precedence: `knocked_out` and `paralyzed` suppress action; `knocked_down`
requires a stand action; `dazed` suppresses action but permits resistance rolls;
`held` permits only break attempts. Precedence is a table in the core, covered by
tests for every ordered pair.

### Vehicles

Vehicles are a separate entity kind with three traits (`Durability` as both HP and
armour, `Handling` as an RV, `Velocity` as a score — `4c:1306-1338`) and a combat
path with collision rules (`4c:1344-1361`). Vehicles cannot wrestle or be wrestled
(`4c:1342`). They reuse L0 for manoeuvre and crash rolls and L1 for damage, and
add one rule that the character model lacks: **passengers take 10 damage per legacy sector
(3 tiles) the vehicle moved before a collision**. That, plus `Vehicle` as a power that
*buffs an existing vehicle* (`4c:821-823`), means vehicles are first-class
entities, not items with stats.

## 02.6 L3 — the world

### Sectors

The world is a graph of **sectors**, each one a **1 m × 1 m cell** (`AD-33`). *Sector* is
the graph's word for a cell and *tile* is the same cell when a rule counts distance; the
4C source's ~10-foot unit is always called a **legacy sector**. A sector has adjacency
(4- or 8-neighbour on a grid, or an authored graph), occupancy, terrain, material
value (for knock-through and collisions, `4c:1131-1146`), and visibility. Sector
membership is the *only* spatial fact combat needs, which keeps the encounter
machine independent of the renderer's geometry.

The tile is the unit the world is *authored* in — one Blender grid square at 1 unit = 1 m
(`08:08.1`) — and it is **fixed**: structural, like the ladder, and not changeable by any
content parameter, campaign or mod (`AD-14`, `AD-33`). It is deliberately *not* the 4C
source's unit. The spec's tables are written in **legacy sectors** of about 10 feet
(`4c:914`), and `1 legacy sector = 3 tiles` — 10 ft is 3.048 m, rounded to 3 so that every
derived distance stays an integer. Counts convert once, at the boundary
(§"Distance is content in tiles" below); nothing downstream sees the legacy unit, and
nothing in the simulation sees metres.

**Footprint and height both gate passage.** An entity carries a footprint in tiles and a
height extent in metres (`AD-26`); an opening carries a clear width in tiles and a clear
height. Passage requires both:

```
passable(entity, opening) := entity.height_extent <= opening.clearance_z
                          && entity.footprint_tiles <= opening.width_tiles
```

The height clause is independent of tile size and comes from the heightfield (`§02.12`).
The width clause is what a 1 m tile buys, and it is why a 3 m character cannot use a door
authored for a smaller one. **Occupancy is one entity per tile** — a tile is roughly
shoulder-width personal space. Entering an occupied tile is a **contest**, not a free
move: it resolves through the existing Brawn tables (`4c:1013-1042`), success displacing
the occupant into an adjacent free tile or knocking them down (`4c:1175`), and failure
ending the move. `rules.corpse_occupies` (default `false`: a body is difficult terrain,
not a blocker) decides what a defeated entity leaves behind, and
`rules.holds_multiple` (default `1`) is the escape hatch for a crowd scene.

**Height is mechanical (`AD-28`).** Every world entity carries `z_q16` and a height
extent, and elevation is rule-bearing: it feeds row steps, cover, line of sight, falling
and movement cost. The full model is `§02.12`. The important property for this section is
that elevation is a property of the **sector heightfield**, part of the world content
pack, not of the renderer — so combat, AI and the editor all read the same integers, and
a save's meaning never depends on what the art happens to look like.

### Distance is content in tiles

Every distance-bearing rule is a **content parameter in integer tiles**, shipped in the
rules pack alongside the ladders and rank bands (`core-rules.pack.json`, `03:03.5`). The
simulation never reads metres, and never learns a weapon's name (`§02.8`). Three properties
make the set safe to edit:

- **One granularity block, locked.** `tile_mm` and `legacy_sector_tiles` describe the
  conversion, are structural, and are **not moddable** (`AD-14`, `AD-33`). Changing them
  changes the world's geometry, which is a generator-class change (`§02.6`), not a balance
  tweak.
- **The 4C tables are authored verbatim in legacy sectors and converted once.** A deviation
  from `4c` is therefore a deviation in the *source's* unit, and the pack can be compared
  against the book line by line — which is the citation convention the rest of this spec
  uses.
- **Per-distance modifiers encode their own unit in the key.** `4c:880`'s attack and dodge
  penalties, `4c:995`'s optional range penalty and `4c:1004`'s rush bonus are all *per
  distance*, so a naive "sector → tile" rewrite would silently triple them. They are stored
  as `{ steps, per_tiles }`, and `per_tiles` must equal `legacy_sector_tiles` for the
  shipped profile. This is the most dangerous key in the set, and the loader asserts it.

```json
{
  "schema": "worldspiracy.rules/1",
  "granularity": { "tile_mm": 1000, "legacy_sector_tiles": 3 },

  "legacy": {
    "movement_sectors":        [1, 2, 3],        // 4c:897-914
    "climb_sectors":           [1, 2],           // 4c:907-912
    "range_sectors": { "shotgun": 2, "bow": 4, "crossbow": 4,
                       "pistol": 4, "rifle": 8 },// 4c:983
    "thrown_sectors_per_row": 1,                 // 4c:990
    "power_range_divisor": 10                    // 4c:991
  },

  "per_distance": {
    "attack_penalty": { "steps": -1, "per_tiles": 3 },   // 4c:880
    "dodge_penalty":  { "steps": -1, "per_tiles": 3 },   // 4c:880
    "rush_bonus":     { "steps":  1, "per_tiles": 3 },   // 4c:1004
    "range_penalty":  { "steps": -1, "per_tiles": 3 }    // 4c:995
  },

  "overrides": {}
}
```

The annotations and `//` citations are for this document only; JSON has no comments, so the
published record carries each citation in its schema description (the repo's convention of
citing the rule where it lives). The
loader derives the tile counts (`movement_tiles: [3,6,9]`, `range_tiles.rifle: 24`),
validates that every `per_tiles` equals `legacy_sector_tiles`, and reports each overridden
key.

**The M1 profile is a subset, deliberately.** It ships the keys a rule actually
reads — granularity, the attack and dodge per-distance penalties, the movement
bands, elevation, cover and knockback — and `rules::Rules::parse` accepts exactly
those. `per_distance.rush_bonus` and `range_penalty`, `legacy.climb_sectors`,
`thrown_sectors_per_row` and `power_range_divisor`, and
`fall_damage_per_level`/`climb_cost_per_level` are **not** shipped yet: rushing,
the optional range penalty, thrown reach (which lives on the item record instead),
power reach, falling and climbing arrive with the rules that read them (M2/M3). A
key that configures nothing is deleted rather than documented (`CONTRIBUTING.md`
rule 3), and re-adding one is a record plus the rule that consumes it. The editor shows the *effective* numbers read-only and `wsp_preview` resolves against
the same table, so the editor cannot disagree with the game (`§02.10`).

The whole block is rules-bearing: it sits inside `rules_hash`, and a mismatch is a hard,
explained refusal (`AD-11`, `AD-29`). Granularity is the deliberate exception to *"a
disputed rule is a content parameter"* (`§02.11`): a campaign may retune 4C's distances,
but not the size of a tile.

### Procedural baseline plus delta

A whole authored world is expensive to ship and to mod; a purely procedural world
is not authorable. **Decision.** The world is a **procedural baseline with authored
overrides**:

- A campaign seed deterministically generates terrain, population, and schedules.
- Content authors *named* places (sectors, buildings, NPCs, factions) that
  override the baseline.
- The save stores only the **delta** from the baseline (`AD-11`), which is what
  keeps saves small and makes "same seed, same world" true even after a mod changes
  the generator — the generator version is recorded in `meta`.

Consequence: changing the world generator invalidates deltas. So the generator has
its own version field inside the save, and a mismatch triggers either a rebase
(walk the delta onto the new baseline, reporting what could not be placed) or a
clear refusal. Silently mis-placing the player's world is the one outcome that is
not allowed.

### AI

AI is deterministic and lives in L3, driving the same `ActionPlan` the player's UI
produces — there is no separate "AI action" path, so an AI action and a player
action resolve identically. Each actor has a **behaviour record** from content
(aggression, morale, target preference, flee threshold, faction loyalties,
schedule). Decisions are made from integer scores; no floats, no wall clock, no
`Math.random` (`AD-6`). Morale uses the Repute/public-reaction tables where the
rules call for it (`4c:1276-1292`).

### Factions and reputation

Repute is a character-level score (`4c:204-208`, `1274-1292`), but a CRPG needs
per-faction standing. The layering is explicit: **faction standing is L4 campaign
state**; **Repute is the L1 trait**, and reputation-affecting events update both
according to content rules. The public-reaction table is reused for faction
reactions with a per-faction modifier, and the "reversed for criminals" rule
(`4c:1292`) is a content flag (`reaction_polarity`), not a code branch.

## 02.7 L4 — the campaign layer

4C deliberately says nothing about campaigns, quests or dialogue
(`4c:1188-1190`). A CRPG is mostly this. The layer is content-driven and stored as
state:

| System | Model | Notes |
|---|---|---|
| **Quests** | A directed graph of steps; each step has entry conditions, completion conditions, and effects, all from the DSL vocabulary plus campaign predicates | The DSL is shared with powers so authors learn one vocabulary |
| **Dialogue** | A node graph with conditions, player options, skill checks that resolve through L0, and effects | Skill checks reuse the resolution primitive; "Persuade" is a skill tag, not a special case |
| **Journal** | A projection of quest/dialogue state into player-readable text | Human-readable by design — the journal is part of `core` in the save (`AD-11`) |
| **Inventory & economy** | Items with the 4C equipment statistics plus campaign metadata; prices scaled by Lifestyle band (`4c:1255-1272`) | Lifestyle gates procurement, as the spec intends |
| **Progression** | Fortune as the currency of advancement: `+1 RV costs current value`, new power `1000`, new skill `250` (`4c:1381-1391`) | Costs are content, so a campaign can tune pacing |
| **Fortune gain/loss** | Scaled by impact: Personal ±5 … Global ±100 (`4c:1229-1251`) | Authored on the events that cause them |
| **Time of day** | Panels accumulated → clock; schedules keyed to the clock | Ties exploration to the world |

The one rule is that the campaign layer never contains a damage formula. If a
quest needs to hurt someone, it emits an `Attack` or an `Effect` into L2.

## 02.8 The equipment catalogue as content

Everything in `4c:1395-1569` — grenades, the Omphalos IX, the Shrouded Relay, the
Voltaic-Concussion Fist, potions — is **content**, not code, and it is the
reference implementation of the content model:

- Grenades: an area effect, a save, and a per-colour outcome list
  (`4c:1411-1437`). The "mimics power X at RV N for absorption purposes"
  (`4c:1415`) is a declared tag, so `Absorption` can react to a grenade.
- The Omphalos IX: five named modifiers (Accurate `+20`, Deadly Accuracy
  red→blue, Armor Piercing half armour, Limited Ammo, Range Drawback halve RV and
  cap at Red under 12 legacy sectors, 36 tiles) plus ammunition (`4c:1476-1488`). Every
  one of those is expressible in the DSL, which is the test that the DSL is good enough.
- Potions: a duration, an effect, and the *lesser/greater* variant rule
  (`4c:1565-1569`) — variants are generated from the base by a content transform,
  not written three times.

Every weapon's reach is a **range record** in the vocabulary of `§02.4` — `fixed`,
`table_row` or `rank_fraction` — and every distance inside a modifier is a parameter of
that modifier (Range Drawback's 12 sectors → 36 tiles, `4c:1484`). A rifle's reach is 24
tiles in the rules pack (`§02.6`), and a mod may change it; the string `rifle` never
appears in Rust.

Anything an item can express that the DSL cannot is a gap in the DSL, and the fix
is to extend the DSL, not to add an `if item.id == "omphalos_ix"` in Rust.

## 02.9 Serialisation and the determinism contract

`Sim` is the single serialised value. The serialiser is explicit and versioned,
never `derive(Serialize)` on the whole graph, because:

- Save format stability is a promise (`AD-11`), and a derived serialiser changes
  whenever the struct changes.
- The `core` half must stay readable JSON for the player (`AD-11`).
- The `world` half must be compact.

```
envelope { save_version, engine_version, generator_version, ruleset,
           content_hash, mods:[{id, content_hash}], seed, rng_streams,
           playtime, clock }
core     { characters:[…], party, inventory, journal, quests, factions, flags }
world    base64(zstd(delta { sectors, entities, schedules, ai states, conditions }))
```

`ruleset` records the ladder variant (`basic` | `advanced`). This is not cosmetic:
the two ladders resolve the same Rank Value differently at the top of the scale and
Basic has no row for `1001..9999` (`§02.2`), so a save created under Advanced must
not be silently loaded under Basic. A mismatch is a hard refusal with an explanation,
not a clamp.

The zstd step reuses the encoder shipped for the editor (`AD-13`), so the game and
the editor share one compression implementation. Retention is the constraint to
watch: `keep_revisions: 8` (launcher config) means eight copies of the world delta
exist on disk, so a 2 MiB save costs ~16 MiB per slot. The budget is a **512 KiB
target and a 4 MiB ceiling** per save, asserted by test (R6).

**The determinism contract**, stated so it can be tested and so modders can be held
to it:

1. Given the same `engine_version`, `seed`, `content_hash`, mod set, and an
   ordered command stream, the sim produces an identical state hash after every
   tick.
2. Nothing observable by the sim reads the wall clock, the locale, the platform,
   or any browser API.
3. All randomness comes from the seeded streams; there is exactly one gameplay
   stream, and a separate cosmetic stream that is never saved and never read by
   the sim.
4. State-visible numbers are integers or fixed-point with declared rounding.
5. Script mods (`AD-8`) are held to the same contract, and the harness reports
   which script broke it. **As built:** a script gets no RNG at all — it declares
   dice in an effect and the core draws them — its answer is compiled by the DSL's
   own compiler into the same `HookEffect` queue, and the set of live scripts with
   their content hashes is recorded in the save's `meta.scripts`, so a replay that
   differs because of a script can name it (`03:03.7`, `crates/kobra-core/src/script.rs`).
6. The simulation is multithreaded (`AD-5`, `07:07.4`) and the contract is unchanged
   by thread count: parallel systems are maps over a fixed id-derived partition,
   reductions are ordered, RNG sub-streams are seeded per `(tick, system, partition)`,
   and the golden corpus is replayed at 1, 2, 4 and 8 threads with hashes compared.

The verification harness is a state hash printed after every command, compared
against a committed golden file. Every rules bug fix adds a replay fixture
(`§05.4`).

## 02.10 What the core exposes

The full ABI is in `01-runtime-and-language-split.md` §"The ABI". For the rules
layer specifically, the core must be able to answer these without the caller
knowing the rules:

| Call | Purpose |
|---|---|
| `wsp_new_campaign(config_json) -> sim` | Build a world from a seed + content |
| `wsp_character_create(mode, budget, seed) -> character` | Rolled / budgeted / authored |
| `wsp_command(sim, command_json) -> events_json` | The one mutation entry point |
| `wsp_tick(sim, elapsed) -> events_json` | Advance the clock |
| `wsp_preview(sim, action) -> prediction` | The UI's "what would happen if" (used heavily by the editor) |
| `wsp_validate_content(bytes) -> report` | Shared with the editor and the runtime |
| `wsp_save(sim) / wsp_load(bytes)` | Envelope in, envelope out |

`wsp_preview` deserves a note: it is the same resolution path run against a
hypothetical action without committing it, and it is why the "one primitive"
design in §02.2 pays off. The UI can show exact odds and the editor can show a
playtest consequence, and neither can disagree with the real resolution.

## 02.11 Open rules decisions

A full read of the spec produces a long list of places where 4C leaves something
undecided, contradicts itself, or defers to the Gamemaster. Each one is a decision
the engine must make *once*, in the open, and record — because the alternative is an
implementer inventing a rule in a `match` arm. Every entry is a content-visible or
save-visible choice, so each gets a decision id, a recommendation, and a home in
content where it can be overridden.

This register is the artifact the rules tests are written against.

| # | Issue | Spec | Decision |
|---|---|---|---|
| D1 | Generation table is non-monotone: `00-04 → 10`, `05-09 → 3`, `10-19 → 6`, `20-39 → 10` | `4c:171-180` | Reproduce literally; expose as `rules.generation_table`; document in the creation UI. Assert the distribution by test. |
| D2 | Block table contradicts itself: labels say −1/−4/−7 row steps, prose says −1/−3/−6 | `4c:1101-1103` | Use the **prose** (−1/−3/−6): it is the mechanically meaningful text, and the labels look like an off-by-one from counting the starting row. Content parameter `rules.block_table`. |
| D3 | Mind Control succeeds on Red *or greater*, so only Black resists | `4c:656` | Take it literally (Black is the only failure) but treat Blue/Yellow as a **cleaner** control: Blue/Yellow also denies the target its break-free re-roll for the first panel. Documented as an intentional reading, flagged for playtest. |
| D4 | Repute's Master-Table roll never names a Rank Value | `4c:1276` | Resolve the public-reaction roll at the **character's Repute score treated as a Rank Value, clamped into the ladder**. Repute is 0–33, so this lands in the low bands and reads correctly at the table. Content parameter. |
| D5 | Force Field (Mental): whether the field absorbs before armour, and how the Fortitude check interacts, is unstated | `4c:597` | Pipeline order: **absorbing armour (powers) → worn armour → Damage**, and the Fortitude check happens once per attack that exceeds the field. Fits §02.5's damage pipeline. |
| D6 | Omphalos IX: `Accurate (+20)` vs `Range Drawback (halve, cap at Red)` has no order of operations, and the two colour clauses can loop | `4c:1476-1484` | Order of operations: modifiers that adjust the **value** first, then colour caps/downgrades, then colour upgrades. One pass, no loop: `value → colour → downgrade → upgrade`. |
| D7 | Invisibility potion duration is missing | `4c:1555` | Default to the **lesser/greater duration family** (`4c:1565-1569`): 3 panels base, 2 lesser, 4 greater. Content parameter, so a campaign can set it. |
| D8 | Daze/nausea durations from Scream Red and Stench Red are "the next turns" with no number | `4c:1426`, `4c:1435` | `1d10/2` minimum 1 panels, matching the Black row of the same tables. Content parameter. |
| D9 | Exhaustion's "rest for 10 turns" can exceed or undercut the collapse durations | `4c:946` | Rest requirement is `max(10, rolled duration)`; the −10 penalty persists until it is met. |
| D10 | Drowning: whether "winded" (Fortitude −1 row step) is permanent and whether it stacks with the per-turn −1 | `4c:929-933` | **Stacks**, and resets only when the character reaches breathable air and takes a full rest panel. Explicitly stated in the drowning test. |
| D11 | Attacking multiple targets: on Yellow, what does "as if the result was Red" mean for a melee table? | `4c:1057` | Resolve each target individually as **Red on that target's own table** (so a bashing attack is a Hit, and the knockdown check does not fire). |
| D12 | Nine Lives and Fortune assume a "game session" that is never defined | `4c:666` | A session is a **launch-to-shutdown span**. Nine Lives' pool is seeded at session start from the save's day counter, and is not persisted; ordinary Fortune is. Recorded in the save's `meta` for auditability. |
| D13 | `Improved Skills` (+3 to one skill) — replaces or stacks with the advanced-skill +2? | `4c:640` | **Replaces** the skill's bonus (set to +3), it does not add to it. Skills store an absolute `steps` value, never a stack of contributions. |
| D14 | `Amplified Elemental/Energy Control` is in no selection table; `Magic` conflicts with origin bonus-power rules | `4c:430-434`, `648`, `127-130` | Both are implementable and implemented; `Magic` as the sole power **wins over** origin power modifiers (the origin's bonus power is suppressed and reported in the character's creation log). |
| D15 | Vehicles: no initiative, no own attack, and collision damage is asymmetric when both vehicles are identical | `4c:1342-1361` | A vehicle acts on its operator's initiative and has no attacks of its own (weapons are the operator's action). Collisions resolve **attacker-first, single pass** — one damage application per participant, order fixed by initiative, so identical vehicles trade symmetric damage. |
| D16 | Grenades define an "affected sector" informally and have no scatter/throw roll | `4c:1411-1437` | The blast is centred on the tile the grenade was thrown at, with a radius of `grenade.radius_tiles` (default 3 — one legacy sector, so `AD-33` does not silently shrink a 4C blast to a single square metre); the throw uses the standard thrown-weapon range table, and landing off-target is a **tile-level scatter determined by the throw result colour** (Black = one legacy sector, 3 tiles, away from the target, chosen deterministically by actor id). Content parameter. |
| D17 | Vehicle-as-power (`Vehicle`) buffs an existing vehicle by `ceil(RV/2)` | `4c:823` | Applied as a **modifier layer**, recomputed on load, never baked into the vehicle's stored traits — so removing the power removes the buff, and the save does not need a "was buffed" flag. |
| D18 | Rank Values have no ceiling and advancement has no cost curve beyond "current value" | `4c:1383-1391` | Advancement is open-ended; the ladder clamps effective rows at the top, so extreme Values are harmless. A campaign may cap via `gameplay.max_rank_value`. |
| D19 | `Pulling Your Punch` caps damage and success level with no cost | `4c:1088-1090` | Implement as declared intent, no cost, exactly as written — it is a player-agency rule and the tabletop has no cost either. |
| D20 | The Repute gain/loss table is missing from this edition | `4c:1298` | Author one as content in the same shape as the Fortune gain/loss scale (`4c:1229-1251`), so the two reinforce each other. `Repute` gain/loss is always content-authored, never a Rust constant. |
| D21 | Duplicate power rolls may be taken as `+20 RV` with no stated cap | `4c:337`, `4c:396` | No cap, as written, but the creation UI shows the resulting value so an absurd stack is visible. A campaign may cap via `gameplay.max_power_rv`. |
| D22 | Weather Control "may count as two powers" | `4c:523` | Content flag on the power: `costs_slots: 2`. Default **1** (the permissive reading), because charging two slots is the GM's option, not the base rule. |
| D23 | Catching's Blue result says "any result less than Blue" without saying whether that is Black+Red only and whether object modifiers apply first | `4c:1113` | The follow-up roll is compared **before** any modifiers on the caught object, and "less than Blue" means Black or Red. Single named test. |
| D24 | Power names differ between the selection tables and the descriptions | `4c:329` vs `728`, `4c:335` vs `825` | Content ids are canonical and normalised (`wsp.power.super-leap`, `wsp.power.wall-crawling`); the display name is a string id, so a naming mismatch can never break a lookup. The validator rejects a display string used as an id. |
| D25 | Grenades name powers that do not exist under those names (`Elemental Generation (Fire)`, `Amplified Energy Control`) | `4c:1415`, `1417`, `1534` | The DSL gets an **effect tag** vocabulary (`element:fire`, `generation:electrical`) distinct from power ids, and items declare tags. `Absorption` and `Nullification` react to tags, so a grenade and a power are both addressable without aliasing names. |
| D26 | `Advanced Rank Values` is prose and never lists the 17 bands | `4c:229-231` | The bands come from the ladder definition, which is generated from the authoritative source next to the colour table. The ladder file is structural content: not moddable (`AD-14`). |
| D27 | Chameleon is an opposed roll that bypasses the Master Table entirely | `4c:460` | Implemented as an explicit opposed-roll resolution alongside Telepathy's blocking contest; both are the only two opposed mechanics in the system, so they share one helper and one test. |
| D28 | Potion drinking interacts with Fast Attack but not with the default one-attack-per-turn rule | `4c:1542` | Drinking consumes the character's action for the panel unless Fast Attack is held, in which case it consumes one attack. Any additional potion action is subject to D29. |
| D29 | Drinking a second potion before the first expires negates both | `4c:1542` | Enforced as a strict rule with an explicit player warning and confirmation, because silently losing two effects is indistinguishable from a bug. The warning is a UI affordance, not a rules change. |
| D30 | `Headquarters` and `Weapon` defer all content to GM approval | `4c:622-636`, `839-845` | Both are content-bearing powers: the campaign authors the catalogue of legal headquarters and weapons, and the power's Rank Value only gates which are available. The validator reports "no content catalogue declares X" as an error for those powers. |
| D31 | Anti-Arcane grenade's "damage equal to half the target power's RV" clause is unreachable at a fixed RV 40 with three outcome bands | `4c:1411` vs `670` | Implemented with the grenade's own three-band table (Black none / Red half effect / Blue–Yellow disrupted). The unreachable damage clause is **not** implemented, and the omission is recorded here so it is a decision rather than an oversight. |
| D32 | **A spatial trigger region does not exist yet.** Triggers are conditions and effects in the DSL, but "when someone enters this area" needs a region primitive the world model does not have | new (`AD-28`, `AD-30`) | Add `region` to the world pack (a named set of sectors plus an optional level band) and `in_region` / `enters_region` / `within_radius` conditions to the DSL. Small, but it must land before trigger authoring, because a trigger the editor cannot express is a trigger that gets hardcoded. |
| D33 | The 4C sector is "approximately 10 feet" (`4c:914`) — neither a metre nor a Blender grid square, and the source's own word is *approximately* | `4c:914` | The **sector is a 1 m tile** (`AD-33`); the 4C tables stay authored in legacy sectors and convert once (`1 legacy sector = 3 tiles`). Recorded as a deliberate divergence so the citations stay honest. |

| D34 | Entering a tile another entity already occupies is "a contest, not a free move" (`AD-33`), but the wrestling tables' *Struggling* row has no defence roll | `4c:1013-1042`, `AD-33` | Resolved as the mover's own roll: one `d%` on the mover's Brawn; **Blue or better displaces** the occupant into a free adjacent tile, or knocks them down when there is nowhere to go, and anything less ends the move. The occupant does not roll, because the source table has no second roll. Content parameter `rules.occupancy_contest`. Pinned by `l2::encounter::tests::entering_an_occupied_tile_is_a_brawn_contest`. |
| D35 | Initiative ties are not addressed: both sides can roll the same `d%` and add the same Awareness | `4c:886-889` | The **party wins a tie**. The source does not say, so the reading is stated and pinned by a test rather than left to a sort's stability. |
| D36 | **AI turns a decision into a command, never into a mutation.** `02:02.6` says an AI action and a player action resolve identically, but the obvious implementation is for the AI to move entities directly | `02:02.6` | `l3::ai::advance_stations` returns `Decision`s and the sim emits `hostile_contact`; the **client** sends `begin_encounter: "wild"`. The cost is that a client which ignores the event leaves the party standing next to an enemy, so the event carries the station, the target and the intent, and the shipped shell acts on it. Content parameter `rules.ai.auto_engage` is the escape hatch if a campaign wants the sim to open the fight itself. |
| D37 | **A campaign needs to say when its day begins.** A clock that starts at panel 0 starts at midnight, which puts every scheduled NPC at its night post and reads as an abandoned town | `02:02.7` | A `world` record carries `start_minute` (default 360, dawn). Stations are placed at the post that hour names, so the campaign opens where its schedules say the world is. Content, because a campaign set at night is a decision rather than a bug. |
| D38 | **Hostility is a disposition; the behaviour record is what acts on it.** A hostile faction's members could charge on sight, or hold a checkpoint | `02:02.6` | The two records stay separate: the faction's standing band says *who* is hostile, and `Behaviour` says whether this member acts on it (`stationary`, `patrol`, `aggressive`, `defensive`, `skittish`). That is what lets a campaign author an intimidating but non-belligerent presence, and it is pinned by a test that asserts a posted member of a hostile faction does not engage. |
| D39 | **A quest step's entry conditions are checked before its completion conditions**, and a quest nothing can start is reported | `02:02.7` | The validator warns `quest_never_starts` for a quest with neither an entry condition nor any effect that starts it, because "the quest never starts" and "the quest is broken" are indistinguishable to a player. The two-phase plan/write split (`l4::journal`) then makes re-evaluation idempotent, so a condition cannot read a line this pass has not written. |
| D40 | **A check gates *how* an option resolves, not whether it is offered.** A failed persuasion could refuse the option outright | `02:02.7` | `when` decides whether an option is offered; a `check` decides what happens when it is taken, and its colour is emitted as a `check` event so a scene can branch on it. An offered-but-unlikely option is shown with the colour it needs, which is what makes the rule legible (`02:02.11` rule 2). |
Two general rules follow from the register:

1. **A disputed rule is a content parameter, not a constant.** D1, D2, D4, D6–D8,
   D16 and D18 are all `rules.*` content keys with a shipped default. A campaign or
   a mod can change them, which is exactly the flexibility the spec's own "Advanced"
   boxes intend. The one exception is **granularity**: the tile size and the
   legacy-sector conversion are structural (`AD-33`), because they change the world's
   geometry rather than its balance.
2. **A disputed rule is never silent.** Every entry here has a named test, and the
   chosen reading is surfaced in the character-creation log or the tooltip that
   shows the roll — so a player sees the rule that was applied rather than
   inferring it.

**As built at M2.** The hook set above gained three entries the canonical powers
needed and the original list did not name: `on_absorb` (the absorbed/inflicted
split `Force Field` and `Absorption` react to, `4c:419-424`), `on_power_used`
(the interceptable power resolution `Nullification` and `Reflection` need,
`4c:670`), and a `PowerCtx` whose `owner` is the power's carrier — because the
encounter folds the *target's* powers through the same hook so `Growth/Shrinking`
can change an attacker's roll (`4c:618-620`). `on_acquire` writes through an
`AcquirePlan` rather than mutating the sim, so the same read-only-snapshot rule
that governs a hook governs acquisition. `TurnPlan` carries `consumes_action`, so
`Regeneration` can spend the panel (`4c:718`).

The runtime resolves a canonical id to its Rust kernel and any other id to a
compiled DSL `Template` (`AD:16`); the 51 shipped templates in
`core-powers.pack.json` are the expressiveness witness and the author-facing
reference, not the runtime path for a canonical id. `Magic` is the 52nd kernel
and is excluded from the 51 on purpose: it duplicates other powers rather than
having an effect of its own, so it is pinned by its own test. `tests/dsl.rs`
compares every hook of every template against its kernel over a probe matrix of
colours, attack shapes, owners, distances and parameters; presentation (`notes`,
creation-log lines) is excluded, and everything a roll reads is compared exactly.

**As built at M3.** The campaign layer is content plus a plan/write quest runtime
(`l4::journal`), one predicate vocabulary shared by quests, dialogue and schedules
(`l4::condition`), a dialogue graph whose skill checks resolve through the same
primitive an attack does (`l4::dialogue`), per-faction standing with the
public-reaction table's criminal polarity as a content flag (`l4::factions`), the
`4c:1381-1391` advancement curve and the `4c:1229-1251` impact scale as content
(`l4::progression`), and prices scaled by the buyer's Lifestyle band
(`l4::economy`). L3 gained the clock and the schedules (`l3::clock`), the
procedural baseline and its delta (`l3::gen`) and the behaviour-record AI
(`l3::ai`). The register's `D32` trigger region is still **not built**: the
predicate vocabulary covers flags, standing, items, quests and time, but not a
spatial region, so "when someone enters this area" remains a task.

**As built at M1.** The register above is the plan; this paragraph records the
state of the implementation so the two do not drift. L0 (the resolution
primitive, the Fortune window, the dice algebra), L1's traits, skills, items and
the three creation modes, L2's panel machine
with melee/ranged/dodge, the damage and armour pipeline and the M1 condition set,
and a small L3 sector map with elevation and cover were implemented and tested at
M1. M2 added powers, the DSL, origins, vehicles, the full 18-kind condition set
and the Rust half of the script host; the conditions table in §02.5 is now
complete, with `Shorted` (a force-field device that took more than it can hold,
`4c:595`) and `Collapsed` (`4c:946`) as the two the register's table omitted.
Still not built: **AI**, and the Advanced ladder — `tools/gen-tables` compiles one
ladder, so a save or a config naming `advanced` is refused rather than clamped
(`02:02.2`, and the known gaps in `README.md`).

## 02.12 Elevation, cover and line of sight

Height is mechanical (`AD-28`), and the whole model is built on the mechanism 4C already
uses for situational modifiers: the **Row Step**. The spec's own circumstances are row
steps — *hiding in shadows −5 RS*, *jostling train −2 RS*, *raining −2 RS*
(`4c:1203-1209`) — so elevation adds a table entry, not a subsystem.

### The heightfield

Each sector carries an integer **level** (0 = grade, +1 per storey, negative for
basements/pits) and a **terrain height** used for occlusion. Both belong to the world
content pack. Levels are authored, not derived, so a designer can make a ledge that
*looks* low or high independent of its art. A **level is a storey** (about 3 m), so under
`AD-33` a level and a tile are different lengths on purpose: `rules.climb_cost_per_level`
is 3 tiles precisely to keep vertical and horizontal movement 1:1, and every rule below
states which of the two it means.

An entity's **eye level** is `sector.level + entity.z_level + eye_offset` where
`eye_offset` comes from its size (a `Growth` character is taller, `Shrinking` shorter —
`4c:599-620`), which is how a size-changing power interacts with cover for free.

### Elevation as row steps

```
levels_above = attacker.eye_level - target.eye_level
rs = clamp(sign(levels_above) * min(abs(levels_above), rules.elevation_cap), -cap, +cap)
```

with `rules.elevation_steps = +1 per level`, `rules.elevation_cap = 2`, and
`rules.elevation_applies_to = [ranged, power, perception]` by default. Melee is excluded
because 4C's exclusion is about a blow's geometry over one legacy sector (≈10 ft, `4c:914`),
and the shipped default keeps the source's list. It is worth re-reading under `AD-33`:
melee now happens across one 1 m tile, where a storey of height is a wall rather than a
slope, so a campaign that wants clifftop duels should add `melee` — the key already supports
it. A campaign may also add `rush`, and **rushing** already gains `+1` row step per
`rules.per_distance.rush_bonus.per_tiles` open tiles entered (`4c:1004`), so a downhill
charge is strong through the existing rule rather than through a new one.

This is deliberately the same shape as every other modifier in the game: it shifts the
band index, the existing table resolves, and `wsp_preview` can show *"+2 RS, high ground"*
as the reason (`02:02.10`). No new resolution path exists, so the editor cannot disagree
with the game.

### Line of sight and cover

```rust
// Integer raycast over the sector grid; parallel-safe, no floats.
fn sight(att: Ent, tgt: Ent) -> Sight   // Clear | Covered(rs) | Blocked
```

A supercover traversal of the sectors between attacker and target compares each sector's
**blocking height** against the *lower* participant's eye level:

- nothing rises above it → `Clear`;
- something rises above it but not above the *higher* participant's eye → `Covered`,
  applying `rules.cover_steps` (−2 default);
- something rises above both → `Blocked`.

Cover and elevation **stack only up to `rules.cover_cap`**, so a target behind a wall on a
hill cannot become mathematically unhittable. Both are computed from frozen tick state, so
they are map-only passes and obey the parallel rules (`07:07.4`) — and visibility is the
textbook parallel workload this engine wants.

Terrain that grants high ground must **read** as high ground: silhouette legibility becomes
a rules requirement, not an art preference, because a player who cannot see the ledge
cannot use the mechanic.

### Falling, climbing and area effects

| Rule | Content key | Default |
|---|---|---|
| Falling damage per level dropped | `rules.fall_damage_per_level` | flat, Brawn-independent |
| Extra movement to climb or descend a level | `rules.climb_cost_per_level` | 3 tiles (one legacy sector), reusing the climbing table (`4c:907-912`) — keeps vertical and horizontal movement 1:1 |
| Levels an area effect reaches up or down | effect `vertical_reach` | 1 |
| Perception range added per level of height | `rules.sight_per_level` | modest; folds into the Detection/Supersense model (`4c:476-486`) |

Falling is the one genuinely new rule, because 4C has none. It earns its place through an
emergent interaction rather than by being added for its own sake: **Pound Black already
knocks the defender back one legacy sector** — 3 tiles, `rules.knockback_tiles` (`4c:1175`)
— so knocking someone off a ledge
needs no new combat rule at all — only a height difference and a fall rule to resolve what
happens next. The same applies to a rush that carries both participants over an edge.

### The two rules that keep a free camera safe

`AD-30` makes the camera top-down, isometric or ground-level depending on context. Two
invariants protect the mechanics from that freedom, and both are normative:

1. **Perception is computed from the character, never from the camera.** Sight, cover and
   cover-based targeting read the character's eye level and the heightfield. A ground-level
   camera can see over a wall the character cannot see past, and a top-down camera can see
   what is behind the character's back; if either influenced the rules, the UI would offer
   targets the rules refuse, or hide targets the rules allow.
2. **The rules' heightfield is a quantised layer, separate from the art geometry.** Art may
   have slopes, stairs, rubble and overhangs; the rules read integer levels and blocking
   heights from the world pack. Coupling the rules to a mesh would make balance depend on a
   modeller's output and would put floats inside the simulation.

### Consequences

- **No structural save migration.** `z` and the height extent were stored before any rule
  read them (`AD-26`), so this is a **rules-version** change, not a data change. That is
  precisely what the seam bought.
- **Camera profiles and animations are presentation, not rules.** They change
  `presentation_hash`, never `rules_hash` (`AD-29`), so a cinematic or a re-rigged cast
  cannot invalidate a save.
- **Authoring cost lands in the world pack and the editor:** a per-sector heightmap, a
  blocking-height layer, and an editing view that shows where a shot is clear, covered or
  blocked. The sector editor must gain these before the encounter content is authored, or
  level design and combat design will be done blind (`04:04.4`).
- **AI must value ground.** Behaviour records gain a position score that includes
  elevation, cover and the sight lines a position offers; otherwise the mechanic exists
  and no one uses it.
- **Content, not code, decides the feel.** Every number above is a content key with a
  shipped default, so the campaign can make height decisive or a rounding error without
  touching the engine.

## 02.13 The available-action set

A CRPG's interface is a promise about the rules. A control that is offered and then refused
teaches the player to distrust the interface; a control that is silently missing teaches them
nothing. So **which actions exist right now is engine state**, not a UI guess — the argument
of `AD-15` applied to the interface. The core answers with a projection (`wsp_actions`), and
every page renders that answer rather than deriving it.

### The projection

```json
{ "actions": [
  {"action": "talk",  "label_key": "action.talk",  "enabled": true,  "reason_key": "", "target": 7},
  {"action": "wait",  "label_key": "action.wait",  "enabled": false, "reason_key": "reason.in_encounter"},
  {"action": "fight", "label_key": "action.fight", "enabled": false, "reason_key": "reason.in_encounter"},
  {"action": "spend_fortune", "label_key": "action.spend_fortune", "enabled": true,
   "reason_key": "", "surface": "trade"}
]}
```

- **Every action is always present**, with `enabled` and a `reason_key`. A disabled action is
  shown disabled *with its reason* rather than removed, so the player learns why
  ("Unavailable in a fight") instead of watching a control vanish (`02:02.11` rule 2). A shell
  may choose to hide a disabled entry; it may never invent an enabled one.
- `label_key` and `reason_key` are **string ids**, never English (`AD-22`).
- `target` names the entity the action would act on — Talk's partner, or the leader for a
  self-dialogue.
- `surface` says where a compound action lands. `spend_fortune` is the one compound action:
  Fortune is both the currency and the advancement cost (`4c:1381`), so `surface` is `trade`
  when a merchant is in reach and `advancement` otherwise.

### Exploring, and the modal conversation

`exploring` means: a party leader exists, no encounter is running, and no conversation is
open. A conversation is **modal** — while one is open, `move` and `begin_encounter` refuse
with `in_dialogue`. "You cannot walk away mid-sentence" and "you cannot start a fight
mid-sentence" are therefore rules rather than interface conventions.

| Action | Offered when | `reason_key` when it is not |
|---|---|---|
| `talk` | exploring, and a station within 1 tile has a `dialogue`, **or** the world authors a `self_dialogue` | `reason.in_encounter`, `reason.in_dialogue`, `reason.no_party`, `reason.no_partner` |
| `wait` | exploring | `reason.in_encounter`, `reason.in_dialogue`, `reason.no_party` |
| `fight` | exploring | as `wait` |
| `spend_fortune` | exploring, and a station within 1 tile keeps a `shop`, **or** the leader can afford a Rank Value | `reason.in_encounter`, `reason.in_dialogue`, `reason.no_party`, `reason.nothing_to_spend` |

The reasons are ordered by what blocks the whole exploration set: an encounter, then a
conversation, then no party. A specific reason (`no_partner`, `nothing_to_spend`) is only
reported when nothing structural is in the way.

### Merchants are people, so shops are places

A `shop` record with no station is a counter nobody keeps. A station gains `shop: <shop id>`,
and the trade surface is offered only beside it; `wsp_shops` reports only the counters in
reach. The merchant is a person standing in the world, which is what makes a shop's prices a
function of the buyer's Lifestyle (`4c:1255-1272`) *at a place*, and what lets a mod move or
remove a merchant by editing the world record. A station with an empty faction never joins a
wild encounter (`02:02.6`), so a merchant is not made hostile merely by keeping a counter.

### A fight invited by a conversation

An effect moves campaign state; starting a fight needs the whole sim, which the journal's
effect applier deliberately does not have. A dialogue option may therefore carry an **action**
alongside its effects:

```json
{"id": "threaten", "text_key": "dialogue.shopkeeper.threaten", "action": {"kind": "fight"}}
```

with an optional `encounter` (empty means a wild fight on the current map). Taking the option
closes the conversation and starts the encounter through the same `Encounter` machine, so a
fight invited by a talk and a fight begun from the action bar are one resolution path, not two
(`02:02.6`). The validator refuses an `action.encounter` that names no record. **This is the
only form in which `fight` is available during a conversation** — which is the whole of "Fight
cannot be triggered during a talk unless invited during the talk".

### Talking to yourself

`talk` with nobody in reach is offered only when the world record authors `self_dialogue`. The
leader is then their own partner: the same node graph, the same conditions, the same checks,
the same panel cost. A campaign that authors none simply never offers the action, and the
`no_partner` reason is what the interface says instead.

### Advancement is a surface, not a new rule

`spend_fortune` defers to `l4::progression`: `+1 RV costs the current value` (`4c:1383`), a
new power 1000, a new skill 250 (`4c:1381-1391`). The action is offered when the leader can
afford their **cheapest** stored trait, because a character who cannot afford that cannot
afford anything — an empty panel behind an enabled action is worse than a refused one.

### Consequences

- **The shell holds no rules.** Adding an action, or changing when one is legal, is a change
  to this section and to the projection; the interface gains a button and a string id, and no
  page re-derives a predicate. That is what keeps the play page and the editor's scratch-sim
  from disagreeing (`AD-15`).
- **The refusal and the offer agree.** `wsp_actions` reports available if and only if the
  corresponding command would be accepted, because both read the same state; a divergence is
  the bug `02:02.11` warns about.
- **Every rule above has a test named for it** (`05:05.4`), because a projection that is
  merely plausible is exactly the failure mode this section exists to prevent.

