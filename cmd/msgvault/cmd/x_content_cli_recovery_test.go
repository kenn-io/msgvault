package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

// Execute the production exporter in process: the test executable is not the
// msgvault subprocess binary. Other commands use the real daemon dispatcher.
type xContentExportRunner struct {
	*storeAPIAdapter

	beforeEmit func()
}

func (a *xContentExportRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Args[0] != "export-messages" {
		return a.storeAPIAdapter.RunCLICommand(ctx, req, emit)
	}
	command := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) {
		return a.store, func() {}, nil
	}})
	command.SetContext(ctx)
	command.SetArgs(req.Args[1:])
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	if err := command.Execute(); err != nil {
		return fmt.Errorf("execute fixture exporter: %w", err)
	}
	if a.beforeEmit != nil {
		a.beforeEmit()
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: output.String()})
}

func TestXPendingRemovalFencesCLIExportAndAllowsCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXRestoreFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	runner := &xContentExportRunner{storeAPIAdapter: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xCleanup: cleaner}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: runner, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	request := func(args ...string) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(api.CLIRunRequest{Args: args})
		require.NoError(err)
		// Bound a stuck cleanup lease without requiring any polling throughput.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(encoded)).WithContext(ctx)
		r.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
		r.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, r)
		return response
	}
	export := []string{"export-messages", "--start", "2026-10-29T00:00:00Z", "--end", "2026-11-01T00:00:00Z"}
	runner.beforeEmit = func() {
		writer := flock.New(f.cfg.AnalyticsDir() + ".x-content.lock")
		locked, err := writer.TryLock()
		require.NoError(err)
		if locked {
			require.NoError(writer.Unlock())
		}
		require.NoError(writer.Close())
		assert.False(locked, "the read lease must still cover exported bytes awaiting emission")
	}
	response := request(export...)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "synthetic aurora payload")
	runner.beforeEmit = nil
	require.NoError(f.st.QueueXAuthorRemovalContext(t.Context(), f.source.ID, "1002", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)))
	var retained string
	require.NoError(f.st.DB().QueryRow("SELECT body_text FROM message_bodies WHERE message_id=?", f.message).Scan(&retained))
	require.Equal("synthetic aurora payload", retained, "simulate cancellation after suppression is queued but before purge")
	response = request(export...)
	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "x_content_cleanup_pending")
	assert.NotContains(response.Body.String(), retained)
	response = request("sync-x", "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "100", "--json")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(response.Body.String(), `"type":"error"`)
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(err)
	assert.False(pending)
	response = request(export...)
	assert.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(response.Body.String(), retained)
	assert.Zero(f.providerCalls.Load())
}
