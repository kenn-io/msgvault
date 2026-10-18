package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestConversationPolicyIncludeColdNeverReranksCold(t *testing.T) {
	calls := 0
	h := newTestHandlers(&querytest.MockEngine{})
	h.hybridSearcher = hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
		calls++
		return &HybridSearchResult{}, nil
	})
	r := runToolExpectError(t, "semantic_search_messages", h.semanticSearchMessages, map[string]any{"query": "needle include:cold"})
	assert.Contains(t, resultText(t, r), "lexical")
	assert.Zero(t, calls)
}

func TestConversationPolicyIncludeColdRemoteRoundTrip(t *testing.T) {
	calls := 0
	engine := &querytest.MockEngine{SearchFastWithStatsFunc: func(_ context.Context, q *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
		calls++
		assert.True(t, q.IncludeCold)
		assert.Len(t, q.Alternatives, 2)
		assert.True(t, search.ParseOR(search.Format(q)).IncludeCold)
		return &query.SearchFastResult{Messages: []query.MessageSummary{}, TotalCount: 0}, nil
	}}
	h := newTestHandlers(engine)
	runTool[searchMetadataResponse](t, "search_metadata", h.searchMetadata, map[string]any{"query": "subject:needle | subject:other include:cold"})
	assert.Equal(t, 1, calls)
}
