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
}

// catalog is the complete set of commands system_command may run.
var catalog = map[string]commandSpec{
	"uptime":   {argv: []string{"uptime"}, description: "System uptime and load average"},
	"disk":     {argv: []string{"df", "-h"}, description: "Disk usage per filesystem"},
	"memory":   {argv: []string{"free", "-m"}, description: "Memory usage in MiB"},
	"hostname": {argv: []string{"hostname"}, description: "Machine hostname"},
	"kernel":   {argv: []string{"uname", "-sr"}, description: "Kernel name and release"},
	"who":      {argv: []string{"who"}, description: "Logged-in users"},
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
		Output:     buf.String(),
		Truncated:  buf.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
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
