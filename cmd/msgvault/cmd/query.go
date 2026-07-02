package cmd

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

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
		return runHTTPQuery(cmd, args[0])
	},
}

func runHTTPQuery(cmd *cobra.Command, sqlStr string) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.RunSQLQuery(cmd.Context(), sqlStr)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	return writeQueryResult(cmd.OutOrStdout(), result, queryFormat)
}

func writeQueryResult(w io.Writer, result *query.QueryResult, format string) error {
	if result == nil {
		return errors.New("nil query result")
	}
	switch format {
	case outputFormatJSON:
		return writeJSON(w, result.Columns, result.Rows)
	case "csv":
		return writeCSV(w, result.Columns, result.Rows)
	case "table":
		return writeTable(w, result.Columns, result.Rows)
	default:
		return fmt.Errorf("unknown format %q (use json, csv, or table)", format)
	}
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

// displayVal formats a value for CSV/table output. SQL NULLs become
// empty strings; floats use plain decimal notation (fmt's %v switches
// to scientific notation at ~1e6); other values use fmt.Sprintf.
func displayVal(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	default:
		return fmt.Sprintf("%v", v)
	}
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

// nil error return mirrors writeJSON/writeCSV so the format switch can
// `return writeTable(...)` uniformly; text printing never fails.

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
		&queryFormat, "format", outputFormatJSON,
		"Output format: json, csv, or table",
	)
}
