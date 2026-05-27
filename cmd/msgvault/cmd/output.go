package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

// Common flag variables used across aggregate commands.
var (
	aggLimit  int
	aggAfter  string
	aggBefore string
	aggJSON   bool
)

// parseCommonFlags converts string flags to AggregateOptions.
func parseCommonFlags() (query.AggregateOptions, error) {
	opts := query.DefaultAggregateOptions()

	if aggLimit > 0 {
		opts.Limit = aggLimit
	}

	if aggAfter != "" {
		t, err := time.Parse("2006-01-02", aggAfter)
		if err != nil {
			return opts, fmt.Errorf("invalid after date: %w", err)
		}
		opts.After = &t
	}

	if aggBefore != "" {
		t, err := time.Parse("2006-01-02", aggBefore)
		if err != nil {
			return opts, fmt.Errorf("invalid before date: %w", err)
		}
		opts.Before = &t
	}

	return opts, nil
}

// addCommonAggregateFlags adds shared flags to aggregate commands.
func addCommonAggregateFlags(cmd *cobra.Command) {
	cmd.Flags().IntVarP(
		&aggLimit, "limit", "n", 50, "Maximum number of results",
	)
	cmd.Flags().StringVar(
		&aggAfter, "after", "",
		"Filter to messages after date (YYYY-MM-DD)",
	)
	cmd.Flags().StringVar(
		&aggBefore, "before", "",
		"Filter to messages before date (YYYY-MM-DD)",
	)
	cmd.Flags().BoolVar(
		&aggJSON, flagJSON, false, "Output as JSON",
	)
}

// outputAggregateTable prints aggregate results as a table.
func outputAggregateTable(
	rows []query.AggregateRow, keyHeader string,
) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(
		w, "%s\tCOUNT\tSIZE\tATT SIZE\n",
		strings.ToUpper(keyHeader),
	)
	_, _ = fmt.Fprintln(
		w,
		strings.Repeat("─", len(keyHeader))+
			"\t─────\t────\t────────",
	)

	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			truncate(row.Key, 40),
			row.Count,
			formatSize(row.TotalSize),
			formatSize(row.AttachmentSize),
		)
	}
	_ = w.Flush()
	fmt.Printf("\nShowing %d results\n", len(rows))
}

// outputAggregateJSON prints aggregate results as JSON.
func outputAggregateJSON(rows []query.AggregateRow) error {
	output := make([]map[string]any, len(rows))
	for i, row := range rows {
		output[i] = map[string]any{
			"key":             row.Key,
			"count":           row.Count,
			"total_size":      row.TotalSize,
			"attachment_size": row.AttachmentSize,
		}
	}
	return printJSON(output)
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1fK", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
