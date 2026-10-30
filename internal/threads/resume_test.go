package threads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

func importCheckpoint(t *testing.T, f *importFixture) (*store.SyncRun, *SyncState) {
	t.Helper()
	run, err := f.st.GetLatestCheckpointedSync(f.sourceID)
	require.NoError(t, err)
	state, err := DecodeSyncState(run.CursorBefore.String)
	require.NoError(t, err)
	return run, state
}
func finishImport(t *testing.T, f *importFixture, cap int) *ImportSummary {
	t.Helper()
	for attempt := 0; attempt < 200; attempt++ {
		imp, opts := f.importer(t, cap)
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(t, err)
		if sum.Status == store.SyncStatusCompleted {
			return sum
		}
	}
	require.FailNow(t, "finite fixture did not complete within its bounded page-work allowance")
	return nil
}
func TestThreadsImportPageFailureKeepsCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t)
	f.pageSize = 2
	f.posts = []Media{writerMedia("100", "first"), writerMedia("200", "second")}
	for n := range f.posts {
		f.posts[n].Timestamp = writerTime.Add(-30 * time.Minute)
	}
	imp, opts := f.importer(t, 100)
	imp.afterItem = func() error { return errors.New("injected observation boundary failure") }
	sum, err := imp.Import(t.Context(), opts)
	require.ErrorContains(err, "injected")
	require.NotNil(sum)
	run, state := importCheckpoint(t, f)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Empty(state.OwnPostsHistory.Cursor)
	assert.False(state.OwnPostsHistory.Done)
	assert.Equal(2, state.NextLane)
	finishImport(t, f, 100)
	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=?`), f.sourceID).Scan(&count))
	assert.Equal(3, count)
}
func TestThreadsImportCancellationKeepsLastCommittedPage(t *testing.T) {
	for _, boundary := range []string{"item", "checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newImportFixture(t)
			f.posts = []Media{writerMedia("100", "body")}
			f.posts[0].Timestamp = f.clock.Now().Add(-time.Hour)
			imp, opts := f.importer(t, 100)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if boundary == "item" {
				imp.afterItem = func() error { cancel(); return nil }
			} else {
				imp.beforeCheckpoint = func() error { cancel(); return ctx.Err() }
			}
			_, err := imp.Import(ctx, opts)
			require.ErrorIs(err, context.Canceled)
			run, state := importCheckpoint(t, f)
			assert.Equal(store.SyncStatusFailed, run.Status) // Existing Store convention for user cancellation.
			assert.Empty(state.OwnPostsHead.Cursor)
			assert.False(state.OwnPostsHead.Done)
			// The detached finalization must release execution ownership too.
			execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.sourceID)
			require.NoError(err)
			require.NoError(execution.Release())
			finishImport(t, f, 100)
		})
	}
}
func TestThreadsFairOldRootSweep(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t)
	source, err := f.st.GetSourceByIDContext(t.Context(), f.sourceID)
	require.NoError(err)
	w := NewWriter(f.st, f.sourceID, source.Identifier)
	root := writerMedia("400", "recent zero-reply root")
	root.Timestamp = f.clock.Now().Add(-24 * time.Hour)
	root.HasReplies = new(false)
	applyWriter(t, w, root, ItemOrigin{Kind: "own_posts"})
	for n := 0; n < 12; n++ {
		id := fmt.Sprint(301 + n)
		f.conversations["300"] = append(f.conversations["300"], writerReply(id, "300", "300"))
	}
	// Root 400's first reply was created after its false snapshot.
	f.conversations["400"] = []Media{writerReply("401", "400", "400")}
	for n := 0; n < 6; n++ {
		imp, opts := f.importer(t, 1)
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	f.mu.Lock()
	first := append([]string{}, f.calls...)
	f.mu.Unlock()
	require.Len(first, 6)
	assert.Equal([]string{"/v1.0/me/threads", "/v1.0/me/replies", "/v1.0/me/threads", "/v1.0/me/replies", "/v1.0/400/conversation", "/v1.0/300/conversation"}, first)
	finishImport(t, f, 1)
	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id='401'`), f.sourceID).Scan(&count))
	assert.Equal(1, count)
}
func TestThreadsModesAndFailures(t *testing.T) {
	t.Run("target preserves baseline and floor", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newImportFixture(t)
		finishImport(t, f, 100)
		before, err := f.st.GetSourceByIDContext(t.Context(), f.sourceID)
		require.NoError(err)
		imp, opts := f.importer(t, 100)
		opts.Request.PostID = "300"
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(err)
		assert.Equal(store.SyncStatusCompleted, sum.Status)
		after, err := f.st.GetSourceByIDContext(t.Context(), f.sourceID)
		require.NoError(err)
		assert.Equal(before.SyncCursor, after.SyncCursor)
		assert.Equal(before.SyncConfig, after.SyncConfig)
	})
	t.Run("inclusive subsecond floor and future deferral", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newImportFixture(t)
		floor := writerTime.Add(500 * time.Millisecond)
		f.posts = []Media{writerMedia("100", "below"), writerMedia("101", "exact"), writerMedia("102", "above")}
		f.posts[1].Timestamp = floor
		f.posts[2].Timestamp = floor.Add(time.Nanosecond)
		f.replies = []Media{writerReply("201", "999", "999"), writerReply("202", "999", "999")}
		f.replies[1].Timestamp = floor
		imp, opts := f.importer(t, 100)
		opts.Request.After = floor
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
		var count int
		require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id IN ('100','201')`), f.sourceID).Scan(&count))
		assert.Zero(count)
		require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id IN ('101','102','202')`), f.sourceID).Scan(&count))
		assert.Equal(3, count)
		f.mu.Lock()
		f.calls = nil
		f.mu.Unlock()
		imp, opts = f.importer(t, 100)
		opts.Request.After = f.clock.Now().Add(time.Hour)
		_, err = imp.Import(t.Context(), opts)
		require.NoError(err)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, path := range f.calls {
			assert.NotContains(path, "/me/")
		}
	})
	t.Run("individual unavailable yields but source permission fails", func(t *testing.T) {
		for _, code := range []int{404, 403} {
			f := newImportFixture(t)
			f.failure = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v1.0/300/conversation" {
					if code == 403 {
						respond(w, 403, `{"error":{"code":200}}`)
					} else {
						respond(w, 404, `{"error":{"code":0}}`)
					}
					return true
				}
				return false
			}
			imp, opts := f.importer(t, 100)
			sum, err := imp.Import(t.Context(), opts)
			if code == 403 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Positive(t, sum.Unavailable)
			}
		}
	})
	t.Run("missing root retains a revisitable gap", func(t *testing.T) {
		assert := assert.New(t)
		f := newImportFixture(t)
		m := writerMedia("800", "own reply with missing root")
		m.IsReply = new(true)
		f.replies = []Media{m}
		imp, opts := f.importer(t, 100)
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(t, err)
		assert.Equal(store.SyncStatusPaused, sum.Status)
		assert.Positive(sum.Incomplete)
		_, state := importCheckpoint(t, f)
		assert.False(state.OwnRepliesHistory.Done)
		f.replies[0] = writerReply("800", "900", "900")
		finishImport(t, f, 100)
	})
}
func TestThreadsCursorExpiryAndCycleAcrossResumes(t *testing.T) {
	t.Run("explicit expiry restarts pinned bounds", func(t *testing.T) {
		f := newImportFixture(t)
		f.posts = []Media{writerMedia("100", "a"), writerMedia("101", "b")}
		expired := false
		f.failure = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/v1.0/me/threads" && r.URL.Query().Get("after") == "1" && !expired {
				expired = true
				respond(w, 400, `{"error":{"code":100,"message":"The paging cursor has expired"}}`)
				return true
			}
			return false
		}
		finishImport(t, f, 1)
		assert.True(t, expired)
	})
	t.Run("repeated page advancing cursors never completes", func(t *testing.T) {
		f := newImportFixture(t)
		n := 0
		f.failure = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/v1.0/me/threads" {
				n++
				respond(w, 200, fmt.Sprintf(`{"data":[{"id":"100","timestamp":"2026-10-01T12:00:00Z"}],"paging":{"cursors":{"after":"cursor%d"}}}`, n))
				return true
			}
			return false
		}
		failed := false
		for attempt := 0; attempt < 12; attempt++ {
			imp, opts := f.importer(t, 1)
			_, err := imp.Import(t.Context(), opts)
			if err != nil {
				require.ErrorIs(t, err, ErrPagination)
				failed = true
				break
			}
		}
		assert.True(t, failed)
	})
	t.Run("generic bad request never restarts", func(t *testing.T) {
		f := newImportFixture(t)
		f.failure = func(w http.ResponseWriter, _ *http.Request) bool {
			respond(w, 400, `{"error":{"code":100,"message":"Invalid required field"}}`)
			return true
		}
		imp, opts := f.importer(t, 10)
		_, err := imp.Import(t.Context(), opts)
		require.Error(t, err)
		assert.Len(t, f.calls, 1)
	})
}

func TestThreadsImportSchedulerHandoffPreservesCause(t *testing.T) {
	assert := assert.New(t)
	f := newImportFixture(t)
	f.posts = []Media{writerMedia("100", "head")}
	f.posts[0].Timestamp = f.clock.Now().Add(-time.Hour)
	imp, opts := f.importer(t, 100)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	imp.afterItem = func() error { cancel(jobctx.ErrYieldedToWaiter); return ctx.Err() }
	sum, err := imp.Import(ctx, opts)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(store.SyncStatusCancelled, sum.Status)
	run, state := importCheckpoint(t, f)
	assert.Equal(store.SyncStatusCancelled, run.Status)
	assert.Empty(state.OwnPostsHead.Cursor)
	finishImport(t, f, 100)
}
func TestThreadsImportHeadOverlapStartsNewGuard(t *testing.T) {
	f := newImportFixture(t)
	f.posts = []Media{writerMedia("100", "head")}
	f.posts[0].Timestamp = f.clock.Now().Add(-30 * time.Minute)
	finishImport(t, f, 100)
	f.clock.advance(15 * time.Minute)
	finishImport(t, f, 100)
}

func TestThreadsImportSuccessfulPageWithUsagePause(t *testing.T) {
	assert := assert.New(t)
	f := newImportFixture(t)
	f.posts = []Media{writerMedia("100", "head")}
	f.posts[0].Timestamp = f.clock.Now().Add(-time.Hour)
	f.failure = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("X-App-Usage", `{"call_count":80}`)
		return false
	}
	imp, opts := f.importer(t, 100)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(store.SyncStatusPaused, sum.Status)
	assert.Equal(int64(1), sum.MessagesAdded)
	_, state := importCheckpoint(t, f)
	assert.Equal("1", state.OwnPostsHead.Cursor)
	assert.Equal(1, state.NextLane)
	f.failure = nil
	finishImport(t, f, 100)
}
func TestThreadsImportCancelledFinalizationRetainsCompleteCheckpoint(t *testing.T) {
	f := newImportFixture(t)
	imp, opts := f.importer(t, 100)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts.Progress = func(string) {
		run, err := f.st.GetLatestSync(f.sourceID)
		require.NoError(t, err)
		state, err := DecodeSyncState(run.CursorBefore.String)
		require.NoError(t, err)
		if state.done() {
			cancel()
		}
	}
	_, err := imp.Import(ctx, opts)
	require.ErrorIs(t, err, context.Canceled)
	_, state := importCheckpoint(t, f)
	assert.True(t, state.done())
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	finishImport(t, f, 100)
	assert.Empty(t, f.calls, "committed complete page state needs finalization only")
}
func TestThreadsImportHeadProgressDuringHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t)
	for n := 0; n < 8; n++ {
		m := writerMedia(fmt.Sprint(100+n), "history")
		m.Timestamp = writerTime.Add(-30 * time.Minute)
		f.posts = append(f.posts, m)
	}
	// Finish an empty head and leave the old history interval unfinished.
	imp, opts := f.importer(t, 1)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	f.clock.advance(2 * time.Hour)
	m := writerMedia("900", "new head during history")
	m.Timestamp = f.clock.Now().Add(-time.Minute)
	f.posts = append(f.posts, m)
	found := false
	for n := 0; n < 12; n++ {
		imp, opts = f.importer(t, 1)
		_, err = imp.Import(t.Context(), opts)
		require.NoError(err)
		_, found, err = f.st.ThreadsArchiveMessageMetadataContext(t.Context(), f.sourceID, "900")
		require.NoError(err)
		if found {
			_, state := importCheckpoint(t, f)
			assert.False(state.OwnPostsHistory.Done)
			break
		}
	}
	assert.True(found)
}

func TestThreadsImportRepeatedCursorExpiryKeepsGap(t *testing.T) {
	assert := assert.New(t)
	f := newImportFixture(t)
	f.posts = []Media{writerMedia("100", "first"), writerMedia("101", "second")}
	f.failure = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1.0/me/threads" && r.URL.Query().Get("after") != "" {
			respond(w, 400, `{"error":{"code":100,"message":"The paging cursor has expired"}}`)
			return true
		}
		return false
	}
	gap := false
	for run := 0; run < 12; run++ {
		imp, opts := f.importer(t, 4)
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(t, err)
		if sum.Incomplete > 0 {
			gap = true
			assert.Equal(store.SyncStatusPaused, sum.Status)
			_, state := importCheckpoint(t, f)
			assert.False(state.OwnPostsHistory.Done)
			break
		}
	}
	assert.True(gap)
	f.failure = nil
	finishImport(t, f, 100)
}

func TestThreadsImportAmbiguousRootDenialStopsSource(t *testing.T) {
	f := newImportFixture(t)
	f.failure = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1.0/300/conversation" {
			respond(w, 403, `{"error":{"code":0}}`)
			return true
		}
		return false
	}
	imp, opts := f.importer(t, 100)
	_, err := imp.Import(t.Context(), opts)
	require.Error(t, err)
	_, state := importCheckpoint(t, f)
	assert.False(t, state.AllRoots.Done)
	assert.Zero(t, state.AllRoots.AfterID)
}

func TestThreadsTargetDoesNotClaimSourceHistoryComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t)
	imp, opts := f.importer(t, 100)
	opts.Request.PostID = "300"
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, sum.Status)
	assert.False(sum.HistoryComplete, "one conversation does not complete source-wide history")
	source, err := f.st.GetSourceByIDContext(t.Context(), f.sourceID)
	require.NoError(err)
	assert.False(source.SyncCursor.Valid)
}
func TestThreadsBudgetCancellationPausesCommittedSnapshot(t *testing.T) {
	assert := assert.New(t)
	f := newImportFixture(t)
	f.posts = []Media{writerMedia("100", "head")}
	f.posts[0].Timestamp = f.clock.Now().Add(-time.Hour)
	imp, opts := f.importer(t, 100)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	imp.afterItem = func() error { cancel(&PauseError{Reason: "time_limit"}); return ctx.Err() }
	sum, err := imp.Import(ctx, opts)
	require.NoError(t, err)
	assert.Equal(store.SyncStatusPaused, sum.Status)
	run, state := importCheckpoint(t, f)
	assert.Equal(store.SyncStatusPaused, run.Status)
	assert.Empty(state.OwnPostsHead.Cursor)
	finishImport(t, f, 100)
}
