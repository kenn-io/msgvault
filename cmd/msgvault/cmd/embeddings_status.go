package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/vector"
)

const embeddingsStatusMinAPISchemaVersion = "3.21.0"

var embeddingsStatusJSON, embeddingsStatusWatch bool
var embeddingsStatusSource int64
var embeddingsStatusInterval time.Duration

var embeddingsStatusCmd = &cobra.Command{
	Use: "status", Short: "Show embedding coverage, live progress, and batch timings", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if embeddingsStatusSource < 0 || cmd.Flags().Changed("source") && embeddingsStatusSource == 0 {
			return usageErr(cmd, errors.New("--source must be positive"))
		}
		if embeddingsStatusWatch && embeddingsStatusInterval < time.Second {
			return usageErr(cmd, errors.New("--interval must be at least 1s"))
		}
		client, info, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		current := client
		defer func() { _ = current.Close() }()
		var reconnect func(context.Context) (embeddingStatusReader, error)
		if inv := invocationFromContext(cmd.Context()); info.Kind == HTTPStoreLocalDaemon && inv != nil && inv.cfg != nil {
			endpoint := localDaemonEndpoint{url: info.URL, key: inv.cfg.Server.AuthenticationKey()}
			reconnect = func(ctx context.Context) (embeddingStatusReader, error) {
				next, moved, err := reconnectLocalDaemon(ctx, endpoint)
				if err != nil {
					return nil, err
				}
				if next == nil {
					return current, nil
				}
				// The old connection's close result does not affect the watch.
				_ = current.Close()
				current, endpoint = next, moved
				return next, nil
			}
		}
		return pollEmbeddingStatus(cmd, client, reconnect)
	},
}

// localDaemonEndpoint is where a status client connects and the API key it
// sends.
type localDaemonEndpoint struct{ url, key string }

// reconnectLocalDaemon connects to the running local daemon when its address
// or API key differs from current, as after a restart on the default port 0
// or a key rotation; it returns a nil client when neither changed. It never
// starts a daemon, so a watch left open after the daemon is stopped does not
// start it again.
func reconnectLocalDaemon(ctx context.Context, current localDaemonEndpoint) (*daemonclient.Client, localDaemonEndpoint, error) {
	inv := invocationFromContext(ctx)
	if inv == nil || inv.cfg == nil {
		return nil, current, errors.New("invocation state is required")
	}
	rt, found, err := findRespondingDaemonRuntime(ctx, inv.cfg.Data.DataDir, func(*DaemonRuntime, error) bool { return true })
	switch {
	case found && err != nil:
		return nil, current, fmt.Errorf("%w: the restarted daemon is incompatible with this client: %w", errEmbeddingStatusUnsupported, err)
	case err != nil:
		return nil, current, fmt.Errorf("find local daemon: %w", err)
	case !found:
		return nil, current, errors.New("local daemon is not running")
	}
	if err := inv.cfg.ResolveServerKey(); err != nil {
		return nil, current, fmt.Errorf("resolve server API key: %w", err)
	}
	next := localDaemonEndpoint{url: urlFromDaemonRuntime(rt), key: inv.cfg.Server.AuthenticationKey()}
	if next == current {
		return nil, current, nil
	}
	client, err := newLocalDaemonClient(ctx, inv.cfg, rt)
	if err != nil {
		return nil, current, err
	}
	return client, next, nil
}

// errEmbeddingStatusUnsupported marks a daemon too old to serve status; a
// watch stops on it instead of retrying.
var errEmbeddingStatusUnsupported = errors.New("embeddings status unsupported by daemon")

// embeddingStatusReader is the daemon client calls the status command makes.
type embeddingStatusReader interface {
	APISchemaVersion(ctx context.Context) (string, error)
	EmbeddingStatus(ctx context.Context, sourceID int64) (*vector.EmbeddingStatus, error)
}

// readEmbeddingStatusOnce checks the daemon's API schema until a check
// succeeds, then reads status.
func readEmbeddingStatusOnce(ctx context.Context, client embeddingStatusReader, checked *bool) (*vector.EmbeddingStatus, error) {
	if !*checked {
		schemaVersion, err := client.APISchemaVersion(ctx)
		if err != nil {
			return nil, fmt.Errorf("check daemon embedding status capability: %w", err)
		}
		if !daemonclient.APISchemaVersionAtLeast(schemaVersion, embeddingsStatusMinAPISchemaVersion) {
			return nil, fmt.Errorf("%w: embeddings status requires daemon API schema %s or newer (daemon reports %q); upgrade and restart the daemon",
				errEmbeddingStatusUnsupported, embeddingsStatusMinAPISchemaVersion, schemaVersion)
		}
		*checked = true
	}
	return client.EmbeddingStatus(ctx, embeddingsStatusSource)
}

// pollEmbeddingStatus prints status once, or until interrupted with --watch.
// A watch reports a failed poll, such as a daemon restart, and keeps polling.
// After a failed poll it reconnects when reconnect is set, because a restarted
// local daemon can listen on a new port. reconnect returns the client to use,
// which is the current one when the daemon has not moved.
func pollEmbeddingStatus(cmd *cobra.Command, client embeddingStatusReader, reconnect func(context.Context) (embeddingStatusReader, error)) error {
	checked := false
	for {
		status, err := readEmbeddingStatusOnce(cmd.Context(), client, &checked)
		switch {
		case err == nil:
			if err := writeEmbeddingStatus(cmd.OutOrStdout(), *status, embeddingsStatusJSON); err != nil {
				return err
			}
		case !embeddingsStatusWatch || cmd.Context().Err() != nil || errors.Is(err, errEmbeddingStatusUnsupported):
			return err
		default:
			// The daemon may have restarted on another version.
			checked = false
			if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "embeddings status: %v; retrying in %s\n", err, embeddingsStatusInterval); writeErr != nil {
				return fmt.Errorf("write embedding status error: %w", writeErr)
			}
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
		if err == nil || reconnect == nil {
			continue
		}
		next, reconnectErr := reconnect(cmd.Context())
		switch {
		case errors.Is(reconnectErr, errEmbeddingStatusUnsupported):
			return reconnectErr
		case reconnectErr != nil:
			if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "embeddings status: reconnect: %v\n", reconnectErr); writeErr != nil {
				return fmt.Errorf("write embedding status error: %w", writeErr)
			}
		default:
			client = next
		}
	}
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
	lines := []string{
		fmt.Sprintf("Generation #%d: %s", s.Generation.ID, s.Generation.State),
		fmt.Sprintf("Current: %d/%d  pending: %d", s.Current, s.Eligible, s.Pending),
		fmt.Sprintf("Job: %s  phase: %s (%.1fs)  holding scheduler slot: %t",
			s.Job.State, s.Job.Phase, s.Job.PhaseSeconds, s.Job.HoldingSchedulerSlot),
		fmt.Sprintf("Scheduler: registered=%t schedule=%q after sync=%t slot=%s",
			s.Scheduler.Registered, s.Scheduler.Schedule, s.Scheduler.RunAfterSync, s.Scheduler.SlotHolder),
		fmt.Sprintf("Throughput: %s  ETA: %s  window: %d batches / %.1fs", rate, eta, s.WindowBatches, s.WindowSeconds),
	}
	if !s.CoverageSampledAt.IsZero() {
		lines = append(lines, fmt.Sprintf("Coverage sampled: %s (%.1fs ago)", s.CoverageSampledAt.UTC().Format(time.RFC3339), max(0, s.SampledAt.Sub(s.CoverageSampledAt).Seconds())))
	}
	if s.SourceID != 0 {
		lines = append(lines, fmt.Sprintf("Coverage source: %d; diagnostics and throughput: all sources in this run", s.SourceID))
	}
	if s.DiagnosticsStale {
		lines = append(lines, "Batch diagnostics: stale")
	}
	if _, err := fmt.Fprintln(out, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("write embedding status: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Latest message-embedding pass failures: %d (all sources)\n", s.Failed); err != nil {
		return fmt.Errorf("write embedding status: %w", err)
	}
	if s.Diagnostics == nil {
		if _, err := fmt.Fprintln(out, "Batch diagnostics: unavailable"); err != nil {
			return fmt.Errorf("write embedding status: %w", err)
		}
		return nil
	}
	return writeEmbeddingBatches(out, *s.Diagnostics)
}

// writeEmbeddingBatches prints the sampled pass's error and its batch table.
func writeEmbeddingBatches(out io.Writer, d vector.EmbeddingDiagnostics) error {
	if d.LastError != nil {
		if _, err := fmt.Fprintf(out, "Sampled pass error: %s\n", d.LastError.Code); err != nil {
			return fmt.Errorf("write embedding status: %w", err)
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	// Write errors surface from Flush.
	_, _ = fmt.Fprintln(w, "BATCH\tPHASE\tATTEMPTED\tCOMPLETED\tCHARS\tPROVIDER ms\tREQUEST ms\t"+
		"DB WRITE ms\tELAPSED ms\tREQUESTS\tRETRIES\t429s")
	write := func(b vector.EmbeddingBatch) {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%d\t%d\n",
			b.Sequence, b.Phase, b.Attempted, b.Completed, b.Chars, b.ProviderMS, b.RequestMS,
			b.DBWriteMS, b.ElapsedMS, b.Requests, b.Retries, b.RateLimits)
	}
	for _, b := range d.RecentBatches {
		write(b)
	}
	if b := d.CurrentBatch; b != nil {
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
		return "more than " + formatETA(maxDisplayedEmbeddingETA)
	}
	return formatETA(time.Duration(seconds * float64(time.Second)))
}

// formatETA renders a duration as a compact h/m/s string, dropping leading
// zero components so "3 hours" reads as "3h02m18s" and "45 seconds"
// as "45s". Rounds to whole seconds.
func formatETA(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func init() {
	embeddingsStatusCmd.Flags().BoolVar(&embeddingsStatusJSON, "json", false, "Output JSON (one snapshot per line with --watch)")
	embeddingsStatusCmd.Flags().BoolVar(&embeddingsStatusWatch, "watch", false, "Repeat status until interrupted")
	embeddingsStatusCmd.Flags().DurationVar(&embeddingsStatusInterval, "interval", 5*time.Second, "Polling interval with --watch (minimum 1s)")
	embeddingsStatusCmd.Flags().Int64Var(&embeddingsStatusSource, "source", 0, "Filter coverage by source ID; diagnostics remain generation-wide")
	embeddingsCmd.AddCommand(embeddingsStatusCmd)
}
