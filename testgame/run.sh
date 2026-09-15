#!/usr/bin/env bash
# End-to-end drive for the testgame fixture.
#
# It answers one question: does the package this repository produces actually
# run under the launcher? Every step is a real assertion against a real server,
# because a fixture that only looks right is worse than no fixture.
#
#   ./run.sh              package, verify, launch, drive
#   ./run.sh --keep       leave the server running and print the URL
#
# The drive is modelled on launcher/../.e2e/run.sh but exercises the packaged
# fixture instead of a synthetic folder built on the spot.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
LAUNCHER_SRC="$REPO/launcher"
PACKAGING_SRC="$REPO/packaging"
PACK="$PACKAGING_SRC/kobra-pack"
OUT="$HERE/dist"
INSTALL="$OUT/install/Pkgtest"
STATE="$HERE/.state"
LOG="$STATE/launcher.log"

# The deterministic default port for com.kobra.pkgtest (base 18900, span 50).
PORT=${PORT:-18915}
# A separate port for the local update server used by phase B.
UPDATE_PORT=${UPDATE_PORT:-18999}
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

export GOMODCACHE="$REPO/.gocache/mod"
export GOCACHE="$REPO/.gocache/build"
export GOSUMDB=off
export XDG_STATE_HOME="$STATE"

pass=0; fail=0
check() {
  if [ "$2" = "$3" ]; then echo "  PASS  $1"; pass=$((pass+1));
  else echo "  FAIL  $1 (expected [$2] got [$3])"; fail=$((fail+1)); fi
}
checkcontains() {
  case "$3" in *"$2"*) echo "  PASS  $1"; pass=$((pass+1));;
  *) echo "  FAIL  $1 (expected to contain [$2] got [$3])"; fail=$((fail+1));; esac
}

cleanup() {
  [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  wait 2>/dev/null
  return 0
}
trap cleanup EXIT

echo "== 1. build the tools (the launcher VERSION must be a bare semver, §4.6) =="
rm -rf "$STATE"
mkdir -p "$STATE"
make -C "$PACKAGING_SRC" build >/dev/null 2>&1 || { echo "  FAIL  kobra-pack build"; exit 1; }
make -C "$LAUNCHER_SRC" dist-linux >/dev/null 2>&1 || { echo "  FAIL  launcher build"; exit 1; }
LV=$("$LAUNCHER_SRC/dist/linux-amd64/launcher" --version | awk '{print $2}')
check "launcher reports 0.1.0" "0.1.0" "$LV"

echo "== 2. package the fixture =="
rm -rf "$OUT"
if ! "$PACK" build --config "$HERE/pkg.toml" --platform linux-x64 \
     --out "$OUT" --install > "$STATE/pack.log" 2>&1; then
  echo "  FAIL  packager exited non-zero:"; tail -20 "$STATE/pack.log"; exit 1
fi
checkcontains "packager verified its output" "pack.verify.ok" "$(cat "$STATE/pack.log")"
check "release 1 ZIP produced" "1" "$(ls "$OUT"/pkgtest-2026.09.1-linux-x64.zip 2>/dev/null | wc -l)"
check "update archive produced" "1" "$(ls "$OUT"/pkgtest-2026.10.1-linux-x64.tar.zst 2>/dev/null | wc -l)"
check "latest.json produced" "1" "$(ls "$OUT"/latest.json 2>/dev/null | wc -l)"

echo "== 3. package structure (§2.4, §2.5, §9.1) =="
# Only data/.keep may exist as a FILE under the package's top-level data/.
# game/assets/data/ is ordinary shipped content, not the user's data directory,
# so the check is scoped to <GameName>/data/.
BAD=$(python3 - "$OUT/pkgtest-2026.09.1-linux-x64.zip" <<'PY'
import sys, zipfile
names = zipfile.ZipFile(sys.argv[1]).namelist()
bad = [n for n in names
       if n.count("/") >= 2 and n.split("/", 2)[1] == "data"
       and not n.endswith("/") and not n.endswith("/data/.keep")]
print(",".join(sorted(bad)))
PY
)
check "no saves shipped in the ZIP" "" "$BAD"
check "data/.keep present" "1" "$(unzip -l "$OUT/pkgtest-2026.09.1-linux-x64.zip" | grep -c 'data/\.keep')"
check "empty data/saves/ is a real entry" "1" "$(unzip -l "$OUT/pkgtest-2026.09.1-linux-x64.zip" | grep -c 'data/saves/$')"
# The update archive must carry no data/, no README, no LICENSES, and no
# release manifest: a manifest cannot be inside the archive it indexes
# (Packaging spec §6.4, §9.1; internal/update/verify.go rejects it explicitly).
TARLIST=$(zstd -dc "$OUT/pkgtest-2026.10.1-linux-x64.tar.zst" | tar -tf -)
for pat in '^data/' '^README' '^LICENSES' 'release\.manifest\.json'; do
  if echo "$TARLIST" | grep -qE "$pat"; then
    echo "  FAIL  update archive contains $pat"; fail=$((fail+1))
  else
    echo "  PASS  update archive excludes $pat"; pass=$((pass+1))
  fi
done
# ...but the distribution ZIP does carry it, so a human install has a release
# document to inspect.
check "release manifest ships in the ZIP" "1" \
  "$(unzip -l "$OUT/pkgtest-2026.10.1-linux-x64.zip" | grep -c 'game/release\.manifest\.json')"
# One manifest, two copies, identical bytes: what makes a single signature
# cover both the install and the download.
if unzip -p "$OUT/pkgtest-2026.10.1-linux-x64.zip" '*/game/release.manifest.json' \
   | cmp -s - "$OUT/pkgtest-2026.10.1.release.manifest.json"; then
  echo "  PASS  ZIP manifest and published manifest are byte-identical"; pass=$((pass+1))
else
  echo "  FAIL  ZIP manifest differs from the published manifest"; fail=$((fail+1))
fi
# §8.2.3.3: the launcher refuses a manifest that does not name its archive, so
# the published manifest must declare one and it must match the real bytes.
ARCHIVE_OK=$(python3 - "$OUT" <<'PY'
import hashlib, json, pathlib, sys
out = pathlib.Path(sys.argv[1])
manifest = json.loads((out / "pkgtest-2026.10.1.release.manifest.json").read_text())
archive = manifest.get("archive") or {}
target = out / archive.get("name", "")
if not archive.get("hash") or not target.is_file():
    print("missing")
else:
    digest = "sha256:" + hashlib.sha256(target.read_bytes()).hexdigest()
    print("ok" if digest == archive["hash"] and target.stat().st_size == archive["size"] else "mismatch")
PY
)
check "published manifest names its archive correctly" "ok" "$ARCHIVE_OK"
# Every archive member must be in the manifest index, or the launcher rejects
# the release with "unlisted_entry" (internal/update/verify.go).
MISSING=$(python3 - "$OUT" <<'PY'
import json, subprocess, sys, tarfile, io
out = sys.argv[1]
import pathlib
out = pathlib.Path(out)
manifest = json.loads((out / "pkgtest-2026.10.1.release.manifest.json").read_text())
indexed = {f["path"] for f in manifest["files"]}
raw = subprocess.run(["zstd", "-dc", str(out / "pkgtest-2026.10.1-linux-x64.tar.zst")],
                     capture_output=True, check=True).stdout
with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
    members = {m.name for m in tar.getmembers() if m.isfile()}
print(",".join(sorted(members - indexed)))
PY
)
check "every archive member is in the manifest index" "" "$MISSING"

echo "== 4. determinism (§5.2): two builds are byte-identical =="
sha256sum "$OUT"/*.zip "$OUT"/*.tar.zst > "$STATE/run1.sums"
"$PACK" build --config "$HERE/pkg.toml" --platform linux-x64 --out "$OUT" >/dev/null 2>&1
sha256sum "$OUT"/*.zip "$OUT"/*.tar.zst > "$STATE/run2.sums"
if diff -q "$STATE/run1.sums" "$STATE/run2.sums" >/dev/null; then
  echo "  PASS  rebuild is byte-identical"; pass=$((pass+1))
else
  echo "  FAIL  rebuild differs:"; diff "$STATE/run1.sums" "$STATE/run2.sums"; fail=$((fail+1))
fi
# Re-extract, because the second packaging run is the canonical output.
"$PACK" build --config "$HERE/pkg.toml" --platform linux-x64 --out "$OUT" --install >/dev/null 2>&1

echo "== 5. start the launcher on the installed package =="
chmod +x "$INSTALL/launcher/launcher"
"$INSTALL/launcher/launcher" --print-url --port "$PORT" > "$LOG" 2>&1 &
PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(head -1 "$LOG" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  if ! kill -0 "$PID" 2>/dev/null; then echo "  FAIL  launcher exited early:"; cat "$LOG"; exit 1; fi
  sleep 0.1
done
checkcontains "token handed over in the fragment" "#t=" "$URL"
checkcontains "served on the deterministic port" ":$PORT/" "$URL"

echo "== 6. unauthenticated surface is opaque (FR-SRV-9) =="
PROBE=$(curl -s -m 2 "http://127.0.0.1:$PORT/__kobra/probe")
checkcontains "probe app" '"app":"kobra-launcher"' "$PROBE"
checkcontains "probe game_id" '"game_id":"com.kobra.pkgtest"' "$PROBE"
checkcontains "probe release" '"release":"2026.09.1"' "$PROBE"
case "$PROBE" in *"$REPO"*) echo "  FAIL  the probe leaked a filesystem path"; fail=$((fail+1));;
  *) echo "  PASS  the probe leaks no filesystem path"; pass=$((pass+1));; esac

echo "== 7. security gates =="
check "wrong Host -> 421" "421" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 -H 'Host: evil.example' "http://127.0.0.1:$PORT/api/state")"
check "cross-origin GET -> 403" "403" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 -H "Origin: http://evil.example" "http://127.0.0.1:$PORT/api/state")"
check "no session -> 401" "401" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 "http://127.0.0.1:$PORT/api/state")"
check "traversal -> 404" "404" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 --path-as-is "http://127.0.0.1:$PORT/assets/../../launcher/launcher.config.json")"
check "data/ is not static -> 404" "404" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 "http://127.0.0.1:$PORT/data/saves/index.json")"

echo "== 8. session exchange (FR-SRV-7) =="
TOK=${URL#*#t=}
JAR="$STATE/cookies.txt"; rm -f "$JAR"
BODY=$(curl -s -m 2 -c "$JAR" -X POST -H 'Content-Type: application/json' -H "Origin: http://127.0.0.1:$PORT" \
  -d "{\"token\":\"$TOK\"}" "http://127.0.0.1:$PORT/__kobra/session")
checkcontains "session id returned" '"session_id"' "$BODY"
checkcontains "api_version 1" '"api_version":1' "$BODY"
CSRF=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$BODY")
SID=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['session_id'])" "$BODY")
check "csrf token is 43 chars" "43" "${#CSRF}"
check "bootstrap token is single-use" "401" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 -X POST -H 'Content-Type: application/json' -d "{\"token\":\"$TOK\"}" "http://127.0.0.1:$PORT/__kobra/session")"
AUTH=(-b "$JAR" -H "X-Kobra-CSRF: $CSRF" -H "Origin: http://127.0.0.1:$PORT" -H 'Content-Type: application/json')

echo "== 9. /api/state agrees with the shipped manifests (§4.6) =="
STATEJSON=$(curl -s -m 2 "${AUTH[@]}" "http://127.0.0.1:$PORT/api/state")
checkcontains "state game_id" '"game_id":"com.kobra.pkgtest"' "$STATEJSON"
checkcontains "state release" '"release":"2026.09.1"' "$STATEJSON"
checkcontains "state engine_version" '"engine_version":"0.1.0"' "$STATEJSON"
checkcontains "state save_version" '"save_version":1' "$STATEJSON"
checkcontains "state data_writable" '"data_writable":true' "$STATEJSON"
# The launcher's engine_version/save_version are compile-time constants, so this
# catches a fixture that claims a different engine or save format.
ENGINE_JSON=$(python3 -c "
import json;d=json.load(open('$INSTALL/game/engine/engine.manifest.json'))
print(d['engine_version'], d['save_version'])")
check "engine manifest agrees with /api/state" "0.1.0 1" "$ENGINE_JSON"

echo "== 10. static serving and MIME (FR-SRV-13) =="
hdr() { curl -s -D - -o /dev/null -m 2 "http://127.0.0.1:$PORT$1"; }
checkcontains "index.html is text/html" 'Content-Type: text/html' "$(hdr /index.html)"
checkcontains "shell.js is text/javascript" 'Content-Type: text/javascript' "$(hdr /shell.js)"
checkcontains "game.wasm is application/wasm" 'Content-Type: application/wasm' "$(hdr /engine/game.wasm)"
checkcontains "assets/shell.css is text/css" 'Content-Type: text/css' "$(hdr /assets/shell.css)"
checkcontains "checker.png is image/png" 'Content-Type: image/png' "$(hdr /assets/textures/checker.png)"
checkcontains "click.ogg is audio/ogg" 'Content-Type: audio/ogg' "$(hdr /assets/audio/click.ogg)"
checkcontains "box.obj uses the per-game MIME map" 'Content-Type: text/plain' "$(hdr /assets/models/box.obj)"
checkcontains "CSP is sent" 'Content-Security-Policy' "$(hdr /index.html)"
checkcontains "nosniff is sent" 'X-Content-Type-Options: nosniff' "$(hdr /index.html)"
checkcontains "engine/ is immutable" 'immutable' "$(hdr /engine/game.wasm)"
checkcontains "shell.js is no-cache" 'Cache-Control: no-cache' "$(hdr /shell.js)"
checkcontains "range request -> 206" '206 Partial Content' "$(curl -s -D - -o /dev/null -m 2 -H 'Range: bytes=0-3' "http://127.0.0.1:$PORT/shell.js")"
# The wasm the page will instantiate must really be a module, not a placeholder.
WASM_OK=$(curl -s -m 2 "http://127.0.0.1:$PORT/engine/game.wasm" | node -e "
let b=[];process.stdin.on('data',d=>b.push(d)).on('end',()=>{
  WebAssembly.instantiate(Buffer.concat(b),{}).then(({instance})=>{
    console.log(instance.exports.tick(41)===42?'valid':'wrong-export');
  }).catch(e=>console.log('invalid:'+e.message));});")
check "served wasm instantiates and computes" "valid" "$WASM_OK"

echo "== 11. save round trip, conflict and config merge =="
checkcontains "first write is revision 1" '"revision":1' "$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{"levelId":"sandbox","tick":7},"claim":true,"if_revision":0}' "http://127.0.0.1:$PORT/api/save")"
checkcontains "second write is revision 2" '"revision":2' "$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{"levelId":"roundtrip","tick":8},"if_revision":1}' "http://127.0.0.1:$PORT/api/save")"
CONF=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{},"if_revision":1}' "http://127.0.0.1:$PORT/api/save")
checkcontains "stale revision conflicts" '"error":"conflict"' "$CONF"
checkcontains "conflict names current_revision" '"current_revision":2' "$CONF"
checkcontains "save reads back" '"levelId":"roundtrip"' "$(curl -s -m 5 "${AUTH[@]}" "http://127.0.0.1:$PORT/api/data/saves/slot1")"
checkcontains "index lists the slot" '"slot":"slot1"' "$(curl -s -m 5 "${AUTH[@]}" "http://127.0.0.1:$PORT/api/data/saves")"
check "a bad slot is rejected" "400" "$(curl -s -o /dev/null -w '%{http_code}' -m 5 "${AUTH[@]}" -X POST -d '{"slot":"../evil","payload":{}}' "http://127.0.0.1:$PORT/api/save")"
check "a missing CSRF header is 403" "403" "$(curl -s -o /dev/null -w '%{http_code}' -m 5 -b "$JAR" -H "Origin: http://127.0.0.1:$PORT" -H 'Content-Type: application/json' -X POST -d '{"slot":"slot1","payload":{}}' "http://127.0.0.1:$PORT/api/save")"
printf '{"audio":{"volume":0.5},"engine_owned_key":{"keep":true}}\n' > "$INSTALL/data/config/settings.json"
curl -s -m 5 "${AUTH[@]}" -X POST -d '{"merge":{"audio":{"volume":0.8}},"if_revision":0}' "http://127.0.0.1:$PORT/api/config" >/dev/null
checkcontains "config merge preserves unknown keys" '"engine_owned_key"' "$(cat "$INSTALL/data/config/settings.json")"
checkcontains "config merge applies the change" '"volume":0.8' "$(cat "$INSTALL/data/config/settings.json")"

echo "== 12. on-disk save shape (§11.2, §11.4) =="
SAVEFILE="$INSTALL/data/saves/slot1.json"
checkcontains "save schema tag" 'kobra.save/1' "$(cat "$SAVEFILE" 2>/dev/null)"
checkcontains "save revision" '"revision":2' "$(cat "$SAVEFILE" 2>/dev/null)"
checkcontains "save checksum present" 'sha256:' "$(cat "$SAVEFILE" 2>/dev/null)"
checkcontains "payload preserved" '"levelId":"roundtrip"' "$(cat "$SAVEFILE" 2>/dev/null)"
check "backup rotation produced a .bak" "1" "$(ls "$INSTALL/data/saves/" | grep -c 'slot1\.json\.bak')"
check "revision sidecars exist" "1" "$([ "$(ls "$INSTALL/data/saves/" | grep -c 'slot1\.json\.[0-9]')" -ge 1 ] && echo 1 || echo 0)"

echo "== 13. heartbeat, diagnostics, drain (FR-SRV-18..20) =="
check "heartbeat -> 204" "204" "$(curl -s -o /dev/null -w '%{http_code}' -m 2 "${AUTH[@]}" -X POST "http://127.0.0.1:$PORT/__kobra/heartbeat")"
DIAG=$(curl -s -m 2 "${AUTH[@]}" "http://127.0.0.1:$PORT/__kobra/diagnostics")
checkcontains "diagnostics reports the port" "\"port\":$PORT" "$DIAG"
checkcontains "diagnostics reports the origin" "\"origin\":\"http://127.0.0.1:$PORT\"" "$DIAG"
case "$DIAG" in *"$REPO"*) echo "  FAIL  diagnostics leaked a filesystem path"; fail=$((fail+1));;
  *) echo "  PASS  diagnostics leaks no path"; pass=$((pass+1));; esac
check "goodbye without a CSRF header -> 403 (sendBeacon cannot work)" "403" \
  "$(curl -s -o /dev/null -w '%{http_code}' -m 2 -b "$JAR" -H "Origin: http://127.0.0.1:$PORT" -X POST "http://127.0.0.1:$PORT/__kobra/goodbye")"
check "goodbye with the header -> 204" "204" \
  "$(curl -s -o /dev/null -w '%{http_code}' -m 2 "${AUTH[@]}" -X POST "http://127.0.0.1:$PORT/__kobra/goodbye")"

if [ "$KEEP" = "1" ]; then
  echo; echo "  server left running on http://127.0.0.1:$PORT/index.html#t=$TOK"
  echo "  (--keep: the summary below is skipped and the server stays up)"
  echo "  press Ctrl-C to stop"
  wait "$PID"
  exit 0
fi

kill -TERM "$PID" 2>/dev/null
for _ in $(seq 1 100); do kill -0 "$PID" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$PID" 2>/dev/null; then
  echo "  FAIL  launcher did not exit on SIGTERM"; fail=$((fail+1))
else
  wait "$PID"; check "SIGTERM exits 0" "0" "$?"
fi
LOCK="$STATE/kobra-games/com.kobra.pkgtest/instance.lock"
if [ -f "$LOCK" ]; then echo "  FAIL  instance.lock not removed"; fail=$((fail+1));
else echo "  PASS  instance.lock removed"; pass=$((pass+1)); fi
PORTJSON="$STATE/kobra-games/com.kobra.pkgtest/port.json"
checkcontains "port.json records the origin" "http://127.0.0.1:$PORT" "$(cat "$PORTJSON" 2>/dev/null)"

echo "== 14. update discovery against the published out/ directory (§8.5) =="
# The launcher accepts http for the update base (internal/update/http.go), so
# the fixture can serve out/ over loopback instead of standing up a web server
# with TLS.
(cd "$OUT" && python3 -m http.server "$UPDATE_PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
SRV_PID=$!
sleep 0.6
python3 - "$INSTALL/launcher/launcher.config.json" "$UPDATE_PORT" <<'PY'
import json, sys
path, port = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
cfg["update"] = {"channel": "patch", "patch_base_url": f"http://127.0.0.1:{port}"}
json.dump(cfg, open(path, "w"), indent=2)
PY
"$INSTALL/launcher/launcher" --print-url --port "$PORT" > "$STATE/update.log" 2>&1 &
PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(head -1 "$STATE/update.log" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  sleep 0.1
done
TOK=${URL#*#t=}
JAR2="$STATE/cookies2.txt"; rm -f "$JAR2"
BODY=$(curl -s -m 2 -c "$JAR2" -X POST -H 'Content-Type: application/json' -H "Origin: http://127.0.0.1:$PORT" \
  -d "{\"token\":\"$TOK\"}" "http://127.0.0.1:$PORT/__kobra/session")
CSRF2=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$BODY")
CHECK=$(curl -s -m 5 -b "$JAR2" -H "X-Kobra-CSRF: $CSRF2" -H "Origin: http://127.0.0.1:$PORT" "http://127.0.0.1:$PORT/api/update/check")
checkcontains "update check reports current release" '"current":"2026.09.1"' "$CHECK"
checkcontains "update check finds the newer release" '"latest":"2026.10.1"' "$CHECK"
checkcontains "update check reports availability" '"available":true' "$CHECK"
kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null
# The update server stays up: section 15 needs it to download the archive.

echo "== 15. update apply end-to-end (Updater spec §9.2, §14, §15, §16) =="
# The update applies at the NEXT launcher start, before the server binds
# (Updater spec decision D1, §7.1). So: run a launcher to POST the request,
# stop it, then start again and watch the swap happen at boot.

# The installed config is the machine-owned document the §14 merge must
# protect: it says "patch", while the launcher.config.json inside the archive
# says "manual". A naive swap would silently disable the update mechanism that
# performed the swap.
python3 - "$INSTALL/launcher/launcher.config.json" "$UPDATE_PORT" <<'PY'
import json, sys
path, port = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
cfg["update"] = {"channel": "patch", "patch_base_url": f"http://127.0.0.1:{port}"}
json.dump(cfg, open(path, "w"), indent=2)
PY

"$INSTALL/launcher/launcher" --print-url --port "$PORT" > "$STATE/apply1.log" 2>&1 &
PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(head -1 "$STATE/apply1.log" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  sleep 0.1
done
TOK=${URL#*#t=}
JAR3="$STATE/cookies3.txt"; rm -f "$JAR3"
BODY=$(curl -s -m 2 -c "$JAR3" -X POST -H 'Content-Type: application/json' -H "Origin: http://127.0.0.1:$PORT" \
  -d "{\"token\":\"$TOK\"}" "http://127.0.0.1:$PORT/__kobra/session")
CSRF3=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$BODY")
# POST /api/update/apply carries no body but still passes the §12.4
# Content-Type gate, which the AUTH array satisfies.
AUTH3=(-b "$JAR3" -H "X-Kobra-CSRF: $CSRF3" -H "Origin: http://127.0.0.1:$PORT" -H 'Content-Type: application/json')

DATA_BEFORE=$(cd "$INSTALL/data" && find . -type f -exec sha256sum {} \; | sort)

APPLY=$(curl -s -m 5 "${AUTH3[@]}" -X POST "http://127.0.0.1:$PORT/api/update/apply")
checkcontains "apply records the target release" '"target_release":"2026.10.1"' "$APPLY"
checkcontains "apply reports the confirmed size" '"archive_size":' "$APPLY"
checkcontains "apply is pending, not applied inline" '"pending":true' "$APPLY"

# R11.1: the recorded intent is visible before the launch that acts on it.
PROG=$(curl -s -m 5 "${AUTH3[@]}" "http://127.0.0.1:$PORT/api/update/progress")
checkcontains "progress reports the pending target" '"target_release":"2026.10.1"' "$PROG"
case "$PROG" in
  *sha256*) echo "  FAIL  progress leaked an archive hash"; fail=$((fail+1));;
  *) echo "  PASS  progress carries no archive hash"; pass=$((pass+1));;
esac

kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; PID=""

# Restart. The pending marker makes this launch download, verify, extract and
# swap before the server binds, so by the time the URL is printed the install
# has already moved.
"$INSTALL/launcher/launcher" --print-url --port "$PORT" > "$STATE/apply2.log" 2>&1 &
PID=$!
URL=""
for _ in $(seq 1 200); do
  URL=$(head -1 "$STATE/apply2.log" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  sleep 0.1
done
TOK=${URL#*#t=}
JAR4="$STATE/cookies4.txt"; rm -f "$JAR4"
BODY=$(curl -s -m 2 -c "$JAR4" -X POST -H 'Content-Type: application/json' -H "Origin: http://127.0.0.1:$PORT" \
  -d "{\"token\":\"$TOK\"}" "http://127.0.0.1:$PORT/__kobra/session")
CSRF4=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$BODY")
AUTH4=(-b "$JAR4" -H "X-Kobra-CSRF: $CSRF4" -H "Origin: http://127.0.0.1:$PORT")

# E2E-1: the new release's content is installed. The level file is the
# observable difference between the two fixture releases.
LEVELS=$(curl -s -m 5 "http://127.0.0.1:$PORT/assets/data/levels.json")
checkcontains "the swapped-in release serves its new level" "second_release" "$LEVELS"

# E2E-4: the folder reports the new release. Under scope "full" the archive
# deliberately omits game/release.manifest.json, so the §14 config merge is
# what answers here, not a manifest.
CHECK=$(curl -s -m 5 -b "$JAR4" -H "X-Kobra-CSRF: $CSRF4" -H "Origin: http://127.0.0.1:$PORT" \
  "http://127.0.0.1:$PORT/api/update/check")
checkcontains "the installed release is the new one" '"current":"2026.10.1"' "$CHECK"
checkcontains "no further update is available" '"available":false' "$CHECK"

# E2E-5: the local update channel survived the swap (the §14 trap).
CHANNEL=$(python3 -c "
import json,sys
print(json.load(open(sys.argv[1])).get('update',{}).get('channel',''))
" "$INSTALL/launcher/launcher.config.json")
check "local update.channel survived the swap" "patch" "$CHANNEL"
RELCFG=$(python3 -c "
import json,sys
print(json.load(open(sys.argv[1])).get('release',''))
" "$INSTALL/launcher/launcher.config.json")
check "the merged config carries the release's release id" "2026.10.1" "$RELCFG"

# E2E-2: data/ is byte-identical. This is FR-UPD-7, checked against the real
# install rather than a fixture.
DATA_AFTER=$(cd "$INSTALL/data" && find . -type f -exec sha256sum {} \; | sort)
check "data/ is untouched by the update" "$DATA_BEFORE" "$DATA_AFTER"

# E2E-3: the rollback copy exists and holds the previous release.
check "game.old was retained" "1" "$(ls -d "$INSTALL/game.old" 2>/dev/null | wc -l)"
check "game.old holds the previous release" "1" \
  "$(grep -c 'roundtrip' "$INSTALL/game.old/assets/data/levels.json" 2>/dev/null)"
if grep -q 'second_release' "$INSTALL/game.old/assets/data/levels.json" 2>/dev/null; then
  echo "  FAIL  game.old holds the new release"; fail=$((fail+1))
else
  echo "  PASS  game.old does not hold the new release"; pass=$((pass+1))
fi

# E2E-6: no debris. Staging lives in the sidecar (Updater spec §5), not in the
# game folder, so both places are checked.
SIDECAR_UPDATE="$STATE/kobra-games/com.kobra.pkgtest/update"
check "no update.state left in the game folder" "0" "$(ls "$INSTALL/update.state" 2>/dev/null | wc -l)"
check "no staging left in the game folder" "0" "$(ls -d "$INSTALL/game.new" 2>/dev/null | wc -l)"
check "no staging left in the sidecar" "0" \
  "$(ls -d "$SIDECAR_UPDATE/game.new" "$SIDECAR_UPDATE/launcher.new" 2>/dev/null | wc -l)"
check "the archive was removed after extraction" "0" \
  "$(ls "$SIDECAR_UPDATE/download.part" 2>/dev/null | wc -l)"
check "the pending marker was cleared" "0" "$(ls "$SIDECAR_UPDATE/pending.json" 2>/dev/null | wc -l)"

# E2E-18: the outcome is recorded for the shell to report.
RESULT=$(curl -s -m 5 "${AUTH4[@]}" "http://127.0.0.1:$PORT/api/update/progress")
checkcontains "the recorded outcome is a completion" '"outcome":"complete"' "$RESULT"

# E2E-7: retention advances only on a heartbeat (R15.3). Under --print-url no
# browser connects, so nothing may be retired and no counter may exist yet.
# (Retirement at the second heartbeat is covered by the unit tests, which can
# drive a heartbeat directly.)
check "no backup was retired without a heartbeat" "1" "$(ls -d "$INSTALL/game.old" 2>/dev/null | wc -l)"
check "no retention count without a heartbeat" "0" \
  "$(ls "$SIDECAR_UPDATE/started.json" 2>/dev/null | wc -l)"

kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; PID=""

# E2E-17: with no marker, the next launch makes no outbound request at all for
# an update. The update server is stopped first, so an attempt could not pass
# silently.
kill "$SRV_PID" 2>/dev/null; wait "$SRV_PID" 2>/dev/null; SRV_PID=""
"$INSTALL/launcher/launcher" --print-url --port "$PORT" > "$STATE/apply3.log" 2>&1 &
PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(head -1 "$STATE/apply3.log" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  sleep 0.1
done
case "$URL" in
  http://*) echo "  PASS  the launcher starts with no update pending"; pass=$((pass+1));;
  *) echo "  FAIL  the launcher did not start after the update"; fail=$((fail+1));;
esac
DOWNLOADS=$(grep -c 'update.download.begin' "$STATE/kobra-games/com.kobra.pkgtest/logs/launcher.log" 2>/dev/null | tr -d ' \n')
check "no download was attempted without a marker" "0" "${DOWNLOADS:-0}"

kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; PID=""

echo
echo "======================================"
echo "  PASS=$pass  FAIL=$fail"
echo "======================================"
[ "$fail" -eq 0 ]
