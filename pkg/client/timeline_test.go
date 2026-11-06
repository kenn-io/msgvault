package client_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestTimelineMarkdownResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/api/v1/timeline", r.URL.Path)
		assertions.Equal("markdown", r.URL.Query().Get("format"))
		w.Header().Set("Content-Type", "text/markdown")
		_, err := w.Write([]byte("# Timeline\n"))
		assertions.NoError(err)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL)
	requirements.NoError(err)
	resp, err := c.GetTimelineWithResponse(t.Context(), &generated.GetTimelineRequestOptions{
		Query: &generated.GetTimelineQuery{Format: new("markdown")},
	})
	requirements.NoError(err)
	requirements.NotNil(resp)
	assertions.Equal(http.StatusOK, resp.StatusCode)
	assertions.Equal("# Timeline\n", string(resp.Body))
	assertions.Nil(resp.JSON200)
}

func timelineJSONResponse(t *testing.T, body string, format *string) *generated.GetTimelineResp {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/timeline", r.URL.Path)
		assert.Equal(t, format != nil, r.URL.Query().Has("format"))
		if format != nil {
			assert.Equal(t, *format, r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL)
	require.NoError(t, err)
	options := &generated.GetTimelineRequestOptions{}
	if format != nil {
		options.Query = &generated.GetTimelineQuery{Format: format}
	}
	response, err := c.GetTimelineWithResponse(t.Context(), options)
	require.NoError(t, err)
	require.NotNil(t, response.JSON200)
	return response
}

func TestTimelineJSONResponse(t *testing.T) {
	for _, format := range []*string{nil, new("json")} {
		name := "default"
		if format != nil {
			name = *format
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			resp := timelineJSONResponse(t, `{"mode":"day","timezone":"UTC","days":[{"date":"2027-03-01"}]}`, format)
			assertions.Equal("day", resp.JSON200.Mode)
			assertions.Equal("UTC", resp.JSON200.Timezone)
			requirements.Len(resp.JSON200.Days, 1)
			assertions.Equal("2027-03-01", resp.JSON200.Days[0].Date)
		})
	}
}

func TestTimelineMarkdownError(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const body = `{"error":"ambiguous_reference","message":"Choose a person","candidates":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, err := w.Write([]byte(body))
		assertions.NoError(err)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL)
	requirements.NoError(err)
	resp, err := c.GetTimelineWithResponse(t.Context(), &generated.GetTimelineRequestOptions{
		Query: &generated.GetTimelineQuery{Format: new("markdown")},
	})
	var apiErr *runtime.ClientAPIError
	requirements.ErrorAs(err, &apiErr)
	requirements.NotNil(resp)
	assertions.Equal(http.StatusConflict, resp.StatusCode)
	assertions.JSONEq(body, string(resp.Body))
}

func TestGeneratedTimelineDecodesPrimitiveMaps(t *testing.T) {
	require := require.New(t)
	body := `{"mode":"day","omitted":{"budget":{"history":2,"open_loops":"more"},"noise":0,"cold":0},"days":[{"bursts":[{"open":[{"tool":"get_message","args":{"id":42}},{"tool":"get_calendar_event","args":{"ref":"e77"}}]}]}]}`
	response := timelineJSONResponse(t, body, nil)
	require.Len(response.JSON200.Days, 1)
	require.Len(response.JSON200.Days[0].Bursts, 1)
	openers := response.JSON200.Days[0].Bursts[0].Open
	require.Len(openers, 2)
	assert.Equal(t, map[string]any{"id": float64(42)}, openers[0].Args)
	assert.Equal(t, map[string]any{"ref": "e77"}, openers[1].Args)
	assert.Equal(t, map[string]any{"history": float64(2), "open_loops": "more"}, response.JSON200.Omitted.Budget)
}

func TestTimelineNullableFreshnessTimestamps(t *testing.T) {
	response := timelineJSONResponse(t, `{"freshness":{"projections":[{"name":"activity","state":"current","frontier":null}],"sources":[{"source_type":"gmail","identifier":"owner@example.com","scheduling":"manual","outcome":"never","sync_command":"msgvault sync","last_successful_sync_at":null}]}}`, nil)
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
