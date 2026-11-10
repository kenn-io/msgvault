//go:build sqlite_vec

package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestManagedEmbeddingsBuildYieldsToScheduledSync(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.BatchSize = 1
	cfg.Vector.Embeddings.Timeout = 2 * time.Minute
	cfg.Vector.Embeddings.APIKeyEnv = "TEST_EMBEDDING_KEY"
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var count atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		if count.Add(1) == 1 {
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
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "first", "One", "First message")
	require.NoError(t, err)
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "second", "Two", "Second message")
	require.NoError(t, err)
	gate := api.NewSerialOperationGate()
	syncStarted := make(chan struct{})
	jobs := make(chan *scheduler.EmbedJob, 1)
	jobResult := make(chan error, 1)
	sched := scheduler.New(func(ctx context.Context, _ string) error {
		close(syncStarted)
		select {
		case job := <-jobs:
			err := job.Run(ctx)
			jobResult <- err
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}).WithWorkTracker(gate)
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(t, sched.AddAccount("archive@example.test", "0 0 1 1 *"))
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: gate, embeddingsScheduler: sched}
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, nil)
	}()
	select {
	case <-firstRequest:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "provider request did not start")
	}
	require.NoError(t, sched.TriggerSync("archive@example.test", false))
	// The scheduler asks cooperative jobs to yield after its one-minute grace.
	time.Sleep(65 * time.Second)
	close(releaseFirst)
	select {
	case <-syncStarted:
	case err := <-result:
		require.FailNowf(t, "build completed before sync", "%v", err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "sync did not start")
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
	jobs <- &scheduler.EmbedJob{Worker: runtime.Runner, Backend: backend, Store: st, Convergence: runtime.Convergence, Fingerprint: cfg.Vector.GenerationFingerprint(), BackstopInterval: -1}
	select {
	case err := <-jobResult:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "scheduled embedding did not finish")
	}
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build did not finish")
	}
	assert.Equal(t, int32(2), count.Load())
	active, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	generations := embedGenByID(t, dir)
	require.Len(t, generations, 2)
	for _, generation := range generations {
		require.True(t, generation.Valid)
		assert.Equal(t, int64(active.ID), generation.Int64)
	}
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "third", "Three", "Third message")
	require.NoError(t, err)
	require.NoError(t, adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "resume", "--backstop"}, Env: map[string]string{"TEST_EMBEDDING_KEY": "synthetic-key"}}, nil))
	assert.Equal(t, int32(3), count.Load())
	resumed, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, active.ID, resumed.ID)
	generations = embedGenByID(t, dir)
	require.Len(t, generations, 3)
	for _, generation := range generations {
		require.True(t, generation.Valid)
		assert.Equal(t, int64(active.ID), generation.Int64)
	}
}

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
