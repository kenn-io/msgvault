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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/businessreviews"
	"go.kenn.io/msgvault/internal/config"
)

func reviewAPIFixture(t *testing.T, key string) (*Server, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	service := businessreviews.NewService(businessreviews.Options{Targets: func(context.Context) ([]businessreviews.Target, error) { calls.Add(1); return nil, nil }})
	t.Cleanup(service.Close)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: key}}, Store: &mockStore{stats: &StoreStats{}}, Logger: testLogger(), BusinessReviews: service})
	return srv, calls
}
func reviewAPIRequest(t *testing.T, s *Server, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/business-reviews/control", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	return w
}
func TestBusinessReviewsStrictJSONAndNoStore(t *testing.T) {
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

func TestBusinessReviewsRequestMediaTypeAndSize(t *testing.T) {
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
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/business-reviews/control", strings.NewReader(tc.body))
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
func TestBusinessReviewsOwnerAndKeylessPolicy(t *testing.T) {
	assert := assert.New(t)
	s, calls := reviewAPIFixture(t, "owner")
	w := reviewAPIRequest(t, s, `{"action":"capabilities"}`, "")
	assert.Equal(401, w.Code)
	assert.Zero(calls.Load())
	s, calls = reviewAPIFixture(t, "")
	for _, body := range []string{`{"action":"preview_reply","target":"shop","review_id":"r","comment":"Thanks"}`, `{"action":"confirm_reply","confirmation_token":"token","confirmed":true}`, `{"action":"receipt","receipt_id":"r"}`, `{"action":"invalidate_target","target":"shop"}`} {
		w = reviewAPIRequest(t, s, body, "")
		assert.Equal(403, w.Code, w.Body.String())
	}
	assert.Zero(calls.Load())
}

type businessReviewAPIProvider struct{ puts int }

func (p *businessReviewAPIProvider) List(context.Context, string, string, int) (businessreviews.Page, error) {
	return businessreviews.Page{}, nil
}
func (p *businessReviewAPIProvider) Get(context.Context, string) (businessreviews.Review, error) {
	return businessreviews.Review{Name: "accounts/a/locations/l/reviews/r", ReviewID: "r", Comment: "CANARY_PROVIDER_REVIEW"}, nil
}
func (p *businessReviewAPIProvider) UpdateReply(context.Context, string, string) (businessreviews.Reply, error) {
	p.puts++
	return businessreviews.Reply{}, businessreviews.ErrUnknown
}
func TestBusinessReviewsSessionIsolationCSRFAndOwnerReceipts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &businessReviewAPIProvider{}
	service := businessreviews.NewService(businessreviews.Options{Targets: func(context.Context) ([]businessreviews.Target, error) {
		return []businessreviews.Target{{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: "accounts/a/locations/l", Enabled: true, OwnProject: true, RepliesEnabled: true}}, nil
	}, Provider: func(context.Context, businessreviews.Target) (businessreviews.Provider, string, error) {
		return provider, "checked", nil
	}})
	t.Cleanup(service.Close)
	var logs bytes.Buffer
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "remote", APIKey: "remote-key"}}}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), BusinessReviews: service})
	one, first, err := srv.sessions.create()
	require.NoError(err)
	two, second, err := srv.sessions.create()
	require.NoError(err)
	post := func(body, session, csrf, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/business-reviews/control", strings.NewReader(body))
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
	var preview businessreviews.Result
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
