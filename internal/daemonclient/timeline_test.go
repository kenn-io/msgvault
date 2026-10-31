package daemonclient

import (
	"context"
	"encoding/json/v2"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/timeline"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTimelineRoundTrips(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertpkg.Equal(t, "/api/v1/timeline", r.URL.Path)
				assertpkg.Equal(t, format, r.URL.Query().Get("format"))
				assertpkg.Equal(t, "1,3", r.URL.Query().Get("tiers"))
				assertpkg.Equal(t, "true", r.URL.Query().Get("include_cold"))
				if format == "markdown" {
					w.Header().Set("Content-Type", "text/markdown")
					_, err := w.Write([]byte("# Timeline\n"))
					assertpkg.NoError(t, err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"mode":"day","timezone":"UTC","days":[{"date":"2027-03-01","bursts":[{"open":[{"tool":"get_message","args":{"id":9007199254740993}}]}]}]}`))
				assertpkg.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{URL: srv.URL, AllowInsecure: true})
			require.NoError(err)
			got, text, err := c.Timeline(context.Background(), timeline.Request{Format: format, Tiers: []int{1, 3}, IncludeCold: true})
			require.NoError(err)
			if format == "markdown" {
				assert.Equal("# Timeline\n", text)
				assert.Nil(got)
			} else {
				require.NotNil(got)
				assert.Equal(timeline.ModeDay, got.Mode)
				data, err := json.Marshal(got, json.Deterministic(true))
				require.NoError(err)
				assert.Contains(string(data), `"id":9007199254740993`)
			}
		})
	}
}
func TestTimelineAmbiguityKeepsCandidates(t *testing.T) {
	require := requirepkg.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, err := w.Write([]byte(`{"error":"ambiguous_reference","message":"Choose a person","candidates":[{"person_id":7,"person_uid":"synthetic-seven","display_name":"Alice"}]}`))
		assertpkg.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, AllowInsecure: true})
	require.NoError(err)
	_, _, err = c.Timeline(t.Context(), timeline.Request{With: "Alice"})
	var ambiguous *timeline.ErrAmbiguousReference
	require.ErrorAs(err, &ambiguous)
	require.Len(ambiguous.Candidates, 1)
	assertpkg.Equal(t, int64(7), ambiguous.Candidates[0].PersonID)
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
				assertpkg.Equal(t, tc.wantBefore, r.URL.Query().Get("before"))
				assertpkg.Equal(t, tc.wantAfter, r.URL.Query().Get("after"))
				w.Header().Set("Content-Type", "text/markdown")
				_, err := w.Write([]byte("# Timeline\n"))
				assertpkg.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			client, err := New(Config{URL: srv.URL, AllowInsecure: true})
			requirepkg.NoError(t, err)
			_, _, err = client.Timeline(t.Context(), tc.req)
			requirepkg.NoError(t, err)
		})
	}
}
