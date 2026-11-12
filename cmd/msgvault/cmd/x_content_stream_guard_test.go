package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
)

// The test executable is not the msgvault subprocess binary. Run the real
// logs command in-process while keeping the production HTTP runner path.
type xLogsRunner struct{ *storeAPIAdapter }

func (a *xLogsRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if len(req.Args) == 0 || req.Args[0] != "logs" {
		return a.storeAPIAdapter.RunCLICommand(ctx, req, emit)
	}
	cmd := newLogsRoutingTestCommand()
	cmd.SetContext(withInvocation(ctx, &invocation{cfg: a.config}))
	cmd.SetArgs(req.Args[1:])
	cmd.SetOut(optimizeStreamWriter(func(p []byte) (int, error) {
		if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(p)}); err != nil {
			return 0, err
		}
		return len(p), nil
	}))
	if err := cmd.Execute(); err != nil {
		return fmt.Errorf("run logs command: %w", err)
	}
	return nil
}

type pausedMCPWriter struct {
	*httptest.ResponseRecorder

	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	captured []byte
}

type logStreamRecorder struct {
	*httptest.ResponseRecorder

	wrote chan struct{}
}

func (w *logStreamRecorder) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	if err != nil {
		return n, fmt.Errorf("record log response: %w", err)
	}
	return n, nil
}

func (w *pausedMCPWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { w.captured = bytes.Clone(p); close(w.entered); <-w.release })
	n, err := w.ResponseRecorder.Write(p)
	if err != nil {
		return n, fmt.Errorf("record MCP response: %w", err)
	}
	return n, nil
}

func TestXCleanupProceedsDuringLogsFollow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetLogsRoutingGlobals(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	f := newXRestoreFixture(t)
	require.NoError(os.MkdirAll(f.cfg.LogsDir(), 0o700))
	require.NoError(os.WriteFile(filepath.Join(f.cfg.Data.DataDir, "serve.log"), []byte("synthetic log line\n"), 0o600))
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	runner := &xLogsRunner{&storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xCleanup: cleaner}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: runner, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	t.Cleanup(func() {
		// Shutdown must finish after the test context is canceled.
		require.NoError(server.Shutdown(context.WithoutCancel(t.Context())))
	})
	body, err := json.Marshal(api.CLIRunRequest{Args: []string{"logs", "--follow"}})
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
	req.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
	req.Header.Set("Content-Type", "application/json")
	response := &logStreamRecorder{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); server.Router().ServeHTTP(response, req) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-response.wrote:
	case <-time.After(10 * time.Second):
		require.FailNow("logs stream did not start")
	}
	removeCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	_, err = cleaner.RemovePost(removeCtx, f.source.ID, "100", "removed")
	require.NoError(err)
	select {
	case <-done:
		require.FailNow("logs follow ended before cleanup")
	default:
	}
	cancel()
	<-done
	assert.Contains(response.Body.String(), "synthetic log line")
}

func TestXCleanupWaitsForNativeMCPReadResponse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newXRestoreFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xCleanup: cleaner}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: query.NewEngine(f.st.DB(), false), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	cmd := &cobra.Command{Use: "serve"}
	addServeConfigFlags(cmd)
	require.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: f.cfg}))
	t.Cleanup(func() {
		// Shutdown must finish after the test context is canceled.
		require.NoError(server.Shutdown(context.WithoutCancel(t.Context())))
	})
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_message","arguments":{"id":%d}}}`, f.message)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+f.cfg.Server.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	response := &pausedMCPWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(response.release) }) }
	done := make(chan struct{})
	go func() { defer close(done); server.Router().ServeHTTP(response, req) }()
	t.Cleanup(func() { unblock(); <-done })
	select {
	case <-response.entered:
	case <-time.After(10 * time.Second):
		require.FailNow("MCP response did not reach outer writer")
	}
	require.Contains(string(response.captured), "synthetic aurora payload")
	require.NotContains(string(response.captured), `"error":`)
	probe := flock.New(f.cfg.AnalyticsDir() + ".x-content.lock")
	locked, err := probe.TryLock()
	require.NoError(err)
	if locked {
		require.NoError(probe.Unlock())
	}
	require.NoError(probe.Close())
	assert.False(locked, "outer response must retain the content lease")
	removeCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	removed := make(chan error, 1)
	removeDone := make(chan struct{})
	go func() {
		defer close(removeDone)
		_, err := cleaner.RemovePost(removeCtx, f.source.ID, "100", "removed")
		removed <- err
	}()
	t.Cleanup(func() { stop(); unblock(); <-removeDone })
	dbPath, err := f.cfg.DatabasePath()
	require.NoError(err)
	sourceLock := flock.New(dbPath + ".sync-" + strconv.FormatInt(f.source.ID, 10) + ".lock")
	t.Cleanup(func() { require.NoError(sourceLock.Close()) })
	// Source ownership is taken before the content fence; observing it proves
	// the actual removal has started while the response is still paused.
	require.Eventually(func() bool {
		ok, err := sourceLock.TryLock()
		if err != nil {
			return false
		}
		if ok {
			_ = sourceLock.Unlock()
		}
		return !ok
	}, 10*time.Second, 20*time.Millisecond)
	select {
	case <-removeDone:
		require.FailNow("removal finished while the response was paused")
	default:
	}
	unblock()
	<-done
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "synthetic aurora payload")
	require.NoError(<-removed)
	stop()
}
