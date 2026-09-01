package tools

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	_ "modernc.org/sqlite" // embedded database driver (pure Go)
)

// SQL tool policy (see CLAUDE.md):
//   - read-only by default: the embedded connection opens with mode=ro, so
//     the engine itself rejects writes; AEGIS_SQL_MODE=rw unlocks DML.
//     For external DSNs without an ro flag, a statement-prefix allowlist
//     provides the same guard.
//   - single statement only (multi-statement strings rejected).
//   - string literals must be bound parameters, not inline quotes: a query
//     containing '...' with zero placeholders is rejected. Placeholder
//     count must match args length.
//
// External databases: set AEGIS_SQL_DSN to any database/sql DSN and import
// its driver (WORKFLOW.md has the recipe); the audit store stays SQLite.

// sqlMaxRows caps rows returned to keep tool output (and model context) bounded.
const sqlMaxRows = 200

// sqlMaxLoadRows caps CSV rows ingested into a temp table.
const sqlMaxLoadRows = 100_000

// SQLOptions configures the sql_query tool.
type SQLOptions struct {
	// DSN is the database/sql DSN. Empty uses the embedded SQLite file
	// (path must be set then).
	DSN string
	// Path is the embedded SQLite file path (ignored when DSN is set).
	Path string
	// Mode is "ro" (default) or "rw".
	Mode string
}

// SQLInput is the schema for the sql_query tool.
type SQLInput struct {
	// Query is a single SQL statement. Values must be '?' placeholders
	// bound via args, not inline literals.
	Query string `json:"query"`
	// Args are the bound parameter values, in order.
	Args []any `json:"args,omitempty"`
	// AttachCSVs loads workspace CSV files into queryable temp tables
	// before running Query.
	AttachCSVs []CSVTable `json:"attach_csvs,omitempty"`
}

// CSVTable binds a workspace CSV to a temp table name.
type CSVTable struct {
	// Name is the temp table name (letters, digits, underscore only).
	Name string `json:"name"`
	// Path of the CSV file, relative to the workspace root.
	Path string `json:"path"`
}

// SQLOutput is the result of sql_query.
type SQLOutput struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowsReturned int      `json:"rows_returned"`
	Truncated    bool     `json:"truncated"`
}

// NewSQLQuery builds the sql_query tool bound to a workspace root.
func NewSQLQuery(workspace string, opts SQLOptions) (Tool, error) {
	mode := opts.Mode
	if mode == "" {
		mode = "ro"
	}
	open := func() (*sql.DB, error) { return openSQLDB(opts, mode) }

	t, err := New(Config{
		Name:        "sql_query",
		Description: "Run a single SQL query. Default database is embedded SQLite (read-only). Values must be passed as args placeholders (?), not inline literals. Use attach_csvs to query workspace CSV files as temp tables. Max 200 rows returned.",
	}, func(ctx context.Context, in SQLInput) (SQLOutput, error) {
		if err := validateQuery(in.Query, in.Args, mode); err != nil {
			return SQLOutput{}, err
		}
		db, err := open()
		if err != nil {
			return SQLOutput{}, err
		}
		defer db.Close()
		return querySQL(ctx, db, workspace, in)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func openSQLDB(opts SQLOptions, mode string) (*sql.DB, error) {
	dsn := opts.DSN
	if dsn == "" {
		path := opts.Path
		if path == "" {
			path = "aegisgo.db"
		}
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
		if mode == "ro" {
			dsn = fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sql database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// readOnlyPrefixes is the statement allowlist applied when the engine cannot
// enforce read-only itself (external DSNs) — belt over the mode=ro braces.
var readOnlyPrefixes = []string{"select", "with", "explain"}

var identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sqliteReserved blocks temp-table names that would collide with SQL
// keywords (not exhaustive — the common foot-guns).
var sqliteReserved = map[string]bool{
	"order": true, "group": true, "table": true, "select": true, "index": true,
	"where": true, "from": true, "join": true, "on": true, "as": true,
	"having": true, "limit": true, "values": true, "set": true, "by": true,
	"into": true, "and": true, "or": true, "not": true, "null": true,
}

// validateQuery enforces the tool's SQL policy before anything executes.
func validateQuery(q string, args []any, mode string) error {
	q = strings.TrimSpace(q)
	q = strings.TrimSuffix(q, ";")
	if q == "" {
		return fmt.Errorf("query is required")
	}
	if strings.Contains(q, ";") {
		return fmt.Errorf("multi-statement queries are not allowed")
	}

	placeholders := strings.Count(q, "?")
	if placeholders > 0 && placeholders != len(args) {
		return fmt.Errorf("query has %d placeholders but %d args", placeholders, len(args))
	}

	// Inline string literals with no bound values are the classic injection
	// shape: require placeholders for them.
	if strings.Contains(q, "'") && placeholders == 0 {
		return fmt.Errorf("queries containing string literals must bind those values as '?' placeholders in args")
	}

	if mode != "rw" {
		lower := strings.ToLower(q)
		for _, p := range readOnlyPrefixes {
			if strings.HasPrefix(lower, p) {
				return nil
			}
		}
		return fmt.Errorf("read-only mode allows %s statements only (set AEGIS_SQL_MODE=rw to enable writes)",
			strings.Join(readOnlyPrefixes, "/"))
	}
	return nil
}

func querySQL(ctx context.Context, db *sql.DB, workspace string, in SQLInput) (SQLOutput, error) {
	// Temp tables are per-connection: pin one conn for attach + query.
	conn, err := db.Conn(ctx)
	if err != nil {
		return SQLOutput{}, err
	}
	defer conn.Close()

	for _, at := range in.AttachCSVs {
		if err := attachCSV(ctx, conn, workspace, at); err != nil {
			return SQLOutput{}, err
		}
	}

	args := in.Args
	if args == nil {
		args = []any{}
	}
	rows, err := conn.QueryContext(ctx, in.Query, args...)
	if err != nil {
		return SQLOutput{}, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return SQLOutput{}, err
	}
	out := SQLOutput{Columns: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return SQLOutput{}, fmt.Errorf("scanning row: %w", err)
		}
		out.Rows = append(out.Rows, vals)
		if len(out.Rows) >= sqlMaxRows {
			out.Truncated = true
			break
		}
	}
	out.RowsReturned = len(out.Rows)
	return out, rows.Err()
}

// attachCSV loads a workspace CSV into a per-connection temp table with all
// TEXT columns. Table lifetime is the connection's — nothing persists.
func attachCSV(ctx context.Context, conn *sql.Conn, workspace string, at CSVTable) error {
	if !identifierRe.MatchString(at.Name) {
		return fmt.Errorf("invalid table name %q (use letters, digits, underscore)", at.Name)
	}
	if sqliteReserved[strings.ToLower(at.Name)] {
		return fmt.Errorf("table name %q is a SQL reserved word", at.Name)
	}
	path, err := resolvePath(workspace, at.Path)
	if err != nil {
		return err
	}
	records, headers, err := readAllCSV(path, sqlMaxLoadRows)
	if err != nil {
		return err
	}

	cols := make([]string, len(headers))
	for i, h := range headers {
		c := fmt.Sprintf("c%02d_%s", i, sanitizeIdent(h))
		cols[i] = c
	}
	colDefs := make([]string, len(cols))
	for i, c := range cols {
		colDefs[i] = c + " TEXT"
	}
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("DROP TABLE IF EXISTS temp.%s", at.Name)); err != nil {
		return fmt.Errorf("dropping stale temp table: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("CREATE TEMP TABLE %s (%s)", at.Name, strings.Join(colDefs, ","))); err != nil {
		return fmt.Errorf("creating temp table: %w", err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES (%s)", at.Name, placeholders))
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close()
	for _, rec := range records {
		row := make([]any, len(cols))
		for i := range row {
			if i < len(rec) {
				row[i] = rec[i]
			}
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			tx.Rollback()
			return fmt.Errorf("loading csv row: %w", err)
		}
	}
	return tx.Commit()
}

// sanitizeIdent makes a CSV header usable as a column suffix.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "col"
	}
	return out
}
