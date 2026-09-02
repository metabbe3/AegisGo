package router

// Seeded rules: the deterministic shortcuts shipped by default. They cover
// the ops/data commands that should never cost an LLM call. Edit here (or
// the rules table — see internal/router/rulesstore.go) to extend.
//
// Order matters: first match wins, so specific patterns (with optional
// groups present) must precede general ones.

// Seeded returns the default rule set.
func Seeded() []RuleDef {
	return []RuleDef{
		{Name: "uptime", Pattern: `/uptime`, Tool: "system_command",
			ArgsTemplate: `{"command":"uptime"}`, Origin: "seed"},
		{Name: "disk", Pattern: `/disk|/df`, Tool: "system_command",
			ArgsTemplate: `{"command":"disk"}`, Origin: "seed"},
		{Name: "memory", Pattern: `/memory|/free`, Tool: "system_command",
			ArgsTemplate: `{"command":"memory"}`, Origin: "seed"},
		{Name: "hostname", Pattern: `/hostname`, Tool: "system_command",
			ArgsTemplate: `{"command":"hostname"}`, Origin: "seed"},
		{Name: "kernel", Pattern: `/kernel|/uname`, Tool: "system_command",
			ArgsTemplate: `{"command":"kernel"}`, Origin: "seed"},
		{Name: "who", Pattern: `/who`, Tool: "system_command",
			ArgsTemplate: `{"command":"who"}`, Origin: "seed"},
		// csv_head with an explicit row count (specific first: optional
		// groups splice empty when absent, so the general form omits them).
		{Name: "csv_head_n", Pattern: `/csv_head\s+(\S+)\s+(\d+)`, Tool: "read_csv",
			ArgsTemplate: `{"path":"$1","max_rows":$2}`, Origin: "seed"},
		{Name: "csv_head", Pattern: `/csv_head\s+(\S+)`, Tool: "read_csv",
			ArgsTemplate: `{"path":"$1"}`, Origin: "seed"},
		{Name: "csv_summary", Pattern: `/csv_summary\s+(\S+)`, Tool: "csv_stats",
			ArgsTemplate: `{"path":"$1"}`, Origin: "seed"},
		// File tools: workspace paths, shape-validated captures. The
		// patterns intentionally allow ".."-shaped captures — resolvePath /
		// resolveNewPath are the security backstop (same contract as
		// csv_head above). "to" in download is optional but keeps the
		// pattern anchored and shape-checked; both captures splice in the
		// quoted "$N" form.
		{Name: "mkdir", Pattern: `/mkdir\s+([A-Za-z0-9._][A-Za-z0-9._/-]*)`, Tool: "make_dir",
			ArgsTemplate: `{"path":"$1"}`, Origin: "seed"},
		{Name: "download", Pattern: `/download\s+(https?://[^\s]+)\s+(?:to\s+)?([A-Za-z0-9._][A-Za-z0-9._/-]*)`, Tool: "download",
			ArgsTemplate: `{"url":"$1","path":"$2"}`, Origin: "seed"},
		// Specific form (with a path) precedes the bare /ls general form.
		{Name: "list_dir", Pattern: `/(?:ls|list)\s+([A-Za-z0-9._][A-Za-z0-9._/-]*)`, Tool: "list_dir",
			ArgsTemplate: `{"path":"$1"}`, Origin: "seed"},
		{Name: "ls_root", Pattern: `/ls`, Tool: "list_dir",
			ArgsTemplate: `{"path":"."}`, Origin: "seed"},
		{Name: "job_status", Pattern: `/jobs?\s+([A-Za-z0-9_-]+)`, Tool: "job_status",
			ArgsTemplate: `{"job_id":"$1"}`, Origin: "seed"},
	}
}
