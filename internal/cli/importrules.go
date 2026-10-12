package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/router"
	"aegisgo/internal/store"
	"aegisgo/internal/tools"
)

// importRulesCmd is the offline admin twin of the Telegram /register_rules
// gate (#66): it replays a saved /rules export (a file of /addrule lines,
// piped or via -f=) back into THIS box. Import semantics are owned by
// app.RegisterRulesGate — every line parses strictly BEFORE any row is
// written, tools outside the ChatAddable allowlist are rejected, and the
// upsert is idempotent, so re-importing a box's own export converges.
//
// No approval ledger row is created: ctl is the owner-at-keyboard surface
// and the operator's Enter key IS the approval (contrast /register_rules,
// which waits for the ✅/🚫 buttons). The running daemon picks the new
// rules up through its periodic hot-reload (≤5s); a scratch router is
// still built here so the post-import Swap validates the batch exactly
// like the gate does — a broken import is rejected, never half-applied.
//
//	aegis ctl rules export > rules.txt        # box A
//	aegis ctl rules import -f=rules.txt       # box B
//	cat rules.txt | aegis ctl rules import    # same, via stdin
func importRulesCmd(ctx context.Context, st *store.Store, cfg config.Config, args []string, stdout io.Writer) error {
	var path string
	for _, a := range args {
		const p = "-f="
		if len(a) > len(p) && a[:len(p)] == p {
			path = a[len(p):]
			continue
		}
		return fmt.Errorf("unknown import arg %q (want -f=<file> of /addrule lines; no file = stdin)", a)
	}
	in := os.Stdin
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		in = f
	}
	text, err := io.ReadAll(io.LimitReader(in, 1<<20)) // bounded input — rule #10's input-side twin
	if err != nil {
		return fmt.Errorf("reading import: %w", err)
	}

	// Scratch validation router: full builtin toolset (rules only ever
	// name builtin tools) + the current table rows. Same construction as
	// app.Build — the DB is the single rule source (ReconcileSeeded keeps
	// the seeded rows in it), so NO Seeded() append here or every seeded
	// name would collide. The daemon applies the imported rows through its
	// own hot-reload; this Swap exists so an invalid batch is rejected
	// HERE, not discovered by the daemon 5s later.
	set, err := tools.Builtin(tools.Options{Workspace: cfg.Workspace, SQL: tools.SQLOptions{DSN: cfg.SQLDSN, Path: cfg.DBPath, Mode: cfg.SQLMode}})
	if err != nil {
		return fmt.Errorf("building toolset: %w", err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		return fmt.Errorf("building registry: %w", err)
	}
	defs, err := router.LoadRules(ctx, st)
	if err != nil {
		return fmt.Errorf("loading current rules: %w", err)
	}
	rt, err := router.New(reg, defs)
	if err != nil {
		return fmt.Errorf("building validation router: %w", err)
	}

	gate := &app.RegisterRulesGate{Store: st, Router: rt}
	imported, live, err := gate.ReplayAddruleLines(ctx, string(text))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported %d rule(s); %d live in table; daemon hot-reload applies them within %ds\n", imported, live, cfg.RulesReloadSecs)
	return nil
}
