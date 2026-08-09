package cmd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

func TestPrepareDaemonAnalyticsEngineAutoStartsWithSQLFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto

	engine, mode, outcome, async, err := prepareDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err)
	require.NotNil(engine)
	t.Cleanup(func() { _ = engine.Close() })

	assert.True(async)
	assert.Equal(api.AnalyticsModeSQLFallback, mode)
	assert.Equal(startupCacheBuildOutcomeNone, outcome)
	assert.IsType(&query.SQLiteEngine{}, engine)
}

func TestPrepareDaemonAnalyticsEngineDuckDBKeepsGeneralSQLiteQueries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB

	engine, mode, outcome, async, err := prepareDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	requirements.NoError(err)
	requirements.NotNil(engine)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })

	assertions.True(async)
	assertions.Equal(api.AnalyticsModeInitializing, mode)
	assertions.Equal(startupCacheBuildOutcomeNone, outcome)
	assertions.IsType(&query.SQLiteEngine{}, engine)
}

func TestStartDaemonAnalyticsInitializerAutoInstallsDuckDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	stubBuildCacheSubprocess(t, func(ctx context.Context, fullRebuild bool) error {
		_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), fullRebuild)
		return err
	})

	initial := query.NewEngine(s.DB(), false)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		Engine:        initial,
		AnalyticsMode: api.AnalyticsModeSQLFallback,
		Logger:        slog.Default(),
	})
	h := startDaemonAnalyticsInitializer(
		context.Background(), c, s, startupCacheBuildIntentNone, srv, nil, nil,
	)
	require.True(h.WaitContext(context.Background()))
	require.NoError(h.Err())
	assert.True(h.Swapped())
	assert.Equal(api.AnalyticsModeDuckDB, srv.AnalyticsMode())
	assert.IsType(&query.DuckDBEngine{}, srv.QueryEngine())
	require.NoError(closeDaemonAnalyticsEngines(srv, initial, h))
}

func TestStartDaemonAnalyticsInitializerDuckDBFailureDoesNotInstallSQLFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	c.Analytics.AutoBuildCache = true
	sentinel := errors.New("cache build failed")
	stubBuildCacheSubprocess(t, func(context.Context, bool) error { return sentinel })

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		AnalyticsMode: api.AnalyticsModeInitializing,
		Logger:        slog.Default(),
	})
	h := startDaemonAnalyticsInitializer(
		context.Background(), c, s, startupCacheBuildIntentNone, srv, nil, nil,
	)
	require.True(h.WaitContext(context.Background()))
	require.ErrorIs(h.Err(), sentinel)
	assert.False(h.Swapped())
	assert.Nil(srv.QueryEngine())
	assert.Equal(api.AnalyticsModeInitializing, srv.AnalyticsMode())
}

func TestStartDaemonAnalyticsInitializerPublishesFatalDuckDBOutcome(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	sentinel := errors.New("explicit cache build failed")
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		return sentinel
	})
	owner, err := claimServeOwnership(context.Background(), c, "127.0.0.1", 8123, "v-test")
	requirements.NoError(err, "claim serve ownership")
	t.Cleanup(func() { requirements.NoError(owner.Close(), "close serve ownership") })

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		AnalyticsMode: api.AnalyticsModeInitializing,
		Logger:        slog.Default(),
	})
	h := startDaemonAnalyticsInitializer(
		context.Background(), c, s, startupCacheBuildIntentDefault, srv, owner, nil,
	)
	requirements.True(h.WaitContext(context.Background()))
	requirements.ErrorIs(h.Err(), sentinel)

	records, err := daemonRuntimeStore(c.Data.DataDir).List()
	requirements.NoError(err, "list daemon runtime records")
	requirements.Len(records, 1, "daemon runtime records")
	assertions.Equal(
		string(startupCacheBuildOutcomeFatal),
		records[0].Metadata[runtimeStartupCacheBuildOutcome],
	)
	assertions.Equal(api.AnalyticsModeInitializing, srv.AnalyticsMode())
}

type closeOrderAnalyticsEngine struct {
	*querytest.MockEngine

	close func() error
}

func (e *closeOrderAnalyticsEngine) Close() error {
	return e.close()
}

func TestCloseDaemonAnalyticsEnginesRequiresInitializerCompletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := lifecycleTestConfig(t.TempDir())
	var httpShutdown bool
	var workerDone bool
	var closed bool
	engine := &closeOrderAnalyticsEngine{
		MockEngine: &querytest.MockEngine{},
		close: func() error {
			if !httpShutdown || !workerDone {
				return errors.New("analytics engine closed before lifecycle barriers")
			}
			closed = true
			return nil
		},
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		Engine:        engine,
		AnalyticsMode: api.AnalyticsModeSQLFallback,
		Logger:        slog.Default(),
	})
	h := newDaemonAnalyticsInitHandle()

	err := closeDaemonAnalyticsEngines(srv, engine, h)
	require.Error(err)
	assert.False(closed)

	close(h.done)
	httpShutdown = true
	workerDone = true
	require.NoError(closeDaemonAnalyticsEngines(srv, engine, h))
	assert.True(closed)
}

func TestCloseDaemonStoreAfterAnalyticsInitSkipsRunningWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	h := newDaemonAnalyticsInitHandle()

	err = closeDaemonStoreAfterInitializers(s, h, nil)
	require.Error(err)
	require.ErrorContains(err, "analytics initialization is still running")
	require.NoError(s.DB().Ping(), "store must remain open while worker runs")

	close(h.done)
	require.NoError(closeDaemonStoreAfterInitializers(s, h, nil))
	assert.Error(s.DB().Ping(), "store must close after worker joins")
}

func TestCloseDaemonStoreAfterAnalyticsInitSkipsRunningVectorWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	analytics := completedDaemonAnalyticsInitHandle()
	vector := &vectorInitHandle{done: make(chan struct{})}

	err = closeDaemonStoreAfterInitializers(s, analytics, vector)
	require.Error(err)
	require.ErrorContains(err, "vector initialization is still running")
	require.NoError(s.DB().Ping(), "store must remain open while vector worker runs")

	close(vector.done)
	require.NoError(closeDaemonStoreAfterInitializers(s, analytics, vector))
	assert.Error(s.DB().Ping(), "store must close after both workers join")
}
