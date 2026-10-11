package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestTUIScreenReporterUsesSelectedDaemon(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, map[string]any{"event": "screen_viewed", "properties": map[string]any{"screen": "settings", "surface": "tui"}}, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "selected-daemon-key", AllowInsecure: true})
	require.NoError(t, err)
	require.NoError(t, tuiScreenReporter(client)(context.Background(), "settings"))
	assert.Equal(t, 1, requests)
}
