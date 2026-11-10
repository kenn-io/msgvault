//go:build sqlite_vec && pgvector

package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
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
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

func TestManagedEmbeddingsPostgresYieldsAndActivates(t *testing.T) {
	_, dsn := openServePGSchema(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = t.TempDir(), t.TempDir()
	cfg.Data.DatabaseURL = dsn
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.BatchSize = 1
	first, resume := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		if count.Add(1) == 1 {
			close(first)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		assert.NoError(t, writeFakeEmbeddingResponse(w, len(body.Input), 4))
	}))
	t.Cleanup(provider.Close)
	cfg.Vector.Embeddings.Endpoint = provider.URL
	st, err := store.Open(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "first", "One", "First message")
	require.NoError(t, err)
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), st, "archive@example.test", "second", "Two", "Second message")
	require.NoError(t, err)
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "scheduled sync"))
	t.Cleanup(func() { <-sched.Stop().Done() })
	adapter := &storeAPIAdapter{config: cfg, store: st, operationGate: gate, embeddingsScheduler: sched}
	result := make(chan error, 1)
	go func() {
		result <- adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}}, nil)
	}()
	select {
	case <-first:
	case err := <-result:
		require.FailNowf(t, "build ended before provider", "%v", err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "provider did not start")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		done, ok := gate.BeginRequestWorkContext(t.Context(), "sync")
		if ok {
			close(entered)
			<-release
			done()
		}
	}()
	require.Eventually(t, gate.HasRequestWaiters, 10*time.Second, 10*time.Millisecond)
	// The scheduler polls request waiters before the current batch commits.
	time.Sleep(10 * time.Second) //nolint:kennlint // sleeptest: wait for the scheduler to request preemption
	close(resume)
	select {
	case <-entered:
	case err := <-result:
		require.FailNowf(t, "build ended before waiter", "%v", err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "waiter did not enter")
	}
	close(release)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "build did not finish")
	}
	backend, err := pgvector.Open(t.Context(), pgvector.Options{DB: st.DB(), Dimension: 4})
	require.NoError(t, err)
	defer func() { _ = backend.Close() }()
	active, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	missing, err := st.MissingCount(t.Context(), int64(active.ID))
	require.NoError(t, err)
	assert.Zero(t, missing)
	assert.Equal(t, int32(2), count.Load())
}
