package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

// TestMaybeAdoptAnalyticsCacheUpgradesSQLFallbackAfterBuild walks the
// no-restart upgrade path: a daemon that started on live-SQL fallback stays
// there while no cache exists, then switches onto DuckDB once a cache build
// lands.
func TestMaybeAdoptAnalyticsCacheUpgradesSQLFallbackAfterBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	_, err := s.DB().Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'user@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (1, 1, 'thread1', 'email_thread', 'Hello');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet)
			VALUES (1, 1, 1, 'msg1', 'email', '2024-01-15 10:00:00', 'Hello', 'Preview');
	`)
	require.NoError(err, "insert test data")
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("adoption must not spawn a build subprocess")
		return nil
	})

	engine, mode, autoBuild, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()
	require.Equal(api.AnalyticsModeSQLFallback, mode, "no cache yet: startup must fall back to live SQL")
	require.True(autoBuild, "missing cache with auto_build_cache=true should defer the build to the background")

	server := api.NewServerWithOptions(api.ServerOptions{
		Config:                 c,
		Engine:                 engine,
		AnalyticsMode:          mode,
		AnalyticsCacheBuilding: true,
	})
	registerAnalyticsCacheAdoption(server, c, s)
	t.Cleanup(shutdownAnalyticsCacheAdoption)

	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode(),
		"no cache to adopt: the fallback engine must stay")

	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), false)
	require.NoError(err, "buildCache")

	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"a fresh cache should be adopted without a daemon restart")
}

// TestMaybeAdoptAnalyticsCacheRespectsDeliberateSQLMode pins that a
// configured engine="sql" daemon never adopts a cache: live SQL was an
// explicit choice, not a fallback.
func TestMaybeAdoptAnalyticsCacheRespectsDeliberateSQLMode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineSQL

	engine, mode, autoBuild, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()
	require.Equal(api.AnalyticsModeSQL, mode)
	require.False(autoBuild)

	server := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		Engine:        engine,
		AnalyticsMode: mode,
	})
	registerAnalyticsCacheAdoption(server, c, s)
	t.Cleanup(shutdownAnalyticsCacheAdoption)

	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeSQL, server.AnalyticsMode(),
		"deliberate live SQL must never be upgraded")
}
