package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/messageimport"
)

const (
	messageImportTestAPIKey = "synthetic-api-key-for-tests"
	validMessageImportBody  = `{
	  "source": {
	    "identifier": "feeds",
	    "display_name": "Feeds",
	    "account_email": "user@example.com"
	  },
	  "message": {
	    "external_id": "entry-42",
	    "message_type": "feed_item",
	    "conversation": {"external_id": "feed-7", "title": "Example Project releases"},
	    "sender": {"name": "Example Project", "url": "https://example.com"},
	    "title": "Example Project 1.2.0",
	    "url": "https://example.com/releases/1.2.0",
	    "sent_at": "2026-11-07T10:00:00-05:00",
	    "body_markdown": "## Changes\n\n- Faster sync.",
	    "metadata": {"entry_hash": "synthetic-hash"}
	  }
	}`
)

type fakeMessageImportStore struct {
	*mockStore

	result messageimport.Result
	err    error
	calls  int
	req    messageimport.Request
}

func (s *fakeMessageImportStore) ImportMessage(
	_ context.Context,
	req messageimport.Request,
) (messageimport.Result, error) {
	s.calls++
	s.req = req
	return s.result, s.err
}

func newMessageImportTestServer(t *testing.T, importer MessageImporter) *Server {
	t.Helper()
	var store MessageStore = &mockStore{stats: &StoreStats{}}
	if importer != nil {
		typed, ok := importer.(MessageStore)
		require.True(t, ok, "test importer must implement MessageStore")
		store = typed
	}
	return NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: messageImportTestAPIKey}},
		store,
		nil,
		testLogger(),
	)
}

func messageImportRequest(ctx context.Context, body string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/import/message", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", messageImportTestAPIKey)
	return req
}

func decodeErrorResponse(t *testing.T, resp *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var body ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func TestMessageImportRequiresAuthentication(t *testing.T) {
	t.Parallel()
	store := &fakeMessageImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMessageImportTestServer(t, store)

	for _, key := range []string{"", "wrong-key"} {
		req := messageImportRequest(t.Context(), validMessageImportBody)
		req.Header.Set("X-Api-Key", key)
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)

		assert.Equal(t, http.StatusUnauthorized, resp.Code, "key=%q body=%s", key, resp.Body.String())
	}
	assert.Equal(t, 0, store.calls)
}

func TestMessageImportReturnsCreatedAndUpdated(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		status     messageimport.Status
		wantStatus int
	}{
		{status: messageimport.StatusCreated, wantStatus: http.StatusCreated},
		{status: messageimport.StatusUpdated, wantStatus: http.StatusOK},
	} {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)

			store := &fakeMessageImportStore{
				mockStore: &mockStore{stats: &StoreStats{}},
				result: messageimport.Result{
					Status: tt.status, SourceID: 3, MessageID: 902, SourceMessageID: "entry-42",
				},
			}
			srv := newMessageImportTestServer(t, store)
			req := messageImportRequest(t.Context(), validMessageImportBody)
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(tt.wantStatus, resp.Code, "body: %s", resp.Body.String())
			assert.Equal(1, store.calls)
			assert.Equal("feed_item", store.req.Message.MessageType)
			var body MessageImportResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(MessageImportResponse{
				Status: tt.status, SourceID: 3, MessageID: 902, SourceMessageID: "entry-42",
			}, body)
		})
	}
}

func TestMessageImportRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	store := &fakeMessageImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMessageImportTestServer(t, store)

	for _, tt := range []struct {
		name        string
		body        string
		mediaType   string
		wantCode    int
		wantError   string
		wantMessage string
	}{
		{name: "malformed", body: `{"source":`, wantCode: http.StatusBadRequest, wantError: "bad_request"},
		{name: "trailing", body: validMessageImportBody + `{}`, wantCode: http.StatusBadRequest, wantError: "bad_request"},
		{
			name:     "unknown field",
			body:     strings.Replace(validMessageImportBody, `"external_id": "entry-42"`, `"unknown": true, "external_id": "entry-42"`, 1),
			wantCode: http.StatusBadRequest, wantError: "bad_request",
		},
		{
			name:     "two bodies",
			body:     strings.Replace(validMessageImportBody, `"body_markdown"`, `"body_text": "Faster sync.", "body_markdown"`, 1),
			wantCode: http.StatusUnprocessableEntity, wantError: "validation_failed", wantMessage: "mutually exclusive",
		},
		{
			name:     "malformed type",
			body:     strings.Replace(validMessageImportBody, `"feed_item"`, `"Feed Item"`, 1),
			wantCode: http.StatusUnprocessableEntity, wantError: "validation_failed", wantMessage: "message.message_type",
		},
		{
			name:     "sender phone names the field, not the value",
			body:     strings.Replace(validMessageImportBody, `"name": "Example Project"`, `"phone": "(604) 555-0100"`, 1),
			wantCode: http.StatusUnprocessableEntity, wantError: "validation_failed", wantMessage: "message.sender.phone",
		},
		{
			name:      "wrong media type",
			body:      validMessageImportBody,
			mediaType: "text/plain",
			wantCode:  http.StatusUnsupportedMediaType, wantError: "unsupported_media_type",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			req := messageImportRequest(t.Context(), tt.body)
			if tt.mediaType != "" {
				req.Header.Set("Content-Type", tt.mediaType)
			}
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(tt.wantCode, resp.Code, "body: %s", resp.Body.String())
			body := decodeErrorResponse(t, resp)
			assert.Equal(tt.wantError, body.Error)
			assert.Contains(body.Message, tt.wantMessage)
			assert.NotContains(body.Message, "555")
		})
	}
	assert.Equal(t, 0, store.calls)
}

func TestMessageImportRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	store := &fakeMessageImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMessageImportTestServer(t, store)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, messageImportRequest(t.Context(), strings.Repeat("x", int(messageimport.MaxRequestBytes)+1)))

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.Code, "body: %s", resp.Body.String())
	assert.Equal(t, "request_too_large", decodeErrorResponse(t, resp).Error)
	assert.Equal(t, 0, store.calls)
}

func TestMessageImportReturnsUnavailableWithoutCapability(t *testing.T) {
	t.Parallel()
	srv := newMessageImportTestServer(t, nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, messageImportRequest(t.Context(), validMessageImportBody))

	require.Equal(t, http.StatusServiceUnavailable, resp.Code, "body: %s", resp.Body.String())
	assert.Equal(t, "service_unavailable", decodeErrorResponse(t, resp).Error)
}

func TestMessageImportSanitizesInternalErrors(t *testing.T) {
	t.Parallel()
	store := &fakeMessageImportStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		err:       errors.New("database failed near secret body content"),
	}
	srv := newMessageImportTestServer(t, store)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, messageImportRequest(t.Context(), validMessageImportBody))

	require.Equal(t, http.StatusInternalServerError, resp.Code, "body: %s", resp.Body.String())
	body := decodeErrorResponse(t, resp)
	assert.Equal(t, "internal_error", body.Error)
	assert.NotContains(t, body.Message, "secret body content")
}

// The published schema carries the rules a client generated from it relies
// on, so it must accept and reject the same requests the server does.
func TestMessageImportPublishedSchemaMatchesValidation(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	assertions := assert.New(t)

	raw, err := OpenAPIJSONVersion("3.1")
	requirements.NoError(err)
	var doc struct {
		Paths map[string]struct {
			Post struct {
				OperationID string                     `json:"operationId"`
				Security    []map[string]any           `json:"security"`
				Responses   map[string]json.RawMessage `json:"responses"`
			} `json:"post"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	requirements.NoError(json.Unmarshal(raw, &doc))
	op := doc.Paths["/api/v1/import/message"].Post
	assertions.Equal("importMessage", op.OperationID)
	assertions.NotEmpty(op.Security)
	assertions.Contains(op.Responses, "200")
	assertions.Contains(op.Responses, "201")

	defs, err := json.Marshal(doc.Components.Schemas)
	requirements.NoError(err)
	schemaJSON := `{"$schema": "https://json-schema.org/draft/2020-12/schema", ` +
		`"$ref": "#/$defs/MessageImportRequest", "$defs": ` +
		strings.ReplaceAll(string(defs), "#/components/schemas/", "#/$defs/") + `}`
	var schema jsonschema.Schema
	requirements.NoError(json.Unmarshal([]byte(schemaJSON), &schema))
	resolved, err := schema.Resolve(nil)
	requirements.NoError(err)

	server := newMessageImportTestServer(t, &fakeMessageImportStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		result:    messageimport.Result{Status: messageimport.StatusCreated},
	})
	for _, tt := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "complete", body: validMessageImportBody, valid: true},
		{
			name: "title only",
			body: strings.Replace(validMessageImportBody, `"body_markdown": "## Changes\n\n- Faster sync.",`, ``, 1), valid: true,
		},
		{
			name: "two bodies",
			body: strings.Replace(validMessageImportBody, `"body_markdown"`, `"body_html": "<p>x</p>", "body_markdown"`, 1),
		},
		{
			name: "neither title nor body",
			body: strings.Replace(strings.Replace(validMessageImportBody,
				`"body_markdown": "## Changes\n\n- Faster sync.",`, ``, 1),
				`"title": "Example Project 1.2.0",`, ``, 1),
		},
		{
			name: "empty sender",
			body: strings.Replace(validMessageImportBody, `{"name": "Example Project", "url": "https://example.com"}`, `{}`, 1),
		},
		{
			name: "malformed type",
			body: strings.Replace(validMessageImportBody, `"feed_item"`, `"feed-item"`, 1),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var instance map[string]any
			require.NoError(json.Unmarshal([]byte(tt.body), &instance))
			schemaErr := resolved.Validate(instance)

			resp := httptest.NewRecorder()
			server.Router().ServeHTTP(resp, messageImportRequest(t.Context(), tt.body))
			if tt.valid {
				require.NoError(schemaErr, "schema")
				assert.Equal(http.StatusCreated, resp.Code, "server: %s", resp.Body.String())
			} else {
				require.Error(schemaErr, "schema")
				assert.Equal(http.StatusUnprocessableEntity, resp.Code, "server: %s", resp.Body.String())
			}
		})
	}
}
