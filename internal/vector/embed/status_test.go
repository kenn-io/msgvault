//go:build sqlite_vec

package embed

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
)

type diagnosticCapture struct {
	mu   sync.Mutex
	data []byte
}

func (c *diagnosticCapture) SaveEmbeddingDiagnostics(_ context.Context, d vector.EmbeddingDiagnostics) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	c.data, err = json.Marshal(d)
	return err
}
func (c *diagnosticCapture) read(t *testing.T) vector.EmbeddingDiagnostics {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var d vector.EmbeddingDiagnostics
	require.NoError(t, json.Unmarshal(c.data, &d))
	return d
}

// waitFor waits for a timer-driven write that satisfies done.
func (c *diagnosticCapture) waitFor(t *testing.T, done func(vector.EmbeddingDiagnostics) bool) vector.EmbeddingDiagnostics {
	t.Helper()
	var got vector.EmbeddingDiagnostics
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.data == nil {
			return false
		}
		got = vector.EmbeddingDiagnostics{}
		return json.Unmarshal(c.data, &got) == nil && done(got)
	}, 10*time.Second, 10*time.Millisecond)
	return got
}

func (c *diagnosticCapture) written() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data != nil
}

func (d *embeddingDiagnosticRun) currentBatch(t *testing.T) vector.EmbeddingBatch {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotNil(t, d.snapshot.CurrentBatch)
	return *d.snapshot.CurrentBatch
}

type blockedDiagnosticClient struct {
	inner   EmbeddingClient
	started chan struct{}
	release chan struct{}
}

func (c *blockedDiagnosticClient) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.inner.Embed(ctx, inputs)
}

func TestWorkerDiagnosticsVisibleWhileProviderRunsAndAfterCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newWorkerFixture(t, 2)
	capture := &diagnosticCapture{}
	client := &blockedDiagnosticClient{inner: f.FakeClient, started: make(chan struct{}), release: make(chan struct{})}
	w := newTestWorker(f, 2)
	w.deps.Client, w.deps.Diagnostics = client, capture
	done := make(chan error, 1)
	go func() { _, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope()); done <- err }()
	<-client.started
	d := capture.waitFor(t, func(d vector.EmbeddingDiagnostics) bool {
		return d.CurrentBatch != nil && d.CurrentBatch.Phase == "provider"
	})
	assert.Equal(2, d.CurrentBatch.Attempted)
	assert.Zero(d.CurrentBatch.Completed)
	close(client.release)
	require.NoError(<-done)
	d = capture.read(t)
	require.Len(d.RecentBatches, 1)
	assert.Equal(2, d.RecentBatches[0].Completed)
	assert.Positive(d.RecentBatches[0].Chars)
	assert.NotNil(d.FinishedAt)
	assert.NotNil(d.LastSuccessfulBatchAt)
	assert.Nil(d.CurrentBatch)
}

func TestProviderDiagnosticsCountActual429Attempts(t *testing.T) {
	for _, format := range []string{"openai", "voyage"} {
		t.Run(format, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if attempts == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if format == "openai" {
					_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
				} else {
					_, _ = w.Write([]byte(`{"data":[{"index":0,"data":[{"index":0,"embedding":[1]}]}]}`))
				}
			}))
			defer server.Close()
			d := newEmbeddingDiagnosticRun(t.Context(), &diagnosticCapture{}, nil, 1, 1)
			d.beginBatch(1)
			ctx := withEmbeddingDiagnostics(t.Context(), d)
			if format == "openai" {
				client := NewClient(Config{Endpoint: server.URL, Model: "synthetic", Dimension: 1})
				_, err := client.Embed(ctx, []string{"synthetic"})
				require.NoError(err)
			} else {
				client := NewVoyageClient(VoyageConfig{Endpoint: server.URL, Model: "synthetic", Dimension: 1})
				_, err := client.EmbedDocuments(ctx, []DocumentInput{{Chunks: []string{"synthetic"}}})
				require.NoError(err)
			}
			batch := d.currentBatch(t)
			assert.Equal(2, batch.Requests)
			assert.Equal(1, batch.Retries)
			assert.Equal(1, batch.RateLimits)
			d.finish(nil)
		})
	}
}

type embeddingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f embeddingRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestContextDiagnosticsBackstopWithNoWorkKeepsPreviousTimings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	capture := &diagnosticCapture{}
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) { deps.Diagnostics = capture })
	f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
	_, err := f.run()
	require.NoError(err)
	first := capture.read(t)
	require.NotEmpty(first.RecentBatches)
	completed := 0
	for _, batch := range first.RecentBatches {
		completed += batch.Completed
		assert.Positive(batch.Completed, "a batch that covered nothing is not recorded")
	}
	assert.Equal(1, completed)
	_, err = f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(first, capture.read(t), "a converged backstop must not replace the previous pass's timings")
}

type diagnosticWriterFunc func(context.Context, vector.EmbeddingDiagnostics) error

func (f diagnosticWriterFunc) SaveEmbeddingDiagnostics(ctx context.Context, d vector.EmbeddingDiagnostics) error {
	return f(ctx, d)
}

func TestDiagnosticTimingMeasuresElapsedTime(t *testing.T) {
	tests := []struct {
		name    string
		writer  vector.EmbeddingDiagnosticWriter
		measure func(t *testing.T, ctx context.Context)
		got     func(b vector.EmbeddingBatch) float64
	}{
		{
			name: "provider does not wait for diagnostic writes",
			writer: diagnosticWriterFunc(func(ctx context.Context, _ vector.EmbeddingDiagnostics) error {
				<-ctx.Done()
				return ctx.Err()
			}),
			measure: func(_ *testing.T, ctx context.Context) {
				finish := measureEmbeddingProvider(ctx, []string{"synthetic"})
				synctest.Sleep(2 * time.Second)
				finish(1, nil)
			},
			got: func(b vector.EmbeddingBatch) float64 { return b.ProviderMS },
		},
		{
			name:   "write",
			writer: &diagnosticCapture{},
			measure: func(_ *testing.T, ctx context.Context) {
				finish := measureEmbeddingWrite(ctx)
				synctest.Sleep(2 * time.Second)
				finish(nil)
			},
			got: func(b vector.EmbeddingBatch) float64 { return b.DBWriteMS },
		},
		{
			name:   "request round trip",
			writer: &diagnosticCapture{},
			measure: func(t *testing.T, ctx context.Context) {
				t.Helper()
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/embeddings", nil)
				transport := embeddingMeasuredTransport{base: embeddingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					synctest.Sleep(2 * time.Second)
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
						Request:    r,
					}, nil
				})}
				response, err := transport.RoundTrip(req)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
			},
			got: func(b vector.EmbeddingBatch) float64 { return b.RequestMS },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newEmbeddingDiagnosticRun(t.Context(), tt.writer, nil, 1, 1)
				d.beginBatch(1)
				d.completed(1)
				tt.measure(t, withEmbeddingDiagnostics(t.Context(), d))
				assert.InDelta(t, float64(2000), tt.got(d.currentBatch(t)), 0.0001)
				d.finish(nil)
			})
		})
	}
}

func TestDiagnosticWritesAreThrottledAndCatchUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		var mu sync.Mutex
		writes := 0
		var last vector.EmbeddingDiagnostics
		writer := diagnosticWriterFunc(func(_ context.Context, d vector.EmbeddingDiagnostics) error {
			mu.Lock()
			defer mu.Unlock()
			writes++
			last = d
			return nil
		})
		observed := func() (int, vector.EmbeddingDiagnostics) {
			synctest.Wait()
			mu.Lock()
			defer mu.Unlock()
			return writes, last
		}
		d := newEmbeddingDiagnosticRun(t.Context(), writer, nil, 1, 1)
		ctx := withEmbeddingDiagnostics(t.Context(), d)
		d.beginBatch(1)
		n, _ := observed()
		assert.Zero(n, "nothing is written before the pass does work")
		finish := measureEmbeddingProvider(ctx, []string{"synthetic"})
		n, _ = observed()
		assert.Equal(1, n)
		finish(1, nil)
		finishWrite := measureEmbeddingWrite(ctx)
		n, _ = observed()
		assert.Equal(1, n, "phase changes within a second share one write")
		time.Sleep(time.Second)
		n, got := observed()
		assert.Equal(2, n)
		assert.Equal("write", got.CurrentBatch.Phase, "a phase that starts just after a write still shows within a second")
		finishWrite(nil)
		d.finish(nil)
		n, got = observed()
		assert.Equal(3, n)
		assert.NotNil(got.FinishedAt)
	})
}

func TestDiagnosticWriteFailureIsLoggedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		writer := diagnosticWriterFunc(func(context.Context, vector.EmbeddingDiagnostics) error {
			return errors.New("synthetic permission denied")
		})
		d := newEmbeddingDiagnosticRun(t.Context(), writer, slog.New(slog.NewTextHandler(&logs, nil)), 1, 1)
		d.beginBatch(1)
		d.completed(1)
		time.Sleep(2 * time.Second)
		d.completed(1)
		d.finish(nil)
		assert.Equal(t, 1, strings.Count(logs.String(), "synthetic permission denied"))
	})
}

func TestDiagnosticsIgnoreYieldCancellation(t *testing.T) {
	assert := assert.New(t)
	capture := &diagnosticCapture{}
	ctx, cancel := context.WithCancelCause(t.Context())
	d := newEmbeddingDiagnosticRun(ctx, capture, nil, 1, 1)
	measured := withEmbeddingDiagnostics(ctx, d)
	d.beginBatch(1)
	finish := measureEmbeddingProvider(measured, []string{"synthetic"})
	cancel(jobctx.ErrYieldedToWaiter)
	finish(0, fmt.Errorf("embed: %w", ctx.Err()))
	d.finish(nil)
	got := capture.read(t)
	assert.Nil(got.LastError, "yielding to a waiting operation is not a failure")
	require.Len(t, got.RecentBatches, 1)
	assert.Nil(got.RecentBatches[0].Error)
	assert.Zero(got.RecentBatches[0].Chars, "rejected input is not counted")
}

func TestContextDiagnosticsCountCharsForPartialPackedSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests > 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]}]}]}`))
	}))
	defer server.Close()
	client := NewVoyageClient(VoyageConfig{Endpoint: server.URL, Model: "synthetic", Dimension: 1, Limits: RequestLimits{MaxDocuments: 1}})
	w := &ContextWorker{deps: ContextWorkerDeps{Client: client}}
	d := newEmbeddingDiagnosticRun(t.Context(), &diagnosticCapture{}, nil, 1, 1)
	d.beginBatch(2)
	vectors, err := w.embedDocumentsMeasured(withEmbeddingDiagnostics(t.Context(), d), []DocumentInput{
		{Chunks: []string{"ab", "cd"}}, {Chunks: []string{"efg"}},
	})
	require.Error(err)
	require.Len(vectors, 1, "the first packed request succeeded")
	assert.Equal(4, d.currentBatch(t).Chars, "the published prefix counts; the rejected document does not")
	d.finish(nil)
}

func TestCompletingABatchPersistsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := &diagnosticCapture{}
		d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 1)
		d.beginBatch(1)
		finish := measureEmbeddingWrite(withEmbeddingDiagnostics(t.Context(), d))
		d.completed(1)
		finish(nil)
		d.completeBatch()
		time.Sleep(time.Second)
		synctest.Wait()
		got := capture.read(t)
		assert.Nil(t, got.CurrentBatch, "a slow next scan must not show the finished batch as still writing")
		assert.Len(t, got.RecentBatches, 1)
		d.finish(nil)
	})
}

func TestDiagnosticsPassWithoutWorkWritesNothing(t *testing.T) {
	capture := &diagnosticCapture{}
	d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 1)
	d.beginBatch(3)
	finish := measureEmbeddingWrite(withEmbeddingDiagnostics(t.Context(), d))
	finish(nil)
	d.finish(nil)
	assert.False(t, capture.written())
}

func TestDiagnosticsPersistPreBatchFailure(t *testing.T) {
	capture := &diagnosticCapture{}
	d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 1)
	d.finish(errors.New("synthetic scan failure"))
	got := capture.read(t)
	assert.NotNil(t, got.LastError)
	assert.NotNil(t, got.FinishedAt)
	assert.Empty(t, got.RecentBatches)
}

func TestContextDiagnosticsBatchErrors(t *testing.T) {
	t.Run("failed batch keeps its error", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		capture := &diagnosticCapture{}
		f := newContextWorkerFixture(t, func(d *ContextWorkerDeps) { d.Diagnostics = capture })
		id := f.seed("beeper", f.chatID, time.Now().UTC(), "Synthetic body")
		f.client.before = func() {
			f.client.before = nil
			_, err := f.store.DB().Exec(`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = ?`, id)
			require.NoError(err)
		}
		_, err := f.run()
		require.ErrorContains(err, "coverage CAS")
		d := capture.read(t)
		require.NotEmpty(d.RecentBatches)
		assert.NotNil(d.RecentBatches[len(d.RecentBatches)-1].Error, "a failed batch keeps its error")
	})
	t.Run("budget deferral is not a failure", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		capture := &diagnosticCapture{}
		f := newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
			d.Diagnostics = capture
			d.MaxRunUTF8Bytes = 1
		})
		f.seed("beeper", f.chatID, time.Now().UTC(), "First synthetic body")
		f.seed("beeper", f.chatID, time.Now().UTC().Add(time.Minute), "Second synthetic body")
		_, err := f.run()
		require.NoError(err)
		d := capture.read(t)
		assert.Nil(d.LastError, "running out of the per-pass budget defers work")
		for _, batch := range d.RecentBatches {
			assert.Nil(batch.Error)
		}
	})
}

func TestWorkerDiagnosticsKeepPartialCoverageOnStampFailure(t *testing.T) {
	for _, skipFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%t", skipFirst), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newContextWorkerFixture(t, nil)
			ids := []int64{
				f.seed("email", f.chatID, time.Now().UTC(), "First synthetic body"),
				f.seed("email", f.chatID, time.Now().UTC(), "Second synthetic body"),
			}
			tokens := make(map[int64]any)
			for _, id := range ids {
				var token string
				require.NoError(f.store.DB().QueryRow(`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`, id).Scan(&token))
				tokens[id] = token
			}
			_, err := f.store.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_second_stamp BEFORE UPDATE OF embed_gen ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'synthetic stamp failure'); END`, ids[1]))
			require.NoError(err)
			capture := &diagnosticCapture{}
			d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, f.gen, 1)
			d.beginBatch(2)
			ctx := withEmbeddingDiagnostics(t.Context(), d)
			w := NewWorker(WorkerDeps{Store: f.store, MainDB: f.store.DB(), Backend: f.backend})
			if skipFirst {
				_, err = w.stampSkipped(ctx, f.gen, ids[:1], tokens)
				require.NoError(err)
				ids = ids[1:]
			}
			_, err = w.stampCovered(ctx, f.gen, ids, tokens)
			require.ErrorContains(err, "synthetic stamp failure")
			d.finish(err)
			_, stamped, _, _, err := f.store.CoverageCounts(t.Context(), int64(f.gen))
			require.NoError(err)
			assert.Equal(int64(1), stamped)
			got := capture.read(t)
			require.Len(got.RecentBatches, 1)
			assert.Equal(1, got.RecentBatches[0].Completed)
		})
	}
}

func TestWorkerDiagnosticsCountOnlyNewlyCoveredSkips(t *testing.T) {
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	id := f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
	var token string
	require.NoError(f.store.DB().QueryRow(`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`, id).Scan(&token))
	d := newEmbeddingDiagnosticRun(t.Context(), &diagnosticCapture{}, nil, f.gen, 1)
	d.beginBatch(2)
	w := NewWorker(WorkerDeps{Store: f.store, MainDB: f.store.DB(), Backend: f.backend})
	// The second id has no message row, as when a message is deleted after the scan.
	_, err := w.stampSkipped(withEmbeddingDiagnostics(t.Context(), d), f.gen, []int64{id, id + 1000}, map[int64]any{id: token})
	require.NoError(err)
	assert.Equal(t, 1, d.currentBatch(t).Completed)
	d.finish(nil)
}

func TestWorkerDiagnosticsKeepRecoveredBodyReadFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
	_, err := f.store.DB().Exec(`ALTER TABLE message_bodies RENAME TO held_bodies`)
	require.NoError(err)
	var recoverOnce sync.Once
	writer := diagnosticWriterFunc(func(ctx context.Context, d vector.EmbeddingDiagnostics) error {
		if d.LastError != nil {
			recoverOnce.Do(func() {
				_, err := f.store.DB().Exec(`ALTER TABLE held_bodies RENAME TO message_bodies`)
				assert.NoError(err)
			})
		}
		return f.store.SaveEmbeddingDiagnostics(ctx, d)
	})
	// The worker retries a failed read immediately, so it may fail a few
	// times before the diagnostic write restores the table.
	w := NewWorker(WorkerDeps{Store: f.store, MainDB: f.store.DB(), Backend: f.backend, VectorsDB: f.backend.DB(), Client: &fakeEmbeddingClient{dim: 4}, Recorder: f.store, Diagnostics: writer, MaxConsecutiveFailures: math.MaxInt})
	result, err := w.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(1, result.Succeeded)
	d, err := f.store.ReadEmbeddingDiagnostics(t.Context(), int64(f.gen))
	require.NoError(err)
	require.NotNil(d)
	require.GreaterOrEqual(len(d.RecentBatches), 2)
	first, last := d.RecentBatches[0], d.RecentBatches[len(d.RecentBatches)-1]
	require.NotNil(first.Error)
	assert.Equal(operations.PublicErrorInvocationUpstreamFailed, first.Error.Code)
	assert.Zero(first.Completed)
	assert.Equal(1, last.Completed)
	assert.Nil(last.Error)
}
