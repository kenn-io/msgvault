//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func runEmbed(cmd *cobra.Command) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := sqlitevec.RegisterExtension(); err != nil {
		return fmt.Errorf("register sqlite-vec: %w", err)
	}

	vecPath := cfg.Vector.DBPath
	if vecPath == "" {
		vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  cfg.DatabaseDSN(),
		Dimension: cfg.Vector.Embeddings.Dimension,
		MainDB:    s.DB(),
	})
	if err != nil {
		return fmt.Errorf("open vectors.db: %w", err)
	}
	defer func() { _ = backend.Close() }()

	gen, rebuildInProgress, err := pickEmbedGeneration(ctx, backend, embedGenerationOpts{
		FullRebuild: embedFullRebuild,
		Model:       cfg.Vector.Embeddings.Model,
		Dimension:   cfg.Vector.Embeddings.Dimension,
		Fingerprint: cfg.Vector.GenerationFingerprint(),
		Confirm: func() bool {
			return embedYes ||
				confirmEmbed(cmd, "Start a full rebuild? This builds a new generation and atomically swaps it in when complete. ")
		},
		Stderr: errOut,
	})
	if err != nil {
		return err
	}

	client := embed.NewClient(embed.Config{
		Endpoint:   cfg.Vector.Embeddings.Endpoint,
		APIKey:     cfg.Vector.Embeddings.APIKey(),
		Model:      cfg.Vector.Embeddings.Model,
		Dimension:  cfg.Vector.Embeddings.Dimension,
		Timeout:    cfg.Vector.Embeddings.Timeout,
		MaxRetries: cfg.Vector.Embeddings.MaxRetries,
	})
	totalPending, err := pendingCount(ctx, backend.DB(), gen)
	if err != nil {
		return fmt.Errorf("count pending: %w", err)
	}

	worker := embed.NewWorker(embed.WorkerDeps{
		Backend:   backend,
		VectorsDB: backend.DB(),
		MainDB:    s.DB(),
		Client:    client,
		Preprocess: embed.PreprocessConfig{
			StripQuotes:        cfg.Vector.Preprocess.StripQuotesEnabled(),
			StripSignatures:    cfg.Vector.Preprocess.StripSignaturesEnabled(),
			StripHTML:          cfg.Vector.Preprocess.StripHTMLEnabled(),
			StripBase64:        cfg.Vector.Preprocess.StripBase64Enabled(),
			StripURLTracking:   cfg.Vector.Preprocess.StripURLTrackingEnabled(),
			CollapseWhitespace: cfg.Vector.Preprocess.CollapseWhitespaceEnabled(),
		},
		MaxInputChars:   cfg.Vector.Embeddings.MaxInputChars,
		BatchSize:       cfg.Vector.Embeddings.BatchSize,
		EmbedTimeout:    cfg.Vector.Embeddings.Timeout,
		EmbedMaxRetries: cfg.Vector.Embeddings.MaxRetries,
		TotalPending:    totalPending,
		Progress:        newProgressPrinter(errOut, totalPending, cfg.Vector.Embeddings.ETAWindow),
	})

	if n, err := worker.ReclaimStale(ctx); err != nil {
		return fmt.Errorf("reclaim stale: %w", err)
	} else if n > 0 {
		fmt.Fprintf(errOut, "Reclaimed %d stale claims.\n", n)
	}

	res, err := worker.RunOnce(ctx, gen)
	if err != nil {
		return fmt.Errorf("embed run: %w", err)
	}
	fmt.Fprintf(out, "Claimed: %d, succeeded: %d, failed: %d, truncated: %d\n",
		res.Claimed, res.Succeeded, res.Failed, res.Truncated)

	// Activation is a function of the generation's final state, not
	// of the cumulative retry counter — transient failures that the
	// worker later recovers from must not block activation, and an
	// active generation must not be re-activated.
	if rebuildInProgress {
		remaining, err := pendingCount(ctx, backend.DB(), gen)
		if err != nil {
			return fmt.Errorf("count pending: %w", err)
		}
		if remaining == 0 {
			if err := backend.ActivateGeneration(ctx, gen); err != nil {
				return fmt.Errorf("activate generation: %w", err)
			}
			fmt.Fprintf(out, "Generation %d activated.\n", gen)
		} else {
			fmt.Fprintf(errOut,
				"Generation %d still has %d pending rows; run `msgvault embeddings resume` again to finish, then it will activate automatically.\n",
				gen, remaining)
		}
	}
	return nil
}

// embedGenerationOpts bundles the inputs pickEmbedGeneration needs.
// Externalized so tests can drive the logic without the command-line
// globals.
type embedGenerationOpts struct {
	FullRebuild bool
	Model       string
	Dimension   int
	Fingerprint string // must equal Model:Dimension
	// Confirm is only called when FullRebuild is true. Returns
	// true if the user agreed to proceed.
	Confirm func() bool
	Stderr  io.Writer
}

// pickEmbedGeneration resolves which generation this embed run
// should target. Returns (gen, rebuildInProgress, err):
//
//   - FullRebuild: prompt for confirmation, then call
//     CreateGeneration. That call reuses an existing building
//     generation with the matching fingerprint (so interrupted
//     rebuilds resume cleanly), or returns ErrBuildingInProgress
//     for a mismatch.
//   - default mode with a building generation matching the configured
//     fingerprint: resume it. Building takes precedence over active so
//     that an in-flight rebuild for the configured model gets drained
//     to completion before the next activation, even if a stale active
//     generation from a different model still exists.
//   - default mode with no matching building but an active generation
//     matching the configured fingerprint: target the active one
//     (incremental top-up).
//   - default mode with a building generation whose fingerprint
//     differs from the config: error — activating it would silently
//     swap models. The user must explicitly retire the stale build or
//     change config.
//   - otherwise: error with a hint to use --full-rebuild.
//
// rebuildInProgress is true whenever the target is a building
// generation; activation is considered only in that case.
func pickEmbedGeneration(ctx context.Context, backend vector.Backend, opts embedGenerationOpts) (vector.GenerationID, bool, error) {
	if opts.FullRebuild {
		if opts.Confirm != nil && !opts.Confirm() {
			return 0, false, fmt.Errorf("aborted")
		}
		gen, err := backend.CreateGeneration(ctx, opts.Model, opts.Dimension, opts.Fingerprint)
		if err != nil {
			return 0, false, fmt.Errorf("create generation: %w", err)
		}
		_, _ = fmt.Fprintf(opts.Stderr, "Building generation %d (%s).\n",
			gen, opts.Fingerprint)
		return gen, true, nil
	}

	// Check building first. The order here matters in two directions:
	//
	//  1. A matching in-flight rebuild gets drained even if an
	//     (older / stale) active generation also exists — otherwise
	//     `msgvault embeddings build` would top up the active index forever and
	//     leave the new build stranded in `building`.
	//
	//  2. A mismatched in-flight rebuild is rejected immediately,
	//     regardless of whether an active generation matches the
	//     config. If we deferred to the active path on a config-match
	//     here, the user could keep embedding into an active index
	//     while the wrong-model build sat unfinished and untouched
	//     beside it.
	building, bErr := backend.BuildingGeneration(ctx)
	if bErr != nil {
		return 0, false, fmt.Errorf("lookup building generation: %w", bErr)
	}
	if building != nil {
		if building.Fingerprint == opts.Fingerprint {
			// Re-run the initial seed if the prior CreateGeneration
			// crashed between inserting the building row and committing
			// the seed. Without this, a resume could "drain" zero
			// pending rows and activate an unseeded generation.
			err := backend.EnsureSeeded(ctx, building.ID)
			switch {
			case err == nil:
				_, _ = fmt.Fprintf(opts.Stderr, "Resuming building generation %d (%s).\n",
					building.ID, building.Fingerprint)
				return building.ID, true, nil
			case errors.Is(err, vector.ErrGenerationNotBuilding):
				// Race: another actor (daemon, concurrent CLI, retire
				// call) moved the generation out of 'building' between
				// BuildingGeneration and EnsureSeeded. Fall through to
				// the active-generation lookup rather than aborting
				// with a fatal error — if the flip was an activation
				// matching our fingerprint, that's exactly the
				// generation we want to top up.
				_, _ = fmt.Fprintf(opts.Stderr,
					"Building generation %d changed state while resuming; re-resolving.\n",
					building.ID)
			default:
				return 0, false, fmt.Errorf("ensure seeded: %w", err)
			}
		} else {
			return 0, false, fmt.Errorf("in-progress rebuild has fingerprint=%q, config has %q — activate or retire it before running with a different model",
				building.Fingerprint, opts.Fingerprint)
		}
	}

	active, err := vector.ResolveActiveForFingerprint(ctx, backend, opts.Fingerprint)
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(opts.Stderr, "Using active generation %d (%s).\n", active.ID, active.Fingerprint)
		return active.ID, false, nil
	case errors.Is(err, vector.ErrIndexBuilding):
		// Building row vanished between our BuildingGeneration call
		// and ResolveActive's lookup (e.g. concurrent activation).
		// Surface the underlying sentinel so the caller can hint at
		// --full-rebuild.
		return 0, false, fmt.Errorf("resolve active generation: %w (hint: run with --full-rebuild to start)", err)
	default:
		return 0, false, fmt.Errorf("resolve active generation: %w (hint: run with --full-rebuild to start)", err)
	}
}

func pendingCount(ctx context.Context, db *sql.DB, gen vector.GenerationID) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&n); err != nil {
		return 0, fmt.Errorf("query pending: %w", err)
	}
	return n, nil
}

// newProgressPrinter returns an embed.Worker Progress callback that
// emits a rate-limited one-line summary to w. Rate limit is ~2s to
// keep stderr quiet on fast backends (ANE sustains ~500 msg/s at
// batch=100, which would be 5 updates/sec unthrottled). total is the
// pending snapshot at run start; zero disables ETA/percent.
// windowSize controls how many recent batches are used for the
// windowed rate estimate shown in the "(last K)" annotation.
func newProgressPrinter(w io.Writer, total int, windowSize int) func(embed.ProgressReport) {
	return newProgressPrinterWithMinInterval(w, total, windowSize, 2*time.Second)
}

func newProgressPrinterWithMinInterval(w io.Writer, total int, windowSize int, minInterval time.Duration) func(embed.ProgressReport) {
	var lastPrint time.Time
	window := newRateWindow(windowSize)
	return func(p embed.ProgressReport) {
		// Always feed the window — including events the throttle is
		// about to suppress. Otherwise a fast downshift drain (where
		// each singleton report arrives much faster than the 2s
		// throttle) would leak almost all of its samples and the
		// "(last K)" annotation would never reflect drain throughput.
		window.Add(p.BatchMsgs, p.BatchElapsed)

		now := time.Now()
		if now.Sub(lastPrint) < minInterval {
			return
		}
		lastPrint = now

		windowedRate := window.Rate()
		samples := window.Samples()

		msPerMsg := float64(p.BatchElapsed.Milliseconds()) / float64(max1(p.BatchMsgs))
		usPerChar := float64(p.BatchElapsed.Microseconds()) / float64(max1(p.BatchChars))

		if total > 0 && windowedRate > 0 {
			remaining := total - p.Done
			if remaining < 0 {
				remaining = 0
			}
			eta := time.Duration(float64(remaining)/windowedRate) * time.Second
			pct := 100 * float64(p.Done) / float64(total)
			fmt.Fprintf(w,
				"progress: %d/%d (%.1f%%) — %.0f msg/s (last %d), %.1f ms/msg, %.2f µs/char, ETA %s\n",
				p.Done, total, pct, windowedRate, samples, msPerMsg, usPerChar, formatETA(eta))
		} else {
			fmt.Fprintf(w,
				"progress: %d embedded — %.0f msg/s (last %d), %.1f ms/msg, %.2f µs/char\n",
				p.Done, windowedRate, samples, msPerMsg, usPerChar)
		}
	}
}

// max1 floors a denominator at 1 so per-unit averages never divide by
// zero on the rare empty or single-char batch.
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// formatETA renders a duration as h:mm:ss or m:ss, dropping leading
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
