package daemonclient_test

import (
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Exercise both production detail handlers and both client conversion layers.
// HTML without stored plain text and identical alternatives cannot be distinguished from legacy body.
func TestMessageBodiesThroughDaemon(t *testing.T) {
	for _, withEngine := range []bool{false, true} {
		t.Run(map[bool]string{false: "store", true: "engine"}[withEngine], func(t *testing.T) {
			setupAssert := assert.New(t)
			setupRequire := require.New(t)
			fixture := storetest.New(t)
			st := fixture.Store
			opts := api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}
			if withEngine {
				engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
				t.Cleanup(func() { setupAssert.NoError(engine.Close()) })
				opts.Engine = engine
			}
			server := httptest.NewServer(api.NewServerWithOptions(opts).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			setupRequire.NoError(err)
			t.Cleanup(func() { setupAssert.NoError(client.Close()) })
			for _, tc := range []struct{ name, text, html string }{
				{"multipart", "Plain alternative", "<p>HTML alternative</p>"},
				{"html_without_stored_text", "", "<p>Only HTML</p>"},
				{"identical", "<p>Same bytes</p>", "<p>Same bytes</p>"},
				{"empty", "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					id, err := st.PersistMessage(&store.MessagePersistData{
						Message:  &store.Message{SourceID: fixture.Source.ID, ConversationID: fixture.ConvID, SourceMessageID: tc.name, MessageType: "email"},
						BodyText: sql.NullString{String: tc.text, Valid: true},
						BodyHTML: sql.NullString{String: tc.html, Valid: true},
					})
					require.NoError(err)
					detail, err := daemonclient.NewEngineAdapter(client).GetMessage(t.Context(), id)
					require.NoError(err)
					assert.Equal(tc.text, detail.BodyText)
					assert.Equal(tc.html, detail.BodyHTML)
				})
			}
		})
	}
}

// Older daemons have no canonical field. Keep their documented legacy body
// rather than guessing that equal HTML and text means there was no text.
func TestMessageBodiesFromOlderDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.MarshalWrite(w, map[string]any{
			"id": 42, "body": "<p>legacy HTML</p>", "body_html": "<p>legacy HTML</p>",
			"from": "sender@example.com", "to": []string{}, "subject": "Legacy",
			"sent_at": "2026-01-01T00:00:00Z", "snippet": "", "labels": []string{},
			"has_attachments": false, "hidden_text_present": false, "size_bytes": 0,
			"attachments": []any{},
		}))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	detail, err := daemonclient.NewEngineAdapter(client).GetMessage(t.Context(), 42)
	require.NoError(err)
	assert.Equal("<p>legacy HTML</p>", detail.BodyText)
}
