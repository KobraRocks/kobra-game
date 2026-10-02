# 09 — The moddable UI

> **Partly superseded by AD-42, via AD-38.** The contract survives and is the reason this
> document is worth reading: versioned projections, engine-computed signals, data layouts
> with no expression language, save-neutral UI, one registry. The DOM renderer, its
> capability object and the browser shell do not — the interface tier is re-derived natively,
> with Lua as the script language (AD-39).

How a mod ships a panel, a HUD or a whole re-skin, and what the engine owes it.

**Status.** **Built** at M3.6. The normative content is here, the milestone is in
`05:05.5`, and the gates are in `05:05.4` and §09.11. The shipped shell *is* this one: the
base panels are the first UI pack (`09:09.11`, "Dogfood"), registered through the same
registry and receiving the same capability object a mod does.

The premise, decided: **mods run at the user's own risk** (the platform's threat note T14,
`03:03.10`), and a moddable interface is a feature rather than a hazard. A player who
can replace the character sheet or build a HUD adopts the consequences, exactly as they
do with a modded rules pack.

The one rule that keeps this from becoming a second game:

> **The page never decides a rule.** It posts a command and renders a projection.
> Availability, legality, prices and outcomes are the engine's answers (`AD-15`,
> `AD-34`).

## 09.1 The five contracts

Everything a UI can touch is one of five things. If it is not in this table, it is not
API, and a mod that needs it needs an engine change instead.

| Contract | Direction | Shape | Versioned as |
|---|---|---|---|
| **Projection** | engine → UI | read-only JSON of state the sim already owns | `kobra.projection/<name>/1` |
| **Action** | engine → UI | `kobra_actions`: `{action, label_key, enabled, reason_key, …}` (`02:02.13`) | `kobra.ui/v1` |
| **Command** | UI → engine | the closed vocabulary of `02:02.10`, the replay log | `kobra.ui/v1` |
| **String** | content → UI | two tables, `AD-22` | `kobra.content-pack/1` |
| **UI state** | UI → UI | open panels, tab, HUD layout | host settings (§09.9) |

Two consequences are load-bearing enough to state as invariants:

1. **The command stream is the replay log, so UI-only state never enters it.** Sorting a
   list, choosing a tab, remembering a panel position — none of these is a command. If
   they were, a replay would depend on what the player had open, and `AD-21`'s oracle
   would stop meaning anything.
2. **A UI is never a rules engine.** It does not evaluate conditions (§09.4), it does not
   compute prices (§09.5), and it does not decide availability (§09.2).

## 09.2 Layers, and the one choke point

**Engine (`kobra-core`).** rules · state · save · the action set · projections · signals.

      │  subscribed projections + revisions, one way

**The UI host.** A registry, a dispatcher, capability scoping, the command choke point,
keyboard ownership, focus, z-index bands, and per-entry isolation. The base panels ship
with the host; a mod's entries and themes load from the mod's own tree.

The engine has no idea an interface exists, and keeps it that way: a UI is a consumer of
the five contracts and nothing more.

`ctx.post(command)` is the **only** path from any panel — base or mod — to the engine. That
single point is where a command is checked against the entry's declared scope (§09.6),
recorded for diagnostics, and delivered to the simulation. A renderer has no other route to
the engine, so it cannot bypass the choke point even by accident.

## 09.3 Projections, subscription and revisions

A **projection** is a read model of state the sim already owns. Adding one is never a
rules change; it is a new document, a schema id, and a test.

The shipped set: `status`, `roster`, `journal`, `dialogue`, `world`, `shops`, `actions`,
and — new here — `signals` (§09.4).

**Subscription.** A panel declares the projections it reads, and the worker sends only
the union of what is mounted. The page tells the worker once, and again whenever the
mounted set changes:

```ts
worker.postMessage({ type: 'subscribe', projections: ['roster', 'actions', 'signals'] });
```

Without this, every mod panel would inflate every command's payload, and a ten-panel HUD
would be paid for by players who do not have it. The set is **derivable** from a layout
document (§09.5): the first segment of every path is a projection name, so a layout's
subscription set is a scan, not a declaration.

**Revisions.** Each projection carries a monotonically increasing revision. On a command,
the worker includes only projections whose document changed, with their new revision:

```json
{ "type": "views",
  "rev":  { "roster": 42, "actions": 40 },
  "projections": { "roster": { "…": "…" }, "actions": { "…": "…" } },
  "narrated": [ { "event": "moved", "text_key": "log.move", "args": {} } ] }
```

A panel whose projection did not change is not touched, which is what makes a subscription
model pay off rather than merely save bytes.

**Failure is per projection.** An unknown projection name is refused with a reason and the
panel shows a named gap; it does not blank the HUD and it does not fail the boot
(the fail-soft posture, applied to UI).

## 09.4 Signals: gating without a second evaluator

The obvious design is for layout to carry a condition and for the shell to evaluate it.
That would be a second implementation of `02:02.7`'s condition vocabulary, in TypeScript,
drifting from the engine — the exact failure `AD-15` exists to prevent.

Instead, **content declares named signals using the condition vocabulary the engine
already evaluates**, and the engine publishes their truth values:

```json
// a presentation pack — a signal is presentation, so it never touches rules_hash
{ "id": "noir.signal.heard-rumour", "type": "signal",
  "data": { "when": { "flag_set": "heard_rumour" } } }
```

```
kobra_signals -> { "noir.signal.heard-rumour": true, "noir.signal.watch-trusts-me": false }
```

A signal's id is namespaced like any other record (`03:03.5`): it must live under its
pack's namespace, so a pack `noir.ui` declares `noir.signal.*`.

Layout then gates on a boolean:

```json
{ "element": "p", "text-key": "journal.rumour", "when-signal": "noir.signal.heard-rumour" }
```

| Property | Why |
|---|---|
| One vocabulary | Quests, dialogue and the HUD all speak `02:02.7`'s conditions. A modder learns it once. |
| One evaluator | The engine. The shell cannot disagree with a quest about what `standing >= 10` means, because the shell never evaluates it. |
| Validatable | A `signal` is a content record, so the one validator (`03:03.9`) checks its shape and its references at publish and at load. |
| Save-neutral | `signal` is a **presentation** record type (`AD-29`): it changes `presentation_hash`, never `rules_hash`, so a HUD's gating logic cannot invalidate a save. |
| Bounded | Signals are declared, not authored ad hoc. A mod cannot invent a predicate at runtime, which is what keeps this from becoming an expression language by stealth. |

A signal is a *display* gate. It is never a resolution input: an `effect` cannot read one,
and a mod that wants to change what happens in the world writes a quest or a Tier-2 hook.

## 09.5 The layout language

Layout is a **JSON document**, not an HTML fragment, for three reasons: it has a schema, so
the validator can check it; interpolated values always become `textContent`, so there is no
injection surface; and a bad node disables that node with a reason instead of taking down
the panel.

```json
{
  "schema": "kobra.ui-layout/1",
  "api": "kobra.ui/v1",
  "root": {
    "element": "section",
    "class": "noir-sheet",
    "children": [
      { "element": "h2", "text-key": "menu.character" },
      { "element": "p", "text": "{roster.leader.name_key} — Fortune {roster.leader.fortune}" },
      { "element": "p", "text-key": "journal.rumour", "when-signal": "noir.signal.heard-rumour" },
      { "element": "ul", "each": "roster.characters", "as": "c",
        "children": [ { "element": "li", "text": "{c.name_key}  {c.damage}/{c.max_damage}" } ] },
      { "element": "button", "action": "talk", "from": "actions.talk" }
    ]
  }
}
```

### The vocabulary, and nothing else

| Key | Meaning |
|---|---|
| `element` | The tag to create. Required on every node. |
| `class`, `id`, `title-key` | Presentation attributes; `title-key` is a string id |
| `text` | **Path interpolation only** — `{a.b.c}` tokens resolved against the subscribed projections, joined and written as text |
| `text-key` | A string id resolved through the tables (`AD-22`). It may be a `{path}` template — `"{c.name_key}"` resolves the path first and looks the result up — because a projection carries string **ids**, and a static key cannot render a list of records |
| `when-signal` | Show the node only when an engine-computed signal is true |
| `each`, `as` | Iterate a path; the alias is in scope for descendants |
| `action`, `from` | An **engine verb** bound to its action entry, so `enabled`, `label_key` and `reason_key` come from `kobra_actions` |
| `children` | Child nodes |

The verb `action` is one of the action names of `02:02.13` (`talk`, `wait`, `fight`,
`spend_fortune`, `continue_quest`) or a navigation verb owned by the shell (`open`,
`close`, `toggle`, `focus`) with `target` naming a slot. Navigation decides nothing and
needs no engine answer; engine verbs are always gated by the projection.

### Why paths and not expressions

A declaration whose *expressions* are code is a second evaluator: a grammar, a type model,
resource limits, per-node error reporting and a permanent set of surprising limitations —
owned forever, in exchange for letting game logic into markup where it cannot be tested the
way a renderer can.

The alternatives were: compile templates at build time (full power, but a toolchain, so
casual mods are excluded), interpret a general expression language (we own a parser and a
security boundary forever), or have no expressions at all.

**Decision: no expressions.** Two primitives cover what a layout actually needs — a **path**
for display, and a **signal** for gating (§09.4) — and anything beyond them is a renderer
(§09.6). The path design also makes §09.3 free: a path's first segment *is* its projection,
so subscription and dependency tracking are derived rather than maintained.

Lua is the engine's script tier (`AD-39`) and it is deliberately **not** the layout language:
the layout vocabulary stays declarative so a layout can be validated, diffed and shipped by
someone who is not writing code, and so the one implementation of a condition remains the
engine's (`02:02.7`).

### What layout cannot express, and what to do instead

| Wanted | Where it goes |
|---|---|
| Arithmetic, string formatting, sorting, grouping | A renderer (`09.6`) |
| Any rule: a price, a distance, "can I afford it" | Nowhere in the UI — read it from a projection |
| A new condition | A `signal` record, or an engine change if it is a rule |
| A new HUD element with real behaviour | A renderer |

If the same primitive is needed by three mods, it is added to this table as a named,
tested binder operation — deliberately, in the engine's vocabulary where possible. It is
never added as an escape hatch into expressions.

### Validation

Shape and vocabulary are checked per node. Where the check runs, and why:

- **At mount, in the shell**: every node, fail-soft. An unknown key, an unbound path, a
  missing `element`, a verb outside the vocabulary or a signal nobody declares disables
  **that node** with a reason reported to the dev sidebar. One bad node never blanks a
  panel.
- **At publish, in the editor**: the same checks as errors, plus cross-references — every
  `when-signal` resolves to a declared signal, every `text-key` exists, every `action` is a
  known action name, every `renderer`/`layout` path exists in the mod's own tree and is
  declared in `overrides` (§03.9 rule 7).
- **In the engine**, where the file list is known: a `check_ui` pass over the layout
  documents the caller supplies, exactly as `check_overrides` takes the asset array. The
  engine cannot enumerate a mod's tree by itself, so this is publish-time and loader work
  rather than a runtime
  claim (§03.9, "Where the file list exists").

## 09.6 The renderer contract

**Partly retired by `AD-42`.** The contract below was written for a renderer that ran as
JavaScript in the page: a TypeScript interface, light DOM, CSS custom properties and an
iframe transport seam. The interface tier is re-derived natively, so *the mechanism is not
decided* — but the decisions it encoded are, and they bind whatever replaces it:

- **A renderer receives a capability object and nothing else.** No handle to the simulation,
  no direct file or network access. What it can see is in §09.3 (the projections it
  subscribed to); what it can do is `post`.
- **`post` is scoped and mediated.** The scope is the `commands` list the entry declared in
  the mod index (§09.7), validated against the closed command vocabulary at load, so a typo
  is a load error rather than a runtime refusal. **The default is read-only**: a HUD that
  declares no scope cannot mutate anything.
- **String lookup is an id lookup.** A renderer asks for the translation of a string id
  (`AD-22`); it never formats a player-facing string itself.
- **The engine answers questions about actions.** A renderer asks for an action's view —
  legal or not, its label and its reason (`02:02.13`) — and never re-derives it.
- **Theme is a token set, not a stylesheet language.** A mod ships values for named tokens;
  the **z-index bands** (`--z-hud`, `--z-panel`, `--z-modal`, `--z-dev`) are part of the
  contract, because without bands every mod invents a number and the last one loaded wins.
- **Key bindings are claimed, and a conflict is reported.** `onKey`-style registration
  answers whether the binding was taken; arbitration is the host's.
- **A renderer cannot delay the game.** It mounts after the interface paints (`AD-25`), and
  its failure never fails a boot.

**Styling and containment** — light DOM versus a shadow root, a stylesheet the shell links,
and a sandboxed-frame transport — are properties of the retired mechanism. The *isolation
requirement* survives: one entry's failure disables that entry, and one entry cannot read
another's host subtree.

## 09.7 The mod `ui` block

A UI pack is an ordinary mod (`03:03.6`): the mod's index, which the engine reads, gains
one block:

```json
{
  "schema": "kobra.mod-index/1",
  "id": "noir-hud",
  "name": "Noir HUD",
  "mod_api": "kobra.mod/v1",
  "engine_min": "0.3.0",
  "packs": ["data/packs/noir-signals.pack.json"],
  "ui": {
    "api": "kobra.ui/v1",
    "theme": "ui/noir.css",
    "entries": [
      { "id": "character", "mount": "panel.character", "projection": "roster",
        "layout": "ui/character.layout.json", "renderer": "ui/character.lua",
        "commands": ["advance_trait", "equip"], "priority": 10 },
      { "id": "noir-feed", "mount": "hud.right", "projection": "journal",
        "layout": "ui/feed.layout.json" }
    ]
  },
  "overrides": ["ui/character.layout.json", "ui/noir.css"],
  "notes": "Replaces the character sheet and adds a right-hand feed."
}
```

- **Every UI file is declared in `overrides`.** That is not bookkeeping: the loader resolves
  from that list, so an undeclared file is silently never applied — the worst failure mode a
  mod system can have (`03:03.9` rule 7).
- **`layout` alone is Tier 1** (data: no code, no trust needed). **`layout` + `renderer`** is
  Tier 2 — Lua (`AD-39`), scoped, and removable without touching a save (`AD-40`).
- **`mount`** names a slot. Base slots: `panel.journal`, `panel.character`, `panel.shop`,
  `panel.dialogue`, `hud.top`, `hud.left`, `hud.center`, `hud.right`, `hud.bottom`.
- **`projection`** is what the entry subscribes to; a layout's paths must stay within it.
- **`theme`** is a token set, and the theming contract is those tokens plus the
  **z-index bands** (`--z-hud`, `--z-panel`, `--z-modal`, `--z-dev`). Without bands, every
  mod invents a number and the last one loaded wins — the bug `01:01.13` already records.
- **`api`** is the UI version the pack targets. A pack declaring a major the host does not
  speak is **disabled with a reason**, per-mod and fail-soft.

## 09.8 Load sequence and versioning

UI loads **after the first frame**, so a mod cannot delay the game (`AD-25`): the host
resolves the base interface, reads which mods are enabled and their indices, validates each
`ui` block, extends the subscription set, and mounts. A UI failure never fails a boot.

```
base interface mounts  →  enabled mods + indices (content report)
                   →  validate each ui block (fail-soft per mod)
                   →  extend the subscription, request projections
                   →  mount entries by slot and priority
```

The **UI API is permanent** from the moment packs ship, so it is versioned like the script
API (`AD-8`): `kobra.ui/v1` covers the action set, the command vocabulary, the layout schema
and the capability object. Projection documents carry their own `kobra.projection/<name>/1`
ids. A change that breaks either is a major bump, a new
version string, and a compatibility note — not an edit.

## 09.9 UI state

Panel open/closed, the active tab, HUD layout, a chosen theme: **never the save**. UI state
is a function of the player's screen, not of the world, and a save that depended on it would
make `AD-21`'s replay oracle depend on the interface.

**Where it persists is not decided.** The retired runtime put it in the launcher's config
store under a namespaced key per mod (`ui.<mod-id>.<key>`), which is gone with the launcher
(`AD-42`). A native host will own settings *and* the persistence is the host's business —
the engine neither reads nor writes UI state, and absent a store the interface uses
defaults. Corrupt state is ignored. Never a boot failure.

What binds regardless of where it lands: UI state is outside the save, namespaced per mod so
one pack cannot see another's, and its absence is never an error.

## 09.10 Determinism and save-neutrality

| Claim | Mechanism |
|---|---|
| A UI pack cannot invalidate a save | UI packs are `kind: "presentation"` (`AD-29`); `signal` and the UI's records are presentation types, so they move `presentation_hash`, never `rules_hash` |
| A save loads without its UI pack | Nothing in the envelope references a panel, a layout or a signal |
| A UI cannot change the world except by playing it | The only mutation path is `ctx.post` → the command stream, which is the replay log |
| A UI cannot break the oracle | It consumes no RNG and no wall clock; the engine's clock is game state and advances only on a command |
| A replay is independent of the interface | Commands are the log; what was open is not in it |

The honest residual risk, in the same form as `03:03.10`: a renderer is **code the engine
runs in its own process**, so it can do what code can do. The retired runtime's version of
that risk was same-origin JavaScript reading the page and the launcher's session; the native
version is the script and plugin tiers of `AD-39` and `AD-40`, and it is recorded there —
a tier-2 script is sandboxed and scoped, a tier-3 plugin is trusted, in-process code whose
hashes go into the save. The mitigations in both cases are the capability object, the
declared command scope, per-entry isolation, and a mod manager that says plainly what a pack
declares.

## 09.11 Diagnostics and gates

The development surface reports what the interface is running: every mounted entry, its mod,
its slot, its projection and its revision; every renderer's declared API and command scope;
and every unbound path, unknown verb, undeclared signal and refused command as its own code.

**Not built.** The list below is the acceptance list for the interface tier when it is
re-derived (`AD-42`). The implementations it was met by were the retired runtime's, and they
were removed with it. The engine-side half already survives: the signal and save-neutrality
checks run in a game's conformance suite — the first game's is at
`../worldspiracy/tests/replay.rs`.

| Gate | Proves |
|---|---|
| **Layout binder** | Every vocabulary key renders, paths resolve, `when-signal` hides and shows, a bad node disables itself with a reason |
| **Subscription** | An entry receives only its declared projections; an unchanged projection is not re-sent and does not re-render |
| **Signals** | A signal's value is the engine's evaluation of `02:02.7`'s condition, and a `signal` record does not move `rules_hash` |
| **Capability scope** | A read-only entry's `post` is refused; a scoped entry can send exactly its declared commands |
| **Fail-soft** | A broken pack disables only itself, and the base interface still mounts |
| **Save-neutrality** | A save taken with a UI pack loads without it, and `content_hash` is unchanged |
| **Dogfood** | The base panels register through the same registry and capability object a mod uses |

The last row is the point of the whole design: **the base UI is the first UI pack.** If the
shipped panels were special-cased, the public contract would be untested by definition and
would rot before a modder ever found out.

Two additions to the vocabulary above came out of building the base pack against it, and are
tested rather than implied: a **`text-key` may be a `{path}` template** (`"{c.name_key}"`
resolves to a string id, then translates), because a projection carries string ids and a
static key cannot render a list of records; and an entry whose layout binds an action or
gates on a signal **also subscribes** to `actions` and `signals`, because a `when-signal`
gate the subscription does not cover would never update.
