//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// migrateExecer is the minimal subset of *sql.DB that Migrate needs:
// CREATE EXTENSION runs directly on the pool, and the schema apply +
// redundant-index drop run inside a transaction (BeginTx) so they can
// disable the pool-wide statement_timeout. *sql.DB satisfies this; tests
// pass a recording wrapper to assert which statements were issued.
type migrateExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Migrate enables the pgvector extension and applies the embedding
// schema. Safe to run on every startup: every statement uses IF NOT
// EXISTS or its equivalent.
//
// When skipExtension is true, the `CREATE EXTENSION IF NOT EXISTS vector`
// step is skipped — the vector extension is assumed to be managed/installed
// externally (by a DBA on a locked-down/managed PostgreSQL where a
// non-superuser cannot run CREATE EXTENSION). Schema, index, and the
// redundant-index drop below still run, so a non-superuser that holds DDL
// rights on its own schema can bring up the embedding tables. This differs
// from the read-only SkipMigrate path, which suppresses ALL DDL.
//
// The defaultDim argument is informational: a per-dimension HNSW index
// will be created lazily for the first generation that exercises a new
// dimension. If defaultDim > 0, Migrate eagerly creates the index for
// that dimension so the first ANN query doesn't pay the index build.
func Migrate(ctx context.Context, db migrateExecer, defaultDim int, skipExtension bool) error {
	// CREATE EXTENSION runs OUTSIDE the maintenance tx: it is fast and is
	// gated by skipExtension (a non-superuser would be rejected before any
	// DDL we care about). Keeping it on the pool keeps the gate observable.
	if !skipExtension {
		if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			return fmt.Errorf("create extension vector: %w", err)
		}
	}

	// Wrap the schema apply AND the redundant-index drop in a single
	// transaction that disables the pool-wide 30s statement_timeout
	// (finding S1, mirroring EnsureVectorIndex). Two reasons:
	//   - DROP INDEX takes an ACCESS EXCLUSIVE lock; on a busy serve daemon
	//     the lock-wait alone can exceed 30s.
	//   - On a legacy populated DB, schema.sql's `CREATE INDEX IF NOT EXISTS`
	//     for idx_embeddings_msg / idx_embeddings_dim builds over the full
	//     embeddings table and can exceed 30s.
	// schema.sql is fully transaction-safe — it contains no CREATE INDEX
	// CONCURRENTLY, VACUUM, or other statement that cannot run in a tx — so
	// wrapping it is valid. SET LOCAL is tx-scoped and auto-resets on
	// commit/rollback, so the disabled timeout cannot leak onto other pooled
	// connections.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pgvector migrate tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for pgvector migrate: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply pgvector schema: %w", err)
	}
	// Shed the redundant idx_embeddings_gen_msg index on existing DBs: it is
	// a pure leading-prefix of the embeddings primary key
	// (generation_id, message_id, chunk_index), so it only added write
	// amplification on the hottest table. New DBs never create it (the
	// CREATE was removed from schema.sql); this DROP cleans up DBs migrated
	// before that change. [V3]
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_embeddings_gen_msg`); err != nil {
		return fmt.Errorf("drop redundant idx_embeddings_gen_msg: %w", err)
	}
	// NOTE: the dead pending_embeddings queue table is NOT dropped here. The
	// scan-and-fill design replaced the per-generation seed queue with a live
	// messages.embed_gen scan, so the table is otherwise unused — BUT the
	// one-time upgrade backfill (BackfillEmbedGenForUpgrade) must first consult
	// it to preserve the legacy "pending re-embed" signal: a message could
	// carry BOTH a stale active-gen embedding AND an active-gen pending row.
	// Dropping it here, before the backfill reads it, would let the backfill
	// stamp those messages "covered" and never re-embed them. The drop now
	// happens in the writable Open path AFTER the backfill has consulted it
	// (see dropDeadPendingEmbeddings, called from Open). Migrate runs on
	// read-only opens too, where dropping would be wrong (it must linger until
	// a writable open honors the signal).
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pgvector migrate tx: %w", err)
	}

	if defaultDim > 0 {
		// EnsureVectorIndex opens its own statement_timeout-disabling tx
		// (HNSW builds are slow), so it runs after the migrate tx commits.
		if err := EnsureVectorIndex(ctx, db, defaultDim); err != nil {
			return err
		}
	}
	return nil
}

// EnsureVectorIndex creates a partial HNSW cosine index restricted to
// rows where dimension = dim. The partial WHERE guard lets generations
// with different dimensions coexist in the same embeddings table — the
// expression cast `(embedding::vector(dim))` only fires for rows that
// already match, so a 4-dim row never trips a 768-dim index. Idempotent.
func EnsureVectorIndex(ctx context.Context, db migrateExecer, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("invalid dimension %d", dim)
	}
	// vector_cosine_ops matches the cosine-similarity distance operator
	// (<=>) used by Search. HNSW build cost is modest for empty
	// tables, so creating eagerly on the first generation is cheap and
	// avoids paying for it on the first ANN query.
	stmt := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_embeddings_hnsw_d%d
		   ON embeddings
		USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
		   WHERE dimension = %d`,
		dim, dim, dim,
	)

	// Wrap the CREATE INDEX in a transaction that disables the pool-wide 30s
	// statement_timeout: EnsureVectorIndex is also called lazily from
	// CreateGeneration over a possibly-populated embeddings table, and HNSW
	// builds are slow enough to trip the shared store pool's 30s timeout on a
	// large archive (finding S1). SET LOCAL is tx-scoped and auto-resets on
	// commit/rollback, so the disabled timeout cannot leak onto other pooled
	// connections. This statement is a plain CREATE INDEX (NOT CONCURRENTLY),
	// so running it inside a transaction is valid.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hnsw index tx for dim %d: %w", dim, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for hnsw index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("create hnsw index for dim %d: %w", dim, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hnsw index for dim %d: %w", dim, err)
	}
	return nil
}

// VectorIndexName returns the dimension-specific HNSW index name.
// Exposed mainly for diagnostic purposes.
func VectorIndexName(dim int) string {
	return fmt.Sprintf("idx_embeddings_hnsw_d%d", dim)
}
