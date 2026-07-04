package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

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

// maxFTSBodyChars bounds the message-body text fed to to_tsvector. PostgreSQL
// imposes a hard 1MB (1048575 bytes) limit on a single tsvector value and
// errors with SQLSTATE 54000 ("string is too long for tsvector") when a
// document exceeds it.
//
// IMPORTANT — this is a HEURISTIC, not a guarantee. PostgreSQL's tsvector limit
// is on the packed lexeme+position bytes, NOT on the character count of the
// input. A character cap therefore CANNOT bound the resulting tsvector size for
// adversarial or multibyte input: a body of ~600000 distinct 2-byte multibyte
// tokens packs ~1.2MB of lexeme bytes and still trips SQLSTATE 54000 (verified
// empirically). The 600000-char cap makes the error unlikely for typical
// (mostly-ASCII, repetitive) bodies, but does not make it impossible.
//
// A residual SQLSTATE 54000 is handled GRACEFULLY rather than wedging FTS:
//   - Sync path (FTSUpsert): ALL FIVE tsvector inputs (subject, body, from, to,
//     cc) are additionally byte-truncated in Go to maxFTSBodyBytes BEFORE
//     binding (see below) — the SQL LEFT char cap cannot bound multibyte input,
//     so byte-truncating every field (not just the body) is what keeps a
//     multibyte subject/recipient list from tripping the limit. Any UpsertFTS
//     error is warn-only at the call site — the message still persists with
//     search_fts left NULL.
//   - Backfill path (FTSBackfillBatchSQL): the body lives in the DB, so only
//     the LEFT char cap applies; backfillFTSRowByRow skips the offending row
//     (with a logged warning) and continues, so one pathological row never
//     aborts BackfillFTS or wedges later batches.
//
// SQLite's FTS5 has no such limit, so this cap is PostgreSQL-only.
const maxFTSBodyChars = 600000

// maxFTSBodyBytes is the BYTE bound applied to EACH tsvector input field
// (subject, body, from, to, cc) on the sync path (FTSUpsert) as defense-in-
// depth, in addition to the SQL LEFT char cap. It is
// well under PostgreSQL's 1MB (1048575-byte) tsvector limit: for the worst-case
// shape — a body of all-distinct multibyte tokens, where the packed tsvector
// (lexeme bytes + per-position overhead) is roughly the same order as the input
// byte length — bounding the input to 700000 bytes keeps the resulting tsvector
// under the limit with comfortable margin. (The empirical overflow was ~600000
// distinct 2-byte chars producing a 1.2MB tsvector; 700000 input bytes of that
// same density stays below 1MB.) Truncation is rune-safe (never splits a
// multibyte rune). A UTF-8-safe byte truncation is not cleanly available in SQL
// (no convert_to/convert_from boundary hack is attempted), so the backfill SQL
// path keeps the char cap and relies on the row-by-row skip for any residual.
const maxFTSBodyBytes = 700000

// truncateBytesRuneSafe returns s truncated to at most maxFTSBodyBytes bytes
// without splitting a multibyte UTF-8 rune. If s already fits it is returned
// unchanged.
func truncateBytesRuneSafe(s string) string {
	if len(s) <= maxFTSBodyBytes {
		return s
	}
	// Walk back from maxFTSBodyBytes to the start of the rune that straddles
	// the boundary so we never emit a partial rune.
	cut := maxFTSBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// FTSUpsert updates the tsvector column on messages for a single message.
// PostgreSQL stores the FTS index inline on `messages.search_fts`, so there
// is no separate virtual table — the operation is an UPDATE, not an INSERT.
//
// ALL FIVE tsvector inputs (subject, body, from, to, cc) are bounded twice on
// this sync path: byte-truncated to maxFTSBodyBytes in Go here (rune-safe,
// robust against multibyte input that the SQL char cap cannot bound) and
// additionally LEFT-capped to maxFTSBodyChars in SQL. The SQL LEFT char cap
// cannot bound multibyte input, so a multibyte subject/recipient list could
// otherwise still trip SQLSTATE 54000 and leave search_fts NULL — the Go
// byte-truncation closes that gap for every field, not just the body. A
// residual 54000 is still possible only for pathologically dense input;
// callers treat the returned error as warn-only on the sync path (search_fts
// stays NULL), so a bad input can never wedge FTS.
func (d *PostgreSQLDialect) FTSUpsert(q querier, doc FTSDoc) error {
	subject := truncateBytesRuneSafe(doc.Subject)
	body := truncateBytesRuneSafe(doc.Body)
	fromAddr := truncateBytesRuneSafe(doc.FromAddr)
	toAddrs := truncateBytesRuneSafe(doc.ToAddrs)
	ccAddrs := truncateBytesRuneSafe(doc.CcAddrs)
	charCap := strconv.Itoa(maxFTSBodyChars)
	_, err := q.Exec(
		`UPDATE messages SET search_fts =
			setweight(to_tsvector('simple', LEFT(COALESCE($2, ''), `+charCap+`)), 'A') ||
			setweight(to_tsvector('simple', LEFT(COALESCE($4, ''), `+charCap+`)), 'B') ||
			to_tsvector('simple', LEFT(COALESCE($3, ''), `+charCap+`)) ||
			to_tsvector('simple', LEFT(COALESCE($5, ''), `+charCap+`)) ||
			to_tsvector('simple', LEFT(COALESCE($6, ''), `+charCap+`))
		WHERE id = $1`,
		doc.MessageID, subject, body,
		fromAddr, toAddrs, ccAddrs,
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
	charCap := strconv.Itoa(maxFTSBodyChars)
	return `UPDATE messages m SET search_fts =
		setweight(to_tsvector('simple', LEFT(COALESCE(m.subject, ''), ` + charCap + `)), 'A') ||
		to_tsvector('simple', LEFT(COALESCE(src.body_text, ''), ` + charCap + `)) ||
		setweight(to_tsvector('simple', LEFT(COALESCE(
			CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
			     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
			END,
			(SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
			''
		), ` + charCap + `)), 'B') ||
		to_tsvector('simple', LEFT(COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''), ` + charCap + `)) ||
		to_tsvector('simple', LEFT(COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), ''), ` + charCap + `))
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
// Probes for the existence of any NULL search_fts row so an interrupted
// backfill that leaves a low-id row NULL (and later inserts continue normally)
// still flags the gap — the previous max-vs-max comparison missed that case.
//
// Uses EXISTS rather than COUNT(*): a GIN index on search_fts cannot serve an
// `IS NULL` predicate, so COUNT(*) was a full sequential scan of every message
// on each startup. EXISTS short-circuits at the first NULL row. The partial
// btree index idx_messages_search_fts_null (created by EnsureFTSIndex) makes
// even the false case index-served and self-pruning as backfill completes.
func (d *PostgreSQLDialect) FTSNeedsBackfill(db *sql.DB) bool {
	var exists bool
	if err := db.QueryRow(
		"SELECT EXISTS (SELECT 1 FROM messages WHERE search_fts IS NULL)",
	).Scan(&exists); err != nil {
		return false
	}
	return exists
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
//
// Runs on the querier so RebuildFTS can route it through the maintenance
// transaction: the full-table `UPDATE messages SET search_fts = NULL` here
// has the same cost as FTSClearSQL (which is already hatched), and the GIN
// rebuild over a populated table can likewise exceed the pool-wide 30s
// statement_timeout on a large archive (finding S1).
func (d *PostgreSQLDialect) FTSRebuildSchema(q querier) error {
	if _, err := q.Exec("DROP INDEX IF EXISTS messages_search_fts_idx"); err != nil {
		return fmt.Errorf("drop messages_search_fts_idx: %w", err)
	}
	if _, err := q.Exec("UPDATE messages SET search_fts = NULL"); err != nil {
		return fmt.Errorf("clear search_fts: %w", err)
	}
	if _, err := q.Exec(
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
		// FTS tsvector column for legacy PG databases created before FTS
		// support. Inline in schema_pg.sql's CREATE TABLE (a no-op on a
		// pre-existing table), so without this an upgraded DB never gets the
		// column and FTS stays unavailable. Its GIN index is created
		// separately by EnsureFTSIndex AFTER this migration runs. [cr2-10]
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS search_fts TSVECTOR`, "search_fts"},
		// embed_gen: per-message vector-embedding watermark. NULL default
		// means every legacy row reads as "needs embedding", which is
		// correct — the scan-and-fill worker (and backstop) will embed and
		// stamp them. No backfill.
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS embed_gen BIGINT`, "embed_gen"},
		// last_modified: row-level last-modified watermark, the embed
		// worker's optimistic-CAS token. Existing rows get the default
		// (CURRENT_TIMESTAMP at the time the column is added); the triggers
		// created by EnsureTriggers keep it current thereafter.
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS last_modified TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP`, "last_modified"},
	}
}

// EnsureFTSIndex creates the GIN index on messages.search_fts idempotently.
// It runs after LegacyColumnMigrations (which adds search_fts on legacy DBs),
// so the column is guaranteed present. The index is intentionally NOT in
// schema_pg.sql: that file is Exec'd as one statement before migrations, and
// a legacy table missing the column would fail the index there and roll back
// the entire schema apply (cr2-10).
func (d *PostgreSQLDialect) EnsureFTSIndex(q querier) error {
	if _, err := q.Exec(
		"CREATE INDEX IF NOT EXISTS messages_search_fts_idx ON messages USING GIN (search_fts)",
	); err != nil {
		return fmt.Errorf("create messages_search_fts_idx: %w", err)
	}
	// Partial btree index that serves the FTSNeedsBackfill probe (a GIN index
	// on search_fts cannot answer an IS NULL predicate). It only indexes the
	// rows still awaiting backfill, so it self-prunes to empty as backfill
	// completes and stays tiny thereafter.
	if _, err := q.Exec(
		"CREATE INDEX IF NOT EXISTS idx_messages_search_fts_null ON messages (id) WHERE search_fts IS NULL",
	); err != nil {
		return fmt.Errorf("create idx_messages_search_fts_null: %w", err)
	}
	return nil
}

// EnsureTriggers creates the last_modified maintenance triggers idempotently.
// Two triggers feed messages.last_modified:
//
//   - trg_messages_last_modified (BEFORE UPDATE on messages): sets
//     NEW.last_modified in-row. BEFORE → no secondary write → no recursion.
//     The WHEN guard (OLD.last_modified IS NOT DISTINCT FROM NEW.last_modified)
//     yields to an explicit last_modified write in the UPDATE rather than
//     overriding it, mirroring the SQLite trigger's guard.
//   - trg_message_bodies_last_modified (AFTER INSERT OR UPDATE on
//     message_bodies): bumps the parent message's last_modified so body
//     edits move the worker's CAS token too.
//
// CREATE TRIGGER is not idempotent before PG14, so each trigger is dropped
// (IF EXISTS) and recreated; the functions use CREATE OR REPLACE. Re-running
// InitSchema is therefore safe. Runs on the querier so InitSchema can route
// it through the maintenance transaction (consistent with EnsureFTSIndex).
func (d *PostgreSQLDialect) EnsureTriggers(q querier) error {
	stmts := []string{
		`CREATE OR REPLACE FUNCTION set_messages_last_modified() RETURNS trigger AS $$
		 BEGIN
		     NEW.last_modified := CURRENT_TIMESTAMP;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_messages_last_modified ON messages`,
		`CREATE TRIGGER trg_messages_last_modified
		     BEFORE UPDATE ON messages FOR EACH ROW
		     WHEN (OLD.last_modified IS NOT DISTINCT FROM NEW.last_modified)
		     EXECUTE FUNCTION set_messages_last_modified()`,
		`CREATE OR REPLACE FUNCTION bump_message_last_modified() RETURNS trigger AS $$
		 BEGIN
		     UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_last_modified ON message_bodies`,
		`CREATE TRIGGER trg_message_bodies_last_modified
		     AFTER INSERT OR UPDATE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION bump_message_last_modified()`,
	}
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure last_modified triggers: %w", err)
		}
	}
	return nil
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
	return postgresColumnExistsSQL("messages", "embed_gen")
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

// IsBusyError reports whether err indicates write contention that a bounded
// retry loop should treat as "retry later". The SQLSTATEs covered:
//
//   - 55P03 (lock_not_available): a NOWAIT request (or lock_timeout) could not
//     acquire a lock. We do not set lock_timeout here, so this fires for NOWAIT
//     callers; included for completeness.
//   - 40P01 (deadlock_detected): the deadlock detector aborted this transaction.
//   - 57014 (query_canceled): raised when statement_timeout fires. Under
//     contention a statement blocks on a lock until statement_timeout cancels
//     it, so 57014 is the common contention symptom on PostgreSQL. 57014 is
//     also raised by user/context cancellation; treating it as busy is
//     acceptable because every busy-retry loop here is bounded, so a genuine
//     cancel cannot spin indefinitely.
func (d *PostgreSQLDialect) IsBusyError(err error) bool {
	return isPgError(err, "55P03") || isPgError(err, "40P01") || isPgError(err, "57014")
}

// IsFTSValueTooLargeError reports whether err is PostgreSQL's
// program_limit_exceeded (SQLSTATE 54000), which to_tsvector raises as
// "string is too long for tsvector". This is the single FTS error the backfill
// may skip-and-continue on; all other errors abort.
func (d *PostgreSQLDialect) IsFTSValueTooLargeError(err error) bool {
	return isPgError(err, "54000")
}

// exclusiveLockTables is the table list BeginExclusive locks IN EXCLUSIVE
// MODE. It mirrors every INSERT/UPDATE/DELETE the sync/import pipeline emits
// (verified against internal/store/messages.go, internal/store/sync.go,
// internal/store/account_identities.go, internal/store/migrations.go, and
// internal/sync/*.go) PLUS every table reached by ON DELETE CASCADE when
// RemoveSourceSerialized deletes a source.
//
// Invariant: every table with an ON DELETE CASCADE foreign-key chain to
// sources(id) MUST appear here, otherwise the cascade DELETE can race a
// concurrent writer to that table and reopen the very race the EXCLUSIVE
// lock exists to close. TestExclusiveLockTablesCoverCascade pins this by
// diffing the catalog against this list.
//
//   - source_import_items: written by UpsertSourceImportItem (internal/store/sync.go)
//     and cascade-reachable from sources — a real race before it was added here.
//   - sync_checkpoints: cascade-reachable from sources; no writer today, but
//     included so a future checkpoint writer cannot race the cascade.
//   - imap_folder_state: written by UpsertIMAPFolderStates after IMAP syncs
//     and cascade-reachable from sources.
//
// collections is included (despite not being a direct sources cascade target)
// so a concurrent collection rename cannot race the collection_sources cascade.
var exclusiveLockTables = []string{
	"sync_runs", "sources", "conversations", "conversation_participants",
	"messages", "message_recipients", "message_labels", "message_bodies", "message_raw",
	"attachments", "labels", "participants", "participant_identifiers", "reactions",
	"collections", "collection_sources", "account_identities", "applied_migrations",
	"source_import_items", "sync_run_items", "sync_checkpoints",
	"imap_folder_state",
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
// The locked set lives in exclusiveLockTables. A SET LOCAL
// statement_timeout = 0 is issued first so a busy daemon's lock-wait
// (and the cascade DELETE / FTSDelete that RemoveSourceSerialized runs on
// this same connection afterwards) cannot be cancelled by the pool-wide
// 30s statement_timeout on a large archive (finding S1). SET LOCAL
// auto-resets at COMMIT/ROLLBACK, so it cannot leak to other pooled
// connections.
func (d *PostgreSQLDialect) BeginExclusive(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx,
		"LOCK TABLE "+strings.Join(exclusiveLockTables, ", ")+" IN EXCLUSIVE MODE",
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

// MaintenanceTimeoutResetSQL disables the per-statement timeout for the
// current transaction. SET LOCAL auto-resets at tx end, so the pool-wide
// statement_timeout cannot leak away on other connections.
func (d *PostgreSQLDialect) MaintenanceTimeoutResetSQL() string {
	return "SET LOCAL statement_timeout = 0"
}

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
