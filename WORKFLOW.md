# WORKFLOW.md — How Work Happens Here

## Everyday Loop

1. Read CLAUDE.md rules + PRODUCT.md scope. If a change fights the scope
   boundaries, stop and discuss before coding.
2. Branch or worktree off main (keep main always deployable).
3. Make the change: internal/ logic first, thin cmd/ wiring, then tests.
4. `make vet && make test` — green or it doesn't ship.
5. Commit with a `why:`-oriented message; note any dependency bumps.

## Task Recipes

### Add a builtin tool

1. Create `internal/tools/<name>tool.go`: define `In`/`Out` structs (field
   comments become the schema description the LLM reads), build with
   `functool.New`, bind to a workspace root, resolve paths via `resolvePath`.
2. Append it in `internal/tools/registry.go` — never reorder existing tools.
3. Unit test against `testdata/` (or a temp dir): happy path, bounds, and a
   workspace-escape rejection.
4. If it replaces or renames a tool, update CLAUDE.md's architecture map.

### Add an LLM provider

1. Add a constant + validation branch in `internal/config`.
2. Add a case in `internal/provider.New` constructing the provider client
   from env config. Everything else (tools, MCP, serving) is provider-blind.
3. Smoke-test with real creds locally in a shell (`AEGIS_*` env), never in
   committed tests.

### Attach an external MCP server

No code. Set, e.g.:

```bash
AEGIS_MCP_SERVERS="stdio:/usr/local/bin/some-mcp-server --flag,https://mcp.internal:8443/mcp"
```

stdio specs run a subprocess; http(s) URLs use streamable HTTP. Startup fails
fast if any server is unreachable — intentional (see connect.go).

### Expose your own tools as an MCP server

Copy `examples/mcp-echo-server/` — it is the reference for the mcp-go server
side (tool schema, handlers, stdio transport) that other agents can consume.

### Bump dependencies

`mcp-go`: prefer stable tags. `agent-framework-go`: pinned to a commit (no
tags upstream); after bumping, check constructor signatures against
`reference/agent-framework-go` and run the full gate plus one real run.

## Release / Deploy (Linux)

```bash
make build                     # or: GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/...
scp bin/aegis-serve user@host:~/
```

`deploy/aegis-serve.service` is a systemd unit template:

```bash
sudo cp deploy/aegis-serve.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now aegis-serve
```

Keep provider env vars in the unit's `Environment=` lines or a
`/etc/aegisgo.env` file — never in the repo.

## Reference Repos

`make clone-refs` (or the initial setup) shallow-clones both upstream
libraries under `reference/`. They are gitignored and **read-only**:

- consult their `examples/` when framework APIs churn;
- never import from `reference/` — dependencies come from go.mod;
- refresh with `git -C reference/<repo> pull` when needed.

## Commit / PR Conventions

- Subject: imperative, ≤72 chars. Body: what + why, mention env var or
  behavior changes.
- Dependency bumps are their own commit, never mixed with features.
- Anything touching `internal/tools` path safety or `internal/config`
  validation deserves an extra careful re-read — those are the security and
  fail-fast boundaries.
