package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/personscope"
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
		assert.Equal("lexical", request.SearchMode)
		assert.Equal(1001, request.CandidateLimit)
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
		"/api/v1/documents/search?q=shipping+damage&source_id=4,9&message_type=email,mms&attachment_id=41&message_id=42&limit=7&cursor=opaque&mode=lexical&candidate_limit=1001",
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

func TestDocumentSearchHTTPResolvesDurablePersonScope(t *testing.T) {
	assertions := assert.New(t)
	server, catalog := newTestServerWithMockStore(t)
	catalog.personContextFunc = func(_ context.Context, id int64) (*store.Person, error) {
		assertions.Equal(int64(40), id)
		return &store.Person{ID: id, ParticipantIDs: []int64{4, 9}}, nil
	}
	catalog.documentSearchFunc = func(
		_ context.Context,
		request store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error) {
		require.NotNil(t, request.Person)
		assertions.Equal([]int64{4, 9}, request.Person.ParticipantIDs)
		assertions.Equal([]personscope.Direction{personscope.FromPerson, personscope.Group}, request.Person.Directions)
		require.NotNil(t, request.After)
		require.NotNil(t, request.Before)
		assertions.Equal("2026-08-01", request.After.Format("2006-01-02"))
		assertions.Equal("2026-08-20", request.Before.Format("2006-01-02"))
		return store.DocumentSearchResponse{}, nil
	}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/documents/search?q=inspection&person_id=40&direction=from_person,group&after=2026-08-01&before=2026-08-20", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestDocumentSearchHTTPExplicitSemanticUnavailableDoesNotFallBack(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	calls := 0
	catalog.documentSearchFunc = func(context.Context, store.DocumentSearchRequest) (store.DocumentSearchResponse, error) {
		calls++
		return store.DocumentSearchResponse{}, nil
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=evidence&mode=semantic&candidate_limit=25", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "semantic_search_unavailable")
	assert.Zero(t, calls)
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

func TestDocumentStatusHTTPExplicitAndCurrentRoutesRejectCrossOriginKeylessRequests(t *testing.T) {
	for _, path := range []string{
		"/api/v1/documents/status?profile_id=profile&input_key=original&media_type=application%2Fpdf",
		"/api/v1/documents/status/current",
	} {
		t.Run(path, func(t *testing.T) {
			server, catalog := newTestServerWithMockStore(t)
			calls := 0
			catalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
				calls++
				return "private-profile", []string{"application/pdf"}, nil
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("Origin", "https://cross-origin.example")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Zero(t, calls)
			assert.Contains(t, response.Body.String(), "cross_origin_loopback")
		})
	}
}

func TestDocumentStatusHTTPExplicitAndCurrentRoutesRequireConfiguredAPIKey(t *testing.T) {
	for _, path := range []string{
		"/api/v1/documents/status?profile_id=profile&input_key=original&media_type=application%2Fpdf",
		"/api/v1/documents/status/current",
	} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server, catalog := newTestServerWithMockStore(t)
			server.cfg.Server.APIKey = "synthetic-document-status-key"
			resolverCalls := 0
			catalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
				resolverCalls++
				return "private-profile", []string{"application/pdf"}, nil
			}

			unauthenticated := httptest.NewRecorder()
			server.Router().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(http.StatusUnauthorized, unauthenticated.Code, unauthenticated.Body.String())
			assert.Zero(resolverCalls)

			authenticatedRequest := httptest.NewRequest(http.MethodGet, path, nil)
			authenticatedRequest.Header.Set("X-Api-Key", "synthetic-document-status-key")
			authenticated := httptest.NewRecorder()
			server.Router().ServeHTTP(authenticated, authenticatedRequest)
			require.Equal(http.StatusOK, authenticated.Code, authenticated.Body.String())
			if path == "/api/v1/documents/status/current" {
				assert.Equal(1, resolverCalls)
			}
			assert.NotContains(authenticated.Body.String(), "private-profile")
		})
	}
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
		[]string{"q", "source_id", "message_type", "attachment_id", "message_id", "person_id", "participant_id", "direction", "after", "before", "limit", "cursor", "mode", "candidate_limit"},
		names)
}

func TestDocumentVectorStatusHTTPIsUsefulBeforeTargetConfiguration(t *testing.T) {
	fixture := storetest.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Attachments.Documents.Index.Embeddings.Enabled = true
	server := NewServerWithOptions(ServerOptions{Config: c, Store: fixture.Store, Logger: slog.New(slog.DiscardHandler)})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/vectors/status", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"enabled":true,"configured":false}`, response.Body.String())
}

func TestOpenAPIDocumentVectorStatusUsesSnakeCaseCoverage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	document := OpenAPIDocument()
	operation := document.Paths["/api/v1/documents/vectors/status"].Get
	requirements.NotNil(operation)
	coverage := document.Components.Schemas.Map()["DocumentVectorCoverage"]
	requirements.NotNil(coverage)
	assertions.ElementsMatch([]string{"required", "ready"}, slices.Collect(maps.Keys(coverage.Properties)))
	encoded, err := json.Marshal(store.DocumentVectorCoverage{Required: 4, Ready: 3})
	requirements.NoError(err)
	assertions.JSONEq(`{"required":4,"ready":3}`, string(encoded))
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

func TestDocumentStatusHTTPResolvesCurrentDurableScopeWithoutPublishingIdentifiers(t *testing.T) {
	assertions := assert.New(t)
	server, catalog := newTestServerWithMockStore(t)
	server.cfg.Attachments.Documents.Scope.MessageTypes = []string{"email", "mms"}
	privateProfileID := "private-current-document-profile"
	catalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
		return privateProfileID, []string{"application/pdf", "application/epub+zip"}, nil
	}
	catalog.documentStatusFunc = func(
		_ context.Context, profileID, inputKey string, mediaTypes, messageTypes []string,
	) (store.DocumentIndexStatus, error) {
		assertions.Equal(privateProfileID, profileID)
		assertions.Equal("original", inputKey)
		assertions.Equal([]string{"application/pdf", "application/epub+zip"}, mediaTypes)
		assertions.Equal([]string{"email", "mms"}, messageTypes)
		return store.DocumentIndexStatus{ProfileExists: true, ProfileEnabled: true, ReadyOwners: 4}, nil
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/status/current", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), `"ready_owners":4`)
	assertions.NotContains(response.Body.String(), privateProfileID)
}

func TestDocumentStatusHTTPRejectsPartialScopeAndFixesUnavailableCurrentScope(t *testing.T) {
	assert := assert.New(t)
	server, catalog := newTestServerWithMockStore(t)
	catalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
		return "", nil, store.ErrDocumentIndexStatusScopeUnavailable
	}

	partial := httptest.NewRecorder()
	server.Router().ServeHTTP(partial, httptest.NewRequest(
		http.MethodGet, "/api/v1/documents/status?profile_id=private-profile", nil,
	))
	assert.Equal(http.StatusBadRequest, partial.Code)
	assert.NotContains(partial.Body.String(), "private-profile")

	missingExplicit := httptest.NewRecorder()
	server.Router().ServeHTTP(missingExplicit, httptest.NewRequest(http.MethodGet, "/api/v1/documents/status", nil))
	assert.Equal(http.StatusBadRequest, missingExplicit.Code)

	unavailableServer, unavailableCatalog := newTestServerWithMockStore(t)
	unavailableCatalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
		return "", nil, store.ErrDocumentIndexStatusScopeUnavailable
	}
	unavailable := httptest.NewRecorder()
	unavailableServer.Router().ServeHTTP(unavailable, httptest.NewRequest(http.MethodGet, "/api/v1/documents/status/current", nil))
	assert.Equal(http.StatusServiceUnavailable, unavailable.Code)
	assert.Contains(unavailable.Body.String(), "document_status_scope_unavailable")
	assert.NotContains(unavailable.Body.String(), "target_profile_id")
}

func TestDocumentStatusHTTPCorruptDurableScopeRemainsRetryableAndPrivate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := storetest.New(t)
	profileID := "private-corrupt-selected-profile"
	profile := store.DocumentExtractionProfile{
		ID: profileID, Fingerprint: strings.Repeat("c", 64),
		Provider: "test", Endpoint: "https://example.invalid/documents",
		Region: "test", Model: "test-model",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"},
		PolicyJSON:        []byte(`{"normalization":1,"chunking":1}`),
	}
	_, err := fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	const corruptScopeRecord = "corrupt-current-scope-record"
	// Valid JSON with the wrong shape exercises scope decoding on both the
	// SQLite text column and PostgreSQL JSONB column.
	_, err = fixture.Store.DB().ExecContext(t.Context(), fixture.Store.Rebind(
		`UPDATE document_extraction_profiles SET allowed_media_types = ? WHERE id = ?`,
	), `{"media_type":"`+corruptScopeRecord+`"}`, profile.ID)
	requirements.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Attachments.Documents.Enabled = true
	server := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: fixture.Store, Logger: slog.New(slog.DiscardHandler),
	})

	current := doGet(server, "/api/v1/documents/status/current")
	requirements.Equal(http.StatusServiceUnavailable, current.Code, current.Body.String())
	assertions.Contains(current.Body.String(), "document_status_scope_unavailable")
	assertions.NotContains(current.Body.String(), profileID)
	assertions.NotContains(current.Body.String(), corruptScopeRecord)
	assertions.NotContains(current.Body.String(), "current document index status scope is invalid")

	operationsStatus := doGet(server, "/api/v1/operations/status")
	requirements.Equal(http.StatusOK, operationsStatus.Code, operationsStatus.Body.String())
	var status OperationStatusResponse
	requirements.NoError(json.Unmarshal(operationsStatus.Body.Bytes(), &status))
	for _, lane := range status.Lanes {
		if lane.Kind == operations.KindDocumentExtraction {
			assertions.True(lane.Configured)
			return
		}
	}
	requirements.Fail("document extraction lane was not returned")
}

func TestOpenAPIDocumentStatusParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	document := OpenAPIDocument()
	operation := document.Paths["/api/v1/documents/status"].Get
	require.NotNil(operation)
	names := make([]string, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		names = append(names, parameter.Name)
	}
	assert.ElementsMatch([]string{"profile_id", "input_key", "media_type", "message_type"}, names)
	required := map[string]bool{"profile_id": true, "input_key": true, "media_type": true}
	for _, parameter := range operation.Parameters {
		assert.Equal(required[parameter.Name], parameter.Required, parameter.Name)
	}
	currentPath := document.Paths["/api/v1/documents/status/current"]
	require.NotNil(currentPath)
	current := currentPath.Get
	require.NotNil(current)
	assert.Empty(current.Parameters)
}
