package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newDiagnoseFTSCmd() *cobra.Command {
	var opts store.FTSDiagnosticOptions
	var jsonOut bool
	command := &cobra.Command{
		Use:   "diagnose-fts",
		Short: "Check a bounded page of stored search documents",
		Long: `Compare search documents with the stored body and envelope, without changing
the archive or contacting providers. Runs through the configured daemon.

Each message is read in one SQL snapshot. Pages observe different times;
use a quiescent isolated copy for a stable archive-wide result. A match checks
the stored representation, not physical index postings or original MIME.

Only a complete page interval with no defects or inconclusive comparisons
returns success. Continue with --cursor from next_cursor, using the same archive.
A lost response can be retried with the original cursor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Limit < 1 || opts.Limit > 1000 || opts.Timeout < time.Second || opts.Timeout > 5*time.Minute {
				return usageErr(cmd, errors.New("--limit must be 1..1000 and --timeout must be 1s..5m"))
			}
			if !isDaemonCLISubprocess() {
				// Older daemons refuse this unrecognized command in the runner's
				// allowlist, even if an unrelated feature uses the same API minor.
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			cfg, _ := invocationConfigLogger(invocationFromCommand(cmd))
			if cfg == nil {
				return errors.New("configuration is unavailable")
			}
			st, err := store.OpenReadOnlyContext(cmd.Context(), cfg.DatabaseDSN())
			if err != nil {
				return errors.New("cannot open archive read-only for FTS diagnosis")
			}
			defer func() { _ = st.Close() }()
			if err := st.ValidateSchemaContext(cmd.Context()); err != nil {
				return err
			}
			return runFTSDiagnostic(cmd, st, opts, jsonOut)
		},
	}
	command.Flags().StringVar(&opts.Cursor, "cursor", "", "Resume the reported ID interval on this archive")
	command.Flags().IntVar(&opts.Limit, "limit", 100, "Maximum messages to check (1..1000)")
	command.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Maximum diagnostic runtime (1s..5m)")
	command.Flags().BoolVar(&jsonOut, "json", false, "Output the bounded report as JSON")
	return command
}

func runFTSDiagnostic(cmd *cobra.Command, st *store.Store, opts store.FTSDiagnosticOptions, jsonOut bool) error {
	report, runErr := st.DiagnoseFTSContext(cmd.Context(), opts)
	if err := writeFTSDiagnostic(cmd.OutOrStdout(), report, jsonOut); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if report.Outcome != "ok" {
		return fmt.Errorf("FTS diagnostic result: %s", report.Outcome)
	}
	return nil
}

func writeFTSDiagnostic(w io.Writer, r store.FTSDiagnosticReport, jsonOut bool) error {
	if jsonOut {
		data, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
		if _, err = fmt.Fprintln(w, string(data)); err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(w, "FTS %s: %s; checked %d, matched %d, defects %d, inconclusive %d; complete=%t\n",
		r.Backend, r.Outcome, r.Checked, r.Matched, r.Defects, r.Inconclusive, r.Complete); err != nil {
		return fmt.Errorf("write FTS diagnostic: %w", err)
	}
	for _, entry := range r.Entries {
		if entry.State == "matched" {
			continue
		}
		if _, err := fmt.Fprintf(w, "Message %d: %s", entry.MessageID, entry.State); err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
		for _, issue := range entry.Issues {
			if _, err := fmt.Fprintf(w, "; %s", ftsDiagnosticDescription(issue)); err != nil {
				return fmt.Errorf("write FTS diagnostic: %w", err)
			}
		}
		if entry.Reason != "" {
			if _, err := fmt.Fprintf(w, "; %s", ftsDiagnosticDescription(entry.Reason)); err != nil {
				return fmt.Errorf("write FTS diagnostic: %w", err)
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
	}
	if r.Reason != "" {
		if _, err := fmt.Fprintln(w, ftsDiagnosticDescription(r.Reason)); err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
	}
	if r.NextCursor != "" {
		if _, err := fmt.Fprintf(w, "Continue: msgvault diagnose-fts --cursor %s\n", r.NextCursor); err != nil {
			return fmt.Errorf("write FTS diagnostic: %w", err)
		}
	}
	return nil
}

func ftsDiagnosticDescription(code string) string {
	switch code {
	case "missing":
		return "search document absent"
	case "obsolete_version":
		return "indexing version differs from the current format"
	case "document_mismatch":
		return "search document differs from stored data"
	case "auxiliary_mismatch":
		return "phrase-search fields differ from the expected fields"
	case "identity_mismatch":
		return "FTS row identity differs from its message"
	case "input_limit":
		return "comparison input exceeds the diagnostic limit"
	case "expected_vector_overflow":
		return "expected PostgreSQL vector cannot be constructed; cause of stored state unproven"
	case "message_disappeared":
		return "message disappeared after selection"
	case "fts_unavailable":
		return "search index is unavailable"
	case "page_limit":
		return "more messages remain in the selected interval"
	case "canceled", "interrupted":
		return "verification interrupted"
	case "timeout":
		return "verification reached its time limit"
	case "selection_failed", "comparison_failed":
		return "SQL verification failed; retry from the reported cursor"
	default:
		return code
	}
}

func init() { rootCmd.AddCommand(newDiagnoseFTSCmd()) }
