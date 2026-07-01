//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// precheckVectorFeatures validates vector configuration cheaply so runServe
// can fail fast on misconfiguration while deferring the expensive backend
// open/migrate/backfill to the background init task. Returns nil when
// vector search is disabled. mainPath is used to fail fast when it is a
// postgres:// DSN but this binary lacks the pgvector build tag — the one
// "binary built without support" case the cheap precheck can still catch
// synchronously, since setupVectorFeatures would otherwise only discover
// it later inside the background init goroutine.
func precheckVectorFeatures(mainPath string) error {
	if !cfg.Vector.Enabled {
		return nil
	}
	if store.IsPostgresURL(mainPath) && !pgvector.Available() {
		return errors.New("vector search is enabled in config but this binary was built without vector support; " +
			"to use vector search on PostgreSQL, rebuild with `go build -tags \"fts5 sqlite_vec pgvector\"` " +
			"or set [vector] enabled = false")
	}
	if err := cfg.Vector.Validate(); err != nil {
		return fmt.Errorf("vector config: %w", err)
	}
	if cronExpr := cfg.Vector.Embed.Schedule.Cron; cronExpr != "" {
		if err := scheduler.ValidateCronExpr(cronExpr); err != nil {
			return fmt.Errorf("invalid embed cron expression %q: %w", cronExpr, err)
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
func setupVectorFeatures(ctx context.Context, mainStore *store.Store, mainPath string, readOnly bool) (*vectorFeatures, error) {
	if !cfg.Vector.Enabled {
		return nil, nil //nolint:nilnil // vector disabled: callers nil-check vf; (nil, nil) means "no features, no error"
	}
	if err := cfg.Vector.Validate(); err != nil {
		return nil, fmt.Errorf("vector config: %w", err)
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
			Dimension:   cfg.Vector.Embeddings.Dimension,
			BuildScope:  cfg.Vector.Embed.Scope.BuildScope(),
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
			SkipExtension: cfg.Vector.SkipExtensionCreate,
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
		vecPath := cfg.Vector.DBPath
		if vecPath == "" {
			vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:       vecPath,
			MainPath:   mainPath,
			Dimension:  cfg.Vector.Embeddings.Dimension,
			MainDB:     mainDB,
			BuildScope: cfg.Vector.Embed.Scope.BuildScope(),
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

	client := embed.NewClient(embed.Config{
		Endpoint:   cfg.Vector.Embeddings.Endpoint,
		APIKey:     cfg.Vector.Embeddings.APIKey(),
		Model:      cfg.Vector.Embeddings.Model,
		Dimension:  cfg.Vector.Embeddings.Dimension,
		Timeout:    cfg.Vector.Embeddings.Timeout,
		MaxRetries: cfg.Vector.Embeddings.MaxRetries,
	})

	worker := embed.NewWorker(embed.WorkerDeps{
		Backend:   backend,
		VectorsDB: vectorsDB,
		MainDB:    mainDB,
		Store:     mainStore,
		Client:    client,
		Preprocess: embed.PreprocessConfig{
			StripQuotes:        cfg.Vector.Preprocess.StripQuotesEnabled(),
			StripSignatures:    cfg.Vector.Preprocess.StripSignaturesEnabled(),
			StripHTML:          cfg.Vector.Preprocess.StripHTMLEnabled(),
			StripBase64:        cfg.Vector.Preprocess.StripBase64Enabled(),
			StripURLTracking:   cfg.Vector.Preprocess.StripURLTrackingEnabled(),
			CollapseWhitespace: cfg.Vector.Preprocess.CollapseWhitespaceEnabled(),
		},
		MaxInputChars: cfg.Vector.Embeddings.MaxInputChars,
		BatchSize:     cfg.Vector.Embeddings.BatchSize,
		BuildScope:    cfg.Vector.Embed.Scope.BuildScope(),
		// Rebind makes the worker's body-fetch + watermark SQL run on pgx.
		// SQLiteDialect.Rebind is identity, so the SQLite path is unchanged.
		Rebind:           dialect.Rebind,
		LastModifiedExpr: lastModifiedExpr,
		Log:              logger,
	})

	engine := hybrid.NewEngine(backend, mainDB, client, hybrid.Config{
		ExpectedFingerprint: cfg.Vector.GenerationFingerprint(),
		RRFK:                cfg.Vector.Search.RRFK,
		KPerSignal:          cfg.Vector.Search.KPerSignal,
		SubjectBoost:        cfg.Vector.Search.SubjectBoost,
		// BuildFilter's participant/label lookups run against mainDB with ?
		// placeholders. On PG those must become $N or pgx rejects them, so
		// the serve/MCP hybrid engine (shared via vectorFeatures.HybridEngine)
		// carries the dialect's Rebind. SQLite's Rebind is identity.
		Rebind:     dialect.Rebind,
		BuildScope: cfg.Vector.Embed.Scope.BuildScope(),
	})

	// No sync-time enqueue: newly-persisted messages get embed_gen = NULL
	// by column default and the scan-and-fill worker picks them up.

	return &vectorFeatures{
		Backend:      backend,
		HybridEngine: engine,
		Worker:       worker,
		Cfg:          cfg.Vector,
		Close:        closeFn,
	}, nil
}
