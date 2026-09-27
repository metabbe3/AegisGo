# ADR-0015: MCP streamable-HTTP transport with mandatory bearer gate

Date: 2026-09-27
Status: accepted

## Context

ADR-0014 shipped `aegis mcp-server` over stdio — fine for local clients
(Claude Desktop spawns the process), useless for anything remote. Batch-5
queue item 3 asked for the streamable-HTTP transport from mcp-go v0.58
plus a token, because an HTTP listener is reachable by everything on the
network, not just the process that spawned us.

## Decision

1. `aegis mcp-server --http <addr>` serves the SAME `mcpserver.New`
   registry via `server.NewStreamableHTTPServer`, stateless mode (every
   request is a self-contained JSON-RPC frame; AegisGo tools hold no
   session state worth keeping).
2. `AEGIS_MCP_TOKEN` is REQUIRED for `--http`. Empty token = the command
   refuses to start, naming the env var. This deliberately differs from
   `AEGIS_HTTP_TOKEN` (empty = open LAN): the REST surface is read-mostly
   admin, while the MCP surface can drive `system_command` — refusing
   beats exposing it ungated. stdio stays token-free: the spawning client
   is already local.
3. The gate is a wrapper `http.Handler` (constant-time `Bearer` compare,
   401 before any JSON-RPC frame parses), so the transport and the gate
   stay independently testable.

## Consequences

- Remote MCP clients configure the endpoint URL plus one bearer token;
  no OAuth dance needed for the single-operator deployment we have.
- mcp-go's built-in DNS-rebinding protection stays active (it runs inside
  the wrapped handler).
- If multi-client deployments ever need sessions/streaming GET, stateful
  mode is one option flip away — not needed today.
