package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fixedFileBlobStore struct{ content []byte }

func (s fixedFileBlobStore) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(s.content)), int64(len(s.content)), nil
}

type fileSearchEngine struct {
	*querytest.MockEngine

	request      query.FileSearchRequest
	result       *query.FileSearchResponse
	groupRequest query.FileGroupRequest
	groupResult  *query.ExploreGroupResponse
	groupErr     error
}

func (e *fileSearchEngine) SearchFiles(_ context.Context, request query.FileSearchRequest) (*query.FileSearchResponse, error) {
	e.request = request
	return e.result, nil
}

func (e *fileSearchEngine) GroupFiles(_ context.Context, request query.FileGroupRequest) (*query.ExploreGroupResponse, error) {
	e.groupRequest = request
	return e.groupResult, e.groupErr
}

type fileCatalogStore struct {
	*mockStore

	files      map[int64]store.FileMetadata
	batchCalls int
	batchIDs   []int64
}

func (s *fileCatalogStore) GetFileMetadata(_ context.Context, id int64) (*store.FileMetadata, error) {
	file, ok := s.files[id]
	if !ok {
		return nil, nil //nolint:nilnil // not-found is the expected catalog contract
	}
	return &file, nil
}

func (s *fileCatalogStore) GetFileMetadataBatch(_ context.Context, ids []int64) (map[int64]store.FileMetadata, error) {
	s.batchCalls++
	s.batchIDs = append([]int64(nil), ids...)
	result := make(map[int64]store.FileMetadata, len(ids))
	for _, id := range ids {
		if file, ok := s.files[id]; ok {
			result[id] = file
		}
	}
	return result, nil
}

func TestFilesSearchUsesAnalyticalQueryAndOneCatalogBatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{}, result: &query.FileSearchResponse{
		Files: []query.FileRow{
			{ID: 1, Key: "file:1", EntryKey: "message:1", MessageID: 11, ConversationID: 21, OccurredAt: now, Filename: "local.png", MimeType: "image/png", MIMEFamily: query.FileMIMEImage},
			{ID: 2, Key: "file:2", EntryKey: "message:2", MessageID: 12, ConversationID: 22, OccurredAt: now, Filename: "remote.pdf", MimeType: "application/pdf", MIMEFamily: query.FileMIMEPDF},
			{ID: 3, Key: "file:3", EntryKey: "message:3", MessageID: 13, ConversationID: 23, OccurredAt: now, Filename: "missing.bin"},
			{ID: 4, Key: "file:4", EntryKey: "message:4", MessageID: 14, ConversationID: 24, OccurredAt: now, Filename: "metadata.txt"},
		},
		TotalCount: 4, CacheRevision: "cache-files", SearchProvenance: query.SearchProvenance{},
	}}
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{
		1: {ID: 1, MessageID: 11, ConversationID: 21, Filename: "local.png", MimeType: "image/png", ContentHash: hash, StoragePath: "aa/" + hash},
		2: {ID: 2, MessageID: 12, ConversationID: 22, Filename: "remote.pdf", MimeType: "application/pdf", URL: "https://files.example/remote.pdf"},
		3: {ID: 3, MessageID: 13, ConversationID: 23, Filename: "missing.bin", ContentHash: hash},
		4: {ID: 4, MessageID: 14, ConversationID: 24, Filename: "metadata.txt"},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  catalog, Engine: engine, Logger: testLogger(),
	})

	body := bytes.NewBufferString(`{"predicate":{"filters":[{"dimension":"source","values":["7"]}]},"filename_query":"report","mime_families":["image","pdf"],"sort":{"field":"size","direction":"asc"},"limit":25}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/search", body)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result FileSearchHTTPResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&result))
	requirements.Len(result.Files, 4)
	assertions.Equal([]int64{1, 2, 3, 4}, catalog.batchIDs)
	assertions.Equal(1, catalog.batchCalls)
	assertions.Equal(FileContentLocal, result.Files[0].ContentState)
	assertions.True(result.Files[0].ContentAvailable)
	assertions.Equal(FileContentURLOnly, result.Files[1].ContentState)
	assertions.False(result.Files[1].ContentAvailable)
	assertions.Equal(FileContentMissingBlob, result.Files[2].ContentState)
	assertions.Equal(FileContentMetadataOnly, result.Files[3].ContentState)
	assertions.Equal([]int64{7}, engine.request.Explore.Context.SourceIDs)
	assertions.Equal("report", engine.request.FilenameQuery)
	assertions.Equal([]query.FileMIMEFamily{query.FileMIMEImage, query.FileMIMEPDF}, engine.request.MIMEFamilies)
	assertions.Equal(query.SortSpec{Field: "size", Direction: "asc"}, engine.request.Sort)
}

// TestPersonFilesSearchWidensScopeToIdentityCluster covers the identity
// consistency between the Relationships hub panes: the person-scoped files
// search must see the same cluster the person detail header and relationship
// timeline report, so files attached only to a linked alias's messages are
// found. An unlinked participant stays scoped to its own ID.
func TestPersonFilesSearchWidensScopeToIdentityCluster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	primary, err := st.EnsureParticipant("primary@example.com", "Primary", "example.com")
	requirements.NoError(err)
	secondary, err := st.EnsureParticipant("secondary@example.com", "Secondary", "example.com")
	requirements.NoError(err)
	solo, err := st.EnsureParticipant("solo@example.com", "Solo", "example.com")
	requirements.NoError(err)
	_, err = st.LinkParticipants(primary, secondary)
	requirements.NoError(err)

	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{}, result: &query.FileSearchResponse{
		Files: []query.FileRow{}, TotalCount: 0, CacheRevision: "cache-files",
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})
	search := func(participantID int64) {
		request := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/people/%d/files/search", participantID), bytes.NewBufferString(`{"predicate":{}}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	}

	search(primary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.request.Explore.Context.ParticipantIDs,
		"a linked participant's files search must scope to every cluster member")

	search(secondary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.request.Explore.Context.ParticipantIDs,
		"any cluster member resolves the same scope")

	search(solo)
	assertions.Equal([]int64{solo}, engine.request.Explore.Context.ParticipantIDs,
		"an unlinked participant stays scoped to its own ID")
}

func TestFileMetadataNamesEveryContentStateAndContainingAuthorities(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{
		1: {ID: 1, MessageID: 11, ConversationID: 21, Filename: "metadata.txt"},
		2: {ID: 2, MessageID: 12, ConversationID: 22, Filename: "remote.txt", URL: "https://files.example/remote.txt"},
		3: {ID: 3, MessageID: 13, ConversationID: 23, Filename: "missing.pdf", MimeType: "application/pdf", ContentHash: hash},
		4: {ID: 4, MessageID: 14, ConversationID: 24, Filename: "local.png", MimeType: "image/png", ContentHash: hash, StoragePath: "bb/" + hash},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: catalog,
		Engine: &querytest.MockEngine{}, Logger: testLogger(),
	})

	wants := []struct {
		id        int
		state     FileContentState
		available bool
	}{
		{id: 1, state: FileContentMetadataOnly},
		{id: 2, state: FileContentURLOnly},
		{id: 3, state: FileContentMissingBlob},
		{id: 4, state: FileContentLocal, available: true},
	}
	for _, want := range wants {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+string(rune('0'+want.id)), nil)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		var metadata FileMetadataResponse
		requirements.NoError(json.NewDecoder(response.Body).Decode(&metadata))
		assertions.Equal(want.state, metadata.ContentState)
		assertions.Equal(want.available, metadata.ContentAvailable)
		assertions.Equal(int64(10+want.id), metadata.MessageID)
		assertions.Equal(int64(20+want.id), metadata.ConversationID)
	}
}

func TestFileContentUsesSelectedAttachmentMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{
		7: {ID: 7, MessageID: 11, ConversationID: 21, Filename: "selected.png", MimeType: "image/png", ContentHash: hash, StoragePath: "bb/blob"},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: catalog,
		Engine: &querytest.MockEngine{}, BlobStore: fixedFileBlobStore{content: []byte("png")}, Logger: testLogger(),
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/7/content", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("image/png", response.Header().Get("Content-Type"))
	assertions.Contains(response.Header().Get("Content-Disposition"), "selected.png")
	assertions.Equal("png", response.Body.String())
}

func TestFilesSearchNamesUnavailableCache(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{}, Engine: &querytest.MockEngine{}, Logger: testLogger(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/search", bytes.NewBufferString(`{"predicate":{}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "cache")
}

func TestFilesSearchPreservesLegitimateEmptyFilenameAndMIME(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{}, result: &query.FileSearchResponse{
		Files: []query.FileRow{{
			ID: 9, Key: "file:9", EntryKey: "message:9", MessageID: 9,
			ConversationID: 9, OccurredAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
			MIMEFamily: query.FileMIMEOther,
		}},
		TotalCount: 1, CacheRevision: "cache-empty", SearchProvenance: query.SearchProvenance{},
	}}
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{
		9: {ID: 9, MessageID: 9, ConversationID: 9},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  catalog, Engine: engine, Logger: testLogger(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/search", bytes.NewBufferString(`{"predicate":{}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result FileSearchHTTPResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&result))
	requirements.Len(result.Files, 1)
	assertions.Empty(result.Files[0].Filename)
	assertions.Empty(result.Files[0].MimeType)
}

func TestFileGroupsUsesFilePopulationAndForwardsEveryConstraint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{}, groupResult: &query.ExploreGroupResponse{
		Rows:       []query.ExploreGroupRow{{Key: "7", Label: "Example source", Count: 2, EstimatedBytes: 300, LatestAt: now}},
		TotalCount: 1, CacheRevision: "cache-files", SearchProvenance: query.SearchProvenance{},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{}, Engine: engine, Logger: testLogger(),
	})
	body := bytes.NewBufferString(`{
		"predicate":{"filters":[{"dimension":"source","values":["7"]}]},
		"filename_query":" invoice ","mime_families":["pdf"],"grouping":["participant"],
		"sort":[{"field":"estimated_bytes","direction":"desc"}],"limit":25
	}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/groups", body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result FileGroupsHTTPResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&result))
	requirements.Len(result.Rows, 1)
	assertions.Equal(int64(2), result.Rows[0].Count)
	assertions.Equal([]int64{7}, engine.groupRequest.Explore.Context.SourceIDs)
	assertions.Equal("invoice", engine.groupRequest.FilenameQuery)
	assertions.Equal([]query.FileMIMEFamily{query.FileMIMEPDF}, engine.groupRequest.MIMEFamilies)
	assertions.Equal("participant", engine.groupRequest.Dimension)
	assertions.Equal(query.SortSpec{Field: "estimated_bytes", Direction: "desc"}, engine.groupRequest.Sort)
	assertions.Equal(query.PageSpec{Limit: 25}, engine.groupRequest.Page)
}

func TestFileGroupsNamesUnavailableCacheWithoutGenericFallback(t *testing.T) {
	engine := &fileSearchEngine{
		MockEngine: &querytest.MockEngine{},
		groupErr:   &query.CacheUnavailableError{Readiness: query.CacheStaleSchema},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{}, Engine: engine, Logger: testLogger(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/groups", bytes.NewBufferString(`{
		"predicate":{},"grouping":["source"]
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "stale_schema")
}

func TestFileGroupsCursorBindsRequestAndCacheRevision(t *testing.T) {
	requirements := require.New(t)
	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{}, groupResult: &query.ExploreGroupResponse{
		Rows:       []query.ExploreGroupRow{{Key: "1", Label: "First", Count: 1}},
		TotalCount: 2, CacheRevision: "cache-one", SearchProvenance: query.SearchProvenance{},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{}, Engine: engine, Logger: testLogger(),
	})
	requestBody := `{"predicate":{},"filename_query":"invoice","mime_families":["pdf"],"grouping":["source"],"limit":1}`
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/v1/files/groups", bytes.NewBufferString(requestBody))
	firstRequest.Header.Set("Content-Type", "application/json")
	firstResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(firstResponse, firstRequest)
	requirements.Equal(http.StatusOK, firstResponse.Code, firstResponse.Body.String())
	var first FileGroupsHTTPResponse
	requirements.NoError(json.NewDecoder(firstResponse.Body).Decode(&first))
	requirements.NotEmpty(first.NextCursor)

	secondBody := fmt.Sprintf(`{"predicate":{},"filename_query":"invoice","mime_families":["pdf"],"grouping":["source"],"limit":1,"cursor":%q}`, first.NextCursor)
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/v1/files/groups", bytes.NewBufferString(secondBody))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(secondResponse, secondRequest)
	requirements.Equal(http.StatusOK, secondResponse.Code, secondResponse.Body.String())
	assert.Equal(t, query.PageSpec{Limit: 1, Offset: 1}, engine.groupRequest.Page)

	engine.groupResult.CacheRevision = "cache-two"
	staleRequest := httptest.NewRequest(http.MethodPost, "/api/v1/files/groups", bytes.NewBufferString(secondBody))
	staleRequest.Header.Set("Content-Type", "application/json")
	staleResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(staleResponse, staleRequest)
	assert.Equal(t, http.StatusConflict, staleResponse.Code)
	assert.Contains(t, staleResponse.Body.String(), "archive_revision_changed")
}
