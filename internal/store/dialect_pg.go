package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for database/sql

	"go.kenn.io/msgvault/internal/sqldialect"
)

// PostgreSQLDialect implements Dialect for PostgreSQL.
//
// The zero value is ready to use, and every method except ReadWatermarkBounds
// is stateless — callers outside Store that only want Rebind can go on
// constructing one per call. ReadWatermarkBounds remembers the last bound it
// computed while every backend in the database was visible to it, which is the
// floor it falls back to when pg_stat_activity starts redacting other roles'
// backends; the Store's single long-lived instance carries that across pages.
type PostgreSQLDialect struct {
	visibilityMu      sync.Mutex
	fullyVisibleAt    time.Time
	fullyVisibleBound time.Time
}

func (d *PostgreSQLDialect) DriverName() string { return "pgx" }

// Rebind converts ? placeholders to PostgreSQL $1, $2, ... numbered
// placeholders. Delegates to sqldialect so the query package's
// PostgreSQLQueryDialect.Rebind stays in lockstep.
func (d *PostgreSQLDialect) Rebind(query string) string {
	return sqldialect.RebindPostgreSQL(query)
}

// Now returns the PostgreSQL expression for the current timestamp.
func (d *PostgreSQLDialect) Now() string { return "NOW()" }

// ContentChangedNow returns the PostgreSQL expression that stamps
// content_changed_at. clock_timestamp() reads the wall clock at the moment of
// the call, so — unlike NOW(), which is fixed for the duration of a transaction
// — rows written by one transaction get microsecond-resolution stamps that can
// be told apart, rather than one shared instant.
//
// It is a clock, not a sequence: PostgreSQL guarantees neither that successive
// readings differ nor that they only move forward. Ties the
// (content_changed_at, id) cursor breaks by id. Backward movement it cannot
// repair — a cursor only advances — and what that costs a consumer is written
// up with the feed's other exceptions in docs/api-server.md, rather than
// restated here.
func (d *PostgreSQLDialect) ContentChangedNow() string { return "clock_timestamp()" }

// TimestampParam returns t unchanged (as UTC): PostgreSQL's TIMESTAMPTZ
// compares natively and needs no textual reformatting.
func (d *PostgreSQLDialect) TimestampParam(t time.Time) any { return t.UTC() }

// pgWatermarkBoundsQuery reads the change feed's clock and commit bound in one
// round trip.
//
// The bound is the oldest transaction start among the backends that hold a
// write lock on THIS database's `messages`, floored by this statement's own
// transaction start. Correctness rests on three facts about PostgreSQL:
//
//   - A statement that modifies rows takes ROW EXCLUSIVE on its target
//     relation, and holds it to end of transaction. It takes it before it
//     touches a row, so before the trigger evaluates clock_timestamp(). Any
//     transaction that has stamped a watermark is therefore in pg_locks here,
//     including one that reached `messages` only through the message_bodies
//     trigger — that trigger's UPDATE locks `messages` too.
//
//   - A backend publishes xact_start into shared memory when its transaction
//     begins, before any statement of that transaction can run. So a row
//     stamped by transaction X at instant S has X's xact_start visible here at
//     every instant after S, and xact_start <= S. Anything that could still
//     commit therefore sits at or above MIN(xact_start), and a page bounded
//     strictly below it cannot skip over it. Autocommit statements are covered:
//     PostgreSQL wraps each in an implicit transaction with its own xact_start.
//
//   - transaction_timestamp() closes the race in the other direction. A
//     transaction that takes the lock AFTER this read cannot appear in it, but
//     it also cannot stamp anything at or below this statement's own start.
//     clock_timestamp() would not do: it advances past the read and would leave
//     a sliver of time uncovered.
//
// The lock scope is what keeps the bound from being a database-wide stall
// signal. Filtering only on "is in a transaction" would let a connection that
// has nothing to do with `messages` — an idle-in-transaction reporting session,
// a batch writing some other table — freeze the feed for everyone. Read locks
// are excluded by mode for the same reason: a long-running SELECT on `messages`
// holds ACCESS SHARE and cannot produce a watermark. So is autovacuum, which
// takes SHARE UPDATE EXCLUSIVE and never rewrites the column. What remains are
// the modes a row write or a table rewrite actually holds.
//
// to_regclass rather than a cast so a database without the table yields NULL
// instead of an error; the COALESCE then degrades to this transaction's start
// rather than to NULL, which the keyset predicate would silently read as "no
// rows". The lock's relation OID is resolved through the session's search_path,
// so the bound is scoped to the schema this connection actually writes.
//
// THE QUERY MUST NOT FAIL OPEN. Every filter above narrows the set of
// transactions that hold the bound back, so a filter that stops matching does
// not raise an error — it silently returns the clock, which is the broken
// bound this whole mechanism exists to replace. Two ways that can happen are
// checked in Go rather than left to the SQL:
//
//   - to_regclass returns NULL if `messages` cannot be resolved on the
//     session's search_path. The query then joins against NULL, matches
//     nothing, and every uncommitted write becomes invisible to the bound. The
//     last two output columns exist so ReadWatermarkBounds can refuse instead.
//
//   - pg_stat_activity REDACTS xact_start, state and backend_type (they read
//     as NULL) for backends belonging to other roles, unless the reading role
//     is a superuser or a member of pg_read_all_stats — verified against
//     PostgreSQL 17. A redacted backend cannot hold the bound back, so a write
//     made by a different role could be lost exactly as it was before this
//     bound existed. The count of redacted backends THAT COULD BE HOLDING A
//     STAMP is returned so the bound can be capped at the last instant every
//     such backend was visible; see visibilityFloor. msgvault connects with one
//     role, so in an ordinary deployment the count is zero and nothing is
//     capped.
//
// The redaction count carries the same lock scope as the bound itself, and for
// the same reason. The two are halves of one partition: every backend in this
// database holding a write lock on `messages` is either visible, in which case
// the bound already accounts for it through its xact_start, or redacted, in
// which case it is counted here. A backend that is in neither set — a pooled
// connection sitting idle, a long SELECT holding ACCESS SHARE, autovacuum doing
// its routine SHARE UPDATE EXCLUSIVE work, a session writing some other table —
// cannot have stamped a watermark on `messages`, so it belongs in neither.
// Counting it anyway (which an unscoped count does) freezes the feed for
// everyone until that backend TERMINATES, which behind a connection pool is
// never; docs/api-server.md promises exactly the opposite.
//
// Each predicate in the count fails in a known direction:
//
//   - a.backend_type IS NULL is the redaction test. If a backend stops looking
//     redacted while its xact_start is still hidden it drops out of both halves
//     and the count fails OPEN. It cannot: the same privilege check nulls all
//     three columns together, so backend_type is visible exactly when
//     xact_start is.
//   - a.datname = current_database() and the l.relation / l.mode scope narrow
//     the count, so each of them fails OPEN if it stops matching. That is why
//     they are the SAME predicates the bound uses rather than a second,
//     independently-drifting spelling: a scope that narrowed here but not there
//     would leave a writer in neither half, which is silent row loss. pg_locks
//     is not redacted for other roles' backends (verified on PostgreSQL 17), so
//     the lock half of the join sees a foreign writer in full.
//   - No l.granted filter. An ungranted lock request cannot have stamped
//     anything yet, so filtering on it would be sound, but omitting it can only
//     over-count, and over-counting fails CLOSED — a visible stall rather than
//     a silent loss.
//
// One residual is known and not covered: a PREPARED transaction holds its locks
// with pg_locks.pid NULL, so the join drops it from both halves. It needs
// max_prepared_transactions > 0, which is off by default and which msgvault
// never uses. What that costs a consumer is written up with the feed's other
// exceptions in docs/api-server.md, rather than restated here.
const pgWriteLockModes = `('RowExclusiveLock', 'ShareRowExclusiveLock', ` +
	`'ExclusiveLock', 'AccessExclusiveLock')`

const pgWatermarkBoundsQuery = `
	SELECT clock_timestamp(),
	       transaction_timestamp(),
	       LEAST(transaction_timestamp(),
	             COALESCE((SELECT MIN(a.xact_start)
	                       FROM pg_stat_activity a
	                       JOIN pg_locks l ON l.pid = a.pid
	                       WHERE a.datname = current_database()
	                         AND a.xact_start IS NOT NULL
	                         AND l.relation = to_regclass('messages')
	                         AND l.mode IN ` + pgWriteLockModes + `),
	                      transaction_timestamp())),
	       to_regclass('messages') IS NOT NULL,
	       (SELECT count(*)
	          FROM pg_stat_activity a
	          JOIN pg_locks l ON l.pid = a.pid
	          WHERE a.datname = current_database()
	            AND a.backend_type IS NULL
	            AND l.relation = to_regclass('messages')
	            AND l.mode IN ` + pgWriteLockModes + `)`

// ReadWatermarkBounds implements Dialect. See pgWatermarkBoundsQuery for why
// the bound is what it is.
func (d *PostgreSQLDialect) ReadWatermarkBounds(
	ctx context.Context, db *sql.DB,
) (WatermarkBounds, error) {
	var now, statementStart, bound time.Time
	var messagesResolved bool
	var redacted int
	if err := db.QueryRowContext(ctx, pgWatermarkBoundsQuery).Scan(
		&now, &statementStart, &bound, &messagesResolved, &redacted,
	); err != nil {
		return WatermarkBounds{}, fmt.Errorf("read change-feed watermark bounds: %w", err)
	}
	if !messagesResolved {
		return WatermarkBounds{}, errors.New(
			"read change-feed watermark bounds: `messages` does not resolve on this " +
				"connection's search_path, so the bound cannot see which transactions " +
				"are still writing to it")
	}
	floored, err := d.visibilityFloor(bound.UTC(), statementStart.UTC(), redacted)
	if err != nil {
		return WatermarkBounds{}, err
	}
	return WatermarkBounds{Now: now.UTC(), CommitBound: floored}, nil
}

// visibilityFloor caps the bound at the last one computed while every backend
// in the database was visible.
//
// A bound that was safe once is safe forever — it asserts that everything
// stamped below it had already committed, and committed does not un-commit. So
// when pg_stat_activity starts redacting backends, falling back to the last
// bound taken with full visibility is sound: a backend this role cannot see
// belongs to another role, so it connected on a connection that did not exist
// when full visibility last held, and every stamp it makes is above that
// instant.
//
// The cost is the same one the whole design trades in: the feed stops
// advancing, visibly, instead of quietly resuming the old data loss. In an
// ordinary msgvault deployment redacted is zero on every call and this returns
// its argument unchanged.
//
// When there is no floor to fall back to — a process that has never once read
// the bound while every writer of `messages` was visible, which is what a
// server restarted during a foreign role's open write transaction looks like —
// there is no safe answer at all, and this REFUSES. Returning the unfloored
// bound instead would step the consumer's cursor over the invisible writer's
// stamped-but-uncommitted change and never come back to it: permanent row loss,
// reached by a filter matching nothing rather than by a decision. It is the
// same call ReadWatermarkBounds already makes when `messages` cannot be
// resolved, and it heals by itself the moment the foreign transaction ends.
func (d *PostgreSQLDialect) visibilityFloor(
	bound, statementStart time.Time, redacted int,
) (time.Time, error) {
	d.visibilityMu.Lock()
	defer d.visibilityMu.Unlock()
	if redacted == 0 {
		// Recorded only if this reading is NEWER than the one already held.
		// Concurrent pages read the bound at once and finish in whatever order
		// the pool gives them, so without the comparison a reading that started
		// first and finished last would overwrite a newer one and move the floor
		// backwards — and the next redacted reading would publish a
		// complete_through the feed had already passed. Each reading carries the
		// instant its own transaction started, which is when it was taken, so
		// that is what they are ordered by. A lower floor is never unsafe (a
		// bound safe once is safe forever, see below); it is just stale, and
		// making the published bound regress for no reason is not something to
		// leave to timing.
		if statementStart.After(d.fullyVisibleAt) {
			d.fullyVisibleBound = bound
			d.fullyVisibleAt = statementStart
		}
		return bound, nil
	}
	if bound.Before(d.fullyVisibleBound) {
		// The live bound is already the tighter of the two: a writer this role
		// CAN see is holding it below the floor, so there is nothing to cap.
		return bound, nil
	}
	if d.fullyVisibleAt.IsZero() {
		return time.Time{}, fmt.Errorf(
			"read change-feed watermark bounds: %d connection(s) belonging to another "+
				"PostgreSQL role are writing to `messages`, this role cannot see when "+
				"their transactions began, and no earlier reading was taken while every "+
				"writer was visible — so this page has no bound it can trust and serving "+
				"it would skip their changes. It clears on its own when those "+
				"transactions end; to stop it recurring, grant the role msgvault "+
				"connects as membership of pg_read_all_stats", redacted)
	}
	return d.fullyVisibleBound, nil
}

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
			setweight(to_tsvector('simple', LEFT(COALESCE($5, ''), `+charCap+`)), 'C') ||
			setweight(to_tsvector('simple', LEFT(COALESCE($6, ''), `+charCap+`)), 'C') ||
			setweight(to_tsvector('simple', LEFT(COALESCE($3, ''), `+charCap+`)), 'D'),
			indexing_version = `+strconv.Itoa(CurrentFTSIndexingVersion)+`
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
		"ts_rank(ARRAY[0.1, 0.1, 0.4, 1.0]::real[], m.search_fts, to_tsquery('simple', ?)) DESC",
		1
}

// FTSDeleteSQL returns the SQL to clear tsvector data for messages belonging to a source.
func (d *PostgreSQLDialect) FTSDeleteSQL() string {
	return `UPDATE messages SET search_fts = NULL WHERE source_id = $1`
}

func (d *PostgreSQLDialect) InvalidateFTSForMessage(q querier, messageID int64) error {
	_, err := q.Exec(
		"UPDATE messages SET search_fts = NULL, indexing_version = NULL WHERE id = $1",
		messageID,
	)
	return err
}

// FTSBackfillBatchSQL returns the SQL to populate tsvector for a range of message IDs.
// Parameters: $1=fromID, $2=toID. Uses LEFT JOIN on message_bodies via a subquery
// so messages without a body row are still indexed (subject + participants).
func (d *PostgreSQLDialect) FTSBackfillBatchSQL() string {
	charCap := strconv.Itoa(maxFTSBodyChars)
	return `UPDATE messages m SET search_fts =
		setweight(to_tsvector('simple', LEFT(COALESCE(m.subject, ''), ` + charCap + `)), 'A') ||
		setweight(to_tsvector('simple', LEFT(COALESCE(
			CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
			     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
			END,
			(SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
			''
		), ` + charCap + `)), 'B') ||
		setweight(to_tsvector('simple', LEFT(COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''), ` + charCap + `)), 'C') ||
		setweight(to_tsvector('simple', LEFT(COALESCE((SELECT STRING_AGG(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), ''), ` + charCap + `)), 'C') ||
		setweight(to_tsvector('simple', LEFT(COALESCE(src.body_text, ''), ` + charCap + `)), 'D'),
		indexing_version = ` + strconv.Itoa(CurrentFTSIndexingVersion) + `
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
func (d *PostgreSQLDialect) FTSAvailable(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, postgresColumnExistsSQL("messages", "search_fts")).Scan(&count)
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err == nil && count > 0, nil
}

func postgresFTSNeedsBackfillSQL() string {
	return fmt.Sprintf(
		"SELECT EXISTS (SELECT 1 FROM messages WHERE search_fts IS NULL OR indexing_version IS DISTINCT FROM %d)",
		CurrentFTSIndexingVersion,
	)
}

// FTSNeedsBackfill reports whether the tsvector column needs population or
// uses an obsolete field-weight layout. It probes for a NULL search_fts or an
// indexing_version other than CurrentFTSIndexingVersion, so both interrupted
// backfills and durable layout migrations remain visible.
//
// Uses EXISTS rather than COUNT(*): a GIN index on search_fts cannot serve an
// `IS NULL` predicate, so COUNT(*) was a full sequential scan of every message
// on each startup. EXISTS short-circuits at the first NULL row. The versioned
// partial btree index created by EnsureFTSIndex makes even the false
// case index-served and self-pruning as backfill completes.
func (d *PostgreSQLDialect) FTSNeedsBackfill(db *sql.DB) bool {
	var exists bool
	if err := db.QueryRow(
		postgresFTSNeedsBackfillSQL(),
	).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// FTSNeedsBackfillQuick uses the same exact EXISTS probe as FTSNeedsBackfill:
// the versioned stale-row index makes it as cheap as any approximation would
// be, while QueryRowContext lets foreground callers cancel connection waits.
func (d *PostgreSQLDialect) FTSNeedsBackfillQuick(ctx context.Context, db *sql.DB) bool {
	var exists bool
	if err := db.QueryRowContext(
		ctx,
		postgresFTSNeedsBackfillSQL(),
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
func (d *PostgreSQLDialect) FTSRebuildSchema(ctx context.Context, q contextQuerier) error {
	if _, err := q.ExecContext(ctx, "DROP INDEX IF EXISTS messages_search_fts_idx"); err != nil {
		return fmt.Errorf("drop messages_search_fts_idx: %w", err)
	}
	if _, err := q.ExecContext(ctx, "UPDATE messages SET search_fts = NULL"); err != nil {
		return fmt.Errorf("clear search_fts: %w", err)
	}
	if _, err := q.ExecContext(ctx,
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
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS source_is_from_me BOOLEAN`, "source_is_from_me"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS identity_is_from_me BOOLEAN NOT NULL DEFAULT FALSE`, "identity_is_from_me"},
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
		// content_changed_at: content-scoped change watermark. No default here
		// and none in schema_pg.sql, so on PostgreSQL the INSERT trigger is the
		// single writer for new rows, and the backfill seeds pre-existing rows
		// from last_modified. SQLite is not the same: a database created from
		// schema.sql stamps inserts from a column DEFAULT and gets no INSERT
		// trigger at all (SQLiteDialect.EnsureTriggers).
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS content_changed_at TIMESTAMPTZ`, "content_changed_at"},
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
	// Version the partial-index name as well as its predicate: IF NOT EXISTS
	// cannot alter the legacy NULL-only index on upgraded databases.
	staleIndexName := fmt.Sprintf("idx_messages_search_fts_stale_v%d", CurrentFTSIndexingVersion)
	if _, err := q.Exec(
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON messages (id) WHERE search_fts IS NULL OR indexing_version IS DISTINCT FROM %d", staleIndexName, CurrentFTSIndexingVersion),
	); err != nil {
		return fmt.Errorf("create %s: %w", staleIndexName, err)
	}
	return nil
}

// EnsureTriggers creates the maintenance triggers for BOTH message watermarks
// idempotently. The two answer different questions and are deliberately scoped
// differently.
//
// messages.last_modified is a TRUE row-level watermark: blanket by design, it
// moves on ANY change to a message row or its body, because the embed worker
// uses it as an optimistic-CAS token and must not miss a content edit that
// lands between its read and its stamp. Two triggers feed it:
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
// messages.content_changed_at is the opposite: both COLUMN-scoped and
// VALUE-scoped, so an external consumer maintaining an incremental copy of the
// archive is woken only when the message's own content, routing, or lifecycle
// actually changed — never by bookkeeping such as embed_gen or
// indexing_version. Four triggers feed it here, built from
// MessagesContentColumns so the SQLite and PostgreSQL definitions cannot drift
// (a fresh SQLite database runs three of them — see the first entry):
//
//   - trg_messages_content_changed_ins (BEFORE INSERT on messages): stamps new
//     rows. PostgreSQL's column carries no DEFAULT — neither in schema_pg.sql
//     nor on the ALTER TABLE upgrade path — so on this backend the trigger is
//     the single writer for new rows. SQLite is where that stops being true: a
//     database created from schema.sql gives the column a DEFAULT
//     byte-identical to SQLiteDialect.ContentChangedNow, and EnsureTriggers
//     there detects it (contentChangedAtDefaultStamps) and creates no INSERT
//     trigger at all, because a SQLite trigger cannot assign to NEW and merely
//     having a row trigger on messages costs every INSERT a statement journal.
//     So the single writer for new rows is this trigger on PostgreSQL, the
//     column DEFAULT on a fresh SQLite database, and SQLite's AFTER INSERT
//     trigger of the same name on a SQLite database upgraded by ALTER TABLE ADD
//     COLUMN, which cannot carry a non-constant DEFAULT. The WHEN guard yields
//     to an explicit write in the INSERT in every version.
//   - trg_messages_content_changed_at (BEFORE UPDATE OF <content columns> on
//     messages): UPDATE OF scopes it to the columns a statement NAMES, which
//     is not enough on its own — UpsertMessage's ON CONFLICT DO UPDATE
//     re-assigns ten content columns on every re-sync of a known message — so
//     contentChangedValueGuard additionally requires one of them to have
//     actually changed value. IS DISTINCT FROM makes that comparison null-safe
//     in both directions.
//   - trg_message_bodies_content_changed_ins / _upd (AFTER INSERT / AFTER
//     UPDATE on message_bodies): a body edit must reach the parent's
//     watermark. They stay two triggers rather than one AFTER INSERT OR
//     UPDATE because only the UPDATE half can carry a value guard (a WHEN
//     clause on an INSERT trigger cannot reference OLD), and the guard is
//     required: upsertMessageBody always executes its ON CONFLICT DO UPDATE,
//     even when messageBodyChanges reports nothing changed.
//
// CREATE TRIGGER is not idempotent before PG14, so each trigger is dropped
// (IF EXISTS) and recreated; the functions use CREATE OR REPLACE. Re-running
// InitSchema is therefore safe, and DROP + CREATE also lets a later change to
// the content-column list actually reach an already-deployed archive. Runs on
// the querier so InitSchema can route it through the maintenance transaction
// (consistent with EnsureFTSIndex).
func (d *PostgreSQLDialect) EnsureTriggers(q querier) error {
	cols := contentChangedTriggerColumnList()
	guard := contentChangedValueGuard("IS DISTINCT FROM")
	now := d.ContentChangedNow()
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
		// content_changed_at, from here down. BEFORE on messages so the stamp
		// lands in-row with no secondary write and therefore no recursion —
		// the same shape the last_modified message trigger uses, and the
		// reason the PostgreSQL definitions differ from their AFTER-trigger
		// SQLite counterparts.
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION set_messages_content_changed_at() RETURNS trigger AS $$
		 BEGIN
		     NEW.content_changed_at := %s;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`, now),
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_ins ON messages`,
		`CREATE TRIGGER trg_messages_content_changed_ins
		     BEFORE INSERT ON messages FOR EACH ROW
		     WHEN (NEW.content_changed_at IS NULL)
		     EXECUTE FUNCTION set_messages_content_changed_at()`,
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_at ON messages`,
		fmt.Sprintf(`CREATE TRIGGER trg_messages_content_changed_at
		     BEFORE UPDATE OF %s ON messages FOR EACH ROW
		     WHEN (OLD.content_changed_at IS NOT DISTINCT FROM NEW.content_changed_at AND %s)
		     EXECUTE FUNCTION set_messages_content_changed_at()`, cols, guard),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION bump_message_content_changed_at() RETURNS trigger AS $$
		 BEGIN
		     UPDATE messages SET content_changed_at = %s WHERE id = NEW.message_id;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_ins ON message_bodies`,
		`CREATE TRIGGER trg_message_bodies_content_changed_ins
		     AFTER INSERT ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION bump_message_content_changed_at()`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_upd ON message_bodies`,
		`CREATE TRIGGER trg_message_bodies_content_changed_upd
		     AFTER UPDATE ON message_bodies FOR EACH ROW
		     WHEN (OLD.body_text IS DISTINCT FROM NEW.body_text
		           OR OLD.body_html IS DISTINCT FROM NEW.body_html)
		     EXECUTE FUNCTION bump_message_content_changed_at()`,
	}
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure message watermark triggers: %w", err)
		}
	}
	return nil
}

// DatabaseSize queries pg_database_size() for the current database.
func (d *PostgreSQLDialect) DatabaseSize(
	ctx context.Context,
	db *sql.DB,
	_ string,
) (int64, error) {
	var size int64
	err := db.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&size)
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
	// persons and person_participants: MergeParticipants (reached from the
	// Beeper import path) repoints bindings and bumps person revisions, so
	// both belong to the sync/import write set this lock mirrors.
	"persons", "person_participants",
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
//
// Before LOCK TABLE, the transaction takes the identity-mutation row lock
// (the archive_metadata identity-revision row, mirroring
// lockIdentityMutationTx). Identity mutations acquire that row first and
// then write person tables BEFORE participants/messages (MergeParticipants
// repoints person bindings before repointing archive references), the
// opposite of this list's order — so without the shared row lock, a
// serialized source removal racing an importer-driven merge could deadlock:
// LOCK TABLE holds participants and waits on persons while the merge holds
// persons and waits on participants. Taking the row lock first serializes
// the two paths instead.
func (d *PostgreSQLDialect) BeginExclusive(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	rollback := func(err error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO archive_metadata (key, value) VALUES ($1, '0') ON CONFLICT DO NOTHING",
		identityRevisionKey); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx,
		"UPDATE archive_metadata SET value = value WHERE key = $1",
		identityRevisionKey); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx,
		"LOCK TABLE "+strings.Join(exclusiveLockTables, ", ")+" IN EXCLUSIVE MODE",
	); err != nil {
		return rollback(err)
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
