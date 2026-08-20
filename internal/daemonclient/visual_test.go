package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualSearchUsesSharedPersonScopeContract(t *testing.T) {
	require := require.New(t)
	after := time.Date(2026, 8, 20, 12, 34, 56, 123456789, time.FixedZone("test", -7*60*60))
	before := after.Add(90 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/search/attachments/visual", r.URL.Path)
		var request struct {
			PersonID   int64                   `json:"person_id"`
			Directions []personscope.Direction `json:"directions"`
			After      string                  `json:"after"`
			Before     string                  `json:"before"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		assert.Equal(t, int64(40), request.PersonID)
		assert.Equal(t, []personscope.Direction{personscope.ToPerson, personscope.Group}, request.Directions)
		assert.Equal(t, after.UTC().Format(time.RFC3339Nano), request.After)
		assert.Equal(t, before.UTC().Format(time.RFC3339Nano), request.Before)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(visual.SearchResponse{
			Results: []visual.AttachmentSearchResult{{
				AttachmentID: 17, MessageID: 18,
				PersonProvenance: &personscope.Provenance{
					ParticipantIDs: []int64{4}, Roles: []personscope.Role{personscope.RoleTo},
					Directions: []personscope.Direction{personscope.ToPerson},
				},
			}},
		}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	response, err := client.SearchVisualAttachmentsFiltered(t.Context(), VisualSearchOptions{
		Text: "inspection diagram", PersonID: 40,
		Directions: []personscope.Direction{personscope.ToPerson, personscope.Group},
		After:      &after, Before: &before,
	})
	require.NoError(err)
	require.Len(response.Results, 1)
	assert.Equal(t, []personscope.Role{personscope.RoleTo}, response.Results[0].PersonProvenance.Roles)
}

func TestVisualImageSearchPreservesRFC3339Bounds(t *testing.T) {
	after := time.Date(2026, 8, 20, 12, 34, 56, 123456789, time.FixedZone("test", -7*60*60))
	before := after.Add(90 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, r.ParseMultipartForm(1<<20)) {
			return
		}
		assert.Equal(t, after.UTC().Format(time.RFC3339Nano), r.FormValue("after"))
		assert.Equal(t, before.UTC().Format(time.RFC3339Nano), r.FormValue("before"))
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(visual.SearchResponse{}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)

	_, err = client.SearchVisualAttachmentsFiltered(t.Context(), VisualSearchOptions{
		Image: []byte("synthetic image"), After: &after, Before: &before,
	})

	require.NoError(t, err)
}
