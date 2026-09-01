// Package app assembles the hybrid pipeline shared by every AegisGo
// binary: store → tools → registry → router (DB-backed rules, hot reload)
// → MCP-extended LLM fallback. Commands stay thin.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/mcpclient"
	"aegisgo/internal/provider"
	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
)

// App is a fully wired hybrid agent.
type App struct {
	Engine *engine.Engine
	Store  *store.Store
	Config config.Config
}

// Build opens the store and assembles the engine. The returned cleanup
// stops the rules reloader, releases MCP connections, and closes the store.
func Build(ctx context.Context, cfg config.Config, tier config.Tier,
	iface string, logger *slog.Logger) (*App, func(), error) {

	if logger == nil {
		logger = slog.Default()
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening store: %w", err)
	}

	set, err := tools.Builtin(tools.Options{
		Workspace: cfg.WorkspaceDir(),
		SQL:       tools.SQLOptions{DSN: cfg.SQLDSN, Path: cfg.DBPath, Mode: cfg.SQLMode},
	})
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		st.Close()
		return nil, nil, err
	}

	mcpTools, releaseMCP, err := mcpclient.Connect(ctx, cfg.MCPServers)
	if err != nil {
		st.Close()
		return nil, nil, err
	}

	rules, err := router.LoadRules(ctx, st)
	if err != nil {
		releaseMCP()
		st.Close()
		return nil, nil, fmt.Errorf("loading rules: %w", err)
	}
	rt, err := router.New(reg, rules)
	if err != nil {
		releaseMCP()
		st.Close()
		return nil, nil, err
	}
	stopReload := router.StartHotReload(rt, st,
		time.Duration(cfg.RulesReloadSecs)*time.Second, logger)

	// Provider config only matters when the LLM fallback is switched on;
	// AEGIS_LLM=off boots a router-only agent with zero credentials.
	var llm engine.LLMRunner
	model := ""
	if !cfg.LLMDisabled() {
		if err := cfg.Validate(); err != nil {
			stopReload()
			releaseMCP()
			st.Close()
			return nil, nil, err
		}
		a, err := provider.New(cfg, tier, append(reg.FuncTools(), mcpTools...), logger)
		if err != nil {
			stopReload()
			releaseMCP()
			st.Close()
			return nil, nil, err
		}
		llm = a
		model = cfg.ModelFor(tier)
	} else {
		logger.Info("LLM fallback disabled (AEGIS_LLM=off) — router-only mode")
	}

	cleanup := func() {
		stopReload()
		releaseMCP()
		st.Close()
	}
	logger.Info("engine ready",
		"iface", iface,
		"llm", !cfg.LLMDisabled(),
		"model", model,
		"provider", cfg.Provider,
		"rules", len(rt.RuleDefs()),
		"builtin_tools", len(reg.All()),
		"mcp_tools", len(mcpTools),
		"workspace", cfg.WorkspaceDir(),
	)
	return &App{
		Engine: &engine.Engine{Router: rt, LLM: llm, Store: st, IFace: iface, Model: model},
		Store:  st,
		Config: cfg,
	}, cleanup, nil
}
