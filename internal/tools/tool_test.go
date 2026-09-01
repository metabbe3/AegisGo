package tools

import (
	"context"
	"strings"
	"testing"
)

// demoIn/demoOut back a minimal typed handler so New's wiring can be
// asserted directly, independent of any concrete tool.
type demoIn struct {
	Text string `json:"text"`
}

type demoOut struct {
	Text string `json:"text"`
}

// newDemoTool builds a working tool with the given name.
func newDemoTool(t *testing.T, name string) Tool {
	t.Helper()
	tl, err := New(Config{Name: name, Description: "demo tool " + name},
		func(ctx context.Context, in demoIn) (demoOut, error) {
			return demoOut{Text: in.Text}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestToolNameDescription(t *testing.T) {
	tl := newDemoTool(t, "alpha")
	if got := tl.Name(); got != "alpha" {
		t.Errorf("Name() = %q, want %q", got, "alpha")
	}
	if got := tl.Description(); got != "demo tool alpha" {
		t.Errorf("Description() = %q, want %q", got, "demo tool alpha")
	}
}

func TestExecuteEmptyArgs(t *testing.T) {
	tl := newDemoTool(t, "alpha")
	ctx := context.Background()

	// nil args must decode to the zero-value input — the len(args) > 0
	// guard exists so router rules can omit JSON entirely.
	got, err := tl.Execute(ctx, nil)
	if err != nil {
		t.Fatalf("Execute(nil): %v", err)
	}
	out, ok := got.(demoOut)
	if !ok {
		t.Fatalf("Execute(nil) type = %T, want demoOut", got)
	}
	if out.Text != "" {
		t.Errorf("Execute(nil) text = %q, want empty", out.Text)
	}

	// Empty object is the same zero value via the unmarshal path.
	got, err = tl.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("Execute({}): %v", err)
	}
	if out, ok = got.(demoOut); !ok || out.Text != "" {
		t.Errorf("Execute({}) = %+v (%T)", got, got)
	}

	// Positive control: well-formed args reach the handler.
	got, err = tl.Execute(ctx, []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Execute(hi): %v", err)
	}
	if out, ok = got.(demoOut); !ok || out.Text != "hi" {
		t.Errorf("Execute(hi) = %+v (%T)", got, got)
	}
}

func TestExecuteBadJSON(t *testing.T) {
	tl := newDemoTool(t, "alpha")
	_, err := tl.Execute(context.Background(), []byte(`{"text":`))
	if err == nil {
		t.Fatal("malformed args accepted")
	}
	// Actual shape: "decoding args for <name>: <json error>".
	if !strings.Contains(err.Error(), "decoding args for alpha: ") {
		t.Errorf("error = %q, want it to wrap the tool name after %q", err, "decoding args for alpha:")
	}
}

func TestNewPropagatesHandlerError(t *testing.T) {
	// functool.New rejects a nil handler; New must wrap that failure with
	// the tool name so the builder log line is actionable.
	_, err := New[demoIn, demoOut](Config{Name: "alpha", Description: "d"}, nil)
	if err == nil {
		t.Fatal("nil handler accepted")
	}
	if !strings.Contains(err.Error(), "building tool alpha: ") {
		t.Errorf("error = %q, want it to wrap the tool name after %q", err, "building tool alpha:")
	}
}
