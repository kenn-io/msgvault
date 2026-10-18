package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

type domainPage struct {
	Data []struct {
		ID int64 `json:"id"`
	} `json:"data"`
	Returned int  `json:"returned"`
	Offset   int  `json:"offset"`
	HasMore  bool `json:"has_more"`
}

func TestSearchByDomainsPagination(t *testing.T) {
	for _, tc := range []struct{ requested, effective int }{
		{0, 100}, {1, 1}, {100, 100}, {499, 499}, {500, 500}, {501, 500}, {1000, 500},
	} {
		for _, available := range []int{0, tc.effective - 1, tc.effective, tc.effective + 1} {
			t.Run(fmt.Sprintf("limit=%d/available=%d", tc.requested, available), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				for _, offset := range []int{1500, 10_000_000} {
					after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
					before := after.AddDate(1, 0, 0)
					var calls []query.Pagination
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					eng := &querytest.MockEngine{SearchByDomainsFunc: func(gotCtx context.Context, domains []string, gotAfter, gotBefore *time.Time, limit, gotOffset int, sourceIDs []int64) ([]query.MessageSummary, error) {
						assert.Equal(ctx, gotCtx)
						assert.Equal([]string{"example.com", "example.org"}, domains)
						assert.Equal(&after, gotAfter)
						assert.Equal(&before, gotBefore)
						assert.Nil(sourceIDs)
						assert.Positive(limit)
						assert.LessOrEqual(limit, 500)
						calls = append(calls, query.Pagination{Limit: limit, Offset: gotOffset})
						require.LessOrEqual(len(calls), 2)
						remaining := max(0, available-(gotOffset-offset))
						rows := make([]query.MessageSummary, min(limit, remaining, 500))
						for i := range rows {
							rows[i].ID = int64(gotOffset + i + 1)
						}
						return rows, nil
					}}
					h := newTestHandlers(eng)
					result, err := h.searchByDomains(ctx, toolRequest{arguments: map[string]any{
						"domains": " example.com, example.org ", "limit": float64(tc.requested), "offset": float64(offset),
						"after": "2024-01-01", "before": "2025-01-01",
					}})
					require.NoError(err)
					var page domainPage
					require.NoError(json.Unmarshal([]byte(resultText(t, result)), &page))
					wantCount := min(available, tc.effective)
					assert.Len(page.Data, wantCount)
					assert.NotNil(page.Data)
					assert.Equal(wantCount, page.Returned)
					assert.Equal(offset, page.Offset)
					assert.Equal(available > tc.effective, page.HasMore)
					for i, row := range page.Data {
						assert.Equal(int64(offset+i+1), row.ID)
					}
					wantCalls := []query.Pagination{{Limit: min(tc.effective+1, 500), Offset: offset}}
					if tc.effective == 500 && available >= 500 {
						wantCalls = append(wantCalls, query.Pagination{Limit: 1, Offset: offset + 500})
					}
					assert.Equal(wantCalls, calls)
				}
			})
		}
	}
}

func TestSearchByDomainsProbeError(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("call=%d", failAt), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			eng := &querytest.MockEngine{SearchByDomainsFunc: func(context.Context, []string, *time.Time, *time.Time, int, int, []int64) ([]query.MessageSummary, error) {
				calls++
				if calls == failAt {
					return nil, errors.New("domain probe failed")
				}
				return make([]query.MessageSummary, 500), nil
			}}
			h := newTestHandlers(eng)
			result, err := h.searchByDomains(t.Context(), toolRequest{arguments: map[string]any{"domains": "example.com", "limit": float64(500)}})
			require.Error(err)
			assert.Nil(result, "a failed probe must not return partial success")
			assert.Contains(err.Error(), "search messages by domain")
			assert.Equal(failAt, calls)
		})
	}
}

func TestSearchByDomainsPaginationWire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	tool := toolsByName(t, rawListTools(t, opts, false))[ToolSearchByDomains]
	schema, ok := tool["outputSchema"].(map[string]any)
	require.True(ok)
	properties, ok := schema["properties"].(map[string]any)
	require.True(ok)
	for _, name := range []string{"data", "returned", "offset", "has_more"} {
		assert.Contains(properties, name)
		assert.Contains(schema["required"], name)
	}
	assert.NotContains(properties, "total")
}

// Keep the production MCP -> daemon adapter -> HTTP handler path intact. The
// static engine data isolates paging from database setup and live-index changes.
func TestSearchByDomainsPaginationThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rows := make([]query.MessageSummary, 2010)
	for i := range rows {
		rows[i].ID = int64(i + 1)
	}
	eng := &querytest.MockEngine{SearchByDomainsFunc: func(_ context.Context, _ []string, _, _ *time.Time, limit, offset int, _ []int64) ([]query.MessageSummary, error) {
		return rows[min(offset, len(rows)):min(offset+limit, len(rows))], nil
	}}
	st := testutil.NewTestStore(t)
	daemon := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: eng, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	h := newTestHandlers(daemonclient.NewEngineAdapter(client))
	for _, offset := range []int{1500, 2000} {
		page := runTool[domainPage](t, ToolSearchByDomains, h.searchByDomains, map[string]any{
			"domains": "example.com", "limit": float64(500), "offset": float64(offset),
		})
		want := min(500, len(rows)-offset)
		require.Len(page.Data, want)
		assert.Equal(want, page.Returned)
		assert.Equal(offset, page.Offset)
		assert.Equal(offset == 1500, page.HasMore)
		for i, row := range page.Data {
			assert.Equal(int64(offset+i+1), row.ID)
		}
	}
}
