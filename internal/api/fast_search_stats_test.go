package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestHandleFastSearchStatsAreOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		parameter string
		wantView  query.ViewType
		wantStats bool
	}{
		{name: "omitted", wantView: query.ViewNoStats},
		{name: "senders", parameter: "&view_type=senders", wantView: query.ViewSenders, wantStats: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			var gotView query.ViewType
			engine := &querytest.MockEngine{
				SearchFastWithStatsFunc: func(_ context.Context, _ *search.Query, _ string,
					_ query.MessageFilter, view query.ViewType, _, _ int) (*query.SearchFastResult, error) {
					gotView = view
					result := &query.SearchFastResult{TotalCount: 7}
					if view != query.ViewNoStats {
						result.Stats = &query.TotalStats{}
					}
					return result, nil
				},
			}
			server := newTestServerWithEngine(t, engine)
			response := doGet(server, "/api/v1/search/fast?q=needle"+tc.parameter)
			require.Equal(http.StatusOK, response.Code, "body: %s", response.Body.String())
			assert.Equal(tc.wantView, gotView)
			var body map[string]json.RawMessage
			require.NoError(json.NewDecoder(response.Body).Decode(&body))
			stats, present := body["stats"]
			assert.Equal(tc.wantStats, present)
			if tc.wantStats {
				assert.NotEqual("null", string(stats))
			}
		})
	}
}
