# How should the hybrid pipeline cut LLM cost further — what do Hermes Agents and Dify teach?

Date: 2026-09-02
Status: answered → shipped as the AEGIS_CLASSIFIER tier + the single binary

## Findings (primary sources)

**Hermes Agents (NousResearch, MIT)** is single-tier: every message pays
an LLM call plus a large fixed prompt — SOUL.md identity + context files
with 20,000-char floors, 60–70 tool schemas in context, full conversation
replay. Its only relief (context compression) triggers at 50% of the
context window and is itself an LLM pass. Mitigations (prompt-cache
discipline, delegate_task subagents, tool_search deferral) discount the
per-message toll; none remove it.
— hermes-agent.nousresearch.com/docs (architecture, prompt assembly,
context compression & caching, tool search); github.com/NousResearch/hermes-agent

**Dify (langgenius/dify)** is two-tier by construction: a cheap-model
Question-Classify node (one fixed compact prompt, exactly one LLM call,
robust JSON parsing with a deterministic fallback class) routes to
branches; known paths run deterministic nodes at zero LLM cost; expensive
models are confined to nodes that genuinely generate; every node records
token/price telemetry.
— docs.dify.ai (workflow-chatflow, question-classifier, if/else, agent
nodes); github.com/langgenius/dify `dify_graph/nodes/question_classifier`

## What this means for us

AegisGo's router→fallback split was already Dify-shaped; the missing
piece was the middle tier. Borrowed: the classifier tier
(AEGIS_CLASSIFIER=on, decision_source=llm_classifier) with Dify's
deterministic-fallback lesson (any failure falls through to the smart
LLM, never errors) and byte-stable prompts for cache friendliness.
Rejected: Hermes-style per-message context/persona injection. Confirmed
by the ship: classifier hits are tool-choice-labeled corpus, so recurring
shapes graduate into regex rules — spend falls as traffic grows.

Full summary also lives in PRODUCT.md → "Prior art".
