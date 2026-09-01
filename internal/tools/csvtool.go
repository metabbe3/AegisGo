package tools

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"
)

// csvPreview is the max rows read_csv returns when max_rows is unset.
const csvPreview = 20

// CSVReadInput is the schema for the read_csv tool.
type CSVReadInput struct {
	// Path of the CSV file, relative to the workspace root.
	Path string `json:"path"`
	// MaxRows caps how many data rows are returned (default 20).
	MaxRows *int `json:"max_rows,omitempty"`
}

// CSVReadOutput is the result of read_csv.
type CSVReadOutput struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	TotalRows int        `json:"total_rows"`
	Truncated bool       `json:"truncated"`
}

// CSVStatsInput is the schema for the csv_stats tool.
type CSVStatsInput struct {
	// Path of the CSV file, relative to the workspace root.
	Path string `json:"path"`
}

// CSVStatsOutput is the result of csv_stats.
type CSVStatsOutput struct {
	TotalRows int         `json:"total_rows"`
	Columns   []CSVColumn `json:"columns"`
}

// CSVColumn describes one CSV column: name, empty-cell count, and a guessed
// kind (number, date, or text).
type CSVColumn struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Empty int    `json:"empty"`
}

// NewReadCSV builds the read_csv tool bound to a workspace root.
func NewReadCSV(workspace string) (Tool, error) {
	return New(Config{
		Name:        "read_csv",
		Description: "Read a CSV file from the workspace. Returns column names, a preview of data rows, and the total row count. Use csv_stats first when you only need shape/type information.",
	}, func(ctx context.Context, in CSVReadInput) (CSVReadOutput, error) {
		limit := csvPreview
		if in.MaxRows != nil && *in.MaxRows >= 0 {
			limit = *in.MaxRows
		}
		return readCSV(workspace, in.Path, limit)
	})
}

// NewCSVStats builds the csv_stats tool bound to a workspace root.
func NewCSVStats(workspace string) (Tool, error) {
	return New(Config{
		Name:        "csv_stats",
		Description: "Get statistics for a CSV file: total rows and, per column, the name, empty-cell count, and detected kind (number, date, or text).",
	}, func(ctx context.Context, in CSVStatsInput) (CSVStatsOutput, error) {
		return csvStats(workspace, in.Path)
	})
}

func readCSV(workspace, name string, limit int) (CSVReadOutput, error) {
	path, err := resolvePath(workspace, name)
	if err != nil {
		return CSVReadOutput{}, err
	}
	f, err := openCSV(path)
	if err != nil {
		return CSVReadOutput{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate ragged rows; agents should see the data as-is

	header, err := r.Read()
	if err != nil {
		return CSVReadOutput{}, fmt.Errorf("reading %q: %w", name, err)
	}

	out := CSVReadOutput{Columns: header}
	// Read to EOF so TotalRows is exact even when the preview is capped;
	// rows beyond the limit are counted, not stored.
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return CSVReadOutput{}, fmt.Errorf("reading %q: %w", name, err)
		}
		out.TotalRows++
		if len(out.Rows) < limit {
			out.Rows = append(out.Rows, rec)
		}
	}
	out.Truncated = out.TotalRows > len(out.Rows)
	return out, nil
}

func csvStats(workspace, name string) (CSVStatsOutput, error) {
	path, err := resolvePath(workspace, name)
	if err != nil {
		return CSVStatsOutput{}, err
	}
	f, err := openCSV(path)
	if err != nil {
		return CSVStatsOutput{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		return CSVStatsOutput{}, fmt.Errorf("reading %q: %w", name, err)
	}

	cols := make([]CSVColumn, len(header))
	allNumeric := make([]bool, len(header))
	allDate := make([]bool, len(header))
	empty := make([]int, len(header))
	for i := range allNumeric {
		allNumeric[i], allDate[i] = true, true
	}

	rows := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return CSVStatsOutput{}, fmt.Errorf("reading %q: %w", name, err)
		}
		rows++
		for i := 0; i < len(header) && i < len(rec); i++ {
			v := rec[i]
			if v == "" {
				empty[i]++
				continue
			}
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				allNumeric[i] = false
			}
			if !looksLikeDate(v) {
				allDate[i] = false
			}
		}
	}

	for i, h := range header {
		cols[i] = CSVColumn{Name: h, Empty: empty[i], Kind: "text"}
		if allNumeric[i] {
			cols[i].Kind = "number"
		} else if allDate[i] {
			cols[i].Kind = "date"
		}
		if rows > 0 && empty[i] == rows {
			cols[i].Kind = "empty"
		}
	}
	return CSVStatsOutput{TotalRows: rows, Columns: cols}, nil
}

// readAllCSV reads every record of a workspace CSV (bounded by maxRows),
// returning records and headers. Used by the SQL tool's attach_csv.
func readAllCSV(path string, maxRows int) ([][]string, []string, error) {
	f, err := openCSV(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	headers, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("reading header: %w", err)
	}
	var records [][]string
	for len(records) < maxRows {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading record: %w", err)
		}
		records = append(records, rec)
	}
	return records, headers, nil
}

// looksLikeDate recognizes the handful of date layouts that realistically
// appear in CSV exports. Good enough for a kind hint; not a validator.
func looksLikeDate(v string) bool {
	layouts := []string{
		time.RFC3339, "2006-01-02", "2006/01/02", "2006-01-02 15:04:05",
		"02/01/2006", "20060102",
	}
	for _, l := range layouts {
		if _, err := time.Parse(l, v); err == nil {
			return true
		}
	}
	return false
}
