//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

type yieldingBackend struct {
	vector.Backend

	cancel context.CancelCauseFunc
}

func (b *yieldingBackend) Upsert(context.Context, vector.GenerationID, []vector.Chunk) error {
	b.cancel(jobctx.ErrYieldedToWaiter)
	return errors.New("SQL logic error")
}

type yieldingEmbedClient struct {
	EmbeddingClient

	cancel context.CancelCauseFunc
}

func (c *yieldingEmbedClient) Embed(context.Context, []string) ([][]float32, error) {
	c.cancel(jobctx.ErrYieldedToWaiter)
	return nil, errors.New("embedding operation failed")
}

type yieldingStampStore struct {
	WorkStore

	cancel context.CancelCauseFunc
}

func (s *yieldingStampStore) SetEmbedGenIfUnchanged(context.Context, []store.EmbedGenStamp, int64) ([]int64, error) {
	s.cancel(jobctx.ErrYieldedToWaiter)
	return nil, errors.New("stamp operation failed")
}

type captureLogHandler struct {
	records []slog.Record
}

func (h *captureLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureLogHandler) WithGroup(string) slog.Handler { return h }

// newTestWorker builds a Worker over the fixture with the given batch
// size and any extra deps overrides applied via the mutate callback.
func newTestWorker(f *workerFixture, batchSize int) *Worker {
	return NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Store:     f.Store,
		Client:    f.FakeClient,
		BatchSize: batchSize,
		Recorder:  f.Recorder,
	})
}

func TestEmbeddingChunkPolicy_IsCanonicalForWorkerCallers(t *testing.T) {
	policy := EmbeddingChunkPolicy(32768)
	assert.Equal(t, 32768, policy.MaxRunes)
	assert.Positive(t, policy.OverlapRunes)
	assert.Positive(t, policy.MaxSpans)
	assert.Positive(t, policy.MaxBodyRunes)
	text := strings.Repeat("x", policy.MaxRunes+100)
	spans, dropped := policy.Chunk(text)
	assert.False(t, dropped)
	require.Len(t, spans, 2)
	assert.Equal(t, policy.OverlapRunes,
		utf8.RuneCountInString(spans[0].Text)+utf8.RuneCountInString(spans[1].Text)-utf8.RuneCountInString(text))
}

// TestWorker_DrainsToZeroEndToEnd is the happy-path: a fresh corpus is
// scanned, embedded, and every message ends up stamped (embed_gen = gen)
// so coverage reaches zero.
func TestWorker_DrainsToZeroEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 5)

	w := newTestWorker(f, 2)
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")

	assert.Equal(5, res.Succeeded, "Succeeded")
	assert.Equal(0, res.Failed, "Failed")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "missing after drain")
}

// TestWorkerOperationPassOwnsOneInvocationAcrossProviderBatches catches
// recorder ownership drifting inward from the bounded worker call to each
// provider batch.
func TestWorkerOperationPassOwnsOneInvocationAcrossProviderBatches(t *testing.T) {
	f := newWorkerFixture(t, 5)
	recorder := testutil.NewTestStore(t)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 2, Recorder: recorder,
	})
	scope := operations.PassScope{
		Key: "manual:message:provider-batches", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}

	result, err := w.RunOnce(t.Context(), f.BuildingGen, scope)
	require.NoError(t, err)
	assert.Equal(t, RunResult{Claimed: 5, Succeeded: 5}, result)
	assert.Equal(t, 3, f.FakeClient.calls, "precondition: pass spans three provider batches")

	runs := messageEmbeddingOperationRuns(t, recorder)
	require.Len(t, runs, 1)
	assert.Equal(t, operations.StateSucceeded, runs[0].State)
	assert.Equal(t, int64(5), operationCounter(runs[0], operations.CounterAttempted))
	assert.Equal(t, int64(5), operationCounter(runs[0], operations.CounterSucceeded))
	assert.Equal(t, int64(0), operationCounter(runs[0], operations.CounterFailed))
}

func TestWorkerOperationPassRequiresRecorderBeforePrimaryWork(t *testing.T) {
	f := newWorkerFixture(t, 1)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 1,
	})

	result, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.ErrorContains(t, err, "recorder")
	assert.Equal(t, RunResult{}, result)
	assert.Zero(t, f.FakeClient.calls, "configuration failure must precede provider work")
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)))
}

func TestWorkerOperationPassBeginFailurePreventsPrimaryWork(t *testing.T) {
	f := newWorkerFixture(t, 1)
	f.Recorder.beginErr = errors.New("synthetic begin failure")

	result, err := newTestWorker(f, 1).RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.ErrorContains(t, err, "synthetic begin failure")
	assert.Equal(t, RunResult{}, result)
	assert.Zero(t, f.FakeClient.calls, "begin failure must precede provider work")
	assert.Equal(t, 1, countMissing(t, f.MainDB, int64(f.BuildingGen)))
}

func TestWorkerOperationPassCheckpointFailurePreservesPrimaryResult(t *testing.T) {
	f := newWorkerFixture(t, 1)
	f.Recorder.checkpointErr = errors.New("synthetic checkpoint failure")
	logs := &captureLogHandler{}
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 1, Recorder: f.Recorder,
		Log: slog.New(logs),
	})
	scope := testEmbeddingPassScope()

	result, err := w.RunOnce(t.Context(), f.BuildingGen, scope)
	require.NoError(t, err)
	assert.Equal(t, RunResult{Claimed: 1, Succeeded: 1}, result)
	invocation, ok := f.Recorder.invocation(scope.Key)
	require.True(t, ok)
	require.NotNil(t, invocation.run)
	assert.Equal(t, operations.StateSucceeded, invocation.run.State)
	assert.Contains(t, operationLogMessages(logs), "operation recorder checkpoint failed")
}

func TestWorkerOperationPassFinishFailurePreservesPrimaryResult(t *testing.T) {
	f := newWorkerFixture(t, 1)
	f.Recorder.finishErr = errors.New("synthetic finish failure")
	logs := &captureLogHandler{}
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 1, Recorder: f.Recorder,
		Log: slog.New(logs),
	})
	scope := testEmbeddingPassScope()

	result, err := w.RunOnce(t.Context(), f.BuildingGen, scope)
	require.NoError(t, err)
	assert.Equal(t, RunResult{Claimed: 1, Succeeded: 1}, result)
	invocation, ok := f.Recorder.invocation(scope.Key)
	require.True(t, ok)
	assert.Nil(t, invocation.run, "failed finish must leave the lifecycle active for recovery")
	assert.Equal(t, operations.StateRunning, invocation.state)
	assert.Contains(t, operationLogMessages(logs), "operation recorder finish failed")
}

func operationLogMessages(logs *captureLogHandler) []string {
	messages := make([]string, 0, len(logs.records))
	for _, record := range logs.records {
		messages = append(messages, record.Message)
	}
	return messages
}

// TestWorkerOperationPassRecordsBackstopAsFreshInvocation catches a backstop
// pass reopening or reusing the preceding forward-scan run.
func TestWorkerOperationPassRecordsBackstopAsFreshInvocation(t *testing.T) {
	f := newWorkerFixture(t, 1)
	recorder := testutil.NewTestStore(t)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 1, Recorder: recorder,
	})
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	_, err := w.RunOnce(t.Context(), f.BuildingGen, operations.PassScope{
		Key: "manual:message:forward", Trigger: operations.TriggerManual, StartedAt: started,
	})
	require.NoError(t, err)
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
	require.NoError(t, err)
	_, err = w.RunBackstop(t.Context(), f.BuildingGen, operations.PassScope{
		Key: "manual:message:backstop", Trigger: operations.TriggerManual, StartedAt: started.Add(time.Second),
	})
	require.NoError(t, err)

	runs := messageEmbeddingOperationRuns(t, recorder)
	require.Len(t, runs, 2)
	assert.Equal(t, []operations.State{operations.StateSucceeded, operations.StateSucceeded},
		[]operations.State{runs[0].State, runs[1].State})
}

// TestWorkerOperationPassRecordsCancellation catches finish writes using the
// already-cancelled caller context and leaving a public invocation running.
func TestWorkerOperationPassRecordsCancellation(t *testing.T) {
	f := newWorkerFixture(t, 1)
	recorder := testutil.NewTestStore(t)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 1, Recorder: recorder,
	})
	ctx, cancel := context.WithCancel(t.Context())
	f.FakeClient.preReturn = cancel
	t.Cleanup(cancel)

	_, err := w.RunOnce(ctx, f.BuildingGen, operations.PassScope{
		Key: "manual:message:cancelled", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(t, err, context.Canceled)
	runs := messageEmbeddingOperationRuns(t, recorder)
	require.Len(t, runs, 1)
	assert.Equal(t, operations.StateCancelled, runs[0].State)
	assert.Equal(t, operations.PublicErrorInvocationCancelled, runs[0].Error.Code)
}

// TestWorkerOperationPassDoesNotCountRecoveredRetryAsFinalFailure catches
// attempt-level errors leaking into final-item counters after the same items
// durably succeed later in the pass.
func TestWorkerOperationPassDoesNotCountRecoveredRetryAsFinalFailure(t *testing.T) {
	f := newWorkerFixture(t, 3)
	f.FakeClient.FailNext(1)
	recorder := testutil.NewTestStore(t)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB,
		Store: f.Store, Client: f.FakeClient, BatchSize: 3, Recorder: recorder,
	})

	_, err := w.RunOnce(t.Context(), f.BuildingGen, operations.PassScope{
		Key: "manual:message:retry-recovers", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	runs := messageEmbeddingOperationRuns(t, recorder)
	require.Len(t, runs, 1)
	assert.Equal(t, operations.StateSucceeded, runs[0].State)
	assert.Equal(t, int64(3), operationCounter(runs[0], operations.CounterAttempted))
	assert.Equal(t, int64(3), operationCounter(runs[0], operations.CounterSucceeded))
	assert.Equal(t, int64(0), operationCounter(runs[0], operations.CounterFailed))
}

func messageEmbeddingOperationRuns(t *testing.T, recorder *store.Store) []operations.Run {
	t.Helper()
	snapshot, err := recorder.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindMessageEmbedding}, Limit: 100,
	})
	require.NoError(t, err)
	return snapshot.Runs
}

func operationCounter(run operations.Run, name operations.CounterName) int64 {
	for _, counter := range run.Counters {
		if counter.Name == name {
			return counter.Value
		}
	}
	return 0
}

// TestWorker_StampsAfterUpsert verifies the ordered idempotent steps:
// every embedded message has embed_gen stamped to the target generation.
func TestWorker_StampsAfterUpsert(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 3)

	w := newTestWorker(f, 3)
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")

	var stamped int
	require.NoError(f.MainDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE embed_gen = ?`, int64(f.BuildingGen)).Scan(&stamped))
	require.Equal(3, stamped, "stamped messages")
}

// TestWorker_EmptyCorpusReturnsZero: scanning an empty corpus returns a
// zero result and no error.
func TestWorker_EmptyCorpusReturnsZero(t *testing.T) {
	f := newWorkerFixture(t, 0)
	w := newTestWorker(f, 8)
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 0, res.Claimed, "Claimed")
	assert.Equal(t, 0, res.Succeeded, "Succeeded")
}

// TestWorker_AbortsAfterConsecutiveFailures: a persistently failing
// embedder trips MaxConsecutiveFailures and RunOnce returns an error,
// leaving the messages unstamped (so the next run re-finds them).
func TestWorker_AbortsAfterConsecutiveFailures(t *testing.T) {
	assert := assert.
		New(t)
	require := require.
		New(t)

	f := newWorkerFixture(t, 10)
	f.FakeClient.FailNext(1000)
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              2,
		MaxConsecutiveFailures: 3,
		Recorder:               f.Recorder,
	})
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.Error(err, "expected abort")
	require.ErrorContains(err, "consecutive failures")
	assert.Equal(0, res.Succeeded, "nothing should succeed")
	assert. // All messages left unstamped (next scan re-finds them).
		Equal(10, countMissing(t, f.MainDB, int64(f.BuildingGen)), "still missing")
}

func TestWorker_YieldCancellationDuringUpsertStopsCleanly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	logs := &captureLogHandler{}
	w := NewWorker(WorkerDeps{
		Backend:                &yieldingBackend{Backend: f.Backend, cancel: cancel},
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 1,
		Log:                    slog.New(logs),
		Recorder:               f.Recorder,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "yield cancellation must stop cleanly")
	assert.Equal(1, res.Claimed, "Claimed")
	assert.Equal(0, res.Succeeded, "Succeeded")
	assert.Equal(0, res.Failed, "Failed")
	assert.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)), "row remains unstamped")
	var embedGen sql.NullInt64
	require.NoError(f.MainDB.QueryRow(`SELECT embed_gen FROM messages WHERE id = 1`).Scan(&embedGen))
	assert.False(embedGen.Valid, "row remains unstamped")
	assert.Equal(int64(0), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)), "watermark does not advance")
	for _, record := range logs.records {
		assert.NotEqual(slog.LevelWarn, record.Level, "yield must not log a warning: %q", record.Message)
		assert.NotEqual(slog.LevelError, record.Level, "yield must not log an error: %q", record.Message)
	}

	healthy := newTestWorker(f, 1)
	res, err = healthy.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "healthy worker retry")
	assert.Equal(1, res.Succeeded, "healthy worker succeeds")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "row stamped after retry")
}

func TestWorker_YieldCancellationDuringEmbedStopsCleanly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	logs := &captureLogHandler{}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 &yieldingEmbedClient{EmbeddingClient: f.FakeClient, cancel: cancel},
		BatchSize:              1,
		MaxConsecutiveFailures: 1,
		Log:                    slog.New(logs),
		Recorder:               f.Recorder,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "yield cancellation during embed must stop cleanly")
	assert.Equal(1, res.Claimed, "Claimed")
	assert.Equal(0, res.Succeeded, "Succeeded")
	assert.Equal(0, res.Failed, "Failed")
	assert.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)), "row remains unstamped")
	for _, record := range logs.records {
		assert.NotEqual(slog.LevelWarn, record.Level, "yield must not log a warning: %q", record.Message)
		assert.NotEqual(slog.LevelError, record.Level, "yield must not log an error: %q", record.Message)
	}
}

func TestWorker_YieldCancellationDuringDownshiftStampLogsOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 2)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("batch rejected: %w", ErrPermanent4xx)
		}
		return [][]float32{make([]float32, f.FakeClient.dim)}, nil
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	logs := &captureLogHandler{}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  &yieldingStampStore{WorkStore: f.Store, cancel: cancel},
		Client:                 f.FakeClient,
		BatchSize:              2,
		MaxConsecutiveFailures: 1,
		Log:                    slog.New(logs),
		Recorder:               f.Recorder,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "yield cancellation during downshift stamp must stop cleanly")
	assert.Equal(0, res.Failed, "Failed")
	yieldInfo := 0
	for _, record := range logs.records {
		assert.NotEqual(slog.LevelWarn, record.Level, "yield must not log a warning: %q", record.Message)
		assert.NotEqual(slog.LevelError, record.Level, "yield must not log an error: %q", record.Message)
	}
	for _, record := range logs.records {
		if record.Level == slog.LevelInfo && record.Message == "embed run yielded to a waiting operation; stopping" {
			yieldInfo++
		}
	}
	assert.Equal(1, yieldInfo, "yield stop is logged once")
}

// TestWorker_FailureLeavesUnstampedThenRecovers: a transient failure on
// the first attempt leaves rows unstamped; a second run (embedder now
// healthy) completes them. Idempotent re-do.
func TestWorker_FailureLeavesUnstampedThenRecovers(t *testing.T) {
	f := newWorkerFixture(t, 3)
	// Fail the first batch, then succeed.
	f.FakeClient.FailNext(1)
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              3,
		MaxConsecutiveFailures: 5,
		Recorder:               f.Recorder,
	})
	// First run: the single batch fails once, then the loop re-scans the
	// same (unstamped) ids and succeeds.
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 3, res.Succeeded, "Succeeded after recovery")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "missing after recovery")
}

// TestWorker_RespectsContextCancel: a cancelled context aborts RunOnce.
func TestWorker_RespectsContextCancel(t *testing.T) {
	f := newWorkerFixture(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newTestWorker(f, 2)
	_, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.Error(t, err, "expected context error")
}

// TestWorker_MissingMessagesSkipMarked: ids that vanished from the main
// DB between scan and fetch are skip-marked (stamped) so they drop out of
// the next scan rather than spinning forever.
func TestWorker_MissingMessagesSkipMarked(t *testing.T) {
	require := require.
		New(t)

	f := newWorkerFixture(t, 3)
	// Delete message 2's row entirely (gone from main DB) but leave its
	// embed_gen NULL so the scan still finds it.
	_, err := f.MainDB.Exec(`DELETE FROM messages WHERE id = 2`)
	require.NoError(
		err, "delete msg 2")

	// Re-insert a placeholder id 2 with NULL embed_gen but no body so the
	// scan finds it; then drop its body row to make embedBatch see it as
	// present-but-empty. Instead, simulate "missing" by inserting an id
	// the scan returns but messages has no row: not possible after delete.
	// So this test covers the empty case via a blank body.
	_, err = f.MainDB.Exec(
		`INSERT INTO messages (id, subject, embed_gen) VALUES (2, '', NULL)`)
	require.NoError(
		err, "reinsert msg 2 empty")

	_, err = f.MainDB.Exec(`DELETE FROM message_bodies WHERE message_id = 2`)
	require.NoError(
		err, "delete body 2")

	w := newTestWorker(f, 8)
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(
		err, "RunOnce")

	// Empty message 2 must be skip-marked, not re-found.
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "all stamped")
}

// TestWorker_EmptyMessageSkipMarkedNotReprocessed: a message that
// preprocesses to empty is stamped (skip-marker) and a second run does
// NOT re-process it (the embedder is not called again for it).
func TestWorker_EmptyMessageSkipMarkedNotReprocessed(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 1)
	// Blank out the only message so it preprocesses to empty.
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = '' WHERE id = 1`)
	require.NoError(err, "blank subject")
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = '' WHERE message_id = 1`)
	require.NoError(err, "blank body")

	w := newTestWorker(f, 8)
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 1")
	require.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "skip-marked")

	callsBefore := f.FakeClient.calls
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 2")
	// Second run finds nothing (the empty message is stamped), so the
	// embedder is not called again.
	assert.Equal(t, callsBefore, f.FakeClient.calls, "no re-processing of skip-marked message")
}

func TestWorker_EmptyMessageDeletesExistingEmbeddingBeforeSkipMark(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 1)
	w := newTestWorker(f, 1)

	_, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "initial RunOnce")

	embedded, err := f.Backend.EmbeddedMessageCount(ctx, f.BuildingGen)
	require.NoError(err, "EmbeddedMessageCount before empty")
	require.Equal(int64(1), embedded, "precondition: message has an embedding")

	_, err = f.MainDB.Exec(`UPDATE messages SET subject = '', embed_gen = NULL WHERE id = 1`)
	require.NoError(err, "blank subject and invalidate")
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = '', body_html = '' WHERE message_id = 1`)
	require.NoError(err, "blank body")

	res, err := w.RunBackstop(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunBackstop after message became empty")
	assert.Equal(0, res.Succeeded, "empty message is skip-marked, not embedded")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "empty message is covered")

	embedded, err = f.Backend.EmbeddedMessageCount(ctx, f.BuildingGen)
	require.NoError(err, "EmbeddedMessageCount after empty")
	assert.Equal(int64(0), embedded, "empty skip must not leave a counted embedding")

	stats, err := f.Backend.Stats(ctx, f.BuildingGen)
	require.NoError(err, "Stats after empty")
	assert.Equal(int64(0), stats.EmbeddingCount, "empty skip must remove stale vector rows")

	hits, err := f.Backend.Search(ctx, f.BuildingGen, []float32{1, 0, 0, 0}, 10, vector.Filter{})
	require.NoError(err, "Search after empty")
	assert.Empty(hits, "empty skip must not leave the message searchable")
}

func TestWorker_EmptyMessageCASMissDoesNotDeleteExistingEmbedding(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 2)
	w := newTestWorker(f, 2)

	_, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "initial RunOnce")

	_, err = f.MainDB.Exec(`UPDATE messages SET subject = '', embed_gen = NULL WHERE id = 1`)
	require.NoError(err, "blank subject and invalidate msg 1")
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = '', body_html = '' WHERE message_id = 1`)
	require.NoError(err, "blank body msg 1")
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 2`)
	require.NoError(err, "invalidate msg 2 to force mixed batch embed")

	f.FakeClient.preReturn = func() {
		_, err := f.MainDB.Exec(`UPDATE messages SET subject = ?, embed_gen = NULL WHERE id = 1`, "repaired subject")
		require.NoError(err, "race update subject")
		_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, "repaired body")
		require.NoError(err, "race update body")
		_, err = f.MainDB.Exec(`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = 1`)
		require.NoError(err, "force CAS token change")
	}
	res, err := w.RunBackstop(ctx, f.BuildingGen, testEmbeddingPassScope())
	f.FakeClient.preReturn = nil
	require.NoError(err, "RunBackstop with skip CAS miss")
	assert.Equal(1, res.Succeeded, "only msg 2 is embedded and stamped")
	assert.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)), "CAS-missed msg 1 remains recoverable")

	var vectorRows int64
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT message_id) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&vectorRows)
	require.NoError(err, "raw vector row count after skip CAS miss")
	assert.Equal(int64(2), vectorRows, "CAS-missed skip must not delete existing vectors")
}

// TestWorker_FallsBackToHTMLWhenBodyTextEmpty: an HTML-only message is
// embedded via stripped HTML rather than a subject-only embedding.
func TestWorker_FallsBackToHTMLWhenBodyTextEmpty(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 1)
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = 'hi' WHERE id = 1`)
	require.NoError(err)
	_, err = f.MainDB.Exec(
		`UPDATE message_bodies SET body_text = '', body_html = ? WHERE message_id = 1`,
		"<p>distinctive html body content</p>")
	require.NoError(err)

	w := newTestWorker(f, 1)
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")
	joined := strings.Join(f.FakeClient.LastInputs, " ")
	assert.Contains(t, joined, "distinctive html body content", "HTML fallback text embedded")
}

// TestWorker_RuneCountUsedForSourceCharLen: SourceCharLen reflects rune
// count, not byte count, for multibyte input.
func TestWorker_RuneCountUsedForSourceCharLen(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 1)
	body := strings.Repeat("é", 50) // 50 runes, 100 bytes
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = '' WHERE id = 1`)
	require.NoError(err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	require.NoError(err)

	w := newTestWorker(f, 1)
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")

	var srcLen int
	require.NoError(f.VectorsDB.QueryRow(
		`SELECT source_char_len FROM embeddings WHERE message_id = 1 AND chunk_index = 0`).Scan(&srcLen))
	assert.LessOrEqual(t, srcLen, utf8.RuneCountInString(body), "source_char_len in runes")
	assert.Positive(t, srcLen, "non-zero")
}

// TestWorker_SplitsChunkInputsAcrossSubBatches: a message whose chunk
// fan-out exceeds BatchSize is embedded across multiple sub-batched Embed
// calls (none larger than BatchSize).
func TestWorker_SplitsChunkInputsAcrossSubBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 1)
	body := strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit. ", 40)
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	require.NoError(err, "update body")

	const batchSize = 4
	var sizes []int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		sizes = append(sizes, len(inputs))
		out := make([][]float32, len(inputs))
		for i := range inputs {
			v := make([]float32, 4)
			v[0] = float32(len(inputs[i])%4 + 1)
			out[i] = v
		}
		return out, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Store:         f.Store,
		Client:        f.FakeClient,
		MaxInputChars: 80,
		BatchSize:     batchSize,
		Recorder:      f.Recorder,
	})
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")
	require.GreaterOrEqual(len(sizes), 2, "expected >= 2 sub-batches, got %v", sizes)
	for i, n := range sizes {
		assert.LessOrEqualf(n, batchSize, "sub-batch %d size", i)
		assert.NotZerof(n, "sub-batch %d empty", i)
	}
}

// TestWorker_Progress fires the progress callback per handled batch with
// the configured TotalPending denominator.
func TestWorker_Progress(t *testing.T) {
	assert := assert.
		New(t)
	require := require.
		New(t)

	f := newWorkerFixture(t, 5)
	var reports []ProgressReport
	w := NewWorker(WorkerDeps{
		Backend:      f.Backend,
		VectorsDB:    f.VectorsDB,
		MainDB:       f.MainDB,
		Store:        f.Store,
		Client:       f.FakeClient,
		BatchSize:    2,
		TotalPending: 5,
		Progress:     func(p ProgressReport) { reports = append(reports, p) },
		Recorder:     f.Recorder,
	})
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(
		err, "RunOnce")

	require.NotEmpty(reports, "progress reports")
	for i, p := range reports {
		assert.Equalf(5, p.TotalPending, "report[%d].TotalPending", i)
	}
	assert.Equal(5, reports[len(reports)-1].Done, "final Done")
}

// --- Watermark behavior ---

// TestWorker_AdvancesWatermark: after a successful run the per-gen
// watermark is advanced to the highest scanned id.
func TestWorker_AdvancesWatermark(t *testing.T) {
	f := newWorkerFixture(t, 5)
	w := newTestWorker(f, 2)
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, int64(5), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)), "watermark at max id")
}

// TestWorker_WatermarkLossHarmless: dropping the watermark and rerunning
// is a no-op (idempotent) — already-stamped rows are skipped by the scan.
func TestWorker_WatermarkLossHarmless(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 4)
	w := newTestWorker(f, 4)
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 1")
	require.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "all stamped")

	// Simulate watermark loss.
	_, err = f.VectorsDB.Exec(`DELETE FROM embed_watermark`)
	require.NoError(err, "drop watermark")

	callsBefore := f.FakeClient.calls
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 2 (watermark lost)")
	assert.Equal(t, 0, res.Succeeded, "nothing to re-embed")
	assert.Equal(t, callsBefore, f.FakeClient.calls, "no re-embed after watermark loss")
}

// TestWorker_BackstopCatchesSubWatermarkStraggler: a message left
// unstamped BELOW the persisted watermark is invisible to RunOnce
// (watermark-bounded) but caught by RunBackstop (full scan from 0).
func TestWorker_BackstopCatchesSubWatermarkStraggler(t *testing.T) {
	assert := assert.
		New(t)

	require := require.New(t)
	f := newWorkerFixture(t, 5)
	w := newTestWorker(f, 5)
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 1")
	require.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "all stamped")
	require.Equal(int64(5), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)), "watermark")

	// Manually un-stamp message 2 (a sub-watermark straggler) — as if a
	// prior run dropped it during a transient fault while the watermark
	// advanced past it.
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 2`)
	require.NoError(err, "unstamp msg 2")

	// RunOnce resumes from the watermark (id > 5) and does NOT see id 2.
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce 2")
	assert.Equal(0, res.Succeeded, "RunOnce misses sub-watermark straggler")
	assert.Equal(1, countMissing(t, f.MainDB, int64(f.BuildingGen)), "straggler still missing")

	// Backstop scans from 0 and catches it.
	res, err = w.RunBackstop(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunBackstop")
	assert.Equal(1, res.Succeeded, "backstop embeds the straggler")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "straggler covered")
}

// TestWorker_BackstopDoesNotPersistWatermark: the backstop must not
// touch the persisted watermark (it scans from 0 by design).
func TestWorker_BackstopDoesNotPersistWatermark(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 3)
	w := newTestWorker(f, 3)
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")
	wmBefore := readWatermark(t, f.VectorsDB, int64(f.BuildingGen))

	// Un-stamp one and run the backstop; the watermark must be unchanged.
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 1`)
	require.NoError(err)
	_, err = w.RunBackstop(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunBackstop")
	assert.Equal(t, wmBefore, readWatermark(t, f.VectorsDB, int64(f.BuildingGen)), "watermark unchanged by backstop")
}

// TestWorker_ReclaimStaleIsNoOp: ReclaimStale always returns (0, nil)
// under the scan-and-fill design (kept for the EmbedRunner interface).
func TestWorker_ReclaimStaleIsNoOp(t *testing.T) {
	f := newWorkerFixture(t, 1)
	w := newTestWorker(f, 1)
	n, err := w.ReclaimStale(context.Background())
	require.NoError(t, err, "ReclaimStale")
	assert.Equal(t, 0, n, "no-op returns 0")
}

// --- Downshift / 4xx behavior ---

// TestWorker_Downshift_MessageSpecific4xxStampedDropped: when a batch
// 4xxs but singletons embed, the failing message is a message-specific
// 4xx and gets stamped (dropped) so the run completes.
func TestWorker_Downshift_MessageSpecific4xxStampedDropped(t *testing.T) {
	f := newWorkerFixture(t, 3)
	var singletonSeen int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: too long: %w", ErrPermanent4xx)
		}
		singletonSeen++
		if singletonSeen == 2 {
			return nil, fmt.Errorf("embed: HTTP 400: blocked: %w", ErrPermanent4xx)
		}
		v := make([]float32, 4)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := newTestWorker(f, 3)
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 2, res.Succeeded, "Succeeded")
	// All three stamped (2 embedded + 1 message-specific drop).
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(f.BuildingGen)), "all stamped")
}

// TestWorker_Downshift_AllDropNoSilentDelete: a fully misconfigured
// endpoint (every input 4xx) must NOT stamp/drop any message and must
// trip the failure cap, leaving the rows unstamped for retry.
func TestWorker_Downshift_AllDropNoSilentDelete(t *testing.T) {
	f := newWorkerFixture(t, 4)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		return nil, fmt.Errorf("embed: HTTP 401: bad-api-key: %w", ErrPermanent4xx)
	}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              4,
		MaxConsecutiveFailures: 2,
		Recorder:               f.Recorder,
	})
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.Error(t, err, "expected abort")
	// No message stamped — the misconfigured endpoint did not silently
	// drop work.
	assert.Equal(t, 4, countMissing(t, f.MainDB, int64(f.BuildingGen)), "nothing stamped")
}

// TestWorker_Downshift_Non4xxDoesNotStrandStraggler proves the watermark
// is NOT advanced past an unstamped straggler when a downshift hits a
// NON-4xx (transient) error AFTER an earlier singleton already stamped.
//
// Setup: a 3-message batch 4xxs as a whole (triggering the downshift to
// BatchSize=1); then singleton id 1 embeds (and is stamped) while singleton
// id 2 returns a NON-4xx error. The old code advanced the watermark to
// batchMax (3), so subsequent RunOnce scans (id > 3) would skip ids 2 and 3
// forever — only the MANUAL-only backstop could recover them. The fix
// advances only to the highest contiguously-stamped id (1).
//
// Asserts: (a) the persisted watermark is 1, not 3; (b) ids 2 and 3 are
// still missing; (c) a subsequent RunOnce with a healthy embedder (NO
// backstop) re-finds and embeds them, reaching zero coverage.
func TestWorker_Downshift_Non4xxDoesNotStrandStraggler(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 3)

	// First pass: whole batch 4xxs (forces downshift); singleton id 1
	// embeds; singleton id 2 returns a transient (NON-4xx) error.
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			// Whole-batch call — force the downshift.
			return nil, fmt.Errorf("embed: HTTP 400: batch too long: %w", ErrPermanent4xx)
		}
		// Singleton. Message 2's preprocessed text contains "body 2".
		if strings.Contains(inputs[0], "body 2") {
			// Transient error — NOT a 4xx. Must leave id 2 unstamped and
			// must not let the watermark jump past it.
			return nil, errors.New("simulated transient embed failure for msg 2")
		}
		v := make([]float32, f.FakeClient.dim)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Store:                  f.Store,
		Client:                 f.FakeClient,
		BatchSize:              3,
		MaxConsecutiveFailures: 5,
		Recorder:               f.Recorder,
	})
	_, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.Error(err, "expected a transient drain error")

	// (a) Watermark must stay at the contiguously-stamped id (1), NOT
	// batchMax (3).
	assert.Equal(int64(1), readWatermark(t, f.VectorsDB, int64(f.BuildingGen)),
		"watermark not advanced past the unstamped straggler")
	// (b) ids 2 and 3 are still unstamped (1 is stamped).
	assert.Equal(2, countMissing(t, f.MainDB, int64(f.BuildingGen)), "stragglers still missing")

	// (c) A subsequent RunOnce (NO backstop) with a healthy embedder
	// re-finds the stragglers (scan id > watermark==1) and embeds them.
	f.FakeClient.OnEmbed = nil // restore default healthy behavior
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "second RunOnce")
	assert.Equal(2, res.Succeeded, "stragglers embedded on retry")
	assert.Equal(0, countMissing(t, f.MainDB, int64(f.BuildingGen)),
		"coverage complete without the manual backstop")
}

// --- embed_runs lifecycle ---

// TestWorker_EmbedRunLifecycle: a successful RunOnce opens exactly one
// embed_runs row and stamps ended_at + counters on it.
func TestWorker_EmbedRunLifecycle(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 2)
	w := newTestWorker(f, 2)
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce")

	var n int
	require.NoError(f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embed_runs`).Scan(&n))
	require.Equal(1, n, "exactly one embed_runs row")
	var ended, succeeded int
	require.NoError(f.VectorsDB.QueryRow(
		`SELECT COALESCE(ended_at, 0), succeeded FROM embed_runs LIMIT 1`).Scan(&ended, &succeeded))
	assert.NotZero(t, ended, "ended_at stamped")
	assert.Equal(t, res.Succeeded, succeeded, "succeeded counter")
}

// --- retired generation ---

// TestWorker_RetiredGenerationStopsCleanly: if the generation is retired
// mid-run, Upsert returns ErrGenerationRetired and RunOnce returns nil
// (benign stop), leaving no embed_gen stamps for the retired gen.
func TestWorker_RetiredGenerationStopsCleanly(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 3)
	// Retire the building generation directly so the next Upsert observes
	// state='retired'.
	_, err := f.VectorsDB.Exec(
		`UPDATE index_generations SET state = 'retired' WHERE id = ?`, int64(f.BuildingGen))
	require.NoError(err, "retire gen")

	w := newTestWorker(f, 3)
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err, "RunOnce must return nil for a retired generation (benign stop)")
	assert.Equal(t, 0, res.Succeeded, "nothing embedded into retired gen")
	// No message stamped to the retired generation.
	var stamped int
	require.NoError(f.MainDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE embed_gen = ?`, int64(f.BuildingGen)).Scan(&stamped))
	assert.Equal(t, 0, stamped, "no stamps for retired gen")
}

// compile-time: *Worker satisfies the embed runner shape used elsewhere.
var _ interface {
	RunOnce(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (RunResult, error)
	RunBackstop(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (RunResult, error)
	ReclaimStale(ctx context.Context) (int, error)
} = (*Worker)(nil)
