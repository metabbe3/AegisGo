// In-package tests for the classifier tier: parseClassify/stripThink unit
// matrices plus appClassifier.Classify against a fake fast model and the
// real builtin registry (FuncTool-validated execution, no provider I/O).
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"aegisgo/internal/tools"
)

// fakeRunner is a canned engine.LLMRunner: yields one text answer, or an
// error; records prompts so tests can assert the classify prompt shape.
type fakeRunner struct {
	text    string
	err     error
	prompts []string
}

func (f *fakeRunner) RunText(_ context.Context, msg string, _ ...agent.Option) agent.ResponseStream {
	f.prompts = append(f.prompts, msg)
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		yield(&agent.ResponseUpdate{
			Role:     message.RoleAssistant,
			Contents: message.Contents{&message.TextContent{Text: f.text}},
		}, nil)
	}
}

// repoRoot resolves the repo root (testdata/ lives there) from
// internal/app's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func TestParseClassify(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantTool string
		wantArgs string
		wantOK   bool
	}{
		{"clean json", `{"tool":"read_csv","args":{"path":"a.csv"}}`, "read_csv", `{"path":"a.csv"}`, true},
		{"prose wrapped", `Sure! Here you go:\n{"tool":"csv_stats","args":{"path":"a.csv"}}`, "csv_stats", `{"path":"a.csv"}`, true},
		{"think wrapped", "<think>the user wants the csv stats</think>\n{\"tool\":\"csv_stats\",\"args\":{\"path\":\"a.csv\"}}", "csv_stats", `{"path":"a.csv"}`, true},
		{"freeform class", `{"tool":"freeform","args":{}}`, "freeform", `{}`, true},
		{"missing args defaults to empty object", `{"tool":"uptime_tool"}`, "uptime_tool", `{}`, true},
		{"no json at all", `I cannot answer that in JSON, sorry.`, "", "", false},
		{"json without tool key", `{"result":"nope"}`, "", "", false},
		{"last object with a tool wins", `{"tool":"a"} then {"tool":"read_csv","args":{"path":"b.csv"}}`, "read_csv", `{"path":"b.csv"}`, true},
		{"tool must be a string", `{"tool":42}`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.ReplaceAll(tc.in, `\n`, "\n")
			tool, args, ok := parseClassify(in)
			if ok != tc.wantOK || tool != tc.wantTool {
				t.Fatalf("parseClassify = (%q, %s, %v), want (%q, %s, %v)", tool, args, ok, tc.wantTool, tc.wantArgs, tc.wantOK)
			}
			if ok && string(args) != tc.wantArgs {
				t.Errorf("args = %s, want %s", args, tc.wantArgs)
			}
		})
	}
}

func TestStripThink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"no tags here", "no tags here"},
		{"<think>reasoning</think>{\"tool\":\"x\"}", `{"tool":"x"}`},
		{"a<think>one</think>b<think>two</think>c", "abc"},
		{"<think>unterminated tail", ""},
		{"{}", "{}"},
	}
	for _, tc := range cases {
		if got := stripThink(tc.in); got != tc.want {
			t.Errorf("stripThink(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// newTestClassifier builds the real appClassifier over the builtin registry.
func newTestClassifier(t *testing.T, runner *fakeRunner) *appClassifier {
	t.Helper()
	set, err := tools.Builtin(tools.Options{Workspace: repoRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(set...)
	if err != nil {
		t.Fatal(err)
	}
	return &appClassifier{llm: runner, reg: reg}
}

// TestClassifyHit: the chosen tool runs through the schema-validated
// FuncTool path and the answer is the marshaled tool output.
func TestClassifyHit(t *testing.T) {
	runner := &fakeRunner{text: `{"tool":"read_csv","args":{"path":"testdata/sample.csv","max_rows":2}}`}
	c := newTestClassifier(t, runner)

	answer, tool, ok := c.Classify(context.Background(), "show me the first rows of the sample csv")
	if !ok || tool != "read_csv" {
		t.Fatalf("Classify = (%q, %q, %v), want read_csv hit", answer, tool, ok)
	}
	if !strings.Contains(answer, `"1001"`) {
		t.Errorf("answer = %s, want the read_csv payload", answer)
	}
	// The prompt carries the fixed instruction plus the catalog and request.
	p := runner.prompts[0]
	for _, want := range []string{"text classification engine", "- read_csv:", "- freeform:", "Request: show me"} {
		if !strings.Contains(p, want) {
			t.Errorf("classify prompt missing %q:\n%s", want, p)
		}
	}
}

// TestClassifyDeclines: every failure mode declines (ok=false) — freeform,
// hallucinated tools, tool-level arg rejection, and provider errors. None
// of them may surface as an error.
func TestClassifyDeclines(t *testing.T) {
	cases := []struct {
		name   string
		runner *fakeRunner
	}{
		{"freeform choice", &fakeRunner{text: `{"tool":"freeform","args":{}}`}},
		{"unparseable model text", &fakeRunner{text: "I refuse to answer in JSON."}},
		{"hallucinated tool name", &fakeRunner{text: `{"tool":"rm_rf","args":{}}`}},
		{"tool rejects the args", &fakeRunner{text: `{"tool":"read_csv","args":{"path":"../../etc/passwd"}}`}},
		{"catalog key miss", &fakeRunner{text: `{"tool":"system_command","args":{"command":"rm -rf /"}}`}},
		{"provider error", &fakeRunner{err: errors.New("fast model down")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClassifier(t, tc.runner)
			answer, tool, ok := c.Classify(context.Background(), "anything")
			if ok {
				t.Fatalf("Classify = (%q, %q, true), want decline", answer, tool)
			}
		})
	}
}
