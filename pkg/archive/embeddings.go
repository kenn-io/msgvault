package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// Embeddings configures message embeddings with a host-owned client. Changing
// Model, Dimension, or either prefix requires Setup again and starts a rebuild.
// Model, Dimension, and the prefixes must match the configuration on Client.
type Embeddings struct {
	Client         *embedclient.Client
	Model          string
	Dimension      int
	DocumentPrefix string
	QueryPrefix    string
}

type embeddingRuntime struct {
	backend *pgvector.Backend
	config  vector.Config
	engine  *hybrid.Engine
	job     *scheduler.EmbedJob
}

func embeddingConnectionURL(ctx context.Context, postgres PostgreSQL, db *sql.DB) (string, error) {
	var schema string
	err := db.QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'vector'`).Scan(&schema)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("find pgvector schema: %w", err)
	}
	var path string
	if postgres.Schema == "" && schema != "" {
		if err := db.QueryRowContext(ctx, "SELECT current_setting('search_path')").Scan(&path); err != nil {
			return "", fmt.Errorf("read archive search path: %w", err)
		}
	}
	return postgres.connectionURL(schema, path)
}

func embeddingConfig(settings *Embeddings) vector.Config {
	cfg := vector.Config{Enabled: true, Backend: "pgvector"}
	cfg.Embeddings.Model = settings.Model
	cfg.Embeddings.Dimension = settings.Dimension
	cfg.Embeddings.DocumentPrefix = settings.DocumentPrefix
	cfg.Embeddings.QueryPrefix = settings.QueryPrefix
	cfg.ApplyDefaults()
	return cfg
}

func setupEmbeddings(ctx context.Context, st *store.Store, settings *Embeddings) error {
	if !pgvector.Available() {
		return nil
	}
	var available bool
	if err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	cfg := embeddingConfig(settings)
	var generationsExist bool
	if err := st.DB().QueryRowContext(ctx, `SELECT to_regclass('index_generations') IS NOT NULL`).Scan(&generationsExist); err != nil {
		return err
	}
	if generationsExist {
		rows, err := st.DB().QueryContext(ctx, `SELECT fingerprint FROM index_generations WHERE state IN ('active','building')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var fingerprint string
			if err := rows.Scan(&fingerprint); err != nil {
				_ = rows.Close()
				return err
			}
			if err := vector.ValidateGenerationPolicy(fingerprint); err != nil {
				_ = rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: settings.Dimension, SkipExtension: true})
	if err != nil {
		return err
	}
	building, err := backend.BuildingGeneration(ctx)
	if err != nil {
		return err
	}
	if building != nil {
		if building.Fingerprint == cfg.GenerationFingerprint() {
			return nil
		}
		if err := backend.RetireGeneration(ctx, building.ID, false); err != nil {
			return err
		}
	}
	active, err := backend.ActiveGeneration(ctx)
	if err == nil && active.Fingerprint == cfg.GenerationFingerprint() {
		return nil
	}
	if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
		return err
	}
	_, err = backend.CreateGeneration(ctx, settings.Model, settings.Dimension, cfg.GenerationFingerprint())
	return err
}

func openEmbeddings(ctx context.Context, st *store.Store, settings *Embeddings) (*embeddingRuntime, error) {
	var runtime *embeddingRuntime
	unavailable := func(err error) (*embeddingRuntime, error) {
		return runtime, fmt.Errorf("pgvector is missing or Setup has not prepared embeddings for %s: %w", settings.Model, err)
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: settings.Dimension, SkipMigrate: true, ReadOnly: true})
	if err != nil {
		return unavailable(err)
	}
	var storage bool
	if err := st.DB().QueryRowContext(ctx, `SELECT to_regclass('embeddings') IS NOT NULL`).Scan(&storage); err != nil {
		return unavailable(err)
	}
	if !storage {
		return unavailable(errors.New("embedding storage is missing"))
	}
	runtime = &embeddingRuntime{backend: backend}
	if settings.Client == nil || settings.Model == "" || settings.Dimension <= 0 {
		return unavailable(errors.New("embedding client, model, and dimension are required"))
	}
	cfg := embeddingConfig(settings)
	building, err := backend.BuildingGeneration(ctx)
	if err != nil {
		return unavailable(err)
	}
	active, activeErr := backend.ActiveGeneration(ctx)
	if activeErr != nil && !errors.Is(activeErr, vector.ErrNoActiveGeneration) {
		return unavailable(activeErr)
	}
	if (building != nil && building.Fingerprint != cfg.GenerationFingerprint()) || (building == nil && (activeErr != nil || active.Fingerprint != cfg.GenerationFingerprint())) {
		return unavailable(errors.New("generation is missing"))
	}
	client := embed.NewClient(embed.Config{Kit: settings.Client, Model: settings.Model, Dimension: settings.Dimension})
	worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: st.DB(), MainDB: st.DB(), Store: st, Client: client, Preprocess: embed.PreprocessConfigFor(cfg), MaxInputChars: cfg.Embeddings.MaxInputChars, BatchSize: cfg.Embeddings.BatchSize, Rebind: st.Rebind, LastModifiedExpr: "m.last_modified", Recorder: st, Diagnostics: st})
	job := &scheduler.EmbedJob{Strict: true, Worker: worker, Backend: backend, Store: st, Fingerprint: cfg.GenerationFingerprint(), RequestTimeout: cfg.Embeddings.Timeout, BatchSize: cfg.Embeddings.BatchSize, BackstopInterval: cfg.Embed.BackstopInterval}
	engine := hybrid.NewEngine(backend, st.DB(), client, hybrid.Config{ExpectedFingerprint: cfg.GenerationFingerprint(), RRFK: cfg.Search.RRFK, KPerSignal: cfg.Search.KPerSignal, SubjectBoost: cfg.Search.SubjectBoost, Rebind: st.Rebind})
	runtime.config, runtime.engine, runtime.job = cfg, engine, job
	return runtime, nil
}

// Embed runs one embedding pass. Stop calls to Embed before Close.
func (a *Archive) Embed(ctx context.Context) error {
	if a.vectorErr != nil {
		return a.vectorErr
	}
	if a.embeddings == nil {
		return nil
	}
	return a.embeddings.job.Run(ctx)
}

// NewServer constructs an API server using the archive's configured services.
func (a *Archive) NewServer(options ServerOptions) *Server {
	if options.Store == nil {
		options.Store = a.Store()
	}
	if options.Engine == nil {
		options.Engine = a.QueryEngine()
	}
	if a.embeddings != nil && a.embeddings.engine != nil {
		if options.HybridEngine == nil {
			options.HybridEngine = a.embeddings.engine
		}
		if options.Backend == nil {
			options.Backend = a.embeddings.backend
		}
		if !options.VectorCfg.AnyLaneEnabled() {
			options.VectorCfg = a.embeddings.config
		}
	}
	server := NewServer(options)
	if a.vectorErr != nil {
		server.SetVectorInitError(a.vectorErr)
	}
	return server
}
