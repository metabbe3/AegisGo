// Exported helpers over backupOnce for the non-daemon callers (#67):
// the ctl admin path and (next) the /backup gate. They keep the sidecar
// naming + retention owned by internal/app so the daemon's daily 04:30
// loop and the on-demand paths can never drift apart.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aegisgo/internal/store"
)

// backupLogger is the minimal sink backupOnce needs — *slog.Logger and
// the narrow test double both satisfy it structurally.
type backupLogger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// BackupNow runs one sidecar backup NOW instead of waiting for the
// 04:30 timer: same backupOnce, same prune-to-7 retention.
func BackupNow(ctx context.Context, st *store.Store, dbPath string, logger backupLogger) error {
	return backupOnce(ctx, st, dbPath, logger)
}

// SidecarPath returns the destination a backup taken at ts would write —
// callers stat it right after a run to verify the artifact exists.
func SidecarPath(dbPath string, ts time.Time) string {
	base := filepath.Base(dbPath)
	return filepath.Join(filepath.Dir(dbPath),
		fmt.Sprintf("%s.backup-%s.sidecar", base, ts.Format("20060102")))
}

// backupOnce runs one sidecar backup: VACUUM INTO a dated file next to the
// DB, then prunes older sidecars beyond the retention window. Failures are
// returned AND logged by the caller loop — a silent failure here means the
// digest's "last backup" line goes stale, which the owner notices.
func backupOnce(ctx context.Context, st *store.Store, dbPath string, logger backupLogger) error {
	dest := SidecarPath(dbPath, time.Now())
	// VACUUM INTO refuses an existing file; same-day reruns must be
	// idempotent, so drop the earlier same-day attempt first.
	_ = os.Remove(dest)
	if err := st.BackupTo(ctx, dest); err != nil {
		logger.Error("backup failed", "err", err, "dest", dest)
		return err
	}
	logger.Info("backup ok", "dest", dest)
	pruneSidecars(filepath.Dir(dbPath), filepath.Base(dbPath), 7)
	return nil
}

// pruneSidecars keeps only the newest retention backups matching this DB's
// sidecar prefix. Best-effort: a prune error never fails the backup itself.
func pruneSidecars(dir, dbBase string, retention int) {
	prefix := dbBase + ".backup-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".sidecar") {
			names = append(names, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // date-named: newest first
	if len(names) > retention {
		for _, n := range names[retention:] {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}
