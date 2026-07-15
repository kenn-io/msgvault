package cmd

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

// newAdoptionTestDaemon seeds a store with one message, opens the daemon's
// startup analytics engine (live-SQL fallback, no cache yet), and registers
// the API server for cache adoption — the state a daemon is in right after
// starting over an archive with no Parquet cache.
func newAdoptionTestDaemon(t *testing.T) (*config.Config, *api.Server) {
	t.Helper()
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
	require.NoError(t, err, "insert test data")

	engine, mode, autoBuild, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(t, err, "openDaemonAnalyticsEngine")
	t.Cleanup(func() { _ = engine.Close() })
	require.Equal(t, api.AnalyticsModeSQLFallback, mode, "no cache yet: startup must fall back to live SQL")
	require.True(t, autoBuild, "missing cache with auto_build_cache=true should defer the build to the background")

	server := api.NewServerWithOptions(api.ServerOptions{
		Config:                 c,
		Engine:                 engine,
		AnalyticsMode:          mode,
		AnalyticsCacheBuilding: true,
	})
	registerAnalyticsCacheAdoption(server, c, s)
	t.Cleanup(shutdownAnalyticsCacheAdoption)
	return c, server
}

// TestMaybeAdoptAnalyticsCacheUpgradesSQLFallbackAfterBuild walks the
// no-restart upgrade path: a daemon that started on live-SQL fallback stays
// there while no cache exists, then switches onto DuckDB once a cache build
// lands.
func TestMaybeAdoptAnalyticsCacheUpgradesSQLFallbackAfterBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, server := newAdoptionTestDaemon(t)
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("adoption must not spawn a build subprocess")
		return nil
	})

	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode(),
		"no cache to adopt: the fallback engine must stay")

	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), false)
	require.NoError(err, "buildCache")

	buildLock, err := cacheBuildFileLock(c.AnalyticsDir())
	require.NoError(err, "cacheBuildFileLock")
	locked, err := buildLock.TryLock()
	require.NoError(err, "hold build lock")
	require.True(locked, "hold build lock")
	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode(),
		"adoption must defer while another process holds the build lock")
	require.NoError(buildLock.Unlock(), "release build lock")

	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"a fresh cache should be adopted without a daemon restart")
}

// TestMaybeAdoptAnalyticsCacheDemotesWhenCacheRemoved pins the demotion
// path: removing the analytics directory (what remove-account does) must
// drop an adopted DuckDB daemon back to live SQL instead of leaving views
// pointing at missing Parquet files, and a later build must re-adopt.
func TestMaybeAdoptAnalyticsCacheDemotesWhenCacheRemoved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, server := newAdoptionTestDaemon(t)

	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), false)
	require.NoError(err, "buildCache")
	maybeAdoptAnalyticsCache()
	require.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(), "adopt the fresh cache")

	require.NoError(os.RemoveAll(c.AnalyticsDir()), "remove analytics dir")
	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode(),
		"missing cache files must demote aggregate views to live SQL")

	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err, "rebuild cache")
	maybeAdoptAnalyticsCache()
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"a rebuilt cache must be re-adopted after a demotion")
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
