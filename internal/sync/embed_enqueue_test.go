package sync

import (
	"context"
	"errors"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

// failingEnqueuer always fails EnqueueMessages, recording how many times
// it was invoked. It simulates a broken vector-search queue (e.g. a
// pending_embeddings INSERT failure on PostgreSQL, or a missing
// vectors.db on SQLite).
type failingEnqueuer struct {
	calls int
}

func (f *failingEnqueuer) EnqueueMessages(_ context.Context, _ []int64) error {
	f.calls++
	return errors.New("simulated enqueue failure")
}

// newEnqueueTestEnv builds a sync test environment backed by the store
// selected via MSGVAULT_TEST_DB (SQLite by default, PostgreSQL under
// `make test-pg`). Unlike newTestEnv it does NOT hard-code SQLite, so the
// same test exercises the enqueue paths on both backends.
func newEnqueueTestEnv(t *testing.T, enq EmbedEnqueuer) *TestEnv {
	t.Helper()

	st := testutil.NewTestStore(t)

	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{
		EmailAddress:  testEmail,
		MessagesTotal: 0,
		HistoryID:     1000,
	}

	syncer := New(mock, st, nil)
	syncer.SetEmbedEnqueuer(enq)

	return &TestEnv{
		Store:   st,
		Mock:    mock,
		Syncer:  syncer,
		TmpDir:  t.TempDir(),
		Context: context.Background(),
	}
}

// TestFullSync_EnqueueFailureIsNonFatal verifies that when the vector
// enqueue fails during a full sync, the sync still succeeds and the
// message rows stay persisted — on EVERY backend. This is the SQLite
// parity behavior: enqueue failures are warn-and-continue, never a hard
// error (missed IDs are recovered by `msgvault embed --full-rebuild`).
//
// Has teeth: with the previous PostgreSQL hard-bail
// (`if s.store.IsPostgreSQL() { return ..., fmt.Errorf("vector enqueue
// failed (PG)") }`), this test fails on the PostgreSQL backend
// (`make test-pg`) because Full() would return an error instead of
// succeeding.
func TestFullSync_EnqueueFailureIsNonFatal(t *testing.T) {
	enq := &failingEnqueuer{}
	env := newEnqueueTestEnv(t, enq)

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})

	summary, err := env.Syncer.Full(env.Context, testEmail)
	requirepkg.NoError(t, err, "full sync must succeed despite enqueue failure")
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(0))})

	// The enqueuer was actually exercised (the failure path was hit).
	assertpkg.Positive(t, enq.calls, "enqueuer should have been invoked")

	// Messages are persisted even though the enqueue failed.
	assertMessageCount(t, env.Store, 2)
}

// TestIncrementalSync_EnqueueFailureIsNonFatal verifies the same parity
// behavior for the incremental-sync batch enqueue site.
func TestIncrementalSync_EnqueueFailureIsNonFatal(t *testing.T) {
	enq := &failingEnqueuer{}
	env := newEnqueueTestEnv(t, enq)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 2
	env.Mock.AddMessage("new-msg-1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("new-msg-2", testMIME(), []string{"INBOX"})
	env.SetHistory(12350,
		historyAdded("new-msg-1"),
		historyAdded("new-msg-2"),
	)

	summary, err := env.Syncer.Incremental(env.Context, source)
	requirepkg.NoError(t, err, "incremental sync must succeed despite enqueue failure")
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})

	assertpkg.Positive(t, enq.calls, "enqueuer should have been invoked")
	assertMessageCount(t, env.Store, 2)
}

// TestIncrementalSync_PerMessageEnqueueFailureIsNonFatal verifies the
// parity behavior for the per-message enqueue site in handleLabelChange
// (a label added to a message that does not yet exist locally, so it is
// fetched and ingested inline).
func TestIncrementalSync_PerMessageEnqueueFailureIsNonFatal(t *testing.T) {
	require := requirepkg.New(t)
	enq := &failingEnqueuer{}
	env := newEnqueueTestEnv(t, enq)
	source := env.CreateSourceWithHistory(t, "12340")
	_, err := env.Store.EnsureLabel(source.ID, "INBOX", "Inbox", "system")
	require.NoError(err, "EnsureLabel INBOX")
	_, err = env.Store.EnsureLabel(source.ID, "STARRED", "Starred", "system")
	require.NoError(err, "EnsureLabel STARRED")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.AddMessage("new-msg", testMIME(), []string{"INBOX", "STARRED"})
	env.SetHistory(12350, historyLabelAdded("new-msg", "STARRED"))

	_, err = env.Syncer.Incremental(env.Context, source)
	require.NoError(err, "incremental sync must succeed despite per-message enqueue failure")

	assertpkg.Positive(t, enq.calls, "enqueuer should have been invoked")
	assertMessageCount(t, env.Store, 1)
}

// compile-time check that failingEnqueuer satisfies the interface.
var _ EmbedEnqueuer = (*failingEnqueuer)(nil)
