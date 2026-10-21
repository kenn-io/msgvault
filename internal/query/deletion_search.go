package query

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/search"
)

type boundedDeletionSearchPage func(
	context.Context, *search.Query, MessageFilter, int, int,
) ([]MessageSummary, error)

type deletionTargetsByIDs func(context.Context, []int64) ([]DeletionTarget, error)

// resolveBoundedDeletionTargets searches once for at most maxResults IDs, then
// resolves those exact IDs through the authoritative deletion-target lookup.
// Matches that cannot be deleted are omitted from targets and included in matched.
func resolveBoundedDeletionTargets(
	ctx context.Context,
	searchQuery *search.Query,
	filter MessageFilter,
	mode DeletionSearchMode,
	maxResults int,
	fastSearch boundedDeletionSearchPage,
	deepSearch boundedDeletionSearchPage,
	resolveIDs deletionTargetsByIDs,
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

	filter = filter.Clone()
	filter.Pagination = Pagination{}
	filter.HideDeletedFromSource = true

	searchScope := *searchQuery
	searchScope.HideDeleted = true
	searchScope.DeletionScope = search.DeletionScopeActive
	searchQuery = &searchScope

	var messages []MessageSummary
	var err error
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
	targets, err := resolveIDs(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve bounded deletion search targets: %w", err)
	}
	return targets, len(messages), nil
}
