package query

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
)

// SenderNameHint offers an address query after an exact sender search misses.
type SenderNameHint struct {
	Message string `json:"message"`
	Query   string `json:"query"`
}

// SenderNameMatcher is optional on engines with exact sender-address search.
type SenderNameMatcher interface {
	MatchSenderName(ctx context.Context, q *search.Query, filter MessageFilter) *SenderNameHint
}

// MatchSenderName counts corrected queries within the same search and view scope.
// The hint is best effort: it is nil when none applies or the lookup fails.
func (e *SQLiteEngine) MatchSenderName(ctx context.Context, q *search.Query, filter MessageFilter) *SenderNameHint {
	return e.matchSenderName(ctx, q, filter, false)
}

func (e *SQLiteEngine) matchSenderName(ctx context.Context, q *search.Query, filter MessageFilter, metadata bool) *SenderNameHint {
	if q.HasOR() || len(q.FromAddrs) != 1 || strings.Contains(q.FromAddrs[0], "@") {
		return nil
	}
	scoped := *q
	scoped.FromAddrs = nil
	var conditions []string
	var args []any
	var join string
	if metadata {
		conditions, args, join = e.buildFilteredMetadataSearchQueryParts(ctx, &scoped, filter)
	} else {
		conditions, args, join = e.buildFilteredDeepSearchQueryParts(ctx, &scoped, filter)
	}
	conditions, args = appendSourceFilter(conditions, args, "m.", nil, sourcescope.IDs(ctx))
	conditions = append(conditions, "p_hint.email_address <> ''")
	// Count all messages at each matching address, including messages whose own
	// sender label differs, so the count agrees with the corrected address query.
	sql := fmt.Sprintf(`WITH eligible AS (
 SELECT m.id, LOWER(p_hint.email_address) AS address, %s AS name
 FROM messages m %s
 JOIN message_recipients mr_hint ON mr_hint.message_id = m.id AND mr_hint.recipient_type = 'from'
 JOIN participants p_hint ON p_hint.id = mr_hint.participant_id
 WHERE %s
 ) SELECT address, COUNT(DISTINCT id) FROM eligible
 WHERE address IN (SELECT address FROM eligible WHERE %s OR %s)
 GROUP BY address ORDER BY COUNT(DISTINCT id) DESC, address LIMIT 3`,
		recipientNameExpr("mr_hint", "p_hint"), join, strings.Join(conditions, " AND "), metadataContainsExpression(e.dialect, "name"), metadataContainsExpression(e.dialect, "address"))
	pattern := "%" + escapeSQLiteLike(q.FromAddrs[0]) + "%"
	args = append(args, pattern, pattern)
	rows, err := e.queryContext(ctx, sql, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var labels []string
	var first string
	for rows.Next() {
		var address string
		var count int64
		if err := rows.Scan(&address, &count); err != nil {
			return nil
		}
		if first == "" {
			first = address
		}
		unit := "messages"
		if count == 1 {
			unit = "message"
		}
		labels = append(labels, fmt.Sprintf("%s (%d %s)", address, count, unit))
	}
	if rows.Err() != nil || first == "" {
		return nil
	}
	scoped.FromAddrs = []string{first}
	retry := search.Format(&scoped)
	return &SenderNameHint{Message: fmt.Sprintf("from: matches addresses; senders matching %q: %s; try %s", q.FromAddrs[0], strings.Join(labels, ", "), retry), Query: retry}
}

func (e *DuckDBEngine) MatchSenderName(ctx context.Context, q *search.Query, filter MessageFilter) *SenderNameHint {
	if e.sqliteEngine == nil {
		return nil
	}
	return e.sqliteEngine.MatchSenderName(ctx, q, filter)
}
