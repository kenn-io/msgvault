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
// Use RebuildEmbeddings when its vector identity changes.
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
		return vector.Config{}, fmt.Errorf("vector identity: %w", err)
	}
	cfg := vector.Config{Enabled: true, Backend: "pgvector"}
	cfg.Embeddings.Model = settings.Descriptor.Model.Name
	cfg.Embeddings.Dimension = settings.Descriptor.Model.Dimensions
	cfg.Embeddings.VectorIdentity = identity
	cfg.ApplyDefaults()
	return cfg, nil
}

func embeddingGeneration(ctx context.Context, backend *pgvector.Backend, cfg vector.Config, create, replace bool) (vector.Generation, bool, error) {
	fingerprint := cfg.GenerationFingerprint()
	building, err := backend.BuildingGeneration(ctx)
	if err != nil {
		return vector.Generation{}, false, err
	}
	if building != nil && building.Fingerprint != fingerprint {
		if !replace {
			return vector.Generation{}, false, fmt.Errorf("%w: building fingerprint %q differs from configured %q; use RebuildEmbeddings to replace it", vector.ErrBuildingInProgress, building.Fingerprint, fingerprint)
		}
		if err := backend.RetireGeneration(ctx, building.ID, false); err != nil {
			return vector.Generation{}, false, err
		}
		building = nil
	}
	active, err := backend.ActiveGeneration(ctx)
	if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
		return vector.Generation{}, false, err
	}
	if err == nil && active.Fingerprint != fingerprint && !replace {
		return vector.Generation{}, false, fmt.Errorf("embedding generation %q differs from configured %q; use RebuildEmbeddings to replace it", active.Fingerprint, fingerprint)
	}
	if building != nil {
		return *building, true, nil
	}
	if err == nil && active.Fingerprint == fingerprint {
		return active, false, nil
	}
	if !create {
		return vector.Generation{}, false, fmt.Errorf("%w: run Setup to create an embedding generation", vector.ErrNoActiveGeneration)
	}
	id, err := backend.CreateGeneration(ctx, cfg.Embeddings.Model, cfg.Embeddings.Dimension, fingerprint)
	if err != nil {
		return vector.Generation{}, false, err
	}
	return vector.Generation{ID: id, Fingerprint: fingerprint}, true, nil
}

func setupEmbeddings(ctx context.Context, st *store.Store, cfg vector.Config, rebuild bool) error {
	if !pgvector.Available() {
		return nil
	}
	var extensionSchema string
	if err := st.DB().QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'vector'`).Scan(&extensionSchema); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var exists bool
	if err := st.DB().QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), 'index_generations')) IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	// A new generation creates its own indexes; existing generations need migration to check them.
	dimension := 0
	if rebuild {
		dimension = cfg.Embeddings.Dimension
	}
	if exists && !rebuild {
		backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: cfg.Embeddings.Dimension, SkipMigrate: true, ReadOnly: true})
		if err != nil {
			return err
		}
		_, _, err = embeddingGeneration(ctx, backend, cfg, false, false)
		if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
			return err
		}
		if err == nil {
			dimension = cfg.Embeddings.Dimension
		}
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: dimension, SkipExtension: true, ExtensionSchema: extensionSchema})
	if err != nil {
		return err
	}
	_, _, err = embeddingGeneration(ctx, backend, cfg, true, rebuild)
	return err
}

func openEmbeddings(ctx context.Context, st *store.Store, settings *Embeddings, cfg vector.Config) (*embeddingRuntime, error) {
	unavailable := func(err error) (*embeddingRuntime, error) {
		return nil, fmt.Errorf("pgvector is missing or Setup has not prepared embeddings: %w", err)
	}
	backend, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), Dimension: cfg.Embeddings.Dimension, SkipMigrate: true, ReadOnly: true})
	if err != nil {
		return unavailable(err)
	}
	var storage bool
	if err := st.DB().QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), 'embeddings')) IS NOT NULL`).Scan(&storage); err != nil {
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

// Embed runs a full embedding pass. The caller serializes calls across all handles
// for the same archive and stops them before Close.
func (a *Archive) Embed(ctx context.Context) error {
	return a.embed(ctx, false)
}

func (a *Archive) embed(ctx context.Context, replace bool) error {
	if a.vectorErr != nil {
		return a.vectorErr
	}
	if a.embeddings == nil {
		return nil
	}
	rt := a.embeddings
	gen, building, err := embeddingGeneration(ctx, rt.backend, rt.config, false, replace)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := rt.worker.RunBackstop(ctx, gen.ID, operations.PassScope{Key: "archive-embed:" + uuid.NewString(), Trigger: operations.TriggerManual, StartedAt: now})
	if err == nil && res.Failed > 0 {
		err = fmt.Errorf("embedding failed for %d messages", res.Failed)
	}
	if err != nil {
		return err
	}
	if building {
		return rt.backend.ActivateGeneration(ctx, gen.ID, false)
	}
	missing, err := a.store.MissingCount(ctx, int64(gen.ID))
	if err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("embedding generation has %d messages remaining", missing)
	}
	return nil
}

// NewServer constructs an API server using the archive's configured services.
func (a *Archive) NewServer(options ServerOptions) *Server {
	if options.Store == nil {
		options.Store = a.Store()
	}
	if options.Engine == nil {
		options.Engine = a.QueryEngine()
	}
	if a.embeddings != nil {
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
