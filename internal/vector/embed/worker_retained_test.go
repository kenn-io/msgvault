//go:build sqlite_vec

package embed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
)

// upsertFailingBackend fails publication while provider output remains checkpointed.
type upsertFailingBackend struct {
	vector.Backend

	fail bool
}

func (b *upsertFailingBackend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	if b.fail {
		return errors.New("simulated upsert failure")
	}
	return b.Backend.Upsert(ctx, gen, chunks)
}

const retainedLongBody = "alpha sentence number one. alpha sentence number two. alpha sentence number three. alpha sentence four."

func newRetainingWorker(f *workerFixture, backend vector.Backend, batchSize int) *Worker {
	return NewWorker(WorkerDeps{
		Backend: backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB, Store: f.Store,
		Client: f.FakeClient, BatchSize: batchSize, MaxInputChars: 50,
		MaxConsecutiveFailures: 1, Recorder: f.Recorder,
	})
}

func countChunks(t *testing.T, f *workerFixture, id int64) int {
	t.Helper()
	var n int
	require.NoError(t, f.VectorsDB.QueryRow(
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = ?`,
		int64(f.BuildingGen), id).Scan(&n))
	return n
}

func TestWorker_CheckpointFailureStopsLaterRequests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		batchSize int
		when      string
	}{
		{name: "first entry", batchSize: 1},
		{name: "second entry in response", batchSize: 10, when: "WHEN NEW.input_key = '1'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t, 1)
			_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, retainedLongBody)
			require.NoError(t, err)
			_, err = f.VectorsDB.Exec(`CREATE TRIGGER fail_checkpoint BEFORE INSERT ON embedding_checkpoints ` + tc.when + `
				BEGIN SELECT RAISE(ABORT, 'checkpoint storage unavailable'); END`)
			require.NoError(t, err)
			requests := 0
			f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
				requests++
				return fakeVectors(inputs), nil
			}
			ctx := jobctx.WithProgress(t.Context())
			_, err = newRetainingWorker(f, f.Backend, tc.batchSize).RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
			require.ErrorContains(t, err, "checkpoint storage unavailable")
			assert.Equal(t, 1, requests, "a failed save stops before the next provider request")
			assert.False(t, jobctx.HasProgress(ctx))
			assert.Zero(t, countChunks(t, f, 1), "unfinished messages remain unpublished")
		})
	}
}

func TestWorker_CheckpointedOutputIsReusedUntilInputChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newWorkerFixture(t, 1)
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, retainedLongBody)
	require.NoError(err)
	var requested []string
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		var saved int
		require.NoError(f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints`).Scan(&saved))
		assert.Equal(len(requested), saved, "earlier requests are saved before the next starts")
		requested = append(requested, inputs...)
		return fakeVectors(inputs), nil
	}
	backend := &upsertFailingBackend{Backend: f.Backend, fail: true}
	w := newRetainingWorker(f, backend, 1)

	passCtx := jobctx.WithProgress(context.Background())
	_, err = w.RunOnce(passCtx, f.BuildingGen, testEmbeddingPassScope())
	require.Error(err, "the upsert failure stops the pass")
	require.Greater(len(requested), 1, "the message spans several requests")
	assert.True(jobctx.HasProgress(passCtx), "the completed request is durably checkpointed")
	first := slices.Clone(requested)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		requested = append(requested, inputs...)
		return fakeVectors(inputs), nil
	}
	w = newRetainingWorker(f, backend, 1)

	requested = nil
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.Error(err)
	assert.Empty(requested, "unchanged input reuses the checkpointed output")

	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`,
		strings.ReplaceAll(retainedLongBody, "alpha", "omega"))
	require.NoError(err)
	_, err = w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.Error(err, "the changed input also fails to publish")
	require.NotEmpty(requested, "changed input is requested again")
	for _, input := range requested {
		assert.NotContains(first, input, "output for the old input is not reused")
	}
	var checkpointCount int
	require.NoError(f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints`).Scan(&checkpointCount))
	assert.Equal(len(requested), checkpointCount, "only the current input revision remains checkpointed")
	changed := slices.Clone(requested)

	requested = nil
	backend.fail = false
	res, err := w.RunOnce(context.Background(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(1, res.Succeeded)
	assert.Empty(requested, "the new input publishes from checkpointed output")
	assert.Equal(len(changed), countChunks(t, f, 1), "every chunk of the new input is published")
	var saved int
	require.NoError(f.VectorsDB.QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints`).Scan(&saved))
	assert.Zero(saved, "published checkpoints are removed")
}

// An endpoint-wide 4xx during singleton recovery must not count a message
// published from checkpointed output as proof that the endpoint works; otherwise
// the rejected siblings are stamped as dropped and their vectors deleted.
func TestWorker_Downshift_CheckpointedOutputDoesNotDropRejectedSiblings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 3)
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, retainedLongBody)
	require.NoError(err)
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = ? WHERE id IN (2, 3)`, int64(f.BuildingGen))
	require.NoError(err)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) { return fakeVectors(inputs), nil }
	backend := &upsertFailingBackend{Backend: f.Backend, fail: true}
	w := newRetainingWorker(f, backend, 3)
	_, err = w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.Error(err, "message 1's output is retained but not stored")

	// Messages 2 and 3 have vectors from earlier content and now need work.
	for _, id := range []int64{2, 3} {
		require.NoError(f.Backend.Upsert(ctx, f.BuildingGen, []vector.Chunk{{
			MessageID: id, Vector: []float32{1, 0, 0, 0}, SourceCharLen: 6, ChunkCharEnd: 6,
		}}))
	}
	_, err = f.MainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id IN (2, 3)`)
	require.NoError(err)
	backend.fail = false
	var requested []string
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		requested = append(requested, inputs...)
		return nil, fmt.Errorf("embed: HTTP 401: bad-api-key: %w", ErrPermanent4xx)
	}
	_, err = w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.ErrorIs(err, ErrPermanent4xx)

	for _, input := range requested {
		assert.NotContains(input, "alpha", "message 1 publishes from checkpointed output")
	}
	assert.Equal(2, countMissing(t, f.MainDB, int64(f.BuildingGen)), "rejected siblings stay retryable")
	assert.Positive(countChunks(t, f, 1), "checkpointed output is published")
	assert.Equal(1, countChunks(t, f, 2), "message 2 keeps its vector")
	assert.Equal(1, countChunks(t, f, 3), "message 3 keeps its vector")
}

var errProviderUnavailable = errors.New("provider unavailable")

// yieldDuringEmbed models the scheduler asking a pass to yield while a
// provider request is in flight. With cancel set, the runtime limit also
// expires before the request returns, so its error carries the scheduler's
// cancellation alongside any independent failure.
type yieldDuringEmbed struct {
	name   string
	cancel bool
}

var yieldDuringEmbedCases = []yieldDuringEmbed{
	{name: "within grace"},
	{name: "cancelled after grace", cancel: true},
}

func (y yieldDuringEmbed) context(t *testing.T) (context.Context, func(error) error) {
	t.Helper()
	ctx, requestYield := jobctx.WithPreemption(context.Background())
	ctx, cancel := context.WithCancelCause(ctx)
	t.Cleanup(func() { cancel(nil) })
	return ctx, func(err error) error {
		requestYield()
		if !y.cancel {
			return err
		}
		cancel(jobctx.ErrYieldedToWaiter)
		return errors.Join(context.Canceled, err)
	}
}

func TestWorker_ProviderErrorDuringYieldIsReturned(t *testing.T) {
	for _, count := range []int{1, 2} {
		for _, tc := range yieldDuringEmbedCases {
			t.Run(fmt.Sprintf("%d messages/%s", count, tc.name), func(t *testing.T) {
				f := newWorkerFixture(t, count)
				ctx, yield := tc.context(t)
				f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
					if len(inputs) > 1 {
						return nil, fmt.Errorf("embed: HTTP 400: batch rejected: %w", ErrPermanent4xx)
					}
					return nil, yield(errProviderUnavailable)
				}
				_, err := newRetainingWorker(f, f.Backend, count).RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
				require.ErrorIs(t, err, errProviderUnavailable, "provider failures survive both initial requests and singleton recovery")
				require.NotErrorIs(t, err, context.Canceled, "scheduler cancellation is not reported")
				assert.Equal(t, count, countMissing(t, f.MainDB, int64(f.BuildingGen)))
			})
		}
	}
}

func TestWorker_PlainYieldDuringEmbedStopsCleanly(t *testing.T) {
	for _, tc := range yieldDuringEmbedCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t, 2)
			ctx, yield := tc.context(t)
			requests := 0
			f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
				requests++
				if err := yield(nil); err != nil {
					return nil, err
				}
				return fakeVectors(inputs), nil
			}
			w := newRetainingWorker(f, f.Backend, 1)
			_, err := w.RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
			require.NoError(t, err, "a yield without an independent failure is not an error")
			assert.Equal(t, 1, requests, "the next message waits for another pass")
		})
	}
}

func fakeVectors(inputs []string) [][]float32 {
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		out[i] = []float32{float32(len(input)%4 + 1), 0, 0, 0}
	}
	return out
}

// A backstop interrupted inside a long message keeps its completed requests
// while the forward scan handles mail that arrived in between.
func TestWorker_BackstopOutputSurvivesForwardScan(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 1)
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = NULL WHERE id = 1`)
	require.NoError(err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`,
		strings.Repeat("a", 20)+strings.Repeat("b", 20))
	require.NoError(err)
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB, Store: f.Store,
		Client: f.FakeClient, BatchSize: 1, MaxInputChars: 20, Recorder: f.Recorder,
	})
	require.NoError(w.wm.SetWatermark(t.Context(), f.BuildingGen, 1))

	var requested [][]string
	ctx, cancel := context.WithCancelCause(t.Context())
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		requested = append(requested, slices.Clone(inputs))
		if len(requested) == 2 {
			cancel(jobctx.ErrRunBudgetExceeded)
			return nil, context.Canceled
		}
		return fakeVectors(inputs), nil
	}
	_, err = w.RunBackstop(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.ErrorIs(err, context.Canceled)
	require.Equal([][]string{{strings.Repeat("a", 20)}, {strings.Repeat("b", 20)}}, requested)

	_, err = f.MainDB.Exec(`INSERT INTO messages (id, subject) VALUES (2, NULL)`)
	require.NoError(err)
	_, err = f.MainDB.Exec(`INSERT INTO message_bodies (message_id, body_text) VALUES (2, 'forward-only')`)
	require.NoError(err)
	requested = nil
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		requested = append(requested, slices.Clone(inputs))
		return fakeVectors(inputs), nil
	}
	_, err = w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(t, [][]string{{"forward-only"}}, requested, "the forward scan handles the new message")

	requested = nil
	res, err := w.RunBackstop(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err)
	assert.True(t, res.ScanComplete)
	assert.Equal(t, [][]string{{strings.Repeat("b", 20)}}, requested,
		"the backstop reuses its completed request and sends only the interrupted one")
	assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)))
}

// A request of the same batch that succeeded before a sibling's rejection is
// fresh evidence that the endpoint works, even though singleton recovery
// publishes that message from checkpointed output.
func TestWorker_Downshift_EarlierRequestInBatchProvesEndpointHealth(t *testing.T) {
	require := require.New(t)
	f := newWorkerFixture(t, 2)
	_, err := f.MainDB.Exec(`UPDATE messages SET subject = NULL`)
	require.NoError(err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, strings.Repeat("a", 40))
	require.NoError(err)
	_, err = f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 2`, strings.Repeat("b", 10))
	require.NoError(err)
	var requested [][]string
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		requested = append(requested, slices.Clone(inputs))
		if strings.Contains(strings.Join(inputs, ""), "b") {
			return nil, fmt.Errorf("embed: HTTP 400: input rejected: %w", ErrPermanent4xx)
		}
		return fakeVectors(inputs), nil
	}
	w := NewWorker(WorkerDeps{
		Backend: f.Backend, VectorsDB: f.VectorsDB, MainDB: f.MainDB, Store: f.Store,
		Client: f.FakeClient, BatchSize: 2, MaxInputChars: 20, Recorder: f.Recorder,
	})
	res, err := w.RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(err)
	assert.Equal(t, [][]string{
		{strings.Repeat("a", 20), strings.Repeat("a", 20)},
		{strings.Repeat("b", 10)},
		{strings.Repeat("b", 10)},
	}, requested, "recovery requests only the rejected message again")
	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, 2, countChunks(t, f, 1))
	assert.Zero(t, countMissing(t, f.MainDB, int64(f.BuildingGen)), "the rejected message is dropped as message-specific")
}

func TestPersonWorkerReturnedResponseSurvivesRunCancellation(t *testing.T) {
	f := newWorkerFixture(t, 0)
	backend, ok := f.Backend.(vector.PersonBackend)
	require.True(t, ok)
	source := newPersonWorkerSource(personDocument(1, "revision-one", "Synthetic One"))
	ctx, cancel := context.WithCancel(t.Context())
	client := &contextSemanticFake{dim: 4, before: cancel}
	newWorker := func() *PersonWorker {
		return NewPersonWorker(PersonWorkerDeps{Store: source, Backend: backend, Client: client,
			Gate: allowSemanticPersonGate(), Recorder: f.Recorder, BatchSize: 1})
	}
	_, err := newWorker().RunOnce(ctx, f.BuildingGen, testEmbeddingPassScope())
	require.ErrorIs(t, err, context.Canceled)
	revisions, err := backend.ListPersonRevisions(t.Context(), f.BuildingGen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{1: "revision-one"}, revisions, "the successful response is durable after the run expires")
	_, err = newWorker().RunOnce(t.Context(), f.BuildingGen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Equal(t, 1, client.Calls(), "a rebuilt worker reuses the published response")
}

func TestWorkerResolvedSourceReleasesUnpublishedCheckpoint(t *testing.T) {
	for _, state := range []string{"deleted", "missing", "empty", "stamped", "unfinished"} {
		t.Run(state, func(t *testing.T) {
			f := newContextWorkerFixture(t, nil)
			id := f.seed("email", f.chatID, time.Now(), "Synthetic message awaiting publication")
			client := &fakeEmbeddingClient{dim: 4}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client.OnEmbed = func(inputs []string) ([][]float32, error) {
				cancel()
				return fakeVectors(inputs), nil
			}
			newWorker := func() *Worker {
				return NewWorker(WorkerDeps{Backend: f.backend, VectorsDB: f.backend.DB(), MainDB: f.store.DB(),
					Store: f.store, Client: client, BatchSize: 25, MaxInputChars: 1000, Recorder: newTestOperationRecorder()})
			}
			_, err := newWorker().RunOnce(ctx, f.gen, testEmbeddingPassScope())
			require.ErrorIs(t, err, context.Canceled)
			var saved int
			require.NoError(t, f.backend.DB().QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints WHERE owner = ?`, fmt.Sprintf("message:%d", id)).Scan(&saved))
			require.Equal(t, 1, saved)
			switch state {
			case "deleted":
				require.NoError(t, f.store.MarkMessageDeleted(f.sourceID, "email-1"))
			case "missing":
				_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = ?`, id)
				require.NoError(t, err)
			case "empty":
				_, err = f.store.DB().Exec(`UPDATE message_bodies SET body_text = '' WHERE message_id = ?`, id)
				require.NoError(t, err)
				_, err = f.store.DB().Exec(`UPDATE messages SET subject = '', snippet = '' WHERE id = ?`, id)
				require.NoError(t, err)
			case "stamped":
				require.NoError(t, f.store.SetEmbedGen(t.Context(), []int64{id}, int64(f.gen)))
			}
			client.OnEmbed = func([]string) ([][]float32, error) {
				require.FailNow(t, "saved or resolved message must not return to the provider")
				return nil, errors.New("unexpected provider request")
			}
			if state == "unfinished" {
				// A finish sweep must keep live work whose stamp has not landed.
				require.NoError(t, newWorker().pruneDeletedCheckpoints(t.Context(), f.gen))
				require.NoError(t, f.backend.DB().QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints WHERE owner = ?`, fmt.Sprintf("message:%d", id)).Scan(&saved))
				assert.Equal(t, 1, saved)
			}
			result, err := newWorker().RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.True(t, result.ScanComplete)
			require.NoError(t, f.backend.DB().QueryRow(`SELECT COUNT(*) FROM embedding_checkpoints WHERE owner = ?`, fmt.Sprintf("message:%d", id)).Scan(&saved))
			assert.Zero(t, saved, "a completed scan releases resolved output")
		})
	}
}
