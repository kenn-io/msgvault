package cmd

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

var queryFormat string

var queryCmd = &cobra.Command{
	Use:   "query [sql]",
	Short: "Run a SQL query against the analytics cache",
	Long: `Run arbitrary SQL against the Parquet analytics cache.

The following views are available:
  messages, participants, message_recipients, labels,
  message_labels, attachments, conversations, sources

Convenience views:
  v_messages   - messages with resolved sender and labels
  v_senders    - per-sender aggregates
  v_domains    - per-domain aggregates
  v_labels     - label name with message count and size
  v_threads    - per-conversation aggregates

Output formats:
  json   - JSON object with columns, rows, row_count (default)
  csv    - CSV with header row
  table  - Aligned text table

Examples:
  msgvault query "SELECT from_email, COUNT(*) AS n FROM v_messages GROUP BY 1 ORDER BY 2 DESC LIMIT 10"
  msgvault query --format csv "SELECT * FROM v_senders ORDER BY message_count DESC"
  msgvault query --format table "SELECT name, message_count FROM v_labels"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := cfg.DatabaseDSN()
		analyticsDir := cfg.AnalyticsDir()

		staleness := cacheNeedsBuild(dbPath, analyticsDir)
		if staleness.NeedsBuild {
			fmt.Fprintf(os.Stderr,
				"Building analytics cache (%s)...\n",
				staleness.Reason)
			result, err := buildCache(
				dbPath, analyticsDir, staleness.FullRebuild,
			)
			if err != nil {
				return fmt.Errorf("build cache: %w", err)
			}
			if !result.Skipped {
				fmt.Fprintf(os.Stderr,
					"Cached %d messages.\n",
					result.ExportedCount)
			}
		}

		if !query.HasCompleteParquetData(analyticsDir) {
			return fmt.Errorf(
				"analytics cache is empty — sync some " +
					"messages first")
		}

		return executeQuery(
			analyticsDir, args[0], queryFormat, os.Stdout,
		)
	},
}

// executeQuery opens an in-memory DuckDB, registers views over
// the Parquet files in analyticsDir, runs the SQL, and writes
// the results in the requested format.
func executeQuery(
	analyticsDir, sqlStr, format string, w io.Writer,
) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	threads := runtime.GOMAXPROCS(0)
	if _, err := db.Exec(
		fmt.Sprintf("SET threads = %d", threads),
	); err != nil {
		return fmt.Errorf("set threads: %w", err)
	}

	if err := query.RegisterViews(db, analyticsDir); err != nil {
		return fmt.Errorf("register views: %w", err)
	}

	rows, err := db.Query(sqlStr)
	if err != nil {
		return fmt.Errorf("execute query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("get columns: %w", err)
	}

	var allRows [][]any
	for rows.Next() {
		row, scanErr := scanRow(cols, rows)
		if scanErr != nil {
			return scanErr
		}
		allRows = append(allRows, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	switch format {
	case "json":
		return writeJSON(w, cols, allRows)
	case "csv":
		return writeCSV(w, cols, allRows)
	case "table":
		return writeTable(w, cols, allRows)
	default:
		return fmt.Errorf("unknown format %q (use json, csv, or table)", format)
	}
}

// scanRow scans a single row into a slice of interface{} values,
// converting []byte to string for clean serialization.
func scanRow(
	cols []string, rows *sql.Rows,
) ([]any, error) {
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("scan row: %w", err)
	}
	for i, v := range vals {
		if b, ok := v.([]byte); ok {
			vals[i] = string(b)
		}
	}
	return vals, nil
}

func writeJSON(
	w io.Writer, cols []string, rows [][]any,
) error {
	result := query.QueryResult{
		Columns:  cols,
		Rows:     rows,
		RowCount: len(rows),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// displayVal formats a value for CSV/table output. SQL NULLs
// become empty strings; other values use fmt.Sprintf.
func displayVal(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func writeCSV(
	w io.Writer, cols []string, rows [][]any,
) error {
	cw := csv.NewWriter(w)

	if err := cw.Write(cols); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	for _, row := range rows {
		record := make([]string, len(row))
		for i, v := range row {
			record[i] = displayVal(v)
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}

	cw.Flush()
	return cw.Error()
}

func writeTable(
	w io.Writer, cols []string, rows [][]any,
) error {
	// Convert all values to strings for width calculation
	strRows := make([][]string, len(rows))
	for i, row := range rows {
		strRows[i] = make([]string, len(row))
		for j, v := range row {
			strRows[i][j] = displayVal(v)
		}
	}

	// Calculate column widths (min = header length)
	widths := make([]int, len(cols))
	for i, col := range cols {
		widths[i] = len(col)
	}
	for _, row := range strRows {
		for i, val := range row {
			if len(val) > widths[i] {
				widths[i] = len(val)
			}
		}
	}

	// Print header
	for i, col := range cols {
		if i > 0 {
			_, _ = fmt.Fprint(w, "  ")
		}
		_, _ = fmt.Fprintf(w, "%-*s", widths[i], col)
	}
	_, _ = fmt.Fprintln(w)

	// Print separator
	for i, width := range widths {
		if i > 0 {
			_, _ = fmt.Fprint(w, "  ")
		}
		_, _ = fmt.Fprint(w, strings.Repeat("-", width))
	}
	_, _ = fmt.Fprintln(w)

	// Print rows
	for _, row := range strRows {
		for i, val := range row {
			if i > 0 {
				_, _ = fmt.Fprint(w, "  ")
			}
			_, _ = fmt.Fprintf(w, "%-*s", widths[i], val)
		}
		_, _ = fmt.Fprintln(w)
	}

	// Print row count
	_, _ = fmt.Fprintf(w, "(%d rows)\n", len(rows))
	return nil
}

func init() {
	rootCmd.AddCommand(queryCmd)
	queryCmd.Flags().StringVar(
		&queryFormat, "format", "json",
		"Output format: json, csv, or table",
	)
}
