# docs/ — the memory that survives /clear

Chat context is rented; the repo is owned. This folder holds what a fresh
session needs after `/clear`: research already done, decisions already
made, mistakes already paid for.

**The /clear contract:** CLAUDE.md tells a new session the rules;
`docs/handoff.md` tells it where the work stands. Read CLAUDE.md →
handoff.md → continue.

| Path | What lands there | When |
|---|---|---|
| `research/` | One file per question, findings with source links | During research |
| `lessons-learned.md` | Append-only: symptom → cause → the rule it became | After every surprise |
| `decisions/` | One ADR per real fork in the road | When a decision would otherwise be re-litigated |
| `handoff.md` | Rolling state: current, next steps, traps | When a session ends mid-work |

Rules of the folder: append, don't rewrite · date everything · cite
sources · every claim here must be verifiable in the repo.
