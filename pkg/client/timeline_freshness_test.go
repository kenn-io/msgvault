package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestTimelineNullableFreshnessTimestamps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/timeline", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"freshness":{"projections":[{"name":"activity","state":"current","frontier":null}],"sources":[{"source_type":"gmail","identifier":"owner@example.com","scheduling":"manual","outcome":"never","sync_command":"msgvault sync","last_successful_sync_at":null}]}}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	client, err := New(srv.URL)
	require.NoError(t, err)
	response, err := client.GetTimelineWithResponse(t.Context(), &generated.GetTimelineRequestOptions{})
	require.NoError(t, err)
	require.NotNil(t, response.JSON200)
	t.Run("projection frontier", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		requirements.Len(response.JSON200.Freshness.Projections, 1)
		projection := response.JSON200.Freshness.Projections[0]
		assertions.Nil(projection.Frontier)
		assertions.Equal("activity", projection.Name)
		requirements.NoError(projection.Validate())
	})
	t.Run("source last success", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		requirements.Len(response.JSON200.Freshness.Sources, 1)
		source := response.JSON200.Freshness.Sources[0]
		assertions.Nil(source.LastSuccessfulSyncAt)
		assertions.Equal("never", source.Outcome)
		requirements.NoError(source.Validate())
	})
}
