package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/testutil"
)

const messageImportE2EBody = `{
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
    "body_markdown": "## Changes\n\n- Faster widgetsync."
  }
}`

func newMessageImportE2EServer(t *testing.T) *api.Server {
	t.Helper()
	st := testutil.NewTestStore(t)
	adapter := &storeAPIAdapter{
		store: st,
		messageImporter: messageimport.NewImporter(st, messageimport.Hooks{
			AfterSourceSetup: func() error { return nil },
			RefreshCache:     func(context.Context, string) error { return nil },
		}),
	}
	return api.NewServer(&config.Config{Server: config.ServerConfig{}}, adapter, nil, slog.New(slog.DiscardHandler))
}

func postMessageImport(t *testing.T, srv *api.Server, body string, wantCode int) api.MessageImportResponse {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/message", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, wantCode, resp.Code, "body: %s", resp.Body.String())
	var decoded api.MessageImportResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	return decoded
}

func getJSON(t *testing.T, srv *api.Server, path string, into any) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, "GET %s: %s", path, resp.Body.String())
	require.NoError(t, json.NewDecoder(resp.Body).Decode(into))
}

func TestMessageImportAPIToStoreArchivesAndUpdatesOneMessage(t *testing.T) {
	assert := assert.New(t)
	srv := newMessageImportE2EServer(t)

	created := postMessageImport(t, srv, messageImportE2EBody, http.StatusCreated)
	assert.Equal(messageimport.StatusCreated, created.Status)
	assert.Equal("entry-42", created.SourceMessageID)

	var initial api.MessageDetail
	getJSON(t, srv, fmt.Sprintf("/api/v1/messages/%d", created.MessageID), &initial)
	assert.Equal("Example Project 1.2.0", initial.Subject)
	assert.Equal("feed_item", initial.MessageType)
	assert.Equal("Example Project releases", initial.ConversationTitle)
	assert.Contains(initial.Body, "Faster widgetsync.")
	assert.Contains(initial.Body, "https://example.com/releases/1.2.0")

	var found api.SearchResult
	getJSON(t, srv, "/api/v1/search?"+url.Values{"q": {"widgetsync"}, "message_type": {"feed_item"}}.Encode(), &found)
	require.Len(t, found.Messages, 1)
	assert.Equal(created.MessageID, found.Messages[0].ID)

	retried := postMessageImport(t, srv, messageImportE2EBody, http.StatusOK)
	assert.Equal(messageimport.StatusUpdated, retried.Status)
	assert.Equal(created.MessageID, retried.MessageID)

	replacement := strings.ReplaceAll(messageImportE2EBody, "1.2.0", "1.2.1")
	replacement = strings.ReplaceAll(replacement, "Faster widgetsync.", "Fixed the gizmocrash.")
	updated := postMessageImport(t, srv, replacement, http.StatusOK)
	assert.Equal(messageimport.StatusUpdated, updated.Status)
	assert.Equal(created.MessageID, updated.MessageID)

	var current api.MessageDetail
	getJSON(t, srv, fmt.Sprintf("/api/v1/messages/%d", updated.MessageID), &current)
	assert.Equal("Example Project 1.2.1", current.Subject)
	assert.Contains(current.Body, "Fixed the gizmocrash.")
	assert.NotContains(current.Body, "widgetsync")

	var stale api.SearchResult
	getJSON(t, srv, "/api/v1/search?"+url.Values{"q": {"widgetsync"}}.Encode(), &stale)
	assert.Empty(stale.Messages, "the replaced body is no longer searchable")
}
