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
	for _, version := range []string{"", "3.27.0", "3.28.0", "3.29.0", "3.30.0", "3.34.0", "3.35.0", "5.0.0", "5.1.0"} {
		t.Run(version, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

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
			_testRequire.NoError(err)
			err = client.RunCLICommand(t.Context(), CLIRunRequest{Args: []string{"sync-x", "--source-id=1", "--json"}}, nil)
			if version == "5.1.0" {
				_testRequire.NoError(err)
				_testAssert.Equal(int32(1), calls.Load())
			} else {
				_testRequire.ErrorContains(err, "x_ingestion")
				_testAssert.Zero(calls.Load())
			}
		})
	}
}
