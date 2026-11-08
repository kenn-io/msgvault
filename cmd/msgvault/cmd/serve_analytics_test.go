package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
		t.Context(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err)
	require.NotNil(engine)
	t.Cleanup(func() { _ = engine.Close() })

	assert.True(async)
	assert.Equal(api.AnalyticsModeSQLFallback, mode)
	assert.Equal(startupCacheBuildOutcomeNone, outcome)
	assert.IsType(&query.SQLiteEngine{}, engine)
}

func TestRunServeAllowsDeletionIDsWhileAnalyticsBuildBlocked(t *testing.T) {
	cfg := testConfigValue()

	assert := assert.New(t)
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	c.Vector.Enabled = false
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	buildStarted := make(chan struct{})
	stubBuildCacheSubprocess(t, func(ctx context.Context, _ bool) error {
		close(buildStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	select {
	case <-buildStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before analytics build was blocked")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics cache build did not start")
	}
	waitForServeHealthBounded(t, c.Server.APIPort, errCh)

	client := &http.Client{Timeout: serveLifecycleTestTimeout}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/deletions", c.Server.APIPort),
		strings.NewReader(`{"message_ids":[999999],"dry_run":true}`),
	)
	require.NoError(err, "build ID deletion request")
	request.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(request)
	require.NoError(err, "ID deletion request while cache build is blocked")
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(resp.Body.Close())
	require.NoError(readErr)
	assert.Equal(http.StatusBadRequest, resp.StatusCode)
	assert.Contains(string(body), "no_messages_matched")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestPrepareDaemonAnalyticsEngineDuckDBKeepsGeneralSQLiteQueries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB

	engine, mode, outcome, async, err := prepareDaemonAnalyticsEngine(
		t.Context(), c, s, startupCacheBuildIntentNone,
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
		_, err := buildCache(ctx, c.DatabaseDSN(), c.AnalyticsDir(), fullRebuild)
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
		t.Context(), c, s, startupCacheBuildIntentNone, srv, nil, nil,
	)
	require.True(h.WaitContext(t.Context()))
	require.NoError(h.Err())
	assert.False(srv.AnalyticsInitializationActive())
	assert.True(h.Swapped())
	assert.Equal(api.AnalyticsModeDuckDB, srv.AnalyticsMode())
	assert.IsType(&query.DuckDBEngine{}, srv.QueryEngine())
	require.NoError(closeDaemonAnalyticsEngines(srv, initial, h))
}

func TestStartDaemonAnalyticsInitializerTracksAutoInitializationIndependently(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	_, err := buildCache(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	state, err := query.ReadCacheSyncState(c.AnalyticsDir())
	requirements.NoError(err)
	state.SchemaVersion--
	encoded, err := json.Marshal(state)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(query.CacheStatePath(c.AnalyticsDir()), encoded, 0o600))
	started := make(chan struct{})
	release := make(chan struct{})
	stubBuildCacheSubprocess(t, func(ctx context.Context, full bool) error {
		close(started)
		select {
		case <-release:
			_, err := buildCache(ctx, c.DatabaseDSN(), c.AnalyticsDir(), full)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	initial := query.NewEngine(s.DB(), false)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: c, Engine: initial, AnalyticsMode: api.AnalyticsModeSQLFallback, Logger: slog.Default(),
	})
	h := startDaemonAnalyticsInitializer(
		t.Context(), c, s, startupCacheBuildIntentNone, srv, nil, nil,
	)
	<-started
	assertions.True(srv.AnalyticsInitializationActive())
	live, ok := srv.QueryEngine().(*query.LiveExploreEngine)
	requirements.True(ok)
	snapshot, releaseSnapshot, _, err := live.BeginExploreSnapshot(t.Context())
	requirements.NoError(err)
	defer releaseSnapshot()

	close(release)
	requirements.True(h.WaitContext(t.Context()))
	requirements.NoError(h.Err())
	assertions.False(srv.AnalyticsInitializationActive())
	assertions.IsType(&query.DuckDBEngine{}, srv.QueryEngine())
	_, err = live.Explore(snapshot, query.ExploreRequest{})
	requirements.NoError(err, "an in-flight live request survives the swap")
	releaseSnapshot()
	requirements.NoError(closeDaemonAnalyticsEngines(srv, initial, h))
	_, err = live.Explore(t.Context(), query.ExploreRequest{})
	assertions.ErrorIs(err, sql.ErrConnDone)
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
		t.Context(), c, s, startupCacheBuildIntentNone, srv, nil, nil,
	)
	require.True(h.WaitContext(t.Context()))
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
	owner, err := claimServeOwnership(t.Context(), c, "127.0.0.1", 8123, "v-test")
	requirements.NoError(err, "claim serve ownership")
	t.Cleanup(func() { requirements.NoError(owner.Close(), "close serve ownership") })

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        c,
		AnalyticsMode: api.AnalyticsModeInitializing,
		Logger:        slog.Default(),
	})
	h := startDaemonAnalyticsInitializer(
		t.Context(), c, s, startupCacheBuildIntentDefault, srv, owner, nil,
	)
	requirements.True(h.WaitContext(t.Context()))
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
	s, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	h := newDaemonAnalyticsInitHandle()

	err = closeDaemonStoreAfterInitializers(s, h, nil)
	require.Error(err)
	require.ErrorContains(err, "analytics initialization is still running")
	require.NoError(s.DB().PingContext(t.Context()), "store must remain open while worker runs")

	close(h.done)
	require.NoError(closeDaemonStoreAfterInitializers(s, h, nil))
	assert.Error(s.DB().PingContext(t.Context()), "store must close after worker joins")
}

func TestCloseDaemonStoreAfterAnalyticsInitSkipsRunningVectorWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	analytics := completedDaemonAnalyticsInitHandle()
	vector := &vectorInitHandle{done: make(chan struct{})}

	err = closeDaemonStoreAfterInitializers(s, analytics, vector)
	require.Error(err)
	require.ErrorContains(err, "vector initialization is still running")
	require.NoError(s.DB().PingContext(t.Context()), "store must remain open while vector worker runs")

	close(vector.done)
	require.NoError(closeDaemonStoreAfterInitializers(s, analytics, vector))
	assert.Error(s.DB().PingContext(t.Context()), "store must close after both workers join")
}

func TestStartDaemonAnalyticsInitializerValidCacheWaitsForDuckDB(t *testing.T) {
	require := require.New(t)
	c, st := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	_, err := buildCache(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)
	started, release := make(chan struct{}), make(chan struct{})
	oldOpen := openDaemonDuckDBEngineForRun
	openDaemonDuckDBEngineForRun = func(ctx context.Context, c *config.Config, st *store.Store) (*query.DuckDBEngine, error) {
		close(started)
		select {
		case <-release:
			return oldOpen(ctx, c, st)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.Cleanup(func() { openDaemonDuckDBEngineForRun = oldOpen })
	initial, mode, _, _, err := prepareDaemonAnalyticsEngine(t.Context(), c, st, startupCacheBuildIntentNone)
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: c, Engine: initial, AnalyticsMode: mode, Logger: slog.Default()})
	h := startDaemonAnalyticsInitializer(t.Context(), c, st, startupCacheBuildIntentNone, srv, nil, nil)
	select {
	case <-started:
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("cache open did not start")
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(`{"limit":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "building")
	close(release)
	require.True(h.WaitContext(t.Context()))
	require.NoError(h.Err())
	assert.IsType(t, &query.DuckDBEngine{}, srv.QueryEngine())
	require.NoError(closeDaemonAnalyticsEngines(srv, initial, h))
}

func TestStartDaemonAnalyticsInitializerFullRebuildFailure(t *testing.T) {
	for _, scenario := range []string{"absent", "partial publication", "stale schema"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			c, st := openTestDaemonAnalyticsStore(t)
			c.Analytics.Engine = config.AnalyticsEngineAuto
			c.Analytics.AutoBuildCache = true
			if scenario != "absent" {
				publishStaleTestCache(t, c, st)
				state, err := query.ReadCacheSyncState(c.AnalyticsDir())
				require.NoError(err)
				if scenario == "stale schema" {
					state.SchemaVersion--
				} else {
					state.FullRebuildRequired = true
				}
				encoded, err := json.Marshal(state)
				require.NoError(err)
				require.NoError(os.WriteFile(query.CacheStatePath(c.AnalyticsDir()), encoded, 0o600))
			}
			initial := query.NewEngine(st.DB(), false)
			srv := api.NewServerWithOptions(api.ServerOptions{Config: c, Engine: initial, AnalyticsMode: api.AnalyticsModeSQLFallback, Logger: slog.Default()})
			stubBuildCacheSubprocess(t, func(_ context.Context, full bool) error {
				assert.True(t, full)
				assert.IsType(t, &query.LiveExploreEngine{}, srv.QueryEngine())
				return errors.New("simulated build failure")
			})
			h := startDaemonAnalyticsInitializer(t.Context(), c, st, startupCacheBuildIntentNone, srv, nil, nil)
			require.True(h.WaitContext(t.Context()))
			require.NoError(h.Err())
			assert.False(t, srv.AnalyticsInitializationActive())
			if scenario == "partial publication" {
				assert.IsType(t, &query.DuckDBEngine{}, srv.QueryEngine())
			} else {
				assert.IsType(t, &query.LiveExploreEngine{}, srv.QueryEngine())
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(`{"limit":1}`))
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				srv.Router().ServeHTTP(response, req)
				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
			}
			require.NoError(closeDaemonAnalyticsEngines(srv, initial, h))
		})
	}
}

func TestOpenDaemonAnalyticsEngineFullRebuildHook(t *testing.T) {
	for _, mode := range []string{config.AnalyticsEngineAuto, config.AnalyticsEngineDuckDB} {
		for _, intent := range []startupCacheBuildIntent{startupCacheBuildIntentNone, startupCacheBuildIntentFull} {
			t.Run(fmt.Sprintf("%s/%s", mode, intent), func(t *testing.T) {
				require := require.New(t)
				c, st := openTestDaemonAnalyticsStore(t)
				c.Analytics.Engine = mode
				c.Analytics.AutoBuildCache = true
				publishStaleTestCache(t, c, st)
				fired := false
				calls := 0
				build := func(ctx context.Context, full bool) error {
					calls++
					assert.Equal(t, intent == startupCacheBuildIntentFull, full)
					assert.Equal(t, mode == config.AnalyticsEngineAuto && full, fired)
					_, err := buildCache(ctx, c.DatabaseDSN(), c.AnalyticsDir(), full)
					return err
				}
				if intent == startupCacheBuildIntentNone {
					stubBuildCacheSubprocess(t, build)
				} else {
					stubStartupCacheBuild(t, func(ctx context.Context, got startupCacheBuildIntent) error {
						assert.Equal(t, intent, got)
						return build(ctx, true)
					})
				}
				engine, _, _, err := openDaemonAnalyticsEngineWithRebuild(t.Context(), c, st, intent, func() { fired = true })
				require.NoError(err)
				assert.Equal(t, 1, calls)
				require.NoError(engine.Close())
			})
		}
	}
}
