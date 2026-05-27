package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for database/sql

	"go.kenn.io/msgvault/internal/sqldialect"
)

// PostgreSQLDialect implements Dialect for PostgreSQL.
type PostgreSQLDialect struct{}

func (d *PostgreSQLDialect) DriverName() string { return "pgx" }

// Rebind converts ? placeholders to PostgreSQL $1, $2, ... numbered
// placeholders. Delegates to sqldialect so the query package's
// PostgreSQLQueryDialect.Rebind stays in lockstep.
func (d *PostgreSQLDialect) Rebind(query string) string {
	return sqldialect.RebindPostgreSQL(query)
}

// Now returns the PostgreSQL expression for the current timestamp.
func (d *PostgreSQLDialect) Now() string { return "NOW()" }

// BoolTrueExpr returns the bare column name. PostgreSQL has a real BOOLEAN
// type and rejects integer comparisons (`col = 1`) against boolean columns.
func (d *PostgreSQLDialect) BoolTrueExpr(col string) string { return col }

// JSONBindExpr returns "?::JSONB" — PG won't implicit-cast text to JSONB,
// so a bare placeholder bound to a Go string raises a column-type
// mismatch on the sources.sync_config write path.
func (d *PostgreSQLDialect) JSONBindExpr() string { return "?::JSONB" }

// BuildFTSArg formats search terms for to_tsquery: each term is split
// into letter/digit-only lexemes via sqldialect.EscapeTSQueryTerm so
// punctuation like `-`, `.`, `@` (which would otherwise produce
// invalid tsquery strings such as `---:*` or `foo-bar:*`) becomes a
// lexeme boundary. Each surviving lexeme is suffixed with ":*" for
// prefix matching and joined with " & ". Matches the shape emitted by
// the query package's PostgreSQLQueryDialect.BuildFTSTerm so the API
// search and engine deep-search return the same hits. If no lexemes
// survive, returns "" so the caller can substitute a FALSE predicate
// rather than feed to_tsquery an empty argument.
func (d *PostgreSQLDialect) BuildFTSArg(terms []string) string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		for _, lex := range sqldialect.EscapeTSQueryTerm(t) {
			out = append(out, lex+":*")
		}
	}
	return strings.Join(out, " & ")
}

// InsertOrIgnore rewrites INSERT OR IGNORE INTO to INSERT INTO and appends
// " ON CONFLICT DO NOTHING" for complete statements. A statement is treated
// as a prefix (caller will append VALUES tuples + InsertOrIgnoreSuffix) only
// when it ends with the bare "VALUES" keyword; otherwise the rewrite assumes
// the input is a complete statement (VALUES-tuple, INSERT...SELECT, etc.)
// and appends the conflict clause.
func (d *PostgreSQLDialect) InsertOrIgnore(sql string) string {
	s := strings.Replace(sql, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
	trimmed := strings.TrimRight(s, " \t\n\r")
	if strings.HasSuffix(strings.ToUpper(trimmed), "VALUES") {
		return s
	}
	return trimmed + " ON CONFLICT DO NOTHING"
}

// InsertOrIgnorePrefix strips "OR IGNORE" from a chunked insert prefix —
// PostgreSQL's conflict clause is appended by InsertOrIgnoreSuffix instead.
// The input must end with "VALUES " (prefix form used by insertInChunks).
func (d *PostgreSQLDialect) InsertOrIgnorePrefix(sql string) string {
	return strings.Replace(sql, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
}

// InsertOrIgnoreSuffix returns the PostgreSQL suffix for conflict-ignoring batch inserts.
func (d *PostgreSQLDialect) InsertOrIgnoreSuffix() string {
	return " ON CONFLICT DO NOTHING"
}

// FTSUpsert updates the tsvector column on messages for a single message.
// PostgreSQL stores the FTS index inline on `messages.search_fts`, so there
// is no separate virtual table — the operation is an UPDATE, not an INSERT.
func (d *PostgreSQLDialect) FTSUpsert(q querier, doc FTSDoc) error {
	_, err := q.Exec(
		`UPDATE messages SET search_fts =
			setweight(to_tsvector('simple', COALESCE($2, '')), 'A') ||
			setweight(to_tsvector('simple', COALESCE($4, '')), 'B') ||
			to_tsvector('simple', COALESCE($3, '')) ||
			to_tsvector('simple', COALESCE($5, '')) ||
			to_tsvector('simple', COALESCE($6, ''))
		WHERE id = $1`,
		doc.MessageID, doc.Subject, doc.Body,
		doc.FromAddr, doc.ToAddrs, doc.CcAddrs,
	)
	return err
}

// FTSSearchClause returns SQL fragments for tsvector full-text search.
// PostgreSQL stores the tsvector on the messages table — no JOIN needed.
// Uses to_tsquery (not plainto_tsquery) so the bound argument can carry
// prefix-match operators ("invo:*" matches "invoice"); BuildFTSArg
// produces the matching shape. Uses `?` placeholders; loggedDB rebinds
// to `$N` at execution time. ts_rank needs the query term a second time,
// so orderArgCount is 1.
func (d *PostgreSQLDialect) FTSSearchClause() (join, where, orderBy string, orderArgCount int) {
	return "",
		"m.search_fts @@ to_tsquery('simple', ?)",
		"ts_rank(m.search_fts, to_tsquery('simple', ?)) DESC",
		1
}

// FTSDeleteSQL returns the SQL to clear tsvector data for messages belonging to a source.
func (d *PostgreSQLDialect) FTSDeleteSQL() string {
	return `UPDATE messages SET search_fts = NULL WHERE source_id = $1`
}

// FTSBackfillBatchSQL returns the SQL to populate tsvector for a range of message IDs.
// Parameters: $1=fromID, $2=toID. Uses LEFT JOIN on message_bodies via a subquery
// so messages without a body row are still indexed (subject + participants).
func (d *PostgreSQLDialect) FTSBackfillBatchSQL() string {
	return `UPDATE messages m SET search_fts =
		setweight(to_tsvector('simple', COALESCE(m.subject, '')), 'A') ||
		to_tsvector('simple', COALESCE(src.body_text, '')) ||
		setweight(to_tsvector('simple', COALESCE(
			CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
			     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
			END,
			(SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
			''
		)), 'B') ||
		to_tsvector('simple', COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), '')) ||
		to_tsvector('simple', COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), ''))
	FROM (
		SELECT m2.id, mb.body_text
		FROM messages m2
		LEFT JOIN message_bodies mb ON mb.message_id = m2.id
		WHERE m2.id >= $1 AND m2.id < $2
	) src
	WHERE m.id = src.id`
}

// FTSAvailable reports whether tsvector search is available.
// PostgreSQL always supports tsvector — check that the column exists.
func (d *PostgreSQLDialect) FTSAvailable(db *sql.DB) bool {
	var count int
	err := db.QueryRow(postgresColumnExistsSQL("messages", "search_fts")).Scan(&count)
	return err == nil && count > 0
}

// FTSNeedsBackfill reports whether the tsvector column needs population.
// Counts NULL search_fts rows directly so an interrupted backfill that
// leaves a low-id row NULL (and later inserts continue normally) still
// flags the gap — the previous max-vs-max comparison missed that case.
// Costs one indexable WHERE COUNT(*) per startup probe.
func (d *PostgreSQLDialect) FTSNeedsBackfill(db *sql.DB) bool {
	var nullCount int64
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE search_fts IS NULL",
	).Scan(&nullCount); err != nil {
		return false
	}
	return nullCount > 0
}

// FTSClearSQL returns the SQL to clear all tsvector data.
func (d *PostgreSQLDialect) FTSClearSQL() string {
	return "UPDATE messages SET search_fts = NULL"
}

// SchemaFTS returns "" for PostgreSQL — the tsvector column is part of the
// main schema_pg.sql, not a separate file.
func (d *PostgreSQLDialect) SchemaFTS() string {
	return ""
}

// FTSRebuildSchema clears every tsvector and recreates the GIN index
// so the caller's backfill can repopulate from scratch. The DROP +
// CREATE INDEX pair is the PG analogue of SQLite's DROP-and-recreate
// of the messages_fts virtual table; it covers a malformed index just
// as the SQLite path covers a malformed shadow table.
func (d *PostgreSQLDialect) FTSRebuildSchema(db *sql.DB) error {
	if _, err := db.Exec("DROP INDEX IF EXISTS messages_search_fts_idx"); err != nil {
		return fmt.Errorf("drop messages_search_fts_idx: %w", err)
	}
	if _, err := db.Exec("UPDATE messages SET search_fts = NULL"); err != nil {
		return fmt.Errorf("clear search_fts: %w", err)
	}
	if _, err := db.Exec(
		"CREATE INDEX IF NOT EXISTS messages_search_fts_idx ON messages USING GIN (search_fts)",
	); err != nil {
		return fmt.Errorf("create messages_search_fts_idx: %w", err)
	}
	return nil
}

// LegacyColumnMigrations returns the ALTER TABLE ADD COLUMN statements that
// bring older PostgreSQL databases up to the current schema. PostgreSQL has
// supported `ADD COLUMN IF NOT EXISTS` since 9.6, so each statement is
// idempotent on its own; IsDuplicateColumnError remains as a safety net.
// Types are translated from the SQLite list:
//
//	INTEGER (id ref) → BIGINT, INTEGER (counter) → INTEGER,
//	TEXT → TEXT, DATETIME → TIMESTAMPTZ, JSON → JSONB.
func (d *PostgreSQLDialect) LegacyColumnMigrations() []ColumnMigration {
	return []ColumnMigration{
		{`ALTER TABLE sources ADD COLUMN IF NOT EXISTS sync_config JSONB`, "sync_config"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN IF NOT EXISTS oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN IF NOT EXISTS phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN IF NOT EXISTS canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS sender_id BIGINT REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted_from_source_at TIMESTAMPTZ`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ`, "deleted_at"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
	}
}

// DatabaseSize queries pg_database_size() for the current database.
func (d *PostgreSQLDialect) DatabaseSize(db *sql.DB, _ string) (int64, error) {
	var size int64
	err := db.QueryRow("SELECT pg_database_size(current_database())").Scan(&size)
	if err != nil {
		return 0, fmt.Errorf("pg_database_size: %w", err)
	}
	return size, nil
}

// InitConn performs PostgreSQL-specific connection initialization.
// Per-connection settings are applied through pgx RuntimeParams during open,
// so they affect every pooled connection.
func (d *PostgreSQLDialect) InitConn(db *sql.DB) error { return nil }

// SchemaFiles returns the schema files to execute during InitSchema.
// For PostgreSQL the full native schema is in schema_pg.sql.
func (d *PostgreSQLDialect) SchemaFiles() []string {
	return []string{"schema_pg.sql"}
}

// CheckpointWAL is a no-op for PostgreSQL (no WAL checkpoint needed).
func (d *PostgreSQLDialect) CheckpointWAL(db *sql.DB) error { return nil }

// SchemaStaleCheck returns the SQL to check whether migrations are needed.
// PostgreSQL uses information_schema instead of pragma_table_info.
func (d *PostgreSQLDialect) SchemaStaleCheck() string {
	return postgresColumnExistsSQL("conversations", "conversation_type")
}

// IsDuplicateColumnError returns true if the error is a "column already exists" error.
// PostgreSQL SQLSTATE 42701 = duplicate_column.
func (d *PostgreSQLDialect) IsDuplicateColumnError(err error) bool {
	return isPgError(err, "42701")
}

// IsConflictError returns true if the error is a unique constraint violation.
// PostgreSQL SQLSTATE 23505 = unique_violation.
func (d *PostgreSQLDialect) IsConflictError(err error) bool {
	return isPgError(err, "23505")
}

// IsNoSuchTableError returns true if the error indicates a missing table.
// PostgreSQL SQLSTATE 42P01 = undefined_table.
func (d *PostgreSQLDialect) IsNoSuchTableError(err error) bool {
	return isPgError(err, "42P01")
}

// IsNoSuchModuleError always returns false for PostgreSQL (no module concept).
func (d *PostgreSQLDialect) IsNoSuchModuleError(err error) bool { return false }

// IsReturningError always returns false for PostgreSQL (RETURNING always supported).
func (d *PostgreSQLDialect) IsReturningError(err error) bool { return false }

// IsBusyError reports whether err indicates the database is held by another
// connection. PostgreSQL surfaces this as SQLSTATE 55P03 (lock_not_available)
// for statement_timeout-triggered lock waits and 40P01 (deadlock_detected)
// for deadlocks; both mean "retry later.".
func (d *PostgreSQLDialect) IsBusyError(err error) bool {
	return isPgError(err, "55P03") || isPgError(err, "40P01")
}

// BeginExclusive opens a transaction on conn and locks every table the
// sync path writes to in EXCLUSIVE mode. SQLite's BEGIN EXCLUSIVE blocks
// all writers database-wide, so the PG counterpart must cover the full
// set of tables a sync touches — not just sync_runs — for callers like
// RemoveSourceSerialized to safely cascade-delete a source without
// racing a concurrent sync. EXCLUSIVE conflicts with the ROW EXCLUSIVE
// lock that INSERT/UPDATE/DELETE acquire; ACCESS SHARE (reads) is still
// permitted.
//
// The table list mirrors every INSERT/UPDATE/DELETE the sync/import
// pipeline emits (verified against internal/store/messages.go,
// internal/store/sync.go, internal/store/account_identities.go,
// internal/store/migrations.go, and internal/sync/*.go) plus the
// collection_sources / account_identities / applied_migrations rows
// reached by ON DELETE CASCADE when RemoveSourceSerialized deletes a
// source. collections is included so a concurrent collection rename
// cannot race the cascade.
func (d *PostgreSQLDialect) BeginExclusive(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx,
		"LOCK TABLE sync_runs, sources, conversations, conversation_participants, "+
			"messages, message_recipients, message_labels, message_bodies, message_raw, "+
			"attachments, labels, participants, participant_identifiers, reactions, "+
			"collections, collection_sources, account_identities, applied_migrations "+
			"IN EXCLUSIVE MODE",
	); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}

// BeginWriteSQL returns "BEGIN"; PostgreSQL relies on SelectForUpdate
// to row-lock the modified row inside the transaction.
func (d *PostgreSQLDialect) BeginWriteSQL() string { return "BEGIN" }

// SelectForUpdate returns " FOR UPDATE" so a SELECT inside a write
// transaction takes a row-level lock that serializes subsequent merges.
func (d *PostgreSQLDialect) SelectForUpdate() string { return " FOR UPDATE" }

// isPgError checks if err is a pgconn.PgError with the given SQLSTATE code.
func isPgError(err error, code string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == code
	}
	return false
}

func postgresColumnExistsSQL(tableName, columnName string) string {
	return fmt.Sprintf(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = '%s'
		  AND column_name = '%s'`, tableName, columnName)
}
