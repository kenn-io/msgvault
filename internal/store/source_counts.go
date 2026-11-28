package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SourceMessageCounts holds per-source message totals as account listings
// report them: live messages and messages retained after their source
// deleted them. Dedup-hidden rows count in neither.
type SourceMessageCounts struct {
	Live          int64
	SourceDeleted int64
}

// SourceMessageCountsContext returns the stored SourceMessageCounts of every
// source. Sources without a row have no counted messages (their counts are
// zero). Triggers on messages keep the rows current in the writing
// transaction, so the read never scans messages.
func (s *Store) SourceMessageCountsContext(ctx context.Context) (map[int64]SourceMessageCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, live_count, source_deleted_count FROM source_message_counts`)
	if err != nil {
		return nil, fmt.Errorf("read source message counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[int64]SourceMessageCounts)
	for rows.Next() {
		var sourceID int64
		var c SourceMessageCounts
		if err := rows.Scan(&sourceID, &c.Live, &c.SourceDeleted); err != nil {
			return nil, fmt.Errorf("scan source message counts: %w", err)
		}
		counts[sourceID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read source message counts: %w", err)
	}
	return counts, nil
}

// initSourceMessageCountsContext installs source_message_counts and its
// triggers in one short transaction, then recounts each source in its own
// transaction. A recount locks only that source's counts row, so writers to
// other sources never wait on it and no transaction spans the whole archive.
// Recounting is idempotent, so an interrupted run simply starts over.
func (s *Store) initSourceMessageCountsContext(ctx context.Context) error {
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		return s.installSourceMessageCounts(ctx, tx)
	}); err != nil {
		return err
	}
	sourceIDs, err := s.sourceIDsContext(ctx)
	if err != nil {
		return err
	}
	for _, sourceID := range sourceIDs {
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			return s.recountSourceMessagesTx(ctx, tx, sourceID)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sourceIDsContext(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sources ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list sources to count: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan source to count: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sources to count: %w", err)
	}
	return ids, nil
}

// recountSourceMessagesTx replaces one source's counts with a fresh count.
// The triggers are already installed, so concurrent writers add their
// changes to the same row. On PostgreSQL the row lock orders the two: a
// writer adds its changes to the row as it commits, so a writer that did so
// first has committed before the lock is granted and the count sees its rows,
// and a writer that commits later waits and adds its changes to the recounted
// value. SQLite serializes writers, so the count and the replacement already
// see one consistent archive.
func (s *Store) recountSourceMessagesTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_message_counts (source_id)
		SELECT id FROM sources WHERE id = ?
		ON CONFLICT (source_id) DO NOTHING`, sourceID); err != nil {
		return fmt.Errorf("create message counts for source %d: %w", sourceID, err)
	}
	lock := `SELECT source_id FROM source_message_counts WHERE source_id = ?`
	if s.IsPostgreSQL() {
		lock += ` FOR UPDATE`
	}
	var locked int64
	err := tx.QueryRowContext(ctx, lock, sourceID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		// The source was deleted after it was listed.
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock message counts for source %d: %w", sourceID, err)
	}
	var counts SourceMessageCounts
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT
			COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END), 0)
		FROM messages WHERE source_id = ?`,
		LiveMessagesWhere("", true), SourceDeletedMessagesWhere("")), sourceID,
	).Scan(&counts.Live, &counts.SourceDeleted); err != nil {
		return fmt.Errorf("count messages for source %d: %w", sourceID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE source_message_counts SET live_count = ?, source_deleted_count = ?
		WHERE source_id = ?`, counts.Live, counts.SourceDeleted, sourceID); err != nil {
		return fmt.Errorf("store message counts for source %d: %w", sourceID, err)
	}
	return nil
}

// installSourceMessageCounts creates source_message_counts and the triggers
// that keep it equal to a COUNT over messages using LiveMessagesWhere(.., true)
// and SourceDeletedMessagesWhere. Triggers cover every writer, including
// cascades from deleted sources and conversations, and run in the writer's
// transaction.
func (s *Store) installSourceMessageCounts(ctx context.Context, tx *loggedTx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS source_message_counts (
			source_id BIGINT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
			live_count BIGINT NOT NULL DEFAULT 0,
			source_deleted_count BIGINT NOT NULL DEFAULT 0
		)`,
	}
	if s.IsPostgreSQL() {
		statements = append(statements, postgresSourceMessageCountTriggers()...)
	} else {
		statements = append(statements, sqliteSourceMessageCountTriggers()...)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install source message counts: %w", err)
		}
	}
	return nil
}

// sqliteSourceMessageCountTriggers adjust one counts row per changed message.
// They use upserts, not INSERT OR IGNORE: SQLite lets the conflict clause of
// the statement that fires a trigger override OR IGNORE.
func sqliteSourceMessageCountTriggers() []string {
	const live = `(%[1]s.deleted_at IS NULL AND %[1]s.deleted_from_source_at IS NULL)`
	const sourceDeleted = `(%[1]s.deleted_at IS NULL AND %[1]s.deleted_from_source_at IS NOT NULL)`
	add := fmt.Sprintf(`INSERT INTO source_message_counts (source_id, live_count, source_deleted_count)
		SELECT NEW.source_id, `+live+`, `+sourceDeleted+` WHERE NEW.deleted_at IS NULL
		ON CONFLICT (source_id) DO UPDATE SET
			live_count = live_count + excluded.live_count,
			source_deleted_count = source_deleted_count + excluded.source_deleted_count;`, "NEW")
	subtract := fmt.Sprintf(`UPDATE source_message_counts SET
			live_count = live_count - `+live+`,
			source_deleted_count = source_deleted_count - `+sourceDeleted+`
		WHERE source_id = OLD.source_id AND OLD.deleted_at IS NULL;`, "OLD")
	return []string{
		`DROP TRIGGER IF EXISTS trg_source_message_counts_insert`,
		`CREATE TRIGGER trg_source_message_counts_insert
			AFTER INSERT ON messages FOR EACH ROW
			WHEN NEW.deleted_at IS NULL
			BEGIN ` + add + ` END`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_update`,
		`CREATE TRIGGER trg_source_message_counts_update
			AFTER UPDATE OF source_id, deleted_at, deleted_from_source_at ON messages FOR EACH ROW
			WHEN OLD.source_id IS NOT NEW.source_id
				OR (OLD.deleted_at IS NULL) IS NOT (NEW.deleted_at IS NULL)
				OR (OLD.deleted_from_source_at IS NULL) IS NOT (NEW.deleted_from_source_at IS NULL)
			BEGIN ` + subtract + add + ` END`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_delete`,
		`CREATE TRIGGER trg_source_message_counts_delete
			AFTER DELETE ON messages FOR EACH ROW
			WHEN OLD.deleted_at IS NULL
			BEGIN ` + subtract + ` END`,
	}
}

// postgresSourceMessageCountTriggers record changes in two steps. A trigger
// on each messages statement appends the statement's per-source changes to
// source_message_count_deltas. A deferred trigger then folds the
// transaction's deltas into source_message_counts once, as it commits.
//
// Updating the counts row from every statement instead would make a long
// import slower with each message: PostgreSQL keeps every version of a row a
// transaction updates, and each later update walks them all. Appending
// deltas keeps each statement's cost constant, and the counts row is locked
// only while the transaction commits.
func postgresSourceMessageCountTriggers() []string {
	const deltas = `SELECT source_id,
			(deleted_at IS NULL AND deleted_from_source_at IS NULL)::int AS live,
			(deleted_at IS NULL AND deleted_from_source_at IS NOT NULL)::int AS source_deleted
		FROM %s`
	const record = `INSERT INTO source_message_count_deltas (source_id, live_count, source_deleted_count)
		SELECT source_id, SUM(live), SUM(source_deleted) FROM (%s) d
		GROUP BY source_id
		HAVING SUM(live) <> 0 OR SUM(source_deleted) <> 0;`
	negated := `SELECT source_id, -live AS live, -source_deleted AS source_deleted FROM (` + fmt.Sprintf(deltas, "old_rows") + `) o`
	return []string{
		// No foreign key: a source delete must not wait on or cascade into
		// another transaction's pending deltas. The fold skips deleted sources.
		`CREATE TABLE IF NOT EXISTS source_message_count_deltas (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			xact XID8 NOT NULL DEFAULT pg_current_xact_id(),
			source_id BIGINT NOT NULL,
			live_count BIGINT NOT NULL,
			source_deleted_count BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_source_message_count_deltas_xact
			ON source_message_count_deltas (xact)`,
		`CREATE OR REPLACE FUNCTION source_message_counts_changed() RETURNS trigger AS $$
		 BEGIN
		     IF TG_OP = 'INSERT' THEN
		         ` + fmt.Sprintf(record, fmt.Sprintf(deltas, "new_rows")) + `
		     ELSIF TG_OP = 'UPDATE' THEN
		         ` + fmt.Sprintf(record, fmt.Sprintf(deltas, "new_rows")+` UNION ALL `+negated) + `
		     ELSE
		         ` + fmt.Sprintf(record, negated) + `
		     END IF;
		     RETURN NULL;
		 END;
		 $$ LANGUAGE plpgsql`,
		// The deferred trigger fires once per delta row at commit. The first
		// firing folds every delta of the transaction and records the highest
		// folded ID, so the remaining firings return at once. A delta added
		// after an early fold, such as under SET CONSTRAINTS ALL IMMEDIATE,
		// has a higher ID and is folded by its own firing.
		`CREATE OR REPLACE FUNCTION source_message_counts_fold() RETURNS trigger AS $$
		 BEGIN
		     IF NEW.id <= COALESCE(NULLIF(current_setting('msgvault.source_counts_folded', true), '')::bigint, 0) THEN
		         RETURN NULL;
		     END IF;
		     PERFORM set_config('msgvault.source_counts_folded',
		         (SELECT MAX(id) FROM source_message_count_deltas WHERE xact = pg_current_xact_id())::text, true);
		     WITH moved AS (
		         DELETE FROM source_message_count_deltas WHERE xact = pg_current_xact_id()
		         RETURNING source_id, live_count, source_deleted_count)
		     INSERT INTO source_message_counts AS c (source_id, live_count, source_deleted_count)
		     SELECT source_id, SUM(live_count), SUM(source_deleted_count) FROM moved
		     WHERE EXISTS (SELECT 1 FROM sources WHERE sources.id = moved.source_id)
		     GROUP BY source_id
		     HAVING SUM(live_count) <> 0 OR SUM(source_deleted_count) <> 0
		     ORDER BY source_id
		     ON CONFLICT (source_id) DO UPDATE SET
		         live_count = c.live_count + excluded.live_count,
		         source_deleted_count = c.source_deleted_count + excluded.source_deleted_count;
		     RETURN NULL;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_fold ON source_message_count_deltas`,
		`CREATE CONSTRAINT TRIGGER trg_source_message_counts_fold
		     AFTER INSERT ON source_message_count_deltas DEFERRABLE INITIALLY DEFERRED
		     FOR EACH ROW EXECUTE FUNCTION source_message_counts_fold()`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_insert ON messages`,
		`CREATE TRIGGER trg_source_message_counts_insert
		     AFTER INSERT ON messages REFERENCING NEW TABLE AS new_rows
		     FOR EACH STATEMENT EXECUTE FUNCTION source_message_counts_changed()`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_update ON messages`,
		`CREATE TRIGGER trg_source_message_counts_update
		     AFTER UPDATE ON messages REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
		     FOR EACH STATEMENT EXECUTE FUNCTION source_message_counts_changed()`,
		`DROP TRIGGER IF EXISTS trg_source_message_counts_delete ON messages`,
		`CREATE TRIGGER trg_source_message_counts_delete
		     AFTER DELETE ON messages REFERENCING OLD TABLE AS old_rows
		     FOR EACH STATEMENT EXECUTE FUNCTION source_message_counts_changed()`,
	}
}

// SourceNativeMessagesWhere restricts verification counts and samples to records
// in the source provider. Aliases and table names are trusted SQL.
func SourceNativeMessagesWhere(alias, sources string) string {
	return fmt.Sprintf("(COALESCE(%s.message_type, '') <> 'calendar_event' OR EXISTS (SELECT 1 FROM %s native_source WHERE native_source.id = %s.source_id AND native_source.source_type IN ('gcal', 'ics', 'message_import')))", alias, sources, alias)
}

// CountNativeMessagesForSource counts messages originating with the source provider.
func (s *Store) CountNativeMessagesForSource(sourceID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND %s AND %s`, LiveMessagesWhere("", true), SourceNativeMessagesWhere("messages", "sources")), sourceID).Scan(&count)
	return count, err
}
