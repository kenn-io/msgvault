package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestDuckDBHiddenTextRows(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("reader@example.com", "teams")
	hiddenID := b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: 10, MessageType: "teams", Snippet: "hiddenneedle", HiddenTextPresent: true})
	visibleID := b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: 10, MessageType: "teams", Snippet: "hiddenneedle"})
	engine := b.BuildEngine()
	q := search.Parse("hiddenneedle")
	for _, tc := range []struct {
		name string
		read func(context.Context) ([]MessageSummary, error)
	}{
		{"list", func(ctx context.Context) ([]MessageSummary, error) { return engine.ListMessages(ctx, MessageFilter{}) }},
		{"fast", func(ctx context.Context) ([]MessageSummary, error) {
			return engine.SearchFast(ctx, q, MessageFilter{}, 10, 0)
		}},
		{"stats", func(ctx context.Context) ([]MessageSummary, error) {
			result, err := engine.SearchFastWithStats(ctx, q, "hiddenneedle", MessageFilter{}, ViewSenders, 10, 0)
			if err != nil {
				return nil, err
			}
			return result.Messages, nil
		}},
		{"cached page", func(ctx context.Context) ([]MessageSummary, error) {
			result, err := engine.SearchFastWithStats(ctx, q, "hiddenneedle", MessageFilter{}, ViewNoStats, 10, 0)
			if err != nil {
				return nil, err
			}
			return result.Messages, nil
		}},
		{"timeline", func(ctx context.Context) ([]MessageSummary, error) {
			return engine.ListConversationMessages(ctx, 10, TextFilter{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, err := tc.read(t.Context())
			require.NoError(err)
			require.Len(rows, 2)
			got := make(map[int64]bool)
			for _, row := range rows {
				got[row.ID] = row.HiddenTextPresent
			}
			assert.Equal(map[int64]bool{hiddenID: true, visibleID: false}, got)
		})
	}
}
