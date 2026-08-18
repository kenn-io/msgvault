//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const contextualDocumentUTF8Limit = 100_000

type embeddingRuntime struct {
	Runner      scheduler.EmbedRunner
	QueryClient hybrid.EmbeddingClient
	Convergence scheduler.ConvergenceChecker
}

type embeddingRuntimeDeps struct {
	Backend          vector.Backend
	VectorsDB        *sql.DB
	MainDB           *sql.DB
	Store            *store.Store
	Rebind           func(string) string
	LastModifiedExpr string
	TotalPending     int
	Progress         func(embed.ProgressReport)
	Log              *slog.Logger
}

type legacyConvergenceChecker struct {
	store *store.Store
	scope vector.BuildScope
}

func (c *legacyConvergenceChecker) CheckConvergence(ctx context.Context, gen vector.GenerationID) (scheduler.ConvergenceResult, error) {
	missing, err := c.store.MissingCountScoped(ctx, int64(gen), c.scope.MessageTypes, c.scope.SourceIDs)
	if err != nil {
		return scheduler.ConvergenceResult{}, fmt.Errorf("message coverage: %w", err)
	}
	return scheduler.ConvergenceResult{
		MessageCoverageComplete: missing == 0,
		MessageCoverageMissing:  missing,
		ReconciliationComplete:  true,
	}, nil
}

type contextualConvergenceChecker struct {
	legacy    *legacyConvergenceChecker
	publisher vector.DocumentPublisher
}

func (c *contextualConvergenceChecker) CheckConvergence(ctx context.Context, gen vector.GenerationID) (scheduler.ConvergenceResult, error) {
	state, err := c.legacy.CheckConvergence(ctx, gen)
	if err != nil {
		return scheduler.ConvergenceResult{}, err
	}
	latest, err := c.legacy.store.LatestEmbeddingChangeSequence(ctx)
	if err != nil {
		return scheduler.ConvergenceResult{}, fmt.Errorf("latest embedding change sequence: %w", err)
	}
	progress, err := c.publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		return scheduler.ConvergenceResult{}, fmt.Errorf("contextual document progress: %w", err)
	}
	state.LatestJournalSequence = latest
	state.ConsumedJournalSequence = progress.ChangeSequence
	state.ReconciliationComplete = contextualReconciliationComplete(progress.ReconcileCursor) && progress.JournalCursor == ""
	return state, nil
}

func contextualReconciliationComplete(cursor string) bool {
	value, ok := strings.CutPrefix(cursor, "done:")
	if !ok {
		return false
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	return err == nil && sequence >= 0
}

func convergenceError(gen vector.GenerationID, state scheduler.ConvergenceResult) error {
	return fmt.Errorf("generation %d has not converged: message_coverage_complete=%t (missing=%d), journal=%d/%d, reconciliation_complete=%t",
		gen, state.MessageCoverageComplete, state.MessageCoverageMissing, state.ConsumedJournalSequence,
		state.LatestJournalSequence, state.ReconciliationComplete)
}

func embeddingPreprocessConfig(cfg vector.Config) embed.PreprocessConfig {
	chunkPolicy := embed.EmbeddingChunkPolicy(cfg.Embeddings.MaxInputChars)
	return embed.PreprocessConfig{
		StripQuotes:        cfg.Preprocess.StripQuotesEnabled(),
		StripSignatures:    cfg.Preprocess.StripSignaturesEnabled(),
		StripHTML:          cfg.Preprocess.StripHTMLEnabled(),
		StripBase64:        cfg.Preprocess.StripBase64Enabled(),
		StripURLTracking:   cfg.Preprocess.StripURLTrackingEnabled(),
		CollapseWhitespace: cfg.Preprocess.CollapseWhitespaceEnabled(),
		MaxBodyRunes:       chunkPolicy.MaxBodyRunes,
	}
}

func newEmbeddingRuntime(vectorCfg vector.Config, deps embeddingRuntimeDeps) (*embeddingRuntime, error) {
	checker, err := newConvergenceChecker(vectorCfg, deps.Store, deps.Backend)
	if err != nil {
		return nil, err
	}
	switch vectorCfg.Embeddings.EffectiveAPIFormat() {
	case vector.APIFormatOpenAI:
		client := embed.NewClient(embed.Config{
			Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
			Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
			Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
		})
		worker := embed.NewWorker(embed.WorkerDeps{
			Backend: deps.Backend, VectorsDB: deps.VectorsDB, MainDB: deps.MainDB,
			Store: deps.Store, Client: client, Preprocess: embeddingPreprocessConfig(vectorCfg),
			MaxInputChars: vectorCfg.Embeddings.MaxInputChars,
			BatchSize:     vectorCfg.Embeddings.BatchSize, BuildScope: vectorCfg.Embed.Scope.BuildScope(),
			Rebind: deps.Rebind, LastModifiedExpr: deps.LastModifiedExpr,
			TotalPending: deps.TotalPending, Progress: deps.Progress, Log: deps.Log,
		})
		return &embeddingRuntime{Runner: worker, QueryClient: client, Convergence: checker}, nil
	case vector.APIFormatVoyageContextual:
		if vectorCfg.Embeddings.Model != "voyage-context-4" {
			return nil, fmt.Errorf("vector.embeddings.model: api_format=%q requires %q, got %q",
				vector.APIFormatVoyageContextual, "voyage-context-4", vectorCfg.Embeddings.Model)
		}
		publisher, ok := deps.Backend.(vector.DocumentPublisher)
		if !ok {
			return nil, errors.New("voyage contextual embeddings require a document publisher backend")
		}
		client := embed.NewVoyageClient(embed.VoyageConfig{
			Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
			Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
			Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
			Limits: embed.RequestLimits{MaxDocuments: vectorCfg.Embeddings.BatchSize,
				MaxChunks: 16_000, MaxUTF8Bytes: contextualDocumentUTF8Limit},
		})
		policy := embed.AssemblyPolicy{
			MaxChunkRunes:        vectorCfg.Embeddings.MaxInputChars,
			MaxDocumentUTF8Bytes: contextualDocumentUTF8Limit,
			Preprocess:           embeddingPreprocessConfig(vectorCfg),
		}
		assembler := embed.CompositeAssembler{Policy: policy, Chat: embed.ChatWindowAssembler{Policy: policy}}
		worker := embed.NewContextWorker(embed.ContextWorkerDeps{
			Backend: deps.Backend, Publisher: publisher, Store: deps.Store,
			Assembler: assembler, Client: client, BuildScope: vectorCfg.Embed.Scope.BuildScope(),
			ChangeBatchSize:    vectorCfg.Embeddings.BatchSize,
			ReconcileBatchSize: vectorCfg.Embeddings.BatchSize,
		})
		return &embeddingRuntime{Runner: worker, QueryClient: client, Convergence: checker}, nil
	default:
		return nil, fmt.Errorf("unsupported embedding api format %q", vectorCfg.Embeddings.APIFormat)
	}
}

func newConvergenceChecker(vectorCfg vector.Config, mainStore *store.Store, backend vector.Backend) (scheduler.ConvergenceChecker, error) {
	legacy := &legacyConvergenceChecker{store: mainStore, scope: vectorCfg.Embed.Scope.BuildScope()}
	if vectorCfg.Embeddings.EffectiveAPIFormat() != vector.APIFormatVoyageContextual {
		return legacy, nil
	}
	publisher, ok := backend.(vector.DocumentPublisher)
	if !ok {
		return nil, errors.New("voyage contextual embeddings require a document publisher backend")
	}
	return &contextualConvergenceChecker{legacy: legacy, publisher: publisher}, nil
}

// precheckVectorFeatures validates vector configuration cheaply so runServe
// can fail fast on misconfiguration while deferring the expensive backend
// open/migrate/backfill to the background init task. Returns nil when
// vector search is disabled. mainPath drives a dialect-aware build-tag
// check that fails fast on the "binary built without backend support" case
// the cheap precheck can catch synchronously: a postgres:// DSN needs the
// pgvector tag, a SQLite path needs the sqlite_vec tag. Without this,
// setupVectorFeatures would only discover the gap later inside the
// background init goroutine.
func precheckVectorFeatures(mainPath string) error {
	if !cfg.Vector.AnyLaneEnabled() {
		return nil
	}
	if store.IsPostgresURL(mainPath) && !pgvector.Available() {
		return errors.New("vector search is enabled in config but this binary was built without vector support; " +
			"to use vector search on PostgreSQL, rebuild with `go build -tags \"fts5 sqlite_vec pgvector\"` " +
			"or set [vector] enabled = false")
	}
	if !store.IsPostgresURL(mainPath) && !sqlitevec.Available() {
		return errors.New("vector search is enabled in config but this binary was built without sqlite-vec support; " +
			"to use vector search on SQLite, rebuild with `go build -tags \"fts5 sqlite_vec\"` or `make build`, " +
			"or set [vector] enabled = false")
	}
	if err := cfg.Vector.Validate(); err != nil {
		return fmt.Errorf("vector config: %w", err)
	}
	if cronExpr := cfg.Vector.Embed.Schedule.Cron; cfg.Vector.Enabled && cronExpr != "" {
		if err := scheduler.ValidateCronExpr(cronExpr); err != nil {
			return fmt.Errorf("invalid embed cron expression %q: %w", cronExpr, err)
		}
	}
	if cronExpr := cfg.Vector.Multimodal.Schedule.Cron; cfg.Vector.Multimodal.Enabled && cronExpr != "" {
		if err := scheduler.ValidateCronExpr(cronExpr); err != nil {
			return fmt.Errorf("invalid multimodal cron expression %q: %w", cronExpr, err)
		}
	}
	return nil
}

// setupVectorFeatures builds the vector backend, hybrid engine, and embed
// worker used by the serve daemon and the MCP command. The backend is
// dialect-selected from mainPath: a postgres:// DSN uses the pgvector
// backend sharing mainStore's DB (no separate vectors.db, no ATTACH);
// otherwise the sqlitevec backend opens/attaches vectors.db. Returns
// (nil, nil) when cfg.Vector.Enabled is false. The returned Close function
// must be called on shutdown.
//
// mainStore is the already-opened main-database store. On SQLite, mainPath
// is the msgvault.db filesystem path FusedSearch uses to ATTACH
// vectors.db; on PostgreSQL it is the DSN, used only for dialect detection
// (store.IsPostgresURL).
//
// readOnly marks mainDB as a read-only connection — e.g. the MCP server's
// store.OpenReadOnly. On PostgreSQL it sets BOTH pgvector.Options.SkipMigrate
// and pgvector.Options.ReadOnly: SkipMigrate suppresses the privileged
// CREATE EXTENSION + full migrate, and ReadOnly suppresses ALL remaining
// writes — the extension-less schema apply, the orphan reset, and the
// embed_gen backfill — because PG vector tables share the (read-only) main
// connection and any DDL/UPDATE would be rejected with SQLSTATE 25006. On
// SQLite it sets sqlitevec.Options.ReadOnly so only the one-time embed_gen
// upgrade backfill — which WRITES messages.embed_gen + applied_migrations
// through the main handle — is skipped (the query-only handle would reject
// those writes); Migrate still runs there because it only touches the
// separate vectors.db, which is read-write regardless.
func setupVectorFeatures(ctx context.Context, mainStore *store.Store, mainPath string, readOnly bool, openers ...visual.StreamOpener) (*vectorFeatures, error) {
	if !cfg.Vector.AnyLaneEnabled() {
		return nil, nil //nolint:nilnil // vector disabled: callers nil-check vf; (nil, nil) means "no features, no error"
	}
	if err := cfg.Vector.Validate(); err != nil {
		return nil, fmt.Errorf("vector config: %w", err)
	}
	// Resolve [vector.embed.scope] accounts to source IDs before any
	// consumer derives a build scope or generation fingerprint from the
	// config (backend coverage gates, the embed worker/job, the hybrid
	// engine's expected fingerprint). Unknown accounts fail vector init
	// loudly rather than silently embedding the full corpus. The resolved
	// config is a local copy: this runs on the daemon's background init
	// goroutine while HTTP handlers may already be reading the global cfg,
	// so the global must stay unmutated.
	vecCfg, err := resolvedVectorConfig(mainStore)
	if err != nil {
		return nil, fmt.Errorf("vector embed scope: %w", err)
	}
	mainDB := mainStore.DB()

	// Resolve the dialect once from the main DSN. The worker is
	// dialect-portable via Rebind, so the serve daemon and MCP run vector
	// features on PostgreSQL the same way `msgvault embed` does. SQLite's
	// Rebind is identity so the SQLite path is unchanged.
	var dialect store.Dialect = &store.SQLiteDialect{}
	// lastModifiedExpr is the dialect-correct SELECT expression for the embed
	// worker's last_modified CAS token. SQLite needs CAST(... AS TEXT) to
	// defeat go-sqlite3's DATETIME→time.Time coercion (which would break
	// round-trip equality); PG uses the bare column.
	lastModifiedExpr := "CAST(m.last_modified AS TEXT)"
	if store.IsPostgresURL(mainPath) {
		dialect = &store.PostgreSQLDialect{}
		lastModifiedExpr = "m.last_modified"
	}

	var (
		backend   vector.Backend
		vectorsDB *sql.DB
		closeFn   func() error
	)
	if store.IsPostgresURL(mainPath) {
		// Same database handle as the main store: pgvector embeddings
		// live alongside messages, so there is no separate vectors.db.
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:          mainDB,
			Dimension:   vecCfg.Embeddings.Dimension,
			BuildScope:  vecCfg.Embed.Scope.BuildScope(),
			SkipMigrate: readOnly,
			// ReadOnly MUST track readOnly here: this is the MCP read-only
			// path (store.OpenReadOnly). When set, Open performs no writes —
			// no schema apply, no orphan reset, no upgrade backfill — so the
			// query-only connection never attempts DDL/UPDATE (SQLSTATE 25006).
			ReadOnly: readOnly,
			// On a managed/locked-down PG the `vector` extension is
			// pre-installed by an admin and CREATE EXTENSION would fail
			// for the msgvault role; SkipExtensionCreate lets schema +
			// index DDL still run. Ignored when SkipMigrate (readOnly).
			SkipExtension: vecCfg.SkipExtensionCreate,
		})
		if err != nil {
			return nil, fmt.Errorf("open pgvector backend: %w", err)
		}
		backend = pgb
		vectorsDB = pgb.DB()
		closeFn = pgb.Close
	} else {
		if err := sqlitevec.RegisterExtension(); err != nil {
			return nil, fmt.Errorf("register sqlite-vec: %w", err)
		}
		vecPath := vecCfg.DBPath
		if vecPath == "" {
			vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:       vecPath,
			MainPath:   mainPath,
			Dimension:  vecCfg.Embeddings.Dimension,
			MainDB:     mainDB,
			BuildScope: vecCfg.Embed.Scope.BuildScope(),
			// Honor the read-only signal on SQLite too: when mainDB is a
			// query-only handle (MCP), skip the embed_gen upgrade backfill,
			// which would write through it. Migrate still runs (vectors.db
			// is read-write).
			ReadOnly: readOnly,
		})
		if err != nil {
			return nil, fmt.Errorf("open vectors.db: %w", err)
		}
		backend = sb
		vectorsDB = sb.DB()
		closeFn = sb.Close
	}

	features := &vectorFeatures{Backend: backend, Cfg: vecCfg, Close: closeFn}
	if vecCfg.Enabled {
		runtime, err := newEmbeddingRuntime(vecCfg, embeddingRuntimeDeps{
			Backend: backend, VectorsDB: vectorsDB, MainDB: mainDB, Store: mainStore,
			Rebind: dialect.Rebind, LastModifiedExpr: lastModifiedExpr, Log: logger,
		})
		if err != nil {
			_ = closeFn()
			return nil, fmt.Errorf("configure embedding runtime: %w", err)
		}
		features.Runner = runtime.Runner
		features.Convergence = runtime.Convergence
		features.HybridEngine = hybrid.NewEngine(backend, mainDB, runtime.QueryClient, hybrid.Config{
			ExpectedFingerprint: vecCfg.GenerationFingerprint(),
			RRFK:                vecCfg.Search.RRFK,
			KPerSignal:          vecCfg.Search.KPerSignal,
			SubjectBoost:        vecCfg.Search.SubjectBoost,
			// BuildFilter's participant/label lookups run against mainDB with ?
			// placeholders. On PG those must become $N or pgx rejects them, so
			// the serve/MCP hybrid engine (shared via vectorFeatures.HybridEngine)
			// carries the dialect's Rebind. SQLite's Rebind is identity.
			Rebind:     dialect.Rebind,
			BuildScope: vecCfg.Embed.Scope.BuildScope(),
		})
	}
	if vecCfg.Multimodal.Enabled && !readOnly {
		if len(openers) == 0 || openers[0] == nil {
			_ = closeFn()
			return nil, errors.New("configure multimodal runtime: attachment content store is unavailable")
		}
		visualRuntime, err := newVisualRuntime(ctx, vecCfg, mainStore, backend, openers[0])
		if err != nil {
			_ = closeFn()
			return nil, fmt.Errorf("configure multimodal runtime: %w", err)
		}
		features.Visual = visualRuntime
	}
	return features, nil
}

func newVisualRuntime(ctx context.Context, vecCfg vector.Config, mainStore *store.Store, backend vector.Backend, opener visual.StreamOpener) (*visualFeatures, error) {
	fingerprint := vecCfg.MultimodalGenerationFingerprint()
	var visualBackend visual.Backend
	switch typed := backend.(type) {
	case *sqlitevec.Backend:
		visualBackend = typed.Visual()
	case *pgvector.Backend:
		visualBackend = typed.Visual()
	default:
		return nil, errors.New("selected vector backend has no visual lane")
	}
	if building, err := mainStore.BuildingVisualGeneration(ctx); err == nil && building.Fingerprint != fingerprint {
		tokens, tokenErr := mainStore.ListVisualGenerationTokens(ctx, building.ID)
		if tokenErr != nil {
			return nil, tokenErr
		}
		vectorTokens := make([]visual.VectorToken, len(tokens))
		for i, token := range tokens {
			vectorTokens[i] = visual.VectorToken(token)
		}
		if err := visualBackend.DeleteTokens(ctx, vectorTokens); err != nil {
			return nil, err
		}
		if err := mainStore.RetireVisualGeneration(ctx, building.ID); err != nil {
			return nil, err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	generation, err := mainStore.EnsureVisualGeneration(ctx, store.VisualGenerationSpec{
		Fingerprint: fingerprint, Model: vecCfg.Multimodal.Model,
		Dimension: vecCfg.Multimodal.Dimension,
	})
	if err != nil {
		return nil, err
	}
	provider, err := visual.NewVoyageClient(visual.VoyageConfig{
		Endpoint: vecCfg.Multimodal.Endpoint, APIKey: vecCfg.Multimodal.APIKey(),
		Model: vecCfg.Multimodal.Model, Dimension: vecCfg.Multimodal.Dimension,
	})
	if err != nil {
		return nil, err
	}
	consumerKey := "visual/" + fingerprint
	reconciler, err := visual.NewReconciler(mainStore, opener, visual.ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: consumerKey,
		MessageTypes: vecCfg.Multimodal.Scope.BuildScope().MessageTypes,
		// Two maximum-size media items remain below the provider's 64 MiB
		// encoded-request ceiling. This also bounds each scheduled pass by two
		// paid owners and roughly 40 MiB of decoded media.
		PageSize: 2, LeaseOwner: consumerKey, LeaseDuration: 2 * time.Minute,
		MediaPolicy: visual.MediaPolicy{MaxBytes: 20 << 20, MaxPixels: 16_000_000,
			IncludeImages: vecCfg.Multimodal.ImagesEnabled(), IncludeVideo: vecCfg.Multimodal.VideoEnabled(),
			AllowAnimatedGIF: vecCfg.Multimodal.AnimatedGIFsEnabled()},
		ContextPolicy: visual.ContextPolicy{MaxChars: vecCfg.Multimodal.MaxContextChars,
			InputVersion: fingerprint, EligibilityVersion: fingerprint},
	})
	if err != nil {
		return nil, err
	}
	worker, err := visual.NewWorker(mainStore, provider, visualBackend, visual.WorkerConfig{
		Dimension: vecCfg.Multimodal.Dimension, ProviderTimeout: 45 * time.Second,
		LeaseDuration: 2 * time.Minute, MaxBatchItems: 2,
	})
	if err != nil {
		return nil, err
	}
	return &visualFeatures{Archive: mainStore, Backend: visualBackend, Provider: provider, Reconciler: reconciler, Worker: worker, Generation: generation}, nil
}
