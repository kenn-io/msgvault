package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/mattn/go-sqlite3"
)

// SQLiteDialect implements Dialect for SQLite (the default backend).
type SQLiteDialect struct{}

func (d *SQLiteDialect) DriverName() string { return "sqlite3" }

// Rebind is a no-op for SQLite — it uses ? placeholders natively.
func (d *SQLiteDialect) Rebind(query string) string { return query }

// Now returns the SQLite expression for the current UTC timestamp.
func (d *SQLiteDialect) Now() string { return "datetime('now')" }

// InsertOrIgnore is a no-op for SQLite — the syntax is native.
func (d *SQLiteDialect) InsertOrIgnore(sql string) string { return sql }

// BoolTrueExpr returns "col = 1" — SQLite stores booleans as 0/1 INTEGER.
func (d *SQLiteDialect) BoolTrueExpr(col string) string { return col + " = 1" }

// JSONBindExpr is "?" on SQLite — JSON columns are plain TEXT.
func (d *SQLiteDialect) JSONBindExpr() string { return "?" }

// BuildFTSArg formats search terms as an FTS5 MATCH argument: each
// term double-quote-escaped, suffixed with "*" for prefix match, and
// space-joined (FTS5 treats space as implicit AND). Embedded "*" is
// stripped first so user input cannot break the trailing prefix
// operator. Matches the shape produced by the query package's
// SQLiteQueryDialect.BuildFTSTerm so the API search path and the
// engine deep-search path return the same hits for the same input —
// searching "invo" must match "invoice" in both paths.
//
// Terms that would tokenize to nothing under the default FTS5
// tokenizer (no Unicode letter or digit — e.g. "!!!", "---", "") are
// dropped. If all terms drop, returns "" so the caller can
// short-circuit instead of dispatching a malformed FTS5 MATCH that
// errors at the driver. Mirrors the empty-fallback shape in
// PostgreSQLDialect.BuildFTSArg.
func (d *SQLiteDialect) BuildFTSArg(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		if !hasFTSToken(t) {
			continue
		}
		t = strings.ReplaceAll(t, `"`, `""`)
		t = strings.ReplaceAll(t, "*", "")
		quoted = append(quoted, `"`+t+`"*`)
	}
	return strings.Join(quoted, " ")
}

// hasFTSToken reports whether s contains at least one rune that the
// default FTS5 tokenizer (unicode61) would emit as part of a token —
// i.e., a Unicode letter or digit. Punctuation-only strings tokenize
// to nothing, so a MATCH built from them is a syntax error.
func hasFTSToken(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// InsertOrIgnorePrefix is a no-op for SQLite — OR IGNORE stays in the prefix.
func (d *SQLiteDialect) InsertOrIgnorePrefix(sql string) string { return sql }

// InsertOrIgnoreSuffix returns "" for SQLite — OR IGNORE is in the statement prefix.
func (d *SQLiteDialect) InsertOrIgnoreSuffix() string { return "" }

// FTSUpsert inserts or replaces an FTS5 row. FTS5 requires rowid to be
// specified explicitly so the virtual table's rowid matches messages.id;
// the dialect owns this detail so callers don't pass messageID twice.
func (d *SQLiteDialect) FTSUpsert(q querier, doc FTSDoc) error {
	_, err := q.Exec(
		`INSERT OR REPLACE INTO messages_fts(rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		doc.MessageID, doc.MessageID, doc.Subject, doc.Body,
		doc.FromAddr, doc.ToAddrs, doc.CcAddrs,
	)
	return err
}

// FTSSearchClause returns SQL fragments for FTS5 full-text search.
//
// The bm25 weights approximate PostgreSQL's setweight field-priority
// preferences (subject heaviest, then sender, then body / other
// recipients) for typical email shapes. This is a best-effort SQLite
// tuning, NOT a strict cross-backend parity guarantee.
//
// Weights are positional over every column declared in messages_fts —
// UNINDEXED columns count too even though they cannot match — so the
// leading 1.0 is the placeholder for `message_id UNINDEXED`. The
// remaining slots map to (subject, body, from_addr, to_addr, cc_addr).
// PostgreSQL applies setweight 'A'=1.0 to subject and 'B'=0.4 to sender,
// leaving body and other recipients at default 'D'=0.1 — a 10:4:1 ratio,
// which bm25 reproduces as 10/1/4/1/1 across (subject, body, from, to,
// cc). bm25 returns lower (more negative) scores for more relevant rows,
// so callers ORDER BY this expression ascending (the default).
//
// Known divergence: SQLite's bm25() applies Okapi BM25 document-length
// normalization while PostgreSQL's default ts_rank() does not, so very
// long subject-hit documents can still rank below short body-hit
// documents on SQLite while PG ranks them subject-first. See the
// docs-site search ranking page ("Where Ordering Can Diverge") and
// TestFTSRank_KnownDivergence for the expected-behavior pin and
// rationale.
func (d *SQLiteDialect) FTSSearchClause() (join, where, orderBy string, orderArgCount int) {
	return "JOIN messages_fts ON messages_fts.rowid = m.id",
		"messages_fts MATCH ?",
		"bm25(messages_fts, 1.0, 10.0, 1.0, 4.0, 1.0, 1.0)",
		0
}

// FTSDeleteSQL returns the SQL to delete a message's FTS5 entry.
func (d *SQLiteDialect) FTSDeleteSQL() string {
	return `DELETE FROM messages_fts WHERE message_id IN (
		SELECT id FROM messages WHERE source_id = ?
	)`
}

// FTSBackfillBatchSQL returns the SQL to backfill FTS5 for a range of message IDs.
// Parameters: fromID(?), toID(?)
func (d *SQLiteDialect) FTSBackfillBatchSQL() string {
	return `INSERT OR REPLACE INTO messages_fts (rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		SELECT m.id, m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
			COALESCE(
				CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
				     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
				END,
				(SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
				''
			),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id >= ? AND m.id < ?`
}

// FTSAvailable probes for FTS5 by querying the virtual table.
// Checking sqlite_master alone is insufficient: a binary built without FTS5
// support will fail with "no such module: fts5" even if the table exists.
func (d *SQLiteDialect) FTSAvailable(db *sql.DB) bool {
	var probe int
	err := db.QueryRowContext(context.Background(), "SELECT 1 FROM messages_fts LIMIT 1").Scan(&probe)
	return err == nil || errors.Is(err, sql.ErrNoRows)
}

// FTSNeedsBackfill reports whether the FTS5 table needs population.
// Probes for the existence of ANY message lacking an FTS entry, matching the
// PostgreSQL EXISTS(search_fts IS NULL) semantics. The previous MAX(rowid)
// vs MAX(id) heuristic missed a hole left at a LOW id while later ids were
// indexed — reachable because UpsertFTS failures during sync are
// warn-and-continue (sync.go) while the message row still commits, so id N can
// be unindexed while N+1.. are indexed. messages_fts.rowid == messages.id and
// there are no triggers, so the NOT EXISTS join is rowid-served and cheap on
// FTS5 (no full body scan).
func (d *SQLiteDialect) FTSNeedsBackfill(db *sql.DB) bool {
	var exists bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (
			SELECT 1 FROM messages m
			 WHERE NOT EXISTS (
			     SELECT 1 FROM messages_fts f WHERE f.rowid = m.id
			 )
		)`,
	).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// FTSClearSQL returns the SQL to clear all FTS5 data.
func (d *SQLiteDialect) FTSClearSQL() string {
	return "DELETE FROM messages_fts"
}

// SchemaFTS returns the embedded filename containing FTS5 virtual table DDL.
func (d *SQLiteDialect) SchemaFTS() string {
	return "schema_sqlite.sql"
}

// FTSRebuildSchema drops and recreates the messages_fts virtual table. The
// DROP pathway discards FTS5 shadow tables in their entirety, which is the
// only reliable fix when those shadow tables are malformed — the `rebuild`
// pragma reads from them and `delete-all` is rejected on contentful tables.
//
// Runs on the querier so RebuildFTS can route it through the maintenance
// transaction (finding S1). SQLite DDL is transactional, so DROP/CREATE of
// the virtual table run fine inside the tx runMaintenance opens; SQLite has
// no statement_timeout, so the hatch is a plain transaction here.
func (d *SQLiteDialect) FTSRebuildSchema(q querier) error {
	if _, err := q.Exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
		return fmt.Errorf("drop messages_fts: %w", err)
	}
	schema, err := schemaFS.ReadFile("schema_sqlite.sql")
	if err != nil {
		return fmt.Errorf("read schema_sqlite.sql: %w", err)
	}
	if _, err := q.Exec(string(schema)); err != nil {
		if d.IsNoSuchModuleError(err) {
			return errors.New("cannot rebuild FTS: this msgvault binary was built without " +
				"FTS5 support (rebuild with `-tags fts5`)",
			)
		}
		return fmt.Errorf("create messages_fts: %w", err)
	}
	return nil
}

// EnsureFTSIndex is a no-op for SQLite: its messages_fts virtual table (and
// the index it implies) is created via the SchemaFTS file during InitSchema,
// not a post-migration step (cr2-10).
func (d *SQLiteDialect) EnsureFTSIndex(querier) error { return nil }

// EnsureTriggers is a no-op for SQLite: the last_modified triggers are
// `CREATE TRIGGER IF NOT EXISTS` in schema.sql, which InitSchema re-execs
// idempotently on every open (fresh and existing DBs alike).
func (d *SQLiteDialect) EnsureTriggers(querier) error { return nil }

// LegacyColumnMigrations returns the ALTER TABLE ADD COLUMN statements that
// bring older SQLite databases up to the current schema. IsDuplicateColumnError
// silences these when the column already exists (idempotent migrations).
func (d *SQLiteDialect) LegacyColumnMigrations() []ColumnMigration {
	return []ColumnMigration{
		{`ALTER TABLE sources ADD COLUMN sync_config JSON`, "sync_config"},
		{`ALTER TABLE messages ADD COLUMN rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN sender_id INTEGER REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN deleted_from_source_at DATETIME`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN deleted_at DATETIME`, "deleted_at"},
		{`ALTER TABLE messages ADD COLUMN delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
		// embed_gen: per-message vector-embedding watermark. NULL default
		// means every legacy row reads as "needs embedding", which is
		// correct — the scan-and-fill worker (and backstop) will embed and
		// stamp them. No backfill.
		{`ALTER TABLE messages ADD COLUMN embed_gen INTEGER`, "embed_gen"},
		// last_modified: row-level last-modified watermark, the embed
		// worker's optimistic-CAS token. SQLite rejects a non-constant
		// DEFAULT in ADD COLUMN ("Cannot add a column with non-constant
		// default"), so the column is added with no default (existing rows
		// get NULL) and InitSchema's backfillLastModified follows up with a
		// one-shot `UPDATE ... SET last_modified = CURRENT_TIMESTAMP WHERE
		// last_modified IS NULL` so the CAS token is a comparable value
		// (NULL would never match `last_modified = ?`). Fresh DBs keep the
		// CREATE TABLE default in schema.sql, which IS allowed.
		{`ALTER TABLE messages ADD COLUMN last_modified DATETIME`, "last_modified"},
	}
}

// DatabaseSize returns the on-disk size of the SQLite database file.
// Returns (0, nil) for in-memory databases or when the file cannot be stat'd.
func (d *SQLiteDialect) DatabaseSize(_ *sql.DB, dbPath string) (int64, error) {
	if dbPath == "" || dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") {
		return 0, nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0, nil //nolint:nilerr // missing/unstattable db file reports 0 size, not an error
	}
	return info.Size(), nil
}

// InitConn is a no-op for SQLite — PRAGMAs are set via DSN parameters.
func (d *SQLiteDialect) InitConn(db *sql.DB) error { return nil }

// SchemaFiles returns the schema files to execute during InitSchema.
func (d *SQLiteDialect) SchemaFiles() []string {
	return []string{"schema.sql"}
}

// CheckpointWAL forces a WAL checkpoint using TRUNCATE mode.
func (d *SQLiteDialect) CheckpointWAL(db *sql.DB) error {
	var busy, log, checkpointed int
	err := db.QueryRowContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	if err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf(
			"WAL checkpoint incomplete: database busy "+
				"(log=%d, checkpointed=%d)", log, checkpointed,
		)
	}
	return nil
}

// SchemaStaleCheck returns the SQL to check whether the most recent migration column exists.
func (d *SQLiteDialect) SchemaStaleCheck() string {
	return "SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'embed_gen'"
}

// IsDuplicateColumnError returns true if the error is "duplicate column name" from ALTER TABLE.
func (d *SQLiteDialect) IsDuplicateColumnError(err error) bool {
	return isSQLiteError(err, "duplicate column name")
}

// IsConflictError returns true if the error is a UNIQUE constraint violation.
func (d *SQLiteDialect) IsConflictError(err error) bool {
	return isSQLiteError(err, "UNIQUE constraint failed")
}

// IsNoSuchTableError returns true if the error indicates a missing table.
func (d *SQLiteDialect) IsNoSuchTableError(err error) bool {
	return isSQLiteError(err, "no such table")
}

// IsNoSuchModuleError returns true if the error indicates a missing module (e.g., fts5).
func (d *SQLiteDialect) IsNoSuchModuleError(err error) bool {
	return isSQLiteError(err, "no such module: fts5")
}

// IsReturningError returns true if the error indicates RETURNING is not supported.
func (d *SQLiteDialect) IsReturningError(err error) bool {
	return isSQLiteError(err, "RETURNING")
}

// BeginExclusive opens a SQLite "BEGIN EXCLUSIVE" transaction on conn.
// In WAL mode this blocks concurrent writers while readers can proceed.
func (d *SQLiteDialect) BeginExclusive(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE")
	return err
}

// BeginWriteSQL returns "BEGIN IMMEDIATE" so the transaction reserves
// the SQLite writer lock at BEGIN, removing the snapshot-isolation race
// that lets two deferred transactions both read the pre-update value.
func (d *SQLiteDialect) BeginWriteSQL() string { return "BEGIN IMMEDIATE" }

// SelectForUpdate returns "" — SQLite has no FOR UPDATE; serialization
// comes from BEGIN IMMEDIATE.
func (d *SQLiteDialect) SelectForUpdate() string { return "" }

// MaintenanceTimeoutResetSQL returns "" — SQLite has no statement_timeout,
// so Store.runMaintenance issues no reset statement and SQLite's
// transactional behavior is unchanged.
func (d *SQLiteDialect) MaintenanceTimeoutResetSQL() string { return "" }

// IsBusyError returns true for SQLITE_BUSY and SQLITE_LOCKED. Matching on
// the result code is more robust than substring matching: BUSY surfaces as
// "database is locked" but LOCKED surfaces as "database table is locked",
// so a single substring cannot catch both.
func (d *SQLiteDialect) IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	var serr sqlite3.Error
	if errors.As(err, &serr) {
		return serr.Code == sqlite3.ErrBusy || serr.Code == sqlite3.ErrLocked
	}
	var serrPtr *sqlite3.Error
	if errors.As(err, &serrPtr) && serrPtr != nil {
		return serrPtr.Code == sqlite3.ErrBusy || serrPtr.Code == sqlite3.ErrLocked
	}
	return false
}

// IsFTSValueTooLargeError always returns false for SQLite: FTS5 has no
// per-value size limit analogous to PostgreSQL's tsvector "string is too long"
// (SQLSTATE 54000), so the backfill never has a row to skip on SQLite.
func (d *SQLiteDialect) IsFTSValueTooLargeError(err error) bool { return false }
