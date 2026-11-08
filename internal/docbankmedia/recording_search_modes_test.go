package docbankmedia

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordingSearchAcceptsNonlexicalReport(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request SearchRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, mode, request.Mode)
				require.NoError(t, json.NewEncoder(w).Encode(SearchReport{
					RequestedMode: mode, ActualMode: mode, MediaSourceSelection: true,
					MediaSelections: []SearchMediaSelection{}, Results: []SearchHit{},
					Coverage: SearchCoverage{State: "incomplete", ScopedDocuments: 1},
				}))
			}))
			t.Cleanup(remote.Close)
			client, err := NewClient(remote.URL, nil)
			require.NoError(t, err)
			report, err := client.Search(t.Context(), SearchRequest{
				Query: "delivery time", Mode: mode, Profile: "recording-vectors", Limit: 20, ContentFirst: true,
				Fence:        &SearchFence{VaultUID: "fixture", ContentVersionIDs: []string{"123e4567-e89b-42d3-a456-426614174000"}},
				MediaSources: []SearchMediaSelector{{SearchMediaSource: SearchMediaSource{SourceID: "source", SourceVersionID: "version", ContentVersionID: "123e4567-e89b-42d3-a456-426614174000"}}},
			})
			require.NoError(t, err)
			assert.Equal(t, mode, report.ActualMode)
		})
	}
}
