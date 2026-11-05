package query

import (
	"context"
	"fmt"
	"strings"
)

// SenderLabeler resolves the sender label shown beside a message row.
type SenderLabeler interface {
	SenderLabels(ctx context.Context, messageIDs []int64) (map[int64]string, error)
}

// SenderLabels resolves each archived message's sender label, including
// messages deleted from their source, which stay visible in Explore and Files.
func (e *DuckDBEngine) SenderLabels(ctx context.Context, messageIDs []int64) (map[int64]string, error) {
	labels := make(map[int64]string, len(messageIDs))
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	query := `WITH page AS (
		SELECT id, sender_id FROM messages WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)
	), senders AS (
		SELECT mr.message_id, FIRST(` + recipientNameExpr("mr", "p") + `) AS from_name
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.recipient_type = 'from' AND mr.message_id IN (SELECT id FROM page)
		GROUP BY mr.message_id
	)
	SELECT page.id, COALESCE(senders.from_name, ` + participantNameExpr("direct") + `, '')
	FROM page
	LEFT JOIN senders ON senders.message_id = page.id
	LEFT JOIN participants direct ON direct.id = page.sender_id AND senders.message_id IS NULL`
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sender labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("scan sender label: %w", err)
		}
		labels[id] = label
	}
	return labels, rows.Err()
}
