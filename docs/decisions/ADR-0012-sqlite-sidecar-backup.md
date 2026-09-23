# ADR-0012: Daily SQLite sidecar backup

Date: 2026-09-23
Status: accepted

## Context
The single embedded SQLite file holds audit trail, rules, approvals, inbox,
and fallback corpus — the entire organizational memory of a running AegisGo
instance. A corrupt or accidentally deleted file is unrecoverable memory
loss. Backups were previously a manual affair.

## Decision
- Daily `VACUUM INTO` snapshot at 04:30 local into
  `<db>.backup-YYYYMMDD.sidecar` next to the DB file.
- Retention: newest 7 files (prune best-effort, never fails the backup).
- Same-day reruns are idempotent (destination removed first — VACUUM INTO
  refuses existing files).
- Scheduled at 04:30 to avoid colliding with the 07:00 digest read burst.
- `backupOnce` takes a minimal logger interface so tests run without slog.

## Consequences
- VACUUM INTO runs OUTSIDE the single-writer batcher (it cannot run inside
  a transaction), so the backup is online-safe while the batcher writes.
- Restores are a file copy with the service stopped.
- Failures are logged; repeated failures surface indirectly via digest
  staleness.
