package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aegisgo/internal/engine"
	"aegisgo/internal/tools"
)

// appClassifier is the engine.Classifier implementation: a Dify-style
// question-classify tier running on the fast model. One fixed-prompt call
// picks a native tool; the chosen tool runs through the SAME schema-
// validated path the LLM uses (FuncTool().Call) — classifier output is
// model output, so the router's lighter Execute validation (trusted rule
// args only) never applies to it. Every failure mode declines (ok=false)
// and falls through to the smart LLM; nothing here can fail a request.
type appClassifier struct {
	llm engine.LLMRunner
	reg *tools.Registry
	// prompt is the full byte-stable prefix (instruction + tool catalog),
	// built once: the registry is fixed after boot, and re-serializing the
	// catalog per call would only churn provider prompt caches.
	prompt string
}

// classifyInstruction is the byte-stable prefix of the classifier prompt
// (kept identical across calls so provider prompt caching can work — the
// Hermes cost lesson). Only the request line varies.
const classifyInstruction = `You are a text classification engine for a workspace agent. Analyze the request and answer with exactly ONE JSON object and nothing else:
{"tool":"<tool name>","args":{<the tool's arguments as JSON>}}
Pick the single best tool, or "freeform" when the request needs reasoning, writing, or no listed tool. File paths in args are workspace-relative (e.g. "data/sales.csv"), never absolute.
Example — request "show the first 5 rows of data/sales.csv" → {"tool":"read_csv","args":{"path":"data/sales.csv","max_rows":5}}
Tool catalog:`

// newClassifierPrompt serializes the byte-stable prompt prefix: the fixed
// instruction plus the registry's tool catalog and the freeform escape.
// Called once per boot; only the request line is appended per call.
func newClassifierPrompt(reg *tools.Registry) string {
	var sb strings.Builder
	sb.WriteString(classifyInstruction)
	for _, t := range reg.All() {
		sb.WriteString("\n- ")
		sb.WriteString(t.Name())
		sb.WriteString(": ")
		sb.WriteString(t.Description())
	}
	sb.WriteString("\n- freeform: the request needs reasoning, writing, or no listed tool")
	return sb.String()
}

// Classify runs the fast tier for one prompt. ok=false means "not
// classifiable" — the engine falls through to the smart LLM.
func (c *appClassifier) Classify(ctx context.Context, prompt string) (string, string, bool) {
	resp, err := c.llm.RunText(ctx, c.prompt+"\n\nRequest: "+prompt).Collect()
	if err != nil {
		return "", "", false
	}
	tool, args, ok := parseClassify(resp.String())
	if !ok || tool == "freeform" {
		return "", "", false
	}
	tl, found := c.reg.Get(tool)
	if !found {
		return "", "", false // hallucinated tool name — decline, don't error
	}
	out, err := tl.FuncTool().Call(ctx, string(args))
	if err != nil {
		// The tool rejected the args (schema, workspace sandbox, catalog
		// key): the classifier picked wrong — decline to the smart LLM.
		return "", "", false
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprint(out))
	}
	return string(b), tool, true
}

// parseClassify extracts the model's tool choice. Models wrap JSON in
// prose and reasoning tags, so the LAST parseable JSON object carrying a
// string "tool" wins (answers land at the end; Dify strips <think> blocks
// for the same reason). Scanning from the end returns on the first hit —
// the same winner the old keep-overwriting forward pass picked, without
// re-decoding every tail of the text. args may be absent — an empty object
// is fine for tools without parameters.
func parseClassify(text string) (string, json.RawMessage, bool) {
	text = stripThink(text)
	for i := len(text) - 1; i >= 0; i-- {
		if text[i] != '{' {
			continue
		}
		var m struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		if err := dec.Decode(&m); err == nil && m.Tool != "" {
			if len(m.Args) == 0 {
				m.Args = json.RawMessage(`{}`)
			}
			return m.Tool, m.Args, true
		}
	}
	return "", nil, false
}

// stripThink removes <think>…</think> reasoning wrappers some local models
// emit around the actual answer (Dify strips the same blocks).
func stripThink(s string) string {
	if !strings.Contains(s, "<think>") {
		return s
	}
	var sb strings.Builder
	rest := s
	for {
		open := strings.Index(rest, "<think>")
		if open < 0 {
			sb.WriteString(rest)
			return sb.String()
		}
		sb.WriteString(rest[:open])
		rest = rest[open+len("<think>"):]
		if close := strings.Index(rest, "</think>"); close >= 0 {
			rest = rest[close+len("</think>"):]
		} else {
			return sb.String() // unterminated: drop the tail
		}
	}
}
