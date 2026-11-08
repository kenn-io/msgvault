package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/googlereviews"
)

func reviewAPIFixture(t *testing.T, key string) (*Server, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	service := googlereviews.NewService(googlereviews.Options{Targets: func(context.Context) ([]googlereviews.Target, error) { calls.Add(1); return nil, nil }})
	t.Cleanup(service.Close)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: key}}, Store: &mockStore{stats: &StoreStats{}}, Logger: testLogger(), GoogleReviews: service})
	return srv, calls
}
func reviewAPIRequest(t *testing.T, s *Server, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/google-reviews/control", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	return w
}
func TestGoogleReviewsStrictJSONAndNoStore(t *testing.T) {
	assert := assert.New(t)
	s, calls := reviewAPIFixture(t, "owner")
	valid := reviewAPIRequest(t, s, `{"action":"capabilities"}`, "owner")
	require.Equal(t, 200, valid.Code, valid.Body.String())
	assert.Equal("no-store", valid.Header().Get("Cache-Control"))
	baseline := calls.Load()
	for _, body := range []string{`null`, `{"action":"capabilities","target":null}`, `{"action":"capabilities","other":true}`, `{"action":"capabilities","action":"get"}`, `{"Action":"capabilities"}`, `{"action":"capabilities","comment":"CANARY"}`, `{"action":"capabilities"} {}`} {
		w := reviewAPIRequest(t, s, body, "owner")
		assert.Equal(400, w.Code, w.Body.String())
		assert.Equal("no-store", w.Header().Get("Cache-Control"))
		assert.NotContains(w.Body.String(), "CANARY")
	}
	assert.Equal(baseline, calls.Load())
}

func TestGoogleReviewsRequestMediaTypeAndSize(t *testing.T) {
	const canary = "CANARY_PRIVATE_REQUEST"
	capabilities := `{"action":"capabilities"}`
	oversized := `{"action":"list","target":"shop","page_token":"` + canary + strings.Repeat("x", 32768) + `"}`
	for _, tc := range []struct {
		name, mediaType, body, code string
		status                      int
	}{
		{"non_json", "text/plain", canary, "unsupported_media_type", 415},
		{"malformed_media_type", "application/json; charset=", canary, "unsupported_media_type", 415},
		{"missing_media_type", "", canary, "unsupported_media_type", 415},
		{"oversized_json", "application/json", oversized, "request_too_large", 413},
		{"one_byte_over_limit", "application/json", capabilities + strings.Repeat(" ", 32769-len(capabilities)), "request_too_large", 413},
		{"exact_limit", "application/json; charset=utf-8", capabilities + strings.Repeat(" ", 32768-len(capabilities)), "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			srv, calls := reviewAPIFixture(t, "owner")
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/google-reviews/control", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.mediaType)
			r.Header.Set("X-Api-Key", "owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assert.Equal(tc.status, w.Code, w.Body.String())
			assert.Equal("no-store", w.Header().Get("Cache-Control"))
			assert.NotContains(w.Body.String(), canary)
			if tc.status != http.StatusOK {
				var response ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				assert.Equal(tc.code, response.Error)
				assert.Zero(calls.Load(), "rejected input must not reach the service's target resolver")
			} else {
				assert.Positive(calls.Load(), "a request at the limit must reach the service")
			}
		})
	}
}
func TestGoogleReviewsOwnerAndKeylessPolicy(t *testing.T) {
	assert := assert.New(t)
	s, calls := reviewAPIFixture(t, "owner")
	w := reviewAPIRequest(t, s, `{"action":"capabilities"}`, "")
	assert.Equal(401, w.Code)
	assert.Zero(calls.Load())
	s, calls = reviewAPIFixture(t, "")
	for _, body := range []string{`{"action":"preview_reply","target":"shop","review_id":"r","comment":"Thanks"}`, `{"action":"confirm_reply","confirmation_token":"token","confirmed":true}`, `{"action":"receipt","receipt_id":"r"}`, `{"action":"invalidate_target","target":"shop"}`, `{"action":"resume_target","target":"shop","confirmation_token":"token"}`, `{"action":"backfill","target":"shop"}`} {
		w = reviewAPIRequest(t, s, body, "")
		assert.Equal(403, w.Code, w.Body.String())
	}
	assert.Zero(calls.Load())
}

type googleReviewSearchArchive struct{}

func (googleReviewSearchArchive) BeginGoogleReviewsBackfill(context.Context, string, string) (googlereviews.BackfillState, error) {
	return googlereviews.BackfillState{}, googlereviews.ErrArchive
}
func (googleReviewSearchArchive) ApplyGoogleReviewsPage(context.Context, string, string, string, googlereviews.Page) (googlereviews.BackfillState, error) {
	return googlereviews.BackfillState{}, googlereviews.ErrArchive
}
func (googleReviewSearchArchive) FailGoogleReviewsBackfill(context.Context, string, string, string) (googlereviews.BackfillState, error) {
	return googlereviews.BackfillState{}, googlereviews.ErrArchive
}
func (googleReviewSearchArchive) GoogleReviewsBackfillState(_ context.Context, target, _ string) (googlereviews.BackfillState, error) {
	return googlereviews.BackfillState{Target: target, Status: "not_started"}, nil
}
func (googleReviewSearchArchive) SearchGoogleReviews(_ context.Context, query, target string, offset, limit int) (googlereviews.ArchiveSearchResponse, error) {
	return googlereviews.ArchiveSearchResponse{
		Query: query, Target: target, Page: offset/limit + 1, PageSize: limit, Total: 1,
		Results: []googlereviews.ArchivedReview{{
			Target: "shop", Review: googlereviews.Review{ReviewID: "synthetic", Comment: "synthetic review text"},
		}},
	}, nil
}

func TestGoogleReviewsBackfillReturnsBusyWhenArchiveGateHeld(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "archive backup")
	require.True(t, ok)
	defer release()
	service := googlereviews.NewService(googlereviews.Options{
		Archive:           googleReviewSearchArchive{},
		BeginArchiveWrite: gate.BeginWorkContext,
		Targets: func(context.Context) ([]googlereviews.Target, error) {
			return []googlereviews.Target{{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: "accounts/a/locations/l", Enabled: true, OwnProject: true}}, nil
		},
	})
	t.Cleanup(service.Close)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}},
		Store:  &mockStore{stats: &StoreStats{}}, Logger: testLogger(), GoogleReviews: service, OperationGate: gate,
	})
	response := reviewAPIRequest(t, srv, `{"action":"backfill","target":"shop"}`, "owner")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "operation_in_progress", body.Error)
	assert.Contains(t, body.Message, "archive backup")
}

func TestGoogleReviewsArchiveSearchIsOwnerOnlyAndNotCached(t *testing.T) {
	require := require.New(t)
	service := googlereviews.NewService(googlereviews.Options{
		Archive: googleReviewSearchArchive{},
		Targets: func(context.Context) ([]googlereviews.Target, error) {
			return []googlereviews.Target{{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Enabled: true}}, nil
		},
	})
	t.Cleanup(service.Close)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}},
		Store:  &mockStore{stats: &StoreStats{}}, Logger: testLogger(), GoogleReviews: service,
	})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/api/v1/google-reviews/archive/search?q=synthetic&target=shop&page=1&page_size=10", nil)
	request.Header.Set("X-Api-Key", "owner")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.Equal("no-store", response.Header().Get("Cache-Control"))
	assert.Contains(t, response.Body.String(), "synthetic review text")

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/api/v1/google-reviews/archive/search?q=synthetic", nil)
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/api/v1/google-reviews/archive/search?q=synthetic&page=1&page=2", nil)
	request.Header.Set("X-Api-Key", "owner")
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

type googleReviewAPIProvider struct{ puts int }

func (p *googleReviewAPIProvider) List(context.Context, string, string, int) (googlereviews.Page, error) {
	return googlereviews.Page{}, nil
}
func (p *googleReviewAPIProvider) Get(context.Context, string) (googlereviews.Review, error) {
	return googlereviews.Review{Name: "accounts/a/locations/l/reviews/r", ReviewID: "r", Comment: "CANARY_PROVIDER_REVIEW"}, nil
}
func (p *googleReviewAPIProvider) UpdateReply(context.Context, string, string) (googlereviews.Reply, error) {
	p.puts++
	return googlereviews.Reply{}, googlereviews.ErrUnknown
}
func TestGoogleReviewsSessionIsolationCSRFAndOwnerReceipts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &googleReviewAPIProvider{}
	service := googlereviews.NewService(googlereviews.Options{Targets: func(context.Context) ([]googlereviews.Target, error) {
		return []googlereviews.Target{{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: "accounts/a/locations/l", Enabled: true, OwnProject: true, RepliesEnabled: true}}, nil
	}, Provider: func(context.Context, googlereviews.Target) (googlereviews.Provider, string, error) {
		return provider, "checked", nil
	}})
	t.Cleanup(service.Close)
	var logs bytes.Buffer
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "remote", APIKey: "remote-key"}}}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), GoogleReviews: service})
	one, first, err := srv.sessions.create()
	require.NoError(err)
	two, second, err := srv.sessions.create()
	require.NoError(err)
	post := func(body, session, csrf, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/google-reviews/control", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if session != "" {
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			request.Header.Set("Origin", "http://localhost")
			request.Header.Set(csrfHeaderName, csrf)
		}
		if key != "" {
			request.Header.Set("X-Api-Key", key)
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		return response
	}
	previewBody := `{"action":"preview_reply","target":"shop","review_id":"r","comment":"CANARY_EXACT_REPLY"}`
	denied := post(previewBody, one, "", "")
	assert.Equal(403, denied.Code)
	denied = post(previewBody, "", "", "remote-key")
	assert.Equal(403, denied.Code)
	previewResponse := post(previewBody, one, first.CSRFToken, "")
	require.Equal(200, previewResponse.Code, previewResponse.Body.String())
	var preview googlereviews.Result
	require.NoError(json.Unmarshal(previewResponse.Body.Bytes(), &preview))
	require.NotNil(preview.Preview)
	receiptBody := `{"action":"receipt","receipt_id":"` + preview.Preview.ReceiptID + `"}`
	denied = post(receiptBody, two, second.CSRFToken, "")
	assert.Equal(403, denied.Code)
	allowed := post(receiptBody, one, first.CSRFToken, "")
	assert.Equal(200, allowed.Code)
	confirm := `{"action":"confirm_reply","confirmation_token":"` + preview.Preview.ConfirmationToken + `","confirmed":true}`
	result := post(confirm, one, first.CSRFToken, "")
	require.Equal(200, result.Code, result.Body.String())
	assert.Contains(result.Body.String(), `"unknown"`)
	assert.NotContains(result.Body.String(), "CANARY")
	result = post(receiptBody, one, first.CSRFToken, "")
	require.Equal(200, result.Code)
	assert.NotContains(result.Body.String(), "CANARY")
	assert.Equal(1, provider.puts)
	// Separate owner-key requests share one principal, as separate CLI
	// invocations must be able to inspect the receipt issued by a preview.
	ownerPreview := post(previewBody, "", "", "owner")
	require.Equal(200, ownerPreview.Code, ownerPreview.Body.String())
	require.NoError(json.Unmarshal(ownerPreview.Body.Bytes(), &preview))
	require.NotNil(preview.Preview)
	ownerReceipt := post(`{"action":"receipt","receipt_id":"`+preview.Preview.ReceiptID+`"}`, "", "", "owner")
	require.Equal(200, ownerReceipt.Code, ownerReceipt.Body.String())
	assert.Contains(ownerReceipt.Body.String(), `"pending_confirmation"`)
	assert.NotContains(logs.String(), "CANARY")
	assert.NotContains(logs.String(), preview.Preview.ConfirmationToken)
	assert.NotContains(logs.String(), preview.Preview.ReceiptID)
}
