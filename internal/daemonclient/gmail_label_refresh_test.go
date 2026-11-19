package daemonclient

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCLISyncLabelRefreshCapability(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name    string
		version string
		refresh bool
		full    bool
		wantErr string
	}{
		{name: "ordinary old daemon", version: "5.8.0", full: true},
		{name: "refresh old daemon", version: "5.8.0", full: true, refresh: true, wantErr: "requires daemon API schema 5.20.0"},
		{name: "refresh previous daemon", version: "5.19.0", full: true, refresh: true, wantErr: "requires daemon API schema 5.20.0"},
		{name: "refresh unknown daemon", full: true, refresh: true, wantErr: "requires daemon API schema 5.20.0"},
		{name: "refresh supported daemon", version: "5.20.0", full: true, refresh: true},
		{name: "incremental rejected", version: "5.20.0", refresh: true, wantErr: "requires a full sync"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			var syncCalls, healthCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					healthCalls.Add(1)
					writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": scenario.version})
					return
				}
				syncCalls.Add(1)
				assert.Equal(t, "/api/v1/cli/sync-full", r.URL.Path)
				assert.Equal(t, "reader@example.test", r.URL.Query().Get("email"))
				assert.Equal(t, scenario.refresh, r.URL.Query().Get("refresh_labels") == "true")
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte("{\"type\":\"complete\"}\n"))
			}))
			t.Cleanup(srv.Close)
			client := newTestStore(srv, "")
			err := client.RunCLISync(t.Context(), CLISyncRequest{
				Full: scenario.full, RefreshLabels: scenario.refresh, Email: "reader@example.test",
			}, nil)
			if scenario.wantErr != "" {
				require.ErrorContains(t, err, scenario.wantErr)
				assert.Zero(t, syncCalls.Load(), "unsupported repair never silently becomes ordinary acquisition")
			} else {
				require.NoError(t, err)
				assert.Equal(t, int32(1), syncCalls.Load())
			}
			if !scenario.refresh {
				assert.Zero(t, healthCalls.Load())
			}
		})
	}
}
