# `specs/` — sources of truth

| File | What it is |
|---|---|
| `4c_system.md` | The 4C System (Libre Edition) rules text. Given, unmodified. |
| `4c_system_master_tables.csv` | **Authoritative** Master Tables: both ladders, colours, canonical bucket order. This is what the engine's table generator consumes. |
| `4c_system_master_tables.as-exported.csv` | The original text export, **kept unmodified as evidence**. It is corrupt (see below) and must not be used by code. |
| `verify_master_tables.py` | Property test over the authoritative CSV. Stdlib only. |

```sh
python3 specs/verify_master_tables.py        # 0 = every invariant holds
```

## Why there are two master-table files

The as-exported CSV cannot be used, in three independent ways.

**1. Two of its three blocks contain no colours.** The file holds three tables. The
first two ("Basic 4C System Master Table", "Advanced 4C System Master Table") have
canonical headers but every data cell is the bucket label repeated (`00-04`,
`05-09`, …). In the source document the colours of those tables live in **cell
shading**, and the text-only export dropped it. Only the third block, "Basic Master
Table (Colors)", carries colours.

**2. Its only colour block has a scrambled header.** In that block the data columns
are in canonical `d%` order but the header is permuted. The mismatch is confined to
ten columns:

| Canonical bucket | Header position | Cycle |
|---|---|---|
| `00-04`, `05-09`, `15-19` | 2, 4, 1 | 3-cycle |
| `10-14`, `20-24` | 5, 3 | 2-cycle |
| `80-84`, `85-89`, `90-93` | 19, 17, 18 | 3-cycle |

Every other column matches. A reader who trusts the header gets a scrambled table.

**3. Its colour data is wrong in 22 cells.** No column permutation repairs them. The
defects are adjacent pair-swaps concentrated in the top rows; the most consequential
is that the export slid the Black band rightward at high rank, whereas the source
keeps a **Black `00-04` cell even at Rank Value 1000** — an irreducible 5 % failure
band. That band is a real design property of how the game feels, and the export
quietly removed it.

**The Advanced colour table does not exist in the export at all.** It has to come
from the source.

## Provenance

The authoritative values were taken from the Libre Edition *Master Tables*
document, in which every colour cell carries the colour **twice**: as cell text
(`Blck`/`Red`/`Blue`/`Yel`) and as cell shading (`#000000`/`#800000`/`#0000FF`/
`#FFFF00`). The two encodings agree cell-for-cell, and the OCR of the same document
agrees with both, so the transcription is cross-validated rather than inferred.

- Source: Internet Archive item `4cSystemSuperheroRoleplayinglibreEdition`, file
  `MasterTables.docx` (and `MasterTables_djvu.txt`).

**Status: transcribed, mechanically verified, awaiting one human visual diff**
against the source document. That review is the last step of R1, and it is what the
golden snapshot in the engine's table generator pins.

## What the property test does and does not prove

`verify_master_tables.py` asserts, on both ladders:

| # | Invariant |
|---|---|
| V1 | Two tables; the canonical bucket list; colours in `{Blck,Red,Blue,Yel}` |
| V2 | The `d%` buckets partition `0..99` exactly — no gap, no overlap (Basic `18×5 + 4+3+2+1 = 100`; Advanced `(1+2+3+4) + 16×5 + (4+3+2+1) = 100`) |
| V3 | The Rank Value bands are contiguous and complete; only the top band is open-ended |
| V4 | Rows are non-decreasing left to right |
| V5 | Columns are non-decreasing as the Rank Value rises |
| V6 | The lowest bucket is `Blck` in every row — the irreducible failure band |
| V7 | Each row is `Blck*`, `Red*`, `Blue*`, `Yel*` with no colour re-entering |

**They do not prove the table is right, and it is important not to believe they do.**
Measured against the as-exported block, V4/V5 flag **8 of the 22** corrupt cells; the
other **14 are masked**, because a pair-swap inside an otherwise monotone run stays
monotone. So:

- V1–V7 are a **regression guard**: they make it impossible to re-introduce a
  transcription slip of the kind that is visible in the sequence.
- The **dual-encoded source transcription plus the reviewed golden snapshot** is what
  actually establishes correctness.
- Any future disagreement between this file and the source is resolved by a human
  looking at `MasterTables.docx`, not by a test.

## The two ladders are not nested

This matters for the engine, not just for the data:

- Basic has a row that is exactly `1000`.
- Advanced has `1000-1499`, and splits Basic's `150-999` into `150-249`, `250-499`,
  `500-999`.

So the same Rank Value resolves differently under the two ladders, and Basic has no
row at all for `1001..9999` (the top row clamps). The chosen ladder is therefore part
of a save's identity, not a display setting.

## Correcting this data again

1. Change `4c_system_master_tables.csv`.
2. Run `python3 specs/verify_master_tables.py`.
3. Diff against `MasterTables.docx` cell by cell — the tables below are in the same
   layout as the source, so this is a visual comparison.

For reference, the authoritative tables in compact form (`K`=Blck, `R`=Red,
`B`=Blue, `Y`=Yel), Basic buckets
`00-04 05-09 10-14 15-19 20-24 25-29 30-34 35-39 40-44 45-49 50-54 55-59 60-64 65-69 70-74 75-79 80-84 85-89 90-93 94-96 97-98 99`:

```
1000      KRRRRRRBBBBBBBBYYYYYYY
150-999   KKRRRRRRBBBBBBBBYYYYYY
100-149   KKKRRRRRRBBBBBBBBYYYYY
75-99     KKKKRRRRRRBBBBBBBYYYYY
50-74     KKKKKRRRRRRBBBBBBBYYYY
40-49     KKKKKKRRRRRRBBBBBBYYYY
30-39     KKKKKKKRRRRRRBBBBBBYYY
20-29     KKKKKKKKRRRRRRBBBBBYYY
10-19     KKKKKKKKKRRRRRRBBBBBYY
6-9       KKKKKKKKKKRRRRRRBBBBYY
3-5       KKKKKKKKKKKRRRRRRBBBBY
1-2       KKKKKKKKKKKKRRRRRRBBBY
0         KKKKKKKKKKKKKRRRRRRBBY
```

Advanced buckets
`00 01-02 03-05 06-09 10-14 15-19 20-24 25-29 30-34 35-39 40-44 45-49 50-54 55-59 60-64 65-69 70-74 75-79 80-84 85-89 90-93 94-96 97-98 99`:

```
5000+     KRRRRRBBBBBBBBYYYYYYYYYY
2500-4999 KRRRRRRBBBBBBBBYYYYYYYYY
1500-2499 KRRRRRRRBBBBBBBBYYYYYYYY
1000-1499 KRRRRRRRRBBBBBBBBYYYYYYY
500-999   KKRRRRRRRBBBBBBBBYYYYYYY
250-499   KKKRRRRRRRBBBBBBBBYYYYYY
150-249   KKKKRRRRRRBBBBBBBBYYYYYY
100-149   KKKKKRRRRRRBBBBBBBBYYYYY
75-99     KKKKKKRRRRRRBBBBBBBYYYYY
50-74     KKKKKKKRRRRRRBBBBBBBYYYY
40-49     KKKKKKKKRRRRRRBBBBBBYYYY
30-39     KKKKKKKKKRRRRRRBBBBBBYYY
20-29     KKKKKKKKKKRRRRRRBBBBBYYY
10-19     KKKKKKKKKKKRRRRRRBBBBBYY
6-9       KKKKKKKKKKKKRRRRRRBBBBYY
3-5       KKKKKKKKKKKKKRRRRRRBBBBY
1-2       KKKKKKKKKKKKKKRRRRRRBBBY
0         KKKKKKKKKKKKKKKRRRRRRBBY
```
