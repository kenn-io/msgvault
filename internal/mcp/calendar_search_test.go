package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCalendarSearchMetadataHintFollowsAvailableOccurrenceTool(t *testing.T) {
	st := testutil.NewTestStore(t)
	engine := &querytest.MockEngine{
		SearchFastWithStatsFunc: func(context.Context, *search.Query, string, query.MessageFilter, query.ViewType, int, int) (*query.SearchFastResult, error) {
			return &query.SearchFastResult{Messages: []query.MessageSummary{}, TotalCount: 0}, nil
		},
	}

	const warning = "Archive results cannot prove free time."
	const occurrenceHint = "Recurring meetings appear once as a series; use list_calendar_events for occurrences in a window. " + warning
	for _, tc := range []struct {
		name  string
		opts  ServeOptions
		query string
		want  string
	}{
		{name: "owner with occurrence tool", opts: ServeOptions{Engine: engine, CalEvents: st}, query: "message_type:calendar_event", want: occurrenceHint},
		{name: "delegated reader with occurrence tool", opts: ServeOptions{Engine: engine, CalEvents: st, DelegatedOnly: true, GrantPermissions: []string{"search.read", "calendar.event.read"}}, query: "message_type:calendar_event", want: occurrenceHint},
		{name: "delegated search without occurrence tool", opts: ServeOptions{Engine: engine, CalEvents: st, DelegatedOnly: true, GrantPermissions: []string{"search.read"}}, query: "message_type:calendar_event", want: warning},
		{name: "owner without occurrence tool", opts: ServeOptions{Engine: engine}, query: "message_type:calendar_event", want: warning},
		{name: "non-calendar search", opts: ServeOptions{Engine: engine, CalEvents: st}, query: "planning", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := rawCallTool(t, tc.opts, ToolSearchMetadata, map[string]any{"query": tc.query})
			response := fmt.Sprint(result["content"])
			require.NotEqual(t, true, result["isError"], response)
			if tc.want == "" {
				assert.NotContains(t, response, warning)
				return
			}
			assert.Contains(t, response, tc.want)
		})
	}
}

type calendarSearchHintResponse struct {
	Hint string `json:"hint"`
}

func TestCalendarSearchHintIsIncludedInBodySearchResponses(t *testing.T) {
	const want = "Recurring meetings appear once as a series; use list_calendar_events for occurrences in a window. Archive results cannot prove free time."
	engine := &querytest.MockEngine{
		SearchMessageBodiesFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return []query.MessageSummary{}, nil
		},
	}
	h := &handlers{engine: engine, calendarEventsAdvertised: true}
	body := runTool[calendarSearchHintResponse](t, ToolSearchMessageBodies, h.searchMessageBodies, map[string]any{
		"query": "message_type:calendar_event planning",
	})
	assert.Equal(t, want, body.Hint)

	h = &handlers{
		engine:                   &querytest.MockEngine{},
		calendarEventsAdvertised: true,
		hybridSearcher: hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
			return &HybridSearchResult{Hits: []HybridSearchHit{}}, nil
		}),
	}
	semantic := runTool[calendarSearchHintResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{
		"query": "message_type:calendar_event planning",
		"mode":  searchModeHybrid,
	})
	assert.Equal(t, want, semantic.Hint)
}
