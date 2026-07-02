package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestRebuildFTSUsesLocalDaemonHTTPAndPreservesStderr(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/rebuild-fts", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"progress","done":2,"total":4}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete","indexed":3}` + "\n"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	useLocal = true

	doneErr := captureStderr(t)
	root := newTestRootCmd()
	root.AddCommand(&cobra.Command{
		Use:   rebuildFTSCmd.Use,
		Short: rebuildFTSCmd.Short,
		Long:  rebuildFTSCmd.Long,
		RunE:  rebuildFTSCmd.RunE,
	})
	root.SetArgs([]string{"rebuild-fts"})

	err := root.Execute()
	errOut := doneErr()
	require.NoError(err, "rebuild-fts")

	assert.Equal(1, int(requests.Load()), "rebuild endpoint calls")
	assert.Contains(errOut, "Rebuilding full-text search index...", "start banner")
	assert.Contains(errOut, "[===============               ]  50%", "progress bar")
	assert.Contains(errOut, "[==============================] 100%  3 messages indexed.", "final progress")
}
