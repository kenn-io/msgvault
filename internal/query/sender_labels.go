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
	rows, err := e.db.QueryContext(ctx, "SELECT id, from_name FROM v_messages WHERE id IN ("+
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
