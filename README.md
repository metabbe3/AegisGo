# AegisGo

Reusable Go template for **lightweight AI agents on Linux servers**: one
static binary that reads workspace files (CSV/docs), calls external tools
over **MCP**, talks to any major LLM provider via **environment variables**,
and serves other microservices over REST.

Built on [microsoft/agent-framework-go] (agent loop) and [mark3labs/mcp-go]
(MCP client). MIT licensed.

## Quickstart

```bash
make build          # bin/aegis-agent + bin/aegis-serve
make test           # no keys or network needed
```

Ask a question about the sample CSV:

```bash
export AEGIS_PROVIDER=openai AEGIS_MODEL=gpt-4o-mini OPENAI_API_KEY=sk-...
bin/aegis-agent "Summarize testdata/sample.csv: total revenue by shipped status"
```

Or with a free local model via [Ollama]:

```bash
export AEGIS_PROVIDER=openai-compat AEGIS_MODEL=llama3
export OPENAI_API_KEY=ollama OPENAI_BASE_URL=http://localhost:11434/v1
bin/aegis-agent "What is the most ordered item in testdata/sample.csv?"
```

Run as a service for your microservices:

```bash
AEGIS_ADDR=:8080 bin/aegis-serve &
curl -s localhost:8080/v1/agent/run -d '{"prompt":"How many rows in testdata/sample.csv?"}'
# {"output":"10 ..."}
```

## Configuration (env)

| Variable | Meaning |
|---|---|
| `AEGIS_PROVIDER` | `openai` \| `openai-compat` \| `anthropic` \| `foundry` |
| `AEGIS_MODEL` | default model id (deployment name for Foundry) |
| `AEGIS_MODEL_FAST` / `AEGIS_MODEL_SMART` | per-tier models; `--tier fast` picks the cheap one |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | OpenAI creds; base URL enables Ollama/OpenRouter/vLLM |
| `ANTHROPIC_API_KEY` | Anthropic creds |
| `FOUNDRY_ENDPOINT` | Foundry project endpoint (auth via `azidentity`) |
| `AEGIS_WORKSPACE` | root file tools may read from (default: cwd) — security boundary |
| `AEGIS_MCP_SERVERS` | comma list: `stdio:<command args>` or `https://host/mcp` |
| `AEGIS_INSTRUCTIONS` | override the system prompt |

## MCP

AegisGo **consumes** MCP servers at startup; every remote tool becomes a
native agent tool:

```bash
export AEGIS_MCP_SERVERS="stdio:./bin/mcp-echo-server,https://mcp.example.com/mcp"
```

Try it with the included example server:

```bash
go build -o /tmp/mcp-echo ./examples/mcp-echo-server
AEGIS_MCP_SERVERS="stdio:/tmp/mcp-echo" bin/aegis-agent ...
# logs: agent ready ... mcp_tools=2 builtin_tools=3
```

To **expose your own** Go tools as an MCP server for other agents, copy
`examples/mcp-echo-server/` — it shows the mcp-go server side (tool schemas,
handlers, stdio transport).

## Layout & Docs

- `CLAUDE.md` — rules for AI agents working on this repo (architecture map,
  hard rules, pinned-dependency risks)
- `PRODUCT.md` — vision, scope, cost strategy, roadmap
- `WORKFLOW.md` — dev loop, recipes (add a tool / provider / MCP server),
  Linux deployment
- `deploy/aegis-serve.service` — hardened systemd unit template

`reference/` (gitignored, `make clone-refs`) holds shallow clones of both
upstream libraries for offline reading.

## Linux Deployment

```bash
GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/...
```

Copy `deploy/aegis-serve.service` to `/etc/systemd/system/`, set the
provider env vars, `systemctl enable --now aegis-serve`. See WORKFLOW.md.

[microsoft/agent-framework-go]: https://github.com/microsoft/agent-framework-go
[mark3labs/mcp-go]: https://github.com/mark3labs/mcp-go
[Ollama]: https://ollama.com
