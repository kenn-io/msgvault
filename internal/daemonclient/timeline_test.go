package daemonclient

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/timeline"
)

func TestTimelineRoundTrips(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/timeline", r.URL.Path)
				assert.Equal(t, format, r.URL.Query().Get("format"))
				assert.Equal(t, "1,3", r.URL.Query().Get("tiers"))
				assert.Equal(t, "true", r.URL.Query().Get("include_cold"))
				if format == "markdown" {
					w.Header().Set("Content-Type", "text/markdown")
					_, err := w.Write([]byte("# Timeline\n"))
					assert.NoError(t, err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"mode":"day","timezone":"UTC","days":[{"date":"2027-03-01","bursts":[{"open":[{"tool":"get_message","args":{"id":9007199254740993}}]}]}]}`))
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{URL: srv.URL, AllowInsecure: true})
			requirements.NoError(err)
			got, text, err := c.Timeline(context.Background(), timeline.Request{Format: format, Tiers: []int{1, 3}, IncludeCold: true})
			requirements.NoError(err)
			if format == "markdown" {
				assertions.Equal("# Timeline\n", text)
				assertions.Nil(got)
			} else {
				requirements.NotNil(got)
				assertions.Equal(timeline.ModeDay, got.Mode)
				data, err := json.Marshal(got, json.Deterministic(true))
				requirements.NoError(err)
				assertions.Contains(string(data), `"id":9007199254740993`)
			}
		})
	}
}
func TestTimelineAmbiguityKeepsCandidates(t *testing.T) {
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, err := w.Write([]byte(`{"error":"ambiguous_reference","message":"Choose a person","candidates":[{"person_id":7,"person_uid":"synthetic-seven","display_name":"Alice"}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, AllowInsecure: true})
	requirements.NoError(err)
	_, _, err = c.Timeline(t.Context(), timeline.Request{With: "Alice"})
	var ambiguous *timeline.ErrAmbiguousReference
	requirements.ErrorAs(err, &ambiguous)
	requirements.Len(ambiguous.Candidates, 1)
	assert.Equal(t, int64(7), ambiguous.Candidates[0].PersonID)
}

func TestTimelineHTTPPreservesNeighborPresence(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		req                   timeline.Request
		wantBefore, wantAfter string
	}{
		{"omitted", timeline.Request{}, "", ""},
		{"zero", timeline.Request{BeforeSet: true, AfterSet: true}, "0", "0"},
		{"positive", timeline.Request{Before: 2, After: 3}, "2", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.wantBefore, r.URL.Query().Get("before"))
				assert.Equal(t, tc.wantAfter, r.URL.Query().Get("after"))
				w.Header().Set("Content-Type", "text/markdown")
				_, err := w.Write([]byte("# Timeline\n"))
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			client, err := New(Config{URL: srv.URL, AllowInsecure: true})
			require.NoError(t, err)
			_, _, err = client.Timeline(t.Context(), tc.req)
			require.NoError(t, err)
		})
	}
}
