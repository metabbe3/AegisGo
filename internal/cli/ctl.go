package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	return fmt.Errorf("usage: aegis ctl rules list|mine|promote <name>|demote <name> | stats | replay <trace-id>")
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
		w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tORIGIN\tENABLED\tTOOL\tPATTERN")
		for _, r := range rules {
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\n",
				r["name"], r["state"], r["origin"], r["enabled"], r["tool"], r["pattern"])
		}
		return w.Flush()
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
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TS\tIFACE\tSOURCE\tRULE\tMODEL\tLATENCY\tOUTCOME")
	for _, a := range trail {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%dms\t%s\n",
			a.TS, a.Interface, a.DecisionSource, a.RuleID, a.Model, a.LatencyMS, a.Outcome)
	}
	return w.Flush()
}
