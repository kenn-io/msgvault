package cmd

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestTUISessionUsesSelectedDaemonAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout bool
	}{
		{name: "delivery"},
		{name: "timeout", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				assert.Equal(t, "selected-daemon-key", r.Header.Get("X-Api-Key"))
				if tc.timeout {
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
					return
				}
				var body map[string]any
				assert.NoError(t, json.UnmarshalRead(r.Body, &body))
				assert.Equal(t, map[string]any{"event": "session_ended", "properties": map[string]any{"surface": "tui", "duration_bucket": "1_to_5m"}}, body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			cfg := &config.Config{}
			cfg.Remote.APIKey = "selected-daemon-key"
			cfg.Remote.AllowInsecure = true
			cancel()
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			started := time.Now()
			reportTUISession(ctx, cfg, HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, 2*time.Minute)
			if tc.timeout {
				assert.Less(t, time.Since(started), 5*time.Second)
			}
			assert.Equal(t, 1, requests)
		})
	}
}
