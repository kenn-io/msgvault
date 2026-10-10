package cmd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingStatusCommandChecksDaemonCapability(t *testing.T) {
	oldJSON, oldWatch, oldSource, oldInterval := embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource, embeddingsStatusInterval
	t.Cleanup(func() {
		embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource, embeddingsStatusInterval = oldJSON, oldWatch, oldSource, oldInterval
	})
	embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource, embeddingsStatusInterval = true, false, 0, 5*time.Second
	for _, version := range []string{"3.10.0", "3.11.0"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var statusRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
				case "/api/v1/embeddings/status":
					statusRequests.Add(1)
					if version != "3.11.0" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(`{"generation":{"id":1,"state":"building"},"pending":4}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			command := &cobra.Command{}
			command.SetContext(withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
			}))
			var output bytes.Buffer
			command.SetOut(&output)
			err := embeddingsStatusCmd.RunE(command, nil)
			if version != "3.11.0" {
				require.Error(err)
				require.ErrorContains(err, "requires daemon API schema 3.11.0")
				require.ErrorContains(err, fmt.Sprintf("daemon reports %q", version))
				require.ErrorContains(err, "upgrade")
				assert.Zero(statusRequests.Load(), "reject incompatible daemons before requesting the new route")
				assert.Empty(output.String())
			} else {
				require.NoError(err)
				assert.Equal(int64(1), statusRequests.Load())
				assert.Contains(output.String(), `"pending":4`)
			}
		})
	}
}

func TestEmbeddingStatusOutputExplainsProviderAndWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var out bytes.Buffer
	status := vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{ID: 4, State: "building"}, Eligible: 10, Current: 3, Pending: 7, Job: vector.EmbeddingJobState{State: "running", Phase: "provider", HoldingSchedulerSlot: true}, Diagnostics: &vector.EmbeddingDiagnostics{RecentBatches: []vector.EmbeddingBatch{{Sequence: 1, Attempted: 3, Completed: 3, ProviderMS: 2000, DBWriteMS: 15, Requests: 2, Retries: 1, RateLimits: 1}}}}
	require.NoError(writeEmbeddingStatus(&out, status, false))
	assert.Contains(out.String(), "Current: 3/10")
	assert.Contains(out.String(), "ETA: unknown")
	assert.Contains(out.String(), "provider")
	assert.Contains(out.String(), "2000")
	out.Reset()
	require.NoError(writeEmbeddingStatus(&out, status, true))
	assert.Contains(out.String(), `"eta_seconds":null`)
	assert.Contains(out.String(), `"rate_limits":1`)
	out.Reset()
	status.Diagnostics = nil
	status.Failed = 2
	require.NoError(writeEmbeddingStatus(&out, status, false))
	assert.Contains(out.String(), "Latest message-embedding pass failures: 2 (all sources)")
}

func TestEmbeddingStatusRejectsExplicitZeroSource(t *testing.T) {
	oldSource := embeddingsStatusSource
	t.Cleanup(func() { embeddingsStatusSource = oldSource })
	embeddingsStatusSource = 0
	command := &cobra.Command{}
	command.Flags().Int64("source", 0, "Source ID")
	require.NoError(t, command.Flags().Set("source", "0"))
	require.ErrorContains(t, embeddingsStatusCmd.RunE(command, nil), "--source must be positive")
}

func TestEmbeddingStatusOutputBoundsLargeETA(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{seconds: 90, want: "ETA: 1m30s"},
		{seconds: 3.6e10, want: "ETA: more than 10000h0m0s"},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		require.NoError(t, writeEmbeddingStatus(&out, vector.EmbeddingStatus{ETASeconds: &tt.seconds}, false))
		assert.Contains(t, out.String(), tt.want)
	}
}
