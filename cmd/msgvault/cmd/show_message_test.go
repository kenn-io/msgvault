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

func TestShowMessageUsesLocalDaemonHTTPAndPreservesTextOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, messageRequests := messageHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedJSON := showMessageJSON
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		showMessageJSON = savedJSON
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	useLocal = true
	showMessageJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "show-message", RunE: showMessageCmd.RunE, Args: showMessageCmd.Args}
	cmd.SetArgs([]string{"remote-42"})

	err := cmd.Execute()
	out := done()
	require.NoError(err, "show-message")

	assert.Equal(1, int(messageRequests.Load()), "message endpoint calls")
	assert.Contains(out, "Message ID: 42 (Gmail: remote-42)", "message id")
	assert.Contains(out, "From:    Alice <alice@example.com>", "from")
	assert.Contains(out, "To:      Bob <bob@example.com>", "to")
	assert.Contains(out, "Subject: Test Subject", "subject")
	assert.Contains(out, "Hello over HTTP", "body")
}

func TestShowMessageHTTPNotFoundPreservesCLIError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := messageHTTPNotFoundDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedJSON := showMessageJSON
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		showMessageJSON = savedJSON
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	useLocal = true
	showMessageJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "show-message", RunE: showMessageCmd.RunE, Args: showMessageCmd.Args}
	cmd.SetArgs([]string{"missing"})

	err := cmd.Execute()
	out := done()
	require.Error(err, "show-message")

	assert.Empty(out, "stdout")
	require.ErrorContains(err, "message not found: missing", "not found error")
	assert.NotContains(err.Error(), "API error", "transport details")
}

func messageHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	registerStatsProbeHandler(mux)
	mux.HandleFunc("/api/v1/cli/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("id") != "remote-42" {
			http.Error(w, "wrong id", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 42,
			"source_message_id": "remote-42",
			"conversation_id": 7,
			"subject": "Test Subject",
			"snippet": "short",
			"sent_at": "2024-01-02T03:04:05Z",
			"size_estimate": 512,
			"has_attachments": false,
			"from": [{"email": "alice@example.com", "name": "Alice"}],
			"to": [{"email": "bob@example.com", "name": "Bob"}],
			"labels": ["INBOX"],
			"attachments": [],
			"body_text": "Hello over HTTP"
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func messageHTTPNotFoundDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	registerStatsProbeHandler(mux)
	mux.HandleFunc("/api/v1/cli/message", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Message not found"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
