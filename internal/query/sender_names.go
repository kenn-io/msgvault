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
	sender, scoped, ok := senderNameScope(q)
	if !ok {
		return nil
	}
	knownConditions, knownArgs := appendSourceFilter([]string{"LOWER(p.email_address) = LOWER(?)"}, []any{sender}, "m.", nil, q.AccountIDs)
	knownConditions, knownArgs = appendSourceFilter(knownConditions, knownArgs, "m.", nil, sourcescope.IDs(ctx))
	var known bool
	if err := e.queryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM participants p
 CROSS JOIN message_recipients mr CROSS JOIN messages m
 WHERE mr.participant_id = p.id AND mr.recipient_type = 'from' AND m.id = mr.message_id AND `+strings.Join(knownConditions, " AND ")+`)`, knownArgs...).Scan(&known); err != nil {
		return senderHintFailure(err)
	}
	if known {
		return nil
	}
	conditions, args, join := buildParts(ctx, scoped)
	conditions, args = appendSourceFilter(conditions, args, "m.", nil, sourcescope.IDs(ctx))
	senderText := strings.TrimPrefix(sender, "@")
	if senderText == "" {
		senderText = sender
	}
	pattern := "%" + escapeSQLiteLike(senderText) + "%"
	participantIndex, recipientIndex := "", ""
	candidateArgs := []any{pattern, pattern}
	if candidate, ok := metadataTermCandidate(senderText); ok && e.hasMetadataFTS(ctx) {
		participantIndex = " AND p.id IN (SELECT rowid FROM participants_metadata_fts WHERE participants_metadata_fts MATCH ?)"
		recipientIndex = " AND mr.id IN (SELECT rowid FROM recipients_metadata_fts WHERE recipients_metadata_fts MATCH ?)"
		candidateArgs = append(candidateArgs, candidate, pattern, candidate)
	} else {
		candidateArgs = append(candidateArgs, pattern)
	}
	// First find addresses with matching sender metadata. Count all scoped
	// messages at each address, but require a matching label within that scope.
	// CROSS JOIN preserves SQLite's candidate-to-message lookup order.
	sql := fmt.Sprintf(`WITH candidates AS (
 SELECT LOWER(p.email_address) AS address FROM participants p
 WHERE p.email_address <> '' AND (%s OR %s)%s
 UNION
 SELECT LOWER(p.email_address) FROM message_recipients mr
 JOIN participants p ON p.id = mr.participant_id
 WHERE mr.recipient_type = 'from' AND p.email_address <> '' AND %s%s
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
 HAVING EXISTS (
 SELECT 1 FROM participants p_hint
 CROSS JOIN message_recipients mr_hint
 CROSS JOIN messages m %s
 WHERE LOWER(p_hint.email_address) = c.address
 AND mr_hint.participant_id = p_hint.id AND mr_hint.recipient_type = 'from'
 AND m.id = mr_hint.message_id AND %s
 AND (%s OR %s))
 ORDER BY COUNT(DISTINCT m.id) DESC, c.address LIMIT 3`,
		metadataContainsExpression(e.dialect, "p.display_name"), metadataContainsExpression(e.dialect, "p.email_address"),
		participantIndex, metadataContainsExpression(e.dialect, "mr.display_name"), recipientIndex, join, strings.Join(conditions, " AND "), join, strings.Join(conditions, " AND "),
		metadataContainsExpression(e.dialect, recipientNameExpr("mr_hint", "p_hint")), metadataContainsExpression(e.dialect, "p_hint.email_address"))
	args = append(append(candidateArgs, args...), args...)
	args = append(args, pattern, pattern)
	rows, err := e.queryContext(ctx, sql, args...)
	if err != nil {
		return senderHintFailure(err)
	}
	defer func() { _ = rows.Close() }()
	var labels []string
	var retry string
	for rows.Next() {
		var address string
		var count int64
		if err := rows.Scan(&address, &count); err != nil {
			return senderHintFailure(err)
		}
		if labels == nil {
			retry = search.Format(withSender(*q, address))
			address = "try " + retry
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
	message := fmt.Sprintf("from: matches addresses; senders matching %q: %s", senderText, labels[0])
	if len(labels) > 1 {
		message += "; other matching senders: " + strings.Join(labels[1:], ", ")
	}
	hint := SearchHint{Code: "sender_name_requires_address", Message: message}
	hint.RetryWith.Query = retry
	return []SearchHint{hint}
}

// senderNameScope returns the one from: value shared by q and its
// alternatives, and a copy of q without it that keeps only sender-bearing
// alternatives. A retained alternative left unrestricted widens the count to
// the parent scope.
func senderNameScope(q *search.Query) (string, *search.Query, bool) {
	sender := ""
	for _, node := range append([]*search.Query{q}, q.Alternatives...) {
		switch {
		case len(node.FromAddrs) > 1, len(node.FromAddrs) == 1 && sender != "" && node.FromAddrs[0] != sender:
			return "", nil, false
		case len(node.FromAddrs) == 1:
			sender = node.FromAddrs[0]
		}
	}
	scoped := *q
	scoped.FromAddrs, scoped.Alternatives = nil, nil
	for _, alternative := range q.Alternatives {
		if len(alternative.FromAddrs) == 0 {
			continue
		}
		branch := *alternative
		branch.FromAddrs = nil
		if branch.IsEmpty() {
			scoped.Alternatives = nil
			break
		}
		scoped.Alternatives = append(scoped.Alternatives, &branch)
	}
	return sender, &scoped, sender != ""
}

// withSender returns a copy of q with each from: value replaced by address.
func withSender(q search.Query, address string) *search.Query {
	if len(q.FromAddrs) > 0 {
		q.FromAddrs = []string{address}
	}
	alternatives := q.Alternatives
	q.Alternatives = nil
	for _, alternative := range alternatives {
		q.Alternatives = append(q.Alternatives, withSender(*alternative, address))
	}
	return &q
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
