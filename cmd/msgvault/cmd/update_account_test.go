package cmd

import (
	"bytes"
	"encoding/json"
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

func TestUpdateAccountUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	requests := &atomic.Int32{}

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	registerStatsProbeHandler(mux)
	mux.HandleFunc("/api/v1/cli/account", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		requests.Add(1)

		var req struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&req), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal("alice@example.com", req.Email, "email")
		assert.Equal("Work", req.DisplayName, "display name")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"alice@example.com","display_name":"Work"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedDisplayName := updateDisplayName
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		updateDisplayName = savedDisplayName
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	updateDisplayName = "Work"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  updateAccountCmd.Use,
		Args: updateAccountCmd.Args,
		RunE: updateAccountCmd.RunE,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"alice@example.com"})

	err := cmd.Execute()
	require.NoError(err, "update-account")

	assert.Equal(1, int(requests.Load()), "update account endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal("Updated account alice@example.com: display name set to \"Work\"\n", stdout.String())
}

func TestUpdateAccountSourceIDUsesTypedDaemonRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	savedDisplayName := updateDisplayName
	savedSourceID := updateAccountSourceID
	t.Cleanup(func() {
		updateDisplayName = savedDisplayName
		updateAccountSourceID = savedSourceID
	})
	requests := &atomic.Int32{}
	dataDir := t.TempDir()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	registerStatsProbeHandler(mux)
	mux.HandleFunc("/api/v1/cli/account", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			SourceID    int64  `json:"source_id"`
			DisplayName string `json:"display_name"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal(int64(42), req.SourceID)
		assert.Equal("Work", req.DisplayName)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"alice@example.com","display_name":"Work"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: server.URL},
	}
	useLocal = true

	cmd := newUpdateAccountCmd()
	cmd.SetArgs([]string{"--source-id", "42", "--display-name", "Work"})
	require.NoError(cmd.Execute())
	assert.Equal(int32(1), requests.Load())
}

func TestUpdateAccountSelectorValidation(t *testing.T) {
	savedDisplayName := updateDisplayName
	savedSourceID := updateAccountSourceID
	t.Cleanup(func() {
		updateDisplayName = savedDisplayName
		updateAccountSourceID = savedSourceID
	})

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "explicit zero", args: []string{"--source-id", "0", "--display-name", "Work"}, want: "positive"},
		{name: "token plus ID", args: []string{"alice@example.test", "--source-id", "42", "--display-name", "Work"}, want: "mutually exclusive"},
		{name: "missing selector", args: []string{"--display-name", "Work"}, want: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newUpdateAccountCmd()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
