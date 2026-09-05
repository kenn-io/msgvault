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

func (d *PostgreSQLDialect) DriverName() string { return postgresDriverName }

// Rebind converts ? placeholders to PostgreSQL $1, $2, ... numbered
// placeholders. Delegates to sqldialect so the query package's
// PostgreSQLQueryDialect.Rebind stays in lockstep.
func (d *PostgreSQLDialect) Rebind(query string) string {
	return sqldialect.RebindPostgreSQL(query)
}

func (d *PostgreSQLDialect) UnicodeLowerExpression(expr string) string {
	return "LOWER(" + expr + ")"
}

func (d *PostgreSQLDialect) BlobPrefixSQL(column string) string {
	return "SUBSTRING(" + column + " FROM 1 FOR ?)"
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

// RFC822CanonicalIDExpr strips one clean angle-bracket pair from a stored
// Message-ID. PostgreSQL TEXT cannot contain embedded NUL bytes, so its native
// character-oriented LENGTH and SUBSTR semantics are sufficient.
func (d *PostgreSQLDialect) RFC822CanonicalIDExpr(col string) string {
	return fmt.Sprintf(`CASE
		WHEN LENGTH(%[1]s) > 2
		 AND SUBSTR(%[1]s, 1, 1) = '<'
		 AND SUBSTR(%[1]s, LENGTH(%[1]s), 1) = '>'
		 AND SUBSTR(%[1]s, 2, 1) NOT IN ('<', '>', ' ')
		 AND SUBSTR(%[1]s, LENGTH(%[1]s) - 1, 1) NOT IN ('<', '>', ' ')
			THEN SUBSTR(%[1]s, 2, LENGTH(%[1]s) - 2)
			ELSE %[1]s
	END`, col)
}

// RFC822CanonicalIDIndexDefinition defines the composite expression index over
// canonical Message-ID and source. A bare CASE is not valid PostgreSQL index
// syntax, so the expression has its required extra parenthesis pair. The
// functions in the expression are immutable and therefore indexable.
func (d *PostgreSQLDialect) RFC822CanonicalIDIndexDefinition() string {
	return fmt.Sprintf(
		"ON messages((%s), source_id)",
		d.RFC822CanonicalIDExpr("rfc822_message_id"),
	)
}

// JSONBindExpr returns "?::JSONB" — PG won't implicit-cast text to JSONB,
// so a bare placeholder bound to a Go string raises a column-type
// mismatch on the sources.sync_config write path.
func (d *PostgreSQLDialect) JSONBindExpr() string { return "?::JSONB" }

func (d *PostgreSQLDialect) JSONIsDistinctExpr(col string) string {
	return col + " IS DISTINCT FROM ?::JSONB"
}

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
		{`ALTER TABLE applied_migrations ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1`, "applied_migrations.version"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS backstop_upper_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_upper_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS backstop_after_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_after_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS optimistic_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.optimistic_document_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS reconcile_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.reconcile_document_key"},
		{`ALTER TABLE person_sweep_cursors ADD COLUMN IF NOT EXISTS backstop_document_key TEXT NOT NULL DEFAULT ''`, "person_sweep_cursors.backstop_document_key"},
		{`ALTER TABLE carddav_address_books ADD COLUMN IF NOT EXISTS needs_full_reconcile BOOLEAN NOT NULL DEFAULT FALSE`, "carddav_address_books.needs_full_reconcile"},
		{`ALTER TABLE carddav_address_books ADD COLUMN IF NOT EXISTS sync_token TEXT NOT NULL DEFAULT ''`, "carddav_address_books.sync_token"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN IF NOT EXISTS pending_operation TEXT CHECK (pending_operation IN ('delete'))`, "carddav_conflicts.pending_operation"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN IF NOT EXISTS connection_generation BIGINT`, "carddav_conflicts.connection_generation"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN IF NOT EXISTS book_sync_revision BIGINT`, "carddav_conflicts.book_sync_revision"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN IF NOT EXISTS previous_mapping_revision BIGINT`, "carddav_conflicts.previous_mapping_revision"},
		{`ALTER TABLE carddav_conflicts ADD COLUMN IF NOT EXISTS pending_started_at TIMESTAMPTZ`, "carddav_conflicts.pending_started_at"},
		{`ALTER TABLE sources ADD COLUMN IF NOT EXISTS sync_config JSONB`, "sync_config"},
		{`ALTER TABLE sync_runs ADD COLUMN IF NOT EXISTS sync_type TEXT NOT NULL DEFAULT ''`, "sync_runs.sync_type"},
		{`ALTER TABLE sync_runs ADD COLUMN IF NOT EXISTS request_fingerprint TEXT`, "sync_runs.request_fingerprint"},
		{`ALTER TABLE sync_runs ADD COLUMN IF NOT EXISTS operation_id TEXT`, "sync_runs.operation_id"},
		{`ALTER TABLE imap_folder_state ADD COLUMN IF NOT EXISTS highest_modseq NUMERIC(20, 0) NOT NULL DEFAULT 0`, "imap_folder_state.highest_modseq"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE messages ADD COLUMN IF NOT EXISTS list_id TEXT`, "list_id"},
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
		{`ALTER TABLE labels ADD COLUMN IF NOT EXISTS system_role TEXT`, "labels.system_role"},
		{`ALTER TABLE participant_identifiers ADD COLUMN IF NOT EXISTS service_id BIGINT REFERENCES communication_services(id) ON DELETE SET NULL`, "pi_service_id"},
		{`ALTER TABLE participant_identifiers ADD COLUMN IF NOT EXISTS scope_kind TEXT`, "pi_scope_kind"},
		{`ALTER TABLE participant_identifiers ADD COLUMN IF NOT EXISTS scope_value TEXT`, "pi_scope_value"},
		{postgresParticipantLinkIdentityMatchCandidateMigration, "participant_links.identity_match_candidate_id"},
		{postgresIdentityMatchObservationConflictOriginMigration, identityMatchObservationConflictOriginMigrationDesc},
		{postgresIdentityMatchCandidateSourcesMigration, identityMatchCandidateSourcesTableName},
		{postgresIdentityMatchEvidenceSourcesMigration, identityMatchEvidenceSourcesTableName},
		{postgresIdentityMatchPreConflictStateMigration, "identity_match_candidates.pre_conflict_state"},
		{postgresIdentityMatchApplicationPendingMigration, "identity_match_candidates.application_pending"},
		{`ALTER TABLE embedding_changes ADD COLUMN IF NOT EXISTS old_message_type TEXT`, "embedding_changes.old_message_type"},
		{`ALTER TABLE embedding_changes ADD COLUMN IF NOT EXISTS new_message_type TEXT`, "embedding_changes.new_message_type"},
		{`ALTER TABLE embedding_change_clock ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT FALSE`, "embedding_change_clock.enabled"},
		{`ALTER TABLE attribute_definitions ADD COLUMN IF NOT EXISTS is_sensitive BOOLEAN NOT NULL DEFAULT FALSE`, "attribute_definitions.is_sensitive"},
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
		// email_address: immutable envelope address for identity discovery.
		// Legacy rows stay NULL (unfillable without re-parsing raw MIME) and
		// discovery falls back to the participant's email for them.
		{`ALTER TABLE message_recipients ADD COLUMN IF NOT EXISTS email_address TEXT`, "message_recipients.email_address"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS attachment_role TEXT NOT NULL DEFAULT 'unknown' CHECK (attachment_role IN ('standalone', 'inline', 'avatar', 'thumbnail', 'preview', 'sticker', 'ui_asset', 'unknown'))`, "attachments.attachment_role"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS role_source TEXT NOT NULL DEFAULT 'unknown' CHECK (role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'legacy_api', 'raw_mime_repair', 'unknown'))`, "attachments.role_source"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS source_part_key TEXT CHECK (source_part_key IS NULL OR source_part_key != '')`, "attachments.source_part_key"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS content_id TEXT`, "attachments.content_id"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS rebuild_id TEXT REFERENCES document_extraction_rebuilds(id) ON DELETE SET NULL`, "document_extractions.rebuild_id"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS request_count INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0)`, "document_extractions.request_count"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0 AND retry_count <= request_count)`, "document_extractions.retry_count"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS provider_latency_ms BIGINT NOT NULL DEFAULT 0 CHECK (provider_latency_ms >= 0)`, "document_extractions.provider_latency_ms"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS normalization_version INTEGER`, "document_extractions.normalization_version"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS document_family TEXT`, "document_extractions.document_family"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS unit_kind TEXT`, "document_extractions.unit_kind"},
		{`ALTER TABLE document_extractions ADD COLUMN IF NOT EXISTS normalized_truncated BOOLEAN NOT NULL DEFAULT FALSE`, "document_extractions.normalized_truncated"},
		{`ALTER TABLE document_units ADD COLUMN IF NOT EXISTS heading_marks JSONB NOT NULL DEFAULT '[]'::jsonb`, "document_units.heading_marks"},
		{`ALTER TABLE document_index_state ADD COLUMN IF NOT EXISTS target_profile_id TEXT`, "document_index_state.target_profile_id"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS attachment_state TEXT`, "attachments.attachment_state"},
		{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS attachment_skip_reason TEXT`, "attachments.attachment_skip_reason"},
		// vcard_projection_revision: the lock and change token native vCard
		// envelope commits serialize on. Existing rows take the same DEFAULT 1
		// as fresh ones; the absolute value never matters, only that a
		// projection write moves it, so no backfill is needed.
		{`ALTER TABLE persons ADD COLUMN IF NOT EXISTS vcard_projection_revision BIGINT NOT NULL DEFAULT 1`,
			"persons.vcard_projection_revision"},
		{`ALTER TABLE person_names ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_names.source_resource_uid"},
		{`ALTER TABLE person_contact_points ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_contact_points.source_resource_uid"},
		{`ALTER TABLE person_addresses ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_addresses.source_resource_uid"},
		{`ALTER TABLE person_dates ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_dates.source_resource_uid"},
		{`ALTER TABLE person_categories ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_categories.source_resource_uid"},
		{`ALTER TABLE person_media ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_media.source_resource_uid"},
		{`ALTER TABLE participant_contact_observations ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "participant_contact_observations.source_resource_uid"},
		{`ALTER TABLE organization_names ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_names.source_resource_uid"},
		{`ALTER TABLE organization_identifiers ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_identifiers.source_resource_uid"},
		{`ALTER TABLE organization_addresses ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_addresses.source_resource_uid"},
		{`ALTER TABLE organization_contact_points ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_contact_points.source_resource_uid"},
		{`ALTER TABLE organization_categories ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_categories.source_resource_uid"},
		{`ALTER TABLE organization_media ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "organization_media.source_resource_uid"},
		{`ALTER TABLE person_relationships ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_relationships.source_resource_uid"},
		{`ALTER TABLE person_relationship_reviews ADD COLUMN IF NOT EXISTS source_resource_uid TEXT`, "person_relationship_reviews.source_resource_uid"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN IF NOT EXISTS targets_json TEXT`, "person_enrichment_attempts.targets_json"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN IF NOT EXISTS provider_started_at TIMESTAMPTZ`, "person_enrichment_attempts.provider_started_at"},
		{`ALTER TABLE person_enrichment_attempts ADD COLUMN IF NOT EXISTS dispatch_authorized_at TIMESTAMPTZ`, "person_enrichment_attempts.dispatch_authorized_at"},
		{`ALTER TABLE person_enrichment_work ADD COLUMN IF NOT EXISTS has_fresh_trigger BOOLEAN NOT NULL DEFAULT FALSE`, "person_enrichment_work.has_fresh_trigger"},
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

func (d *PostgreSQLDialect) ValidateMessageWatermarks(querier) error { return nil }

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
		// Deletion is a content change: without the bump, a body deleted
		// between a visual context snapshot and its claim passes the
		// content-stamp CAS and publishes an embedding of the deleted text.
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION bump_message_content_changed_at_del() RETURNS trigger AS $$
		 BEGIN
		     UPDATE messages SET content_changed_at = %s WHERE id = OLD.message_id;
		     RETURN OLD;
		 END;
		 $$ LANGUAGE plpgsql`, now),
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_del ON message_bodies`,
		`CREATE TRIGGER trg_message_bodies_content_changed_del
		     AFTER DELETE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION bump_message_content_changed_at_del()`,
		// Source mutations share this transaction-level advisory lock. Contextual
		// activation takes its exclusive form before reading the journal clock, so
		// neither side can hold a source row while waiting for the other side.
		`CREATE OR REPLACE FUNCTION lock_embedding_change_clock() RETURNS trigger AS $$
		 BEGIN
		     PERFORM pg_advisory_xact_lock_shared(
		         hashtextextended('msgvault.embedding_change_clock', 0));
		     RETURN NULL;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_clock_messages ON messages`,
		`CREATE TRIGGER trg_embedding_clock_messages
		     BEFORE INSERT OR UPDATE OR DELETE ON messages FOR EACH STATEMENT
		     EXECUTE FUNCTION lock_embedding_change_clock()`,
		`DROP TRIGGER IF EXISTS trg_embedding_clock_bodies ON message_bodies`,
		`CREATE TRIGGER trg_embedding_clock_bodies
		     BEFORE INSERT OR UPDATE OR DELETE ON message_bodies FOR EACH STATEMENT
		     EXECUTE FUNCTION lock_embedding_change_clock()`,
		`DROP TRIGGER IF EXISTS trg_embedding_clock_conversations ON conversations`,
		`CREATE TRIGGER trg_embedding_clock_conversations
		     BEFORE UPDATE OF title ON conversations FOR EACH STATEMENT
		     EXECUTE FUNCTION lock_embedding_change_clock()`,
		`DROP TRIGGER IF EXISTS trg_embedding_clock_membership ON conversation_participants`,
		`CREATE TRIGGER trg_embedding_clock_membership
		     BEFORE INSERT OR UPDATE OR DELETE ON conversation_participants FOR EACH STATEMENT
		     EXECUTE FUNCTION lock_embedding_change_clock()`,
		`DROP TRIGGER IF EXISTS trg_embedding_clock_participants ON participants`,
		`CREATE TRIGGER trg_embedding_clock_participants
		     BEFORE UPDATE OF display_name, email_address, phone_number ON participants FOR EACH STATEMENT
		     EXECUTE FUNCTION lock_embedding_change_clock()`,
		`DROP FUNCTION IF EXISTS append_embedding_change(TEXT, BIGINT, BIGINT, BIGINT, TIMESTAMPTZ, TIMESTAMPTZ, BIGINT)`,
		`CREATE OR REPLACE FUNCTION append_embedding_change(
		     p_kind TEXT,
		     p_message_id BIGINT,
		     p_old_message_type TEXT,
		     p_new_message_type TEXT,
		     p_old_conversation_id BIGINT,
		     p_new_conversation_id BIGINT,
		     p_old_sent_at TIMESTAMPTZ,
		     p_new_sent_at TIMESTAMPTZ,
		     p_participant_id BIGINT
		 ) RETURNS VOID AS $$
		 DECLARE
		     next_sequence BIGINT;
		 BEGIN
		     UPDATE embedding_change_clock
		        SET sequence = sequence + 1
		      WHERE singleton = 1 AND enabled
		      RETURNING sequence INTO next_sequence;
		     IF next_sequence IS NULL THEN
		         RETURN;
		     END IF;
		     INSERT INTO embedding_changes (
		         sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		         new_conversation_id, old_sent_at, new_sent_at, participant_id
		     ) VALUES (
		         next_sequence, p_kind, p_message_id,
		         p_old_message_type, p_new_message_type,
		         p_old_conversation_id, p_new_conversation_id,
		         p_old_sent_at, p_new_sent_at, p_participant_id
		     );
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION journal_contextual_message_change() RETURNS trigger AS $$
		 BEGIN
		     IF TG_OP = 'DELETE' THEN
		         IF OLD.deleted_at IS NULL
		            AND OLD.deleted_from_source_at IS NULL
		            AND (
		                COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		                OR EXISTS (SELECT 1 FROM message_bodies WHERE message_id = OLD.id)
		            ) THEN
		             PERFORM append_embedding_change(
		                 'message_delete', OLD.id, OLD.message_type, NULL,
		                 OLD.conversation_id, NULL,
		                 COALESCE(OLD.sent_at, OLD.received_at, OLD.internal_date), NULL, NULL);
		         END IF;
		         RETURN OLD;
		     END IF;

		     IF (EXISTS (
		         SELECT 1 FROM message_bodies WHERE message_id = NEW.id
		     ) AND (
		         (
		             (
		                 (OLD.message_type IN ('beeper', 'meeting_transcript') AND OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                 OR (NEW.message_type IN ('beeper', 'meeting_transcript') AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		                 OR (
		                     OLD.message_type IS DISTINCT FROM NEW.message_type
		                     AND (
		                         (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                         OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		                     )
		                 )
		             ) AND (
		                 OLD.message_type IS DISTINCT FROM NEW.message_type
		                 OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		                 OR OLD.sent_at IS DISTINCT FROM NEW.sent_at
		                 OR OLD.received_at IS DISTINCT FROM NEW.received_at
		                 OR OLD.internal_date IS DISTINCT FROM NEW.internal_date
		                 OR OLD.sender_id IS DISTINCT FROM NEW.sender_id
		                 OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
		                 OR OLD.deleted_from_source_at IS DISTINCT FROM NEW.deleted_from_source_at
		             )
		         ) OR (
		             COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		             AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		             AND (
		                 (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		                 OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		             ) AND (
		                 OLD.message_type IS DISTINCT FROM NEW.message_type
		                 OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		                 OR OLD.subject IS DISTINCT FROM NEW.subject
		                 OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
		                 OR OLD.deleted_from_source_at IS DISTINCT FROM NEW.deleted_from_source_at
		             )
		         )
		     )) OR (
		         OLD.message_type IS DISTINCT FROM NEW.message_type
		         AND (
		             (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		             OR (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL)
		         )
		     ) OR (
		         COALESCE(OLD.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		         AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		         AND (
		             OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
		             OR OLD.deleted_from_source_at IS DISTINCT FROM NEW.deleted_from_source_at
		         )
		     ) OR (
		         OLD.embed_gen IS NOT NULL
		         AND NEW.embed_gen IS NULL
		         AND COALESCE(NEW.message_type, '') NOT IN ('beeper', 'meeting_transcript')
		         AND NEW.deleted_at IS NULL
		         AND NEW.deleted_from_source_at IS NULL
		         AND NOT EXISTS (SELECT 1 FROM message_bodies WHERE message_id = NEW.id)
		     ) THEN
		         PERFORM append_embedding_change(
		             'message_update', NEW.id,
		             CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  THEN OLD.message_type END,
		             CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                  THEN NEW.message_type END,
		             CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  THEN OLD.conversation_id END,
		             CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                  THEN NEW.conversation_id END,
		             CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  THEN COALESCE(OLD.sent_at, OLD.received_at, OLD.internal_date) END,
		             CASE WHEN NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                  THEN COALESCE(NEW.sent_at, NEW.received_at, NEW.internal_date) END,
		             NULL);
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_messages ON messages`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_message_insert ON messages`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_message_delete ON messages`,
		`CREATE TRIGGER trg_embedding_changes_messages
		     AFTER UPDATE ON messages FOR EACH ROW
		     EXECUTE FUNCTION journal_contextual_message_change()`,
		`CREATE TRIGGER trg_embedding_changes_message_delete
		     BEFORE DELETE ON messages FOR EACH ROW
		     EXECUTE FUNCTION journal_contextual_message_change()`,
		`CREATE OR REPLACE FUNCTION journal_contextual_body_change() RETURNS trigger AS $$
		 DECLARE
		     change_message_id BIGINT;
		     change_message_type TEXT;
		     change_conversation_id BIGINT;
		     change_sent_at TIMESTAMPTZ;
		     body_message_id BIGINT;
		 BEGIN
		     IF TG_OP = 'DELETE' THEN
		         body_message_id := OLD.message_id;
		     ELSE
		         body_message_id := NEW.message_id;
		     END IF;
		     IF TG_OP = 'UPDATE'
		        AND OLD.body_text IS NOT DISTINCT FROM NEW.body_text
		        AND OLD.body_html IS NOT DISTINCT FROM NEW.body_html THEN
		         RETURN NEW;
		     END IF;
		     SELECT id, message_type, conversation_id, COALESCE(sent_at, received_at, internal_date)
		       INTO change_message_id, change_message_type, change_conversation_id, change_sent_at
		       FROM messages
		      WHERE id = body_message_id
		        AND deleted_at IS NULL
		        AND deleted_from_source_at IS NULL;
		     IF FOUND THEN
		         IF TG_OP = 'INSERT' THEN
		             PERFORM append_embedding_change(
		                 'message_insert', change_message_id, NULL, change_message_type,
		                 NULL, change_conversation_id,
		                 NULL, change_sent_at, NULL);
		         ELSIF TG_OP = 'DELETE' THEN
		             PERFORM append_embedding_change(
		                 'message_body', change_message_id, change_message_type, NULL,
		                 change_conversation_id, NULL,
		                 change_sent_at, NULL, NULL);
		         ELSE
		             PERFORM append_embedding_change(
		                 'message_body', change_message_id, change_message_type, change_message_type,
		                 change_conversation_id, change_conversation_id,
		                 change_sent_at, change_sent_at, NULL);
		         END IF;
		     END IF;
		     IF TG_OP = 'DELETE' THEN
		         RETURN OLD;
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_bodies ON message_bodies`,
		`CREATE TRIGGER trg_embedding_changes_bodies
		     AFTER INSERT OR UPDATE OR DELETE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION journal_contextual_body_change()`,
		`CREATE OR REPLACE FUNCTION journal_beeper_conversation_title() RETURNS trigger AS $$
		 BEGIN
		     IF OLD.title IS DISTINCT FROM NEW.title
		        AND EXISTS (
		            SELECT 1 FROM messages m
		            JOIN message_bodies mb ON mb.message_id = m.id
		             WHERE m.conversation_id = NEW.id
		               AND m.message_type = 'beeper'
		               AND m.deleted_at IS NULL
		               AND m.deleted_from_source_at IS NULL
		        ) THEN
		         PERFORM append_embedding_change(
		             'conversation_title', NULL, NULL, NULL,
		             OLD.id, NEW.id, NULL, NULL, NULL);
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_conversation_title ON conversations`,
		`CREATE TRIGGER trg_embedding_changes_conversation_title
		     AFTER UPDATE OF title ON conversations FOR EACH ROW
		     EXECUTE FUNCTION journal_beeper_conversation_title()`,
		`CREATE OR REPLACE FUNCTION journal_beeper_membership_change() RETURNS trigger AS $$
		 DECLARE
		     old_conversation BIGINT;
		     new_conversation BIGINT;
		     change_participant BIGINT;
		     affects_context BOOLEAN;
		 BEGIN
		     IF TG_OP = 'INSERT' THEN
		         old_conversation := NEW.conversation_id;
		         new_conversation := NEW.conversation_id;
		         change_participant := NEW.participant_id;
		     ELSIF TG_OP = 'DELETE' THEN
		         old_conversation := OLD.conversation_id;
		         new_conversation := OLD.conversation_id;
		         change_participant := OLD.participant_id;
		     ELSE
		         IF OLD.conversation_id IS NOT DISTINCT FROM NEW.conversation_id
		            AND OLD.participant_id IS NOT DISTINCT FROM NEW.participant_id
		            AND OLD.role IS NOT DISTINCT FROM NEW.role
		            AND OLD.joined_at IS NOT DISTINCT FROM NEW.joined_at
		            AND OLD.left_at IS NOT DISTINCT FROM NEW.left_at THEN
		             RETURN NEW;
		         END IF;
		         old_conversation := OLD.conversation_id;
		         new_conversation := NEW.conversation_id;
		         change_participant := NEW.participant_id;
		     END IF;
		     SELECT EXISTS (
		         SELECT 1 FROM messages m
		         JOIN message_bodies mb ON mb.message_id = m.id
		          WHERE m.conversation_id IN (old_conversation, new_conversation)
		            AND m.message_type = 'beeper'
		            AND m.deleted_at IS NULL
		     ) INTO affects_context;
		     IF affects_context THEN
		         PERFORM append_embedding_change(
		             'conversation_participant', NULL, NULL, NULL,
		             old_conversation, new_conversation,
		             NULL, NULL, change_participant);
		     END IF;
		     IF TG_OP = 'DELETE' THEN
		         RETURN OLD;
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_membership ON conversation_participants`,
		`CREATE TRIGGER trg_embedding_changes_membership
		     AFTER INSERT OR UPDATE OR DELETE ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION journal_beeper_membership_change()`,
		`CREATE OR REPLACE FUNCTION journal_beeper_participant_display_name() RETURNS trigger AS $$
		 BEGIN
		     IF COALESCE(NULLIF(TRIM(OLD.display_name), ''),
		                 NULLIF(OLD.email_address, ''), NULLIF(OLD.phone_number, ''), '')
		        IS DISTINCT FROM COALESCE(NULLIF(TRIM(NEW.display_name), ''),
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
		        ) THEN
		         PERFORM append_embedding_change(
		             'participant_display_name', NULL, NULL, NULL,
		             NULL, NULL, NULL, NULL, NEW.id);
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_embedding_changes_participant_display_name ON participants`,
		`CREATE TRIGGER trg_embedding_changes_participant_display_name
		     AFTER UPDATE OF display_name, email_address, phone_number ON participants FOR EACH ROW
		     EXECUTE FUNCTION journal_beeper_participant_display_name()`,
		`CREATE OR REPLACE FUNCTION capture_attachment_change() RETURNS trigger AS $$
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM attachment_change_consumers) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' THEN
		         INSERT INTO attachment_change_log
		             (event_kind, new_message_id, new_attachment_id,
		              new_content_hash, new_source_part_key, new_role)
		         VALUES ('attachment_insert', NEW.message_id, NEW.id,
		                 NEW.content_hash, NEW.source_part_key, NEW.attachment_role);
		         RETURN NEW;
		     ELSIF TG_OP = 'UPDATE' THEN
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
		         RETURN NEW;
		     ELSE
		         INSERT INTO attachment_change_log
		             (event_kind, old_message_id, old_attachment_id,
		              old_content_hash, old_source_part_key, old_role)
		         VALUES ('attachment_delete', OLD.message_id, OLD.id,
		                 OLD.content_hash, OLD.source_part_key, OLD.attachment_role);
		         RETURN OLD;
		     END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_insert ON attachments`,
		`CREATE TRIGGER trg_attachment_change_insert
		     AFTER INSERT ON attachments FOR EACH ROW
		     EXECUTE FUNCTION capture_attachment_change()`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_update ON attachments`,
		`CREATE TRIGGER trg_attachment_change_update
		     AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		         storage_path, media_type, width, height, duration_ms,
		         source_attachment_id, attachment_metadata, attachment_role,
		         role_source, source_part_key, content_id, encryption_version
		     ON attachments FOR EACH ROW
		     WHEN (OLD.message_id IS DISTINCT FROM NEW.message_id
		        OR OLD.filename IS DISTINCT FROM NEW.filename
		        OR OLD.mime_type IS DISTINCT FROM NEW.mime_type
		        OR OLD.size IS DISTINCT FROM NEW.size
		        OR OLD.content_hash IS DISTINCT FROM NEW.content_hash
		        OR OLD.storage_path IS DISTINCT FROM NEW.storage_path
		        OR OLD.media_type IS DISTINCT FROM NEW.media_type
		        OR OLD.width IS DISTINCT FROM NEW.width
		        OR OLD.height IS DISTINCT FROM NEW.height
		        OR OLD.duration_ms IS DISTINCT FROM NEW.duration_ms
		        OR OLD.source_attachment_id IS DISTINCT FROM NEW.source_attachment_id
		        OR OLD.attachment_metadata IS DISTINCT FROM NEW.attachment_metadata
		        OR OLD.attachment_role IS DISTINCT FROM NEW.attachment_role
		        OR OLD.role_source IS DISTINCT FROM NEW.role_source
		        OR OLD.source_part_key IS DISTINCT FROM NEW.source_part_key
		        OR OLD.content_id IS DISTINCT FROM NEW.content_id
		        OR OLD.encryption_version IS DISTINCT FROM NEW.encryption_version)
		     EXECUTE FUNCTION capture_attachment_change()`,
		`DROP TRIGGER IF EXISTS trg_attachment_change_delete ON attachments`,
		`CREATE TRIGGER trg_attachment_change_delete
		     AFTER DELETE ON attachments FOR EACH ROW
		     EXECUTE FUNCTION capture_attachment_change()`,
		`CREATE OR REPLACE FUNCTION capture_attachment_message_live_change() RETURNS trigger AS $$
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM attachment_change_consumers) THEN
		         RETURN NEW;
		     END IF;
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
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_attachment_message_live_change ON messages`,
		`CREATE TRIGGER trg_attachment_message_live_change
		     AFTER UPDATE OF deleted_at, deleted_from_source_at ON messages FOR EACH ROW
		     WHEN ((OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		           IS DISTINCT FROM
		           (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL))
		     EXECUTE FUNCTION capture_attachment_message_live_change()`,
		`CREATE OR REPLACE FUNCTION invalidate_visual_publication_attachment() RETURNS trigger AS $$
		 BEGIN
		     IF TG_OP <> 'INSERT' THEN
		         INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		         SELECT vp.generation_id, vp.pending_vector_token FROM visual_publications vp
		         WHERE vp.pending_vector_token IS NOT NULL
		           AND vp.message_id = OLD.message_id
		           AND (vp.blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		                OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                    AND LOWER(OLD.storage_path) =
		                        SUBSTRING(vp.blob_hash FROM 1 FOR 2) || '/' || vp.blob_hash))
		         ON CONFLICT (generation_id, vector_token) DO NOTHING;
		         UPDATE visual_publications vp
		         SET state = CASE WHEN EXISTS (
		                 SELECT 1 FROM attachments a
		                 JOIN messages m ON m.id = a.message_id
		                 WHERE a.message_id = vp.message_id
		                   AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		                   AND a.attachment_role = 'standalone'
		                   AND a.role_source IN ('mime_disposition', 'provider_explicit',
		                                         'importer_semantics', 'raw_mime_repair')
		                   AND (LOWER(COALESCE(a.content_hash, '')) = vp.blob_hash
		                        OR ((a.content_hash IS NULL OR a.content_hash = '')
		                            AND LOWER(a.storage_path) =
		                                SUBSTRING(vp.blob_hash FROM 1 FOR 2) || '/' || vp.blob_hash))
		             ) THEN 'stale' ELSE 'tombstoned' END,
		             pending_vector_token = NULL,
		             updated_at = CURRENT_TIMESTAMP
		         WHERE vp.message_id = OLD.message_id
		           AND (vp.blob_hash = LOWER(COALESCE(OLD.content_hash, ''))
		                OR ((OLD.content_hash IS NULL OR OLD.content_hash = '')
		                    AND LOWER(OLD.storage_path) =
		                        SUBSTRING(vp.blob_hash FROM 1 FOR 2) || '/' || vp.blob_hash));
		     END IF;
		     IF TG_OP <> 'DELETE'
		        AND NEW.attachment_role = 'standalone'
		        AND NEW.role_source IN ('mime_disposition', 'provider_explicit',
		                                'importer_semantics', 'raw_mime_repair')
		        AND EXISTS (SELECT 1 FROM messages m WHERE m.id = NEW.message_id
		                    AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL) THEN
		         INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		         SELECT vp.generation_id, vp.pending_vector_token FROM visual_publications vp
		         WHERE vp.pending_vector_token IS NOT NULL
		           AND vp.message_id = NEW.message_id
		           AND (vp.blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		                OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                    AND LOWER(NEW.storage_path) =
		                        SUBSTRING(vp.blob_hash FROM 1 FOR 2) || '/' || vp.blob_hash))
		         ON CONFLICT (generation_id, vector_token) DO NOTHING;
		         UPDATE visual_publications vp
		         SET state = 'stale', pending_vector_token = NULL, updated_at = CURRENT_TIMESTAMP
		         WHERE vp.message_id = NEW.message_id
		           AND (vp.blob_hash = LOWER(COALESCE(NEW.content_hash, ''))
		                OR ((NEW.content_hash IS NULL OR NEW.content_hash = '')
		                    AND LOWER(NEW.storage_path) =
		                        SUBSTRING(vp.blob_hash FROM 1 FOR 2) || '/' || vp.blob_hash));
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_insert ON attachments`,
		`CREATE TRIGGER trg_visual_publication_attachment_insert
		     AFTER INSERT ON attachments FOR EACH ROW
		     EXECUTE FUNCTION invalidate_visual_publication_attachment()`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_update ON attachments`,
		`CREATE TRIGGER trg_visual_publication_attachment_update
		     AFTER UPDATE OF message_id, filename, mime_type, size, content_hash,
		         storage_path, media_type, width, height, duration_ms,
		         source_attachment_id, attachment_metadata, attachment_role,
		         role_source, source_part_key, content_id, encryption_version
		     ON attachments FOR EACH ROW
		     WHEN (OLD.message_id IS DISTINCT FROM NEW.message_id
		        OR OLD.filename IS DISTINCT FROM NEW.filename
		        OR OLD.mime_type IS DISTINCT FROM NEW.mime_type
		        OR OLD.size IS DISTINCT FROM NEW.size
		        OR OLD.content_hash IS DISTINCT FROM NEW.content_hash
		        OR OLD.storage_path IS DISTINCT FROM NEW.storage_path
		        OR OLD.media_type IS DISTINCT FROM NEW.media_type
		        OR OLD.width IS DISTINCT FROM NEW.width
		        OR OLD.height IS DISTINCT FROM NEW.height
		        OR OLD.duration_ms IS DISTINCT FROM NEW.duration_ms
		        OR OLD.source_attachment_id IS DISTINCT FROM NEW.source_attachment_id
		        OR OLD.attachment_metadata IS DISTINCT FROM NEW.attachment_metadata
		        OR OLD.attachment_role IS DISTINCT FROM NEW.attachment_role
		        OR OLD.role_source IS DISTINCT FROM NEW.role_source
		        OR OLD.source_part_key IS DISTINCT FROM NEW.source_part_key
		        OR OLD.content_id IS DISTINCT FROM NEW.content_id
		        OR OLD.encryption_version IS DISTINCT FROM NEW.encryption_version)
		     EXECUTE FUNCTION invalidate_visual_publication_attachment()`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_attachment_delete ON attachments`,
		`CREATE TRIGGER trg_visual_publication_attachment_delete
		     AFTER DELETE ON attachments FOR EACH ROW
		     EXECUTE FUNCTION invalidate_visual_publication_attachment()`,
		`CREATE OR REPLACE FUNCTION ledger_visual_publication_tokens() RETURNS trigger AS $$
		 BEGIN
		     IF OLD.current_vector_token IS NOT NULL THEN
		         INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		         VALUES (OLD.generation_id, OLD.current_vector_token)
		         ON CONFLICT (generation_id, vector_token) DO NOTHING;
		     END IF;
		     IF OLD.pending_vector_token IS NOT NULL THEN
		         INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		         VALUES (OLD.generation_id, OLD.pending_vector_token)
		         ON CONFLICT (generation_id, vector_token) DO NOTHING;
		     END IF;
		     RETURN OLD;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_delete_ledger ON visual_publications`,
		`CREATE TRIGGER trg_visual_publication_delete_ledger
		     BEFORE DELETE ON visual_publications FOR EACH ROW
		     EXECUTE FUNCTION ledger_visual_publication_tokens()`,
		`CREATE OR REPLACE FUNCTION invalidate_visual_publication_message_live() RETURNS trigger AS $$
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
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_live_change ON messages`,
		`CREATE TRIGGER trg_visual_publication_message_live_change
		     AFTER UPDATE OF deleted_at, deleted_from_source_at ON messages FOR EACH ROW
		     WHEN ((OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		           IS DISTINCT FROM
		           (NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL))
		     EXECUTE FUNCTION invalidate_visual_publication_message_live()`,
		`CREATE OR REPLACE FUNCTION invalidate_visual_publication_message_content() RETURNS trigger AS $$
		 BEGIN
		     INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		     SELECT generation_id, pending_vector_token FROM visual_publications
		     WHERE pending_vector_token IS NOT NULL AND message_id = NEW.id
		     ON CONFLICT (generation_id, vector_token) DO NOTHING;
		     UPDATE visual_publications
		     SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		         prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		         pending_vector_token = NULL,
		         updated_at = CURRENT_TIMESTAMP
		     WHERE message_id = NEW.id AND state <> 'tombstoned'
		       AND (state = 'current' OR prepared_revision IS NOT NULL
		            OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_content_change ON messages`,
		`CREATE TRIGGER trg_visual_publication_message_content_change
		     AFTER UPDATE OF subject, message_type ON messages FOR EACH ROW
		     WHEN (OLD.subject IS DISTINCT FROM NEW.subject
		           OR OLD.message_type IS DISTINCT FROM NEW.message_type)
		     EXECUTE FUNCTION invalidate_visual_publication_message_content()`,
		`CREATE OR REPLACE FUNCTION seed_visual_publication_scope_entry() RETURNS trigger AS $$
		 BEGIN
		     INSERT INTO visual_publications
		         (generation_id, message_id, blob_hash, media_input_key,
		          source_fence, attachment_role, role_source, state)
     SELECT vg.id, a.message_id,
		            CASE WHEN LOWER(COALESCE(a.content_hash, '')) ~ '^[0-9a-f]{64}$'
		                 THEN LOWER(a.content_hash)
		                 ELSE SUBSTRING(a.storage_path FROM 4) END,
		            'original', 0, 'unknown', 'unknown', 'stale'
		     FROM attachments a JOIN visual_generations vg
		          ON vg.state IN ('building', 'active')
		     WHERE a.message_id = NEW.id
		       AND a.attachment_role = 'standalone'
		       AND a.role_source IN ('mime_disposition', 'provider_explicit',
		                             'importer_semantics', 'raw_mime_repair')
		       AND (LOWER(COALESCE(a.content_hash, '')) ~ '^[0-9a-f]{64}$'
		            OR (COALESCE(a.content_hash, '') = ''
		                AND COALESCE(a.storage_path, '') ~ '^[0-9a-f]{2}/[0-9a-f]{64}$'
		                AND SUBSTRING(a.storage_path FROM 1 FOR 2) = SUBSTRING(a.storage_path FROM 4 FOR 2)))
		     ON CONFLICT (generation_id, message_id, blob_hash, media_input_key) DO UPDATE SET
		         state = 'stale',
		         outcome_kind = NULL, outcome_reason = NULL,
		         prepared_revision = NULL,
		         updated_at = CURRENT_TIMESTAMP
		     WHERE visual_publications.state = 'tombstoned';
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_type_scope ON messages`,
		`CREATE TRIGGER trg_visual_publication_message_type_scope
		     AFTER UPDATE OF message_type ON messages FOR EACH ROW
		     WHEN (OLD.message_type IS DISTINCT FROM NEW.message_type)
		     EXECUTE FUNCTION seed_visual_publication_scope_entry()`,
		`CREATE OR REPLACE FUNCTION invalidate_visual_publication_message_body() RETURNS trigger AS $$
		 DECLARE target_message_id BIGINT;
		 BEGIN
		     IF TG_OP = 'DELETE' THEN
		         target_message_id := OLD.message_id;
		     ELSE
		         target_message_id := NEW.message_id;
		     END IF;
		     INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		     SELECT generation_id, pending_vector_token FROM visual_publications
		     WHERE pending_vector_token IS NOT NULL AND message_id = target_message_id
		     ON CONFLICT (generation_id, vector_token) DO NOTHING;
		     UPDATE visual_publications
		     SET state = CASE WHEN state = 'current' THEN 'stale' ELSE state END,
		         prepared_revision = NULL, outcome_kind = NULL, outcome_reason = NULL,
		         pending_vector_token = NULL,
		         updated_at = CURRENT_TIMESTAMP
		     WHERE message_id = target_message_id AND state <> 'tombstoned'
		       AND (state = 'current' OR prepared_revision IS NOT NULL
		            OR pending_vector_token IS NOT NULL OR outcome_kind IS NOT NULL);
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_insert ON message_bodies`,
		`CREATE TRIGGER trg_visual_publication_message_body_insert
		     AFTER INSERT ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION invalidate_visual_publication_message_body()`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_delete ON message_bodies`,
		`CREATE TRIGGER trg_visual_publication_message_body_delete
		     AFTER DELETE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION invalidate_visual_publication_message_body()`,
		`DROP TRIGGER IF EXISTS trg_visual_publication_message_body_update ON message_bodies`,
		`CREATE TRIGGER trg_visual_publication_message_body_update
		     AFTER UPDATE OF body_text, body_html ON message_bodies FOR EACH ROW
		     WHEN (OLD.body_text IS DISTINCT FROM NEW.body_text
		           OR OLD.body_html IS DISTINCT FROM NEW.body_html)
		     EXECUTE FUNCTION invalidate_visual_publication_message_body()`,
	}
	stmts = append(stmts, personSweepPostgreSQLTriggerStatements()...)
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure message watermark triggers: %w", err)
		}
	}
	return nil
}

func personSweepPostgreSQLTriggerStatements() []string {
	recipientRole := personSweepRecipientRolePredicate("mr.recipient_type")
	messageRoster := personSweepRosterPredicateValuesSQL(
		"_message_id", "_is_from_me", "_sender_id",
		"c.conversation_type", "pp.person_id",
	)
	rosterScope := personSweepRosterPredicateSQL("pp.person_id")
	bindingRoster := personSweepRosterPredicateSQL("_person_id")
	oldRecipientRole := personSweepRecipientRolePredicate("OLD.recipient_type")
	newRecipientRole := personSweepRecipientRolePredicate("NEW.recipient_type")
	recipientRoleChangeKind := personSweepRecipientRoleChangeKindSQL(
		"OLD.recipient_type", "NEW.recipient_type")
	recipientRoleChangeEffect := personSweepRecipientRoleChangeEffectSQL(
		"OLD.recipient_type", "NEW.recipient_type")
	return []string{
		`CREATE OR REPLACE FUNCTION msgvault_append_person_sweep_change(
		     _person_id BIGINT, _source_lane TEXT, _change_kind TEXT,
		     _evidence_effect TEXT, _source_id BIGINT, _message_id BIGINT)
		 RETURNS VOID AS $$
		 DECLARE next_sequence BIGINT;
		 BEGIN
		     UPDATE person_sweep_change_clock
		        SET sequence = sequence + 1
		      WHERE singleton = TRUE AND enabled = TRUE
		      RETURNING sequence INTO next_sequence;
		     IF next_sequence IS NOT NULL THEN
		         INSERT INTO person_sweep_changes
		             (sequence, person_id, source_lane, change_kind,
		              evidence_effect, source_id, message_id, recorded_at)
		         VALUES (next_sequence, _person_id, _source_lane, _change_kind,
		                 _evidence_effect, _source_id, _message_id, CURRENT_TIMESTAMP);
		     END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION msgvault_append_person_sweep_document_changes(
		     _person_id BIGINT, _source_id BIGINT, _message_id BIGINT,
		     _change_kind TEXT, _evidence_effect TEXT)
		 RETURNS VOID AS $$
		 DECLARE occurrence RECORD;
		 DECLARE next_sequence BIGINT;
		 BEGIN
		     FOR occurrence IN
		         SELECT source_id, attachment_id, occurrence_key
		           FROM document_occurrences
		          WHERE message_id = _message_id
		          ORDER BY attachment_id, occurrence_key
		     LOOP
		         next_sequence := NULL;
		         UPDATE person_sweep_change_clock
		            SET sequence = sequence + 1
		          WHERE singleton = TRUE AND enabled = TRUE
		          RETURNING sequence INTO next_sequence;
		         IF next_sequence IS NOT NULL THEN
		             INSERT INTO person_sweep_changes
		                 (sequence, person_id, source_lane, change_kind,
		                  evidence_effect, source_id, message_id, attachment_id,
		                  occurrence_key, recorded_at)
		             VALUES (next_sequence, _person_id, 'document_text', _change_kind,
		                     _evidence_effect, occurrence.source_id, _message_id,
		                     occurrence.attachment_id, occurrence.occurrence_key,
		                     CURRENT_TIMESTAMP);
		         END IF;
		     END LOOP;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP FUNCTION IF EXISTS msgvault_person_sweep_message_scope(BIGINT, BIGINT, BIGINT, TEXT, BIGINT)`,
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION msgvault_person_sweep_message_scope(
		     _message_id BIGINT, _conversation_id BIGINT, _sender_id BIGINT,
		     _is_from_me BOOLEAN, _message_type TEXT, _source_id BIGINT)
		 RETURNS TABLE(person_id BIGINT, source_lane TEXT, source_id BIGINT, message_id BIGINT)
		 AS $$
		     SELECT pp.person_id,
		            CASE WHEN _message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            _source_id, _message_id
		       FROM person_participants pp
		       JOIN person_tracking pt ON pt.person_id = pp.person_id
		      WHERE pp.participant_id = _sender_id
		     UNION
		     SELECT pp.person_id,
		            CASE WHEN _message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            _source_id, _message_id
		       FROM message_recipients mr
		       JOIN person_participants pp ON pp.participant_id = mr.participant_id
		       JOIN person_tracking pt ON pt.person_id = pp.person_id
		      WHERE mr.message_id = _message_id AND %s
		     UNION
		     SELECT pp.person_id,
		            CASE WHEN _message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            _source_id, _message_id
		       FROM conversations c
		       JOIN conversation_participants cp ON cp.conversation_id = c.id
		       JOIN person_participants pp ON pp.participant_id = cp.participant_id
		       JOIN person_tracking pt ON pt.person_id = pp.person_id
		      WHERE c.id = _conversation_id AND %s
		 $$ LANGUAGE sql STABLE`, recipientRole, messageRoster),
		`CREATE OR REPLACE FUNCTION msgvault_person_sweep_participant_message_scope(
		     _message_id BIGINT, _participant_id BIGINT)
		 RETURNS TABLE(person_id BIGINT, source_lane TEXT, source_id BIGINT, message_id BIGINT)
		 AS $$
		     SELECT pp.person_id,
		            CASE WHEN m.message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            m.source_id, m.id
		       FROM messages m
		       JOIN person_participants pp ON pp.participant_id = _participant_id
		       JOIN person_tracking pt ON pt.person_id = pp.person_id
		      WHERE m.id = _message_id AND m.deleted_at IS NULL
		        AND m.deleted_from_source_at IS NULL
		 $$ LANGUAGE sql STABLE`,
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION msgvault_person_sweep_roster_scope(
		     _conversation_id BIGINT, _participant_id BIGINT)
		 RETURNS TABLE(person_id BIGINT, source_lane TEXT, source_id BIGINT, message_id BIGINT)
		 AS $$
		     SELECT pp.person_id,
		            CASE WHEN m.message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            m.source_id, m.id
		       FROM conversations c
		       JOIN messages m ON m.conversation_id = c.id
		       JOIN person_participants pp ON pp.participant_id = _participant_id
		       JOIN person_tracking pt ON pt.person_id = pp.person_id
		      WHERE c.id = _conversation_id AND %s
		        AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		 $$ LANGUAGE sql STABLE`, rosterScope),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION msgvault_person_sweep_binding_scope(
		     _person_id BIGINT, _participant_id BIGINT)
		 RETURNS TABLE(person_id BIGINT, source_lane TEXT, source_id BIGINT, message_id BIGINT)
		 AS $$
		     SELECT _person_id,
		            CASE WHEN m.message_type = 'meeting_transcript'
		                 THEN 'meeting_text' ELSE 'conversation_text' END,
		            m.source_id, m.id
		       FROM messages m
		       JOIN person_tracking pt ON pt.person_id = _person_id
		      WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		        AND (m.sender_id = _participant_id
		             OR EXISTS (SELECT 1 FROM message_recipients mr
		                         WHERE mr.message_id = m.id
		                           AND mr.participant_id = _participant_id
		                           AND %s)
		             OR EXISTS (SELECT 1 FROM conversations c
		                         JOIN conversation_participants cp
		                           ON cp.conversation_id = c.id
		                         WHERE c.id = m.conversation_id
		                           AND %s
		                           AND cp.participant_id = _participant_id))
		 $$ LANGUAGE sql STABLE`, recipientRole, bindingRoster),
		`CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_messages() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 DECLARE kind TEXT;
		 DECLARE effect TEXT;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' THEN
		         IF NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL THEN
		             FOR affected IN SELECT * FROM msgvault_person_sweep_message_scope(
		                 NEW.id, NEW.conversation_id, NEW.sender_id, NEW.is_from_me,
		                 NEW.message_type, NEW.source_id)
		             LOOP
		                 PERFORM msgvault_append_person_sweep_change(
		                     affected.person_id, affected.source_lane, 'upsert', '',
		                     affected.source_id, affected.message_id);
		             END LOOP;
		         END IF;
		     ELSIF TG_OP = 'DELETE' THEN
		         IF OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL THEN
		             FOR affected IN SELECT * FROM msgvault_person_sweep_message_scope(
		                 OLD.id, OLD.conversation_id, OLD.sender_id, OLD.is_from_me,
		                 OLD.message_type, OLD.source_id)
		             LOOP
			         PERFORM msgvault_append_person_sweep_change(
			             affected.person_id, affected.source_lane, 'delete', 'source-deleted',
			             affected.source_id, affected.message_id);
			         PERFORM msgvault_append_person_sweep_document_changes(
			             affected.person_id, affected.source_id, affected.message_id,
			             'delete', 'source-deleted');
		             END LOOP;
		         END IF;
		     ELSE
		         kind := CASE
		             WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		                 THEN 'delete'
		             WHEN OLD.sender_id IS DISTINCT FROM NEW.sender_id
		               OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN 'scope'
		             ELSE 'upsert' END;
		         effect := CASE
		             WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		                 THEN 'source-deleted'
		             WHEN (OLD.deleted_at IS NOT NULL OR OLD.deleted_from_source_at IS NOT NULL)
		                  AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN 'source-reimported'
		             WHEN OLD.sender_id IS DISTINCT FROM NEW.sender_id
		               OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN 'identity-reassigned'
		             ELSE 'source-edited' END;
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_message_scope(
		                 OLD.id, OLD.conversation_id, OLD.sender_id, OLD.is_from_me,
		                 OLD.message_type, OLD.source_id)
		              WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		             UNION
		             SELECT * FROM msgvault_person_sweep_message_scope(
		                 NEW.id, NEW.conversation_id, NEW.sender_id, NEW.is_from_me,
		                 NEW.message_type, NEW.source_id)
		              WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		         LOOP
			     PERFORM msgvault_append_person_sweep_change(
			         affected.person_id, affected.source_lane, kind, effect,
			         affected.source_id, affected.message_id);
			     IF OLD.sender_id IS DISTINCT FROM NEW.sender_id
			        OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
			        OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
			        OR OLD.deleted_from_source_at IS DISTINCT FROM NEW.deleted_from_source_at THEN
			         PERFORM msgvault_append_person_sweep_document_changes(
			             affected.person_id, affected.source_id, affected.message_id,
			             kind, effect);
			     END IF;
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_bodies() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 DECLARE target_message_id BIGINT;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     target_message_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.message_id ELSE NEW.message_id END;
		     IF TG_OP <> 'UPDATE' OR OLD.body_text IS DISTINCT FROM NEW.body_text
		                           OR OLD.body_html IS DISTINCT FROM NEW.body_html THEN
		         FOR affected IN
		             SELECT scope.* FROM messages m
		             CROSS JOIN LATERAL msgvault_person_sweep_message_scope(
		                 m.id, m.conversation_id, m.sender_id, m.is_from_me,
		                 m.message_type, m.source_id) scope
		             WHERE m.id = target_message_id AND m.deleted_at IS NULL
		               AND m.deleted_from_source_at IS NULL
		         LOOP
		             PERFORM msgvault_append_person_sweep_change(
		                 affected.person_id, affected.source_lane, 'upsert',
		                 CASE WHEN TG_OP = 'INSERT' THEN '' ELSE 'source-edited' END,
		                 affected.source_id, affected.message_id);
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_recipients() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' AND %s THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_participant_message_scope(
		             NEW.message_id, NEW.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-relinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-relinked');
		         END LOOP;
		     ELSIF TG_OP = 'DELETE' AND %s THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_participant_message_scope(
		             OLD.message_id, OLD.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-unlinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-unlinked');
		         END LOOP;
		     ELSIF TG_OP = 'UPDATE' AND (
		        OLD.message_id IS DISTINCT FROM NEW.message_id
		        OR OLD.participant_id IS DISTINCT FROM NEW.participant_id) THEN
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_participant_message_scope(OLD.message_id, OLD.participant_id)
		              WHERE %s
		             UNION
		             SELECT * FROM msgvault_person_sweep_participant_message_scope(NEW.message_id, NEW.participant_id)
		              WHERE %s
		         LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'identity-reassigned',
			         affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'identity-reassigned');
		         END LOOP;
		     ELSIF TG_OP = 'UPDATE' AND OLD.recipient_type IS DISTINCT FROM NEW.recipient_type THEN
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_participant_message_scope(OLD.message_id, OLD.participant_id)
		              WHERE %s
		             UNION
		             SELECT * FROM msgvault_person_sweep_participant_message_scope(NEW.message_id, NEW.participant_id)
		              WHERE %s
		         LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, %s, %s, affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, %s, %s);
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`, newRecipientRole, oldRecipientRole,
			oldRecipientRole, newRecipientRole, oldRecipientRole, newRecipientRole,
			recipientRoleChangeKind, recipientRoleChangeEffect,
			recipientRoleChangeKind, recipientRoleChangeEffect),
		`CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_roster() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_roster_scope(
		             NEW.conversation_id, NEW.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-relinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-relinked');
		         END LOOP;
		     ELSIF TG_OP = 'DELETE' THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_roster_scope(
		             OLD.conversation_id, OLD.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-unlinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-unlinked');
		         END LOOP;
		     ELSIF OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		        OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
		        OR OLD.role IS DISTINCT FROM NEW.role OR OLD.joined_at IS DISTINCT FROM NEW.joined_at
		        OR OLD.left_at IS DISTINCT FROM NEW.left_at THEN
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_roster_scope(OLD.conversation_id, OLD.participant_id)
		             UNION
		             SELECT * FROM msgvault_person_sweep_roster_scope(NEW.conversation_id, NEW.participant_id)
		         LOOP
		             PERFORM msgvault_append_person_sweep_change(affected.person_id,
		                 affected.source_lane,
		                 CASE WHEN OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		                            OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
		                      THEN 'scope' ELSE 'upsert' END,
		                 CASE WHEN OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		                            OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
		                      THEN 'identity-reassigned' ELSE 'source-edited' END,
			         affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id,
			         CASE WHEN OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
			                    OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
			              THEN 'scope' ELSE 'upsert' END,
			         CASE WHEN OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
			                    OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
			              THEN 'identity-reassigned' ELSE 'source-edited' END);
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_bindings() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_binding_scope(
		             NEW.person_id, NEW.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-relinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-relinked');
		         END LOOP;
		     ELSIF TG_OP = 'DELETE' THEN
		         FOR affected IN SELECT * FROM msgvault_person_sweep_binding_scope(
		             OLD.person_id, OLD.participant_id) LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'scope-unlinked', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'scope-unlinked');
		         END LOOP;
		     ELSIF OLD.person_id IS DISTINCT FROM NEW.person_id
		        OR OLD.participant_id IS DISTINCT FROM NEW.participant_id THEN
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_binding_scope(OLD.person_id, OLD.participant_id)
		             UNION
		             SELECT * FROM msgvault_person_sweep_binding_scope(NEW.person_id, NEW.participant_id)
		         LOOP
			     PERFORM msgvault_append_person_sweep_change(affected.person_id,
			         affected.source_lane, 'scope', 'identity-reassigned', affected.source_id, affected.message_id);
			     PERFORM msgvault_append_person_sweep_document_changes(affected.person_id,
			         affected.source_id, affected.message_id, 'scope', 'identity-reassigned');
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_messages ON messages`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_message_insert ON messages`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_message_update ON messages`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_message_delete ON messages`,
		// INSERT and UPDATE publication must fire AFTER the row operation:
		// PostgreSQL fires row-level BEFORE INSERT triggers even when an
		// INSERT ... ON CONFLICT DO UPDATE takes the update path, so a BEFORE
		// trigger double-publishes (once as INSERT, once as UPDATE) for every
		// conflict upsert. AFTER INSERT does not fire on the conflict path,
		// which matches SQLite's conflict-update semantics exactly. DELETE
		// stays BEFORE because the scope functions resolve the doomed row.
		`CREATE TRIGGER trg_person_sweep_changes_message_insert
		     AFTER INSERT ON messages FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_messages()`,
		fmt.Sprintf(`CREATE TRIGGER trg_person_sweep_changes_message_update
		     AFTER UPDATE OF %s ON messages FOR EACH ROW
		     WHEN (%s)
		     EXECUTE FUNCTION msgvault_person_sweep_changes_messages()`,
			contentChangedTriggerColumnList(), contentChangedValueGuard("IS DISTINCT FROM")),
		`CREATE TRIGGER trg_person_sweep_changes_message_delete
		     BEFORE DELETE ON messages FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_messages()`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_bodies ON message_bodies`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_bodies_delete ON message_bodies`,
		`CREATE TRIGGER trg_person_sweep_changes_bodies
		     AFTER INSERT OR UPDATE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_bodies()`,
		`CREATE TRIGGER trg_person_sweep_changes_bodies_delete
		     BEFORE DELETE ON message_bodies FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_bodies()`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_recipients ON message_recipients`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_recipients_delete ON message_recipients`,
		`CREATE TRIGGER trg_person_sweep_changes_recipients
		     AFTER INSERT OR UPDATE ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_recipients()`,
		`CREATE TRIGGER trg_person_sweep_changes_recipients_delete
		     BEFORE DELETE ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_recipients()`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_roster ON conversation_participants`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_roster_delete ON conversation_participants`,
		`CREATE TRIGGER trg_person_sweep_changes_roster
		     AFTER INSERT OR UPDATE ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_roster()`,
		`CREATE TRIGGER trg_person_sweep_changes_roster_delete
		     BEFORE DELETE ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_roster()`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_bindings ON person_participants`,
		`DROP TRIGGER IF EXISTS trg_person_sweep_changes_bindings_delete ON person_participants`,
		`CREATE TRIGGER trg_person_sweep_changes_bindings
		     AFTER INSERT OR UPDATE ON person_participants FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_bindings()`,
		`CREATE TRIGGER trg_person_sweep_changes_bindings_delete
		     BEFORE DELETE ON person_participants FOR EACH ROW
		     EXECUTE FUNCTION msgvault_person_sweep_changes_bindings()`,
	}
}

// EnsureActivityProjectionTriggers installs the PostgreSQL triggers that keep
// the dated-activity projection queue current. They live in a separate
// migration from EnsureTriggers because the message watermark migration is
// already recorded on upgraded archives and must not be rerun just to add
// activity support.
func (d *PostgreSQLDialect) EnsureActivityProjectionTriggers(q querier) error {
	stmts := []string{
		`DROP TRIGGER IF EXISTS trg_activity_queue_messages_insert ON messages`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_messages_update ON messages`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_insert ON message_recipients`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_update ON message_recipients`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_recipients_delete ON message_recipients`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_people_insert ON conversation_participants`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_people_update ON conversation_participants`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_people_delete ON conversation_participants`,
		`DROP TRIGGER IF EXISTS trg_activity_queue_conversation_type_update ON conversations`,
		`DROP TRIGGER IF EXISTS trg_activity_direct_link_delete_dirty ON activity_event_persons`,
		`CREATE OR REPLACE FUNCTION enqueue_activity_projection_message(p_message_id BIGINT) RETURNS VOID AS $$
		 BEGIN
		     IF p_message_id IS NULL OR NOT EXISTS (
		         SELECT 1 FROM messages WHERE id = p_message_id
		     ) THEN
		         RETURN;
		     END IF;
		     INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		     VALUES (p_message_id, 1, CURRENT_TIMESTAMP)
		     ON CONFLICT (message_id) DO UPDATE SET
		         revision = activity_projection_queue.revision + 1,
		         queued_at = CURRENT_TIMESTAMP;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION enqueue_activity_projection_conversation(p_conversation_id BIGINT) RETURNS VOID AS $$
		 DECLARE
		     message_record RECORD;
		 BEGIN
		     FOR message_record IN
		         SELECT id FROM messages WHERE conversation_id = p_conversation_id
		     LOOP
		         PERFORM enqueue_activity_projection_message(message_record.id);
		     END LOOP;
		 END;
		 $$ LANGUAGE plpgsql`,
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION queue_activity_messages() RETURNS trigger AS $$
		 BEGIN
		     IF %s THEN
		         PERFORM enqueue_activity_projection_message(NEW.id);
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`, activityValueGuard("IS DISTINCT FROM")),
		`CREATE OR REPLACE FUNCTION queue_activity_recipient_insert() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_message(NEW.message_id);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_recipient_update() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_message(OLD.message_id);
		     PERFORM enqueue_activity_projection_message(NEW.message_id);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_recipient_delete() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_message(OLD.message_id);
		     RETURN OLD;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_conversation_people_insert() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_conversation(NEW.conversation_id);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_conversation_people_update() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_conversation(OLD.conversation_id);
		     PERFORM enqueue_activity_projection_conversation(NEW.conversation_id);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_conversation_people_delete() RETURNS trigger AS $$
		 BEGIN
		     PERFORM enqueue_activity_projection_conversation(OLD.conversation_id);
		     RETURN OLD;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION queue_activity_conversation_type_update() RETURNS trigger AS $$
		 BEGIN
		     IF OLD.conversation_type IS DISTINCT FROM NEW.conversation_type THEN
		         PERFORM enqueue_activity_projection_conversation(NEW.id);
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION dirty_activity_direct_link_delete() RETURNS trigger AS $$
		 BEGIN
		     IF OLD.evidence = 'direct' THEN
		         UPDATE person_contact_state
		         SET dirty_at = CURRENT_TIMESTAMP
		         WHERE person_id = OLD.person_id;
		     END IF;
		     RETURN OLD;
		 END;
		 $$ LANGUAGE plpgsql`,
		fmt.Sprintf(`CREATE TRIGGER trg_activity_queue_messages_update
		     AFTER UPDATE OF %s ON messages FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_messages()`, activityTriggerColumnList()),
		`CREATE TRIGGER trg_activity_queue_recipients_insert
		     AFTER INSERT ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_recipient_insert()`,
		`CREATE TRIGGER trg_activity_queue_recipients_update
		     AFTER UPDATE ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_recipient_update()`,
		`CREATE TRIGGER trg_activity_queue_recipients_delete
		     AFTER DELETE ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_recipient_delete()`,
		`CREATE TRIGGER trg_activity_queue_conversation_people_insert
		     AFTER INSERT ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_conversation_people_insert()`,
		`CREATE TRIGGER trg_activity_queue_conversation_people_update
		     AFTER UPDATE ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_conversation_people_update()`,
		`CREATE TRIGGER trg_activity_queue_conversation_people_delete
		     AFTER DELETE ON conversation_participants FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_conversation_people_delete()`,
		`CREATE TRIGGER trg_activity_queue_conversation_type_update
		     AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
		     EXECUTE FUNCTION queue_activity_conversation_type_update()`,
		`CREATE TRIGGER trg_activity_direct_link_delete_dirty
		     AFTER DELETE ON activity_event_persons FOR EACH ROW
		     EXECUTE FUNCTION dirty_activity_direct_link_delete()`,
	}
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("ensure activity projection triggers: %w", err)
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

// IsSerializationFailureError reports whether err is PostgreSQL's
// serialization_failure (SQLSTATE 40001). Under REPEATABLE READ a
// SELECT ... FOR UPDATE (or an UPDATE/DELETE) that targets a row another
// transaction updated and committed after this transaction's snapshot cannot
// be satisfied without either ignoring that commit or reading outside the
// snapshot, so PostgreSQL aborts with 40001 rather than choose. The condition
// it reports is exactly "the row I locked changed under me", which is why the
// vCard envelope commit can translate it into a projection conflict.
func (d *PostgreSQLDialect) IsSerializationFailureError(err error) bool {
	return isPgError(err, "40001")
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
// RemoveSourceSerialized deletes a source. The list also includes the
// identity candidate/evidence tables reached by explicit polymorphic endpoint
// cleanup before the source cascade.
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
//   - imap_folder_state and imap_message_memberships: written after IMAP syncs
//     and cascade-reachable from sources.
//
// collections is included (despite not being a direct sources cascade target)
// so a concurrent collection rename cannot race the collection_sources cascade.
var exclusiveLockTables = []string{
	"sync_runs", "sources", "conversations", "conversation_participants",
	"messages", "message_recipients", "message_labels", "message_bodies", "message_raw",
	"attachments", "document_occurrences", "labels", "participants", "participant_identifiers", "reactions",
	"participant_contact_observations", identityMatchCandidatesTableName, identityMatchCandidateSourcesTableName,
	identityMatchEvidenceTableName, identityMatchEvidenceSourcesTableName,
	// persons and person_participants: MergeParticipants (reached from the
	// Beeper import path) repoints bindings and bumps person revisions, so
	// both belong to the sync/import write set this lock mirrors.
	"persons", "person_participants",
	"person_sweep_changes", "person_sweep_sync_publications",
	// Activity projection rows are written from and cascade with the sync
	// archive. The queue is trigger-written by every message/participant
	// mutation, so it belongs to the same exclusive write set.
	"activity_events", "activity_event_persons", "person_contact_state",
	"activity_projection_queue",
	"collections", "collection_sources", "account_identities", "applied_migrations",
	"sync_operations",
	"source_import_items", "sync_run_items", "sync_checkpoints",
	"imap_folder_state", "imap_message_memberships",
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

// RowWriterLockSQL returns "": PostgreSQL row locks come from SelectForUpdate,
// and a self-assign UPDATE would make concurrent REPEATABLE READ lockers of
// the same row report a spurious serialization failure.
func (d *PostgreSQLDialect) RowWriterLockSQL(table, column string) string { return "" }

// MaintenanceTimeoutResetSQL disables the per-statement timeout for the
// current transaction. SET LOCAL auto-resets at tx end, so the pool-wide
// statement_timeout cannot leak away on other connections.
func (d *PostgreSQLDialect) MaintenanceTimeoutResetSQL() string {
	return "SET LOCAL statement_timeout = 0"
}

// isPgError checks if err is a pgconn.PgError with the given SQLSTATE code.
func isPgError(err error, code string) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
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
