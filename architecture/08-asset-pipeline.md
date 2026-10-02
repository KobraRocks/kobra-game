# 08 — 3D asset pipeline contract

This document is **normative for anyone who produces art for Worldspiracy**, including
contractors. It exists because of `AD-31`: the studio's plan is to fund 3D artists from
demo pre-sales, and the way that plan fails is not budget — it is that two artists deliver
two beautiful characters with two different rigs, two different scales and two different
animation conventions, and neither can use the other's work. The compatibility surface of
a 3D game is the **shared skeleton**, and it has to exist before the first hire.

It is enforced mechanically, not by good intentions: the import validator (§08.9) checks
every rule here at load and reports failures exactly the way content validation does
(`AD-15`), and the acceptance step is the editor (§08.10), not a render.

## 08.1 Units, scale and orientation

| Quantity | Value | Why |
|---|---|---|
| World unit | **1 metre** (glTF unit is metres) | The only sane default; conversions happen once, here |
| Sector (tile) | **1.00 m** | `AD-33`: the sector is a 1 m tile, so one Blender grid square is one sector. Granularity is fixed and not moddable |
| Legacy 4C sector | **3 tiles (3.00 m)** | The 4C System writes its tables at `1 sector ≈ 10 feet` (`4c:914`); `1 legacy sector = 3 tiles`, converted once (`02:02.6`). 10 ft is 3.048 m, rounded to 3 m so every derived distance stays an integer |
| Character height | 1.60–2.00 m for a baseline human | 4C's Growth table starts at "max 9 feet" for the lowest band (`4c:603-616`), so the cast spans a wide range; the *baseline* is one metre eighty, and a character is 2–3 tiles tall in extent and 1 tile wide |
| Grid alignment | Sector (tile) origins at integer multiples of **1.00 m**; props snap to the 1 m grid | One Blender grid square is one sector (`AD-33`), so level layout needs no custom grid scale |
| Up axis | **+Y up** | glTF convention |
| Forward axis | **−Z in the rest pose** | glTF's camera convention; one convention, never "it depends" |
| Handedness | Right-handed, metres, radians, quaternions | glTF |
| Pivot | At the feet, centred, at the sector origin | Feet-at-origin is what makes placement, cover and elevation queries trivial |

An asset that is 1.8 units tall and one that is 1.8 *metres* tall are indistinguishable in
a viewer and catastrophic in a scene. This is why scale is a validator **error**.

## 08.2 The shared humanoid rig

One skeleton for the whole humanoid cast. Everything else — every animation clip, every
piece of gear, every LOD — is authored against it.

- **Bone names are fixed.** The exact list lives in `content/rig/humanoid.bones.json` and
  is the validator's source of truth; it is a plain, conventional humanoid set
  (`hips`, `spine_01..03`, `neck`, `head`, `clavicle_l/r`, `upperarm_l/r`, `lowerarm_l/r`,
  `hand_l/r`, finger sets, `thigh_l/r`, `shin_l/r`, `foot_l/r`, `toe_l/r`).
- **≤ 80 bones target, 128 hard cap**, with **≤ 4 bone influences per vertex**. Beyond
  that, GPU skinning cost grows without a visible gain at the camera distances the game
  uses.
- **Rest pose is a relaxed T-pose or A-pose**, documented per rig, and consistent across
  the cast — a mixed rest pose changes what "retarget" means.
- **Non-humanoid creatures** may have their own rig, but must declare it in `rig` so the
  validator knows which contract to apply. They cannot share humanoid clips, and that is
  priced in rather than discovered.
- **Retargeting is the economy of the cast**: one clip library serves every humanoid,
  which is why a bespoke rig is an error and not a stylistic choice.

## 08.3 Gameplay anchors (part of the rig, not an afterthought)

These are authored once per rig and inherited by every character built on it. They are how
`AD-28`'s elevation and 4C's melee/ranged distinction reach the art.

| Anchor | Used by | Rule |
|---|---|---|
| `eye_level` | Line of sight, cover, perception (`02:02.12`) | A named bone or an explicit offset; **present on every humanoid rig** |
| `hit_zone_head`, `hit_zone_torso`, `hit_zone_limbs` | Called shots, cover, `Nail`/concuss resolution | Non-overlapping volumes; head present |
| `cover_high`, `cover_low` | Cover evaluation and cover animation | Bone markers, not guessed from the mesh |
| `slot_mainhand`, `slot_offhand`, `slot_head`, `slot_body`, `slot_back` | Equipment attachment (`AD-29`) | Empty transforms; the mesh is attached at runtime |
| `foot_l`, `foot_r` | Footstep events, ground snapping on slopes | Exposed so the animation events can reference them |
| `capsule_radius`, `capsule_height` | Movement, collision, occupancy | Declared per character, from the rig, not from the bounding box |

## 08.4 Mesh and material budget

| Tier | LOD0 | LOD1 | LOD2 | Notes |
|---|---|---|---|---|
| Player / major character | ≤ 25k tris | ≤ 8k | ≤ 2k | 2 materials maximum (body, gear) |
| Common NPC | ≤ 12k tris | ≤ 4k | ≤ 1k | 1 material preferred |
| Crowd / background | ≤ 4k tris | ≤ 1.5k | ≤ 500 | Impostor or billboard LOD2 is fine (`AD-30`) |

- **2 materials per character, maximum.** Every extra material is a draw call multiplied by
  the number of visible characters, and crowd scenes are where the frame budget goes.
- Atlas textures **per character**, not per material, to keep the batch count flat.
- LODs are **authored, not generated at import**, with the same skeleton so they retarget
  identically. LOD2 may be a billboard cutout (`AD-30` keeps billboards first-class).
- No material may require a shader the engine does not ship. New shader *features* are an
  engine change, not an asset delivery; new shader *parameters* are content.

## 08.5 Textures

- **KTX2** container, mip chains **pre-generated at authoring time** (never at runtime —
  `07:07.6`), power-of-two dimensions, ≤ 2048² for characters and ≤ 4096² for terrain.
- Colour maps are **sRGB**; normal, roughness, metallic, ambient-occlusion and mask maps are
  **linear**. Mixing them is the single most common art bug and is a validator warning.
- A BC7 (colour) / BC5 (normals) variant is produced when the adapter exposes
  `texture-compression-bc`; an uncompressed RGBA8 variant always ships as the fallback,
  because BC availability on Linux depends on the driver. The asset manifest declares both
  (`03:03.4`), so the renderer picks at load.
- Normal maps are authored in **OpenGL convention** (+Y up), which is the glTF convention.
- **PBR metallic-roughness**, glTF-standard factors; no custom BRDF. Emissive is allowed
  and is how the game's energy/elemental effects read at night.

**Procedural materials do not survive the export, and this is the number-one surprise for
anyone coming from Blender.** glTF carries PBR metallic-roughness *factors* and *textures* —
nothing else. Noise, Voronoi, gradient and colour-ramp node networks, procedural wear, and
anything driven by a shader node graph must be **baked to textures** before export, or they
simply vanish and the artist reports that the engine "lost" their material. The same applies
to Blender-only features with no glTF equivalent (subsurface, sheen, clearcoat, procedural
displacement). Bake, then export; the build's compiler cannot invent what glTF cannot carry.

## 08.6 Animation

| Rule | Value |
|---|---|
| Sampling | **30 fps, baked**, no runtime IK for shipped clips (runtime IK, if any, is a gameplay feature, not an asset feature) |
| Length budget | ≤ 5 s per clip; loop points on frame boundaries |
| Root motion | **In-place by default.** Locomotion clips carry no forward translation — movement is simulation state (`02:02.6`). Explicit `rm_` prefixed clips are the exception, and the engine decides whether to consume them |
| Clip naming | `<state>[_<variant>][_<direction>]`, lowercase snake: `idle`, `idle_combat`, `walk_fwd`, `run_fwd`, `attack_light_01`, `attack_heavy_01`, `hit_react_front`, `death_fwd`, `cast_01` |
| Required set per humanoid | `idle`, `walk_fwd`, `run_fwd`, `attack_light_01`, `hit_react_front`, `death_fwd`, `interact` — the minimum for the demo to be playable |
| Events | Named markers on the timeline (`footstep`, `hit_frame`, `release`, `vfx_spawn`) consumed by the sim as **integer events at fixed frames**, never as callbacks |
| Blending | Clips must look correct when cross-faded over 0.15–0.3 s; transitions are the engine's, so clips must not assume a hard cut |

Marker events are the boundary between animation as *art* and animation as *gameplay*: a
`hit_frame` marker is what lets a 4C attack resolve on the frame the artist intended
without the rules knowing anything about animation. Markers are declared in the clip and
carried into the content manifest by the validator.

## 08.7 Naming and packaging

- Every asset is namespaced by its pack: `wsp.character.detective.body.lod0`,
  `wsp.anim.humanoid.attack_light_01`, `noir.prop.streetlamp`.
- One `.glb` per character, containing its LODs, materials and skeleton; one `.glb` or
  shared library per animation set. Sharing a skeleton across files is normal and expected.
- **Source files accompany the delivery** (`.blend` or equivalent). The `.glb` is the
  runtime artifact; the source is the maintainable one, and a cast that cannot be edited
  is a cast that cannot be fixed.
- Textures travel beside the model under the pack's `assets/` subtree so the overlay rules
  apply unchanged (`03:03.4`).

## 08.8 Placeholder art is a pack, and the demo reuses everything else

The demo ships with placeholder or asset-store models as a **`presentation` pack**
(`AD-29`). The final cast is the same kind of pack against the same descriptors, so
replacing it:

- changes no rules record, so `rules_hash` is unchanged and **no save is invalidated**;
- changes no quest, item, power or level, so all demo content survives into the full game;
- can be applied **partially** — the party first, crowds later — because descriptors are
  per record.

This is the same mechanism the HD-2D plan would have used to ship a 3D character pack
(`AD-27`). The decision changed; the mechanism did not, which is why the demo is not
throwaway work.

## 08.9 Validation: what the importer rejects, and what it warns about

Errors block the asset. Warnings are reported and shown in the editor's asset inspector.

| Severity | Check |
|---|---|
| **Error** | Scale: character height outside 0.2–12 m, or a rig whose root is not at the feet |
| **Error** | Rig: bone names do not match the declared contract; bone count > 128; > 4 vertex influences |
| **Error** | Missing gameplay anchors (`eye_level`, capsule, required slots) |
| **Error** | Required animation set absent; duplicate clip names; a clip whose length is 0 |
| **Error** | Texture dimensions not power-of-two, or above the tier ceiling |
| **Error** | A material requiring an unknown shader feature |
| **Error** | Naming: id not namespaced under the pack, or a path outside the pack's `assets/` subtree |
| **Warning** | Colour space mismatch (an sRGB map in a linear slot or vice versa) |
| **Warning** | Triangle or material count above the tier budget |
| **Warning** | Rest pose differs from the documented one; a clip that assumes a hard cut |
| **Warning** | Missing LOD1/LOD2, or LODs that are not on the shared skeleton |
| **Warning** | Excessive bone count relative to the tier; oversized texture for its role |
| **Info** | Animation marker summary, texture memory estimate, draw-call estimate |

The draw-call and texture-memory estimates are the numbers that matter when a contractor
delivers twenty characters at once, and they are cheap to compute from the manifest.

## 08.10 Acceptance

A delivery is accepted when:

1. it imports with **no errors** and the warnings are understood and either accepted or fixed;
2. it loads in the editor, previews on the **shared skeleton**, and plays each declared clip;
3. its gameplay anchors resolve and the elevation/cover overlay (`04:04.4`) reports sensible
   values for it;
4. its animation marker events fire in a playtest encounter;
5. the perf harness reports the asset within its tier budget (`07:07.1`).

Anything else is a work-in-progress, not a delivery. That is the whole point: acceptance is
a checklist both sides can read, so art direction stays about taste and the pipeline stays
about facts.

## 08.11 The Blender pipeline

Blender is the DCC, the level-geometry editor and the headless asset compiler; it is not
the game editor (`AD-32`). Three things make that work rather than merely being a
preference.

### Version, pinned

**Blender 5.2 LTS.** The exporter script records the version it was run under, and the
validator **warns** when a delivery was produced by a different one, because glTF output and
the Python API both drift between releases. Contractors are told the version in the brief;
"it looked right in Blender" is not acceptance (`§08.10`).

### `extras` is the carrier

Blender custom properties export into glTF `extras`, so the contract's metadata crosses the
boundary inside the standard file rather than in a side format:

| Blender-side | glTF `extras` key | Consumed by |
|---|---|---|
| Custom property on the armature | `wsp_rig` (contract id, e.g. `humanoid_v1`) | Validator: which bone set to check |
| Empty named `eye_level` / `slot_mainhand` / … | `wsp_anchor` (anchor id) | Validator and runtime: gameplay anchors (`§08.3`) |
| Custom property on a mesh | `wsp_lod` (`0` \| `1` \| `2`), `wsp_layer` | Renderer: LOD selection; validator: budget check |
| Custom property on a material | `wsp_material_role` (`body` \| `gear` \| `prop`) | Validator: the two-material ceiling |
| Clip custom property | `wsp_clip` (state, variant, direction, loop, markers) | Runtime: the animation state machine (`§08.6`) |

Two of these need **small exporter work** rather than being free: glTF has no LOD concept,
so grouping is a convention the exporter must emit; and Blender timeline markers are **not**
exported by default, so clip markers must be written into `wsp_clip` by the addon. Both are
afternoons, not projects — but they are work, not configuration.

### The headless compiler

```sh
blender -b -P tools/blender/exporter.py -- --pack <pack> --out <stage>
```

It runs on a workstation, in CI, and on a contractor's machine, and it does the same five
things in the same order:

1. **Normalise** the delivery — apply transforms, fix scale and axis, enforce the rest pose,
   rename to the pack's namespace — so a near-miss is *repaired* rather than rejected.
2. **Export** glTF 2.0 binary with `extras` enabled, plus the manifest fragment.
3. **Derive** LODs that were not delivered, and convert textures to KTX2 with mip chains
   (via **KTX-Software**, a second pinned tool: Blender's exporter does not emit KTX2).
4. **Render** the thumbnail the editor and mod manager show for the asset.
5. **Report** the contract result: errors block, warnings are shown in the editor's asset
   inspector (`§08.9`).

Lightmap baking stays on a workstation. Cycles bakes headless on CPU; **EEVEE needs a GL
context**, so anything viewport-style or EEVEE-based does not belong in CI without a GPU or
a virtual framebuffer.

### The gameplay layers are proposed, not taken

A level modelled in Blender is geometry; the rules read a **quantised heightfield**
(`02:02.12`). The build derives *proposals* for sector levels and blocking heights from
meshes tagged as collision/ground, writes them into the world pack as generated candidates,
and a designer confirms or edits them in the web editor, which holds the authoritative
version. Generated proposals, authored authority: the boring part is automatic and the rules
still never read a mesh at runtime (`AD-30`).

## 08.12 What this contract deliberately does not fix

- **Art direction.** Palette, silhouette language, proportion stylisation and level of
  detail are the studio's taste, documented separately as it emerges.
- **The creature rigs.** They must declare a rig contract, but their bone sets are their own.
- **Facial animation and lipsync.** Out of scope for v1; when it arrives it becomes a
  declared add-on to the humanoid contract rather than a change to it.
- **Destruction, cloth and physics rigs.** Not in v1 (`AD-30`'s simulation is 4C-based and
  integer); adding them later is a rules-and-engine decision, not an asset one.
