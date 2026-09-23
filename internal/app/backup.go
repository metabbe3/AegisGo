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

// backupOnce runs one sidecar backup: VACUUM INTO a dated file next to the
// DB, then prunes older sidecars beyond the retention window. Failures are
// returned AND logged by the caller loop — a silent failure here means the
// digest's "last backup" line goes stale, which the owner notices.
func backupOnce(ctx context.Context, st *store.Store, dbPath string, logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}) error {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	name := fmt.Sprintf("%s.backup-%s.sidecar", base, time.Now().Format("20060102"))
	dest := filepath.Join(dir, name)
	// VACUUM INTO refuses an existing file; same-day reruns must be
	// idempotent, so drop yesterday's-attempt artifact first.
	_ = os.Remove(dest)
	if err := st.BackupTo(ctx, dest); err != nil {
		logger.Error("backup failed", "err", err, "dest", dest)
		return err
	}
	logger.Info("backup ok", "dest", dest)
	pruneSidecars(dir, base, 7)
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
