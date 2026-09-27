package store

import (
	"context"
	"fmt"
)

// SourceMessageCounts holds per-source message totals as account listings
// report them: live messages and messages retained after their source
// deleted them. Dedup-hidden rows count in neither.
type SourceMessageCounts struct {
	Live          int64
	SourceDeleted int64
}

// CountMessagesBySourceContext returns SourceMessageCounts for every source
// with messages in one pass, instead of two counts per source. Sources with
// no messages are absent from the map (their counts are zero).
func (s *Store) CountMessagesBySourceContext(ctx context.Context) (map[int64]SourceMessageCounts, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT source_id,
			SUM(CASE WHEN %s THEN 1 ELSE 0 END),
			SUM(CASE WHEN %s THEN 1 ELSE 0 END)
		FROM messages
		GROUP BY source_id`,
		LiveMessagesWhere("", true), SourceDeletedMessagesWhere("")))
	if err != nil {
		return nil, fmt.Errorf("count messages by source: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[int64]SourceMessageCounts)
	for rows.Next() {
		var sourceID int64
		var c SourceMessageCounts
		if err := rows.Scan(&sourceID, &c.Live, &c.SourceDeleted); err != nil {
			return nil, fmt.Errorf("scan message counts by source: %w", err)
		}
		counts[sourceID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count messages by source: %w", err)
	}
	return counts, nil
}
