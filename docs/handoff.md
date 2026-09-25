# Handoff — rolling state

> A fresh session reads CLAUDE.md, then THIS file, then continues.

## Current state (2026-09-02)

Single `aegis` binary shipped (agent · serve · ctl · version; logic in
internal/cli). Hybrid pipeline live: regex_router → optional
llm_classifier (AEGIS_CLASSIFIER) → llm, every run audited with a
decision_source. File tools (make_dir, list_dir, background download +
job_status) workspace-sandboxed and e2e-proven.

Repo-wide simplify/refactor pass COMPLETE on branch
`refactor/simplify-pass` (commits 52659e6..0ef4b98, one per cluster) —
including the closeout /simplify application (4 reviewers → deduped
findings applied): task.WaitFor bounded-join idiom (Group.Wait and
telegram PollLoop.Wait share it), store.FallbackShapes (typed method
owning the stats/miner GROUP BY), stats counts() helper, engine
tryClassifier + compareShadow owning its nil-LLM guard (compareInline
deleted), app.Build fail() boot-ladder closure, download .part cleanup
collapsed into one renamed-keyed defer, listDir slice+min, miner-grade
suffix reverse in resolveNewPath, loop.Periodic stop now JOINS an
in-flight tick (bounded 2s) so a fast tick cannot write into a store the
caller just closed. Three new stdlib-only helper packages — internal/logx
(slog build + nil-guards), internal/loop (Periodic), internal/task
(Group) — plus store.QueryAll[T], engine finish* tails, tools
optInt/optPos/openCSVReader/checkPathName, router compileAll. Net ≈
−300 non-test lines, +~400 test lines, zero new dependencies. All gates
green: make vet/test/check/cover (94.1%).

## Behavior changes in the pass (all deliberate, each a fix)

| # | Change | Pinned by |
|---|--------|-----------|
| BC1 | `?stream=1` shadow rules execute the tool ONCE, not twice | TestRunStreamingShadowExecutesToolOnce |
| BC2 | AEGIS_LOG_LEVEL is live; serve logs JSON on stdout incl. slog.Default lines | logx tests + cli serve wiring |
| BC3 | store stats/miner loops surface rows.Err() via store.QueryAll | store tests |
| BC4 | rules hot-reload stop() is idempotent (was: close-of-closed panic); stop also joins an in-flight tick (≤2s) | TestStartHotReloadStopIdempotent, TestPeriodicStopJoinsInFlightTick |
| BC5 | serve shutdown JOINS async runs (tasks.Wait 10s) — was a 500ms sleep | task tests + TestServeGracefulShutdown |
| BC6 | expired answers swept every 5min, not only lazily on read | TestDeleteExpiredAnswers |
| BC7 | idle telegram worker cadence 1s → 5s (Wake covers immediacy) | — (constant) |
| BC8 | streaming tools_used join is sorted (deterministic set) | engine tests |
| BC9 | telegram poll goroutine joined at shutdown (PollLoop.Wait) | TestPollLoopWaitJoinsShutdown |

Also: REST/gRPC async runs now carry the sync path's 5-minute bound
(completion still persists on a fresh context).

## In flight

- Nothing open. Closeout simplify review ran over the full branch diff,
  findings applied and committed.

## Next steps (candidates, in order)

1. REST/gRPC surface for job status (/v1/jobs/{id}) — today jobs are
   pollable only through agent prompts (deliberate; ADR if needed).
2. gRPC server-streaming Run (SSE-equivalent) — roadmap item.
3. Token/price accounting per audit row (Dify node-telemetry pattern).
4. Merge `refactor/simplify-pass` → main once reviewed.

## Deferred by the pass (do NOT blindly do later)

- Bounding telegram pool.Stop() itself — touches the claim-then-send net
  for a hang-only benefit.
- Batching Enqueue/ClaimReply into the store batcher (durability
  exceptions are documented in code).
- Consolidating store.batcher / telegram WorkerPool / PollLoop into one
  helper — drain and DB-is-the-queue semantics differ; forcing them in
  is a trap.
- Exporting writeJSON to internal/httpx (zero consumers), merging
  sseWrite into writeJSON (SSE legitimately differs).
- Full list with reasons: plan file jolly-sauteeing-candle.md "Skip list".

## Open questions / traps

- agent-framework-go is a pinned preview commit — API churn is the top
  breakage risk (see CLAUDE.md pinned deps; omitempty quirk in
  lessons-learned).
- Small-model stages are retry-bounded, not deterministic — keep the
  retries when touching scripts/e2e.sh s11.
- `make cover` can FAIL spuriously after test-cache invalidation
  mid-session; `go clean -testcache` then rerun. Real gate excludes
  internal/pb (generated stubs) via Makefile PKGS.

## Pointers

- Research: docs/research/2026-09-02-hermes-dify-classifier.md
- Decisions: docs/decisions/ADR-0001 (single binary), ADR-0002 (classifier)
- Lessons: docs/lessons-learned.md

---

## Brainstorm batch-4 (2026-09-23 malam — owner: "brainstorm lagi lanjut malam")

Queue terurut nilai/efort; sesi sore udah merged #25-32:

1. **/stats di Telegram (S)** — versi teks web dashboard: runs/deflection/by-source/rules-by-state/build. Dispatcher render dari statser yang udah ada. DoD: command + test + help text.
2. **aegis ctl jobs (S)** — inspect background download jobs (list/status). Manager udah ada di internal/tools/jobs.go. DoD: subcommand ctl + test.
3. **MCP bearer token utk HTTP/SSE transport (S→M)** — stdio lokal aman; remote perlu AEGIS_MCP_TOKEN + constant-time compare. DoD: config knob + middleware + test salah-token 401.
4. ~~reload_rules dari chat~~ ✅ CLOSED 25 Sep (merge #37): wiring udah ada sejak awal; E2E dispatcher test sekarang pin contract-nya. Follow-up opsi: bot live test /reload_rules via @KyociPersonalBot.
5. ~~SSE dashboard live~~ ✅ DONE 25 Sep (merge #36): /v1/events live-verified (regex_router event <1s); dashboard JS subscribe = follow-up.
6. **Rule mining v2 (M)** — multi-pattern synthesis dari corpus. Jaga invariant #12: hanya path-arg tools, divergence selalu demote. DoD: proposal format + test.
7. **Store WAL checkpoint tuning (M)** — ukur batcher p99 dulu; tuning hanya kalau data bilang perlu. DoD: benchmark script + hasil tercatat.

Urutan malam: 1 → 2 → 3 (semua S, cepat); 4-5 kalau waktu; 6-7 riset dulu.
Branch per fitur, changelog+ADR kalau keputusan baru, deploy build+kickstart tiap batch.

## Brainstorm batch-5 (2026-09-25 malam)

Audit queue batch-4: item 1 (/stats) dan 2 (ctl jobs) TERNYATA SUDAH ADA
sejak lama (dispatcher alias "/status","/stats" + `ctl jobs` + GET /v1/jobs) —
queue dicoret, jangan diusulkan lagi. Item 3 (MCP token) di-upgrade jadi
transport HTTP penuh. Queue malam ini:

1. **Dashboard live feed via SSE (S)** — follow-up resmi merge #36: page GET /
   subscribe /v1/events (EventSource), prepend baris run (source · latency),
   cap 8 baris, degrade diam tanpa JS. Meta-refresh 30s TETAP (stats angka
   hanya refresh via reload; SSE cuma bawa run events).
2. **/jobs di Telegram (S)** — lihat background download jobs dari chat:
   narrow source interface (lister) + SetJobs + help + render human (bukan
   JSON). DoD: command + test + help.
3. **MCP streamable HTTP transport + bearer token (M)** — mcp-go v0.58 sudah
   bawa server/streamable_http.go; expose `aegis mcp-server --http :7847`
   dengan AEGIS_MCP_TOKEN (constant-time compare; kosong = tolak remote,
   stdio tetap tanpa token). DoD: flag + config knob + test salah-token.
4. (Cadangan kalau cepat) **JobManager cancel (S→M)** — CancelJob(id) +
   context cancel di download.
