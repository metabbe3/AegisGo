# CLAUDE.md — Working Rules for AI Agents on This Repo

Rules for Claude Code (and any AI assistant) contributing to AegisGo. Follow
these unless the user explicitly overrides them. See PRODUCT.md for what the
project is, WORKFLOW.md for how changes are made.

## Commands

```bash
make build        # compile bin/aegis-agent and bin/aegis-serve
make test         # go test ./...          — no network, no API keys needed
make vet          # go vet ./...
go run ./cmd/aegis-agent "question"    # CLI agent (env-configured)
go run ./cmd/aegis-serve              # HTTP service on :8080
```

Run `make vet && make test` before declaring any change done. Both must pass.
Tests must never require network access or real API keys; fake providers or
the in-process MCP transport instead.

## Architecture Map

```
cmd/aegis-agent        CLI: one-shot prompt or interactive REPL
cmd/aegis-serve        long-running HTTP service (POST /v1/agent/run, GET /healthz)
internal/config        env parsing, validation, model-tier resolution (fast/smart)
internal/tools         builtin file tools: read_csv, csv_stats, read_doc
internal/mcpclient     mark3labs/mcp-go client + adapter to framework tools
internal/provider      env-switched agent factory (openai | openai-compat | anthropic | foundry)
internal/server        stdlib net/http handlers wrapping the agent
examples/mcp-echo-server   reference for the MCP SERVER side (mcp-go)
testdata/              fixtures used by unit tests
reference/             gitignored shallow clones of upstream libs — read, never import
```

Data flow: cmd → config.Load → tools.Builtin + mcpclient.Connect → provider.New
→ agent.RunText(...).Collect(). The HTTP layer wraps the same agent; CLI and
serve share everything below internal/.

## Hard Rules

1. **Stdlib-first.** No new dependency without a stated reason in the PR/commit
   text. `encoding/csv`, `net/http`, `log/slog` cover most needs.
2. **Workspace path safety is a security boundary.** Every file tool must
   resolve paths through `internal/tools.resolvePath`, which is symlink-aware
   and rejects anything escaping the workspace. Never bypass it; never add a
   file-reading tool that doesn't use it.
3. **Configuration is env-only.** New knobs go in `internal/config` with an
   `AEGIS_*` (or the provider's standard, e.g. `OPENAI_*`) env var, a default,
   and validation in `Config.Validate`. No flags for things env should own.
4. **Tools are `functool.New` generics.** Input/output schemas derive from the
   `In`/`Out` structs — keep field comments one-line and model-readable; they
   become the tool description the LLM plans with. Register new tools in
   `tools.Builtin` (append, never reorder).
5. **Bounded tool output.** Cap anything a tool returns (see `docHardCap`).
   Unbounded file reads blow the model context.
6. **MCP goes through the bridge.** External MCP servers are adapted in
   `internal/mcpclient`; don't import `modelcontextprotocol/go-sdk` here even
   though agent-framework-go uses it internally — mark3labs/mcp-go is this
   project's MCP dependency by decision.
7. **Providers are selected at runtime.** `internal/provider.New` switches on
   config; adding a provider means a new case there plus config plumbing.
   Never hardcode a model or key.

## Pinned Dependencies & Known Risks

- `github.com/mark3labs/mcp-go v0.58.0` — stable tag. Prefer tags over @main.
- `github.com/microsoft/agent-framework-go v0.0.0-20260901091852-de5a4c162072`
  — **public preview, no release tags**; pinned to a commit. Upstream API
  churn is the top breakage risk for this repo. If the build breaks after a
  bump, check `reference/agent-framework-go` for the new signatures (tool
  interface, provider constructors, `RunText`/`Collect`).
- When bumping either, run `make vet && make test` and exercise one real
  agent run before committing.

## Style

- Go 1.26, standard `gofmt` formatting, package comments on internal packages.
- Errors wrap with `%w` and start lowercase. User-facing CLI/server errors say
  which env var is missing.
- Keep binaries thin (cmd/ wires packages; logic lives in internal/).
- Comments explain *why*, especially around security and provider quirks.

## Definition of Done (per change)

1. `make vet && make test` green.
2. New behavior covered by a test that fails without the change.
3. Docs touched if behavior/env vars/workflow changed (README, WORKFLOW.md).
4. No secrets, no `.env` files, no `reference/` paths imported in code.
