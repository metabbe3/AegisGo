package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// renderPayload turns an approval payload into a human sentence
// (AegisGo UX rule: chat is for humans — raw JSON never reaches the
// chat). Unknown shapes degrade to "key: value" lines; camel/snake
// keys become Title Case words.
func renderPayload(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
		// Not a JSON object: show it as-is (bounded), it may be a plain
		// command someone typed.
		if len(raw) > 100 {
			raw = raw[:100] + "…"
		}
		return raw
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s %s", humanKey(k), humanValue(v)))
	}
	// Deterministic order: sort by rendered text.
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, " · ")
}

// humanKey rewrites "command"/"csv_path" → "Command"/"Csv Path" style
// labels without underscores.
func humanKey(k string) string {
	k = strings.ReplaceAll(k, "_", " ")
	words := strings.Fields(k)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// humanValue renders one JSON value without quotes or braces.
func humanValue(v any) string {
	switch t := v.(type) {
	case string:
		s := t
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		// Values are words too: "reload_rules" reads as "reload rules".
		if strings.Contains(s, "_") && !strings.ContainsAny(s, " /:.\\") {
			s = strings.ReplaceAll(s, "_", " ")
		}
		return s
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case nil:
		return "—"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "?"
		}
		s := string(b)
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		return strings.ReplaceAll(strings.ReplaceAll(s, "\"", "'"), "_", " ")
	}
}

// humanKind maps internal kinds to plain labels ("system_command" →
// "System command"); unknown kinds just lose their underscores.
func humanKind(k string) string {
	return strings.ToUpper(k[:1]) + strings.ReplaceAll(k[1:], "_", " ")
}
