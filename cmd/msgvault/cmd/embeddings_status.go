package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/vector"
)

const embeddingsStatusMinAPISchemaVersion = "3.11.0"

var embeddingsStatusJSON, embeddingsStatusWatch bool
var embeddingsStatusSource int64
var embeddingsStatusInterval time.Duration

var embeddingsStatusCmd = &cobra.Command{
	Use: "status", Short: "Show embedding coverage, live progress, and batch timings", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if embeddingsStatusSource < 0 || cmd.Flags().Changed("source") && embeddingsStatusSource == 0 {
			return usageErr(cmd, errors.New("--source must be positive"))
		}
		if embeddingsStatusInterval < time.Second {
			return usageErr(cmd, errors.New("--interval must be at least 1s"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		schemaVersion, err := client.APISchemaVersion(cmd.Context())
		if err != nil {
			return fmt.Errorf("check daemon embedding status capability: %w", err)
		}
		if !daemonclient.APISchemaVersionAtLeast(schemaVersion, embeddingsStatusMinAPISchemaVersion) {
			return fmt.Errorf("embeddings status requires daemon API schema %s or newer (daemon reports %q); upgrade and restart the daemon", embeddingsStatusMinAPISchemaVersion, schemaVersion)
		}
		for {
			status, err := client.EmbeddingStatus(cmd.Context(), embeddingsStatusSource)
			if err != nil {
				return err
			}
			if err := writeEmbeddingStatus(cmd.OutOrStdout(), *status, embeddingsStatusJSON); err != nil {
				return err
			}
			if !embeddingsStatusWatch {
				return nil
			}
			timer := time.NewTimer(embeddingsStatusInterval)
			select {
			case <-cmd.Context().Done():
				timer.Stop()
				return cmd.Context().Err()
			case <-timer.C:
			}
		}
	},
}

func writeEmbeddingStatus(out io.Writer, s vector.EmbeddingStatus, asJSON bool) error {
	if asJSON {
		return json.MarshalEncode(jsontext.NewEncoder(out), s)
	}
	rate, eta := "unknown", "unknown"
	if s.MessagesPerMinute != nil {
		rate = fmt.Sprintf("%.1f messages/min", *s.MessagesPerMinute)
	}
	if s.ETASeconds != nil {
		eta = formatEmbeddingETA(*s.ETASeconds)
	}
	if _, err := fmt.Fprintf(out, "Generation #%d: %s\nCurrent: %d/%d  pending: %d\nJob: %s  phase: %s (%.1fs)  holding scheduler slot: %t\nScheduler: registered=%t schedule=%q after sync=%t slot=%s\nThroughput: %s  ETA: %s  window: %d batches / %.1fs\n",
		s.Generation.ID, s.Generation.State, s.Current, s.Eligible, s.Pending, s.Job.State, s.Job.Phase, s.Job.PhaseSeconds, s.Job.HoldingSchedulerSlot, s.Scheduler.Registered, s.Scheduler.Schedule, s.Scheduler.RunAfterSync, s.Scheduler.SlotHolder, rate, eta, s.WindowBatches, s.WindowSeconds); err != nil {
		return fmt.Errorf("write embedding status: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Latest message-embedding pass failures: %d (all sources)\n", s.Failed); err != nil {
		return fmt.Errorf("write embedding status: %w", err)
	}
	if s.Diagnostics == nil {
		if _, err := fmt.Fprintln(out, "Batch diagnostics: unavailable until a new embedding pass runs"); err != nil {
			return fmt.Errorf("write embedding status: %w", err)
		}
		return nil
	}
	if s.Diagnostics.LastError != nil {
		if _, err := fmt.Fprintf(out, "Sampled pass error: %s\n", s.Diagnostics.LastError.Code); err != nil {
			return fmt.Errorf("write embedding status: %w", err)
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BATCH\tPHASE\tATTEMPTED\tCOMPLETED\tCHARS\tPROVIDER ms\tREQUEST ms\tDB WRITE ms\tELAPSED ms\tREQUESTS\tRETRIES\t429s")
	write := func(b vector.EmbeddingBatch) {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%d\t%d\n", b.Sequence, b.Phase, b.Attempted, b.Completed, b.Chars, b.ProviderMS, b.RequestMS, b.DBWriteMS, b.ElapsedMS, b.Requests, b.Retries, b.RateLimits)
	}
	for _, b := range s.Diagnostics.RecentBatches {
		write(b)
	}
	if b := s.Diagnostics.CurrentBatch; b != nil {
		write(*b)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush embedding status: %w", err)
	}
	return nil
}

// maxDisplayedEmbeddingETA keeps a stalled rate from overflowing time.Duration.
const maxDisplayedEmbeddingETA = 10000 * time.Hour

func formatEmbeddingETA(seconds float64) string {
	if seconds >= maxDisplayedEmbeddingETA.Seconds() {
		return "more than " + maxDisplayedEmbeddingETA.String()
	}
	return time.Duration(seconds * float64(time.Second)).Round(time.Second).String()
}

func init() {
	embeddingsStatusCmd.Flags().BoolVar(&embeddingsStatusJSON, "json", false, "Output JSON (one snapshot per line with --watch)")
	embeddingsStatusCmd.Flags().BoolVar(&embeddingsStatusWatch, "watch", false, "Repeat status until interrupted")
	embeddingsStatusCmd.Flags().DurationVar(&embeddingsStatusInterval, "interval", 5*time.Second, "Polling interval with --watch (minimum 1s)")
	embeddingsStatusCmd.Flags().Int64Var(&embeddingsStatusSource, "source", 0, "Filter coverage by source ID; diagnostics remain generation-wide")
	embeddingsCmd.AddCommand(embeddingsStatusCmd)
}
