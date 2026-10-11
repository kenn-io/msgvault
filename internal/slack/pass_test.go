package slack

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInterruptedPublicSyncCompletesPass(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.scopes = "channels:read,channels:history,users:read,users:read.email"
	f.convs = []*fakeConv{
		{ID: "C01", Name: "first", Kind: "public", Members: []string{"UME"},
			Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "first original"}}},
		{ID: "C02", Name: "second", Kind: "public", Members: []string{"UME"},
			Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "second original"}}},
		{ID: "C03", Name: "third", Kind: "public", Members: []string{"UME"},
			Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "third original"}}},
	}
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	initial, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	baseline, err := imp.store.GetLastSuccessfulSync(initial.SourceID)
	require.NoError(err)
	f.mu.Lock()
	for _, c := range f.convs {
		c.Msgs[0].Replies = []fakeMsg{{TS: ts(1500), ThreadTS: ts(1), User: "UME", Text: "late reply"}}
	}
	f.mu.Unlock()
	now = tsBase.Add(26 * time.Hour)
	completed := false
	for range 4 {
		ctx, cancel := context.WithCancel(context.Background())
		opts.Progress = func(line string) {
			if strings.HasPrefix(line, "conversation ") {
				cancel()
			}
		}
		resumed := NewImporter(imp.store, imp.client, opts.TeamID)
		resumed.now = func() time.Time { return now }
		_, err = resumed.Import(ctx, opts)
		cancel()
		if err == nil {
			completed = true
			break
		}
		require.ErrorIs(err, context.Canceled)
		now = now.Add(time.Minute)
	}
	require.True(completed, "three channels must finish a pass across bounded attempts")
	last, err := imp.store.GetLastSuccessfulSync(initial.SourceID)
	require.NoError(err)
	assert.Greater(last.ID, baseline.ID)
	var count int
	require.NoError(imp.store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(6, count, "the completed pass includes the late reply in every channel")

	// Completing a pass must release its visited set: the next pass discovers
	// replies that arrived in a channel already visited by the previous pass.
	f.mu.Lock()
	f.convs[0].Msgs[0].Replies = append(f.convs[0].Msgs[0].Replies,
		fakeMsg{TS: ts(1600), ThreadTS: ts(1), User: "UME", Text: "next pass reply"})
	f.mu.Unlock()
	now = tsBase.Add(28 * time.Hour)
	opts.Progress = nil
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(7, count)
}

func TestScheduledYieldKeepsRecoverableCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	opts.Progress = func(line string) {
		if strings.HasPrefix(line, "conversation ") {
			cancel(jobctx.ErrYieldedToWaiter)
		}
	}
	sum, err := imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	latest, err := imp.store.GetLatestSync(sum.SourceID)
	require.NoError(err)
	assert.Equal("cancelled", latest.Status, "scheduler handoffs are not failed imports")
	assert.Empty(latest.ErrorMessage.String)
	checkpoint, err := imp.store.GetLatestCheckpointedSync(sum.SourceID)
	require.NoError(err)
	assert.Equal(latest.ID, checkpoint.ID, "the next attempt must resume the handoff")
	assert.NotEmpty(checkpoint.CursorBefore.String)
}

func TestInterruptedPublicPassHonorsNewWorkOptions(t *testing.T) {
	for _, mode := range []string{"limited", "no threads"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := testWorkspace(t)
			f.scopes = "channels:read,channels:history,users:read,users:read.email"
			f.convs = []*fakeConv{
				{ID: "C01", Name: "first", Kind: "public", Members: []string{"UME"}, Msgs: []fakeMsg{
					{TS: ts(1), User: "UME", Text: "older", Replies: []fakeMsg{{TS: ts(3), ThreadTS: ts(1), User: "UME", Text: "reply"}}},
					{TS: ts(2), User: "UME", Text: "newer"},
				}},
				{ID: "C02", Name: "second", Kind: "public", Members: []string{"UME"}},
			}
			imp, opts := testImporter(t, f)
			if mode == "limited" {
				opts.Limit = 1
			} else {
				opts.NoThreads = true
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts.Progress = func(line string) {
				if strings.HasPrefix(line, "conversation ") {
					cancel()
				}
			}
			_, err := imp.Import(ctx, opts)
			require.ErrorIs(err, context.Canceled)
			opts.Limit, opts.NoThreads, opts.Progress = 0, false, nil
			_, err = imp.Import(context.Background(), opts)
			require.NoError(err)
			var count int
			require.NoError(imp.store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
			assert.Equal(3, count, "the wider request must finish work left by the earlier visit")
		})
	}
}

func TestScheduledYieldPreservesFetchFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.scopes = "channels:read,channels:history,users:read,users:read.email"
	f.convs = append(f.convs, &fakeConv{ID: "C03", Name: "other", Kind: "public", Members: []string{"UME"}})
	f.failReplies[ts(5)] = true
	imp, opts := testImporter(t, f)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	opts.Progress = func(line string) {
		if strings.HasPrefix(line, "conversation ") {
			cancel(jobctx.ErrYieldedToWaiter)
		}
	}
	sum, err := imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	require.Positive(sum.FetchErrors)
	latest, err := imp.store.GetLatestSync(sum.SourceID)
	require.NoError(err)
	assert.Equal("failed", latest.Status)
	assert.Positive(latest.ErrorsCount)
	f.mu.Lock()
	delete(f.failReplies, ts(5))
	f.mu.Unlock()
	opts.Progress = nil
	_, err = imp.Import(context.Background(), opts)
	require.ErrorContains(err, "partial Slack sync", "the interrupted pass retains its earlier failure")
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
		"SELECT COUNT(*) FROM messages WHERE source_message_id = ?"), "C01:"+ts(100)).Scan(&count))
	assert.Equal(1, count, "the next pass must retry failed channel visits")
}

func TestInterruptedPublicPassWithFetchFailureStillRefreshesHealthyChannels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.scopes = "channels:read,channels:history,users:read,users:read.email"
	f.convs = []*fakeConv{
		{ID: "C01", Name: "broken", Kind: "public", Members: []string{"UME"}, Msgs: []fakeMsg{
			{TS: ts(1), User: "UME", Text: "root", Replies: []fakeMsg{{TS: ts(2), ThreadTS: ts(1), User: "UME", Text: "reply"}}},
		}},
		{ID: "C02", Name: "healthy", Kind: "public", Members: []string{"UME"}},
		{ID: "C03", Name: "other", Kind: "public", Members: []string{"UME"}},
	}
	f.failReplies[ts(1)] = true
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	// The failure persists through two passes. The healthy channel gains
	// activity between them and must be visited again despite that failure.
	for pass := range 2 {
		messageTS := ts(10)
		if pass > 0 {
			now = now.Add(2 * time.Hour)
			messageTS = ts(1500)
		}
		f.mu.Lock()
		f.convs[1].Msgs = append(f.convs[1].Msgs, fakeMsg{TS: messageTS, User: "UME", Text: "new activity"})
		if pass > 0 {
			f.convs[1].Msgs[0].Replies = []fakeMsg{{TS: ts(1501), ThreadTS: ts(10), User: "UME", Text: "late reply"}}
		}
		f.mu.Unlock()
		finished := false
		for range 4 {
			ctx, cancel := context.WithCancelCause(context.Background())
			opts.Progress = func(line string) {
				if strings.HasPrefix(line, "conversation ") {
					cancel(jobctx.ErrYieldedToWaiter)
				}
			}
			resumed := NewImporter(imp.store, imp.client, opts.TeamID)
			resumed.now = func() time.Time { return now }
			sum, err := resumed.Import(ctx, opts)
			cancel(nil)
			require.Error(err, "the pass contains a real fetch failure")
			latest, readErr := imp.store.GetLatestSync(sum.SourceID)
			require.NoError(readErr)
			assert.Equal("failed", latest.Status, "later handoffs must not hide this pass's fetch failure")
			state, readErr := imp.loadResumeState(t.Context(), sum.SourceID)
			require.NoError(readErr)
			if state.HistoryPass == nil {
				finished = true
				break
			}
			require.ErrorIs(err, context.Canceled)
			now = now.Add(time.Minute)
		}
		require.True(finished, "a failed pass must finish so healthy channels can refresh next pass")
		var count int
		require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
			"SELECT COUNT(*) FROM messages WHERE source_message_id = ?"), "C02:"+messageTS).Scan(&count))
		assert.Equal(1, count)
		if pass > 0 {
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
				"SELECT COUNT(*) FROM messages WHERE source_message_id = ?"), "C02:"+ts(1501)).Scan(&count))
			assert.Equal(1, count, "late replies must also refresh despite the persistent failure")
		}
	}
}

func TestFinalCheckpointYieldPreservesEarlierFetchFailure(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to cancel the final checkpoint")
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.scopes = "channels:read,channels:history,users:read,users:read.email"
	f.convs = append(f.convs, &fakeConv{ID: "C03", Name: "other", Kind: "public", Members: []string{"UME"}})
	f.failReplies[ts(5)] = true
	imp, opts := testImporter(t, f)
	firstCtx, firstCancel := context.WithCancelCause(t.Context())
	defer firstCancel(nil)
	opts.Progress = func(line string) {
		if strings.HasPrefix(line, "conversation ") {
			firstCancel(jobctx.ErrYieldedToWaiter)
		}
	}
	first, err := imp.Import(firstCtx, opts)
	require.ErrorIs(err, context.Canceled)
	require.Positive(first.FetchErrors)

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	imp.store.DB().SetMaxOpenConns(1)
	conn, err := imp.store.DB().Conn(t.Context())
	require.NoError(err)
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok, "cancellation trigger requires SQLite")
		return sqliteConn.RegisterFunc("yield_final_checkpoint", func() int {
			cancel(jobctx.ErrYieldedToWaiter)
			return 0
		}, false)
	})
	require.NoError(err)
	require.NoError(conn.Close())
	// The first and per-channel checkpoints still contain history_pass.
	// Cancel only when the final checkpoint clears it after a failed pass.
	_, err = imp.store.DB().Exec(`CREATE TEMP TRIGGER yield_final_checkpoint
		BEFORE UPDATE OF cursor_before ON sync_runs
		WHEN NEW.status = 'running' AND json_extract(NEW.cursor_before, '$.history_pass') IS NULL
		BEGIN SELECT yield_final_checkpoint(); END`)
	require.NoError(err)
	opts.Progress = nil
	resumed, err := imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	require.ErrorContains(err, "write sync checkpoint")
	assert.Zero(resumed.FetchErrors, "the failed fetch occurred in the previous attempt")
	latest, readErr := imp.store.GetLatestSync(first.SourceID)
	require.NoError(readErr)
	assert.Equal("failed", latest.Status)
	assert.Contains(latest.ErrorMessage.String, "fetch error", "the queued continuation must retain the pass failure")
	assert.ErrorContains(err, "fetch error", "the scheduler must receive the failure as well as the cancellation")
}

func TestScheduledYieldDuringFTSWrite(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite authorizer to interrupt the FTS write")
	for _, tc := range []struct {
		name       string
		failFTS    bool
		wantStatus string
		wantErrors int
	}{
		{name: "yield during write", wantStatus: "cancelled"},
		{name: "independent failure before yield", failFTS: true, wantStatus: "failed", wantErrors: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := testWorkspace(t)
			f.scopes = "channels:read,channels:history,users:read,users:read.email"
			f.convs = []*fakeConv{{ID: "C01", Name: "general", Kind: "public", Members: []string{"UME"},
				Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "recoverable message"}}}}
			imp, opts := testImporter(t, f)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var reachedFTS atomic.Bool
			imp.store.DB().SetMaxOpenConns(1)
			conn, err := imp.store.DB().Conn(t.Context())
			require.NoError(err)
			require.NoError(conn.Raw(func(driverConn any) error {
				sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
				require.True(ok)
				sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
					if action == sqlite3.SQLITE_INSERT && table == "messages_fts" && reachedFTS.CompareAndSwap(false, true) {
						if tc.failFTS {
							return sqlite3.SQLITE_DENY
						}
						cancel(jobctx.ErrYieldedToWaiter)
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(conn.Close())
			if tc.failFTS {
				// A genuine index failure remains visible when the scheduler
				// subsequently yields after the message has been archived.
				opts.Progress = func(line string) {
					if strings.HasPrefix(line, "conversation ") {
						cancel(jobctx.ErrYieldedToWaiter)
					}
				}
			}

			sum, err := imp.Import(ctx, opts)
			require.ErrorIs(err, context.Canceled)
			require.True(reachedFTS.Load(), "the interruption must occur at the FTS write")
			assert.Equal(tc.wantErrors, sum.Errors)
			latest, err := imp.store.GetLatestSync(sum.SourceID)
			require.NoError(err)
			assert.Equal(tc.wantStatus, latest.Status)
			assert.Equal(int64(tc.wantErrors), latest.ErrorsCount)
			checkpoint, err := imp.store.GetLatestCheckpointedSync(sum.SourceID)
			require.NoError(err)
			assert.Equal(latest.ID, checkpoint.ID)
			if !tc.failFTS {
				assert.Empty(latest.ErrorMessage.String)
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
				var matches int
				require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts
					WHERE messages_fts MATCH 'recoverable'`).Scan(&matches))
				assert.Equal(1, matches, "the continuation must index the interrupted message")
			}
		})
	}
}
