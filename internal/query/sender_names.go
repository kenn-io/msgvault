package query

import (
	"context"
	"fmt"
	"log"
	"strings"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
)

// SenderNameMatcher is optional on engines with exact sender-address search.
type SenderNameMatcher interface {
	MatchSenderName(ctx context.Context, q *search.Query) []SearchHint
}

// MatchSenderName counts corrected queries within the same search scope.
// The hint is best effort: it is nil when none applies or the lookup fails.
func (e *SQLiteEngine) MatchSenderName(ctx context.Context, q *search.Query) []SearchHint {
	return e.matchSenderName(ctx, q, e.buildSearchQueryParts)
}

func (e *SQLiteEngine) matchSenderName(ctx context.Context, q *search.Query,
	buildParts func(context.Context, *search.Query) ([]string, []any, string)) []SearchHint {
	if q.HasOR() || len(q.FromAddrs) != 1 {
		return nil
	}
	scoped := *q
	scoped.FromAddrs = nil
	conditions, args, join := buildParts(ctx, &scoped)
	conditions, args = appendSourceFilter(conditions, args, "m.", nil, sourcescope.IDs(ctx))
	// First find addresses with matching sender metadata. Count all scoped
	// messages at each address, but require a matching label within that scope.
	// CROSS JOIN preserves SQLite's candidate-to-message lookup order.
	sql := fmt.Sprintf(`WITH candidates AS (
 SELECT LOWER(p.email_address) AS address FROM participants p
 WHERE p.email_address <> '' AND (%s OR %s)
 UNION
 SELECT LOWER(p.email_address) FROM message_recipients mr
 JOIN participants p ON p.id = mr.participant_id
 WHERE mr.recipient_type = 'from' AND p.email_address <> '' AND %s
 )
 SELECT c.address, COUNT(DISTINCT m.id)
 FROM candidates c
 CROSS JOIN participants p_hint
 CROSS JOIN message_recipients mr_hint
 CROSS JOIN messages m %s
 WHERE LOWER(p_hint.email_address) = c.address
 AND mr_hint.participant_id = p_hint.id AND mr_hint.recipient_type = 'from'
 AND m.id = mr_hint.message_id AND %s
 GROUP BY c.address
 HAVING MAX(CASE WHEN %s OR %s THEN 1 ELSE 0 END) = 1
 ORDER BY COUNT(DISTINCT m.id) DESC, c.address LIMIT 3`,
		metadataContainsExpression(e.dialect, "p.display_name"), metadataContainsExpression(e.dialect, "p.email_address"),
		metadataContainsExpression(e.dialect, "mr.display_name"), join, strings.Join(conditions, " AND "),
		metadataContainsExpression(e.dialect, recipientNameExpr("mr_hint", "p_hint")), metadataContainsExpression(e.dialect, "p_hint.email_address"))
	senderText := strings.TrimPrefix(q.FromAddrs[0], "@")
	pattern := "%" + escapeSQLiteLike(senderText) + "%"
	args = append([]any{pattern, pattern, pattern}, args...)
	args = append(args, pattern, pattern)
	rows, err := e.queryContext(ctx, sql, args...)
	if err != nil {
		return senderHintFailure(err)
	}
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var address string
		var count int64
		if err := rows.Scan(&address, &count); err != nil {
			return senderHintFailure(err)
		}
		if labels == nil {
			scoped.FromAddrs = []string{address}
			address = "try " + search.Format(&scoped)
		}
		unit := "messages"
		if count == 1 {
			unit = "message"
		}
		labels = append(labels, fmt.Sprintf("%s (%d %s)", address, count, unit))
	}
	if err := rows.Err(); err != nil || labels == nil {
		return senderHintFailure(err)
	}
	retry := search.Format(&scoped)
	message := fmt.Sprintf("from: matches addresses; senders matching %q: %s", senderText, labels[0])
	if len(labels) > 1 {
		message += "; other matching senders: " + strings.Join(labels[1:], ", ")
	}
	hint := SearchHint{Code: "sender_name_requires_address", Message: message}
	hint.RetryWith.Query = retry
	return []SearchHint{hint}
}

// senderHintFailure logs a suppressed lookup error, if any, and returns no hint.
func senderHintFailure(err error) []SearchHint {
	if err != nil {
		log.Printf("warning: sender hint query failed: %v", err)
	}
	return nil
}

func (e *DuckDBEngine) MatchSenderName(ctx context.Context, q *search.Query) []SearchHint {
	if e.sqliteEngine == nil {
		return nil
	}
	return e.sqliteEngine.MatchSenderName(ctx, q)
}
