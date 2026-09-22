package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// System tool security model: the ONLY thing user/model input may choose is
// a catalog KEY. The argv behind that key is fixed in source. This defeats
// the find -exec / awk system() / sed 'e' class of escapes that make
// binary-name allowlists meaningless — with us, the argument list is never
// influenced by input at all. Adding a command means editing this file (see
// WORKFLOW.md); commands with user-controlled flags do not get added.

// systemMaxOutput bounds captured stdout+stderr.
const systemMaxOutput = 64 * 1024

// systemDefaultTimeout bounds each command run.
const systemDefaultTimeout = 10 * time.Second

// commandSpec is one fixed-argv catalog entry.
type commandSpec struct {
	argv        []string
	description string
	// tier is the L-tier policy verdict for this command (blueprint §L).
	// L1 = auto-allow (read-only/build, always logged).
	// L2 = needs approval before execution (hitl gate) — not in catalog yet.
	// L3 = hard-deny (never catalog-able; enforced by absence + review).
	tier string
}

// catalog is the complete set of commands system_command may run.
// Every entry MUST be tier L1: a command needing approval (L2) or
// forbidden outright (L3) does not belong here — see docs/decisions
// ADR-0002. Catalog additions are human-edited (CLAUDE.md rule #3).
var catalog = map[string]commandSpec{
	"uptime":   {argv: []string{"uptime"}, description: "System uptime and load average", tier: "L1"},
	"disk":     {argv: []string{"df", "-h"}, description: "Disk usage per filesystem", tier: "L1"},
	"memory":   {argv: []string{"free", "-m"}, description: "Memory usage in MiB", tier: "L1"},
	"hostname": {argv: []string{"hostname"}, description: "Machine hostname", tier: "L1"},
	"kernel":   {argv: []string{"uname", "-sr"}, description: "Kernel name and release", tier: "L1"},
	"who":      {argv: []string{"who"}, description: "Logged-in users", tier: "L1"},
}

// SystemInput is the schema for the system_command tool.
type SystemInput struct {
	// Command selects a catalog entry: uptime, disk, memory, hostname,
	// kernel, or who. Never a free-form command line.
	Command string `json:"command"`
}

// SystemOutput is the result of system_command.
type SystemOutput struct {
	Command    string `json:"command"`
	PolicyTier string `json:"policy_tier"` // L1 auto-allow · L2 needs-approval · L3 hard-deny
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// NewSystemCommand builds the system_command tool (unix only — the spec
// targets Linux servers).
func NewSystemCommand() (Tool, error) {
	return New(Config{
		Name:        "system_command",
		Description: "Run a whitelisted system command. command must be one of: uptime, disk, memory, hostname, kernel, who. Input never chooses arguments, only the command name.",
	}, func(ctx context.Context, in SystemInput) (SystemOutput, error) {
		return runSystem(ctx, in)
	})
}

func runSystem(ctx context.Context, in SystemInput) (SystemOutput, error) {
	key := strings.TrimSpace(in.Command)
	spec, ok := catalog[key]
	if !ok {
		return SystemOutput{}, fmt.Errorf("unknown command %q (available: %s)", key, catalogKeys())
	}

	ctx, cancel := context.WithTimeout(ctx, systemDefaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.argv[0], spec.argv[1:]...) //nolint:gosec // argv is fixed in catalog
	// Own a process group so a timeout kills the command AND anything it
	// spawned; Context kills only the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	start := time.Now()
	var buf limitedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return SystemOutput{}, fmt.Errorf("command %q timed out after %s", key, systemDefaultTimeout)
	}
	if err != nil {
		return SystemOutput{}, fmt.Errorf("command %q failed: %w", key, err)
	}
	return SystemOutput{
		Command:    key,
		PolicyTier: spec.tier,
		Output:     buf.String(),
		Truncated:  buf.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}

// PolicyTier reports the L-tier verdict for a catalog key ("L1"…), or
// "" when unknown — callers can log the verdict alongside the audit row
// without executing anything.
func PolicyTier(key string) string {
	return catalog[strings.TrimSpace(key)].tier
}

// CatalogCommands describes the catalog for docs/tests.
func CatalogCommands() map[string]string {
	out := make(map[string]string, len(catalog))
	for k, v := range catalog {
		out[k] = v.description
	}
	return out
}

func catalogKeys() string {
	keys := make([]string, 0, len(catalog))
	for k := range catalog {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

// limitedBuffer caps captured output so a chatty command can't blow memory.
type limitedBuffer struct {
	b         bytes.Buffer
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.b.Len() >= systemMaxOutput {
		l.truncated = true
		return len(p), nil // swallow
	}
	room := systemMaxOutput - l.b.Len()
	if len(p) > room {
		l.b.Write(p[:room])
		l.truncated = true
		return len(p), nil
	}
	return l.b.Write(p)
}

func (l *limitedBuffer) String() string { return l.b.String() }
