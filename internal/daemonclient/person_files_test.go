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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestSearchPersonFilesUsesGeneratedDaemonContract(t *testing.T) {
	assertions := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/people/40/files/search", r.URL.Path)
		var request generated.PersonFileSearchHTTPRequest
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		assertions.Equal([]generated.PersonFileSearchHTTPRequestDirections{
			generated.PersonFileSearchHTTPRequestDirectionsFromPerson,
			generated.PersonFileSearchHTTPRequestDirectionsGroup,
		}, request.Directions)
		assertions.Equal("inspection", *request.FilenameQuery)
		assertions.Equal([]string{"image", "pdf"}, request.MimeFamilies)
		assertions.Equal(int64(25), *request.Limit)
		assertions.Equal("opaque", *request.Cursor)
		if !assertions.Len(request.Predicate.Filters, 2) {
			return
		}
		assertions.Equal("after", string(request.Predicate.Filters[0].Dimension))
		assertions.Equal([]string{"2026-08-01T12:34:56.123456789Z"}, request.Predicate.Filters[0].Values)
		assertions.Equal("before", string(request.Predicate.Filters[1].Dimension))
		assertions.Equal([]string{"2026-08-20T13:45:56.987654321Z"}, request.Predicate.Filters[1].Values)

		w.Header().Set("Content-Type", "application/json")
		filename, mimeType := "inspection.png", "image/png"
		assertions.NoError(json.NewEncoder(w).Encode(generated.PersonFileSearchHTTPResponse{
			Files: []generated.PersonFileSearchRow{{
				ID: 17, Key: "file:17", EntryKey: "source:20:message:synthetic-message",
				MessageID: 18, ConversationID: 19, OccurredAt: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
				SourceID: 20, SourceType: "synthetic", SourceIdentifier: "synthetic-source",
				ContainingTitle: "Synthetic inspection", Filename: &filename, MimeType: &mimeType,
				MimeFamily: "image", ContentState: generated.PersonFileSearchRowContentStateMetadataOnly,
				PersonProvenance: generated.PersonFileProvenance{
					ParticipantIds: []int64{4}, Roles: []generated.PersonFileProvenanceRoles{generated.From},
					Directions: []generated.PersonFileProvenanceDirections{generated.FromPerson},
				},
			}},
			TotalCount: 1, CacheRevision: "synthetic-revision",
		}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	after := time.Date(2026, 8, 1, 12, 34, 56, 123456789, time.UTC)
	before := time.Date(2026, 8, 20, 13, 45, 56, 987654321, time.UTC)
	response, err := client.SearchPersonFiles(t.Context(), PersonFileSearchOptions{
		PersonID: 40, Directions: []personscope.Direction{personscope.FromPerson, personscope.Group},
		After: &after, Before: &before, Filename: "inspection",
		MIMEFamilies: []query.FileMIMEFamily{query.FileMIMEImage, query.FileMIMEPDF},
		Limit:        25, Cursor: "opaque",
	})
	require.NoError(t, err)
	require.Len(t, response.Files, 1)
	assertions.Equal(int64(18), response.Files[0].MessageID)
	assertions.Equal([]generated.PersonFileProvenanceRoles{generated.From}, response.Files[0].PersonProvenance.Roles)
}
