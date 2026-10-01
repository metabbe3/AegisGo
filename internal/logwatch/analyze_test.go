package logwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzePatterns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gw.log")
	os.WriteFile(p, []byte(
		"level=INFO request ok path=/healthz trace=abc latency=12ms\n"+
			"level=INFO request ok path=/healthz trace=def latency=9ms\n"+
			"level=INFO request ok path=/healthz trace=ghi latency=21ms\n"+
			"level=ERROR db timeout after 5000ms query=SELECT\n"+
			"level=ERROR db timeout after 3000ms query=SELECT\n"+
			"plain line\n"), 0o644)

	a, err := Analyze(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.LinesRead != 6 || a.ErrorLines != 2 {
		t.Fatalf("analysis = %+v", a)
	}
	if a.ErrorRate < 0.33 || a.ErrorRate > 0.34 {
		t.Fatalf("error rate = %v", a.ErrorRate)
	}
	// top-1 pattern must be the healthz shape with count 3
	if len(a.Top) == 0 || a.Top[0].Count != 3 {
		t.Fatalf("top = %+v", a.Top)
	}
	// top error = the timeout shape with count 2
	if len(a.TopErrors) == 0 || a.TopErrors[0].Count != 2 {
		t.Fatalf("top errors = %+v", a.TopErrors)
	}
	if a.TopErrors[0].Template == a.Top[0].Template {
		t.Fatal("error template equals info template — normalize broken")
	}
}

func TestNormalizeMasksVolatile(t *testing.T) {
	in := `request 3f2b1c4d-aaaa-bbbb-cccc-dddddddddddd failed ip=10.1.2.3 took 187ms "SELECT x" [worker-7] port 8080`
	got := normalize(in)
	for _, want := range []string{"UUID", "IP", "DUR", `"STR"`, "[...]", "N"} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalize(%q) = %q, missing %s", in, got, want)
		}
	}
}

func TestAnalyzeMissingFile(t *testing.T) {
	if _, err := Analyze(filepath.Join(t.TempDir(), "nope"), 10); err == nil {
		t.Fatal("missing file should error")
	}
}

func TestAnalyzeCapsLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.log")
	var body []byte
	for i := 0; i < 600; i++ {
		body = append(body, []byte("same line every time\n")...)
	}
	os.WriteFile(p, body, 0o644)
	a, err := Analyze(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if a.LinesRead != 100 {
		t.Fatalf("lines read = %d, want capped 100", a.LinesRead)
	}
}

// TestAnalyzeTailWindowBounded pins Hard Rule 10 on /analyze: a log larger
// than the 8 MiB window is READ as a tail window, never slurped — Analyze
// must not allocate proportional to the file size. (Pre-fix Analyze did
// os.ReadFile on the whole file.)
func TestAnalyzeTailWindowBounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.log")
	// 9 MiB of lines; each line ~64 bytes → ~147k lines.
	var body []byte
	line := strings.Repeat("x", 63) + "\n"
	for bodyLen := 0; bodyLen < 9<<20; {
		body = append(body, line...)
		bodyLen += len(line)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Analyze(p, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if a.LinesRead != 5000 {
		t.Fatalf("lines read = %d, want windowed 5000", a.LinesRead)
	}
}

// TestAnalyzeRejectsNonRegularFile pins the defense-in-depth guard: a
// device file (e.g. /dev/zero renamed) must be refused — pre-fix a
// non-regular file fed size-based allocation unbounded.
func TestAnalyzeRejectsNonRegularFile(t *testing.T) {
	if _, err := Analyze("/dev/null", 10); err == nil {
		t.Fatal("non-regular file should error")
	}
}

// TestTailFileRejectsNonRegular pins the tailFile defense-in-depth: a
// device file (block/char special) must yield zero lines even if a legacy
// stored watch points at it — the poll loop can never spin on /dev/zero.
func TestTailFileRejectsNonRegular(t *testing.T) {
	lines, _, _ := tailFile("/dev/zero", 0, 0)
	if len(lines) != 0 {
		t.Fatalf("device file yielded %d lines", len(lines))
	}
}
