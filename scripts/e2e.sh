#!/bin/bash
# e2e.sh — staged end-to-end verification of AegisGo against the REAL
# binaries (make build), on loopback only. Each stage proves one slice of
# the product with the interfaces a user would actually touch: CLI router,
# workspace file tools + hot-reloaded rules, REST matrix, gRPC, MCP attach,
# Telegram (against a python3 fake Bot API), aegisctl admin ops, the miner,
# the audit trail, and (when a local Ollama model exists) real LLM fallback
# with a live tool call.
#
# Usage: ./scripts/e2e.sh   (or `make e2e`)
#
# Rules of the road:
#   - every wait is a bounded poll (wait_for); no bare sleeps
#   - each stage prints `PASS/FAIL <id>`; a summary table ends the run
#   - fixed ports 18080-18083 + 18090, guarded up front (a busy port means
#     a previous run is still alive — that's an error, not a race to win)
#   - everything the script starts (serves, fake Bot API, an `ollama serve`
#     it started itself) is killed by the EXIT trap
#
# macOS notes (this is a Darwin-first script):
#   - `free` does not exist on macOS, so /memory and /free are ASSERTED to
#     fail with `command "memory" failed` — the documented Darwin caveat.
#   - no GNU `timeout` on macOS: long-running calls use run_watchdog below.

set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)

# ---- fixed ports ------------------------------------------------------------
P_HTTP=18080   # aegis-serve HTTP: S2 workspace serve, then the S3/S4/S5 serve
P_GRPC=18081   # aegis-serve gRPC (S5, same process as P_HTTP)
P_AUX=18082    # S6 MCP-attach serve, reused by the S7 poll-mode serve
P_S7WEB=18083  # S7 webhook-mode serve (webhook endpoint mounts here)
P_TG=18090     # python3 fake Telegram Bot API (S7)

# ---- per-run workspace ------------------------------------------------------
WORK=$(mktemp -d "${TMPDIR:-/tmp}/aegisgo-e2e.XXXXXX")
WS="$WORK/ws"      # agent workspace (AEGIS_WORKSPACE)
DB="$WORK/db"      # one SQLite file per stage
OUT="$WORK/out"    # captured outputs + serve logs
mkdir -p "$WS/data" "$WS/logs" "$DB" "$OUT"

BG_PIDS=""        # space-separated pids started by this script
LAST_PID=""       # most recent bg() pid
S2_PID="" S3_PID="" S6_PID="" S7_POLL_PID="" S7_WEB_PID="" TG_PID=""
OLLAMA_PID=""     # set only when WE started `ollama serve`
OLLAMA_OK=0       # 1 when a local Ollama server + model are usable (S11)
OLLAMA_MODEL=""
GRPCURL_OK=0      # 1 when grpcurl is installed (S5)
ASYNC_TRACE=""    # filled by S3, joined in S10
SSE_TRACE=""
STAGE_IDS=""
STAGE_RESULTS=""
FAILS=0

# Sanitize: leftover AEGIS_*/provider vars from the caller's shell must not
# leak into the serves we boot (e.g. a stray AEGIS_TELEGRAM_TOKEN would
# silently enable Telegram everywhere).
unset AEGIS_PROVIDER AEGIS_MODEL AEGIS_MODEL_FAST AEGIS_MODEL_SMART \
      AEGIS_INSTRUCTIONS AEGIS_LLM AEGIS_ADDR AEGIS_GRPC_ADDR AEGIS_DB_PATH \
      AEGIS_WORKSPACE AEGIS_RULES_RELOAD AEGIS_MINER_INTERVAL \
      AEGIS_MCP_SERVERS AEGIS_SQL_DSN AEGIS_SQL_MODE \
      AEGIS_TELEGRAM_TOKEN AEGIS_TELEGRAM_CHATS AEGIS_TELEGRAM_MODE \
      AEGIS_TELEGRAM_WEBHOOK_URL AEGIS_TELEGRAM_WEBHOOK_SECRET \
      AEGIS_TELEGRAM_API_BASE AEGIS_TELEGRAM_WORKERS \
      OPENAI_API_KEY OPENAI_BASE_URL ANTHROPIC_API_KEY FOUNDRY_ENDPOINT

# ---- output helpers ---------------------------------------------------------
note() { printf '    ok: %s\n' "$*"; }
bad()  { printf '    FAIL: %s\n' "$*"; }
stage_hdr() { printf '\n==== %s %s\n' "$1" "$2"; }

# assert_contains <file> <needle> <desc> — fixed-string membership
assert_contains() {
  if grep -qF -- "$2" "$1"; then note "$3"; return 0; fi
  bad "$3 — wanted \"$2\" in $1"
  sed -n '1,15p' "$1" 2>/dev/null | sed 's/^/        | /'
  return 1
}
# assert_absent <file> <needle> <desc>
assert_absent() {
  if grep -qF -- "$2" "$1"; then
    bad "$3 — did NOT want \"$2\" in $1"
    sed -n '1,15p' "$1" 2>/dev/null | sed 's/^/        | /'
    return 1
  fi
  note "$3"; return 0
}
# assert_matches <file> <regex> <desc> — for tabwriter-aligned output
assert_matches() {
  if grep -qE -- "$2" "$1"; then note "$3"; return 0; fi
  bad "$3 — wanted /$2/ in $1"
  sed -n '1,15p' "$1" 2>/dev/null | sed 's/^/        | /'
  return 1
}
# assert_eq <actual> <expected> <desc>
assert_eq() {
  if [ "$1" = "$2" ]; then note "$3"; return 0; fi
  bad "$3 — got '$1' want '$2'"; return 1
}
assert_ge() { # actual min desc (integers)
  if [ "${1:-0}" -ge "$2" ] 2>/dev/null; then note "$3 ($1 >= $2)"; return 0; fi
  bad "$3 — got '$1' want >= $2"; return 1
}

# wait_for <desc> <timeout-secs> <cmd...> — bounded poll, no bare sleeps.
wait_for() {
  local desc=$1 tmo=$2; shift 2
  local start=$SECONDS
  while :; do
    if "$@" >/dev/null 2>&1; then
      note "$desc (waited $((SECONDS - start))s)"
      return 0
    fi
    if [ $((SECONDS - start)) -ge "$tmo" ]; then
      bad "$desc — not satisfied within ${tmo}s"
      return 1
    fi
    sleep 0.3
  done
}

# sq <db> <sql> — sqlite3 CLI with a busy timeout (serves hold the WAL).
sq() { sqlite3 -batch -cmd '.timeout 5000' "$1" "$2"; }

# bg <name> <cmd...> — start a background process, tracked for cleanup.
bg() {
  local name=$1; shift
  "$@" >"$OUT/$name.log" 2>&1 &
  LAST_PID=$!
  BG_PIDS="$BG_PIDS $LAST_PID"
  note "started $name (pid $LAST_PID, log $OUT/$name.log)"
}

# stop_bg <pid> <name> — TERM (graceful drain), bounded wait, then KILL.
stop_bg() {
  local pid=$1 name=$2 i=0
  kill -TERM "$pid" 2>/dev/null || true
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 100 ]; do
    sleep 0.1; i=$((i + 1))
  done
  kill -9 "$pid" 2>/dev/null || true
  note "stopped $name"
}

# stop_if_alive <pid> <name> — teardown guard for failure paths (no-op when
# the stage already stopped its serve, or never started one).
stop_if_alive() {
  [ -n "$1" ] || return 0
  if kill -0 "$1" 2>/dev/null; then stop_bg "$1" "$2"; fi
  return 0
}

# run_watchdog <secs> <cmd...> — run a command, kill it after <secs>
# (stands in for GNU timeout, which macOS lacks).
run_watchdog() {
  local tmo=$1; shift
  local start=$SECONDS pid rc
  "$@" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [ $((SECONDS - start)) -ge "$tmo" ]; then
      kill -TERM "$pid" 2>/dev/null || true
      sleep 1
      kill -9 "$pid" 2>/dev/null || true
      bad "command exceeded ${tmo}s timeout"
      return 1
    fi
    sleep 0.5
  done
  wait "$pid" && rc=0 || rc=$?
  return "$rc"
}

# agent_off <db> <outfile> <prompt> — one-shot CLI run with the LLM off.
agent_off() {
  local db=$1 out=$2; shift 2
  AEGIS_LLM=off AEGIS_DB_PATH="$db" AEGIS_WORKSPACE="$WS" \
    "$REPO/bin/aegis-agent" "$*" >"$out" 2>"$out.err"
}

# http_post <url> <json> <outfile> — POST with a JSON body, response captured.
http_post() {
  curl -s -X POST -H 'Content-Type: application/json' -d "$2" "$1" >"$3" 2>/dev/null
}

# port_busy <port> — 0 when something already listens on it.
port_busy() {
  local p=$1
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1
    return $?
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then return 0; fi
  return 1
}

die() { printf 'FATAL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  local p
  for p in $BG_PIDS; do kill -TERM "$p" 2>/dev/null || true; done
  sleep 1 # one graceful beat; happy-path teardown already drained
  for p in $BG_PIDS; do
    if kill -0 "$p" 2>/dev/null; then kill -9 "$p" 2>/dev/null || true; fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

record() { # id result — append to the summary table
  STAGE_IDS="$STAGE_IDS $1"
  STAGE_RESULTS="$STAGE_RESULTS $2"
  if [ "$2" = "FAIL" ]; then FAILS=$((FAILS + 1)); fi
}
run_stage() { # id desc fn
  stage_hdr "$1" "$2"
  if $3; then
    record "$1" PASS; printf 'PASS %s\n' "$1"
  else
    record "$1" FAIL; printf 'FAIL %s\n' "$1"
  fi
}
skip_stage() { # id desc reason
  stage_hdr "$1" "$2"
  printf '    SKIP: %s\n' "$3"
  record "$1" SKIP
}

# =====================================================================
# S0 — build + prerequisites
# =====================================================================
s0() {
  local c
  for c in go sqlite3 curl python3; do
    command -v "$c" >/dev/null 2>&1 || { bad "missing prerequisite: $c"; return 1; }
  done
  note "prerequisites present (go, sqlite3, curl, python3)"

  (cd "$REPO" && make build) >"$OUT/build.log" 2>&1 || {
    bad "make build failed"; tail -20 "$OUT/build.log"; return 1
  }
  # aegisctl is the admin tool; not part of `make build`'s deploy set.
  (cd "$REPO" && go build -o bin/aegisctl ./cmd/aegisctl) >>"$OUT/build.log" 2>&1 || {
    bad "building aegisctl failed"; tail -20 "$OUT/build.log"; return 1
  }
  local b
  for b in aegis-agent aegis-serve aegisctl mcp-echo-server; do
    [ -x "$REPO/bin/$b" ] || { bad "missing binary bin/$b"; return 1; }
  done
  note "built aegis-agent, aegis-serve, aegisctl, mcp-echo-server"

  # grpcurl drives S5; missing = skip with a note (brew install grpcurl).
  if command -v grpcurl >/dev/null 2>&1; then
    GRPCURL_OK=1; note "grpcurl found — S5 will run"
  else
    printf '    WARN: grpcurl not found — S5 will SKIP (brew install grpcurl)\n'
  fi

  # Ollama drives S11. Only start a server if none is running; whatever we
  # start here is killed by the EXIT trap.
  if curl -sf --max-time 3 http://localhost:11434/api/tags >"$OUT/ollama-tags.json" 2>/dev/null; then
    note "ollama server already running"
  elif command -v ollama >/dev/null 2>&1; then
    printf '    ollama installed but server down — starting one for this run\n'
    bg ollama-serve ollama serve
    OLLAMA_PID=$LAST_PID
    wait_for "ollama server up" 45 \
      curl -sf --max-time 2 http://localhost:11434/api/tags -o "$OUT/ollama-tags.json" \
      || { printf '    WARN: ollama serve did not come up — S11 will SKIP\n'; return 0; }
  else
    printf '    WARN: ollama not installed — S11 will SKIP (brew install ollama)\n'
    return 0
  fi

  # Pick a model: prefer the small qwen2.5 the docs mention, else any.
  OLLAMA_MODEL=$(python3 - "$OUT/ollama-tags.json" <<'PY'
import json, sys
try:
    tags = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
names = [m.get("name", "") for m in tags.get("models", [])]
pref = [n for n in names if n.startswith("qwen2.5:0.5b")] or \
       [n for n in names if n.startswith("qwen2.5")] or names
print(pref[0] if pref else "")
PY
)
  if [ -n "$OLLAMA_MODEL" ]; then
    OLLAMA_OK=1
    note "ollama model available: $OLLAMA_MODEL — S11 will run"
  else
    printf '    WARN: no ollama model pulled — S11 will SKIP (ollama pull qwen2.5:0.5b)\n'
  fi
}

# =====================================================================
# S1 — CLI router matrix (AEGIS_LLM=off, zero credentials)
# =====================================================================
s1() {
  local db="$DB/s1.db" o="$OUT/s1"
  local hn; hn=$(hostname)

  agent_off "$db" "$o-uptime"  "/uptime"  || return 1
  assert_contains "$o-uptime" "load average" "/uptime answers with the load average" || return 1

  agent_off "$db" "$o-disk" "/disk" || return 1
  assert_contains "$o-disk" "Filesystem" "/disk answers with df output" || return 1
  agent_off "$db" "$o-df" "/df" || return 1
  assert_contains "$o-df" "Filesystem" "/df hits the same disk rule" || return 1

  agent_off "$db" "$o-hostname" "/hostname" || return 1
  assert_contains "$o-hostname" '"command": "hostname"' "/hostname ran the hostname command" || return 1
  assert_contains "$o-hostname" "$hn" "/hostname output matches this machine" || return 1

  agent_off "$db" "$o-kernel" "/kernel" || return 1
  assert_contains "$o-kernel" "Darwin" "/kernel reports Darwin" || return 1
  agent_off "$db" "$o-uname" "/uname" || return 1
  assert_contains "$o-uname" "Darwin" "/uname hits the same kernel rule" || return 1

  agent_off "$db" "$o-who" "/who" || return 1
  assert_contains "$o-who" '"command": "who"' "/who ran the who command" || return 1

  # Darwin caveat, asserted: the memory rule maps to `free -m`, which does
  # not exist on macOS. If these ever SUCCEED here, the catalog drifted.
  agent_off "$db" "$o-memory" "/memory" || return 1
  assert_contains "$o-memory" 'command "memory" failed' \
    "/memory fails on Darwin with the documented error" || return 1
  agent_off "$db" "$o-free" "/free" || return 1
  assert_contains "$o-free" 'command "memory" failed' \
    "/free fails on Darwin with the documented error" || return 1

  agent_off "$db" "$o-csv3" "/csv_head data/sales.csv 3" || return 1
  assert_contains "$o-csv3" '"total_rows": 5' "/csv_head reports the full row total" || return 1
  assert_contains "$o-csv3" '"truncated": true' "/csv_head honors the 3-row cap" || return 1
  assert_contains "$o-csv3" "alice" "/csv_head returns the first rows (alice)" || return 1
  assert_absent "$o-csv3" "erin" "/csv_head cap excludes row 5 (erin)" || return 1

  agent_off "$db" "$o-stats" "/csv_summary data/sales.csv" || return 1
  assert_contains "$o-stats" '"total_rows": 5' "/csv_summary reports 5 rows" || return 1
  assert_contains "$o-stats" '"kind"' "/csv_summary profiles columns" || return 1

  agent_off "$db" "$o-miss" "what is the capital of France" || return 1
  assert_contains "$o-miss" "the LLM fallback is disabled" \
    "router miss with AEGIS_LLM=off returns the fast llm_disabled message" || return 1
}

# =====================================================================
# S2 — workspace reads + hot-reloaded SEARCH rule (sql_query attach_csv)
# =====================================================================
s2() {
  local db="$DB/s2.db" o="$OUT/s2" url="http://127.0.0.1:$P_HTTP"

  bg s2-serve env \
    AEGIS_ADDR="127.0.0.1:$P_HTTP" AEGIS_GRPC_ADDR=none AEGIS_LLM=off \
    AEGIS_DB_PATH="$db" AEGIS_WORKSPACE="$WS" \
    AEGIS_RULES_RELOAD=2 AEGIS_MINER_INTERVAL=0 \
    "$REPO/bin/aegis-serve"
  S2_PID=$LAST_PID
  wait_for "workspace serve healthy on :$P_HTTP" 20 \
    curl -sf "$url/healthz" || return 1

  # CSV via the router, over HTTP this time. Note the REST body embeds the
  # tool's JSON as an escaped string, so needles carry the \" escapes.
  http_post "$url/v1/agent/run" '{"prompt":"/csv_head data/sales.csv"}' "$o-csv.json" || return 1
  assert_contains "$o-csv.json" "alice" "/csv_head over HTTP returns the needle row" || return 1
  http_post "$url/v1/agent/run" '{"prompt":"/csv_summary data/sales.csv"}' "$o-cstats.json" || return 1
  assert_contains "$o-cstats.json" 'total_rows\": 5' "/csv_summary over HTTP" || return 1

  # SEARCH FILES, the production way: a rule hot-reloaded into the live
  # serve. The capture group is shape-guarded ([a-z0-9]+) so the bare $1
  # splice cannot break out of the JSON string; the LIKE value rides a
  # placeholder, honoring the sql_query policy. attach_csv columns come out
  # position-prefixed (c00_name, c01_product, ...), so the query addresses
  # c00_name rather than the CSV header "name".
  sq "$db" "INSERT INTO rules (name,pattern,tool,args_template,origin,enabled,created_ts,state) VALUES
    ('search_sales','/search sales ([a-z0-9]+)','sql_query',
     '{\"query\":\"SELECT * FROM sales WHERE c00_name LIKE ?\",\"args\":[\"%\$1%\"],\"attach_csvs\":[{\"name\":\"sales\",\"path\":\"data/sales.csv\"}]}',
     'manual',1,datetime('now'),'active'),
    ('read_log','/read log','read_doc','{\"path\":\"logs/app.log\"}','manual',1,datetime('now'),'active'),
    ('read_notes','/read notes','read_doc','{\"path\":\"notes.md\"}','manual',1,datetime('now'),'active');" \
    || { bad "inserting hot-reload rules"; return 1; }
  note "inserted search_sales + read_log + read_notes rules"

  s2_search_ok() {
    http_post "$url/v1/agent/run" '{"prompt":"/search sales alice"}' "$o-search.tmp" \
      && grep -q 'Widget' "$o-search.tmp"   # REST escapes inner quotes; bare token suffices
  }
  wait_for "hot reload picks up /search within one 2s interval" 15 s2_search_ok || return 1
  http_post "$url/v1/agent/run" '{"prompt":"/search sales alice"}' "$o-search.json" || return 1
  # Trailing comma anchors the count: SQLOutput always marshals rows_returned
  # before truncated, so `1,` cannot prefix-match `12,`.
  assert_contains "$o-search.json" 'rows_returned\": 1,' "/search returns exactly one row" || return 1
  assert_contains "$o-search.json" 'Widget' "/search returns alice's product" || return 1
  assert_contains "$o-search.json" '\"42\"' "/search returns alice's qty" || return 1

  http_post "$url/v1/agent/run" '{"prompt":"/read log"}' "$o-readlog.json" || return 1
  assert_contains "$o-readlog.json" "disk 91% full" "/read log surfaces the ERROR line" || return 1
  http_post "$url/v1/agent/run" '{"prompt":"/read notes"}' "$o-readnotes.json" || return 1
  assert_contains "$o-readnotes.json" "mango-tree-77" "/read notes reads the markdown file" || return 1
}

# =====================================================================
# S3 — HTTP endpoint matrix (sync | async | SSE | stats | 404 | trace echo)
# =====================================================================
s3() {
  local db="$DB/s3.db" o="$OUT/s3" url="http://127.0.0.1:$P_HTTP"
  local hn; hn=$(hostname)

  bg s3-serve env \
    AEGIS_ADDR="127.0.0.1:$P_HTTP" AEGIS_GRPC_ADDR="127.0.0.1:$P_GRPC" AEGIS_LLM=off \
    AEGIS_DB_PATH="$db" AEGIS_WORKSPACE="$WS" \
    AEGIS_RULES_RELOAD=2 AEGIS_MINER_INTERVAL=0 \
    "$REPO/bin/aegis-serve"
  S3_PID=$LAST_PID
  wait_for "serve A healthy on :$P_HTTP (gRPC on :$P_GRPC)" 20 \
    curl -sf "$url/healthz" || return 1

  curl -sf "$url/healthz" >"$o-healthz.json" || return 1
  assert_contains "$o-healthz.json" '"status":"ok"' "GET /healthz" || return 1
  curl -sf "$url/readyz" >"$o-readyz.json" || return 1
  assert_contains "$o-readyz.json" '"status":"ready"' "GET /readyz (store reachable)" || return 1

  # sync run + trace echo: the supplied X-Trace-Id is honored and echoed.
  curl -s -D "$o-sync.hdr" -o "$o-sync.json" \
    -H 'Content-Type: application/json' -H 'X-Trace-Id: e2e-s3-sync' \
    -d '{"prompt":"/hostname"}' "$url/v1/agent/run" || return 1
  # Status line, not a bare "200" (which Content-Length or a header value
  # could satisfy); curl -D writes it as the first header-dump line.
  assert_matches "$o-sync.hdr" '^HTTP/1\.1 200' "sync run answers HTTP 200" || return 1
  assert_contains "$o-sync.hdr" "e2e-s3-sync" "X-Trace-Id supplied by the caller is echoed" || return 1
  assert_contains "$o-sync.json" '"decision_source":"regex_router"' \
    "sync run deflected to the regex router" || return 1
  assert_contains "$o-sync.json" "e2e-s3-sync" "body carries the same trace_id" || return 1
  assert_contains "$o-sync.json" "$hn" "sync run returns this hostname" || return 1

  # router miss with the LLM off: fast llm_disabled answer, still 200.
  http_post "$url/v1/agent/run" '{"prompt":"explain quantum tunneling"}' "$o-miss.json" || return 1
  assert_contains "$o-miss.json" '"decision_source":"llm_disabled"' \
    "router miss reports llm_disabled" || return 1

  # async: 202 + trace, then poll the answer store until done.
  local code
  code=$(curl -s -o "$o-async.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' -d '{"prompt":"/disk"}' "$url/v1/agent/run?async=1") || return 1
  assert_eq "$code" "202" "async run answers 202" || return 1
  ASYNC_TRACE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["trace_id"])' "$o-async.json")
  [ -n "$ASYNC_TRACE" ] || { bad "async 202 carried no trace_id"; return 1; }
  note "async trace_id: $ASYNC_TRACE"
  s3_async_done() {
    curl -sf "$url/v1/answers/$ASYNC_TRACE" >"$o-answer.tmp" 2>/dev/null \
      && grep -q '"status":"done"' "$o-answer.tmp"
  }
  wait_for "async answer completes" 20 s3_async_done || return 1
  curl -sf "$url/v1/answers/$ASYNC_TRACE" >"$o-answer.json" || return 1
  assert_contains "$o-answer.json" "Filesystem" "async answer holds the /disk output" || return 1

  # SSE streaming: content-type + delta chunks + done event with metadata.
  curl -sN --max-time 20 -D "$o-sse.hdr" -o "$o-sse.txt" \
    -H 'Content-Type: application/json' -d '{"prompt":"/kernel"}' \
    "$url/v1/agent/run?stream=1" || return 1
  assert_contains "$o-sse.hdr" "text/event-stream" "SSE response content-type" || return 1
  assert_contains "$o-sse.txt" "event: delta" "SSE emits delta events" || return 1
  assert_contains "$o-sse.txt" "event: done" "SSE terminates with a done event" || return 1
  assert_contains "$o-sse.txt" "Darwin" "SSE body carries the kernel output" || return 1
  SSE_TRACE=$(python3 - "$o-sse.txt" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    if line.startswith("data: ") and "decision_source" in line:
        print(json.loads(line[6:])["trace_id"]); break
PY
)
  [ -n "$SSE_TRACE" ] || { bad "SSE done event carried no trace_id"; return 1; }
  note "SSE trace_id: $SSE_TRACE"

  # stats surface
  curl -sf "$url/v1/stats" >"$o-stats.json" || return 1
  python3 - "$o-stats.json" <<'PY' || { bad "/v1/stats counts are wrong"; return 1; }
import json, sys
s = json.load(open(sys.argv[1]))
assert s["total_runs"] >= 4, s
assert s["by_decision_source"].get("regex_router", 0) >= 3, s
assert s["by_decision_source"].get("llm_disabled", 0) >= 1, s
print("    ok: /v1/stats (runs=%d, router=%d, llm_disabled=%d)" % (
    s["total_runs"], s["by_decision_source"]["regex_router"],
    s["by_decision_source"].get("llm_disabled", 0)))
PY

  # unknown path 404s
  code=$(curl -s -o "$o-404.txt" -w '%{http_code}' "$url/v1/definitely-not-a-route") || return 1
  assert_eq "$code" "404" "unknown path answers 404" || return 1
}

# =====================================================================
# S4 — hot reload: new rule routes within ~one reload interval
# =====================================================================
s4() {
  local url="http://127.0.0.1:$P_HTTP"
  sq "$DB/s3.db" "INSERT INTO rules (name,pattern,tool,args_template,origin,enabled,created_ts,state) VALUES
    ('ping_rule','/ping','system_command','{\"command\":\"hostname\"}','manual',1,datetime('now'),'active');" \
    || { bad "inserting /ping rule"; return 1; }
  note "inserted ping_rule"

  s4_ping_ok() {
    http_post "$url/v1/agent/run" '{"prompt":"/ping"}' "$OUT/s4-ping.tmp" \
      && grep -q '"decision_source":"regex_router"' "$OUT/s4-ping.tmp"
  }
  wait_for "/ping routes within ~3s of INSERT" 15 s4_ping_ok || return 1
  http_post "$url/v1/agent/run" '{"prompt":"/ping"}' "$OUT/s4-ping.json" || return 1
  assert_contains "$OUT/s4-ping.json" 'command\": \"hostname\"' "/ping executes the new rule" || return 1

  curl -sf "$url/v1/stats" >"$OUT/s4-stats.json" || return 1
  local active
  active=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["rules_by_state"].get("active",0))' \
    "$OUT/s4-stats.json")
  assert_ge "$active" 10 "/v1/stats rules_by_state reflects the new rule" || return 1
}

# =====================================================================
# S5 — gRPC surface (same engine), via grpcurl reflection
# =====================================================================
s5() {
  local o="$OUT/s5"
  grpcurl -plaintext "127.0.0.1:$P_GRPC" list >"$o-list.txt" 2>"$o-list.err" \
    || { bad "grpcurl list failed"; cat "$o-list.err"; return 1; }
  assert_contains "$o-list.txt" "aegisgo.v1.Agent" "reflection lists aegisgo.v1.Agent" || return 1

  grpcurl -plaintext -d '{"prompt":"/hostname"}' "127.0.0.1:$P_GRPC" \
    aegisgo.v1.Agent/Run >"$o-run.json" 2>"$o-run.err" \
    || { bad "Agent/Run failed"; cat "$o-run.err"; return 1; }
  # grpcurl prints proto fields in lowerCamel (decisionSource).
  assert_contains "$o-run.json" '"decisionSource": "regex_router"' \
    "Agent/Run deflected to the regex router" || return 1

  grpcurl -plaintext -d '{}' "127.0.0.1:$P_GRPC" \
    aegisgo.v1.Agent/Ready >"$o-ready.json" 2>"$o-ready.err" \
    || { bad "Agent/Ready failed"; cat "$o-ready.err"; return 1; }
  assert_contains "$o-ready.json" '"ready": true' "Agent/Ready reports ready" || return 1

  # Unknown trace: NotFound, not an internal error and not a hang.
  if grpcurl -plaintext -d '{"trace_id":"e2e-bogus-trace"}' "127.0.0.1:$P_GRPC" \
      aegisgo.v1.Agent/GetAnswer >"$o-getanswer.json" 2>"$o-getanswer.err"; then
    bad "Agent/GetAnswer with a bogus trace unexpectedly succeeded"
    return 1
  fi
  cat "$o-getanswer.err" "$o-getanswer.json" >"$o-getanswer.all"
  assert_contains "$o-getanswer.all" "NotFound" \
    "Agent/GetAnswer with a bogus trace answers NotFound" || return 1
}

# =====================================================================
# S6 — external MCP server attach (stdio mcp-echo-server)
# =====================================================================
s6() {
  bg s6-serve env \
    AEGIS_ADDR="127.0.0.1:$P_AUX" AEGIS_GRPC_ADDR=none AEGIS_LLM=off \
    AEGIS_DB_PATH="$DB/s6.db" AEGIS_WORKSPACE="$WS" \
    AEGIS_MCP_SERVERS="stdio:$REPO/bin/mcp-echo-server" \
    AEGIS_MINER_INTERVAL=0 \
    "$REPO/bin/aegis-serve"
  S6_PID=$LAST_PID
  wait_for "MCP-attached serve healthy on :$P_AUX" 20 \
    curl -sf "http://127.0.0.1:$P_AUX/healthz" || return 1
  assert_contains "$OUT/s6-serve.log" '"mcp_tools":2' \
    "boot log reports the echo server's 2 MCP tools" || return 1
}

# =====================================================================
# S7 — Telegram interface against a python3 fake Bot API
# =====================================================================
write_fake_bot() {
  cat >"$OUT/fakebot.py" <<'PY'
# Fake Telegram Bot API for e2e: answers getMe, queues updates for
# getUpdates (mini long-poll so the client's loop stays paced), and records
# every sendMessage/editMessageText/setWebhook call to a JSONL file.
import json, os, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, SENT, QUEUE = int(sys.argv[1]), sys.argv[2], sys.argv[3]

def record(obj):
    with open(SENT, "a") as f:
        f.write(json.dumps(obj) + "\n")

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except ValueError:
            body = {}
        method = self.path.rsplit("/", 1)[-1]
        if method == "getMe":
            self.reply({"ok": True, "result": {"id": 1, "username": "e2e_bot"}})
        elif method == "getUpdates":
            time.sleep(1.0)  # mini long-poll: pace the client's loop
            updates = []
            if os.path.exists(QUEUE):
                try:
                    with open(QUEUE) as f:
                        updates = json.load(f)
                except ValueError:
                    updates = []
                with open(QUEUE, "w") as f:
                    f.write("[]")
            self.reply({"ok": True, "result": updates})
        elif method in ("sendMessage", "editMessageText", "setWebhook", "deleteWebhook"):
            rec = {"method": method}
            for k in ("chat_id", "text", "url", "secret_token"):
                if k in body:
                    rec[k] = body[k]
            record(rec)
            self.reply({"ok": True, "result": {"message_id": int(time.time() * 1000) % 100000}})
        else:
            self.reply({"ok": True, "result": True})

    def reply(self, obj):
        data = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY
}

s7() {
  local tg="http://127.0.0.1:$P_TG" sent="$OUT/tg-sent.jsonl" queue="$OUT/tg-queue.json"
  local hn; hn=$(hostname)

  write_fake_bot
  : >"$sent"
  cat >"$queue" <<'JSON'
[{"update_id": 1001, "message": {"message_id": 10, "chat": {"id": 424242, "type": "private"},
  "text": "/hostname", "from": {"id": 7, "username": "nick", "first_name": "Nick"}}}]
JSON

  bg tg-fake python3 "$OUT/fakebot.py" "$P_TG" "$sent" "$queue"
  TG_PID=$LAST_PID
  wait_for "fake Bot API up on :$P_TG" 10 \
    curl -sf -X POST "$tg/bote2e-token/getMe" -d '{}' || return 1

  # --- poll mode: real engine → dispatcher → sendMessage, zero ingress ---
  bg s7-poll env \
    AEGIS_ADDR="127.0.0.1:$P_AUX" AEGIS_GRPC_ADDR=none AEGIS_LLM=off \
    AEGIS_DB_PATH="$DB/s7poll.db" AEGIS_WORKSPACE="$WS" AEGIS_MINER_INTERVAL=0 \
    AEGIS_TELEGRAM_TOKEN=e2e-token AEGIS_TELEGRAM_CHATS=424242 \
    AEGIS_TELEGRAM_MODE=poll AEGIS_TELEGRAM_API_BASE="$tg" \
    "$REPO/bin/aegis-serve"
  S7_POLL_PID=$LAST_PID
  s7_poll_replied() { grep -q 'regex_router via hostname' "$sent"; }
  wait_for "poll mode delivers the update and replies /hostname" 30 s7_poll_replied || return 1
  assert_contains "$sent" "$hn" "the reply carries this machine's hostname" || return 1
  assert_contains "$sent" '"chat_id": 424242' "the reply went to the allowlisted chat" || return 1
  stop_bg "$S7_POLL_PID" s7-poll

  # --- webhook mode: secret-guarded endpoint, ack-then-process ----------
  mv "$sent" "$OUT/tg-sent-poll.jsonl"; : >"$sent"
  bg s7-web env \
    AEGIS_ADDR="127.0.0.1:$P_S7WEB" AEGIS_GRPC_ADDR=none AEGIS_LLM=off \
    AEGIS_DB_PATH="$DB/s7web.db" AEGIS_WORKSPACE="$WS" AEGIS_MINER_INTERVAL=0 \
    AEGIS_TELEGRAM_TOKEN=e2e-token AEGIS_TELEGRAM_CHATS=424242 \
    AEGIS_TELEGRAM_MODE=webhook AEGIS_TELEGRAM_WEBHOOK_URL="http://127.0.0.1:$P_S7WEB" \
    AEGIS_TELEGRAM_WEBHOOK_SECRET=e2e-secret AEGIS_TELEGRAM_API_BASE="$tg" \
    "$REPO/bin/aegis-serve"
  S7_WEB_PID=$LAST_PID
  wait_for "webhook serve healthy on :$P_S7WEB" 20 \
    curl -sf "http://127.0.0.1:$P_S7WEB/healthz" || return 1
  s7_webhook_set() { grep -q "\"url\": \"http://127.0.0.1:$P_S7WEB/telegram/webhook\"" "$sent"; }
  wait_for "setWebhook registered the secret-guarded endpoint" 15 s7_webhook_set || return 1
  assert_contains "$sent" '"secret_token": "e2e-secret"' "setWebhook carried the shared secret" || return 1

  # wrong secret → 401, before any inbox write
  local code
  code=$(curl -s -o "$OUT/s7-wrongsecret.txt" -w '%{http_code}' -X POST \
    -H 'X-Telegram-Bot-Api-Secret-Token: not-the-secret' \
    -H 'Content-Type: application/json' \
    -d '{"update_id":2000,"message":{"message_id":20,"chat":{"id":424242,"type":"private"},"text":"/uptime"}}' \
    "http://127.0.0.1:$P_S7WEB/telegram/webhook") || return 1
  assert_eq "$code" "401" "webhook with a wrong secret is rejected 401" || return 1

  # valid secret → 200 ack now, reply soon after (ack-then-process)
  code=$(curl -s -o "$OUT/s7-webhook.txt" -w '%{http_code}' -X POST \
    -H 'X-Telegram-Bot-Api-Secret-Token: e2e-secret' \
    -H 'Content-Type: application/json' \
    -d '{"update_id":2001,"message":{"message_id":21,"chat":{"id":424242,"type":"private"},"text":"/kernel"}}' \
    "http://127.0.0.1:$P_S7WEB/telegram/webhook") || return 1
  assert_eq "$code" "200" "webhook with the right secret is acked 200" || return 1
  s7_web_replied() { grep -q 'Darwin' "$sent"; }
  wait_for "webhook update processed → sendMessage reply" 20 s7_web_replied || return 1
  assert_contains "$sent" 'regex_router via kernel' "the webhook reply is a router hit" || return 1

  # unknown chats are never answered: allowlist denial, then no sendMessage.
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'X-Telegram-Bot-Api-Secret-Token: e2e-secret' \
    -H 'Content-Type: application/json' \
    -d '{"update_id":2002,"message":{"message_id":22,"chat":{"id":999999,"type":"private"},"text":"/uptime"}}' \
    "http://127.0.0.1:$P_S7WEB/telegram/webhook") || return 1
  assert_eq "$code" "200" "unknown-chat update is acked (no processing)" || return 1
  s7_denied() {
    [ "$(sq "$DB/s7web.db" \
      "SELECT status FROM telegram_inbox WHERE update_id=2002")" = "skipped" ]
  }
  wait_for "unknown chat marked skipped in the inbox" 15 s7_denied || return 1
  assert_absent "$sent" '"chat_id": 999999' \
    "no reply was ever sent to the unknown chat" || return 1

  stop_bg "$S7_WEB_PID" s7-web
  stop_bg "$TG_PID" tg-fake
}

# =====================================================================
# S8 — aegisctl admin ops (rules lifecycle, stats, replay)
# =====================================================================
s8() {
  local o="$OUT/s8"
  ctl() { AEGIS_DB_PATH="$1" "$REPO/bin/aegisctl" "${@:2}"; }

  # rules list + demote/promote against the S2 database (it holds the
  # hot-inserted search rule).
  ctl "$DB/s2.db" rules list >"$o-list.txt" 2>"$o-list.err" || { bad "aegisctl rules list"; return 1; }
  assert_contains "$o-list.txt" "NAME" "rules list prints a header" || return 1
  assert_contains "$o-list.txt" "uptime" "rules list shows seeded rules" || return 1
  assert_contains "$o-list.txt" "search_sales" "rules list shows the manual search rule" || return 1

  ctl "$DB/s2.db" rules demote search_sales >/dev/null 2>&1 || { bad "rules demote"; return 1; }
  ctl "$DB/s2.db" rules list >"$o-demoted.txt" || return 1
  assert_matches "$o-demoted.txt" '^search_sales[[:space:]]+demoted' \
    "demote flips state to demoted" || return 1
  ctl "$DB/s2.db" rules promote search_sales >/dev/null 2>&1 || { bad "rules promote"; return 1; }
  ctl "$DB/s2.db" rules list >"$o-promoted.txt" || return 1
  assert_matches "$o-promoted.txt" '^search_sales[[:space:]]+active' \
    "promote restores state to active" || return 1

  # stats + replay against the S3 database (REST traffic).
  ctl "$DB/s3.db" stats >"$o-stats.json" 2>"$o-stats.err" || { bad "aegisctl stats"; return 1; }
  python3 - "$o-stats.json" <<'PY' || { bad "stats deflection rate not > 0"; return 1; }
import json, sys
s = json.load(open(sys.argv[1]))
assert s["deflection_rate"] > 0, s
print("    ok: deflection_rate = %.2f" % s["deflection_rate"])
PY

  ctl "$DB/s3.db" replay e2e-s3-sync >"$o-replay.txt" 2>"$o-replay.err" \
    || { bad "replay of a known trace"; return 1; }
  assert_matches "$o-replay.txt" '[[:space:]]rest[[:space:]]+regex_router' \
    "replay shows the REST run as a router hit" || return 1

  if ctl "$DB/s3.db" replay e2e-no-such-trace >"$o-replay-bogus.txt" 2>&1; then
    bad "replay of an unknown trace should fail"
    return 1
  fi
  assert_contains "$o-replay-bogus.txt" "no audit rows" \
    "replay of an unknown trace errors clearly" || return 1
}

# =====================================================================
# S9 — miner: fallback corpus → shadow rule
# =====================================================================
s9() {
  local db="$DB/s9.db" o="$OUT/s9"
  # Boot the schema + seed once (aegisctl opens and migrates the DB).
  AEGIS_DB_PATH="$db" "$REPO/bin/aegisctl" rules list >"$o-boot.txt" 2>&1 \
    || { bad "booting s9 schema"; return 1; }
  sq "$db" "INSERT INTO fallback_events (ts,trace_id,normalized_prompt,raw_prompt,tools_used) VALUES
    (datetime('now'),'e2e-m9-1','summarize <path>','summarize notes/q3.md','read_doc'),
    (datetime('now'),'e2e-m9-2','summarize <path>','summarize notes/q4.md','read_doc'),
    (datetime('now'),'e2e-m9-3','summarize <path>','summarize notes/q1.md','read_doc');" \
    || { bad "seeding fallback corpus"; return 1; }
  note "seeded 3 same-shape fallback events (read_doc)"

  AEGIS_DB_PATH="$db" "$REPO/bin/aegisctl" rules mine --threshold 3 >"$o-mine.txt" 2>&1 \
    || { bad "aegisctl rules mine"; return 1; }
  assert_contains "$o-mine.txt" "shadow rule mined_1" "miner proposes shadow rule mined_1" || return 1

  local n
  n=$(sq "$db" "SELECT COUNT(*) FROM rules WHERE name='mined_1' AND state='shadow' AND origin='mined'")
  assert_eq "$n" "1" "rules table gains the mined_1 shadow row" || return 1

  AEGIS_DB_PATH="$db" "$REPO/bin/aegisctl" rules list >"$o-list.txt" || return 1
  assert_matches "$o-list.txt" '^mined_1[[:space:]]+shadow' "rules list shows mined_1 as shadow" || return 1
}

# =====================================================================
# S10 — audit trail joins (trace_id ↔ rows, decision_source mix, telegram)
# =====================================================================
s10() {
  local s3db="$DB/s3.db" s7db="$DB/s7poll.db"

  local n_sync n_async n_stream n_off n_router n_tg astat
  n_sync=$(sq "$s3db" "SELECT COUNT(*) FROM audit_events WHERE trace_id='e2e-s3-sync'")
  assert_ge "$n_sync" 1 "sync trace_id joins to an audit row" || return 1
  n_async=$(sq "$s3db" "SELECT COUNT(*) FROM audit_events WHERE trace_id='$ASYNC_TRACE'")
  assert_ge "$n_async" 1 "async trace_id joins to an audit row" || return 1
  n_stream=$(sq "$s3db" "SELECT COUNT(*) FROM audit_events WHERE trace_id='$SSE_TRACE'")
  assert_ge "$n_stream" 1 "SSE trace_id joins to an audit row" || return 1

  astat=$(sq "$s3db" "SELECT status FROM answers WHERE trace_id='$ASYNC_TRACE'")
  assert_eq "$astat" "done" "async answer row reached status=done" || return 1

  printf '    decision_source mix (s3.db):\n'
  sq "$s3db" "SELECT '      ' || decision_source || ' x' || COUNT(*) FROM audit_events GROUP BY decision_source ORDER BY 1"
  n_router=$(sq "$s3db" "SELECT COUNT(*) FROM audit_events WHERE decision_source='regex_router'")
  n_off=$(sq "$s3db" "SELECT COUNT(*) FROM audit_events WHERE decision_source='llm_disabled'")
  assert_ge "$n_router" 1 "audit rows include regex_router" || return 1
  assert_ge "$n_off" 1 "audit rows include llm_disabled" || return 1

  printf '    interface mix (s3.db):\n'
  sq "$s3db" "SELECT '      ' || interface || ' x' || COUNT(*) FROM audit_events GROUP BY interface"

  n_tg=$(sq "$s7db" "SELECT COUNT(*) FROM audit_events WHERE interface='telegram'")
  assert_ge "$n_tg" 1 "telegram interface row exists in the poll-mode DB" || return 1
}

# =====================================================================
# S11 — real LLM fallback via local Ollama (+ MCP echo tool invocation)
# =====================================================================
s11() {
  local db="$DB/s11.db" o="$OUT/s11"

  # A 0.5b model doing tool calls over OpenAI-compat gets room (240s),
  # bounded by run_watchdog so a hang fails loudly instead of stalling CI.
  # The prompt pins a marker ('e2e-tool-call'): the echo tool returns its
  # message verbatim, so a compliant final answer quotes the marker — the
  # stage proves the Ollama LLM really drove the MCP echo tool end-to-end.
  run_watchdog 240 env \
    AEGIS_LLM=on \
    AEGIS_PROVIDER=openai-compat \
    OPENAI_BASE_URL=http://localhost:11434/v1 \
    OPENAI_API_KEY=ollama \
    AEGIS_MODEL="$OLLAMA_MODEL" \
    AEGIS_MCP_SERVERS="stdio:$REPO/bin/mcp-echo-server" \
    AEGIS_DB_PATH="$db" AEGIS_WORKSPACE="$WS" AEGIS_MINER_INTERVAL=0 \
    "$REPO/bin/aegis-agent" \
    "Call the echo tool with the exact message 'e2e-tool-call', then reply with the exact text the tool returned." \
    >"$o-llm.out" 2>"$o-llm.err" || { bad "LLM run failed/timed out"; tail -5 "$o-llm.err"; return 1; }

  [ -s "$o-llm.out" ] || { bad "LLM answer is empty"; cat "$o-llm.err"; return 1; }
  note "LLM answered ($(wc -c <"$o-llm.out" | tr -d ' ') bytes)"

  local n_llm n_fb tools
  n_llm=$(sq "$db" "SELECT COUNT(*) FROM audit_events WHERE decision_source='llm'")
  assert_ge "$n_llm" 1 "audit row records decision_source=llm" || return 1
  n_fb=$(sq "$db" "SELECT COUNT(*) FROM fallback_events")
  assert_ge "$n_fb" 1 "fallback_events row persisted for the mining corpus" || return 1

  # The framework's own tool-call record is the authoritative proof: the
  # engine writes tools_used from the FunctionCallContent names it observed,
  # not from anything the model merely claims in prose. tools_used members
  # are comma-joined, so wrapping in commas makes the membership test
  # boundary-safe ("echo" cannot match "read_doc,echoplexus").
  tools=$(sq "$db" "SELECT COALESCE(GROUP_CONCAT(DISTINCT tools_used),'') FROM fallback_events")
  if [ -z "$tools" ]; then
    bad "LLM invoked no tools — expected the MCP echo tool (fallback_events.tools_used is empty)"
    sed -n '1,5p' "$o-llm.out" | sed 's/^/        | /'
    return 1
  fi
  case ",$tools," in
    *,echo,*) note "LLM invoked the MCP echo tool (tools_used: $tools)" ;;
    *) bad "MCP echo tool was NOT invoked — tools_used='$tools'"; return 1 ;;
  esac
  assert_contains "$o-llm.out" "e2e-tool-call" \
    "answer quotes the echoed marker 'e2e-tool-call'" || return 1
}

# =====================================================================
# workspace fixtures (shared by S1-S3, S6-S7, S11)
# =====================================================================
make_workspace() {
  cat >"$WS/data/sales.csv" <<'CSV'
name,product,qty
alice,Widget,42
bob,Gadget,7
carol,Sprocket,13
dave,Widget,99
erin,Gizmo,5
CSV
  cat >"$WS/logs/app.log" <<'LOG'
2026-09-01T09:00:01 INFO  api listening on :8080
2026-09-01T09:04:17 WARN  slow query 812ms
2026-09-01T09:05:02 ERROR disk 91% full on /data
2026-09-01T09:06:44 INFO  gc pause 3ms
LOG
  cat >"$WS/notes.md" <<'MD'
# Ops notes

Quarterly review checklist — access code mango-tree-77.
Rotate keys monthly. Keep the router seeded.
MD
}

# =====================================================================
# main
# =====================================================================
main() {
  printf 'aegisgo e2e — %s\n  repo %s\n  work %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$REPO" "$WORK"

  local busy="" hint="" p
  for p in $P_HTTP $P_GRPC $P_AUX $P_S7WEB $P_TG; do
    if port_busy "$p"; then busy="$busy $p"; fi
  done
  # lsof takes one port per -iTCP flag; build a paste-ready command that
  # names each busy port (multiple -i options are ORed).
  [ -z "$busy" ] || {
    for p in $busy; do hint="$hint -iTCP:$p"; done
    die "ports busy:$busy — a previous e2e run may still be alive (lsof -nP$hint -sTCP:LISTEN)"
  }

  make_workspace

  run_stage S0 "build + prerequisites" s0
  run_stage S1 "CLI router matrix (AEGIS_LLM=off)" s1
  run_stage S2 "workspace reads + hot-reloaded /search rule" s2
  stop_if_alive "$S2_PID" s2-serve   # free :18080 for serve A
  run_stage S3 "HTTP endpoint matrix" s3
  run_stage S4 "hot reload" s4
  if [ "$GRPCURL_OK" = "1" ]; then
    run_stage S5 "gRPC surface (grpcurl)" s5
  else
    skip_stage S5 "gRPC surface (grpcurl)" "grpcurl not installed (brew install grpcurl)"
  fi
  stop_if_alive "$S3_PID" s3-serve   # graceful drain before the DB is inspected
  run_stage S6 "MCP server attach" s6
  stop_if_alive "$S6_PID" s6-serve   # free :18082 for the poll-mode serve
  run_stage S7 "Telegram (fake Bot API: poll + webhook)" s7
  stop_if_alive "$S7_WEB_PID" s7-web
  stop_if_alive "$S7_POLL_PID" s7-poll
  stop_if_alive "$TG_PID" tg-fake
  run_stage S8 "aegisctl admin ops" s8
  run_stage S9 "miner demo" s9
  run_stage S10 "audit trail joins" s10
  if [ "$OLLAMA_OK" = "1" ]; then
    run_stage S11 "Ollama LLM fallback + MCP echo" s11
  else
    skip_stage S11 "Ollama LLM fallback + MCP echo" "no ollama model available (ollama pull qwen2.5:0.5b)"
  fi

  printf '\n==================== e2e summary ====================\n'
  local ids res i
  ids=($STAGE_IDS); res=($STAGE_RESULTS)
  for i in "${!ids[@]}"; do
    printf '%-6s %s\n' "${ids[$i]}" "${res[$i]}"
  done
  printf '%s\n' "----------------------------------------------------"
  if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL (%d stage(s) failed)\n' "$FAILS"
    exit 1
  fi
  printf 'RESULT: PASS (no failed stages)\n'
}

main "$@"
