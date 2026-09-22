# ADR-0001: Agent-org SDLC takes over development

- **Status**: Accepted
- **Date**: 2026-09-22
- **Context**: Owner (metabbe3) handed development of AegisGo to the
  Hermes agent org: "full SDLC, continuous development, no deadline,
  production-ready + richer features, real-case testing, clean reusable
  code" (owner decision log, blueprint-v1). The repo already ships a
  strong base: hybrid pipeline (router → LLM fallback), audit trail,
  single-writer SQLite, 15 packages tested, 94.3% coverage, gate 90%.
- **Decision**:
  1. Every feature = new branch `feat/<name>`; all work lands on the
     branch; verified; pushed; merged to main. No direct commits to main.
  2. `make vet && make test && make cover && make check` are the referee
     before every merge (CLAUDE.md DoD stays authoritative for code rules).
  3. docs/ becomes source of truth: decisions (ADR), agent-org handoff,
     CHANGELOG.md per merge (blueprint J).
  4. Backlog order starts with docs foundation, then L-tier command
     policy engine, N1 declarative commands, K4 HITL gate, K2 memory
     tiers (blueprint K/L/N, owner-approved).
- **Consequences**: git history stays reviewable per feature; rollback =
  revert one merge; agents have an auditable trail (branch → gates →
  merge) that matches the audit-trail philosophy of the product itself.
