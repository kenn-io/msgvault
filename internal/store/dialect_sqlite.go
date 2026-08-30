package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// SQLiteTimestampLayout is the Go layout matching strftime('%Y-%m-%d %H:%M:%f').
const SQLiteTimestampLayout = "2006-01-02 15:04:05.000"

// SQLiteDialect implements Dialect for SQLite (the default backend).
//
// The zero value is ready to use, and every method except ReadWatermarkBounds
// is stateless — callers outside Store that only want Rebind or BuildFTSArg can
// go on constructing one per call. ReadWatermarkBounds remembers the newest
// instant it has proved the database had no write in flight, which the Store's
// single long-lived instance accumulates across pages.
type SQLiteDialect struct {
	quiescentMu sync.Mutex
	quiescentAt time.Time
}

func (d *SQLiteDialect) DriverName() string { return sqliteutil.DriverName() }

// Rebind is a no-op for SQLite — it uses ? placeholders natively.
func (d *SQLiteDialect) Rebind(query string) string { return query }

// Now returns the SQLite expression for the current UTC timestamp.
func (d *SQLiteDialect) Now() string { return "datetime('now')" }

// ContentChangedNow returns the SQLite expression that stamps
// content_changed_at at millisecond resolution. strftime's %f gives
// milliseconds as a floor on collision spacing, not a guarantee of
// distinctness, but it is the finest resolution SQLite's DATETIME text
// format supports, and the trigger's WHEN guard plus the (content_changed_at,
// id) cursor tolerate ties.
func (d *SQLiteDialect) ContentChangedNow() string {
	return `strftime('%Y-%m-%d %H:%M:%f','now')`
}

// TimestampParam formats t to match ContentChangedNow's textual format.
// SQLite's driver otherwise serialises time.Time with a "+00:00" suffix,
// which sorts BELOW an equal stored value under lexical comparison and
// would silently drop every row sharing the cursor's instant.
func (d *SQLiteDialect) TimestampParam(t time.Time) any {
	if t.IsZero() {
		return "" // sorts below every stored timestamp: "from the beginning"
	}
	return t.UTC().Format(SQLiteTimestampLayout)
}

// sqliteQuiescentProbeTimeout is how long ReadWatermarkBounds waits for the
// SQLite write lock before giving up on advancing the bound for this page. It
// is deliberately short: a page that waits is a consumer that waits, and the
// fallback (the newest instant the database was already proved quiescent at)
// costs only freshness, never correctness. SQLite write transactions are
// normally sub-millisecond, so 250ms times out only against a writer that is
// genuinely holding the lock — which is exactly the case where waiting longer
// would not have helped either.
//
// The probe is a WRITE-lock acquisition, so polling the feed costs the database
// writer throughput in a way an ordinary read does not: measured on one machine
// against three concurrent writers, eight clients paging the feed in a tight
// loop cut writes to 15% of the unloaded rate, where eight clients running an
// equivalent plain SELECT left 49%. One consumer polling once a second is free;
// a consumer that polls as fast as it can is competing with the importer for
// the write lock. Poll on an interval. Drain a backlog by asking for a bigger
// page, or by paging straight on from the last row's cursor while a full page
// keeps coming back — not by shortening the interval, which buys nothing but
// lock contention.
const sqliteQuiescentProbeTimeout = 250 * time.Millisecond

// ReadWatermarkBounds implements Dialect.
//
// SQLite has no pg_stat_activity: nothing exposes when another connection's
// write transaction began, or whether one is open at all. What it has instead
// is a single writer. Acquiring the write lock is therefore a proof rather than
// an observation — while this probe holds it, no other write transaction
// exists, so every content_changed_at stamp in the database has committed. The
// clock read inside that lock is a valid commit bound:
//
//   - A write that committed before the lock was acquired stamped itself
//     earlier still, so it is strictly below the reading (or equal to it, which
//     the page's strict `<` also excludes — a delay, not a loss).
//   - A write that starts after the probe releases the lock cannot be stamped
//     before it acquires the lock, which is after the reading.
//
// When the lock cannot be taken within sqliteQuiescentProbeTimeout, a writer is
// in flight and its start time is unknowable, so the bound falls back to the
// newest instant this dialect has already proved quiescent — the last probe
// that succeeded. That is always safe (any write in flight now began after it)
// and it is why the instant is remembered rather than recomputed: the fallback
// is the whole liveness story on SQLite. The feed then stops advancing until
// the writer finishes, and says so through the lag between CommitBound and Now.
// That lag is the age of the last successful probe, NOT the age of the writer
// in flight: probes run only when something reads the bound, so a writer that
// started a moment ago inherits the whole gap since the last quiet reading. It
// is an upper bound on the writer's age, which is the safe direction, but it is
// not a measurement of it — unlike PostgreSQL's, which reads xact_start.
//
// A fresh dialect that has never completed a probe reports the zero time, so
// the feed publishes nothing until it first sees the database idle. That is the
// honest answer — it has no evidence any stamp has committed — and it resolves
// on the first quiet moment.
//
// The probe holds the write lock for one clock read, and commits nothing, so it
// writes no WAL frames.
func (d *SQLiteDialect) ReadWatermarkBounds(
	ctx context.Context, db *sql.DB,
) (WatermarkBounds, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return WatermarkBounds{}, fmt.Errorf("read change-feed watermark bounds: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return d.proveQuiescentInstant(func() (time.Time, bool, time.Time, error) {
		quiescent, proved, err := d.probeQuiescentInstant(ctx, conn)
		if err != nil {
			return time.Time{}, false, time.Time{}, err
		}
		// A successful probe already read the clock, under the write lock; that
		// reading is this call's server_time as much as a bare SELECT would be,
		// and reusing it keeps Now and CommitBound from disagreeing by a
		// millisecond for no reason. Only a probe that timed out needs the clock
		// separately.
		now := quiescent
		if !proved {
			if now, err = d.readClock(ctx, conn); err != nil {
				return time.Time{}, false, time.Time{}, err
			}
		}
		return quiescent, proved, now, nil
	})
}

// proveQuiescentInstant runs one probe and folds its result into the remembered
// proof, and it holds quiescentMu across BOTH — which is the whole point.
//
// The probe proves its instant under the database's write lock, so concurrent
// pages prove instants in a definite order; but two of them can return from the
// probe in one order and reach this state in the other, storing an older proof
// over a newer one. Nothing unsafe follows — an instant proved quiescent stays
// proved — but the published complete_through would regress, which a consumer
// is entitled to never see. Taking this lock before the probe makes the order
// proofs are stored in the order they were proved in: the write lock and this
// mutex are then acquired in the same sequence.
//
// Serialising costs nothing real. The probe's own work is already serialised by
// the write lock it takes, so a caller that waits here is a caller that would
// have waited on the database instead — and waiting here does not hold a
// database lock while it does.
func (d *SQLiteDialect) proveQuiescentInstant(
	probe func() (quiescent time.Time, proved bool, now time.Time, err error),
) (WatermarkBounds, error) {
	d.quiescentMu.Lock()
	defer d.quiescentMu.Unlock()

	quiescent, proved, now, err := probe()
	if err != nil {
		return WatermarkBounds{}, err
	}
	if proved {
		// The MOST RECENT proof, not the greatest one. They differ only if the
		// database clock steps backwards, and there the greatest is the wrong
		// answer: it would stand above stamps taken after the step, which may
		// still be in flight.
		d.quiescentAt = quiescent
	}
	bound := d.quiescentAt
	if bound.After(now) {
		// Only reachable if the database clock stepped backwards between two
		// probes, which breaks the watermark itself and is outside what this
		// bound can repair (docs/api-server.md says so). Hold the published
		// invariant — CommitBound is never after Now — rather than emit a pair
		// that contradicts the contract on top of it.
		bound = now
	}
	return WatermarkBounds{Now: now, CommitBound: bound}, nil
}

// probeQuiescentInstant takes the SQLite write lock, reads the clock under it,
// and releases it. The second return is false when a writer held the lock for
// longer than the probe was willing to wait — not an error, just no new
// evidence.
func (d *SQLiteDialect) probeQuiescentInstant(
	ctx context.Context, conn *sql.Conn,
) (time.Time, bool, error) {
	restore, err := d.useProbeBusyTimeout(ctx, conn)
	if err != nil {
		return time.Time{}, false, err
	}
	defer restore()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if d.IsBusyError(err) {
			return time.Time{}, false, nil
		}
		if isSQLiteError(err, "readonly") {
			// A read-only handle can never take the write lock, so it can never
			// establish the bound — and it cannot assume there is no writer
			// either, because another process may hold the same file open for
			// writing. Say so instead of serving a feed that silently returns
			// nothing.
			return time.Time{}, false, fmt.Errorf(
				"read change-feed watermark bounds: the content-change feed needs a "+
					"writable database handle to establish how far writes have "+
					"committed: %w", err)
		}
		return time.Time{}, false, fmt.Errorf("read change-feed watermark bounds: %w", err)
	}

	stamp, err := d.readClock(ctx, conn)
	if err != nil {
		d.rollback(ctx, conn)
		return time.Time{}, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		d.rollback(ctx, conn)
		return time.Time{}, false, fmt.Errorf("release change-feed watermark probe: %w", err)
	}
	return stamp, true, nil
}

// useProbeBusyTimeout narrows this connection's busy timeout to the probe's,
// returning a function that puts the connection's own value back. The
// connection returns to the pool afterwards, so leaving the probe's timeout on
// it would silently shorten every unrelated statement that later borrows it.
func (d *SQLiteDialect) useProbeBusyTimeout(ctx context.Context, conn *sql.Conn) (func(), error) {
	var configured int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&configured); err != nil {
		return nil, fmt.Errorf("read busy timeout for change-feed watermark probe: %w", err)
	}
	set := func(c context.Context, ms int64) error {
		_, err := conn.ExecContext(c, fmt.Sprintf("PRAGMA busy_timeout = %d", ms))
		return err
	}
	// Narrow, never widen: a store configured to give up on a busy database
	// sooner than this means it, and the probe has a safe fallback either way.
	if err := set(ctx, min(configured, sqliteQuiescentProbeTimeout.Milliseconds())); err != nil {
		return nil, fmt.Errorf("set busy timeout for change-feed watermark probe: %w", err)
	}
	return func() {
		// WithoutCancel: the connection must be handed back with its own
		// timeout even when the caller's context has already expired.
		if err := set(context.WithoutCancel(ctx), configured); err != nil {
			// This is a connection-local setting, on a connection already in
			// hand, run on a context that cannot be cancelled — so it fails
			// only when the connection itself is no longer usable, and then the
			// statements that would inherit the probe's timeout cannot run on
			// it either. Retiring the connection here is possible (conn.Raw
			// returning driver.ErrBadConn discards one rather than pooling it),
			// but it closes the *sql.Conn the caller still holds and may still
			// use. So report rather than act: if the connection does see
			// further use, every statement that borrows it gives up on a busy
			// database after the probe's timeout rather than the configured
			// one, and a "database is locked" from an unrelated query is
			// otherwise unexplainable.
			slog.Warn("change-feed watermark probe could not restore the connection's busy timeout",
				slog.Int64("configured_ms", configured),
				slog.Int64("left_at_ms", min(configured, sqliteQuiescentProbeTimeout.Milliseconds())),
				slog.Any("error", err))
		}
	}, nil
}

// rollback releases a probe transaction that could not be committed. It runs on
// an uncancellable context so a cancelled request cannot return a connection to
// the pool with the write lock still held.
func (d *SQLiteDialect) rollback(ctx context.Context, conn *sql.Conn) {
	_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
}

// readClock reads the database clock in exactly the format the triggers stamp,
// so the reading and the watermarks it bounds are comparable.
func (d *SQLiteDialect) readClock(ctx context.Context, conn *sql.Conn) (time.Time, error) {
	var stamp nullableTimestamp
	if err := conn.QueryRowContext(ctx, "SELECT "+d.ContentChangedNow()).Scan(&stamp); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	if !stamp.Valid {
		return time.Time{}, errors.New("read database clock: no value returned")
	}
	return stamp.Time.UTC(), nil
}

// InsertOrIgnore is a no-op for SQLite — the syntax is native.
func (d *SQLiteDialect) InsertOrIgnore(sql string) string { return sql }

// BoolTrueExpr returns "col = 1" — SQLite stores booleans as 0/1 INTEGER.
func (d *SQLiteDialect) BoolTrueExpr(col string) string { return col + " = 1" }

// JSONBindExpr is "?" on SQLite — JSON columns are plain TEXT.
func (d *SQLiteDialect) JSONBindExpr() string { return "?" }

func (d *SQLiteDialect) JSONIsDistinctExpr(col string) string { return col + " IS NOT ?" }

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
// recipients) for typical email shapes. PostgreSQL assigns recipients
// weight C and body weight D so body-only search can distinguish them,
// then supplies explicit rank weights that keep C and D equivalent. This is a
// best-effort SQLite tuning, NOT a strict cross-backend parity guarantee.
//
// Weights are positional over every column declared in messages_fts —
// UNINDEXED columns count too even though they cannot match — so the
// leading 1.0 is the placeholder for `message_id UNINDEXED`. The
// remaining slots map to (subject, body, from_addr, to_addr, cc_addr).
// PostgreSQL applies setweight 'A'=1.0 to subject and 'B'=0.4 to sender,
// with explicit C/D rank weights of 0.1 for recipients/body — a 10:4:1 ratio,
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

func (d *SQLiteDialect) InvalidateFTSForMessage(q querier, messageID int64) error {
	_, err := q.Exec("DELETE FROM messages_fts WHERE rowid = ?", messageID)
	if d.IsNoSuchTableError(err) {
		// A missing FTS table cannot contain a stale searchable row. Preserve
		// the existing best-effort indexing contract so canonical message
		// persistence can continue and a later rebuild can recreate the index.
		return nil
	}
	return err
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
func (d *SQLiteDialect) FTSAvailable(ctx context.Context, db *sql.DB) (bool, error) {
	var probe int
	err := db.QueryRowContext(ctx, "SELECT 1 FROM messages_fts LIMIT 1").Scan(&probe)
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err == nil || errors.Is(err, sql.ErrNoRows), nil
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

// FTSNeedsBackfillQuick compares MAX(id) against MAX(rowid) — two B-tree
// lookups, instant at any archive size. It catches the dominant staleness
// (tail of the messages table not yet indexed: fresh import, interrupted
// backfill) but misses interior holes; FTSNeedsBackfill stays authoritative.
func (d *SQLiteDialect) FTSNeedsBackfillQuick(ctx context.Context, db *sql.DB) bool {
	var msgMax int64
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(id), 0) FROM messages",
	).Scan(&msgMax); err != nil || msgMax == 0 {
		return false
	}
	var ftsMax int64
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(rowid), 0) FROM messages_fts",
	).Scan(&ftsMax); err != nil {
		return false
	}
	return ftsMax < msgMax
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
func (d *SQLiteDialect) FTSRebuildSchema(ctx context.Context, q contextQuerier) error {
	if _, err := q.ExecContext(ctx, "DROP TABLE IF EXISTS messages_fts"); err != nil {
		return fmt.Errorf("drop messages_fts: %w", err)
	}
	schema, err := schemaFS.ReadFile("schema_sqlite.sql")
	if err != nil {
		return fmt.Errorf("read schema_sqlite.sql: %w", err)
	}
	if _, err := q.ExecContext(ctx, string(schema)); err != nil {
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

func (d *SQLiteDialect) ValidateMessageWatermarks(q querier) error {
	_, err := d.contentChangedAtDefaultStamps(q)
	return err
}

// EnsureTriggers creates the content_changed_at maintenance triggers and
// re-scopes the messages last_modified trigger (see lastModifiedUpdateOfColumns
// for why the latter cannot stay a blanket AFTER UPDATE in schema.sql).
//
// The message_bodies last_modified triggers ARE still schema.sql's: they write
// messages.last_modified directly rather than reacting to a messages UPDATE, so
// no trigger of ours can re-enter them.
//
// content_changed_at's triggers are built here because their column list comes
// from MessagesContentColumns, shared with the PostgreSQL dialect so the two
// backends cannot drift, and because DROP + CREATE can replace a definition on
// an existing archive where CREATE TRIGGER IF NOT EXISTS silently would not.
// The same DROP + CREATE is what lets the re-scoped last_modified trigger reach
// an archive that already carries schema.sql's older, blanket definition.
func (d *SQLiteDialect) EnsureTriggers(q querier) error {
	cols := contentChangedTriggerColumnList()
	guard := contentChangedValueGuard("IS NOT")
	now := d.ContentChangedNow()
	insertStampedByDefault, err := d.contentChangedAtDefaultStamps(q)
	if err != nil {
		return err
	}
	lastModifiedCols, err := d.lastModifiedUpdateOfColumns(q)
	if err != nil {
		return err
	}
	stmts := []string{
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_ins`,
	}
	if !insertStampedByDefault {
		// Every new row gets a watermark. On a database upgraded by ALTER TABLE
		// ADD COLUMN this trigger is the only writer, because SQLite forbids a
		// non-constant DEFAULT there. A fresh database has the DEFAULT
		// (schema.sql) instead and this trigger is NOT created at all: SQLite
		// triggers cannot assign to NEW, so the stamp has to be a second
		// UPDATE of the row just inserted, and merely HAVING a row trigger on
		// messages makes SQLite compile a trigger subprogram into every INSERT
		// and open a statement journal for it whether or not the body runs.
		// That cost lands on inserts that SHARE a transaction, and only on
		// those. Measured on this project's schema with the trigger present and
		// its WHEN guard never once satisfied (verified: the body never ran),
		// Linux, WAL, synchronous=FULL and again at synchronous=OFF with the
		// same result: 100k rows inserted inside ONE transaction took 7.3s with
		// the trigger against 1.4s without, while 20k messages persisted one
		// transaction each (PersistMessage, the per-message path) showed no
		// measurable difference at all -- 12.9s with against 13.2s without.
		// The paths that pay are therefore the bulk ones: internal/fakevault's
		// INSERT ... SELECT generator and subset.go's message copy. The WHEN
		// guard yields to an explicit write in the INSERT rather than
		// clobbering it.
		stmts = append(stmts, fmt.Sprintf(`CREATE TRIGGER trg_messages_content_changed_ins
		    AFTER INSERT ON messages FOR EACH ROW
		    WHEN NEW.content_changed_at IS NULL
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.id;
		    END`, now))
	}
	stmts = append(stmts,
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_at`,
		// UPDATE OF scopes to the columns the statement names; the value guard
		// then requires one of them to have actually changed. Recursion is
		// impossible: the trigger's own UPDATE touches only content_changed_at,
		// which is not in the column list. The IS guard is null-safe -- with
		// `=`, a NULL watermark is never stamped (measured).
		fmt.Sprintf(`CREATE TRIGGER trg_messages_content_changed_at
		    AFTER UPDATE OF %s ON messages FOR EACH ROW
		    WHEN OLD.content_changed_at IS NEW.content_changed_at AND %s
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.id;
		    END`, cols, guard, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_ins`,
		fmt.Sprintf(`CREATE TRIGGER trg_message_bodies_content_changed_ins
		    AFTER INSERT ON message_bodies FOR EACH ROW
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.message_id;
		    END`, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_upd`,
		// Value-guarded like the messages trigger: upsertMessageBody always
		// runs its ON CONFLICT DO UPDATE, even when messageBodyChanges reports
		// nothing changed, and PersistMessage calls it for every persisted
		// message. Unguarded, every resync would bump.
		fmt.Sprintf(`CREATE TRIGGER trg_message_bodies_content_changed_upd
		    AFTER UPDATE ON message_bodies FOR EACH ROW
		    WHEN OLD.body_text IS NOT NEW.body_text OR OLD.body_html IS NOT NEW.body_html
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = NEW.message_id;
		    END`, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_del`,
		// Deletion is a content change: without the bump, a body deleted
		// between a visual context snapshot and its claim passes the
		// content-stamp CAS and publishes an embedding of the deleted text.
		fmt.Sprintf(`CREATE TRIGGER trg_message_bodies_content_changed_del
		    AFTER DELETE ON message_bodies FOR EACH ROW
		    BEGIN
		        UPDATE messages SET content_changed_at = %s WHERE id = OLD.message_id;
		    END`, now),
		`DROP TRIGGER IF EXISTS trg_messages_last_modified`,
		// Identical to schema.sql's definition except for the UPDATE OF scope,
		// which is what keeps the stamp above from re-entering it.
		fmt.Sprintf(`CREATE TRIGGER trg_messages_last_modified
		    AFTER UPDATE OF %s ON messages FOR EACH ROW
		    WHEN OLD.last_modified = NEW.last_modified
		    BEGIN
		        UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
		    END`, lastModifiedCols),
		`DROP TRIGGER IF EXISTS trg_embedding_changes_message_insert`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_message_update`,
		`CREATE TRIGGER trg_embedding_changes_message_update
		    AFTER UPDATE OF message_type, conversation_id, subject, sent_at, received_at, internal_date, sender_id, deleted_at, deleted_from_source_at, embed_gen
		    ON messages FOR EACH ROW
		    WHEN (EXISTS (
		        SELECT 1 FROM message_bodies WHERE message_id = NEW.id
		    ) AND (
		        (
		            (
		                (OLD.message_type IN ('beeper', 'meeting_transcript') AND OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                OR (NEW.message_type IN ('beeper', 'meeting_transcript') AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		                OR (
		                    OLD.message_type IS NOT NEW.message_type
		                    AND (
		                        (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                        OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		                    )
		                )
		            ) AND (
		                OLD.message_type IS NOT NEW.message_type
		                OR OLD.conversation_id IS NOT NEW.conversation_id
		                OR OLD.sent_at IS NOT NEW.sent_at
		                OR OLD.received_at IS NOT NEW.received_at
		                OR OLD.internal_date IS NOT NEW.internal_date
		                OR OLD.sender_id IS NOT NEW.sender_id
		                OR OLD.deleted_at IS NOT NEW.deleted_at
		                OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at
		            )
		        ) OR (
		            COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		            AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		            AND (
		                (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		            ) AND (
		                OLD.message_type IS NOT NEW.message_type
		                OR OLD.conversation_id IS NOT NEW.conversation_id
		                OR OLD.subject IS NOT NEW.subject
		                OR OLD.deleted_at IS NOT NEW.deleted_at
		                OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at
		            )
		        )
		    )) OR (
		        OLD.message_type IS NOT NEW.message_type
		        AND (
		            (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		            OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		        )
		    ) OR (
		        COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		        AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		        AND (
		            OLD.deleted_at IS NOT NEW.deleted_at
		            OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at
		        )
		    ) OR (
		        OLD.embed_gen IS NOT NULL
		        AND NEW.embed_gen IS NULL
		        AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		        AND NEW.deleted_at IS NULL
		        AND NEW.deleted_from_source_at IS NULL
		        AND NOT EXISTS (SELECT 1 FROM message_bodies WHERE message_id = NEW.id)
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'message_update', NEW.id,
		               CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                    THEN OLD.message_type END,
		               CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                    THEN NEW.message_type END,
		               CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                    THEN OLD.conversation_id END,
		               CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                    THEN NEW.conversation_id END,
		               CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                    THEN strftime('%Y-%m-%d %H:%M:%f', COALESCE(OLD.sent_at, OLD.received_at, OLD.internal_date)) END,
		               CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                    THEN strftime('%Y-%m-%d %H:%M:%f', COALESCE(NEW.sent_at, NEW.received_at, NEW.internal_date)) END,
		               NULL
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_message_delete`,
		`CREATE TRIGGER trg_embedding_changes_message_delete
		    BEFORE DELETE ON messages FOR EACH ROW
		    WHEN OLD.deleted_at IS NULL
		         AND OLD.deleted_from_source_at IS NULL
		         AND (
		             COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		             OR EXISTS (SELECT 1 FROM message_bodies WHERE message_id = OLD.id)
		         )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'message_delete', OLD.id,
		               OLD.message_type, NULL,
		               OLD.conversation_id, NULL,
		               strftime('%Y-%m-%d %H:%M:%f', COALESCE(OLD.sent_at, OLD.received_at, OLD.internal_date)), NULL, NULL
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_body_insert`,
		`CREATE TRIGGER trg_embedding_changes_body_insert
		    AFTER INSERT ON message_bodies FOR EACH ROW
		    WHEN EXISTS (
		        SELECT 1 FROM messages
		         WHERE id = NEW.message_id
		           AND deleted_at IS NULL
		           AND deleted_from_source_at IS NULL
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT c.sequence, 'message_insert', m.id,
		               NULL, m.message_type,
		               NULL, m.conversation_id,
		               NULL,
		               strftime('%Y-%m-%d %H:%M:%f', COALESCE(m.sent_at, m.received_at, m.internal_date)), NULL
		          FROM embedding_change_clock c
		          JOIN messages m ON m.id = NEW.message_id
		         WHERE c.singleton = 1 AND c.enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_body_update`,
		`CREATE TRIGGER trg_embedding_changes_body_update
		    AFTER UPDATE ON message_bodies FOR EACH ROW
		    WHEN (OLD.body_text IS NOT NEW.body_text OR OLD.body_html IS NOT NEW.body_html)
		         AND EXISTS (
		             SELECT 1 FROM messages
		              WHERE id = NEW.message_id
		                AND deleted_at IS NULL
		                AND deleted_from_source_at IS NULL
		         )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT c.sequence, 'message_body', m.id,
		               m.message_type, m.message_type,
		               m.conversation_id, m.conversation_id,
		               strftime('%Y-%m-%d %H:%M:%f', COALESCE(m.sent_at, m.received_at, m.internal_date)),
		               strftime('%Y-%m-%d %H:%M:%f', COALESCE(m.sent_at, m.received_at, m.internal_date)), NULL
		          FROM embedding_change_clock c
		          JOIN messages m ON m.id = NEW.message_id
		         WHERE c.singleton = 1 AND c.enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_body_delete`,
		`CREATE TRIGGER trg_embedding_changes_body_delete
		    AFTER DELETE ON message_bodies FOR EACH ROW
		    WHEN EXISTS (
		        SELECT 1 FROM messages
		         WHERE id = OLD.message_id
		           AND deleted_at IS NULL
		           AND deleted_from_source_at IS NULL
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT c.sequence, 'message_body', m.id,
		               m.message_type, NULL,
		               m.conversation_id, NULL,
		               strftime('%Y-%m-%d %H:%M:%f', COALESCE(m.sent_at, m.received_at, m.internal_date)),
		               NULL, NULL
		          FROM embedding_change_clock c
		          JOIN messages m ON m.id = OLD.message_id
		         WHERE c.singleton = 1 AND c.enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_conversation_title`,
		`CREATE TRIGGER trg_embedding_changes_conversation_title
		    AFTER UPDATE OF title ON conversations FOR EACH ROW
		    WHEN OLD.title IS NOT NEW.title
		         AND EXISTS (
		             SELECT 1 FROM messages m
		             JOIN message_bodies mb ON mb.message_id = m.id
		              WHERE m.conversation_id = NEW.id
		                AND m.message_type = 'beeper'
		                AND m.deleted_at IS NULL
		                AND m.deleted_from_source_at IS NULL
		         )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'conversation_title', NULL, NULL, NULL,
		               OLD.id, NEW.id, NULL, NULL, NULL
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_membership_insert`,
		`CREATE TRIGGER trg_embedding_changes_membership_insert
		    AFTER INSERT ON conversation_participants FOR EACH ROW
		    WHEN EXISTS (
		        SELECT 1 FROM messages m
		        JOIN message_bodies mb ON mb.message_id = m.id
		         WHERE m.conversation_id = NEW.conversation_id
		           AND m.message_type = 'beeper'
		           AND m.deleted_at IS NULL
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'conversation_participant', NULL, NULL, NULL,
		               NEW.conversation_id, NEW.conversation_id,
		               NULL, NULL, NEW.participant_id
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_membership_update`,
		`CREATE TRIGGER trg_embedding_changes_membership_update
		    AFTER UPDATE ON conversation_participants FOR EACH ROW
		    WHEN (
		        OLD.conversation_id IS NOT NEW.conversation_id
		        OR OLD.participant_id IS NOT NEW.participant_id
		        OR OLD.role IS NOT NEW.role
		        OR OLD.joined_at IS NOT NEW.joined_at
		        OR OLD.left_at IS NOT NEW.left_at
		    ) AND (
		        EXISTS (
		            SELECT 1 FROM messages m
		            JOIN message_bodies mb ON mb.message_id = m.id
		             WHERE m.conversation_id = OLD.conversation_id
		               AND m.message_type = 'beeper'
		               AND m.deleted_at IS NULL
		        ) OR EXISTS (
		            SELECT 1 FROM messages m
		            JOIN message_bodies mb ON mb.message_id = m.id
		             WHERE m.conversation_id = NEW.conversation_id
		               AND m.message_type = 'beeper'
		               AND m.deleted_at IS NULL
		        )
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'conversation_participant', NULL, NULL, NULL,
		               OLD.conversation_id, NEW.conversation_id,
		               NULL, NULL, NEW.participant_id
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_membership_delete`,
		`CREATE TRIGGER trg_embedding_changes_membership_delete
		    AFTER DELETE ON conversation_participants FOR EACH ROW
		    WHEN EXISTS (
		        SELECT 1 FROM messages m
		        JOIN message_bodies mb ON mb.message_id = m.id
		         WHERE m.conversation_id = OLD.conversation_id
		           AND m.message_type = 'beeper'
		           AND m.deleted_at IS NULL
		    )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'conversation_participant', NULL, NULL, NULL,
		               OLD.conversation_id, OLD.conversation_id,
		               NULL, NULL, OLD.participant_id
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_participant_display_name`,
		`CREATE TRIGGER trg_embedding_changes_participant_display_name
		    AFTER UPDATE OF display_name, email_address, phone_number ON participants FOR EACH ROW
		    WHEN COALESCE(NULLIF(TRIM(OLD.display_name), ''),
		                  NULLIF(OLD.email_address, ''), NULLIF(OLD.phone_number, ''), '')
		         IS NOT COALESCE(NULLIF(TRIM(NEW.display_name), ''),
		                        NULLIF(NEW.email_address, ''), NULLIF(NEW.phone_number, ''), '')
		         AND (
		             EXISTS (
		                 SELECT 1
		                   FROM conversation_participants cp
		                   JOIN messages m ON m.conversation_id = cp.conversation_id
		                   JOIN message_bodies mb ON mb.message_id = m.id
		                  WHERE cp.participant_id = NEW.id
		                    AND m.message_type = 'beeper'
		                    AND m.deleted_at IS NULL
		             ) OR EXISTS (
		                 SELECT 1
		                   FROM messages m
		                   JOIN message_bodies mb ON mb.message_id = m.id
		                  WHERE m.sender_id = NEW.id
		                    AND m.message_type = 'beeper'
		                    AND m.deleted_at IS NULL
		             )
		         )
		    BEGIN
		        UPDATE embedding_change_clock
		           SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE;
		        INSERT INTO embedding_changes (
		            sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		            new_conversation_id, old_sent_at, new_sent_at, participant_id
		        )
		        SELECT sequence, 'participant_display_name', NULL, NULL, NULL,
		               NULL, NULL, NULL, NULL, NEW.id
		          FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE;
		    END`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_insert`,
		`CREATE TRIGGER trg_attachment_change_insert
		    AFTER INSERT ON attachments FOR EACH ROW
		    WHEN EXISTS (SELECT 1 FROM attachment_change_consumers)
		    BEGIN
		        INSERT INTO attachment_change_log
		            (event_kind, new_message_id, new_attachment_id,
		             new_content_hash, new_source_part_key, new_role)
		        VALUES ('attachment_insert', NEW.message_id, NEW.id,
		                NEW.content_hash, NEW.source_part_key, NEW.attachment_role);
		    END`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_update`,
		`CREATE TRIGGER trg_attachment_change_update
		    AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		        storage_path, media_type, width, height, duration_ms,
		        source_attachment_id, attachment_metadata, attachment_role,
		        role_source, source_part_key, content_id, encryption_version
		    ON attachments FOR EACH ROW
		    WHEN EXISTS (SELECT 1 FROM attachment_change_consumers)
		      AND (OLD.message_id IS NOT NEW.message_id
		        OR OLD.filename IS NOT NEW.filename
		        OR OLD.mime_type IS NOT NEW.mime_type
		        OR OLD.size IS NOT NEW.size
		        OR OLD.content_hash IS NOT NEW.content_hash
		        OR OLD.storage_path IS NOT NEW.storage_path
		        OR OLD.media_type IS NOT NEW.media_type
		        OR OLD.width IS NOT NEW.width
		        OR OLD.height IS NOT NEW.height
		        OR OLD.duration_ms IS NOT NEW.duration_ms
		        OR OLD.source_attachment_id IS NOT NEW.source_attachment_id
		        OR OLD.attachment_metadata IS NOT NEW.attachment_metadata
		        OR OLD.attachment_role IS NOT NEW.attachment_role
		        OR OLD.role_source IS NOT NEW.role_source
		        OR OLD.source_part_key IS NOT NEW.source_part_key
		        OR OLD.content_id IS NOT NEW.content_id
		        OR OLD.encryption_version IS NOT NEW.encryption_version)
		    BEGIN
		        INSERT INTO attachment_change_log
		            (event_kind, old_message_id, new_message_id,
		             old_attachment_id, new_attachment_id,
		             old_content_hash, new_content_hash,
		             old_source_part_key, new_source_part_key,
		             old_role, new_role)
		        VALUES ('attachment_update', OLD.message_id, NEW.message_id,
		                OLD.id, NEW.id, OLD.content_hash, NEW.content_hash,
		                OLD.source_part_key, NEW.source_part_key,
		                OLD.attachment_role, NEW.attachment_role);
		    END`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_delete`,
		`CREATE TRIGGER trg_attachment_change_delete
		    AFTER DELETE ON attachments FOR EACH ROW
		    WHEN EXISTS (SELECT 1 FROM attachment_change_consumers)
		    BEGIN
		        INSERT INTO attachment_change_log
		            (event_kind, old_message_id, old_attachment_id,
		             old_content_hash, old_source_part_key, old_role)
		        VALUES ('attachment_delete', OLD.message_id, OLD.id,
		                OLD.content_hash, OLD.source_part_key, OLD.attachment_role);
		    END`,
		`DROP TRIGGER IF EXISTS trg_attachment_message_live_change`,
		`CREATE TRIGGER trg_attachment_message_live_change
		    AFTER UPDATE OF deleted_at, deleted_from_source_at ON messages FOR EACH ROW
		    WHEN EXISTS (SELECT 1 FROM attachment_change_consumers)
		      AND ((OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		           IS NOT
		           (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL))
		    BEGIN
		        INSERT INTO attachment_change_log
		            (event_kind, old_message_id, new_message_id,
		             old_attachment_id, new_attachment_id,
		             old_content_hash, new_content_hash,
		             old_source_part_key, new_source_part_key,
		             old_role, new_role)
		        SELECT
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN 'message_live_enter' ELSE 'message_live_exit' END,
		            CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                 THEN OLD.id END,
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN NEW.id END,
		            CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                 THEN a.id END,
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN a.id END,
		            CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                 THEN a.content_hash END,
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN a.content_hash END,
		            CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                 THEN a.source_part_key END,
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN a.source_part_key END,
		            CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                 THEN a.attachment_role END,
		            CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN a.attachment_role END
		        FROM attachments a WHERE a.message_id = NEW.id;
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_insert`,
		`CREATE TRIGGER trg_visual_publication_attachment_insert
		    AFTER INSERT ON attachments FOR EACH ROW
		    WHEN NEW.attachment_role = 'standalone'
		      AND NEW.role_source IN ('mime_disposition', 'provider_explicit',
		                              'importer_semantics', 'raw_mime_repair')
		      AND EXISTS (SELECT 1 FROM messages m WHERE m.id = NEW.message_id
		                  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = NEW.message_id
		          AND (blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		               OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                   AND LOWER(NEW.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash))
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = 'stale', pending_vector_token = NULL, updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = NEW.message_id
		          AND (blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		               OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                   AND LOWER(NEW.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash));
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_update`,
		`CREATE TRIGGER trg_visual_publication_attachment_update
		    AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		        storage_path, media_type, width, height, duration_ms,
		        source_attachment_id, attachment_metadata, attachment_role,
		        role_source, source_part_key, content_id, encryption_version
		    ON attachments FOR EACH ROW
		    WHEN OLD.message_id IS NOT NEW.message_id
		      OR OLD.filename IS NOT NEW.filename
		      OR OLD.mime_type IS NOT NEW.mime_type
		      OR OLD.size IS NOT NEW.size
		      OR OLD.content_hash IS NOT NEW.content_hash
		      OR OLD.storage_path IS NOT NEW.storage_path
		      OR OLD.media_type IS NOT NEW.media_type
		      OR OLD.width IS NOT NEW.width
		      OR OLD.height IS NOT NEW.height
		      OR OLD.duration_ms IS NOT NEW.duration_ms
		      OR OLD.source_attachment_id IS NOT NEW.source_attachment_id
		      OR OLD.attachment_metadata IS NOT NEW.attachment_metadata
		      OR OLD.attachment_role IS NOT NEW.attachment_role
		      OR OLD.role_source IS NOT NEW.role_source
		      OR OLD.source_part_key IS NOT NEW.source_part_key
		      OR OLD.content_id IS NOT NEW.content_id
		      OR OLD.encryption_version IS NOT NEW.encryption_version
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL
		          AND message_id = OLD.message_id
		          AND (blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		               OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                   AND LOWER(OLD.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash))
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN EXISTS (
		                SELECT 1 FROM attachments a
		                JOIN messages m ON m.id = a.message_id
		                WHERE a.message_id = visual_publications.message_id
		                  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		                  AND a.attachment_role = 'standalone'
		                  AND a.role_source IN ('mime_disposition', 'provider_explicit',
		                                        'importer_semantics', 'raw_mime_repair')
		                  AND (LOWER(COALESCE(a.content_hash, '')) = visual_publications.blob_hash
		                       OR ((a.content_hash IS NULL OR a.content_hash = '')
		                           AND LOWER(a.storage_path) =
		                               SUBSTR(visual_publications.blob_hash, 1, 2) || '/' ||
		                               visual_publications.blob_hash))
		            ) THEN 'stale' ELSE 'tombstoned' END,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = OLD.message_id
		          AND (blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		               OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                   AND LOWER(OLD.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash));

		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND NEW.attachment_role = 'standalone'
		          AND NEW.role_source IN ('mime_disposition', 'provider_explicit',
		                                  'importer_semantics', 'raw_mime_repair')
		          AND message_id = NEW.message_id
		          AND EXISTS (SELECT 1 FROM messages m WHERE m.id = NEW.message_id
		                      AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)
		          AND (blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		               OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                   AND LOWER(NEW.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash))
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = 'stale', pending_vector_token = NULL, updated_at = CURRENT_TIMESTAMP
		        WHERE NEW.attachment_role = 'standalone'
		          AND NEW.role_source IN ('mime_disposition', 'provider_explicit',
		                                  'importer_semantics', 'raw_mime_repair')
		          AND message_id = NEW.message_id
		          AND EXISTS (SELECT 1 FROM messages m WHERE m.id = NEW.message_id
		                      AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)
		          AND (blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		               OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                   AND LOWER(NEW.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash));
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_delete`,
		`CREATE TRIGGER trg_visual_publication_attachment_delete
		    AFTER DELETE ON attachments FOR EACH ROW
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL
		          AND message_id = OLD.message_id
		          AND (blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		               OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                   AND LOWER(OLD.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash))
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN EXISTS (
		                SELECT 1 FROM attachments a
		                JOIN messages m ON m.id = a.message_id
		                WHERE a.message_id = visual_publications.message_id
		                  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		                  AND a.attachment_role = 'standalone'
		                  AND a.role_source IN ('mime_disposition', 'provider_explicit',
		                                        'importer_semantics', 'raw_mime_repair')
		                  AND (LOWER(COALESCE(a.content_hash, '')) = visual_publications.blob_hash
		                       OR ((a.content_hash IS NULL OR a.content_hash = '')
		                           AND LOWER(a.storage_path) =
		                               SUBSTR(visual_publications.blob_hash, 1, 2) || '/' ||
		                               visual_publications.blob_hash))
		            ) THEN 'stale' ELSE 'tombstoned' END,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = OLD.message_id
		          AND (blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		               OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                   AND LOWER(OLD.storage_path) =
		                       SUBSTR(blob_hash, 1, 2) || '/' || blob_hash));
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_delete_ledger`,
		`CREATE TRIGGER trg_visual_publication_delete_ledger
		    BEFORE DELETE ON visual_publications FOR EACH ROW
		    WHEN OLD.current_vector_token IS NOT NULL OR OLD.pending_vector_token IS NOT NULL
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT OLD.generation_id, OLD.current_vector_token
		        WHERE OLD.current_vector_token IS NOT NULL
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT OLD.generation_id, OLD.pending_vector_token
		        WHERE OLD.pending_vector_token IS NOT NULL
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_live_change`,
		`CREATE TRIGGER trg_visual_publication_message_live_change
		    AFTER UPDATE OF deleted_at, deleted_from_source_at ON messages FOR EACH ROW
		    WHEN ((OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		          IS NOT
		          (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL))
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = NEW.id
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN NEW.deleted_at IS NULL
		                              AND NEW.deleted_from_source_at IS NULL
		                         THEN 'stale' ELSE 'tombstoned' END,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = NEW.id;
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_content_change`,
		`CREATE TRIGGER trg_visual_publication_message_content_change
		    AFTER UPDATE OF subject, message_type ON messages FOR EACH ROW
		    WHEN OLD.subject IS NOT NEW.subject OR OLD.message_type IS NOT NEW.message_type
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = NEW.id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL)
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		            prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = NEW.id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_type_scope`,
		// Seed only canonical owners, mirroring canonicalVisualBlobHash: a
		// 64-hex content hash, or (when the hash is absent) a strict CAS
		// storage path. Hashless URLs or malformed legacy values would
		// otherwise create owners validateVisualOwner rejects, wedging every
		// tombstone sweep.
		fmt.Sprintf(`CREATE TRIGGER trg_visual_publication_message_type_scope
		    AFTER UPDATE OF message_type ON messages FOR EACH ROW
		    WHEN OLD.message_type IS NOT NEW.message_type
		    BEGIN
		        INSERT INTO visual_publications
		            (generation_id, message_id, blob_hash, media_input_key,
		             source_fence, attachment_role, role_source, state)
		        SELECT vg.id, a.message_id,
		               CASE WHEN LOWER(COALESCE(a.content_hash, '')) GLOB '%[1]s'
		                    THEN LOWER(a.content_hash)
		                    ELSE SUBSTR(a.storage_path, 4) END,
		               'original', 0, 'unknown', 'unknown', 'stale'
		        FROM attachments a JOIN visual_generations vg
		             ON vg.state IN ('building', 'active')
		        WHERE a.message_id = NEW.id
		          AND a.attachment_role = 'standalone'
		          AND a.role_source IN ('mime_disposition', 'provider_explicit',
		                                'importer_semantics', 'raw_mime_repair')
		          AND (LOWER(COALESCE(a.content_hash, '')) GLOB '%[1]s'
		               OR (COALESCE(a.content_hash, '') = ''
		                   AND COALESCE(a.storage_path, '') GLOB '[0-9a-f][0-9a-f]/%[1]s'
		                   AND SUBSTR(a.storage_path, 1, 2) = SUBSTR(a.storage_path, 4, 2)))
		        ON CONFLICT (generation_id, message_id, blob_hash, media_input_key) DO UPDATE SET
		            state = 'stale',
		            outcome_kind = NULL, outcome_reason = NULL,
		            prepared_revision = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE visual_publications.state = 'tombstoned';
		    END`, strings.Repeat("[0-9a-f]", 64)),
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_insert`,
		`CREATE TRIGGER trg_visual_publication_message_body_insert
		    AFTER INSERT ON message_bodies FOR EACH ROW
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = NEW.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL)
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		            prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = NEW.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_delete`,
		`CREATE TRIGGER trg_visual_publication_message_body_delete
		    AFTER DELETE ON message_bodies FOR EACH ROW
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = OLD.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL)
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		            prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = OLD.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		    END`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_update`,
		`CREATE TRIGGER trg_visual_publication_message_body_update
		    AFTER UPDATE OF body_text, body_html ON message_bodies FOR EACH ROW
		    WHEN OLD.body_text IS NOT NEW.body_text OR OLD.body_html IS NOT NEW.body_html
		    BEGIN
		        INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		        SELECT generation_id, pending_vector_token FROM visual_publications
		        WHERE pending_vector_token IS NOT NULL AND message_id = NEW.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL)
		        ON CONFLICT (generation_id, vector_token) DO NOTHING;

		        UPDATE visual_publications
		        SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		            prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		            pending_vector_token = NULL,
		            updated_at = CURRENT_TIMESTAMP
		        WHERE message_id = NEW.message_id AND state <> 'tombstoned'
		          AND (state = 'current' OR prepared_revision IS NOT NULL
		               OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		    END`,
	)
	stmts = append(stmts, personSweepSQLiteTriggerStatements()...)
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure content_changed_at triggers: %w", err)
		}
	}
	return nil
}

func personSweepSQLiteTriggerStatements() []string {
	messageScope := func(row string) string {
		recipientRole := personSweepRecipientRolePredicate("mr.recipient_type")
		roster := personSweepRosterPredicateValuesSQL(
			row+".id", row+".is_from_me", row+".sender_id",
			"c.conversation_type", "pp.person_id",
		)
		return fmt.Sprintf(`
			SELECT pp.person_id,
			       CASE WHEN %[1]s.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       %[1]s.source_id AS source_id, %[1]s.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = %[1]s.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN %[1]s.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       %[1]s.source_id, %[1]s.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = %[1]s.id AND %[2]s
			UNION
			SELECT pp.person_id,
			       CASE WHEN %[1]s.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       %[1]s.source_id, %[1]s.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = %[1]s.conversation_id AND %[3]s`, row, recipientRole, roster)
	}
	currentMessageForParticipant := func(message, participant, recipientType string) string {
		return fmt.Sprintf(`
			SELECT pp.person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       m.source_id AS source_id, m.id AS message_id
			FROM messages m
			JOIN person_participants pp ON pp.participant_id = %[2]s
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE m.id = %[1]s AND m.deleted_at IS NULL
			  AND m.deleted_from_source_at IS NULL AND %[3]s`, message, participant,
			personSweepRecipientRolePredicate(recipientType))
	}
	currentMessageScope := func(message string) string {
		recipientRole := personSweepRecipientRolePredicate("mr.recipient_type")
		roster := personSweepRosterPredicateSQL("pp.person_id")
		return fmt.Sprintf(`
			SELECT pp.person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       m.source_id AS source_id, m.id AS message_id
			FROM messages m
			JOIN person_participants pp ON pp.participant_id = m.sender_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE m.id = %[1]s AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
			UNION
			SELECT pp.person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       m.source_id, m.id
			FROM messages m
			JOIN message_recipients mr ON mr.message_id = m.id
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE m.id = %[1]s AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
			  AND %[2]s
			UNION
			SELECT pp.person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       m.source_id, m.id
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE m.id = %[1]s AND %[3]s
			  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`,
			message, recipientRole, roster)
	}
	currentConversationForParticipant := func(conversation, participant string) string {
		return fmt.Sprintf(`
			SELECT pp.person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       m.source_id AS source_id, m.id AS message_id
			FROM conversations c
			JOIN messages m ON m.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = %[2]s
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = %[1]s AND %[3]s
			  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`,
			conversation, participant, personSweepRosterPredicateSQL("pp.person_id"))
	}
	currentMessagesForBinding := func(person, participant string) string {
		return fmt.Sprintf(`
			SELECT %[1]s AS person_id,
			       CASE WHEN m.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       m.source_id AS source_id, m.id AS message_id
			FROM messages m
			JOIN person_tracking pt ON pt.person_id = %[1]s
			WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
			  AND (m.sender_id = %[2]s
			       OR EXISTS (SELECT 1 FROM message_recipients mr
			                  WHERE mr.message_id = m.id AND mr.participant_id = %[2]s
			                    AND %[3]s)
			       OR EXISTS (SELECT 1 FROM conversations c
			                  JOIN conversation_participants cp ON cp.conversation_id = c.id
			                  WHERE c.id = m.conversation_id
			                    AND %[4]s
			                    AND cp.participant_id = %[2]s))`, person, participant,
			personSweepRecipientRolePredicate("mr.recipient_type"),
			personSweepRosterPredicateSQL(person))
	}

	appendTrigger := func(
		name, timing, action, table, when, scope, mode, kind, effect string,
	) []string {
		whenClause := " WHEN EXISTS (SELECT 1 FROM person_tracking)"
		if when != "" {
			whenClause = " WHEN EXISTS (SELECT 1 FROM person_tracking) AND (" + when + ")"
		}
		messageRows := `SELECT affected.person_id,affected.source_lane,
			affected.source_id,affected.message_id,0 AS attachment_id,'' AS occurrence_key
			FROM (` + scope + `) affected`
		documentRows := `SELECT affected.person_id,'document_text' AS source_lane,
			o.source_id,o.message_id,o.attachment_id,o.occurrence_key
			FROM (` + scope + `) affected
			JOIN document_occurrences o ON o.message_id=affected.message_id`
		switch mode {
		case "both":
			scope = messageRows + " UNION " + documentRows
		case "document":
			scope = documentRows
		default:
			scope = messageRows
		}
		return []string{
			"DROP TRIGGER IF EXISTS " + name,
			fmt.Sprintf(`CREATE TRIGGER %s
			    %s %s ON %s FOR EACH ROW%s
			    BEGIN
			        UPDATE person_sweep_change_clock
			           SET sequence = sequence + (SELECT COUNT(*) FROM (%s) affected)
			         WHERE singleton = TRUE AND enabled = TRUE;
			        INSERT INTO person_sweep_changes
			            (sequence, person_id, source_lane, change_kind, evidence_effect,
			             source_id, message_id, attachment_id, occurrence_key, recorded_at)
			        SELECT c.sequence - totals.total +
			                   ROW_NUMBER() OVER (ORDER BY affected.person_id,
			                                                affected.source_id,
			                                                affected.message_id,
			                                                affected.attachment_id,
			                                                affected.occurrence_key),
			               affected.person_id, affected.source_lane, %s, %s,
			               affected.source_id, affected.message_id,
			               NULLIF(affected.attachment_id,0),affected.occurrence_key,CURRENT_TIMESTAMP
			          FROM (%s) affected
			          CROSS JOIN (SELECT COUNT(*) AS total FROM (%s)) totals
			          JOIN person_sweep_change_clock c ON c.singleton = TRUE
			         WHERE c.enabled = TRUE;
			    END`, name, timing, action, table, whenClause, scope, kind, effect, scope, scope),
		}
	}

	stmts := make([]string, 0, 40)
	add := func(parts []string) { stmts = append(stmts, parts...) }
	// Fresh message INSERTs publish from upsertMessageWith. Retaining even an
	// inert SQLite row trigger here adds a trigger subprogram and statement
	// journal to the hottest bulk-ingest path.
	stmts = append(stmts, "DROP TRIGGER IF EXISTS trg_person_sweep_messages_insert")
	add(appendTrigger("trg_person_sweep_messages_update", "AFTER",
		"UPDATE OF "+contentChangedTriggerColumnList(), "messages",
		contentChangedValueGuard("IS NOT"),
		`SELECT * FROM (`+messageScope("OLD")+`)
		 WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		 UNION
		 SELECT * FROM (`+messageScope("NEW")+`)
		 WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL`,
		"message", `CASE WHEN (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'delete'
		       WHEN OLD.sender_id IS NOT NEW.sender_id
		         OR OLD.conversation_id IS NOT NEW.conversation_id THEN 'scope'
		       ELSE 'upsert' END`,
		`CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'source-deleted'
		       WHEN (OLD.deleted_at IS NOT NULL OR OLD.deleted_from_source_at IS NOT NULL)
		            AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		       THEN 'source-reimported'
		       WHEN OLD.sender_id IS NOT NEW.sender_id
		         OR OLD.conversation_id IS NOT NEW.conversation_id THEN 'identity-reassigned'
		       ELSE 'source-edited' END`))
	add(appendTrigger("trg_person_sweep_messages_delete", "BEFORE", "DELETE", "messages",
		"OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL",
		messageScope("OLD"), "both", "'delete'", "'source-deleted'"))
	add(appendTrigger("trg_person_sweep_documents_message_update", "AFTER",
		"UPDATE OF sender_id, conversation_id, deleted_at, deleted_from_source_at", "messages",
		`OLD.sender_id IS NOT NEW.sender_id
		 OR OLD.conversation_id IS NOT NEW.conversation_id
		 OR OLD.deleted_at IS NOT NEW.deleted_at
		 OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at`,
		`SELECT * FROM (`+messageScope("OLD")+`)
		 WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		 UNION
		 SELECT * FROM (`+messageScope("NEW")+`)
		 WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL`,
		"document",
		`CASE WHEN (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'delete' ELSE 'scope' END`,
		`CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'source-deleted'
		       WHEN (OLD.deleted_at IS NOT NULL OR OLD.deleted_from_source_at IS NOT NULL)
		            AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		       THEN 'source-reimported'
		       ELSE 'identity-reassigned' END`))

	add(appendTrigger("trg_person_sweep_bodies_insert", "AFTER", "INSERT", "message_bodies", "",
		currentMessageScope("NEW.message_id"),
		"message", "'upsert'", "''"))
	add(appendTrigger("trg_person_sweep_bodies_update", "AFTER", "UPDATE", "message_bodies",
		"OLD.body_text IS NOT NEW.body_text OR OLD.body_html IS NOT NEW.body_html",
		currentMessageScope("NEW.message_id"),
		"message", "'upsert'", "'source-edited'"))
	add(appendTrigger("trg_person_sweep_bodies_delete", "BEFORE", "DELETE", "message_bodies", "",
		currentMessageScope("OLD.message_id"),
		"message", "'upsert'", "'source-edited'"))

	add(appendTrigger("trg_person_sweep_recipients_insert", "AFTER", "INSERT", "message_recipients", "",
		currentMessageForParticipant("NEW.message_id", "NEW.participant_id", "NEW.recipient_type"),
		"both", "'scope'", "'scope-relinked'"))
	add(appendTrigger("trg_person_sweep_recipients_update", "AFTER", "UPDATE", "message_recipients",
		"OLD.message_id IS NOT NEW.message_id OR OLD.participant_id IS NOT NEW.participant_id OR OLD.recipient_type IS NOT NEW.recipient_type",
		currentMessageForParticipant("OLD.message_id", "OLD.participant_id", "OLD.recipient_type")+" UNION "+
			currentMessageForParticipant("NEW.message_id", "NEW.participant_id", "NEW.recipient_type"),
		"both",
		`CASE WHEN OLD.message_id IS NOT NEW.message_id
		            OR OLD.participant_id IS NOT NEW.participant_id
		       THEN 'scope' ELSE `+personSweepRecipientRoleChangeKindSQL(
			"OLD.recipient_type", "NEW.recipient_type")+` END`,
		`CASE WHEN OLD.message_id IS NOT NEW.message_id
		            OR OLD.participant_id IS NOT NEW.participant_id
		       THEN 'identity-reassigned' ELSE `+personSweepRecipientRoleChangeEffectSQL(
			"OLD.recipient_type", "NEW.recipient_type")+` END`))
	add(appendTrigger("trg_person_sweep_recipients_delete", "BEFORE", "DELETE", "message_recipients", "",
		currentMessageForParticipant("OLD.message_id", "OLD.participant_id", "OLD.recipient_type"),
		"both", "'scope'", "'scope-unlinked'"))

	add(appendTrigger("trg_person_sweep_roster_insert", "AFTER", "INSERT", "conversation_participants", "",
		currentConversationForParticipant("NEW.conversation_id", "NEW.participant_id"),
		"both", "'scope'", "'scope-relinked'"))
	add(appendTrigger("trg_person_sweep_roster_update", "AFTER", "UPDATE", "conversation_participants",
		"OLD.conversation_id IS NOT NEW.conversation_id OR OLD.participant_id IS NOT NEW.participant_id OR OLD.role IS NOT NEW.role OR OLD.joined_at IS NOT NEW.joined_at OR OLD.left_at IS NOT NEW.left_at",
		currentConversationForParticipant("OLD.conversation_id", "OLD.participant_id")+" UNION "+
			currentConversationForParticipant("NEW.conversation_id", "NEW.participant_id"),
		"both",
		`CASE WHEN OLD.conversation_id IS NOT NEW.conversation_id
		            OR OLD.participant_id IS NOT NEW.participant_id
		       THEN 'scope' ELSE 'upsert' END`,
		`CASE WHEN OLD.conversation_id IS NOT NEW.conversation_id
		            OR OLD.participant_id IS NOT NEW.participant_id
		       THEN 'identity-reassigned' ELSE 'source-edited' END`))
	add(appendTrigger("trg_person_sweep_roster_delete", "BEFORE", "DELETE", "conversation_participants", "",
		currentConversationForParticipant("OLD.conversation_id", "OLD.participant_id"),
		"both", "'scope'", "'scope-unlinked'"))

	add(appendTrigger("trg_person_sweep_bindings_insert", "AFTER", "INSERT", "person_participants", "",
		currentMessagesForBinding("NEW.person_id", "NEW.participant_id"),
		"both", "'scope'", "'scope-relinked'"))
	add(appendTrigger("trg_person_sweep_bindings_update", "AFTER", "UPDATE", "person_participants",
		"OLD.person_id IS NOT NEW.person_id OR OLD.participant_id IS NOT NEW.participant_id",
		currentMessagesForBinding("OLD.person_id", "OLD.participant_id")+" UNION "+
			currentMessagesForBinding("NEW.person_id", "NEW.participant_id"),
		"both", "'scope'", "'identity-reassigned'"))
	add(appendTrigger("trg_person_sweep_bindings_delete", "BEFORE", "DELETE", "person_participants", "",
		currentMessagesForBinding("OLD.person_id", "OLD.participant_id"),
		"both", "'scope'", "'scope-unlinked'"))
	return stmts
}

func dropPersonSweepSQLiteTriggers(q querier) error {
	for _, stmt := range personSweepSQLiteTriggerStatements() {
		if !strings.HasPrefix(stmt, "DROP TRIGGER IF EXISTS ") {
			continue
		}
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("drop person sweep trigger for recipient table rebuild: %w", err)
		}
	}
	return nil
}

// EnsureActivityProjectionTriggers repairs activity trigger definitions on
// archives created before the activity projection queue became part of the
// production write path. Message INSERTs are intentionally not trigger-backed:
// even a trigger that does no work adds a compiled trigger subprogram and a
// statement journal to every fresh message INSERT. The conversation trigger
// is scoped to real conversation_type changes — a blanket AFTER UPDATE
// trigger requeued whole archives on routine statistics recomputation. SQLite
// resolves the column reference at fire time, so the definition is valid even
// before the conversation_type ADD COLUMN migration has run.
//
// The messages trigger is built here rather than in schema.sql because its
// column list comes from MessagesActivityColumns, shared with the PostgreSQL
// dialect so the two backends cannot drift, and because DROP + CREATE replaces
// the blanket definition an earlier build left on an existing archive where
// CREATE TRIGGER IF NOT EXISTS silently would not. The recipient triggers are
// repaired here in addition to their fresh-schema bootstrap definitions
// because the legacy envelope migration rebuilds their table and must restore
// them inside the same transaction. This installer runs after
// LegacyColumnMigrations, so every column it names already exists.
func (d *SQLiteDialect) EnsureActivityProjectionTriggers(q querier) error {
	stmts := []string{
		`DROP TRIGGER IF EXISTS trg_activity_queue_messages_insert`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_messages_update`,
		fmt.Sprintf(`CREATE TRIGGER trg_activity_queue_messages_update
		     AFTER UPDATE OF %s ON messages FOR EACH ROW
		     WHEN %s
		     BEGIN
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         VALUES (NEW.id, 1, CURRENT_TIMESTAMP)
		         ON CONFLICT(message_id) DO UPDATE SET
			         revision = activity_projection_queue.revision + 1,
			         queued_at = CURRENT_TIMESTAMP;
			 END`, activityTriggerColumnList(), activityValueGuard("IS NOT")),
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_insert`,
		`CREATE TRIGGER trg_activity_queue_recipients_insert
		     AFTER INSERT ON message_recipients FOR EACH ROW
		     BEGIN
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         VALUES (NEW.message_id, 1, CURRENT_TIMESTAMP)
		         ON CONFLICT(message_id) DO UPDATE SET
		             revision = activity_projection_queue.revision + 1,
		             queued_at = CURRENT_TIMESTAMP;
		     END`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_update`,
		`CREATE TRIGGER trg_activity_queue_recipients_update
		     AFTER UPDATE ON message_recipients FOR EACH ROW
		     BEGIN
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         SELECT id, 1, CURRENT_TIMESTAMP
		         FROM messages
		         WHERE id = OLD.message_id
		         ON CONFLICT(message_id) DO UPDATE SET
		             revision = activity_projection_queue.revision + 1,
		             queued_at = CURRENT_TIMESTAMP;
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         VALUES (NEW.message_id, 1, CURRENT_TIMESTAMP)
		         ON CONFLICT(message_id) DO UPDATE SET
		             revision = activity_projection_queue.revision + 1,
		             queued_at = CURRENT_TIMESTAMP;
		     END`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_delete`,
		`CREATE TRIGGER trg_activity_queue_recipients_delete
		     AFTER DELETE ON message_recipients FOR EACH ROW
		     BEGIN
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         SELECT id, 1, CURRENT_TIMESTAMP
		         FROM messages
		         WHERE id = OLD.message_id
		         ON CONFLICT(message_id) DO UPDATE SET
		             revision = activity_projection_queue.revision + 1,
		             queued_at = CURRENT_TIMESTAMP;
		     END`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_type_update`,
		`CREATE TRIGGER trg_activity_queue_conversation_type_update
		     AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
		     WHEN OLD.conversation_type IS NOT NEW.conversation_type
		     BEGIN
		         INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		         SELECT id, 1, CURRENT_TIMESTAMP
		         FROM messages WHERE conversation_id = NEW.id
		         ON CONFLICT(message_id) DO UPDATE SET
		             revision = activity_projection_queue.revision + 1,
		             queued_at = CURRENT_TIMESTAMP;
		     END`,
	}
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure activity projection triggers: %w", err)
		}
	}
	return nil
}

// lastModifiedUpdateOfColumns renders every live column of `messages` EXCEPT
// content_changed_at, for the last_modified trigger's `UPDATE OF` clause.
//
// last_modified is a blanket row-level watermark: it must move whenever any
// real column changes, so the list is "everything except", not a curated set.
// The one exclusion exists because SQLite triggers cannot assign to NEW, so
// trg_messages_content_changed_at stamps the watermark with a SECOND UPDATE of
// the row. A blanket AFTER UPDATE last_modified trigger fires on that stamp
// too, and because the stamp does not name last_modified its WHEN guard
// (OLD.last_modified = NEW.last_modified) holds — so it overwrites whatever
// last_modified the caller's original statement had just set by hand. That
// silently disarms ApplyMessageDateRepairs, whose CAS compares against exactly
// that written token. `UPDATE OF` matches on the columns a statement NAMES, so
// excluding content_changed_at excludes the stamp and nothing else.
// PostgreSQL needs none of this: it stamps in a BEFORE trigger, in place.
//
// Read from the live table rather than MessagesContentColumns +
// MessagesNonContentColumns so it cannot drift from the real schema and so
// PostgreSQL-only columns (search_fts) are naturally absent. EnsureTriggers
// runs after LegacyColumnMigrations, so every column already exists.
//
// The names are read as SEPARATE VALUES and quoted individually, because they
// are interpolated into DDL and the ARCHIVE supplies them: SQLite accepts any
// text as a column name when it is quoted, so a name can itself be a second
// statement, and go-sqlite3 executes every statement in a string it is handed.
// A raw group_concat list therefore made any archive able to run arbitrary SQL
// at open. quoteIdentifier is the same contract commonColumns applies to the
// subset copy's column list: render every name quoted, doubling any quote
// inside it. commonColumns itself is not reusable here — it takes
// a *sql.Tx with a second schema ATTACHed and returns the intersection of the
// two, and neither exists on this path.
//
// json_group_array rather than group_concat because the separator has to be one
// no column name can contain, and rather than a rows loop because querier
// deliberately exposes only QueryRow (its two implementations return different
// concrete row types and cannot be unified behind one interface). JSON1 is
// built into the bundled SQLite and this project already relies on it.
func (d *SQLiteDialect) lastModifiedUpdateOfColumns(q querier) (string, error) {
	var encoded sql.NullString
	err := q.QueryRow(
		`SELECT json_group_array(name) FROM (
		     SELECT name FROM pragma_table_info('messages')
		     WHERE name <> 'content_changed_at' ORDER BY cid)`,
	).Scan(&encoded)
	if err != nil {
		return "", fmt.Errorf("read messages columns for last_modified trigger: %w", err)
	}
	var names []string
	if encoded.Valid {
		if err := json.Unmarshal([]byte(encoded.String), &names); err != nil {
			return "", fmt.Errorf("read messages columns for last_modified trigger: %w", err)
		}
	}
	if len(names) == 0 {
		return "", errors.New(
			"cannot scope the last_modified trigger: messages has no columns")
	}
	return strings.Join(quoteIdentifiers(names), ", "), nil
}

// contentChangedAtDefaultStamps reports whether messages.content_changed_at
// carries exactly the DEFAULT that ContentChangedNow writes, which is the case
// on a database created from schema.sql. It returns false, and no error, when
// the column has NO default — the shape an ALTER TABLE ADD COLUMN upgrade
// produces, where the INSERT trigger is the only writer.
//
// A default that is neither of those is REJECTED: EnsureTriggers fails and the
// archive does not open.
//
// Rejecting rather than tolerating, because the INSERT trigger cannot rescue
// such an archive. The trigger fires only WHEN NEW.content_changed_at IS NULL,
// and SQLite applies a non-NULL column DEFAULT BEFORE the trigger runs, so a
// drifted default stays authoritative and goes on writing timestamps of its own
// SHAPE. The change feed compares SQLite timestamps LEXICALLY, so once two
// shapes are in the table ("2026-08-03 22:10:04" against
// "2026-08-03 22:10:04.731") a cursor sorts rows into the wrong place and
// silently skips or repeats changes, with nothing reporting it. An archive that
// refuses to open, naming the column and both defaults, is strictly better than
// a feed that quietly loses records.
//
// This costs no legitimate archive anything: SQLite has no ALTER COLUMN and
// refuses a non-constant DEFAULT in ADD COLUMN, so neither a fresh nor an
// upgraded archive can reach this state. Only a hand-rewritten schema can.
func (d *SQLiteDialect) contentChangedAtDefaultStamps(q querier) (bool, error) {
	var dflt sql.NullString
	err := q.QueryRow(
		`SELECT dflt_value FROM pragma_table_info('messages') WHERE name = 'content_changed_at'`,
	).Scan(&dflt)
	if errors.Is(err, sql.ErrNoRows) {
		// The column migration has not run yet on this handle; the caller's
		// trigger is then the only possible writer.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read messages.content_changed_at default: %w", err)
	}
	switch {
	case !dflt.Valid || dflt.String == "":
		return false, nil
	case dflt.String == d.ContentChangedNow():
		return true, nil
	}
	return false, fmt.Errorf(
		"messages.content_changed_at carries the DEFAULT %s, but this build stamps "+
			"the change-feed watermark with %s. The two produce timestamps of "+
			"different shapes, and the feed orders them lexically, so mixing them "+
			"silently skips or repeats changes. Repair the column default to %s "+
			"before opening this archive",
		dflt.String, d.ContentChangedNow(), d.ContentChangedNow())
}

// LegacyColumnMigrations returns the ALTER TABLE ADD COLUMN statements that
// bring older SQLite databases up to the current schema. IsDuplicateColumnError
// silences these when the column already exists (idempotent migrations).
func (d *SQLiteDialect) LegacyColumnMigrations() []ColumnMigration {
	return []ColumnMigration{
		{`ALTER TABLE person_sweep_cursors ADD COLUMN backstop_upper_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_upper_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN backstop_after_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_after_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN optimistic_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.optimistic_document_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN reconcile_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.reconcile_document_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN backstop_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_document_key"},
		{`ALTER TABLE carddav_address_books ADD COLUMN needs_full_reconcile BOOLEAN NOT NULL DEFAULT FALSE`, "carddav_address_books.needs_full_reconcile"},
		{`ALTER TABLE carddav_address_books ADD COLUMN sync_token TEXT NOT NULL DEFAULT ''`, "carddav_address_books.sync_token"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN pending_operation TEXT CHECK (pending_operation IN ('delete'))`, "carddav_conflicts.pending_operation"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN connection_generation INTEGER`, "carddav_conflicts.connection_generation"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN book_sync_revision INTEGER`, "carddav_conflicts.book_sync_revision"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN previous_mapping_revision INTEGER`, "carddav_conflicts.previous_mapping_revision"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN pending_started_at DATETIME`, "carddav_conflicts.pending_started_at"},
		{`ALTER TABLE sources ADD COLUMN sync_config JSON`, "sync_config"},
		{`ALTER TABLE sync_runs ADD COLUMN sync_type TEXT NOT NULL DEFAULT ''`, "sync_runs.sync_type"},
		{`ALTER TABLE imap_folder_state ADD COLUMN highest_modseq TEXT NOT NULL DEFAULT '0'`, "imap_folder_state.highest_modseq"},
		{`ALTER TABLE messages ADD COLUMN rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN sender_id INTEGER REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN source_is_from_me BOOLEAN`, "source_is_from_me"},
		{`ALTER TABLE messages ADD COLUMN identity_is_from_me BOOLEAN NOT NULL DEFAULT FALSE`, "identity_is_from_me"},
		{`ALTER TABLE messages ADD COLUMN message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN deleted_from_source_at DATETIME`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN deleted_at DATETIME`, "deleted_at"},
		{`ALTER TABLE messages ADD COLUMN delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
		{`ALTER TABLE labels ADD COLUMN system_role TEXT`, "labels.system_role"},
		{`ALTER TABLE participant_identifiers ADD COLUMN service_id INTEGER REFERENCES communication_services(id) ON DELETE SET NULL`, "pi_service_id"},
		{`ALTER TABLE participant_identifiers ADD COLUMN scope_kind TEXT`, "pi_scope_kind"},
		{`ALTER TABLE participant_identifiers ADD COLUMN scope_value TEXT`, "pi_scope_value"},
		{sqliteParticipantLinkIdentityMatchCandidateMigration, "participant_links.identity_match_candidate_id"},
		{sqliteIdentityMatchObservationConflictOriginMigration, identityMatchObservationConflictOriginMigrationDesc},
		{sqliteIdentityMatchCandidateSourcesMigration, identityMatchCandidateSourcesTableName},
		{sqliteIdentityMatchEvidenceSourcesMigration, identityMatchEvidenceSourcesTableName},
		{sqliteIdentityMatchPreConflictStateMigration, "identity_match_candidates.pre_conflict_state"},
		{sqliteIdentityMatchApplicationPendingMigration, "identity_match_candidates.application_pending"},
		{`ALTER TABLE embedding_changes ADD COLUMN old_message_type TEXT`, "embedding_changes.old_message_type"},
		{`ALTER TABLE embedding_changes ADD COLUMN new_message_type TEXT`, "embedding_changes.new_message_type"},
		{`ALTER TABLE embedding_change_clock ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT FALSE`, "embedding_change_clock.enabled"},
		{`ALTER TABLE attribute_definitions ADD COLUMN is_sensitive BOOLEAN NOT NULL DEFAULT FALSE`, "attribute_definitions.is_sensitive"},
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
		// content_changed_at: content-scoped change watermark. No default here,
		// because SQLite rejects a non-constant DEFAULT in ADD COLUMN; fresh
		// databases DO carry one (schema.sql), and on this upgrade path the
		// INSERT trigger stamps new rows instead. InitSchema's backfill seeds
		// pre-existing rows from last_modified, a better starting point than
		// "now", which would make an existing archive look like every message
		// changed at upgrade time.
		{`ALTER TABLE messages ADD COLUMN content_changed_at DATETIME`, "content_changed_at"},
		// email_address: immutable envelope address for identity discovery.
		// Legacy rows stay NULL (unfillable without re-parsing raw MIME) and
		// discovery falls back to the participant's email for them.
		{`ALTER TABLE message_recipients ADD COLUMN email_address TEXT`, "message_recipients.email_address"},
		{`ALTER TABLE attachments ADD COLUMN attachment_role TEXT NOT NULL DEFAULT 'unknown' CHECK (attachment_role IN ('standalone', 'inline', 'avatar', 'thumbnail', 'preview', 'sticker', 'ui_asset', 'unknown'))`, "attachments.attachment_role"},
		{`ALTER TABLE attachments ADD COLUMN role_source TEXT NOT NULL DEFAULT 'unknown' CHECK (role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'legacy_api', 'raw_mime_repair', 'unknown'))`, "attachments.role_source"},
		{`ALTER TABLE attachments ADD COLUMN source_part_key TEXT CHECK (source_part_key IS NULL OR source_part_key != '')`, "attachments.source_part_key"},
		{`ALTER TABLE attachments ADD COLUMN content_id TEXT`, "attachments.content_id"},
		{`ALTER TABLE document_extractions ADD COLUMN rebuild_id TEXT REFERENCES document_extraction_rebuilds(id) ON DELETE SET NULL`, "document_extractions.rebuild_id"},
		{`ALTER TABLE document_extractions ADD COLUMN request_count INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0)`, "document_extractions.request_count"},
		{`ALTER TABLE document_extractions ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0 AND retry_count <= request_count)`, "document_extractions.retry_count"},
		{`ALTER TABLE document_extractions ADD COLUMN provider_latency_ms INTEGER NOT NULL DEFAULT 0 CHECK (provider_latency_ms >= 0)`, "document_extractions.provider_latency_ms"},
		{`ALTER TABLE document_extractions ADD COLUMN normalization_version INTEGER`, "document_extractions.normalization_version"},
		{`ALTER TABLE document_extractions ADD COLUMN document_family TEXT`, "document_extractions.document_family"},
		{`ALTER TABLE document_extractions ADD COLUMN unit_kind TEXT`, "document_extractions.unit_kind"},
		{`ALTER TABLE document_extractions ADD COLUMN normalized_truncated BOOLEAN NOT NULL DEFAULT FALSE`, "document_extractions.normalized_truncated"},
		{`ALTER TABLE document_units ADD COLUMN heading_marks JSON NOT NULL DEFAULT '[]'`, "document_units.heading_marks"},
		{`ALTER TABLE document_index_state ADD COLUMN target_profile_id TEXT`, "document_index_state.target_profile_id"},
		{`ALTER TABLE attachments ADD COLUMN attachment_state TEXT`, "attachments.attachment_state"},
		{`ALTER TABLE attachments ADD COLUMN attachment_skip_reason TEXT`, "attachments.attachment_skip_reason"},
		// vcard_projection_revision: the lock and change token native vCard
		// envelope commits serialize on. Existing rows start at 1 like fresh
		// ones; the absolute value never matters, only that a projection
		// write moves it, so no backfill is needed.
		{`ALTER TABLE persons ADD COLUMN vcard_projection_revision INTEGER NOT NULL DEFAULT 1`,
			"persons.vcard_projection_revision"},
		{`ALTER TABLE person_names ADD COLUMN source_resource_uid TEXT`, "person_names.source_resource_uid"},
		{`ALTER TABLE person_contact_points ADD COLUMN source_resource_uid TEXT`, "person_contact_points.source_resource_uid"},
		{`ALTER TABLE person_addresses ADD COLUMN source_resource_uid TEXT`, "person_addresses.source_resource_uid"},
		{`ALTER TABLE person_dates ADD COLUMN source_resource_uid TEXT`, "person_dates.source_resource_uid"},
		{`ALTER TABLE person_categories ADD COLUMN source_resource_uid TEXT`, "person_categories.source_resource_uid"},
		{`ALTER TABLE person_media ADD COLUMN source_resource_uid TEXT`, "person_media.source_resource_uid"},
		{`ALTER TABLE participant_contact_observations ADD COLUMN source_resource_uid TEXT`, "participant_contact_observations.source_resource_uid"},
		{`ALTER TABLE organization_names ADD COLUMN source_resource_uid TEXT`, "organization_names.source_resource_uid"},
		{`ALTER TABLE organization_identifiers ADD COLUMN source_resource_uid TEXT`, "organization_identifiers.source_resource_uid"},
		{`ALTER TABLE organization_addresses ADD COLUMN source_resource_uid TEXT`, "organization_addresses.source_resource_uid"},
		{`ALTER TABLE organization_contact_points ADD COLUMN source_resource_uid TEXT`, "organization_contact_points.source_resource_uid"},
		{`ALTER TABLE organization_categories ADD COLUMN source_resource_uid TEXT`, "organization_categories.source_resource_uid"},
		{`ALTER TABLE organization_media ADD COLUMN source_resource_uid TEXT`, "organization_media.source_resource_uid"},
		{`ALTER TABLE person_relationships ADD COLUMN source_resource_uid TEXT`, "person_relationships.source_resource_uid"},
		{`ALTER TABLE person_relationship_reviews ADD COLUMN source_resource_uid TEXT`, "person_relationship_reviews.source_resource_uid"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN targets_json TEXT`, "person_enrichment_attempts.targets_json"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN provider_started_at DATETIME`, "person_enrichment_attempts.provider_started_at"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN dispatch_authorized_at DATETIME`, "person_enrichment_attempts.dispatch_authorized_at"},
		{`ALTER TABLE person_enrichment_work ADD COLUMN has_fresh_trigger BOOLEAN NOT NULL DEFAULT FALSE`, "person_enrichment_work.has_fresh_trigger"},
	}
}

// DatabaseSize returns SQLite's logical main-database allocation:
// PRAGMA page_count multiplied by PRAGMA page_size. The query sees committed
// pages that still reside in WAL, while deliberately excluding WAL framing and
// SHM sidecar overhead. In-memory databases retain the historical 0 result.
func (d *SQLiteDialect) DatabaseSize(
	ctx context.Context,
	db *sql.DB,
	dbPath string,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if dbPath == "" || dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") {
		return 0, nil
	}

	var pageCount int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("query SQLite page count: %w", err)
	}
	var pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("query SQLite page size: %w", err)
	}
	return pageCount * pageSize, nil
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

// RowWriterLockSQL returns a self-assign UPDATE on the row, which is how a
// deferred SQLite transaction reserves the writer lock before it reads.
func (d *SQLiteDialect) RowWriterLockSQL(table, column string) string {
	return "UPDATE " + table + " SET " + column + " = " + column + " WHERE id = ?"
}

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
	if serr, ok := errors.AsType[sqlite3.Error](err); ok {
		return serr.Code == sqlite3.ErrBusy || serr.Code == sqlite3.ErrLocked
	}
	var serrPtr *sqlite3.Error
	if errors.As(err, &serrPtr) && serrPtr != nil {
		return serrPtr.Code == sqlite3.ErrBusy || serrPtr.Code == sqlite3.ErrLocked
	}
	return false
}

// IsSerializationFailureError always returns false for SQLite. Only one write
// transaction runs at a time, so a locked row can never have been changed and
// committed by another transaction while this one held its snapshot — the
// condition PostgreSQL reports as SQLSTATE 40001 does not arise.
func (d *SQLiteDialect) IsSerializationFailureError(err error) bool { return false }

// IsFTSValueTooLargeError always returns false for SQLite: FTS5 has no
// per-value size limit analogous to PostgreSQL's tsvector "string is too long"
// (SQLSTATE 54000), so the backfill never has a row to skip on SQLite.
func (d *SQLiteDialect) IsFTSValueTooLargeError(err error) bool { return false }
