package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The client suite exercises the real daemon. This independent remote fixture
// pins the executable's exit status and the complete refused-ranking envelope.
func TestLoopWaitingStaleExitStatus(t *testing.T) {
	assertions := assert.New(t)
	const stale = `{"error":"loops_stale","message":"Archive freshness requires attention","freshness":{"as_of":"2026-10-17T12:00:00Z","stale_after_seconds":86400,"stale":true,"stale_reasons":["source_never_synced:owner@example.test"],"warnings":[],"sources":[{"source_id":null,"source_type":"gmail","identifier":"owner@example.test","scheduling":"scheduled","last_successful_sync_at":null,"last_sync_errors":0,"outcome":"never","age_seconds":null,"stale":true,"sync_command":"msgvault sync-incremental owner@example.test"}],"projections":{}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = io.WriteString(w, `{"status":"ok","api_schema_version":"3.25.0"}`)
		case "/api/v1/loops/waiting":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, stale)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\nurl = \""+server.URL+"\"\nallow_insecure = true\n"), 0o600))
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"msgvault", "--home", home, "--config", configPath, "loops", "waiting", "--json"}
	stdout, stderr, code := captureLoopMain(t, run)
	assertions.Equal(2, code)
	assertions.JSONEq(stale, stdout)
	assertions.Contains(stderr, "msgvault sync-incremental owner@example.test")
}

func captureLoopMain(t *testing.T, runCommand func() int) (string, string, int) {
	t.Helper()
	outReader, outWriter, err := os.Pipe()
	require.NoError(t, err)
	errReader, errWriter, err := os.Pipe()
	require.NoError(t, err)
	oldOut, oldErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	os.Stdout, os.Stderr = outWriter, errWriter
	var stdout, stderr bytes.Buffer
	outDone, errDone := make(chan struct{}), make(chan struct{})
	go func() { _, _ = io.Copy(&stdout, outReader); close(outDone) }()
	go func() { _, _ = io.Copy(&stderr, errReader); close(errDone) }()
	code := runCommand()
	_ = outWriter.Close()
	_ = errWriter.Close()
	<-outDone
	<-errDone
	_ = outReader.Close()
	_ = errReader.Close()
	return stdout.String(), stderr.String(), code
}
