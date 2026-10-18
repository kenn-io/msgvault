package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// Exercise offset handling through a production consumer with schema-valid
// integer arguments.
func TestPaginationOffsetArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want int
	}{
		{"missing", map[string]any{}, 0},
		{"zero", map[string]any{"offset": float64(0)}, 0},
		{"past 1000", map[string]any{"offset": float64(1500)}, 1500},
		{"large archive", map[string]any{"offset": float64(2_000_000)}, 2_000_000},
		{"ceiling", map[string]any{"offset": float64(maxOffset)}, maxOffset},
		{"safe integer above ceiling", map[string]any{"offset": maxJSONSafeInteger}, maxOffset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &querytest.MockEngine{ListMessagesFunc: func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
				assert.Equal(t, tc.want, filter.Pagination.Offset)
				return nil, nil
			}}
			h := newTestHandlers(eng)
			resp := runTool[listMessagesResponse](t, ToolListMessages, h.listMessages, tc.args)
			assert.Equal(t, tc.want, resp.Offset)
		})
	}
}

// The MCP schema accepts nonnegative safe integers; offsets above the handler's
// ceiling are clamped before they reach the query engine.
func FuzzMCPOffset(f *testing.F) {
	for _, offset := range []uint64{0, 1, 1500, 2_000_000, maxOffset, uint64(maxJSONSafeInteger)} {
		f.Add(offset)
	}
	f.Fuzz(func(t *testing.T, raw uint64) {
		v := float64(raw % (uint64(maxJSONSafeInteger) + 1))
		var got int
		eng := &querytest.MockEngine{ListMessagesFunc: func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
			got = filter.Pagination.Offset
			return nil, nil
		}}
		h := newTestHandlers(eng)
		_, err := h.listMessages(context.Background(), toolRequest{arguments: map[string]any{"offset": v}})
		require.NoError(t, err)
		assert.Equal(t, int(min(v, float64(maxOffset))), got)
	})
}

func TestSearchInMessageOffsetPast1000(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	eng := &querytest.MockEngine{Messages: map[int64]*query.MessageDetail{
		1: {ID: 1, BodyText: strings.Repeat("hit ", 1502)},
	}}
	h := newTestHandlers(eng)
	resp := runTool[searchInMessageResponse](t, ToolSearchInMessage, h.searchInMessage, map[string]any{
		"id": float64(1), "query": "hit", "offset": float64(1500), "limit": float64(1),
	})
	require.Len(resp.Data, 1)
	require.NotNil(resp.Data[0].CharOffset)
	assert.Equal(6000, *resp.Data[0].CharOffset)
	assert.Equal(1500, resp.Offset)
	assert.True(resp.HasMore)
}

func TestHybridOffsetPast1000(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window *int
	}{
		{"unset", nil}, {"unlimited", new(0)}, {"past window", new(1200)},
		{"at window", new(1500)}, {"crossing window", new(1501)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			window := tc.window
			backend := paginationBackend{&fakeBackend{active: vector.Generation{
				ID: 1, Model: "fake", Dimension: 4, Fingerprint: "fake:4", State: vector.GenerationActive,
			}}}
			messages := make(map[int64]*query.MessageDetail, 1502)
			for i := range 1502 {
				id := int64(i + 1)
				backend.searchHits = append(backend.searchHits, vector.Hit{MessageID: id, Score: 1 - float64(i)/2000})
				messages[id] = &query.MessageDetail{ID: id, BodyText: "hit"}
			}
			h := &handlers{
				engine: &querytest.MockEngine{Messages: messages}, backend: backend,
				hybridEngine: hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{ExpectedFingerprint: "fake:4"}),
				vectorCfg:    vector.Config{Search: vector.SearchConfig{MaxPageSizeHybrid: window}},
			}
			args := map[string]any{"query": "hit", "mode": searchModeVector, "offset": float64(1500)}
			if window != nil && *window > 0 && *window <= 1500 {
				result := runToolExpectError(t, ToolSemanticSearchMessages, h.semanticSearchMessages, args)
				assert.Contains(resultText(t, result), "pagination_limit")
				assert.Contains(resultText(t, result), "1500")
			} else {
				resp := runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, args)
				want := 2
				if window != nil && *window > 0 {
					want = *window - 1500
				}
				require.Len(t, resp.Data, want)
				for i, row := range resp.Data {
					assert.Equal(int64(1501+i), row.ID)
				}
				assert.Equal(1500, resp.Offset)
				assert.False(resp.HasMore)
			}
		})
	}
}

// Honor the backend's fetch limit so ranking-window tests see only the rows
// that a production vector backend would return.
type paginationBackend struct{ *fakeBackend }

func (b paginationBackend) Search(ctx context.Context, gen vector.GenerationID, queryVector []float32, limit int, filter vector.Filter) ([]vector.Hit, error) {
	hits, err := b.fakeBackend.Search(ctx, gen, queryVector, limit, filter)
	return hits[:min(limit, len(hits))], err
}
