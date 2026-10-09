package daemonclient

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestMetadataSearchEmptyScopeOmitsUnrequestedStats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		filter query.MessageFilter
	}{
		{"empty sources", "needle", query.MessageFilter{SourceIDs: []int64{}}},
		{"conflicting types", "message_type:sms needle", query.MessageFilter{MessageType: "email"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			called := false
			store := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				writeJSONResponse(t, w, map[string]any{"total_count": 99})
			})
			engine := NewEngineAdapter(store)
			result, err := engine.SearchFastWithStats(t.Context(), search.Parse(tc.query), tc.query, tc.filter, query.ViewNoStats, 2, 0)
			require.NoError(err)
			assert.Zero(result.TotalCount)
			assert.Empty(result.Messages)
			assert.Nil(result.Stats)
			assert.False(called)
		})
	}
}
