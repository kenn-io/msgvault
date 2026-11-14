package daemonclient_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestArchiveMessageClientClassifiesWriteAndPreviewFailures(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/messages/7/archive", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	archived := true
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived})
	var failure *emailtags.MessageTagError
	requirementsForTest.ErrorAs(err, &failure)
	assertionsForTest.Equal("remote_unknown", failure.Code)
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
	requirementsForTest.Error(err)
	assertionsForTest.NotErrorAs(err, &failure, "preview failures must not claim a provider write happened")
}

func TestBeeperArchiveClientVersionAndPayload(t *testing.T) {
	for _, version := range []string{"5.1.0", "5.2.0", "5.3.0"} {
		t.Run(version, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, err := fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					assertions.NoError(err)
					return
				}
				posts++
				var payload map[string]bool
				if !assertions.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				assertions.True(payload["whole_chat"])
				assertions.False(payload["archived"])
				_, err := w.Write([]byte(`{"message_id":7,"chat_id":"fixture-chat","archived":false,"verified":true}`))
				assertions.NoError(err)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(client.Close()) })
			desired := false
			result, err := client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &desired, WholeChat: true})
			if version != "5.3.0" {
				requirements.Error(err)
				assertions.Zero(posts)
				return
			}
			requirements.NoError(err)
			assertions.Equal("fixture-chat", result.ChatID)
			assertions.Equal(1, posts)
		})
	}
}
