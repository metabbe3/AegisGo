package store

import "strings"

// normalize canonicalizes a prompt for clustering: quoted strings, path-like
// tokens, and numbers become placeholders so "summarize data/2024.csv" and
// "summarize data/2025.csv" land in the same bucket. This is the Phase-3
// miner's clustering key; quality here decides rule quality there.
func normalize(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	// Quoted spans collapse to <q> first; they may contain paths/numbers.
	var b strings.Builder
	inQuote := false
	for _, r := range p {
		switch {
		case r == '"' || r == '\'' || r == '`':
			if !inQuote {
				inQuote = true
				b.WriteString("<q>")
			} else {
				inQuote = false
			}
		case inQuote:
			// swallow quoted content
		default:
			b.WriteRune(r)
		}
	}

	fields := strings.Fields(b.String())
	for i, f := range fields {
		switch {
		case strings.ContainsAny(f, `/\`):
			fields[i] = "<path>"
		case looksNumeric(f):
			fields[i] = "<n>"
		}
	}
	return strings.Join(fields, " ")
}

// looksNumeric reports whether the field is a bare number (3, 3.5, 1,000).
func looksNumeric(f string) bool {
	hasDigit := false
	for _, r := range f {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.' || r == ',':
			// separators allowed
		default:
			return false
		}
	}
	return hasDigit
}
