package query

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/search"
)

type boundedDeletionSearchPage func(
	context.Context, *search.Query, MessageFilter, int, int,
) ([]MessageSummary, error)

// resolveBoundedDeletionTargets searches sources staging can delete once for at
// most maxResults IDs, then resolves those exact IDs through authoritative state.
// Matches that cannot be deleted are omitted from targets and included in matched.
func resolveBoundedDeletionTargets(
	ctx context.Context,
	searchQuery *search.Query,
	filter MessageFilter,
	mode DeletionSearchMode,
	maxResults int,
	fastSearch boundedDeletionSearchPage,
	deepSearch boundedDeletionSearchPage,
	authority *SQLiteEngine,
) ([]DeletionTarget, int, error) {
	if searchQuery == nil {
		return nil, 0, errors.New("deletion search query is required")
	}
	if err := searchQuery.Err(); err != nil {
		return nil, 0, fmt.Errorf("invalid deletion search query: %w", err)
	}
	if maxResults < 1 {
		return nil, 0, errors.New("bounded deletion search requires a positive result limit")
	}
	if maxResults > MaxDeletionTargetSearchResults {
		return nil, 0, fmt.Errorf("bounded deletion search limit cannot exceed %d", MaxDeletionTargetSearchResults)
	}

	searchQuery, filter, err := authority.executableDeletionScope(ctx, searchQuery, filter)
	if err != nil {
		return nil, 0, err
	}

	var messages []MessageSummary
	switch mode {
	case DeletionSearchFast:
		messages, err = fastSearch(ctx, searchQuery, filter, maxResults, 0)
	case DeletionSearchDeep:
		messages, err = deepSearch(ctx, searchQuery, filter, maxResults, 0)
	default:
		return nil, 0, fmt.Errorf("unsupported bounded deletion search mode %q", mode)
	}
	if err != nil {
		return nil, 0, err
	}
	if len(messages) == 0 {
		return []DeletionTarget{}, 0, nil
	}

	ids := make([]int64, len(messages))
	for i, message := range messages {
		if message.ID <= 0 {
			return nil, 0, fmt.Errorf("bounded deletion search returned invalid message ID %d", message.ID)
		}
		ids[i] = message.ID
	}
	targets, err := authority.executableDeletionTargetsByMessageIDs(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve bounded deletion search targets: %w", err)
	}
	return targets, len(messages), nil
}

// executableDeletionScope limits a bounded deletion search to active messages
// from sources staging can delete, so other matches cannot use up the limit.
func (e *SQLiteEngine) executableDeletionScope(
	ctx context.Context, q *search.Query, filter MessageFilter,
) (*search.Query, MessageFilter, error) {
	sourceIDs, err := e.executableDeletionSourceIDs(ctx)
	if err != nil {
		return nil, MessageFilter{}, err
	}
	filter = filter.Clone()
	filter.Pagination = Pagination{}
	filter.HideDeletedFromSource = true
	filter.SourceIDs = selectedSourceIDs(sourceIDs, filter)

	scoped := *q
	scoped.HideDeleted = true
	scoped.DeletionScope = search.DeletionScopeActive
	return &scoped, filter, nil
}

func (e *SQLiteEngine) executableDeletionSourceIDs(ctx context.Context) ([]int64, error) {
	rows, err := e.queryContext(ctx, `SELECT id FROM sources WHERE source_type IN `+executableDeletionSourceTypesSQL)
	if err != nil {
		return nil, fmt.Errorf("list executable deletion sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan executable deletion source: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// selectedSourceIDs keeps the allowed sources selected by the filter.
// The result is never nil, so an empty
// intersection matches nothing instead of every source.
func selectedSourceIDs(allowed []int64, filter MessageFilter) []int64 {
	requested := filter.SourceIDs
	if requested == nil && filter.SourceID != nil {
		requested = []int64{*filter.SourceID}
	}
	selected := []int64{}
	for _, id := range allowed {
		if requested == nil || slices.Contains(requested, id) {
			selected = append(selected, id)
		}
	}
	return selected
}
