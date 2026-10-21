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
// A mismatch means the search cache or live archive changed during resolution;
// callers must not stage a partial target set.
func resolveBoundedDeletionTargets(
	ctx context.Context,
	searchQuery *search.Query,
	filter MessageFilter,
	mode DeletionSearchMode,
	maxResults int,
	fastSearch boundedDeletionSearchPage,
	deepSearch boundedDeletionSearchPage,
	resolveIDs deletionTargetsByIDs,
) ([]DeletionTarget, error) {
	if searchQuery == nil {
		return nil, errors.New("deletion search query is required")
	}
	if err := searchQuery.Err(); err != nil {
		return nil, fmt.Errorf("invalid deletion search query: %w", err)
	}
	if maxResults < 1 {
		return nil, errors.New("bounded deletion search requires a positive result limit")
	}
	if maxResults > MaxDeletionTargetSearchResults {
		return nil, fmt.Errorf("bounded deletion search limit cannot exceed %d", MaxDeletionTargetSearchResults)
	}

	filter = filter.Clone()
	filter.Pagination = Pagination{}

	var messages []MessageSummary
	var err error
	switch mode {
	case DeletionSearchFast:
		messages, err = fastSearch(ctx, searchQuery, filter, maxResults, 0)
	case DeletionSearchDeep:
		messages, err = deepSearch(ctx, searchQuery, filter, maxResults, 0)
	case DeletionSearchFastThenDeep:
		messages, err = fastSearch(ctx, searchQuery, filter, maxResults, 0)
		if err == nil && len(messages) == 0 && searchQuery.HasTextTerms() {
			messages, err = deepSearch(ctx, searchQuery, filter, maxResults, 0)
		}
	default:
		return nil, fmt.Errorf("unsupported bounded deletion search mode %q", mode)
	}
	if err != nil {
		return nil, err
	}
	if len(messages) > maxResults {
		return nil, fmt.Errorf("bounded deletion search returned %d rows above its %d-row limit", len(messages), maxResults)
	}
	if len(messages) == 0 {
		return []DeletionTarget{}, nil
	}

	ids := make([]int64, len(messages))
	for i, message := range messages {
		if message.ID <= 0 {
			return nil, fmt.Errorf("bounded deletion search returned invalid message ID %d", message.ID)
		}
		ids[i] = message.ID
	}
	targets, err := resolveIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve bounded deletion search targets: %w", err)
	}
	if len(targets) != len(ids) {
		return nil, fmt.Errorf(
			"resolve bounded deletion search targets: search returned %d messages but only %d eligible targets remain; retry staging",
			len(ids), len(targets),
		)
	}
	return targets, nil
}
