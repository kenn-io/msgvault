//go:build sqlite_vec

package embed

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
)

// latestMessagePassCounters reads the counters status reports for the latest
// message-embedding pass.
func latestMessagePassCounters(t *testing.T, f *contextWorkerFixture) map[string]int64 {
	t.Helper()
	lane, err := f.store.LaneStatus(t.Context(), operations.KindMessageEmbedding)
	require.NoError(t, err)
	require.NotNil(t, lane.Latest)
	counters := make(map[string]int64)
	for _, counter := range lane.Latest.Counters {
		counters[string(counter.Name)] = counter.Value
	}
	return counters
}

func latestMessagePassState(t *testing.T, f *contextWorkerFixture) operations.State {
	t.Helper()
	lane, err := f.store.LaneStatus(t.Context(), operations.KindMessageEmbedding)
	require.NoError(t, err)
	require.NotNil(t, lane.Latest)
	return lane.Latest.State
}

func failEmbedGenStamp(t *testing.T, f *contextWorkerFixture, id int64) {
	t.Helper()
	_, err := f.store.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_stamp BEFORE UPDATE OF embed_gen ON messages
		WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'synthetic stamp failure'); END`, id))
	require.NoError(t, err)
}

func newStoreBackedWorker(f *contextWorkerFixture) *Worker {
	return NewWorker(WorkerDeps{
		Store: f.store, MainDB: f.store.DB(), Backend: f.backend, VectorsDB: f.backend.DB(),
		Client: &fakeEmbeddingClient{dim: 4}, Recorder: f.store, BatchSize: 10, MaxConsecutiveFailures: 1,
	})
}

func TestWorkerCoverageStampFailureStampsNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	f.seed("email", f.chatID, time.Now().UTC(), "First synthetic body")
	second := f.seed("email", f.chatID, time.Now().UTC(), "Second synthetic body")
	failEmbedGenStamp(t, f, second)
	w := newStoreBackedWorker(f)

	_, err := w.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.ErrorContains(err, "synthetic stamp failure")
	assert.Equal(2, countMissing(t, f.store.DB(), int64(f.gen)),
		"a failed stamp leaves no row covered that the pass did not count")
	counters := latestMessagePassCounters(t, f)
	assert.Equal(int64(0), counters[string(operations.CounterSucceeded)])
	assert.Equal(int64(2), counters[string(operations.CounterFailed)])

	_, err = f.store.DB().Exec(`DROP TRIGGER fail_stamp`)
	require.NoError(err)
	_, err = w.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	counters = latestMessagePassCounters(t, f)
	assert.Equal(int64(2), counters[string(operations.CounterSucceeded)], "the retry covers both messages")
	assert.Equal(int64(0), counters[string(operations.CounterFailed)])
}

func TestWorkerCountsCommittedSkipsWhenCoverageStampFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Now().UTC(), "")
	embedded := f.seed("email", f.chatID, time.Now().UTC(), "Synthetic body")
	failEmbedGenStamp(t, f, embedded)

	_, err := newStoreBackedWorker(f).RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.ErrorContains(err, "synthetic stamp failure")
	counters := latestMessagePassCounters(t, f)
	assert.Equal(int64(1), counters[string(operations.CounterSucceeded)], "the committed skip stamp succeeded")
	assert.Equal(int64(1), counters[string(operations.CounterFailed)], "only the unstamped message failed")
}

func TestContextWorkerBudgetDeferralIsNotAFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	f.deps.MaxRunUTF8Bytes = 1
	f.deps.Recorder = f.store
	f.restartWorker()
	f.seed("email", f.chatID, time.Now().UTC(), "First synthetic body")
	f.seed("email", f.chatID, time.Now().UTC().Add(time.Minute), "Second synthetic body")

	_, err := f.run()
	require.NoError(err)
	assert.Equal(operations.StateSucceeded, latestMessagePassState(t, f), "the deferring pass finishes")
	counters := latestMessagePassCounters(t, f)
	assert.Equal(int64(1), counters[string(operations.CounterAttempted)], "deferred scopes belong to the next pass")
	assert.Equal(int64(0), counters[string(operations.CounterFailed)])
	assert.Equal(int64(1), counters[string(operations.CounterSucceeded)])

	_, err = f.run()
	require.NoError(err)
	assert.Equal(operations.StateSucceeded, latestMessagePassState(t, f))
	counters = latestMessagePassCounters(t, f)
	assert.Equal(int64(1), counters[string(operations.CounterAttempted)])
	assert.Equal(int64(0), counters[string(operations.CounterFailed)])
	assert.Equal(int64(1), counters[string(operations.CounterSucceeded)], "the next pass covers the deferred message")
}

func TestClaimedInScopesCountsOnlyClaimedMessages(t *testing.T) {
	scopes := []preparedScope{
		{key: "chat", liveVersions: []SourceVersion{{MessageID: 2}, {MessageID: 3}, {MessageID: 9}}},
		{key: "email", liveVersions: []SourceVersion{{MessageID: 4}}},
		{key: "duplicate", liveVersions: []SourceVersion{{MessageID: 2}}},
	}

	assert.Equal(t, 3, claimedInScopes([]int64{1, 2, 3, 4}, scopes),
		"message 9 is outside the page and message 2 counts once")
	assert.Equal(t, 0, claimedInScopes([]int64{1, 2}, nil))
}
