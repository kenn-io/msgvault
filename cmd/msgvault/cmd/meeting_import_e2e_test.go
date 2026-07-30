package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

const meetingImportE2EBody = `{
  "source": {
    "identifier": "local-meetings",
    "display_name": "Local Meetings",
    "account_email": "user@example.com"
  },
  "meeting": {
    "external_id": "42",
    "title": "Weekly planning",
    "started_at": "2026-07-23T18:00:00Z",
    "summary_text": "Initial summary.",
    "transcript": "Speaker 1: initial transcript",
    "organizer": {"name": "Test Organizer", "email": "organizer@example.com"},
    "attendees": [{"name": "Test Attendee", "email": "attendee@example.com"}]
  }
}`

func postMeetingImport(
	t *testing.T,
	srv *api.Server,
	body string,
) api.MeetingImportResponse {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/meeting",
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.Code, "body: %s", resp.Body.String())
	var decoded api.MeetingImportResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	return decoded
}

func TestMeetingImportAPIToStoreUpdatesCanonicalMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	adapter := &storeAPIAdapter{
		store: st,
		meetingImporter: meetingimport.NewImporter(st, meetingimport.Hooks{
			AfterSourceSetup: func() error { return nil },
			RefreshCache:     func(_ context.Context, _ string) error { return nil },
		}),
	}
	srv := api.NewServer(
		&config.Config{Server: config.ServerConfig{}},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	created := postMeetingImport(t, srv, meetingImportE2EBody)
	assert.Equal(meetingimport.StatusCreated, created.Status)

	getReq := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/v1/messages/%d", created.MessageID),
		nil,
	)
	getResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getResp, getReq)
	require.Equal(http.StatusOK, getResp.Code, "body: %s", getResp.Body.String())
	var initial api.MessageDetail
	require.NoError(json.NewDecoder(getResp.Body).Decode(&initial))
	assert.Equal("Weekly planning", initial.Subject)
	assert.Contains(initial.Body, "Initial summary.")
	assert.Contains(initial.Body, "Speaker 1: initial transcript")
	assert.Contains(initial.To, "Test Attendee <attendee@example.com>")

	replacement := strings.ReplaceAll(meetingImportE2EBody, "Weekly planning", "Replacement title")
	replacement = strings.ReplaceAll(replacement, "Initial summary.", "Replacement summary.")
	replacement = strings.ReplaceAll(replacement, "Speaker 1: initial transcript", "Speaker 2: replacement transcript")
	replacement = strings.Replace(
		replacement,
		`"attendees": [{"name": "Test Attendee", "email": "attendee@example.com"}]`,
		`"attendees": []`,
		1,
	)
	updated := postMeetingImport(t, srv, replacement)
	assert.Equal(meetingimport.StatusUpdated, updated.Status)
	assert.Equal(created.MessageID, updated.MessageID)

	getReq = httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/v1/messages/%d", updated.MessageID),
		nil,
	)
	getResp = httptest.NewRecorder()
	srv.Router().ServeHTTP(getResp, getReq)
	require.Equal(http.StatusOK, getResp.Code, "body: %s", getResp.Body.String())
	var current api.MessageDetail
	require.NoError(json.NewDecoder(getResp.Body).Decode(&current))
	assert.Equal("Replacement title", current.Subject)
	assert.Contains(current.Body, "Replacement summary.")
	assert.Contains(current.Body, "Speaker 2: replacement transcript")
	assert.Empty(current.To)
}
