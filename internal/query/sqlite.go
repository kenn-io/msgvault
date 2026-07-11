package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// SQLiteEngine implements Engine using direct SQL queries.
// Despite its name, it is dialect-agnostic and supports both SQLite
// (default) and PostgreSQL via the dialect field.
type SQLiteEngine struct {
	db      *sql.DB
	dialect Dialect

	// FTS availability cache - thread-safe with mutex.
	// Only caches successful checks; errors cause retries on next call.
	ftsMu      sync.Mutex
	ftsResult  bool
	ftsChecked bool
}

// NewSQLiteEngine creates a new SQLite-backed query engine.
func NewSQLiteEngine(db *sql.DB) *SQLiteEngine {
	return &SQLiteEngine{db: db, dialect: SQLiteQueryDialect{}}
}

// NewEngineWithDialect creates a query engine with an explicit dialect.
// Use this to construct a PostgreSQL-backed engine:
//
//	engine := query.NewEngineWithDialect(db, query.PostgreSQLQueryDialect{})
func NewEngineWithDialect(db *sql.DB, d Dialect) *SQLiteEngine {
	return &SQLiteEngine{db: db, dialect: d}
}

// hasFTSTable checks if the FTS index is available for this dialect.
// Result is cached after first successful check. Errors cause retries on next call.
// Thread-safe via mutex.
func (e *SQLiteEngine) hasFTSTable(ctx context.Context) bool {
	e.ftsMu.Lock()
	defer e.ftsMu.Unlock()

	// Fast path: already successfully checked
	if e.ftsChecked {
		return e.ftsResult
	}

	// The dialect's HasFTSTableSQL() probe is the existence check for BOTH
	// backends: SQLite checks sqlite_master for the messages_fts virtual
	// table; PostgreSQL checks information_schema for the messages.search_fts
	// column. We must NOT run a hardcoded SQLite-only `SELECT 1 FROM
	// messages_fts` secondary probe unconditionally — on PostgreSQL there is
	// no messages_fts relation (PG uses an inline search_fts TSVECTOR column),
	// so that probe errors with `relation "messages_fts" does not exist`
	// (42P01), causing FTS to be cached as unavailable and PG Search to
	// silently fall back to subject/snippet LIKE instead of the tsvector
	// ranking path.
	var count int
	err := e.queryRowContext(ctx, e.dialect.HasFTSTableSQL()).Scan(&count)
	if err != nil {
		// On error (canceled context, temporary DB issue), return false
		// but don't cache so next call can retry.
		return false
	}
	if count == 0 {
		e.ftsResult = false
		e.ftsChecked = true
		return false
	}

	// Dialect-aware liveness probe. SQLite's existence check (sqlite_master)
	// does NOT prove the fts5 module is loadable: a DB built by an
	// fts5-enabled binary still lists messages_fts in sqlite_master when
	// opened by a no-fts5 binary, but querying it fails with
	// `no such module: fts5`. Run the dialect's liveness SQL to confirm the
	// table is actually queryable, mirroring store.SQLiteDialect.FTSAvailable.
	// PostgreSQL returns "" here (its column probe is authoritative).
	if liveness := e.dialect.FTSLivenessSQL(); liveness != "" {
		var probe int
		lerr := e.queryRowContext(ctx, liveness).Scan(&probe)
		// sql.ErrNoRows means the table is queryable but empty — still
		// available. Any other error (e.g. no such module) means FTS is
		// not usable: cache false so search uses the LIKE fallback.
		if lerr != nil && !errors.Is(lerr, sql.ErrNoRows) {
			e.ftsResult = false
			e.ftsChecked = true
			return false
		}
	}

	e.ftsResult = true
	e.ftsChecked = true
	return e.ftsResult
}

// Close is a no-op for SQLiteEngine since it doesn't own the connection.
func (e *SQLiteEngine) Close() error {
	return nil
}

// queryContext runs QueryContext with dialect-aware placeholder rebinding.
func (e *SQLiteEngine) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return e.db.QueryContext(ctx, e.dialect.Rebind(query), args...)
}

// queryRowContext runs QueryRowContext with dialect-aware placeholder rebinding.
func (e *SQLiteEngine) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return e.db.QueryRowContext(ctx, e.dialect.Rebind(query), args...)
}

// escapeSQLiteLike escapes LIKE wildcard characters (%, _, \) with \.
func escapeSQLiteLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// aggDimension describes the variable parts of an aggregate query for a given ViewType.
type aggDimension struct {
	keyExpr   string // SQL expression for the grouping key
	joins     string // JOIN clauses for the dimension table(s)
	whereExpr string // additional WHERE condition (e.g., key IS NOT NULL)
}

// aggDimensionForView returns the SQL dimension definition for a given ViewType.
func aggDimensionForView(d Dialect, view ViewType, timeGranularity TimeGranularity) (aggDimension, error) {
	switch view {
	case ViewSenders:
		return aggDimension{
			keyExpr: "p.email_address",
			joins: `JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
				JOIN participants p ON p.id = mr.participant_id`,
			whereExpr: "p.email_address IS NOT NULL",
		}, nil
	case ViewSenderNames:
		nameExpr := recipientNameExpr("mr", "p")
		return aggDimension{
			keyExpr: nameExpr,
			joins: `JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
				JOIN participants p ON p.id = mr.participant_id`,
			whereExpr: nameExpr + " != ''",
		}, nil
	case ViewRecipients:
		return aggDimension{
			keyExpr: "p.email_address",
			joins: `JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type IN ('to', 'cc', 'bcc')
				JOIN participants p ON p.id = mr.participant_id`,
			whereExpr: "p.email_address IS NOT NULL",
		}, nil
	case ViewRecipientNames:
		nameExpr := recipientNameExpr("mr", "p")
		return aggDimension{
			keyExpr: nameExpr,
			joins: `JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type IN ('to', 'cc', 'bcc')
				JOIN participants p ON p.id = mr.participant_id`,
			whereExpr: nameExpr + " != ''",
		}, nil
	case ViewDomains:
		return aggDimension{
			keyExpr: "p.domain",
			joins: `JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
				JOIN participants p ON p.id = mr.participant_id`,
			whereExpr: "p.domain IS NOT NULL AND p.domain != ''",
		}, nil
	case ViewLabels:
		return aggDimension{
			keyExpr: "l.name",
			joins: `JOIN message_labels ml ON ml.message_id = m.id
				JOIN labels l ON l.id = ml.label_id`,
			whereExpr: "",
		}, nil
	case ViewTime:
		var gran string
		switch timeGranularity {
		case TimeYear:
			gran = "year"
		case TimeMonth:
			gran = "month"
		case TimeDay:
			gran = "day"
		default:
			return aggDimension{}, fmt.Errorf("unsupported time granularity: %d", timeGranularity)
		}
		return aggDimension{
			keyExpr:   d.TimeTruncExpression("m.sent_at", gran),
			joins:     "",
			whereExpr: "m.sent_at IS NOT NULL",
		}, nil
	default:
		return aggDimension{}, fmt.Errorf("unsupported view type: %v", view)
	}
}

// buildAggregateSQL builds a complete aggregate query from a dimension and filter parts.
func buildAggregateSQL(dim aggDimension, filterJoins string, filterWhere string, sort string) string {
	allJoins := dim.joins
	if filterJoins != "" {
		allJoins += "\n" + filterJoins
	}

	allWhere := filterWhere
	if dim.whereExpr != "" {
		allWhere += " AND " + dim.whereExpr
	}

	// The outer derived table needs an explicit alias — PostgreSQL
	// rejects subqueries in FROM without one ("syntax error at or near
	// ')'"); SQLite tolerates either form, so `AS agg` is portable.
	return fmt.Sprintf(`
		SELECT key, count, total_size, attachment_size, attachment_count, total_unique
		FROM (
			SELECT
				%s as key,
				COUNT(*) as count,
				COALESCE(SUM(m.size_estimate), 0) as total_size,
				COALESCE(SUM(att.att_size), 0) as attachment_size,
				COALESCE(SUM(att.att_count), 0) as attachment_count,
				COUNT(*) OVER() as total_unique
			FROM messages m
			%s
			LEFT JOIN (
				SELECT message_id, SUM(size) as att_size, COUNT(*) as att_count
				FROM attachments
				GROUP BY message_id
			) att ON att.message_id = m.id
			WHERE %s
			GROUP BY key
		) AS agg
		%s
		LIMIT ?
	`, dim.keyExpr, allJoins, allWhere, sort)
}

// optsToFilterConditions converts AggregateOptions into WHERE conditions and args.
func optsToFilterConditions(d Dialect, opts AggregateOptions, prefix string) ([]string, []any) {
	var conditions []string
	var args []any

	// Always exclude rows soft-deleted by deduplicate; gate
	// source-deleted on opts.HideDeletedFromSource via the helper.
	conditions = append(conditions, store.LiveMessagesWhere(strings.TrimSuffix(prefix, "."), opts.HideDeletedFromSource))

	conditions, args = appendSourceFilter(
		conditions, args, prefix, opts.SourceID, opts.SourceIDs,
	)
	// Bind time.Time values directly. Formatting to a naive
	// "2006-01-02 15:04:05" string and binding that to a PG TIMESTAMPTZ
	// column parses the string in session TimeZone (not UTC); pgx
	// encodes time.Time correctly on both backends, and go-sqlite3
	// formats it to a sortable RFC3339-with-fractional layout.
	if opts.After != nil {
		conditions = append(conditions, prefix+"sent_at >= ?")
		args = append(args, *opts.After)
	}
	if opts.Before != nil {
		conditions = append(conditions, prefix+"sent_at < ?")
		args = append(args, *opts.Before)
	}
	if opts.WithAttachmentsOnly {
		conditions = append(conditions, d.BoolTrueExpr(prefix+"has_attachments"))
	}

	return conditions, args
}

// sortClause returns ORDER BY clause for aggregates.
// Always includes a secondary sort by key to ensure deterministic ordering when
// primary sort values are equal (e.g., two labels with the same count).
// Returns an error if the SortField is not a valid enum value.
func sortClause(opts AggregateOptions) (string, error) {
	var field string
	switch opts.SortField {
	case SortByCount:
		field = "count"
	case SortBySize:
		field = "total_size"
	case SortByAttachmentSize:
		field = "attachment_size"
	case SortByName:
		field = "key"
	default:
		return "", fmt.Errorf("unsupported sort field: %d", opts.SortField)
	}

	dir := "DESC"
	if opts.SortDirection == SortAsc {
		dir = "ASC"
	}

	// Secondary sort by key ensures deterministic ordering for ties
	if field == "key" {
		return fmt.Sprintf("ORDER BY %s %s", field, dir), nil
	}
	return fmt.Sprintf("ORDER BY %s %s, key ASC", field, dir), nil
}

// buildFilterJoinsAndConditions builds JOIN and WHERE clauses from a MessageFilter.
// Returns joinClauses (already joined by \n), conditions (slice), and args.
// This is used for SubAggregate to apply drill-down filters before sub-grouping.
func (e *SQLiteEngine) buildFilterJoinsAndConditions(filter MessageFilter, tableAlias string) (string, []string, []any) {
	// Every structured filter below resolves through an EXISTS / NOT EXISTS
	// correlated subquery, so this builder emits no JOIN of its own. The
	// empty join slot is preserved in the return shape because callers
	// (SubAggregate) concatenate the search-side FTS join onto it.
	var conditions []string
	var args []any

	prefix := ""
	if tableAlias != "" {
		prefix = tableAlias + "."
	}

	// Include all messages (deleted messages shown with indicator in TUI)

	// Always exclude rows soft-deleted by deduplicate; gate
	// source-deleted on filter.HideDeletedFromSource via the helper.
	conditions = append(conditions, store.LiveMessagesWhere(strings.TrimSuffix(prefix, "."), filter.HideDeletedFromSource))

	conditions, args = appendSourceFilter(
		conditions, args, prefix, filter.SourceID, filter.SourceIDs,
	)

	if filter.ConversationID != nil {
		conditions = append(conditions, prefix+"conversation_id = ?")
		args = append(args, *filter.ConversationID)
	}

	if filter.After != nil {
		conditions = append(conditions, prefix+"sent_at >= ?")
		args = append(args, *filter.After)
	}

	if filter.Before != nil {
		conditions = append(conditions, prefix+"sent_at < ?")
		args = append(args, *filter.Before)
	}

	if filter.WithAttachmentsOnly {
		conditions = append(conditions, e.dialect.BoolTrueExpr(prefix+"has_attachments"))
	}

	if filter.MessageType != "" {
		condition, conditionArgs := sqliteMessageTypeCondition(tableAlias, []string{filter.MessageType})
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// Sender + sender-name filters — check both message_recipients (email)
	// and direct sender_id (WhatsApp/chat). Also checks phone_number for
	// phone-based lookups (e.g., from:+447...). Uses EXISTS (not a plain
	// JOIN) so a message with multiple 'from' rows is not multiplied into
	// duplicate result rows.
	//
	// When BOTH the email and the display name are filtered, they must
	// match the SAME from-row (or the SAME direct sender), not two
	// independent EXISTS that a multi-author message could satisfy via
	// different rows. So fold them into a single correlated EXISTS per
	// branch when both are set; otherwise keep the per-field EXISTS.
	if filter.Sender != "" && filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND (p_filter_from.email_address = ? OR p_filter_from.phone_number = ?)
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM participants p_direct_sender
			WHERE p_direct_sender.id = m.sender_id
			  AND (p_direct_sender.email_address = ? OR p_direct_sender.phone_number = ?)
			  AND %s = ?
		))`, recipientNameExpr("mr_filter_from", "p_filter_from"), participantNameExpr("p_direct_sender")))
		args = append(args, filter.Sender, filter.Sender, filter.SenderName, filter.Sender, filter.Sender, filter.SenderName)
	} else {
		if filter.Sender != "" {
			conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND (p_filter_from.email_address = ? OR p_filter_from.phone_number = ?)
		) OR EXISTS (
			SELECT 1 FROM participants p_direct_sender
			WHERE p_direct_sender.id = m.sender_id
			  AND (p_direct_sender.email_address = ? OR p_direct_sender.phone_number = ?)
		))`)
			args = append(args, filter.Sender, filter.Sender, filter.Sender, filter.Sender)
		} else if filter.MatchesEmpty(ViewSenders) {
			// A message has an "empty sender" only if it has no from-recipient with a
			// non-empty email/phone AND no direct sender_id. NOT EXISTS keeps the
			// predicate message-scoped (no per-from-row multiplication).
			conditions = append(conditions, `(NOT EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND (
			    (p_filter_from.email_address IS NOT NULL AND p_filter_from.email_address != '') OR
			    (p_filter_from.phone_number IS NOT NULL AND p_filter_from.phone_number != '')
			  )
		) AND m.sender_id IS NULL)`)
		}

		// Sender name filter — check both message_recipients (email) and direct sender_id (WhatsApp/chat).
		// Uses EXISTS so a message with multiple 'from' rows sharing the queried
		// display name is not multiplied into duplicate result rows.
		if filter.SenderName != "" {
			conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM participants p_direct_sender
			WHERE p_direct_sender.id = m.sender_id
			  AND %s = ?
		))`, recipientNameExpr("mr_filter_from", "p_filter_from"), participantNameExpr("p_direct_sender")))
			args = append(args, filter.SenderName, filter.SenderName)
		}
	}

	if filter.SenderName == "" && filter.MatchesEmpty(ViewSenderNames) {
		// A message has an "empty sender name" only if it has no from-recipient name AND no direct sender_id with a name.
		conditions = append(conditions, fmt.Sprintf(`(NOT EXISTS (
			SELECT 1 FROM message_recipients mr_sn
			JOIN participants p_sn ON p_sn.id = mr_sn.participant_id
			WHERE mr_sn.message_id = m.id
			  AND mr_sn.recipient_type = 'from'
			  AND %s != ''
		) AND NOT EXISTS (
			SELECT 1 FROM participants p_ds
			WHERE p_ds.id = m.sender_id
			  AND %s IS NOT NULL
		))`, recipientNameExpr("mr_sn", "p_sn"), participantNameExpr("p_ds")))
	}

	// Recipient + recipient-name filters — use EXISTS to avoid 1:N join
	// multiplication.
	//
	// When BOTH the email and the display name are filtered, they must match
	// the SAME to/cc/bcc row, not two independent EXISTS that a
	// multi-recipient message could satisfy via different rows. So fold them
	// into a single correlated EXISTS when both are set; otherwise keep the
	// per-field EXISTS.
	if filter.Recipient != "" && filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_filter_to
			JOIN participants p_filter_to ON p_filter_to.id = mr_filter_to.participant_id
			WHERE mr_filter_to.message_id = m.id
			  AND mr_filter_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND p_filter_to.email_address = ?
			  AND %s = ?
		)`, recipientNameExpr("mr_filter_to", "p_filter_to")))
		args = append(args, filter.Recipient, filter.RecipientName)
	} else if filter.Recipient != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr_filter_to
			JOIN participants p_filter_to ON p_filter_to.id = mr_filter_to.participant_id
			WHERE mr_filter_to.message_id = m.id
			  AND mr_filter_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND p_filter_to.email_address = ?
		)`)
		args = append(args, filter.Recipient)
	} else if filter.MatchesEmpty(ViewRecipients) {
		conditions = append(conditions, `NOT EXISTS (
			SELECT 1 FROM message_recipients mr_filter_to
			WHERE mr_filter_to.message_id = m.id
			  AND mr_filter_to.recipient_type IN ('to', 'cc', 'bcc')
		)`)
	}

	// Recipient name filter — use EXISTS to avoid 1:N join multiplication.
	// When the recipient email is also set, the combined predicate above
	// already constrains the name to the same to/cc/bcc row.
	if filter.RecipientName != "" && filter.Recipient == "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_filter_to
			JOIN participants p_filter_to ON p_filter_to.id = mr_filter_to.participant_id
			WHERE mr_filter_to.message_id = m.id
			  AND mr_filter_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s = ?
		)`, recipientNameExpr("mr_filter_to", "p_filter_to")))
		args = append(args, filter.RecipientName)
	} else if filter.RecipientName == "" && filter.MatchesEmpty(ViewRecipientNames) {
		conditions = append(conditions, fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM message_recipients mr_rn
			JOIN participants p_rn ON p_rn.id = mr_rn.participant_id
			WHERE mr_rn.message_id = m.id
			  AND mr_rn.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s != ''
		)`, recipientNameExpr("mr_rn", "p_rn")))
	}

	// Domain filter — use EXISTS so a message with multiple 'from' rows sharing
	// the queried domain is not multiplied into duplicate result rows.
	if filter.Domain != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND p_filter_from.domain = ?
		)`)
		args = append(args, filter.Domain)
	} else if filter.MatchesEmpty(ViewDomains) {
		// A message has an "empty domain" only if it has no from-recipient with a
		// non-empty domain. NOT EXISTS keeps the predicate message-scoped.
		conditions = append(conditions, `NOT EXISTS (
			SELECT 1 FROM message_recipients mr_filter_from
			JOIN participants p_filter_from ON p_filter_from.id = mr_filter_from.participant_id
			WHERE mr_filter_from.message_id = m.id
			  AND mr_filter_from.recipient_type = 'from'
			  AND p_filter_from.domain IS NOT NULL
			  AND p_filter_from.domain != ''
		)`)
	}

	// Label filter — use EXISTS to avoid 1:N join multiplication.
	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_labels ml_filter
			JOIN labels l_filter ON l_filter.id = ml_filter.label_id
			WHERE ml_filter.message_id = m.id
			  AND LOWER(l_filter.name) = LOWER(?)
		)`)
		args = append(args, filter.Label)
	} else if filter.MatchesEmpty(ViewLabels) {
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id)")
	}

	// Time period filter
	if filter.TimeRange.Period != "" {
		granularity := filter.TimeRange.Granularity
		if granularity == TimeYear && len(filter.TimeRange.Period) > 4 {
			switch len(filter.TimeRange.Period) {
			case 7:
				granularity = TimeMonth
			case 10:
				granularity = TimeDay
			}
		}

		var gran string
		switch granularity {
		case TimeYear:
			gran = "year"
		case TimeMonth:
			gran = "month"
		case TimeDay:
			gran = "day"
		default:
			gran = "month"
		}
		timeExpr := e.dialect.TimeTruncExpression(prefix+"sent_at", gran)
		conditions = append(conditions, timeExpr+" = ?")
		args = append(args, filter.TimeRange.Period)
	}

	return "", conditions, args
}

// SubAggregate performs aggregation on a filtered subset of messages.
// This is used for sub-grouping after drill-down.
func (e *SQLiteEngine) SubAggregate(ctx context.Context, filter MessageFilter, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	// Reconcile opts.HideDeletedFromSource into filter so the helper
	// inside buildFilterJoinsAndConditions / optsToFilterConditions
	// sees the OR of both fields. Mirrors the DuckDB SubAggregate
	// path so both engines emit one authoritative live-message
	// predicate per query.
	if opts.HideDeletedFromSource {
		filter.HideDeletedFromSource = true
	}
	filterJoins, filterConditions, args := e.buildFilterJoinsAndConditions(filter, "m")

	// Add opts-based conditions. Note: optsToFilterConditions emits
	// its own LiveMessagesWhere clause (correct for the Aggregate
	// caller below, which doesn't go through buildFilterJoinsAndConditions).
	// In SubAggregate this means both filter-side and opts-side helpers
	// emit the same clause, producing a redundant-but-correct AND chain.
	optsConds, optsArgs := optsToFilterConditions(e.dialect, opts, "m.")
	filterConditions = append(filterConditions, optsConds...)
	args = append(args, optsArgs...)
	if !aggregateHasExplicitMessageType(filter, opts) {
		filterConditions = append(filterConditions, emailOnlyFilterM)
	}

	searchJoins, searchConds, searchArgs :=
		e.buildAggregateSearchParts(ctx, opts.SearchQuery, groupBy)
	filterConditions = append(filterConditions, searchConds...)
	args = append(args, searchArgs...)
	if searchJoins != "" {
		filterJoins += "\n" + searchJoins
	}

	return e.executeAggregate(ctx, groupBy, opts, filterJoins, filterConditions, args)
}

// Aggregate performs grouping based on the provided ViewType.
func (e *SQLiteEngine) Aggregate(ctx context.Context, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	conditions, args := optsToFilterConditions(e.dialect, opts, "m.")
	if !aggregateHasExplicitMessageType(MessageFilter{}, opts) {
		conditions = append(conditions, emailOnlyFilterM)
	}

	searchJoins, searchConds, searchArgs :=
		e.buildAggregateSearchParts(ctx, opts.SearchQuery, groupBy)
	conditions = append(conditions, searchConds...)
	args = append(args, searchArgs...)

	return e.executeAggregate(
		ctx, groupBy, opts, searchJoins, conditions, args,
	)
}

func aggregateHasExplicitMessageType(filter MessageFilter, opts AggregateOptions) bool {
	if filter.MessageType != "" {
		return true
	}
	if opts.SearchQuery == "" {
		return false
	}
	return len(search.Parse(opts.SearchQuery).MessageTypes) > 0
}

func sqliteMessageTypeCondition(alias string, messageTypes []string) (string, []any) {
	var conditions []string
	var args []any
	var exact []string
	includeEmail := false

	for _, typ := range messageTypes {
		typ = strings.TrimSpace(strings.ToLower(typ))
		if typ == "" {
			continue
		}
		if typ == messageTypeEmail {
			includeEmail = true
			continue
		}
		exact = append(exact, typ)
	}

	col := "message_type"
	if alias != "" {
		col = alias + ".message_type"
	}
	if includeEmail {
		conditions = append(conditions,
			fmt.Sprintf("(%s = ? OR %s IS NULL OR %s = '')", col, col, col))
		args = append(args, messageTypeEmail)
	}
	if len(exact) > 0 {
		placeholders := make([]string, len(exact))
		for i, typ := range exact {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		conditions = append(conditions,
			fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ",")))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return "(" + strings.Join(conditions, " OR ") + ")", args
}

// buildAggregateSearchParts parses a search query for aggregate views
// and returns (joins, conditions, args). For Labels view with label
// search, filters the grouping column directly.
func (e *SQLiteEngine) buildAggregateSearchParts(
	ctx context.Context, searchQuery string, groupBy ViewType,
) (string, []string, []any) {
	if searchQuery == "" {
		return "", nil, nil
	}

	q := search.Parse(searchQuery)

	var conditions []string
	var args []any

	// For Labels view with label search, filter the grouping
	// column (l.name) directly instead of adding a conflicting
	// label join. Strip labels from the parsed query before
	// building the generic parts.
	if groupBy == ViewLabels && len(q.Labels) > 0 {
		var labelParts []string
		for _, label := range q.Labels {
			labelParts = append(labelParts, metadataContainsExpression(e.dialect, "l.name"))
			args = append(args,
				"%"+escapeSQLiteLike(label)+"%")
		}
		conditions = append(conditions,
			"("+strings.Join(labelParts, " OR ")+")")
		q.Labels = nil
	}

	searchConds, searchArgs, ftsJoin :=
		e.buildSearchQueryParts(ctx, q)
	conditions = append(conditions, searchConds...)
	args = append(args, searchArgs...)

	// The only join buildSearchQueryParts emits is the optional FTS join;
	// all structured filters are EXISTS subqueries.
	return ftsJoin, conditions, args
}

// executeAggregate is the shared implementation for Aggregate and SubAggregate.
func (e *SQLiteEngine) executeAggregate(ctx context.Context, groupBy ViewType, opts AggregateOptions, filterJoins string, filterConditions []string, args []any) ([]AggregateRow, error) {
	dim, err := aggDimensionForView(e.dialect, groupBy, opts.TimeGranularity)
	if err != nil {
		return nil, err
	}

	sort, err := sortClause(opts)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit == 0 {
		limit = 100
	}

	filterWhere := "1=1"
	if len(filterConditions) > 0 {
		filterWhere = strings.Join(filterConditions, " AND ")
	}

	query := buildAggregateSQL(dim, filterJoins, filterWhere, sort)
	args = append(args, limit)
	return e.executeAggregateQuery(ctx, query, args)
}

// executeAggregateQuery runs an aggregate query and returns the results.
// Expects 6 columns: key, count, total_size, attachment_size, attachment_count, total_unique.
func (e *SQLiteEngine) executeAggregateQuery(ctx context.Context, query string, args []any) ([]AggregateRow, error) {
	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []AggregateRow
	for rows.Next() {
		var row AggregateRow
		if err := rows.Scan(&row.Key, &row.Count, &row.TotalSize, &row.AttachmentSize, &row.AttachmentCount, &row.TotalUnique); err != nil {
			return nil, fmt.Errorf("scan aggregate row: %w", err)
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregate rows: %w", err)
	}

	return results, nil
}

// ListMessages retrieves messages matching the filter.
func (e *SQLiteEngine) ListMessages(ctx context.Context, filter MessageFilter) ([]MessageSummary, error) {
	filterJoins, conditions, args := e.buildFilterJoinsAndConditions(filter, "m")

	// Build ORDER BY with validation. Every structured filter in
	// buildFilterJoinsAndConditions resolves through an EXISTS / NOT EXISTS
	// correlated subquery — including the from-side Sender, SenderName and
	// Domain branches — so the message_recipients/participants 1:N
	// relationships cannot multiply a message into duplicate result rows, and
	// SELECT DISTINCT is therefore unnecessary (and avoided per the SQL
	// guideline: never DISTINCT + JOIN). The displayed sender is resolved via
	// a correlated scalar subquery (LIMIT 1) so messages with multiple 'from'
	// rows still produce exactly one result row.
	var orderBy string
	switch filter.Sorting.Field {
	case MessageSortByDate:
		orderBy = "m.sent_at"
	case MessageSortBySize:
		orderBy = "COALESCE(m.size_estimate, 0)"
	case MessageSortBySubject:
		orderBy = "COALESCE(m.subject, '')"
	default:
		return nil, fmt.Errorf("unsupported message sort field: %d", filter.Sorting.Field)
	}
	if filter.Sorting.Direction == SortDesc {
		orderBy += " DESC"
	} else {
		orderBy += " ASC"
	}
	// Stable tiebreaker on the PK so pagination is deterministic when the
	// primary sort field ties (e.g. identical sent_at). m.id is non-null
	// and unique, mirroring GetGmailIDsByFilter's ORDER BY ... , m.id DESC.
	orderBy += ", m.id DESC"

	limit := filter.Pagination.Limit
	if limit == 0 {
		limit = 500
	}

	whereClause := "1=1"
	if len(conditions) > 0 {
		whereClause = strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_message_id,
			m.conversation_id,
			COALESCE(conv.source_conversation_id, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.snippet, ''),
			COALESCE(p_sender.email_address, ''),
			%s,
			COALESCE(p_sender.phone_number, ''),
			m.sent_at,
			COALESCE(m.size_estimate, 0),
			m.has_attachments,
			m.attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, ''),
			COALESCE(conv.title, '')
		FROM messages m
		%s
		LEFT JOIN conversations conv ON conv.id = m.conversation_id
		%s
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, sqliteSenderNameExpr, sqliteSenderJoin, filterJoins, whereClause, orderBy)

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// Fetch labels for each message (batch would be more efficient but this is simpler)
	if len(results) > 0 {
		if err := fetchParticipantsForMessageList(ctx, e.db, e.dialect.Rebind, "", results); err != nil {
			return nil, fmt.Errorf("fetch participants: %w", err)
		}
		if err := e.fetchLabelsForMessages(ctx, results); err != nil {
			return nil, fmt.Errorf("fetch labels: %w", err)
		}
	}

	return results, nil
}

// GetMessageSummariesByIDs returns summary rows (no body, no raw
// MIME) for the supplied IDs in the same order as ids. Missing IDs
// are silently dropped. Designed for vector/hybrid search hit
// hydration: ~3 SQL round-trips total (one base query + one labels
// batch) regardless of len(ids), versus 7N round-trips when callers
// loop GetMessage per hit.
func (e *SQLiteEngine) GetMessageSummariesByIDs(ctx context.Context, ids []int64) ([]MessageSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_message_id,
			m.conversation_id,
			COALESCE(conv.source_conversation_id, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.snippet, ''),
			COALESCE(p_sender.email_address, ''),
			%s,
			COALESCE(p_sender.phone_number, ''),
			m.sent_at,
			COALESCE(m.size_estimate, 0),
			m.has_attachments,
			m.attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, ''),
			COALESCE(conv.title, '')
		FROM messages m
		%s
		LEFT JOIN conversations conv ON conv.id = m.conversation_id
		WHERE m.id IN (%s) AND %s
	`, sqliteSenderNameExpr, sqliteSenderJoin, strings.Join(placeholders, ","), store.LiveMessagesWhere("m", true))

	rows, err := e.queryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("get message summaries by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]MessageSummary, len(ids))
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		byID[msg.ID] = msg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// Reassemble in caller-order so search rank is preserved.
	results := make([]MessageSummary, 0, len(byID))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			results = append(results, m)
		}
	}
	if len(results) > 0 {
		if err := e.fetchLabelsForMessages(ctx, results); err != nil {
			return nil, fmt.Errorf("fetch labels: %w", err)
		}
	}
	return results, nil
}

func (e *SQLiteEngine) fetchLabelsForMessages(ctx context.Context, messages []MessageSummary) error {
	return fetchLabelsForMessageList(ctx, e.db, e.dialect.Rebind, "", messages)
}

// GetMessage retrieves a full message by internal ID.
func (e *SQLiteEngine) GetMessage(ctx context.Context, id int64) (*MessageDetail, error) {
	return e.getMessageByQuery(ctx, "m.id = ?", id)
}

// GetMessageBySourceID retrieves a full message by source message ID (e.g., Gmail ID).
// Note: This searches across all accounts and returns the first match. For Gmail,
// message IDs are unique per account but theoretically could collide across accounts.
// In practice, Gmail IDs are random enough that collisions are astronomically unlikely.
// If you need to guarantee uniqueness, use the internal ID from GetMessage instead.
//
// A2 (deferred): the unscoped match mirrors the deletion write path
// (internal/store/messages.go MarkMessageDeletedByGmailID). Adding a source_id
// scope here is deferred for the same reason — see that function's doc and
// docs/internal/PG_STATUS.md.
func (e *SQLiteEngine) GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*MessageDetail, error) {
	return e.getMessageByQuery(ctx, "m.source_message_id = ?", sourceMessageID)
}

func (e *SQLiteEngine) getMessageByQuery(ctx context.Context, whereClause string, args ...any) (*MessageDetail, error) {
	return getMessageByQueryShared(ctx, e.db, e.dialect.Rebind, "", whereClause, args...)
}

// GetAttachment retrieves attachment metadata by ID.
func (e *SQLiteEngine) GetAttachment(ctx context.Context, id int64) (*AttachmentInfo, error) {
	var att AttachmentInfo
	err := e.queryRowContext(ctx, `
		SELECT id, COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(size, 0), COALESCE(content_hash, ''), COALESCE(storage_path, '')
		FROM attachments
		WHERE id = ?
	`, id).Scan(&att.ID, &att.Filename, &att.MimeType, &att.Size, &att.ContentHash, &att.StoragePath)
	if err == sql.ErrNoRows {
		return nil, nil //nolint:nilnil // Engine.GetAttachment uses (nil, nil) for not-found; callers branch on the nil result
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment: %w", err)
	}
	if isURLStoragePath(att.StoragePath) {
		att.URL = att.StoragePath
		att.ContentHash = ""
		att.StoragePath = ""
	}
	return &att, nil
}

// GetAttachmentsByHash retrieves all attachment metadata matching a content
// hash in stable ID order.
func (e *SQLiteEngine) GetAttachmentsByHash(ctx context.Context, contentHash string) ([]AttachmentInfo, error) {
	rows, err := e.queryContext(ctx, `
		SELECT id, COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(size, 0), COALESCE(content_hash, ''), COALESCE(storage_path, '')
		FROM attachments
		WHERE content_hash = ?
		ORDER BY id
	`, contentHash)
	if err != nil {
		return nil, fmt.Errorf("get attachments by hash: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var attachments []AttachmentInfo
	for rows.Next() {
		var att AttachmentInfo
		if err := rows.Scan(&att.ID, &att.Filename, &att.MimeType, &att.Size, &att.ContentHash, &att.StoragePath); err != nil {
			return nil, fmt.Errorf("scan attachment by hash: %w", err)
		}
		attachments = append(attachments, att)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments by hash: %w", err)
	}
	return attachments, nil
}

// GetMessageRaw returns the decompressed raw MIME data for a message.
func (e *SQLiteEngine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	return getMessageRawShared(ctx, e.db, e.dialect.Rebind, "", id)
}

// ListAccounts returns all source accounts.
func (e *SQLiteEngine) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	rows, err := e.queryContext(ctx, `
		SELECT id, source_type, identifier, COALESCE(display_name, '')
		FROM sources
		ORDER BY identifier
	`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var accounts []AccountInfo
	for rows.Next() {
		var acc AccountInfo
		if err := rows.Scan(&acc.ID, &acc.SourceType, &acc.Identifier, &acc.DisplayName); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, acc)
	}

	return accounts, rows.Err()
}

// GetTotalStats returns overall statistics.
func (e *SQLiteEngine) GetTotalStats(ctx context.Context, opts StatsOptions) (*TotalStats, error) {
	stats := &TotalStats{}

	// Build search conditions when SearchQuery is set.
	var searchConditions []string
	var searchArgs []any
	var searchFTSJoin string
	hasSearchMessageTypes := false
	if opts.SearchQuery != "" {
		q := search.Parse(opts.SearchQuery)
		hasSearchMessageTypes = len(q.MessageTypes) > 0
		searchConditions, searchArgs, searchFTSJoin = e.buildSearchQueryParts(ctx, q)
	}

	// Build WHERE clause for messages — always use m. prefix since we alias
	// the messages table for compatibility with search joins.
	var conditions []string
	var args []any
	// Restrict to email messages only; NULL and '' handle pre-message_type data.
	// Exclude rows soft-deleted by deduplicate; gate source-deleted on
	// opts.HideDeletedFromSource via the helper.
	if !hasSearchMessageTypes {
		conditions = append(conditions, emailOnlyFilterM)
	}
	conditions = append(conditions, store.LiveMessagesWhere("m", opts.HideDeletedFromSource))
	conditions, args = appendSourceFilter(
		conditions, args, "m.", opts.SourceID, opts.SourceIDs,
	)
	if opts.WithAttachmentsOnly {
		conditions = append(conditions, e.dialect.BoolTrueExpr("m.has_attachments"))
	}
	// Merge search conditions
	conditions = append(conditions, searchConditions...)
	args = append(args, searchArgs...)

	whereClause := "1=1"
	if len(conditions) > 0 {
		whereClause = strings.Join(conditions, " AND ")
	}

	// Build join clause for search. buildSearchQueryParts only ever emits
	// the optional FTS join (every structured filter is an EXISTS
	// subquery), so the FTS join is the sole join template here.
	joinClause := ""
	if searchFTSJoin != "" {
		joinClause += searchFTSJoin + "\n"
	}

	// Message stats — when the FTS join is present, use a subquery so the
	// outer COUNT sees only messages rows. The FTS JOIN is 1:1, and the
	// search filters from buildSearchQueryParts are all EXISTS-based (no
	// 1:N multiplication). SELECT without DISTINCT is correct and avoids the
	// PostgreSQL restriction that bans SELECT DISTINCT in subqueries with ORDER BY.
	var msgQuery string
	if joinClause != "" {
		msgQuery = fmt.Sprintf(`
			SELECT
				COUNT(*),
				COALESCE(SUM(CASE WHEN deleted_from_source_at IS NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(size_estimate), 0)
			FROM messages
			WHERE id IN (
				SELECT m.id FROM messages m
				%s
				WHERE %s
			)
		`, joinClause, whereClause)
	} else {
		msgQuery = fmt.Sprintf(`
			SELECT
				COUNT(*),
				COALESCE(SUM(CASE WHEN m.deleted_from_source_at IS NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN m.deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(size_estimate), 0)
			FROM messages m
			WHERE %s
		`, whereClause)
	}

	if err := e.queryRowContext(ctx, msgQuery, args...).Scan(
		&stats.MessageCount,
		&stats.ActiveMessageCount,
		&stats.SourceDeletedMessageCount,
		&stats.TotalSize,
	); err != nil {
		return nil, fmt.Errorf("message stats: %w", err)
	}

	// Attachment stats — use IN subquery only when search joins are present.
	var attQuery string
	if joinClause != "" {
		attQuery = fmt.Sprintf(`
			SELECT COUNT(*), COALESCE(SUM(a.size), 0)
			FROM attachments a
			WHERE a.message_id IN (
				SELECT m.id FROM messages m
				%s
				WHERE %s
			)
		`, joinClause, whereClause)
	} else {
		attQuery = fmt.Sprintf(`
			SELECT COUNT(*), COALESCE(SUM(a.size), 0)
			FROM attachments a
			JOIN messages m ON m.id = a.message_id
			WHERE %s
		`, whereClause)
	}

	if err := e.queryRowContext(ctx, attQuery, args...).Scan(&stats.AttachmentCount, &stats.AttachmentSize); err != nil {
		return nil, fmt.Errorf("attachment stats: %w", err)
	}

	// Label count - filter by source when sourceID is provided
	var labelQuery string
	if opts.SourceID != nil {
		labelQuery = "SELECT COUNT(*) FROM labels WHERE source_id = ?"
		if err := e.queryRowContext(ctx, labelQuery, *opts.SourceID).Scan(&stats.LabelCount); err != nil {
			return nil, fmt.Errorf("label count: %w", err)
		}
	} else {
		labelQuery = "SELECT COUNT(*) FROM labels"
		if err := e.queryRowContext(ctx, labelQuery).Scan(&stats.LabelCount); err != nil {
			return nil, fmt.Errorf("label count: %w", err)
		}
	}

	// Account count - verify source exists when filtering by sourceID
	if opts.SourceID != nil {
		if err := e.queryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE id = ?", *opts.SourceID).Scan(&stats.AccountCount); err != nil {
			return nil, fmt.Errorf("account count: %w", err)
		}
	} else {
		if err := e.queryRowContext(ctx, "SELECT COUNT(*) FROM sources").Scan(&stats.AccountCount); err != nil {
			return nil, fmt.Errorf("account count: %w", err)
		}
	}

	return stats, nil
}

// GetGmailIDsByFilter returns Gmail message IDs (source_message_id) matching a filter.
// This is more efficient than ListMessages when you only need the IDs.
//
// All filter predicates that would otherwise need 1:N joins
// (recipients, labels) are expressed as EXISTS subqueries so messages
// can never appear in the result set more than once. Without that, we
// would need SELECT DISTINCT — and PostgreSQL rejects SELECT DISTINCT
// when ORDER BY references columns not in the SELECT list, breaking
// the "most recent first" ordering callers (MCP, TUI) depend on under
// Pagination.Limit. The EXISTS form also matches the SQL guidance in
// CLAUDE.md ("Never use SELECT DISTINCT with JOINs — use EXISTS").
func (e *SQLiteEngine) GetGmailIDsByFilter(ctx context.Context, filter MessageFilter) ([]string, error) {
	var conditions []string
	var args []any

	// Exclude remote-deleted and dedup-soft-deleted messages.
	// Always pass true: this surface feeds remote-deletion staging and
	// must never honor an opt-in.
	conditions = append(conditions, store.LiveMessagesWhere("m", true))

	conditions, args = appendSourceFilter(conditions, args, "m.", filter.SourceID, filter.SourceIDs)

	// Scope to Gmail sources only — this function is used for
	// Gmail-specific deletion/staging workflows and must not return
	// WhatsApp or other source IDs. 1:1 with messages, so kept as a
	// JOIN; the other filter predicates below use EXISTS to stay
	// non-multiplicative.
	joins := []string{`JOIN sources s_gmail ON s_gmail.id = m.source_id AND s_gmail.source_type = 'gmail'`}

	// When BOTH the email and the display name are filtered, they must
	// match the SAME from-row (or the SAME direct sender), not two
	// independent EXISTS that a multi-author message could satisfy via
	// different rows.
	if filter.Sender != "" && filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(
			EXISTS (
				SELECT 1 FROM message_recipients mr_from
				JOIN participants p_from ON p_from.id = mr_from.participant_id
				WHERE mr_from.message_id = m.id AND mr_from.recipient_type = 'from'
				  AND (p_from.email_address = ? OR p_from.phone_number = ?)
				  AND %s = ?
			)
			OR EXISTS (
				SELECT 1 FROM participants p_ds
				WHERE p_ds.id = m.sender_id
				  AND (p_ds.email_address = ? OR p_ds.phone_number = ?)
				  AND %s = ?
			)
		)`, recipientNameExpr("mr_from", "p_from"), participantNameExpr("p_ds")))
		args = append(args, filter.Sender, filter.Sender, filter.SenderName, filter.Sender, filter.Sender, filter.SenderName)
	} else if filter.Sender != "" {
		conditions = append(conditions, `(
			EXISTS (
				SELECT 1 FROM message_recipients mr_from
				JOIN participants p_from ON p_from.id = mr_from.participant_id
				WHERE mr_from.message_id = m.id AND mr_from.recipient_type = 'from'
				  AND (p_from.email_address = ? OR p_from.phone_number = ?)
			)
			OR EXISTS (
				SELECT 1 FROM participants p_ds
				WHERE p_ds.id = m.sender_id
				  AND (p_ds.email_address = ? OR p_ds.phone_number = ?)
			)
		)`)
		args = append(args, filter.Sender, filter.Sender, filter.Sender, filter.Sender)
	} else if filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(
			EXISTS (
				SELECT 1 FROM message_recipients mr_from
				JOIN participants p_from ON p_from.id = mr_from.participant_id
				WHERE mr_from.message_id = m.id AND mr_from.recipient_type = 'from'
				  AND %s = ?
			)
			OR EXISTS (
				SELECT 1 FROM participants p_ds
				WHERE p_ds.id = m.sender_id AND %s = ?
			)
		)`, recipientNameExpr("mr_from", "p_from"), participantNameExpr("p_ds")))
		args = append(args, filter.SenderName, filter.SenderName)
	}

	// When BOTH the recipient email and the display name are filtered, they
	// must match the SAME to/cc/bcc row, not two independent EXISTS that a
	// multi-recipient message could satisfy via different rows.
	if filter.Recipient != "" && filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_to
			JOIN participants p_to ON p_to.id = mr_to.participant_id
			WHERE mr_to.message_id = m.id
			  AND mr_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND p_to.email_address = ?
			  AND %s = ?
		)`, recipientNameExpr("mr_to", "p_to")))
		args = append(args, filter.Recipient, filter.RecipientName)
	} else if filter.Recipient != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr_to
			JOIN participants p_to ON p_to.id = mr_to.participant_id
			WHERE mr_to.message_id = m.id
			  AND mr_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND p_to.email_address = ?
		)`)
		args = append(args, filter.Recipient)
	} else if filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_to
			JOIN participants p_to ON p_to.id = mr_to.participant_id
			WHERE mr_to.message_id = m.id
			  AND mr_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s = ?
		)`, recipientNameExpr("mr_to", "p_to")))
		args = append(args, filter.RecipientName)
	}

	if filter.Domain != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr_from
			JOIN participants p_from ON p_from.id = mr_from.participant_id
			WHERE mr_from.message_id = m.id AND mr_from.recipient_type = 'from'
			  AND p_from.domain = ?
		)`)
		args = append(args, filter.Domain)
	}

	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_labels ml
			JOIN labels l ON l.id = ml.label_id
			WHERE ml.message_id = m.id AND LOWER(l.name) = LOWER(?)
		)`)
		args = append(args, filter.Label)
	}

	if filter.TimeRange.Period != "" {
		// Infer granularity from TimePeriod format if not explicitly set
		granularity := filter.TimeRange.Granularity
		if granularity == TimeYear && len(filter.TimeRange.Period) > 4 {
			switch len(filter.TimeRange.Period) {
			case 7:
				granularity = TimeMonth
			case 10:
				granularity = TimeDay
			}
		}

		var gran string
		switch granularity {
		case TimeYear:
			gran = "year"
		case TimeMonth:
			gran = "month"
		case TimeDay:
			gran = "day"
		default:
			gran = "month"
		}
		timeExpr := e.dialect.TimeTruncExpression("m.sent_at", gran)
		conditions = append(conditions, timeExpr+" = ?")
		args = append(args, filter.TimeRange.Period)
	}

	if filter.After != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, *filter.After)
	}
	if filter.Before != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, *filter.Before)
	}

	// Build query - only add LIMIT if explicitly set. DISTINCT is not
	// needed because every multiplicative filter is now an EXISTS
	// subquery; messages.id is PK so each row contributes exactly one
	// source_message_id.
	query := fmt.Sprintf(`
		SELECT m.source_message_id
		FROM messages m
		%s
		WHERE %s
		ORDER BY m.sent_at DESC, m.id DESC
	`, strings.Join(joins, "\n"), strings.Join(conditions, " AND "))

	// Only add LIMIT if explicitly set (0 means no limit)
	if filter.Pagination.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Pagination.Limit)
	}

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}

// GetGmailIDsByMessageIDs returns Gmail message IDs (source_message_id)
// for the given internal message IDs. It enforces the same constraints
// as GetGmailIDsByFilter: only live messages (LiveMessagesWhere — not
// remote-deleted, not dedup-soft-deleted) from Gmail sources.
// Non-qualifying IDs are silently dropped, mirroring
// GetMessageSummariesByIDs semantics. The lookup is chunked so large
// explicit selections stay under the backend's bind-parameter limit.
func (e *SQLiteEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return gmailIDsByMessageIDsChunked(ctx, ids, e.gmailIDsForMessageIDChunk)
}

func (e *SQLiteEngine) gmailIDsForMessageIDChunk(ctx context.Context, ids []int64) ([]gmailIDRow, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT m.source_message_id, m.sent_at, m.id
		FROM messages m
		JOIN sources s_gmail ON s_gmail.id = m.source_id AND s_gmail.source_type = 'gmail'
		WHERE %s AND m.id IN (%s)
	`, store.LiveMessagesWhere("m", true), strings.Join(placeholders, ","))

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids by message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDRows(rows)
}

// inListChunkSize bounds the IN-list size per deletion-staging lookup.
// Staging feeds full resolved Gmail-ID sets into GetAccountsByGmailIDs
// and full explicit selections into GetGmailIDsByMessageIDs, either of
// which can exceed the SQLite bind-parameter limit
// (SQLITE_MAX_VARIABLE_NUMBER, 32766 by default). 500 stays well under
// every backend's limit.
const inListChunkSize = 500

// accountsByGmailIDsChunked runs queryChunk over gmailIDs in
// inListChunkSize batches and returns the union of accounts,
// deduplicated and sorted ascending.
func accountsByGmailIDsChunked(
	ctx context.Context,
	gmailIDs []string,
	queryChunk func(ctx context.Context, chunk []string) ([]string, error),
) ([]string, error) {
	seen := make(map[string]struct{})
	for start := 0; start < len(gmailIDs); start += inListChunkSize {
		end := min(start+inListChunkSize, len(gmailIDs))
		accounts, err := queryChunk(ctx, gmailIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			seen[a] = struct{}{}
		}
	}
	accounts := make([]string, 0, len(seen))
	for a := range seen {
		accounts = append(accounts, a)
	}
	slices.Sort(accounts)
	return accounts, nil
}

// GetAccountsByGmailIDs returns the distinct Gmail account identifiers
// owning live messages with the given Gmail IDs (source_message_id),
// sorted ascending. Deletion staging uses this to stamp the manifest
// with its account and to reject selections spanning multiple accounts,
// since delete-staged executes a manifest against a single mailbox.
// The lookup is chunked so arbitrarily large selections stay under the
// backend's bind-parameter limit.
func (e *SQLiteEngine) GetAccountsByGmailIDs(ctx context.Context, gmailIDs []string) ([]string, error) {
	if len(gmailIDs) == 0 {
		return nil, nil
	}
	return accountsByGmailIDsChunked(ctx, gmailIDs, e.accountsForGmailIDChunk)
}

func (e *SQLiteEngine) accountsForGmailIDChunk(ctx context.Context, gmailIDs []string) ([]string, error) {
	placeholders := make([]string, len(gmailIDs))
	args := make([]any, len(gmailIDs))
	for i, id := range gmailIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT s.identifier
		FROM sources s
		WHERE s.source_type = 'gmail' AND EXISTS (
			SELECT 1 FROM messages m
			WHERE m.source_id = s.id AND %s AND m.source_message_id IN (%s)
		)
		ORDER BY s.identifier
	`, store.LiveMessagesWhere("m", true), strings.Join(placeholders, ","))

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get accounts by gmail ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}

// SearchByDomains returns messages where any participant (from, to, cc, or bcc)
// belongs to one of the given domains. Uses the shared executeSearchQuery
// path so results carry the same fields as Search/SearchFast (including
// deleted_at, conversation_title, message_type, and labels).
func (e *SQLiteEngine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]MessageSummary, error) {
	if len(domains) == 0 {
		return nil, nil
	}

	// Lower-cased placeholders for case-insensitive domain matching.
	placeholders := make([]string, len(domains))
	args := make([]any, 0, len(domains)+2)
	for i, d := range domains {
		placeholders[i] = "?"
		args = append(args, strings.ToLower(d))
	}

	conditions := []string{emailOnlyFilterM}
	// Hide dedup losers (deleted_at) and source-deleted rows so this MCP-facing
	// surface matches the visibility rules of Search/SearchFast.
	conditions = append(conditions,
		store.LiveMessagesWhere("m", true),
		fmt.Sprintf(`EXISTS (
		SELECT 1 FROM message_recipients mr_dom
		JOIN participants p_dom ON p_dom.id = mr_dom.participant_id
		WHERE mr_dom.message_id = m.id
		  AND LOWER(p_dom.domain) IN (%s)
	)`, strings.Join(placeholders, ", ")))

	if after != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, *after)
	}
	if before != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, *before)
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	return e.executeSearchQuery(ctx, conditions, args, "", limit, offset)
}

// Search performs a Gmail-style search query.
// buildSearchQueryParts builds the WHERE conditions, args, and FTS join
// for a search query. This is shared between Search and SearchFastCount.
// Every structured filter resolves through an EXISTS / NOT EXISTS
// correlated subquery, so the only join this ever emits is the optional
// FTS join (ftsJoin); there is no separate non-EXISTS join slot.
func (e *SQLiteEngine) buildSearchQueryParts(ctx context.Context, q *search.Query) (conditions []string, args []any, ftsJoin string) {
	// Exclude rows soft-deleted by deduplicate; gate source-deleted on
	// q.HideDeleted via the helper.
	conditions = append(conditions, store.LiveMessagesWhere("m", q.HideDeleted))

	// From filter - uses EXISTS to avoid join multiplication in aggregates.
	// Handles both exact addresses and @domain patterns.
	if len(q.FromAddrs) > 0 {
		var fromParts []string
		for _, addr := range q.FromAddrs {
			if strings.HasPrefix(addr, "@") {
				fromParts = append(fromParts,
					"LOWER(p_from.email_address) LIKE ?")
				args = append(args, "%"+addr)
			} else {
				fromParts = append(fromParts,
					"LOWER(p_from.email_address) = LOWER(?)")
				args = append(args, addr)
			}
		}
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_from
			JOIN participants p_from ON p_from.id = mr_from.participant_id
			WHERE mr_from.message_id = m.id
			  AND mr_from.recipient_type = 'from'
			  AND (%s)
		)`, strings.Join(fromParts, " OR ")))
	}

	// To filter - EXISTS to avoid join multiplication. The column side
	// is wrapped in LOWER(); lowercase the bound args Go-side so the
	// IN list also matches stored case-folded values (mirrors the
	// From-filter convention above).
	if len(q.ToAddrs) > 0 {
		placeholders := make([]string, len(q.ToAddrs))
		for i, addr := range q.ToAddrs {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(addr))
		}
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_to
			JOIN participants p_to ON p_to.id = mr_to.participant_id
			WHERE mr_to.message_id = m.id
			  AND mr_to.recipient_type = 'to'
			  AND LOWER(p_to.email_address) IN (%s)
		)`, strings.Join(placeholders, ",")))
	}

	// CC filter - EXISTS to avoid join multiplication
	if len(q.CcAddrs) > 0 {
		placeholders := make([]string, len(q.CcAddrs))
		for i, addr := range q.CcAddrs {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(addr))
		}
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_cc
			JOIN participants p_cc ON p_cc.id = mr_cc.participant_id
			WHERE mr_cc.message_id = m.id
			  AND mr_cc.recipient_type = 'cc'
			  AND LOWER(p_cc.email_address) IN (%s)
		)`, strings.Join(placeholders, ",")))
	}

	// BCC filter - EXISTS to avoid join multiplication
	if len(q.BccAddrs) > 0 {
		placeholders := make([]string, len(q.BccAddrs))
		for i, addr := range q.BccAddrs {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(addr))
		}
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_recipients mr_bcc
			JOIN participants p_bcc ON p_bcc.id = mr_bcc.participant_id
			WHERE mr_bcc.message_id = m.id
			  AND mr_bcc.recipient_type = 'bcc'
			  AND LOWER(p_bcc.email_address) IN (%s)
		)`, strings.Join(placeholders, ",")))
	}

	// Label filter - case-insensitive substring match using EXISTS
	// so each label term can match a different row in message_labels.
	for _, label := range q.Labels {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM message_labels ml_lbl
			JOIN labels l_lbl ON l_lbl.id = ml_lbl.label_id
			WHERE ml_lbl.message_id = m.id
			  AND %s
		)`, metadataContainsExpression(e.dialect, "l_lbl.name")))
		args = append(args, "%"+escapeSQLiteLike(label)+"%")
	}

	// Subject filter. Use the dialect's Unicode-aware fold on both sides so
	// SQLite and PostgreSQL retain the same case-insensitive substring contract.
	if len(q.SubjectTerms) > 0 {
		for _, term := range q.SubjectTerms {
			conditions = append(conditions, metadataContainsExpression(e.dialect, "m.subject"))
			args = append(args, "%"+escapeSQLiteLike(term)+"%")
		}
	}

	// message_type: filter (e.g. sms, whatsapp, calendar_event). The store
	// API path (store/api.go) honors q.MessageTypes; the FTS query path must
	// too, or `--mode=fts` search silently ignores message_type scoping for
	// every non-email type.
	if len(q.MessageTypes) > 0 {
		condition, conditionArgs := sqliteMessageTypeCondition("m", q.MessageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// Has attachment filter
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions, e.dialect.BoolTrueExpr("m.has_attachments"))
	}

	// Date range filters. Bind time.Time directly rather than a naive
	// "2006-01-02 15:04:05" string: a formatted, offset-less string compared
	// against a PG TIMESTAMPTZ column is parsed in the session TimeZone (not
	// UTC), shifting the boundary under any non-UTC session. pgx encodes
	// time.Time with an explicit offset (timezone-stable), and go-sqlite3
	// serializes it to a sortable RFC3339 layout, so SQLite stays correct.
	// Matches optsToFilterConditions / the store search path. [cr2-9]
	if q.AfterDate != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, *q.AfterDate)
	}
	if q.BeforeDate != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, *q.BeforeDate)
	}

	// Size filters
	if q.LargerThan != nil {
		conditions = append(conditions, "m.size_estimate > ?")
		args = append(args, *q.LargerThan)
	}
	if q.SmallerThan != nil {
		conditions = append(conditions, "m.size_estimate < ?")
		args = append(args, *q.SmallerThan)
	}

	// Full-text search: use dialect FTS if available, fall back to LIKE.
	if len(q.TextTerms) > 0 {
		if e.hasFTSTable(ctx) {
			ftsJoin = e.dialect.FTSJoin()
			expr, arg := e.dialect.BuildFTSTerm(q.TextTerms)
			conditions = append(conditions, expr)
			if arg != "" {
				args = append(args, arg)
			}
		} else {
			// Fall back to LIKE-based search on subject/snippet only.
			// LOWER both sides so PostgreSQL's case-sensitive LIKE
			// returns the same hits as SQLite's ASCII-folded LIKE.
			for _, term := range q.TextTerms {
				likeTerm := "%" + escapeSQLiteLike(term) + "%"
				conditions = append(conditions,
					"(LOWER(m.subject) LIKE LOWER(?) ESCAPE '\\' OR LOWER(m.snippet) LIKE LOWER(?) ESCAPE '\\')")
				args = append(args, likeTerm, likeTerm)
			}
		}
	}

	// Account filter
	conditions, args = appendSourceFilter(conditions, args, "m.", nil, q.AccountIDs)

	return conditions, args, ftsJoin
}

func (e *SQLiteEngine) Search(ctx context.Context, q *search.Query, limit, offset int) ([]MessageSummary, error) {
	conditions, args, ftsJoin := e.buildSearchQueryParts(ctx, q)
	return e.executeSearchQuery(ctx, conditions, args, ftsJoin, limit, offset)
}

// SearchMessageBodies performs exact body-only full-text search and uses the
// active backend's native tokenizer to attach bounded context to every hit.
func (e *SQLiteEngine) SearchMessageBodies(ctx context.Context, q *search.Query, limit, offset int) ([]MessageSummary, error) {
	if q == nil || len(q.TextTerms) == 0 {
		return nil, errors.New("message body search requires at least one free-text term")
	}
	if err := validateMessageBodyContextQuery(q.TextTerms); err != nil {
		return nil, err
	}
	if !e.hasFTSTable(ctx) {
		return nil, fmt.Errorf("%w: run 'msgvault rebuild-fts' with an FTS-enabled build", ErrMessageBodySearchUnavailable)
	}
	if readinessSQL := e.dialect.FTSBodySearchReadinessSQL(); readinessSQL != "" {
		var ready bool
		if err := e.queryRowContext(ctx, readinessSQL).Scan(&ready); err != nil {
			return nil, fmt.Errorf("check message body search index readiness: %w", err)
		}
		if !ready {
			return nil, fmt.Errorf("%w: run 'msgvault rebuild-fts' or complete the FTS backfill, then retry", ErrMessageBodySearchIndexStale)
		}
	}

	structured := *q
	structured.TextTerms = nil
	conditions, args, ftsJoin := e.buildSearchQueryParts(ctx, &structured)
	expr, arg := e.dialect.BuildFTSBodyTerm(q.TextTerms)
	conditions = append(conditions, expr)
	if arg != "" {
		args = append(args, arg)
	}
	if ftsJoin == "" {
		ftsJoin = e.dialect.FTSJoin()
	}
	results, err := e.executeSearchQuery(ctx, conditions, args, ftsJoin, limit, offset)
	if err != nil {
		return nil, err
	}
	if err := e.attachMessageBodySearchContexts(ctx, results, q.TextTerms); err != nil {
		return nil, fmt.Errorf("extract message body contexts: %w", err)
	}
	return results, nil
}

// buildMetadataSearchQueryParts builds the metadata-only predicate shared by
// SearchFast and SearchFastCount. Structured operators retain the generic
// Search semantics, while free text is deliberately kept off the composite
// body FTS index.
func metadataContainsExpression(d Dialect, column string) string {
	value := d.UnicodeLowerExpression("COALESCE(" + column + ", '')")
	pattern := d.UnicodeLowerExpression("?")
	return fmt.Sprintf(`%s LIKE %s ESCAPE '\'`, value, pattern)
}

func (e *SQLiteEngine) buildMetadataSearchQueryParts(ctx context.Context, q *search.Query) (conditions []string, args []any, ftsJoin string) {
	structured := *q
	structured.TextTerms = nil
	conditions, args, ftsJoin = e.buildSearchQueryParts(ctx, &structured)

	for _, term := range q.TextTerms {
		pattern := "%" + escapeSQLiteLike(term) + "%"
		conditions = append(conditions, fmt.Sprintf(`(
			%s OR
			%s OR
			EXISTS (
				SELECT 1
				FROM message_recipients mr_meta
				JOIN participants p_meta ON p_meta.id = mr_meta.participant_id
				WHERE mr_meta.message_id = m.id
				  AND (
					%s OR
					%s OR
					%s OR
					%s
				  )
			) OR
			EXISTS (
				SELECT 1
				FROM participants p_direct_meta
				WHERE p_direct_meta.id = m.sender_id
				  AND (
					%s OR
					%s OR
					%s
				  )
			)
		)`,
			metadataContainsExpression(e.dialect, "m.subject"),
			metadataContainsExpression(e.dialect, "m.snippet"),
			metadataContainsExpression(e.dialect, "p_meta.email_address"),
			metadataContainsExpression(e.dialect, "p_meta.display_name"),
			metadataContainsExpression(e.dialect, "p_meta.phone_number"),
			metadataContainsExpression(e.dialect, "mr_meta.display_name"),
			metadataContainsExpression(e.dialect, "p_direct_meta.email_address"),
			metadataContainsExpression(e.dialect, "p_direct_meta.display_name"),
			metadataContainsExpression(e.dialect, "p_direct_meta.phone_number"),
		))
		for range 9 {
			args = append(args, pattern)
		}
	}

	return conditions, args, ftsJoin
}

// SearchFast searches message metadata and merges MessageFilter context into
// the query (drill-down filters, hide-deleted, etc.).
func (e *SQLiteEngine) SearchFast(ctx context.Context, q *search.Query, filter MessageFilter, limit, offset int) ([]MessageSummary, error) {
	mergedQuery := MergeFilterIntoQuery(q, filter)
	conditions, args, ftsJoin := e.buildMetadataSearchQueryParts(ctx, mergedQuery)
	return e.executeSearchQuery(ctx, conditions, args, ftsJoin, limit, offset)
}

// executeSearchQuery runs a search query built from conditions and the
// optional FTS join, returning paginated MessageSummary results. Shared
// by Search and SearchFast.
func (e *SQLiteEngine) executeSearchQuery(ctx context.Context, conditions []string, args []any, ftsJoin string, limit, offset int) ([]MessageSummary, error) {
	if limit == 0 {
		limit = 100
	}

	whereClause := strings.Join(conditions, " AND ")
	if whereClause == "" {
		whereClause = "1=1"
	}

	// All filter conditions in buildSearchQueryParts use EXISTS subqueries,
	// never plain JOINs, so no row multiplication occurs from filter conditions.
	// The sender is hydrated via a correlated scalar subquery (LIMIT 1) so that
	// messages with multiple 'from' recipients do not produce multiple result rows.
	query := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_message_id,
			m.conversation_id,
			COALESCE(conv.source_conversation_id, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.snippet, ''),
			COALESCE(p_sender.email_address, ''),
			%s,
			COALESCE(p_sender.phone_number, ''),
			m.sent_at,
			COALESCE(m.size_estimate, 0),
			m.has_attachments,
			m.attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, ''),
			COALESCE(conv.title, '')
		FROM messages m
		%s
		LEFT JOIN conversations conv ON conv.id = m.conversation_id
		%s
		WHERE %s
		ORDER BY m.sent_at DESC, m.id DESC
		LIMIT ? OFFSET ?
	`, sqliteSenderNameExpr, sqliteSenderJoin, ftsJoin, whereClause)

	args = append(args, limit, offset)

	rows, err := e.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// Fetch labels for results
	if len(results) > 0 {
		if err := e.fetchLabelsForMessages(ctx, results); err != nil {
			return nil, fmt.Errorf("fetch labels: %w", err)
		}
	}

	return results, nil
}

// MergeFilterIntoQuery combines a MessageFilter context with a search.Query.
// Most context filters are appended to existing query filters.
//
// Note on semantics: Appending to FromAddrs/ToAddrs produces OR semantics
// within each dimension (IN clause). Labels use per-term EXISTS subqueries
// with AND semantics (message must have all labels). MessageType and date
// filters are scoped intersections so an in-view search cannot widen outside
// the current drill-down context.
func MergeFilterIntoQuery(q *search.Query, filter MessageFilter) *search.Query {
	// Copy all fields from original query (preserves any future non-slice fields)
	merged := *q

	// Deep copy slices to avoid mutating original (shallow copy + append can
	// mutate if original slice has spare capacity)
	merged.TextTerms = append([]string(nil), q.TextTerms...)
	merged.FromAddrs = append([]string(nil), q.FromAddrs...)
	merged.ToAddrs = append([]string(nil), q.ToAddrs...)
	merged.CcAddrs = append([]string(nil), q.CcAddrs...)
	merged.BccAddrs = append([]string(nil), q.BccAddrs...)
	merged.SubjectTerms = append([]string(nil), q.SubjectTerms...)
	merged.Labels = append([]string(nil), q.Labels...)
	merged.MessageTypes = append([]string(nil), q.MessageTypes...)
	// Deep-copy AccountIDs alongside the other slices so the merged
	// query never aliases the original's slice header. Filter overrides
	// below replace the deep-copied slice when set.
	merged.AccountIDs = append([]int64(nil), q.AccountIDs...)

	// Account filter - always apply if set. Multi-source SourceIDs takes
	// precedence over single SourceID, matching appendSourceFilter
	// semantics elsewhere in the package: a non-nil but empty SourceIDs
	// slice is "match nothing" (the caller explicitly scoped to no
	// sources) and must clear any AccountIDs the original query carried.
	// Allocate a fresh slice (not append-from-nil, which would collapse
	// an explicit empty back to nil and lose the match-nothing signal).
	if filter.SourceIDs != nil {
		merged.AccountIDs = make([]int64, len(filter.SourceIDs))
		copy(merged.AccountIDs, filter.SourceIDs)
	} else if filter.SourceID != nil {
		merged.AccountIDs = []int64{*filter.SourceID}
	}

	// Sender filter - append to existing from: filters
	if filter.Sender != "" {
		merged.FromAddrs = append(merged.FromAddrs, filter.Sender)
	}

	// Recipient filter - append to existing to: filters
	if filter.Recipient != "" {
		merged.ToAddrs = append(merged.ToAddrs, filter.Recipient)
	}

	// Label filter - append to existing label: filters
	if filter.Label != "" {
		merged.Labels = append(merged.Labels, filter.Label)
	}

	// message_type filter - scope FTS search to the drill-down context's
	// type (e.g. Texts mode → sms/mms). Without this, SearchFast within a
	// type-scoped view would silently widen back to all message types.
	if filter.MessageType != "" {
		messageTypes, noMatches := ScopedMessageTypes(merged.MessageTypes, filter.MessageType)
		merged.MessageTypes = messageTypes
		if noMatches {
			merged.AccountIDs = []int64{}
		}
	}

	// Attachment filter - set if context requires attachments
	if filter.WithAttachmentsOnly {
		hasAttachment := true
		merged.HasAttachment = &hasAttachment
	}

	// Domain filter - add as @domain pattern (handled specially in Search)
	if filter.Domain != "" {
		merged.FromAddrs = append(merged.FromAddrs, "@"+filter.Domain)
	}

	// Hide-deleted filter
	if filter.HideDeletedFromSource {
		merged.HideDeleted = true
	}

	// Date range filters — intersect (take the stricter bound) so
	// a user-supplied after:/before: cannot widen beyond the current
	// drill-down context.
	if filter.After != nil {
		if merged.AfterDate == nil || filter.After.After(*merged.AfterDate) {
			merged.AfterDate = filter.After
		}
	}
	if filter.Before != nil {
		if merged.BeforeDate == nil || filter.Before.Before(*merged.BeforeDate) {
			merged.BeforeDate = filter.Before
		}
	}

	// TimeRange.Period can be converted to date bounds. A period
	// like "2024" → [2024-01-01, 2025-01-01), "2024-03" →
	// [2024-03-01, 2024-04-01), "2024-03-15" → [2024-03-15, 2024-03-16).
	if filter.TimeRange.Period != "" {
		if after, before, ok := timePeriodToBounds(
			filter.TimeRange.Period,
		); ok {
			if merged.AfterDate == nil ||
				after.After(*merged.AfterDate) {
				merged.AfterDate = &after
			}
			if merged.BeforeDate == nil ||
				before.Before(*merged.BeforeDate) {
				merged.BeforeDate = &before
			}
		}
	}

	// Note: SenderName, RecipientName, ConversationID, and
	// EmptyValueTargets cannot be represented in search.Query
	// and are not merged. Deep search within those drill-down
	// contexts will not be scoped to the current view.

	return &merged
}

// timePeriodToBounds converts a time period string to half-open date
// bounds [after, before). Returns ok=false if the format is unrecognized.
func timePeriodToBounds(period string) (after, before time.Time, ok bool) {
	switch len(period) {
	case 4: // "2024" → year
		t, err := time.Parse("2006", period)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return t, t.AddDate(1, 0, 0), true
	case 7: // "2024-03" → month
		t, err := time.Parse("2006-01", period)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return t, t.AddDate(0, 1, 0), true
	case 10: // "2024-03-15" → day
		t, err := time.Parse("2006-01-02", period)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return t, t.AddDate(0, 0, 1), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// SearchFastCount returns the total count of messages matching a search query.
// Uses the same query logic as SearchFast to ensure consistent counts.
func (e *SQLiteEngine) SearchFastCount(ctx context.Context, q *search.Query, filter MessageFilter) (int64, error) {
	mergedQuery := MergeFilterIntoQuery(q, filter)
	conditions, args, ftsJoin := e.buildMetadataSearchQueryParts(ctx, mergedQuery)
	return e.executeSearchCount(ctx, conditions, args, ftsJoin)
}

func (e *SQLiteEngine) executeSearchCount(ctx context.Context, conditions []string, args []any, ftsJoin string) (int64, error) {
	whereClause := strings.Join(conditions, " AND ")
	if whereClause == "" {
		whereClause = "1=1"
	}

	query := fmt.Sprintf(`
		SELECT COUNT(DISTINCT m.id)
		FROM messages m
		%s
		WHERE %s
	`, ftsJoin, whereClause)

	var count int64
	if err := e.queryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("search fast count: %w", err)
	}
	return count, nil
}

// getMetadataSearchStats computes every aggregate from the same metadata-only
// predicate used by SearchFast and SearchFastCount. Generic GetTotalStats keeps
// its composite full-text semantics for its independent public contract.
func (e *SQLiteEngine) getMetadataSearchStats(ctx context.Context, conditions []string, args []any, ftsJoin string) (*TotalStats, error) {
	whereClause := strings.Join(conditions, " AND ")
	if whereClause == "" {
		whereClause = "1=1"
	}

	searchMatches := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_id,
			m.deleted_from_source_at,
			COALESCE(m.size_estimate, 0) AS size_estimate
		FROM messages m
		%s
		WHERE %s
	`, ftsJoin, whereClause)

	stats := &TotalStats{}
	messageStatsQuery := fmt.Sprintf(`
		WITH search_matches AS (%s)
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN deleted_from_source_at IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(size_estimate), 0),
			COUNT(DISTINCT source_id)
		FROM search_matches
	`, searchMatches)
	if err := e.queryRowContext(ctx, messageStatsQuery, args...).Scan(
		&stats.MessageCount,
		&stats.ActiveMessageCount,
		&stats.SourceDeletedMessageCount,
		&stats.TotalSize,
		&stats.AccountCount,
	); err != nil {
		return nil, fmt.Errorf("metadata search message stats: %w", err)
	}

	attachmentStatsQuery := fmt.Sprintf(`
		WITH search_matches AS (%s)
		SELECT COUNT(*), COALESCE(SUM(a.size), 0)
		FROM attachments a
		JOIN search_matches sm ON sm.id = a.message_id
	`, searchMatches)
	if err := e.queryRowContext(ctx, attachmentStatsQuery, args...).Scan(
		&stats.AttachmentCount,
		&stats.AttachmentSize,
	); err != nil {
		return nil, fmt.Errorf("metadata search attachment stats: %w", err)
	}

	labelStatsQuery := fmt.Sprintf(`
		WITH search_matches AS (%s)
		SELECT COUNT(DISTINCT l.name)
		FROM labels l
		JOIN message_labels ml ON ml.label_id = l.id
		JOIN search_matches sm ON sm.id = ml.message_id
	`, searchMatches)
	if err := e.queryRowContext(ctx, labelStatsQuery, args...).Scan(&stats.LabelCount); err != nil {
		return nil, fmt.Errorf("metadata search label stats: %w", err)
	}

	return stats, nil
}

// SearchFastWithStats builds the metadata-only predicate once and reuses it
// for messages, count, and stats so all three describe the same match set.
func (e *SQLiteEngine) SearchFastWithStats(ctx context.Context, q *search.Query, queryStr string,
	filter MessageFilter, statsGroupBy ViewType, limit, offset int) (*SearchFastResult, error) {
	mergedQuery := MergeFilterIntoQuery(q, filter)
	conditions, args, ftsJoin := e.buildMetadataSearchQueryParts(ctx, mergedQuery)
	results, err := e.executeSearchQuery(ctx, conditions, args, ftsJoin, limit, offset)
	if err != nil {
		return nil, err
	}

	// Best-effort count: don't abort the search if count fails.
	count, countErr := e.executeSearchCount(ctx, conditions, args, ftsJoin)
	if countErr != nil {
		log.Printf("warning: search count failed (using -1): %v", countErr)
		count = -1
	}

	stats, _ := e.getMetadataSearchStats(ctx, conditions, args, ftsJoin)

	return &SearchFastResult{
		Messages:   results,
		TotalCount: count,
		Stats:      stats,
	}, nil
}
