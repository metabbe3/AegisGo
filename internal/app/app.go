// Package app assembles the hybrid pipeline shared by every AegisGo
// binary: store → tools → registry → router (DB-backed rules, hot reload)
// → MCP-extended LLM fallback. Commands stay thin.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aegisgo/internal/config"
	"aegisgo/internal/engine"
	"aegisgo/internal/logwatch"
	"aegisgo/internal/logx"
	"aegisgo/internal/mcpclient"
	"aegisgo/internal/miner"
	"aegisgo/internal/provider"
	"aegisgo/internal/router"
	"aegisgo/internal/server"
	"aegisgo/internal/store"
	"aegisgo/internal/telegram"
	"aegisgo/internal/tools"
	"aegisgo/internal/trace"
)

// App is a fully wired hybrid agent.
type App struct {
	Engine *engine.Engine
	Store  *store.Store
	Config config.Config
	// Registry is the native tool set the router serves; cmd surfaces it
	// to MCP clients (internal/mcpserver, ADR-0014).
	Registry *tools.Registry
	// Jobs is the shared background-job manager (downloads); serve
	// exposes it at GET /v1/jobs.
	Jobs *tools.JobManager
	// Webhook is non-nil when the Telegram interface runs in webhook mode;
	// mount it on the HTTP server (cmd wiring passes it to server.Deps).
	Webhook http.Handler
}

// Build opens the store and assembles the engine. The returned cleanup
// stops the rules reloader, releases MCP connections, and closes the store.
func Build(ctx context.Context, cfg config.Config, tier config.Tier,
	iface string, logger *slog.Logger) (*App, func(), error) {

	logger = logx.Or(logger)
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening store: %w", err)
	}

	jobs := tools.NewJobManager()
	set, err := tools.Builtin(tools.Options{
		Workspace: cfg.WorkspaceDir(),
		SQL:       tools.SQLOptions{DSN: cfg.SQLDSN, Path: cfg.DBPath, Mode: cfg.SQLMode},
		Download: tools.DownloadOptions{
			TimeoutSecs:  cfg.DownloadTimeoutSecs,
			MaxBytes:     cfg.DownloadMaxBytes,
			AllowPrivate: cfg.DownloadAllowPrivate,
		},
		Jobs: jobs,
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
	stopReload := router.StartHotReload(ctx, rt, st,
		time.Duration(cfg.RulesReloadSecs)*time.Second, logger)

	// fail tears down every boot resource acquired so far. The arms above
	// (before stopReload existed) keep their own narrower cleanup; the ones
	// below own exactly these three resources.
	fail := func(err error) (*App, func(), error) {
		stopReload()
		releaseMCP()
		st.Close()
		return nil, nil, err
	}

	// Self-mining: fallback corpus → shadow rules → promotion via the
	// engine's tool-choice comparison. Promotion bar from config.
	engine.PromoteAfter = max(1, cfg.MinerPromoteAfter)
	stopMiner := miner.Start(ctx, st, miner.Options{Threshold: cfg.MinerThreshold},
		time.Duration(cfg.MinerIntervalSecs)*time.Second, logger)

	// Provider config only matters when the LLM fallback is switched on;
	// AEGIS_LLM=off boots a router-only agent with zero credentials.
	var llm engine.LLMRunner
	model := ""
	var classifier engine.Classifier
	var classifierModel string
	if !cfg.LLMDisabled() {
		if err := cfg.Validate(); err != nil {
			return fail(err)
		}
		a, err := provider.New(cfg, tier, append(reg.FuncTools(), mcpTools...), logger)
		if err != nil {
			return fail(err)
		}
		llm = a
		model = cfg.ModelFor(tier)

		// Dify-style fast tier (AEGIS_CLASSIFIER=on): a second agent at the
		// fast tier, built TOOL-LESS on purpose. The classify call is pure
		// text-in/JSON-out (the catalog rides in the prompt); AegisGo
		// executes the chosen tool itself through the schema-validated
		// FuncTool path — one execution, no agent self-execution, and no
		// external MCP surfaces the classifier could steer. Without a
		// distinct fast model there is nothing to save, so we warn and boot
		// without it.
		if cfg.ClassifierEnabled() {
			if fast := cfg.ModelFor(config.TierFast); fast == "" {
				logger.Warn("AEGIS_CLASSIFIER=on but no fast model configured — set AEGIS_MODEL_FAST; classifier disabled")
			} else {
				fa, err := provider.New(cfg, config.TierFast, nil, logger)
				if err != nil {
					return fail(fmt.Errorf("building classifier: %w", err))
				}
				classifier = &appClassifier{llm: fa, reg: reg, prompt: newClassifierPrompt(reg)}
				classifierModel = fast
				logger.Info("classifier tier active (AEGIS_CLASSIFIER=on)", "model", fast)
			}
		}
	} else {
		logger.Info("LLM fallback disabled (AEGIS_LLM=off) — router-only mode")
	}

	cleanup := func() {
		stopMiner()
		stopReload()
		releaseMCP()
		st.Close()
	}

	// Telegram interface: fully built, dormant until a token is set.
	eng := &engine.Engine{Router: rt, LLM: llm, Classifier: classifier,
		ClassifierModel: classifierModel, Store: st, IFace: iface, Model: model}
	var webhook http.Handler
	var stopTelegram func()
	if cfg.TelegramEnabled() {
		var err error
		webhook, stopTelegram, err = startTelegram(ctx, cfg, eng, st, rt, jobs, reg, logger)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		// Mirror the base cleanup plus stopTelegram. stopMiner() must stay:
		// without it the miner goroutine keeps ticking and st.Close() below
		// can yank the store out from under an in-flight Mine.
		cleanup = func() {
			if stopTelegram != nil {
				stopTelegram()
			}
			stopMiner()
			stopReload()
			releaseMCP()
			st.Close()
		}
	} else {
		logger.Info("telegram disabled — set AEGIS_TELEGRAM_TOKEN (+ AEGIS_TELEGRAM_CHATS) to enable")
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
		"telegram", cfg.TelegramEnabled(),
	)
	return &App{
		Engine:   eng,
		Store:    st,
		Config:   cfg,
		Webhook:  webhook,
		Registry: reg,
		Jobs:     jobs,
	}, cleanup, nil
}

// startTelegram boots the Telegram interface: client → inbox → dispatcher
// → workers, then the chosen transport (webhook or long-poll). A bad token
// fails startup loudly; an empty chat allowlist warns but boots (the bot
// will skip everything until configured — secure default).
func startTelegram(ctx context.Context, cfg config.Config, eng *engine.Engine,
	st *store.Store, rt *router.Router, jobs *tools.JobManager, reg *tools.Registry,
	logger *slog.Logger) (http.Handler, func(), error) {

	client := telegram.NewHTTPClient(cfg.TelegramToken, cfg.TelegramAPIBase)
	botName, err := client.GetMe(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("telegram: token rejected (getMe): %w", err)
	}
	if len(cfg.TelegramChats) == 0 {
		logger.Warn("telegram enabled but AEGIS_TELEGRAM_CHATS is empty — " +
			"all updates will be skipped until the allowlist is set")
	}

	inbox := telegram.NewInbox(st)
	dispatcher := telegram.NewDispatcher(eng, client, inbox, cfg.TelegramChats,
		func() []string {
			var lines []string
			for _, d := range rt.RuleDefs() {
				lines = append(lines, fmt.Sprintf("%s → %s → %s", d.Name, d.Pattern, d.Tool))
			}
			return lines
		}, logger, st)

	workers := max(1, cfg.TelegramWorkers)
	// 5s idle cadence: Wake() fires on every enqueue, so the interval only
	// governs how often an idle worker re-checks the inbox (crash recovery
	// of un-woken rows) — 1s was pure database churn.
	pool := telegram.NewWorkerPool(inbox, workers, 5*time.Second, dispatcher.Process, logger)

	// The transport outlives the boot context (it serves until shutdown).
	tgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pool.Start(tgCtx)

	// Approval notifier: push NEW pending approvals to the allowlisted
	// chats (ADR-0006). Runs in poll mode AND webhook mode — it reads the
	// ledger, not the transport. Disabled cleanly when no chats are set.
	notif := telegram.NewNotifier(&approvalSourceShim{st: st}, client, cfg.TelegramChats,
		5*time.Second, logger)
	// Decision editing (ADR-0009): pushed button messages get rewritten
	// to their final state when decided from ANY path this process sees.
	editReg := telegram.NewEditRegistry()
	notif.SetEditRegistry(editReg)
	go notif.Start(tgCtx)

	// Daily SQLite sidecar backup (ADR-0012): VACUUM INTO a dated file
	// next to the DB at 04:30, keep the newest 7. Silent success, logged
	// failure — the digest surfaces repeated failures as stale uptime.
	go func() {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 4, 30, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		tm := time.NewTimer(time.Until(next))
		defer tm.Stop()
		for {
			select {
			case <-tgCtx.Done():
				return
			case <-tm.C:
				backupOnce(tgCtx, st, cfg.DBPath, logger)
				tm.Reset(24 * time.Hour)
			}
		}
	}()

	// Daily digest (ADR-0010): one deterministic morning message per
	// chat — uptime, runs, pending count, last decisions. No LLM.
	dig := telegram.NewDigest(client, cfg.TelegramChats, st, st, st, logger)
	go func() {
		// Align to the next 07:00 local, then every 24h.
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		tm := time.NewTimer(time.Until(next))
		defer tm.Stop()
		for {
			select {
			case <-tgCtx.Done():
				return
			case <-tm.C:
				dig.Send(tgCtx)
				tm.Reset(24 * time.Hour)
			}
		}
	}()

	// First REAL gated action: /reload_rules re-reads the rules table and
	// swaps the live router — only after a human approves (ADR-0007).
	// The diff variant embeds the old→new rule changes in the approval
	// reason: approve what you SEE (ADR-0011).
	rg := &ReloadDiffGate{ReloadGate: &ReloadGate{Store: st, Router: rt}}
	dispatcher.RegisterGated(map[string]telegram.GatedAction{"/reload_rules": gatedText{rg}})

	// /addrule (owner directive 28 Sep): add a router rule from chat,
	// gated L2 — the approval reason shows the exact rule being added.
	ag := &AddRuleGate{Store: st, Router: rt}
	dispatcher.RegisterGated(map[string]telegram.GatedAction{"/addrule": addRuleText{g: ag}})

	// /status + /history share the store REST already serves.
	dispatcher.SetStats(st)
	dispatcher.SetHistory(st)
	dispatcher.SetHealth(st)

	// 24/7 log watchdog (owner 28 Sep): watches live in SQLite, goroutine
	// per watch, alerts → owner chat, add/remove from chat without rebuild.
	watchMgr := logwatch.NewManager(16)
	watchStore := &logwatch.StoreAdapter{
		Exec: func(ctx context.Context, q string, args ...any) error { return st.Exec(ctx, q, args...) },
		QueryRows: func(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
			return st.QueryMaps(ctx, q, args...)
		},
	}
	// analyze_log as the single code path for the /api_perf family:
	// Telegram gates call the SAME tool the router serves (containment
	// via resolvePath inside the tool — Hard Rule 2 now holds everywhere).
	execTool := func(ctx context.Context, name string, args map[string]any) (any, error) {
		t, ok := reg.Get(name)
		if !ok {
			return nil, fmt.Errorf("tool %s not found", name)
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		return t.Execute(ctx, raw)
	}
	_ = execTool

	wgate := &WatchGate{
		Mgr:   watchMgr,
		Store: watchStore,
		Notify: func(text string) {
			for _, chat := range cfg.TelegramChats {
				if _, err := client.SendMessage(context.Background(), chat, text, 0); err != nil {
					logger.Error("watchdog alert send", "error", err)
				}
			}
		},
	}
	if n, err := wgate.StartWatches(tgCtx); err != nil {
		logger.Error("watchdog rehydrate", "error", err)
	} else if n > 0 {
		logger.Info("watchdog rehydrated", "watches", n)
	}
	dispatcher.SetWatch(wgate.HandleWatchText)

	// Deterministic log analysis family (owner 28 Sep): /api_perf
	// /exceptions /access /audit /behaviour — zero LLM. Path reads
	// delegate to the analyze_log TOOL (same resolvePath containment
	// as every other file tool — Hard Rule 2).
	lagate := &LogAnalysisGate{
		QueryAuditRows: func(ctx context.Context, n int) ([]map[string]any, error) {
			return st.QueryMaps(ctx,
				`SELECT interface, decision_source, rule_id, outcome, latency_ms, confidence, tokens_in, tokens_out
				 FROM audit_events ORDER BY rowid DESC LIMIT ?`, n)
		},
		ExecTool: execTool,
	}
	dispatcher.SetLogAnalysis(lagate.HandleText)

	// /jobs and /cancel_job share the job manager REST already serves.
	dispatcher.SetJobs(jobsSource{jobs})
	dispatcher.SetJobCancel(jobsSource{jobs})

	// Decision edits: drain queue → EditMessageText on recorded targets.
	ed := telegram.NewEditor(client, editReg, logger)
	dispatcher.OnDecided(ed.EnqueueDecided)
	go ed.Run(tgCtx)

	var webhook http.Handler
	var poller *telegram.PollLoop
	if cfg.TelegramUseWebhook() {
		if cfg.TelegramWebhookURL == "" {
			cancel()
			return nil, nil, fmt.Errorf("telegram: webhook mode needs AEGIS_TELEGRAM_WEBHOOK_URL")
		}
		secret := cfg.TelegramWebhookSecret
		if secret == "" {
			// Per-boot secret: always enforced, rotation = restart.
			secret, _ = trace.New(context.Background(), "")
		}
		publicURL := strings.TrimSuffix(cfg.TelegramWebhookURL, "/") + server.WebhookPath
		if err := client.SetWebhook(tgCtx, publicURL, secret); err != nil {
			cancel()
			return nil, nil, fmt.Errorf("telegram: setWebhook failed: %w", err)
		}
		webhook = telegram.NewWebhookHandler(secret, inbox, pool, logger)
		logger.Info("telegram: webhook transport active", "bot", botName, "url", publicURL)
	} else {
		poller = telegram.NewPollLoop(client, inbox, pool, logger)
		go poller.Run(tgCtx)
		logger.Info("telegram: long-poll transport active", "bot", botName)
	}

	stop := func() {
		pool.Stop()
		cancel()
		// Join the poll goroutine (bounded): an in-flight getUpdates must
		// not outlive the store close it may still write into.
		if poller != nil && !poller.Wait(10*time.Second) {
			logger.Warn("telegram: long-poll transport still running after 10s; proceeding")
		}
	}
	return webhook, stop, nil
}

// approvalSourceShim adapts *store.Store to telegram.ApprovalSource.
type approvalSourceShim struct{ st *store.Store }

// jobsSource adapts *tools.JobManager to the dispatcher's narrow joblister
// (telegram stays import-free of internal/tools; conversion is field-by-field).
type jobsSource struct{ m *tools.JobManager }

func (s jobsSource) ListJobs() []telegram.Job {
	js := s.m.ListJobs()
	out := make([]telegram.Job, len(js))
	for i, j := range js {
		out[i] = telegram.Job{ID: j.ID, Kind: j.Kind, Status: j.Status,
			Meta: j.Meta, CreatedAt: j.CreatedAt}
	}
	return out
}

func (s jobsSource) CancelJob(id string) (telegram.Job, bool, bool) {
	j, known, issued := s.m.CancelJob(id)
	return telegram.Job{ID: j.ID, Kind: j.Kind, Status: j.Status,
		Meta: j.Meta, CreatedAt: j.CreatedAt}, known, issued
}

func (s *approvalSourceShim) PendingApprovals(ctx context.Context, limit int) ([]telegram.ApprovalInfo, error) {
	rows, err := s.st.PendingApprovals(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]telegram.ApprovalInfo, 0, len(rows))
	for _, a := range rows {
		out = append(out, telegram.ApprovalInfo{ID: a.ID, Kind: a.Kind, Reason: a.Reason, Payload: a.Payload, CreatedAt: a.CreatedAt})
	}
	return out, nil
}
