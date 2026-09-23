package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RotatingWriter is a size-capped file writer that rotates the log when it
// exceeds MaxBytes, keeping the newest N rotated files. Stdlib-only by
// design (repo rule #1): no lumberjack. The active file stays open between
// writes; rotation renames it to <path>.<unix-nano> and reopens a fresh
// one. Writes after a failed rotation fall back to the previous file —
// losing rotation is better than losing logs.
type RotatingWriter struct {
	// Path is the active log file; parent dirs are created on first write.
	Path string
	// MaxBytes rotates once the active file grows past this. <=0 = 10 MiB.
	MaxBytes int64
	// KeepFiles is how many rotated files to retain. <=0 = 5.
	KeepFiles int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Write implements io.Writer. It lazily opens the file so a zero-value
// writer that is never written never touches disk.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if w.MaxBytes <= 0 {
		w.MaxBytes = 10 << 20
	}
	if w.size+int64(len(p)) > w.MaxBytes {
		w.rotateLocked()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close flushes and closes the active file if open.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func (w *RotatingWriter) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return fmt.Errorf("logx: create log dir: %w", err)
	}
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logx: open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logx: stat log file: %w", err)
	}
	w.f, w.size = f, st.Size()
	return nil
}

// rotateLocked renames the active file and reopens. On any failure it keeps
// writing to the current handle (rotate failure must not stop logging).
func (w *RotatingWriter) rotateLocked() {
	rotated := fmt.Sprintf("%s.%d", w.Path, time.Now().UnixNano())
	if w.f != nil {
		_ = w.f.Close()
	}
	if err := os.Rename(w.Path, rotated); err != nil {
		// Reopen (or keep) the active file and continue appending.
		if f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			w.f = f
		}
		return
	}
	if f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		// Unreachable in practice (we just renamed away); keep nil so the
		// next Write retries openLocked.
		w.f = nil
		w.size = 0
		return
	} else {
		w.f = f
	}
	w.size = 0
	w.pruneLocked()
}

// pruneLocked deletes the oldest rotated files beyond KeepFiles. The
// nano-suffix sorts lexicographically = chronologically.
func (w *RotatingWriter) pruneLocked() {
	keep := w.KeepFiles
	if keep <= 0 {
		keep = 5
	}
	entries, err := os.ReadDir(filepath.Dir(w.Path))
	if err != nil {
		return
	}
	base := filepath.Base(w.Path)
	var rotated []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, base+".") && n != base {
			rotated = append(rotated, n)
		}
	}
	sort.Strings(rotated)
	for _, n := range rotated[:max(0, len(rotated)-keep)] {
		_ = os.Remove(filepath.Join(filepath.Dir(w.Path), n))
	}
}
