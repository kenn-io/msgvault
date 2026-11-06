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

func TestTimelineJSONResponse(t *testing.T) {
	for _, format := range []*string{nil, new("json")} {
		name := "default"
		if format != nil {
			name = *format
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(format != nil, r.URL.Query().Has("format"))
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"mode":"day","timezone":"UTC","days":[{"date":"2027-03-01"}]}`))
				assertions.NoError(err)
			}))
			t.Cleanup(srv.Close)
			c, err := client.New(srv.URL)
			requirements.NoError(err)
			options := &generated.GetTimelineRequestOptions{}
			if format != nil {
				options = &generated.GetTimelineRequestOptions{Query: &generated.GetTimelineQuery{Format: format}}
			}
			resp, err := c.GetTimelineWithResponse(t.Context(), options)
			requirements.NoError(err)
			requirements.NotNil(resp.JSON200)
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
