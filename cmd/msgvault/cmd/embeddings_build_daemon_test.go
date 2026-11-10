//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestManagedEmbeddingsCommandsDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"embeddings", "build", "--full-rebuild", "--yes"},
		{"embeddings", "resume", "--backstop"},
		{"build-embeddings", "--yes"},
	} {
		t.Run(args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			adapter := &storeAPIAdapter{config: &config.Config{}}
			childCalled := false
			err := adapter.runCLICommandWithRunner(adapter.invocationContext(t.Context()), api.CLIRunRequest{Args: args}, nil,
				func(context.Context, []string, map[string]string, string, func(string, string) error) error {
					childCalled = true
					return nil
				})
			require.ErrorContains(t, err, "vector search not enabled")
			assert.False(t, childCalled)
		})
	}
}

func TestManagedEmbeddingsReportsGateWait(t *testing.T) {
	gate := api.NewSerialOperationGate()
	release, ok := gate.BeginWorkContext(t.Context())
	require.True(t, ok)
	defer release()
	cfg := config.NewDefaultConfig()
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Endpoint = "https://example.com/v1"
	cfg.Vector.Embeddings.Model = "test-model"
	adapter := &storeAPIAdapter{config: cfg, operationGate: gate}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan api.CLIRunEvent, 1)
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes"}}, func(event api.CLIRunEvent) error {
			select {
			case events <- event:
			default:
			}
			return nil
		})
	}()
	select {
	case event := <-events:
		assert.Equal(t, cliStreamStderr, event.Type)
		assert.Contains(t, event.Data, "Waiting for archive operation gate")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "gate wait message was not emitted")
	}
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestManagedEmbeddingsBuildWithoutConfirmationAborts(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Endpoint = "https://example.com/v1"
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	input, writer, err := os.Pipe()
	require.NoError(t, err)
	oldStdin := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = oldStdin; _ = writer.Close(); _ = input.Close() }()
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: api.NewSerialOperationGate()}
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild"}}, nil)
	}()
	select {
	case err := <-result:
		require.ErrorContains(t, err, "aborted")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "build waited on daemon stdin")
	}
}

func TestManagedEmbeddingsSQLLoggingFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"resume slow equals", []string{"embeddings", "resume", "--log-sql-slow-ms=25"}, "configure daemon SQL logging with [log] sql_trace or sql_slow_ms instead of CLI flags"},
		{"false and zero", []string{"embeddings", "build", "--log-sql=false", "--log-sql-slow-ms=0"}, "shutting down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := api.NewSerialOperationGate()
			gate.StartDrain()
			cfg := config.NewDefaultConfig()
			cfg.Vector.Enabled = true
			cfg.Vector.Embeddings.Model = "test-model"
			cfg.Vector.Embeddings.Endpoint = "https://example.com/v1"
			adapter := &storeAPIAdapter{config: cfg, operationGate: gate}
			err := adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: tc.args}, nil)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestManagedEmbeddingsReconcilesMessageChangedDuringYield(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.BatchSize = 1
	cfg.Vector.Embeddings.APIKeyEnv = "TEST_EMBEDDING_KEY"
	firstRequest, releaseFirst := make(chan struct{}), make(chan struct{})
	signals := make(chan (<-chan struct{}), 1)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case signals <- jobctx.PreemptionSignal(r.Context()):
		default:
		}
		return transport.RoundTrip(r)
	})
	defer func() { http.DefaultTransport = transport }()
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		if requests.Add(1) == 1 {
			close(firstRequest)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		}
		assert.NoError(t, writeFakeEmbeddingResponse(w, len(body.Input), 4))
	}))
	t.Cleanup(provider.Close)
	cfg.Vector.Embeddings.Endpoint = provider.URL
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	_, firstID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "first", "One", "First message")
	require.NoError(t, err)
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "second", "Two", "Second message")
	require.NoError(t, err)
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "sync"))
	t.Cleanup(func() { <-sched.Stop().Done() })
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: gate, embeddingsScheduler: sched}
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, nil)
	}()
	select {
	case <-firstRequest:
	case err := <-result:
		require.FailNowf(t, "build ended before provider", "%v", err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "provider did not start")
	}
	entered, update := make(chan struct{}), make(chan struct{})
	changed := make(chan error, 1)
	go func() {
		release, ok := gate.BeginRequestWorkContext(t.Context(), "sync")
		if !ok {
			changed <- t.Context().Err()
			return
		}
		defer release()
		close(entered)
		select {
		case <-update:
		case <-t.Context().Done():
			return
		}
		changed <- st.UpsertMessageBodyContext(t.Context(), firstID, sql.NullString{String: "Updated first message", Valid: true}, sql.NullString{})
	}()
	require.Eventually(t, gate.HasRequestWaiters, 10*time.Second, 10*time.Millisecond)
	signal := <-signals
	require.NotNil(t, signal)
	select {
	case <-signal:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build was not asked to yield")
	}
	close(releaseFirst)
	select {
	case <-entered:
	case err := <-result:
		require.FailNowf(t, "build ended before sync", "%v", err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "sync did not run")
	}
	err = adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "resume"}}, nil)
	require.ErrorContains(t, err, "already running")
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(t, err)
	defer func() { _ = backend.Close() }()
	state := &invocation{cfg: cfg}
	personGate := vector.NewPinnedExactSemanticPersonEmbeddingGate(cfg.Vector, currentSemanticPersonVectorConfigSource(state), st)
	runtime, err := newEmbeddingRuntime(cfg.Vector, embeddingRuntimeDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st, PersonGate: personGate, APIKey: "synthetic-key"})
	require.NoError(t, err)
	job := &scheduler.EmbedJob{Worker: runtime.Runner, Backend: backend, Store: st, Convergence: runtime.Convergence, Fingerprint: cfg.Vector.GenerationFingerprint(), BackstopInterval: -1}
	require.NoError(t, job.Run(t.Context()))
	scheduledActive, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, vector.GenerationID(1), scheduledActive.ID)
	close(update)
	require.NoError(t, <-changed)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build did not finish")
	}
	active, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, vector.GenerationID(1), active.ID)
	missing, err := st.MissingCount(t.Context(), int64(active.ID))
	require.NoError(t, err)
	assert.Zero(t, missing)
	assert.Equal(t, int32(3), requests.Load())
}

func TestManagedEmbeddingsRejectsReplacedScopedSourceAfterYield(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.BatchSize = 1
	cfg.Vector.Embeddings.APIKeyEnv = "TEST_EMBEDDING_KEY"
	started, release := make(chan struct{}), make(chan struct{})
	signals := make(chan (<-chan struct{}), 1)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case signals <- jobctx.PreemptionSignal(r.Context()):
		default:
		}
		return transport.RoundTrip(r)
	})
	defer func() { http.DefaultTransport = transport }()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		assert.NoError(t, writeFakeEmbeddingResponse(w, len(body.Input), 4))
	}))
	t.Cleanup(provider.Close)
	cfg.Vector.Embeddings.Endpoint = provider.URL
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	source, _, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "original@example.test", "first", "First", "First message")
	require.NoError(t, err)
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "original@example.test", "second", "Second", "Second message")
	require.NoError(t, err)
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "sync"))
	t.Cleanup(func() { <-sched.Stop().Done() })
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: gate, embeddingsScheduler: sched}
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes", "--account", "original@example.test"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, nil)
	}()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "provider did not start")
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	go func() {
		done, ok := gate.BeginRequestWorkContext(t.Context(), "account replacement")
		if !ok {
			return
		}
		defer done()
		close(entered)
		select {
		case <-finish:
		case <-t.Context().Done():
		}
	}()
	require.Eventually(t, gate.HasRequestWaiters, 10*time.Second, 10*time.Millisecond)
	signal := <-signals
	require.NotNil(t, signal)
	select {
	case <-signal:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build was not asked to yield")
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "replacement did not enter")
	}
	require.NoError(t, st.RemoveSource(source.ID))
	replacement, _, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "replacement@example.test", "replacement", "Replacement", "Private replacement message")
	require.NoError(t, err)
	require.Equal(t, source.ID, replacement.ID)
	close(finish)
	select {
	case err := <-result:
		require.ErrorContains(t, err, "embedding scope changed")
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build did not finish")
	}
	assert.Equal(t, int32(1), calls.Load(), "replacement message must not reach the provider")
}

func TestManagedEmbeddingsStreamsWorkerWarnings(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.APIKeyEnv = "TEST_EMBEDDING_KEY"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic provider failure", http.StatusBadRequest)
	}))
	t.Cleanup(provider.Close)
	cfg.Vector.Embeddings.Endpoint = provider.URL
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "first", "First", "First message")
	require.NoError(t, err)
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: api.NewSerialOperationGate()}
	var stderr strings.Builder
	err = adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, func(event api.CLIRunEvent) error {
		if event.Type == cliStreamStderr {
			stderr.WriteString(event.Data)
		}
		return nil
	})
	assert.Contains(t, stderr.String(), "embed batch failed", "build error: %v", err)
}

func TestManagedEmbeddingsBuildUsesSavedRecipeAndRunningArchive(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Model = "running-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.APIKeyEnv = "TEST_EMBEDDING_KEY"
	var model string
	var inputs []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		model = body.Model
		inputs = body.Input
		assert.NoError(t, writeFakeEmbeddingResponse(w, len(body.Input), 4))
	}))
	t.Cleanup(provider.Close)
	cfg.Vector.Embeddings.Endpoint = provider.URL
	saved := *cfg
	saved.Vector.Embeddings.Model = "saved-model"
	stripQuotes := false
	saved.Vector.Preprocess.StripQuotes = &stripQuotes
	saved.Vector.DBPath = filepath.Join(dir, "other-vectors.db")
	require.NoError(t, saved.Save())
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "first", "First", "First message\n> quoted text")
	require.NoError(t, err)
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: api.NewSerialOperationGate()}
	err = adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "saved-model", model)
	require.Len(t, inputs, 1)
	assert.Contains(t, inputs[0], "quoted text")
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(t, err)
	defer func() { _ = backend.Close() }()
	active, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "saved-model", active.Model)
	_, err = os.Stat(saved.Vector.DBPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestManagedEmbedCancelledBeforeStoreOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	ctx, cancel := context.WithCancel(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	err := runEmbedWithOptions(cmd, managedEmbedOptions{yield: func() error { return nil }})
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(cfg.DatabaseDSN())
	assert.ErrorIs(t, err, os.ErrNotExist)
}
