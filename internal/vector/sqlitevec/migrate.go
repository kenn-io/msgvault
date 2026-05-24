//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed schema.sql
var schemaSQL string

// Migrate runs the baseline schema and ensures a vec0 virtual table
// for `defaultDim` exists. Safe to run on every startup.
//
// PRAGMA foreign_keys is applied per-connection by the ConnectHook
// registered in RegisterExtension; it is intentionally not run here.
func Migrate(ctx context.Context, db *sql.DB, defaultDim int) error {
	// Migrate legacy embeddings + vec0 tables to the chunked layout
	// BEFORE running the baseline schema. The baseline CREATE TABLE IF
	// NOT EXISTS would skip the new columns on a legacy DB, and the
	// embeddings table rename relies on the legacy columns still being
	// present.
	if err := migrateEmbeddingsToChunked(ctx, db); err != nil {
		return fmt.Errorf("migrate embeddings to chunked layout: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// Idempotent column additions for databases initialized before
	// these columns existed. SQLite returns "duplicate column name"
	// when re-applied; we treat that as success.
	for _, m := range []struct {
		sql  string
		desc string
	}{
		{`ALTER TABLE index_generations ADD COLUMN seeded_at INTEGER`, "seeded_at"},
	} {
		if _, err := db.ExecContext(ctx, m.sql); err != nil &&
			!isDuplicateColumnErr(err) {
			return fmt.Errorf("migrate vectors.db (%s): %w", m.desc, err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if err := migrateVecTablesToChunked(ctx, db); err != nil {
		return fmt.Errorf("migrate vec tables to chunked layout: %w", err)
	}
	return EnsureVectorTable(ctx, db, defaultDim)
}

// migrateEmbeddingsToChunked detects the pre-chunking schema (no
// embedding_id column) and rewrites the embeddings table in place:
// adds embedding_id as the AUTOINCREMENT rowid, populates it from
// existing rows (preserving message_id == embedding_id so the
// dimension-specific vec0 table's rowid stays valid through
// migrateVecTablesToChunked), and adds chunk_index/chunk_char_start/
// chunk_char_end with their zero defaults so legacy rows become
// chunk 0 of their message.
//
// All schema mutation and the copy happen inside one transaction.
// If the process crashes mid-way SQLite rolls back atomically, so a
// subsequent retry of Migrate() observes the unchanged legacy schema
// and runs the migration cleanly. Crucially, this prevents the
// pathological half-state where the new embeddings table exists,
// looks "already migrated" by the column probe, but is empty —
// silently dropping every legacy row.
//
// Idempotent: a freshly-initialized DB has no `embeddings` table at
// migrate time, and an already-migrated DB has the new layout — both
// paths return nil with no SQL executed. A leftover `embeddings_legacy`
// from a previous failed run (theoretically impossible with the
// transactional approach, but defensive against schema state left by
// older migration code) is also handled.
func migrateEmbeddingsToChunked(ctx context.Context, db *sql.DB) error {
	exists, err := tableExists(ctx, db, "embeddings")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	hasNew, err := columnExists(ctx, db, "embeddings", "embedding_id")
	if err != nil {
		return err
	}
	if hasNew {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `ALTER TABLE embeddings RENAME TO embeddings_legacy`); err != nil {
		return fmt.Errorf("rename legacy embeddings: %w", err)
	}
	// SQLite preserves index NAMES across ALTER TABLE RENAME (only the
	// indexed table reference is rewritten), so the legacy
	// `idx_embeddings_msg` still occupies the namespace and would
	// collide with the new index of the same name below. Drop it
	// explicitly — the legacy table is going away in this same tx
	// anyway, so dropping its index has no separate consequence.
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_embeddings_msg`); err != nil {
		return fmt.Errorf("drop legacy idx_embeddings_msg: %w", err)
	}
	// Create the new embeddings table inline (cannot re-run the full
	// schemaSQL inside the tx because it would also try to recreate
	// the vec0 tables, which can't be created mid-transaction with
	// vec0-internal state). The DDL must match schema.sql exactly so
	// the subsequent CREATE TABLE IF NOT EXISTS after the tx is a
	// genuine no-op.
	if _, err := tx.ExecContext(ctx, `CREATE TABLE embeddings (
		embedding_id     INTEGER PRIMARY KEY AUTOINCREMENT,
		generation_id    INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
		message_id       INTEGER NOT NULL,
		chunk_index      INTEGER NOT NULL DEFAULT 0,
		embedded_at      INTEGER NOT NULL,
		source_char_len  INTEGER NOT NULL,
		chunk_char_start INTEGER NOT NULL DEFAULT 0,
		chunk_char_end   INTEGER NOT NULL DEFAULT 0,
		truncated        INTEGER NOT NULL DEFAULT 0,
		UNIQUE (generation_id, message_id, chunk_index)
	)`); err != nil {
		return fmt.Errorf("create new embeddings table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`); err != nil {
		return fmt.Errorf("recreate idx_embeddings_msg: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX idx_embeddings_gen_msg ON embeddings(generation_id, message_id)`); err != nil {
		return fmt.Errorf("create idx_embeddings_gen_msg: %w", err)
	}
	// Copy without specifying embedding_id so AUTOINCREMENT allocates
	// a fresh, globally-unique rowid for each legacy row. The legacy
	// embeddings PK was (generation_id, message_id), which allows the
	// same message_id to appear under multiple generations (e.g. one
	// active + one building); equating embedding_id with message_id
	// would have collided on the new UNIQUE constraint in that case.
	// chunk_char_start/_end default to 0 — they describe the
	// preprocessed-text span used at embed time and we don't have
	// that information for already-embedded rows. chunk_char_* is
	// debug-only metadata, so a zero placeholder is acceptable.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO embeddings
			(generation_id, message_id, chunk_index,
			 embedded_at, source_char_len, chunk_char_start, chunk_char_end, truncated)
		SELECT generation_id, message_id, 0,
			   embedded_at, source_char_len, 0, source_char_len, truncated
		FROM embeddings_legacy`); err != nil {
		return fmt.Errorf("copy legacy embeddings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE embeddings_legacy`); err != nil {
		return fmt.Errorf("drop embeddings_legacy: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	return nil
}

// migrateVecTablesToChunked rebuilds every existing vec0 table whose
// schema names the second column "message_id" (pre-chunking) into the
// new layout where that column is "embedding_id". The data is copied
// 1:1 because migrateEmbeddingsToChunked preserved embedding_id ==
// message_id for legacy rows, so each vec0 row's rowid is still the
// correct embedding_id under the new schema; only the column name
// changes.
//
// Idempotent: skips tables already on the new layout.
func migrateVecTablesToChunked(ctx context.Context, db *sql.DB) error {
	// Only the user-facing vec0 virtual tables — `sqlite_master.sql`
	// names them via `CREATE VIRTUAL TABLE ... USING vec0`. The
	// associated shadow tables (`vectors_vec_dN_chunks`,
	// `vectors_vec_dN_info`, `vectors_vec_dN_rowids`,
	// `vectors_vec_dN_vector_chunks00`, ...) are sqlite-vec internals
	// that come along when the virtual table is created and disappear
	// when it's dropped. Filtering by type='table' alone would catch
	// them and try to ALTER their internal schemas; matching on the
	// CREATE statement keeps us to the parent table only.
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table'
		  AND name LIKE 'vectors_vec_d%'
		  AND sql LIKE '%USING vec0%'`)
	if err != nil {
		return fmt.Errorf("list vec tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("scan vec table name: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate vec tables: %w", err)
	}
	for _, name := range names {
		hasNew, err := columnExists(ctx, db, name, "embedding_id")
		if err != nil {
			return err
		}
		if hasNew {
			continue
		}
		// Pull the dimension out of the table name (vectors_vec_dN).
		// We do not parse the CREATE statement — the name is the
		// authoritative source for the dim everywhere else in the
		// code.
		var dim int
		if _, err := fmt.Sscanf(name, "vectors_vec_d%d", &dim); err != nil || dim <= 0 {
			return fmt.Errorf("decode dim from %q: %w", name, err)
		}
		// vec0 virtual tables cannot be ALTERed and cannot be RENAMEd
		// (the rename does not carry the shadow tables — *_rowids,
		// *_info, *_chunks, *_vector_chunks00 — that vec0 maintains
		// alongside the virtual table). Instead: stream every row out
		// into memory, DROP the virtual table (which cleanly removes
		// its shadow tables too), recreate it under the new schema,
		// and re-INSERT. Acceptable because the data is small
		// (~3 KB/row at 768 dims) and the migration runs once per
		// existing database.
		if err := rebuildVecTableForChunking(ctx, db, name, dim); err != nil {
			return fmt.Errorf("rebuild %s: %w", name, err)
		}
	}
	return nil
}

// rebuildVecTableForChunking drops the legacy vec0 table named `name`
// and recreates it under the new schema (embedding_id PRIMARY KEY in
// place of message_id PRIMARY KEY), preserving every row.
//
// The new vec0 rowid is the AUTOINCREMENT embedding_id allocated by
// migrateEmbeddingsToChunked, looked up here via the (generation_id,
// message_id) of each legacy row. This mapping is required because
// the legacy embeddings PK allows the same message_id under multiple
// generations, so a simple rowid=message_id carry-over would collide;
// the explicit lookup keeps each generation's vectors disjoint in
// the rebuilt vec0 table.
//
// The drop + create + re-insert sequence runs inside a single
// transaction. SQLite supports transactional DDL on virtual tables;
// sqlite-vec's vec0 cooperates because its shadow tables (chunks,
// info, rowids, vector_chunks00) are regular tables under the same
// transaction boundary. A crash mid-rebuild rolls back to the legacy
// state, so a subsequent Migrate() retry observes the unchanged
// schema and runs cleanly.
func rebuildVecTableForChunking(ctx context.Context, db *sql.DB, name string, dim int) error {
	type vecRow struct {
		gen   int64
		msgID int64
		blob  []byte
	}
	// Read every legacy row up-front, *outside* the transaction. The
	// virtual-table reader returns a custom iterator implementation;
	// holding it open across a DDL operation against the same table
	// (the DROP that follows) deadlocks or panics depending on the
	// build. Materializing into memory is cheap (one vec is ~3 KB at
	// 768 dims; even 100K vecs is ~300 MB and pre-chunking deployments
	// are typically much smaller — the user's vault has ~12K vecs).
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT generation_id, message_id, embedding FROM %s`, name))
	if err != nil {
		return fmt.Errorf("read legacy rows: %w", err)
	}
	var legacyRows []vecRow
	for rows.Next() {
		var r vecRow
		if err := rows.Scan(&r.gen, &r.msgID, &r.blob); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy row: %w", err)
		}
		legacyRows = append(legacyRows, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy rows: %w", err)
	}

	// Build (generation_id, message_id) -> embedding_id mapping from
	// the already-migrated embeddings table. The migration above
	// inserted exactly one chunk_index=0 row per legacy embedding, so
	// every legacy vec0 row has a corresponding embedding_id.
	type key struct{ gen, msg int64 }
	mapping := make(map[key]int64, len(legacyRows))
	mapRows, err := db.QueryContext(ctx,
		`SELECT generation_id, message_id, embedding_id FROM embeddings WHERE chunk_index = 0`)
	if err != nil {
		return fmt.Errorf("read embedding_id mapping: %w", err)
	}
	for mapRows.Next() {
		var g, m, eid int64
		if err := mapRows.Scan(&g, &m, &eid); err != nil {
			_ = mapRows.Close()
			return fmt.Errorf("scan embedding_id mapping: %w", err)
		}
		mapping[key{g, m}] = eid
	}
	if err := mapRows.Close(); err != nil {
		return fmt.Errorf("close mapping rows: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE %s`, name)); err != nil {
		return fmt.Errorf("drop legacy %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(
		generation_id INTEGER PARTITION KEY,
		embedding_id  INTEGER PRIMARY KEY,
		embedding     FLOAT[%d]
	)`, name, dim)); err != nil {
		return fmt.Errorf("create new %s: %w", name, err)
	}
	if len(legacyRows) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (generation_id, embedding_id, embedding) VALUES (?, ?, ?)`, name))
		if err != nil {
			return fmt.Errorf("prepare reinsert: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for _, r := range legacyRows {
			eid, ok := mapping[key{r.gen, r.msgID}]
			if !ok {
				// Defensive: every legacy vec0 row should have a
				// matching embeddings row from the prior migration
				// step. If not, we'd silently drop the vector — flag
				// it loudly instead so the operator can investigate.
				return fmt.Errorf("no embedding_id mapping for gen=%d msg=%d (legacy embeddings/vec0 out of sync)", r.gen, r.msgID)
			}
			if _, err := stmt.ExecContext(ctx, r.gen, eid, r.blob); err != nil {
				return fmt.Errorf("reinsert row gen=%d msg=%d eid=%d: %w", r.gen, r.msgID, eid, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rebuild tx: %w", err)
	}
	committed = true
	return nil
}

// tableExists is a thin wrapper that asks sqlite_master whether a
// regular or virtual table with `name` exists in the connected DB.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','virtual') AND name = ?`, name,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", name, err)
	}
	return n > 0, nil
}

// columnExists asks pragma_table_info whether `column` is one of the
// columns of `table`. Works for both regular tables and vec0 virtual
// tables (which expose their declared columns via the same pragma).
func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?`, table), column,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("probe %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// isDuplicateColumnErr matches SQLite's "duplicate column name" error
// from ALTER TABLE … ADD COLUMN re-runs. Same approach as
// internal/store/db_logger.go: substring match keeps the migration
// path driver-agnostic.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// EnsureVectorTable creates vectors_vec_dN for the given dimension if
// it does not already exist. The PARTITION KEY scopes rows by
// generation; the PRIMARY KEY rowid is embedding_id, which joins back
// to the embeddings metadata table to recover (message_id,
// chunk_index). Idempotent.
func EnsureVectorTable(ctx context.Context, db *sql.DB, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("invalid dimension %d", dim)
	}
	q := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS vectors_vec_d%d USING vec0(
		generation_id INTEGER PARTITION KEY,
		embedding_id  INTEGER PRIMARY KEY,
		embedding     FLOAT[%d]
	)`, dim, dim)
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("create vectors_vec_d%d: %w", dim, err)
	}
	return nil
}

// VectorTableName returns the dimension-specific vec0 table name.
func VectorTableName(dim int) string {
	return fmt.Sprintf("vectors_vec_d%d", dim)
}
