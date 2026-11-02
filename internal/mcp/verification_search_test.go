package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestVerificationSearchDropsPreservePagePosition(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	rows := []query.MessageSummary{{ID: 1, Subject: "482913"}, {ID: 2, Subject: "482913"}, {ID: 3, Subject: "482913"}}
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			1: {ID: 1, BodyText: "Your verification code is 482913"},
			2: {ID: 2, BodyText: "Invoice number 482913"},
			3: {ID: 3, BodyText: "Your verification code is 482913"},
		},
		SearchFastWithStatsFunc: func(_ context.Context, _ *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, limit, offset int) (*query.SearchFastResult, error) {
			return &query.SearchFastResult{Messages: rows[offset:min(len(rows), offset+limit)], TotalCount: 3}, nil
		},
	}
	h := newTestHandlers(engine)
	h.unmaskVerification = false
	first := runTool[searchMetadataResponse](t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "482913", "limit": float64(2)})
	requirements.Len(first.Data, 1)
	assertions.Equal(int64(2), first.Data[0].ID)
	assertions.Equal(1, first.MaskedHitsDropped)
	assertions.Equal(2, first.NextOffset)
	assertions.True(first.HasMore)
	last := runTool[searchMetadataResponse](t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "482913", "limit": float64(2), "offset": float64(first.NextOffset)})
	assertions.Empty(last.Data)
	assertions.Equal(1, last.MaskedHitsDropped)
	assertions.Equal(3, last.NextOffset)
	assertions.False(last.HasMore)
	h.unmaskVerification = true
	full := runTool[searchMetadataResponse](t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "482913", "limit": float64(2)})
	assertions.Len(full.Data, 2)
	assertions.Zero(full.MaskedHitsDropped)
	items, dropped, err := h.guardVerificationSearchItems(t.Context(), []searchMessageItem{{MessageSummary: rows[0]}, {MessageSummary: rows[1]}}, search.Parse("482913"))
	requirements.NoError(err)
	assertions.Len(items, 2)
	assertions.Zero(dropped)
	h.unmaskVerification = false
	items, dropped, err = h.guardVerificationSearchItems(t.Context(), items, search.Parse("482913"))
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(int64(2), items[0].ID)
	assertions.Equal(1, dropped)
}
