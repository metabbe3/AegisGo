# Lessons Learned

Append-only. One entry per surprise. Never edit old entries.

---

## 2026-09-02 — functool rejects omitempty and nil maps in tool outputs

**Symptom:** using the Job struct directly as a tool output failed every
FuncTool().Call with "cannot apply defaults to a struct"; a nil
map[string]string field failed schema validation ("null, want object").

**Cause:** the pinned agent-framework version derives a JSON schema from
the output type; applyDefaults errors on any non-required (omitempty)
property of a struct output, and nil maps fail validation.

**Rule now:** tool output structs carry no omitempty, and map fields are
always emitted non-nil (see JobStatusOutput in internal/tools/jobs.go —
the comment and shape pin this; the parity tests keep it honest).

---

## 2026-09-02 — validation that skips a "safe mode" never runs when it matters

**Symptom:** a negative AEGIS_DOWNLOAD_TIMEOUT was accepted silently in
router-only runs (AEGIS_LLM=off) but rejected in normal runs.

**Cause:** cfg.Validate() only runs on the LLM-enabled boot path — the
exact mode the e2e suite exercises skipped every check.

**Rule now:** safety validation lives in the layer that always runs
(internal/tools DownloadOptions.withDefaults errors loudly in every
mode), not in a mode-gated one.

---

## 2026-09-02 — resolveNewPath must rebuild from the resolved ancestor

**Symptom:** every resolveNewPath call on macOS temp dirs would have
failed containment: t.TempDir() lives under /var → /private/var, so the
raw joined path never prefixes the EvalSymlinks'd root.

**Cause:** containment was first written as a string-prefix check on the
raw candidate.

**Rule now:** walk to the deepest existing ancestor, EvalSymlinks THAT,
check containment, then rejoin the non-existent suffix onto the resolved
ancestor (internal/tools/pathutil.go). Pinned by the symlinked-root test.

---

## 2026-09-02 — the fast-tier classifier agent must be tool-less

**Symptom:** with tools attached, the fast agent answered classification
with an apology ("this function did not work due to an unexpected
additional property") instead of JSON — the framework had self-executed
the model's malformed envelope-shaped call and fed the error back.

**Cause:** giving the classify agent the tool schemas invites native
function calls; the model wrapped its classify-JSON INSIDE the call
arguments; the framework executed and rejected it.

**Rule now:** build the classifier agent with nil tools — pure
text-in/JSON-out; AegisGo itself executes the chosen tool through the
schema-validated FuncTool path (internal/app/classifier.go).

---

## 2026-09-02 — small-model e2e stages need bounded retries

**Symptom:** the Ollama 0.5b stage intermittently dropped the echoed
marker or the classify JSON; a single-shot assertion failed roughly one
run in three.

**Cause:** small models flake on format compliance — correct behavior,
unreliable execution.

**Rule now:** verification stages against small models retry a bounded
number of times and assert on any success (scripts/e2e.sh s11); a
decline is correct engine behavior, not a bug.

---

## 2026-09-02 — background jobs: unique partials, and retention tests count completions

**Symptom (avoided by review):** two downloads to one target would share
a .part file; and the retention test first drafted polled evicted job
ids forever because the retention cap legitimately removes them.

**Cause:** shared temp naming + testing eviction by polling the evicted.

**Rule now:** partials are per-job temp files (os.CreateTemp) renamed on
success; retention tests count fn completions (atomic counter) instead of
polling ids the cap may have already reclaimed (internal/tools).

### 2026-09-25 (session #36 #37)
- **Test SSE handler dengan cancel eksplisit**: handler SSE by design blocking sampai client disconnect — test tanpa context cancel = hang sampai timeout go test. Selalu: `ctx, cancel := context.WithCancel(req.Context())` + cancel dari goroutine. (Test yang hang sempat bikin package timeout 120s.)
- **`trace.New` return (id, ctx) dua nilai** — bukan ctx saja. Test baru yang salah asumsi langsung build fail; cek signature sebelum tulis test.
- **engine.WrapRun: satu wrapper satu owner** — double-wrap panic by design, biar ordering surprise gak invisible. Middleware pattern tanpa import cycle (engine gak tahu SSE ada).
- **GLM flash via Z.ai anthropic-compat verified**: ANTHROPIC_BASE_URL=api.z.ai/api/anthropic + AEGIS_MODEL=glm-5.3-flash jalan (rc 0, 4.6s). Flash mikir kelamaan buat prompt simple (thinking block) — cocok classifier/fallback, bukan jalur interaktif.
