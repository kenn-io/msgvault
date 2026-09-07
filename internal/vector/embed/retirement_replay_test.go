//go:build sqlite_vec

package embed

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func TestRetiredGenerationInvocationReplaysCancellation(t *testing.T) {
	for _, workerKind := range []string{"message", "context", "person"} {
		t.Run(workerKind, func(t *testing.T) {
			require := require.New(t)
			recorder := testutil.NewSQLiteTestStore(t)
			scope := testEmbeddingPassScope()
			kind := operations.KindMessageEmbedding
			var run func() (RunResult, error)
			switch workerKind {
			case "message":
				f := newWorkerFixture(t, 3)
				require.NoError(f.Backend.RetireGeneration(t.Context(), f.BuildingGen, false))
				worker := newTestWorker(f, 3)
				worker.deps.Recorder = recorder
				run = func() (RunResult, error) { return worker.RunOnce(t.Context(), f.BuildingGen, scope) }
			case "context":
				f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) { deps.Recorder = recorder })
				f.seed("email", f.chatID, time.Now().UTC(), "synthetic message")
				f.client.before = func() {
					require.NoError(f.backend.RetireGeneration(t.Context(), f.gen, false))
				}
				run = func() (RunResult, error) { return f.worker.RunOnce(t.Context(), f.gen, scope) }
			case "person":
				kind = operations.KindPersonEmbedding
				backend := newPersonWorkerBackend()
				backend.upsertErr = vector.ErrGenerationRetired
				worker := newTestPersonWorker(newPersonWorkerSource(personDocument(1, "revision", "Synthetic Person")),
					backend, &personWorkerClient{}, 1)
				worker.deps.Recorder = recorder
				run = func() (RunResult, error) { return worker.RunOnce(t.Context(), 9, scope) }
			}
			_, firstErr := run()
			require.ErrorIs(firstErr, context.Canceled)
			snapshot, err := recorder.ListRuns(t.Context(), operations.Query{Kinds: []operations.Kind{kind}, Limit: 10})
			require.NoError(err)
			require.Len(snapshot.Runs, 1)
			stored := snapshot.Runs[0]
			assert.Equal(t, operations.StateCancelled, stored.State)
			assert.Equal(t, operations.FixedPublicError(operations.PublicErrorInvocationCancelled), stored.Error)
			_, replayErr := run()
			require.ErrorIs(replayErr, context.Canceled)
			assert.ErrorIs(t, operations.TerminalReplayOutcome(&stored), context.Canceled)
		})
	}
}
