# ADR-0002 — Dify-style fast-tier classifier on router misses

Date: 2026-09-02
Status: accepted

## Context

Research (docs/research/2026-09-02-hermes-dify-classifier.md) showed the
missing middle tier between the regex router (free, exact) and the smart
LLM (expensive, flexible): one cheap classify call that resolves
tool-shaped misses natively. Goal: known patterns decay from one cheap
call to zero LLM calls.

## Options considered

1. Do nothing — router + fallback only; misses always pay full price.
2. Classifier with tools attached — model answers via native function
   call; rejected: the framework self-executes malformed calls and the
   answer becomes an apology (lesson 2026-09-02, tool-less classifier).
3. Tool-less fast agent answering strict JSON; AegisGo executes the
   chosen tool itself through the schema-validated FuncTool path.

## Decision

Option 3, opt-in via AEGIS_CLASSIFIER=on (+ AEGIS_MODEL_FAST required;
warns and boots without otherwise). New decision_source llm_classifier;
classifier hits recorded as tools_used-labeled fallback corpus so the
miner graduates recurring shapes into regex rules; every failure mode
declines and falls through to the smart LLM — never an error path.

## Consequences

- Router → llm_classifier → llm ordering on every interface; kill
  switch stays absolute (AEGIS_LLM=off bypasses the classifier).
- Small-model JSON flakiness absorbed by robust parsing (<think>
  stripping, last-valid-object) plus bounded e2e retries.
- Revisit when: the classifier's decline rate on real traffic makes it
  pure overhead (watch /v1/stats by_decision_source).
