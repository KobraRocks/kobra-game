#!/usr/bin/env bash
#
# Sync the published schemas into every vendored copy.
#
# architecture/schemas/ is the published set (FS Appendix B) and the only place a
# schema is edited. Go's //go:embed cannot reach outside its own package, and the
# packager ships the set to publishers, so identical bytes have to exist in
# several places. Keeping those in step by hand is the footgun this script
# removes: edit architecture/schemas/, run `make sync-schemas`, done.
#
# It is deliberately not the only guard. The drift tests
# (TestSchemaCopiesMatchThePublishedSet in the launcher,
# TestEmbeddedSchemasMatchArchitecture in the packager) still fail the build if
# someone forgets to run this, because a script nobody runs is not a guard.
#
# Usage:
#   scripts/sync-schemas.sh           copy the published set over every copy
#   scripts/sync-schemas.sh --check   report drift, change nothing, exit non-zero
#
# Exit codes: 0 clean or synced, 1 drift found under --check or a broken layout,
# 2 usage.

set -euo pipefail

check=0
case "${1:-}" in
  --check) check=1 ;;
  "") ;;
  *)
    echo "usage: $(basename "$0") [--check]" >&2
    exit 2
    ;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$repo_root/architecture/schemas"

if [ ! -d "$src" ]; then
  echo "sync-schemas: $src is missing; run this from a full checkout" >&2
  exit 1
fi

# Full mirrors hold every published file, no more and no less: an extra file here
# is drift, and so is a missing one.
full_mirrors=(
  "$repo_root/launcher/schemas"
  "$repo_root/packaging/internal/pack/schemas"
)

# Subset mirrors are //go:embed directories that carry only the files their
# package needs. A name is synced only when it is already present, so widening an
# embedded set stays a deliberate edit rather than a side effect of publishing.
subset_mirrors=(
  "$repo_root/launcher/internal/config/schema"
  "$repo_root/launcher/internal/server/testschema"
)

# Copies that live beside the code consuming them rather than in a schema
# directory. These are listed one by one on purpose: a glob over launcher/ would
# pick up unrelated JSON later and this script would delete it.
single_files=(
  "port-deny-list.json:launcher/port-deny-list.json"
)

published=()
for f in "$src"/*.json; do
  [ -e "$f" ] || continue
  published+=("$(basename "$f")")
done

if [ "${#published[@]}" -eq 0 ]; then
  echo "sync-schemas: no schemas found in $src" >&2
  exit 1
fi

is_published() {
  local name="$1" candidate
  for candidate in "${published[@]}"; do
    [ "$candidate" = "$name" ] && return 0
  done
  return 1
}

changed=0
drift=0

# sync_file FROM TO — make TO byte-identical to FROM.
sync_file() {
  local from="$1" to="$2"
  if [ -f "$to" ] && cmp -s "$from" "$to"; then
    return 0
  fi
  if [ "$check" -eq 1 ]; then
    echo "drift: ${to#"$repo_root"/}" >&2
    drift=$((drift + 1))
    return 0
  fi
  cp -f "$from" "$to"
  echo "updated ${to#"$repo_root"/}"
  changed=$((changed + 1))
}

# drop_file PATH — a copy with no published counterpart is stale.
drop_file() {
  local path="$1"
  if [ "$check" -eq 1 ]; then
    echo "drift: ${path#"$repo_root"/} has no counterpart in architecture/schemas" >&2
    drift=$((drift + 1))
    return 0
  fi
  rm -f "$path"
  echo "removed ${path#"$repo_root"/}"
  changed=$((changed + 1))
}

for dir in "${full_mirrors[@]}"; do
  if [ ! -d "$dir" ]; then
    echo "sync-schemas: $dir is missing; it is a declared mirror" >&2
    exit 1
  fi
  for name in "${published[@]}"; do
    sync_file "$src/$name" "$dir/$name"
  done
  for existing in "$dir"/*.json; do
    [ -e "$existing" ] || continue
    if ! is_published "$(basename "$existing")"; then
      drop_file "$existing"
    fi
  done
done

for dir in "${subset_mirrors[@]}"; do
  if [ ! -d "$dir" ]; then
    echo "sync-schemas: $dir is missing; it is a declared mirror" >&2
    exit 1
  fi
  for existing in "$dir"/*.json; do
    [ -e "$existing" ] || continue
    name="$(basename "$existing")"
    if is_published "$name"; then
      sync_file "$src/$name" "$existing"
    else
      drop_file "$existing"
    fi
  done
done

for pair in "${single_files[@]}"; do
  name="${pair%%:*}"
  rel="${pair#*:}"
  if [ ! -f "$src/$name" ]; then
    echo "sync-schemas: architecture/schemas/$name is missing but $rel expects it" >&2
    exit 1
  fi
  if [ ! -d "$repo_root/$(dirname "$rel")" ]; then
    echo "sync-schemas: $rel is declared but its directory is missing" >&2
    exit 1
  fi
  sync_file "$src/$name" "$repo_root/$rel"
done

if [ "$check" -eq 1 ]; then
  if [ "$drift" -gt 0 ]; then
    echo "sync-schemas: $drift file(s) out of sync; run 'make sync-schemas'" >&2
    exit 1
  fi
  echo "schemas: all copies match architecture/schemas"
  exit 0
fi

if [ "$changed" -eq 0 ]; then
  echo "schemas: already in sync"
else
  echo "schemas: $changed file(s) updated from architecture/schemas"
fi
