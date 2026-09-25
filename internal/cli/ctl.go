package cli

import (
	"aegisgo/internal/tools"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"aegisgo/internal/config"
	"aegisgo/internal/miner"
	"aegisgo/internal/store"
)

// runCtl is the offline admin tool: it operates directly on the AegisGo
// database (no server needed) to list/promote/demote router rules, run the
// miner on demand, print stats, and replay a trace.
//
//	aegis ctl rules list
//	aegis ctl rules mine [--threshold N]
//	aegis ctl rules promote <name> | demote <name>
//	aegis ctl stats
//	aegis ctl replay <trace-id>
func runCtl(args []string, stdout io.Writer) error {
	cfg := config.Load()
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if len(args) == 0 {
		return ctlUsage()
	}
	switch args[0] {
	case "rules":
		return rulesCmd(ctx, st, cfg, args[1:], stdout)
	case "stats":
		return statsCmd(ctx, st, stdout)
	case "jobs":
		return jobsCmd(ctx, cfg, stdout)
	case "replay":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegis ctl replay <trace-id>")
		}
		return replayCmd(ctx, st, args[1], stdout)
	default:
		return ctlUsage()
	}
}

func ctlUsage() error {
	return fmt.Errorf("usage: aegis ctl rules list|mine|promote <name>|demote <name> | stats | jobs | replay <trace-id>")
}

// jobsCmd lists live background jobs from the running server's GET
// /v1/jobs (jobs are in-process state — a fresh CLI has none of its own).
func jobsCmd(ctx context.Context, cfg config.Config, stdout io.Writer) error {
	base := os.Getenv("AEGIS_ADDR")
	if base == "" {
		base = "http://localhost:8080"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/jobs", nil)
	if err != nil {
		return err
	}
	if cfg.HTTPToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.HTTPToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("querying %s/v1/jobs (is the server running?): %w", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/jobs: %s: %s", resp.Status, body)
	}
	var jobs []tools.Job
	if err := json.Unmarshal(body, &jobs); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Fprintln(stdout, "no jobs (running or retained)")
		return nil
	}
	for _, j := range jobs {
		line := fmt.Sprintf("%s  %-9s %s", j.ID, j.Status, j.Kind)
		if j.Error != "" {
			line += "  err: " + j.Error
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func rulesCmd(ctx context.Context, st *store.Store, cfg config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aegis ctl rules list|mine|promote <name>|demote <name>")
	}
	switch args[0] {
	case "list":
		rules, err := st.RuleStates(ctx)
		if err != nil {
			return err
		}
		rows := make([][]any, len(rules))
		for i, r := range rules {
			rows[i] = []any{r["name"], r["state"], r["origin"], r["enabled"], r["tool"], r["pattern"]}
		}
		return writeTable(stdout, []string{"NAME", "STATE", "ORIGIN", "ENABLED", "TOOL", "PATTERN"}, rows)
	case "mine":
		fs := flag.NewFlagSet("mine", flag.ContinueOnError)
		th := fs.Int("threshold", cfg.MinerThreshold, "minimum cluster size")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		proposals, err := miner.Mine(ctx, st, miner.Options{Threshold: *th}, nil)
		if err != nil {
			return err
		}
		if len(proposals) == 0 {
			fmt.Fprintln(stdout, "no eligible clusters (raise traffic or lower --threshold)")
			return nil
		}
		for _, p := range proposals {
			fmt.Fprintf(stdout, "shadow rule %s: %s → %s (cluster %d)\n", p.Name, p.Pattern, p.Tool, p.ClusterSize)
		}
		fmt.Fprintln(stdout, "\nnext: drive traffic matching the shape; promotion is automatic after agreement streak")
		return nil
	case "promote":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegis ctl rules promote <name>")
		}
		return st.SetRuleState(ctx, args[1], store.RuleStateActive, true)
	case "demote":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegis ctl rules demote <name>")
		}
		return st.SetRuleState(ctx, args[1], store.RuleStateDemoted, false)
	default:
		return fmt.Errorf("unknown rules subcommand %q", args[0])
	}
}

func statsCmd(ctx context.Context, st *store.Store, stdout io.Writer) error {
	s, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(s, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return nil
}

func replayCmd(ctx context.Context, st *store.Store, traceID string, stdout io.Writer) error {
	trail, err := st.Replay(ctx, traceID)
	if err != nil {
		return err
	}
	if len(trail) == 0 {
		return fmt.Errorf("no audit rows for trace %s", traceID)
	}
	rows := make([][]any, len(trail))
	for i, a := range trail {
		rows[i] = []any{a.TS, a.Interface, a.DecisionSource, a.RuleID, a.Model,
			fmt.Sprintf("%dms", a.LatencyMS), a.Outcome}
	}
	return writeTable(stdout, []string{"TS", "IFACE", "SOURCE", "RULE", "MODEL", "LATENCY", "OUTCOME"}, rows)
}

// writeTable renders header + rows as an aligned table with the ctl
// conventions (tabwriter, 2-space padding) — one spelling of "print an
// admin listing" for the rules and replay commands.
func writeTable(stdout io.Writer, header []string, rows [][]any) error {
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, row := range rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = fmt.Sprint(v)
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	return w.Flush()
}
