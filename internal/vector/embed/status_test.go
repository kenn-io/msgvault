//go:build sqlite_vec

package embed

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

type diagnosticCapture struct {
	mu          sync.Mutex
	data        []byte
	fingerprint string
}

func (c *diagnosticCapture) SaveEmbeddingDiagnostics(_ context.Context, d vector.EmbeddingDiagnostics) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	c.data, err = json.Marshal(d)
	c.fingerprint = d.GenerationFingerprint
	return err
}
func (c *diagnosticCapture) ReadEmbeddingDiagnostics(_ context.Context, _ int64) (*vector.EmbeddingDiagnostics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		return nil, nil //nolint:nilnil // No pass has saved diagnostics yet.
	}
	var d vector.EmbeddingDiagnostics
	if err := json.Unmarshal(c.data, &d); err != nil {
		return nil, err
	}
	d.GenerationFingerprint = c.fingerprint
	return &d, nil
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

func (d *embeddingDiagnosticRun) currentBatch(t *testing.T) vector.EmbeddingBatch {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotNil(t, d.snapshot.CurrentBatch)
	return *d.snapshot.CurrentBatch
}

type blockedDiagnosticClient struct {
	inner       EmbeddingClient
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func (c *blockedDiagnosticClient) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	c.startedOnce.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.inner.Embed(ctx, inputs)
}

func TestWorkerDiagnosticsVisibleWhileProviderRuns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newWorkerFixture(t, 2)
	capture := &diagnosticCapture{}
	client := &blockedDiagnosticClient{inner: f.FakeClient, started: make(chan struct{}), release: make(chan struct{})}
	w := newTestWorker(f, 2)
	w.deps.Client, w.deps.Diagnostics = client, capture
	done := make(chan error, 1)
	go func() { _, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope()); done <- err }()
	select {
	case <-client.started:
	case err := <-done:
		require.FailNowf("worker returned before calling provider", "RunOnce returned %v", err)
	}
	d := capture.waitFor(t, func(d vector.EmbeddingDiagnostics) bool {
		return d.CurrentBatch != nil && d.CurrentBatch.Phase == "provider"
	})
	assert.Equal(2, d.CurrentBatch.Attempted)
	assert.Zero(d.CurrentBatch.Completed)
	close(client.release)
	require.NoError(<-done)
}

type delayedDiagnosticScanStore struct {
	WorkStore

	delay              func()
	scans              int
	firstScanStartedAt time.Time
}

func (s *delayedDiagnosticScanStore) ScanForEmbedding(ctx context.Context, target, afterID int64, limit int) ([]int64, error) {
	if s.scans == 0 {
		s.firstScanStartedAt = time.Now()
	}
	s.scans++
	s.delay()
	return s.WorkStore.ScanForEmbedding(ctx, target, afterID, limit)
}

func TestWorkerDiagnosticsBatchIncludesScanTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newWorkerFixture(t, 1)
		capture := &diagnosticCapture{}
		const scanDelay = 100 * time.Millisecond
		scanning := &delayedDiagnosticScanStore{WorkStore: f.Store, delay: func() { time.Sleep(scanDelay) }}
		w := newTestWorker(f, 1)
		w.deps.Store, w.deps.Diagnostics = scanning, capture
		_, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
		require.NoError(err)
		got := capture.read(t)
		require.NotEmpty(got.RecentBatches)
		assert.False(got.RecentBatches[0].StartedAt.After(scanning.firstScanStartedAt), "batch timing starts before scanning")
		assert.GreaterOrEqual(got.RecentBatches[0].ElapsedMS, float64(scanDelay)/float64(time.Millisecond), "the completed batch includes its scan delay")
	})
}

type diagnosticAssemblerFunc func(context.Context, SourceSnapshot, []AffectedScope) ([]Document, error)

func (f diagnosticAssemblerFunc) AssembleScopes(ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error) {
	return f(ctx, snapshot, scopes)
}

func TestContextWorkerDiagnosticsBatchIncludesPreparationTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newContextWorkerFixture(t, nil)
		f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
		capture := &diagnosticCapture{}
		assembler := f.deps.Assembler
		const preparationDelay = 100 * time.Millisecond
		var preparationStartedAt time.Time
		f.deps.Assembler = diagnosticAssemblerFunc(func(ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error) {
			if len(scopes) > 0 {
				if preparationStartedAt.IsZero() {
					preparationStartedAt = time.Now()
				}
				time.Sleep(preparationDelay)
			}
			return assembler.AssembleScopes(ctx, snapshot, scopes)
		})
		f.deps.Diagnostics = capture
		f.restartWorker()
		_, err := f.run()
		require.NoError(err)
		got := capture.read(t)
		require.NotEmpty(got.RecentBatches)
		assert.False(got.RecentBatches[0].StartedAt.After(preparationStartedAt), "batch timing starts before assembly")
		assert.GreaterOrEqual(got.RecentBatches[0].ElapsedMS, float64(preparationDelay)/float64(time.Millisecond), "the completed batch includes preparation")
	})
}

func TestProviderDiagnosticsCountActual429Attempts(t *testing.T) {
	for _, format := range []string{"openai", "voyage partial packed success"} {
		t.Run(format, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			attempts := 0
			acceptedChars := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if format != "openai" && attempts > 2 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if format != "openai" {
					var request struct {
						Inputs [][]string `json:"inputs"`
					}
					if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
						return
					}
					for _, document := range request.Inputs {
						for _, text := range document {
							acceptedChars += utf8.RuneCountInString(text)
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if format == "openai" {
					_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
				} else {
					_, _ = w.Write([]byte(`{"data":[{"index":0,"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]}]}]}`))
				}
			}))
			defer server.Close()
			d := newEmbeddingDiagnosticRun(t.Context(), &diagnosticCapture{}, nil, 1, 1)
			d.beginBatch(1, time.Now())
			ctx := withEmbeddingDiagnostics(t.Context(), d)
			if format == "openai" {
				client := NewClient(Config{Endpoint: server.URL, Model: "synthetic", Dimension: 1})
				_, err := client.Embed(ctx, []string{"synthetic"})
				require.NoError(err)
			} else {
				inputs := []DocumentInput{{Chunks: []string{"ab", "cd"}}, {Chunks: []string{"efg"}}}
				client := NewVoyageClient(VoyageConfig{Endpoint: server.URL, Model: "synthetic", Dimension: 1, Limits: RequestLimits{MaxDocuments: 1}, DocumentPrefix: "document: λ "})
				w := &ContextWorker{deps: ContextWorkerDeps{Client: client}}
				vectors, err := w.embedDocumentsMeasured(ctx, inputs)
				require.Error(err)
				require.Len(vectors, 1, "the first packed request succeeded")
			}
			batch := d.currentBatch(t)
			wantRequests := 2
			if format != "openai" {
				wantRequests++
			}
			assert.Equal(wantRequests, batch.Requests)
			assert.Equal(1, batch.Retries)
			assert.Equal(1, batch.RateLimits)
			if format != "openai" {
				assert.Positive(acceptedChars)
				assert.Equal(acceptedChars, batch.Chars, "count the provider-bound text once, including its prefix")
			}
			d.finish(nil)
		})
	}
}

type embeddingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f embeddingRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestMeasuredTransportShowsRetryWaitUntilNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := &diagnosticCapture{}
		d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 1)
		d.beginBatch(1, time.Now())
		defer d.finish(nil)
		ctx := withEmbeddingAttemptGroup(withEmbeddingDiagnostics(t.Context(), d))
		finishProvider := measureEmbeddingProvider(ctx, []string{"synthetic"})
		defer finishProvider(1, nil)
		providerStarted := d.currentBatch(t).PhaseStartedAt
		synctest.Sleep(time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/embeddings", nil)
		require.NoError(t, err)
		transport := embeddingMeasuredTransport{base: embeddingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			assert.Equal(t, "provider", d.currentBatch(t).Phase)
			synctest.Sleep(2 * time.Second)
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		response, err := transport.RoundTrip(request)
		require.NoError(t, err)
		assert.Equal(t, "provider", d.currentBatch(t).Phase)
		assert.Equal(t, providerStarted, d.currentBatch(t).PhaseStartedAt)
		require.NoError(t, response.Body.Close())
		waiting := d.currentBatch(t)
		assert.Equal(t, "retry_wait", waiting.Phase)
		assert.Equal(t, time.Now().UTC(), waiting.PhaseStartedAt)
		synctest.Sleep(30 * time.Second)
		synctest.Wait()
		assert.Equal(t, "retry_wait", capture.read(t).CurrentBatch.Phase)
		response, err = transport.RoundTrip(request)
		require.NoError(t, err)
		assert.Equal(t, "provider", d.currentBatch(t).Phase)
		assert.Equal(t, waiting.PhaseStartedAt.Add(30*time.Second), d.currentBatch(t).PhaseStartedAt)
		require.NoError(t, response.Body.Close())
		assert.InDelta(t, float64(4000), d.currentBatch(t).RequestMS, 0.0001)
	})
}

type diagnosticWriterFunc func(context.Context, vector.EmbeddingDiagnostics) error

func (f diagnosticWriterFunc) SaveEmbeddingDiagnostics(ctx context.Context, d vector.EmbeddingDiagnostics) error {
	return f(ctx, d)
}

func TestDiagnosticTimingMeasuresElapsedTime(t *testing.T) {
	tests := []struct {
		name    string
		measure func(t *testing.T, ctx context.Context)
		got     func(b vector.EmbeddingBatch) float64
	}{
		{
			name: "provider",
			measure: func(_ *testing.T, ctx context.Context) {
				finish := measureEmbeddingProvider(ctx, []string{"synthetic"})
				synctest.Sleep(2 * time.Second)
				finish(1, nil)
			},
			got: func(b vector.EmbeddingBatch) float64 { return b.ProviderMS },
		},
		{
			name: "write",
			measure: func(_ *testing.T, ctx context.Context) {
				finish := measureEmbeddingWrite(ctx)
				synctest.Sleep(2 * time.Second)
				finish(nil)
			},
			got: func(b vector.EmbeddingBatch) float64 { return b.DBWriteMS },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newEmbeddingDiagnosticRun(t.Context(), &diagnosticCapture{}, nil, 1, 1)
				d.beginBatch(1, time.Now())
				tt.measure(t, withEmbeddingDiagnostics(t.Context(), d))
				assert.InDelta(t, float64(2000), tt.got(d.currentBatch(t)), 0.0001)
				d.finish(nil)
			})
		})
	}
}

func TestWorkerDiagnosticsCountCommittedCoverage(t *testing.T) {
	// A failed coverage stamp rolls back its whole batch, so only skip stamps
	// committed before it count as completed.
	for mode, wantStamped := range map[string]int{"covered": 0, "skipped then covered": 1, "missing skip": 1} {
		t.Run(mode, func(t *testing.T) {
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
			d.beginBatch(2, time.Now())
			ctx := withEmbeddingDiagnostics(t.Context(), d)
			w := NewWorker(WorkerDeps{Store: f.store, MainDB: f.store.DB(), Backend: f.backend})
			switch mode {
			case "missing skip":
				_, err = w.stampSkipped(ctx, f.gen, []int64{ids[0], ids[1] + 1000}, tokens)
				require.NoError(err)
			case "skipped then covered":
				_, err = w.stampSkipped(ctx, f.gen, ids[:1], tokens)
				require.NoError(err)
				ids = ids[1:]
			}
			if mode != "missing skip" {
				_, err = w.stampCovered(ctx, f.gen, ids, tokens)
				require.ErrorContains(err, "synthetic stamp failure")
			}
			d.finish(err)
			_, stamped, _, _, err := f.store.CoverageCounts(t.Context(), int64(f.gen))
			require.NoError(err)
			assert.Equal(int64(wantStamped), stamped)
			got := capture.read(t)
			require.Len(got.RecentBatches, 1)
			assert.Equal(wantStamped, got.RecentBatches[0].Completed)
		})
	}
}

func TestWorkerDiagnosticsKeepRecoveredBodyReadFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	require.NoError(err)
	failedRead := false
	require.NoError(conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok)
		sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if !failedRead && action == sqlite3.SQLITE_READ && table == "message_bodies" {
				failedRead = true
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(conn.Close())
	w := NewWorker(WorkerDeps{Store: f.store, MainDB: f.store.DB(), Backend: f.backend, VectorsDB: f.backend.DB(), Client: &fakeEmbeddingClient{dim: 4}, Recorder: f.store, Diagnostics: f.store, MaxConsecutiveFailures: 2})
	result, err := w.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(1, result.Succeeded)
	d, err := f.store.ReadEmbeddingDiagnostics(t.Context(), int64(f.gen))
	require.NoError(err)
	require.NotNil(d)
	require.Len(d.RecentBatches, 2)
	first, last := d.RecentBatches[0], d.RecentBatches[len(d.RecentBatches)-1]
	require.NotNil(first.Error)
	assert.Equal(operations.PublicErrorInvocationUpstreamFailed, first.Error.Code)
	assert.Zero(first.Completed)
	assert.Equal(1, last.Completed)
	assert.Nil(last.Error)
}

func TestContextDiagnosticsSizeRejection(t *testing.T) {
	for _, mode := range []string{"recovered", "unrecoverable", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			unrecoverable := mode == "unrecoverable"
			acceptedChars := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Inputs [][]string `json:"inputs"`
				}
				if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
					return
				}
				if unrecoverable || len(request.Inputs) > 1 || (mode == "truncated" && len(request.Inputs[0][0]) > 2500) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"detail":"The batch has too many tokens in a chunk for the model context"}`))
					return
				}
				for _, text := range request.Inputs[0] {
					acceptedChars += utf8.RuneCountInString(text)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"index":0,"data":[{"index":0,"embedding":[1]}]}]}`))
			}))
			defer server.Close()
			client := NewVoyageClient(VoyageConfig{Endpoint: server.URL, Model: "synthetic", Dimension: 1, DocumentPrefix: "document: λ "})
			w := &ContextWorker{deps: ContextWorkerDeps{Client: client}}
			capture := &diagnosticCapture{}
			d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 1)
			inputs := []DocumentInput{{Chunks: []string{"ab"}}, {Chunks: []string{"cde"}}}
			if unrecoverable {
				inputs = []DocumentInput{{Chunks: []string{strings.Repeat("a", 4*truncatedContextualDocumentFloorUTF8Bytes)}}}
			}
			if mode == "truncated" {
				inputs = []DocumentInput{{Chunks: []string{strings.Repeat("é", 2048)}}}
			}
			d.beginBatch(len(inputs), time.Now())
			results, err := w.embedDocuments(withEmbeddingDiagnostics(t.Context(), d), inputs)
			require.NoError(err, "an oversized document is recovered or left uncovered without failing the pass")
			require.Len(results, len(inputs))
			d.finish(nil)
			got := capture.read(t)
			if unrecoverable {
				assert.NotNil(got.LastError)
				return
			}
			assert.Nil(got.LastError, "a rejection the worker recovered from is not a pass error")
			require.Len(got.RecentBatches, 1)
			assert.Nil(got.RecentBatches[0].Error)
			assert.Equal(acceptedChars, got.RecentBatches[0].Chars, "only the accepted provider-bound text counts")
		})
	}
}

func TestDiagnosticsClassifyPassErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		yield    bool
		preBatch bool
	}{
		{name: "shutdown", err: fmt.Errorf("RunOnce: %w", context.Canceled)},
		{name: "retired message generation", err: fmt.Errorf("upsert: %w", vector.ErrGenerationRetired)},
		{name: "retired contextual generation", err: errContextGenerationRetired},
		{name: "yielded to waiter", err: fmt.Errorf("embed: %w", context.DeadlineExceeded), yield: true},
		{name: "scan failure", err: errors.New("synthetic scan failure"), preBatch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			capture := &diagnosticCapture{}
			ctx := t.Context()
			if tt.yield {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadlineCause(ctx, time.Now().Add(-time.Second), jobctx.ErrYieldedToWaiter)
				defer cancel()
			}
			d := newEmbeddingDiagnosticRun(ctx, capture, nil, 1, 2)
			if !tt.preBatch {
				d.beginBatch(1, time.Now())
				finish := measureEmbeddingProvider(withEmbeddingDiagnostics(ctx, d), []string{"synthetic"})
				finish(0, tt.err)
			}
			d.finish(tt.err)
			got := capture.read(t)
			assert.NotNil(got.FinishedAt)
			if tt.preBatch {
				assert.NotNil(got.LastError)
				assert.Empty(got.RecentBatches)
				return
			}
			assert.Nil(got.LastError)
			if assert.Len(got.RecentBatches, 1) {
				assert.Nil(got.RecentBatches[0].Error)
			}
		})
	}
}

func TestDiagnosticWriteScheduling(t *testing.T) {
	for _, mode := range []string{"stuck writer", "throttle and catch up", "heartbeat", "failure backoff", "final write recovery", "cancel before finish", "cancel retry wait", "completed batch"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				var mu sync.Mutex
				var logs bytes.Buffer
				var writes []vector.EmbeddingDiagnostics
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var stuckWriterContext context.Context
				finalAttempts := 0
				writer := diagnosticWriterFunc(func(ctx context.Context, snapshot vector.EmbeddingDiagnostics) error {
					if mode == "stuck writer" {
						stuckWriterContext = ctx
						_, hasDeadline := ctx.Deadline()
						assert.True(hasDeadline, "each diagnostic write has a deadline")
						<-ctx.Done()
						return ctx.Err()
					}
					mu.Lock()
					defer mu.Unlock()
					writes = append(writes, snapshot)
					if strings.HasPrefix(mode, "cancel ") && snapshot.FinishedAt != nil {
						assert.NoError(ctx.Err(), "the initial terminal write survives cancellation") //nolint:testifylint // The diagnostics goroutine cannot call FailNow.
						finalAttempts++
						if mode == "cancel retry wait" {
							time.AfterFunc(embeddingDiagnosticSaveInterval/2, cancel)
						}
						return errors.New("database is locked")
					}
					if mode == "final write recovery" {
						if snapshot.FinishedAt == nil {
							return errors.New("database is locked")
						}
						finalAttempts++
						if finalAttempts == 1 {
							<-ctx.Done()
							return ctx.Err()
						}
						if finalAttempts == 2 {
							return errors.New("database is locked")
						}
					}
					if mode == "failure backoff" {
						return errors.New("attempt to write a readonly database")
					}
					return nil
				})
				observed := func() []vector.EmbeddingDiagnostics {
					synctest.Wait()
					mu.Lock()
					defer mu.Unlock()
					return slices.Clone(writes)
				}
				d := newEmbeddingDiagnosticRun(ctx, writer, slog.New(slog.NewTextHandler(&logs, nil)), 1, 1)
				ctx = withEmbeddingDiagnostics(ctx, d)
				d.beginBatch(1, time.Now())
				switch mode {
				case "stuck writer":
					finishProvider := measureEmbeddingProvider(ctx, []string{"synthetic"})
					synctest.Wait()
					finishProvider(1, nil)
					finishWrite := measureEmbeddingWrite(ctx)
					finishWrite(nil)
					d.completed(1)
					d.completeBatch()
				case "throttle and catch up":
					finish := measureEmbeddingProvider(ctx, []string{"synthetic"})
					assert.Len(observed(), 1)
					finish(1, nil)
					finishWrite := measureEmbeddingWrite(ctx)
					assert.Len(observed(), 1, "phase changes within a second share one write")
					synctest.Sleep(time.Second)
					got := observed()
					require.Len(t, got, 2)
					assert.Equal("write", got[1].CurrentBatch.Phase)
					finishWrite(nil)
					d.finish(nil)
					got = observed()
					require.Len(t, got, 3)
					assert.NotNil(got[2].FinishedAt)
				case "heartbeat":
					finish := measureEmbeddingProvider(ctx, []string{"synthetic"})
					start := time.Now()
					assert.Len(observed(), 1)
					synctest.Sleep(embeddingDiagnosticHeartbeat)
					got := observed()
					require.Len(t, got, 2)
					assert.Equal(start.Add(embeddingDiagnosticHeartbeat).UTC(), got[1].UpdatedAt)
					synctest.Sleep(time.Second)
					d.completed(1)
					synctest.Sleep(embeddingDiagnosticSaveInterval)
					assert.Len(observed(), 3, "a change during a heartbeat writes within the save interval")
					finish(1, nil)
				case "cancel before finish", "cancel retry wait":
					d.completed(1)
					assert.Len(observed(), 1)
					if mode == "cancel before finish" {
						cancel()
					}
				case "final write recovery":
					d.completed(1)
					assert.Len(observed(), 1, "the running snapshot write failed first")
				case "failure backoff":
					d.completed(1)
					assert.Len(observed(), 1)
					synctest.Sleep(embeddingDiagnosticSaveInterval)
					assert.Len(observed(), 2, "the first failure retries quickly")
					for range 10 {
						d.completed(1)
						synctest.Sleep(embeddingDiagnosticSaveInterval / 2)
					}
					assert.Len(observed(), 2, "persistent failures back off even as work progresses")
					synctest.Sleep(embeddingDiagnosticHeartbeat)
					assert.Len(observed(), 3, "it retries with the heartbeat")
				case "completed batch":
					finish := measureEmbeddingWrite(ctx)
					d.completed(1)
					finish(nil)
					got := observed()
					require.Len(t, got, 1)
					require.NotNil(t, got[0].CurrentBatch, "persist the incomplete batch before completing it")
					assert.Empty(got[0].RecentBatches)
					d.completeBatch()
					synctest.Sleep(embeddingDiagnosticSaveInterval)
					got = observed()
					require.Len(t, got, 2)
					assert.Nil(got[1].CurrentBatch, "a slow scan must not show a completed batch as writing")
					require.Len(t, got[1].RecentBatches, 1)
					assert.Equal(1, got[1].RecentBatches[0].Completed)
				}
				if mode != "throttle and catch up" {
					d.finish(nil)
				}
				if mode == "stuck writer" {
					require.NotNil(t, stuckWriterContext)
					require.ErrorIs(t, stuckWriterContext.Err(), context.DeadlineExceeded, "finish returns after the stuck writer's deadline expires")
				}
				if strings.HasPrefix(mode, "cancel ") {
					assert.Equal(1, finalAttempts, "cancellation stops retries after the initial terminal write")
				}
				if mode == "final write recovery" {
					got := observed()
					require.Len(t, got, 4)
					assert.Equal(3, finalAttempts)
					assert.NotNil(got[3].FinishedAt)
					assert.Nil(got[3].CurrentBatch)
					require.Len(t, got[3].RecentBatches, 1)
					assert.Equal(1, got[3].RecentBatches[0].Completed)
					assert.Zero(d.saveFailures, "the final write recovered")
				}
				if mode == "failure backoff" {
					assert.Equal(1, strings.Count(logs.String(), "attempt to write a readonly database"))
				}
			})
		})
	}
}

func TestDiagnosticNoWorkPass(t *testing.T) {
	for _, mode := range []string{"no previous snapshot, nothing written", "empty scan", "reconciliation write"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				capture := &diagnosticCapture{}
				d := newEmbeddingDiagnosticRun(t.Context(), capture, nil, 1, 2)
				if mode == "no previous snapshot, nothing written" {
					d.beginBatch(1, time.Now())
					d.completeBatch()
				} else {
					old := time.Now().UTC().Add(-time.Hour)
					d.previous = &vector.EmbeddingDiagnostics{GenerationID: 1, RunID: 1, StartedAt: old, UpdatedAt: old, CurrentBatch: &vector.EmbeddingBatch{Phase: "write", StartedAt: old}}
				}
				if mode == "reconciliation write" {
					d.previous.FinishedAt = &d.previous.UpdatedAt
					d.previous.CurrentBatch = nil
					d.previous.RecentBatches = []vector.EmbeddingBatch{{Completed: 3}}
					d.beginBatch(1, time.Now())
					finishWrite := measureEmbeddingWrite(withEmbeddingDiagnostics(t.Context(), d))
					finishWrite(nil)
					assert.Zero(d.currentBatch(t).Completed)
					d.completeBatch()
					assert.Empty(d.snapshot.RecentBatches, "reconciliation without newly covered rows is not appended")
				}
				d.finish(nil)
				if mode == "no previous snapshot, nothing written" {
					assert.Nil(capture.data)
					return
				}
				got := capture.read(t)
				if mode == "reconciliation write" {
					assert.Equal(*d.previous, got, "a successful reconciliation preserves finished history")
					return
				}
				assert.NotNil(got.FinishedAt, "a no-work pass must not restore an unfinished snapshot")
				assert.Nil(got.CurrentBatch, "the interrupted batch is no longer running")
			})
		})
	}
}

type blockedContextCoverageStore struct {
	ContextWorkStore

	started chan struct{}
	release chan struct{}
}

func (s *blockedContextCoverageStore) SetEmbedGenGroupIfUnchanged(ctx context.Context, stamps []store.EmbedGenStamp, metadata store.EmbedGenMetadataVersion, target int64) (bool, int, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, 0, ctx.Err()
	}
	return s.ContextWorkStore.SetEmbedGenGroupIfUnchanged(ctx, stamps, metadata, target)
}

func TestContextResumeDiagnosticsVisibleDuringFirstCoverageWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	interrupted := errors.New("synthetic interruption after publication")
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Hooks.AfterPublish = func() error { return interrupted }
	})
	f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "Synthetic preserved document")
	_, err := f.run()
	require.ErrorIs(err, interrupted)
	f.deps.Hooks.AfterPublish = nil
	capture := &diagnosticCapture{}
	blocked := &blockedContextCoverageStore{ContextWorkStore: f.store, started: make(chan struct{}, 1), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(blocked.release) }) })
	f.deps.Diagnostics = capture
	f.restartWorker()
	f.worker.deps.Store = blocked
	done := make(chan error, 1)
	go func() { _, err := f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope()); done <- err }()
	snapshot := capture.waitFor(t, func(d vector.EmbeddingDiagnostics) bool {
		return d.CurrentBatch != nil && d.CurrentBatch.Phase == "write"
	})
	assert.Len(blocked.started, 1)
	assert.Zero(snapshot.CurrentBatch.Completed)
	release.Do(func() { close(blocked.release) })
	require.NoError(<-done)
}
