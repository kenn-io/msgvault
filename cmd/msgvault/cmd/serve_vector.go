//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document/voyage"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const contextualDocumentUTF8Limit = 100_000

type embeddingRuntime struct {
	Runner            scheduler.EmbedRunner
	QueryClient       hybrid.EmbeddingClient
	PersonQueryClient personsearch.QueryEmbedder
	Convergence       scheduler.ConvergenceChecker
	PersonGate        vector.SemanticPersonEmbeddingGate
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
	PersonGate       vector.SemanticPersonEmbeddingGate
}

type legacyConvergenceChecker struct {
	store         *store.Store
	scope         vector.BuildScope
	personBackend vector.PersonBackend
	personGate    vector.SemanticPersonEmbeddingGate
}

func (c *legacyConvergenceChecker) CheckConvergence(ctx context.Context, gen vector.GenerationID) (scheduler.ConvergenceResult, error) {
	missing, err := c.store.MissingCountScoped(ctx, int64(gen), c.scope.MessageTypes, c.scope.SourceIDs)
	if err != nil {
		return scheduler.ConvergenceResult{}, fmt.Errorf("message coverage: %w", err)
	}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: missing == 0,
		MessageCoverageMissing:  missing,
		ReconciliationComplete:  true,
		PersonCoverageComplete:  true,
	}
	coverage, err := c.CheckPersonCoverage(ctx, gen)
	if err != nil {
		return scheduler.ConvergenceResult{}, err
	}
	state.PersonCoverageMismatched = coverage.Mismatched
	state.PersonCoverageRejected = coverage.Rejected
	state.PersonCoverageComplete = coverage.Complete()
	return state, nil
}

func (c *legacyConvergenceChecker) CheckPersonCoverage(
	ctx context.Context, gen vector.GenerationID,
) (personsearch.Coverage, error) {
	if c.personGate == nil {
		return personsearch.Coverage{}, errors.New("semantic person embedding convergence gate is not configured")
	}
	if err := c.personGate.Check(ctx); err != nil {
		if vector.SemanticPersonEmbeddingAuthorizationUnavailable(err) {
			return personsearch.Coverage{}, nil
		}
		return personsearch.Coverage{}, fmt.Errorf("semantic person embedding convergence gate: %w", err)
	}
	if c.personBackend == nil {
		return personsearch.Coverage{}, nil
	}
	documents, err := c.store.ListPersonSemanticDocumentsContext(ctx)
	if err != nil {
		return personsearch.Coverage{}, fmt.Errorf("person semantic documents: %w", err)
	}
	revisions, err := c.personBackend.ListPersonRevisions(ctx, gen)
	if err != nil {
		return personsearch.Coverage{}, fmt.Errorf("person vector revisions: %w", err)
	}
	rejected, err := c.personBackend.CountRejectedPersons(ctx, gen)
	if err != nil {
		return personsearch.Coverage{}, fmt.Errorf("rejected person vectors: %w", err)
	}
	coverage := personsearch.Coverage{Rejected: rejected}
	current := make(map[int64]string, len(documents))
	for _, document := range documents {
		if document.Text == "" {
			continue
		}
		current[document.PersonID] = document.Revision
		if revisions[document.PersonID] != document.Revision {
			coverage.Mismatched++
		}
	}
	for personID := range revisions {
		if _, ok := current[personID]; !ok {
			coverage.Mismatched++
		}
	}
	return coverage, nil
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

func (c *contextualConvergenceChecker) CheckPersonCoverage(
	ctx context.Context, gen vector.GenerationID,
) (personsearch.Coverage, error) {
	return c.legacy.CheckPersonCoverage(ctx, gen)
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
	message := convergenceStateMessage(gen, state)
	if !state.MessageCoverageComplete {
		message += "; " + strings.TrimSpace(remainingCoverageHint(gen, state.MessageCoverageMissing))
	}
	if guidance := personCoverageGuidance(gen, state); guidance != "" {
		message += "; " + guidance
	}
	return errors.New(message)
}

func manualConvergenceError(gen vector.GenerationID, state scheduler.ConvergenceResult) error {
	message := convergenceStateMessage(gen, state)
	if !state.MessageCoverageComplete {
		message += fmt.Sprintf("; generation still has %d message(s) needing embedding; run `msgvault embeddings resume --backstop` to process them",
			state.MessageCoverageMissing)
	}
	if guidance := personCoverageGuidance(gen, state); guidance != "" {
		message += "; " + guidance
	}
	if state.PersonCoverageRejected == 0 {
		message += "; or pass `--force` to activate anyway"
	}
	return errors.New(message)
}

func personCoverageGuidance(gen vector.GenerationID, state scheduler.ConvergenceResult) string {
	guidance := make([]string, 0, 2)
	if state.PersonCoverageMismatched > 0 ||
		(!state.PersonCoverageComplete && state.PersonCoverageRejected == 0) {
		guidance = append(guidance,
			"run `msgvault embeddings resume --backstop` to retry curated person profiles")
	}
	if state.PersonCoverageRejected > 0 {
		guidance = append(guidance, fmt.Sprintf(
			"%d terminal curated person profile(s) will not be retried at the same source revision; source revision must change before retrying, or run `msgvault embeddings activate %d --force` to activate anyway",
			state.PersonCoverageRejected, gen))
	}
	return strings.Join(guidance, "; ")
}

func convergenceStateMessage(gen vector.GenerationID, state scheduler.ConvergenceResult) string {
	return fmt.Sprintf("generation %d has not converged: message_coverage_complete=%t (missing=%d), person_coverage_complete=%t (mismatched=%d, rejected=%d), journal=%d/%d, reconciliation_complete=%t",
		gen, state.MessageCoverageComplete, state.MessageCoverageMissing, state.PersonCoverageComplete,
		state.PersonCoverageMismatched, state.PersonCoverageRejected, state.ConsumedJournalSequence,
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
	personBackend, ok := deps.Backend.(vector.PersonBackend)
	if !ok {
		return nil, errors.New("person embeddings require a person vector backend")
	}
	personGate := deps.PersonGate
	if personGate == nil {
		return nil, errors.New("semantic person embedding runtime gate is required")
	}
	checker, err := newConvergenceChecker(vectorCfg, deps.Store, deps.Backend, personGate)
	if err != nil {
		return nil, err
	}
	switch vectorCfg.Embeddings.EffectiveAPIFormat() {
	case vector.APIFormatOpenAI:
		messageClient := newOpenAIEmbedClient(vectorCfg)
		personClient := embed.NewClient(embed.Config{
			Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
			Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
			Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
			BeforeRequest: personGate.Check,
		})
		messageWorker := embed.NewWorker(embed.WorkerDeps{
			Backend: deps.Backend, VectorsDB: deps.VectorsDB, MainDB: deps.MainDB,
			Store: deps.Store, Client: messageClient, Preprocess: embeddingPreprocessConfig(vectorCfg),
			MaxInputChars: vectorCfg.Embeddings.MaxInputChars,
			BatchSize:     vectorCfg.Embeddings.BatchSize, BuildScope: vectorCfg.Embed.Scope.BuildScope(),
			Rebind: deps.Rebind, LastModifiedExpr: deps.LastModifiedExpr,
			TotalPending: deps.TotalPending, Progress: deps.Progress, Log: deps.Log,
		})
		personWorker := embed.NewPersonWorker(embed.PersonWorkerDeps{
			Store: deps.Store, Backend: personBackend, Client: personClient,
			Gate:      personGate,
			BatchSize: vectorCfg.Embeddings.BatchSize, MaxInputChars: vectorCfg.Embeddings.MaxInputChars,
		})
		worker := embed.NewGenerationWorker(messageWorker, personWorker)
		return &embeddingRuntime{
			Runner: worker, QueryClient: messageClient, PersonQueryClient: personClient,
			Convergence: checker, PersonGate: personGate,
		}, nil
	case vector.APIFormatVoyageContextual:
		messageClient, err := newVoyageContextualEmbedClient(vectorCfg)
		if err != nil {
			return nil, err
		}
		publisher, ok := deps.Backend.(vector.DocumentPublisher)
		if !ok {
			return nil, errors.New("voyage contextual embeddings require a document publisher backend")
		}
		personClient := embed.NewVoyageClient(embed.VoyageConfig{
			Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
			Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
			Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
			Limits: embed.RequestLimits{MaxDocuments: vectorCfg.Embeddings.BatchSize,
				MaxChunks: 16_000, MaxUTF8Bytes: contextualDocumentUTF8Limit},
			BeforeRequest: personGate.Check,
		})
		policy := embed.AssemblyPolicy{
			MaxChunkRunes:        vectorCfg.Embeddings.MaxInputChars,
			MaxDocumentUTF8Bytes: contextualDocumentUTF8Limit,
			Preprocess:           embeddingPreprocessConfig(vectorCfg),
		}
		assembler := embed.CompositeAssembler{Policy: policy, Chat: embed.ChatWindowAssembler{Policy: policy}}
		messageWorker := embed.NewContextWorker(embed.ContextWorkerDeps{
			Backend: deps.Backend, Publisher: publisher, Store: deps.Store,
			Assembler: assembler, Client: messageClient, BuildScope: vectorCfg.Embed.Scope.BuildScope(),
			ChangeBatchSize:    vectorCfg.Embeddings.BatchSize,
			ReconcileBatchSize: vectorCfg.Embeddings.BatchSize,
		})
		personWorker := embed.NewPersonWorker(embed.PersonWorkerDeps{
			Store: deps.Store, Backend: personBackend, Client: personClient,
			Gate:      personGate,
			BatchSize: vectorCfg.Embeddings.BatchSize, MaxInputChars: vectorCfg.Embeddings.MaxInputChars,
		})
		worker := embed.NewGenerationWorker(messageWorker, personWorker)
		return &embeddingRuntime{
			Runner: worker, QueryClient: messageClient, PersonQueryClient: personClient,
			Convergence: checker, PersonGate: personGate,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported embedding api format %q", vectorCfg.Embeddings.APIFormat)
	}
}

// newOpenAIEmbedClient builds the OpenAI-compatible embeddings client. Both
// the embed worker and the query path use it, so they always agree on
// endpoint, model, dimension and retry policy.
func newOpenAIEmbedClient(vectorCfg vector.Config) *embed.Client {
	return embed.NewClient(embed.Config{
		Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
		Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
		Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
	})
}

// newVoyageContextualEmbedClient builds the Voyage contextualized embeddings
// client, rejecting a model the contextual endpoint does not serve. The client
// is shared by the document side (ContextWorker, input_type=document) and the
// query side (EmbedQuery, input_type=query); the role is chosen per call, not
// per client, so query-time callers get the query role for free.
func newVoyageContextualEmbedClient(vectorCfg vector.Config) (*embed.VoyageClient, error) {
	if vectorCfg.Embeddings.Model != "voyage-context-4" {
		return nil, fmt.Errorf("vector.embeddings.model: api_format=%q requires %q, got %q",
			vector.APIFormatVoyageContextual, "voyage-context-4", vectorCfg.Embeddings.Model)
	}
	return embed.NewVoyageClient(embed.VoyageConfig{
		Endpoint: vectorCfg.Embeddings.Endpoint, APIKey: vectorCfg.Embeddings.APIKey(),
		Model: vectorCfg.Embeddings.Model, Dimension: vectorCfg.Embeddings.Dimension,
		Timeout: vectorCfg.Embeddings.Timeout, MaxRetries: vectorCfg.Embeddings.MaxRetries,
		Limits: embed.RequestLimits{MaxDocuments: vectorCfg.Embeddings.BatchSize,
			MaxChunks: 16_000, MaxUTF8Bytes: contextualDocumentUTF8Limit},
	}), nil
}

// newQueryEmbeddingClient selects the query-time embedding client for the
// configured vector.embeddings.api_format, using the same constructors
// newEmbeddingRuntime uses for the indexing side.
//
// Query-only callers (search, eval) need this rather than newEmbeddingRuntime:
// they never embed a document, so they must not require the document
// publisher backend or build an embed worker. Constructing embed.NewClient
// unconditionally here would send an OpenAI-compatible request body to
// Voyage's /contextualizedembeddings endpoint under a config that indexed with
// the contextual one — the wrong endpoint, the wrong request shape, and no
// input_type=query role.
func newQueryEmbeddingClient(vectorCfg vector.Config) (hybrid.EmbeddingClient, error) {
	switch vectorCfg.Embeddings.EffectiveAPIFormat() {
	case vector.APIFormatOpenAI:
		return newOpenAIEmbedClient(vectorCfg), nil
	case vector.APIFormatVoyageContextual:
		client, err := newVoyageContextualEmbedClient(vectorCfg)
		if err != nil {
			return nil, err
		}
		return client, nil
	default:
		return nil, fmt.Errorf("unsupported embedding api format %q", vectorCfg.Embeddings.APIFormat)
	}
}

func newConvergenceChecker(
	vectorCfg vector.Config,
	mainStore *store.Store,
	backend vector.Backend,
	personGate vector.SemanticPersonEmbeddingGate,
) (scheduler.ConvergenceChecker, error) {
	personBackend, ok := backend.(vector.PersonBackend)
	if !ok {
		return nil, errors.New("person embeddings require a person vector backend")
	}
	if personGate == nil {
		return nil, errors.New("semantic person embedding convergence gate is required")
	}
	legacy := &legacyConvergenceChecker{
		store: mainStore, scope: vectorCfg.Embed.Scope.BuildScope(),
		personBackend: personBackend, personGate: personGate,
	}
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
		personGate := vector.NewPinnedExactSemanticPersonEmbeddingGate(
			vecCfg, currentSemanticPersonVectorConfigSource(), mainStore,
		)
		runtime, err := newEmbeddingRuntime(vecCfg, embeddingRuntimeDeps{
			Backend: backend, VectorsDB: vectorsDB, MainDB: mainDB, Store: mainStore,
			Rebind: dialect.Rebind, LastModifiedExpr: lastModifiedExpr, Log: logger,
			PersonGate: personGate,
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
		personSearchBackend, ok := backend.(personsearch.Backend)
		if !ok {
			_ = closeFn()
			return nil, errors.New("person search requires a person vector backend")
		}
		personCoverage, ok := runtime.Convergence.(personsearch.CoverageChecker)
		if !ok {
			_ = closeFn()
			return nil, errors.New("person search requires a person coverage checker")
		}
		features.PersonSearchEngine = personsearch.NewEngine(
			personSearchBackend, mainStore, runtime.PersonQueryClient,
			personsearch.Config{
				ExpectedFingerprint: vecCfg.GenerationFingerprint(),
				PersonCoverage:      personCoverage.CheckPersonCoverage,
				Gate:                runtime.PersonGate,
			},
		)
	}
	if vecCfg.Multimodal.Enabled && !readOnly {
		if len(openers) == 0 || openers[0] == nil {
			_ = closeFn()
			return nil, errors.New("configure multimodal runtime: attachment content store is unavailable")
		}
		visualRuntime, err := newVisualRuntime(ctx, vecCfg, mainStore, backend, openers[0])
		switch {
		case err != nil && !vecCfg.Enabled:
			// Multimodal is the only configured lane: swallowing its
			// initialization failure would leave vector status disabled with
			// nothing to report. There is no text search to preserve.
			_ = closeFn()
			return nil, fmt.Errorf("configure multimodal runtime: %w", err)
		case err != nil:
			// The visual lane fails closed without probed authority, but its
			// misconfiguration must not take message vector search down.
			logger.Error("multimodal lane unavailable", "error", err)
		default:
			features.Visual = visualRuntime
		}
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
		// The abandoned generation is never revisited; a consumer left
		// registered under its fingerprint would pin journal pruning forever.
		if err := mainStore.UnregisterAttachmentChangeConsumer(ctx, "visual/"+building.Fingerprint); err != nil {
			return nil, err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// A retired generation for this fingerprint may still reference backend
	// vectors if its retirement cleanup crashed or failed; Ensure refuses to
	// restart over live references, so delete those vectors and purge the
	// ledger first.
	retired, err := mainStore.ListRetiredVisualGenerations(ctx)
	if err != nil {
		return nil, err
	}
	for _, retiredGeneration := range retired {
		if retiredGeneration.Fingerprint != fingerprint {
			continue
		}
		tokens, err := mainStore.ListVisualGenerationTokens(ctx, retiredGeneration.ID)
		if err != nil {
			return nil, err
		}
		if len(tokens) == 0 {
			continue
		}
		vectorTokens := make([]visual.VectorToken, len(tokens))
		for index, token := range tokens {
			vectorTokens[index] = visual.VectorToken(token)
		}
		if err := visualBackend.DeleteTokens(ctx, vectorTokens); err != nil {
			return nil, err
		}
		if err := mainStore.PurgeRetiredVisualGeneration(ctx, retiredGeneration.ID); err != nil {
			return nil, err
		}
	}
	generation, err := mainStore.EnsureVisualGeneration(ctx, store.VisualGenerationSpec{
		Fingerprint: fingerprint, Model: vecCfg.Multimodal.Model,
		Dimension: vecCfg.Multimodal.Dimension,
	})
	if err != nil {
		return nil, err
	}
	manifest, err := loadVisualCapabilityManifest(vecCfg.Multimodal.CapabilitiesFile)
	if err != nil {
		return nil, err
	}
	mediaPolicy := visual.MediaPolicy{MaxBytes: 20 << 20, MaxPixels: 16_000_000,
		IncludeImages: vecCfg.Multimodal.ImagesEnabled(), IncludeVideo: vecCfg.Multimodal.VideoEnabled(),
		AllowAnimatedGIF: vecCfg.Multimodal.AnimatedGIFsEnabled()}
	// The provider validates EVERYTHING that leaves the machine, including
	// consented still-image QUERIES: with include_images=false and
	// allow_image_queries=true the document policy alone would reject query
	// input the search layer permits. Document eligibility (the reconciler's
	// mediaPolicy) stays unchanged. Configs with images already enabled are
	// identical, so no existing consent fingerprint moves.
	providerMedia := mediaPolicy
	if vecCfg.Multimodal.ImageQueriesEnabled() {
		providerMedia.IncludeImages = true
	}
	provider, err := visual.NewVoyageProvider(visual.VoyageConfig{
		APIKey: vecCfg.Multimodal.APIKey(), Model: vecCfg.Multimodal.Model,
		Dimension: vecCfg.Multimodal.Dimension, Manifest: manifest, Media: providerMedia,
	})
	if err != nil {
		return nil, err
	}
	// A format is only eligible when the probe authorized the exact request
	// shapes this archive sends: the document capability always, and its
	// interleaved twin because owning-message context accompanies media
	// whenever the message has any.
	// Every visual search embeds its text query through the same client;
	// without probed text-query authority the lane would index (and bill)
	// while rejecting every search. Fail initialization with the remedy.
	if !slices.Contains(provider.AuthorizedCapabilities(), voyage.CapabilityQueryText) {
		return nil, errors.New("the capability manifest does not authorize text queries; re-run `msgvault multimodal probe` and configure the new manifest")
	}
	mediaPolicy.AuthorizedCapabilities = eligibleVisualCapabilities(
		provider.AuthorizedCapabilities(), vecCfg.Multimodal.MaxContextChars > 0)
	consumerKey := "visual/" + fingerprint
	if provider.PolicyFingerprint() != "" {
		// A changed capability manifest re-opens reconciliation so every
		// candidate is re-evaluated under the new upload authority.
		if _, err := mainStore.SyncVisualGenerationCapabilityFingerprint(
			ctx, generation.ID, consumerKey, provider.PolicyFingerprint()); err != nil {
			return nil, err
		}
	}
	buildScope := vecCfg.Multimodal.Scope.BuildScope()
	scopeCheck := visualScopeCheck(mainStore, vecCfg.Multimodal.Scope.Accounts, buildScope.SourceIDs)
	reconciler, err := visual.NewReconciler(mainStore, opener, visual.ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: consumerKey,
		MessageTypes: buildScope.MessageTypes, SourceIDs: buildScope.SourceIDs,
		// Two maximum-size media items remain below the provider's 64 MiB
		// encoded-request ceiling. The reconciler enforces this as a per-pass
		// OWNER cap (not just a message page size), so each scheduled pass is
		// bounded by two paid owners and roughly 40 MiB of decoded media even
		// when one message carries many standalone attachments.
		PageSize: 2, LeaseOwner: consumerKey, LeaseDuration: 2 * time.Minute,
		MediaPolicy: mediaPolicy,
		ContextPolicy: visual.ContextPolicy{MaxChars: vecCfg.Multimodal.MaxContextChars,
			InputVersion: fingerprint, EligibilityVersion: fingerprint},
	})
	if err != nil {
		return nil, err
	}
	// Batches beyond one item need the probed batch capability.
	maxBatch := 1
	if slices.Contains(provider.AuthorizedCapabilities(), voyage.CapabilityBatchLimits) {
		maxBatch = 2
	}
	worker, err := visual.NewWorker(mainStore, provider, visualBackend, visual.WorkerConfig{
		Dimension: vecCfg.Multimodal.Dimension, ProviderTimeout: 45 * time.Second,
		LeaseDuration: 2 * time.Minute, MaxBatchItems: maxBatch,
	})
	if err != nil {
		return nil, err
	}
	return &visualFeatures{Archive: mainStore, Backend: visualBackend, Provider: provider, Reconciler: reconciler, Worker: worker, Generation: generation, PolicyFingerprint: provider.PolicyFingerprint(), ScopeCheck: scopeCheck}, nil
}

// visualScopeCheck re-resolves the configured multimodal account scope and
// refuses to proceed when the mapping to source IDs drifted — a deleted or
// re-created account changes it while SQLite may reuse the old numeric ID.
// The daemon must be reinitialized so the fingerprint, generation, and
// consent are re-derived for the new scope.
func visualScopeCheck(s *store.Store, accounts []string, expected []int64) func(context.Context) error {
	if len(accounts) == 0 {
		return func(context.Context) error { return nil }
	}
	want := slices.Clone(expected)
	slices.Sort(want)
	return func(context.Context) error {
		ids, err := resolveEmbedAccountList(s, accounts, true)
		if err != nil {
			return fmt.Errorf("[vector.multimodal.scope] accounts: %w", err)
		}
		got := vector.NewBuildScope(nil, ids).SourceIDs
		slices.Sort(got)
		if !slices.Equal(want, got) {
			return errors.New("the multimodal account scope no longer resolves to the same sources; restart the daemon to re-derive the visual generation and re-record consent")
		}
		return nil
	}
}

// loadVisualCapabilityManifest reads and strictly validates the operator's
// probed Voyage capability manifest. The multimodal lane cannot run without
// one: nothing has upload authority until a probe recorded it.
func loadVisualCapabilityManifest(path string) (voyage.CapabilityManifest, error) {
	if strings.TrimSpace(path) == "" {
		return voyage.CapabilityManifest{}, errors.New(
			"vector.multimodal.capabilities_file is not set; run `msgvault multimodal probe` and configure the manifest path")
	}
	file, err := os.Open(path)
	if err != nil {
		return voyage.CapabilityManifest{}, fmt.Errorf("open Voyage capability manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := voyage.DecodeCapabilityManifest(file)
	if err != nil {
		return voyage.CapabilityManifest{}, fmt.Errorf("decode Voyage capability manifest %s: %w", path, err)
	}
	return manifest, nil
}

// eligibleVisualCapabilities filters probed document capabilities to those
// whose interleaved twin is also probed when message context is enabled, so
// no eligible owner can produce a request shape the manifest does not cover.
func eligibleVisualCapabilities(authorized []string, contextEnabled bool) []string {
	interleavedTwin := map[string]string{
		voyage.CapabilityImageJPEG:        voyage.CapabilityInterleavedJPEG,
		voyage.CapabilityImagePNG:         voyage.CapabilityInterleavedPNG,
		voyage.CapabilityImageWebP:        voyage.CapabilityInterleavedWebP,
		voyage.CapabilityImageGIFStill:    voyage.CapabilityInterleavedGIFStill,
		voyage.CapabilityImageGIFAnimated: voyage.CapabilityInterleavedGIFAnimated,
		voyage.CapabilityVideoMP4:         voyage.CapabilityInterleavedMP4,
	}
	eligible := make([]string, 0, len(authorized))
	for _, capability := range authorized {
		twin, isDocument := interleavedTwin[capability]
		if !isDocument {
			continue
		}
		if contextEnabled && !slices.Contains(authorized, twin) {
			continue
		}
		eligible = append(eligible, capability)
	}
	slices.Sort(eligible)
	return eligible
}
