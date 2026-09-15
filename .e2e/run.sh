#!/usr/bin/env bash
# End-to-end smoke test for the launcher (Launcher spec §26.6):
# build, create a synthetic game folder, start the server, drive the API the way
# the shell would, then verify the drain removes the lock.
set -uo pipefail

# ROOT is derived from this script's location so the harness runs from any
# checkout; it used to hardcode one absolute path.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAUNCHER=$ROOT/launcher
TESTDIR=$ROOT/.e2e
GAME=$TESTDIR/TestGame

export GOMODCACHE=$ROOT/.gocache/mod
export GOCACHE=$ROOT/.gocache/build
export GOSUMDB=off
# No GOFLAGS override: the launcher module is vendored, and the smoke test must
# build what ships (it used to force -mod=mod and so never exercised vendor/).
export XDG_STATE_HOME=$TESTDIR/state

pass=0; fail=0
check() { # check <name> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  PASS  $1"; pass=$((pass+1));
  else echo "  FAIL  $1 (expected [$2] got [$3])"; fail=$((fail+1)); fi
}
checkcontains() {
  case "$3" in *"$2"*) echo "  PASS  $1"; pass=$((pass+1));;
  *) echo "  FAIL  $1 (expected to contain [$2] got [$3])"; fail=$((fail+1));; esac
}

echo "== 1. build =="
rm -rf "$GAME"
mkdir -p "$GAME/launcher" "$GAME/game/assets" "$GAME/data/saves" "$GAME/data/config"
(cd "$LAUNCHER" && go build -trimpath -buildvcs=false -o "$GAME/launcher/launcher" ./cmd/kobra-launcher) || { echo "BUILD FAILED"; exit 1; }
cp "$LAUNCHER/port-deny-list.json" "$GAME/launcher/"
echo "  built $(ls -la "$GAME/launcher/launcher" | awk '{print $5}') bytes"

cat > "$GAME/launcher/launcher.config.json" <<'JSON'
{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.kobra.testgame",
  "game_name": "Test Game",
  "release": "2026.09.1",
  "port": { "base": 18765, "span": 40, "require_confirmation": false },
  "browser_preference": ["chrome"]
}
JSON
cat > "$GAME/game/index.html" <<'HTML'
<!doctype html><html><head><title>Test</title></head><body><script src="/shell.js"></script></body></html>
HTML
printf 'console.log("shell");\n' > "$GAME/game/shell.js"
printf 'wasm-bytes' > "$GAME/game/engine.wasm"
mkdir -p "$GAME/game/engine" && printf 'engine' > "$GAME/game/engine/core.wasm"
printf 'asset-bytes' > "$GAME/game/assets/tex.png"

echo "== 2. --version and --check-port (fast paths) =="
V=$("$GAME/launcher/launcher" --version 2>&1); checkcontains "version prints name" "kobra-launcher" "$V"
CP=$("$GAME/launcher/launcher" --check-port 2>&1); checkcontains "check-port reports free" '"free":true' "$CP"
echo "  check-port output: $CP"

echo "== 3. start the server =="
LOG=$TESTDIR/launcher.out
rm -f "$LOG"
"$GAME/launcher/launcher" --yes --no-open --port 18771 > "$LOG" 2>&1 &
PID=$!
for i in $(seq 1 60); do
  if curl -s -m 1 -o /dev/null "http://127.0.0.1:18771/__kobra/health" 2>/dev/null; then break; fi
  if ! kill -0 $PID 2>/dev/null; then echo "  launcher exited early:"; cat "$LOG"; exit 1; fi
  sleep 0.1
done
echo "  pid=$PID"

echo "== 4. probe and health are unauthenticated and opaque =="
PROBE=$(curl -s -m 2 -H 'Host: 127.0.0.1:18771' http://127.0.0.1:18771/__kobra/probe)
checkcontains "probe app" '"app":"kobra-launcher"' "$PROBE"
checkcontains "probe game_id" '"game_id":"com.kobra.testgame"' "$PROBE"
checkcontains "probe has no path" '"instance"' "$PROBE"
HEALTH=$(curl -s -m 2 http://127.0.0.1:18771/__kobra/health)
checkcontains "health app" '"app":"kobra-launcher"' "$HEALTH"

echo "== 5. security gates =="
# DNS-rebinding attempt: wrong Host must be 421.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -H 'Host: evil.example' http://127.0.0.1:18771/api/state)
check "wrong Host -> 421" "421" "$CODE"
# IPv6 literal / localhost Host must also fail (FR-SRV-3).
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -H 'Host: localhost:18771' http://127.0.0.1:18771/api/state)
check "localhost Host -> 421" "421" "$CODE"
# Cross-origin GET is rejected.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -H 'Origin: http://evil.example' http://127.0.0.1:18771/api/state)
check "bad Origin -> 403" "403" "$CODE"
# No session yet.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 http://127.0.0.1:18771/api/state)
check "no session -> 401" "401" "$CODE"
# Static traversal.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 --path-as-is "http://127.0.0.1:18771/assets/../../launcher/launcher.config.json")
check "traversal -> 404" "404" "$CODE"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "http://127.0.0.1:18771/data/saves/index.json")
check "data path not static -> 404" "404" "$CODE"

echo "== 6. session exchange (bootstrap token) =="
TOKEN=not-a-real-token-but-long-enough-to-pass-length-check
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -X POST -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\"}" http://127.0.0.1:18771/__kobra/session)
check "bad token -> 401" "401" "$CODE"
printf '  note: no --print-url token available; starting a second launcher for token flow\n'

kill $PID 2>/dev/null; wait $PID 2>/dev/null

echo "== 7. restart with --print-url to obtain a real token =="
"$GAME/launcher/launcher" --print-url --port 18771 > "$LOG" 2>&1 &
PID=$!
URL=""
for i in $(seq 1 60); do
  URL=$(head -1 "$LOG" 2>/dev/null | tr -d '\r')
  case "$URL" in http://*) break;; esac
  if ! kill -0 $PID 2>/dev/null; then echo "  launcher exited early:"; cat "$LOG"; exit 1; fi
  sleep 0.1
done
echo "  url: $URL"
checkcontains "print-url has fragment" '#t=' "$URL"
FRAG=${URL#*#t=}
JAR=$TESTDIR/cookies.txt
rm -f "$JAR"
BODY=$(curl -s -m 2 -c "$JAR" -X POST -H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:18771' \
  -d "{\"token\":\"$FRAG\"}" http://127.0.0.1:18771/__kobra/session)
checkcontains "session returns id" '"session_id"' "$BODY"
checkcontains "session api_version" '"api_version":1' "$BODY"
CSRF=$(python3 -c "import json,sys; print(json.loads(sys.argv[1])['csrf_token'])" "$BODY")
SID=$(python3 -c "import json,sys; print(json.loads(sys.argv[1])['session_id'])" "$BODY")
check "csrf length 43" "43" "${#CSRF}"
# Single-use: replay must fail.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -X POST -H 'Content-Type: application/json' \
  -d "{\"token\":\"$FRAG\"}" http://127.0.0.1:18771/__kobra/session)
check "token replay -> 401" "401" "$CODE"

AUTH=(-b "$JAR" -H "X-Kobra-CSRF: $CSRF" -H "Origin: http://127.0.0.1:18771" -H 'Content-Type: application/json')

echo "== 8. GET /api/state =="
STATE=$(curl -s -m 2 "${AUTH[@]}" http://127.0.0.1:18771/api/state)
checkcontains "state api_version" '"api_version":1' "$STATE"
checkcontains "state game_id" '"game_id":"com.kobra.testgame"' "$STATE"
checkcontains "state data_writable" '"data_writable":true' "$STATE"
checkcontains "state data_dir_kind" '"data_dir_kind":"game"' "$STATE"

echo "== 9. save write / read / list / conflict =="
W1=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{"level":3,"hp":90},"claim":true,"if_revision":0}' http://127.0.0.1:18771/api/save)
checkcontains "first write rev 1" '"revision":1' "$W1"
W2=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{"level":4},"if_revision":1}' http://127.0.0.1:18771/api/save)
checkcontains "second write rev 2" '"revision":2' "$W2"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{"level":5},"if_revision":1}' http://127.0.0.1:18771/api/save)
check "stale revision -> 409" "409" "$CODE"
CONF=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"slot1","payload":{},"if_revision":1}' http://127.0.0.1:18771/api/save)
checkcontains "conflict carries current_revision" '"current_revision":2' "$CONF"
SAVE=$(curl -s -m 5 "${AUTH[@]}" http://127.0.0.1:18771/api/data/saves/slot1)
checkcontains "read returns payload" '"level":4' "$SAVE"
LIST=$(curl -s -m 5 "${AUTH[@]}" http://127.0.0.1:18771/api/data/saves)
checkcontains "list has slot1" '"slot":"slot1"' "$LIST"
checkcontains "list revision 2" '"revision":2' "$LIST"

echo "== 10. CSRF and identifier gates =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -b "$JAR" -H 'Origin: http://127.0.0.1:18771' -H 'Content-Type: application/json' \
  -X POST -d '{"slot":"slot1","payload":{}}' http://127.0.0.1:18771/api/save)
check "missing CSRF header -> 403" "403" "$CODE"
BAD=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"../evil","payload":{}}' http://127.0.0.1:18771/api/save)
checkcontains "bad slot rejected" 'bad_identifier' "$BAD"
BAD2=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"slot":"COM1","payload":{}}' http://127.0.0.1:18771/api/save)
checkcontains "device name rejected" 'bad_identifier' "$BAD2"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -b "$JAR" -H "X-Kobra-CSRF: $CSRF" -H 'Origin: http://127.0.0.1:18771' -H 'Content-Type: text/plain' \
  -X POST -d 'x' http://127.0.0.1:18771/api/save)
check "bad content-type -> 415" "415" "$CODE"

echo "== 11. config merge preserves unknown keys =="
printf '{"audio":{"volume":0.5},"custom_engine_key":{"keep":true}}\n' > "$GAME/data/config/settings.json"
CR=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"merge":{"audio":{"volume":0.8}}}' http://127.0.0.1:18771/api/config)
checkcontains "config write rev" '"revision"' "$CR"
checkcontains "unmentioned key preserved" '"custom_engine_key"' "$(cat "$GAME/data/config/settings.json")"
checkcontains "merged value written" '"volume":0.8' "$(cat "$GAME/data/config/settings.json")"
BADK=$(curl -s -m 5 "${AUTH[@]}" -X POST -d '{"merge":{"evil_key":1}}' http://127.0.0.1:18771/api/config)
checkcontains "unknown config key rejected" 'malformed_body' "$BADK"

echo "== 12. static serving and headers =="
HDR=$(curl -s -D - -o /dev/null -m 2 http://127.0.0.1:18771/shell.js)
checkcontains "CSP header present" 'Content-Security-Policy' "$HDR"
checkcontains "nosniff present" 'X-Content-Type-Options: nosniff' "$HDR"
checkcontains "shell.js no-cache" 'Cache-Control: no-cache' "$HDR"
HDRW=$(curl -s -D - -o /dev/null -m 2 http://127.0.0.1:18771/engine/core.wasm)
checkcontains "wasm MIME" 'Content-Type: application/wasm' "$HDRW"
checkcontains "engine immutable" 'immutable' "$HDRW"
RANGE=$(curl -s -D - -o /dev/null -m 2 -H 'Range: bytes=0-2' http://127.0.0.1:18771/shell.js)
checkcontains "range -> 206" '206 Partial Content' "$RANGE"
RANGEBAD=$(curl -s -D - -o /dev/null -m 2 -H 'Range: bytes=0-1,3-4' http://127.0.0.1:18771/shell.js)
checkcontains "multi-range -> 416" '416' "$RANGEBAD"

echo "== 13. heartbeat, diagnostics, shutdown, drain =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 -b "$JAR" -H "X-Kobra-CSRF: $CSRF" -H 'Origin: http://127.0.0.1:18771' -X POST http://127.0.0.1:18771/__kobra/heartbeat)
check "heartbeat -> 204" "204" "$CODE"
DIAG=$(curl -s -m 2 "${AUTH[@]}" http://127.0.0.1:18771/__kobra/diagnostics)
checkcontains "diagnostics has port" '"port":18771' "$DIAG"
checkcontains "diagnostics has origin" '"origin":"http://127.0.0.1:18771"' "$DIAG"
checkcontains "diagnostics has log_tail" '"log_tail"' "$DIAG"
case "$DIAG" in *"$ROOT"*) echo "  FAIL  diagnostics leaked a filesystem path"; fail=$((fail+1));; *) echo "  PASS  diagnostics has no filesystem path"; pass=$((pass+1));; esac
case "$DIAG" in *"$(whoami)"*) echo "  FAIL  diagnostics leaked the OS username"; fail=$((fail+1));; *) echo "  PASS  diagnostics has no OS username"; pass=$((pass+1));; esac

# Drain: SIGTERM and confirm exit 0 and lock removal.
kill -TERM $PID
for i in $(seq 1 100); do kill -0 $PID 2>/dev/null || break; sleep 0.1; done
if kill -0 $PID 2>/dev/null; then echo "  FAIL  launcher did not exit on SIGTERM"; fail=$((fail+1)); else
  wait $PID; RC=$?
  check "SIGTERM exit code 0" "0" "$RC"
fi
LOCKFILE=$XDG_STATE_HOME/kobra-games/com.kobra.testgame/instance.lock
if [ -f "$LOCKFILE" ]; then echo "  FAIL  instance.lock not removed"; fail=$((fail+1)); else echo "  PASS  instance.lock removed"; pass=$((pass+1)); fi

echo "== 14. port.json and origin stability across restart =="
PORTJSON=$XDG_STATE_HOME/kobra-games/com.kobra.testgame/port.json
checkcontains "port.json written" '"port": 18771' "$(cat "$PORTJSON" 2>/dev/null)"
checkcontains "origin recorded" 'http://127.0.0.1:18771' "$(cat "$PORTJSON" 2>/dev/null)"
checkcontains "schema recorded" 'kobra.port-state/1' "$(cat "$PORTJSON" 2>/dev/null)"

echo "== 15. save file shape on disk =="
SAVEFILE=$GAME/data/saves/slot1.json
checkcontains "save schema" 'kobra.save/1' "$(cat "$SAVEFILE")"
checkcontains "save revision" '"revision":2' "$(cat "$SAVEFILE")"
checkcontains "save checksum" 'sha256:' "$(cat "$SAVEFILE")"
checkcontains "payload preserved" '"level":4' "$(cat "$SAVEFILE")"
checkcontains "creation time preserved" '"created"' "$(cat "$SAVEFILE")"
if [ -f "$SAVEFILE.bak" ]; then echo "  PASS  .bak rotation produced"; pass=$((pass+1)); else echo "  FAIL  no .bak"; fail=$((fail+1)); fi
REVS=$(ls "$GAME/data/saves/" | grep -c 'slot1.json\.[0-9]')
if [ "$REVS" -ge 1 ]; then echo "  PASS  revision sidecars present ($REVS)"; pass=$((pass+1)); else echo "  FAIL  no revision sidecars"; fail=$((fail+1)); fi

echo "== 16. interruption recovery (\u00a719.4) =="
# Simulate a swap interrupted after the first move: game/ is gone and game.old/
# holds the previous release while update.state is present. Startup must roll
# back so the game is runnable again.
mv "$GAME/game" "$GAME/game.old"
cat > "$GAME/update.state" <<'JSON'
{"schema":"kobra.update-state/1","release":"2026.10.1","step":"begin","from":"game","to":"game.old","staging":"game.new"}
JSON
OUT=$("$GAME/launcher/launcher" --diagnostics 2>&1)
if [ -d "$GAME/game" ]; then echo "  PASS  interrupted swap rolled back"; pass=$((pass+1));
else echo "  FAIL  game/ not restored by recovery"; fail=$((fail+1)); fi
if [ -f "$GAME/update.state" ]; then echo "  FAIL  update.state survived recovery"; fail=$((fail+1));
else echo "  PASS  update.state cleared"; pass=$((pass+1)); fi
if [ -f "$GAME/game/index.html" ]; then echo "  PASS  rolled-back release is runnable"; pass=$((pass+1));
else echo "  FAIL  rolled-back release missing index.html"; fail=$((fail+1)); fi

# Staging lives under the sidecar (Updater spec R5.3), not in the game folder —
# the fixture drive asserts "no staging left in the game folder" as a feature,
# so a portable folder carries no interrupted download. A game.new/ in the game
# folder is therefore NOT the launcher's to delete, and recovery must leave it
# alone. The real stray-staging cleanup (a game.new/ under <sidecar>/update with
# no update.state) is covered by TestRecoverMatrix's "bullet 4" case.
mkdir -p "$GAME/game.new/partial"
OUT=$("$GAME/launcher/launcher" --diagnostics 2>&1)
if [ -d "$GAME/game.new" ]; then echo "  PASS  the game folder's own game.new is left alone"; pass=$((pass+1));
else echo "  FAIL  the launcher removed a game-folder directory it does not own"; fail=$((fail+1)); fi
rm -rf "$GAME/game.new"

echo "== 17. dev-only --game-dir is absent from the release binary (FR-LNCH-1) =="
# A release binary must not define the flag at all: that is what keeps the
# executable-path rule intact for shipped builds.
set +e
DEVOUT=$("$GAME/launcher/launcher" --game-dir "$GAME" --diagnostics 2>&1)
DEVRC=$?
set -e
check "release --game-dir exits 1" "1" "$DEVRC"
case "$DEVOUT" in
  *"not defined"*) echo "  PASS  release binary rejects --game-dir"; pass=$((pass+1));;
  *) echo "  FAIL  release binary accepted --game-dir: $DEVOUT"; fail=$((fail+1));;
esac

# The dev build honours it: built here so the check runs on every suite run.
(cd "$LAUNCHER" && go build -tags kobra_dev -trimpath -buildvcs=false -o "$TESTDIR/launcher-dev" ./cmd/kobra-launcher) 2>/dev/null
if [ -x "$TESTDIR/launcher-dev" ]; then
  # Serve a *different* folder than the one the binary lives in, and confirm the
  # override wins over the executable path.
  mkdir -p "$TESTDIR/OtherGame/game" "$TESTDIR/OtherGame/launcher"
  echo '<html>OVERRIDDEN</html>' > "$TESTDIR/OtherGame/game/index.html"
  cat > "$TESTDIR/OtherGame/launcher/launcher.config.json" <<'JSON'
{"schema":"kobra.launcher-config/1","game_id":"com.kobra.overridetest","game_name":"Override","port":{"base":19750,"span":10,"require_confirmation":false}}
JSON
  XDG_STATE_HOME="$TESTDIR/state-dev" "$TESTDIR/launcher-dev" --game-dir "$TESTDIR/OtherGame" --yes --no-open --port 19761 > "$TESTDIR/dev.log" 2>&1 &
  DEVPID=$!
  for i in $(seq 1 60); do curl -s -m 1 -o /dev/null http://127.0.0.1:19761/__kobra/health 2>/dev/null && break; sleep 0.1; done
  PROBE=$(curl -s -m 2 http://127.0.0.1:19761/__kobra/probe)
  checkcontains "dev build serves the overridden folder" '"game_id":"com.kobra.overridetest"' "$PROBE"
  checkcontains "dev build announces itself" "development build" "$(cat "$TESTDIR/dev.log")"
  kill $DEVPID 2>/dev/null; wait $DEVPID 2>/dev/null || true
  rm -rf "$TESTDIR/OtherGame" "$TESTDIR/state-dev" "$TESTDIR/dev.log" "$TESTDIR/launcher-dev"
else
  echo "  FAIL  could not build the kobra_dev binary"; fail=$((fail+1))
fi

echo
echo "======================================"
echo "  PASS=$pass  FAIL=$fail"
echo "======================================"
[ "$fail" -eq 0 ]
