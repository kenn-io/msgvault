package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ExploreMeetingResolver is the optional cache-backed capability used by
// meeting endpoints that need the exact logical Explore selection rather than
// one rendered page.
type ExploreMeetingResolver interface {
	ResolveExploreMeetings(
		ctx context.Context, selection ExploreSelectionRequest, limit int,
	) (*ExploreMeetingSelection, error)
}

// ExploreMeetingSelection reports both the complete logical selection and its
// meeting-only subset. MessageIDs is bounded by limit and LimitExceeded tells
// callers to reject the selection rather than consume a partial population.
type ExploreMeetingSelection struct {
	SelectedCount    int64            `json:"selected_count"`
	MeetingCount     int64            `json:"meeting_count"`
	MessageIDs       []int64          `json:"message_ids"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
	LimitExceeded    bool             `json:"limit_exceeded"`
}

// ResolveExploreMeetings resolves an Explore selection against one committed
// Parquet cache snapshot. It uses the same logical-entry and cluster expansion
// path as ExploreSelectionStats, while projecting only counts and meeting IDs.
func (e *DuckDBEngine) ResolveExploreMeetings(
	ctx context.Context, request ExploreSelectionRequest, limit int,
) (*ExploreMeetingSelection, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if limit < 1 {
		return nil, fmt.Errorf("%w: meeting selection limit must be positive", ErrInvalidExploreRequest)
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
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
	explore, err := e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}
	conditions, args := buildExploreConditions(explore)
	selection, selectionArgs := exploreSelectionWhere(request)
	args = append(args, selectionArgs...)
	args = append(args, limit+1)

	rows, err := e.db.QueryContext(ctx, buildExploreLogicalSQLNoLists(conditions)+`),
selected AS (
	SELECT entry_kind, anchor_message_id, message_type
	FROM logical_entries`+selection+`
), selection_counts AS (
	SELECT COUNT(*)::BIGINT AS selected_count,
		COALESCE(SUM(CASE WHEN message_type = 'meeting_transcript' THEN 1 ELSE 0 END), 0)::BIGINT AS meeting_count
	FROM selected
), meeting_ids AS (
	SELECT anchor_message_id AS message_id
	FROM selected
	WHERE message_type = 'meeting_transcript'
	ORDER BY anchor_message_id
	LIMIT ?
)
SELECT selection_counts.selected_count, selection_counts.meeting_count, meeting_ids.message_id
FROM selection_counts
LEFT JOIN meeting_ids ON true
ORDER BY meeting_ids.message_id NULLS LAST`, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve analytical meeting selection: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := &ExploreMeetingSelection{
		MessageIDs: []int64{}, CacheRevision: state.Revision(), SearchProvenance: provenance,
	}
	for rows.Next() {
		var id sql.NullInt64
		if err := rows.Scan(&result.SelectedCount, &result.MeetingCount, &id); err != nil {
			return nil, fmt.Errorf("scan analytical meeting selection: %w", err)
		}
		if id.Valid {
			result.MessageIDs = append(result.MessageIDs, id.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical meeting selection: %w", err)
	}
	if len(result.MessageIDs) > limit {
		result.MessageIDs = result.MessageIDs[:limit]
		result.LimitExceeded = true
	}
	return result, nil
}

func exploreSelectionWhere(request ExploreSelectionRequest) (string, []any) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, len(request.IncludedKeys)+len(request.ExcludedKeys))
	if request.IncludedKeys != nil {
		if len(request.IncludedKeys) == 0 {
			conditions = append(conditions, "false")
		} else {
			placeholders := make([]string, len(request.IncludedKeys))
			for i, key := range request.IncludedKeys {
				placeholders[i] = "?"
				args = append(args, key)
			}
			conditions = append(conditions, "entry_key IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(request.ExcludedKeys) > 0 {
		placeholders := make([]string, len(request.ExcludedKeys))
		for i, key := range request.ExcludedKeys {
			placeholders[i] = "?"
			args = append(args, key)
		}
		conditions = append(conditions, "entry_key NOT IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

var _ ExploreMeetingResolver = (*DuckDBEngine)(nil)
