package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// morePagesEngine reports one more row than each page holds so every paged
// endpoint issues a next cursor from the small explore fixture.
type morePagesEngine struct {
	*query.DuckDBEngine
}

func (e morePagesEngine) ExploreGroups(ctx context.Context, request query.ExploreGroupRequest) (*query.ExploreGroupResponse, error) {
	result, err := e.DuckDBEngine.ExploreGroups(ctx, request)
	if err == nil {
		result.TotalCount = int64(request.Page.Offset+len(result.Rows)) + 1
	}
	return result, err
}

func (e morePagesEngine) ExploreFiles(ctx context.Context, request query.ExploreFilesRequest) (*query.ExploreFilesResponse, error) {
	result, err := e.DuckDBEngine.ExploreFiles(ctx, request)
	if err == nil {
		result.TotalCount = int64(request.Page.Offset+len(result.Files)) + 1
	}
	return result, err
}

func (e morePagesEngine) GroupFiles(ctx context.Context, request query.FileGroupRequest) (*query.ExploreGroupResponse, error) {
	result, err := e.DuckDBEngine.GroupFiles(ctx, request)
	if err == nil {
		result.TotalCount = int64(request.Page.Offset+len(result.Rows)) + 1
	}
	return result, err
}

func (e morePagesEngine) SearchFiles(ctx context.Context, request query.FileSearchRequest) (*query.FileSearchResponse, error) {
	result, err := e.DuckDBEngine.SearchFiles(ctx, request)
	if err == nil {
		result.TotalCount = int64(request.Page.Offset+len(result.Files)) + 1
	}
	return result, err
}

func (e morePagesEngine) SearchPeople(ctx context.Context, request query.PersonSearchRequest) (*query.PersonSearchResponse, error) {
	result, err := e.DuckDBEngine.SearchPeople(ctx, request)
	if err == nil {
		result.TotalCount = int64(request.Page.Offset+len(result.Rows)) + 1
	}
	return result, err
}

func TestPagedExploreSearchRejectsBadSemanticCursors(t *testing.T) {
	endpoints := []struct {
		path string
		body string
	}{
		{"/api/v1/explore/groups", `{"grouping":["source"],"query":"alpha","search_mode":"semantic","limit":1%s}`},
		{"/api/v1/explore/files", `{"predicate":{"query":"alpha","search_mode":"semantic"},"limit":1%s}`},
		{"/api/v1/files/groups", `{"predicate":{"query":"alpha","search_mode":"semantic"},"grouping":["source"],"limit":1%s}`},
		{"/api/v1/files/search", `{"predicate":{"query":"alpha","search_mode":"semantic"},"limit":1%s}`},
		{"/api/v1/participants/search", `{"predicate":{"query":"alpha","search_mode":"semantic"},"limit":1%s}`},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.path, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			backend := &fakeVectorBackend{
				active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
				searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2}, {MessageID: 3, Score: .7, Rank: 3}},
			}
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
				Store: &fileCatalogStore{mockStore: &mockStore{stats: &StoreStats{}}, files: map[int64]store.FileMetadata{
					11: {ID: 11, MessageID: 1, ConversationID: 101, Filename: "older.txt", MimeType: "text/plain"},
					12: {ID: 12, MessageID: 2, ConversationID: 102, Filename: "newest.pdf", MimeType: "application/pdf"},
				}},
				Engine:       morePagesEngine{newExploreDuckDBFixture(t)},
				HybridEngine: hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"}),
				Backend:      backend, Logger: testLogger(),
			})
			first := postExploreJSON(t, srv, endpoint.path, fmt.Sprintf(endpoint.body, ""))
			require.Equal(http.StatusOK, first.Code, first.Body.String())
			var page struct {
				NextCursor string `json:"next_cursor"`
			}
			require.NoError(json.Unmarshal(first.Body.Bytes(), &page))
			require.NotEmpty(page.NextCursor)
			cursor, err := srv.decodeExploreCursor(page.NextCursor)
			require.NoError(err)
			require.NotEmpty(cursor.Snapshot)

			missing := cursor
			missing.Snapshot = ""
			response := postExploreJSON(t, srv, endpoint.path,
				fmt.Sprintf(endpoint.body, fmt.Sprintf(`,"cursor":%q`, srv.encodeExploreCursor(missing))))
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), `"error":"invalid_cursor"`)
			assert.Contains(response.Body.String(), "semantic cursor is missing its candidate snapshot")

			stale := cursor
			stale.SearchRevision = "stale-revision"
			response = postExploreJSON(t, srv, endpoint.path,
				fmt.Sprintf(endpoint.body, fmt.Sprintf(`,"cursor":%q`, srv.encodeExploreCursor(stale))))
			assert.Equal(http.StatusConflict, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), "search_revision_changed")
		})
	}
}
