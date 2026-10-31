package daemonclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXIngestionRequiresCompletedDaemonVersion(t *testing.T) {
	for _, version := range []string{"", "3.27.0", "3.28.0", "3.29.0"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					return
				}
				assert.Equal(t, "/api/v1/cli/run", r.URL.Path)
				calls.Add(1)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = fmt.Fprintln(w, `{"type":"complete"}`)
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "example-owner-key", AllowInsecure: true})
			require.NoError(t, err)
			err = client.RunCLICommand(t.Context(), CLIRunRequest{Args: []string{"sync-x", "--source-id=1", "--json"}}, nil)
			if version == "3.29.0" {
				require.NoError(t, err)
				assert.Equal(t, int32(1), calls.Load())
			} else {
				require.ErrorContains(t, err, "x_ingestion")
				assert.Zero(t, calls.Load())
			}
		})
	}
}
