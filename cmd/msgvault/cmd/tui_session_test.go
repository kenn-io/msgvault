package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
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
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				requests := 0
				release := make(chan struct{})
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					assert.Equal("/api/v1/telemetry/events", r.URL.Path)
					assert.Equal("selected-daemon-key", r.Header.Get("X-Api-Key"))
					var body map[string]any
					assert.NoError(json.UnmarshalRead(r.Body, &body))
					assert.Equal(map[string]any{"event": "session_ended", "properties": map[string]any{"surface": "tui", "duration_bucket": "1_to_5m"}}, body)
					if tc.timeout {
						<-release
					} else {
						time.Sleep(5 * time.Millisecond)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued"}`))
				}))
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				var exitStart time.Time
				root := &cobra.Command{Use: "tui", RunE: func(cmd *cobra.Command, _ []string) error {
					ctx, cancel := context.WithCancel(cmd.Context())
					cfg := &config.Config{}
					cfg.Remote.APIKey = "selected-daemon-key"
					cfg.Remote.AllowInsecure = true
					cancel()
					require.ErrorIs(ctx.Err(), context.Canceled)
					reportTUISession(ctx, cfg, HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, 2*time.Minute)
					exitStart = time.Now()
					return nil
				}}
				root.SetArgs([]string{})
				require.NoError(executeRootContext(context.Background(), root))
				synctest.Wait()
				want := 5 * time.Millisecond
				if tc.timeout {
					want = 50 * time.Millisecond
				}
				assert.Equal(want, time.Since(exitStart))
				assert.Equal(1, requests)
				close(release)
				invocationFromCommand(root).sends.Wait(time.Second)
			})
		})
	}
}
