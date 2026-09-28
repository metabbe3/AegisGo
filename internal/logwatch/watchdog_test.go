package logwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTailFileNewContentOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	os.WriteFile(p, []byte("one\ntwo\n"), 0o644)

	lines, size, off := tailFile(p, 0, 0)
	if len(lines) != 2 || lines[0] != "one" {
		t.Fatalf("first tail = %v", lines)
	}
	// append, re-tail: only the new line
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("three\n")
	f.Close()
	lines, size2, off2 := tailFile(p, size, off)
	if len(lines) != 1 || lines[0] != "three" {
		t.Fatalf("second tail = %v (size %d→%d)", lines, size, size2)
	}
	if off2 <= off {
		t.Fatalf("offset did not advance: %d → %d", off, off2)
	}
}

func TestTailFileRotationResets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	os.WriteFile(p, []byte("old-rotated-content-xxxxxxxxxxxx\n"), 0o644)
	_, size, off := tailFile(p, 0, 0)
	// rotation: file replaced with something smaller
	os.WriteFile(p, []byte("fresh\n"), 0o644)
	lines, _, _ := tailFile(p, size, off)
	if len(lines) != 1 || lines[0] != "fresh" {
		t.Fatalf("post-rotation tail = %v", lines)
	}
}

func TestTailFileMissingIsQuiet(t *testing.T) {
	lines, _, _ := tailFile(filepath.Join(t.TempDir(), "nope.log"), 0, 0)
	if len(lines) != 0 {
		t.Fatalf("missing file produced %v", lines)
	}
}

func TestManagerFiresAlertWithCooldown(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gw.log")
	os.WriteFile(p, []byte("level=INFO ok\n"), 0o644)

	m := NewManager(8)
	defer m.Stop()
	err := m.Add(context.Background(), Watch{
		Name: "err", Path: p, Pattern: `panic|FATAL`,
		Every: 10 * time.Millisecond, Cooldown: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	// initial content has no match; append two matches quickly — only the
	// first may alert (cooldown suppresses the second but still counts it)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("panic: boom\npanic: boom again\n")
	f.Close()

	select {
	case a := <-m.Alerts():
		if a.Watch != "err" || a.Line != "panic: boom" {
			t.Fatalf("alert = %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert fired")
	}
	// second match within cooldown: suppressed (no second alert)
	select {
	case a := <-m.Alerts():
		t.Fatalf("cooldown failed, second alert = %+v", a)
	case <-time.After(200 * time.Millisecond):
	}
	if c := m.Counts()["err"]; c != 2 {
		t.Fatalf("counts = %d, want 2 (suppressed still counted)", c)
	}
}

func TestManagerBadPatternRejected(t *testing.T) {
	m := NewManager(4)
	defer m.Stop()
	if err := m.Add(context.Background(), Watch{Name: "x", Path: "/tmp/x", Pattern: `[`}); err == nil {
		t.Fatal("bad pattern accepted")
	}
}

func TestManagerReplaceSameName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	os.WriteFile(p, []byte(""), 0o644)
	m := NewManager(4)
	defer m.Stop()
	if err := m.Add(context.Background(), Watch{Name: "dup", Path: p, Pattern: "x", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(context.Background(), Watch{Name: "dup", Path: p, Pattern: "y", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if len(m.Counts()) != 1 {
		t.Fatalf("replace left %d watches", len(m.Counts()))
	}
}
