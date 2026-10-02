#!/usr/bin/env python3
"""Verify the authoritative 4C master tables.

Standard library only, so it runs anywhere the game's build runs.

The Master Table is the foundation every roll in the game resolves through, and
its colour data has already been corrupted once (see README.md). This script is
the property test that makes that failure mode impossible to repeat silently:

  V1  structure         two tables, correct bucket counts, colours in the domain
  V2  bucket coverage   the d% buckets partition 0..99 exactly, no gap, no overlap
  V3  band coverage     the Rank Value bands are contiguous and complete
  V4  row monotonic     colours never decrease left to right (Blck<Red<Blue<Yel)
  V5  column monotonic  colours never decrease as the Rank Value rises
  V6  irreducible band  the lowest bucket is Blck in every row
  V7  band shape        each row is Blck*, Red*, Blue*, Yel* with no re-entry

V4 and V5 together catch every one of the 22 cells the as-exported CSV got wrong.

Usage:  python3 verify_master_tables.py [path-to-csv]
Exit:   0 when every invariant holds, 1 otherwise (with the failing cell named).
"""

from __future__ import annotations

import csv
import sys
from dataclasses import dataclass, field
from pathlib import Path

COLOURS = ["Blck", "Red", "Blue", "Yel"]
RANK = {c: i for i, c in enumerate(COLOURS)}

# The bands each ladder must declare, ascending. Only the last band of a ladder
# may be open-ended ("5000+"); every other band is inclusive on both ends.
BASIC_BANDS = [
    "0", "1-2", "3-5", "6-9", "10-19", "20-29", "30-39", "40-49", "50-74",
    "75-99", "100-149", "150-999", "1000",
]
ADVANCED_BANDS = [
    "0", "1-2", "3-5", "6-9", "10-19", "20-29", "30-39", "40-49", "50-74",
    "75-99", "100-149", "150-249", "250-499", "500-999", "1000-1499",
    "1500-2499", "2500-4999", "5000+",
]
EXPECTED = {"Basic": (BASIC_BANDS, 22), "Advanced": (ADVANCED_BANDS, 24)}

# The canonical d% bucket list per ladder. These are the buckets the source
# document prints, in ascending order; the irregular tail (90-93, 94-96, 97-98,
# 99) is intentional finer granularity, not a gap.
BASIC_BUCKETS = [
    "00-04", "05-09", "10-14", "15-19", "20-24", "25-29", "30-34", "35-39",
    "40-44", "45-49", "50-54", "55-59", "60-64", "65-69", "70-74", "75-79",
    "80-84", "85-89", "90-93", "94-96", "97-98", "99",
]
ADVANCED_BUCKETS = [
    "00", "01-02", "03-05", "06-09", "10-14", "15-19", "20-24", "25-29",
    "30-34", "35-39", "40-44", "45-49", "50-54", "55-59", "60-64", "65-69",
    "70-74", "75-79", "80-84", "85-89", "90-93", "94-96", "97-98", "99",
]
EXPECTED_BUCKETS = {"Basic": BASIC_BUCKETS, "Advanced": ADVANCED_BUCKETS}


class Failure(Exception):
    pass


def parse_band(label: str) -> tuple[int, int | None]:
    """"1000" -> (1000, 1000); "00-04" -> (0, 4); "5000+" -> (5000, None)."""
    if label.endswith("+"):
        return int(label[:-1]), None
    if "-" in label:
        lo, hi = label.split("-", 1)
        return int(lo), int(hi)
    return int(label), int(label)


@dataclass
class Table:
    name: str
    buckets: list[str] = field(default_factory=list)
    order: list[str] = field(default_factory=list)  # bands, as written (descending)
    data: dict[str, list[str]] = field(default_factory=dict)

    @property
    def ascending(self) -> list[str]:
        return list(reversed(self.order))

    def colours(self, band: str) -> list[str]:
        return self.data[band]


def load(path: Path) -> dict[str, Table]:
    tables: dict[str, Table] = {}
    with path.open(newline="") as f:
        for lineno, row in enumerate(csv.reader(f), start=1):
            if not row or not any(cell.strip() for cell in row):
                continue
            name = row[0].strip()
            if name not in EXPECTED:
                raise Failure(f"line {lineno}: unknown table {name!r}")
            table = tables.setdefault(name, Table(name))
            label = row[1].strip()
            if label == "Rank Value":
                table.buckets = [cell.strip() for cell in row[2:]]
                continue
            if not table.buckets:
                raise Failure(f"line {lineno}: data row before the bucket header")
            if len(row) != len(table.buckets) + 2:
                raise Failure(
                    f"line {lineno}: {name}/{label} has {len(row) - 2} cells, "
                    f"the header declares {len(table.buckets)}"
                )
            table.order.append(label)
            table.data[label] = [cell.strip() for cell in row[2:]]
    missing = [n for n in EXPECTED if n not in tables]
    if missing:
        raise Failure(f"missing table(s): {missing}")
    return tables


def check_structure(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        bands, want_count = EXPECTED[name]
        want_buckets = EXPECTED_BUCKETS[name]
        if table.buckets != want_buckets:
            raise Failure(
                f"{name}: bucket header is not the canonical list\n"
                f"      got  {table.buckets}\n      want {want_buckets}"
            )
        if len(table.buckets) != want_count:
            raise Failure(f"{name}: {len(table.buckets)} buckets, expected {want_count}")
        if table.order != list(reversed(bands)):
            raise Failure(f"{name}: rows are not the bands in descending rank order")
        for label, cells in table.data.items():
            bad = sorted({c for c in cells if c not in RANK})
            if bad:
                raise Failure(f"{name}/{label}: unknown colour(s) {bad}")
    return []


def check_buckets(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        covered: dict[int, str] = {}
        for b in table.buckets:
            lo, hi = parse_band(b)
            if hi is None:
                raise Failure(f"{name}: d% bucket {b!r} must be bounded")
            for v in range(lo, hi + 1):
                if v in covered:
                    raise Failure(f"{name}: {v} covered by {covered[v]!r} and {b!r}")
                covered[v] = b
        gap = sorted(set(range(100)) - set(covered))
        if gap:
            raise Failure(f"{name}: d% values not covered: {gap}")
    return []


def check_bands(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        prev_hi = -1
        for i, label in enumerate(table.ascending):
            lo, hi = parse_band(label)
            if i == 0 and lo != 0:
                raise Failure(f"{name}: first band {label!r} must start at 0")
            if lo != prev_hi + 1:
                raise Failure(f"{name}: band {label!r} starts at {lo}, expected {prev_hi + 1}")
            if hi is None:
                if i != len(table.ascending) - 1:
                    raise Failure(f"{name}: only the last band may be open-ended")
            elif hi < lo:
                raise Failure(f"{name}: band {label!r} is inverted")
            else:
                prev_hi = hi
    return []


def check_monotonic(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        for label in table.ascending:  # V4: left to right
            cells = table.colours(label)
            for i in range(1, len(cells)):
                if RANK[cells[i]] < RANK[cells[i - 1]]:
                    raise Failure(
                        f"{name}/{label}: colour decreases at {table.buckets[i]!r} "
                        f"({cells[i - 1]} -> {cells[i]})"
                    )
        for j, bucket in enumerate(table.buckets):  # V5: as the rank rises
            for i in range(1, len(table.ascending)):
                lower = table.colours(table.ascending[i - 1])[j]
                upper = table.colours(table.ascending[i])[j]
                if RANK[upper] < RANK[lower]:
                    raise Failure(
                        f"{name}/{bucket}: colour decreases from row "
                        f"{table.ascending[i - 1]!r} ({lower}) to "
                        f"{table.ascending[i]!r} ({upper})"
                    )
    return []


def check_irreducible_band(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        lowest = table.buckets[0]
        for label, cells in table.data.items():
            if cells[0] != "Blck":
                raise Failure(
                    f"{name}/{label}: {lowest!r} is {cells[0]}, expected Blck "
                    f"(the irreducible failure band)"
                )
    return ["the lowest d% bucket is Blck in every row of both tables"]


def check_band_shape(tables: dict[str, Table]) -> list[str]:
    for name, table in tables.items():
        for label, cells in table.data.items():
            seen: set[int] = set()
            last = -1
            for c in cells:
                r = RANK[c]
                if r < last:
                    raise Failure(f"{name}/{label}: {c} appears after a higher colour")
                if r != last:
                    if r in seen:
                        raise Failure(f"{name}/{label}: {c} appears in two separate runs")
                    seen.add(r)
                    last = r
    return ["every row is Blck*, Red*, Blue*, Yel* with no re-entry"]


CHECKS = [
    ("V1 structure", check_structure),
    ("V2 bucket coverage", check_buckets),
    ("V3 band coverage", check_bands),
    ("V4/V5 monotonicity", check_monotonic),
    ("V6 irreducible band", check_irreducible_band),
    ("V7 band shape", check_band_shape),
]


def main(argv: list[str]) -> int:
    path = Path(argv[1]) if len(argv) > 1 else (
        Path(__file__).with_name("4c_system_master_tables.csv")
    )
    if not path.is_file():
        print(f"FAIL: {path} not found", file=sys.stderr)
        return 1
    try:
        tables = load(path)
        for name, fn in CHECKS:
            for note in fn(tables):
                print(f"  {name}: {note}")
            print(f"ok {name}")
    except Failure as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        return 1

    rows = sum(len(t.data) for t in tables.values())
    cells = sum(len(c) for t in tables.values() for c in t.data.values())
    print(f"\nPASS: {path.name} — {len(tables)} tables, {rows} rank rows, {cells} colour cells")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
