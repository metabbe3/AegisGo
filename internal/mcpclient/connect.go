package mcpclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/microsoft/agent-framework-go/tool"
)

// clientIdentity is the implementation name AegisGo advertises during MCP
// initialization.
const clientIdentity = "aegisgo"

// Connect dials every configured MCP server and returns the adapted tools
// plus a release function that closes all connections. Servers are
// addressed by spec:
//
//	stdio:<command> [args...]   launch a local subprocess speaking MCP over stdio
//	http://host/mcp             connect to a streamable HTTP MCP endpoint
//
// Failing servers abort startup with a descriptive error: an agent that
// silently runs without half its tools is worse than one that fails fast.
func Connect(ctx context.Context, specs []string) ([]tool.Tool, func(), error) {
	if len(specs) == 0 {
		return nil, func() {}, nil
	}

	var tools []tool.Tool
	var clients []*client.Client
	release := func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}

	for _, spec := range specs {
		cli, err := dial(ctx, spec)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("mcp server %q: %w", spec, err)
		}
		clients = append(clients, cli)

		adapted, err := AdaptAll(ctx, cli)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("mcp server %q: %w", spec, err)
		}
		tools = append(tools, adapted...)
	}
	return tools, release, nil
}

func dial(ctx context.Context, spec string) (*client.Client, error) {
	var (
		cli *client.Client
		err error
	)
	switch {
	case strings.HasPrefix(spec, "stdio:"):
		rest := strings.TrimSpace(strings.TrimPrefix(spec, "stdio:"))
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("stdio spec needs a command")
		}
		cli, err = client.NewStdioMCPClient(fields[0], nil, fields[1:]...)
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		cli, err = client.NewStreamableHttpClient(spec)
	default:
		return nil, fmt.Errorf("unsupported spec (want stdio:<command...> or an http(s) URL)")
	}
	if err != nil {
		return nil, err
	}

	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: clientIdentity, Version: "0.1.0"}
	if _, err := cli.Initialize(ctx, req); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return cli, nil
}
