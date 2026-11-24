package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// Embeddings configures a host-owned client and the descriptor used to build it.
// Changing its vector identity requires Setup and rebuilds the index.
type Embeddings struct {
	Client     *embedclient.Client
	Descriptor embedmodel.Descriptor
}

type embeddingRuntime struct {
	backend *pgvector.Backend
	config  vector.Config
	engine  *hybrid.Engine
	worker  *embed.Worker
}

func validateEmbeddings(settings *Embeddings) error {
	if settings == nil || settings.Client == nil {
		return nil
	}
	if err := settings.Descriptor.Validate(); err != nil {
		return fmt.Errorf("embedding descriptor: %w", err)
	}
	if settings.Descriptor.Model.Dimensions <= 0 {
		return errors.New("embedding descriptor requires positive dimensions")
	}
	return nil
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

func embeddingConfig(settings *Embeddings) (vector.Config, error) {
	identity, err := settings.Descriptor.VectorIdentity()
	if err != nil {
		return vector.Config{}, err
	}
	cfg := vector.Config{Enabled: true, Backend: "pgvector"}
	cfg.Embeddings.Model = settings.Descriptor.Model.Name
	cfg.Embeddings.Dimension = settings.Descriptor.Model.Dimensions
	cfg.Embeddings.DocumentPrefix = settings.Descriptor.Roles.DocumentPrefix
	cfg.Embeddings.QueryPrefix = settings.Descriptor.Roles.QueryPrefix
	cfg.Embeddings.VectorIdentity = identity
	cfg.ApplyDefaults()
	return cfg, nil
}

func embeddingGeneration(ctx context.Context, backend *pgvector.Backend, cfg vector.Config, create bool) (vector.Generation, bool, error) {
	fingerprint := cfg.GenerationFingerprint()
	building, err := backend.BuildingGeneration(ctx)
	if err != nil {
		return vector.Generation{}, false, err
	}
	if building != nil {
		if building.Fingerprint != fingerprint {
			return vector.Generation{}, false, fmt.Errorf("%w: building fingerprint %q differs from configured %q; finish the build or retire it", vector.ErrBuildingInProgress, building.Fingerprint, fingerprint)
		}
		return *building, true, nil
	}
	active, err := backend.ActiveGeneration(ctx)
	if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
		return vector.Generation{}, false, err
	}
	if err == nil && active.Fingerprint == fingerprint {
		return active, false, nil
	}
	if !create {
		return vector.Generation{}, false, fmt.Errorf("embedding generation %q differs from configured %q; run Setup", active.Fingerprint, fingerprint)
	}
	id, err := backend.CreateGeneration(ctx, cfg.Embeddings.Model, cfg.Embeddings.Dimension, fingerprint)
	if err != nil {
		return vector.Generation{}, false, err
	}
	return vector.Generation{ID: id, Fingerprint: fingerprint}, true, nil
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
	cfg, err := embeddingConfig(settings)
	if err != nil {
		return err
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: cfg.Embeddings.Dimension, SkipExtension: true})
	if err != nil {
		return err
	}
	_, _, err = embeddingGeneration(ctx, backend, cfg, true)
	return err
}

func openEmbeddings(ctx context.Context, st *store.Store, settings *Embeddings) (*embeddingRuntime, error) {
	unavailable := func(err error) (*embeddingRuntime, error) {
		return nil, fmt.Errorf("pgvector is missing or Setup has not prepared embeddings: %w", err)
	}
	cfg, err := embeddingConfig(settings)
	if err != nil {
		return unavailable(err)
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: cfg.Embeddings.Dimension, SkipMigrate: true, ReadOnly: true})
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
	client := embed.NewClient(embed.Config{Kit: settings.Client, Model: cfg.Embeddings.Model, Dimension: cfg.Embeddings.Dimension})
	worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: st.DB(), MainDB: st.DB(), Store: st, Client: client, Preprocess: embed.PreprocessConfigFor(cfg), MaxInputChars: cfg.Embeddings.MaxInputChars, BatchSize: cfg.Embeddings.BatchSize, Rebind: st.Rebind, LastModifiedExpr: "m.last_modified", Recorder: st, Diagnostics: st})
	engine := hybrid.NewEngine(backend, st.DB(), client, hybrid.Config{ExpectedFingerprint: cfg.GenerationFingerprint(), RRFK: cfg.Search.RRFK, KPerSignal: cfg.Search.KPerSignal, SubjectBoost: cfg.Search.SubjectBoost, Rebind: st.Rebind})
	return &embeddingRuntime{backend: backend, config: cfg, engine: engine, worker: worker}, nil
}

// Embed runs a full embedding pass. The caller serializes calls and stops them before Close.
func (a *Archive) Embed(ctx context.Context) error {
	if a.vectorErr != nil {
		return a.vectorErr
	}
	if a.embeddings == nil {
		return nil
	}
	rt := a.embeddings
	gen, building, err := embeddingGeneration(ctx, rt.backend, rt.config, false)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := rt.worker.RunBackstop(ctx, gen.ID, operations.PassScope{Key: "archive-embed:" + uuid.NewString(), Trigger: operations.TriggerManual, StartedAt: now})
	if err != nil {
		return err
	}
	if res.Failed > 0 {
		return fmt.Errorf("embedding failed for %d messages", res.Failed)
	}
	if building {
		missing, err := a.store.MissingCount(ctx, int64(gen.ID))
		if err != nil {
			return err
		}
		if missing > 0 {
			return fmt.Errorf("embedding generation has %d messages remaining", missing)
		}
		if err := rt.backend.ActivateGeneration(ctx, gen.ID, false); err != nil {
			return err
		}
	}
	return a.pruneEmbeddings(ctx)
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
