# Handoff — rolling state

> A fresh session reads CLAUDE.md, then THIS file, then continues.

## Current state (2026-09-02)

Single `aegis` binary shipped (agent · serve · ctl · version; logic in
internal/cli). Hybrid pipeline live: regex_router → optional
llm_classifier (AEGIS_CLASSIFIER) → llm, every run audited with a
decision_source. File tools (make_dir, list_dir, background download +
job_status) workspace-sandboxed and e2e-proven. All gates green:
make vet/test/check/cover (94%) and 13/13 e2e stages including the real
Ollama LLM + MCP-echo + classifier stage.

## In flight

- Nothing open. Working tree carries docs-only additions.

## Next steps (candidates, in order)

1. REST/gRPC surface for job status (/v1/jobs/{id}) — today jobs are
   pollable only through agent prompts (deliberate; ADR if needed).
2. gRPC server-streaming Run (SSE-equivalent) — roadmap item.
3. Token/price accounting per audit row (Dify node-telemetry pattern).

## Open questions / traps

- agent-framework-go is a pinned preview commit — API churn is the top
  breakage risk (see CLAUDE.md pinned deps; omitempty quirk in
  lessons-learned).
- Small-model stages are retry-bounded, not deterministic — keep the
  retries when touching scripts/e2e.sh s11.

## Pointers

- Research: docs/research/2026-09-02-hermes-dify-classifier.md
- Decisions: docs/decisions/ADR-0001 (single binary), ADR-0002 (classifier)
- Lessons: docs/lessons-learned.md
