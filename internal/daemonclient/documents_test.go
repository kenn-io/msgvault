package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestSearchDocumentsUsesGeneratedDaemonContract(t *testing.T) {
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/documents/search", r.URL.Path)
		assert.Equal("damaged carton", r.URL.Query().Get("q"))
		assert.ElementsMatch([]string{"3", "7"}, r.URL.Query()["source_id"])
		assert.Equal("22", r.URL.Query().Get("attachment_id"))
		assert.Equal("5", r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(store.DocumentSearchResponse{
			Revision: 9, Truncated: true,
			Results: []store.DocumentSearchResult{{
				AttachmentID: 22, MessageID: 23, SourceID: 3,
				OccurrenceKey: "occurrence", CanonicalBlobHash: "hash", ChunkKey: "chunk",
				Filename: "claim.docx", Excerpt: "damaged carton", ProfileID: "profile",
				ExtractionID: "extraction", Provider: "mistral", Model: "ocr",
				MatchedSignals: []string{"content"}, Rank: 1,
			}},
		}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	response, err := client.SearchDocuments(context.Background(), store.DocumentSearchRequest{
		Query: "damaged carton", SourceIDs: []int64{3, 7}, AttachmentID: 22, PageSize: 5,
	})
	require.NoError(t, err)
	assert.Equal(int64(9), response.Revision)
	assert.True(response.Truncated)
	require.Len(t, response.Results, 1)
	assert.Equal("claim.docx", response.Results[0].Filename)
	assert.Equal([]string{"content"}, response.Results[0].MatchedSignals)
}

func TestDocumentIndexStatusUsesGeneratedDaemonContract(t *testing.T) {
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/documents/status", r.URL.Path)
		assert.Equal("profile", r.URL.Query().Get("profile_id"))
		assert.Equal("original", r.URL.Query().Get("input_key"))
		assert.ElementsMatch(
			[]string{"application/pdf", "application/epub+zip"},
			r.URL.Query()["media_type"],
		)
		assert.ElementsMatch([]string{"email", "mms"}, r.URL.Query()["message_type"])
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(store.DocumentIndexStatusResponse{
			Status: store.DocumentIndexStatus{
				ProfileExists: true, ExactConsent: true, ReadyOwners: 4,
				AverageProviderLatencyMS: 12.5,
			},
			ActiveRebuild: &store.DocumentIndexRebuildStatus{
				SnapshotOwners: 6, RemainingOwners: 2,
			},
		}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	response, err := client.GetDocumentIndexStatus(context.Background(), store.DocumentIndexStatusRequest{
		ProfileID: "profile", ExtractionInputKey: "original",
		AllowedMediaTypes:   []string{"application/pdf", "application/epub+zip"},
		AllowedMessageTypes: []string{"email", "mms"},
	})
	require.NoError(t, err)
	assert.True(response.Status.ProfileExists)
	assert.True(response.Status.ExactConsent)
	assert.Equal(int64(4), response.Status.ReadyOwners)
	assert.InDelta(12.5, response.Status.AverageProviderLatencyMS, 0.001)
	require.NotNil(t, response.ActiveRebuild)
	assert.Equal(int64(6), response.ActiveRebuild.SnapshotOwners)
	assert.Equal(int64(2), response.ActiveRebuild.RemainingOwners)
}
