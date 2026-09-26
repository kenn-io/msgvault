package cmd

import (
	"context"
	"database/sql"
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
	previousCfg := cfg
	cfg = c
	t.Cleanup(func() { cfg = previousCfg })
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
	adapter := &storeAPIAdapter{store: s, cacheJobs: jobs}
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
