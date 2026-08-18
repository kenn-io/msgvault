package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentSearchHTTPPreservesDedicatedContract(t *testing.T) {
	assert := assert.New(t)
	server, catalog := newTestServerWithMockStore(t)
	catalog.documentSearchFunc = func(
		_ context.Context,
		request store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		assert.Equal("shipping damage", request.Query)
		assert.Equal([]int64{4, 9}, request.SourceIDs)
		assert.Equal([]string{"email", "mms"}, request.MessageTypes)
		assert.Equal(int64(41), request.AttachmentID)
		assert.Equal(int64(42), request.MessageID)
		assert.Equal(7, request.PageSize)
		assert.Equal("opaque", request.Cursor)
		return store.DocumentSearchResponse{
			Revision: 12, NextCursor: "next",
			Results: []store.DocumentSearchResult{{
				AttachmentID: 41, MessageID: 42, Filename: "damage-photo.docx",
				Excerpt: "shipping damage", MatchedSignals: []string{"content"}, Rank: 1,
			}},
		}, nil
	}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/documents/search?q=shipping+damage&source_id=4,9&message_type=email,mms&attachment_id=41&message_id=42&limit=7&cursor=opaque",
		nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var body store.DocumentSearchResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Results, 1)
	assert.Equal("damage-photo.docx", body.Results[0].Filename)
	assert.Equal("next", body.NextCursor)
}

func TestDocumentSearchHTTPMapsCursorRevisionConflict(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		return store.DocumentSearchResponse{}, store.ErrDocumentSearchCursorStale
	}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/documents/search?q=evidence&cursor=stale", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Contains(t, response.Body.String(), "document_index_changed")
}

func TestDocumentSearchHTTPMapsUnavailableFTS(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		return store.DocumentSearchResponse{}, store.ErrDocumentSearchUnavailable
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "document_search_unavailable")
}

func TestDocumentSearchHTTPRejectsCrossOriginKeylessRequest(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	calls := 0
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		calls++
		return store.DocumentSearchResponse{}, nil
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", "https://cross-origin.example")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Equal(t, 0, calls)
	assert.Contains(t, response.Body.String(), "cross_origin_loopback")
}

func TestDocumentSearchHTTPHasDedicatedRateLimit(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	server.documentSearchRateLimiter.Close()
	server.documentSearchRateLimiter = NewRateLimiter(1, 1)
	t.Cleanup(server.documentSearchRateLimiter.Close)
	calls := 0
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		calls++
		return store.DocumentSearchResponse{}, nil
	}

	for _, wantStatus := range []int{http.StatusOK, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, request)
		assert.Equal(t, wantStatus, response.Code, response.Body.String())
	}
	assert.Equal(t, 1, calls)
}

func TestDocumentSearchHTTPWaitsOnOperationGate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	server, catalog := newTestServerWithMockStore(t)
	gate := NewSerialOperationGate()
	server.operationGate = gate
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "document build")
	require.True(ok)
	t.Cleanup(release)
	calls := 0
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		calls++
		return store.DocumentSearchResponse{}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(0, calls)
	assert.Contains(response.Body.String(), "document build")
}

func TestDocumentSearchHTTPLeavesReconciliationToSearchStore(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	reconcileCalls := 0
	catalog.documentReconcileFunc = func(context.Context) error {
		reconcileCalls++
		return nil
	}
	catalog.documentSearchFunc = func(
		context.Context,
		store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		return store.DocumentSearchResponse{}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 0, reconcileCalls)
}

func TestDocumentSearchHTTPDoesNotRegisterUnconsentedJournalConsumer(t *testing.T) {
	fixture := storetest.New(t)
	server := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: fixture.Store, Logger: slog.New(slog.DiscardHandler),
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	_, err := fixture.Store.GetAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.ErrorIs(t, err, store.ErrAttachmentChangeConsumerMissing)
}

func TestOpenAPIDocumentSearchParameters(t *testing.T) {
	document := OpenAPIDocument()
	operation := document.Paths["/api/v1/documents/search"].Get
	require.NotNil(t, operation)
	names := make([]string, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		names = append(names, parameter.Name)
	}
	assert.ElementsMatch(t,
		[]string{"q", "source_id", "message_type", "attachment_id", "message_id", "limit", "cursor"},
		names)
}

func TestDocumentStatusHTTPPreservesScopedContract(t *testing.T) {
	assert := assert.New(t)
	server, catalog := newTestServerWithMockStore(t)
	reconciled := false
	catalog.documentReconcileFunc = func(context.Context) error {
		reconciled = true
		return nil
	}
	catalog.documentStatusFunc = func(
		_ context.Context,
		profileID string,
		inputKey string,
		mediaTypes []string,
		messageTypes []string,
	) (store.DocumentIndexStatus, error) {
		assert.True(reconciled, "status must reconcile attachment changes before reading counts")
		assert.Equal("profile", profileID)
		assert.Equal("original", inputKey)
		assert.Equal([]string{"application/pdf", "application/epub+zip"}, mediaTypes)
		assert.Equal([]string{"email", "mms"}, messageTypes)
		return store.DocumentIndexStatus{ProfileExists: true, ReadyOwners: 4}, nil
	}
	catalog.documentRebuildFunc = func(
		_ context.Context,
		profileID string,
		inputKey string,
	) (store.DocumentExtractionRebuild, error) {
		assert.Equal("profile", profileID)
		assert.Equal("original", inputKey)
		return store.DocumentExtractionRebuild{ID: "rebuild", SnapshotOwners: 6}, nil
	}
	catalog.documentRemainingFunc = func(
		_ context.Context,
		rebuild store.DocumentExtractionRebuild,
		mediaTypes []string,
		messageTypes []string,
	) (int64, error) {
		assert.Equal("rebuild", rebuild.ID)
		assert.Equal([]string{"application/pdf", "application/epub+zip"}, mediaTypes)
		assert.Equal([]string{"email", "mms"}, messageTypes)
		return 2, nil
	}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/documents/status?profile_id=profile&input_key=original&"+
			"media_type=application%2Fpdf&media_type=application%2Fepub%2Bzip&"+
			"message_type=email&message_type=mms", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var body store.DocumentIndexStatusResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.True(body.Status.ProfileExists)
	assert.Equal(int64(4), body.Status.ReadyOwners)
	require.NotNil(t, body.ActiveRebuild)
	assert.Equal(int64(6), body.ActiveRebuild.SnapshotOwners)
	assert.Equal(int64(2), body.ActiveRebuild.RemainingOwners)
}

func TestOpenAPIDocumentStatusParameters(t *testing.T) {
	document := OpenAPIDocument()
	operation := document.Paths["/api/v1/documents/status"].Get
	require.NotNil(t, operation)
	names := make([]string, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		names = append(names, parameter.Name)
	}
	assert.ElementsMatch(t, []string{"profile_id", "input_key", "media_type", "message_type"}, names)
	for _, parameter := range operation.Parameters {
		if parameter.Name == "media_type" {
			assert.True(t, parameter.Required)
		}
	}
}
