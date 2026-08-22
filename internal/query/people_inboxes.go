package query

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PersonInboxRequest struct {
	CanonicalID int64
}

type PersonInboxRow struct {
	SourceID          int64      `json:"source_id"`
	SourceType        string     `json:"source_type"`
	SourceIdentifier  string     `json:"source_identifier"`
	ConversationCount int64      `json:"conversation_count"`
	ReceivedCount     int64      `json:"received_count"`
	SentCount         int64      `json:"sent_count"`
	LatestReceivedAt  *time.Time `json:"latest_received_at,omitempty"`
	LatestSentAt      *time.Time `json:"latest_sent_at,omitempty"`
	LatestAt          time.Time  `json:"latest_at"`
}

type PersonInboxResponse struct {
	Rows             []PersonInboxRow `json:"rows"`
	CacheRevision    string           `json:"cache_revision"`
	IdentityRevision int64            `json:"identity_revision"`
}

// ListPersonInboxes returns one row per chat-capable source used by a
// canonical participant cluster. relationship_activity is physically one
// row per message, canonical identity, and participant domain, so the
// contact_chat CTE first reduces it to one row per message-level logical
// activity before computing directional message totals.
func (e *DuckDBEngine) ListPersonInboxes(ctx context.Context, request PersonInboxRequest) (*PersonInboxResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if request.CanonicalID < 1 {
		return nil, fmt.Errorf("%w: canonical participant ID must be positive", ErrInvalidExploreRequest)
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}

	queryText := fmt.Sprintf(`
WITH contact_chat AS (
	SELECT DISTINCT message_id AS entry_key, source_id, source_type,
	                conversation_id, occurred_at, is_from_me
	FROM read_parquet('%s', hive_partitioning=true, union_by_name=true)
	WHERE canonical_id = ? AND is_chat
)
SELECT c.source_id, c.source_type, s.account_email,
	   COUNT(DISTINCT c.conversation_id),
	   COUNT(*) FILTER (WHERE NOT c.is_from_me),
	   COUNT(*) FILTER (WHERE c.is_from_me),
	   MAX(c.occurred_at) FILTER (WHERE NOT c.is_from_me),
	   MAX(c.occurred_at) FILTER (WHERE c.is_from_me),
	   MAX(c.occurred_at)
FROM contact_chat c
JOIN read_parquet('%s') s ON s.id = c.source_id
GROUP BY c.source_id, c.source_type, s.account_email
ORDER BY MAX(c.occurred_at) DESC, c.source_id`,
		e.identityActivityPath(), quoteIdentitySQLPath(e.parquetPath(datasetSources)))
	rows, err := e.db.QueryContext(ctx, queryText, request.CanonicalID)
	if err != nil {
		return nil, fmt.Errorf("query person inboxes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	response := &PersonInboxResponse{
		Rows:             make([]PersonInboxRow, 0),
		CacheRevision:    state.Revision(),
		IdentityRevision: state.IdentityRevision,
	}
	for rows.Next() {
		var row PersonInboxRow
		var latestReceivedAt, latestSentAt sql.NullTime
		if err := rows.Scan(
			&row.SourceID, &row.SourceType, &row.SourceIdentifier,
			&row.ConversationCount, &row.ReceivedCount, &row.SentCount,
			&latestReceivedAt, &latestSentAt, &row.LatestAt,
		); err != nil {
			return nil, fmt.Errorf("scan person inbox row: %w", err)
		}
		if latestReceivedAt.Valid {
			row.LatestReceivedAt = &latestReceivedAt.Time
		}
		if latestSentAt.Valid {
			row.LatestSentAt = &latestSentAt.Time
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person inboxes: %w", err)
	}
	return response, nil
}
