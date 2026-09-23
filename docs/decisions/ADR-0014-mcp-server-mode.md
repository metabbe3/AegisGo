# ADR-0014: MCP server mode — the native tool registry over MCP

Date: 2026-09-23
Status: accepted

## Context
AegisGo already speaks REST, gRPC, and Telegram, and can CONSUME MCP servers
(internal/mcpclient). The missing direction: exposing the native tools so
any MCP client (Claude Desktop, other agents) can call them — "one agent,
every interface".

## Decision
- `internal/mcpserver.New(reg)` wraps the existing *tools.Registry; every
  tool is registered under its own name.
- Input schema is a single required `input` string: the tool's JSON argument
  object. The Tool interface takes json.RawMessage — passing JSON-as-string
  is the honest mapping; the tool's own unmarshal validates.
- `aegis mcp-server` (internal/cli) serves it over stdio, the transport MCP
  clients conventionally spawn.
- Handler errors are MCP error results, never transport errors.

## Consequences
- Same security posture as the router path: fixed-argv system_command,
  read-only sql_query, resolvePath sandboxing — all inherited because the
  adapter calls the SAME tools, no reimplementation.
- Bounded tool listing comes free: the registry is human-curated.
- examples/mcp-echo-server remains the reference for third-party servers;
  internal/mcpserver is the real surface.
