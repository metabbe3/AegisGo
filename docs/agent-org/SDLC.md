# AegisGo Agent Org — SDLC Process

Owner mandate 22 Sep 2026: full SDLC, continuous development, no deadline.
Repo: github.com/metabbe3/AegisGo (clone ~/Documents/aegisgo on Mac mini).

## Roles

- **CEO (strategy)** — picks next feature from blueprint backlog K1-K7 +
  gaps (docs/, L-tier policy engine, N declarative layer), writes task
  spec with DONE WHEN, dispatches to CTO queue.
- **CTO (builder)** — executes via the pipeline below, one feature per
  branch, verifies independently, merges.
- **QA (reviewer)** — post-merge verification: gates re-run, live smoke,
  report deltas.

## Pipeline per feature (NON-NEGOTIABLE)

1. `git checkout -b feat/<name>` from main — **never commit to main**.
2. Implement following CLAUDE.md hard rules (fixed-argv, path safety,
   single-writer batcher, decision_source contract, bounded output).
3. Verify BEFORE declaring done:
   - `make vet` — 0 findings
   - `make test` — all green
   - `make cover` — >= 90% (gate), regression vs previous = investigate
   - `make check` — no TODO/FIXME/skips
   - New behavior covered by a test that fails without the change
4. Update docs: CHANGELOG entry + WORKFLOW.md recipe if user-facing +
   ADR in docs/decisions/ if architectural.
5. `git add <narrow paths> && git commit -m "<area>: <what> (why: ...)"`
6. `git push origin feat/<name>`
7. Merge to main (fast-forward or merge commit), push main.
8. Post-merge QA smoke: `AEGIS_LLM=off go run ./cmd/aegis-agent "/uptime"`.

## Queue & State

- Queue file: ~/.hermes/data/aegisgo-growth/cto-queue.json (same schema
  as other orgs: id/date/spec/priority/north_star/status/result).
- Decisions log: docs/decisions/ + ~/.hermes/data/aegisgo-growth/
  ceo-decisions.md (mirror).
- Handoff: docs/agent-org/handoff.md — every session appends state/done/
  in-flight/traps/next-first-step.
- Changelog: CHANGELOG.md (Keep-a-Changelog format) — every merge.

## Scope gates

- New dependency: needs stated reason in commit + CLAUDE.md pinned-deps
  note (stdlib-first rule #1).
- security/auth/argv/policy changes: SEC-REVIEW line in the task spec
  BEFORE build (CLAUDE.md rules #2-#4 are the reference).
- Deploy = merge to main (repo IS production for a template). No server
  deploy needed until an integration consumes it.

## Backlog priority (from blueprint, owner-approved)

1. docs/ foundation (this branch) — ADR-0001 bootstrap + agent-org files.
2. CHANGELOG.md + lessons-learned.md seed.
3. L-tier command policy engine (blueprint L) — catalog + tier verdicts
   in audit rows.
4. N1 YAML command manifest (declarative commands, no function).
5. K4 HITL: `requires_approval` tool flag → Telegram pause/resume.
6. K2 memory tiers (episodic + semantic via sqlite-vec research).
7. K1 MCP client expansion + K6 prompt registry.
