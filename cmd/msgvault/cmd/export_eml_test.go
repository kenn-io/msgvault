package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestExportEMLUsesLocalDaemonHTTPAndPreservesFileOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	server, rawRequests := emlHTTPDaemon(t, raw)
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

	outputPath := filepath.Join(dataDir, "message.eml")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)

	err := runExportEML(cmd, "gmail-raw", outputPath)
	require.NoError(err, "export-eml")

	got, err := os.ReadFile(outputPath)
	require.NoError(err, "read output")
	assert.Equal(raw, got, "raw MIME")
	assert.Equal(1, int(rawRequests.Load()), "raw endpoint calls")
	assert.Contains(out.String(), "Exported message to: "+outputPath, "stdout")
	assert.Contains(out.String(), "("+strconv.Itoa(len(raw))+" bytes)", "stdout size")
}

func TestExportEMLHTTPNotFoundPreservesCLIError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := emlHTTPNotFoundDaemon(t)
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

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)

	err := runExportEML(cmd, "missing", filepath.Join(dataDir, "missing.eml"))
	require.Error(err, "export-eml")

	assert.Empty(out.String(), "stdout")
	require.ErrorContains(err, "message not found: missing", "not found error")
	assert.NotContains(err.Error(), "API error", "transport details")
}

func emlHTTPDaemon(t *testing.T, raw []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("id") != "gmail-raw" {
			http.Error(w, "wrong id", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("X-Msgvault-Source-Message-Id", "gmail-raw")
		_, _ = w.Write(raw)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func emlHTTPNotFoundDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message/raw", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Message not found"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
