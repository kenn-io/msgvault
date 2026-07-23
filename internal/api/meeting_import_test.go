package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingimport"
)

const (
	meetingImportTestAPIKey = "synthetic-api-key-for-tests"
	validMeetingImportBody  = `{
	  "source": {
	    "identifier": "local-meetings",
	    "display_name": "Local Meetings",
	    "account_email": "user@example.com"
	  },
	  "meeting": {
	    "external_id": "42",
	    "title": "Weekly planning",
	    "started_at": "2026-07-23T18:00:00Z",
	    "ended_at": "2026-07-23T18:30:00Z",
	    "summary_markdown": "## Summary\n\nReviewed the launch plan.",
	    "transcript_segments": [
	      {"speaker": "Test Speaker", "text": "Review the launch plan.", "offset_seconds": 4}
	    ],
	    "organizer": {"name": "Test Organizer", "email": "organizer@example.com"},
	    "attendees": [{"name": "Test Attendee", "email": "attendee@example.com"}],
	    "metadata": {"provider_key": "synthetic-value"}
	  }
	}`
)

type fakeMeetingImportStore struct {
	*mockStore

	result meetingimport.Result
	err    error
	calls  int
	req    meetingimport.Request
}

func (s *fakeMeetingImportStore) ImportMeeting(
	_ context.Context,
	req meetingimport.Request,
) (meetingimport.Result, error) {
	s.calls++
	s.req = req
	return s.result, s.err
}

func newMeetingImportTestServer(t *testing.T, importer MeetingImporter) *Server {
	t.Helper()
	base := &mockStore{stats: &StoreStats{}}
	var store MessageStore = base
	if importer != nil {
		typed, ok := importer.(MessageStore)
		require.True(t, ok, "test importer must implement MessageStore")
		store = typed
	}
	return NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}},
		store,
		nil,
		testLogger(),
	)
}

func meetingImportRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/meeting", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	return req
}

func TestMeetingImportRequiresAuthentication(t *testing.T) {
	store := &fakeMeetingImportStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		result:    meetingimport.Result{Status: meetingimport.StatusCreated},
	}
	srv := newMeetingImportTestServer(t, store)

	for _, key := range []string{"", "wrong-key"} {
		req := meetingImportRequest(validMeetingImportBody)
		if key == "" {
			req.Header.Del("X-Api-Key")
		} else {
			req.Header.Set("X-Api-Key", key)
		}
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)

		assert.Equal(t, http.StatusUnauthorized, resp.Code, "key=%q body=%s", key, resp.Body.String())
	}
	assert.Equal(t, 0, store.calls)
}

func TestMeetingImportReturnsCreatedAndUpdated(t *testing.T) {
	tests := []struct {
		name       string
		status     meetingimport.Status
		wantStatus int
	}{
		{name: "created", status: meetingimport.StatusCreated, wantStatus: http.StatusCreated},
		{name: "updated", status: meetingimport.StatusUpdated, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			store := &fakeMeetingImportStore{
				mockStore: &mockStore{stats: &StoreStats{}},
				result: meetingimport.Result{
					Status:          tt.status,
					SourceID:        3,
					MessageID:       901,
					SourceMessageID: "meeting:42",
				},
			}
			srv := newMeetingImportTestServer(t, store)
			req := meetingImportRequest(validMeetingImportBody)
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(tt.wantStatus, resp.Code, "body: %s", resp.Body.String())
			assert.Equal(1, store.calls)
			assert.Equal("42", store.req.Meeting.ExternalID)
			var body MeetingImportResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(tt.status, body.Status)
			assert.Equal(int64(3), body.SourceID)
			assert.Equal(int64(901), body.MessageID)
			assert.Equal("meeting:42", body.SourceMessageID)
		})
	}
}

func TestMeetingImportRejectsInvalidRequests(t *testing.T) {
	store := &fakeMeetingImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMeetingImportTestServer(t, store)

	unknown := strings.Replace(validMeetingImportBody, `"external_id":`, `"unknown": true, "external_id":`, 1)
	noContent := strings.Replace(
		validMeetingImportBody,
		`"summary_markdown": "## Summary\n\nReviewed the launch plan.",`,
		`"summary_markdown": "",`,
		1,
	)
	noContent = strings.Replace(noContent,
		`"transcript_segments": [
	      {"speaker": "Test Speaker", "text": "Review the launch plan.", "offset_seconds": 4}
	    ],`,
		`"transcript_segments": [],`,
		1,
	)

	tests := []struct {
		name      string
		body      string
		mediaType string
		wantCode  int
		wantError string
	}{
		{name: "malformed", body: `{"source":`, mediaType: "application/json", wantCode: http.StatusBadRequest, wantError: "bad_request"},
		{name: "trailing", body: validMeetingImportBody + `{}`, mediaType: "application/json", wantCode: http.StatusBadRequest, wantError: "bad_request"},
		{name: "unknown field", body: unknown, mediaType: "application/json", wantCode: http.StatusBadRequest, wantError: "bad_request"},
		{name: "semantic validation", body: noContent, mediaType: "application/json", wantCode: http.StatusUnprocessableEntity, wantError: "validation_failed"},
		{name: "wrong media type", body: validMeetingImportBody, mediaType: "text/plain", wantCode: http.StatusUnsupportedMediaType, wantError: "unsupported_media_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			req := meetingImportRequest(tt.body)
			req.Header.Set("Content-Type", tt.mediaType)
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(tt.wantCode, resp.Code, "body: %s", resp.Body.String())
			var body ErrorResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(tt.wantError, body.Error)
			assert.NotContains(body.Message, "Review the launch plan")
		})
	}
	assert.Equal(t, 0, store.calls)
}

func TestMeetingImportRejectsOversizedBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := &fakeMeetingImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMeetingImportTestServer(t, store)
	req := meetingImportRequest(strings.Repeat("x", int(meetingimport.MaxRequestBytes)+1))
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusRequestEntityTooLarge, resp.Code, "body: %s", resp.Body.String())
	var body ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal("request_too_large", body.Error)
	assert.Equal(0, store.calls)
}

func TestMeetingImportReturnsUnavailableWithoutCapability(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := newMeetingImportTestServer(t, nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, meetingImportRequest(validMeetingImportBody))

	require.Equal(http.StatusServiceUnavailable, resp.Code, "body: %s", resp.Body.String())
	var body ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal("service_unavailable", body.Error)
}

func TestMeetingImportSanitizesInternalErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := &fakeMeetingImportStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		err:       errors.New("database failed near secret transcript content"),
	}
	srv := newMeetingImportTestServer(t, store)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, meetingImportRequest(validMeetingImportBody))

	require.Equal(http.StatusInternalServerError, resp.Code, "body: %s", resp.Body.String())
	var body ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal("internal_error", body.Error)
	assert.NotContains(body.Message, "secret transcript content")
}

func TestMeetingImportOpenAPIDocument(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := &fakeMeetingImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMeetingImportTestServer(t, store)
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code)
	var doc map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&doc))
	paths, ok := doc["paths"].(map[string]any)
	require.True(ok, "paths object")
	path, ok := paths["/api/v1/import/meeting"].(map[string]any)
	require.True(ok, "meeting import path object")
	post, ok := path["post"].(map[string]any)
	require.True(ok, "meeting import operation object")
	assert.Equal("importMeeting", post["operationId"])
	assert.NotEmpty(post["security"])
	responses, ok := post["responses"].(map[string]any)
	require.True(ok, "responses object")
	assert.Contains(responses, "200")
	assert.Contains(responses, "201")
}

func TestMeetingImportBodyLimitDoesNotReadPastBoundary(t *testing.T) {
	store := &fakeMeetingImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := newMeetingImportTestServer(t, store)
	body := bytes.NewReader(bytes.Repeat([]byte("x"), int(meetingimport.MaxRequestBytes)+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/meeting", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.Code)
}
