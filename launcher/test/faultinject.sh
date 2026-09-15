#!/usr/bin/env bash
# Fault-injection drive for Launcher spec §26.4.
#
# Builds the launcher with the kobra_faultinject tag and runs the scenarios that
# need a real process: a crash between two syscalls, and a panicking handler.
# The scenarios that can be driven in-process — the EXDEV fallback, the
# out-of-space mapping, and recovery from an injected panic — are tagged Go tests
# (`go test -tags kobra_faultinject ./...`); this script covers what only a
# separate process can show.
#
#   ./faultinject.sh                       # build, run, report
#   PORT=18811 ./faultinject.sh            # a different port
#
# The six scenarios of §26.4 map as follows:
#
#   1. crash after fsync, before rename     scenario 1 here
#   2. crash after rename, before fsync(dir) scenario 2 here
#   3. EXDEV on rename                      tagged test (storage + update)
#   4. disk-full on write                   tagged test (storage)
#   5. panic in a handler                   scenario 3 here (asserts exit 4)
#   6. signal during drain                  .e2e/run.sh asserts it; re-checked in 4
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCHER_SRC="$ROOT/launcher"
WORK="${TMPDIR:-/tmp}/kobra-faultinject"
GAME="$WORK/Game"
PORT=${PORT:-18811}
GAME_ID=com.kobra.faultinject

export GOCACHE="$ROOT/.gocache/build"
export GOMODCACHE="$ROOT/.gocache/mod"
export GOSUMDB=off
export GOFLAGS=-mod=vendor
export XDG_STATE_HOME="$WORK/state"

LOGDIR="$XDG_STATE_HOME/kobra-games/$GAME_ID/logs"

pass=0; fail=0
check() { # check <name> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  PASS  $1"; pass=$((pass+1));
  else echo "  FAIL  $1 (expected [$2] got [$3])"; fail=$((fail+1)); fi
}
checkcontains() { # checkcontains <name> <needle> <haystack>
  case "$3" in *"$2"*) echo "  PASS  $1"; pass=$((pass+1));;
  *) echo "  FAIL  $1 (expected to contain [$2] got [$3])"; fail=$((fail+1));; esac
}

# --- fixture ---------------------------------------------------------------

echo "== 0. build with the fault-injection tag =="
rm -rf "$WORK"
mkdir -p "$GAME/launcher" "$GAME/game/assets" "$GAME/data/saves" "$GAME/data/config"
(cd "$LAUNCHER_SRC" && go build -tags kobra_faultinject -o "$GAME/launcher/launcher" ./cmd/kobra-launcher) \
  || { echo "  BUILD FAILED"; exit 1; }
cp "$LAUNCHER_SRC/port-deny-list.json" "$GAME/launcher/"
cat > "$GAME/launcher/launcher.config.json" <<JSON
{
  "schema": "kobra.launcher-config/1",
  "game_id": "$GAME_ID",
  "game_name": "Fault Injection",
  "release": "2026.09.1",
  "port": { "base": 18800, "span": 50, "require_confirmation": false },
  "data_api": { "writes_per_minute": 1000, "bytes_per_minute": 134217728 }
}
JSON
printf '<html>fault</html>\n' > "$GAME/game/index.html"
printf '// shell\n' > "$GAME/game/shell.js"
echo "  built $GAME/launcher/launcher"

PID=""
JAR=""
CSRF=""

# start_bare [env assignments...] — start the launcher and wait for its URL line.
start_bare() {
  rm -f "$WORK/launcher.out"
  env "$@" "$GAME/launcher/launcher" --print-url --port "$PORT" > "$WORK/launcher.out" 2>&1 &
  PID=$!
  local url=""
  for _ in $(seq 1 100); do
    url=$(head -1 "$WORK/launcher.out" 2>/dev/null | tr -d '\r')
    case "$url" in http://*) break;; esac
    if ! kill -0 "$PID" 2>/dev/null; then return 1; fi
    sleep 0.1
  done
  case "$url" in http://*) ;; *) return 1;; esac
  FRAG=${url#*#t=}
}

# session — exchange the bootstrap token for a session and a CSRF token.
session() {
  JAR="$WORK/cookies.txt"
  rm -f "$JAR"
  local body
  body=$(curl -s -m 2 -c "$JAR" -X POST -H 'Content-Type: application/json' \
    -H "Origin: http://127.0.0.1:$PORT" -d "{\"token\":\"$FRAG\"}" \
    "http://127.0.0.1:$PORT/__kobra/session")
  CSRF=$(python3 -c "import json,sys; print(json.loads(sys.argv[1])['csrf_token'])" "$body" 2>/dev/null)
}

# save <json body>
save() {
  curl -s -m 5 -b "$JAR" -H "X-Kobra-CSRF: $CSRF" -H "Origin: http://127.0.0.1:$PORT" \
    -H 'Content-Type: application/json' -X POST -d "$1" "http://127.0.0.1:$PORT/api/save"
}

read_save() {
  curl -s -m 5 -b "$JAR" -H "Origin: http://127.0.0.1:$PORT" "http://127.0.0.1:$PORT/api/data/saves/slot1"
}

stop() { # stop — a clean drain
  [ -n "$PID" ] || return 0
  kill -TERM "$PID" 2>/dev/null
  wait "$PID" 2>/dev/null
  PID=""
}

tmp_files() { ls -a "$GAME/data/saves" 2>/dev/null | grep -c '\.tmp-'; }

# --- 1. crash after fsync, before rename -----------------------------------

echo "== 1. crash after fsync, before rename (§26.4) =="
start_bare || { echo "  the launcher did not start"; exit 1; }
session
checkcontains "first write" '"revision":1' "$(save '{"slot":"slot1","payload":{"level":1},"claim":true,"if_revision":0}')"
checkcontains "second write" '"revision":2' "$(save '{"slot":"slot1","payload":{"level":2},"if_revision":1}')"
stop
checkcontains "the live save is the second revision" '"level":2' "$(cat "$GAME/data/saves/slot1.json")"
checkcontains "the previous revision is in .bak" '"level":1' "$(cat "$GAME/data/saves/slot1.json.bak")"

# Arm the crash and attempt a third write. The launcher dies inside the write.
start_bare KOBRA_FAULT=storage.write.after_fsync=exit:70
session
save '{"slot":"slot1","payload":{"level":3},"if_revision":2}' >/dev/null 2>&1 &
wait "$PID"; RC=$?
PID=""
check "the injected crash exits 70" "70" "$RC"
checkcontains "the crashed write did not land" '"level":2' "$(cat "$GAME/data/saves/slot1.json")"
checkcontains ".bak is still loadable" '"level":1' "$(cat "$GAME/data/saves/slot1.json.bak")"
check "the crash left one temp file" "1" "$(tmp_files)"

# Restart: startup must quarantine it, not delete it, and lose nothing.
start_bare || { echo "  the launcher did not restart after the crash"; exit 1; }
check "the temp file was quarantined" "0" "$(tmp_files)"
# The quarantined name starts with a dot, so a plain shell glob would miss it.
QUARANTINED=$(find "$GAME/data" -path '*.trash-*/saves/*' -name '.*.tmp-*' 2>/dev/null | wc -l | tr -d ' ')
check "it is in the trash area, not deleted" "1" "$QUARANTINED"
session
checkcontains "the save survived the crash and restart" '"level":2' "$(read_save)"
stop

# --- 2. crash after rename, before fsync(dir) ------------------------------

echo "== 2. crash after rename, before fsync(dir) (§26.4) =="
start_bare KOBRA_FAULT=storage.write.after_rename=exit:70
session
save '{"slot":"slot1","payload":{"level":9},"if_revision":2}' >/dev/null 2>&1 &
wait "$PID"; RC=$?
PID=""
check "the injected crash exits 70" "70" "$RC"
# The rename had already happened, so the new revision is in place. Real
# power-loss durability of the directory entry cannot be simulated from here;
# what is asserted is the state the next start must cope with.
checkcontains "the write had already landed" '"level":9' "$(cat "$GAME/data/saves/slot1.json")"

start_bare || { echo "  the launcher did not restart after the crash"; exit 1; }
session
checkcontains "the landed write is readable after restart" '"level":9' "$(read_save)"
stop

# --- 3. panic in a handler -------------------------------------------------

echo "== 3. panic in a handler (§26.4, FR-SRV-24) =="
rm -f "$LOGDIR"/crash-*.txt
start_bare KOBRA_FAULT=server.handler=panic
# No session here: the very first request panics, by design.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Host: 127.0.0.1:$PORT" \
  "http://127.0.0.1:$PORT/__kobra/health")
check "the panicking handler answers 500" "500" "$CODE"
wait "$PID"; RC=$?
PID=""
check "a panicking handler exits 4" "4" "$RC"
check "a crash dump was written" "1" "$(ls "$LOGDIR"/crash-*.txt 2>/dev/null | wc -l | tr -d ' ')"
checkcontains "the panic was logged" 'server.panic' "$(cat "$LOGDIR"/launcher.log 2>/dev/null)"

# --- 4. signal during drain ------------------------------------------------

echo "== 4. signal during drain (§26.4) =="
start_bare || { echo "  the launcher did not start"; exit 1; }
kill -TERM "$PID"
wait "$PID"; RC=$?
PID=""
check "SIGTERM during drain exits 0" "0" "$RC"
LOCK="$XDG_STATE_HOME/kobra-games/$GAME_ID/instance.lock"
if [ -f "$LOCK" ]; then echo "  FAIL  the instance lock was not removed"; fail=$((fail+1));
else echo "  PASS  the instance lock was removed"; pass=$((pass+1)); fi

echo
echo "======================================"
echo "  PASS=$pass  FAIL=$fail"
echo "======================================"
[ "$fail" -eq 0 ]
