package daemonclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestArchiveMessageClientClassifiesWriteAndPreviewFailures(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"5.8.0"}`))
			assert.NoError(t, err)
			return
		}
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/messages/7/archive", r.URL.Path)
		var payload map[string]bool
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if payload["whole_chat"] {
			assertions.Contains(payload, "archived")
			assertions.False(payload["archived"])
			_, err := w.Write([]byte(`{"message_id":7,"chat_id":"fixture-chat","archived":false,"verified":true}`))
			assertions.NoError(err)
			return
		}
		_, _ = w.Write([]byte(`{`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(client.Close()) })
	archived := true
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived})
	var failure *emailtags.MessageTagError
	requirements.ErrorAs(err, &failure)
	assertions.Equal("remote_unknown", failure.Code)
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
	requirements.Error(err)
	assertions.NotErrorAs(err, &failure, "preview failures must not claim a provider write happened")
	desired := false
	result, err := client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &desired, WholeChat: true})
	requirements.NoError(err)
	assertions.Equal("fixture-chat", result.ChatID)
}
