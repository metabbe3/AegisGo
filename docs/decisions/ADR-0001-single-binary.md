# ADR-0001 — one `aegis` binary instead of three

Date: 2026-09-02
Status: accepted

## Context

Three entry points (aegis-agent, aegis-serve, aegisctl) shipped as three
binaries with duplicated wiring. Deployments copied multiple artifacts;
docs, e2e, and the systemd unit tracked three names; the shared pipeline
(internal/app) was the only real commonality. User ask: one lightweight
binary in the Dify spirit — known patterns cheap, LLM only for
complexity.

## Options considered

1. Keep three binaries — familiar, zero migration, but three artifacts
   and three doc/e2e surfaces forever.
2. Thin wrappers kept as aliases — backward compatible, but three
   untested main packages fighting the make-check gate.
3. One dispatcher binary (`aegis agent|serve|ctl|version`) with all
   logic in a tested internal/cli package.

## Decision

Option 3. `cmd/aegis` is a 4-line shim; agent/serve/ctl moved verbatim
into internal/cli behind Main(ctx, args, stdout, stderr) int with
per-subcommand FlagSets (no global flag state).

## Consequences

- One artifact to deploy; subcommands share flags and conventions.
- Old names gone repo-wide (grep-verified); systemd ExecStart carries
  ` serve`; user-visible deltas: unified `aegis:` error prefix, ctl
  usage re-prefixed.
- Revisit when: a subcommand needs its own release cadence or build
  tags that bloat the shared binary.
