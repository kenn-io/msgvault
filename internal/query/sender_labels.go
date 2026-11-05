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

// sqlMessageSenderLabel renders the label of a message's sender: its
// sender_id participant, else its first "from" recipient. It labels the
// participant like analytical_entries' sender_display.
func sqlMessageSenderLabel(message string) string {
	return `(SELECT ` + sqlAnalyticalEntriesParticipantLabel("sender") + ` FROM participants sender
		WHERE sender.id = COALESCE(` + message + `.sender_id, (SELECT MIN(mr.participant_id) FROM message_recipients mr
			WHERE mr.message_id = ` + message + `.id AND mr.recipient_type = 'from')))`
}

// SenderLabels resolves each archived message's sender label, including
// messages deleted from their source, which stay visible in Explore and Files.
func (e *DuckDBEngine) SenderLabels(ctx context.Context, messageIDs []int64) (map[int64]string, error) {
	labels := make(map[int64]string, len(messageIDs))
	if e.analyticsDir == "" || len(messageIDs) == 0 {
		return labels, nil
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	rows, err := e.db.QueryContext(ctx, "SELECT m.id, COALESCE("+sqlMessageSenderLabel("m")+", '') FROM messages m WHERE m.id IN ("+
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...)
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
