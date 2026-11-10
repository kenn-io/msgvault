package query

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/sync/semaphore"
)

type sqliteExploreReader struct {
	path   string
	slot   *semaphore.Weighted
	store  *store.Store
	conn   *sql.Conn
	nonce  string
	closed bool
}

type liveExploreSnapshotKey struct{}
type liveExploreSnapshot struct {
	engine   *LiveExploreEngine
	revision string
}

// LiveExploreEngine serves entry listings during or after an automatic full rebuild.
// The archive pool is borrowed; the separate Explore reader is owned.
type LiveExploreEngine struct {
	*SQLiteEngine

	exploreReader *sqliteExploreReader
}

func NewLiveExploreEngine(db *sql.DB, archivePath string) *LiveExploreEngine {
	return &LiveExploreEngine{
		SQLiteEngine:  NewSQLiteEngine(db),
		exploreReader: &sqliteExploreReader{path: archivePath, slot: semaphore.NewWeighted(1)},
	}
}

// BeginExploreSnapshot binds identity, search, and listing reads to one revision.
// release ends the snapshot and lets the next request use the owned connection.
func (e *LiveExploreEngine) BeginExploreSnapshot(ctx context.Context) (context.Context, func(), string, error) {
	r := e.exploreReader
	if r == nil {
		return ctx, nil, "", &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := r.slot.Acquire(ctx, 1); err != nil {
		return ctx, nil, "", fmt.Errorf("acquire live Explore reader: %w", err)
	}
	fail := func(err error) (context.Context, func(), string, error) { r.slot.Release(1); return ctx, nil, "", err }
	if r.closed {
		return fail(sql.ErrConnDone)
	}
	if r.store == nil {
		reader, err := store.OpenReadOnlyContext(ctx, r.path)
		if err != nil {
			return fail(err)
		}
		r.store = reader
	}
	if r.conn != nil {
		if err := r.conn.PingContext(ctx); err != nil {
			_ = r.conn.Close()
			r.conn = nil
		}
	}
	if r.conn == nil {
		conn, err := r.store.DB().Conn(ctx)
		if err != nil {
			return fail(err)
		}
		r.conn, r.nonce = conn, uuid.New().String()
	}
	tx, err := r.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	// Cancellation can discard the previous transaction's connection after
	// Ping returns. A replacement needs a fresh nonce before it serves rows.
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) {
		_ = r.conn.Close()
		r.conn, err = r.store.DB().Conn(ctx)
		if err == nil {
			r.nonce = uuid.New().String()
			tx, err = r.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		}
	}
	if err != nil {
		return fail(err)
	}
	var version int64
	if err := tx.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		_ = tx.Rollback()
		return fail(err)
	}
	revision := fmt.Sprintf("live-%s-%d", r.nonce, version)
	ctx = store.WithReadSnapshotContext(ctx, e.db, tx)
	ctx = context.WithValue(ctx, liveExploreSnapshotKey{}, liveExploreSnapshot{e, revision})
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(); r.slot.Release(1) }) }
	return ctx, release, revision, nil
}

// Explore lists current archive rows while auto mode has no analytical cache.
func (e *LiveExploreEngine) Explore(ctx context.Context, request ExploreRequest) (*ExploreResponse, error) {
	if len(request.Context.ParticipantIDs) > 0 || len(request.Context.AdditionalParticipantGroups) > 0 ||
		request.SentOnly || request.PersonInteractionsOnly || request.Search.Mode == SearchSemantic || request.Search.Mode == SearchHybrid {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	provenance, err := validateExploreRequest(request)
	if err != nil {
		return nil, err
	}
	snapshot, ok := ctx.Value(liveExploreSnapshotKey{}).(liveExploreSnapshot)
	if !ok || snapshot.engine != e {
		var release func()
		ctx, release, snapshot.revision, err = e.BeginExploreSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	conditions, args := buildExploreConditionsWithDialect(request, exploreConditionDialect{
		unicodeLower:   e.dialect.UnicodeLowerExpression,
		envelopeColumn: "email_address",
		dateComparison: func(column, operator string) string {
			return sqliteutil.TimestampKeyFunction + "(" + column + ") " + operator + " ?"
		},
		dateParam:    func(value time.Time) any { return sqliteutil.TimestampKey(value) },
		messageTypes: sqliteMessageTypeCondition,
		candidates: func(ids []int64) (string, any) {
			encoded, _ := json.Marshal(ids)
			return "message_id IN (SELECT value FROM json_each(?))", string(encoded)
		},
	})
	conditions = "NOT internally_deleted AND occurred_at IS NOT NULL AND (" + conditions + ")"
	core := "WITH " + buildScalarAnalyticalEntriesCTEWithSource("entry_core", false, "s.identifier", "NOT ("+archiveOnlyDeletionTargetGuard+")", "mr.email_address", senderEnvelopeDomainExpr("mr.email_address"))
	chat := sqlIsChatPredicate("message_type", "conversation_type")
	response := &ExploreResponse{Rows: make([]EntryRow, 0), CacheRevision: snapshot.revision, SearchProvenance: provenance}
	countSQL := core + ` SELECT COUNT(*) FILTER (WHERE NOT (` + chat + `)) +
  COUNT(DISTINCT CASE WHEN ` + chat + ` THEN conversation_id END)
  FROM entry_core AS analytical_entries WHERE ` + conditions
	if err := e.queryRowContext(ctx, countSQL, args...).Scan(&response.TotalCount); err != nil {
		return nil, fmt.Errorf("count live entries: %w", err)
	}
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	order := sqliteutil.TimestampKeyFunction + "(occurred_at)"
	keysSQL := core + `, entry_keys AS (
  SELECT ` + sqlMessageEntryKeyExpr("") + ` AS entry_key, ` + identityindex.EntryKindSQL("message_type") + ` AS entry_kind,
   message_id AS anchor_id, conversation_id, source_id, ` + order + ` AS order_at
  FROM entry_core AS analytical_entries WHERE (` + conditions + `) AND NOT (` + chat + `)
  UNION ALL
  SELECT ` + sqlConversationEntryKeyExpr("") + `, 'conversation', NULL, conversation_id, source_id, MAX(` + order + `)
  FROM entry_core AS analytical_entries WHERE (` + conditions + `) AND (` + chat + `)
  GROUP BY conversation_id, source_id
 ) SELECT entry_key, entry_kind, anchor_id, conversation_id FROM entry_keys
 ORDER BY order_at DESC, source_id ASC, entry_key ASC LIMIT ? OFFSET ?`
	keyArgs := append(append(append([]any{}, args...), args...), limit, request.Page.Offset)
	rows, err := e.queryContext(ctx, keysSQL, keyArgs...)
	if err != nil {
		return nil, fmt.Errorf("page live entries: %w", err)
	}
	var values []string
	var pageArgs []any
	for rows.Next() {
		var key, kind string
		var anchor, conversation sql.NullInt64
		if err := rows.Scan(&key, &kind, &anchor, &conversation); err != nil {
			_ = rows.Close()
			return nil, err
		}
		values = append(values, "(?, ?, ?, ?, ?)")
		pageArgs = append(pageArgs, key, kind, anchor, conversation, len(values))
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return response, nil
	}
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, response.TotalCount)
	rows, err = e.queryContext(ctx, buildSQLiteExplorePageSQL(core, conditions, chat, values), pageArgs...)
	if err != nil {
		return nil, fmt.Errorf("hydrate live entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if err := scanExploreRows(rows, response); err != nil {
		return nil, err
	}
	return response, nil
}

func buildSQLiteExplorePageSQL(core, conditions, chat string, values []string) string {
	// SQLite's bare grouped columns select one sender row, like DuckDB FIRST.
	senders := strings.ReplaceAll(sqlPageSenderLabels("SELECT anchor_id FROM page WHERE entry_kind <> 'conversation'"), "FIRST(", "(")
	return core + `, page_keys(entry_key, entry_kind, anchor_id, page_conversation_id, position) AS MATERIALIZED (
 VALUES ` + strings.Join(values, ",") + `
 ), members AS MATERIALIZED (
 SELECT p.entry_key, analytical_entries.* FROM page_keys p JOIN entry_core AS analytical_entries ON analytical_entries.message_id = p.anchor_id
 WHERE p.entry_kind <> 'conversation' AND (` + conditions + `)
 UNION ALL
 SELECT p.entry_key, analytical_entries.* FROM page_keys p JOIN entry_core AS analytical_entries ON analytical_entries.conversation_id = p.page_conversation_id
 WHERE p.entry_kind = 'conversation' AND (` + conditions + `) AND (` + chat + `)
 ), stats AS (
 SELECT entry_key, COUNT(*) AS message_count, MAX(has_attachments) AS has_attachments,
 MAX(deleted_from_source) AS deleted_from_source FROM members GROUP BY entry_key
 ), page AS MATERIALIZED (
 SELECT p.entry_key, p.entry_kind, p.position,
 (SELECT message_id FROM members m WHERE m.entry_key = p.entry_key
  ORDER BY ` + sqliteutil.TimestampKeyFunction + `(occurred_at) DESC, message_id DESC LIMIT 1) AS anchor_id
 FROM page_keys p
 ), links AS (
 SELECT m.entry_key, mr.participant_id FROM members m JOIN message_recipients mr ON mr.message_id = m.message_id
 UNION
 SELECT entry_key, sender_id FROM members WHERE sender_id IS NOT NULL
 UNION
 SELECT p.entry_key, cp.participant_id FROM page_keys p JOIN conversation_participants cp ON cp.conversation_id = p.page_conversation_id
 WHERE p.entry_kind = 'conversation'
 ), page_senders AS (` + senders + `)
 SELECT p.entry_key, p.entry_kind, m.id, m.conversation_id, m.sent_at,
 m.source_id, COALESCE(s.source_type, ''), COALESCE(s.identifier, ''),
 COALESCE(m.message_type, ''), COALESCE(c.conversation_type, ''),
 CASE WHEN p.entry_kind = 'conversation' THEN COALESCE(NULLIF(c.title, ''), 'Conversation')
 ELSE COALESCE(NULLIF(m.subject, ''), NULLIF(c.title, ''), m.snippet, '') END,
 COALESCE(m.snippet, ''),
 (SELECT json_group_array(participant_id) FROM (SELECT participant_id FROM links WHERE entry_key = p.entry_key ORDER BY participant_id)),
 (SELECT json_group_array(label) FROM (SELECT ` + sqlAnalyticalEntriesParticipantLabel("person") + ` AS label
 FROM participants person WHERE person.id IN (SELECT participant_id FROM links WHERE entry_key = p.entry_key) GROUP BY label ORDER BY label)),
 NULL, stats.message_count, stats.has_attachments,
 (SELECT COUNT(*) FROM attachments a WHERE a.message_id IN (SELECT message_id FROM members WHERE entry_key = p.entry_key)),
 (SELECT COALESCE(SUM(size), 0) FROM attachments a WHERE a.message_id IN (SELECT message_id FROM members WHERE entry_key = p.entry_key)),
 stats.deleted_from_source, ?, NULL, CASE WHEN p.entry_kind = 'conversation' THEN '' ELSE COALESCE(senders.sender_label, '') END
 FROM page p JOIN messages m ON m.id = p.anchor_id
 JOIN sources s ON s.id = m.source_id LEFT JOIN conversations c ON c.id = m.conversation_id
 JOIN stats ON stats.entry_key = p.entry_key
 LEFT JOIN page_senders senders ON senders.sender_message_id = p.anchor_id
 ORDER BY p.position`
}

// Close releases the owned reader after all requests have finished.
func (e *LiveExploreEngine) Close() error {
	if e.exploreReader == nil {
		return nil
	}
	r := e.exploreReader
	if err := r.slot.Acquire(context.Background(), 1); err != nil {
		return fmt.Errorf("close live Explore reader: %w", err)
	}
	defer r.slot.Release(1)
	r.closed = true
	var connErr, readerErr error
	if r.conn != nil {
		connErr = r.conn.Close()
		if errors.Is(connErr, sql.ErrConnDone) {
			connErr = nil
		}
		r.conn = nil
	}
	if r.store != nil {
		readerErr = r.store.Close()
		r.store = nil
	}
	return errors.Join(connErr, readerErr)
}
