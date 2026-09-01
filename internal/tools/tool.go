package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// Tool is AegisGo's common tool contract: one implementation, two entry
// points. Deterministic callers (the hybrid router, the CLI, tests) invoke
// Execute directly with JSON args they constructed themselves; the LLM path
// goes through FuncTool, which carries the JSON schema functool derives
// from the typed handler's In/Out structs plus input validation.
type Tool interface {
	Name() string
	Description() string

	// Execute runs the tool with raw JSON args. Args come from trusted
	// code (router rules), not from model output, so validation beyond
	// unmarshaling is intentionally lighter than the LLM path.
	Execute(ctx context.Context, args json.RawMessage) (any, error)

	// FuncTool returns the agent-framework adapter for the same handler.
	FuncTool() tool.FuncTool
}

// Config names and describes a tool for both entry points.
type Config struct {
	Name        string
	Description string
}

// New builds a dual-entry Tool from a typed handler. The In/Out structs are
// the single source of truth: their field comments become the schema
// description the LLM plans with (via functool), and their shape is what
// Execute unmarshals into.
func New[In, Out any](cfg Config, h func(ctx context.Context, in In) (Out, error)) (Tool, error) {
	ft, err := functool.New(functool.Config{Name: cfg.Name, Description: cfg.Description}, h)
	if err != nil {
		return nil, fmt.Errorf("building tool %s: %w", cfg.Name, err)
	}
	return &dualTool[In, Out]{cfg: cfg, ft: ft, h: h}, nil
}

type dualTool[In, Out any] struct {
	cfg Config
	ft  tool.FuncTool
	h   func(ctx context.Context, in In) (Out, error)
}

func (d *dualTool[In, Out]) Name() string        { return d.cfg.Name }
func (d *dualTool[In, Out]) Description() string { return d.cfg.Description }

func (d *dualTool[In, Out]) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	var in In
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("decoding args for %s: %w", d.cfg.Name, err)
		}
	}
	return d.h(ctx, in)
}

func (d *dualTool[In, Out]) FuncTool() tool.FuncTool { return d.ft }

// Registry indexes tools by name for the router and preserves registration
// order for the LLM tool list.
type Registry struct {
	byName map[string]Tool
	order  []Tool
}

// NewRegistry indexes ts, rejecting duplicate names loudly — a silently
// shadowed tool is a routing bug.
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		if _, dup := r.byName[t.Name()]; dup {
			return nil, fmt.Errorf("duplicate tool name %q", t.Name())
		}
		r.byName[t.Name()] = t
		r.order = append(r.order, t)
	}
	return r, nil
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// All returns the tools in registration order.
func (r *Registry) All() []Tool { return r.order }

// FuncTools returns the LLM-path adapters for every registered tool.
func (r *Registry) FuncTools() []tool.Tool {
	out := make([]tool.Tool, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, t.FuncTool())
	}
	return out
}
