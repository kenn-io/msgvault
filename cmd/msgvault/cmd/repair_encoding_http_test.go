package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairEncodingUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		assert.Equal(t, "/api/v1/cli/repair-encoding", r.URL.Path, "path")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Scanning messages for invalid UTF-8...\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"repair warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	configureRemoteSyncTest(t, server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: repairEncodingCmd.Use, Args: repairEncodingCmd.Args, RunE: repairEncodingCmd.RunE}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "repair-encoding command")

	assert.Equal(t, int32(1), requests.Load(), "HTTP requests")
	assert.Equal(t, "Scanning messages for invalid UTF-8...\n", stdout.String())
	assert.Equal(t, "repair warning\n", stderr.String())
}
