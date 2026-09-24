package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestManualSyncRefreshVerifiesConversationOnlyChangesInBackground(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.AutoBuildCache = true
	c.Analytics.MinRebuildInterval = 0
	source, err := s.GetOrCreateSource("gmail", "user@example.test")
	require.NoError(err)
	conversationID, err := s.EnsureConversationWithType(source.ID, "thread-1", "email_thread", "Original title")
	require.NoError(err)
	_, err = s.UpsertMessage(&store.Message{
		SourceID: source.ID, SourceMessageID: "message-1", ConversationID: conversationID,
		MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)
	_, err = s.EnsureConversationWithType(source.ID, "thread-1", "email_thread", "Updated title")
	require.NoError(err)
	light, err := cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	require.False(light.NeedsBuild, "conversation titles have no indexed staleness signal")

	ctx, cancel := context.WithCancel(t.Context())
	verified := make(chan cacheStaleness, 1)
	jobs := newCacheBuildJobs(ctx, nil, func(context.Context, buildCacheMode) error {
		verified <- cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
		return nil
	})
	t.Cleanup(func() {
		cancel()
		waitCtx, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer stop()
		require.True(jobs.waitContext(waitCtx), "background verification must finish before store cleanup")
	})
	adapter := &storeAPIAdapter{store: s, config: c, cacheJobs: jobs}
	require.NoError(adapter.queueCacheRefreshAfterManualSync(false, false))
	waitCtx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(jobs.waitContext(waitCtx))
	select {
	case full := <-verified:
		assert.True(full.NeedsBuild)
		assert.True(full.HasConversationTypeDrift)
		assert.Contains(full.Reason, "conversation metadata changed")
	default:
		require.FailNow("manual sync did not queue full cache verification")
	}
}

func TestManualSyncRefreshChecksFilesBeforeThrottling(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "usable"
		if damaged {
			name = "missing shard"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			c, s := openPublishedQueryTestStore(t)
			c.Analytics.AutoBuildCache = true
			c.Analytics.MinRebuildInterval = 6 * time.Hour
			_, err := s.DB().Exec(`INSERT INTO messages
				(id, source_id, source_message_id, conversation_id, message_type, sent_at)
				VALUES (2, 1, 'message-2', 1, 'email', '2024-01-02 00:00:00')`)
			require.NoError(err)
			if damaged {
				shards, err := filepath.Glob(filepath.Join(c.AnalyticsDir(), "messages", "*", "*.parquet"))
				require.NoError(err)
				require.Len(shards, 1)
				require.NoError(os.Remove(shards[0]))
			}
			ctx, cancel := context.WithCancel(t.Context())
			completed := make(chan *buildResult, 1)
			jobs := newCacheBuildJobs(ctx, nil, func(context.Context, buildCacheMode) error {
				result, err := buildCacheScheduled(c.DatabaseDSN(), c.AnalyticsDir(),
					c.Analytics.MinRebuildInterval, time.Now)
				completed <- result
				return err
			})
			t.Cleanup(func() {
				cancel()
				waitCtx, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
				defer stop()
				require.True(jobs.waitContext(waitCtx))
			})
			adapter := &storeAPIAdapter{store: s, config: c, cacheJobs: jobs}
			require.NoError(adapter.queueCacheRefreshAfterManualSync(false, false))
			waitCtx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
			defer stop()
			require.True(jobs.waitContext(waitCtx))
			select {
			case result := <-completed:
				require.NotNil(result)
				assert.Equal(!damaged, result.Skipped)
			default:
				require.FailNow("manual sync did not queue cache verification")
			}
			engine, err := openDaemonDuckDBEngine(c, s)
			require.NoError(err)
			t.Cleanup(func() { _ = engine.Close() })
			result, err := engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
			require.NoError(err)
			require.Len(result.Rows, 1)
			wantCount := 1
			if damaged {
				wantCount = 2
			}
			assert.EqualValues(wantCount, result.Rows[0][0])
		})
	}
}
