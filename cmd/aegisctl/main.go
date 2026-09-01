// Command aegisctl is the offline admin tool: it operates directly on the
// AegisGo database (no server needed) to list/promote/demote router rules,
// run the miner on demand, print stats, and replay a trace.
//
//	aegisctl rules list
//	aegisctl rules mine [--threshold N]
//	aegisctl rules promote <name> | demote <name>
//	aegisctl stats
//	aegisctl replay <trace-id>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"aegisgo/internal/config"
	"aegisgo/internal/miner"
	"aegisgo/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aegisctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := config.Load()
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "rules":
		return rulesCmd(ctx, st, cfg, args[1:])
	case "stats":
		return statsCmd(ctx, st)
	case "replay":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegisctl replay <trace-id>")
		}
		return replayCmd(ctx, st, args[1])
	default:
		return usage()
	}
}

func usage() error {
	return fmt.Errorf("usage: aegisctl rules list|mine|promote <name>|demote <name> | stats | replay <trace-id>")
}

func rulesCmd(ctx context.Context, st *store.Store, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aegisctl rules list|mine|promote <name>|demote <name>")
	}
	switch args[0] {
	case "list":
		rules, err := st.RuleStates(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
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
			fmt.Println("no eligible clusters (raise traffic or lower --threshold)")
			return nil
		}
		for _, p := range proposals {
			fmt.Printf("shadow rule %s: %s → %s (cluster %d)\n", p.Name, p.Pattern, p.Tool, p.ClusterSize)
		}
		fmt.Println("\nnext: drive traffic matching the shape; promotion is automatic after agreement streak")
		return nil
	case "promote":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegisctl rules promote <name>")
		}
		return st.SetRuleState(ctx, args[1], store.RuleStateActive, true)
	case "demote":
		if len(args) != 2 {
			return fmt.Errorf("usage: aegisctl rules demote <name>")
		}
		return st.SetRuleState(ctx, args[1], store.RuleStateDemoted, false)
	default:
		return fmt.Errorf("unknown rules subcommand %q", args[0])
	}
}

func statsCmd(ctx context.Context, st *store.Store) error {
	s, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(out))
	return nil
}

func replayCmd(ctx context.Context, st *store.Store, traceID string) error {
	trail, err := st.Replay(ctx, traceID)
	if err != nil {
		return err
	}
	if len(trail) == 0 {
		return fmt.Errorf("no audit rows for trace %s", traceID)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TS\tIFACE\tSOURCE\tRULE\tMODEL\tLATENCY\tOUTCOME")
	for _, a := range trail {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%dms\t%s\n",
			a.TS, a.Interface, a.DecisionSource, a.RuleID, a.Model, a.LatencyMS, a.Outcome)
	}
	return w.Flush()
}
