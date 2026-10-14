package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aegisgo/internal/app"
	"aegisgo/internal/config"
	"aegisgo/internal/store"
)

// backupCmd is the on-demand twin of the daily 04:30 sidecar backup
// (#67): the same backupOnce path, run when the operator asks. `list`
// shows the sidecar history so a backup can be verified BEFORE a
// restore needs it — an untested backup is a hope, not a plan.
//
//	aegis ctl backup          # run one backup now (VACUUM INTO + prune)
//	aegis ctl backup list     # sidecar history, newest first
func backupCmd(ctx context.Context, st *store.Store, cfg config.Config, args []string, stdout io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: aegis ctl backup [list]")
	}
	if len(args) == 1 && args[0] != "list" {
		return fmt.Errorf("unknown backup arg %q (want list)", args[0])
	}
	if len(args) == 1 && args[0] == "list" {
		rows, err := listSidecars(cfg.DBPath)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Fprintln(stdout, "no sidecar backups yet (daily 04:30, or run: aegis ctl backup)")
			return nil
		}
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s  %s\n", r.name, r.size)
		}
		fmt.Fprintf(stdout, "\n(%d sidecar(s), retention 7)\n", len(rows))
		return nil
	}
	// Quiet logger: ctl is a foreground command; the outcome line goes to
	// stdout, not a daemon log — the operator's eyes are the sink.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	if err := app.BackupNow(ctx, st, cfg.DBPath, quiet); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	dest := app.SidecarPath(cfg.DBPath, time.Now())
	info, err := os.Stat(dest)
	if err != nil {
		return fmt.Errorf("backup written but stat failed: %w", err)
	}
	fmt.Fprintf(stdout, "backup ok: %s (%s) in %s\n", dest, humanBytes(info.Size()), time.Since(start).Round(time.Millisecond))
	return nil
}

// sidecarRow is one listing line: file name + human size.
type sidecarRow struct {
	name string
	size string
}

// listSidecars returns the DB's sidecar files newest-first (date-named,
// lexical reverse). A missing DB dir is an honest empty list, not an
// error — a fresh box simply has no backups yet.
func listSidecars(dbPath string) ([]sidecarRow, error) {
	prefix, _, err := sidecarParts(dbPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(dbPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rows []sidecarRow
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".sidecar") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // raced with prune; skip rather than fail the listing
		}
		rows = append(rows, sidecarRow{name: n, size: humanBytes(info.Size())})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name > rows[j].name })
	return rows, nil
}

// sidecarParts derives the sidecar prefix + parent dir from the DB path,
// centralizing the naming so BackupNow and the listing never drift.
func sidecarParts(dbPath string) (prefix, dir string, err error) {
	if dbPath == "" {
		return "", "", fmt.Errorf("AEGIS_DB_PATH is empty — set it or use the default db path")
	}
	return filepath.Base(dbPath) + ".backup-", filepath.Dir(dbPath), nil
}

// humanBytes renders a byte count for humans (10 KiB, 1.5 MiB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
