//go:build sqlite_vec || pgvector

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
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func runEmbed(cmd *cobra.Command) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	release, err := acquireDirectSQLiteWriteLock(cfg)
	if err != nil {
		return err
	}
	defer release()

	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db: %w", err)
	}
	defer func() { _ = s.Close() }()

	// Auto-migrate the main schema before any embed_gen access. On an
	// upgraded SQLite DB whose messages table predates the embed_gen
	// column, InitSchema's LegacyColumnMigrations adds it; without this
	// the backfill UPDATE and CoverageCounts below fail with "no such
	// column: embed_gen". serve.go does the same before setupVectorFeatures.
	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}

	var (
		backend   vector.Backend
		vectorsDB *sql.DB
		closeFn   func() error
		rebind    func(string) string
		// lastModifiedExpr is the dialect-correct SELECT expression for the
		// embed worker's last_modified CAS token. SQLite needs CAST(... AS
		// TEXT) to defeat go-sqlite3's DATETIME→time.Time coercion (which
		// would break round-trip equality); PG uses the bare column.
		lastModifiedExpr = "CAST(m.last_modified AS TEXT)"
	)
	if s.IsPostgreSQL() {
		// pgvector embeddings live in the same Postgres database as
		// messages — no separate vectors.db. The queue/worker layer is
		// dialect-aware via rebind, so the build pipeline runs directly
		// against pgx.
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:            s.DB(),
			Dimension:     cfg.Vector.Embeddings.Dimension,
			BuildScope:    cfg.Vector.Embed.Scope.BuildScope(),
			SkipExtension: cfg.Vector.SkipExtensionCreate,
		})
		if err != nil {
			return fmt.Errorf("open pgvector backend: %w", err)
		}
		backend = pgb
		vectorsDB = pgb.DB()
		closeFn = pgb.Close
		rebind = (&store.PostgreSQLDialect{}).Rebind
		lastModifiedExpr = "m.last_modified"
	} else {
		if err := sqlitevec.RegisterExtension(); err != nil {
			return fmt.Errorf("register sqlite-vec: %w", err)
		}
		vecPath := cfg.Vector.DBPath
		if vecPath == "" {
			vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:       vecPath,
			MainPath:   cfg.DatabaseDSN(),
			Dimension:  cfg.Vector.Embeddings.Dimension,
			MainDB:     s.DB(),
			BuildScope: cfg.Vector.Embed.Scope.BuildScope(),
		})
		if err != nil {
			return fmt.Errorf("open vectors.db: %w", err)
		}
		backend = sb
		vectorsDB = sb.DB()
		closeFn = sb.Close
	}
	defer func() { _ = closeFn() }()

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
	// "Pending" is now the count of live messages still needing work for
	// this generation (embed_gen <> gen), read from the main DB coverage
	// rather than a queue table.
	scope := cfg.Vector.Embed.Scope.BuildScope()
	missing, err := s.MissingCountScoped(ctx, int64(gen), scope.MessageTypes)
	if err != nil {
		return fmt.Errorf("coverage counts: %w", err)
	}
	totalPending := int(missing)

	worker := embed.NewWorker(embed.WorkerDeps{
		Backend:   backend,
		VectorsDB: vectorsDB,
		MainDB:    s.DB(),
		Store:     s,
		Client:    client,
		Preprocess: embed.PreprocessConfig{
			StripQuotes:        cfg.Vector.Preprocess.StripQuotesEnabled(),
			StripSignatures:    cfg.Vector.Preprocess.StripSignaturesEnabled(),
			StripHTML:          cfg.Vector.Preprocess.StripHTMLEnabled(),
			StripBase64:        cfg.Vector.Preprocess.StripBase64Enabled(),
			StripURLTracking:   cfg.Vector.Preprocess.StripURLTrackingEnabled(),
			CollapseWhitespace: cfg.Vector.Preprocess.CollapseWhitespaceEnabled(),
		},
		MaxInputChars:    cfg.Vector.Embeddings.MaxInputChars,
		BatchSize:        cfg.Vector.Embeddings.BatchSize,
		BuildScope:       scope,
		Rebind:           rebind,
		LastModifiedExpr: lastModifiedExpr,
		TotalPending:     totalPending,
		Progress:         newProgressPrinter(errOut, totalPending, cfg.Vector.Embeddings.ETAWindow),
	})

	var res embed.RunResult
	if embedBackstop {
		res, err = worker.RunBackstop(ctx, gen)
	} else {
		res, err = worker.RunOnce(ctx, gen)
	}
	if err != nil {
		return fmt.Errorf("embed run: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Scanned: %d, succeeded: %d, failed: %d, truncated: %d\n",
		res.Claimed, res.Succeeded, res.Failed, res.Truncated)

	// Activation is a function of the generation's final coverage, not
	// of the cumulative retry counter — transient failures that the
	// worker later recovers from must not block activation, and an
	// active generation must not be re-activated.
	if rebuildInProgress {
		_, _, _, remaining, err := s.CoverageCountsScoped(ctx, int64(gen), scope.MessageTypes)
		if err != nil {
			return fmt.Errorf("coverage counts: %w", err)
		}
		if remaining == 0 {
			// force=false: we already gated on remaining==0 above, and the
			// backend re-asserts the no-missing gate atomically.
			if err := backend.ActivateGeneration(ctx, gen, false); err != nil {
				return fmt.Errorf("activate generation: %w", err)
			}
			_, _ = fmt.Fprintf(out, "Generation %d activated.\n", gen)
		} else {
			_, _ = fmt.Fprint(errOut, remainingCoverageHint(gen, remaining))
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
			return 0, false, errors.New("aborted")
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
			// Resume the matching build. Under scan-and-fill there is no
			// seed pass to re-run on resume — the worker discovers work by
			// scanning messages.embed_gen, so a crash before any embedding
			// simply leaves the whole corpus needing work. If the build was
			// already activated by a concurrent actor, the worker's scan is
			// still harmless (covered rows are skipped) and the subsequent
			// activation gate will report it is not building.
			_, _ = fmt.Fprintf(opts.Stderr, "Resuming building generation %d (%s).\n",
				building.ID, building.Fingerprint)
			return building.ID, true, nil
		}
		return 0, false, fmt.Errorf("in-progress rebuild has fingerprint=%q, config has %q — activate or retire it before running with a different model",
			building.Fingerprint, opts.Fingerprint)
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
			remaining := max(total-p.Done, 0)
			eta := time.Duration(float64(remaining)/windowedRate) * time.Second
			pct := 100 * float64(p.Done) / float64(total)
			_, _ = fmt.Fprintf(w,
				"progress: %d/%d (%.1f%%) — %.0f msg/s (last %d), %.1f ms/msg, %.2f µs/char, ETA %s\n",
				p.Done, total, pct, windowedRate, samples, msPerMsg, usPerChar, formatETA(eta))
		} else {
			_, _ = fmt.Fprintf(w,
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
