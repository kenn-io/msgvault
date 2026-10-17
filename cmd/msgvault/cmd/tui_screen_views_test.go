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

func TestTUIScreenAndActionReportersUseSelectedDaemon(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(json.UnmarshalRead(r.Body, &body))
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "selected-daemon-key", AllowInsecure: true})
	require.NoError(err)
	require.NoError(tuiScreenReporter(client)(context.Background(), "settings"))
	require.NoError(tuiActionReporter(client)(context.Background(), "search_run", "search_type", "hybrid"))
	assert.Equal([]map[string]any{
		{"event": "screen_viewed", "properties": map[string]any{"screen": "settings", "surface": "tui"}},
		{"event": "search_run", "properties": map[string]any{"search_type": "hybrid", "surface": "tui"}},
	}, requests)
}
